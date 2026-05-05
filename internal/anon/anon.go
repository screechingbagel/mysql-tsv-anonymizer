// Package anon combines a tsv.Reader, tsv.Writer, and a slice of per-column
// text/template templates into a row-by-row anonymization processor.
//
// For each row the processor walks cells in order. A nil slot means
// passthrough (the raw escaped bytes from the source are written verbatim).
// A non-nil slot means the template is executed and its output replaces the
// cell. If the template output equals faker.SentinelNULL the cell is encoded
// as a SQL NULL (\N). If the sentinel appears as a substring of a longer
// output an error is returned — this guards against accidental concatenation
// producing a corrupted sentinel.
package anon

import (
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/tsv"
)

// RowEnded is called after each row is fully written. bytesAtRowEnd is the
// value of w.BytesWritten() immediately after EndRow(), and can be used by
// callers to build .idx chunk offsets.
type RowEnded func(bytesAtRowEnd int64) error

// ProcessAll anonymizes every row from r and writes it to w, using slots to
// decide per-column treatment. It is equivalent to ProcessAllWithRowHook with
// a nil hook.
func ProcessAll(r *tsv.Reader, w *tsv.Writer, slots []*template.Template) error {
	return ProcessAllWithRowHook(r, w, slots, nil)
}

// ProcessAllWithRowHook anonymizes every row from r and writes it to w. After
// each row is written, hook (if non-nil) is called with the byte offset at the
// end of that row. Processing stops on the first error; io.EOF from r is
// treated as clean termination and returns nil.
func ProcessAllWithRowHook(r *tsv.Reader, w *tsv.Writer, slots []*template.Template, hook RowEnded) error {
	var sb strings.Builder
	for {
		cells, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("anon: read row: %w", err)
		}

		if len(cells) != len(slots) {
			return fmt.Errorf("anon: cell count %d != slot count %d", len(cells), len(slots))
		}

		for i, cell := range cells {
			if slots[i] == nil {
				if err := w.WritePassthrough(cell); err != nil {
					return fmt.Errorf("anon: write passthrough cell %d: %w", i, err)
				}
				continue
			}

			// Execute template.
			sb.Reset()
			if err := slots[i].Execute(&sb, nil); err != nil {
				return fmt.Errorf("anon: execute template cell %d: %w", i, err)
			}
			out := sb.String()

			switch {
			case out == faker.SentinelNULL:
				if err := w.WriteNULL(); err != nil {
					return fmt.Errorf("anon: write NULL cell %d: %w", i, err)
				}
			case strings.Contains(out, faker.SentinelNULL):
				return fmt.Errorf("anon: cell %d: template output contains NULL sentinel as substring — use {{ null }} alone", i)
			default:
				if err := w.WriteSubstituted([]byte(out)); err != nil {
					return fmt.Errorf("anon: write substituted cell %d: %w", i, err)
				}
			}
		}

		if err := w.EndRow(); err != nil {
			return fmt.Errorf("anon: end row: %w", err)
		}

		if hook != nil {
			if err := hook(w.BytesWritten()); err != nil {
				return fmt.Errorf("anon: row hook: %w", err)
			}
		}
	}
}
