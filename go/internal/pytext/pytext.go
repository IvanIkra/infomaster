// Package pytext copies Python's string functions whose results are stored or hashed: casefold, split, quote and
// counting by code points.
package pytext

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

var folder = cases.Fold()

// Casefold is str.casefold().
func Casefold(s string) string { return folder.String(s) }

// IsSpace is str.isspace() for one character: Unicode spaces plus the separators \x1c to \x1f.
func IsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Split is str.split() without arguments.
func Split(s string) []string { return strings.FieldsFunc(s, IsSpace) }

// Strip is str.strip() without arguments.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// Len is len(str): code points.
func Len(s string) int { return utf8.RuneCountInString(s) }

// Cut is s[:n] by code points.
func Cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"

// Quote is urllib.parse.quote(s, safe): UTF-8 bytes outside the unreserved set and `safe` are %XX (upper case).
// Python's default safe is "/".
func Quote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < utf8.RuneSelf && (strings.IndexByte(unreserved, c) >= 0 || strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte("0123456789ABCDEF"[c>>4])
		b.WriteByte("0123456789ABCDEF"[c&15])
	}
	return b.String()
}
