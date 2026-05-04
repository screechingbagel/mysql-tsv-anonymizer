// Package tsv reads and writes the field-tab, line-newline, no-enclosure,
// backslash-escape dialect produced by mysqlsh util.dumpInstance.
//
// The byte-level invariant: cells passed through unmodified must round-trip
// byte-for-byte. Cells written via WriteSubstituted are encoded fresh using
// the escape table in escape.go.
//
// The escape table in escape.go is derived from github.com/hexon/mysqltsv
// (BSD-2-Clause; see LICENSE in this directory).
package tsv
