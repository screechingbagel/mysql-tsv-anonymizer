package config

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

const sampleYAML = `
filters:
  users:
    columns:
      email:
        value: "{{ fakerEmail }}"
      name:
        value: "static-name"
`

func TestLoadRaw_Parses(t *testing.T) {
	path := writeYAML(t, sampleYAML)
	raw, err := LoadRaw(path)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	tbl, ok := raw.Filters["users"]
	if !ok {
		t.Fatalf("users table missing from raw config")
	}
	if got, want := tbl.Columns["email"].Value, "{{ fakerEmail }}"; got != want {
		t.Errorf("email value = %q, want %q", got, want)
	}
	if got, want := tbl.Columns["name"].Value, "static-name"; got != want {
		t.Errorf("name value = %q, want %q", got, want)
	}
}

func TestCompile_BindsToFaker(t *testing.T) {
	path := writeYAML(t, sampleYAML)
	raw, err := LoadRaw(path)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	f := faker.New(rand.NewPCG(1, 1))
	cc, err := raw.Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	tpl, ok := cc.Rules["users"]["email"]
	if !ok {
		t.Fatalf("users.email rule missing")
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "@") {
		t.Errorf("fakerEmail output %q does not contain '@'", buf.String())
	}
}

func TestCompile_SyntaxErrorFails(t *testing.T) {
	path := writeYAML(t, `
filters:
  users:
    columns:
      email:
        value: "{{ fakerEmail "
`)
	raw, err := LoadRaw(path)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	f := faker.New(rand.NewPCG(1, 1))
	if _, err := raw.Compile(f); err == nil {
		t.Fatalf("Compile succeeded on malformed template; want error")
	}
}

func TestCompile_UnknownFunctionFails(t *testing.T) {
	path := writeYAML(t, `
filters:
  users:
    columns:
      email:
        value: "{{ fakerNoSuchFunction }}"
`)
	raw, err := LoadRaw(path)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	f := faker.New(rand.NewPCG(1, 1))
	if _, err := raw.Compile(f); err == nil {
		t.Fatalf("Compile succeeded on unknown function; want error")
	}
}
