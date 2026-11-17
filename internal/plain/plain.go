// Package plain makes text safe to show as plain text in a terminal, a web page or a phone app: one line, with no
// control character (an escape sequence, a bell, a NUL) and no invisible format character (a bidi override that
// reorders what is shown, a zero-width space). The joiners that scripts and emoji sequences need stay. The tag
// characters go with the other format characters, though a flag of a region uses them: they can carry text no
// one sees.
package plain

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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

// reSentenceEnd is where a sentence may end: its marks, what closes a quote, a bracket or an emphasis after them,
// and a space.
var reSentenceEnd = regexp.MustCompile("[.!?]+[\"'”’)\\]*_`]*\\s+")

// SentenceEnds are the places where the sentences of a text end, each as the offset of its mark and the offset
// where the next sentence begins: at a mark that a space follows. With every false, a mark that a small letter
// follows ends nothing ("e.g. two columns"), so a sentence that opens with a small letter joins the one before.
// A mark with no space after it ("v2.1", "config.yaml") never ends one.
func SentenceEnds(text string, every bool) [][2]int {
	var out [][2]int
	for _, m := range reSentenceEnd.FindAllStringIndex(text, -1) {
		if r, _ := utf8.DecodeRuneInString(text[m[1]:]); !every && unicode.IsLower(r) {
			continue
		}
		out = append(out, [2]int{m[0], m[1]})
	}
	return out
}

// KeptEnd is the end of a long text that a summary keeps: its last two sentences, from the mark that closed the
// sentence before them, when that is at most most bytes; else its last sentence, from the mark before it; else its
// last least bytes, cut at a character. It is never shorter than the last least bytes. An end that opens with a
// mark holds whole sentences after it, and one that does not was cut inside a sentence.
func KeptEnd(text string, least, most int) string {
	text = strings.TrimRightFunc(text, unicode.IsSpace)
	if len(text) <= least {
		return text
	}
	ends := SentenceEnds(text, false)
	for _, back := range []int{2, 1} {
		if len(ends) < back {
			continue
		}
		if from := ends[len(ends)-back][0]; len(text)-from <= most {
			if len(text)-from < least {
				break
			}
			return text[from:]
		}
	}
	from := len(text) - least
	for from < len(text) && !utf8.RuneStart(text[from]) {
		from++
	}
	return text[from:]
}
