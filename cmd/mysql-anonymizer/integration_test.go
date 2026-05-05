//go:build !nointeg

package main

import (
	"bytes"
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
