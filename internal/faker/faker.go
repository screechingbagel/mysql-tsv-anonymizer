// Package faker provides per-worker fake-data generators used in anonymization
// templates.
//
// Each Faker wraps a *gofakeit.Faker bound to its own RNG, so workers can run
// in parallel while still producing deterministic output for a given seed. All
// randomness flows through the wrapped instance — no package-level globals or
// shared counters.
package faker

import (
	"math/rand/v2"
	"strings"
	"text/template"

	"github.com/brianvoe/gofakeit/v7"
)

// SentinelNULL is the sentinel string that template output may emit to signal
// "this cell should be encoded as a SQL NULL." anon.Apply translates it.
const SentinelNULL = "::NULL::"

// Faker is a per-worker fake-data generator. Construct one per worker with New
// and reuse it for the lifetime of that worker; do not share across goroutines
// (gofakeit.Faker is not safe for concurrent use unless explicitly locked, and
// we deliberately leave locking off for throughput).
type Faker struct {
	gf *gofakeit.Faker
}

// New returns a Faker whose randomness is sourced from src. src is a
// math/rand/v2 Source (gofakeit/v7's NewFaker takes the same interface).
// Pass a deterministic Source for reproducible output, or a crypto-backed
// one for non-deterministic output.
func New(src rand.Source) *Faker {
	return &Faker{gf: gofakeit.NewFaker(src, false)}
}

// IBAN returns a fake IBAN-shaped string (XX####################).
func (f *Faker) IBAN() string {
	return strings.ToUpper(f.gf.Lexify("??####################"))
}

// SWIFT returns a fake SWIFT/BIC-shaped string (XXXXXX##).
func (f *Faker) SWIFT() string {
	return strings.ToUpper(f.gf.Lexify("??????##"))
}

// EIN returns a fake US Employer Identification Number (##-#######).
func (f *Faker) EIN() string {
	return f.gf.Numerify("##-#######")
}

// SecondaryAddress returns a fake secondary address (e.g. "Apt. 423").
func (f *Faker) SecondaryAddress() string {
	return "Apt. " + f.gf.DigitN(3)
}

// Invoice returns a fake invoice identifier of the form "INV-XXXXXXXX",
// where the suffix is 8 alphanumeric characters drawn from this Faker's RNG.
func (f *Faker) Invoice() string {
	return "INV-" + f.gf.Password(true, true, true, false, false, 8)
}

// FuncMap returns a text/template.FuncMap exposing this Faker's generators.
// All randomness flows through f, so two Fakers seeded identically produce
// identical template output. Function names match the keys used in the
// production anonymizer config.
func (f *Faker) FuncMap() template.FuncMap {
	return template.FuncMap{
		// Sentinels
		"null": func() string { return SentinelNULL },

		// Names
		"fakerName":      f.gf.Name,
		"fakerFirstName": f.gf.FirstName,
		"fakerLastName":  f.gf.LastName,

		// Contact
		"fakerEmail": f.gf.Email,
		"fakerPhone": f.gf.Phone,

		// Address
		"fakerAddress":          func() string { return f.gf.Address().Address },
		"fakerStreetAddress":    f.gf.Street,
		"fakerSecondaryAddress": f.SecondaryAddress,
		"fakerCity":             f.gf.City,
		"fakerPostcode":         f.gf.Zip,

		// Company / finance
		"fakerCompany": f.gf.Company,
		"fakerIBAN":    f.IBAN,
		"fakerSwift":   f.SWIFT,
		"fakerEIN":     f.EIN,

		// Invoice
		"fakerInvoice": f.Invoice,

		// Identifiers
		"uuidv4": f.gf.UUID,

		// Random strings — sprig-compatible signatures:
		//   {{ randAlphaNum 8 }}   {{ randNumeric 5 }}
		// Password(lower, upper, numeric, special, space, num) gives alphanumeric.
		"randAlphaNum": func(n int) string { return f.gf.Password(true, true, true, false, false, n) },
		"randNumeric":  func(n int) string { return f.gf.DigitN(uint(n)) },

		// String manipulation helpers used in config:
		//   {{ randAlphaNum 10 | upper }}
		"upper": strings.ToUpper,
		"lower": strings.ToLower,
	}
}
