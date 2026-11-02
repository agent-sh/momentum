package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A change is in progress while it is on its pull request (proposed, revising, landing, or checked again after a
// review), and an unreadable package counts as in progress; the runner's root and the case commands both ask this.
func TestAChangeIsOpenWhileItsPullRequestIs(t *testing.T) {
	st := testStore(t)
	s := &Ship{Store: st, Dir: t.TempDir()}
	if _, open := s.ChangeOpen("c00000001"); open {
		t.Fatal("a case with no package is open")
	}
	// a change before its proposal (prepared, checked with no pull request) is the build's to resume or end; one on its
	// pull request is its review's
	for _, tc := range []struct {
		state, pr string
		want      bool
	}{{Prepared, "", false}, {Checked, "", false}, {Checked, "https://example.com/pr/1", true}, {Proposed, "https://example.com/pr/1", true},
		{Revising, "https://example.com/pr/1", true}, {Landing, "https://example.com/pr/1", true}, {Landed, "", false}, {RolledBack, "", false},
		{Dropped, "", false}} {
		p := Package{Case: "c00000001", Kind: "repo", State: tc.state, PR: tc.pr}
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		if got, open := s.ChangeOpen("c00000001"); open != tc.want || got != tc.state {
			t.Errorf("%s %q: open %v (%q)", tc.state, tc.pr, open, got)
		}
	}
	write(t, filepath.Join(s.pkgDir("c00000001"), "package.json"), "{not json")
	if _, open := s.ChangeOpen("c00000001"); !open {
		t.Error("an unreadable package is not open")
	}
	if _, open := (*Ship)(nil).ChangeOpen("c00000001"); open {
		t.Error("no ship tool, yet open")
	}
}

// A revision whose report names a root another case holds leaves the root alone: a merge would close the case while
// its change is revising on its pull request.
func TestARevisionsReportedRootLeavesItsCaseOnItsPullRequest(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	a := openCase(t, st, "failures", "a", 1)
	mech, fix := "the lockfile is read before the lock", "take the lock first"
	if _, err := st.UpdateCase(ctx, a.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	b := openCase(t, st, "failures", "b", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:shared", "fixture"); err != nil {
		t.Fatal(err)
	}
	agent := fakeAgent(t, strings.Replace(okResult, `"case":"x"`, `"case":"x","root":"root:shared"`, 1))
	r := runnerFor(t, st, agent)
	var logged []string
	r.Log = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	p := Package{Case: a.ID, Kind: "repo", State: Revising, PR: "https://example.invalid/o/r/pull/7", Work: filepath.Join(r.Dir, "work", a.ID),
		Host: filepath.Join(r.Dir, "host", a.ID), BaseCommit: "synthetic-base",
		Reviews: []Review{{Round: 1, High: 1, Text: "round 1: one high finding", Comment: "https://example.invalid/o/r/pull/7#c1"}}}
	write(t, filepath.Join(p.Work, "fixture.txt"), "synthetic run clone")
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Case: a.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if live, _ := st.CaseLive(ctx, a.ID); live.ID != a.ID {
		t.Fatalf("the case went into %s by its report's root while its change is revising", live.ID)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "its change is revising on its pull request") {
		t.Fatalf("the runner did not say why it left the root: %q", logged)
	}
}

// A revision whose check passed and whose build was cut short before its proposal leaves the package checked, its
// pull request open: the change is in progress, the next build is told of its review, and a reported root leaves the
// case where it is.
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
	if _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	if p, err = proposePatch(t, s, ctx, c.ID, pr); err != nil || p.State != Proposed {
		t.Fatalf("propose: %+v %v", p, err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "no comment", Fix: "add one"}},
		Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || p.State != Revising {
		t.Fatalf("reviewed: %+v %v", p, err)
	}
	// the revision edits and checks, and is cut short before ship propose
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is the answer, 42.", 1))
	if p, err = checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
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

// A build that resumes a change before its proposal and ends well with the change where it began (it reports blocked:
// its plan cannot count the problem) is a stop, so two in a row trip the stop rule before the case's budget is gone;
// and once its case leaves testing (closed by hand here, as a build may close it), the next loop run drops the
// change: both independent clones go. Scope checks read the owner's checkout, never these clones.
func TestAChangeThatCannotGoOnEndsWithItsCase(t *testing.T) {
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
	if _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	none := Signal{Source: store.Transcripts, Kinds: []string{"tool_result"}, Tool: "Bash", Match: []string{"zz-matches-nothing"}}
	if _, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: none, Window: 7 * 24 * time.Hour}); err == nil {
		t.Fatal("a plan that counts nothing was proposed")
	}
	blocked := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":4,"session_id":"s1",` +
		`"structured_output":{"case":"x","step":"build","outcome":"blocked","summary":"the plan cannot count the problem","blocked_by":"ship propose"}}`
	agent := fakeAgent(t, blocked)
	r := runnerFor(t, st, agent)
	r.Ship = s
	var steps []string
	for i := 0; i < 4; i++ {
		done, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range done {
			steps = append(steps, d.Step+" "+d.Run.Status+" "+d.Run.Failure+" "+d.Skipped)
		}
	}
	got, _ := st.CaseByID(ctx, c.ID)
	health, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health", All: true})
	if got.Cost.Runs != 2 || len(health) != 1 || !strings.Contains(strings.Join(steps, "\n"), "no progress") {
		t.Fatalf("steps:\n%s\ncase %s over %d runs; loop cases %d", strings.Join(steps, "\n"), got.Status, got.Cost.Runs, len(health))
	}
	if args, _ := agentArgs(t, agent); !strings.Contains(args, "close the case or send it back") {
		t.Errorf("the build is not told it may end the case:\n%s", args)
	}
	c2 := testingCase2(t, st)
	if _, err := s.Gate.Repository(ctx, f.main); err != nil {
		t.Fatalf("scope checked the independent clones instead of the owner's checkout: %v", err)
	}
	if _, err := st.CloseCase(ctx, c.ID, store.NotWorthIt, "", "its plan cannot count the problem", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	pkg, _, _ := s.Load(c.ID)
	for _, dir := range []string{p.Work, p.Host} {
		if _, err := os.Stat(dir); pkg.State != Dropped || !os.IsNotExist(err) {
			t.Fatalf("the change of a closed case: %s, its clone %s: %v", pkg.State, dir, err)
		}
	}
	if out := run(t, f.main, "git", "branch", "--list", p.Branch); strings.TrimSpace(out) != "" {
		t.Errorf("its branch stays: %q", out)
	}
	if _, err := s.Prepare(ctx, c2.ID, f.main); err != nil {
		t.Errorf("another case's change, once the stuck one is dropped: %v", err)
	}
}

// A case sent back by hand while its change was before its proposal has its change dropped by the next loop run, so
// the build after its next investigation starts from its new findings, not the old change.
func TestAChangeWhoseCaseWentBackIsDropped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	if _, err := s.Prepare(ctx, c.ID, f.main); err != nil {
		t.Fatal(err)
	}
	mech := "not the answer: the export reads the lockfile first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &mech}, "cli"); err != nil {
		t.Fatal(err)
	}
	r := runnerFor(t, st, fakeAgent(t, investigatedResult))
	r.Ship = s
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if pkg, _, _ := s.Load(c.ID); pkg.State != Dropped {
		t.Fatalf("the change of a case sent back: %s", pkg.State)
	}
	if mid, _ := st.CaseByID(ctx, c.ID); mid.Status != store.CaseTesting {
		t.Fatalf("after its investigation the case is %s", mid.Status)
	}
	build := fakeAgent(t, okResult)
	r = runnerFor(t, st, build) // the harness's directory is mounted into its sandbox: a new harness, a new runner
	r.Ship = s
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if args, _ := agentArgs(t, build); strings.Contains(args, "resume it") || strings.Contains(args, "were checked") {
		t.Errorf("the build resumes the old change:\n%s", args)
	}
	// the case can be prepared again: its dropped change is replaced, not refused as a package it already has
	if p, err := s.Prepare(ctx, c.ID, f.main); err != nil || p.State != Prepared {
		t.Errorf("the case could not be prepared again after its change was dropped: %+v %v", p, err)
	}
}
