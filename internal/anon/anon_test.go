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

func mustTemplate(src string, fm template.FuncMap) *template.Template {
	t, err := template.New("").Funcs(fm).Parse(src)
	if err != nil {
		panic(err)
	}
	return t
}

func newFaker() *faker.Faker {
	return faker.New(rand.NewPCG(42, 99))
}

// runProcess runs ProcessAll over the given raw TSV bytes and returns the output bytes.
func runProcess(t *testing.T, input string, slots []*template.Template, f *faker.Faker) (string, error) {
	t.Helper()
	r := tsv.NewReader(strings.NewReader(input))
	var buf bytes.Buffer
	w := tsv.NewWriter(&buf)
	err := ProcessAll(r, w, slots, f)
	if err != nil {
		return "", err
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return buf.String(), nil
}

// TestProcessRow_PassthroughOnly — all nil slots, output equals input.
func TestProcessRow_PassthroughOnly(t *testing.T) {
	input := "alice\tbob\tcarol\n"
	slots := []*template.Template{nil, nil, nil}
	got, err := runProcess(t, input, slots, newFaker())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

// TestProcessRow_Substitution — nil/nil/template slots, first two cells passthrough, third replaced.
func TestProcessRow_Substitution(t *testing.T) {
	f := newFaker()
	fm := f.FuncMap()
	tmpl := mustTemplate(`REPLACED`, fm)
	slots := []*template.Template{nil, nil, tmpl}

	got, err := runProcess(t, "alice\tbob\tcarol\n", slots, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := strings.Split(strings.TrimSuffix(got, "\n"), "\t")
	if len(parts) != 3 {
		t.Fatalf("expected 3 cells, got %d: %q", len(parts), got)
	}
	if parts[0] != "alice" {
		t.Errorf("cell[0]: got %q, want %q", parts[0], "alice")
	}
	if parts[1] != "bob" {
		t.Errorf("cell[1]: got %q, want %q", parts[1], "bob")
	}
	if parts[2] != "REPLACED" {
		t.Errorf("cell[2]: got %q, want %q", parts[2], "REPLACED")
	}
}

// TestProcessRow_NullSentinel — {{ null }} template produces \N in output.
func TestProcessRow_NullSentinel(t *testing.T) {
	f := newFaker()
	fm := f.FuncMap()
	tmpl := mustTemplate(`{{ null }}`, fm)
	slots := []*template.Template{nil, tmpl}

	got, err := runProcess(t, "alice\tsome_value\n", slots, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The null sentinel should produce \N
	parts := strings.Split(strings.TrimSuffix(got, "\n"), "\t")
	if len(parts) != 2 {
		t.Fatalf("expected 2 cells, got %d: %q", len(parts), got)
	}
	if parts[1] != `\N` {
		t.Errorf("cell[1]: got %q, want %q", parts[1], `\N`)
	}
}

// TestProcessRow_SentinelMisuseFails — prefix-{{ null }} template returns error.
func TestProcessRow_SentinelMisuseFails(t *testing.T) {
	f := newFaker()
	fm := f.FuncMap()
	// Template that has the sentinel embedded in a larger string
	tmpl := mustTemplate(`prefix-{{ null }}`, fm)
	slots := []*template.Template{tmpl}

	_, err := runProcess(t, "alice\n", slots, f)
	if err == nil {
		t.Fatal("expected error for sentinel misuse, got nil")
	}
}

// TestProcessRow_CellCountMismatch — 2 cells but 3 slots → error.
func TestProcessRow_CellCountMismatch(t *testing.T) {
	slots := []*template.Template{nil, nil, nil}
	_, err := runProcess(t, "alice\tbob\n", slots, newFaker())
	if err == nil {
		t.Fatal("expected error for cell/slot count mismatch, got nil")
	}
}

// TestProcessAll_MultipleRows verifies that ProcessAll handles multiple rows correctly.
func TestProcessAll_MultipleRows(t *testing.T) {
	input := "a\tb\nc\td\n"
	slots := []*template.Template{nil, nil}
	got, err := runProcess(t, input, slots, newFaker())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

// TestProcessAllWithRowHook verifies the hook is called after each row.
func TestProcessAllWithRowHook(t *testing.T) {
	input := "a\tb\nc\td\n"
	slots := []*template.Template{nil, nil}

	r := tsv.NewReader(strings.NewReader(input))
	var buf bytes.Buffer
	w := tsv.NewWriter(&buf)

	var hookCalls []int64
	hook := RowEnded(func(bytesAtRowEnd int64) error {
		hookCalls = append(hookCalls, bytesAtRowEnd)
		return nil
	})

	err := ProcessAllWithRowHook(r, w, slots, newFaker(), hook)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if len(hookCalls) != 2 {
		t.Fatalf("expected 2 hook calls (one per row), got %d", len(hookCalls))
	}
	// Each call should report a positive, monotonically increasing byte count.
	if hookCalls[0] <= 0 {
		t.Errorf("first hook call: %d, want > 0", hookCalls[0])
	}
	if hookCalls[1] <= hookCalls[0] {
		t.Errorf("hook calls not monotonically increasing: %v", hookCalls)
	}
}

// TestProcessAll_EOF verifies that an empty stream returns nil (not an error).
func TestProcessAll_EOF(t *testing.T) {
	r := tsv.NewReader(strings.NewReader(""))
	var buf bytes.Buffer
	w := tsv.NewWriter(&buf)
	err := ProcessAll(r, w, []*template.Template{}, newFaker())
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
}
