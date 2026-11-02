package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// hours puts n hours of n sessions of their own, one event each, spread over a span from a time: the first problem
// of them hold the problem goodSignal counts, the rest an ordinary result of the same tool.
func hours(t *testing.T, st *store.Store, prefix string, from time.Time, span time.Duration, n, problem int) []store.Event {
	t.Helper()
	var evs []store.Event
	for i := 0; i < n; i++ {
		text := "Exit code 0 ok"
		if i < problem {
			text = "Exit code 1 zz-signal-text: the thing broke"
		}
		at := from.Add(time.Duration(i) * span / time.Duration(max(n, 1)))
		evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s%d@%d", prefix, i, at.UnixNano()), TS: at,
			Session: fmt.Sprintf("%s%d", prefix, i), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: text, Meta: fmt.Sprintf(`{"is_error":%t}`, i < problem)})
	}
	if len(evs) > 0 {
		if _, err := st.Put(context.Background(), evs); err != nil {
			t.Fatal(err)
		}
	}
	return evs
}

// measured is a case whose change landed a day after its plan was counted: six of a hundred session hours held its
// problem in the day before the plan, ten more held it between the plan and the change, and its package's landed
// commit is the head of a repository's checkout. The measure step runs on a clock of the test's own.
type measured struct {
	r                *Runner
	st               *store.Store
	c                store.Case
	repo             string
	planAt, deployed time.Time
	clock            time.Time
}

func measuredCase(t *testing.T) *measured {
	t.Helper()
	return measuredCaseWithRead(t, true)
}

func measuredCaseWithRead(t *testing.T, read bool) *measured {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	m := &measured{st: st, r: runnerFor(t, st, nil)}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.deployed = time.Now().Add(-365 * 24 * time.Hour).Truncate(time.Hour)
	m.planAt = m.deployed.Add(-time.Hour)
	m.clock = m.deployed.Add(10 * time.Minute)
	before := hours(t, st, "b", m.planAt.Add(-24*time.Hour), 24*time.Hour, 100, 6)
	hours(t, st, "between", m.planAt.Add(time.Minute), 10*time.Minute, 10, 10) // neither window's
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	f := store.Finding{Family: "failures", Signature: "failures/v1:mm", Title: "the thing breaks"}
	for _, e := range before[:6] {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "m", "n"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: m.repo, PR: "https://github.com/agent-sh/fake/pull/7", State: Landed,
		Landed: []string{head}, LandedAt: m.deployed, Plan: &Plan{Signal: goodSignal, Window: 24 * time.Hour, Units: SessionHours, Before: 6,
			BeforeBase: 100, At: m.planAt}}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if m.c, err = st.DeployCase(ctx, c.ID, m.r.Ship.pkgDir(c.ID), m.deployed, "landed", "test"); err != nil {
		t.Fatal(err)
	}
	if read {
		if err := st.MarkRead(ctx, store.Transcripts, time.Now().Add(365*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func (m *measured) measure(t *testing.T, o Options) Measured {
	t.Helper()
	ms, err := m.r.Measure(context.Background(), o)
	if err != nil || len(ms) != 1 {
		t.Fatalf("the measure step: %+v %v", ms, err)
	}
	return ms[0]
}

func (m *measured) theCase(t *testing.T) store.Case {
	t.Helper()
	c, err := m.st.CaseByID(context.Background(), m.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (m *measured) notes(t *testing.T) []store.Note {
	t.Helper()
	ns, err := m.st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Note
	for _, n := range ns {
		if n.Done.IsZero() {
			out = append(out, n)
		}
	}
	return out
}

// A landed change is measured from when it is in effect: the repository's checkout holds its commits, and the file
// it is installed in, when its plan names one, changed after the landing. Until then it waits, and a problem note
// says so once it has waited a window; the note goes when the change is in effect, and its window after counts
// from then.
func TestAChangeIsMeasuredFromWhenItIsInEffect(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	run(t, m.repo, "git", "checkout", "-q", "-b", "elsewhere", "HEAD~0")
	write(t, filepath.Join(m.repo, "b.txt"), "b\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "other work")
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Landed = []string{strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))}
	m.r.Ship.save(&p)
	run(t, m.repo, "git", "checkout", "-q", "main")
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "not in effect") {
		t.Fatalf("a change the checkout does not hold: %+v", got)
	}
	if len(m.notes(t)) != 0 {
		t.Fatal("a note before the change waited a window")
	}
	m.clock = m.deployed.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || m.theCase(t).Status != store.CaseDeployed {
		t.Fatalf("a change not in effect after a window: %+v", got)
	}
	if ns := m.notes(t); len(ns) != 1 || ns[0].ID != "measure:"+m.c.ID || ns[0].Kind != store.NoteProblem || !strings.Contains(ns[0].Title, "not in effect") {
		t.Fatalf("the problem note %+v", ns)
	}
	noteAt := m.notes(t)[0].At
	m.clock = m.clock.Add(time.Hour)
	m.measure(t, Options{})
	if ns := m.notes(t); len(ns) != 1 || !ns[0].At.Equal(noteAt) {
		t.Fatalf("the problem note added again: %+v", ns)
	}
	run(t, m.repo, "git", "merge", "-q", "--ff-only", "elsewhere")
	if got := m.measure(t, Options{}); got.Status != "measuring" || !got.Due.Equal(m.clock.Add(24*time.Hour)) {
		t.Fatalf("in effect: %+v", got)
	}
	if c := m.theCase(t); c.Status != store.CaseMeasuring || !c.Deployed.Equal(m.clock) || c.Measurement.Before != 6 {
		t.Fatalf("the case measures from when its change is in effect: %+v", c)
	}
	if ns := m.notes(t); len(ns) != 0 {
		t.Fatalf("the problem note stands once the change is in effect: %+v", ns)
	}
	// a change the machine runs from a file outside the checkout is in effect once that file changed after the landing
	m2 := measuredCase(t)
	bin := filepath.Join(t.TempDir(), "tool")
	write(t, bin, "the old build")
	os.Chtimes(bin, m2.deployed.Add(-time.Hour), m2.deployed.Add(-time.Hour))
	p2, _, _ := m2.r.Ship.Load(m2.c.ID)
	p2.Plan.Installed = bin
	m2.r.Ship.save(&p2)
	if got := m2.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "has not changed since the checkout held the change") {
		t.Fatalf("an installed file not rebuilt: %+v", got)
	}
	os.Chtimes(bin, m2.deployed.Add(time.Minute), m2.deployed.Add(time.Minute))
	if got := m2.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("an installed file rebuilt after the landing: %+v", got)
	}
	_ = ctx
}

// A change whose problem's share of the active hours fell beyond chance closes its case as improved, its counts
// and p on the case, its package kept, with a shipped note of its effect. The window before ends at the plan and
// the window after starts when the change was in effect, so what came between counts in neither. A dry measure
// changes nothing. A problem it fixed that is back at its old share brings the case back, regressed, with a
// decided note; one below it does not.
func TestAChangeThatHelpedClosesItsCaseAsImproved(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Before, p.Plan.BeforeBase = 40, 400
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{Dry: true}); got.Status != "measuring" || m.theCase(t).Status != store.CaseDeployed {
		t.Fatalf("a dry measure of a deployed case: %+v %s", got, m.theCase(t).Status)
	}
	if done, err := m.r.Once(ctx, Options{Dry: true}); err != nil || !hasStep(done, StepMeasure, "measuring") {
		t.Fatalf("a run of the loop takes the measure step: %+v %v", done, err)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("start: %+v", got)
	}
	effect := m.clock
	hours(t, m.st, "a", effect.Add(time.Minute), 23*time.Hour, 100, 1)
	m.clock = effect.Add(23 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("before its window ended: %+v", got)
	}
	m.clock = effect.Add(25 * time.Hour)
	if got := m.measure(t, Options{Dry: true}); got.Status != "improved" || m.theCase(t).Status != store.CaseMeasuring || len(m.notes(t)) != 0 {
		t.Fatalf("a dry measure past its window: %+v", got)
	}
	got := m.measure(t, Options{})
	if got.Status != "improved" || got.Result.Before.Hits != 40 || got.Result.Before.Base != 400 || got.Result.After.Hits != 1 || got.Result.After.Base != 100 {
		t.Fatalf("measured %+v", got)
	}
	c := m.theCase(t)
	if c.Status != store.CaseClosed || c.Reason != store.Improved || c.Measurement.Before != 40 || c.Measurement.After != 1 ||
		!strings.Contains(c.Measurement.Effect, "40 of 400 active hours before, 1 of 100 after") {
		t.Fatalf("the case %+v", c)
	}
	if p, _, _ := m.r.Ship.Load(c.ID); p.State != Kept || p.Result == nil || p.Result.Before.Hits != 40 {
		t.Fatalf("the package %+v", p)
	}
	ns := m.notes(t)
	if len(ns) != 1 || ns[0].Kind != store.NoteShipped || !strings.Contains(ns[0].Effect, "p=") || len(ns[0].Links) != 1 {
		t.Fatalf("notes %+v", ns)
	}

}

func noteKinds(ns []store.Note) map[store.NoteKind]int {
	out := map[store.NoteKind]int{}
	for _, n := range ns {
		out[n.Kind]++
	}
	return out
}

func hasStep(done []Done, step, did string) bool {
	for _, d := range done {
		if d.Step == step && strings.HasPrefix(d.Did, did) {
			return true
		}
	}
	return false
}

// A change whose problem's share did not fall is rolled back by the ship tool, and only when the loop is live:
// while it is not, the rollback is held with a decided note, once, and the case keeps measuring. A rollback that
// fails leaves the case measuring with its reason, and the second in a row puts a problem note in the report. A
// case whose package was rolled back while the case still measures is taken back to testing.
func TestAChangeThatDidNotHelpIsRolledBackOnlyWhenLive(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	m.measure(t, Options{})
	effect := m.clock
	hours(t, m.st, "a", effect.Add(time.Minute), 23*time.Hour, 100, 6)
	m.clock = effect.Add(25 * time.Hour)
	for i := 0; i < 2; i++ {
		if got := m.measure(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "no measured effect") {
			t.Fatalf("not live: %+v", got)
		}
	}
	if ns := m.notes(t); len(ns) != 1 || ns[0].Kind != store.NoteDecided || !strings.Contains(ns[0].Detail, "momentum ship rollback "+m.c.ID) {
		t.Fatalf("the held rollback's note %+v", ns)
	}
	if m.theCase(t).Status != store.CaseMeasuring {
		t.Fatal("a held rollback moved the case")
	}
	m.r.Ship.Live = true // the rollback runs, and fails here: the fixture's checkout has no origin and the gate no owner
	got := m.measure(t, Options{})
	if got.Status != "failed" || !strings.Contains(got.Why, "the rollback") || m.theCase(t).Status != store.CaseMeasuring {
		t.Fatalf("a rollback that failed: %+v", got)
	}
	if log, _ := m.st.CaseLog(ctx, m.c.ID); log[len(log)-1].Action != "not rolled back" {
		t.Fatalf("its log %+v", log[len(log)-1])
	}
	for _, n := range m.notes(t) {
		if n.Kind == store.NoteProblem {
			t.Fatalf("a problem note after one failure: %+v", n)
		}
	}
	m.measure(t, Options{})
	problems := 0
	for _, n := range m.notes(t) {
		if n.Kind == store.NoteProblem && n.ID == "measure:"+m.c.ID && strings.Contains(n.Title, "cannot roll back") {
			problems++
		}
	}
	if problems != 1 {
		t.Fatalf("no problem note after two failures: %+v", m.notes(t))
	}
	// its revert went in and the case did not follow: the step finishes it
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.State = RolledBack
	m.r.Ship.save(&p)
	if got := m.measure(t, Options{}); got.Status != "rolled back" || m.theCase(t).Status != store.CaseTesting {
		t.Fatalf("a package rolled back: %+v %s", got, m.theCase(t).Status)
	}
	for _, n := range m.notes(t) {
		if n.Kind == store.NoteProblem {
			t.Fatalf("the problem note stands after the rollback went through: %+v", n)
		}
	}
}

// A window after too quiet for any count to show an effect waits one more window and then counts both; still too
// quiet, the change is rolled back, since nothing could show it helped.
func TestAWindowTooQuietToShowAnythingWaitsOneMore(t *testing.T) {
	for _, second := range []int{60, 0} {
		t.Run(fmt.Sprint(second), func(t *testing.T) {
			m := measuredCase(t)
			m.measure(t, Options{})
			effect := m.clock
			hours(t, m.st, "a", effect.Add(time.Minute), 23*time.Hour, 60, 0)
			m.clock = effect.Add(25 * time.Hour)
			got := m.measure(t, Options{})
			if got.Status != "waiting" || !strings.Contains(got.Why, "too quiet") || !got.Due.Equal(effect.Add(48*time.Hour)) {
				t.Fatalf("a quiet window: %+v", got)
			}
			hours(t, m.st, "s", effect.Add(25*time.Hour), 22*time.Hour, second, 0)
			m.clock = effect.Add(49 * time.Hour)
			got = m.measure(t, Options{})
			switch {
			case second > 0 && (got.Status != "improved" || got.Result.After.Base != 120):
				t.Fatalf("two windows that together can tell: %+v", got)
			case second == 0 && (got.Status != "held" || !strings.Contains(got.Why, "nothing could show it helped")):
				t.Fatalf("two windows too quiet: %+v", got)
			}
		})
	}
}

// A plan with no time it was counted at, or no units, measures nothing and rolls nothing back: its window before has
// no end.
func TestAPlanWithoutItsTimeOrUnitsMeasuresNothing(t *testing.T) {
	for _, broken := range []func(*Plan){func(p *Plan) { p.At = time.Time{} }, func(p *Plan) { p.Units = "" }} {
		m := measuredCase(t)
		p, _, _ := m.r.Ship.Load(m.c.ID)
		broken(p.Plan)
		m.r.Ship.save(&p)
		if got := m.measure(t, Options{}); got.Status != "failed" || m.theCase(t).Status != store.CaseDeployed {
			t.Fatalf("a broken plan: %+v", got)
		}
	}
}

// The measure step is narrowed as the run is: to the families and the case it names.
func TestTheMeasureStepIsNarrowedAsTheRunIs(t *testing.T) {
	m := measuredCase(t)
	for _, o := range []Options{{Case: "c-other"}, {Families: []string{"sessions"}}} {
		if ms, err := m.r.Measure(context.Background(), o); err != nil || len(ms) != 0 {
			t.Fatalf("%+v measured %+v %v", o, ms, err)
		}
	}
	if got := m.measure(t, Options{Families: []string{"failures"}, Case: m.c.ID}); got.Case != m.c.ID {
		t.Fatalf("its own family and case: %+v", got)
	}
	if m.theCase(t).Status != store.CaseMeasuring {
		t.Fatal("the case was not measured")
	}
	_ = errors.Is
}

// The loop's six investigation and review sessions can quote the problem without fixing it. They count in
// neither window, even when their results fail; successful quotes outside the loop are no hits either.
func TestTheLoopsOwnRunsCannotMakeAnUnchangedProblemImprove(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	// Replace the fixture's six hits with one, keeping all 100 activity units.
	before := hours(t, m.st, "b", m.planAt.Add(-24*time.Hour), 24*time.Hour, 100, 1)
	f := store.Finding{Family: "failures", Signature: "failures/v1:single", Title: "one failure", Evidence: []store.Evidence{{Ref: "ev:" + before[0].ID()}}}
	c, _, err := m.st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		var evs []store.Event
		for i, name := range []string{"investigate", "build", "review1", "revise1", "review2", "revise2"} {
			cwd := filepath.Join(m.r.Dir, "runs", name)
			if i == 0 {
				cwd = m.r.Dir
			}
			evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: name, Session: name, TS: m.planAt.Add(-time.Hour),
				Cwd: cwd, Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "evidence: zz-signal-text", Meta: fmt.Sprintf(`{"is_error":%t}`, failed)})
		}
		if _, err := m.st.Put(ctx, evs); err != nil {
			t.Fatal(err)
		}
		if _, err := m.r.Ship.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", m.planAt); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "in 1 session hours") {
			t.Fatalf("the loop passed the gate: %v", err)
		}
		got, err := Tally(ctx, m.st, goodSignal, SessionHours, m.planAt.Add(-24*time.Hour), m.planAt, m.r.Dir)
		if err != nil || got.Hits != 1 || got.Base != 100 {
			t.Fatalf("before %+v %v", got, err)
		}
	}
	p.Plan.Before, p.Plan.BeforeBase = 1, 100
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.measure(t, Options{})
	effect := m.clock
	hours(t, m.st, "after", effect.Add(time.Minute), 23*time.Hour, 100, 1)
	// A successful result quotes the failure outside the loop; it adds activity, never a hit.
	e := store.Event{Source: store.Transcripts, SourceKey: "quote", Session: "quote", Actor: store.ActorTool, TS: effect.Add(time.Hour), Kind: "tool_result", Tool: "Bash",
		Summary: "zz-signal-text", Meta: `{"is_error":false}`}
	if _, err := m.st.Put(ctx, []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	e.SourceKey, e.Session, e.Cwd, e.Meta = "loop-after", "loop-after", filepath.Join(m.r.Dir, "work", "case"), `{"is_error":true}`
	if _, err := m.st.Put(ctx, []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := Tally(ctx, m.st, goodSignal, SessionHours, effect, effect.Add(24*time.Hour), m.r.Dir)
	if err != nil || got.Hits != 1 || got.Base != 101 {
		t.Fatalf("after %+v %v", got, err)
	}
	m.clock = effect.Add(49 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "held" {
		t.Fatalf("unchanged problem: %+v", got)
	}
}

// An installed file built after landing but before the checkout took the change still holds the old code. The
// latest touched file dates the checkout; a later build starts measurement. Deleted files use the landing.
func TestAnInstalledFileMustFollowTheCheckoutChange(t *testing.T) {
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	bin := filepath.Join(t.TempDir(), "tool")
	write(t, bin, "old build")
	run(t, m.repo, "git", "checkout", "-q", "-b", "change")
	write(t, filepath.Join(m.repo, "b.txt"), "the change")
	run(t, m.repo, "git", "add", "b.txt")
	run(t, m.repo, "git", "commit", "-q", "-m", "the landed change")
	p.Landed = []string{strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))}
	run(t, m.repo, "git", "checkout", "-q", "main")
	p.Plan.Installed, p.Changed = bin, []string{"a.txt", "b.txt", "deleted.txt"}
	checkoutAt := m.deployed.Add(time.Hour)
	if err := os.Chtimes(bin, m.deployed.Add(time.Minute), m.deployed.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("checkout without change: %+v", got)
	}
	run(t, m.repo, "git", "merge", "-q", "--ff-only", "change")
	if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), m.deployed, m.deployed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(m.repo, "b.txt"), checkoutAt, checkoutAt); err != nil {
		t.Fatal(err)
	}
	m.clock = checkoutAt.Add(time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "checkout") {
		t.Fatalf("old build: %+v", got)
	}
	if err := os.Chtimes(bin, m.clock, m.clock); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("rebuilt: %+v", got)
	}
}

// Every measuring run checks the change again. Losing it holds the decision, posts a problem after a window,
// and restarts from its return with a log entry; activity during its absence cannot keep the change.
func TestAMeasuringChangeRestartsAfterItReturnsToEffect(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	m.clock = m.clock.Add(10 * 24 * time.Hour)
	m.measure(t, Options{})
	old := m.clock
	run(t, m.repo, "git", "checkout", "-q", "--orphan", "other")
	run(t, m.repo, "git", "commit", "-q", "--allow-empty", "-m", "other tree")
	m.clock = old.Add(time.Minute)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "not in effect") {
		t.Fatalf("lost: %+v", got)
	}
	if len(m.notes(t)) != 0 {
		t.Fatal("a fresh gap was dated from landing")
	}
	hours(t, m.st, "absent", old.Add(time.Hour), 22*time.Hour, 100, 0)
	m.clock = old.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || got.Result != nil {
		t.Fatalf("decided while absent: %+v", got)
	}
	if ns := m.notes(t); len(ns) != 1 || !strings.Contains(ns[0].Title, "not in effect") {
		t.Fatalf("no problem: %+v", ns)
	}
	run(t, m.repo, "git", "checkout", "-q", "main")
	if got := m.measure(t, Options{}); got.Status != "measuring" || !got.Due.Equal(m.clock.Add(24*time.Hour)) {
		t.Fatalf("restart: %+v", got)
	}
	if !m.theCase(t).Deployed.Equal(m.clock) || len(m.notes(t)) != 0 {
		t.Fatalf("restart not saved or note unresolved: %+v", m.theCase(t))
	}
	log, err := m.st.CaseLog(ctx, m.c.ID)
	if err != nil || log[len(log)-1].Action != "measuring restarted" {
		t.Fatalf("restart log: %+v %v", log, err)
	}
	m.clock = m.clock.Add(23 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("old window decided: %+v", got)
	}
}

// A keep interrupted after the close is finished on the next run, even for an older package with no saved
// result. Keeping twice is harmless. The recovered package is watched and can bring its case back.
func TestAKeepInterruptedAfterTheCloseIsFinished(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	m.measure(t, Options{})
	hours(t, m.st, "after", m.clock.Add(time.Minute), 23*time.Hour, 100, 0)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	res, _ := measure.Compare(measure.Counts{Hits: 6, Base: 100}, measure.Counts{Hits: 0, Base: 100})
	if _, err := m.st.CloseMeasured(ctx, m.c.ID, 0, res.Effect, "test"); err != nil {
		t.Fatal(err)
	}
	m.clock = m.theCase(t).Closed
	if ms, err := m.r.Measure(ctx, Options{Dry: true}); err != nil || len(ms) != 0 {
		t.Fatalf("dry recovery: %+v %v", ms, err)
	}
	if p, _, _ := m.r.Ship.Load(m.c.ID); p.State != Landed {
		t.Fatal("dry recovery saved")
	}
	if os.Geteuid() != 0 {
		dir := m.r.Ship.pkgDir(m.c.ID)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		ms, err := m.r.Measure(ctx, Options{})
		if restore := os.Chmod(dir, 0o700); restore != nil {
			t.Fatal(restore)
		}
		if err != nil || len(ms) != 1 || ms[0].Status != "failed" {
			t.Fatalf("keep save failure: %+v %v", ms, err)
		}
		if m.theCase(t).Status != store.CaseClosed {
			t.Fatal("failed keep lost the close")
		}
	}
	if _, err := m.r.Measure(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.State != Kept || p.Result == nil || p.Result.After.Base != 100 {
		t.Fatalf("keep not recovered: %+v", p)
	}
	if _, err := m.r.Ship.Keep(m.c.ID, res); err != nil {
		t.Fatalf("keep twice: %v", err)
	}
	for i := 0; i < 3; i++ {
		hours(t, m.st, fmt.Sprint("back", i), m.clock.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, 12)
	}
	m.clock = m.clock.Add(72 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "back" {
		t.Fatalf("recovered keep not watched: %+v", got)
	}
}

// Watch reads each completed window once, saves the last three separately and leaves quiet progress untouched when dry.
func TestWatchReadsEachCompletedWindowOnce(t *testing.T) {
	ctx := context.Background()
	m := keptCase(t)
	closed := m.theCase(t).Closed
	for i := 0; i < 3; i++ {
		hours(t, m.st, fmt.Sprint("window", i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, 6)
	}
	m.clock = closed.Add(47 * time.Hour)
	before, _ := os.ReadFile(filepath.Join(m.r.Ship.pkgDir(m.c.ID), "package.json"))
	if ms, err := m.r.Measure(ctx, Options{Dry: true}); err != nil || len(ms) != 0 {
		t.Fatalf("dry quiet: %+v %v", ms, err)
	}
	after, _ := os.ReadFile(filepath.Join(m.r.Ship.pkgDir(m.c.ID), "package.json"))
	if string(before) != string(after) {
		t.Fatal("dry watch saved quiet progress")
	}
	if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("partial: %+v %v", ms, err)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 1 || len(p.WatchRecent) != 1 || p.WatchRecent[0] != (measure.Counts{Hits: 6, Base: 100}) {
		t.Fatalf("progress: %+v", p)
	}
	// Re-reading the first window would now break the run of three.
	hours(t, m.st, "late", closed.Add(time.Hour), time.Hour, 1000, 0)
	m.clock = closed.Add(48 * time.Hour)
	if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("second: %+v %v", ms, err)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 2 || len(p.WatchRecent) != 2 {
		t.Fatalf("progress: %+v", p)
	}
	m.clock = closed.Add(72 * time.Hour)
	if got := m.measure(t, Options{Dry: true}); got.Status != "back" || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("dry back: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 2 {
		t.Fatal("dry back saved progress")
	}
	if got := m.measure(t, Options{}); got.Status != "back" || !strings.Contains(got.Why, "window 3") {
		t.Fatalf("back: %+v", got)
	}
	if m.theCase(t).Status != store.CaseOpen {
		t.Fatal("case not reopened")
	}
	for _, n := range m.notes(t) {
		if n.Kind == store.NoteDecided && strings.Contains(n.Detail, "lesson in tacit is not withdrawn") {
			return
		}
	}
	t.Fatal("back note omits lesson status")
}

func keptCase(t *testing.T) *measured {
	t.Helper()
	m := measuredCase(t)
	m.measure(t, Options{})
	hours(t, m.st, "after", m.clock.Add(time.Minute), 23*time.Hour, 100, 0)
	m.clock = m.clock.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("keep: %+v", got)
	}
	return m
}

// An empty window is named for where it fell. Two empty windows after are too quiet to tell, not evidence of
// missing activity before the change.
func TestAnEmptyWindowIsNamedInTheRollback(t *testing.T) {
	for _, emptyBefore := range []bool{false, true} {
		m := measuredCase(t)
		if emptyBefore {
			p, _, _ := m.r.Ship.Load(m.c.ID)
			p.Plan.Before, p.Plan.BeforeBase = 0, 0
			if err := m.r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
		}
		m.measure(t, Options{})
		m.clock = m.clock.Add(49 * time.Hour)
		got := m.measure(t, Options{})
		want := "no activity in the two windows after"
		if emptyBefore {
			want = "no activity in the window before"
		}
		if got.Status != "held" || !strings.Contains(got.Why, want) {
			t.Fatalf("empty before=%v: %+v", emptyBefore, got)
		}
		if !emptyBefore && !strings.Contains(got.Why, "too quiet to tell") {
			t.Fatal(got.Why)
		}
	}
}

// A rollback by hand leaves the case in testing. Its next measure run resolves either kind of problem note;
// dry runs and runs narrowed to another case leave it alone.
func TestAHandRollbackResolvesItsMeasurementProblem(t *testing.T) {
	ctx := context.Background()
	for _, title := range []string{"cannot roll back", "not in effect"} {
		m := measuredCase(t)
		m.measure(t, Options{})
		p, _, _ := m.r.Ship.Load(m.c.ID)
		m.r.problem(ctx, &p, title, "waiting")
		p.State = RolledBack
		if err := m.r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		if _, err := m.st.RollBackCase(ctx, m.c.ID, "by hand", "test"); err != nil {
			t.Fatal(err)
		}
		for _, o := range []Options{{Dry: true}, {Case: "another"}} {
			if _, err := m.r.Measure(ctx, o); err != nil {
				t.Fatal(err)
			}
			if len(m.notes(t)) != 1 {
				t.Fatal("note resolved outside scope")
			}
		}
		if _, err := m.r.Measure(ctx, Options{}); err != nil {
			t.Fatal(err)
		}
		if len(m.notes(t)) != 0 {
			t.Fatalf("note still stands: %+v", m.notes(t))
		}
		p, _, _ = m.r.Ship.Load(m.c.ID)
		if p.Problem != "" {
			t.Fatal("package still holds problem")
		}
	}
}

// A quiet window that holds hits still waits by its activity alone. Reading its hits to choose the wait would
// decide early. The shipped note names the full two-window span once both together can tell.
func TestAQuietWindowWaitsWithoutReadingItsHits(t *testing.T) {
	m := measuredCase(t)
	m.measure(t, Options{})
	start := m.clock
	hours(t, m.st, "quiet", start.Add(time.Minute), 23*time.Hour, 60, 6)
	m.clock = start.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !got.Due.Equal(start.Add(48*time.Hour)) {
		t.Fatalf("peeked at hits: %+v", got)
	}
	hours(t, m.st, "busy", start.Add(25*time.Hour), 22*time.Hour, 1000, 0)
	m.clock = start.Add(49 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "improved" || got.Result.After.Hits != 6 || got.Result.After.Base != 1060 {
		t.Fatalf("two windows: %+v", got)
	}
	for _, n := range m.notes(t) {
		if n.Kind == store.NoteShipped && strings.Contains(n.Detail, "2d") {
			return
		}
	}
	t.Fatal("shipped note did not name both windows")
}

// A real fixture origin lets the step's rollback finish. Landing and rollback each leave their decided note,
// and the successful rollback resolves the measurement's problem instead of leaving an alarm behind.
func TestTheStepLandsAndRollsBackWithItsNotes(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, _ := reviewedChange(t, s, st, f)
	r := runnerFor(t, st, nil)
	r.Ship, r.Dir = s, s.Dir
	clock := time.Now().Truncate(time.Millisecond)
	r.Now = func() time.Time { return clock }
	s.Now = r.Now
	done := r.reviews(ctx, c.ID)
	if len(done) != 1 || done[0].Step != "land" || done[0].Skipped != "" {
		t.Fatalf("landing: %+v", done)
	}
	m := &measured{r: r, st: st, c: c}
	ns := m.notes(t)
	if len(ns) != 1 || ns[0].Kind != store.NoteDecided || !strings.Contains(ns[0].Detail, "momentum ship rollback "+c.ID) {
		t.Fatalf("landing note: %+v", ns)
	}
	run(t, f.main, "git", "fetch", "origin", "main")
	run(t, f.main, "git", "merge", "--ff-only", "origin/main")
	p, _, _ := s.Load(c.ID)
	p.Plan.Before, p.Plan.BeforeBase, p.Plan.Window = 6, 100, 24*time.Hour
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	m.measure(t, Options{})
	hours(t, st, "after", clock.Add(time.Minute), 23*time.Hour, 100, 6)
	clock = clock.Add(25 * time.Hour)
	if err := st.MarkRead(ctx, store.Transcripts, clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	r.problem(ctx, &p, "cannot roll back", "a prior failure")
	if got := m.measure(t, Options{}); got.Status != "rolled back" {
		t.Fatalf("rollback: %+v", got)
	}
	if m.theCase(t).Status != store.CaseTesting {
		t.Fatal("rollback did not move the case")
	}
	if body := run(t, f.dir, "git", "--git-dir", f.origin, "show", "main:fake.go"); !strings.Contains(body, "return 41") {
		t.Fatal("origin did not roll back")
	}
	ns = m.notes(t)
	if kinds := noteKinds(ns); kinds[store.NoteDecided] != 2 || kinds[store.NoteProblem] != 0 {
		t.Fatalf("rollback notes: %+v", ns)
	}
	for _, n := range ns {
		if strings.Contains(n.Title, "rolled back") && strings.Contains(n.Detail, "no measured effect") {
			return
		}
	}
	t.Fatal("rollback decided note missing")
}

// A late run reads every completed window: three windows at the old share cannot hide behind a quiet fourth.
func TestWatchCatchesAnEndedRunOfWindowsOnALateRun(t *testing.T) {
	m := keptCase(t)
	closed := m.theCase(t).Closed
	for i := 0; i < 4; i++ {
		hits := 6
		if i == 3 {
			hits = 0
		}
		hours(t, m.st, fmt.Sprint(i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, hits)
	}
	m.clock = closed.Add(96 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "back" || !strings.Contains(got.Why, "window 3") {
		t.Fatalf("late: %+v", got)
	}
}

// A late return at the baseline share has the same three-window rule as an immediate one, across reloads.
func TestWatchCatchesALateReturnAtTheOldShare(t *testing.T) {
	ctx := context.Background()
	for _, delay := range []int{0, 6, 26, 104} {
		m := keptCase(t)
		closed := m.theCase(t).Closed
		p, _, _ := m.r.Ship.Load(m.c.ID)
		p.Result.Before, p.Result.After = measure.Counts{Hits: 30, Base: 300}, measure.Counts{Hits: 5, Base: 100}
		if err := m.r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < delay+3; i++ {
			hits := 5
			if i >= delay {
				hits = 10
			}
			hours(t, m.st, fmt.Sprint("watch", i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, hits)
			m.clock = closed.Add(time.Duration(i+1) * 24 * time.Hour)
			ms, err := m.r.Measure(ctx, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if i < delay+2 && len(ms) != 0 {
				t.Fatalf("early: %+v", ms)
			}
			if i == delay+2 && (len(ms) != 1 || ms[0].Status != "back") {
				t.Fatalf("missed return: %+v", ms)
			}
		}
	}
}
