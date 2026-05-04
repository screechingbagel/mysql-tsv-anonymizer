package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
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

func TestParseFlags_SeedZeroExplicit(t *testing.T) {
	o, err := parseFlags([]string{"--in", "x", "--out", "y", "-c", "z", "--seed", "0"})
	if err != nil {
		t.Errorf("expected --seed 0 to be accepted, got %v", err)
	}
	if o.Seed != 0 {
		t.Errorf("expected Seed=0, got %d", o.Seed)
	}
}

// mkTinyDump creates a minimal synthetic mysqlsh dump directory in a temp dir.
func mkTinyDump(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"@.done.json":             `{}`,
		"@.json":                  `{"version":"2.0.1","dumper":"synthetic"}`,
		"fx.json":                 `{}`,
		"fx.sql":                  ``,
		"fx@users.json":           `{"compression":"zstd","extension":"tsv.zst","options":{"columns":["id","name","email"],"fieldsTerminatedBy":"\t","fieldsEscapedBy":"\\","linesTerminatedBy":"\n"}}`,
		"fx@users.sql":            ``,
		"fx@users@@0.tsv.zst":     ``,
		"fx@users@@0.tsv.zst.idx": ``,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("mkTinyDump: write %s: %v", name, err)
		}
	}
	return dir
}

// mkConfig writes a temp YAML config file and returns the parsed *RawConfig.
func mkConfig(t *testing.T, body string) *config.RawConfig {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("mkConfig: create temp file: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("mkConfig: write: %v", err)
	}
	f.Close()
	rc, err := config.LoadRaw(f.Name())
	if err != nil {
		t.Fatalf("mkConfig: LoadRaw: %v", err)
	}
	return rc
}

func TestValidate_HappyPath(t *testing.T) {
	dir := mkTinyDump(t)
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
	schemas, err := Validate(rc, m)
	if err != nil {
		t.Fatalf("Validate returned unexpected error: %v", err)
	}
	if _, ok := schemas["fx@users"]; !ok {
		t.Errorf("expected schemas[\"fx@users\"] to exist, got keys: %v", schemas)
	}
}

func TestValidate_MissingTable(t *testing.T) {
	dir := mkTinyDump(t)
	m, err := dump.WalkManifest(dir)
	if err != nil {
		t.Fatalf("WalkManifest: %v", err)
	}
	rc := mkConfig(t, `
filters:
  nope:
    columns:
      email:
        value: "fake@example.com"
`)
	_, err = Validate(rc, m)
	if err == nil {
		t.Fatal("expected an error for missing table, got nil")
	}
	if !strings.Contains(err.Error(), "table") {
		t.Errorf("expected error to contain \"table\", got: %v", err)
	}
}

func TestValidate_MissingColumn(t *testing.T) {
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
        value: "fake"
`)
	_, err = Validate(rc, m)
	if err == nil {
		t.Fatal("expected an error for missing column, got nil")
	}
	if !strings.Contains(err.Error(), "column") {
		t.Errorf("expected error to contain \"column\", got: %v", err)
	}
}

func TestValidate_NonZstdCompression(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"@.done.json":         `{}`,
		"@.json":              `{"version":"2.0.1","dumper":"synthetic"}`,
		"fx.json":             `{}`,
		"fx.sql":              ``,
		"fx@users.json":       `{"compression":"none","extension":"tsv","options":{"columns":["id","name","email"],"fieldsTerminatedBy":"\t","fieldsEscapedBy":"\\","linesTerminatedBy":"\n"}}`,
		"fx@users.sql":        ``,
		"fx@users@@0.tsv":     ``,
		"fx@users@@0.tsv.idx": ``,
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
		t.Fatal("expected an error for non-zstd compression, got nil")
	}
	if !strings.Contains(err.Error(), "zstd") {
		t.Errorf("expected error to contain \"zstd\", got: %v", err)
	}
}
