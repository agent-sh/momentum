// Package plain makes text safe to show as plain text in a terminal, a web page or a phone app: one line, with no
// control character (an escape sequence, a bell, a NUL) and no invisible format character (a bidi override that
// reorders what is shown, a zero-width space). The joiners that scripts and emoji sequences need stay. The tag
// characters go with the other format characters, though a flag of a region uses them: they can carry text no
// one sees.
package plain

import (
	"strings"
	"unicode"
)

// Line is s on one line: each run of whitespace, line breaks among it, becomes one space; control and format
// characters are dropped, but for the zero-width non-joiner and joiner.
func Line(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case r == '\u200c' || r == '\u200d': // the zero-width non-joiner and joiner
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// Clip is s cut to at most n characters, its last one an ellipsis when it was cut.
func Clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
