// Package config loads and validates the anonymizer configuration file.
package config

import (
	"fmt"
	"os"
	"text/template"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"

	"gopkg.in/yaml.v3"
)

// RawConfig is the parsed YAML form, before template compilation.
type RawConfig struct {
	Filters map[string]TableConf `yaml:"filters"`
}

// TableConf holds the column rules for a single table.
type TableConf struct {
	Columns map[string]ColumnConf `yaml:"columns"`
}

// ColumnConf holds the Go template string for a single column.
type ColumnConf struct {
	Value string `yaml:"value"`
}

// CompiledConfig is the ready-to-use form after template compilation.
type CompiledConfig struct {
	// Rules: table name → column name → compiled template.
	Rules map[string]map[string]*template.Template
}

// LoadRaw reads and YAML-parses the config file at path.
func LoadRaw(path string) (*RawConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	var raw RawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: parse YAML: %w", err)
	}
	return &raw, nil
}

// Compile pre-parses every column template against f's FuncMap.
func (r *RawConfig) Compile(f *faker.Faker) (*CompiledConfig, error) {
	fm := f.FuncMap()
	cc := &CompiledConfig{
		Rules: make(map[string]map[string]*template.Template, len(r.Filters)),
	}
	for table, tf := range r.Filters {
		cols := make(map[string]*template.Template, len(tf.Columns))
		for col, cf := range tf.Columns {
			tpl, err := template.New("").Funcs(fm).Parse(cf.Value)
			if err != nil {
				return nil, fmt.Errorf("config: compile %s.%s (%q): %w",
					table, col, cf.Value, err)
			}
			cols[col] = tpl
		}
		cc.Rules[table] = cols
	}
	return cc, nil
}
