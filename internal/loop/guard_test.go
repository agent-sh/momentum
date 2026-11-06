package loop

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/evidenceview"
	"github.com/agent-sh/momentum/internal/store"
)

// The guard tests use only what the measured path already had (a plan's and a package's JSON, the measure step's
// status and words, the case and its notes), so that against the code before them they run and fail rather than not
// build.

// guardedReason is the close reason of a guard's watch that ended with no recurrence.
const guardedReason = store.CloseReason("guarded")

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
// session of its own. The source is read to read. It returns a case in testing whose evidence is the newest fifty
// failures, as a detector keeps a finding's newest events.
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
	return testingCaseOf(t, st, "failures/v2:zz-tool", failures[len(failures)-50:])
}

// recurring is a tool's store that began 60 days before at: one successful result an hour from 28 days before at
// until useUntil (in source use), and bursts of three failed results in three hours, each in a session of its own, at
// the times given (before at). Its sources' last complete scans started at read, as a real scan records them. It
// returns a case in testing whose evidence is every failure, and the failures.
func recurring(t *testing.T, st *store.Store, use string, at, useUntil, read time.Time, bursts ...time.Time) (store.Case, []store.Event) {
	t.Helper()
	evs := []store.Event{toolResult("began", at.Add(-60*aDay), "began", false)}
	for i, u := 0, at.Add(-28*aDay); u.Before(useUntil); i, u = i+1, u.Add(time.Hour) {
		evs = append(evs, toolResult(fmt.Sprint("use-", i), u, fmt.Sprint("use-", i), false))
	}
	for i := range evs {
		evs[i].Source = use
	}
	var failures []store.Event
	for b, from := range bursts {
		for j := 0; j < 3; j++ {
			failures = append(failures, toolResult(fmt.Sprintf("b%d-%d", b, j), from.Add(time.Duration(j)*time.Hour+30*time.Minute), fmt.Sprintf("b%d-%d", b, j), true))
		}
	}
	put(t, st, append(evs, append(failures, toolResult("tool-began", at.Add(-60*aDay), "tool-began", false))...))
	for _, source := range []string{toolSignal.Source, use} {
		if err := st.MarkRead(context.Background(), source, read); err != nil {
			t.Fatal(err)
		}
	}
	return testingCaseOf(t, st, "failures/v2:zz-recurring-"+use, failures), failures
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
	Incidents int           `json:"incidents"`
	Exposure  int           `json:"exposure"`
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

// A guard stands in wherever a measured plan is refused as too new or too rare and the case's evidence holds a past
// incident of the signal's. The tool that failed in one burst its source began with, then worked for five and a half
// days: the burst is dropped (the source began with it), no incident is left, and 132 hours of quiet use are its
// exposure. Four bursts a week and a half apart in 684 hours of use: four incidents. A two-week window keeps the
// one-week gap between incidents. A case the measured path can measure still gets its measured plan.
func TestAGuardStandsInWhereAMeasuredPlanIsRefused(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour)
	t.Run("a problem that stopped", func(t *testing.T) {
		st := testStore(t)
		from := now.Add(-8 * aDay)
		c := stoppedProblem(t, st, from, 60, now, now)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
		g, _ := guardOf(t, plan)
		if err != nil || g == nil || g.Incidents != 0 || g.Exposure != 132 || g.Gap != 7*aDay || !g.Since.Equal(from.Add(60*time.Hour)) ||
			!strings.Contains(g.Instead, "began inside the 28d before") {
			t.Fatalf("a preventive fix for a problem that stopped: %+v %+v %v", plan, g, err)
		}
	})
	t.Run("a rare problem", func(t *testing.T) {
		st := testStore(t)
		c, _ := recurring(t, st, toolSignal.Source, now, now, now, fourBursts(now)...)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
		g, units := guardOf(t, plan)
		if err != nil || g == nil || g.Incidents != 4 || g.Exposure != 684 || g.Gap != 7*aDay || g.Tool != "Bash" || units != "" || g.Units != SessionHours ||
			!strings.Contains(g.Instead, "fewer than 5") {
			t.Fatalf("a rare problem's guard: %+v %v", g, err)
		}
	})
	t.Run("a two-week window", func(t *testing.T) { // four bursts nine to twelve days apart, none in the gate's two weeks
		st := testStore(t)
		c, _ := recurring(t, st, toolSignal.Source, now, now, now, now.Add(-50*aDay), now.Add(-41*aDay), now.Add(-32*aDay), now.Add(-20*aDay))
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, toolSignal, 14*aDay, "", now)
		g, _ := guardOf(t, plan)
		if err != nil || g == nil || g.Gap != 7*aDay || g.Incidents != 4 || plan.Window != 14*aDay {
			t.Fatalf("a two-week window's guard: %+v %v", g, err)
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

// A guard is refused only where no watch could be kept: a history with no exposure; a signal that counts none of the
// case's evidence in the gate, the problem's form there having changed; a signal with no tool whose events share no
// tool or directory; and, on the too-new path too, a gate that holds an active problem and none of the case's evidence.
// A signal with no tool whose events all share one counts its exposure in that tool's hours.
func TestAGuardIsRefusedOnlyWhereNoWatchCouldBeKept(t *testing.T) {
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
	t.Run("no exposure", func(t *testing.T) {
		st := testStore(t)
		from := now.Add(-61 * time.Hour)
		refusedWith(t, st, stoppedProblem(t, st, from, 60, from.Add(60*time.Hour), now), toolSignal, "began inside the 28d before",
			"holds no exposure")
	})
	t.Run("an active gate on the too-new path", func(t *testing.T) {
		st := testStore(t)
		c := stoppedProblem(t, st, now.Add(-20*aDay), 10, now, now) // its evidence is the burst 20 days back
		var active []store.Event                                    // the problem in the gate, none of it the case's
		for i := 0; i < 6; i++ {
			active = append(active, toolResult(fmt.Sprint("active-", i), now.Add(-time.Duration(i+1)*time.Hour+10*time.Minute), fmt.Sprint("active-", i), true))
		}
		put(t, st, active)
		refusedWith(t, st, c, toolSignal, "began inside the 28d before", "the case has no evidence in the window, which holds its problem in 6 session hours")
	})
	t.Run("the gate's evidence is not counted", func(t *testing.T) {
		st := testStore(t)
		_, failures := recurring(t, st, toolSignal.Source, now, now, now, fourBursts(now)[:3]...)
		var changed []store.Event // the problem's new form in the gate, which the signal does not count, is the case's evidence there
		for j := 0; j < 3; j++ {
			e := toolResult(fmt.Sprint("new-", j), now.Add(-2*aDay+time.Duration(j)*time.Hour), fmt.Sprint("new-", j), true)
			e.Summary = "Exit code 1 zz-tool-crashed differently"
			changed = append(changed, e)
		}
		put(t, st, changed)
		c := testingCaseOf(t, st, "failures/v2:zz-changed", append(failures, changed...))
		refusedWith(t, st, c, toolSignal, "fewer than 5", "the signal counts 0 of the case's 3 events in the window: it does not measure the case's problem")
	})
	noTool := Signal{Source: toolSignal.Source, Kinds: toolSignal.Kinds, Match: toolSignal.Match}
	t.Run("no shared unit", func(t *testing.T) {
		st := testStore(t)
		_, failures := recurring(t, st, toolSignal.Source, now, now, now, fourBursts(now)...)
		for i := range failures { // half the failures are of another tool, in another directory
			failures[i].SourceKey += "-moved"
			if i%2 == 0 {
				failures[i].Tool, failures[i].Cwd = "Read", "/work/a"
			} else {
				failures[i].Cwd = "/work/b"
			}
		}
		put(t, st, failures)
		refusedWith(t, st, testingCaseOf(t, st, "failures/v2:zz-moved", failures), noTool, "share no tool or directory")
	})
	t.Run("a shared tool", func(t *testing.T) {
		st := testStore(t)
		c, _ := recurring(t, st, toolSignal.Source, now, now, now, fourBursts(now)...)
		var other []store.Event // another tool, used every hour in sessions of its own: not the problem's unit
		for i, u := 0, now.Add(-28*aDay); u.Before(now); i, u = i+1, u.Add(time.Hour) {
			e := toolResult(fmt.Sprint("read-", i), u.Add(10*time.Minute), fmt.Sprint("read-", i), false)
			e.Tool = "Read"
			other = append(other, e)
		}
		put(t, st, other)
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, noTool, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Tool != "Bash" || g.Exposure != 684 {
			t.Fatalf("a signal with no tool whose events share one: %+v %v", g, err)
		}
	})
}

// A measured plan whose problem went quiet after it is counted again when a hand landing comes more than a window
// later: by then the gate holds nothing, so the recount gives a guard's watch, not a measurement.
func TestAHandLandingsRecountOfAProblemThatWentQuietIsAGuardWatch(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	plannedAt := time.Now().Truncate(time.Hour).Add(-8 * aDay)
	landed := plannedAt.Add(8 * aDay)
	_, failures := recurring(t, st, toolSignal.Source, plannedAt, landed, landed, plannedAt.Add(-27*aDay), plannedAt.Add(-19*aDay-12*time.Hour), plannedAt.Add(-12*aDay))
	var active []store.Event // the problem every hour for the three days before the plan
	for i, u := 0, plannedAt.Add(-3*aDay); u.Before(plannedAt); i, u = i+1, u.Add(time.Hour) {
		active = append(active, toolResult(fmt.Sprint("active-", i), u.Add(20*time.Minute), fmt.Sprint("active-", i), true))
	}
	put(t, st, active)
	c := testingCaseOf(t, st, "failures/v2:zz-quiet", append(failures, active...))
	s := &Ship{Store: st}
	plan, err := s.validPlan(ctx, c, toolSignal, 7*aDay, "", plannedAt)
	if g, _ := guardOf(t, plan); err != nil || g != nil || plan.Gate < MinSignal {
		t.Fatalf("the measured plan: %+v %v", plan, err)
	}
	plan, err = s.validPlan(ctx, c, toolSignal, 7*aDay, "", landed)
	if g, _ := guardOf(t, plan); err != nil || g == nil || g.Incidents != 3 {
		t.Fatalf("counted again at the landing: %+v %+v %v", plan, g, err)
	}
}

// guarded is a case whose change landed with the guard its proposal was counted with, and whose change takes effect
// effectAfter past its plan: the four bursts before its plan, the tool used every hour (in source use) until then, or
// the store problem gives (the plan's time and the end of its use). Its repository holds a commit before the change,
// for the change to go out of effect. The measure step runs on a clock of the test's own, a year back; the loop is not
// live.
func guarded(t *testing.T, effectAfter time.Duration, use string, problem func(st *store.Store, planAt, useUntil time.Time) store.Case) *measured {
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
	if problem == nil {
		problem = func(st *store.Store, planAt, useUntil time.Time) store.Case {
			c, _ := recurring(t, st, use, planAt, useUntil, planAt, fourBursts(planAt)...)
			return c
		}
	}
	c := problem(st, m.planAt, m.deployed.Add(-time.Hour))
	plan, err := m.r.Ship.validPlan(ctx, c, toolSignal, 7*aDay, "", m.planAt)
	if g, _ := guardOf(t, plan); err != nil || g == nil {
		t.Fatalf("the proposal's plan: %+v %v", plan, err)
	}
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "before the change")
	write(t, filepath.Join(m.repo, "a.txt"), "b\n")
	run(t, m.repo, "git", "commit", "-q", "-am", "the change")
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

// scanned marks the fixture's sources read as a real scan does: its last complete run started ten minutes before the
// pass's clock.
func (m *measured) scanned(t *testing.T) {
	t.Helper()
	for _, source := range []string{toolSignal.Source, "zz-use"} {
		if err := m.st.MarkRead(context.Background(), source, m.r.now().Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

// pass is a pass of the measure step as production runs one, its sources' scans started before it.
func (m *measured) pass(t *testing.T, o Options) Measured {
	t.Helper()
	m.scanned(t)
	return m.measure(t, o)
}

// passes is a pass of the measure step over every case, its sources' scans started before it.
func (m *measured) passes(t *testing.T, o Options) ([]Measured, error) {
	t.Helper()
	m.scanned(t)
	return m.r.Measure(context.Background(), o)
}

// usesIn puts n units of the tool's successful use in a source from a time, perHour sessions of their own an hour.
func usesIn(t *testing.T, st *store.Store, source, prefix string, from time.Time, n, perHour int) {
	t.Helper()
	var evs []store.Event
	for i := 0; i < n; i++ {
		e := toolResult(fmt.Sprint(prefix, i), from.Add(time.Duration(i/perHour)*time.Hour+time.Duration(i%perHour)*time.Minute), fmt.Sprint(prefix, i), false)
		e.Source = source
		evs = append(evs, e)
	}
	put(t, st, evs)
}

// weekly puts 25 units of the tool's use in a source in each of the five stretches of a 30-day watch from a time:
// 125 in all.
func weekly(t *testing.T, st *store.Store, source, prefix string, from time.Time) {
	t.Helper()
	for i := 0; i < 5; i++ {
		usesIn(t, st, source, fmt.Sprint(prefix, i, "-"), from.Add(time.Duration(i)*7*aDay+time.Hour), 25, 5)
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

// shown is what `case show` prints of a case.
func shown(t *testing.T, c store.Case) string {
	t.Helper()
	var b bytes.Buffer
	if err := evidenceview.Case(&b, c, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// closeGuarded runs a guarded case's watch to its end, the tool in use every week, and returns the pass that closed it.
func closeGuarded(t *testing.T, m *measured) Measured {
	t.Helper()
	m.pass(t, Options{})
	weekly(t, m.st, toolSignal.Source, "w", m.clock)
	m.clock = m.clock.Add(30*aDay + time.Hour)
	got := m.pass(t, Options{})
	if got.Status != "guarded" {
		t.Fatalf("the guard's close: %+v", got)
	}
	return got
}

// A guarded watch starts when the change first takes effect, with a log line that says it is guarded and what it
// assumes, and `case show` says it is watching. It lasts 30 days; the tool in use every week of them and no recurrence,
// it closes the case as guarded, never improved. The close, the case's status, `case show` and the note (a decided one,
// no shipped improvement) give the 125 hours watched and P = 1 - (684/809)^4.5, had the problem kept happening, and
// say that a problem that stopped by itself shows the same; P stays out of Fisher's p. A pass after the close keeps
// its place in the store's writes.
func TestAGuardedWatchClosesAsGuardedNeverImproved(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	got := m.pass(t, Options{})
	if got.Status != "measuring" || !jsonHas(t, got, `"guarded":true`) || !got.Due.Equal(m.clock.Add(30*aDay)) { // from its sighting: no file of the change dates its checkout
		t.Fatalf("the watch starts: %+v", got)
	}
	if l := lastLog(t, m.st, m.c.ID); l.Action != "guarding" || !strings.Contains(l.Detail, "guarded, not measured") ||
		!strings.Contains(l.Detail, "stopped by itself") || strings.Contains(l.Detail, "baseline") {
		t.Fatalf("its log line: %+v", l)
	}
	c := m.theCase(t)
	if evidenceview.StatusLine(c) != "measuring (guarded)" || !strings.Contains(shown(t, c), "watching 30d for a recurrence") || strings.Contains(shown(t, c), "recurrences") {
		t.Fatalf("a watch before its close: %q %s", evidenceview.StatusLine(c), shown(t, c))
	}
	effect := m.clock
	weekly(t, m.st, toolSignal.Source, "w", effect)
	m.clock = effect.Add(29 * aDay)
	if got := m.pass(t, Options{}); got.Status != "waiting" {
		t.Fatalf("inside its watch: %+v", got)
	}
	m.clock = effect.Add(30*aDay + time.Hour)
	got = m.pass(t, Options{})
	recur := 1 - math.Pow(684.0/809, 4.5)
	if got.Status != "guarded" || got.Result == nil || got.Result.Improved || got.Result.P != 0 || !jsonHas(t, got.Result, fmt.Sprintf(`"recur":%v`, recur)) ||
		!jsonHas(t, got.Result, `"exposure":684`) || !jsonHas(t, got.Result, `"watched":125`) ||
		!strings.Contains(got.Result.Effect, "no recurrence in 125 session hours watched") || !strings.Contains(got.Result.Effect, fmt.Sprintf("P=%.2f%%", 100*recur)) {
		t.Fatalf("the watch ended: %+v %+v", got, got.Result)
	}
	c = m.theCase(t)
	status := fmt.Sprintf("closed: guarded, 125 watched, P=%.0f%% had it kept happening; or it stopped by itself", 100*recur)
	if c.Status != store.CaseClosed || c.Reason != guardedReason || c.Measurement.After != 0 || !jsonHas(t, c.Measurement, `"Watched":125`) || evidenceview.StatusLine(c) != status {
		t.Fatalf("the case: %+v %q", c, evidenceview.StatusLine(c))
	}
	if s := shown(t, c); !strings.Contains(s, "  guarded:") || !strings.Contains(s, "no recurrence in 125 units of exposure watched") ||
		!strings.Contains(s, fmt.Sprintf("P=%.1f%% had it kept happening", 100*recur)) || !strings.Contains(s, "stopped by itself") || strings.Contains(s, "improv") {
		t.Fatalf("case show: %s", s)
	}
	if l := lastLog(t, m.st, c.ID); l.Action != "closed" || !strings.HasPrefix(l.Detail, "guarded: guarded, not measured") {
		t.Fatalf("its close: %+v", l)
	}
	ns := m.notes(t)
	if len(ns) != 1 || ns[0].Kind != store.NoteDecided || !strings.HasPrefix(ns[0].Title, "momentum guarded: ") || !strings.Contains(ns[0].Detail, "Had it kept happening") ||
		!strings.Contains(ns[0].Detail, "stopped by itself") || strings.Contains(ns[0].Detail, "improv") || noteKinds(ns)[store.NoteShipped] != 0 {
		t.Fatalf("its note: %+v", ns)
	}
	m.clock = c.Closed.Add(time.Hour)
	if ms, err := m.passes(t, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("a quiet close: %+v %v", ms, err)
	}
	if p, _, _ := m.r.Ship.Load(c.ID); !jsonHas(t, p, `"guard_seq":`) {
		t.Fatalf("its place in the store's writes after a quiet pass: %+v", p)
	}
}

// With no incident after the one its source began with, a guard's history gives no rate: its watch closes as guarded
// with the exposure watched, says there is no rate to compare, and gives no P.
func TestAGuardWithNoIncidentAfterItsFirstReportsNoRate(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, func(st *store.Store, planAt, useUntil time.Time) store.Case {
		return stoppedProblem(t, st, planAt.Add(-8*aDay), 60, useUntil, planAt)
	})
	got := closeGuarded(t, m)
	if got.Result == nil || !strings.Contains(got.Result.Effect, "no rate to compare: the history holds no incident after its first") || strings.Contains(got.Result.Effect, "P=") ||
		!jsonHas(t, got.Result, `"no_rate":true`) {
		t.Fatalf("a watch whose history holds no incident after its first: %+v", got.Result)
	}
	if s := evidenceview.StatusLine(m.theCase(t)); !strings.Contains(s, "no rate to compare") || strings.Contains(s, "P=") {
		t.Fatalf("its status: %q", s)
	}
}

// A watch is something only while the problem's unit was in use: one whose last two weeks saw no use of the tool is
// held as not in use, never passed, and the case keeps watching.
func TestAGuardsWatchWithAWeekOutOfUseIsHeld(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	effect := m.clock
	usesIn(t, m.st, toolSignal.Source, "w", effect.Add(time.Hour), 25, 5)
	usesIn(t, m.st, toolSignal.Source, "x", effect.Add(8*aDay), 25, 5)
	m.clock = effect.Add(30*aDay + time.Hour)
	if got := m.pass(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "not in use") {
		t.Fatalf("a watch with weeks out of use: %+v", got)
	}
	if c := m.theCase(t); c.Status != store.CaseMeasuring {
		t.Fatalf("the case: %+v", c)
	}
}

// A recurrence counts from the watch's first start, whatever came after it: a failure inside the watch, then the
// change out of effect and back in effect (which restarts a measured plan's window), brings the case back regressed,
// never to a guarded close.
func TestARecurrenceBeforeARestartStillRegressesTheGuard(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	effect := m.clock
	weekly(t, m.st, toolSignal.Source, "w", effect)
	put(t, m.st, []store.Event{toolResult("again", effect.Add(2*time.Hour), "again", true)})
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1") // out of effect
	m.clock = effect.Add(3 * time.Hour)
	var says []string
	says = append(says, m.pass(t, Options{}).Why)
	run(t, m.repo, "git", "checkout", "-q", "main") // in effect again
	m.clock = effect.Add(5 * time.Hour)
	says = append(says, m.pass(t, Options{}).Why)
	m.clock = effect.Add(31 * aDay)
	says = append(says, m.pass(t, Options{}).Why)
	if c := m.theCase(t); c.Reason == guardedReason || !strings.Contains(strings.Join(says, " | "), "regressed: its problem came back during its guard's watch") {
		t.Fatalf("a recurrence before a restart: %s %v", c.Reason, says)
	}
}

// A recurrence brings a guarded case back regressed. During the watch the change goes as one with no measured effect
// does (held here, the loop not being live, with its decided note). After a guarded close: a recurrence dated inside the
// watch and stored only after the close, a row rewritten after a pass into a form the signal counts, and new evidence
// of its signature each bring it back regressed, and it loses its guard's columns.
func TestARecurrenceBringsAGuardedCaseBackRegressed(t *testing.T) {
	ctx := context.Background()
	t.Run("during the watch", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		m.pass(t, Options{})
		effect := m.clock
		put(t, m.st, []store.Event{toolResult("again", effect.Add(2*time.Hour), "again", true)})
		m.clock = effect.Add(3 * time.Hour)
		got := m.pass(t, Options{})
		if got.Status != "held" || !strings.Contains(got.Why, "regressed: its problem came back during its guard's watch") || !strings.Contains(got.Why, "not live") {
			t.Fatalf("a recurrence inside the watch: %+v", got)
		}
		if ns := m.notes(t); len(ns) != 1 || ns[0].Kind != store.NoteDecided || !strings.Contains(ns[0].Detail, "momentum ship rollback "+m.c.ID) {
			t.Fatalf("the held rollback's note: %+v", ns)
		}
		// its rollback held, it keeps measuring, and says it recurred
		c := m.theCase(t)
		if evidenceview.StatusLine(c) != "measuring (guarded: recurred, rollback held)" || !strings.Contains(shown(t, c), "recurred: 1 units of its problem came back") {
			t.Fatalf("a held recurrence: %q %s", evidenceview.StatusLine(c), shown(t, c))
		}
	})
	back := func(t *testing.T, m *measured) {
		t.Helper()
		if got := m.pass(t, Options{}); got.Status != "back" || !strings.Contains(got.Why, "came back") {
			t.Fatalf("a recurrence after the close: %+v", got)
		}
		c := m.theCase(t)
		if l := lastLog(t, m.st, c.ID); c.Status != store.CaseOpen || c.Episode != 2 || l.Action != "regressed" || !strings.HasPrefix(l.Detail, "closed as guarded") ||
			c.Measurement.Exposure != 0 || c.Measurement.Watched != 0 {
			t.Fatalf("the case back: %+v %+v", c, l)
		}
	}
	t.Run("stored after the close", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		effect := m.clock
		closeGuarded(t, m)
		put(t, m.st, []store.Event{toolResult("late", effect.Add(5*aDay), "late", true)}) // dated inside the watch
		m.clock = m.theCase(t).Closed.Add(2 * time.Hour)
		if got := m.pass(t, Options{Dry: true}); got.Status != "back" || m.theCase(t).Status != store.CaseClosed {
			t.Fatalf("a dry watch: %+v", got)
		}
		back(t, m)
	})
	t.Run("rewritten after a pass", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		closeGuarded(t, m)
		m.clock = m.theCase(t).Closed.Add(time.Hour)
		if ms, err := m.passes(t, Options{}); err != nil || len(ms) != 0 {
			t.Fatalf("a quiet pass: %+v %v", ms, err)
		}
		// a use inside the watch read again as the failure it was
		e := toolResult("w0-3", time.Time{}, "w0-3", true)
		evs, err := m.st.Events(ctx, store.Query{Source: toolSignal.Source, Kinds: toolSignal.Kinds, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range evs {
			if x.SourceKey == "w0-3" {
				e.TS = x.TS
			}
		}
		if e.TS.IsZero() {
			t.Fatal("no use to rewrite")
		}
		put(t, m.st, []store.Event{e})
		m.clock = m.clock.Add(time.Hour)
		back(t, m)
	})
	t.Run("new evidence", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		closeGuarded(t, m)
		c := m.theCase(t)
		later := toolResult("evidence", c.Closed.Add(-time.Minute).Add(2*time.Minute), "evidence", true)
		put(t, m.st, []store.Event{later})
		f := store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title, Evidence: []store.Evidence{{Ref: "ev:" + later.ID()}}}
		if _, _, err := m.st.ObserveCase(ctx, f, "test"); err != nil {
			t.Fatal(err)
		}
		c = m.theCase(t)
		if l := lastLog(t, m.st, c.ID); c.Status != store.CaseOpen || l.Action != "regressed" || c.Measurement.Exposure != 0 {
			t.Fatalf("new evidence after a guarded close: %+v %+v", c, l)
		}
	})
}

// A case closed as guarded is watched by its signal alone: a source of its plan that stopped since does not hold the
// watch, and a recurrence brings the case back. When the signal's own source stops, a problem note says no recurrence
// can be seen.
func TestAClosedGuardIsWatchedByItsSignalAlone(t *testing.T) {
	m := guarded(t, time.Hour, "zz-use", nil)
	m.pass(t, Options{})
	weekly(t, m.st, "zz-use", "w", m.clock)
	m.clock = m.clock.Add(30*aDay + time.Hour)
	if got := m.pass(t, Options{}); got.Status != "guarded" {
		t.Fatalf("the guard's close: %+v", got)
	}
	closed := m.theCase(t).Closed
	m.r.Ship.SourceLive = func(source string) bool { return source != toolSignal.Source }
	m.clock = closed.Add(time.Hour)
	if ms, err := m.passes(t, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("a pass with the signal's source stopped: %+v %v", ms, err)
	}
	stopped := false
	for _, n := range m.notes(t) {
		stopped = stopped || n.Kind == store.NoteProblem && strings.Contains(n.Title, "the signal's source stopped")
	}
	if !stopped {
		t.Fatalf("no note that the signal's source stopped: %+v", m.notes(t))
	}
	m.r.Ship.SourceLive = func(source string) bool { return source != "zz-use" }
	put(t, m.st, []store.Event{toolResult("back", closed.Add(time.Hour), "back", true)})
	m.clock = closed.Add(2 * time.Hour)
	if got := m.pass(t, Options{}); got.Status != "back" {
		t.Fatalf("a recurrence with another source stopped: %+v", got)
	}
}

// A guard's watch starts at once when its change is first seen in effect, from that first in-effect time, and its
// history is counted again through that start when the watch ends: a change that takes effect 25 days after its plan,
// the tool used every hour since and the problem quiet, closes with the 1283 hours of exposure through its start.
func TestAGuardIsCountedAgainThroughItsStartWhenItsWatchEnds(t *testing.T) {
	m := guarded(t, 25*aDay, toolSignal.Source, nil)
	if got := m.pass(t, Options{}); got.Status != "measuring" || !jsonHas(t, got, `"guarded":true`) {
		t.Fatalf("a change in effect long after its problem went quiet: %+v", got)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	if g, _ := guardOf(t, p.Plan); g == nil || g.Exposure != 684 || !jsonHas(t, p, `"guard_from":"`) {
		t.Fatalf("its watch starts with the guard as proposed: %+v", g)
	}
	weekly(t, m.st, toolSignal.Source, "w", m.clock)
	m.clock = m.clock.Add(30*aDay + time.Hour)
	if got := m.pass(t, Options{}); got.Status != "guarded" {
		t.Fatalf("the watch's end: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if g, _ := guardOf(t, p.Plan); g == nil || g.Exposure != 1283 || g.Incidents != 4 || m.theCase(t).Measurement.Exposure != 1283 {
		t.Fatalf("its guard counted through its start: %+v %+v", g, m.theCase(t).Measurement)
	}
}

// A guard's watch starts with reads as a real scan records them, the newest complete scan started before the pass:
// nothing holds it for a read past the moment its change is seen in effect, and a recurrence in its first hour is
// acted on.
func TestAGuardsWatchStartsWithReadsBeforeThePass(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	var says []string
	for i := 0; i < 4; i++ {
		if i == 1 {
			put(t, m.st, []store.Event{toolResult("again", m.clock.Add(-time.Minute), "again", true)})
		}
		got := m.pass(t, Options{})
		says = append(says, got.Status+": "+got.Why)
		m.clock = m.clock.Add(10 * time.Minute)
	}
	if c := m.theCase(t); c.Status == store.CaseDeployed || !strings.Contains(strings.Join(says, " | "), "regressed") {
		t.Fatalf("the watch never started, or the recurrence was not acted on: %q; case %s", says, c.Status)
	}
}

// A case is guarded by its package's plan, not by guard columns an older build left behind: a measured plan's case
// that carries them shows as measured.
func TestACaseIsGuardedByItsPlan(t *testing.T) {
	m := measuredCase(t)
	m.measure(t, Options{})
	db, err := sql.Open("sqlite", m.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET guard_exposure = 684 WHERE id = ?`, m.c.ID); err != nil {
		t.Fatal(err)
	}
	c := m.theCase(t)
	if s := shown(t, c); evidenceview.StatusLine(c) != "measuring" || strings.Contains(s, "guarded:") || !strings.Contains(s, "measured:") {
		t.Fatalf("a measured plan's case with stale guard columns: %q %s", evidenceview.StatusLine(c), s)
	}
}

// A guarded case's lesson goes out marked guarded, not measured, with its P, had the problem kept happening.
func TestAGuardedCasesLessonIsMarkedGuarded(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	db, err := sql.Open("sqlite", st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE cases SET status = ?, reason = ?, before_n = 4, guard_exposure = 684, guard_watched = 700, guard_recur = 0.9581 WHERE id = ?",
		store.CaseClosed, "guarded", c.ID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	tacit := script(t, dir, "tacit", `printf '%s\n' "$@" > '`+args+`'`)
	if err := PublishLesson(ctx, st, &Ship{Store: st, Dir: filepath.Join(dir, "loop")}, c.ID, "Keep the tool's state fresh.", tacit, "fixture"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(string(raw), "\n")
	var desc, body string
	for i, field := range fields {
		switch field {
		case "--desc":
			desc = fields[i+1]
		case "--body":
			body = fields[i+1]
		}
	}
	if !strings.HasPrefix(desc, "unverified lesson from a loop run, guarded, not measured (P=96% that the problem, had it kept happening") ||
		!strings.Contains(desc, "stopped by itself") || !strings.Contains(body, "guarded, not measured") {
		t.Fatalf("a guarded case's lesson: %q %q", desc, body)
	}
}

// End to end: a preventive fix for the rare problem is checked and proposed with its guard (the request's body and
// the case's proposed line say guarded, and that a problem that stopped by itself shows the same), reviewed and landed
// by a live loop with a landed note that says so; its watch starts once the checkout holds it, and one recurrence rolls
// it back and takes its case back to testing, regressed.
func TestAGuardedChangeIsProposedLandedAndRolledBackOnARecurrence(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	now := time.Now().Truncate(time.Hour)
	c, _ := recurring(t, st, toolSignal.Source, now, now, now, fourBursts(now)...)
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
	if g, _ := guardOf(t, p.Plan); g == nil || g.Incidents != 4 {
		t.Fatalf("its plan: %+v", p.Plan)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "Guarded, not measured") || !strings.Contains(string(calls), "a close that claims nothing for the change") ||
		!strings.Contains(string(calls), "stopped by itself") || strings.Contains(string(calls), "improved") || strings.Contains(string(calls), "Fisher") {
		t.Fatalf("the request's body does not say it is guarded: %s", calls)
	}
	if l := lastLog(t, st, c.ID); l.Action != "proposed" || !strings.Contains(l.Detail, "guarded in session hours: 4 incidents") || !strings.Contains(l.Detail, "stopped by itself") ||
		strings.Contains(l.Detail, "baseline") {
		t.Fatalf("its proposed line: %+v", l)
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
	m := &measured{r: r, st: st, c: c}
	landedNote := false
	for _, n := range m.notes(t) {
		landedNote = landedNote || strings.HasPrefix(n.ID, "landed:") && strings.Contains(n.Detail, "Guarded, not measured") && strings.Contains(n.Detail, "stopped by itself")
	}
	if !landedNote {
		t.Fatalf("the landed note does not say it is guarded: %+v", m.notes(t))
	}
	run(t, f.main, "git", "fetch", "origin", "main")
	run(t, f.main, "git", "merge", "--ff-only", "origin/main")
	if got := m.pass(t, Options{}); got.Status != "measuring" || !jsonHas(t, got, `"guarded":true`) {
		t.Fatalf("the watch starts: %+v", got)
	}
	put(t, st, []store.Event{toolResult("again", clock.Add(time.Hour), "again", true)})
	clock = clock.Add(2 * time.Hour)
	if got := m.pass(t, Options{}); got.Status != "rolled back" || !strings.Contains(got.Why, "regressed") {
		t.Fatalf("a recurrence: %+v", got)
	}
	if m.theCase(t).Status != store.CaseTesting {
		t.Fatal("the rollback did not bring the case back")
	}
	if body := run(t, f.dir, "git", "--git-dir", f.origin, "show", "main:fake.go"); !strings.Contains(body, "return 41") {
		t.Fatal("origin did not roll back")
	}
}

// backWithin runs passes of the measure step at the clocks given and says whether one of them brought the guarded
// case back or found its problem back during the watch, and the case after them.
func backWithin(t *testing.T, m *measured, clocks ...time.Time) (bool, store.Case) {
	t.Helper()
	found := false
	for _, at := range clocks {
		m.clock = at
		ms, err := m.passes(t, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range ms {
			found = found || x.Status == "back" || strings.Contains(x.Why, "regressed")
		}
	}
	return found, m.theCase(t)
}

// The write sequence a guarded close reads by never goes down: a forced rebuild of the signal's source after a pass
// deletes the rows that held the highest, and a recurrence written then is still read.
func TestARebuildDoesNotHideARecurrenceAfterAGuardedClose(t *testing.T) {
	ctx := context.Background()
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	closeGuarded(t, m)
	closed := m.theCase(t).Closed
	if back, _ := backWithin(t, m, closed.Add(time.Hour)); back {
		t.Fatal("a quiet pass brought the case back")
	}
	if _, err := m.st.Rebuild(ctx, toolSignal.Source, true); err != nil {
		t.Fatal(err)
	}
	put(t, m.st, []store.Event{toolResult("back", closed.Add(time.Hour), "back", true)})
	if back, c := backWithin(t, m, closed.Add(2*time.Hour)); !back || c.Status != store.CaseOpen {
		t.Fatalf("a recurrence written after a rebuild: %+v", c)
	}
}

// A recurrence a build from before the write sequence writes, with none of its own, is still read after a guarded
// close.
func TestARecurrenceAnOlderBuildWroteBringsAGuardedCaseBack(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	closeGuarded(t, m)
	closed := m.theCase(t).Closed
	if back, _ := backWithin(t, m, closed.Add(time.Hour)); back {
		t.Fatal("a quiet pass brought the case back")
	}
	db, err := sql.Open("sqlite", m.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := toolResult("old-build", closed.Add(90*time.Minute), "old-build", true)
	if _, err := db.Exec(`INSERT INTO events (id, source, source_key, ts, session, agent, harness, model, cwd, actor, kind, tool, summary, meta, ref, heuristic, experiment, ingested)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID(), e.Source, e.SourceKey, e.TS.UnixMilli(), e.Session, "", e.Harness, "", "", string(e.Actor), e.Kind, e.Tool,
		e.Summary, e.Meta, "", 0, "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	put(t, m.st, []store.Event{toolResult("ok-after", closed.Add(100*time.Minute), "ok-after", false)})
	if back, c := backWithin(t, m, closed.Add(2*time.Hour)); !back || c.Status != store.CaseOpen {
		t.Fatalf("a recurrence an older build wrote: %+v", c)
	}
}

// No date bounds a recurrence: one dated ahead of the pass that reads it after a guarded close, and one stored before
// the closing pass but dated past its clock, each bring the case back.
func TestARecurrenceIsReadHoweverItIsDated(t *testing.T) {
	t.Run("ahead of the pass after the close", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		closeGuarded(t, m)
		closed := m.theCase(t).Closed
		put(t, m.st, []store.Event{toolResult("ahead", time.Now().Add(3*time.Hour), "ahead", true)})
		if back, c := backWithin(t, m, closed.Add(time.Hour), time.Now().Add(4*time.Hour)); !back || c.Status != store.CaseOpen {
			t.Fatalf("a recurrence dated ahead of its pass: %+v", c)
		}
	})
	t.Run("past the closing pass's clock", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		m.pass(t, Options{})
		weekly(t, m.st, toolSignal.Source, "w", m.clock)
		closing := m.clock.Add(30*aDay + time.Hour)
		put(t, m.st, []store.Event{toolResult("late-dated", closing.Add(2*time.Minute), "late-dated", true)})
		if back, c := backWithin(t, m, closing, closing.Add(time.Hour)); !back || c.Reason == guardedReason && c.Status == store.CaseClosed {
			t.Fatalf("a recurrence dated past the closing pass: %+v", c)
		}
	})
}

// A guarded case that comes back on new evidence names the reason it closed with, and shows no watch of its last
// episode as a measurement.
func TestAGuardedComebackShowsNoOldWatch(t *testing.T) {
	ctx := context.Background()
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	closeGuarded(t, m)
	c := m.theCase(t)
	later := toolResult("evidence", c.Closed.Add(time.Minute), "evidence", true)
	put(t, m.st, []store.Event{later})
	f := store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title, Evidence: []store.Evidence{{Ref: "ev:" + later.ID()}}}
	if _, _, err := m.st.ObserveCase(ctx, f, "test"); err != nil {
		t.Fatal(err)
	}
	c = m.theCase(t)
	l := lastLog(t, m.st, c.ID)
	if s := shown(t, c); strings.Contains(l.Detail, "already solved") || !strings.Contains(l.Detail, "since it closed as guarded") || strings.Contains(s, "measured:") ||
		strings.Contains(s, "guarded:") {
		t.Fatalf("a guarded comeback: %s: %s\n%s", l.Action, l.Detail, s)
	}
}
