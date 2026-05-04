package dump

import (
	"encoding/json"
	"fmt"
	"os"
)

// InstanceMeta is the subset of @.json that the anonymizer cares about.
// mysqlsh's @.json has no `compression` field — that lives in the per-table
// JSON. We use Version as a format-discriminator (must start with "2.").
// See testdata/fixtures/notes.md for the full mysqlsh schema.
type InstanceMeta struct {
	Version string `json:"version"`
	Dumper  string `json:"dumper"`
}

func ReadInstanceMeta(path string) (*InstanceMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dump: read %s: %w", path, err)
	}
	var m InstanceMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("dump: parse %s: %w", path, err)
	}
	return &m, nil
}

// TableMeta is the per-table sidecar JSON. Compression top-level; columns at
// options.columns in physical order matching TSV cell order.
type TableMeta struct {
	Compression string `json:"compression"`
	Extension   string `json:"extension"`
	Options     struct {
		Columns            []string `json:"columns"`
		FieldsTerminatedBy string   `json:"fieldsTerminatedBy"`
		FieldsEscapedBy    string   `json:"fieldsEscapedBy"`
		LinesTerminatedBy  string   `json:"linesTerminatedBy"`
	} `json:"options"`
}

func ReadTableMeta(path string) (*TableMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dump: read %s: %w", path, err)
	}
	var m TableMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("dump: parse %s: %w", path, err)
	}
	return &m, nil
}
