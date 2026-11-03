package main

import (
	"strings"
	"testing"
)

// Each usage entry's description follows its own command lines: `loop runs` describes the runs, and `loop eval`
// has a description of its own after its two lines, not the runs' one.
func TestUsageDescribesLoopRunsAndEvalApart(t *testing.T) {
	lines := strings.Split(usage, "\n")
	at := func(prefix string) int {
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return i
			}
		}
		t.Fatalf("no usage line %q", prefix)
		return 0
	}
	runs, eval := at("momentum loop runs "), at("momentum loop eval ")
	if !strings.Contains(lines[runs+1], "the loop's runs, and what the live ones spent") {
		t.Errorf("loop runs is described by %q", lines[runs+1])
	}
	if !strings.Contains(lines[eval+2], "golden evaluation") {
		t.Errorf("loop eval is described by %q", lines[eval+2])
	}
}
