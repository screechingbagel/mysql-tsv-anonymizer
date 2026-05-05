package main

import (
	"fmt"
	"strings"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
)

// tableSchema holds the ordered column list and a fast name→index lookup for
// one table, as derived from the per-table sidecar JSON in the dump.
type tableSchema struct {
	Columns  []string
	ColIndex map[string]int
}

// tablePart returns the table name portion of a manifest key of the form
// "<schema>@<table>". It returns the part after the last '@'.
func tablePart(manifestKey string) string {
	i := strings.LastIndex(manifestKey, "@")
	if i < 0 {
		return manifestKey
	}
	return manifestKey[i+1:]
}

// Validate cross-checks rc (the user config) against m (the dump manifest).
// It returns a map keyed by the manifest key ("<schema>@<table>") for every
// table referenced in the config, or an error describing the first mismatch.
func Validate(rc *config.RawConfig, m *dump.Manifest) (map[string]*tableSchema, error) {
	// Parse every table's per-table json once, asserting compression.
	metas := make(map[string]*dump.TableMeta, len(m.Tables))
	for tableKey, te := range m.Tables {
		if te.MetaPath == "" {
			// Unconfigured tables with missing sidecar are a dump error too,
			// but only flag them if the dump claims to have chunks for the table.
			if len(te.Chunks) > 0 {
				return nil, fmt.Errorf("validate: table %q has chunks but no per-table json sidecar", tableKey)
			}
			continue
		}
		tm, err := dump.ReadTableMeta(te.MetaPath)
		if err != nil {
			return nil, fmt.Errorf("validate: read meta for table %q: %w", tableKey, err)
		}
		if tm.Compression != "zstd" {
			return nil, fmt.Errorf("validate: table %q has compression %q; only zstd is supported",
				tableKey, tm.Compression)
		}
		metas[tableKey] = tm
	}

	schemas := make(map[string]*tableSchema, len(rc.Filters))
	for tableKey, tf := range rc.Filters {
		var matches []string
		for k := range m.Tables {
			if tablePart(k) == tableKey {
				matches = append(matches, k)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("validate: config references table %q but it is not in the dump", tableKey)
		case 1:
			// proceed
		default:
			return nil, fmt.Errorf("validate: table %q is ambiguous across schemas: %s", tableKey, strings.Join(matches, ", "))
		}

		matched := matches[0]
		tm, ok := metas[matched]
		if !ok {
			// Should be unreachable: matched key came from m.Tables; the loop
			// above either parsed its meta or errored. The only path that skips
			// is MetaPath == "" with no chunks — for a configured table that's
			// a dump error.
			return nil, fmt.Errorf("validate: configured table %q has no per-table json sidecar", matched)
		}

		cols := tm.Options.Columns
		colIdx := make(map[string]int, len(cols))
		for i, c := range cols {
			colIdx[c] = i
		}
		for colName := range tf.Columns {
			if _, ok := colIdx[colName]; !ok {
				return nil, fmt.Errorf("validate: config references column %q.%q but it is not in the dump (have %v)",
					tableKey, colName, cols)
			}
		}
		schemas[matched] = &tableSchema{
			Columns:  cols,
			ColIndex: colIdx,
		}
	}
	return schemas, nil
}
