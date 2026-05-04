package tsv

import (
	"bufio"
	"io"
)

type Writer struct {
	w         *bufio.Writer
	scratch   []byte
	sepNeeded bool
	bytes     int64
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 64*1024)}
}

func (w *Writer) writeSep() error {
	if !w.sepNeeded {
		return nil
	}
	if err := w.w.WriteByte('\t'); err != nil {
		return err
	}
	w.bytes++
	return nil
}

// WritePassthrough writes cell bytes verbatim — no escaping. Use only with
// bytes that came from Reader.Next() for an unmodified cell.
func (w *Writer) WritePassthrough(cell []byte) error {
	if err := w.writeSep(); err != nil {
		return err
	}
	n, err := w.w.Write(cell)
	w.bytes += int64(n)
	w.sepNeeded = true
	return err
}

// WriteSubstituted writes cell as fresh data, applying mysqlsh escape rules.
func (w *Writer) WriteSubstituted(cell []byte) error {
	if err := w.writeSep(); err != nil {
		return err
	}
	w.scratch = escapeInto(w.scratch[:0], cell)
	n, err := w.w.Write(w.scratch)
	w.bytes += int64(n)
	w.sepNeeded = true
	return err
}

// WriteNULL writes the SQL NULL token (\N).
func (w *Writer) WriteNULL() error {
	if err := w.writeSep(); err != nil {
		return err
	}
	if _, err := w.w.Write([]byte{'\\', 'N'}); err != nil {
		return err
	}
	w.bytes += 2
	w.sepNeeded = true
	return nil
}

// EndRow writes the row separator. After it, the next Write* starts a new row.
func (w *Writer) EndRow() error {
	if err := w.w.WriteByte('\n'); err != nil {
		return err
	}
	w.bytes++
	w.sepNeeded = false
	return nil
}

// BytesWritten returns the running total of decompressed bytes written.
// Used by callers to build .idx offsets.
func (w *Writer) BytesWritten() int64 { return w.bytes }

func (w *Writer) Flush() error { return w.w.Flush() }
