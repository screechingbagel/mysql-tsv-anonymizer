//go:build !nointeg

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
		chunkPath := filepath.Join(dir, "fx@users@@"+strconv.Itoa(idx)+".tsv.zst")
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

	// 2. Email column is replaced (no more "@x.com").
	// 3. id and name columns are byte-identical.
	verifyChunk := func(idx int, expectedNames []string, expectedIDs []string) {
		chunkPath := filepath.Join(outDir, "fx@users@@"+strconv.Itoa(idx)+".tsv.zst")
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
