// Derived from github.com/hexon/mysqltsv (BSD-2-Clause).
// See LICENSE in this directory.
package tsv

// escapeInto appends a backslash-escaped form of src to dst and returns
// the extended slice. Bytes that need escaping per the mysqlsh dialect:
//
//	NUL    -> \0
//	BS     -> \b
//	LF     -> \n
//	CR     -> \r
//	TAB    -> \t
//	SUB    -> \Z   (0x1A)
//	'\\'   -> \\
//
// All other bytes are written verbatim.
func escapeInto(dst, src []byte) []byte {
	for _, c := range src {
		switch c {
		case 0:
			dst = append(dst, '\\', '0')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case 0x1A:
			dst = append(dst, '\\', 'Z')
		case '\\':
			dst = append(dst, '\\', '\\')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}
