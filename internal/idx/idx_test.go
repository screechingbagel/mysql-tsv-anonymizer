package idx

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func TestWrite_EncodesTotalLength(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, 183); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 8)
	binary.BigEndian.PutUint64(want, 183)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("got %x, want %x", buf.Bytes(), want)
	}
}

func TestWrite_RoundtripsFixture(t *testing.T) {
	data, err := os.ReadFile("../../testdata/fixtures/sample.idx")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 8 {
		t.Fatalf("fixture .idx is %d bytes, want exactly 8 (per Task 1 notes)", len(data))
	}
	length := binary.BigEndian.Uint64(data)

	var buf bytes.Buffer
	if err := Write(&buf, int64(length)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("re-encoded .idx %x != fixture %x", buf.Bytes(), data)
	}
}

func TestWrite_RejectsNegative(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, -1); err == nil {
		t.Error("expected error for negative length")
	}
}
