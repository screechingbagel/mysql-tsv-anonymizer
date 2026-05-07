package faker

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func newDeterministic() *Faker { return New(rand.NewPCG(42, 99)) }

func TestDeterminism_SameSeedSameOutput(t *testing.T) {
	a := newDeterministic()
	b := newDeterministic()
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
		{"SecondaryAddress", func(f *Faker) string { return f.SecondaryAddress() }},
	}
	for _, tc := range cases {
		for i := range 10 {
			if got, want := tc.fn(a), tc.fn(b); got != want {
				t.Errorf("%s [%d]: a=%q b=%q", tc.name, i, got, want)
			}
		}
	}
}

func TestDeterminism_DifferentSeedsDiffer(t *testing.T) {
	a := New(rand.NewPCG(1, 2))
	b := New(rand.NewPCG(3, 4))
	eq := 0
	for range 10 {
		if a.gf.Email() == b.gf.Email() {
			eq++
		}
	}
	if eq == 10 {
		t.Errorf("all 10 emails equal across distinct seeds")
	}
}

func TestInvoice_FormatAndAscending(t *testing.T) {
	f := newDeterministic()
	prev := ""
	for i := range 20 {
		inv := f.Invoice()
		if !strings.HasPrefix(inv, "INV-") || len(inv) != 4+16 {
			t.Errorf("Invoice %q: want INV- + 16 digits", inv)
		}
		if i > 0 && inv <= prev {
			t.Errorf("Invoice not ascending: prev=%q cur=%q", prev, inv)
		}
		prev = inv
	}
}

func TestInvoice_StartsAtZero(t *testing.T) {
	f := newDeterministic()
	if got, want := f.Invoice(), "INV-0000000000000000"; got != want {
		t.Errorf("first invoice = %q, want %q", got, want)
	}
	if got, want := f.Invoice(), "INV-0000000000000001"; got != want {
		t.Errorf("second invoice = %q, want %q", got, want)
	}
}

func TestInvoice_SetBaseAndReset(t *testing.T) {
	f := newDeterministic()
	f.Invoice() // counter = 1
	f.Invoice() // counter = 2
	f.SetInvoiceBase(0)
	if got, want := f.Invoice(), "INV-0000000000000000"; got != want {
		t.Errorf("after SetInvoiceBase(0): got %q, want %q", got, want)
	}
}

func TestInvoice_BaseStridesAcrossChunks(t *testing.T) {
	f := newDeterministic()
	f.SetInvoiceBase(InvoiceStride) // chunk 1
	if got, want := f.Invoice(), "INV-0000001000000000"; got != want {
		t.Errorf("chunk-1 first invoice = %q, want %q", got, want)
	}
}
