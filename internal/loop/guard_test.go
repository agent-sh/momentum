package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The guard tests use only what the measured path already had (a plan's and a result's JSON, the measure step's
// status and words, the case and its notes), so that against the code before guards they run and fail rather than not
// build: there a guard's plan reads as a measured one with no baseline.

// toolSignal counts a tool's failed results in a source of its own, as a tool that logs its calls would give.
var toolSignal = Signal{Source: "zz-tool", Kinds: []string{"tool_result"}, Tool: "Bash", Match: []string{"zz-tool-broke"}}

const aDay = 24 * time.Hour

// toolResult is a result of the tool in a session: failed with the signal's text, or a success.
func toolResult(key string, at time.Time, session string, failed bool) store.Event {
	text := "Exit code 0 ok"
	if failed {
		text = "Exit code 1 zz-tool-broke: the tool failed"
	}
	return store.Event{Source: toolSignal.Source, SourceKey: key, TS: at, Session: session, Harness: "claude", Actor: store.ActorTool,
		Kind: "tool_result", Tool: "Bash", Summary: text, Meta: fmt.Sprintf(`{"is_error":%t}`, failed)}
}

// stoppedProblem is the store of a tool whose source began with an incident: from a clock hour, for hours hours, five
// failed results an hour in a session of the hour's own; then, until quietUntil, one successful result an hour, each in a
// session of its own. The source is read to read. It returns a case in testing whose evidence is the first fifty failures.
func stoppedProblem(t *testing.T, st *store.Store, from time.Time, hours int, quietUntil, read time.Time) store.Case {
	t.Helper()
	ctx := context.Background()
	var evs, failures []store.Event
	for h := 0; h < hours; h++ {
		for j := 0; j < 5; j++ {
			e := toolResult(fmt.Sprintf("inc-%d-%d", h, j), from.Add(time.Duration(h)*time.Hour+time.Duration(j)*12*time.Minute), fmt.Sprint("inc-", h), true)
			evs, failures = append(evs, e), append(failures, e)
		}
	}
	for i, at := 0, from.Add(time.Duration(hours)*time.Hour); at.Before(quietUntil); i, at = i+1, at.Add(time.Hour) {
		evs = append(evs, toolResult(fmt.Sprint("ok-", i), at, fmt.Sprint("ok-", i), false))
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, toolSignal.Source, read); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v2:zz-tool", Title: "the tool fails"}
	for _, e := range failures[:50] {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "the tool's state goes stale", "a test that makes it stale"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, st, c, "main")
	return c
}

// guardJSON is a guard as a plan's JSON records it.
type guardJSON struct {
	Instead   string        `json:"instead"`
	Since     time.Time     `json:"since"`
	Gap       time.Duration `json:"gap"`
	Incidents int           `json:"incidents"`
	Exposure  int           `json:"exposure"`
	Quiet     int           `json:"quiet"`
	Need      int           `json:"need"`
	Watch     time.Duration `json:"watch"`
}

func guardOf(t *testing.T, plan any) *guardJSON {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Guard *guardJSON `json:"guard"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got.Guard
}

// The example: a tool failed hundreds of times in one burst of two and a half days, then stopped, eight days before the
// proposal. Its source began with the burst, so a measured plan is too new; the measured path refused every plan for it.
// A guard stands in: the burst is one incident (its gaps are minutes, never a week), its exposure is the hours the tool
// was used (60 in the burst, 132 after it), so E = ln(20) * 192 = 576 such hours, which the history's eight days of use
// reach in 24 days: a watch of four weeks. A case the measured path can measure still gets its measured plan.
func TestAGuardStandsInOnlyWhereAMeasuredPlanIsRefused(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour)
	t.Run("a problem that stopped", func(t *testing.T) {
		st := testStore(t)
		from := now.Add(-8 * aDay)
		c := stoppedProblem(t, st, from, 60, now, now.Add(time.Hour))
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
		if err != nil {
			t.Fatalf("a preventive fix for a problem that stopped is refused: %v", err)
		}
		g := guardOf(t, plan)
		if g == nil {
			t.Fatalf("a measured plan for a problem too new to measure: %+v", plan)
		}
		if g.Incidents != 1 || g.Exposure != 192 || g.Quiet != 132 || g.Need != 576 || g.Watch != 28*aDay || g.Gap != 7*aDay || !g.Since.Equal(from) ||
			!strings.Contains(g.Instead, "began inside the 28d before") {
			t.Fatalf("the guard: %+v", *g)
		}
		if plan.Units != SessionHours || plan.Before != 0 || plan.BeforeBase != 0 || plan.Gate != 0 || plan.Window != 7*aDay {
			t.Fatalf("a guard's plan holds no measured counts: %+v", plan)
		}
	})
	t.Run("a measurable case", func(t *testing.T) { // the measured path as it was: this passes before guards too
		st := testStore(t)
		c := testingCase(t, st)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, goodSignal, 7*aDay, "", time.Now())
		if err != nil || guardOf(t, plan) != nil || plan.Before != 6 || plan.BeforeBase != 6 || plan.Gate != 6 {
			t.Fatalf("a measurable case's plan: %+v %v", plan, err)
		}
	})
}

// A guard is refused where it cannot say anything: a history whose exposure all lies inside its incident never saw the
// tool in use without the problem, so how often the problem starts cannot be estimated; and one incident in twenty days
// of history asks a watch of about sixty days, longer than the 28 days the loop counts after any change. Each refusal
// names the measured plan's too.
func TestAGuardIsRefusedOnAThinHistoryOrALongWatch(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour)
	st := testStore(t)
	from := now.Add(-61 * time.Hour) // the proposal an hour after the burst, nothing used since
	c := stoppedProblem(t, st, from, 60, from.Add(60*time.Hour), now.Add(time.Hour))
	_, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
	if err == nil || !strings.Contains(err.Error(), "began inside the 28d before") || !strings.Contains(err.Error(), "too thin") {
		t.Fatalf("a history all inside its incident: %v", err)
	}
	st = testStore(t)
	c = stoppedProblem(t, st, now.Add(-20*aDay), 60, now, now.Add(time.Hour))
	_, err = (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
	if err == nil || !strings.Contains(err.Error(), "began inside the 28d before") || !strings.Contains(err.Error(), "longer than the 28d a guard may watch") {
		t.Fatalf("one incident in twenty days: %v", err)
	}
}

// guarded is a case whose change landed with a guard's plan, as a proposal records it: one incident in 20 hours of
// exposure, so its watch needs 60 such hours with no recurrence, expected in a week. The measure step runs on a clock of
// the test's own, a year back; the loop is not live.
func guarded(t *testing.T) *measured {
	t.Helper()
	ctx := context.Background()
	st, db := testStoreAt(t)
	dir := t.TempDir()
	m := &measured{st: st, db: db, r: &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets,
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.deployed = time.Now().Add(-365 * aDay).Truncate(time.Hour)
	m.planAt = m.deployed.Add(-time.Hour)
	m.clock = m.deployed.Add(10 * time.Minute)
	var failures []store.Event
	for i := 0; i < 10; i++ {
		failures = append(failures, toolResult(fmt.Sprint("inc-", i), m.planAt.Add(-5*aDay+time.Duration(i)*time.Hour), fmt.Sprint("inc-", i), true))
	}
	if _, err := st.Put(ctx, failures); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, toolSignal.Source, time.Now().Add(365*aDay)); err != nil {
		t.Fatal(err)
	}
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	f := store.Finding{Family: "failures", Signature: "failures/v2:zz-guarded", Title: "the tool fails"}
	for _, e := range failures {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, st, c, "repo")
	mech, next := "m", "n"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"signal": toolSignal, "window": 7 * aDay, "units": SessionHours, "at": m.planAt, "sources": []string{toolSignal.Source},
		"guard": guardJSON{Instead: "the signal's source began inside the 28d before: the problem is too new to measure with that window", Since: m.planAt.Add(-8 * aDay),
			Gap: 7 * aDay, Incidents: 1, Exposure: 20, Quiet: 10, Need: 60, Watch: 7 * aDay}})
	if err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: m.repo, PR: "https://github.com/agent-sh/fake/pull/7", State: Landed,
		Landed: []string{head}, LandedAt: m.deployed, Plan: &plan}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if m.c, err = st.DeployCase(ctx, c.ID, m.r.Ship.pkgDir(c.ID), m.deployed, "landed", "test"); err != nil {
		t.Fatal(err)
	}
	return m
}

// uses puts n hours of the tool's successful use from a time, one an hour, each in a session of its own.
func uses(t *testing.T, st *store.Store, prefix string, from time.Time, n int) {
	t.Helper()
	var evs []store.Event
	for i := 0; i < n; i++ {
		evs = append(evs, toolResult(fmt.Sprint(prefix, i), from.Add(time.Duration(i)*time.Hour), fmt.Sprint(prefix, i), false))
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
}

func lastLog(t *testing.T, st *store.Store, id string) store.CaseEntry {
	t.Helper()
	log, err := st.CaseLog(context.Background(), id)
	if err != nil || len(log) == 0 {
		t.Fatalf("the case log: %+v %v", log, err)
	}
	return log[len(log)-1]
}

func jsonHas(t *testing.T, v any, text string) bool {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), text)
}

// A guarded watch starts when the change is in effect, with a log line that says it is guarded. It closes the case as
// improved only once its exposure reached the 60 hours it needs with no recurrence: at the end of its week it has 40,
// so it watches a week more; at 65 it closes, and its result, the case's close and its shipped note say it was guarded,
// not a measured drop, with the probability a change that did nothing would have met a recurrence (1 - exp(-65/20)).
func TestAGuardedWatchClosesAsImprovedOnceItsExposureIsReached(t *testing.T) {
	m := guarded(t)
	got := m.measure(t, Options{})
	if got.Status != "measuring" || !jsonHas(t, got, `"guarded":true`) || !got.Due.Equal(m.clock.Add(7*aDay)) {
		t.Fatalf("the watch starts: %+v", got)
	}
	if l := lastLog(t, m.st, m.c.ID); l.Action != "guarding" || !strings.Contains(l.Detail, "guarded, not measured") || strings.Contains(l.Detail, "baseline") {
		t.Fatalf("its log line: %+v", l)
	}
	effect := m.clock
	uses(t, m.st, "w1-", effect.Add(time.Hour), 40)
	m.clock = effect.Add(3 * aDay)
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("inside its week: %+v", got)
	}
	m.clock = effect.Add(7*aDay + time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "40 of the 60") || !got.Due.Equal(effect.Add(14*aDay)) {
		t.Fatalf("a week with 40 of its 60 hours: %+v", got)
	}
	uses(t, m.st, "w2-", effect.Add(7*aDay+time.Hour), 25)
	m.clock = effect.Add(14*aDay + time.Hour)
	got = m.measure(t, Options{})
	if got.Status != "improved" || got.Result == nil || !strings.Contains(got.Result.Effect, "guarded, not a measured drop") ||
		!strings.Contains(got.Result.Effect, "no recurrence in 65 active hours") || !strings.Contains(got.Result.Effect, "p=0.039") ||
		!jsonHas(t, got.Result, `"guarded":true`) {
		t.Fatalf("the watch reached its exposure: %+v %+v", got, got.Result)
	}
	c := m.theCase(t)
	if c.Status != store.CaseClosed || c.Reason != store.Improved || c.Measurement.After != 0 || !strings.HasPrefix(c.Measurement.Effect, "guarded, not a measured drop") {
		t.Fatalf("the case: %+v", c)
	}
	ns := m.notes(t)
	if len(ns) != 1 || ns[0].Kind != store.NoteShipped || !strings.HasPrefix(ns[0].Title, "momentum guarded: ") || !strings.Contains(ns[0].Detail, "guarded, not measured") ||
		strings.Contains(ns[0].Detail, "measurably") || !strings.Contains(ns[0].Detail, "96.1%") {
		t.Fatalf("its note: %+v", ns)
	}
	if p, _, _ := m.r.Ship.Load(c.ID); p.State != Kept || p.Result == nil {
		t.Fatalf("its package: %+v", p)
	}
}

// One recurrence fails a guard's watch at once, before its window ends: the change goes as one with no measured effect
// does (held here, the loop not being live, with its decided note). A case a guard closed comes back, regressed, at
// its problem's first event after the close: the guard said it would not come back, so one event answers it.
func TestARecurrenceFailsAGuardAndBringsItsCaseBack(t *testing.T) {
	ctx := context.Background()
	m := guarded(t)
	m.measure(t, Options{})
	effect := m.clock
	uses(t, m.st, "w-", effect.Add(time.Hour), 5)
	if _, err := m.st.Put(ctx, []store.Event{toolResult("again", effect.Add(2*time.Hour), "again", true)}); err != nil {
		t.Fatal(err)
	}
	m.clock = effect.Add(3 * time.Hour)
	got := m.measure(t, Options{})
	if got.Status != "held" || !strings.Contains(got.Why, "its guard failed") || !strings.Contains(got.Why, "not live") || got.Result == nil ||
		!strings.Contains(got.Result.Effect, "came back") || got.Result.Improved {
		t.Fatalf("a recurrence inside the watch: %+v %+v", got, got.Result)
	}
	if ns := m.notes(t); len(ns) != 1 || ns[0].Kind != store.NoteDecided || !strings.Contains(ns[0].Detail, "momentum ship rollback "+m.c.ID) {
		t.Fatalf("the held rollback's note: %+v", ns)
	}
	if m.theCase(t).Status != store.CaseMeasuring {
		t.Fatal("a held rollback moved the case")
	}

	// a case its guard closed comes back at the first recurrence after the close
	m = guarded(t)
	m.measure(t, Options{})
	effect = m.clock
	uses(t, m.st, "w-", effect.Add(time.Hour), 70)
	m.clock = effect.Add(7*aDay + time.Hour)
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("the guard's close: %+v", got)
	}
	closed := m.theCase(t).Closed
	m.clock = closed.Add(2 * time.Hour)
	if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("a quiet close: %+v %v", ms, err)
	}
	if _, err := m.st.Put(ctx, []store.Event{toolResult("back", closed.Add(time.Hour), "back", true)}); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{Dry: true}); got.Status != "back" || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("a dry watch: %+v", got)
	}
	if got := m.measure(t, Options{}); got.Status != "back" || !strings.Contains(got.Why, "came back") {
		t.Fatalf("a recurrence after the close: %+v", got)
	}
	c := m.theCase(t)
	if c.Status != store.CaseOpen || c.Episode != 2 {
		t.Fatalf("the case back: %+v", c)
	}
	if l := lastLog(t, m.st, c.ID); l.Action != "regressed" {
		t.Fatalf("its log: %+v", l)
	}
	found := false
	for _, n := range m.notes(t) {
		found = found || n.Kind == store.NoteDecided && strings.Contains(n.Title, "is back")
	}
	if !found {
		t.Fatalf("no note that it is back: %+v", m.notes(t))
	}
}

// End to end: a preventive fix for the problem that stopped is checked, proposed with its guard (the request's body says
// so), reviewed and landed by a live loop; its watch starts once the checkout holds it, and one recurrence rolls it back
// and takes its case back to testing, before its window ends.
func TestAGuardedChangeIsProposedLandedAndRolledBackOnARecurrence(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	now := time.Now().Truncate(time.Hour)
	c := stoppedProblem(t, st, now.Add(-8*aDay), 60, now, now.Add(365*aDay))
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	if p, err = proposePatch(t, s, ctx, c.ID, Proposal{Title: "Keep the tool's state fresh", Body: "A preventive fix.", Rollback: "revert", Signal: toolSignal, Window: 7 * aDay}); err != nil {
		t.Fatalf("the preventive fix's proposal: %v", err)
	}
	if g := guardOf(t, p.Plan); g == nil || g.Watch != 28*aDay {
		t.Fatalf("its plan: %+v", p.Plan)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "Guarded, not measured") || strings.Contains(string(calls), "Fisher") {
		t.Fatalf("the request's body does not say it is guarded: %s", calls)
	}
	if _, err := s.Reviewed(ctx, c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "Checked every changed file.", Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil {
		t.Fatal(err)
	}
	r := measurementRunner(t, st)
	r.Ship, r.Dir = s, s.Dir
	clock := time.Now().Truncate(time.Millisecond)
	r.Now = func() time.Time { return clock }
	s.Now = r.Now
	if done := r.reviews(ctx, c.ID); len(done) != 1 || done[0].Step != "land" || done[0].Skipped != "" {
		t.Fatalf("landing: %+v", done)
	}
	run(t, f.main, "git", "fetch", "origin", "main")
	run(t, f.main, "git", "merge", "--ff-only", "origin/main")
	m := &measured{r: r, st: st, c: c}
	if got := m.measure(t, Options{}); got.Status != "measuring" || !jsonHas(t, got, `"guarded":true`) {
		t.Fatalf("the watch starts: %+v", got)
	}
	if _, err := st.Put(ctx, []store.Event{toolResult("again", clock.Add(time.Hour), "again", true)}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "rolled back" || !strings.Contains(got.Why, "its guard failed") {
		t.Fatalf("a recurrence: %+v", got)
	}
	if m.theCase(t).Status != store.CaseTesting {
		t.Fatal("the rollback did not bring the case back")
	}
	if body := run(t, f.dir, "git", "--git-dir", f.origin, "show", "main:fake.go"); !strings.Contains(body, "return 41") {
		t.Fatal("origin did not roll back")
	}
}
