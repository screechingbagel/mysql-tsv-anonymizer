package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
)

// tableSchema holds the ordered column list for one table, as derived from
// the per-table sidecar JSON in the dump.
type tableSchema struct {
	ConfigTable string
	Columns     []string
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
// On success it returns a map keyed by the manifest key ("<schema>@<table>")
// for every table referenced in the config, and a nil error. On failure it
// returns a nil map and an error that is the sorted join of *every* problem
// found — callers must not use the map when the error is non-nil.
func Validate(rc *config.RawConfig, m *dump.Manifest) (map[string]*tableSchema, error) {
	var problems []error
	add := func(err error) { problems = append(problems, err) }

	// Phase 1: parse every table's per-table json once, asserting compression.
	// This sweep covers unconfigured tables too: a non-zstd table anywhere in
	// the dump is a hard error. metas holds only tables that parsed cleanly and
	// are zstd; tables absent from it had a problem recorded here (or, for a
	// configured table with no sidecar and no chunks, are handled in phase 2).
	metas := make(map[string]*dump.TableMeta, len(m.Tables))
	for tableKey, te := range m.Tables {
		if te.MetaPath == "" {
			if len(te.Chunks) > 0 {
				add(fmt.Errorf("validate: table %q has chunks but no per-table json sidecar", tableKey))
			}
			continue
		}
		tm, err := dump.ReadTableMeta(te.MetaPath)
		if err != nil {
			add(fmt.Errorf("validate: read meta for table %q: %w", tableKey, err))
			continue
		}
		if tm.Compression != "zstd" {
			add(fmt.Errorf("validate: table %q has compression %q; only zstd is supported",
				tableKey, tm.Compression))
			continue
		}
		metas[tableKey] = tm
	}

	// Phase 2: per configured table. Iterate config tables in sorted order so
	// the per-table column text below is produced in a stable order even before
	// the final accumulator sort.
	cfgTables := make([]string, 0, len(rc.Filters))
	for t := range rc.Filters {
		cfgTables = append(cfgTables, t)
	}
	sort.Strings(cfgTables)

	schemas := make(map[string]*tableSchema, len(rc.Filters))
	for _, tableKey := range cfgTables {
		tf := rc.Filters[tableKey]

		var matches []string
		for k := range m.Tables {
			if tablePart(k) == tableKey {
				matches = append(matches, k)
			}
		}
		sort.Strings(matches)
		switch len(matches) {
		case 0:
			add(fmt.Errorf("validate: config references table %q but it is not in the dump", tableKey))
			continue
		case 1:
			// proceed
		default:
			add(fmt.Errorf("validate: table %q is ambiguous across schemas: %s", tableKey, strings.Join(matches, ", ")))
			continue
		}

		matched := matches[0]
		tm, ok := metas[matched]
		if !ok {
			// matched is in m.Tables but not in metas, so phase 1 already
			// recorded a problem for it (chunks without sidecar, unreadable
			// sidecar, or non-zstd) — unless it is a configured table with no
			// sidecar and no chunks, which phase 1 ignores. Cover only that.
			if te := m.Tables[matched]; te.MetaPath == "" && len(te.Chunks) == 0 {
				add(fmt.Errorf("validate: configured table %q has no per-table json sidecar", matched))
			}
			continue
		}

		cols := tm.Options.Columns
		colSet := make(map[string]struct{}, len(cols))
		for _, c := range cols {
			colSet[c] = struct{}{}
		}
		colNames := make([]string, 0, len(tf.Columns))
		for c := range tf.Columns {
			colNames = append(colNames, c)
		}
		sort.Strings(colNames)
		for _, colName := range colNames {
			if _, ok := colSet[colName]; !ok {
				add(fmt.Errorf("validate: config references column %q.%q but it is not in the dump (have %v)",
					tableKey, colName, cols))
			}
		}
		schemas[matched] = &tableSchema{
			ConfigTable: tableKey,
			Columns:     cols,
		}
	}

	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].Error() < problems[j].Error() })
		return nil, errors.Join(problems...)
	}
	return schemas, nil
}
