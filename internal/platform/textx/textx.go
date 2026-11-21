// Package textx makes client-controlled text safe to store.
package textx

import (
	"strings"
	"unicode"
)

const maxBytes = 512

// Sanitize returns valid UTF-8 without control characters (Postgres rejects NUL, protobuf invalid UTF-8), at most 512 bytes.
func Sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if len(s) > maxBytes {
		s = strings.ToValidUTF8(s[:maxBytes], "")
	}
	return s
}
