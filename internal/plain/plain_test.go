package plain

import "testing"

// Text shown as plain text loses what a terminal or a renderer would act on: escape sequences, bells, NULs, DEL,
// C1 controls, bidi overrides and isolates, zero-width spaces, a byte order mark and a soft hyphen. Whitespace
// folds to single spaces, and the joiners stay.
func TestLineKeepsOnlyWhatIsShown(t *testing.T) {
	for in, want := range map[string]string{
		"a\x1b[31mred\x07 z\u202eevil":               "a[31mred zevil",
		" two\n lines\r\n\tand more ":                "two lines and more",
		"nul\x00del\x7fc1\u0085x\u009b":              "nuldelc1 x",
		"zero\u200bwidth \u2066iso\u2069 \u200fmark": "zerowidth iso mark",
		"bom\ufeff soft\u00adhyphen":                 "bom softhyphen",
		"family \U0001F468\u200d\U0001F467 ok":       "family \U0001F468\u200d\U0001F467 ok",
		"half\u200cspace":                            "half\u200cspace",
		"plain text, kept":                           "plain text, kept",
		"a \x1b b":                                   "a b",
		"line\u2028para\u2029end":                    "line para end",
	} {
		if got := Line(in); got != want {
			t.Errorf("Line(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClipCutsToNCharacters(t *testing.T) {
	if got := Clip("abcdef", 4); got != "abc\u2026" {
		t.Errorf("Clip: %q", got)
	}
	if got := Clip("\u05e9\u05dc\u05d5\u05dd", 4); got != "\u05e9\u05dc\u05d5\u05dd" {
		t.Errorf("Clip of 4 characters: %q", got)
	}
}
