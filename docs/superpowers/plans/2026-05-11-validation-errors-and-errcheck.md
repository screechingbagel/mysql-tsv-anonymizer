# Validation Error Reporting and errcheck Cleanup — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `Validate` report *every* config-vs-dump problem at once (sorted, deterministic) instead of dying on the first, and make `golangci-lint run` exit clean.

**Architecture:** `cmd/mysql-anonymizer/validate.go`'s `Validate` accumulates a `[]error`, sorts by message text, returns `errors.Join(...)` (nil when empty). Separately, add a `.golangci.yml` that excludes `_test.go` from `errcheck` and excludes `fmt.Fprint*`, then fix the remaining real `errcheck` hits in `pool.go`/`copy.go` — checking write-path closes before the renames and explicitly ignoring read-side closes.

**Tech Stack:** Go 1.26, `errors.Join`, `golangci-lint` v2 (`v2.x` installed locally), Jujutsu (`jj`) for commits.

**Repo conventions (apply to every task):**
- Version control is `jj`, not raw `git`. Commit a finished task with `go fmt ./... && jj commit -m "<msg>"` (this describes the working-copy commit and starts a fresh one). Never run `git commit`/`git reset`/`git checkout`.
- `go fmt ./...` before every commit.
- Run from the repo root: `/Users/bagel/wooby/mysql-eff-anonymizer`.

**Spec:** `docs/superpowers/specs/2026-05-11-validation-errors-and-errcheck-design.md`

---

## Task 1: `Validate` collects and sorts all problems

**Files:**
- Modify: `cmd/mysql-anonymizer/validate.go` (rewrite the `Validate` function and imports)
- Test: `cmd/mysql-anonymizer/main_test.go` (add three tests)

### Background for the implementer

`Validate(rc *config.RawConfig, m *dump.Manifest) (map[string]*tableSchema, error)` cross-checks the user config against the parsed dump manifest. Today it `return nil, fmt.Errorf(...)` on the first mismatch. We want it to keep going, collect all problems, and return them joined.

Two phases (the function already has both):
- **Phase 1** — iterate `m.Tables` (every table in the dump, configured or not) and parse each per-table JSON sidecar to assert `compression == "zstd"`. A non-zstd table *anywhere* is a hard error (existing tests `TestValidate_NonZstdCompression` and `TestValidate_UnconfiguredNonZstd` pin this). `m.Tables` is keyed `"<schema>@<table>"`; `TableEntry.MetaPath` is `""` when there's no sidecar; `TableEntry.Chunks` is the chunk list.
- **Phase 2** — for each table named in `rc.Filters`, find the matching manifest key (table name is the part after the last `@`), then check every column named in the config against `tm.Options.Columns`.

Map iteration order is randomized in Go, so to keep output stable: sort `rc.Filters` keys before phase 2, sort `matches` before joining them into the "ambiguous" message, sort the configured column names before the per-column loop, and finally sort the accumulated `[]error` by `.Error()` before `errors.Join`.

- [ ] **Step 1: Write the failing tests**

Add these three functions to `cmd/mysql-anonymizer/main_test.go` (no new imports needed — `strings`, `testing`, `dump` are already imported):

```go
func TestValidate_ReportsAllMissingColumns(t *testing.T) {
	dir := mkTinyDump(t)
	m, err := dump.WalkManifest(dir)
	if err != nil {
		t.Fatalf("WalkManifest: %v", err)
	}
	rc := mkConfig(t, `
filters:
  users:
    columns:
      aaa:
        value: "x"
      zzz:
        value: "y"
`)
	_, err = Validate(rc, m)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "aaa") || !strings.Contains(msg, "zzz") {
		t.Errorf("expected error to mention both missing columns, got: %v", err)
	}
}

func TestValidate_ReportsTableAndColumnTogether(t *testing.T) {
	dir := mkTinyDump(t)
	m, err := dump.WalkManifest(dir)
	if err != nil {
		t.Fatalf("WalkManifest: %v", err)
	}
	rc := mkConfig(t, `
filters:
  users:
    columns:
      nope:
        value: "x"
  ghost:
    columns:
      whatever:
        value: "y"
`)
	_, err = Validate(rc, m)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"users"."nope"`) {
		t.Errorf("expected error to mention users.nope, got: %v", err)
	}
	if !strings.Contains(msg, "ghost") {
		t.Errorf("expected error to mention missing table ghost, got: %v", err)
	}
}

func TestValidate_ReportsNonZstdAndMissingColumn(t *testing.T) {
	dir := t.TempDir()
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
  orders:
    columns:
      amount:
        value: "0"
  users:
    columns:
      nope:
        value: "x"
`)
	_, err = Validate(rc, m)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "zstd") {
		t.Errorf("expected error to mention the non-zstd table, got: %v", err)
	}
	if !strings.Contains(msg, `"users"."nope"`) {
		t.Errorf("expected error to mention users.nope, got: %v", err)
	}
	if strings.Contains(msg, "no per-table json sidecar") {
		t.Errorf("non-zstd table should not also be reported as missing its sidecar, got: %v", err)
	}
}

func TestValidate_ErrorOrderIsDeterministic(t *testing.T) {
	dir := mkTinyDump(t)
	m, err := dump.WalkManifest(dir)
	if err != nil {
		t.Fatalf("WalkManifest: %v", err)
	}
	rc := mkConfig(t, `
filters:
  users:
    columns:
      c1:
        value: "x"
      c2:
        value: "x"
      c3:
        value: "x"
      c4:
        value: "x"
`)
	_, first := Validate(rc, m)
	if first == nil {
		t.Fatal("expected an error, got nil")
	}
	want := first.Error()
	for i := 0; i < 20; i++ {
		_, err := Validate(rc, m)
		if err == nil {
			t.Fatalf("iteration %d: expected error, got nil", i)
		}
		if got := err.Error(); got != want {
			t.Fatalf("iteration %d: Validate output not stable:\n got %q\nwant %q", i, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the new tests, verify they fail**

Run: `go test ./cmd/mysql-anonymizer/ -run 'TestValidate_ReportsAllMissingColumns|TestValidate_ReportsTableAndColumnTogether|TestValidate_ReportsNonZstdAndMissingColumn|TestValidate_ErrorOrderIsDeterministic' -v`
Expected: all four FAIL — the current `Validate` returns on the first mismatch, so the message mentions only one column / one of {table, column} / only the non-zstd error, and repeated calls return different single errors.

- [ ] **Step 3: Rewrite `validate.go`**

Replace the entire contents of `cmd/mysql-anonymizer/validate.go` with:

```go
package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
)

// tableSchema holds the ordered column list for one table, as derived from
// the per-table sidecar JSON in the dump.
type tableSchema struct {
	ConfigTable string
	Columns     []string
}

// tablePart returns the table name portion of a manifest key of the form
// "<schema>@<table>". It returns the part after the last '@'.
func tablePart(manifestKey string) string {
	i := strings.LastIndex(manifestKey, "@")
	if i < 0 {
		return manifestKey
	}
	return manifestKey[i+1:]
}

// Validate cross-checks rc (the user config) against m (the dump manifest).
// On success it returns a map keyed by the manifest key ("<schema>@<table>")
// for every table referenced in the config, and a nil error. On failure it
// returns a nil map and an error that is the sorted join of *every* problem
// found — callers must not use the map when the error is non-nil.
func Validate(rc *config.RawConfig, m *dump.Manifest) (map[string]*tableSchema, error) {
	var problems []error
	add := func(err error) { problems = append(problems, err) }

	// Phase 1: parse every table's per-table json once, asserting compression.
	// This sweep covers unconfigured tables too: a non-zstd table anywhere in
	// the dump is a hard error. metas holds only tables that parsed cleanly and
	// are zstd; tables absent from it had a problem recorded here (or, for a
	// configured table with no sidecar and no chunks, are handled in phase 2).
	metas := make(map[string]*dump.TableMeta, len(m.Tables))
	for tableKey, te := range m.Tables {
		if te.MetaPath == "" {
			if len(te.Chunks) > 0 {
				add(fmt.Errorf("validate: table %q has chunks but no per-table json sidecar", tableKey))
			}
			continue
		}
		tm, err := dump.ReadTableMeta(te.MetaPath)
		if err != nil {
			add(fmt.Errorf("validate: read meta for table %q: %w", tableKey, err))
			continue
		}
		if tm.Compression != "zstd" {
			add(fmt.Errorf("validate: table %q has compression %q; only zstd is supported",
				tableKey, tm.Compression))
			continue
		}
		metas[tableKey] = tm
	}

	// Phase 2: per configured table. Iterate config tables in sorted order so
	// the per-table column text below is produced in a stable order even before
	// the final accumulator sort.
	cfgTables := make([]string, 0, len(rc.Filters))
	for t := range rc.Filters {
		cfgTables = append(cfgTables, t)
	}
	sort.Strings(cfgTables)

	schemas := make(map[string]*tableSchema, len(rc.Filters))
	for _, tableKey := range cfgTables {
		tf := rc.Filters[tableKey]

		var matches []string
		for k := range m.Tables {
			if tablePart(k) == tableKey {
				matches = append(matches, k)
			}
		}
		sort.Strings(matches)
		switch len(matches) {
		case 0:
			add(fmt.Errorf("validate: config references table %q but it is not in the dump", tableKey))
			continue
		case 1:
			// proceed
		default:
			add(fmt.Errorf("validate: table %q is ambiguous across schemas: %s", tableKey, strings.Join(matches, ", ")))
			continue
		}

		matched := matches[0]
		tm, ok := metas[matched]
		if !ok {
			// matched is in m.Tables but not in metas, so phase 1 already
			// recorded a problem for it (chunks without sidecar, unreadable
			// sidecar, or non-zstd) — unless it is a configured table with no
			// sidecar and no chunks, which phase 1 ignores. Cover only that.
			if te := m.Tables[matched]; te.MetaPath == "" && len(te.Chunks) == 0 {
				add(fmt.Errorf("validate: configured table %q has no per-table json sidecar", matched))
			}
			continue
		}

		cols := tm.Options.Columns
		colSet := make(map[string]struct{}, len(cols))
		for _, c := range cols {
			colSet[c] = struct{}{}
		}
		colNames := make([]string, 0, len(tf.Columns))
		for c := range tf.Columns {
			colNames = append(colNames, c)
		}
		sort.Strings(colNames)
		for _, colName := range colNames {
			if _, ok := colSet[colName]; !ok {
				add(fmt.Errorf("validate: config references column %q.%q but it is not in the dump (have %v)",
					tableKey, colName, cols))
			}
		}
		schemas[matched] = &tableSchema{
			ConfigTable: tableKey,
			Columns:     cols,
		}
	}

	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].Error() < problems[j].Error() })
		return nil, errors.Join(problems...)
	}
	return schemas, nil
}
```

- [ ] **Step 4: Run the new tests, verify they pass**

Run: `go test ./cmd/mysql-anonymizer/ -run 'TestValidate_ReportsAllMissingColumns|TestValidate_ReportsTableAndColumnTogether|TestValidate_ReportsNonZstdAndMissingColumn|TestValidate_ErrorOrderIsDeterministic' -v`
Expected: all four PASS.

- [ ] **Step 5: Run the full suite + vet, verify nothing regressed**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS. In particular `TestValidate_HappyPath`, `TestValidate_MissingTable`, `TestValidate_MissingColumn`, `TestValidate_NonZstdCompression`, `TestValidate_UnconfiguredNonZstd`, `TestRun_RejectsVersion3`, and the `cmd/mysql-anonymizer` integration tests still pass unchanged.

- [ ] **Step 6: Commit**

```bash
go fmt ./...
jj commit -m "validate: report all config/dump problems, not just the first

Validate accumulates every mismatch into a sorted, joined error instead of
returning on the first one, so an operator sees the whole list in one run.
Output order is deterministic: config tables, schema matches, and column names
are sorted before use, and the accumulated errors are sorted by message text
before errors.Join."
```

---

## Task 2: add `.golangci.yml`

**Files:**
- Create: `.golangci.yml`

### Background

`golangci-lint run` currently reports 27 `errcheck` issues with no config file. ~19 are in `*_test.go` (test-helper `Close`/`Write` calls that are fine to ignore), 4 are `fmt.Fprintf`/`fmt.Fprintln` status-line writes to stderr in `internal/progress/progress.go` (a failed write to stderr is unactionable), and 5 are file `Close` calls in `cmd/mysql-anonymizer/{copy,pool}.go` (handled in Task 3). This task adds the config; after it, only the 5 `Close` issues remain.

The installed golangci-lint is v2.x — use the v2 config schema (`version: "2"`, `linters.settings.errcheck`, `linters.exclusions.rules`). This exact config has been verified to leave precisely the 5 `Close` issues.

- [ ] **Step 1: Create `.golangci.yml`** at the repo root with:

```yaml
version: "2"
linters:
  settings:
    errcheck:
      # Writing a status line to stderr can't fail in any actionable way.
      exclude-functions:
        - fmt.Fprint
        - fmt.Fprintf
        - fmt.Fprintln
  exclusions:
    rules:
      # Test helpers do plenty of unchecked Close()/Write() on in-memory or
      # temp-dir fixtures; errcheck noise there isn't worth the churn.
      - path: '_test\.go$'
        linters:
          - errcheck
```

- [ ] **Step 2: Run golangci-lint, verify only the 5 Close issues remain**

Run: `golangci-lint run`
Expected: exit code 1, exactly 5 issues, all `errcheck` "Error return value of `…Close` is not checked" — at `cmd/mysql-anonymizer/copy.go:64`, `cmd/mysql-anonymizer/pool.go:128`, `:133`, `:150`, `:156`. No `_test.go` issues, no `progress.go` issues.

- [ ] **Step 3: Commit**

```bash
go fmt ./...
jj commit -m "lint: add .golangci.yml

Exclude _test.go from errcheck and exclude fmt.Fprint* (stderr status writes).
The remaining errcheck hits are real and fixed in the next change."
```

---

## Task 3: check write-path closes, ignore read-side closes explicitly

**Files:**
- Modify: `cmd/mysql-anonymizer/pool.go` (the `processChunk` function, ~lines 110-191)
- Modify: `cmd/mysql-anonymizer/copy.go:64`

### Background

In `processChunk`, the output chunk file (`outF`) and idx file (`idxF`) are written, `Sync`'d, then `os.Rename`'d into place. Their `Close` is currently a bare `defer`, which (a) is unchecked and (b) would fire *after* the renames — too late for the cleanup defer (which removes the `.tmp` paths) to do anything if Close failed. Fix: close `outF`/`idxF` explicitly, checked, in the happy path *before* the renames; demote the up-top `defer`s to `_ =`-ignored no-op safety nets for the early-return paths (closing an already-closed `*os.File` returns `os.ErrClosed`, harmlessly discarded). The two read-side closes (`inF`, `zr`) become `_ =`-ignored closures too — `zr` is a `zstd.ReadCloser` whose `Close` always returns `nil` anyway. The cleanup-remove `defer` keeps its current declaration position, so LIFO order is unchanged: on an early return the safety-net closes fire before the temp-file removal. Same one-line fix for `copy.go`'s read-side `in.Close()` in `linkOrCopy`.

The zstd *encoder* `zw` is left exactly as-is (it's already closed-and-checked once in the happy path and has no `defer`; errcheck doesn't flag it because it's a missing-call, not an unchecked-call). Don't touch it.

`linkOrCopy`'s `out.Close()`-into-`retErr` defer (lines ~70-74) is also left as-is — that path has no rename and isn't flagged.

- [ ] **Step 1: Edit `cmd/mysql-anonymizer/copy.go`**

Change line 64 from:

```go
	defer in.Close()
```

to:

```go
	defer func() { _ = in.Close() }()
```

- [ ] **Step 2: Edit `cmd/mysql-anonymizer/pool.go` `processChunk`**

Replace the *entire* `processChunk` function with the version below. (Only the file-handle parts changed; the `deriveSeed`/faker/`slots` setup at the top is shown unchanged so you can match the whole function.)

```go
// processChunk handles one (table, chunk) job: derive RNG, compile templates,
// build slot list, stream-rewrite the chunk, atomic-rename outputs.
func processChunk(ctx context.Context, j job, rc *config.RawConfig, jobSeed uint64, outDir string) (err error) {
	hi, lo := deriveSeed(jobSeed, j.tableKey, j.chunk.Index)
	f := faker.New(rand.NewPCG(hi, lo))
	f.SetInvoiceBase(uint64(j.chunk.Index) * faker.InvoiceStride)
	cc, err := rc.Compile(f)
	if err != nil {
		return fmt.Errorf("compile config: %w", err)
	}
	colRules := cc.Rules[j.schema.ConfigTable]
	slots := make([]*template.Template, len(j.schema.Columns))
	for i, col := range j.schema.Columns {
		slots[i] = colRules[col]
	}

	inF, err := os.Open(j.chunk.DataPath)
	if err != nil {
		return err
	}
	defer func() { _ = inF.Close() }()
	zr, err := lzstd.NewReader(inF)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	dstData := filepath.Join(outDir, filepath.Base(j.chunk.DataPath))
	dstIdx := filepath.Join(outDir, filepath.Base(j.chunk.IdxPath))
	tmpData := dstData + ".tmp"
	tmpIdx := dstIdx + ".tmp"

	outF, err := os.Create(tmpData)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmpData)
			_ = os.Remove(tmpIdx)
		}
	}()
	// Safety net for the early-return paths only; the happy path closes outF
	// explicitly below (before the rename). Re-closing returns os.ErrClosed.
	defer func() { _ = outF.Close() }()

	idxF, err := os.Create(tmpIdx)
	if err != nil {
		return err
	}
	defer func() { _ = idxF.Close() }()

	zw, err := lzstd.NewWriter(outF)
	if err != nil {
		return err
	}
	tw := tsv.NewWriter(zw)
	tr := tsv.NewReader(zr)

	hook := func(_ int64) error { return ctx.Err() }
	if err := anon.ProcessAllWithRowHook(tr, tw, slots, hook); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := outF.Sync(); err != nil {
		return err
	}
	// Close before the rename: on a FUSE / object-store mount this is the
	// operation that actually persists the file, and a failure here must be
	// caught while the cleanup defer's .tmp paths still exist.
	if err := outF.Close(); err != nil {
		return err
	}
	if err := idx.Write(idxF, tw.BytesWritten()); err != nil {
		return err
	}
	if err := idxF.Sync(); err != nil {
		return err
	}
	if err := idxF.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpData, dstData); err != nil {
		return err
	}
	if err := os.Rename(tmpIdx, dstIdx); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 3: Run golangci-lint, verify clean**

Run: `golangci-lint run`
Expected: exit code 0, no issues.

- [ ] **Step 4: Run build, vet, and the full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS — in particular the `cmd/mysql-anonymizer` `TestEndToEnd_*` integration tests, which produce and reload chunk + `.idx` output, still pass (regression guard for the close reordering).

- [ ] **Step 5: Commit**

```bash
go fmt ./...
jj commit -m "anonymizer: check write-path Close errors, ignore read-side closes explicitly

processChunk now closes the output chunk and idx files explicitly (checked)
before renaming them into place, so a Close failure is caught while the cleanup
defer can still remove the .tmp files. The up-top defers become _=-ignored
safety nets for the early-return paths. linkOrCopy's read-side in.Close gets the
same _=-ignore. golangci-lint run is now clean."
```

---

## Final verification

- [ ] Run `go build ./... && go vet ./... && go test ./... && golangci-lint run` — everything green, golangci-lint exit 0.
- [ ] Run `jj log -r '::@ & ~empty()' --no-graph -T 'change_id.shortest(8) ++ " " ++ description.first_line() ++ "\n"'` and confirm the three new commits are present (validate / golangci config / write-path closes).

## Out of scope (do not do)

- Wiring golangci-lint into CI (`.github/workflows/`).
- Making `Validate`'s per-table sidecar reads lazy / config-only — the all-tables sweep is intentional.
- Folding per-chunk byte sizes into `WalkManifest`/`ChunkEntry`; leave `main.go` step 7's `os.Stat` loop alone.
- Adding a `defer zw.Close()` or otherwise restructuring the zstd encoder lifecycle.
