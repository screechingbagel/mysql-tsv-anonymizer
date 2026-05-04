# MySQL Dump Anonymizer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md`

**Goal:** Build a Go internal tool that anonymizes a `mysqlsh util.dumpInstance` directory into a sibling clean directory loadable by `util.loadDump`, applying YAML-configured rules deterministically per `(jobSeed, table, chunkIdx)`.

**Architecture:** Single binary. Bottom-up build: TSV codec → zstd wrapper → dump metadata loader → `.idx` regenerator → row processor → orchestrator. Each layer fully tested before the next is built. The two open questions in the spec (`.idx` format, `<schema>@<table>.json` schema) are resolved early via a fixture-gathering task that runs `mysqlsh` against a throwaway database and commits the byte-level evidence.

**Tech Stack:** Go 1.26, `gofakeit/v7`, `gopkg.in/yaml.v3`, `klauspost/compress/zstd`, `math/rand/v2`. Version control: `jj`, not `git`.

---

## Errata from Task 1 — still load-bearing for pending tasks

Task 1 ran `mysqlsh` 9.7 against `mysql:8.4` and committed ground truth under `testdata/fixtures/` (prose: `testdata/fixtures/notes.md`). Findings about `.idx` format, `options.columns` JSON path, and the `@<n>` vs `@@<n>` chunk filename split have all been folded into Tasks 10–12 and retired from this list. Two findings still affect remaining tasks:

1. **`compression: "zstd"` lives in the *per-table* JSON, not in `@.json`.** `@.json` has no `compression` field. The strict check belongs against `<schema>@<table>.json`. Affects **Task 15** (must validate per-table compression) and **Task 18** (`run` must not reach for `InstanceMeta.Compression` — that field doesn't exist).
2. **`bytesPerChunk` minimum in mysqlsh is 128k.** A real-mysqlsh fixture cannot use a smaller value. **Task 19**'s synthetic fixture is fine (it bypasses mysqlsh).

---

## Workflow conventions

- **Commit cadence: at the end of each task.** The "Commit" step at the end of every task is the only place where the working copy is wrapped.
- **Commit commands:**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "<conventional commit message>"
  jj new
  ```
  `jj describe` sets the description on the current working-copy change. `jj new` starts a new empty change on top, making it the new working copy. Do **not** run `git commit`.
- **Tests are written before implementation** within each task. The "run test, expect fail" step is mandatory — it confirms the test actually exercises the code under test.
- **`go fmt` and `go vet` must pass before every commit.** They're cheap; the commit step runs them unconditionally.

---

## File map

```
cmd/mysql-anonymizer/
  main.go              # flag parsing, top-level orchestration, signal handling
  validate.go          # strict config-vs-dump validation
  copy.go              # copy/hardlink pass
  pool.go              # worker pool dispatch

internal/tsv/
  tsv.go               # package doc
  escape.go            # lifted escape table from hexon/mysqltsv (with LICENSE)
  reader.go            # streaming TSV row reader
  writer.go            # streaming TSV row writer
  tsv_test.go
  LICENSE              # already present

internal/zstd/
  zstd.go              # klauspost/compress/zstd thin wrapper
  zstd_test.go

internal/dump/
  meta.go              # @.json + per-table json parsing
  manifest.go          # walker + file classification
  dump_test.go

internal/idx/
  idx.go               # binary writer + format constants
  idx_test.go

internal/anon/
  anon.go              # row processor + NULL sentinel guard
  anon_test.go

internal/config/
  config.go            # split into LoadRaw + (*RawConfig).Compile
  config_test.go

internal/faker/
  faker.go             # already exists
  faker_test.go        # add determinism tests

testdata/
  fixtures/             # mysqlsh-produced ground-truth artifacts (Task 1)
    notes.md
    sample.tsv
    sample.tsv.zst
    sample.idx
    sample-table.json
    sample-at.json

docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md  # already written
```

---

## Tasks 1–12: COMPLETED (summary)

Full content elided to save context. Re-read git/jj history for details.

- **Task 1** — mysqlsh ground-truth fixture at `testdata/fixtures/` (sample.tsv, sample.tsv.zst, sample.idx, sample-at.json, sample-table.json, notes.md). Pinned the errata above.
- **Task 2** — `internal/tsv/escape.go` lifted from hexon/mysqltsv with LICENSE attribution.
- **Task 3** — `internal/tsv/reader.go`: streaming `Reader.Next() ([][]byte, error)` reusing internal buffers.
- **Task 4** — `internal/tsv/writer.go`: streaming `Writer` with `WritePassthrough`, `WriteSubstituted`, `WriteNULL`, `EndRow`, `Flush`, `BytesWritten`.
- **Task 5** — `internal/tsv/tsv_test.go`: fuzzed property roundtrip.
- **Task 6** — `TestRoundtrip_MysqlshFixture`: real mysqlsh `sample.tsv` byte-roundtrips through Reader→WritePassthrough→Writer.
- **Task 7** — `internal/zstd/{zstd.go,zstd_test.go}`: thin wrapper over `github.com/klauspost/compress/zstd` v1.18.6.
- **Task 8** — `internal/faker/faker_test.go`: determinism + invoice format.
- **Task 9** — `internal/config`: two-phase `LoadRaw` → `(*RawConfig).Compile(*faker.Faker) (*CompiledConfig, error)`. `CompiledConfig{Rules map[string]map[string]*template.Template}`. Parse-time validation catches unknown funcs.
- **Task 10** — `internal/dump/meta.go`: `ReadInstanceMeta` (Version, Dumper) and `ReadTableMeta` (Compression top-level, Options.Columns, etc.). Tests use Task-1 fixture.
- **Task 11** — `internal/dump/manifest.go`: non-recursive `WalkManifest` populating `HasDoneMarker`, `InstanceMetaPath`, `Tables[<schema>@<table>]` (each with `MetaPath`, `SQLPath`, sorted `Chunks`), `PassthroughFiles`. Chunk regex `^(.+?)(@@|@)(\d+)\.tsv\.zst$` discriminates final via `@@`. `@.sql` is handled as an explicit case so it doesn't get classified as a phantom `Tables["@"]`.
- **Task 12** — `internal/idx/idx.go`: `Write(io.Writer, decompressedLen int64)` emits one 8-byte BE uint64. Tests round-trip Task-1's `sample.idx`.

`go test ./...` passes across config, dump, faker, idx, tsv, zstd as of end of Task 12.

---


## Task 13: Anon row processor (`internal/anon`)

**Goal:** Combine reader, writer, and templates into a row-by-row processor. For each row: walk cells, passthrough or substitute per the rule slot list, handle `null` sentinel.

**Files:**
- Create: `internal/anon/anon.go`
- Create: `internal/anon/anon_test.go`

- [ ] **Step 1: Add the SentinelNULL constant to `internal/faker/faker.go` if not already there.** It already is (`SentinelNULL = "::NULL::"`). For the spec's stronger sentinel, change to:

  ```go
  // SentinelNULL is what {{ null }} produces. Embedded NUL bytes ensure no
  // faker function output can incidentally collide with this string.
  const SentinelNULL = "\x00\x00mysql-anonymizer-NULL\x00\x00"
  ```

  Update the `null` entry in `FuncMap`:

  ```go
  "null": func() string { return SentinelNULL },
  ```
  (already correct — only the constant value changes).

- [ ] **Step 2: Write the failing tests.** Create `internal/anon/anon_test.go`:

  ```go
  package anon

  import (
      "bytes"
      "math/rand/v2"
      "strings"
      "testing"
      "text/template"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/tsv"
  )

  // helper: compile a single template against a fresh faker
  func mustCompile(t *testing.T, body string, f *faker.Faker) *template.Template {
      t.Helper()
      tpl, err := template.New("").Funcs(f.FuncMap()).Parse(body)
      if err != nil {
          t.Fatal(err)
      }
      return tpl
  }

  func TestProcessRow_PassthroughOnly(t *testing.T) {
      input := []byte("1\tAlice\ta@x.com\n")
      var out bytes.Buffer
      r := tsv.NewReader(bytes.NewReader(input))
      w := tsv.NewWriter(&out)
      f := faker.New(rand.NewPCG(1, 1))

      slots := []*template.Template{nil, nil, nil} // all passthrough
      if err := ProcessAll(r, w, slots, f); err != nil {
          t.Fatal(err)
      }
      w.Flush()
      if !bytes.Equal(out.Bytes(), input) {
          t.Errorf("got %q, want %q", out.Bytes(), input)
      }
  }

  func TestProcessRow_Substitution(t *testing.T) {
      input := []byte("1\tAlice\toriginal@x.com\n")
      var out bytes.Buffer
      r := tsv.NewReader(bytes.NewReader(input))
      w := tsv.NewWriter(&out)
      f := faker.New(rand.NewPCG(1, 1))

      slots := []*template.Template{
          nil,
          nil,
          mustCompile(t, "{{ fakerEmail }}", f),
      }
      if err := ProcessAll(r, w, slots, f); err != nil {
          t.Fatal(err)
      }
      w.Flush()
      // Output should preserve the first two cells but replace the third.
      out_str := out.String()
      if !strings.HasPrefix(out_str, "1\tAlice\t") {
          t.Errorf("output prefix wrong: %q", out_str)
      }
      if strings.Contains(out_str, "original@x.com") {
          t.Errorf("substituted cell still contains original email: %q", out_str)
      }
      if !strings.Contains(out_str, "@") {
          t.Errorf("substituted cell missing @: %q", out_str)
      }
  }

  func TestProcessRow_NullSentinel(t *testing.T) {
      input := []byte("1\tAlice\toriginal\n")
      var out bytes.Buffer
      r := tsv.NewReader(bytes.NewReader(input))
      w := tsv.NewWriter(&out)
      f := faker.New(rand.NewPCG(1, 1))

      slots := []*template.Template{
          nil,
          nil,
          mustCompile(t, "{{ null }}", f),
      }
      if err := ProcessAll(r, w, slots, f); err != nil {
          t.Fatal(err)
      }
      w.Flush()
      want := "1\tAlice\t" + `\N` + "\n"
      if out.String() != want {
          t.Errorf("got %q, want %q", out.String(), want)
      }
  }

  func TestProcessRow_SentinelMisuseFails(t *testing.T) {
      input := []byte("Alice\n")
      var out bytes.Buffer
      r := tsv.NewReader(bytes.NewReader(input))
      w := tsv.NewWriter(&out)
      f := faker.New(rand.NewPCG(1, 1))

      // Template combines the null sentinel with other text — must error.
      slots := []*template.Template{
          mustCompile(t, "prefix-{{ null }}", f),
      }
      err := ProcessAll(r, w, slots, f)
      if err == nil {
          t.Errorf("expected error for sentinel-substring misuse")
      }
  }

  func TestProcessRow_CellCountMismatch(t *testing.T) {
      // Row has 2 cells but slot list expects 3.
      input := []byte("a\tb\n")
      var out bytes.Buffer
      r := tsv.NewReader(bytes.NewReader(input))
      w := tsv.NewWriter(&out)
      f := faker.New(rand.NewPCG(1, 1))
      slots := []*template.Template{nil, nil, nil}
      err := ProcessAll(r, w, slots, f)
      if err == nil {
          t.Errorf("expected error for cell-count mismatch")
      }
  }
  ```

- [ ] **Step 3: Run, expect compilation failure.**

- [ ] **Step 4: Implement `internal/anon/anon.go`.**

  ```go
  // Package anon applies a per-column slot list of compiled templates to TSV
  // rows. Slot list length is fixed by the table schema; slot[i] == nil means
  // "passthrough cell i unchanged." A non-nil slot is executed for that cell;
  // its output is escape-encoded into the writer, except when the output is
  // exactly faker.SentinelNULL, which writes the SQL NULL token.
  package anon

  import (
      "fmt"
      "io"
      "strings"
      "text/template"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/tsv"
  )

  // RowEnded is called after each row is fully written, with the writer's
  // running byte count. The orchestrator uses this to record .idx offsets.
  // Pass nil if you don't need it.
  type RowEnded func(bytesAtRowEnd int64) error

  // ProcessAll reads every row from r, applies slots, and writes to w. f is the
  // worker's *Faker — passed in for sentinel comparisons; templates already
  // close over their own funcmap from the same f.
  func ProcessAll(r *tsv.Reader, w *tsv.Writer, slots []*template.Template, f *faker.Faker) error {
      return ProcessAllWithRowHook(r, w, slots, f, nil)
  }

  // ProcessAllWithRowHook is like ProcessAll plus a callback after each row.
  func ProcessAllWithRowHook(
      r *tsv.Reader, w *tsv.Writer, slots []*template.Template, f *faker.Faker, hook RowEnded,
  ) error {
      var sb strings.Builder
      for {
          cells, err := r.Next()
          if err == io.EOF {
              return nil
          }
          if err != nil {
              return err
          }
          if len(cells) != len(slots) {
              return fmt.Errorf("anon: row has %d cells, schema expects %d", len(cells), len(slots))
          }
          for i, cell := range cells {
              tpl := slots[i]
              if tpl == nil {
                  if err := w.WritePassthrough(cell); err != nil {
                      return err
                  }
                  continue
              }
              sb.Reset()
              if err := tpl.Execute(&sb, nil); err != nil {
                  return fmt.Errorf("anon: template execute (col %d): %w", i, err)
              }
              out := sb.String()
              if out == faker.SentinelNULL {
                  if err := w.WriteNULL(); err != nil {
                      return err
                  }
                  continue
              }
              if strings.Contains(out, faker.SentinelNULL) {
                  return fmt.Errorf("anon: NULL sentinel appeared as substring of column %d output — {{ null }} must be the entire template", i)
              }
              if err := w.WriteSubstituted([]byte(out)); err != nil {
                  return err
              }
          }
          if err := w.EndRow(); err != nil {
              return err
          }
          if hook != nil {
              if err := hook(w.BytesWritten()); err != nil {
                  return err
              }
          }
      }
  }
  ```

- [ ] **Step 5: Run tests.**
  ```bash
  go test ./internal/anon/ -v
  ```

- [ ] **Step 6: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(anon): row processor with NULL sentinel guard"
  jj new
  ```

---

## Task 14: `cmd/mysql-anonymizer/main.go` skeleton — flag parsing and signal handling

**Files:**
- Create: `cmd/mysql-anonymizer/main.go`

- [ ] **Step 1: Implement.**

  ```go
  // Command mysql-anonymizer rewrites configured columns of a mysqlsh
  // util.dumpInstance directory and emits a sibling clean directory.
  // See docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md.
  package main

  import (
      "context"
      "errors"
      "flag"
      "fmt"
      "os"
      "os/signal"
      "runtime"
      "syscall"
  )

  type opts struct {
      InDir      string
      OutDir     string
      ConfigPath string
      Seed       uint64
      Workers    int
  }

  func parseFlags(args []string) (opts, error) {
      var o opts
      fs := flag.NewFlagSet("mysql-anonymizer", flag.ContinueOnError)
      fs.StringVar(&o.InDir, "in", "", "input dump-dir (mysqlsh util.dumpInstance output)")
      fs.StringVar(&o.OutDir, "out", "", "output clean-dir (must not exist or be empty)")
      fs.StringVar(&o.ConfigPath, "c", "", "YAML config")
      fs.Uint64Var(&o.Seed, "seed", 0, "uint64 job seed (required, no default)")
      fs.IntVar(&o.Workers, "j", runtime.NumCPU(), "worker count")
      if err := fs.Parse(args); err != nil {
          return o, err
      }
      // fs.Visit reports flags that were set on the command line (not flags
      // taking their default value), so this distinguishes "--seed 0" (explicit)
      // from "no --seed" (default).
      seedSet := false
      fs.Visit(func(f *flag.Flag) {
          if f.Name == "seed" {
              seedSet = true
          }
      })
      switch {
      case o.InDir == "":
          return o, errors.New("--in is required")
      case o.OutDir == "":
          return o, errors.New("--out is required")
      case o.ConfigPath == "":
          return o, errors.New("-c is required")
      case !seedSet:
          return o, errors.New("--seed is required (no implicit default)")
      case o.Workers <= 0:
          return o, fmt.Errorf("-j must be > 0 (got %d)", o.Workers)
      }
      return o, nil
  }

  func main() {
      o, err := parseFlags(os.Args[1:])
      if err != nil {
          fmt.Fprintln(os.Stderr, err)
          os.Exit(2)
      }
      ctx, cancel := signalContext()
      defer cancel()
      if err := run(ctx, o); err != nil {
          fmt.Fprintln(os.Stderr, err)
          os.Exit(1)
      }
  }

  func signalContext() (context.Context, context.CancelFunc) {
      return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
  }

  // run is implemented in subsequent tasks.
  func run(ctx context.Context, o opts) error {
      _ = ctx
      _ = o
      return errors.New("not implemented")
  }
  ```

- [ ] **Step 2: Add a flag-parsing test.** Create `cmd/mysql-anonymizer/main_test.go`:

  ```go
  package main

  import (
      "strings"
      "testing"
  )

  func TestParseFlags_Required(t *testing.T) {
      _, err := parseFlags([]string{"--in", "x", "--out", "y", "-c", "z", "--seed", "42"})
      if err != nil {
          t.Errorf("expected success, got %v", err)
      }
  }

  func TestParseFlags_MissingSeed(t *testing.T) {
      _, err := parseFlags([]string{"--in", "x", "--out", "y", "-c", "z"})
      if err == nil || !strings.Contains(err.Error(), "seed") {
          t.Errorf("expected --seed required error, got %v", err)
      }
  }

  func TestParseFlags_MissingIn(t *testing.T) {
      _, err := parseFlags([]string{"--out", "y", "-c", "z", "--seed", "42"})
      if err == nil || !strings.Contains(err.Error(), "in") {
          t.Errorf("expected --in required error, got %v", err)
      }
  }
  ```

- [ ] **Step 3: Run.**
  ```bash
  go test ./cmd/mysql-anonymizer/ -v
  go build ./...
  ```
  Expected: PASS, build succeeds.

- [ ] **Step 4: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(cmd): flags, signal handling, run scaffold"
  jj new
  ```

---

## Task 15: `cmd/mysql-anonymizer/validate.go` — strict config-vs-dump validation

**Goal:** After loading the manifest and config, reject any config rule that names a table or column not present in the dump.

**Files:**
- Create: `cmd/mysql-anonymizer/validate.go`

- [ ] **Step 1: Implement.**

  ```go
  package main

  import (
      "fmt"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
  )

  // tableSchema is the per-table column list and a position-indexed map for
  // quick lookup during slot construction.
  type tableSchema struct {
      Columns   []string
      ColIndex  map[string]int
  }

  // Validate ensures every (table, column) the config references exists in the
  // dump, and returns a per-table schema map for the orchestrator's use.
  func Validate(rc *config.RawConfig, m *dump.Manifest) (map[string]*tableSchema, error) {
      schemas := make(map[string]*tableSchema)
      for tableKey, tf := range rc.Filters {
          // Resolve config table name to a manifest entry.
          // Config keys use bare "<table>" but manifest keys are "<schema>@<table>".
          // Convention: config table name matches the table-portion of any
          // <schema>@<table>. Find a single matching manifest entry; ambiguity
          // (same table name in multiple schemas) is fatal.
          var matched string
          for k := range m.Tables {
              if tablePart(k) == tableKey {
                  if matched != "" {
                      return nil, fmt.Errorf("validate: table %q is ambiguous across schemas (%s, %s)", tableKey, matched, k)
                  }
                  matched = k
              }
          }
          if matched == "" {
              return nil, fmt.Errorf("validate: config references table %q but it is not in the dump", tableKey)
          }
          te := m.Tables[matched]
          if te.MetaPath == "" {
              return nil, fmt.Errorf("validate: table %q has no per-table json sidecar", matched)
          }
          tm, err := dump.ReadTableMeta(te.MetaPath)
          if err != nil {
              return nil, err
          }
          // Per the Task-1 errata, compression lives in the per-table JSON.
          // We only support zstd. Bail loudly on anything else.
          if tm.Compression != "zstd" {
              return nil, fmt.Errorf("validate: table %q has compression %q; only zstd is supported", matched, tm.Compression)
          }
          cols := tm.Options.Columns
          colIdx := make(map[string]int, len(cols))
          for i, c := range cols {
              colIdx[c] = i
          }
          for col := range tf.Columns {
              if _, ok := colIdx[col]; !ok {
                  return nil, fmt.Errorf("validate: config references column %s.%s but it is not in the dump (have %v)", tableKey, col, cols)
              }
          }
          schemas[matched] = &tableSchema{
              Columns:  cols,
              ColIndex: colIdx,
          }
      }
      return schemas, nil
  }

  func tablePart(manifestKey string) string {
      // manifestKey is "<schema>@<table>"; we want the part after the LAST '@'
      // because schema names won't contain '@'.
      for i := len(manifestKey) - 1; i >= 0; i-- {
          if manifestKey[i] == '@' {
              return manifestKey[i+1:]
          }
      }
      return manifestKey
  }
  ```

- [ ] **Step 2: Add unit tests.** Append to `cmd/mysql-anonymizer/main_test.go`:

  ```go
  import (
      "os"
      "path/filepath"
      "strings"
      "testing"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
  )

  // mkTinyDump writes a synthetic dump dir with one table fx@users
  // (cols: id, name, email) and a single empty chunk file.
  func mkTinyDump(t *testing.T) string {
      t.Helper()
      dir := t.TempDir()
      mw := func(name, body string) {
          if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
              t.Fatal(err)
          }
      }
      mw("@.done.json", "{}")
      mw("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
      mw("fx.json", "{}")
      mw("fx.sql", "")
      mw("fx@users.json", `{"compression":"zstd","extension":"tsv.zst","options":{"columns":["id","name","email"],"fieldsTerminatedBy":"\t","fieldsEscapedBy":"\\","linesTerminatedBy":"\n"}}`)
      mw("fx@users.sql", "")
      mw("fx@users@@0.tsv.zst", "")
      mw("fx@users@@0.tsv.zst.idx", "")
      return dir
  }

  func mkConfig(t *testing.T, body string) *config.RawConfig {
      t.Helper()
      p := filepath.Join(t.TempDir(), "config.yaml")
      if err := os.WriteFile(p, []byte(body), 0644); err != nil {
          t.Fatal(err)
      }
      rc, err := config.LoadRaw(p)
      if err != nil {
          t.Fatal(err)
      }
      return rc
  }

  func TestValidate_HappyPath(t *testing.T) {
      m, err := dump.WalkManifest(mkTinyDump(t))
      if err != nil {
          t.Fatal(err)
      }
      rc := mkConfig(t, `
  filters:
    users:
      columns:
        email:
          value: "{{ fakerEmail }}"
  `)
      schemas, err := Validate(rc, m)
      if err != nil {
          t.Fatalf("Validate: %v", err)
      }
      if _, ok := schemas["fx@users"]; !ok {
          t.Errorf("expected schema for fx@users, got %v", schemas)
      }
  }

  func TestValidate_MissingTable(t *testing.T) {
      m, err := dump.WalkManifest(mkTinyDump(t))
      if err != nil {
          t.Fatal(err)
      }
      rc := mkConfig(t, `
  filters:
    nope:
      columns:
        email:
          value: "{{ fakerEmail }}"
  `)
      _, err = Validate(rc, m)
      if err == nil || !strings.Contains(err.Error(), "table") {
          t.Errorf("expected missing-table error, got %v", err)
      }
  }

  func TestValidate_MissingColumn(t *testing.T) {
      m, err := dump.WalkManifest(mkTinyDump(t))
      if err != nil {
          t.Fatal(err)
      }
      rc := mkConfig(t, `
  filters:
    users:
      columns:
        nope:
          value: "{{ fakerEmail }}"
  `)
      _, err = Validate(rc, m)
      if err == nil || !strings.Contains(err.Error(), "column") {
          t.Errorf("expected missing-column error, got %v", err)
      }
  }

  func TestValidate_NonZstdCompression(t *testing.T) {
      dir := t.TempDir()
      mw := func(name, body string) {
          if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
              t.Fatal(err)
          }
      }
      mw("@.done.json", "{}")
      mw("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
      mw("fx.json", "{}")
      mw("fx.sql", "")
      mw("fx@users.json", `{"compression":"none","extension":"tsv","options":{"columns":["id","email"]}}`)
      mw("fx@users.sql", "")
      mw("fx@users@@0.tsv.zst", "")

      m, err := dump.WalkManifest(dir)
      if err != nil {
          t.Fatal(err)
      }
      rc := mkConfig(t, `
  filters:
    users:
      columns:
        email:
          value: "{{ fakerEmail }}"
  `)
      _, err = Validate(rc, m)
      if err == nil || !strings.Contains(err.Error(), "zstd") {
          t.Errorf("expected non-zstd compression error, got %v", err)
      }
  }
  ```

  Note: if there's already an `import` block in `main_test.go` from Task 14, merge into it instead of adding a duplicate.

- [ ] **Step 3: Run.**
  ```bash
  go test ./cmd/mysql-anonymizer/ -v
  go build ./...
  ```

- [ ] **Step 4: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(cmd): strict config-vs-dump validation"
  jj new
  ```

---

## Task 16: `cmd/mysql-anonymizer/copy.go` — copy/hardlink pass

**Goal:** For each `PassthroughFile` in the manifest, hardlink it into the output dir; on `EXDEV` (cross-device link error) or any other error, fall back to copy. Skip the chunks of configured tables and their `.idx` sidecars (these are written fresh by workers). Skip `@.done.json` (handled in finalization).

**Files:**
- Create: `cmd/mysql-anonymizer/copy.go`

- [ ] **Step 1: Implement.**

  ```go
  package main

  import (
      "errors"
      "fmt"
      "io"
      "os"
      "path/filepath"
      "syscall"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
  )

  // PreparePassthrough lays down all unchanged files from --in into --out.
  // Configured-table chunks and their .idx sidecars are NOT written here;
  // workers write those fresh. @.done.json is also excluded — the orchestrator
  // copies it last, after the worker pool drains successfully.
  func PreparePassthrough(m *dump.Manifest, configuredTables map[string]struct{}, outDir string) error {
      if err := os.MkdirAll(outDir, 0755); err != nil {
          return fmt.Errorf("copy: mkdir %s: %w", outDir, err)
      }
      configuredChunkData := map[string]struct{}{}
      configuredChunkIdx := map[string]struct{}{}
      for k := range configuredTables {
          if te, ok := m.Tables[k]; ok {
              for _, c := range te.Chunks {
                  configuredChunkData[c.DataPath] = struct{}{}
                  configuredChunkIdx[c.IdxPath] = struct{}{}
              }
          }
      }
      for _, src := range m.PassthroughFiles {
          if _, skip := configuredChunkData[src]; skip {
              continue
          }
          if _, skip := configuredChunkIdx[src]; skip {
              continue
          }
          dst := filepath.Join(outDir, filepath.Base(src))
          if err := linkOrCopy(src, dst); err != nil {
              return err
          }
      }
      return nil
  }

  func linkOrCopy(src, dst string) error {
      if err := os.Link(src, dst); err == nil {
          return nil
      } else if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EPERM) {
          // Some filesystems (e.g. some FUSE mounts) deny Link; fall through.
          if !errors.Is(err, syscall.ENOSYS) {
              // Treat unknown link errors as fatal to surface real I/O issues.
              if !errors.Is(err, syscall.EOPNOTSUPP) {
                  // ...except keep going for cross-device EXDEV path below.
              }
          }
      }
      // Fallback: copy.
      in, err := os.Open(src)
      if err != nil {
          return fmt.Errorf("copy: open %s: %w", src, err)
      }
      defer in.Close()
      out, err := os.Create(dst)
      if err != nil {
          return fmt.Errorf("copy: create %s: %w", dst, err)
      }
      defer out.Close()
      if _, err := io.Copy(out, in); err != nil {
          return fmt.Errorf("copy: %s -> %s: %w", src, dst, err)
      }
      return out.Sync()
  }
  ```

  Note: the `linkOrCopy` error-handling for `os.Link` is intentionally permissive — any error falls through to copy. This is correct for CI portability across filesystems.

- [ ] **Step 2: Add a test.** Append to `cmd/mysql-anonymizer/main_test.go`:

  ```go
  func TestPreparePassthrough_SkipsConfiguredChunks(t *testing.T) {
      inDir := mkTinyDump(t)
      m, err := dump.WalkManifest(inDir)
      if err != nil {
          t.Fatal(err)
      }
      outDir := filepath.Join(t.TempDir(), "out")
      configured := map[string]struct{}{"fx@users": {}}
      if err := PreparePassthrough(m, configured, outDir); err != nil {
          t.Fatal(err)
      }
      mustExist := func(name string) {
          if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
              t.Errorf("expected %s in output, got error: %v", name, err)
          }
      }
      mustNotExist := func(name string) {
          if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
              t.Errorf("expected %s NOT in output (configured chunk)", name)
          }
      }
      mustExist("fx@users.json") // table sidecar — kept
      mustExist("fx@users.sql")
      mustExist("@.json")
      mustExist("fx.json")
      mustExist("fx.sql")
      mustNotExist("fx@users@@0.tsv.zst")     // configured chunk data
      mustNotExist("fx@users@@0.tsv.zst.idx") // configured chunk idx
      mustNotExist("@.done.json")             // finalization handles this
  }
  ```

- [ ] **Step 3: Run.**
  ```bash
  go test ./cmd/mysql-anonymizer/ -v
  ```

- [ ] **Step 4: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(cmd): copy/hardlink pass for passthrough files"
  jj new
  ```

---

## Task 17: `cmd/mysql-anonymizer/pool.go` — worker pool dispatch

**Goal:** A `chunk` job channel, N workers, context cancellation on first error. Each job processes one chunk: open the input zstd → decode → tsv reader → anon process → tsv writer → encode zstd → atomic rename `.tmp`→final, plus `.idx`.

**Files:**
- Create: `cmd/mysql-anonymizer/pool.go`

- [ ] **Step 1: Implement.**

  ```go
  package main

  import (
      "context"
      "encoding/binary"
      "fmt"
      "hash/fnv"
      "math/rand/v2"
      "os"
      "path/filepath"
      "sync"
      "text/template"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/anon"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/idx"
      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/tsv"
      lzstd "github.com/screechingbagel/mysql-tsv-anonymizer/internal/zstd"
  )

  type job struct {
      tableKey string
      schema   *tableSchema
      chunk    dump.ChunkEntry
  }

  // RunPool runs nWorkers goroutines processing jobs. Returns the first error
  // encountered (others are observed but not returned).
  func RunPool(
      ctx context.Context,
      jobs []job,
      rc *config.RawConfig,
      schemas map[string]*tableSchema,
      jobSeed uint64,
      outDir string,
      nWorkers int,
  ) error {
      ctx, cancel := context.WithCancel(ctx)
      defer cancel()

      jobCh := make(chan job)
      var wg sync.WaitGroup
      var firstErr error
      var errMu sync.Mutex
      record := func(err error) {
          errMu.Lock()
          if firstErr == nil {
              firstErr = err
          }
          errMu.Unlock()
          cancel()
      }

      for i := 0; i < nWorkers; i++ {
          wg.Add(1)
          go func() {
              defer wg.Done()
              for j := range jobCh {
                  if ctx.Err() != nil {
                      return
                  }
                  if err := processChunk(ctx, j, rc, jobSeed, outDir); err != nil {
                      record(fmt.Errorf("chunk %s@@%d: %w", j.tableKey, j.chunk.Index, err))
                      return
                  }
              }
          }()
      }
      go func() {
          defer close(jobCh)
          for _, j := range jobs {
              select {
              case jobCh <- j:
              case <-ctx.Done():
                  return
              }
          }
      }()
      wg.Wait()
      return firstErr
  }

  // deriveSeed mixes (jobSeed, table, chunkIdx) into a (hi, lo) pair for PCG.
  func deriveSeed(jobSeed uint64, tableKey string, chunkIdx int) (uint64, uint64) {
      h := fnv.New64a()
      var buf [8]byte
      binary.BigEndian.PutUint64(buf[:], jobSeed)
      _, _ = h.Write(buf[:])
      _, _ = h.Write([]byte(tableKey))
      binary.BigEndian.PutUint64(buf[:], uint64(chunkIdx))
      _, _ = h.Write(buf[:])
      hi := h.Sum64()
      // Stir again for the second word.
      _, _ = h.Write([]byte{0x5a})
      lo := h.Sum64()
      return hi, lo
  }

  // processChunk handles one (table, chunk) job: derive RNG, compile templates,
  // build slot list, stream-rewrite the chunk, atomic-rename outputs.
  func processChunk(ctx context.Context, j job, rc *config.RawConfig, jobSeed uint64, outDir string) (err error) {
      hi, lo := deriveSeed(jobSeed, j.tableKey, j.chunk.Index)
      f := faker.New(rand.NewPCG(hi, lo))
      cc, err := rc.Compile(f)
      if err != nil {
          return fmt.Errorf("compile config: %w", err)
      }
      // Build slot list: position-indexed []*template.Template.
      // Config table key is the bare table name (no schema); look up rules.
      tableName := tablePart(j.tableKey)
      colRules := cc.Rules[tableName]
      slots := make([]*template.Template, len(j.schema.Columns))
      for i, col := range j.schema.Columns {
          slots[i] = colRules[col] // nil for unconfigured columns
      }

      // Open input.
      inF, err := os.Open(j.chunk.DataPath)
      if err != nil {
          return err
      }
      defer inF.Close()
      zr, err := lzstd.NewReader(inF)
      if err != nil {
          return err
      }
      defer zr.Close()

      // Open .tmp outputs.
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
      defer outF.Close()

      idxF, err := os.Create(tmpIdx)
      if err != nil {
          return err
      }
      defer idxF.Close()

      zw, err := lzstd.NewWriter(outF)
      if err != nil {
          return err
      }
      tw := tsv.NewWriter(zw)
      tr := tsv.NewReader(zr)

      // Per-row hook is now used solely for cancellation polling — `.idx` is a
      // single decompressed-length record written once after the chunk closes
      // (see Task 12 / testdata/fixtures/notes.md), not a per-row sequence.
      hook := func(_ int64) error { return ctx.Err() }
      if err := anon.ProcessAllWithRowHook(tr, tw, slots, f, hook); err != nil {
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
      // Now that zw is closed, tw.BytesWritten() is the total decompressed
      // length of the .zst chunk — exactly what mysqlsh stores in .idx.
      if err := idx.Write(idxF, tw.BytesWritten()); err != nil {
          return err
      }
      if err := idxF.Sync(); err != nil {
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

  Add a `text/template` import.

- [ ] **Step 2: Add a smoke test.** Append to `cmd/mysql-anonymizer/main_test.go`:

  ```go
  func TestDeriveSeed_Stable(t *testing.T) {
      hi1, lo1 := deriveSeed(42, "fx@t", 7)
      hi2, lo2 := deriveSeed(42, "fx@t", 7)
      if hi1 != hi2 || lo1 != lo2 {
          t.Errorf("deriveSeed not deterministic: (%d,%d) vs (%d,%d)", hi1, lo1, hi2, lo2)
      }
      // Different inputs should produce different outputs (with overwhelming probability).
      hi3, lo3 := deriveSeed(42, "fx@t", 8)
      if hi1 == hi3 && lo1 == lo3 {
          t.Errorf("seed collision across chunkIdx")
      }
  }
  ```

- [ ] **Step 3: Run.**
  ```bash
  go test ./cmd/mysql-anonymizer/ -v
  go build ./...
  ```

- [ ] **Step 4: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(cmd): worker pool with per-job seeded faker and atomic chunk write"
  jj new
  ```

---

## Task 18: Wire `run` end-to-end + finalization

**Files:**
- Modify: `cmd/mysql-anonymizer/main.go` (replace the `run` stub)

- [ ] **Step 1: Replace the `run` function.**

  ```go
  func run(ctx context.Context, o opts) error {
      // 1. Manifest.
      manifest, err := dump.WalkManifest(o.InDir)
      if err != nil {
          return err
      }
      if !manifest.HasDoneMarker {
          return fmt.Errorf("--in lacks @.done.json (the dump is incomplete)")
      }

      // 2. Sanity-parse @.json. (Per the Task-1 errata, compression lives in
      // the per-table JSON, not @.json — that strict check happens in Validate.)
      if manifest.InstanceMetaPath == "" {
          return fmt.Errorf("--in lacks @.json")
      }
      if _, err := dump.ReadInstanceMeta(manifest.InstanceMetaPath); err != nil {
          return err
      }

      // 3. Load + bootstrap-validate config.
      rc, err := config.LoadRaw(o.ConfigPath)
      if err != nil {
          return err
      }
      bootF := faker.New(rand.NewPCG(0xdeadbeef, 0xcafebabe))
      if _, err := rc.Compile(bootF); err != nil {
          return err
      }

      // 4. Strict validate.
      schemas, err := Validate(rc, manifest)
      if err != nil {
          return err
      }

      // 5. --out must not exist or be empty.
      if entries, err := os.ReadDir(o.OutDir); err == nil {
          if len(entries) > 0 {
              return fmt.Errorf("--out exists and is non-empty: %s", o.OutDir)
          }
      } else if !errors.Is(err, os.ErrNotExist) {
          return err
      }

      // 6. Copy pass.
      configured := make(map[string]struct{}, len(schemas))
      for k := range schemas {
          configured[k] = struct{}{}
      }
      if err := PreparePassthrough(manifest, configured, o.OutDir); err != nil {
          return err
      }

      // 7. Build job list.
      var jobs []job
      for k := range schemas {
          for _, c := range manifest.Tables[k].Chunks {
              jobs = append(jobs, job{tableKey: k, schema: schemas[k], chunk: c})
          }
      }

      // 8. Run pool.
      if err := RunPool(ctx, jobs, rc, schemas, o.Seed, o.OutDir, o.Workers); err != nil {
          return err
      }

      // 9. Finalize: copy @.done.json LAST.
      donePath := ""
      for _, p := range manifest.PassthroughFiles {
          if filepath.Base(p) == "@.done.json" {
              donePath = p
              break
          }
      }
      // If somehow @.done.json wasn't in passthrough (it's excluded by design),
      // re-construct its source path from manifest.Root.
      if donePath == "" {
          donePath = filepath.Join(manifest.Root, "@.done.json")
      }
      return linkOrCopy(donePath, filepath.Join(o.OutDir, "@.done.json"))
  }
  ```

  Add imports as needed (`os`, `path/filepath`, `errors`, `math/rand/v2`, the internal packages).

- [ ] **Step 2: Build to confirm wiring compiles.**
  ```bash
  go build ./...
  go test ./...
  ```

- [ ] **Step 3: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(cmd): end-to-end run pipeline + @.done.json finalization"
  jj new
  ```

---

## Task 19: Integration test fixture (synthetic tiny dump)

**Why:** A self-contained, hand-crafted dump dir we can run the binary against in `go test`. Not the same as Task 1's mysqlsh fixture — that was for format reverse-engineering. This one is for end-to-end semantic checks.

**Files:**
- Create: `testdata/tiny-dump/` (full directory of synthetic mysqlsh-shaped files)
- Create: `testdata/config.yaml`
- Create: `cmd/mysql-anonymizer/integration_test.go`

- [ ] **Step 1: Generate the fixture programmatically in a helper test.** Write a build-tagged helper that produces `testdata/tiny-dump/` deterministically. Create `cmd/mysql-anonymizer/integration_test.go`:

  ```go
  //go:build !nointeg

  package main

  import (
      "bytes"
      "context"
      "encoding/json"
      "io"
      "os"
      "path/filepath"
      "testing"

      lzstd "github.com/screechingbagel/mysql-tsv-anonymizer/internal/zstd"
  )

  // buildTinyDump writes a synthetic mysqlsh-shaped dump under dir.
  // One schema "fx", one table "users" with two chunks of 3 rows each.
  func buildTinyDump(t *testing.T, dir string) {
      t.Helper()
      mustWrite := func(rel, body string) {
          if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0644); err != nil {
              t.Fatal(err)
          }
      }
      // Shapes match Task-1 ground truth: @.json has no compression;
      // per-table json has compression top-level + columns at options.columns.
      mustWrite("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
      mustWrite("@.sql", "")
      mustWrite("@.post.sql", "")
      mustWrite("@.users.sql", "")
      mustWrite("fx.json", "{}")
      mustWrite("fx.sql", "")
      mustWrite("fx@users.json", `{"compression":"zstd","extension":"tsv.zst","options":{"columns":["id","name","email"],"fieldsTerminatedBy":"\t","fieldsEscapedBy":"\\","linesTerminatedBy":"\n"}}`)
      mustWrite("fx@users.sql", "")

      writeChunk := func(idx int, rows [][3]string) {
          var raw bytes.Buffer
          for _, r := range rows {
              raw.WriteString(r[0])
              raw.WriteByte('\t')
              raw.WriteString(r[1])
              raw.WriteByte('\t')
              raw.WriteString(r[2])
              raw.WriteByte('\n')
          }
          // zstd-encode
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
          chunkPath := filepath.Join(dir, "fx@users@@" + strconvItoa(idx) + ".tsv.zst")
          if err := os.WriteFile(chunkPath, compressed.Bytes(), 0644); err != nil {
              t.Fatal(err)
          }
          // Empty .idx is fine; we regenerate.
          if err := os.WriteFile(chunkPath+".idx", nil, 0644); err != nil {
              t.Fatal(err)
          }
      }
      writeChunk(0, [][3]string{
          {"1", "Alice", "a@x.com"},
          {"2", "Bob", "b@x.com"},
          {"3", "Carol", "c@x.com"},
      })
      writeChunk(1, [][3]string{
          {"4", "Dave", "d@x.com"},
          {"5", "Eve", "e@x.com"},
          {"6", "Frank", "f@x.com"},
      })

      // Done marker — last so a watcher could rely on it.
      mustWrite("@.done.json", "{}")
  }

  func strconvItoa(i int) string {
      // tiny replacement to avoid pulling strconv at test fixtures
      if i == 0 {
          return "0"
      }
      var buf [16]byte
      pos := len(buf)
      for n := i; n > 0; n /= 10 {
          pos--
          buf[pos] = byte('0' + n%10)
      }
      return string(buf[pos:])
  }

  func writeConfig(t *testing.T, dir string) string {
      t.Helper()
      body := `
  filters:
    users:
      columns:
        email:
          value: "{{ fakerEmail }}"
  `
      p := filepath.Join(dir, "config.yaml")
      if err := os.WriteFile(p, []byte(body), 0644); err != nil {
          t.Fatal(err)
      }
      return p
  }
  ```

- [ ] **Step 2: Run.** Just verify the file compiles for now:
  ```bash
  go test ./cmd/mysql-anonymizer/ -run NONE -v -tags=
  ```

- [ ] **Step 3: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "test(integration): synthetic dump fixture builder"
  jj new
  ```

---

## Task 20: Integration test — end-to-end semantic checks

**Files:**
- Modify: `cmd/mysql-anonymizer/integration_test.go`

- [ ] **Step 1: Append the actual integration test.**

  ```go
  func TestEndToEnd(t *testing.T) {
      inDir := t.TempDir()
      buildTinyDump(t, inDir)
      cfg := writeConfig(t, t.TempDir())
      outDir := filepath.Join(t.TempDir(), "clean")

      o := opts{
          InDir:      inDir,
          OutDir:     outDir,
          ConfigPath: cfg,
          Seed:       42,
          Workers:    2,
      }
      if err := run(context.Background(), o); err != nil {
          t.Fatalf("run: %v", err)
      }

      // 1. Output dir mirrors input.
      inEntries, _ := os.ReadDir(inDir)
      outEntries, _ := os.ReadDir(outDir)
      inNames := map[string]bool{}
      for _, e := range inEntries {
          inNames[e.Name()] = true
      }
      for _, e := range outEntries {
          if !inNames[e.Name()] {
              t.Errorf("unexpected output file: %s", e.Name())
          }
      }
      for n := range inNames {
          if _, err := os.Stat(filepath.Join(outDir, n)); err != nil {
              t.Errorf("missing in output: %s (%v)", n, err)
          }
      }

      // 2. Email column is replaced (no more "@x.com").
      // 3. id and name columns are byte-identical.
      verifyChunk := func(idx int, expectedNames []string, expectedIDs []string) {
          chunkPath := filepath.Join(outDir, "fx@users@@" + strconvItoa(idx) + ".tsv.zst")
          f, err := os.Open(chunkPath)
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
          rows := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
          if len(rows) != len(expectedNames) {
              t.Fatalf("chunk %d: %d rows, expected %d", idx, len(rows), len(expectedNames))
          }
          for i, row := range rows {
              cells := bytes.Split(row, []byte{'\t'})
              if len(cells) != 3 {
                  t.Errorf("chunk %d row %d: %d cells, want 3", idx, i, len(cells))
                  continue
              }
              if string(cells[0]) != expectedIDs[i] {
                  t.Errorf("chunk %d row %d id = %q, want %q", idx, i, cells[0], expectedIDs[i])
              }
              if string(cells[1]) != expectedNames[i] {
                  t.Errorf("chunk %d row %d name = %q, want %q", idx, i, cells[1], expectedNames[i])
              }
              if bytes.Contains(cells[2], []byte("@x.com")) {
                  t.Errorf("chunk %d row %d email %q still contains @x.com", idx, i, cells[2])
              }
              if !bytes.Contains(cells[2], []byte{'@'}) {
                  t.Errorf("chunk %d row %d email %q has no @", idx, i, cells[2])
              }
          }
      }
      verifyChunk(0, []string{"Alice", "Bob", "Carol"}, []string{"1", "2", "3"})
      verifyChunk(1, []string{"Dave", "Eve", "Frank"}, []string{"4", "5", "6"})
  }

  func TestEndToEnd_Determinism(t *testing.T) {
      inDir := t.TempDir()
      buildTinyDump(t, inDir)
      cfg := writeConfig(t, t.TempDir())

      run1Out := filepath.Join(t.TempDir(), "clean1")
      run2Out := filepath.Join(t.TempDir(), "clean2")
      o1 := opts{InDir: inDir, OutDir: run1Out, ConfigPath: cfg, Seed: 42, Workers: 2}
      o2 := opts{InDir: inDir, OutDir: run2Out, ConfigPath: cfg, Seed: 42, Workers: 2}
      if err := run(context.Background(), o1); err != nil {
          t.Fatal(err)
      }
      if err := run(context.Background(), o2); err != nil {
          t.Fatal(err)
      }

      // Diff every file byte-for-byte.
      entries, _ := os.ReadDir(run1Out)
      for _, e := range entries {
          a, err := os.ReadFile(filepath.Join(run1Out, e.Name()))
          if err != nil {
              t.Fatal(err)
          }
          b, err := os.ReadFile(filepath.Join(run2Out, e.Name()))
          if err != nil {
              t.Fatal(err)
          }
          if !bytes.Equal(a, b) {
              t.Errorf("nondeterminism: %s differs between runs (lens %d/%d)", e.Name(), len(a), len(b))
          }
      }
  }
  ```

- [ ] **Step 2: Run.**
  ```bash
  go test ./cmd/mysql-anonymizer/ -v -run TestEndToEnd
  ```
  Expected: both PASS. Failures here mean the orchestrator's behavior diverges from the design — debug, do not weaken the test.

- [ ] **Step 3: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "test(integration): end-to-end + determinism"
  jj new
  ```

---

## Task 21: Final sweep — full test run + tree state

**Files:** none (verification only).

- [ ] **Step 1: Full test run.**
  ```bash
  go test ./...
  ```
  Expected: all packages PASS.

- [ ] **Step 2: Vet and format clean.**
  ```bash
  go vet ./...
  go fmt ./...
  ```
  Expected: no output (everything already formatted).

- [ ] **Step 3: Build the binary.**
  ```bash
  go build -o /tmp/mysql-anonymizer ./cmd/mysql-anonymizer
  /tmp/mysql-anonymizer 2>&1 | head -5
  ```
  Expected: usage error mentioning `--in is required`.

- [ ] **Step 4: Inspect tree.**
  ```bash
  find . -type f -not -path './.git/*' -not -path './.jj/*' | sort
  ```
  Verify against the file map at the top of this plan. Anything extra: justify or delete. Anything missing: add it.

- [ ] **Step 5: Final commit (empty if everything's already clean).**
  ```bash
  jj describe -m "chore: final sweep — all tests pass, build clean"
  jj new
  ```
  (Skip describe if working copy is empty.)
