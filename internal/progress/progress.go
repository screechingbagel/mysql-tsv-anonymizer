// Package progress prints live "chunks done / bytes done / rate / ETA"
// status lines to stderr while the worker pool runs.
package progress

import (
	"fmt"
	"time"
)

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

// formatDuration renders d as "HhMmSs" / "MmSs" / "Ss", rounded to whole
// seconds. Sub-second durations render as "0s".
func formatDuration(d time.Duration) string {
	s := int64(d / time.Second)
	if s <= 0 {
		return "0s"
	}
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// renderLine formats one status line. It is a pure function: same inputs
// always produce the same string, with no I/O and no clock access.
//
// Rate is reported as integer MiB/s. When elapsed is zero (or rounds to
// zero), rate and ETA both render as "--". When rate is non-zero but
// remaining bytes are zero, ETA renders as "0s".
func renderLine(doneChunks, totalChunks int, doneBytes, totalBytes uint64, elapsed time.Duration) string {
	rateStr := "--"
	etaStr := "--"
	if elapsed >= time.Second {
		bytesPerSec := float64(doneBytes) / elapsed.Seconds()
		mibPerSec := bytesPerSec / (1024 * 1024)
		rateStr = fmt.Sprintf("%d MiB/s", int64(mibPerSec))
		if bytesPerSec > 0 && totalBytes >= doneBytes {
			remaining := float64(totalBytes - doneBytes)
			eta := time.Duration(remaining/bytesPerSec) * time.Second
			etaStr = formatDuration(eta)
		}
	}
	return fmt.Sprintf("[%d/%d chunks · %s / %s · %s · ETA %s]",
		doneChunks, totalChunks,
		formatBytes(doneBytes), formatBytes(totalBytes),
		rateStr, etaStr,
	)
}
