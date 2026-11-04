package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/loop"
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

// One evaluation at a time on the user's service manager: its sweep stops every unit of the evaluation prefix,
// whatever data home started it, so an evaluation under another home (a test, a profile) would stop a running one's
// trials and judges. A second one fails at once on the lock of that prefix, from any data home.
func TestASecondEvaluationFailsOnTheLockFromAnotherHome(t *testing.T) {
	isolatedHome(t)
	was := loop.EvalUnitPrefix
	loop.EvalUnitPrefix = fmt.Sprintf("momentum-run-evalclitest%d-", os.Getpid())
	defer func() { loop.EvalUnitPrefix = was }()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // no lock file is left in the real runtime directory
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	unlock, err := loop.LockEval() // an evaluation running under the first home
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	second := t.TempDir()
	t.Setenv("MOMENTUM_HOME", second)
	path := filepath.Join(second, "golden.jsonl")
	task := `{"id":"fixture","family":"failures","case_title":"worker failed","symptom":{"query":{"event_ids":["e1"]}},"cutoff":"2026-03-02T10:00:00Z","root_cause":"cause","key_facts":["fact"]}`
	if err := os.WriteFile(path, []byte(task), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := momentum("loop", "eval", "--golden", path)
	if code == 0 || !strings.Contains(stderr, "another evaluation runs now") {
		t.Fatalf("a second evaluation from another home: %d %q", code, stderr)
	}
}

// --split selects the held-out tasks, the others, or all of them, and --task narrows that selection.
func TestTheSplitSelectsItsTasks(t *testing.T) {
	all := []loop.GoldenTask{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	held := map[string]bool{"b": true}
	ids := func(ts []loop.GoldenTask) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.ID)
		}
		return strings.Join(out, ",")
	}
	for split, want := range map[string]string{"all": "a,b,c", "held-out": "b", "development": "a,c"} {
		if got, unknown := pickTasks(all, held, nil, split); ids(got) != want || len(unknown) != 0 {
			t.Errorf("%s: %s %v", split, ids(got), unknown)
		}
	}
	if got, unknown := pickTasks(all, held, map[string]bool{"a": true, "b": true}, "development"); ids(got) != "a" || !unknown["b"] {
		t.Errorf("a named held-out task in the development split: %s %v", ids(got), unknown)
	}
}
