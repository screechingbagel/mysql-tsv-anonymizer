package dump

import (
	"os"
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

func TestWalkManifest_TinyTree(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("@.done.json", "{}")
	mustWrite("@.json", `{"version":"2.0.1","dumper":"synthetic"}`)
	mustWrite("@.sql", "")
	mustWrite("fx.json", "{}")
	mustWrite("fx.sql", "")
	mustWrite("fx@t.json", `{"options":{"columns":["id","email"]}}`)
	mustWrite("fx@t.sql", "")
	mustWrite("fx@t@0.tsv.zst", "")
	mustWrite("fx@t@0.tsv.zst.idx", "")
	mustWrite("fx@t@@1.tsv.zst", "")
	mustWrite("fx@t@@1.tsv.zst.idx", "")

	m, err := WalkManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tables) != 1 {
		t.Errorf("len(Tables) = %d, want 1; got keys: %v", len(m.Tables), tablesKeys(m.Tables))
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
	if tbl.Chunks[0].Index != 0 || tbl.Chunks[1].Index != 1 {
		t.Errorf("chunk indices not in order: %+v", tbl.Chunks)
	}
	if tbl.Chunks[0].Final {
		t.Errorf("Chunks[0] (single-@) reported as final")
	}
	if !tbl.Chunks[1].Final {
		t.Errorf("Chunks[1] (double-@@) reported as non-final")
	}
}

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

func tablesKeys(t map[string]*TableEntry) []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	return out
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
