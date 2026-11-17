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

// What a summary keeps of an agent's text: its first SummaryHead bytes and its end (KeptEnd), which is at least
// SummaryEnd bytes and at most SummaryEndMost. The store and the rules that read a live session's last words cut
// with the same three.
const (
	SummaryHead    = 160
	SummaryEnd     = 80
	SummaryEndMost = 480
)

// reSentenceEnd is where a sentence may end: its marks, what closes a quote, a bracket or an emphasis after them,
// and a space.
var reSentenceEnd = regexp.MustCompile("[.!?]+[\"'”’)\\]*_`]*\\s+")

// A numbered list of one-sentence items is one sentence with what leads to it, or each item would read as a
// sentence of its own ("Which do you prefer? 1. Rebase. 2. Merge."): the full stop of an item's number ("1.",
// "12." after a space or at the start) ends nothing, and neither does a mark that an item's number follows. A
// sentence of running text that ends in such a number ("I ran test 3. Should I go on?") joins the next the same
// way.
var (
	reListNumber = regexp.MustCompile(`(^|\s)\d{1,3}$`)
	reListItem   = regexp.MustCompile(`^\d{1,3}\.\s`)
)

// SentenceEnds are the places where the sentences of a text end, each as the offset of its mark and the offset
// where the next sentence begins: at a mark that a space follows. With every false, a mark that a small letter
// follows ends nothing ("e.g. two columns"), so a sentence that opens with a small letter joins the one before,
// and a numbered list's items join what leads to them. A mark with no space after it ("v2.1", "config.yaml")
// never ends one.
func SentenceEnds(text string, every bool) [][2]int {
	var out [][2]int
	for _, m := range reSentenceEnd.FindAllStringIndex(text, -1) {
		if r, _ := utf8.DecodeRuneInString(text[m[1]:]); !every && unicode.IsLower(r) {
			continue
		}
		if !every && (text[m[0]] == '.' && text[m[0]+1] != '.' && reListNumber.MatchString(text[max(0, m[0]-4):m[0]]) || reListItem.MatchString(text[m[1]:])) {
			continue
		}
		out = append(out, [2]int{m[0], m[1]})
	}
	return out
}

// KeptEnd is the end of a long text that a summary keeps. When its last two sentences fit in most bytes, it is
// those, whole, from the mark that closed the sentence before them: an end that opens with a mark holds whole
// sentences after it. Else it is the last most bytes, cut at a character: its first sentence was cut. It is never shorter than the last least bytes. Space at the
// text's end is no part of it.
func KeptEnd(text string, least, most int) string {
	text = strings.TrimRightFunc(text, unicode.IsSpace)
	if len(text) <= least {
		return text
	}
	last := func(n int) string {
		from := len(text) - n
		for from < len(text) && !utf8.RuneStart(text[from]) {
			from++
		}
		return text[from:]
	}
	from := 0 // a text of one or two sentences is its own last two
	if ends := SentenceEnds(text, false); len(ends) >= 2 {
		from = ends[len(ends)-2][0]
	}
	switch n := len(text) - from; {
	case n > most:
		return last(most)
	case n < least:
		return last(least)
	}
	return text[from:]
}
