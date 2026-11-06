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
// build.

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

// testingCaseOf is a case in testing of the tool's failures, its evidence the events given.
func testingCaseOf(t *testing.T, st *store.Store, signature string, evidence []store.Event) store.Case {
	t.Helper()
	ctx := context.Background()
	f := store.Finding{Family: "failures", Signature: signature, Title: "the tool fails"}
	for _, e := range evidence {
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

func put(t *testing.T, st *store.Store, evs []store.Event) {
	t.Helper()
	if len(evs) == 0 {
		return
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
}

// stoppedProblem is the store of a tool whose source began with an incident: from a clock hour, for hours hours, five
// failed results an hour in a session of the hour's own; then, until quietUntil, one successful result an hour, each in a
// session of its own. The source is read to read. It returns a case in testing whose evidence is the first fifty failures.
func stoppedProblem(t *testing.T, st *store.Store, from time.Time, hours int, quietUntil, read time.Time) store.Case {
	t.Helper()
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
	put(t, st, evs)
	if err := st.MarkRead(context.Background(), toolSignal.Source, read); err != nil {
		t.Fatal(err)
	}
	return testingCaseOf(t, st, "failures/v2:zz-tool", failures[:50])
}

// recurring is a tool's store that began 60 days before at: one successful result an hour from 28 days before at
// until useUntil, and bursts of three failed results in three hours, each in a session of its own, at the times given
// (before at). It returns a case in testing whose evidence is every failure, and the failures.
func recurring(t *testing.T, st *store.Store, at, useUntil time.Time, bursts ...time.Time) (store.Case, []store.Event) {
	t.Helper()
	evs := []store.Event{toolResult("began", at.Add(-60*aDay), "began", false)}
	for i, u := 0, at.Add(-28*aDay); u.Before(useUntil); i, u = i+1, u.Add(time.Hour) {
		evs = append(evs, toolResult(fmt.Sprint("use-", i), u, fmt.Sprint("use-", i), false))
	}
	var failures []store.Event
	for b, from := range bursts {
		for j := 0; j < 3; j++ {
			failures = append(failures, toolResult(fmt.Sprintf("b%d-%d", b, j), from.Add(time.Duration(j)*time.Hour+30*time.Minute), fmt.Sprintf("b%d-%d", b, j), true))
		}
	}
	put(t, st, append(evs, failures...))
	if err := st.MarkRead(context.Background(), toolSignal.Source, time.Now().Add(365*aDay)); err != nil {
		t.Fatal(err)
	}
	return testingCaseOf(t, st, "failures/v2:zz-recurring", failures), failures
}

// fourBursts are four bursts a week and a half apart, the last four and a half days before at.
func fourBursts(at time.Time) []time.Time {
	return []time.Time{at.Add(-27 * aDay), at.Add(-19*aDay - 12*time.Hour), at.Add(-12 * aDay), at.Add(-4*aDay - 12*time.Hour)}
}

// guardJSON is a guard as a plan's JSON records it.
type guardJSON struct {
	Instead   string        `json:"instead"`
	Since     time.Time     `json:"since"`
	Gap       time.Duration `json:"gap"`
	Units     string        `json:"units"`
	Tool      string        `json:"tool"`
	Cwd       string        `json:"cwd"`
	Incidents int           `json:"incidents"`
	Exposure  int           `json:"exposure"`
	Before    int           `json:"before"`
	Quiet     int           `json:"quiet"`
	Need      int           `json:"need"`
	Watch     time.Duration `json:"watch"`
}

// guardOf is a plan's guard as its JSON records it, or nil for a measured plan, and the plan's own units there.
func guardOf(t *testing.T, plan any) (*guardJSON, string) {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Units string     `json:"units"`
		Guard *guardJSON `json:"guard"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got.Guard, got.Units
}

// A tool fails in four short bursts a week and a half apart over the 28 days before the proposal, the last four and a
// half days before it, and works in between: too rare for a measured plan (three units in the gate), so a guard stands
// in. Its four incidents came in 684 hours of the tool's use, so its watch needs 684 * (20^(1/4.5) - 1) = 647 of them
// with no recurrence (the Jeffreys predictive), which the history's use reaches in 26.5 days: a watch of four weeks,
// inside the 30 days the store is sure to read again. The problem that stopped, one burst its source began with and
// nothing since, gets none: once that burst is dropped, nothing tells how often it starts. A case the measured path
// can measure still gets its measured plan.
func TestAGuardStandsInOnlyWhereAMeasuredPlanIsRefused(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour)
	t.Run("a rare problem", func(t *testing.T) {
		st := testStore(t)
		c, _ := recurring(t, st, now, now, fourBursts(now)...)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
		if err != nil {
			t.Fatalf("a guard for a rare problem: %v", err)
		}
		g, units := guardOf(t, plan)
		if g == nil {
			t.Fatalf("a measured plan for a problem too rare to measure: %+v", plan)
		}
		if g.Incidents != 4 || g.Exposure != 684 || g.Quiet != 660 || g.Need != 647 || g.Watch != 28*aDay || g.Gap != 7*aDay || g.Tool != "Bash" ||
			!g.Since.Equal(now.Add(-28*aDay)) || !strings.Contains(g.Instead, "fewer than 5") {
			t.Fatalf("the guard: %+v", *g)
		}
		// a build from before guards reads a plan with no units as one to measure by hand
		if units != "" || g.Units != SessionHours || plan.Units != SessionHours || plan.Before != 0 || plan.BeforeBase != 0 {
			t.Fatalf("a guard's plan holds its units in its guard: %q %+v", units, plan)
		}
	})
	t.Run("a problem that stopped", func(t *testing.T) {
		st := testStore(t)
		c := stoppedProblem(t, st, now.Add(-8*aDay), 60, now, now.Add(time.Hour))
		_, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
		if err == nil || !strings.Contains(err.Error(), "began inside the 28d before") || !strings.Contains(err.Error(), "has not come back since the incident its source began with") {
			t.Fatalf("one burst its source began with, then quiet: %v", err)
		}
	})
	t.Run("a measurable case", func(t *testing.T) { // the measured path as it was: this passes before guards too
		st := testStore(t)
		c := testingCase(t, st)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, goodSignal, 7*aDay, "", time.Now())
		if g, _ := guardOf(t, plan); err != nil || g != nil || plan.Before != 6 || plan.BeforeBase != 6 || plan.Gate != 6 {
			t.Fatalf("a measurable case's plan: %+v %v", plan, err)
		}
	})
}

// A guard is refused where it cannot tell: a history whose exposure all lies inside its incident; one hour of use
// after the burst its source began with (the burst is dropped, and no incident is left); three incidents in 28 days,
// whose watch would take 38 days, past what the store is sure to read again; a signal that counts none of the
// case's evidence in the gate, the problem's form there having changed; and a signal with no tool whose events
// share no tool or directory. A signal with no tool whose events all share one counts its exposure in that tool's hours.
func TestAGuardIsRefusedWhereItCannotTell(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour)
	refusedWith := func(t *testing.T, st *store.Store, c store.Case, sig Signal, words ...string) {
		t.Helper()
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, sig, 7*aDay, "", now)
		if err == nil {
			g, _ := guardOf(t, plan)
			t.Fatalf("admitted: %+v %+v", plan, g)
		}
		for _, w := range words {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("refused without %q: %v", w, err)
			}
		}
	}
	t.Run("all inside its incident", func(t *testing.T) {
		st := testStore(t)
		from := now.Add(-61 * time.Hour)
		refusedWith(t, st, stoppedProblem(t, st, from, 60, from.Add(60*time.Hour), now.Add(time.Hour)), toolSignal, "began inside the 28d before", "too thin")
	})
	t.Run("one quiet hour", func(t *testing.T) {
		st := testStore(t)
		from := now.Add(-62 * time.Hour)
		refusedWith(t, st, stoppedProblem(t, st, from, 60, from.Add(61*time.Hour), now.Add(time.Hour)), toolSignal, "has not come back")
	})
	t.Run("three incidents", func(t *testing.T) {
		st := testStore(t)
		c, _ := recurring(t, st, now, now, now.Add(-27*aDay), now.Add(-18*aDay), now.Add(-9*aDay))
		refusedWith(t, st, c, toolSignal, "too rare to prove by recurrence within the store's history")
	})
	t.Run("the gate's evidence is not counted", func(t *testing.T) {
		st := testStore(t)
		c, failures := recurring(t, st, now, now, fourBursts(now)[:3]...)
		// the problem's new form in the gate, which the signal does not count, is the case's evidence there
		var changed []store.Event
		for j := 0; j < 3; j++ {
			e := toolResult(fmt.Sprint("new-", j), now.Add(-2*aDay+time.Duration(j)*time.Hour), fmt.Sprint("new-", j), true)
			e.Summary = "Exit code 1 zz-tool-crashed differently"
			changed = append(changed, e)
		}
		put(t, st, changed)
		c = testingCaseOf(t, st, "failures/v2:zz-changed", append(failures, changed...))
		refusedWith(t, st, c, toolSignal, "fewer than 5", "the signal counts 0 of the case's 3 events in the window: it does not measure the case's problem")
	})
	noTool := Signal{Source: toolSignal.Source, Kinds: toolSignal.Kinds, Match: toolSignal.Match}
	t.Run("no shared unit", func(t *testing.T) {
		st := testStore(t)
		_, failures := recurring(t, st, now, now, fourBursts(now)...)
		for i := range failures { // half the failures are of another tool, in another directory
			failures[i].SourceKey += "-moved"
			if i%2 == 0 {
				failures[i].Tool, failures[i].Cwd = "Read", "/work/a"
			} else {
				failures[i].Cwd = "/work/b"
			}
		}
		put(t, st, failures)
		c := testingCaseOf(t, st, "failures/v2:zz-moved", failures)
		refusedWith(t, st, c, noTool, "share no tool or directory")
	})
	t.Run("a shared tool", func(t *testing.T) {
		st := testStore(t)
		c, _ := recurring(t, st, now, now, fourBursts(now)...)
		var other []store.Event // another tool, used every hour in sessions of its own: not the problem's unit
		for i, u := 0, now.Add(-28*aDay); u.Before(now); i, u = i+1, u.Add(time.Hour) {
			e := toolResult(fmt.Sprint("read-", i), u.Add(10*time.Minute), fmt.Sprint("read-", i), false)
			e.Tool = "Read"
			other = append(other, e)
		}
		put(t, st, other)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, noTool, 7*aDay, "", now)
		g, _ := guardOf(t, plan)
		if err != nil || g == nil || g.Tool != "Bash" || g.Exposure != 684 {
			t.Fatalf("a signal with no tool whose events share one: %+v %v", g, err)
		}
	})
}

// A measured plan whose problem went quiet after it is counted again when a hand landing comes more than a window
// later: by then the gate holds nothing, and the three incidents left in its span would ask a watch past what the store
// is sure to read again. The recount does not turn it into a guard.
func TestAHandLandingsRecountDoesNotGuardAProblemThatWentQuiet(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	plannedAt := time.Now().Truncate(time.Hour).Add(-8 * aDay)
	landed := plannedAt.Add(8 * aDay)
	c, failures := recurring(t, st, plannedAt, landed, plannedAt.Add(-27*aDay), plannedAt.Add(-19*aDay-12*time.Hour), plannedAt.Add(-12*aDay))
	var active []store.Event // the problem every hour for the three days before the plan
	for i, u := 0, plannedAt.Add(-3*aDay); u.Before(plannedAt); i, u = i+1, u.Add(time.Hour) {
		active = append(active, toolResult(fmt.Sprint("active-", i), u.Add(20*time.Minute), fmt.Sprint("active-", i), true))
	}
	put(t, st, active)
	c = testingCaseOf(t, st, "failures/v2:zz-quiet", append(failures, active...))
	s := &Ship{Store: st}
	plan, err := s.validPlan(ctx, c, toolSignal, 7*aDay, "", plannedAt)
	if g, _ := guardOf(t, plan); err != nil || g != nil || plan.Gate < MinSignal {
		t.Fatalf("the measured plan: %+v %v", plan, err)
	}
	plan, err = s.validPlan(ctx, c, toolSignal, 7*aDay, "", landed)
	if g, _ := guardOf(t, plan); err == nil || g != nil || !strings.Contains(err.Error(), "too rare to prove by recurrence within the store's history") {
		t.Fatalf("counted again at the landing: %+v %+v %v", plan, g, err)
	}
}

// guarded is a case whose change landed with the guard its proposal was counted with: the four bursts before its plan,
// the tool used every hour until effectAfter past the plan, when its change takes effect. The measure step runs on a
// clock of the test's own, a year back; the loop is not live.
func guarded(t *testing.T, effectAfter time.Duration) *measured {
	t.Helper()
	ctx := context.Background()
	st, db := testStoreAt(t)
	dir := t.TempDir()
	m := &measured{st: st, db: db, r: &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets,
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.planAt = time.Now().Add(-365 * aDay).Truncate(time.Hour)
	m.deployed = m.planAt.Add(effectAfter)
	m.clock = m.deployed.Add(10 * time.Minute)
	c, _ := recurring(t, st, m.planAt, m.deployed.Add(-time.Hour), fourBursts(m.planAt)...)
	plan, err := m.r.Ship.validPlan(ctx, c, toolSignal, 7*aDay, "", m.planAt)
	if g, _ := guardOf(t, plan); err != nil || g == nil {
		t.Fatalf("the proposal's plan: %+v %v", plan, err)
	}
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
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

// uses puts n units of the tool's successful use from a time, perHour sessions of their own an hour.
func uses(t *testing.T, st *store.Store, prefix string, from time.Time, n, perHour int) {
	t.Helper()
	var evs []store.Event
	for i := 0; i < n; i++ {
		evs = append(evs, toolResult(fmt.Sprint(prefix, i), from.Add(time.Duration(i/perHour)*time.Hour+time.Duration(i%perHour)*time.Minute), fmt.Sprint(prefix, i), false))
	}
	put(t, st, evs)
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

// A guarded watch starts when the change is in effect, with a log line that says it is guarded and what it assumes.
// It closes the case as improved only once its exposure reached the 647 hours it needs with no recurrence: at the end
// of its four weeks it has 300, so it watches on to the 30 days the store is sure to read again; at 700 it closes, and
// its result, the case's close and its shipped note say it was guarded, not a measured drop, with the predictive
// probability that a change that did nothing would have met a recurrence, 1 - (684/(684+700))^4.5 = 95.81%.
func TestAGuardedWatchClosesAsImprovedOnceItsExposureIsReached(t *testing.T) {
	m := guarded(t, time.Hour)
	got := m.measure(t, Options{})
	if got.Status != "measuring" || !jsonHas(t, got, `"guarded":true`) || !got.Due.Equal(m.clock.Add(28*aDay)) {
		t.Fatalf("the watch starts: %+v", got)
	}
	if l := lastLog(t, m.st, m.c.ID); l.Action != "guarding" || !strings.Contains(l.Detail, "guarded, not measured") ||
		!strings.Contains(l.Detail, "assumes the problem was still happening") || strings.Contains(l.Detail, "baseline") {
		t.Fatalf("its log line: %+v", l)
	}
	if c := m.theCase(t); c.Measurement.Before != 4 || !jsonHas(t, c.Measurement, `"Exposure":684`) {
		t.Fatalf("the case keeps its guard's incidents and exposure: %+v", c.Measurement)
	}
	effect := m.clock
	uses(t, m.st, "w1-", effect.Add(time.Hour), 300, 2)
	m.clock = effect.Add(3 * aDay)
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("inside its watch: %+v", got)
	}
	m.clock = effect.Add(28*aDay + time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "300 of the 647 session hours") || !got.Due.Equal(effect.Add(30*aDay)) {
		t.Fatalf("four weeks with 300 of its 647 hours: %+v", got)
	}
	uses(t, m.st, "w2-", effect.Add(28*aDay+time.Hour), 400, 10)
	m.clock = effect.Add(30*aDay + time.Hour)
	got = m.measure(t, Options{})
	if got.Status != "improved" || got.Result == nil || !strings.Contains(got.Result.Effect, "guarded, not a measured drop") ||
		!strings.Contains(got.Result.Effect, "no recurrence in 700 session hours") || !strings.Contains(got.Result.Effect, "predictive probability 95.81%") ||
		got.Result.P >= 0.05 || !jsonHas(t, got.Result, `"guarded":true`) {
		t.Fatalf("the watch reached its exposure: %+v %+v", got, got.Result)
	}
	c := m.theCase(t)
	if c.Status != store.CaseClosed || c.Reason != store.Improved || c.Measurement.After != 0 || !strings.HasPrefix(c.Measurement.Effect, "guarded, not a measured drop") {
		t.Fatalf("the case: %+v", c)
	}
	ns := m.notes(t)
	if len(ns) != 1 || ns[0].Kind != store.NoteShipped || !strings.HasPrefix(ns[0].Title, "momentum guarded: ") || !strings.Contains(ns[0].Detail, "guarded, not measured") ||
		strings.Contains(ns[0].Detail, "measurably") || !strings.Contains(ns[0].Detail, "predictive probability 95.8%") || !strings.Contains(ns[0].Detail, "still been happening") {
		t.Fatalf("its note: %+v", ns)
	}
	if p, _, _ := m.r.Ship.Load(c.ID); p.State != Kept || p.Result == nil {
		t.Fatalf("its package: %+v", p)
	}
	// each pass after the close reads only what the source read since the last: its place moves with the source
	m.clock = c.Closed.Add(time.Hour)
	if ms, err := m.r.Measure(context.Background(), Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("a quiet close: %+v %v", ms, err)
	}
	if p, _, _ := m.r.Ship.Load(c.ID); !jsonHas(t, p, `"guard_read":"`+m.clock.Format(time.RFC3339Nano)) {
		t.Fatalf("its place in the source after a quiet pass: %+v", p)
	}
}

// One recurrence fails a guard's watch at once, before its window ends: the change goes as one with no measured effect
// does (held here, the loop not being live, with its decided note). A case a guard closed comes back, regressed, at
// its problem's first event after the change, one read late too: dated after the watch's end, before the close, and
// stored only after it.
func TestARecurrenceFailsAGuardAndBringsItsCaseBack(t *testing.T) {
	m := guarded(t, time.Hour)
	m.measure(t, Options{})
	effect := m.clock
	uses(t, m.st, "w-", effect.Add(time.Hour), 5, 1)
	put(t, m.st, []store.Event{toolResult("again", effect.Add(2*time.Hour), "again", true)})
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

	// a case its guard closed comes back at a recurrence the store reads only after the close
	m = guarded(t, time.Hour)
	m.measure(t, Options{})
	effect = m.clock
	uses(t, m.st, "w-", effect.Add(time.Hour), 700, 2)
	m.clock = effect.Add(28*aDay + time.Hour)
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("the guard's close: %+v", got)
	}
	closed := m.theCase(t).Closed
	// a failure dated after the watch's end and before the close, which the store reads only after the close
	put(t, m.st, []store.Event{toolResult("late", m.clock.Add(30*time.Minute), "late", true)})
	m.clock = closed.Add(2 * time.Hour)
	if got := m.measure(t, Options{Dry: true}); got.Status != "back" || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("a dry watch: %+v", got)
	}
	if got := m.measure(t, Options{}); got.Status != "back" || !strings.Contains(got.Why, "came back") {
		t.Fatalf("a recurrence read after the close: %+v", got)
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

// A guard's history is counted again when its change takes effect. A change that takes effect 25 days after its plan,
// the tool used every hour since and the problem quiet, meets a quiet stretch the history before it makes unlikely had
// the problem gone on: it had likely stopped before the change, so a watch could not tell the change from that. The
// watch is held for a person, and the case stays deployed.
func TestAGuardIsCountedAgainWhenItsChangeTakesEffect(t *testing.T) {
	m := guarded(t, 25*aDay)
	got := m.measure(t, Options{})
	if got.Status != "held" || !strings.Contains(got.Why, "had likely stopped before the change") || !strings.Contains(got.Why, "judged by hand") {
		t.Fatalf("a change in effect long after its problem went quiet: %+v", got)
	}
	if c := m.theCase(t); c.Status != store.CaseDeployed {
		t.Fatalf("the case: %+v", c)
	}
}

// End to end: a preventive fix for the rare problem is checked, proposed with its guard (the request's body says so),
// reviewed and landed by a live loop; its watch starts once the checkout holds it, and one recurrence rolls it back
// and takes its case back to testing, before its window ends.
func TestAGuardedChangeIsProposedLandedAndRolledBackOnARecurrence(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	now := time.Now().Truncate(time.Hour)
	c, _ := recurring(t, st, now, now, fourBursts(now)...)
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
	if g, _ := guardOf(t, p.Plan); g == nil || g.Watch != 28*aDay {
		t.Fatalf("its plan: %+v", p.Plan)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "Guarded, not measured") || !strings.Contains(string(calls), "predictive probability") || strings.Contains(string(calls), "Fisher") {
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
	put(t, st, []store.Event{toolResult("again", clock.Add(time.Hour), "again", true)})
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
