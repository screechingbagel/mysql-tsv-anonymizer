//go:build !nointeg

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	lzstd "github.com/screechingbagel/mysql-tsv-anonymizer/internal/zstd"
)

// buildTinyDump writes a synthetic mysqlsh-shaped dump under dir.
// One schema "fx", one table "users" with two chunks:
//   - chunk 0 has the non-final @<n> filename pattern
//   - chunk 1 has the final @@<n> pattern (the last chunk of a table)
//
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
		var idxBuf [8]byte
		binary.BigEndian.PutUint64(idxBuf[:], uint64(raw.Len()))
		if err := os.WriteFile(chunkPath+".idx", idxBuf[:], 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Chunk 0: non-final @<n>. Notes cells contain TSV-escaped escape chars.
	writeChunk("@", 0, []row{
		{"1", "Alice", "a@x.com", `tab\there`},
		{"2", "Bob", "b@x.com", `newline\nhere`},
		{"3", "Carol", "c@x.com", `backslash\\here`},
	})
	// Chunk 1: final @@<n>. Notes cells contain NUL, \Z, \b, multi-byte UTF-8, \N.
	writeChunk("@@", 1, []row{
		{"4", "Dave", "d@x.com", `nul\0here`},
		{"5", "Eve", "e@x.com", `ctrlz\Zhere\bbs`},
		{"6", "Frank", "f@x.com", `日本語/français`},
		{"7", "Grace", "g@x.com", `\N`},
	})

	mustWrite("@.done.json", "{}")
}

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

// readChunkRows decompresses a .zst chunk and returns its rows as [][]byte cells.
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
	inEntries, err := os.ReadDir(inDir)
	if err != nil {
		t.Fatalf("ReadDir inDir: %v", err)
	}
	outEntries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("ReadDir outDir: %v", err)
	}
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

	// 3. Each rewritten chunk's .idx must declare the correct decompressed length.
	for _, base := range []string{"fx@users@0.tsv.zst", "fx@users@@1.tsv.zst"} {
		assertIdxMatchesChunk(t, filepath.Join(outDir, base), filepath.Join(outDir, base+".idx"))
	}
}

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
	entries, err := os.ReadDir(run1Out)
	if err != nil {
		t.Fatalf("ReadDir run1Out: %v", err)
	}
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
			if !bytes.Equal(rows1[i][2], rows2[i][2]) {
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

	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		return // outDir wasn't created at all — also a valid clean state.
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
