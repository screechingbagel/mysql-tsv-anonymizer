// Package progress prints live "chunks done / bytes done / rate / ETA"
// status lines to stderr while the worker pool runs.
package progress

import "fmt"

// formatBytes renders n in human-readable IEC units (KiB / MiB / GiB / TiB).
// Uses one decimal for KiB and above, no decimals for plain bytes.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}
