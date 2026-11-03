package loop

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// SPEC's golden key table lists exactly the keys ParseGolden accepts: the task's own, the owner's notes it drops,
// and in the symptom row the symptom's own.
func TestTheGoldenKeyTableMatchesTheParser(t *testing.T) {
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(raw), "Every JSONL line has these exact keys:\n")
	if !ok {
		t.Fatal("SPEC has no golden key table")
	}
	table, _, _ = strings.Cut(strings.TrimLeft(table, "\n"), "\n\n")
	tick := regexp.MustCompile("`([a-z_]+)`")
	var keys []string
	symptom := ""
	for _, row := range strings.Split(table, "\n")[2:] {
		cells := strings.Split(row, "|")
		if len(cells) < 4 {
			t.Fatalf("a table row: %q", row)
		}
		for _, m := range tick.FindAllStringSubmatch(cells[1], -1) {
			keys = append(keys, m[1])
			if m[1] == "symptom" {
				symptom = cells[2]
			}
		}
	}
	want := append(slices.Clone(goldenKeys), goldenNotes[:]...)
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("SPEC lists %v, the parser accepts %v", keys, want)
	}
	for _, key := range goldenSymptomKeys {
		if !strings.Contains(symptom, "`"+key+"`") {
			t.Errorf("SPEC's symptom row does not name %q", key)
		}
	}
}
