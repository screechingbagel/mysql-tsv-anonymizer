# MySQL Anonymizer — Post-Review Cleanup

**Date:** 2026-05-05
**Status:** approved (brainstorm), ready for implementation plan
**Builds on:** `2026-05-03-mysql-anonymizer-design.md`

## Purpose

The mysql-anonymizer implementation passes a full code review against its design spec on every load-bearing correctness property — byte-identity passthrough, NULL-sentinel guard, determinism, `.idx` format, atomic chunk writes, `@.done.json` ordering. While drafting this cleanup spec a follow-up question surfaced one **live blocker** (A0 below): unconfigured-table data chunks are silently dropped from the output. The remaining work is the originally-planned cleanup: small spec divergences with latent risk, integration-test gaps that would let a future regression slip past CI, and dead code accreted during implementation.

The integration test masked A0 because its fixture has exactly one table and that table is configured. Any real dump with both configured and unconfigured tables produces output where the unconfigured tables' sidecar JSON/SQL are present but their `.tsv.zst` and `.tsv.zst.idx` files are missing — `util.loadDump` would either fail noisily or load empty tables.

This spec covers A0 plus the original cleanup pass. Nothing else.

## Scope

Three clusters: **A** spec divergences, **B** test gaps, **C** dead code. Item set **D** from the review (FNV→splitmix64, `--config` long flag, `linkOrCopy` EEXIST handling, bufio sizes, per-chunk template re-compile) is explicitly out.

## A. Spec divergences

### A0. Unconfigured-table chunks are dropped from the output (live blocker)

**Where:** `internal/dump/manifest.go` (the chunk and `.idx` classification branches around lines 82-100).

**Problem:** When `WalkManifest` matches a chunk filename against `chunkRE`, it appends a `ChunkEntry` to `m.Tables[tableKey].Chunks` but never adds the path to `m.PassthroughFiles`. The branch at line 98 (`strings.HasSuffix(name, ".tsv.zst.idx")`) explicitly `continue`s without recording the `.idx` either. As a result, `m.PassthroughFiles` contains zero `.tsv.zst` or `.tsv.zst.idx` paths.

The copy pass in `cmd/mysql-anonymizer/copy.go:37-43` iterates `m.PassthroughFiles` and applies a filter that excludes chunks of *configured* tables (since the worker pool will write those fresh). That filter is currently a no-op, masking the real behavior: **all chunks of unconfigured tables are silently dropped from the output dir.** Their per-table `.json` and `.sql` sidecars are passed through (lines 101-110), so the directory looks structurally complete to a casual inspection — but the actual row data for unconfigured tables is gone.

`@.done.json` still gets written at the end, so downstream tools have no way to detect the missing data short of `util.loadDump` failing or — worse — silently loading empty tables.

The integration test did not catch this because its fixture defines exactly one table and that table is configured. A dump with two-or-more tables, only some of which are configured, produces broken output.

**Fix:** Two small adjustments to `WalkManifest`:

1. After classifying a chunk via `chunkRE`, also append `full` (the `.tsv.zst` path) to `m.PassthroughFiles`. The orchestrator's existing filter in `PreparePassthrough` will exclude configured-table chunks correctly.
2. For the `.tsv.zst.idx` branch (line 98), also append the path to `m.PassthroughFiles` instead of just `continue`-ing. The orchestrator's filter likewise excludes configured-table `.idx` paths.

The orchestrator side (`PreparePassthrough`) already builds `configuredChunkData` and `configuredChunkIdx` sets and skips matching paths — that logic is correct and stays as-is. It was just being run against an incomplete input list.

After the fix: chunks of unconfigured tables hardlink straight through; chunks of configured tables stay out of `PreparePassthrough` and get rewritten by workers. Net behavior matches the design's step-8 intent ("hardlink every file *except* chunks of configured tables").

**Test:**

1. **Manifest-level:** in `internal/dump/dump_test.go`, extend the existing fixture (or add a new one) to include a second table's chunks (`fx@orders@@0.tsv.zst` plus `.idx`). Assert both paths appear in `m.PassthroughFiles`. Assert the configured-table chunks also appear (they were missing before too).
2. **End-to-end:** in `cmd/mysql-anonymizer/integration_test.go`, extend `buildTinyDump` to include a second table that is *not* in the config. Assert that after the run, the unconfigured table's `.tsv.zst` and `.tsv.zst.idx` files are present in `outDir` and byte-identical to the input. This is independent of the B1 fixture upgrade and lands together with A0.

**Risk:** This is the highest-risk change in the cleanup pass — it's the only one that fixes a live correctness bug, and it touches the seam where the orchestrator's filter intersects the manifest's path list. The mitigation is the test pair above, which pins both the manifest contents and the end-to-end behavior.

### A1. Manifest phantom-table classification

**Where:** `internal/dump/manifest.go` (around the file-classification switch).

**Problem:** Files `@.post.sql` and `@.users.sql` are listed in the design as top-level dump files, but the current switch only handles `@.done.json`, `@.json`, and `@.sql`. They fall through to the per-table sidecar branch, which classifies them by the presence of `@` in the name and creates phantom `Tables["@.post"]` / `Tables["@.users"]` entries with empty `MetaPath` and zero chunks. Bytes still copy correctly via `PassthroughFiles`, so this is not a live bug — but the polluted map is confusing and would silently collide if a real config table key ever began with `.`.

**Fix:** Either (a) add explicit cases for `@.post.sql` and `@.users.sql` in the top-level switch, or (b) gate the per-table-sidecar branch on `!strings.HasPrefix(name, "@.")`. Pick whichever reads more cleanly in context; (b) is more robust to mysqlsh adding new top-level `@.<thing>.sql` files in the future.

**Test:** Extend `internal/dump/dump_test.go` with a fixture that includes `@.post.sql` and `@.users.sql`. After walking, assert `m.Tables` is empty (or contains only the legitimate per-table entries) and that both files are present in `PassthroughFiles`.

### A2. Broaden the compression strict-check to all tables

**Where:** `cmd/mysql-anonymizer/validate.go`.

**Problem:** The current check enforces `compression == "zstd"` only for tables referenced by config. Unconfigured tables in any other codec would slip through as opaque hardlinked bytes. This is technically safe in v1 because we never decode unconfigured chunks — but it weakens the contract the spec describes ("fail fast if the dump uses a compression we can't handle") and would surface as a runtime decode error if the codec path ever gets extended.

**Fix:** Walk every entry in `manifest.Tables` during validation. Open and parse each table's `MetaPath` once. Assert `compression == "zstd"` for all of them. Cache parsed metadata in a map keyed by manifest-table-key so the existing config-driven loop reuses the parse instead of re-reading. The cost is bounded by table count: hundreds of small JSON files at startup, ~tens of milliseconds.

**Test:** Add a `validate_test.go` (or extend the existing one) with a manifest fixture that contains a non-`zstd` `compression` value on an unconfigured table. Assert validation fails with a clear error naming the offending table.

### A3. Assert dump-format version prefix

**Where:** `cmd/mysql-anonymizer/main.go` (or wherever `ReadInstanceMeta` is called from `run`).

**Problem:** `internal/dump/meta.go`'s doc comment notes `Version` "must start with `2.`", but no caller asserts it. A future mysqlsh bump to `3.x` could silently change layout invariants the loader relies on.

**Fix:** After `ReadInstanceMeta` returns, check `strings.HasPrefix(meta.Version, "2.")` and fail otherwise. Error message names the version found and notes 2.x is the supported family.

**Test:** Unit test on the validation helper (or an integration-style test driving it through `run`) with a fixture `@.json` containing `version: "3.0"`. Assert validation fails.

## B. Integration-test gaps

These are not live bugs; they are missing nets. Each one closes a specific path through which a future regression could ship undetected.

### B1. Richer integration fixture

**Where:** `cmd/mysql-anonymizer/integration_test.go`'s `buildTinyDump`.

**Three changes:**

1. **Mixed chunk-suffix patterns.** Currently both chunks are written as `fx@users@@<n>.tsv.zst` (final-chunk pattern). Real mysqlsh dumps use `@<n>` for non-final chunks. Rewrite chunk 0 as `fx@users@0.tsv.zst` (non-final, single `@`) and chunk 1 as `fx@users@@1.tsv.zst` (final, double `@@`). The manifest walker already handles both patterns at unit level — this exercises them through the full pipeline.

2. **Escape-character coverage.** Currently the synthetic rows are plain ASCII. Add an unconfigured column whose values include tab (`\t`), newline (`\n`), backslash (`\\`), NUL (`\0`), `\Z` (0x1A), `\b`, multi-byte UTF-8, and a row whose cell value is exactly the `\N` NULL token. The data must round-trip byte-identical through zstd → tsv.Reader → passthrough → tsv.Writer → zstd.

3. **Explicit byte-identity assertion.** Add an assertion that compares the bytes of unconfigured-column cells in the output chunks to the bytes of the same cells in the input chunks. Today's `TestEndToEnd` checks logical content but not bytes — a regression that re-escaped passthrough cells would slip past it.

### B2. `.idx` end-to-end verification

**Where:** `TestEndToEnd` in `cmd/mysql-anonymizer/integration_test.go`.

**Problem:** `.idx` correctness is unit-tested in `internal/idx/idx_test.go`, and end-to-end stability is verified by `TestEndToEnd_Determinism` (same seed → same `.idx`). But no e2e test asserts the `.idx` written by the worker pipeline contains the *correct* value — i.e., the actual decompressed length of its sibling `.zst`.

**Fix:** For each rewritten chunk in `TestEndToEnd`, read the sibling `.idx`, parse 8 BE bytes as `uint64`, decompress the `.zst`, assert `len(decompressed) == idxValue`.

### B3. Different-seeds-differ test

**Where:** `cmd/mysql-anonymizer/integration_test.go`.

**Problem:** Determinism is tested in one direction only (same seed → same output). A regression that accidentally pinned the RNG (e.g., shared faker state, or a constant seed leaking in) would still pass `TestEndToEnd_Determinism`.

**Fix:** Add `TestEndToEnd_DifferentSeeds`: run the binary twice against the same fixture with two distinct seeds; assert at least one substituted-cell value differs between the two output chunks. (Phrase the assertion in terms of cells the config touches, since unconfigured cells must not differ.)

### B4. Context-cancel cleanup test

**Where:** new test in `cmd/mysql-anonymizer/`.

**Problem:** The spec's contract is that worker errors / signal cancellation cancel the shared context, abandon `.tmp` files, and exit without writing `@.done.json`. The signal path is awkward to drive in Go tests, but the underlying mechanism — context cancellation — is not. Today nothing exercises that path.

**Fix:** Drive the worker pool with a context that's cancelled mid-run (e.g., a fixture with multiple chunks plus a goroutine that calls `cancel()` after the first job completes, or after a deterministic sync point). Assert: (a) no `.tmp` files remain in the output dir; (b) `@.done.json` is absent; (c) the run exits non-zero. This is a unit-level check on the cleanup contract, not a true SIGINT integration test.

Real SIGINT testing is explicitly deferred — the spec already lists operability/signal handling as v1-acknowledged gaps.

## C. Dead-code cleanup

These are inert artifacts of earlier iterations. Each removal is mechanical; the only risk is the `tableSchema` collapse (C2/C3) needing care at the validate↔pool seam.

### C1. Drop unused `f *faker.Faker` parameter

**Where:** `internal/anon/anon.go` — `ProcessAll` and any sibling functions taking `f *faker.Faker` only to discard it via `_ = f`.

**Why dead:** Templates close over `f.FuncMap()` at compile time. By the time a row is processed, the faker is already wired into the templates and the parameter is purely decorative.

**Fix:** Remove the parameter. Update all call sites (`pool.go` is the primary one).

### C2. Drop `tableSchema.ColIndex`

**Where:** `cmd/mysql-anonymizer/validate.go`.

**Why dead:** `ColIndex` (a `map[string]int` from column name to position) is built on every validated table but never read — the only consumer iterates `Columns` by position. Premature generalization.

**Fix:** Remove `ColIndex`. If `tableSchema` collapses to a single `[]string` (column-names) field plus the rule slice that lives elsewhere, inline it at callers and delete the type.

### C3. Consolidate `tablePart` lookup

**Where:** `cmd/mysql-anonymizer/validate.go` and `cmd/mysql-anonymizer/pool.go`.

**Why dead-ish:** Both files independently call `tablePart(<manifest-key>)` to translate a `<schema>@<table>` manifest key to the `<table>` config key. If the convention ever changes, two places must change in lockstep — the kind of drift hazard that's free to fix now.

**Fix:** Have `Validate` return a structure that captures the per-table mapping it has already computed (manifest-key → compiled rule slice, in physical column order). `processChunk` looks up its rule slice by manifest key directly, never calling `tablePart`. Single source of truth.

The exact shape of the returned structure is an implementation detail; what the design fixes is "validate already knows the manifest↔config mapping, so it should hand that knowledge to the pool instead of re-deriving it."

## Out of scope (explicit)

- All D-cluster items: FNV→splitmix64 mixer, `--config` long flag, `linkOrCopy` EEXIST behavior, bufio sizes, per-chunk template re-compile / clone-model migration.
- Real SIGINT/SIGTERM integration tests (operability gap, v1-deferred).
- Any refactoring not listed under A/B/C above.

## Risks

- **A0** is a live correctness bug; the fix changes the contents of `m.PassthroughFiles`, which is consumed by `PreparePassthrough`. The orchestrator-side filter logic is unchanged, but the input set it filters over expands. Mitigated by paired manifest-level and end-to-end tests on both configured and unconfigured tables.
- **A2** broadens validation — a dump that previously passed because its non-zstd compression lived on an unconfigured table will now fail. None are known to exist in the current pipeline; the commit message should call this out so anyone hitting it has the context.
- **C3** touches the seam between validation and the worker pool. The risk is silently dropping a rule by mishandling the key translation. Mitigated by: tests in B (which exercise the configured-column-replacement path) plus careful diffing during implementation.

## Testing summary

Each change in **A** lands with a unit-level test that pins the new behavior. **B** is itself a test-coverage section. **C** changes are covered by the existing test suite — they're refactors, so the contract is "tests still pass."

End state: `go test ./...` covers (1) unconfigured-table chunks and `.idx` files passing through to the output dir, (2) every spec divergence's strict check, (3) end-to-end byte-identity through escape characters and `\N` tokens, (4) `.idx` correctness against decompressed length, (5) seed-differs causing output to differ, and (6) context-cancel leaving no `.tmp` files and no `@.done.json`.
