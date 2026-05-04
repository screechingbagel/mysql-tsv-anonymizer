package main

import (
	"io"
	"os"
	"path/filepath"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
)

// PreparePassthrough hardlinks (or copies) every passthrough file from the
// dump manifest into outDir, skipping data and index chunks that belong to
// tables listed in configuredTables (those will be anonymized separately).
//
// @.done.json is never in m.PassthroughFiles (WalkManifest excludes it), so
// no explicit skip is required here.
func PreparePassthrough(m *dump.Manifest, configuredTables map[string]struct{}, outDir string) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}

	// Build sets of chunk paths belonging to configured tables.
	configuredChunkData := make(map[string]struct{})
	configuredChunkIdx := make(map[string]struct{})
	for key := range configuredTables {
		te, ok := m.Tables[key]
		if !ok {
			continue
		}
		for _, ch := range te.Chunks {
			configuredChunkData[ch.DataPath] = struct{}{}
			configuredChunkIdx[ch.IdxPath] = struct{}{}
		}
	}

	for _, src := range m.PassthroughFiles {
		if _, skip := configuredChunkData[src]; skip {
			continue
		}
		if _, skip := configuredChunkIdx[src]; skip {
			continue
		}
		dst := filepath.Join(outDir, filepath.Base(src))
		if err := linkOrCopy(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// linkOrCopy tries to hardlink src to dst; on any link error it falls back to
// a regular file copy. This makes the function portable across filesystems and
// CI environments where cross-device links are disallowed.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
