package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// An investigation is shown what its case holds from earlier runs, its record to check, which its report replaces.
func TestAnInvestigationIsShownWhatItsCaseHolds(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, investigatedResult)
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 2)
	note, fix := "not settled: the next investigation must zz-settle-the-lock-order-against-the-disk", "take the lock first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &note, NextTest: &fix, Alternatives: []string{"a slow disk"}}, "loop:build"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	args, _ := agentArgs(t, agent)
	if !strings.Contains(args, "zz-settle-the-lock-order-against-the-disk") || !strings.Contains(args, "alternative: a slow disk") || !strings.Contains(args, "its record") {
		t.Fatalf("the prompt lacks the case's record:\n%s", args)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting || got.Mechanism == note {
		t.Fatalf("the report did not replace the record: %+v", got)
	}
}

// A loop arm keeps its findings file whatever error ended its run, as the native arm does: both judges read the
// same investigation.
func TestALoopTrialKeepsItsFindingsWhenItsRunErred(t *testing.T) {
	e, task := evalFixture(t)
	e.Runner.Agent.Fallback = "fallback-model"
	e.Runner.Agent.Runner = []string{script(t, t.TempDir(), "refused", `cat > findings.md <<'NOTES'
## Mechanism
the job reads the lockfile before it takes the lock
## Fix
take the lock first
NOTES
mkdir -p transcript.refused.jsonl
echo '{"type":"result","subtype":"success","is_error":true,"result":"API Error: declined by the safeguards","num_turns":2,"total_cost_usd":0.5}'
`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		raw, _ := os.ReadFile(filepath.Join(tr.JudgeDir, "prompt.txt"))
		if !strings.Contains(string(raw), "reads the lockfile before it takes the lock") {
			t.Fatalf("the %s arm's judge lost its findings:\n%s", tr.Arm, raw)
		}
	}
}

// The runner records findings only into the case the run investigated, while it is open or investigating: never
// into the case it went into, nor one moved to testing or closed while it ran; a report with a mechanism and no fix
// leaves it investigating.
func TestRecordFindingsKeepsToItsOwnOpenCase(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a, b, moved, closed, short := openCase(t, st, "failures", "a", 1), openCase(t, st, "failures", "b", 1), openCase(t, st, "failures", "m", 1),
		openCase(t, st, "failures", "c", 1), openCase(t, st, "failures", "s", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:shared", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCaseSignature(ctx, a.ID, "root:shared", "test"); err != nil { // a goes into b
		t.Fatal(err)
	}
	mech, next := "the lock order", "take the lock first"
	if _, err := st.UpdateCase(ctx, moved.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseCase(ctx, closed.ID, store.NotWorthIt, "", "fixture", "test"); err != nil {
		t.Fatal(err)
	}
	f := finding{Mechanism: "zz-run-mechanism", Fix: "zz-run-fix"}
	for _, id := range []string{a.ID, moved.ID, closed.ID} {
		if _, recorded, err := recordFindings(ctx, st, id, f, true, "loop:r"); err != nil || recorded {
			t.Errorf("%s: recorded %v %v", id, recorded, err)
		}
	}
	for _, id := range []string{b.ID, moved.ID, closed.ID} {
		if got, _ := st.CaseByID(ctx, id); strings.Contains(got.Mechanism, "zz-run") {
			t.Errorf("%s took another run's findings: %+v", id, got)
		}
	}
	if got, recorded, err := recordFindings(ctx, st, short.ID, finding{Mechanism: "zz-only-a-mechanism"}, true, "loop:r"); err != nil || !recorded ||
		got.Status != store.CaseInvestigating || got.NextTest != "" {
		t.Fatalf("a report with no fix: %+v %v %v", got, recorded, err)
	}
}

// A killed investigation's findings file reaches its case when stale recovery ends the run.
func TestAKilledInvestigationKeepsItsFindings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "a", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	os.MkdirAll(run.Dir, 0o700)
	write(t, filepath.Join(run.Dir, "findings.md"), "## Mechanism\nthe export reads the lockfile first\n## Fix\ntake the lock first\n")
	later := time.Now().Add(r.Budgets.RunTime + time.Hour)
	r.Now = func() time.Time { return later }
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseInvestigating || got.Mechanism != "the export reads the lockfile first" {
		t.Fatalf("the killed run's findings: %+v", got)
	}
}

// An empty alternative reaches neither arm's judge.
func TestAnEmptyAlternativeReachesNoJudge(t *testing.T) {
	if got := judgeFinding(finding{Mechanism: "m", Alternatives: []string{"disk latency", " "}, Fix: "f"}); strings.Contains(got, "disk latency | ") {
		t.Fatalf("%s", got)
	}
}
