# Progress reporter and ordered fakerInvoice — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a stderr progress reporter (chunks/bytes/rate/ETA) so long runs do not look hung, and make `fakerInvoice` emit ascending-with-deterministic-gaps invoice numbers per chunk.

**Architecture:** A new `internal/progress` package owns a `*Reporter` constructed in `run()` after the job list is built; workers call `r.ChunkDone(size)` after a successful chunk rename. Ordering for invoices is achieved via two new fields on `*faker.Faker` (`invoiceBase`, `invoiceCounter`) plus a `SetInvoiceBase` setter the chunk worker calls with `chunk.Index * faker.InvoiceStride`. Both features preserve byte-for-byte determinism across `-j`.

**Tech Stack:** Go 1.26, stdlib only (no new third-party deps; TTY detection via `os.File.Stat() & os.ModeCharDevice`).

**Spec:** `docs/superpowers/specs/2026-05-07-progress-and-ordered-invoices-design.md`

**Version control:** This repo uses **Jujutsu (`jj`)**. The "Commit" step in each task uses `jj commit -m '...'` which describes the current working-copy revision and starts a new empty one. Do **not** use `git commit`.

**Run before each commit:** `go fmt ./...` (per CLAUDE.md).

---

## File Structure

**Created:**
- `internal/progress/progress.go` — `Reporter` type, formatting helpers.
- `internal/progress/progress_test.go` — unit tests for formatting and atomics.

**Modified:**
- `internal/faker/faker.go` — `Faker` gains `invoiceBase`/`invoiceCounter`; new `SetInvoiceBase`; new exported `InvoiceStride` constant; `Invoice()` rewritten.
- `internal/faker/faker_test.go` — `TestInvoice_Format` updated to new format; new tests for `SetInvoiceBase` behaviour. The `Invoice` case in `TestDeterminism_SameSeedSameOutput` is removed (the new `Invoice()` no longer pulls from RNG).
- `cmd/mysql-anonymizer/pool.go` — `job` gains a `size uint64` field; `RunPool` accepts a `*progress.Reporter`; `processChunk` calls `f.SetInvoiceBase(...)` and `r.ChunkDone(j.size)` on success.
- `cmd/mysql-anonymizer/main.go` — `run()` stats each chunk's data file when building jobs, constructs the reporter, defers `Stop`, passes it to `RunPool`.

**Untouched:** every other file. The integration test does not reference `INV-` strings, and `anon.ProcessAllWithRowHook` already gives us a per-row hook (we do not need a new one).

---

## Task 1: `internal/progress` — package skeleton + size formatter

**Files:**
- Create: `internal/progress/progress.go`
- Create: `internal/progress/progress_test.go`

- [ ] **Step 1: Write the failing test for `formatBytes`**

Append to `internal/progress/progress_test.go`:

```go
package progress

import "testing"

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{uint64(1.4 * 1024 * 1024 * 1024), "1.4 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/progress/...`
Expected: FAIL with `undefined: formatBytes` (package not yet created).

- [ ] **Step 3: Create `internal/progress/progress.go` with `formatBytes`**

```go
// Package progress prints live "chunks done / bytes done / rate / ETA"
// status lines to stderr while the worker pool runs.
package progress

import "fmt"

// formatBytes renders n in human-readable IEC units (KiB / MiB / GiB / TiB).
// Uses one decimal for KiB and above, no decimals for plain bytes.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/progress/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "progress: add formatBytes helper"
```

---

## Task 2: `formatDuration` for ETA / elapsed

**Files:**
- Modify: `internal/progress/progress.go`
- Modify: `internal/progress/progress_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/progress/progress_test.go`:

```go
import "time"

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3*time.Minute + 12*time.Second, "3m12s"},
		{1*time.Hour + 2*time.Minute + 3*time.Second, "1h2m3s"},
		{500 * time.Millisecond, "0s"}, // sub-second rounds to 0s
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

(If the file already has an `import` block, merge `"time"` into it instead of adding a duplicate.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/progress/...`
Expected: FAIL with `undefined: formatDuration`.

- [ ] **Step 3: Implement `formatDuration`**

Add to `internal/progress/progress.go` (and add `"time"` to the import block):

```go
import (
	"fmt"
	"time"
)

// formatDuration renders d as "HhMmSs" / "MmSs" / "Ss", rounded to whole
// seconds. Sub-second durations render as "0s".
func formatDuration(d time.Duration) string {
	s := int64(d / time.Second)
	if s <= 0 {
		return "0s"
	}
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/progress/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "progress: add formatDuration helper"
```

---

## Task 3: `renderLine` — pure formatter for the status line

**Files:**
- Modify: `internal/progress/progress.go`
- Modify: `internal/progress/progress_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/progress/progress_test.go`:

```go
func TestRenderLine_NormalProgress(t *testing.T) {
	got := renderLine(14, 47,
		uint64(1.4*1024*1024*1024),
		uint64(5.8*1024*1024*1024),
		60*time.Second, // elapsed
	)
	// 1.4 GiB / 60s = ~23.9 MiB/s; ETA = (5.8-1.4)/0.0239 ≈ 184s ≈ 3m4s.
	want := "[14/47 chunks · 1.4 GiB / 5.8 GiB · 23 MiB/s · ETA 3m4s]"
	if got != want {
		t.Errorf("renderLine = %q, want %q", got, want)
	}
}

func TestRenderLine_NoRateYet(t *testing.T) {
	got := renderLine(0, 47, 0, 1024*1024*1024, 0)
	want := "[0/47 chunks · 0 B / 1.0 GiB · -- · ETA --]"
	if got != want {
		t.Errorf("renderLine = %q, want %q", got, want)
	}
}

func TestRenderLine_AllDone(t *testing.T) {
	got := renderLine(47, 47, 1024*1024*1024, 1024*1024*1024, 10*time.Second)
	// rate = 1 GiB / 10s = 102.4 MiB/s; ETA = 0.
	want := "[47/47 chunks · 1.0 GiB / 1.0 GiB · 102 MiB/s · ETA 0s]"
	if got != want {
		t.Errorf("renderLine = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/progress/...`
Expected: FAIL with `undefined: renderLine`.

- [ ] **Step 3: Implement `renderLine`**

Add to `internal/progress/progress.go`:

```go
// renderLine formats one status line. It is a pure function: same inputs
// always produce the same string, with no I/O and no clock access.
//
// Rate is reported as integer MiB/s. When elapsed is zero (or rounds to
// zero), rate and ETA both render as "--". When rate is non-zero but
// remaining bytes are zero, ETA renders as "0s".
func renderLine(doneChunks, totalChunks int, doneBytes, totalBytes uint64, elapsed time.Duration) string {
	rateStr := "--"
	etaStr := "--"
	if elapsed >= time.Second {
		bytesPerSec := float64(doneBytes) / elapsed.Seconds()
		mibPerSec := bytesPerSec / (1024 * 1024)
		rateStr = fmt.Sprintf("%d MiB/s", int64(mibPerSec))
		if bytesPerSec > 0 && totalBytes >= doneBytes {
			remaining := float64(totalBytes - doneBytes)
			eta := time.Duration(remaining/bytesPerSec) * time.Second
			etaStr = formatDuration(eta)
		}
	}
	return fmt.Sprintf("[%d/%d chunks · %s / %s · %s · ETA %s]",
		doneChunks, totalChunks,
		formatBytes(doneBytes), formatBytes(totalBytes),
		rateStr, etaStr,
	)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/progress/...`
Expected: PASS (all three `TestRenderLine_*` cases).

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "progress: add renderLine formatter"
```

---

## Task 4: `Reporter` — atomics, Start/Stop, ChunkDone, ticker

**Files:**
- Modify: `internal/progress/progress.go`
- Modify: `internal/progress/progress_test.go`

- [ ] **Step 1: Write the failing test for `ChunkDone` atomics**

Append to `internal/progress/progress_test.go`:

```go
import (
	"bytes"
	"context"
	"sync"
)

func TestReporter_ChunkDoneAccumulates(t *testing.T) {
	r := New(10, 1000, &bytes.Buffer{})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ChunkDone(100)
		}()
	}
	wg.Wait()
	if got := r.doneChunks.Load(); got != 10 {
		t.Errorf("doneChunks = %d, want 10", got)
	}
	if got := r.doneBytes.Load(); got != 1000 {
		t.Errorf("doneBytes = %d, want 1000", got)
	}
}

func TestReporter_StopPrintsFinalLine(t *testing.T) {
	var buf bytes.Buffer
	r := New(2, 2048, &buf)
	r.isTTY = false // force non-TTY mode
	r.tick = time.Hour // disable mid-run ticks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.ChunkDone(1024)
	r.ChunkDone(1024)
	r.Stop(nil)
	out := buf.String()
	if !strings.Contains(out, "2/2 chunks") {
		t.Errorf("final output missing chunk total: %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("final output missing 'done' marker: %q", out)
	}
}

func TestReporter_StopOnError(t *testing.T) {
	var buf bytes.Buffer
	r := New(5, 5000, &buf)
	r.isTTY = false
	r.tick = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.ChunkDone(1000)
	r.Stop(fmt.Errorf("boom"))
	out := buf.String()
	if !strings.Contains(out, "aborted at 1/5 chunks") {
		t.Errorf("error final output missing 'aborted at 1/5 chunks': %q", out)
	}
}
```

(Merge `"bytes"`, `"context"`, `"strings"`, `"sync"` into the existing import block.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/progress/...`
Expected: FAIL with `undefined: New` and other missing symbols.

- [ ] **Step 3: Implement `Reporter`**

Replace the `progress.go` import block and append the type:

```go
import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// Reporter prints a refreshing one-line status to its writer (stderr in
// production). One Reporter is created per run; workers report completed
// chunks via ChunkDone, which is safe for concurrent use.
type Reporter struct {
	totalChunks int
	totalBytes  uint64

	doneChunks atomic.Uint64
	doneBytes  atomic.Uint64

	start time.Time
	out   io.Writer
	isTTY bool
	tick  time.Duration

	stopCh chan struct{}
	doneCh chan struct{}
}

// New constructs a Reporter writing to out. Defaults: TTY auto-detected
// from os.Stderr if out is os.Stderr; otherwise non-TTY. Tick is 500ms on
// TTY, 5s on non-TTY. Both are exported as fields for test injection.
func New(totalChunks int, totalBytes uint64, out io.Writer) *Reporter {
	r := &Reporter{
		totalChunks: totalChunks,
		totalBytes:  totalBytes,
		out:         out,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	if f, ok := out.(*os.File); ok {
		r.isTTY = isCharDevice(f)
	}
	if r.isTTY {
		r.tick = 500 * time.Millisecond
	} else {
		r.tick = 5 * time.Second
	}
	return r
}

// isCharDevice reports whether f looks like a terminal (no x/term dep).
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Start launches the ticker goroutine. It exits when ctx is done or Stop
// is called.
func (r *Reporter) Start(ctx context.Context) {
	r.start = time.Now()
	go r.loop(ctx)
}

func (r *Reporter) loop(ctx context.Context) {
	defer close(r.doneCh)
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			r.render()
		}
	}
}

func (r *Reporter) render() {
	line := renderLine(
		int(r.doneChunks.Load()), r.totalChunks,
		r.doneBytes.Load(), r.totalBytes,
		time.Since(r.start),
	)
	if r.isTTY {
		fmt.Fprintf(r.out, "\r\x1b[K%s", line)
	} else {
		fmt.Fprintln(r.out, line)
	}
}

// ChunkDone records a successful chunk completion. compressedBytes is the
// on-disk size of the input chunk (used for the progress denominator).
func (r *Reporter) ChunkDone(compressedBytes uint64) {
	r.doneChunks.Add(1)
	r.doneBytes.Add(compressedBytes)
}

// Stop signals the ticker to exit, waits for it, then prints a final line.
// If runErr is non-nil the final line is prefixed "aborted at ...".
func (r *Reporter) Stop(runErr error) {
	close(r.stopCh)
	<-r.doneCh
	done := r.doneChunks.Load()
	bytesDone := r.doneBytes.Load()
	elapsed := time.Since(r.start)
	body := renderLine(int(done), r.totalChunks, bytesDone, r.totalBytes, elapsed)
	prefix := "done "
	if runErr != nil {
		prefix = fmt.Sprintf("aborted at %d/%d chunks ", done, r.totalChunks)
	}
	if r.isTTY {
		fmt.Fprintf(r.out, "\r\x1b[K%s%s\n", prefix, body)
	} else {
		fmt.Fprintf(r.out, "%s%s\n", prefix, body)
	}
}
```

- [ ] **Step 4: Run tests with the race detector**

Run: `go test -race ./internal/progress/...`
Expected: PASS for all tests, no race warnings.

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "progress: add Reporter with concurrent ChunkDone and final-line printing"
```

---

## Task 5: `faker.InvoiceStride` constant + `SetInvoiceBase` + reformatted `Invoice`

**Files:**
- Modify: `internal/faker/faker.go`
- Modify: `internal/faker/faker_test.go`

- [ ] **Step 1: Update existing tests for the new format and remove RNG-determinism case**

Replace `internal/faker/faker_test.go` in full:

```go
package faker

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func newDeterministic() *Faker { return New(rand.NewPCG(42, 99)) }

func TestDeterminism_SameSeedSameOutput(t *testing.T) {
	a := newDeterministic()
	b := newDeterministic()
	cases := []struct {
		name string
		fn   func(*Faker) string
	}{
		{"Email", func(f *Faker) string { return f.gf.Email() }},
		{"Name", func(f *Faker) string { return f.gf.Name() }},
		{"Phone", func(f *Faker) string { return f.gf.Phone() }},
		{"IBAN", func(f *Faker) string { return f.IBAN() }},
		{"SWIFT", func(f *Faker) string { return f.SWIFT() }},
		{"EIN", func(f *Faker) string { return f.EIN() }},
		{"SecondaryAddress", func(f *Faker) string { return f.SecondaryAddress() }},
	}
	for _, tc := range cases {
		for i := range 10 {
			if got, want := tc.fn(a), tc.fn(b); got != want {
				t.Errorf("%s [%d]: a=%q b=%q", tc.name, i, got, want)
			}
		}
	}
}

func TestDeterminism_DifferentSeedsDiffer(t *testing.T) {
	a := New(rand.NewPCG(1, 2))
	b := New(rand.NewPCG(3, 4))
	eq := 0
	for range 10 {
		if a.gf.Email() == b.gf.Email() {
			eq++
		}
	}
	if eq == 10 {
		t.Errorf("all 10 emails equal across distinct seeds")
	}
}

func TestInvoice_FormatAndAscending(t *testing.T) {
	f := newDeterministic()
	prev := ""
	for i := range 20 {
		inv := f.Invoice()
		if !strings.HasPrefix(inv, "INV-") || len(inv) != 4+16 {
			t.Errorf("Invoice %q: want INV- + 16 digits", inv)
		}
		if i > 0 && inv <= prev {
			t.Errorf("Invoice not ascending: prev=%q cur=%q", prev, inv)
		}
		prev = inv
	}
}

func TestInvoice_StartsAtZero(t *testing.T) {
	f := newDeterministic()
	if got, want := f.Invoice(), "INV-0000000000000000"; got != want {
		t.Errorf("first invoice = %q, want %q", got, want)
	}
	if got, want := f.Invoice(), "INV-0000000000000001"; got != want {
		t.Errorf("second invoice = %q, want %q", got, want)
	}
}

func TestInvoice_SetBaseAndReset(t *testing.T) {
	f := newDeterministic()
	f.Invoice() // counter = 1
	f.Invoice() // counter = 2
	f.SetInvoiceBase(0)
	if got, want := f.Invoice(), "INV-0000000000000000"; got != want {
		t.Errorf("after SetInvoiceBase(0): got %q, want %q", got, want)
	}
}

func TestInvoice_BaseStridesAcrossChunks(t *testing.T) {
	f := newDeterministic()
	f.SetInvoiceBase(InvoiceStride) // chunk 1
	if got, want := f.Invoice(), "INV-0000001000000000"; got != want {
		t.Errorf("chunk-1 first invoice = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/faker/...`
Expected: FAIL with errors about `SetInvoiceBase`, `InvoiceStride`, and the new `Invoice()` format.

- [ ] **Step 3: Implement the new `Invoice()` and friends**

Replace lines 26-35 (the `Faker` struct) and lines 58-62 (the existing `Invoice` method) of `internal/faker/faker.go`. The full updated regions:

```go
// Faker is a per-worker fake-data generator. Construct one per worker with New
// and reuse it for the lifetime of that worker; do not share across goroutines
// (gofakeit.Faker is not safe for concurrent use unless explicitly locked, and
// we deliberately leave locking off for throughput).
//
// invoiceBase and invoiceCounter back the ordered Invoice() generator. The
// chunk worker calls SetInvoiceBase(chunkIndex * InvoiceStride) before
// processing rows; Invoice() then returns ascending values within that
// chunk's reserved range.
type Faker struct {
	gf             *gofakeit.Faker
	invoiceBase    uint64
	invoiceCounter uint64
}
```

```go
// InvoiceStride is the per-chunk reserved range for ordered invoice numbers.
// Each chunk index i owns invoice numbers in [i*InvoiceStride, (i+1)*InvoiceStride).
// Sized at 1e9 — well above mysqlsh's worst-case rows-per-chunk — so chunks
// never collide.
const InvoiceStride uint64 = 1_000_000_000

// SetInvoiceBase sets the starting number for subsequent Invoice() calls and
// resets the counter to zero. Call this once per chunk before processing
// rows.
func (f *Faker) SetInvoiceBase(b uint64) {
	f.invoiceBase = b
	f.invoiceCounter = 0
}

// Invoice returns the next ordered invoice identifier "INV-<16 digits>".
// Within a chunk the returned numbers ascend from invoiceBase. Across
// chunks (different bases set via SetInvoiceBase) the ranges are disjoint
// by InvoiceStride.
//
// Panics if a chunk produces more than InvoiceStride rows, which would
// collide with the next chunk's range.
func (f *Faker) Invoice() string {
	if f.invoiceCounter >= InvoiceStride {
		panic(fmt.Sprintf("faker: invoice counter %d exceeds stride %d", f.invoiceCounter, InvoiceStride))
	}
	n := f.invoiceBase + f.invoiceCounter
	f.invoiceCounter++
	return fmt.Sprintf("INV-%016d", n)
}
```

Add `"fmt"` to the existing import block of `internal/faker/faker.go` (it currently imports `math/rand/v2`, `strings`, `text/template`, `gofakeit/v7`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/faker/...`
Expected: PASS for all tests.

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "faker: ordered Invoice() with per-chunk base and 16-digit format"
```

---

## Task 6: Wire `SetInvoiceBase` into the chunk worker

**Files:**
- Modify: `cmd/mysql-anonymizer/pool.go:107-112` (the `processChunk` lines that build the `Faker`)

- [ ] **Step 1: Read the current `processChunk` start**

Open `cmd/mysql-anonymizer/pool.go`. Find the lines:

```go
hi, lo := deriveSeed(jobSeed, j.tableKey, j.chunk.Index)
f := faker.New(rand.NewPCG(hi, lo))
cc, err := rc.Compile(f)
```

- [ ] **Step 2: Insert the `SetInvoiceBase` call**

Replace those three lines with:

```go
hi, lo := deriveSeed(jobSeed, j.tableKey, j.chunk.Index)
f := faker.New(rand.NewPCG(hi, lo))
f.SetInvoiceBase(uint64(j.chunk.Index) * faker.InvoiceStride)
cc, err := rc.Compile(f)
```

- [ ] **Step 3: Build to confirm it compiles**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Run all faker + cmd tests**

Run: `go test ./internal/faker/... ./cmd/...`
Expected: PASS. (The integration test does not assert on `INV-` strings.)

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "anonymizer: set per-chunk invoice base before processing rows"
```

---

## Task 7: Stat each chunk's data file when building the job list

**Files:**
- Modify: `cmd/mysql-anonymizer/pool.go` — add `size uint64` to the `job` struct.
- Modify: `cmd/mysql-anonymizer/main.go` — populate `size` when appending to `jobs`.

- [ ] **Step 1: Add `size` to the `job` struct**

In `cmd/mysql-anonymizer/pool.go`, change:

```go
type job struct {
	tableKey string
	schema   *tableSchema
	chunk    dump.ChunkEntry
}
```

to:

```go
type job struct {
	tableKey string
	schema   *tableSchema
	chunk    dump.ChunkEntry
	size     uint64 // compressed bytes of the input chunk; used for progress reporting
}
```

- [ ] **Step 2: Populate `size` in `main.go`'s job-build loop**

In `cmd/mysql-anonymizer/main.go`, find the block:

```go
// 7. Build job list.
var jobs []job
for k := range schemas {
	for _, c := range manifest.Tables[k].Chunks {
		jobs = append(jobs, job{tableKey: k, schema: schemas[k], chunk: c})
	}
}
```

Replace it with:

```go
// 7. Build job list. Stat each chunk so the progress reporter has a
// per-job byte size without needing a second pass over the manifest.
var jobs []job
var totalBytes uint64
for k := range schemas {
	for _, c := range manifest.Tables[k].Chunks {
		fi, err := os.Stat(c.DataPath)
		if err != nil {
			return fmt.Errorf("stat chunk %s: %w", c.DataPath, err)
		}
		size := uint64(fi.Size())
		jobs = append(jobs, job{tableKey: k, schema: schemas[k], chunk: c, size: size})
		totalBytes += size
	}
}
_ = totalBytes // wired up in Task 8
```

- [ ] **Step 3: Build to confirm**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Run cmd tests**

Run: `go test ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "anonymizer: stat chunk sizes when building the job list"
```

---

## Task 8: Wire the progress reporter into `run` and `RunPool`

**Files:**
- Modify: `cmd/mysql-anonymizer/pool.go` — `RunPool` accepts a `*progress.Reporter`; `processChunk` calls `r.ChunkDone` on success.
- Modify: `cmd/mysql-anonymizer/main.go` — construct the reporter, `Start`/`Stop` it, pass it to `RunPool`.

- [ ] **Step 1: Add the reporter parameter to `RunPool`**

In `cmd/mysql-anonymizer/pool.go`:

1. Add the import: `"github.com/screechingbagel/mysql-tsv-anonymizer/internal/progress"`.
2. Change the `RunPool` signature from:

```go
func RunPool(
	ctx context.Context,
	jobs []job,
	rc *config.RawConfig,
	schemas map[string]*tableSchema,
	jobSeed uint64,
	outDir string,
	nWorkers int,
) error {
```

to:

```go
func RunPool(
	ctx context.Context,
	jobs []job,
	rc *config.RawConfig,
	schemas map[string]*tableSchema,
	jobSeed uint64,
	outDir string,
	nWorkers int,
	reporter *progress.Reporter,
) error {
```

3. In the worker body, change:

```go
if err := processChunk(ctx, j, rc, jobSeed, outDir); err != nil {
	record(fmt.Errorf("chunk %s@@%d: %w", j.tableKey, j.chunk.Index, err))
	return
}
```

to:

```go
if err := processChunk(ctx, j, rc, jobSeed, outDir); err != nil {
	record(fmt.Errorf("chunk %s@@%d: %w", j.tableKey, j.chunk.Index, err))
	return
}
if reporter != nil {
	reporter.ChunkDone(j.size)
}
```

(The `nil` check keeps existing tests that call `RunPool` without a reporter working — see Step 4.)

- [ ] **Step 2: Construct, Start, and Stop the reporter in `run`**

In `cmd/mysql-anonymizer/main.go`:

1. Add imports: `"os"` is already there; add `"github.com/screechingbagel/mysql-tsv-anonymizer/internal/progress"`.
2. Replace the placeholder `_ = totalBytes` and the `RunPool(...)` call with:

```go
reporter := progress.New(len(jobs), totalBytes, os.Stderr)
reporter.Start(ctx)
runErr := RunPool(ctx, jobs, rc, schemas, o.Seed, o.OutDir, o.Workers, reporter)
reporter.Stop(runErr)
if runErr != nil {
	return runErr
}
```

So step 8 of `run` reads:

```go
// 8. Run pool with live progress reporting on stderr.
reporter := progress.New(len(jobs), totalBytes, os.Stderr)
reporter.Start(ctx)
runErr := RunPool(ctx, jobs, rc, schemas, o.Seed, o.OutDir, o.Workers, reporter)
reporter.Stop(runErr)
if runErr != nil {
	return runErr
}
```

- [ ] **Step 3: Update any existing test callers of `RunPool`**

Run: `grep -n "RunPool(" cmd/mysql-anonymizer/`
Expected: only one production call site (the one just edited). If any test calls `RunPool` directly, append `, nil` as the new last argument to each call.

- [ ] **Step 4: Build and run all tests**

Run: `go build ./... && go test ./...`
Expected: PASS for the whole module.

- [ ] **Step 5: Smoke-test progress output against the integration fixture**

Run (from the repo root):

```bash
go build -o /tmp/mysql-anonymizer ./cmd/mysql-anonymizer
ls testdata/fixtures
```

If a usable fixture dump + config pair exists (look in `testdata/fixtures` and at the existing `cmd/mysql-anonymizer/integration_test.go` for the in-test invocation pattern), run the binary against it with `--in <fixture-dump> --out /tmp/clean-$$ --c <fixture-config> --seed 1` and confirm a `[X/Y chunks ...]` line is printed on stderr and a `done [...]` line appears at the end. Delete `/tmp/clean-$$` afterwards.

If no fixture is conveniently runnable from the command line, skip the manual run — `go test ./...` already exercises `RunPool` end-to-end via `integration_test.go`, and the unit tests in `internal/progress` cover the Reporter logic.

- [ ] **Step 6: Commit**

```bash
go fmt ./...
jj commit -m "anonymizer: stderr progress reporter (chunks/bytes/rate/ETA)"
```

---

## Task 9: README touch-up

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the `fakerInvoice` row in the README's helper table**

Find the line:

```
| `fakerInvoice` | `INV-XXXXXXXX` |
```

Replace with:

```
| `fakerInvoice` | `INV-` + 16-digit ascending number per chunk (e.g. `INV-0000000000000042`) |
```

- [ ] **Step 2: Add a one-paragraph mention of progress output**

Below the "Usage" section, add:

```markdown
## Progress output

While the run is in flight the tool prints a single status line on stderr,
refreshed every ~500 ms on a TTY (every 5 s on a non-TTY):

    [14/47 chunks · 1.4 GiB / 5.8 GiB · 23 MiB/s · ETA 3m12s]

Bytes are the compressed input size of completed chunks. A final
`done [...]` (or `aborted at N/M chunks [...]` on failure) line is
printed on exit. There is no flag — output goes to stderr regardless,
so redirecting stdout does not silence it.
```

- [ ] **Step 3: Commit**

```bash
jj commit -m "docs: README — note progress output and new fakerInvoice format"
```

---

## Task 10: Final whole-module verification

**Files:** none modified — verification only.

- [ ] **Step 1: Format, vet, build, test with race detector**

Run:

```bash
go fmt ./...
go vet ./...
go build ./...
go test -race ./...
```

Expected: each command exits 0; `go test` reports PASS for every package.

- [ ] **Step 2: Confirm working copy is clean**

Run: `jj st`
Expected: "The working copy has no changes." (any earlier `go fmt` reformatting should have been included in the most recent commit; if `jj st` shows changes, run `jj commit -m "fmt"` to capture them.)

---

## Self-Review

**Spec coverage:**
- §1 Goal (progress, no flag) → Tasks 1-4, 8.
- §1 Output format → Task 3 (`renderLine`), Task 4 (final-line prefix).
- §1 TTY vs. non-TTY → Task 4 (`isCharDevice`, tick choice, `\r\x1b[K` vs newline).
- §1 Architecture → Tasks 1-4 (package structure, atomics, Start/Stop).
- §1 Wiring → Tasks 7-8.
- §1 Tests → Tasks 1-4 (unit), Task 8 step 4 (integration via existing tests).
- §2 Goal + Mechanism → Task 5.
- §2 Wiring → Task 6.
- §2 Determinism (preserved) → Task 6 (per-chunk base derived only from `chunk.Index`).
- §2 Tests → Task 5.
- Out-of-scope items (no `rowSeq`, no `--quiet`, no two-pass) → not addressed, as intended.

**Placeholder scan:** none — every step has the literal code or command.

**Type consistency:**
- `Reporter` fields (`doneChunks`, `doneBytes`, `isTTY`, `tick`, `out`, `start`, `stopCh`, `doneCh`) are referenced consistently across Tasks 1-4 and Task 8.
- `progress.New(totalChunks int, totalBytes uint64, out io.Writer)` is the same signature in Tasks 4 and 8.
- `ChunkDone(uint64)` matches the worker call in Task 8.
- `faker.InvoiceStride` is `uint64`; `j.chunk.Index` is `int` — Task 6 uses `uint64(j.chunk.Index) * faker.InvoiceStride`, no implicit conversion.
- `job.size` is `uint64` (Task 7), passed to `ChunkDone(uint64)` (Task 8) — match.
