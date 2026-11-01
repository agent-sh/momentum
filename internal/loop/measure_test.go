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
			Session: fmt.Sprintf("%s%d", prefix, i), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: text})
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
	ctx := context.Background()
	st := testStore(t)
	m := &measured{st: st, r: runnerFor(t, st, nil)}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.deployed = time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
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
	m.measure(t, Options{})
	if ns := m.notes(t); len(ns) != 1 {
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
	if got := m2.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "has not changed since the landing") {
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
	hours(t, m.st, "a", effect.Add(time.Minute), 23*time.Hour, 100, 0)
	m.clock = effect.Add(23 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("before its window ended: %+v", got)
	}
	m.clock = effect.Add(25 * time.Hour)
	if got := m.measure(t, Options{Dry: true}); got.Status != "improved" || m.theCase(t).Status != store.CaseMeasuring || len(m.notes(t)) != 0 {
		t.Fatalf("a dry measure past its window: %+v", got)
	}
	got := m.measure(t, Options{})
	if got.Status != "improved" || got.Result.Before.Hits != 6 || got.Result.Before.Base != 100 || got.Result.After.Hits != 0 || got.Result.After.Base != 100 {
		t.Fatalf("measured %+v", got)
	}
	c := m.theCase(t)
	if c.Status != store.CaseClosed || c.Reason != store.Improved || c.Measurement.Before != 6 || c.Measurement.After != 0 ||
		!strings.Contains(c.Measurement.Effect, "6 of 100 active hours before, 0 of 100 after") {
		t.Fatalf("the case %+v", c)
	}
	if p, _, _ := m.r.Ship.Load(c.ID); p.State != Kept || p.Result == nil || p.Result.Before.Hits != 6 {
		t.Fatalf("the package %+v", p)
	}
	ns := m.notes(t)
	if len(ns) != 1 || ns[0].Kind != store.NoteShipped || !strings.Contains(ns[0].Effect, "p=0.014") || len(ns[0].Links) != 1 {
		t.Fatalf("notes %+v", ns)
	}
	// three of a hundred hours a week later: below its old share, it stays closed
	later := effect.Add(8 * 24 * time.Hour)
	hours(t, m.st, "c", later.Add(-23*time.Hour), 22*time.Hour, 100, 3)
	m.clock = later
	if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("below its old share: %+v %v", ms, err)
	}
	// six of a hundred: back at its old share
	hours(t, m.st, "d", later.Add(-22*time.Hour), 20*time.Hour, 4, 4)
	m.clock = later.Add(time.Hour)
	if got := m.measure(t, Options{Dry: true}); got.Status != "back" || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("a dry look at a problem back: %+v", got)
	}
	if got := m.measure(t, Options{}); got.Status != "back" {
		t.Fatalf("back: %+v", got)
	}
	if c := m.theCase(t); c.Status != store.CaseOpen || c.Reason != "" {
		t.Fatalf("the case is back: %+v", c)
	}
	if log, _ := m.st.CaseLog(ctx, m.c.ID); log[len(log)-1].Action != "regressed" {
		t.Fatalf("its log %+v", log[len(log)-1])
	}
	if kinds := noteKinds(m.notes(t)); kinds[store.NoteShipped] != 1 || kinds[store.NoteDecided] != 1 {
		t.Fatalf("notes %+v", m.notes(t))
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
