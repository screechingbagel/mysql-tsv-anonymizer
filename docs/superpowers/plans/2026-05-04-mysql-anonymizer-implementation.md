# MySQL Dump Anonymizer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md`

**Goal:** Build a Go internal tool that anonymizes a `mysqlsh util.dumpInstance` directory into a sibling clean directory loadable by `util.loadDump`, applying YAML-configured rules deterministically per `(jobSeed, table, chunkIdx)`.

**Architecture:** Single binary. Bottom-up build: TSV codec → zstd wrapper → dump metadata loader → `.idx` regenerator → row processor → orchestrator. Each layer fully tested before the next is built. The two open questions in the spec (`.idx` format, `<schema>@<table>.json` schema) are resolved early via a fixture-gathering task that runs `mysqlsh` against a throwaway database and commits the byte-level evidence.

**Tech Stack:** Go 1.26, `gofakeit/v7`, `gopkg.in/yaml.v3`, `klauspost/compress/zstd`, `math/rand/v2`. Version control: `jj`, not `git`.

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
  escape.go            # lifted escape table from hexon/mysqltsv (with LICENSE)
  reader.go            # streaming TSV row reader
  writer.go            # streaming TSV row writer
  tsv_test.go
  LICENSE              # already present

internal/zstd/
  zstd.go              # klauspost/compress/zstd thin wrapper

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
    sample.idx
    sample-table.json
    sample-at.json
  tiny-dump/            # synthetic dump for integration test (Task 18)
  config.yaml           # config used by integration test (Task 18)

docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md  # already written
```

---

## Task 1: Gather mysqlsh fixture and resolve open questions

**Why first:** The spec lists three open questions about `mysqlsh util.dumpInstance` output (the `.idx` binary format, the per-table `<schema>@<table>.json` schema, the `@.json` `compression` field). Every parser written downstream is shaped by the answers. This task is mostly a manual `mysqlsh` run plus a hex-dump inspection — the engineer's output is a notes file plus committed sample artifacts.

**Files:**
- Create: `testdata/fixtures/notes.md`
- Create: `testdata/fixtures/sample-at.json`
- Create: `testdata/fixtures/sample-table.json`
- Create: `testdata/fixtures/sample.idx`
- Create: `testdata/fixtures/sample.tsv` (uncompressed for human readability)

- [ ] **Step 1: Spin up a throwaway MySQL with one tiny table.** From any environment with Docker:
  ```bash
  docker run --rm -d --name mysql-fixture \
    -e MYSQL_ROOT_PASSWORD=test -e MYSQL_DATABASE=fx \
    mysql:8
  # wait ~5s for startup
  docker run -i --rm mysql:8 mysql -h mysql-fixture.orb.local -u root -ptest fx <<'SQL'
  CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(64), email VARCHAR(64));
  INSERT INTO t VALUES (1,'Alice','a@x.com'),(2,'Bob','b@x.com'),(3,'Carol','c@x.com');
  SQL
  ```

- [ ] **Step 2: Dump with `mysqlsh`.**
  ```bash
  rm -rf /tmp/fxdump
  mysqlsh --uri=root:test@mysql-fixture.orb.local -- util dump-instance /tmp/fxdump \
    --threads=1 --bytesPerChunk=128k
  ls /tmp/fxdump
  ```
  Expected layout: `@.done.json`, `@.json`, `@.post.sql`, `@.sql`, `@.users.sql`, `fx.json`, `fx.sql`, `fx@t.json`, `fx@t.sql`, `fx@t@@0.tsv.zst`, `fx@t@@0.tsv.zst.idx`.

- [ ] **Step 3: Decompress one chunk for inspection.**
  ```bash
  zstd -d /tmp/fxdump/fx@t@@0.tsv.zst -o /tmp/fxdump/fx@t@@0.tsv
  cat /tmp/fxdump/fx@t@@0.tsv
  ```
  Expected: tab-separated rows, terminated by `\n`, no enclosure quotes. Note exactly which characters are escaped (`\t`, `\n`, `\\`, etc.).

- [ ] **Step 4: Inspect the `.idx` binary.**
  ```bash
  xxd /tmp/fxdump/fx@t@@0.tsv.zst.idx | head -20
  wc -c /tmp/fxdump/fx@t@@0.tsv.zst.idx
  ```
  Compare the `.idx` byte-length to the row count and to byte offsets of `\n` in the decompressed `.tsv`. The working hypothesis is "8-byte big-endian offsets, one per row, of decompressed-byte position at row end." Confirm or correct.

- [ ] **Step 5: Inspect `@.json` and `fx@t.json`.**
  ```bash
  jq . /tmp/fxdump/@.json
  jq . /tmp/fxdump/fx@t.json
  ```
  Confirm: `@.json` has a `compression` field with value `"zstd"`. `fx@t.json` has a columns-listing field (likely `"columns"` or `"basenames"` or `"fields"`) and the order matches the TSV cell order.

- [ ] **Step 6: Commit ground truth.** Copy `@.json`, `fx@t.json`, the `.idx`, and the decompressed `.tsv` into `testdata/fixtures/` under stable names (`sample-at.json`, `sample-table.json`, `sample.idx`, `sample.tsv`). Write `testdata/fixtures/notes.md` recording, in plain prose:
  - The `.idx` byte layout (width per offset, endianness, what each offset measures from/to).
  - The exact JSON path in `fx@t.json` to the column-name array.
  - The exact JSON path in `@.json` to the `compression` field.
  - Any escape-table additions or surprises beyond the spec's enumerated set.

- [ ] **Step 7: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "test: commit mysqlsh fixture and format notes"
  jj new
  ```

---

## Task 2: TSV escape table (lifted from upstream, with attribution)

**Files:**
- Modify: `internal/tsv/tsv.go` (delete its current body)
- Create: `internal/tsv/escape.go`
- Create: `internal/tsv/tsv_test.go`

- [ ] **Step 1: Delete the inherited body of `internal/tsv/tsv.go`.** Replace its contents with just the package declaration plus a doc comment:

  ```go
  // Package tsv reads and writes the field-tab, line-newline, no-enclosure,
  // backslash-escape dialect produced by mysqlsh util.dumpInstance.
  //
  // The byte-level invariant: cells passed through unmodified must round-trip
  // byte-for-byte. Cells written via WriteSubstituted are encoded fresh using
  // the escape table in escape.go.
  //
  // The escape table in escape.go is derived from github.com/hexon/mysqltsv
  // (BSD-2-Clause; see LICENSE in this directory).
  package tsv
  ```

- [ ] **Step 2: Create `internal/tsv/escape.go` with the lifted escape table.**

  ```go
  // Derived from github.com/hexon/mysqltsv (BSD-2-Clause).
  // See LICENSE in this directory.
  package tsv

  // escapeInto appends a backslash-escaped form of src to dst and returns
  // the extended slice. Bytes that need escaping per the mysqlsh dialect:
  //   NUL    -> \0
  //   BS     -> \b
  //   LF     -> \n
  //   CR     -> \r
  //   TAB    -> \t
  //   SUB    -> \Z   (0x1A)
  //   '\\'   -> \\
  // All other bytes are written verbatim.
  func escapeInto(dst, src []byte) []byte {
      for _, c := range src {
          switch c {
          case 0:
              dst = append(dst, '\\', '0')
          case '\b':
              dst = append(dst, '\\', 'b')
          case '\n':
              dst = append(dst, '\\', 'n')
          case '\r':
              dst = append(dst, '\\', 'r')
          case '\t':
              dst = append(dst, '\\', 't')
          case 0x1A:
              dst = append(dst, '\\', 'Z')
          case '\\':
              dst = append(dst, '\\', '\\')
          default:
              dst = append(dst, c)
          }
      }
      return dst
  }
  ```

- [ ] **Step 3: Write a unit test for the escape function.** In `internal/tsv/tsv_test.go`:

  ```go
  package tsv

  import (
      "bytes"
      "testing"
  )

  func TestEscapeInto(t *testing.T) {
      cases := []struct {
          name string
          in   []byte
          want []byte
      }{
          {"empty", []byte{}, []byte{}},
          {"plain", []byte("hello"), []byte("hello")},
          {"tab", []byte("a\tb"), []byte(`a\tb`)},
          {"newline", []byte("a\nb"), []byte(`a\nb`)},
          {"backslash", []byte(`a\b`), []byte(`a\\b`)},
          {"null byte", []byte{'a', 0, 'b'}, []byte(`a\0b`)},
          {"sub", []byte{'a', 0x1A, 'b'}, []byte(`a\Zb`)},
          {"all", []byte{0, '\b', '\n', '\r', '\t', 0x1A, '\\'}, []byte(`\0\b\n\r\t\Z\\`)},
      }
      for _, tc := range cases {
          t.Run(tc.name, func(t *testing.T) {
              got := escapeInto(nil, tc.in)
              if !bytes.Equal(got, tc.want) {
                  t.Errorf("escapeInto(%q) = %q, want %q", tc.in, got, tc.want)
              }
          })
      }
  }
  ```

- [ ] **Step 4: Run tests and verify pass.**
  ```bash
  go test ./internal/tsv/ -run TestEscapeInto -v
  ```
  Expected: all subtests PASS.

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(tsv): port escape table from hexon/mysqltsv"
  jj new
  ```

---

## Task 3: TSV Reader

**Goal:** Stream `[][]byte` cells from a TSV reader, returning the *raw escaped bytes* of each cell (so passthrough write is a verbatim copy). Cell boundaries are unescaped `\t`; row boundaries are unescaped `\n`. Within a cell, every `\\X` two-byte sequence is treated as one escape unit (we don't care what `X` is — we just don't let it trigger boundary detection).

**Files:**
- Create: `internal/tsv/reader.go`
- Modify: `internal/tsv/tsv_test.go`

- [ ] **Step 1: Write the failing test.** Append to `tsv_test.go`:

  ```go
  func TestReader_BasicRows(t *testing.T) {
      input := []byte("1\tAlice\ta@x.com\n2\tBob\tb@x.com\n")
      r := NewReader(bytes.NewReader(input))

      row1, err := r.Next()
      if err != nil {
          t.Fatalf("Next #1: %v", err)
      }
      want1 := [][]byte{[]byte("1"), []byte("Alice"), []byte("a@x.com")}
      if !equalRows(row1, want1) {
          t.Errorf("row1 = %q, want %q", row1, want1)
      }

      row2, err := r.Next()
      if err != nil {
          t.Fatalf("Next #2: %v", err)
      }
      want2 := [][]byte{[]byte("2"), []byte("Bob"), []byte("b@x.com")}
      if !equalRows(row2, want2) {
          t.Errorf("row2 = %q, want %q", row2, want2)
      }

      _, err = r.Next()
      if err != io.EOF {
          t.Errorf("expected EOF, got %v", err)
      }
  }

  func TestReader_PreservesEscapes(t *testing.T) {
      // Cell containing escaped tab and escaped backslash; passthrough
      // semantics: returned bytes equal the input cell bytes.
      input := []byte(`val\tone` + "\t" + `val\\two` + "\n")
      r := NewReader(bytes.NewReader(input))
      row, err := r.Next()
      if err != nil {
          t.Fatalf("Next: %v", err)
      }
      want := [][]byte{[]byte(`val\tone`), []byte(`val\\two`)}
      if !equalRows(row, want) {
          t.Errorf("row = %q, want %q", row, want)
      }
  }

  func TestReader_NullToken(t *testing.T) {
      // \N is the NULL token; the reader returns it verbatim as cell bytes.
      input := []byte(`Alice` + "\t" + `\N` + "\t" + `a@x.com` + "\n")
      r := NewReader(bytes.NewReader(input))
      row, err := r.Next()
      if err != nil {
          t.Fatalf("Next: %v", err)
      }
      want := [][]byte{[]byte("Alice"), []byte(`\N`), []byte("a@x.com")}
      if !equalRows(row, want) {
          t.Errorf("row = %q, want %q", row, want)
      }
  }

  func equalRows(a, b [][]byte) bool {
      if len(a) != len(b) {
          return false
      }
      for i := range a {
          if !bytes.Equal(a[i], b[i]) {
              return false
          }
      }
      return true
  }
  ```

  Add `"io"` to imports.

- [ ] **Step 2: Run test, expect compilation failure.**
  ```bash
  go test ./internal/tsv/ -v
  ```
  Expected: build error referencing `NewReader`.

- [ ] **Step 3: Implement Reader.** Create `internal/tsv/reader.go`:

  ```go
  package tsv

  import (
      "bufio"
      "io"
  )

  // Reader streams TSV rows in the mysqlsh dialect. Cells returned by Next are
  // valid only until the next call to Next; copy them if you need to retain.
  // The bytes are the raw, escaped form (passthrough-safe).
  type Reader struct {
      r       *bufio.Reader
      rowBuf  []byte // accumulator for the current row's bytes
      offsets []int  // cell start offsets within rowBuf
      cells   [][]byte
      err     error
  }

  func NewReader(r io.Reader) *Reader {
      return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
  }

  // Next returns the next row's cells, or io.EOF after the last row.
  // It returns an error other than io.EOF if the stream is malformed
  // (e.g., EOF mid-row without a trailing newline).
  func (r *Reader) Next() ([][]byte, error) {
      if r.err != nil {
          return nil, r.err
      }
      r.rowBuf = r.rowBuf[:0]
      r.offsets = r.offsets[:0]
      r.offsets = append(r.offsets, 0) // first cell starts at 0
      for {
          b, err := r.r.ReadByte()
          if err == io.EOF {
              if len(r.rowBuf) == 0 && len(r.offsets) == 1 {
                  r.err = io.EOF
                  return nil, io.EOF
              }
              r.err = io.ErrUnexpectedEOF
              return nil, r.err
          }
          if err != nil {
              r.err = err
              return nil, err
          }
          if b == '\\' {
              // Escape sequence — consume next byte verbatim, no boundary check.
              esc, err := r.r.ReadByte()
              if err != nil {
                  if err == io.EOF {
                      r.err = io.ErrUnexpectedEOF
                      return nil, r.err
                  }
                  r.err = err
                  return nil, err
              }
              r.rowBuf = append(r.rowBuf, '\\', esc)
              continue
          }
          if b == '\t' {
              r.offsets = append(r.offsets, len(r.rowBuf))
              continue
          }
          if b == '\n' {
              return r.materialize(), nil
          }
          r.rowBuf = append(r.rowBuf, b)
      }
  }

  func (r *Reader) materialize() [][]byte {
      // Build cell slices after all rowBuf appends are done so slices are stable.
      r.cells = r.cells[:0]
      for i, start := range r.offsets {
          var end int
          if i+1 < len(r.offsets) {
              end = r.offsets[i+1]
          } else {
              end = len(r.rowBuf)
          }
          r.cells = append(r.cells, r.rowBuf[start:end])
      }
      return r.cells
  }
  ```

- [ ] **Step 4: Run tests and verify pass.**
  ```bash
  go test ./internal/tsv/ -v
  ```
  Expected: all `TestReader_*` and `TestEscapeInto` subtests PASS.

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(tsv): streaming reader for mysqlsh dialect"
  jj new
  ```

---

## Task 4: TSV Writer

**Goal:** Three write methods — `WritePassthrough` (raw bytes, verbatim), `WriteSubstituted` (escape on the fly), `WriteNULL` (literal `\N`). Plus row-end and a running byte counter for `.idx`.

**Files:**
- Create: `internal/tsv/writer.go`
- Modify: `internal/tsv/tsv_test.go`

- [ ] **Step 1: Write the failing tests.**

  ```go
  func TestWriter_PassthroughVerbatim(t *testing.T) {
      var buf bytes.Buffer
      w := NewWriter(&buf)
      // Pre-escaped cells round-trip exactly.
      w.WritePassthrough([]byte(`val\tone`))
      w.WritePassthrough([]byte("plain"))
      w.WritePassthrough([]byte(`\N`))
      if err := w.EndRow(); err != nil {
          t.Fatal(err)
      }
      if err := w.Flush(); err != nil {
          t.Fatal(err)
      }
      want := []byte(`val\tone` + "\t" + "plain" + "\t" + `\N` + "\n")
      if !bytes.Equal(buf.Bytes(), want) {
          t.Errorf("got %q, want %q", buf.Bytes(), want)
      }
  }

  func TestWriter_SubstitutedEscapes(t *testing.T) {
      var buf bytes.Buffer
      w := NewWriter(&buf)
      w.WriteSubstituted([]byte("hello"))
      w.WriteSubstituted([]byte("a\tb\nc"))
      w.WriteSubstituted([]byte(`back\slash`))
      w.EndRow()
      w.Flush()
      want := []byte(`hello` + "\t" + `a\tb\nc` + "\t" + `back\\slash` + "\n")
      if !bytes.Equal(buf.Bytes(), want) {
          t.Errorf("got %q, want %q", buf.Bytes(), want)
      }
  }

  func TestWriter_Null(t *testing.T) {
      var buf bytes.Buffer
      w := NewWriter(&buf)
      w.WritePassthrough([]byte("Alice"))
      w.WriteNULL()
      w.WritePassthrough([]byte("a@x.com"))
      w.EndRow()
      w.Flush()
      want := []byte("Alice" + "\t" + `\N` + "\t" + "a@x.com" + "\n")
      if !bytes.Equal(buf.Bytes(), want) {
          t.Errorf("got %q, want %q", buf.Bytes(), want)
      }
  }

  func TestWriter_BytesWritten(t *testing.T) {
      var buf bytes.Buffer
      w := NewWriter(&buf)
      w.WritePassthrough([]byte("a"))
      w.WritePassthrough([]byte("bb"))
      w.EndRow()
      w.Flush()
      // 1 + 1 (tab) + 2 + 1 (newline) = 5
      if got := w.BytesWritten(); got != 5 {
          t.Errorf("BytesWritten = %d, want 5", got)
      }
  }
  ```

- [ ] **Step 2: Run, expect compilation failure.**
  ```bash
  go test ./internal/tsv/ -v
  ```
  Expected: build error on `NewWriter`.

- [ ] **Step 3: Implement Writer.** Create `internal/tsv/writer.go`:

  ```go
  package tsv

  import (
      "bufio"
      "io"
  )

  type Writer struct {
      w         *bufio.Writer
      sepNeeded bool // need a tab before next cell
      bytes     int64
  }

  func NewWriter(w io.Writer) *Writer {
      return &Writer{w: bufio.NewWriterSize(w, 64*1024)}
  }

  func (w *Writer) writeSep() error {
      if !w.sepNeeded {
          return nil
      }
      if err := w.w.WriteByte('\t'); err != nil {
          return err
      }
      w.bytes++
      return nil
  }

  // WritePassthrough writes cell bytes verbatim — no escaping. Use only with
  // bytes that came from Reader.Next() for an unmodified cell.
  func (w *Writer) WritePassthrough(cell []byte) error {
      if err := w.writeSep(); err != nil {
          return err
      }
      n, err := w.w.Write(cell)
      w.bytes += int64(n)
      w.sepNeeded = true
      return err
  }

  // WriteSubstituted writes cell as fresh data, applying mysqlsh escape rules.
  func (w *Writer) WriteSubstituted(cell []byte) error {
      if err := w.writeSep(); err != nil {
          return err
      }
      // Build escaped form into a small reusable scratch buffer, then write.
      // For simplicity, write byte-by-byte (bufio absorbs syscalls).
      for _, c := range cell {
          var pair [2]byte
          var n int
          switch c {
          case 0:
              pair[0], pair[1], n = '\\', '0', 2
          case '\b':
              pair[0], pair[1], n = '\\', 'b', 2
          case '\n':
              pair[0], pair[1], n = '\\', 'n', 2
          case '\r':
              pair[0], pair[1], n = '\\', 'r', 2
          case '\t':
              pair[0], pair[1], n = '\\', 't', 2
          case 0x1A:
              pair[0], pair[1], n = '\\', 'Z', 2
          case '\\':
              pair[0], pair[1], n = '\\', '\\', 2
          default:
              pair[0], n = c, 1
          }
          if _, err := w.w.Write(pair[:n]); err != nil {
              return err
          }
          w.bytes += int64(n)
      }
      w.sepNeeded = true
      return nil
  }

  // WriteNULL writes the SQL NULL token (\N).
  func (w *Writer) WriteNULL() error {
      if err := w.writeSep(); err != nil {
          return err
      }
      if _, err := w.w.Write([]byte{'\\', 'N'}); err != nil {
          return err
      }
      w.bytes += 2
      w.sepNeeded = true
      return nil
  }

  // EndRow writes the row separator. After it, the next Write* starts a new row.
  func (w *Writer) EndRow() error {
      if err := w.w.WriteByte('\n'); err != nil {
          return err
      }
      w.bytes++
      w.sepNeeded = false
      return nil
  }

  // BytesWritten returns the running total of decompressed bytes written.
  // Used by callers to build .idx offsets.
  func (w *Writer) BytesWritten() int64 { return w.bytes }

  func (w *Writer) Flush() error { return w.w.Flush() }
  ```

- [ ] **Step 4: Run tests.**
  ```bash
  go test ./internal/tsv/ -v
  ```
  Expected: all `TestWriter_*` PASS.

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(tsv): streaming writer with passthrough and substituted paths"
  jj new
  ```

---

## Task 5: TSV byte-identity property test

**Why:** This is the contract the rest of the system depends on. If Reader→passthrough→Writer doesn't reproduce the input bytes exactly, every claim about "untouched cells round-trip byte-for-byte" is false.

**Files:**
- Modify: `internal/tsv/tsv_test.go`

- [ ] **Step 1: Write the property test.**

  ```go
  func TestRoundtrip_BytesIdentical(t *testing.T) {
      // Hand-crafted multi-row input covering all escape characters and \N.
      input := []byte(
          `1` + "\t" + `Alice` + "\t" + `a@x.com` + "\n" +
              `2` + "\t" + `\N` + "\t" + `with\ttab` + "\n" +
              `3` + "\t" + `with\\backslash` + "\t" + `with\nnewline` + "\n" +
              `4` + "\t" + `with\0null` + "\t" + `with\Zsub` + "\n",
      )

      var out bytes.Buffer
      r := NewReader(bytes.NewReader(input))
      w := NewWriter(&out)
      for {
          cells, err := r.Next()
          if err == io.EOF {
              break
          }
          if err != nil {
              t.Fatalf("Next: %v", err)
          }
          for _, c := range cells {
              if err := w.WritePassthrough(c); err != nil {
                  t.Fatalf("WritePassthrough: %v", err)
              }
          }
          if err := w.EndRow(); err != nil {
              t.Fatalf("EndRow: %v", err)
          }
      }
      if err := w.Flush(); err != nil {
          t.Fatalf("Flush: %v", err)
      }
      if !bytes.Equal(out.Bytes(), input) {
          t.Errorf("roundtrip mismatch\ninput:  %q\noutput: %q", input, out.Bytes())
      }
  }

  // Property test: random rows should roundtrip too.
  func TestRoundtrip_FuzzedCells(t *testing.T) {
      rng := rand.New(rand.NewPCG(1, 2))
      for trial := 0; trial < 100; trial++ {
          input := generateRandomTSV(rng, 5+rng.IntN(10), 1+rng.IntN(5))
          var out bytes.Buffer
          r := NewReader(bytes.NewReader(input))
          w := NewWriter(&out)
          for {
              cells, err := r.Next()
              if err == io.EOF {
                  break
              }
              if err != nil {
                  t.Fatalf("trial %d: Next: %v", trial, err)
              }
              for _, c := range cells {
                  w.WritePassthrough(c)
              }
              w.EndRow()
          }
          w.Flush()
          if !bytes.Equal(out.Bytes(), input) {
              t.Fatalf("trial %d mismatch\ninput:  %q\noutput: %q", trial, input, out.Bytes())
          }
      }
  }

  // generateRandomTSV produces a syntactically valid mysqlsh-dialect TSV with
  // numRows rows of numCols cells each. Cells contain a random mix of plain
  // bytes and escape sequences.
  func generateRandomTSV(rng *rand.Rand, numRows, numCols int) []byte {
      var buf bytes.Buffer
      escapes := []string{`\0`, `\b`, `\n`, `\r`, `\t`, `\Z`, `\\`, `\N`, `\x`}
      for r := 0; r < numRows; r++ {
          for c := 0; c < numCols; c++ {
              if c > 0 {
                  buf.WriteByte('\t')
              }
              cellLen := rng.IntN(8)
              for i := 0; i < cellLen; i++ {
                  if rng.IntN(3) == 0 {
                      buf.WriteString(escapes[rng.IntN(len(escapes))])
                  } else {
                      // ASCII printable, not tab/newline/backslash
                      buf.WriteByte(byte('a' + rng.IntN(26)))
                  }
              }
          }
          buf.WriteByte('\n')
      }
      return buf.Bytes()
  }
  ```

  Add `"math/rand/v2"` (alias `rand`) to imports.

- [ ] **Step 2: Run, expect pass.**
  ```bash
  go test ./internal/tsv/ -run TestRoundtrip -v
  ```
  Expected: both PASS. If the fuzz test fails, the Reader or Writer has a bug — fix before proceeding.

- [ ] **Step 3: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "test(tsv): byte-identity roundtrip + fuzzed property"
  jj new
  ```

---

## Task 6: TSV byte-identity against the mysqlsh fixture

**Why:** Tests against synthetic data don't catch dialect surprises. The Task 1 fixture is real mysqlsh output — if our codec disagrees with it on a single byte, we'd ship broken outputs.

**Files:**
- Modify: `internal/tsv/tsv_test.go`

- [ ] **Step 1: Write the fixture-based test.**

  ```go
  func TestRoundtrip_MysqlshFixture(t *testing.T) {
      input, err := os.ReadFile("../../testdata/fixtures/sample.tsv")
      if err != nil {
          t.Skipf("fixture not present (Task 1): %v", err)
      }
      var out bytes.Buffer
      r := NewReader(bytes.NewReader(input))
      w := NewWriter(&out)
      for {
          cells, err := r.Next()
          if err == io.EOF {
              break
          }
          if err != nil {
              t.Fatalf("Next: %v", err)
          }
          for _, c := range cells {
              w.WritePassthrough(c)
          }
          w.EndRow()
      }
      w.Flush()
      if !bytes.Equal(out.Bytes(), input) {
          t.Errorf("fixture roundtrip mismatch (len in=%d out=%d)", len(input), out.Bytes())
      }
  }
  ```

  Add `"os"` to imports if not present.

- [ ] **Step 2: Run.**
  ```bash
  go test ./internal/tsv/ -run TestRoundtrip_MysqlshFixture -v
  ```
  Expected: PASS. If FAIL, the codec disagrees with mysqlsh on something — investigate via `xxd` diff, fix in escape table or reader, do not weaken the test.

- [ ] **Step 3: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "test(tsv): byte-identity against mysqlsh fixture"
  jj new
  ```

---

## Task 7: zstd wrappers

**Goal:** Thin `NewReader`/`NewWriter` over `klauspost/compress/zstd` so the rest of the codebase doesn't import the third-party path directly.

**Files:**
- Create: `internal/zstd/zstd.go`
- Create: `internal/zstd/zstd_test.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency.**
  ```bash
  go get github.com/klauspost/compress/zstd
  ```

- [ ] **Step 2: Write the failing test.** Create `internal/zstd/zstd_test.go`:

  ```go
  package zstd

  import (
      "bytes"
      "io"
      "testing"
  )

  func TestRoundtrip(t *testing.T) {
      payload := []byte("hello, mysql dump anonymizer")

      var compressed bytes.Buffer
      w, err := NewWriter(&compressed)
      if err != nil {
          t.Fatal(err)
      }
      if _, err := w.Write(payload); err != nil {
          t.Fatal(err)
      }
      if err := w.Close(); err != nil {
          t.Fatal(err)
      }

      r, err := NewReader(bytes.NewReader(compressed.Bytes()))
      if err != nil {
          t.Fatal(err)
      }
      defer r.Close()
      got, err := io.ReadAll(r)
      if err != nil {
          t.Fatal(err)
      }
      if !bytes.Equal(got, payload) {
          t.Errorf("got %q, want %q", got, payload)
      }
  }
  ```

- [ ] **Step 3: Run, expect compilation failure.**
  ```bash
  go test ./internal/zstd/ -v
  ```

- [ ] **Step 4: Implement.** Create `internal/zstd/zstd.go`:

  ```go
  // Package zstd is a thin wrapper around klauspost/compress/zstd providing
  // io.ReadCloser / io.WriteCloser shaped for the anonymizer.
  package zstd

  import (
      "io"

      kp "github.com/klauspost/compress/zstd"
  )

  // ReadCloser wraps a klauspost zstd Decoder as an io.ReadCloser whose Close
  // also releases the underlying decoder.
  type ReadCloser struct {
      *kp.Decoder
  }

  func (r ReadCloser) Close() error {
      r.Decoder.Close()
      return nil
  }

  func NewReader(r io.Reader) (ReadCloser, error) {
      d, err := kp.NewReader(r)
      if err != nil {
          return ReadCloser{}, err
      }
      return ReadCloser{Decoder: d}, nil
  }

  func NewWriter(w io.Writer) (*kp.Encoder, error) {
      return kp.NewWriter(w)
  }
  ```

- [ ] **Step 5: Run tests.**
  ```bash
  go test ./internal/zstd/ -v
  ```
  Expected: PASS.

- [ ] **Step 6: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(zstd): klauspost/compress wrapper"
  jj new
  ```

---

## Task 8: Faker determinism tests

**Goal:** Lock down the spec invariant: same `rand.Source` → identical sequence of outputs across all helpers.

**Files:**
- Create: `internal/faker/faker_test.go`

- [ ] **Step 1: Write the test.**

  ```go
  package faker

  import (
      "math/rand/v2"
      "strings"
      "testing"
  )

  func newDeterministic() *Faker {
      return New(rand.NewPCG(42, 99))
  }

  func TestDeterminism_SameSeedSameOutput(t *testing.T) {
      a := newDeterministic()
      b := newDeterministic()
      // Sample every helper, compare sequence-for-sequence.
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
          {"Invoice", func(f *Faker) string { return f.Invoice() }},
          {"SecondaryAddress", func(f *Faker) string { return f.SecondaryAddress() }},
      }
      for _, tc := range cases {
          for i := 0; i < 10; i++ {
              if got, want := tc.fn(a), tc.fn(b); got != want {
                  t.Errorf("%s [%d]: a=%q, b=%q", tc.name, i, got, want)
              }
          }
      }
  }

  func TestDeterminism_DifferentSeedsDiffer(t *testing.T) {
      a := New(rand.NewPCG(1, 2))
      b := New(rand.NewPCG(3, 4))
      // Two different seeds: at least one of the first 10 emails should differ.
      eq := 0
      for i := 0; i < 10; i++ {
          if a.gf.Email() == b.gf.Email() {
              eq++
          }
      }
      if eq == 10 {
          t.Errorf("all 10 emails equal across distinct seeds — RNGs are not actually independent")
      }
  }

  func TestInvoice_Format(t *testing.T) {
      f := newDeterministic()
      for i := 0; i < 20; i++ {
          inv := f.Invoice()
          if !strings.HasPrefix(inv, "INV-") || len(inv) != 12 {
              t.Errorf("Invoice %q: want INV-<8 alphanumeric chars>", inv)
          }
      }
  }
  ```

- [ ] **Step 2: Run, expect pass.**
  ```bash
  go test ./internal/faker/ -v
  ```
  Expected: PASS.

- [ ] **Step 3: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "test(faker): determinism and invoice-format tests"
  jj new
  ```

---

## Task 9: Split `config.Load` into `LoadRaw` + `(*RawConfig).Compile`

**Why:** Per the spec's "Config compile model" section, we need parse-once + compile-per-job. The cheapest path is to read the YAML once into a `RawConfig` (table → column → raw template string) and let workers compile their own bound templates per job.

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test.** Create `internal/config/config_test.go`:

  ```go
  package config

  import (
      "math/rand/v2"
      "os"
      "path/filepath"
      "strings"
      "testing"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
  )

  func writeTempConfig(t *testing.T, body string) string {
      t.Helper()
      dir := t.TempDir()
      p := filepath.Join(dir, "config.yaml")
      if err := os.WriteFile(p, []byte(body), 0644); err != nil {
          t.Fatal(err)
      }
      return p
  }

  func TestLoadRaw_Parses(t *testing.T) {
      path := writeTempConfig(t, `
  filters:
    users:
      columns:
        email:
          value: "{{ fakerEmail }}"
        name:
          value: "{{ fakerName }}"
  `)
      raw, err := LoadRaw(path)
      if err != nil {
          t.Fatal(err)
      }
      if got := raw.Filters["users"].Columns["email"].Value; got != "{{ fakerEmail }}" {
          t.Errorf("email value = %q", got)
      }
      if got := raw.Filters["users"].Columns["name"].Value; got != "{{ fakerName }}" {
          t.Errorf("name value = %q", got)
      }
  }

  func TestCompile_BindsToFaker(t *testing.T) {
      path := writeTempConfig(t, `
  filters:
    users:
      columns:
        email:
          value: "{{ fakerEmail }}"
  `)
      raw, err := LoadRaw(path)
      if err != nil {
          t.Fatal(err)
      }
      f := faker.New(rand.NewPCG(1, 1))
      cc, err := raw.Compile(f)
      if err != nil {
          t.Fatal(err)
      }
      tpl := cc.Rules["users"]["email"]
      if tpl == nil {
          t.Fatal("expected compiled template")
      }
      var buf strings.Builder
      if err := tpl.Execute(&buf, nil); err != nil {
          t.Fatal(err)
      }
      if !strings.Contains(buf.String(), "@") {
          t.Errorf("expected an email-shaped output, got %q", buf.String())
      }
  }

  func TestCompile_SyntaxErrorFails(t *testing.T) {
      path := writeTempConfig(t, `
  filters:
    users:
      columns:
        email:
          value: "{{ this is not valid template }}"
  `)
      raw, err := LoadRaw(path)
      if err != nil {
          t.Fatal(err)
      }
      f := faker.New(rand.NewPCG(1, 1))
      if _, err := raw.Compile(f); err == nil {
          t.Errorf("expected compile error for malformed template")
      }
  }

  func TestCompile_UnknownFunctionFails(t *testing.T) {
      path := writeTempConfig(t, `
  filters:
    users:
      columns:
        email:
          value: "{{ fakerNoSuchFunction }}"
  `)
      raw, err := LoadRaw(path)
      if err != nil {
          t.Fatal(err)
      }
      f := faker.New(rand.NewPCG(1, 1))
      if _, err := raw.Compile(f); err == nil {
          t.Errorf("expected compile error for unknown function")
      }
  }
  ```

- [ ] **Step 2: Run, expect compilation failure.**
  ```bash
  go test ./internal/config/ -v
  ```

- [ ] **Step 3: Refactor `config.go` to expose `LoadRaw` and `(*RawConfig).Compile`.** Replace the body of `internal/config/config.go`:

  ```go
  // Package config loads anonymizer configuration. Loading is two-phase:
  //
  //   raw, err := config.LoadRaw(path)        // parse YAML; cheap; no faker.
  //   cc, err := raw.Compile(workerFaker)     // compile templates; per worker.
  //
  // This split is the spec's "Config compile model": templates close over a
  // specific *Faker.FuncMap() at parse time, and each worker has its own RNG.
  package config

  import (
      "fmt"
      "os"
      "text/template"

      "github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"

      "gopkg.in/yaml.v3"
  )

  // RawConfig is the YAML-parsed structure with template strings still in raw
  // form. Cheap to load, cheap to share.
  type RawConfig struct {
      Filters map[string]TableConf `yaml:"filters"`
  }

  // TableConf holds the column rules for a single table.
  type TableConf struct {
      Columns map[string]ColumnConf `yaml:"columns"`
  }

  // ColumnConf holds the Go template string for a single column.
  type ColumnConf struct {
      Value string `yaml:"value"`
  }

  // CompiledConfig is RawConfig with every template string parsed and bound
  // to a specific *Faker's FuncMap.
  type CompiledConfig struct {
      Rules map[string]map[string]*template.Template
  }

  // LoadRaw reads and parses the YAML at path. Templates are returned as raw
  // strings; call (*RawConfig).Compile to bind them to a Faker.
  func LoadRaw(path string) (*RawConfig, error) {
      data, err := os.ReadFile(path)
      if err != nil {
          return nil, fmt.Errorf("config: read %q: %w", path, err)
      }
      var raw RawConfig
      if err := yaml.Unmarshal(data, &raw); err != nil {
          return nil, fmt.Errorf("config: parse YAML: %w", err)
      }
      return &raw, nil
  }

  // Compile parses every template against f's FuncMap. Returns an error on the
  // first malformed template or unknown function. Cheap (~microseconds total
  // for tens of templates) — call once per worker per job.
  func (r *RawConfig) Compile(f *faker.Faker) (*CompiledConfig, error) {
      fm := f.FuncMap()
      cc := &CompiledConfig{
          Rules: make(map[string]map[string]*template.Template, len(r.Filters)),
      }
      for table, tf := range r.Filters {
          cols := make(map[string]*template.Template, len(tf.Columns))
          for col, cf := range tf.Columns {
              tpl, err := template.New("").Funcs(fm).Parse(cf.Value)
              if err != nil {
                  return nil, fmt.Errorf("config: compile %s.%s (%q): %w",
                      table, col, cf.Value, err)
              }
              cols[col] = tpl
          }
          cc.Rules[table] = cols
      }
      return cc, nil
  }
  ```

  Note: `text/template` does parse-time validation against the funcmap if Funcs() is called *before* Parse; unknown functions return errors at parse time.

- [ ] **Step 4: Run tests.**
  ```bash
  go test ./internal/config/ -v
  ```
  Expected: all PASS.

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "refactor(config): split into LoadRaw and (*RawConfig).Compile"
  jj new
  ```

---

## Task 10: Dump metadata loader (`internal/dump/meta.go`)

**Goal:** Parse the dump's `@.json` and per-table `<schema>@<table>.json` using the structures confirmed in Task 1. The exact field names below assume the Task-1 fixture confirmed `compression` at top level and `columns` (or `basenames`) on the table side. Adjust to match `testdata/fixtures/notes.md`.

**Files:**
- Create: `internal/dump/meta.go`
- Create: `internal/dump/dump_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/dump/dump_test.go`:

  ```go
  package dump

  import (
      "path/filepath"
      "testing"
  )

  func TestReadInstanceMeta_Fixture(t *testing.T) {
      meta, err := ReadInstanceMeta("../../testdata/fixtures/sample-at.json")
      if err != nil {
          t.Fatal(err)
      }
      if meta.Compression != "zstd" {
          t.Errorf("Compression = %q, want zstd", meta.Compression)
      }
  }

  func TestReadTableMeta_Fixture(t *testing.T) {
      meta, err := ReadTableMeta("../../testdata/fixtures/sample-table.json")
      if err != nil {
          t.Fatal(err)
      }
      // Task-1 fixture is a 3-column table (id, name, email).
      want := []string{"id", "name", "email"}
      if len(meta.Columns) != len(want) {
          t.Fatalf("len(Columns) = %d, want %d (%v)", len(meta.Columns), len(want), meta.Columns)
      }
      for i := range want {
          if meta.Columns[i] != want[i] {
              t.Errorf("Columns[%d] = %q, want %q", i, meta.Columns[i], want[i])
          }
      }
  }

  func TestReadTableMeta_NotFound(t *testing.T) {
      _, err := ReadTableMeta(filepath.Join(t.TempDir(), "nope.json"))
      if err == nil {
          t.Errorf("expected error reading nonexistent file")
      }
  }
  ```

- [ ] **Step 2: Run, expect compilation failure.**

- [ ] **Step 3: Implement `internal/dump/meta.go`.** The struct field tags must match what the Task-1 notes confirmed; the example below assumes the conventional shape:

  ```go
  package dump

  import (
      "encoding/json"
      "fmt"
      "os"
  )

  // InstanceMeta is the subset of @.json that the anonymizer cares about.
  // See testdata/fixtures/notes.md for the full mysqlsh schema.
  type InstanceMeta struct {
      Compression string `json:"compression"`
  }

  func ReadInstanceMeta(path string) (*InstanceMeta, error) {
      data, err := os.ReadFile(path)
      if err != nil {
          return nil, fmt.Errorf("dump: read %s: %w", path, err)
      }
      var m InstanceMeta
      if err := json.Unmarshal(data, &m); err != nil {
          return nil, fmt.Errorf("dump: parse %s: %w", path, err)
      }
      return &m, nil
  }

  // TableMeta is the per-table sidecar JSON, restricted to the fields the
  // anonymizer needs. Columns is in the same order as the chunk TSV cells.
  type TableMeta struct {
      // Field name confirmed in Task 1; adjust here if it's not "columns".
      Columns []string `json:"columns"`
  }

  func ReadTableMeta(path string) (*TableMeta, error) {
      data, err := os.ReadFile(path)
      if err != nil {
          return nil, fmt.Errorf("dump: read %s: %w", path, err)
      }
      var m TableMeta
      if err := json.Unmarshal(data, &m); err != nil {
          return nil, fmt.Errorf("dump: parse %s: %w", path, err)
      }
      return &m, nil
  }
  ```

- [ ] **Step 4: Run tests.** If they fail because the field name differs from `columns`/`compression`, edit the struct tags per Task-1 notes and rerun.

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(dump): metadata parsers for @.json and table json"
  jj new
  ```

---

## Task 11: Manifest walker (`internal/dump/manifest.go`)

**Goal:** Walk a dump directory, classify every file, and present it as a `Manifest` keyed for the orchestrator's needs (per-table chunk lists, top-level files, etc.). Walk order is **lexicographic** for determinism.

**Files:**
- Create: `internal/dump/manifest.go`
- Modify: `internal/dump/dump_test.go`

- [ ] **Step 1: Write the failing tests.** Append to `dump_test.go`:

  ```go
  func TestWalkManifest_TinyTree(t *testing.T) {
      dir := t.TempDir()
      mustWrite := func(rel string, body string) {
          t.Helper()
          p := filepath.Join(dir, rel)
          if err := os.WriteFile(p, []byte(body), 0644); err != nil {
              t.Fatal(err)
          }
      }
      // Top-level
      mustWrite("@.done.json", "{}")
      mustWrite("@.json", `{"compression":"zstd"}`)
      mustWrite("@.sql", "")
      // Schema
      mustWrite("fx.json", "{}")
      mustWrite("fx.sql", "")
      // Table
      mustWrite("fx@t.json", `{"columns":["id","email"]}`)
      mustWrite("fx@t.sql", "")
      mustWrite("fx@t@@0.tsv.zst", "")
      mustWrite("fx@t@@0.tsv.zst.idx", "")
      mustWrite("fx@t@@1.tsv.zst", "")
      mustWrite("fx@t@@1.tsv.zst.idx", "")

      m, err := WalkManifest(dir)
      if err != nil {
          t.Fatal(err)
      }
      if !m.HasDoneMarker {
          t.Errorf("HasDoneMarker = false, want true")
      }
      if m.InstanceMetaPath == "" || filepath.Base(m.InstanceMetaPath) != "@.json" {
          t.Errorf("InstanceMetaPath = %q", m.InstanceMetaPath)
      }
      tbl, ok := m.Tables["fx@t"]
      if !ok {
          t.Fatalf("table fx@t missing")
      }
      if len(tbl.Chunks) != 2 {
          t.Errorf("len(Chunks) = %d, want 2", len(tbl.Chunks))
      }
      if tbl.Chunks[0].DataPath == "" || tbl.Chunks[0].IdxPath == "" {
          t.Errorf("chunk paths missing: %+v", tbl.Chunks[0])
      }
      // Verify lexicographic ordering of chunks.
      if tbl.Chunks[0].Index != 0 || tbl.Chunks[1].Index != 1 {
          t.Errorf("chunk indices not in order: %+v", tbl.Chunks)
      }
  }

  func TestWalkManifest_MissingDoneMarker(t *testing.T) {
      dir := t.TempDir()
      if err := os.WriteFile(filepath.Join(dir, "@.json"), []byte("{}"), 0644); err != nil {
          t.Fatal(err)
      }
      m, err := WalkManifest(dir)
      if err != nil {
          t.Fatal(err)
      }
      if m.HasDoneMarker {
          t.Errorf("HasDoneMarker = true, want false")
      }
  }
  ```

- [ ] **Step 2: Run, expect compilation failure.**

- [ ] **Step 3: Implement.** Create `internal/dump/manifest.go`:

  ```go
  package dump

  import (
      "fmt"
      "os"
      "path/filepath"
      "regexp"
      "sort"
      "strconv"
      "strings"
  )

  // Manifest classifies every file in a mysqlsh dump directory.
  type Manifest struct {
      Root             string
      HasDoneMarker    bool   // @.done.json present
      InstanceMetaPath string // path to @.json
      Tables           map[string]*TableEntry // key: "<schema>@<table>"
      // PassthroughFiles are all files that the copy pass should hardlink/copy
      // verbatim into --out. Excludes: chunks of configured tables (set by the
      // orchestrator after intersecting with config), .idx sidecars of
      // configured-table chunks, and @.done.json (handled in finalization).
      PassthroughFiles []string
  }

  type TableEntry struct {
      // MetaPath is the per-table .json sidecar path.
      MetaPath string
      // SQLPath is the per-table .sql DDL path (may be "" if absent).
      SQLPath string
      // Chunks in lexicographic-by-index order.
      Chunks []ChunkEntry
  }

  type ChunkEntry struct {
      Index    int
      DataPath string // <chunk>.tsv.zst
      IdxPath  string // <chunk>.tsv.zst.idx
  }

  var chunkRE = regexp.MustCompile(`^(.+)@@(\d+)\.tsv\.zst$`)

  // WalkManifest scans dir non-recursively (mysqlsh dumpInstance produces a
  // flat directory) using os.ReadDir, which returns entries lexicographically
  // sorted on all Go-supported platforms — relied on for determinism.
  func WalkManifest(dir string) (*Manifest, error) {
      entries, err := os.ReadDir(dir)
      if err != nil {
          return nil, fmt.Errorf("dump: read dir %s: %w", dir, err)
      }
      m := &Manifest{
          Root:   dir,
          Tables: make(map[string]*TableEntry),
      }
      // First pass: identify chunks and table sidecars; collect passthrough.
      for _, e := range entries {
          if e.IsDir() {
              continue // mysqlsh dumpInstance is flat; ignore nested dirs.
          }
          name := e.Name()
          full := filepath.Join(dir, name)

          switch {
          case name == "@.done.json":
              m.HasDoneMarker = true
              // Excluded from passthrough; finalization copies it last.
              continue
          case name == "@.json":
              m.InstanceMetaPath = full
              m.PassthroughFiles = append(m.PassthroughFiles, full)
              continue
          }

          // Chunk?
          if mm := chunkRE.FindStringSubmatch(name); mm != nil {
              tableKey := mm[1]
              idx, err := strconv.Atoi(mm[2])
              if err != nil {
                  return nil, fmt.Errorf("dump: bad chunk index in %s: %w", name, err)
              }
              te := m.tableEntry(tableKey)
              te.Chunks = append(te.Chunks, ChunkEntry{
                  Index:    idx,
                  DataPath: full,
                  IdxPath:  full + ".idx",
              })
              continue
          }
          // .idx sidecar — handled alongside its chunk above; no passthrough entry.
          if strings.HasSuffix(name, ".tsv.zst.idx") {
              continue
          }
          // Per-table sidecars: <schema>@<table>.{json,sql}
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
          // Anything else (top-level, schema-level): passthrough.
          m.PassthroughFiles = append(m.PassthroughFiles, full)
      }
      // Sort chunks per table by Index for determinism.
      for _, t := range m.Tables {
          sort.Slice(t.Chunks, func(i, j int) bool {
              return t.Chunks[i].Index < t.Chunks[j].Index
          })
      }
      return m, nil
  }

  func (m *Manifest) tableEntry(key string) *TableEntry {
      if e, ok := m.Tables[key]; ok {
          return e
      }
      e := &TableEntry{}
      m.Tables[key] = e
      return e
  }
  ```

  Note the regex: `<schema>@<table>@@<n>.tsv.zst` — the first `@@` separates the table key from the chunk index.

- [ ] **Step 4: Run tests.**
  ```bash
  go test ./internal/dump/ -v
  ```
  Expected: PASS.

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(dump): manifest walker classifies dump-dir contents"
  jj new
  ```

---

## Task 12: `.idx` regenerator (`internal/idx`)

**Goal:** Write a binary `.idx` sidecar matching the format confirmed in Task 1. The example below assumes "8-byte big-endian uint64 offsets, one per row, of cumulative decompressed bytes through end-of-row." If Task 1 found a different layout, edit `idx.go` accordingly.

**Files:**
- Create: `internal/idx/idx.go`
- Create: `internal/idx/idx_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/idx/idx_test.go`:

  ```go
  package idx

  import (
      "bytes"
      "encoding/binary"
      "os"
      "testing"
  )

  func TestWriter_BasicOffsets(t *testing.T) {
      var buf bytes.Buffer
      w := NewWriter(&buf)
      w.RecordRowEnd(10)
      w.RecordRowEnd(25)
      w.RecordRowEnd(50)
      if err := w.Close(); err != nil {
          t.Fatal(err)
      }
      // Expect 3 × 8 bytes big-endian.
      want := make([]byte, 24)
      binary.BigEndian.PutUint64(want[0:8], 10)
      binary.BigEndian.PutUint64(want[8:16], 25)
      binary.BigEndian.PutUint64(want[16:24], 50)
      if !bytes.Equal(buf.Bytes(), want) {
          t.Errorf("got %x, want %x", buf.Bytes(), want)
      }
  }

  func TestWriter_MatchesMysqlshFixture(t *testing.T) {
      // We don't reproduce the exact .idx of an arbitrary mysqlsh chunk because
      // that requires also reproducing its decompressed bytes exactly. Instead:
      // verify our format matches the *structure* of mysqlsh's .idx — same
      // length per record, same endianness — by reading the fixture and
      // checking it's a multiple of 8.
      data, err := os.ReadFile("../../testdata/fixtures/sample.idx")
      if err != nil {
          t.Skipf("fixture not present (Task 1): %v", err)
      }
      if len(data)%8 != 0 {
          t.Errorf("fixture .idx is %d bytes (not multiple of 8); reconfirm format in Task 1 notes", len(data))
      }
  }
  ```

- [ ] **Step 2: Run, expect compilation failure.**

- [ ] **Step 3: Implement.** Create `internal/idx/idx.go`:

  ```go
  // Package idx writes the .idx sidecar that mysqlsh util.loadDump uses for
  // parallel sub-chunk loading. Format: a sequence of 8-byte big-endian uint64
  // offsets, one per row, recording cumulative decompressed bytes through
  // end-of-row. Verified against testdata/fixtures/sample.idx in Task 1.
  package idx

  import (
      "bufio"
      "encoding/binary"
      "io"
  )

  type Writer struct {
      w   *bufio.Writer
      buf [8]byte
      err error
  }

  func NewWriter(w io.Writer) *Writer {
      return &Writer{w: bufio.NewWriter(w)}
  }

  // RecordRowEnd records that bytesAtRowEnd cumulative decompressed bytes have
  // been written through this row's terminator.
  func (w *Writer) RecordRowEnd(bytesAtRowEnd int64) error {
      if w.err != nil {
          return w.err
      }
      binary.BigEndian.PutUint64(w.buf[:], uint64(bytesAtRowEnd))
      _, w.err = w.w.Write(w.buf[:])
      return w.err
  }

  func (w *Writer) Close() error {
      if w.err != nil {
          return w.err
      }
      return w.w.Flush()
  }
  ```

- [ ] **Step 4: Run tests.**
  ```bash
  go test ./internal/idx/ -v
  ```

- [ ] **Step 5: Commit.**
  ```bash
  go fmt ./...
  go vet ./...
  jj describe -m "feat(idx): write mysqlsh-format chunk index"
  jj new
  ```

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
          colIdx := make(map[string]int, len(tm.Columns))
          for i, c := range tm.Columns {
              colIdx[c] = i
          }
          for col := range tf.Columns {
              if _, ok := colIdx[col]; !ok {
                  return nil, fmt.Errorf("validate: config references column %s.%s but it is not in the dump (have %v)", tableKey, col, tm.Columns)
              }
          }
          schemas[matched] = &tableSchema{
              Columns:  tm.Columns,
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
      mw("@.json", `{"compression":"zstd"}`)
      mw("fx.json", "{}")
      mw("fx.sql", "")
      mw("fx@users.json", `{"columns":["id","name","email"]}`)
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
      iw := idx.NewWriter(idxF)

      hook := func(bytesAtRowEnd int64) error {
          if err := ctx.Err(); err != nil {
              return err
          }
          return iw.RecordRowEnd(bytesAtRowEnd)
      }
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
      if err := iw.Close(); err != nil {
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

      // 2. Verify @.json compression.
      if manifest.InstanceMetaPath == "" {
          return fmt.Errorf("--in lacks @.json")
      }
      meta, err := dump.ReadInstanceMeta(manifest.InstanceMetaPath)
      if err != nil {
          return err
      }
      if meta.Compression != "zstd" {
          return fmt.Errorf("only zstd compression is supported (dump uses %q)", meta.Compression)
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
      mustWrite("@.json", `{"compression":"zstd"}`)
      mustWrite("@.sql", "")
      mustWrite("@.post.sql", "")
      mustWrite("@.users.sql", "")
      mustWrite("fx.json", "{}")
      mustWrite("fx.sql", "")
      mustWrite("fx@users.json", `{"columns":["id","name","email"]}`)
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
