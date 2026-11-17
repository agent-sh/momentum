package plain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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

// A summary's end is the text's last two sentences from the mark that closed the one before, when they fit; else
// the last one; else the last bytes. It is never shorter than those bytes.
func TestKeptEndHoldsWholeSentences(t *testing.T) {
	start := strings.Repeat("The report lists every table. ", 10)
	for name, c := range map[string]struct {
		text        string
		least, most int
		want        string
	}{
		"the last two sentences":                {start + "The import ran. If you approve it, I'll build the page.", 20, 100, ". The import ran. If you approve it, I'll build the page."},
		"a mark inside a token ends nothing":    {start + "Plan v2.1 is in docs/plan.md now. I'll check config.yaml at 14:10.", 20, 100, ". Plan v2.1 is in docs/plan.md now. I'll check config.yaml at 14:10."},
		"a question's mark opens the end":       {start + "Should I go on? The import ran. I'll check back.", 20, 100, "? The import ran. I'll check back."},
		"two that do not fit: the last one":     {start + "The import ran on the staging copy and wrote nothing at all. I'll check back.", 10, 40, ". I'll check back."},
		"none that fits: the last bytes":        {start + "The import ran on the staging copy and wrote nothing at all to the live tables", 20, 40, "to the live tables"[len("to the live tables")-18:]},
		"two shorter than the least: the bytes": {start + "Done. Ok.", 20, 100, "table. Done. Ok."[len("table. Done. Ok.")-16:]},
		"a short text whole":                    {"Done. Ok.", 20, 100, "Done. Ok."},
		"space at the end is no sentence":       {start + "The import ran. I'll check back.  \n", 20, 100, ". The import ran. I'll check back."},
	} {
		got := KeptEnd(c.text, c.least, c.most)
		if !strings.HasSuffix(got, c.want) || len(got) > c.most || len(got) < min(c.least, len(strings.TrimSpace(c.text))) || name != "none that fits: the last bytes" && name != "two shorter than the least: the bytes" && got != c.want {
			t.Errorf("%s: KeptEnd = %q, want %q", name, got, c.want)
		}
	}
	// cut at a character, not inside one
	if got := KeptEnd(strings.Repeat("é", 100), 21, 40); len(got) != 20 || !utf8.ValidString(got) {
		t.Errorf("a cut inside a character: %q", got)
	}
}

func TestSentenceEndsAreMarksBeforeASpace(t *testing.T) {
	text := "Two shapes (e.g. a table) work. Pick one! done."
	if got := SentenceEnds(text, false); len(got) != 1 || text[got[0][0]:got[0][1]] != ". " || text[got[0][1]:] != "Pick one! done." {
		t.Errorf("sentence ends: %v", got)
	}
	if got := SentenceEnds(text, true); len(got) != 3 || text[got[0][1]:got[1][0]] != "a table) work" {
		t.Errorf("every mark and space: %v", got)
	}
}
