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

// A revision whose check passed and whose build was cut short before its proposal leaves the package checked, its
// pull request open: the change is in progress, the next build is told of its review, and a reported root leaves
// the case where it is.
func TestACheckedRevisionResumesItsReview(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	if p, err = s.Propose(ctx, c.ID, pr); err != nil || p.State != Proposed {
		t.Fatalf("propose: %+v %v", p, err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "no comment", Fix: "add one"}},
		Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || p.State != Revising {
		t.Fatalf("reviewed: %+v %v", p, err)
	}
	// the revision edits and checks, and is cut short before ship propose
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is the answer, 42.", 1))
	if p, err = s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	if state, open := s.ChangeOpen(c.ID); !open || state != Checked || p.PR == "" {
		t.Fatalf("after the revision's check: state %s, pull request %q, open %v", state, p.PR, open)
	}
	b := openCase(t, st, "failures", "b", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:shared", "fixture"); err != nil {
		t.Fatal(err)
	}
	agent := fakeAgent(t, strings.Replace(okResult, `"case":"x"`, `"case":"x","root":"root:shared"`, 1))
	r := runnerFor(t, st, agent)
	r.Ship = s
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	args, _ := agentArgs(t, agent)
	after, _ := st.CaseByID(ctx, c.ID)
	still, _, _ := s.Load(c.ID)
	if !strings.Contains(args, "was reviewed") || after.Into != "" || still.State != Checked || still.PR == "" {
		t.Fatalf("told of its review %v; the case is %s into %q; the package is %s on %q", strings.Contains(args, "was reviewed"),
			after.Status, after.Into, still.State, still.PR)
	}
}

// A build that finds a prepared change, which a build cut short left with uncommitted work, resumes it: a reported
// root leaves the case where it is, so its work tree stays its own.
func TestABuildResumesAPreparedChange(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	a := testingCase(t, st)
	p, err := s.Prepare(ctx, a.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42")) // the first build was cut short here
	b := openCase(t, st, "failures", "b", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:shared", "fixture"); err != nil {
		t.Fatal(err)
	}
	agent := fakeAgent(t, strings.Replace(okResult, `"case":"x"`, `"case":"x","root":"root:shared"`, 1))
	r := runnerFor(t, st, agent)
	r.Ship = s
	done, err := r.Once(ctx, Options{Case: a.ID, Families: []string{"failures"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0].Step != StepBuild {
		t.Fatalf("%+v", done)
	}
	after, _ := st.CaseByID(ctx, a.ID)
	args, _ := agentArgs(t, agent)
	if after.Into != "" || after.Status != store.CaseTesting || !strings.Contains(args, "resume it") {
		t.Fatalf("case %s: %s into %q; told to resume %v", a.ID, after.Status, after.Into, strings.Contains(args, "resume it"))
	}
}

// A loop that dies after its agent ended leaves the run's report in its stream: stale recovery records the report,
// not the findings file, so one that named a mechanism and a fix moves the case to testing.
func TestStaleRecoveryRecordsAKilledRunsReport(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "a", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	os.MkdirAll(run.Dir, 0o700)
	write(t, filepath.Join(run.Dir, "findings.md"), "## Mechanism\nstill settling it\n")
	write(t, filepath.Join(run.Dir, "transcript.jsonl"), investigatedResult+"\n")
	later := time.Now().Add(r.Budgets.RunTime + time.Hour)
	r.Now = func() time.Time { return later }
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Status != store.CaseTesting || got.NextTest != "take the lock first" {
		t.Fatalf("after stale recovery: %s, mechanism %q, fix %q", got.Status, got.Mechanism, got.NextTest)
	}
}

// The findings are recorded before the run ends: a run whose end cannot be recorded still leaves them.
func TestFindingsComeBeforeTheRunsEnd(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "a", 1)
	run := store.LoopRun{ID: "unreserved", Case: c.ID, Family: c.Family, Step: StepInvestigate, Started: time.Now(), Dir: t.TempDir()}
	res := Result{Status: store.RunOK, CostKnown: true, USD: 1,
		Report: []byte(`{"mechanism":"the export reads the lockfile first","alternatives":[],"fix":"take the lock first","summary":"found"}`)}
	r.finish(ctx, c, &run, res, nil)
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("a run whose end failed lost its findings: %+v", got)
	}
}
