package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A loop arm keeps its findings file whatever error ended its run, as the native arm does: both judges read the same
// investigation.
func TestALoopTrialKeepsItsFindingsWhenItsRunErred(t *testing.T) {
	e, task := evalFixture(t)
	e.Runner.Agent.Fallback = "fallback-model"
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "refused", `cat > findings.md <<'NOTES'
## Mechanism
the job reads the lockfile before it takes the lock
## Fix
take the lock first
NOTES
rmdir tmp || exit 1
ln -s /fixture/absent tmp || exit 1
`+modelResult(`{"type":"result","subtype":"success","is_error":true,"result":"API Error: declined by the safeguards","num_turns":2,"total_cost_usd":0.5}`)+`
`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if tr.Run.Failure != "run" || !strings.Contains(tr.Run.Summary, "run directory tmp is not a directory") || tr.Run.USD != 0.5 {
			t.Fatalf("the %s arm's fallback did not fail after its first attempt: %+v", tr.Arm, tr.Run)
		}
		raw, _ := os.ReadFile(filepath.Join(tr.JudgeDir, "prompt.txt"))
		if !strings.Contains(string(raw), "reads the lockfile before it takes the lock") {
			t.Fatalf("the %s arm's judge lost its findings:\n%s", tr.Arm, raw)
		}
	}
}

// A run cut short whose findings file holds only alternatives gives both arms' judges the same four empty fields.
func TestAnAlternativesOnlyFindingsFileReachesNeitherJudge(t *testing.T) {
	e, task := evalFixture(t)
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "alternatives-only", `cat > findings.md <<'NOTES'
## Alternatives ruled out
a slow disk
NOTES
`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	if trs[0].JudgePromptHash != trs[1].JudgePromptHash {
		t.Fatal("the arms' judge inputs differ")
	}
	raw, _ := os.ReadFile(filepath.Join(trs[0].JudgeDir, "prompt.txt"))
	if strings.Contains(string(raw), "a slow disk") {
		t.Fatalf("alternatives alone reached a judge:\n%s", raw)
	}
}

// An evaluation's loop arm does the runner's bookkeeping, as a live investigation's: an investigation that ended well
// with a report short of a mechanism or a fix did not decide, so its case closes as insufficient evidence in the
// arm's store, and the judge reads the findings the case holds.
func TestAnUndecidedTrialClosesTheLoopArmsCase(t *testing.T) {
	e, task := evalFixture(t)
	e.Arms = []string{"loop"}
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "undecided", modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"the job reads the lockfile first","alternatives":[],"fix":"","summary":"no fix within reach"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 1 || trs[0].Err != "" {
		t.Fatalf("%+v %v", trs, err)
	}
	if trs[0].Status != "closed insufficient-evidence" {
		t.Errorf("the case after an investigation that decided nothing: %q", trs[0].Status)
	}
	raw, _ := os.ReadFile(filepath.Join(trs[0].JudgeDir, "prompt.txt"))
	if !strings.Contains(string(raw), "the job reads the lockfile first") {
		t.Errorf("the judge did not read the case's findings:\n%s", raw)
	}
}
