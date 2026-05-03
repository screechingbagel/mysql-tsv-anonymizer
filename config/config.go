// Package config loads and validates the anonymizer configuration file.
package config

import (
	"fmt"
	"os"
	"text/template"

	"github.com/screechingbagel/mysql-tsv-anonymizer/faker"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure mirroring the YAML layout.
type Config struct {
	Filters map[string]TableConf `yaml:"filters"`
}

// TableConf holds the column rules for a single table.
type TableConf struct {
	Columns map[string]ColumnConf `yaml:"columns"`
}

// ColumnConf holds the Go template string for a single column.
type ColumnConf struct {
	// Value is a Go text/template string. The function map includes all the
	// fakerXxx / randAlphaNum / uuidv4 / null / drop helpers.
	Value string `yaml:"value"`
}

// CompiledConfig is the ready-to-use form after template compilation.
type CompiledConfig struct {
	// Rules: table name → column name → compiled template.
	// Only tables+columns that appear in the config are present.
	Rules map[string]map[string]*template.Template
}

// Load reads the YAML file at path, parses it, and pre-compiles every
// column template. It returns an error if the file is unreadable, the YAML is
// malformed, or any template fails to parse.
func Load(path string) (*CompiledConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}

	var raw Config
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: parse YAML: %w", err)
	}

	fm := faker.FuncMap()

	cc := &CompiledConfig{
		Rules: make(map[string]map[string]*template.Template, len(raw.Filters)),
	}

	for table, tf := range raw.Filters {
		cols := make(map[string]*template.Template, len(tf.Columns))
		for col, cf := range tf.Columns {
			tpl, err := template.New("").Funcs(fm).Parse(cf.Value)
			if err != nil {
				return nil, fmt.Errorf("config: compile template for %s.%s (%q): %w",
					table, col, cf.Value, err)
			}
			cols[col] = tpl
		}
		cc.Rules[table] = cols
	}

	return cc, nil
}
