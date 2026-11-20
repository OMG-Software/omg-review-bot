// Package textutil holds small string helpers shared across packages.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Truncate shortens s to at most n bytes (plus an ellipsis), never splitting a UTF-8 rune.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// SplitList splits a newline- or comma-separated list, trimming blanks.
func SplitList(s string) []string {
	f := func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }
	var out []string
	for _, p := range strings.FieldsFunc(s, f) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FirstLine returns the first non-empty line of s.
func FirstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
