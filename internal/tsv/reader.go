package tsv

import (
	"bufio"
	"io"
)

// Reader streams TSV rows in the mysqlsh dialect. Cells returned by Next are
// valid only until the next call to Next; copy them if you need to retain.
// The bytes are the raw, escaped form (passthrough-safe).
type Reader struct {
	r       *bufio.Reader
	rowBuf  []byte
	offsets []int
	cells   [][]byte
	err     error
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next row's cells, or io.EOF after the last row.
// It returns io.ErrUnexpectedEOF if the stream is malformed (EOF mid-row).
func (r *Reader) Next() ([][]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.rowBuf = r.rowBuf[:0]
	r.offsets = r.offsets[:0]
	r.offsets = append(r.offsets, 0)
	for {
		b, err := r.r.ReadByte()
		if err == io.EOF {
			if len(r.rowBuf) == 0 && len(r.offsets) == 1 {
				r.err = io.EOF
				return nil, io.EOF
			}
			r.err = io.ErrUnexpectedEOF
			return nil, r.err
		}
		if err != nil {
			r.err = err
			return nil, err
		}
		if b == '\\' {
			esc, err := r.r.ReadByte()
			if err != nil {
				if err == io.EOF {
					r.err = io.ErrUnexpectedEOF
					return nil, r.err
				}
				r.err = err
				return nil, err
			}
			r.rowBuf = append(r.rowBuf, '\\', esc)
			continue
		}
		if b == '\t' {
			r.offsets = append(r.offsets, len(r.rowBuf))
			continue
		}
		if b == '\n' {
			return r.materialize(), nil
		}
		r.rowBuf = append(r.rowBuf, b)
	}
}

func (r *Reader) materialize() [][]byte {
	r.cells = r.cells[:0]
	for i, start := range r.offsets {
		var end int
		if i+1 < len(r.offsets) {
			end = r.offsets[i+1]
		} else {
			end = len(r.rowBuf)
		}
		r.cells = append(r.cells, r.rowBuf[start:end])
	}
	return r.cells
}
