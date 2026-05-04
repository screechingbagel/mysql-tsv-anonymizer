package zstd

import (
	"bytes"
	"io"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	payload := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1024)

	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("compressed output empty")
	}
	if buf.Len() >= len(payload) {
		t.Fatalf("compression did not shrink payload: %d >= %d", buf.Len(), len(payload))
	}

	r, err := NewReader(&buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("roundtrip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
