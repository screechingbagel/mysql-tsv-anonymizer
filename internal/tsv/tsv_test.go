package tsv

import (
	"bytes"
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
