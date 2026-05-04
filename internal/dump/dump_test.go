package dump

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadInstanceMeta_Fixture(t *testing.T) {
	meta, err := ReadInstanceMeta("../../testdata/fixtures/sample-at.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(meta.Version, "2.") {
		t.Errorf("Version = %q, want 2.x", meta.Version)
	}
}

func TestReadTableMeta_Fixture(t *testing.T) {
	meta, err := ReadTableMeta("../../testdata/fixtures/sample-table.json")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Compression != "zstd" {
		t.Errorf("Compression = %q, want zstd", meta.Compression)
	}
	cols := meta.Options.Columns
	want := []string{"id", "name", "email"}
	if len(cols) != len(want) {
		t.Fatalf("len(Options.Columns) = %d, want %d (%v)", len(cols), len(want), cols)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Errorf("Options.Columns[%d] = %q, want %q", i, cols[i], want[i])
		}
	}
}

func TestReadTableMeta_NotFound(t *testing.T) {
	_, err := ReadTableMeta(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Errorf("expected error reading nonexistent file")
	}
}
