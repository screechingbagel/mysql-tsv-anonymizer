# MySQL Dump Anonymizer — Design

**Date:** 2026-05-03
**Status:** approved (brainstorm), ready for implementation plan

> **Errata (2026-05-04, after Task 1 fixture verification).** Three load-bearing assumptions in this document turned out to be wrong against real `mysqlsh` 9.7 / mysql 8.4 output. Ground truth lives in `testdata/fixtures/notes.md`; the body below is **not** updated in place except where noted, so callouts here are authoritative when they conflict:
>
> - **`.idx` is NOT per-row offsets.** It is a single 8-byte big-endian `uint64` giving the total decompressed length of its sibling `.zst` chunk. No header, no trailer, one record per chunk. Every other description of `.idx` in this doc — including the entire "`.idx` regeneration" section — must be read with this in mind. There is no random row access; consumers that need row offsets must scan the decompressed stream.
> - **`compression` lives in the per-table JSON, not `@.json`.** The strict-check in step 4 of "Startup" must read `<schema>@<table>.json` (top-level `compression` field), not `@.json`. `@.json` has no `compression` key.
> - **Per-table column array path is `options.columns`** (a JSON string array, in physical column order matching TSV cell order). The per-table JSON also exposes `options.fieldsTerminatedBy`, `linesTerminatedBy`, `fieldsEscapedBy`, `extension`, and `compression`; a robust loader should consult these rather than hard-code the dialect.
> - **Chunk filename suffix:** non-final chunks are `<basename>@<n>.tsv.zst` (single `@`), final chunks are `<basename>@@<n>.tsv.zst` (double `@`). Single-chunk tables get `@@0`. The walker must match both patterns.
> - **`bytesPerChunk` minimum is 128k** (mysqlsh hard-rejects smaller). Forcing multi-chunk in fixtures requires data > 128k.

## Purpose

A CI tool that takes a `mysqlsh util.dumpInstance` directory, rewrites configured columns of configured tables with fake data, and emits a sibling directory in the same format that `util.loadDump` can consume into a staging database.

Internal tool. Narrow scope. One way to do everything.

## Out of scope (explicit)

- Other dump tools (`mysqldump`, `xtrabackup`, etc.).
- Compression formats other than `zstd`. mysqlsh's default; we hard-fail anything else.
- Cross-row / cross-table value consistency. Each row gets independent fakes; if `customer.email` and `donation.email` are the same person in prod, they will not match in clean output.
- Row drop. Rules can replace cells or write SQL `NULL` (`{{ null }}`); they cannot remove rows. Use `mysqldump --where` upstream or `DELETE` in staging.
- Streaming overlap with the dump process. Run after `@.done.json` exists.
- Resume / incremental runs.
- Column types. Templates always render to strings; numeric columns rely on MySQL's implicit string→number coercion at load time, the same way the previous tool worked.

## Inputs and outputs

```
mysql-anonymizer  --in <dump-dir>  --out <clean-dir>  -c <config.yaml>
                  --seed <uint64>  [-j <N>]
```

| Flag | Required | Description |
|---|---|---|
| `--in` | yes | Path to a `mysqlsh util.dumpInstance` output directory. Must contain `@.done.json`. |
| `--out` | yes | Output directory. Must not exist or must be empty. Created if absent. |
| `-c` / `--config` | yes | YAML config (format below). |
| `--seed` | yes | `uint64` job seed. Required and explicit so CI runs are intentionally reproducible. To get non-deterministic output, pass a time-based value. |
| `-j` | no | Worker count. Defaults to `runtime.NumCPU()`. |

Output is a directory mirroring the input layout: chunks of configured tables are decoded → rewritten → re-encoded → re-zstd, with `.idx` sidecars regenerated. All other files are copied (or hardlinked, fallback to copy) byte-for-byte. The dump's `@.done.json` is copied last; its presence in the output dir is the "this clean dir is complete" signal for downstream consumers.

## Module layout

Single binary; everything except `cmd/` is `internal/`.

```
cmd/mysql-anonymizer/        # main, flag parsing, orchestration
internal/dump/               # dump-dir walker, manifest, per-table .json loader
internal/tsv/                # mysqlsh-dialect TSV reader + writer (with LICENSE)
internal/zstd/               # klauspost/compress/zstd wrapper
internal/anon/               # row processor: applies compiled rules to a row
internal/idx/                # .idx regeneration
internal/config/             # YAML + text/template loader (already moved)
internal/faker/              # gofakeit wrapper, value type (already moved)
```

## Configuration

YAML, identical schema to the previous tool's config so existing rule files load unchanged (with one removed feature, `{{ drop }}`).

```yaml
filters:
  <table_name>:
    columns:
      <column_name>:
        value: "<go text/template string>"
```

`<value>` is a `text/template` expression. The runtime binds it per job to the processing worker's `*Faker.FuncMap()` (see "Config compile model" below). Functions:

| Function | Purpose |
|---|---|
| `fakerName`, `fakerFirstName`, `fakerLastName` | Person names |
| `fakerEmail`, `fakerPhone` | Contact |
| `fakerAddress`, `fakerStreetAddress`, `fakerSecondaryAddress`, `fakerCity`, `fakerPostcode` | Address |
| `fakerCompany`, `fakerIBAN`, `fakerSwift`, `fakerEIN` | Company / finance |
| `fakerInvoice` | `INV-` + 8 alphanumeric chars from this worker's RNG. Replaces the previous tool's mutex-counter version. |
| `uuidv4` | UUID v4 |
| `randAlphaNum N`, `randNumeric N` | Random string/digit sequences |
| `upper`, `lower` | String case |
| `null` | Sentinel — emit SQL `NULL`. Must be the entire template output. |

Removed from the previous tool: `drop` sentinel.

## Runtime flow

### Startup

1. Parse flags. Create a throwaway `*Faker` solely so `config.Load` has a `FuncMap` to validate template syntax against.
2. `config.Load(path, bootstrapFaker)` parses the YAML and pre-compiles every column rule into a `*template.Template`. Any syntax error or unknown function fails here, before any data is read. (The bootstrap Faker and its compiled templates are discarded; per-job compilation happens later, bound to per-job RNGs. The startup compile exists only to fail-fast on bad config.)
3. Walk `--in`. Build a manifest classifying every file:
   - top-level: `@.json`, `@.sql`, `@.post.sql`, `@.users.sql`, `@.done.json`
   - per-schema: `<schema>.json`, `<schema>.sql`
   - per-table: `<schema>@<table>.json`, `<schema>@<table>.sql`
   - per-chunk: `<schema>@<table>@@<n>.tsv.zst`, `<schema>@<table>@@<n>.tsv.zst.idx`
4. Read the dump's `@.json`. **Strict check:** `compression == "zstd"`, else fatal.
5. Verify `@.done.json` exists, else fatal (the dump is incomplete or failed).
6. For every table mentioned in the config:
   - Load `<schema>@<table>.json`, extract the ordered `columns` array.
   - Build a `[]*template.Template` slice indexed by column position (`nil` = passthrough).
   - **Strict check:** every column in the rule must exist in the dump's columns; every table in the rule must have a `<schema>@<table>.json` in the dump. Otherwise fatal — stale config is the #1 way an anonymizer silently leaks PII.
7. Verify or create `--out`. Refuse if non-empty.

### Copy pass

8. Hardlink-with-fallback-to-copy every file in the manifest into `--out`, *except*: chunks of configured tables and their `.idx` sidecars. Those are written fresh by workers.

### Worker pool

9. Job queue: one job per `(table, chunkIdx)` of a configured table. Workers = `runtime.NumCPU()` (override `-j N`). The pool size is bounded; a single goroutine processes many jobs sequentially over its lifetime.
10. **Per job** (not per worker goroutine — workers are reused), the worker:
    - Derives `seedHi, seedLo := splitMix(jobSeed, fnv64(tableName), uint64(chunkIdx))` (or any two-`uint64` mixing of the inputs; the exact mixer is an implementation detail, but the inputs and output count are fixed by this design).
    - Constructs `src := rand.NewPCG(seedHi, seedLo)` from `math/rand/v2`. `*PCG` implements `rand.Source`.
    - Constructs `f := faker.New(src)`.
    - Obtains a per-job set of executable templates bound to `f.FuncMap()` (see "Config compile model" below).
11. Per chunk, the worker:
    - Opens `<chunk>.tsv.zst`, wraps in zstd decoder, wraps in `tsv.Reader`.
    - Opens `<chunk>.tsv.zst.tmp`, wraps in zstd encoder, wraps in `tsv.Writer`. Opens `<chunk>.tsv.zst.idx.tmp` alongside.
    - For each row: reader yields `[][]byte` cells (valid until next row). For each cell:
      - If its rule slot is `nil`, write the original bytes through *unchanged* (preserves any pre-existing escaping byte-for-byte — this is the byte-faithful-roundtrip property).
      - Otherwise execute the template, get a `string`. If the result is exactly `faker.SentinelNULL`, write the TSV NULL token (`\N`). Otherwise escape the string and write.
    - The writer emits the row terminator. The new decoded-byte offset is recorded for the regenerated `.idx`.
    - On chunk done: flush, fsync, rename `.tmp`→final for both files. The output dir never contains a half-written chunk.
12. On any worker error: cancel the shared `context.Context`. Other workers stop mid-row, abandon their `.tmp` files (cleaned up via `defer`). Process exits non-zero. `@.done.json` is not copied.

### Finalization

13. After the pool drains successfully, copy the dump's `@.done.json` into `--out`. **`@.done.json` is explicitly excluded from the step-8 copy pass** so it lands in the output only via this final step. Downstream tools that gate on its presence therefore correctly refuse a partial output dir.

### Config compile model

The compiled templates in `internal/config` close over a specific `*Faker.FuncMap()` at parse time. Workers need their own RNG-backed templates, so the per-job compile in step 10 happens via one of two equivalent mechanisms — the implementation may pick either, both are within the spec:

- **(re-load)** Each worker, per job, calls `config.Load(path, workerFaker)`. Re-reads YAML, re-parses templates, ~50 templates × jobs total. Negligible — submillisecond per call. Wasteful on paper, free in practice.
- **(clone)** `config.Load` is split: parse-once produces a `RawConfig` (or a tree of `*template.Template` parsed against a *placeholder* funcmap for syntax validation only). Per job, each template is `Clone()`'d and `Funcs()`-rebound to the worker's funcmap before `Execute`. Standard `text/template` idiom, no re-parse.

Either works. The implementation plan picks one; the design only requires that *each job* has templates bound to *its* worker's RNG.

### Sentinel for SQL NULL

`{{ null }}` must produce a value that the cell writer can detect as "emit `\N`" without colliding with any normal substitution output. Two-part guard:

- The sentinel is a string that cannot appear in any faker function's output: it includes embedded NUL bytes and is sufficiently long that incidental collision is impossible. (Concrete value chosen at implementation; e.g. `"\x00\x00mysql-anonymizer-NULL\x00\x00"`.)
- The cell writer compares its template-execution result to the sentinel **for exact equality**. If the result is exactly the sentinel → write `\N`. If the result *contains* the sentinel as a substring but isn't equal — that's a config error (someone wrote `prefix{{ null }}` or composed `null` with another expression). Worker reports it, cancels context. Fatal.

This makes `{{ null }}` structurally usable only as the entire template output, as documented, with a runtime check that catches misuse instead of silently producing literal sentinel bytes in the data.

## Error handling

| When | Class | Handling |
|---|---|---|
| Startup | Config syntax (template parse), unreadable files, missing `--config` | Print error, exit non-zero. No output dir created. |
| Startup | Stale config (table/column referenced in config but absent in dump), unsupported compression, `--out` non-empty, missing `@.done.json` in input | Print error, exit non-zero. No output dir created. |
| Mid-stream | TSV cell-count mismatch (row has wrong number of cells for column list) | Worker reports error, cancels shared context. Cleanup `.tmp` files on exit. |
| Mid-stream | Template execution error (currently impossible with the built-in funcs, but defensive) | Same. |
| Mid-stream | I/O error (read/write/zstd codec/fsync) | Same. |
| Mid-stream | Template result equals/contains the NULL sentinel except as exact-match | Same — config misuse. |
| Any | SIGINT / SIGTERM | `cmd/` installs a signal handler that cancels the shared context, same path as a worker error. `.tmp` cleanup runs; no `@.done.json` written. |

A half-failed run leaves `--out` partially populated; the absence of `@.done.json` is the contract that says "do not load this."

## TSV codec (`internal/tsv`)

The mysqlsh dump dialect (default `dumpInstance`):
- Field separator `\t`, record separator `\n`.
- **No field enclosure.** (The `mysqltsv` upstream we lifted from is `LOAD DATA INFILE`-shaped and *always* enclosed in `"..."` — we drop that.)
- Backslash escapes: `\0`, `\b`, `\n`, `\r`, `\t`, `\Z` (`\x1A`), `\\`. NULL is the literal two-byte token `\N`.

The package will be rewritten — the inherited `tsv.go` is a starting point only — keeping the escape table from upstream (which is correct) and discarding the encoder API (which is wrong-shape).

Required properties:

- A streaming reader that yields each row as `[][]byte` cells, reusing internal buffers so no per-row allocation in the hot path.
- A streaming writer that emits a row given a mix of *passthrough cells* (already-escaped bytes from the reader, written verbatim) and *substituted cells* (raw template output, escaped by the writer). This split is what makes byte-identity on untouched cells achievable.
- Backslash-escape table per the dialect above.

The exact API shape is an implementation choice; the byte-identity-on-untouched-cells property is the contract.

The `LICENSE` file from upstream stays in `internal/tsv/` as attribution.

## `.idx` regeneration (`internal/idx`)

mysqlsh's per-chunk `.idx` is a sidecar of decoded-byte offsets, used by `util.loadDump` for parallel sub-chunk loading. We always regenerate it because substituted cells almost always change row byte length.

**Open question, to resolve in implementation:** the exact binary layout. A sequence of fixed-width big-endian offsets is the working assumption, but the precise width (8 bytes? 4? variable-length?) and whether it terminates with a total-length record needs confirmation against a real mysqlsh-produced `.idx`. **Cheapest verification path:** `mysqlsh` against a tiny throwaway database, then `xxd` one chunk's `.idx` and cross-reference against the mysqlsh source (it's open source on Oracle's GitHub mirror). The implementation plan must produce a fixture-driven test that round-trips a known mysqlsh `.idx` byte-identically before any production `.idx` is written by our code.

## Dump metadata assumptions (open questions)

The design relies on the layout of mysqlsh's per-table sidecar JSON, but `util.dumpInstance`'s sidecar format is not a publicly stable spec. Two load-bearing assumptions need fixture verification before the `internal/dump` loader is considered correct:

1. **`<schema>@<table>.json` lists columns in the same order as the chunk TSV cells.** Field name (likely `"columns"` or `"fields"`) and ordering both need confirmation. If wrong, every column mapping silently shifts and the wrong cells get rewritten — a silent PII leak. Fixture: dump a tiny schema, parse the JSON, hand-verify against the chunk's first row.
2. **Top-level `@.json` has a `compression` string field.** If present and equal to `"zstd"`, we proceed; otherwise fatal. If the field is named differently or nested, the strict check is broken until the parser is corrected.

Both should be verified during the same throwaway-mysqlsh-run that pins the `.idx` format. Cheapest to do all three at once.

## Faker (`internal/faker`)

Already reshaped:

- `type Faker struct { gf *gofakeit.Faker }`
- `func New(src rand.Source) *Faker` (rand is `math/rand/v2`)
- `(f *Faker) FuncMap() template.FuncMap`
- `Invoice()` is `"INV-" + f.gf.Password(true,true,true,false,false,8)` — RNG-backed, 8 alphanumeric chars.
- All randomness flows through `f.gf`; no package globals; not safe to share across goroutines.

## Determinism

- Per-job seed derivation: `(seedHi, seedLo)` are mixed from `(jobSeed, tableName, chunkIdx)`. Same job seed + same input dir → byte-identical output dir, run after run.
- This is the audit property. CI can `diff -r clean-1 clean-2` to assert "the cleaner is stable" and, when prod data shifts, the diff is *only* the prod change, not RNG noise.
- The seed is a required CLI flag (no implicit time-based default) so CI configs commit to a value.
- **Determinism dependencies the implementation must respect:**
  - Filesystem walk must be deterministic (lexicographic). Use `filepath.WalkDir` or `os.ReadDir` (both return entries sorted) — *not* an unspecified-order walker.
  - `gofakeit.NewFaker(src, false)` is the only entrypoint to gofakeit; never call package-level `gofakeit.Foo()` (which uses a shared default RNG with hidden global state).
  - Per-worker `*Faker` is never shared across goroutines. The pool is one goroutine per worker.
  - Job dispatch order does not affect output content (only `(table, chunkIdx)` does), so dispatching jobs to workers in any order is fine.

## Testing

### Unit (per package)

- `internal/tsv` — roundtrip property test. Generate random `[][]byte` rows (cells include `\t`, `\n`, `\\`, `\0`, `\Z`, NUL bytes). Encode → decode → assert equal. **First test written.**
- `internal/tsv` — byte-identity test. A committed mysqlsh-format fixture; decoding then re-encoding the same cells must produce the input bytes.
- `internal/idx` — regenerate `.idx` from a known TSV; compare against a mysqlsh-produced fixture.
- `internal/faker` — determinism test. Same seed → identical sequence of outputs across all helpers; different seeds → differ.
- `internal/config` — strict tests: missing column → error, missing table → error, valid config → succeeds.
- `internal/anon` — rule-application tests against a hand-written column list.

### Integration

- `testdata/tiny-dump/` fixture: a small synthetic dump (one schema, two tables, two chunks each, hand-crafted to cover all escape characters and `\N`).
- Test runs the binary against it and asserts:
  - Output dir structure matches input dir structure.
  - Configured columns are replaced.
  - Unconfigured columns are byte-identical.
  - `.idx` files are regenerated correctly.
  - Determinism: run twice with the same seed, `diff -r` is empty.

### Out of test scope (v1)

- Live MySQL → dump → anon → load → diff round-trip. Documented as a manual test until the first format surprise forces an automated version.

## Deployment

Final form is an Oracle Linux-based container image (handled separately near the end of implementation). Implications for the design:

- The binary is a single static-ish Go executable (`go build` produces what we need; cgo not required by any dependency in scope).
- No runtime dependencies on host MySQL clients, mysqlsh, or zstd CLIs — everything is in-process via `klauspost/compress/zstd` and our own TSV codec.
- Filesystem assumptions are POSIX; hardlinks should work on the container's overlay filesystem when input and output paths share the same mount, with the copy fallback handling the cases where they don't.

The image will run on Gitlab CI.

## Operability gaps acknowledged for v1

These are *not* in scope for the first cut, but they're known absences worth naming so they don't get forgotten:

- **Progress reporting.** A multi-hour run gives the operator no signal between start and finish. Adding a `chunks-done / chunks-total` line to stderr periodically is trivial; we punt only because nothing in CI today needs it. Should be revisited the first time someone watches a run for an hour.
- **Logging / metrics.** Errors print to stderr; success is silent. No structured logging, no Prometheus push, no run-stats summary. Fine for CI, insufficient for a daemon — but it's not a daemon.
- **`--resume`.** Per-file atomic writes mean a partial output dir has fully-written chunks we *could* skip on re-run, but we don't. Adding it is mechanical when needed.

## Performance notes

- `text/template` per cell on a 35 GiB table is the suspected hot path. The plan starts there because the code already exists; an early microbenchmark will show whether template execution dominates. If it does, the fix (compile each rule into a `func(*faker.Faker) string` closure list at config-load time) is well-scoped and doesn't change anything else.
- Per-worker memory is small and chunk-size-independent: streaming row-by-row through bufio means the live footprint per worker is one decompressed bufio buffer + one row buffer + one encoder bufio buffer (~MiBs total). With ~16 workers, total peak is tens of MiB. Chunks could be much larger than 64 MiB without changing this.
- Hardlink-with-fallback for unchanged files avoids a multi-GiB copy when the dump and clean dirs share a filesystem.
