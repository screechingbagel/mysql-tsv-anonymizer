# Progress reporting and ordered `fakerInvoice`

Two small, independent additions to `mysql-anonymizer`:

1. A live progress reporter so long runs do not look hung.
2. Per-chunk striped numbering for `fakerInvoice` so generated invoice
   numbers are ascending across the table.

Both preserve the existing determinism guarantee (same `--seed` and same
input dump produce byte-identical output regardless of `-j`).

---

## 1. Progress reporter

### Goal

Print a single, refreshing status line on stderr while the worker pool
runs, so the operator can see that work is progressing and roughly how
much is left. No flag — on by default.

### Output format

One line, rewritten in place on a TTY, appended once every few seconds
on a non-TTY:

```
[14/47 chunks · 1.4 GiB / 5.8 GiB · 23 MiB/s · ETA 3m12s]
```

Fields:

- `done/total chunks` — completed chunk jobs vs. total queued.
- `done bytes / total bytes` — sum of *compressed* input sizes, in
  human-readable units (KiB / MiB / GiB).
- rate — `done bytes / elapsed`, in `MiB/s`. Prints `--` while elapsed
  is too small to give a stable rate.
- ETA — `(total - done) / rate`, formatted `1h2m3s` / `3m12s` / `45s`.
  Prints `--` when rate is unknown.

A final line is printed at end-of-run with totals and total elapsed.
On error, the final line is prefixed `aborted at 12/47 chunks ...`
so it is not mistaken for a success summary.

### TTY vs. non-TTY

`golang.org/x/term.IsTerminal(int(os.Stderr.Fd()))` selects:

- TTY: 500 ms tick, line is rewritten via leading `\r` and trailing
  `\x1b[K` (clear-to-EOL). Final line uses `\n`.
- Non-TTY (CI logs, redirected stderr): 5 s tick, each line ends with
  `\n`. Same line format, no carriage returns or escape sequences.

If `NO_COLOR` is set or stderr is not a terminal, no escape sequences
are emitted.

### Architecture

New package `internal/progress`:

```go
type Reporter struct {
    totalChunks uint64
    totalBytes  uint64
    doneChunks  atomic.Uint64
    doneBytes   atomic.Uint64
    start       time.Time
    isTTY       bool
    out         io.Writer // os.Stderr, injectable for tests
    tick        time.Duration
    stop        chan struct{}
    done        chan struct{}
}

func New(totalChunks int, totalBytes uint64, out io.Writer) *Reporter
func (r *Reporter) Start(ctx context.Context)
func (r *Reporter) ChunkDone(compressedBytes uint64)
func (r *Reporter) Stop(err error) // prints final line
```

- `Start` launches one goroutine that ticks at `r.tick`, renders a line
  from the atomics, and writes it to `r.out`. The goroutine exits when
  `ctx.Done()` fires or `Stop` closes `r.stop`; either way it signals
  completion via `r.done` so `Stop` can wait for the last render to
  finish before printing the final line.
- `ChunkDone` is called from each worker after a successful
  `os.Rename` of the chunk's `.tmp` outputs. It is the only mutator on
  the hot path; both updates are `Add`s on `atomic.Uint64`.

### Wiring

In `cmd/mysql-anonymizer/main.go`'s `run`:

1. After step 7 (build job list), compute `totalBytes` by summing
   `os.Stat(chunk.DataPath).Size()` for each job. Cache the size on the
   `job` struct so the worker can pass it to `ChunkDone` without
   re-statting.
2. Construct `progress.New(len(jobs), totalBytes, os.Stderr)`.
3. `Start(ctx)`; `defer r.Stop(runErr)` (capture the eventual error so
   the final line can reflect success vs. abort).
4. Pass the reporter into `RunPool`; workers call `r.ChunkDone(j.size)`
   immediately before `processChunk` returns nil.

### Tests

`internal/progress/progress_test.go`:

- Render with a `bytes.Buffer` in place of stderr, fixed clock, fixed
  totals; assert exact line text after a manual tick.
- TTY vs. non-TTY: passing an explicit `isTTY` flag (constructor
  variant for tests) controls which formatter runs.
- Concurrent `ChunkDone` calls produce monotonically non-decreasing
  counters (race detector).

No integration test — the existing pool tests already exercise the
worker-completion path; visual output is covered by unit tests on the
formatter.

---

## 2. Ordered `fakerInvoice`

### Goal

Make `{{ fakerInvoice }}` emit invoice numbers that are strictly
ascending across each table's chunks (with deterministic gaps between
chunks), instead of the current random `INV-XXXXXXXX`.

### Mechanism

`Faker` gains two unexported fields and one setter:

```go
type Faker struct {
    gf             *gofakeit.Faker
    invoiceBase    uint64
    invoiceCounter uint64
}

func (f *Faker) SetInvoiceBase(b uint64) {
    f.invoiceBase = b
    f.invoiceCounter = 0
}
```

`Invoice` becomes:

```go
const invoiceStride = 1_000_000_000 // 1e9 rows per chunk

func (f *Faker) Invoice() string {
    if f.invoiceCounter >= invoiceStride {
        panic("faker: invoice counter exceeded stride; chunk has more than 1e9 rows")
    }
    n := f.invoiceBase + f.invoiceCounter
    f.invoiceCounter++
    return fmt.Sprintf("INV-%016d", n)
}
```

- 16-digit zero-padded suffix — supports up to ~10⁷ chunks at stride
  10⁹ before the value would exceed 16 digits, well past anything
  realistic.
- Padding ensures values are lexicographically ordered (same width),
  not just numerically.

### Wiring

In `cmd/mysql-anonymizer/pool.go`'s `processChunk`, immediately after
`f := faker.New(...)`:

```go
f.SetInvoiceBase(uint64(j.chunk.Index) * faker.InvoiceStride)
```

`invoiceStride` is exported as `faker.InvoiceStride` so the call site
references the same constant.

No locking is needed — each `Faker` is constructed inside
`processChunk` and used by exactly one goroutine. The bootstrap
`Faker` constructed in `run` for config validation never has
`SetInvoiceBase` called on it, so its `Invoice` returns
`"INV-0000000000000000"`, `"INV-0000000000000001"`, … which is fine
for syntax-validation purposes.

### Determinism

- `invoiceBase` is a pure function of `chunk.Index`.
- `invoiceCounter` is deterministic from there given a fixed input
  chunk (the template runs once per row in a fixed order).
- Therefore the per-`(table, chunk)` byte output is unchanged in
  determinism — same seed and same input still produce identical
  output regardless of `-j`.

### Tests

`internal/faker/faker_test.go` additions:

- After `SetInvoiceBase(0)`, three consecutive `Invoice()` calls
  return `"INV-0000000000000000"`, `"INV-0000000000000001"`,
  `"INV-0000000000000002"`.
- After `SetInvoiceBase(InvoiceStride)`, the first call returns
  `"INV-0000001000000000"` (1e9 zero-padded to 16 digits — no
  collision with chunk 0's range).
- `SetInvoiceBase` resets the counter: call `Invoice()` twice, then
  `SetInvoiceBase(0)`, then `Invoice()` returns `"INV-0000000000000000"`.
- Existing `TestInvoice_Format` is updated to match the new format
  (`INV-` + 16 digits).

The integration test in `cmd/mysql-anonymizer/integration_test.go`
that exercises `fakerInvoice` (if any references the format) is
updated to match.

---

## Out of scope

- A generic `rowSeq` template helper for other ascending IDs.
  `fakerInvoice` is the only ordered identifier needed today; if a
  second one is needed later, factor `SetInvoiceBase` /
  `invoiceCounter` into a shared "row counter" abstraction at that
  point.
- A `--quiet` / `--progress` flag. Progress is on by default; if
  silencing is needed later, add the flag then.
- True consecutive (gap-free) numbering via a pre-scan pass — the
  weaker "ascending with deterministic gaps" satisfies the stated
  requirement at zero runtime cost.
