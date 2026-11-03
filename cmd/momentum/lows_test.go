package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/agent-sh/momentum/internal/paths"
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

// One evaluation at a time: its sweep stops every unit of the evaluation prefix, so a second evaluation would stop
// the first one's trials and judges. A second one fails at once on the evaluation lock beside the store.
func TestASecondEvaluationFailsOnTheLock(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("MOMENTUM_HOME", dir)
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	path := filepath.Join(dir, "golden.jsonl")
	task := `{"id":"fixture","family":"failures","case_title":"worker failed","symptom":{"query":{"event_ids":["e1"]}},"cutoff":"2026-03-02T10:00:00Z","root_cause":"cause","key_facts":["fact"]}`
	if err := os.WriteFile(path, []byte(task), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(paths.DB()+".eval-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := momentum("loop", "eval", "--golden", path)
	if code == 0 || !strings.Contains(stderr, "another evaluation runs now") {
		t.Fatalf("a second evaluation: %d %q", code, stderr)
	}
}
