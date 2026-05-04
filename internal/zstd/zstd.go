// Package zstd is a thin wrapper around klauspost/compress/zstd.
package zstd

import (
	"io"

	kp "github.com/klauspost/compress/zstd"
)

// ReadCloser adapts a *kp.Decoder to io.ReadCloser.
type ReadCloser struct{ *kp.Decoder }

// Close releases the underlying decoder.
func (r ReadCloser) Close() error { r.Decoder.Close(); return nil }

// NewReader returns a zstd-decoding ReadCloser.
func NewReader(r io.Reader) (ReadCloser, error) {
	d, err := kp.NewReader(r)
	if err != nil {
		return ReadCloser{}, err
	}
	return ReadCloser{Decoder: d}, nil
}

// NewWriter returns a zstd-encoding writer.
func NewWriter(w io.Writer) (*kp.Encoder, error) {
	return kp.NewWriter(w)
}
