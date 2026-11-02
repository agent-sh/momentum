package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A changed installed file is checked against the checkout as it stood when the file was written: a rebuild from a
// branch cut before the change is out of effect, though the checkout is back on the change by the next run.
func TestARebuildFromAnOlderTreeStaysOutOfEffect(t *testing.T) {
	m := measuredCase(t)
	ctx := context.Background()
	p, _, _ := m.r.Ship.Load(m.c.ID)
	landed := p.Landed[0]
	p.Plan.Installed, p.Changed = filepath.Join(t.TempDir(), "tool"), []string{"a.txt"}
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	datedGitIn(t, m.repo, base, "checkout", "-q", landed)
	write(t, p.Plan.Installed, "a build of the change")
	os.Chtimes(filepath.Join(m.repo, "a.txt"), base, base)
	os.Chtimes(p.Plan.Installed, base.Add(10*time.Minute), base.Add(10*time.Minute))
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("the build of the change: %s %v", why, err)
	}
	datedGitIn(t, m.repo, base.Add(time.Hour), "checkout", "-q", "--orphan", "before")
	datedGitIn(t, m.repo, base.Add(time.Hour), "commit", "-qm", "a branch cut before the change")
	write(t, p.Plan.Installed, "a rebuild without the change")
	os.Chtimes(p.Plan.Installed, base.Add(70*time.Minute), base.Add(70*time.Minute))
	datedGitIn(t, m.repo, base.Add(2*time.Hour), "checkout", "-q", landed) // back before the next run
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why == "" {
		t.Fatalf("a rebuild from an older tree read as in effect: %v", err)
	}
	write(t, p.Plan.Installed, "a rebuild of the change")
	os.Chtimes(p.Plan.Installed, base.Add(150*time.Minute), base.Add(150*time.Minute))
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("a rebuild from the change again: %s %v", why, err)
	}
}

// A command's source that has not finished a read for a whole window stopped: the step decides without it, from
// both sides, and its note names it, rather than wait on it for good.
func TestAStoppedCommandSourceHoldsNoDecision(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Sources = []string{store.Transcripts, "capacity"}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := m.st.MarkRead(ctx, "capacity", m.deployed.Add(-2*time.Hour)); err != nil { // its timer stopped before the change
		t.Fatal(err)
	}
	// the three windows before the gated one hold the problem at its old share, as the plan counted it
	hours(t, m.st, "baseline", m.planAt.Add(-4*24*time.Hour), 3*24*time.Hour, 300, 18)
	m.measure(t, Options{})
	start := m.clock
	hours(t, m.st, "after", start, 23*time.Hour, 100, 0)
	m.clock = start.Add(25 * time.Hour)
	if err := m.st.MarkRead(ctx, store.Transcripts, m.clock); err != nil {
		t.Fatal(err)
	}
	got := m.measure(t, Options{})
	if got.Status == "waiting" || strings.Contains(got.Why, "capacity") {
		t.Fatalf("a stopped capacity source held the decision: %+v", got)
	}
	log, _ := m.st.CaseLog(ctx, m.c.ID)
	noted := false
	for _, e := range log {
		noted = noted || strings.Contains(e.Detail, "left out of both counts: capacity")
	}
	if !noted {
		t.Fatalf("no note names the stopped source: %+v", log)
	}
}

// A testing case whose proposal waits on a source's read is passed over until the source reads past what the
// proposal needs: a build now would be refused the same way.
func TestAProposalWaitingOnAReadIsPassedOver(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	c := testingCase(t, st)
	now := time.Now()
	r.Now = func() time.Time { return now }
	// another reader of the sessions' activity, which has not read past what the proposal needs
	if _, err := st.Put(ctx, []store.Event{{Source: "zz-reader", SourceKey: "activity", TS: now.Add(-2 * time.Hour), Session: "s1", Actor: store.ActorDetector,
		Kind: "ordinary", Summary: "ordinary activity"}}); err != nil {
		t.Fatal(err)
	}
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", State: Checked, ReadWait: now.Add(-time.Hour), ReadUntil: now.Add(-time.Hour),
		Plan: &Plan{Signal: goodSignal, Window: 7 * 24 * time.Hour, Units: SessionHours, At: now.Add(-time.Hour), Sources: []string{store.Transcripts, "zz-reader"}}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	picked, _, why, err := r.pick(ctx, c.Family, "", false)
	if err != nil || picked.ID != "" || !strings.Contains(why, "waits on a read of zz-reader") {
		t.Fatalf("picked %q: %s %v", picked.ID, why, err)
	}
	if err := st.MarkRead(ctx, "zz-reader", now); err != nil {
		t.Fatal(err)
	}
	if picked, step, _, err := r.pick(ctx, c.Family, "", false); err != nil || picked.ID != c.ID || step != StepBuild {
		t.Fatalf("once read: %q %s %v", picked.ID, step, err)
	}
}

// datedGitIn runs git with its author and committer dates, and so its reflog's, at a moment.
func datedGitIn(t *testing.T, dir string, at time.Time, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+at.Format(time.RFC3339), "GIT_COMMITTER_DATE="+at.Format(time.RFC3339),
		"GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.com", "GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// A reader that began inside the plan's counted span adds activity the baseline held in part or not at all: the plan
// leaves it out of both sides and names it.
func TestAReaderThatBeganInsideTheSpanIsLeftOut(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	if _, err := st.Put(ctx, []store.Event{{Source: "zz-new-reader", SourceKey: "first", TS: now.Add(-3 * 24 * time.Hour), Session: "s1", Actor: store.ActorDetector,
		Kind: "ordinary", Summary: "its first activity"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, "zz-new-reader", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s := Ship{Store: st}
	plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	if err != nil || strings.Contains(strings.Join(plan.Sources, ","), "zz-new-reader") || !strings.Contains(strings.Join(plan.Omitted, ","), "zz-new-reader") {
		t.Fatalf("a reader begun inside the span: %+v %v", plan, err)
	}
}
