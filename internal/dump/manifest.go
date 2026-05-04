package dump

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Manifest classifies every file in a mysqlsh dump directory.
type Manifest struct {
	Root             string
	HasDoneMarker    bool                   // @.done.json present
	InstanceMetaPath string                 // path to @.json
	Tables           map[string]*TableEntry // key: "<schema>@<table>"
	// PassthroughFiles are all files that the copy pass should hardlink/copy
	// verbatim into --out. Excludes: chunks of configured tables (set by the
	// orchestrator after intersecting with config), .idx sidecars of
	// configured-table chunks, and @.done.json (handled in finalization).
	PassthroughFiles []string
}

type TableEntry struct {
	// MetaPath is the per-table .json sidecar path.
	MetaPath string
	// SQLPath is the per-table .sql DDL path (may be "" if absent).
	SQLPath string
	// Chunks in lexicographic-by-index order.
	Chunks []ChunkEntry
}

type ChunkEntry struct {
	Index    int
	DataPath string
	IdxPath  string
	Final    bool // true if filename used the @@ separator (last chunk).
}

// Two filename forms:
//
//	<basename>@<n>.tsv.zst   — non-final
//	<basename>@@<n>.tsv.zst  — final (single-chunk tables get @@0)
//
// The non-greedy `.+?` makes the engine prefer @@ when present.
var chunkRE = regexp.MustCompile(`^(.+?)(@@|@)(\d+)\.tsv\.zst$`)

// WalkManifest scans dir non-recursively (mysqlsh dumpInstance produces a
// flat directory) using os.ReadDir, which returns entries lexicographically
// sorted on all Go-supported platforms — relied on for determinism.
func WalkManifest(dir string) (*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("dump: read dir %s: %w", dir, err)
	}
	m := &Manifest{
		Root:   dir,
		Tables: make(map[string]*TableEntry),
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(dir, name)

		switch name {
		case "@.done.json":
			m.HasDoneMarker = true
			continue
		case "@.json":
			m.InstanceMetaPath = full
			m.PassthroughFiles = append(m.PassthroughFiles, full)
			continue
		case "@.sql":
			m.PassthroughFiles = append(m.PassthroughFiles, full)
			continue
		}

		if mm := chunkRE.FindStringSubmatch(name); mm != nil {
			tableKey := mm[1]
			sep := mm[2]
			idx, err := strconv.Atoi(mm[3])
			if err != nil {
				return nil, fmt.Errorf("dump: bad chunk index in %s: %w", name, err)
			}
			te := m.tableEntry(tableKey)
			te.Chunks = append(te.Chunks, ChunkEntry{
				Index:    idx,
				DataPath: full,
				IdxPath:  full + ".idx",
				Final:    sep == "@@",
			})
			continue
		}
		if strings.HasSuffix(name, ".tsv.zst.idx") {
			continue
		}
		if strings.Contains(name, "@") && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".sql")) {
			tableKey := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".sql")
			te := m.tableEntry(tableKey)
			if strings.HasSuffix(name, ".json") {
				te.MetaPath = full
			} else {
				te.SQLPath = full
			}
			m.PassthroughFiles = append(m.PassthroughFiles, full)
			continue
		}
		m.PassthroughFiles = append(m.PassthroughFiles, full)
	}
	for _, t := range m.Tables {
		sort.Slice(t.Chunks, func(i, j int) bool {
			return t.Chunks[i].Index < t.Chunks[j].Index
		})
	}
	return m, nil
}

func (m *Manifest) tableEntry(key string) *TableEntry {
	if e, ok := m.Tables[key]; ok {
		return e
	}
	e := &TableEntry{}
	m.Tables[key] = e
	return e
}
