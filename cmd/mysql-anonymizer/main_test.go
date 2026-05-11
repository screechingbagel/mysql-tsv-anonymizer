package main

import (
	"context"
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
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := config.LoadRaw(p)
	if err != nil {
		t.Fatal(err)
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

func TestPreparePassthrough_SkipsConfiguredChunks(t *testing.T) {
	inDir := mkTinyDump(t)
	m, err := dump.WalkManifest(inDir)
	if err != nil {
		t.Fatalf("WalkManifest: %v", err)
	}
	outDir := t.TempDir()

	err = PreparePassthrough(m, map[string]struct{}{"fx@users": {}}, outDir)
	if err != nil {
		t.Fatalf("PreparePassthrough: %v", err)
	}

	shouldExist := []string{
		"fx@users.json",
		"fx@users.sql",
		"@.json",
		"fx.json",
		"fx.sql",
	}
	for _, name := range shouldExist {
		p := filepath.Join(outDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist in outDir, got: %v", name, err)
		}
	}

	shouldNotExist := []string{
		"fx@users@@0.tsv.zst",
		"fx@users@@0.tsv.zst.idx",
		"@.done.json",
	}
	for _, name := range shouldNotExist {
		p := filepath.Join(outDir, name)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("expected %s to NOT exist in outDir, but it does", name)
		}
	}
}

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
