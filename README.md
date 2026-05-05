# mysql-anonymizer

Rewrites configured columns in a `mysqlsh util.dumpInstance` directory and
emits a sibling "clean" directory that `util.loadDump` will accept.

## What it does

Given an existing dump directory and a YAML config, the tool produces a new
directory whose layout is byte-equivalent to the input *except* for the
columns named in the config. For each chunk of a configured table:

- Every row is read from the original `.tsv.zst` chunk.
- Cells in **un**configured columns are written back verbatim, preserving the
  original on-disk escape bytes (byte-identity for passthrough cells).
- Cells in configured columns are replaced by the output of a Go
  `text/template` evaluated against a fake-data func map.
- A fresh `.tsv.zst.idx` sidecar is written (8-byte big-endian uncompressed
  length).

Files for tables that are not mentioned in the config are hardlinked
(falling back to copy on cross-device errors) into the output directory
unchanged. `@.done.json` is copied last, so an interrupted run never
publishes a directory that looks complete.

Output is deterministic for a fixed `--seed` regardless of `-j`: each
`(table, chunk_index)` pair derives its own RNG seed, so worker scheduling
does not affect bytes.

## Expected environment / external dependencies

- A `mysqlsh util.dumpInstance` directory at format version `2.x`. Only zstd
  compression is supported; the run aborts if any per-table sidecar declares
  another `compression` value, including for tables not referenced by the
  config.
- The dump must contain `@.done.json` (the run-complete marker mysqlsh writes
  last). Incomplete dumps are rejected.
- No database connection is opened at any point, the tool only reads files on disk.

## Building

`go build ./cmd/mysql-anonymizer`

## Usage

```
mysql-anonymizer \
    --in   <dump-dir>    \
    --out  <clean-dir>   \
    -c     <config.yaml> \
    --seed <uint64>      \
    [-j    <workers>]
```

All four of `--in`, `--out`, `-c`, `--seed` are required (there is no
implicit seed default — that is intentional, so reruns are explicit). `-j`
defaults to `runtime.NumCPU()`. `--out` must be missing or empty.

`SIGINT` / `SIGTERM` cancel the run; in-flight chunks abort and their
`.tmp` files are removed, so the output directory is never left in a
"looks complete but isn't" state.

Exit codes: `0` success, `1` runtime error, `2` flag parse error.

## Config format

YAML, one top-level `filters` map keyed by table name. Each entry has a
`columns` map keyed by column name, and each column has a Go template
string in `value`:

```yaml
filters:
  users:
    columns:
      email:
        value: "{{ fakerEmail }}"
      name:
        value: "{{ fakerFirstName }} {{ fakerLastName }}"
      ssn:
        value: "{{ null }}"
      ref:
        value: "{{ randAlphaNum 10 | upper }}"
  orders:
    columns:
      iban:
        value: "{{ fakerIBAN }}"
```

Fields:

- `filters.<table>` — the bare table name (no schema prefix). The dump's
  manifest key is `<schema>@<table>`; the config matches on the table part.
  An ambiguous match across schemas is an error.
- `filters.<table>.columns.<column>.value` — a `text/template` string. It
  is parsed once at startup against a bootstrap faker (so syntax errors are
  caught before any chunk is touched) and re-parsed per worker against that
  worker's own RNG.

Column names not present in the dump's per-table sidecar (`<schema>@<table>.json`,
`options.columns`) are an error.

### Template functions

| name | output |
| --- | --- |
| `fakerName`, `fakerFirstName`, `fakerLastName` | person names |
| `fakerEmail`, `fakerPhone` | contact |
| `fakerAddress`, `fakerStreetAddress`, `fakerSecondaryAddress`, `fakerCity`, `fakerPostcode` | address parts |
| `fakerCompany` | company name |
| `fakerIBAN`, `fakerSwift`, `fakerEIN` | shape-only fake IBAN / BIC / EIN |
| `fakerInvoice` | `INV-XXXXXXXX` |
| `uuidv4` | UUIDv4 string |
| `randAlphaNum n`, `randNumeric n` | random strings of length `n` |
| `upper`, `lower` | string-pipe helpers |
| `null` | sentinel — the cell is emitted as SQL NULL (`\N`) |

`{{ null }}` must appear alone in the template output. If the sentinel
appears as a substring of a longer output, the run aborts (otherwise an
accidental concatenation would silently emit NULL).

## Sample CI usage

End-to-end pipeline that anonymizes a dump in CI before publishing it as
an artifact:

```sh
# 1. produce the dump from a throwaway DB
mysqlsh --uri="$DB_URI" -- util dump-instance ./dump

# 2. rewrite configured columns
mysql-anonymizer \
    --in   ./dump \
    --out  ./dump-clean \
    -c     ./anonymizer.yaml \
    --seed "$CI_PIPELINE_ID" \
    -j     4

# 3. (optionally) re-load the clean dump to verify
mysqlsh --uri="$VERIFY_DB_URI" -- util load-dump ./dump-clean

# 4. publish ./dump-clean as the artifact
```

Tying `--seed` to a stable build identifier (commit SHA, pipeline ID) makes
the output reproducible across reruns of the same CI job.

## How it works internally

```
cmd/mysql-anonymizer/
    main.go        flag parsing + run() pipeline
    validate.go    config × manifest cross-check
    copy.go        passthrough hardlink/copy pass
    pool.go        worker pool, per-chunk seed derivation, atomic rename
internal/
    config/        YAML load + template Compile against a Faker FuncMap
    dump/          manifest walker, @.json + per-table sidecar parsing
    tsv/           reader/writer for the mysqlsh TSV dialect
    zstd/          klauspost/compress thin wrapper
    idx/           8-byte big-endian uncompressed-length sidecar writer
    faker/         per-worker gofakeit wrapper + template FuncMap
    anon/          row loop: read → per-cell template-or-passthrough → write
```

Pipeline (`run` in `main.go`):

1. **Walk manifest.** `dump.WalkManifest` classifies every file in `--in`
   into instance metadata, per-table metadata, chunks (regex
   `^(.+?)(@@|@)(\d+)\.tsv\.zst$`), `.idx` sidecars, and a passthrough list.
   Refuses to proceed if `@.done.json` is missing.
2. **Sanity-parse `@.json`.** Asserts `version` starts with `2.`.
3. **Bootstrap-compile config.** Templates are parsed once against a
   throwaway Faker so syntax errors fail fast.
4. **Strict validate.** Every per-table sidecar is parsed; any non-zstd
   compression aborts the run (even for unconfigured tables — a defensive
   check). Configured tables must exist unambiguously and every configured
   column name must appear in `options.columns`.
5. **Refuse non-empty output.** `--out` must not exist or be empty.
6. **Passthrough pass.** Every file in the manifest's passthrough list is
   hardlinked into `--out`, except chunks and `.idx` sidecars of
   configured tables (those are produced in step 8). `@.done.json` is
   excluded from this pass and handled in step 9.
7. **Build job list.** One job per `(configured_table, chunk)` pair.
8. **Worker pool.** `nWorkers` goroutines drain a job channel. Each job:
   1. Derives `(hi, lo)` PCG seed from `fnv1a(jobSeed || tableKey || chunkIdx)`,
      so output bytes are independent of scheduling order.
   2. Compiles its own copy of the templates against a per-worker Faker
      (gofakeit is not goroutine-safe).
   3. Streams `chunk.tsv.zst` → zstd decoder → `tsv.Reader` → `anon.ProcessAll`
      → `tsv.Writer` → zstd encoder → `chunk.tsv.zst.tmp`, with a parallel
      `chunk.tsv.zst.idx.tmp` written from `tsv.Writer.BytesWritten()`.
   4. `os.Rename`s both `.tmp` files into place. On any error the `.tmp`
      files are removed, so the output never contains a partial chunk.
9. **Finalize.** `@.done.json` is hardlinked last.

The TSV reader returns each cell as a slice of the *raw escaped bytes*
(no decode). Passthrough cells are written verbatim — no
decode/re-encode round-trip — which is what enforces byte-identity. Only
substituted cells are escaped fresh via the table in `internal/tsv/escape.go`
(derived from `github.com/hexon/mysqltsv`, BSD-2-Clause; license preserved
in that directory).

The `.idx` format was reverse-engineered from a real mysqlsh dump: it is
a single 8-byte big-endian uint64 = total decompressed length of the
sibling chunk. Notes are in `testdata/fixtures/notes.md`.
