// Package idx writes the .idx sidecar that mysqlsh util.loadDump emits
// alongside each chunk. The format (verified against
// testdata/fixtures/sample.idx in Task 1) is a single 8-byte big-endian
// uint64 giving the total decompressed length of the sibling .zst chunk.
// It is not a sequence of per-row offsets and supports no random access.
package idx

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Write encodes decompressedLen as an 8-byte big-endian uint64 to w.
// Callers compute decompressedLen via tsv.Writer.BytesWritten() after
// closing the chunk's TSV stream.
func Write(w io.Writer, decompressedLen int64) error {
	if decompressedLen < 0 {
		return fmt.Errorf("idx: negative decompressed length %d", decompressedLen)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(decompressedLen))
	_, err := w.Write(buf[:])
	return err
}
