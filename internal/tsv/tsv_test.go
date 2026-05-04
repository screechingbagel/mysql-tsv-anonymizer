package tsv

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"testing"
)

func TestEscapeInto(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"empty", []byte{}, []byte{}},
		{"plain", []byte("hello"), []byte("hello")},
		{"tab", []byte("a\tb"), []byte(`a\tb`)},
		{"newline", []byte("a\nb"), []byte(`a\nb`)},
		{"backslash", []byte(`a\b`), []byte(`a\\b`)},
		{"null byte", []byte{'a', 0, 'b'}, []byte(`a\0b`)},
		{"sub", []byte{'a', 0x1A, 'b'}, []byte(`a\Zb`)},
		{"all", []byte{0, '\b', '\n', '\r', '\t', 0x1A, '\\'}, []byte(`\0\b\n\r\t\Z\\`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeInto(nil, tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("escapeInto(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReader_BasicRows(t *testing.T) {
	input := []byte("1\tAlice\ta@x.com\n2\tBob\tb@x.com\n")
	r := NewReader(bytes.NewReader(input))

	row1, err := r.Next()
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	want1 := [][]byte{[]byte("1"), []byte("Alice"), []byte("a@x.com")}
	if !equalRows(row1, want1) {
		t.Errorf("row1 = %q, want %q", row1, want1)
	}

	row2, err := r.Next()
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	want2 := [][]byte{[]byte("2"), []byte("Bob"), []byte("b@x.com")}
	if !equalRows(row2, want2) {
		t.Errorf("row2 = %q, want %q", row2, want2)
	}

	_, err = r.Next()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReader_PreservesEscapes(t *testing.T) {
	input := []byte(`val\tone` + "\t" + `val\\two` + "\n")
	r := NewReader(bytes.NewReader(input))
	row, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := [][]byte{[]byte(`val\tone`), []byte(`val\\two`)}
	if !equalRows(row, want) {
		t.Errorf("row = %q, want %q", row, want)
	}
}

func TestReader_NullToken(t *testing.T) {
	input := []byte(`Alice` + "\t" + `\N` + "\t" + `a@x.com` + "\n")
	r := NewReader(bytes.NewReader(input))
	row, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := [][]byte{[]byte("Alice"), []byte(`\N`), []byte("a@x.com")}
	if !equalRows(row, want) {
		t.Errorf("row = %q, want %q", row, want)
	}
}

func equalRows(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestWriter_PassthroughVerbatim(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.WritePassthrough([]byte(`val\tone`))
	w.WritePassthrough([]byte("plain"))
	w.WritePassthrough([]byte(`\N`))
	if err := w.EndRow(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := []byte(`val\tone` + "\t" + "plain" + "\t" + `\N` + "\n")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestWriter_SubstitutedEscapes(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.WriteSubstituted([]byte("hello"))
	w.WriteSubstituted([]byte("a\tb\nc"))
	w.WriteSubstituted([]byte(`back\slash`))
	w.EndRow()
	w.Flush()
	want := []byte(`hello` + "\t" + `a\tb\nc` + "\t" + `back\\slash` + "\n")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestWriter_Null(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.WritePassthrough([]byte("Alice"))
	w.WriteNULL()
	w.WritePassthrough([]byte("a@x.com"))
	w.EndRow()
	w.Flush()
	want := []byte("Alice" + "\t" + `\N` + "\t" + "a@x.com" + "\n")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestWriter_BytesWritten(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.WritePassthrough([]byte("a"))
	w.WritePassthrough([]byte("bb"))
	w.EndRow()
	w.Flush()
	if got := w.BytesWritten(); got != 5 {
		t.Errorf("BytesWritten = %d, want 5", got)
	}
}

func TestRoundtrip_BytesIdentical(t *testing.T) {
	input := []byte(
		`1` + "\t" + `Alice` + "\t" + `a@x.com` + "\n" +
			`2` + "\t" + `\N` + "\t" + `with\ttab` + "\n" +
			`3` + "\t" + `with\\backslash` + "\t" + `with\nnewline` + "\n" +
			`4` + "\t" + `with\0null` + "\t" + `with\Zsub` + "\n",
	)

	var out bytes.Buffer
	r := NewReader(bytes.NewReader(input))
	w := NewWriter(&out)
	for {
		cells, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		for _, c := range cells {
			if err := w.WritePassthrough(c); err != nil {
				t.Fatalf("WritePassthrough: %v", err)
			}
		}
		if err := w.EndRow(); err != nil {
			t.Fatalf("EndRow: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Errorf("roundtrip mismatch\ninput:  %q\noutput: %q", input, out.Bytes())
	}
}

func TestRoundtrip_FuzzedCells(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 100 {
		input := generateRandomTSV(rng, 5+rng.IntN(10), 1+rng.IntN(5))
		var out bytes.Buffer
		r := NewReader(bytes.NewReader(input))
		w := NewWriter(&out)
		for {
			cells, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("trial %d: Next: %v", trial, err)
			}
			for _, c := range cells {
				w.WritePassthrough(c)
			}
			w.EndRow()
		}
		w.Flush()
		if !bytes.Equal(out.Bytes(), input) {
			t.Fatalf("trial %d mismatch\ninput:  %q\noutput: %q", trial, input, out.Bytes())
		}
	}
}

func TestRoundtrip_MysqlshFixture(t *testing.T) {
	input, err := os.ReadFile("../../testdata/fixtures/sample.tsv")
	if err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	var out bytes.Buffer
	r := NewReader(bytes.NewReader(input))
	w := NewWriter(&out)
	for {
		cells, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		for _, c := range cells {
			if err := w.WritePassthrough(c); err != nil {
				t.Fatalf("WritePassthrough: %v", err)
			}
		}
		if err := w.EndRow(); err != nil {
			t.Fatalf("EndRow: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Errorf("fixture roundtrip mismatch (len in=%d out=%d)", len(input), out.Len())
	}
}

// generateRandomTSV produces a syntactically valid mysqlsh-dialect TSV with
// numRows rows of numCols cells each. Cells contain a random mix of plain
// bytes and escape sequences.
func generateRandomTSV(rng *rand.Rand, numRows, numCols int) []byte {
	var buf bytes.Buffer
	escapes := []string{`\0`, `\b`, `\n`, `\r`, `\t`, `\Z`, `\\`, `\N`, `\x`}
	for range numRows {
		for c := range numCols {
			if c > 0 {
				buf.WriteByte('\t')
			}
			cellLen := rng.IntN(8)
			for range cellLen {
				if rng.IntN(3) == 0 {
					buf.WriteString(escapes[rng.IntN(len(escapes))])
				} else {
					buf.WriteByte(byte('a' + rng.IntN(26)))
				}
			}
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}
