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
		{"Invoice", func(f *Faker) string { return f.Invoice() }},
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

func TestInvoice_Format(t *testing.T) {
	f := newDeterministic()
	for range 20 {
		inv := f.Invoice()
		if !strings.HasPrefix(inv, "INV-") || len(inv) != 12 {
			t.Errorf("Invoice %q: want INV-<8 alphanumeric chars>", inv)
		}
	}
}
