# mysqlsh `util.dumpInstance` fixture notes

Captured 2026-05-04 against `mysql:8` (server reported as 8.4.9) using
`mysqlsh` 9.7.0 on macOS arm64.

## Connection / capture

- Container: `docker run --rm -d --name mysql-fixture -e MYSQL_ROOT_PASSWORD=test -e MYSQL_DATABASE=fx -p 13306:3306 mysql:8`
- Connection string that worked: `root:test@127.0.0.1:13306` (the macOS host's
  published port). The `mysql-fixture.orb.local` DNS suggested in the plan was
  not used; the published port via `127.0.0.1` was simpler and worked.
- Dump command: `mysqlsh --uri=root:test@127.0.0.1:13306 -- util dump-instance /tmp/fxdump --threads=1 --bytesPerChunk=128k`
- `bytesPerChunk` minimum is `128k` — `mysqlsh` rejects anything smaller with
  `ArgumentError: Argument #2: The value of 'bytesPerChunk' option must be greater than or equal to 128k`. Forcing multi-chunk required a separate ~200 KiB
  table (`big`, 200 rows of 1000 bytes), which produced two chunks named
  `fx@big@0.tsv.zst` (non-final) and `fx@big@@1.tsv.zst` (final).

## Fixture contents (`testdata/fixtures/`)

| file              | source                            |
| ----------------- | --------------------------------- |
| `sample-at.json`  | `/tmp/fxdump/@.json`              |
| `sample-table.json` | `/tmp/fxdump/fx@t.json`         |
| `sample.idx`      | `/tmp/fxdump/fx@t@@0.tsv.zst.idx` |
| `sample.tsv`      | `zstd -d` of the chunk            |
| `sample.tsv.zst`  | `/tmp/fxdump/fx@t@@0.tsv.zst`     |

The schema/table is `fx.t` with columns `(id INT PK, name VARCHAR(64), email
VARCHAR(64))`, 10 rows. Rows 4–9 each contain one of `\t \n \\ NUL \r SUB`
inside `name` to exercise escapes; row 10 has `name = NULL`.

## Open question 1 — `.idx` binary format

**Working hypothesis from the spec was WRONG.** The `.idx` file is **not**
one offset per row.

Actual layout: a single 8-byte big-endian unsigned integer giving the **total
uncompressed byte size of the corresponding `.zst` chunk**. No header,
no trailer, no per-row entries. File length is exactly 8 bytes per chunk.

Evidence:

- `sample.idx` (one-chunk dump, 10 rows, 183 bytes uncompressed):
  `00 00 00 00 00 00 00 b7` → `0xb7` = 183. Decompressed TSV is exactly
  183 bytes. No further bytes in the file.
- Multi-chunk run on the `big` table: each `.idx` is 8 bytes:
  - chunk `@0` (non-final): `00 00 00 00 00 01 88 28` → `0x18828` = 100392; `zstd -d | wc -c` → 100392.
  - chunk `@@1` (final):     `00 00 00 00 00 01 88 94` → `0x18894` = 100500; matches `zstd -d | wc -c`.

So the `.idx` is purely a sidecar holding the decompressed length of its
sibling `.zst` chunk (cheap "uncompressed bytes written" without
re-decompressing). Random row access is **not** supported by this index;
consumers that need row offsets must scan the decompressed stream.

Filename suffix convention observed: `<basename>@<n>.tsv.zst` for non-final
chunks and `<basename>@@<n>.tsv.zst` for the final chunk in a chunked dump.
For tables that fit in one chunk (`fx@t`), the lone chunk is `@@0` (i.e. it
is also the final chunk).

## Open question 2 — per-table column listing

Path: `options.columns` (JSON array of strings, in physical column order
matching TSV cell order).

`sample-table.json`:

```json
{
  "options": {
    "schema": "fx",
    "table": "t",
    "columns": ["id", "name", "email"],
    "fieldsTerminatedBy": "\t",
    "fieldsEnclosedBy": "",
    "fieldsOptionallyEnclosed": false,
    "fieldsEscapedBy": "\\",
    "linesTerminatedBy": "\n",
    ...
  },
  "extension": "tsv.zst",
  "chunking": true,
  "compression": "zstd",
  "primaryIndex": ["id"],
  ...
}
```

The TSV in `sample.tsv` has cells in `id, name, email` order, confirming the
array order matches the wire format.

Bonus: the per-table JSON also explicitly states the field/row terminators
(`fieldsTerminatedBy`, `linesTerminatedBy`, `fieldsEscapedBy`) and the
`extension` (`tsv.zst`). A robust loader should read these instead of
hard-coding TSV — they could in principle be different.

## Open question 3 — `compression` field

**Surprise: it is NOT in `@.json`.** The instance-level `@.json` does not
contain any `compression` key (only `binlog_transaction_compression` etc.
under `source.sysvars`, which are unrelated server variables).

Compression metadata lives in the **per-table** JSON: top-level
`.compression` in `fx@t.json` (path: `compression`). For our dump the
value was `"zstd"`. Per-table extension `.extension` (`"tsv.zst"`) is a
secondary corroborating signal.

Implication: the anonymizer needs to read `<schema>@<table>.json` to learn
the compression algorithm; it can't infer it from `@.json` alone.

## Escape table observed in TSV

Quoting rules (from `sample.tsv` hex dump, with
`fieldsEscapedBy="\\"`, `fieldsTerminatedBy="\t"`, `linesTerminatedBy="\n"`,
`fieldsEnclosedBy=""`):

| input byte | hex   | rendered as  | hex on disk |
| ---------- | ----- | ------------ | ----------- |
| TAB        | 0x09  | `\t`         | `5C 74`     |
| LF         | 0x0A  | `\n`         | `5C 6E`     |
| CR         | 0x0D  | `\r`         | `5C 72`     |
| SUB        | 0x1A  | `\Z`         | `5C 5A`     |
| NUL        | 0x00  | `\0`         | `5C 30`     |
| backslash  | 0x5C  | `\\`         | `5C 5C`     |
| NULL value | (n/a) | `\N`         | `5C 4E`     |

Not observed in this fixture (would have to be tested separately if relevant):
`\b` (0x08 → `\b`?), and the behaviour around any non-ASCII / multi-byte
UTF-8 (passes through unescaped — confirmed for ASCII, presumably the same
for utf8mb4 since charset is `utf8mb4`).

This matches the spec's working escape list exactly with one minor note:
`SUB` (0x1A) is escaped as `\Z` (capital), so case matters for the decoder.
No surprises beyond the spec's list; `\b` was simply not exercised here.

## Reproducibility

The `mysqlsh` version, server version, and the exact `bytesPerChunk=128k`
flag are all worth re-checking if the format changes in a future release.
Format discriminator candidates:

- `@.json` `version` field: `"2.0.1"`
- `@.json` `dumper` field: `"mysqlsh Ver 9.7.0 ..."`

A loader should at minimum assert `version` starts with `"2."` before
trusting the layout described above.
