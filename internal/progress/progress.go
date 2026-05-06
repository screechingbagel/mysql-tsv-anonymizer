// Package progress prints live "chunks done / bytes done / rate / ETA"
// status lines to stderr while the worker pool runs.
package progress

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
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

// Reporter prints a refreshing one-line status to its writer (stderr in
// production). One Reporter is created per run; workers report completed
// chunks via ChunkDone, which is safe for concurrent use.
type Reporter struct {
	totalChunks int
	totalBytes  uint64

	doneChunks atomic.Uint64
	doneBytes  atomic.Uint64

	start time.Time
	out   io.Writer
	isTTY bool
	tick  time.Duration

	stopCh chan struct{}
	doneCh chan struct{}
}

// New constructs a Reporter writing to out. Defaults: TTY auto-detected
// from os.Stderr if out is os.Stderr; otherwise non-TTY. Tick is 500ms on
// TTY, 5s on non-TTY. Both are exported as fields for test injection.
func New(totalChunks int, totalBytes uint64, out io.Writer) *Reporter {
	r := &Reporter{
		totalChunks: totalChunks,
		totalBytes:  totalBytes,
		out:         out,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	if f, ok := out.(*os.File); ok {
		r.isTTY = isCharDevice(f)
	}
	if r.isTTY {
		r.tick = 500 * time.Millisecond
	} else {
		r.tick = 5 * time.Second
	}
	return r
}

// isCharDevice reports whether f looks like a terminal (no x/term dep).
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Start launches the ticker goroutine. It exits when ctx is done or Stop
// is called.
func (r *Reporter) Start(ctx context.Context) {
	r.start = time.Now()
	go r.loop(ctx)
}

func (r *Reporter) loop(ctx context.Context) {
	defer close(r.doneCh)
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			r.render()
		}
	}
}

func (r *Reporter) render() {
	line := renderLine(
		int(r.doneChunks.Load()), r.totalChunks,
		r.doneBytes.Load(), r.totalBytes,
		time.Since(r.start),
	)
	if r.isTTY {
		fmt.Fprintf(r.out, "\r\x1b[K%s", line)
	} else {
		fmt.Fprintln(r.out, line)
	}
}

// ChunkDone records a successful chunk completion. compressedBytes is the
// on-disk size of the input chunk (used for the progress denominator).
func (r *Reporter) ChunkDone(compressedBytes uint64) {
	r.doneChunks.Add(1)
	r.doneBytes.Add(compressedBytes)
}

// Stop signals the ticker to exit, waits for it, then prints a final line.
// If runErr is non-nil the final line is prefixed "aborted at ...".
func (r *Reporter) Stop(runErr error) {
	close(r.stopCh)
	<-r.doneCh
	done := r.doneChunks.Load()
	bytesDone := r.doneBytes.Load()
	elapsed := time.Since(r.start)
	body := renderLine(int(done), r.totalChunks, bytesDone, r.totalBytes, elapsed)
	prefix := "done "
	if runErr != nil {
		prefix = fmt.Sprintf("aborted at %d/%d chunks ", done, r.totalChunks)
	}
	if r.isTTY {
		fmt.Fprintf(r.out, "\r\x1b[K%s%s\n", prefix, body)
	} else {
		fmt.Fprintf(r.out, "%s%s\n", prefix, body)
	}
}
