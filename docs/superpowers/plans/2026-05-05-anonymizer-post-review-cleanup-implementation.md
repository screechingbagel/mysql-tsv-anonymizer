# MySQL Anonymizer Post-Review Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Apply the 11 cleanup-pass changes from the post-review cleanup spec — A0 (live blocker: unconfigured-table chunks dropped from output), A1/A2/A3 (spec divergences), B1–B4 (integration-test gaps), C1–C3 (dead code) — without changing any of the load-bearing correctness contracts (byte-identity passthrough, NULL-sentinel guard, determinism, `.idx` format, atomic chunk writes, `@.done.json` ordering).

**Architecture:** All changes land in existing files. No new packages. Source-of-truth spec: `docs/superpowers/specs/2026-05-05-anonymizer-post-review-cleanup-design.md`. Tests are written first; commits are per-task.

**Tech Stack:** Go 1.22+, `text/template`, `klauspost/compress/zstd`, `gofakeit` v6, `math/rand/v2` PCG.

**Workflow note:** Repo uses Jujutsu (`jj`), not git directly. Each task ends with `jj describe -m "…"` (which finalizes the current change) followed by `jj new` (which starts a fresh empty change for the next task). Run `go fmt ./...` before describing. The repo is colocated, so `git log` still works for inspection — but never run `git commit`/`git reset`/`git checkout` against this tree.

---

## File Structure

Files that will be modified:

- `internal/dump/manifest.go` — Tasks 1, 3 (A0, A1)
- `internal/dump/dump_test.go` — Tasks 1, 3 (tests for A0, A1)
- `cmd/mysql-anonymizer/main.go` — Task 4 (A3 version assertion in `run`)
- `cmd/mysql-anonymizer/main_test.go` — Tasks 4, 5 (tests for A3, A2)
- `cmd/mysql-anonymizer/validate.go` — Tasks 5, 6, 7 (A2 broaden compression, C2 drop ColIndex, C3 consolidate tablePart)
- `cmd/mysql-anonymizer/pool.go` — Tasks 7, 8 (C3 use new schema field at lines 108-109, C1 drop f param at line 157)
- `internal/anon/anon.go` — Task 8 (C1 drop f param)
- `internal/anon/anon_test.go` — Task 8 (C1 update call sites)
- `cmd/mysql-anonymizer/integration_test.go` — Tasks 2, 9, 10, 11, 12 (A0 e2e, B1, B2, B3, B4)

Files unchanged: everything else (`internal/{tsv,zstd,idx,faker,config}` source code, all other test files).

---

## Task Ordering Rationale

1–2: A0 first — live blocker.
3: A1 — small, mechanical, independent.
4: A3 — small, mechanical, independent.
5: A2 — broaden compression check; settles `validate.go` shape before refactors.
6: C2 — drop `ColIndex`; small refactor of `validate.go`.
7: C3 — consolidate `tablePart`; depends on the now-stable `validate.go`.
8: C1 — drop `f *Faker` param; touches `anon` and `pool`, otherwise independent.
9: B1 — fixture upgrade for escapes + mixed chunk suffixes + byte-identity.
10: B2 — `.idx` e2e verification.
11: B3 — different-seeds-differ test.
12: B4 — context-cancel cleanup test.

---

## Task 1: A0 — Manifest tracks chunk and `.idx` paths in PassthroughFiles

**Files:**
- Modify: `internal/dump/manifest.go:82-100`
- Test: `internal/dump/dump_test.go` (extend `TestWalkManifest_TinyTree`, or add a new test)

**Context:** `WalkManifest` currently classifies chunks (`chunkRE` match) and `.idx` sidecars (`.tsv.zst.idx` suffix) but never appends those paths to `m.PassthroughFiles`. The orchestrator's `PreparePassthrough` filters `m.PassthroughFiles` for configured-table chunks — but since chunks are absent from the input list, the filter is a no-op and unconfigured-table chunks silently disappear from the output. The fix is two `append` calls.

- [ ] **Step 1: Add a failing test that asserts chunk and `.idx` paths land in `PassthroughFiles`**

Append to `internal/dump/dump_test.go`:

```go
func TestWalkManifest_ChunksInPassthrough(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("@.done.json", "{}")
	mustWrite("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
	mustWrite("fx@t.json", `{"options":{"columns":["id"]}}`)
	mustWrite("fx@t@0.tsv.zst", "")
	mustWrite("fx@t@0.tsv.zst.idx", "")
	mustWrite("fx@t@@1.tsv.zst", "")
	mustWrite("fx@t@@1.tsv.zst.idx", "")

	m, err := WalkManifest(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"fx@t@0.tsv.zst":      false,
		"fx@t@0.tsv.zst.idx":  false,
		"fx@t@@1.tsv.zst":     false,
		"fx@t@@1.tsv.zst.idx": false,
	}
	for _, p := range m.PassthroughFiles {
		base := filepath.Base(p)
		if _, ok := want[base]; ok {
			want[base] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("PassthroughFiles missing %s", k)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dump/ -run TestWalkManifest_ChunksInPassthrough -v`
Expected: FAIL with four "PassthroughFiles missing …" messages.

- [ ] **Step 3: Append chunk path to PassthroughFiles after the `chunkRE` match**

In `internal/dump/manifest.go`, inside the `if mm := chunkRE.FindStringSubmatch(name); mm != nil {` block, after the existing `te.Chunks = append(...)` call and before the `continue`, add:

```go
m.PassthroughFiles = append(m.PassthroughFiles, full)
```

The block becomes:

```go
if mm := chunkRE.FindStringSubmatch(name); mm != nil {
	tableKey := mm[1]
	sep := mm[2]
	idx, err := strconv.Atoi(mm[3])
	if err != nil {
		return nil, fmt.Errorf("dump: bad chunk index in %s: %w", name, err)
	}
	te := m.tableEntry(tableKey)
	te.Chunks = append(te.Chunks, ChunkEntry{
		Index:    idx,
		DataPath: full,
		IdxPath:  full + ".idx",
		Final:    sep == "@@",
	})
	m.PassthroughFiles = append(m.PassthroughFiles, full)
	continue
}
```

- [ ] **Step 4: Append `.idx` path to PassthroughFiles in the suffix branch**

In `internal/dump/manifest.go:98-100`, replace:

```go
if strings.HasSuffix(name, ".tsv.zst.idx") {
	continue
}
```

with:

```go
if strings.HasSuffix(name, ".tsv.zst.idx") {
	m.PassthroughFiles = append(m.PassthroughFiles, full)
	continue
}
```

- [ ] **Step 5: Run the new test plus the full dump-package test suite**

Run: `go test ./internal/dump/ -v`
Expected: all tests PASS, including `TestWalkManifest_ChunksInPassthrough` and the existing `TestWalkManifest_TinyTree` and `TestWalkManifest_MissingDoneMarker`.

- [ ] **Step 6: Run the full repo test suite**

Run: `go test ./...`
Expected: all tests PASS (no regression in `cmd/mysql-anonymizer/`'s `TestPreparePassthrough_SkipsConfiguredChunks`, which still passes because the orchestrator filter excludes configured chunks).

- [ ] **Step 7: Format and commit**

```bash
go fmt ./...
jj describe -m "fix(dump): include chunk and .idx paths in PassthroughFiles

Without this, WalkManifest classified .tsv.zst chunks and their .idx
sidecars but never added them to m.PassthroughFiles. Since the copy
pass iterates that list, chunks of unconfigured tables were silently
dropped from the output dir. Fixes A0 in the post-review cleanup spec."
jj new
```

---

## Task 2: A0 — End-to-end test for unconfigured-table passthrough

**Files:**
- Test: `cmd/mysql-anonymizer/integration_test.go` (new test, possibly small helper extension)

**Context:** Task 1 fixed the manifest. This task adds an end-to-end test that locks in the contract: a dump containing an unconfigured table emerges from a full run with that table's chunks and `.idx` files byte-identical in the output. This test runs against the existing `buildTinyDump` (no fixture upgrade — that comes in Task 9) but extends the dump it constructs.

- [ ] **Step 1: Add a helper that augments `buildTinyDump` with a second unconfigured table**

In `cmd/mysql-anonymizer/integration_test.go`, add this function below `buildTinyDump`:

```go
// addUnconfiguredTable extends a built tiny-dump with a second table "orders"
// that the test config does not reference. Used by A0 regression tests.
func addUnconfiguredTable(t *testing.T, dir string) {
	t.Helper()
	mustWrite := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("fx@orders.json", `{"compression":"zstd","extension":"tsv.zst","options":{"columns":["id","amount"],"fieldsTerminatedBy":"\t","fieldsEscapedBy":"\\","linesTerminatedBy":"\n"}}`)
	mustWrite("fx@orders.sql", "")

	var raw bytes.Buffer
	raw.WriteString("100\t9.99\n")
	raw.WriteString("101\t14.50\n")
	var compressed bytes.Buffer
	zw, err := lzstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(zw, &raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fx@orders@@0.tsv.zst"), compressed.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	// .idx is a single 8-byte BE uint64 = decompressed length.
	var idxBuf [8]byte
	binary.BigEndian.PutUint64(idxBuf[:], uint64(raw.Len()))
	if err := os.WriteFile(filepath.Join(dir, "fx@orders@@0.tsv.zst.idx"), idxBuf[:], 0644); err != nil {
		t.Fatal(err)
	}
}
```

Add `"encoding/binary"` to the import block at the top of `integration_test.go` (it's not currently imported).

- [ ] **Step 2: Add a failing test that asserts the unconfigured table passes through byte-identically**

Append to `cmd/mysql-anonymizer/integration_test.go`:

```go
func TestEndToEnd_UnconfiguredTablePassthrough(t *testing.T) {
	inDir := t.TempDir()
	buildTinyDump(t, inDir)
	addUnconfiguredTable(t, inDir)
	cfg := writeConfig(t, t.TempDir())
	outDir := filepath.Join(t.TempDir(), "clean")

	o := opts{InDir: inDir, OutDir: outDir, ConfigPath: cfg, Seed: 42, Workers: 2}
	if err := run(context.Background(), o); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Both the chunk and its .idx must exist and be byte-identical.
	for _, name := range []string{"fx@orders@@0.tsv.zst", "fx@orders@@0.tsv.zst.idx"} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("output missing %s: %v", name, err)
		}
		want, err := os.ReadFile(filepath.Join(inDir, name))
		if err != nil {
			t.Fatalf("input missing %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs (in %d bytes, out %d bytes)", name, len(want), len(got))
		}
	}
}
```

- [ ] **Step 3: Run the new test to verify it now passes (A0 fix from Task 1 carries it)**

Run: `go test ./cmd/mysql-anonymizer/ -run TestEndToEnd_UnconfiguredTablePassthrough -v`
Expected: PASS. (If it fails, revisit Task 1 — the manifest fix did not land.)

- [ ] **Step 4: Run the full repo test suite to confirm no regressions**

Run: `go test ./...`
Expected: all tests PASS.

- [ ] **Step 5: Format and commit**

```bash
go fmt ./...
jj describe -m "test(integration): unconfigured tables pass through byte-identically

Locks in the A0 contract: a dump containing an unconfigured table emerges
with its chunks and .idx files byte-identical in the output. The original
TestEndToEnd fixture had only one configured table and so masked the bug."
jj new
```

---

## Task 3: A1 — Manifest skips `@.<thing>.sql` from per-table sidecar branch

**Files:**
- Modify: `internal/dump/manifest.go:101-110` (the per-table-sidecar branch)
- Test: `internal/dump/dump_test.go` (new test)

**Context:** `@.post.sql` and `@.users.sql` are top-level dump files but the current switch only handles `@.done.json`, `@.json`, `@.sql`. They fall through to the per-table-sidecar branch (`strings.Contains(name, "@") && (… ".sql")`) and create phantom `Tables["@.post"]` / `Tables["@.users"]` entries. Bytes still copy correctly (they get added to PassthroughFiles via that same branch), but the polluted `m.Tables` is confusing and could mask collisions. Fix is to gate the branch on `!strings.HasPrefix(name, "@.")`.

- [ ] **Step 1: Add a failing test asserting `@.post.sql` and `@.users.sql` don't create phantom Tables entries**

Append to `internal/dump/dump_test.go`:

```go
func TestWalkManifest_TopLevelSQLFilesNotPhantomTables(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("@.done.json", "{}")
	mustWrite("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
	mustWrite("@.sql", "")
	mustWrite("@.post.sql", "")
	mustWrite("@.users.sql", "")

	m, err := WalkManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tables) != 0 {
		t.Errorf("Tables should be empty, got: %v", tablesKeys(m.Tables))
	}
	// Both top-level SQL files must still be in PassthroughFiles so they copy through.
	have := map[string]bool{}
	for _, p := range m.PassthroughFiles {
		have[filepath.Base(p)] = true
	}
	for _, want := range []string{"@.post.sql", "@.users.sql"} {
		if !have[want] {
			t.Errorf("PassthroughFiles missing %s", want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dump/ -run TestWalkManifest_TopLevelSQLFilesNotPhantomTables -v`
Expected: FAIL with `Tables should be empty, got: [@.post @.users]` (or similar).

- [ ] **Step 3: Gate the per-table-sidecar branch on `!strings.HasPrefix(name, "@.")`**

In `internal/dump/manifest.go`, replace lines 101-110:

```go
if strings.Contains(name, "@") && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".sql")) {
	tableKey := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".sql")
	te := m.tableEntry(tableKey)
	if strings.HasSuffix(name, ".json") {
		te.MetaPath = full
	} else {
		te.SQLPath = full
	}
	m.PassthroughFiles = append(m.PassthroughFiles, full)
	continue
}
```

with:

```go
if !strings.HasPrefix(name, "@.") && strings.Contains(name, "@") && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".sql")) {
	tableKey := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".sql")
	te := m.tableEntry(tableKey)
	if strings.HasSuffix(name, ".json") {
		te.MetaPath = full
	} else {
		te.SQLPath = full
	}
	m.PassthroughFiles = append(m.PassthroughFiles, full)
	continue
}
```

The trailing `m.PassthroughFiles = append(m.PassthroughFiles, full)` at line 112 (the catch-all) handles `@.post.sql` and `@.users.sql` now.

- [ ] **Step 4: Run the new test plus existing dump tests**

Run: `go test ./internal/dump/ -v`
Expected: all tests PASS.

- [ ] **Step 5: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 6: Format and commit**

```bash
go fmt ./...
jj describe -m "fix(dump): don't classify @.<thing>.sql as per-table sidecars

@.post.sql and @.users.sql are top-level dump files but were being
matched by the per-table-sidecar branch, creating phantom Tables[\"@.post\"]
and Tables[\"@.users\"] entries. Gate the branch on !HasPrefix(\"@.\") so
those files fall through to the catch-all PassthroughFiles append.
Fixes A1 in the post-review cleanup spec."
jj new
```

---

## Task 4: A3 — Assert `@.json` version starts with `2.`

**Files:**
- Modify: `cmd/mysql-anonymizer/main.go:89-91` (after `ReadInstanceMeta`)
- Test: `cmd/mysql-anonymizer/main_test.go` (new test)

**Context:** `internal/dump/meta.go` documents that `Version` "must start with `2.`" but no caller checks. Add the check after `ReadInstanceMeta` returns inside `run()`. Error names the version found.

- [ ] **Step 1: Add a failing test that injects `@.json` with `version: "3.0"` and expects the run to fail**

Append to `cmd/mysql-anonymizer/main_test.go`:

```go
func TestRun_RejectsVersion3(t *testing.T) {
	dir := mkTinyDump(t)
	// Overwrite @.json with a 3.x version.
	if err := os.WriteFile(filepath.Join(dir, "@.json"),
		[]byte(`{"version":"3.0.0","dumper":"synthetic"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`filters: {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "out")

	o := opts{InDir: dir, OutDir: outDir, ConfigPath: cfgPath, Seed: 1, Workers: 1}
	err := run(context.Background(), o)
	if err == nil {
		t.Fatal("expected version error, got nil")
	}
	if !strings.Contains(err.Error(), "3.0.0") || !strings.Contains(err.Error(), "version") {
		t.Errorf("expected error mentioning version 3.0.0, got: %v", err)
	}
}
```

The test uses `context` — add it to the imports of `main_test.go` if not already present (currently the file imports `"os"`, `"path/filepath"`, `"strings"`, `"testing"`, plus the config and dump packages; add `"context"`).

- [ ] **Step 2: Run the test to verify it fails (no version check yet)**

Run: `go test ./cmd/mysql-anonymizer/ -run TestRun_RejectsVersion3 -v`
Expected: FAIL — currently `run` proceeds past `ReadInstanceMeta` without checking version.

- [ ] **Step 3: Add the version-prefix assertion in `run()`**

In `cmd/mysql-anonymizer/main.go`, replace:

```go
if _, err := dump.ReadInstanceMeta(manifest.InstanceMetaPath); err != nil {
	return err
}
```

with:

```go
instMeta, err := dump.ReadInstanceMeta(manifest.InstanceMetaPath)
if err != nil {
	return err
}
if !strings.HasPrefix(instMeta.Version, "2.") {
	return fmt.Errorf("dump version %q is not supported (only 2.x is supported)", instMeta.Version)
}
```

Add `"strings"` to `main.go`'s import block.

- [ ] **Step 4: Run the new test plus the full main-package suite**

Run: `go test ./cmd/mysql-anonymizer/ -v`
Expected: all PASS.

- [ ] **Step 5: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 6: Format and commit**

```bash
go fmt ./...
jj describe -m "feat(cmd): reject @.json with non-2.x version

mysqlsh 9.x produces version 2.0.1 dumps; future format bumps could
silently change layout invariants the loader relies on. Fail fast in
run() with an error naming the version found. Fixes A3 in the
post-review cleanup spec."
jj new
```

---

## Task 5: A2 — Broaden the compression strict-check to all tables

**Files:**
- Modify: `cmd/mysql-anonymizer/validate.go:31-89` (Validate)
- Test: `cmd/mysql-anonymizer/main_test.go` (new test)

**Context:** Currently `Validate` only opens `MetaPath` for tables referenced by config, so the `tm.Compression != "zstd"` check fires only for those. An unconfigured table in any other codec slips through. Fix: walk every entry in `manifest.Tables`, parse each `MetaPath`, assert `compression == "zstd"`, cache the parsed metas in a map so the configured-table loop reuses them.

- [ ] **Step 1: Add a failing test with a non-zstd unconfigured table**

Append to `cmd/mysql-anonymizer/main_test.go`:

```go
func TestValidate_UnconfiguredNonZstd(t *testing.T) {
	dir := t.TempDir()
	// users is configured (zstd); orders is unconfigured (none) — must still fail.
	files := map[string]string{
		"@.done.json":             `{}`,
		"@.json":                  `{"version":"2.0.1","dumper":"synthetic"}`,
		"fx.json":                 `{}`,
		"fx.sql":                  ``,
		"fx@users.json":           `{"compression":"zstd","extension":"tsv.zst","options":{"columns":["id","name","email"]}}`,
		"fx@users.sql":            ``,
		"fx@users@@0.tsv.zst":     ``,
		"fx@users@@0.tsv.zst.idx": ``,
		"fx@orders.json":          `{"compression":"none","extension":"tsv","options":{"columns":["id","amount"]}}`,
		"fx@orders.sql":           ``,
		"fx@orders@@0.tsv":        ``,
		"fx@orders@@0.tsv.idx":    ``,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	m, err := dump.WalkManifest(dir)
	if err != nil {
		t.Fatalf("WalkManifest: %v", err)
	}
	rc := mkConfig(t, `
filters:
  users:
    columns:
      email:
        value: "fake@example.com"
`)
	_, err = Validate(rc, m)
	if err == nil {
		t.Fatal("expected non-zstd error for unconfigured table, got nil")
	}
	if !strings.Contains(err.Error(), "zstd") || !strings.Contains(err.Error(), "orders") {
		t.Errorf("expected error mentioning zstd and orders, got: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (currently Validate ignores unconfigured tables)**

Run: `go test ./cmd/mysql-anonymizer/ -run TestValidate_UnconfiguredNonZstd -v`
Expected: FAIL — `Validate` currently returns success because it only checks configured tables.

- [ ] **Step 3: Refactor `Validate` to parse all manifest tables once and assert compression on each**

Replace the body of `Validate` in `cmd/mysql-anonymizer/validate.go` (keep the same signature for now; Task 7 will change it) with:

```go
func Validate(rc *config.RawConfig, m *dump.Manifest) (map[string]*tableSchema, error) {
	// Parse every table's per-table json once, asserting compression.
	metas := make(map[string]*dump.TableMeta, len(m.Tables))
	for tableKey, te := range m.Tables {
		if te.MetaPath == "" {
			// Unconfigured tables with missing sidecar are a dump error too,
			// but only flag them if the dump claims to have chunks for the table.
			if len(te.Chunks) > 0 {
				return nil, fmt.Errorf("validate: table %q has chunks but no per-table json sidecar", tableKey)
			}
			continue
		}
		tm, err := dump.ReadTableMeta(te.MetaPath)
		if err != nil {
			return nil, fmt.Errorf("validate: read meta for table %q: %w", tableKey, err)
		}
		if tm.Compression != "zstd" {
			return nil, fmt.Errorf("validate: table %q has compression %q; only zstd is supported",
				tableKey, tm.Compression)
		}
		metas[tableKey] = tm
	}

	schemas := make(map[string]*tableSchema, len(rc.Filters))
	for tableKey, tf := range rc.Filters {
		var matches []string
		for k := range m.Tables {
			if tablePart(k) == tableKey {
				matches = append(matches, k)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("validate: config references table %q but it is not in the dump", tableKey)
		case 1:
			// proceed
		default:
			return nil, fmt.Errorf("validate: table %q is ambiguous across schemas: %s", tableKey, strings.Join(matches, ", "))
		}

		matched := matches[0]
		tm, ok := metas[matched]
		if !ok {
			// Should be unreachable: matched key came from m.Tables; the loop
			// above either parsed its meta or errored. The only path that skips
			// is MetaPath == "" with no chunks — for a configured table that's
			// a dump error.
			return nil, fmt.Errorf("validate: configured table %q has no per-table json sidecar", matched)
		}

		cols := tm.Options.Columns
		colIdx := make(map[string]int, len(cols))
		for i, c := range cols {
			colIdx[c] = i
		}
		for colName := range tf.Columns {
			if _, ok := colIdx[colName]; !ok {
				return nil, fmt.Errorf("validate: config references column %q.%q but it is not in the dump (have %v)",
					tableKey, colName, cols)
			}
		}
		schemas[matched] = &tableSchema{
			Columns:  cols,
			ColIndex: colIdx,
		}
	}
	return schemas, nil
}
```

(`ColIndex` is still here — Task 6 removes it. This task focuses only on the compression-check broadening.)

- [ ] **Step 4: Run the new test plus all existing validate tests**

Run: `go test ./cmd/mysql-anonymizer/ -v`
Expected: all PASS — `TestValidate_HappyPath`, `TestValidate_MissingTable`, `TestValidate_MissingColumn`, `TestValidate_NonZstdCompression`, `TestValidate_UnconfiguredNonZstd`.

- [ ] **Step 5: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 6: Format and commit**

```bash
go fmt ./...
jj describe -m "feat(cmd): broaden compression strict-check to all tables

Validate now parses every manifest table's per-table json and asserts
compression == zstd, not just tables referenced by config. Fixes A2.

Note: dumps containing unconfigured tables in non-zstd codecs that
previously passed validation will now fail. None are known to exist
in our pipeline."
jj new
```

---

## Task 6: C2 — Drop unused `tableSchema.ColIndex`

**Files:**
- Modify: `cmd/mysql-anonymizer/validate.go` (`tableSchema` struct + Validate body)
- No test changes (refactor; existing tests cover the surface)

**Context:** `tableSchema.ColIndex` (a `map[string]int`) is built per table but the only consumer (`pool.go:113-115`) iterates `Columns` by position. The map is dead state. Remove it; collapse the struct.

The struct has two fields and Task 7 adds a third (`ConfigTable string`) — so don't collapse to a `[]string` type alias. Keep it as a struct with just `Columns []string` for now.

- [ ] **Step 1: Remove `ColIndex` from `tableSchema`**

In `cmd/mysql-anonymizer/validate.go`, replace:

```go
type tableSchema struct {
	Columns  []string
	ColIndex map[string]int
}
```

with:

```go
type tableSchema struct {
	Columns []string
}
```

- [ ] **Step 2: Stop populating `ColIndex` in `Validate`**

In `Validate` (post-Task-5 body), replace the configured-table loop's tail:

```go
		cols := tm.Options.Columns
		colIdx := make(map[string]int, len(cols))
		for i, c := range cols {
			colIdx[c] = i
		}
		for colName := range tf.Columns {
			if _, ok := colIdx[colName]; !ok {
				return nil, fmt.Errorf("validate: config references column %q.%q but it is not in the dump (have %v)",
					tableKey, colName, cols)
			}
		}
		schemas[matched] = &tableSchema{
			Columns:  cols,
			ColIndex: colIdx,
		}
```

with:

```go
		cols := tm.Options.Columns
		colSet := make(map[string]struct{}, len(cols))
		for _, c := range cols {
			colSet[c] = struct{}{}
		}
		for colName := range tf.Columns {
			if _, ok := colSet[colName]; !ok {
				return nil, fmt.Errorf("validate: config references column %q.%q but it is not in the dump (have %v)",
					tableKey, colName, cols)
			}
		}
		schemas[matched] = &tableSchema{
			Columns: cols,
		}
```

(`colSet` is local to validation only; it never escapes the function.)

- [ ] **Step 3: Run all tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 4: Vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 5: Format and commit**

```bash
go fmt ./...
jj describe -m "refactor(cmd): drop unused tableSchema.ColIndex

ColIndex was built per validated table but only consumed within Validate
itself; pool.go iterates Columns by position. Replace the map with a
local colSet for the column-existence check. Fixes C2."
jj new
```

---

## Task 7: C3 — Consolidate `tablePart` lookup into `tableSchema`

**Files:**
- Modify: `cmd/mysql-anonymizer/validate.go` (`tableSchema` adds `ConfigTable`; `Validate` populates it)
- Modify: `cmd/mysql-anonymizer/pool.go:108-109` (use `j.schema.ConfigTable` instead of `tablePart(j.tableKey)`)
- No test changes (refactor; existing tests cover the path)

**Context:** Both `validate.go` and `pool.go` independently call `tablePart(j.tableKey)` to map a `<schema>@<table>` manifest key to the bare config table key. Consolidate by capturing the mapping during validation: `tableSchema` carries `ConfigTable string`, `Validate` sets it (it has both forms in scope), and `processChunk` reads `j.schema.ConfigTable`. After this change `tablePart` is private to `validate.go`.

- [ ] **Step 1: Add `ConfigTable` to `tableSchema`**

In `cmd/mysql-anonymizer/validate.go`, change:

```go
type tableSchema struct {
	Columns []string
}
```

to:

```go
type tableSchema struct {
	// ConfigTable is the bare table name as it appears in the YAML config
	// (e.g. "users"). Manifest keys are "<schema>@<table>" (e.g. "fx@users");
	// this field captures the translation done during validation so the
	// worker pool doesn't re-derive it.
	ConfigTable string
	Columns     []string
}
```

- [ ] **Step 2: Populate `ConfigTable` in `Validate`**

In `Validate`'s configured-table loop, change the schema construction. Replace:

```go
		schemas[matched] = &tableSchema{
			Columns: cols,
		}
```

with:

```go
		schemas[matched] = &tableSchema{
			ConfigTable: tableKey,
			Columns:     cols,
		}
```

(`tableKey` here is the loop variable from `for tableKey, tf := range rc.Filters` — i.e., the bare config name. Don't confuse it with the outer-scope `tableKey` in the meta-parsing loop, which uses a different variable name in the same function. Both loops shadow safely; verify by reading the post-Task-5 body.)

- [ ] **Step 3: Update `processChunk` to read `ConfigTable` from the schema**

In `cmd/mysql-anonymizer/pool.go:108-109`, replace:

```go
	tableName := tablePart(j.tableKey)
	colRules := cc.Rules[tableName]
```

with:

```go
	colRules := cc.Rules[j.schema.ConfigTable]
```

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: all PASS — including `TestEndToEnd`, `TestEndToEnd_Determinism`, and `TestEndToEnd_UnconfiguredTablePassthrough`. These exercise the configured-rule application path end-to-end.

- [ ] **Step 5: Verify `tablePart` still has at least one caller (the search loop in `Validate`)**

Run: `grep -n tablePart cmd/mysql-anonymizer/`
Expected: exactly one definition in `validate.go` and one call site in `validate.go`'s ambiguity-resolution loop. No call sites in `pool.go`. (If pool.go still calls it, you missed Step 3.)

- [ ] **Step 6: Format and commit**

```bash
go fmt ./...
jj describe -m "refactor(cmd): consolidate manifest-key→config-table mapping in tableSchema

Validate already knows both the manifest key (fx@users) and the bare
config table name (users); capture the latter in tableSchema.ConfigTable
so processChunk doesn't re-derive it via tablePart. Single source of
truth for the key-shape convention. Fixes C3."
jj new
```

---

## Task 8: C1 — Drop unused `f *faker.Faker` parameter from `anon.ProcessAll*`

**Files:**
- Modify: `internal/anon/anon.go` (signatures of `ProcessAll` and `ProcessAllWithRowHook`)
- Modify: `internal/anon/anon_test.go` (call sites)
- Modify: `cmd/mysql-anonymizer/pool.go:157` (call site)
- No new tests (refactor; existing tests cover the surface)

**Context:** Templates close over `f.FuncMap()` at compile time. By the time `ProcessAll*` runs, the faker is wired into the templates and the `f *faker.Faker` parameter is `_ = f`'d. Drop it.

- [ ] **Step 1: Remove `f *faker.Faker` from both signatures and the unused-param assignments**

In `internal/anon/anon.go`, replace:

```go
func ProcessAll(r *tsv.Reader, w *tsv.Writer, slots []*template.Template, f *faker.Faker) error {
	_ = f // reserved: f's funcmap is already closed over by each template
	return ProcessAllWithRowHook(r, w, slots, f, nil)
}

// ProcessAllWithRowHook anonymizes every row from r and writes it to w. After
// each row is written, hook (if non-nil) is called with the byte offset at the
// end of that row. Processing stops on the first error; io.EOF from r is
// treated as clean termination and returns nil.
func ProcessAllWithRowHook(r *tsv.Reader, w *tsv.Writer, slots []*template.Template, f *faker.Faker, hook RowEnded) error {
	_ = f // reserved: f's funcmap is already closed over by each template
```

with:

```go
func ProcessAll(r *tsv.Reader, w *tsv.Writer, slots []*template.Template) error {
	return ProcessAllWithRowHook(r, w, slots, nil)
}

// ProcessAllWithRowHook anonymizes every row from r and writes it to w. After
// each row is written, hook (if non-nil) is called with the byte offset at the
// end of that row. Processing stops on the first error; io.EOF from r is
// treated as clean termination and returns nil.
func ProcessAllWithRowHook(r *tsv.Reader, w *tsv.Writer, slots []*template.Template, hook RowEnded) error {
```

- [ ] **Step 2: Drop the now-unused `faker` import from `internal/anon/anon.go`**

Verify with `goimports` or by inspection: after Step 1, `faker` is still referenced for `faker.SentinelNULL`. Do NOT remove the import — it's still needed.

- [ ] **Step 3: Update test call sites in `internal/anon/anon_test.go`**

Replace the two call sites:

```go
	err := ProcessAll(r, w, slots, f)
```

with:

```go
	err := ProcessAll(r, w, slots)
```

(`runProcess`'s signature still takes `f *faker.Faker` because some tests pass a faker that's used to bind templates via `f.FuncMap()`. Don't change `runProcess`'s parameter; only change what it forwards to `ProcessAll`.)

Also replace:

```go
	err := ProcessAllWithRowHook(r, w, slots, newFaker(), hook)
```

with:

```go
	err := ProcessAllWithRowHook(r, w, slots, hook)
```

And in `TestProcessAll_EOF`, replace:

```go
	err := ProcessAll(r, w, []*template.Template{}, newFaker())
```

with:

```go
	err := ProcessAll(r, w, []*template.Template{})
```

- [ ] **Step 4: Update the call site in `cmd/mysql-anonymizer/pool.go`**

In `cmd/mysql-anonymizer/pool.go:157`, replace:

```go
	if err := anon.ProcessAllWithRowHook(tr, tw, slots, f, hook); err != nil {
```

with:

```go
	if err := anon.ProcessAllWithRowHook(tr, tw, slots, hook); err != nil {
```

`f` (the per-job `*faker.Faker`) is still constructed earlier and used to compile templates via `rc.Compile(f)`. Don't remove that.

- [ ] **Step 5: Run all tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 6: Vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 7: Format and commit**

```bash
go fmt ./...
jj describe -m "refactor(anon): drop unused *faker.Faker parameter from ProcessAll*

Templates close over f.FuncMap() at compile time, so by the time
ProcessAll runs the faker is already wired in. The parameter was
discarded via _ = f. Drop it from both signatures and update call
sites in pool.go and anon_test.go. Fixes C1."
jj new
```

---

## Task 9: B1 — Richer integration fixture (mixed chunk suffixes, escapes, `\N`, byte-identity)

**Files:**
- Modify: `cmd/mysql-anonymizer/integration_test.go` (`buildTinyDump`, `writeConfig`, `TestEndToEnd`)

**Context:** The current integration fixture has only ASCII data and writes both chunks with the `@@<n>` final-chunk pattern. Three changes:

1. Chunk 0 → `fx@users@0.tsv.zst` (non-final, single `@`).
2. Chunk 1 → `fx@users@@1.tsv.zst` (final, double `@@`).
3. Add a `notes` column (unconfigured) whose values include tab, newline, backslash, NUL, `\Z` (0x1A), `\b`, multi-byte UTF-8, and a `\N` literal-NULL token. Add explicit byte-identity assertions for the unconfigured columns of the rewritten chunks.

This widens what the e2e test catches without changing the worker contract.

- [ ] **Step 1: Update `buildTinyDump` to write the new column layout and mixed chunk suffixes**

Replace `buildTinyDump` in `cmd/mysql-anonymizer/integration_test.go` with:

```go
// buildTinyDump writes a synthetic mysqlsh-shaped dump under dir.
// One schema "fx", one table "users" with two chunks:
//   - chunk 0 has the non-final @<n> filename pattern
//   - chunk 1 has the final @@<n> pattern (the last chunk of a table)
// Columns are id, name, email, notes. Notes contains every TSV escape
// character plus a literal NULL (\N) cell — so the byte-identity contract
// gets exercised for passthrough cells with non-trivial bytes.
func buildTinyDump(t *testing.T, dir string) {
	t.Helper()
	mustWrite := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
	mustWrite("@.sql", "")
	mustWrite("@.post.sql", "")
	mustWrite("@.users.sql", "")
	mustWrite("fx.json", "{}")
	mustWrite("fx.sql", "")
	mustWrite("fx@users.json", `{"compression":"zstd","extension":"tsv.zst","options":{"columns":["id","name","email","notes"],"fieldsTerminatedBy":"\t","fieldsEscapedBy":"\\","linesTerminatedBy":"\n"}}`)
	mustWrite("fx@users.sql", "")

	// rows are 4-cell tuples in physical order: id, name, email, notes.
	// The notes cell is already TSV-escaped (the on-disk byte form).
	type row [4]string
	writeChunk := func(suffix string, idx int, rows []row) {
		var raw bytes.Buffer
		for _, r := range rows {
			raw.WriteString(r[0])
			raw.WriteByte('\t')
			raw.WriteString(r[1])
			raw.WriteByte('\t')
			raw.WriteString(r[2])
			raw.WriteByte('\t')
			raw.WriteString(r[3])
			raw.WriteByte('\n')
		}
		var compressed bytes.Buffer
		zw, err := lzstd.NewWriter(&compressed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(zw, &raw); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		chunkPath := filepath.Join(dir, "fx@users"+suffix+strconv.Itoa(idx)+".tsv.zst")
		if err := os.WriteFile(chunkPath, compressed.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
		// .idx is a single 8-byte BE uint64 = decompressed length.
		var idxBuf [8]byte
		binary.BigEndian.PutUint64(idxBuf[:], uint64(raw.Len()))
		if err := os.WriteFile(chunkPath+".idx", idxBuf[:], 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Chunk 0: non-final @<n>. Notes cells contain TSV-escaped escape chars.
	writeChunk("@", 0, []row{
		{"1", "Alice", "a@x.com", `tab\there`},                      // tab via \t
		{"2", "Bob", "b@x.com", `newline\nhere`},                    // newline via \n
		{"3", "Carol", "c@x.com", `backslash\\here`},                // backslash via \\
	})
	// Chunk 1: final @@<n>. Notes cells contain NUL, \Z, \b, multi-byte UTF-8, \N.
	writeChunk("@@", 1, []row{
		{"4", "Dave", "d@x.com", `nul\0here`},                       // NUL via \0
		{"5", "Eve", "e@x.com", `ctrlz\Zhere\bbs`},                  // \Z and \b
		{"6", "Frank", "f@x.com", `日本語/français`},                  // multi-byte UTF-8
		{"7", "Grace", "g@x.com", `\N`},                             // literal NULL token
	})

	mustWrite("@.done.json", "{}")
}
```

Make sure `"encoding/binary"` is in the import block (Task 2 added it; if it isn't there, add it).

- [ ] **Step 2: Update `writeConfig` to leave `notes` unconfigured (no rule for it)**

Verify `writeConfig` in `cmd/mysql-anonymizer/integration_test.go` still configures only `email`. The current body:

```go
body := `
filters:
  users:
    columns:
      email:
        value: "{{ fakerEmail }}"
`
```

is correct as-is for B1 — `id`, `name`, `notes` are all passthrough.

- [ ] **Step 3: Update `TestEndToEnd` to read both new chunk filenames and assert byte-identity on passthrough cells**

Replace the body of `TestEndToEnd` after the structure check (i.e., from `// 2. Email column is replaced...` through end of function) with:

```go
	// 2. Verify each chunk: id/name/notes are byte-identical to input;
	//    email no longer contains "@x.com".
	verifyChunk := func(srcPath, dstPath string) {
		srcRows := readChunkRows(t, srcPath)
		dstRows := readChunkRows(t, dstPath)
		if len(srcRows) != len(dstRows) {
			t.Fatalf("%s: %d rows in input, %d in output", filepath.Base(srcPath), len(srcRows), len(dstRows))
		}
		for i := range srcRows {
			src, dst := srcRows[i], dstRows[i]
			if len(src) != 4 || len(dst) != 4 {
				t.Errorf("%s row %d: cell counts %d/%d, want 4/4", filepath.Base(srcPath), i, len(src), len(dst))
				continue
			}
			// id, name, notes — byte-identical (passthrough).
			for _, ci := range []int{0, 1, 3} {
				if !bytes.Equal(src[ci], dst[ci]) {
					t.Errorf("%s row %d cell %d: passthrough mismatch: in=%q out=%q",
						filepath.Base(srcPath), i, ci, src[ci], dst[ci])
				}
			}
			// email — must differ from "@x.com" pattern but contain "@".
			if bytes.Contains(dst[2], []byte("@x.com")) {
				t.Errorf("%s row %d email %q still contains @x.com", filepath.Base(srcPath), i, dst[2])
			}
			if !bytes.Contains(dst[2], []byte{'@'}) {
				t.Errorf("%s row %d email %q has no @", filepath.Base(srcPath), i, dst[2])
			}
		}
	}
	verifyChunk(filepath.Join(inDir, "fx@users@0.tsv.zst"), filepath.Join(outDir, "fx@users@0.tsv.zst"))
	verifyChunk(filepath.Join(inDir, "fx@users@@1.tsv.zst"), filepath.Join(outDir, "fx@users@@1.tsv.zst"))
}

// readChunkRows decompresses a .zst chunk and returns its rows as [][]byte
// cells. Used by integration tests for byte-level comparisons.
func readChunkRows(t *testing.T, path string) [][][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := lzstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	rows := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
	out := make([][][]byte, len(rows))
	for i, row := range rows {
		out[i] = bytes.Split(row, []byte{'\t'})
	}
	return out
}
```

(`readChunkRows` is reused by the next two tasks; defining it once here keeps the diff small.)

- [ ] **Step 4: Run the integration tests**

Run: `go test ./cmd/mysql-anonymizer/ -run TestEndToEnd -v`
Expected: PASS — both chunks decode, passthrough cells byte-identical (including the cell that's the literal `\N` NULL-token), email substituted.

- [ ] **Step 5: Run determinism + unconfigured-table tests**

Run: `go test ./cmd/mysql-anonymizer/ -run "TestEndToEnd_(Determinism|UnconfiguredTablePassthrough)" -v`
Expected: PASS. (These reused `buildTinyDump`; the schema change shouldn't affect them.)

- [ ] **Step 6: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 7: Format and commit**

```bash
go fmt ./...
jj describe -m "test(integration): exercise escapes, \\N, mixed chunk suffixes, byte-identity

buildTinyDump now writes chunk 0 as fx@users@0 (non-final, single @) and
chunk 1 as fx@users@@1 (final, double @@), and adds a 'notes' column
whose values include tab, newline, backslash, NUL, \\Z, \\b, multi-byte
UTF-8, and a literal \\N NULL token. TestEndToEnd asserts byte-identity
on all passthrough cells (id, name, notes) and substitution on email.
Fixes B1."
jj new
```

---

## Task 10: B2 — `.idx` end-to-end verification

**Files:**
- Modify: `cmd/mysql-anonymizer/integration_test.go` (extend `TestEndToEnd`)

**Context:** `.idx` correctness is unit-tested in `internal/idx/` and stability is checked by `TestEndToEnd_Determinism`. But no test asserts the `.idx` written by the worker pipeline contains the *correct* decompressed length of its sibling `.zst`.

- [ ] **Step 1: Add a small helper that asserts an `.idx` file matches its sibling chunk's decompressed length**

In `cmd/mysql-anonymizer/integration_test.go`, add (next to `readChunkRows`):

```go
// assertIdxMatchesChunk reads idxPath (8-byte BE uint64), decompresses
// chunkPath, and asserts the lengths match.
func assertIdxMatchesChunk(t *testing.T, chunkPath, idxPath string) {
	t.Helper()
	idxBytes, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read idx %s: %v", idxPath, err)
	}
	if len(idxBytes) != 8 {
		t.Fatalf("idx %s: %d bytes, want 8", idxPath, len(idxBytes))
	}
	declared := binary.BigEndian.Uint64(idxBytes)

	f, err := os.Open(chunkPath)
	if err != nil {
		t.Fatalf("open chunk %s: %v", chunkPath, err)
	}
	defer f.Close()
	zr, err := lzstd.NewReader(f)
	if err != nil {
		t.Fatalf("zstd reader %s: %v", chunkPath, err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress %s: %v", chunkPath, err)
	}
	if uint64(len(data)) != declared {
		t.Errorf("%s: idx says %d bytes, decompressed is %d bytes", chunkPath, declared, len(data))
	}
}
```

- [ ] **Step 2: Call `assertIdxMatchesChunk` for each rewritten chunk inside `TestEndToEnd`**

In `cmd/mysql-anonymizer/integration_test.go`, after the two `verifyChunk(...)` calls inside `TestEndToEnd`, add:

```go
	for _, base := range []string{"fx@users@0.tsv.zst", "fx@users@@1.tsv.zst"} {
		assertIdxMatchesChunk(t, filepath.Join(outDir, base), filepath.Join(outDir, base+".idx"))
	}
```

- [ ] **Step 3: Run TestEndToEnd**

Run: `go test ./cmd/mysql-anonymizer/ -run TestEndToEnd$ -v`
Expected: PASS — the implementation already writes the correct length via `idx.Write(idxF, tw.BytesWritten())`. This test is a regression net, not a fix-driver.

- [ ] **Step 4: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 5: Format and commit**

```bash
go fmt ./...
jj describe -m "test(integration): verify .idx matches decompressed chunk length end-to-end

assertIdxMatchesChunk reads the 8-byte BE idx and the sibling .zst,
asserts decompressed-length parity. Pinned for both rewritten chunks
in TestEndToEnd. Fixes B2."
jj new
```

---

## Task 11: B3 — Different seeds produce different output

**Files:**
- Modify: `cmd/mysql-anonymizer/integration_test.go` (add new test)

**Context:** Determinism is tested in one direction (same seed → same output). Add the dual: two distinct seeds must produce at least one different substituted cell. Guards against accidental seed pinning or shared state.

- [ ] **Step 1: Add the test**

Append to `cmd/mysql-anonymizer/integration_test.go`:

```go
func TestEndToEnd_DifferentSeeds(t *testing.T) {
	inDir := t.TempDir()
	buildTinyDump(t, inDir)
	cfg := writeConfig(t, t.TempDir())

	out1 := filepath.Join(t.TempDir(), "clean1")
	out2 := filepath.Join(t.TempDir(), "clean2")
	o1 := opts{InDir: inDir, OutDir: out1, ConfigPath: cfg, Seed: 42, Workers: 2}
	o2 := opts{InDir: inDir, OutDir: out2, ConfigPath: cfg, Seed: 43, Workers: 2}
	if err := run(context.Background(), o1); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), o2); err != nil {
		t.Fatal(err)
	}

	// Check the email column (the only configured/substituted column).
	differs := false
	for _, base := range []string{"fx@users@0.tsv.zst", "fx@users@@1.tsv.zst"} {
		rows1 := readChunkRows(t, filepath.Join(out1, base))
		rows2 := readChunkRows(t, filepath.Join(out2, base))
		if len(rows1) != len(rows2) {
			t.Fatalf("%s: row counts %d/%d", base, len(rows1), len(rows2))
		}
		for i := range rows1 {
			if !bytes.Equal(rows1[i][2], rows2[i][2]) { // cell 2 = email
				differs = true
				break
			}
		}
		if differs {
			break
		}
	}
	if !differs {
		t.Error("seeds 42 and 43 produced byte-identical email cells across both chunks")
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./cmd/mysql-anonymizer/ -run TestEndToEnd_DifferentSeeds -v`
Expected: PASS. (If it fails, that's a determinism bug — seeds aren't actually flowing through to the RNG.)

- [ ] **Step 3: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 4: Format and commit**

```bash
go fmt ./...
jj describe -m "test(integration): different seeds produce different substituted cells

Complements TestEndToEnd_Determinism (which pins same-seed → same-output)
with the dual: seed=42 vs seed=43 must produce at least one different
email cell. Guards against accidental seed pinning or shared faker state.
Fixes B3."
jj new
```

---

## Task 12: B4 — Context-cancel cleanup test

**Files:**
- Modify: `cmd/mysql-anonymizer/integration_test.go` (add new test)

**Context:** The contract: a cancelled context (the same path SIGINT/SIGTERM and worker errors take) must leave no `.tmp` files in the output dir and must not write `@.done.json`. Drive this by passing a context that is cancelled before `run` is called — the simplest reliable trigger that doesn't depend on chunk-processing timing.

A pre-cancelled context tests the cleanup contract (no `.tmp` files, no `@.done.json`) without racing with chunk completion. Mid-run cancellation tests are flakier; the operability spec already deferred the deeper SIGINT path.

- [ ] **Step 1: Add the test**

Append to `cmd/mysql-anonymizer/integration_test.go`:

```go
func TestEndToEnd_PreCancelledContextLeavesNoTmpAndNoDoneMarker(t *testing.T) {
	inDir := t.TempDir()
	buildTinyDump(t, inDir)
	cfg := writeConfig(t, t.TempDir())
	outDir := filepath.Join(t.TempDir(), "clean")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: every chunk job will observe ctx.Err() before running.

	o := opts{InDir: inDir, OutDir: outDir, ConfigPath: cfg, Seed: 42, Workers: 2}
	err := run(ctx, o)
	if err == nil {
		t.Fatal("expected run to fail under cancelled context, got nil")
	}

	// Output dir should exist (PreparePassthrough creates it before the worker
	// pool runs; passthrough copies are unconditional and may have landed).
	// What MUST NOT be present: any .tmp file, and @.done.json.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		// outDir wasn't created at all — also a valid clean state. Not a failure.
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("found leftover .tmp file in outDir: %s", e.Name())
		}
		if e.Name() == "@.done.json" {
			t.Errorf("@.done.json present in outDir despite cancelled run")
		}
	}
}
```

`strings` is already imported by `main_test.go` but `integration_test.go` does not import it. Add `"strings"` to the import block of `integration_test.go`.

- [ ] **Step 2: Run the test**

Run: `go test ./cmd/mysql-anonymizer/ -run TestEndToEnd_PreCancelledContextLeavesNoTmpAndNoDoneMarker -v`
Expected: PASS. The contract is enforced by:
- `processChunk`'s `defer func() { if err != nil { os.Remove(tmpData); os.Remove(tmpIdx) } }()` cleans up `.tmp` files when context-error short-circuits prevent rename.
- `run()`'s ordering: `RunPool` returns the error, `run()` returns before the `linkOrCopy(@.done.json)` line.

- [ ] **Step 3: Run full repo tests**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 4: Format and commit**

```bash
go fmt ./...
jj describe -m "test(integration): cancelled context leaves no .tmp files and no @.done.json

Pre-cancel the context before run(); assert outDir contains no .tmp
files and no @.done.json. The pre-cancel form avoids racing with chunk
completion while still exercising both cleanup defer paths (.tmp removal
in processChunk; the @.done.json gate in run()). True SIGINT integration
testing remains v1-deferred per the operability gaps section. Fixes B4."
jj new
```

---

## Final verification

After all 12 tasks, run the full suite once more end-to-end:

- [ ] **Run full test suite**

Run: `go test ./...`
Expected: every package PASSes.

- [ ] **Run vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Build**

Run: `go build ./...`
Expected: clean build.

- [ ] **Confirm no stray `.tmp` files anywhere in the working tree**

Run: `find . -name '*.tmp' -type f`
Expected: empty output.

- [ ] **Inspect the jj log**

Run: `jj log -r 'main..@' --no-graph`
Expected: 12 commits in chronological order, each describing one cluster fix per the spec.
