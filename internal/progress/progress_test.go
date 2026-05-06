package progress

import (
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1503238553, "1.4 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3*time.Minute + 12*time.Second, "3m12s"},
		{1*time.Hour + 2*time.Minute + 3*time.Second, "1h2m3s"},
		{500 * time.Millisecond, "0s"}, // sub-second rounds to 0s
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderLine_NormalProgress(t *testing.T) {
	got := renderLine(14, 47,
		1503238553,     // 1.4 GiB
		6227480576,     // 5.8 GiB
		60*time.Second, // elapsed
	)
	// 1.4 GiB / 60s ≈ 25.05 MB/s ≈ 23.9 MiB/s; ETA = (5.8-1.4) / rate ≈ 188s ≈ 3m8s.
	want := "[14/47 chunks · 1.4 GiB / 5.8 GiB · 23 MiB/s · ETA 3m8s]"
	if got != want {
		t.Errorf("renderLine = %q, want %q", got, want)
	}
}

func TestRenderLine_NoRateYet(t *testing.T) {
	got := renderLine(0, 47, 0, 1024*1024*1024, 0)
	want := "[0/47 chunks · 0 B / 1.0 GiB · -- · ETA --]"
	if got != want {
		t.Errorf("renderLine = %q, want %q", got, want)
	}
}

func TestRenderLine_AllDone(t *testing.T) {
	got := renderLine(47, 47, 1024*1024*1024, 1024*1024*1024, 10*time.Second)
	// rate = 1 GiB / 10s = 102.4 MiB/s; ETA = 0.
	want := "[47/47 chunks · 1.0 GiB / 1.0 GiB · 102 MiB/s · ETA 0s]"
	if got != want {
		t.Errorf("renderLine = %q, want %q", got, want)
	}
}
