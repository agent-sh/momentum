package text

import (
	"encoding/json"
	"testing"
)

func TestOneLineFoldsAndClipsOnRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"  a \n\t b  ", 10, "a b"},
		{"abcdef", 4, "abc…"},
		{"ééééé", 3, "éé…"}, // a cut never splits a rune
		{"abc", 3, "abc"},
		{"abc", 0, ""},
	} {
		if got := OneLine(tc.in, tc.n); got != tc.want {
			t.Errorf("OneLine(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestFirstNonEmptySkipsBlank(t *testing.T) {
	if got := FirstNonEmpty("", "  ", " x ", "y"); got != " x " {
		t.Fatalf("got %q", got)
	}
	if got := FirstNonEmpty("", " "); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDescribeTakesTheReadablePartOfACall(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
		want string
	}{
		{"Bash", map[string]any{"command": "go test", "description": "Run the tests"}, "Run the tests"},
		{"Read", map[string]any{"file_path": "/x/y.go"}, "/x/y.go"},
		{"AskUserQuestion", map[string]any{"description": "ignored", "questions": []any{map[string]any{"question": "Ship it?"}}}, "Ship it?"},
		{"ExitPlanMode", map[string]any{"plan": "..."}, "plan ready for your approval"},
		{"Other", map[string]any{"n": 3}, ""},
	} {
		if got := Describe(tc.name, tc.in); got != tc.want {
			t.Errorf("Describe(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBlocksTextReadsStringsAndBlocks(t *testing.T) {
	for raw, want := range map[string]string{
		`"plain"`: "plain",
		`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`: "a b",
		`{"x":1}`: "",
		``:        "",
	} {
		if got := BlocksText(json.RawMessage(raw)); got != want {
			t.Errorf("BlocksText(%s) = %q, want %q", raw, got, want)
		}
	}
}
