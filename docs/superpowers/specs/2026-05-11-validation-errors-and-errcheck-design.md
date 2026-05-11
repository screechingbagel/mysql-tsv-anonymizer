# Validation error reporting and errcheck cleanup

Date: 2026-05-11

Two independent fixes, bundled into one spec because both are small and live in
`cmd/mysql-anonymizer`. They touch different files (`validate.go` /
`main_test.go` for T1; `pool.go` / `copy.go` / `.golangci.yml` for T2) and can
be implemented in either order.

(A third candidate — folding per-chunk byte sizes into `WalkManifest` to avoid
the `os.Stat` loop in `main.go` — was dropped: it would split a cohesive
operation across two files, add a single-consumer field to a shared struct, and
do *more* I/O, all for an unmeasured startup-latency concern. If that latency is
ever actually observed, the targeted fix is a bounded-parallel stat loop in
`main.go`, done then with a measurement behind it. See "Out of scope".)

## T1 — `Validate()` reports all problems, not the first

### Problem

`cmd/mysql-anonymizer/validate.go`'s `Validate` returns on the first mismatch.
When a config references several columns that aren't in the dump (the common
case after a schema change), the operator fixes one, re-runs, hits the next,
and so on. In the ephemeral CI flow — dump a fresh database, run the anonymizer
against it — the run aborts on the first issue without surfacing the rest.

### Change

`Validate` accumulates problems into a `[]error`, **sorts them by `.Error()`**,
and returns `errors.Join(sorted...)` — which is `nil` when nothing accumulated,
so the success path falls out naturally. `main.go`'s existing
`fmt.Fprintln(os.Stderr, err)` prints one problem per line; the process still
exits 1. Success behavior is unchanged: it returns `map[string]*tableSchema, nil`.

The sort matters: phase 1 ranges over `m.Tables`, phase 2 over `rc.Filters`,
and the column check over `tf.Columns` — all maps, so without the sort the
reported order would be nondeterministic run to run, which is bad for operators
and brittle for any future exact-match test. Sorting the message strings makes
the cross-message order stable.

For full determinism the sort isn't enough on its own — one message builds its
*own content* from a map: `"table %q is ambiguous across schemas: %s"` joins
`matches`, which is collected by ranging `m.Tables`. `matches` must be
`sort.Strings`-ed before the join, so both the listed schemas and that line's
sort position are stable. (`tm.Options.Columns`, used for the `have %v` column
hint, is a JSON array — already ordered. The per-missing-column problems are
one entry each in the accumulator, so the outer sort orders them.)

Problems collected (everything currently fatal in this function), in the same
two phases the function already has:

Phase 1 — the all-tables dump sweep (unchanged in *what* it reads; it still
parses every per-table sidecar, including unconfigured tables, because that is
the only way to validate their `compression`):

- a table that has chunks but no per-table JSON sidecar
- per-table sidecar read failure
- per-table `compression != "zstd"` (this is what `TestValidate_UnconfiguredNonZstd`
  and the `TestEndToEnd_*` chain pin — must keep firing for unconfigured tables)

Phase 2 — per configured table:

- config references a table that isn't in the dump
- config table name is ambiguous across schemas
- a configured table's sidecar is missing or failed to parse in phase 1
- each config column not present in the dump's column list — *all* missing
  columns across *all* configured tables, which is the motivating case

Continuation rules:

- Phase 1 records each table's problem and keeps going to the next table.
- In phase 2, a table that is un-checkable (not found, ambiguous, sidecar
  missing or unreadable, non-zstd) records the per-config problem if it isn't
  already covered by a phase-1 record, then skips *that table's* column checks
  and proceeds to the next config table. Avoid double-reporting: if phase 1
  already recorded the sidecar failure / non-zstd for that table, phase 2 just
  skips it silently.
- A table that is found and well-formed reports *all* of its unknown columns
  before moving on.
- On success the function returns `map[string]*tableSchema, nil` exactly as
  today. On failure it returns `nil, joinedErr`; `main.go` already returns
  immediately on `err != nil`, so no caller change.

### Tests

- `Validate` with a config referencing two unknown columns in one configured
  table plus a second config table that isn't in the dump: the returned error
  mentions all three. (Order is deterministic given the sort, so an exact-string
  assertion is acceptable here, though substring checks are fine too.)
- `Validate` with one configured table whose sidecar says non-zstd *and*
  another configured table with an unknown column: both reported.
- `TestValidate_UnconfiguredNonZstd`, `TestValidate_NonZstdCompression`, and
  the `TestEndToEnd_*` cases still pass unchanged.
- Existing success-path tests still pass unchanged.

## T2 — `golangci-lint run` clean of errcheck

### Problem

`golangci-lint run` reports 27 `errcheck` issues with no config file present.
~19 are in `*_test.go` (`integration_test.go`, `tsv_test.go`, `zstd_test.go`),
4 are `fmt.Fprint*` status writes to stderr in `internal/progress/progress.go`,
and 4 are file `Close` calls in `cmd/mysql-anonymizer/{copy,pool}.go` — two of
which (the output chunk file and idx file in `pool.go`) actually matter because
those files are renamed into place.

### Change

New `.golangci.yml` (golangci-lint v2 schema, matching the locally installed
v2.x):

- exclude `_test.go` files from `errcheck` (covers the test-file hits)
- add `fmt.Fprint`, `fmt.Fprintf`, `fmt.Fprintln` to errcheck's
  `exclude-functions` — a failed write of a status line to stderr is
  unactionable
- otherwise leave defaults; do not blanket-exclude `Close`

Code changes in `cmd/mysql-anonymizer/pool.go` `processChunk` — the write-path
closes get checked, but **not** by feeding the deferred close into the named
return. Deferred closes run at function return, *after* `os.Rename`; a late
close error fed into `err` would then trigger the cleanup defer, which tries to
`os.Remove` the temp paths that no longer exist (they were renamed), so it would
no-op while still reporting failure to `RunPool` — a spurious whole-run abort.
And on a FUSE / object-store mount the close *is* the upload, so it must be
checked before the (copy+delete) rename, not after. So instead:

- Close `outF` and `idxF` explicitly in the happy path, checked, *before* the
  `os.Rename` calls and after their respective `Sync`s — i.e. the tail becomes
  `… zw.Close(); outF.Sync(); outF.Close(); idx.Write(idxF, …); idxF.Sync();
  idxF.Close(); os.Rename(tmpData,…); os.Rename(tmpIdx,…)`, each checked. A
  close error here is caught while the cleanup defer's temp paths are still
  valid, so a failed chunk is cleaned up correctly.
- The up-top `defer outF.Close()` / `defer idxF.Close()` become
  `defer func() { _ = outF.Close() }()` / `defer func() { _ = idxF.Close() }()`
  — now just no-op safety nets for the early-return error paths. Closing an
  already-closed `*os.File` returns `os.ErrClosed`, which the `_ =` discards.
  These stay in their *current declaration position* (after the cleanup-remove
  defer), so LIFO order is unchanged: on an early-return path the safety-net
  close fires before the temp-file removal.
- `defer inF.Close()` and `defer zr.Close()` (read side) become
  `defer func() { _ = inF.Close() }()` / `defer func() { _ = zr.Close() }()` —
  explicit-ignore; a config carve-out for `(*os.File).Close` would also hide
  the write-path ones. (`zr` is a `zstd.ReadCloser` whose `Close` always
  returns `nil`, so ignoring it is harmless either way.)

The zstd *encoder* `zw` is untouched: it's already closed-and-checked once in
the happy path and has no `defer`, so errcheck doesn't flag it. Whether it
*should* have a `defer zw.Close()` for the early-return paths is a separate
missing-call question, out of scope here.

Code change in `cmd/mysql-anonymizer/copy.go`:

- `defer in.Close()` (read side, in `linkOrCopy`) becomes
  `defer func() { _ = in.Close() }()`. (`linkOrCopy`'s existing
  `out.Close()`-into-`retErr` defer is left as-is — that path has no rename and
  is not flagged by errcheck.)

End state: `golangci-lint run` exits clean with no `//nolint` comments. The
test files are excluded from `errcheck` only — other linters still apply to
them.

### Tests

`go test ./...` keeps passing and `golangci-lint run` exits 0. The existing
`TestEndToEnd_*` cases act as a regression guard — they confirm the close
reordering still produces loadable chunk + `.idx` output — but they do *not*
directly exercise the property the reordering buys (a `Close` error caught
before `os.Rename` so the cleanup defer can still remove the temp files);
forcing a `*os.File.Close` failure would need a fault-injection seam that
doesn't exist, and adding one isn't warranted for a change this small and
locally reviewable. No new tests.

## Out of scope

- Adding golangci-lint to GitHub Actions.
- Any change to the progress reporter's model.
- Touching errcheck behavior beyond the three excluded `fmt.Fprint*` functions
  and the `_test.go` path exclusion.
- Making `Validate`'s per-table sidecar reads lazy / config-only. The
  all-tables sweep is an intentional dump-integrity check (`compression` of
  unconfigured tables); it stays.
- Folding per-chunk byte sizes into `WalkManifest` / `ChunkEntry`. The current
  `os.Stat` loop in `main.go` step 7 is fine; the speculative startup-latency
  concern is unmeasured. If it ever materialises, fix it with a bounded-parallel
  stat loop in `main.go` — no struct changes, no extra I/O over today.

## Workflow notes

- `jj` for commits (colocated repo); `go fmt ./...` before `jj describe` /
  `jj squash`.
- `go build ./...`, `go test ./...`, `go vet ./...`, `golangci-lint run` all
  green before finishing.
