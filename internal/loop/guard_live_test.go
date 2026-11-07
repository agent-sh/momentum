package loop

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/evidenceview"
	"github.com/agent-sh/momentum/internal/store"
)

// A guard's liveness (SPEC, Measurement, Guards): no wait of a guard waits for a stretch to be read whole, so one
// failed read never holds it for good; and its recurrences are counted before any wait.

// markAll marks sources read as a real scan does: the newest complete run started ten minutes before the pass.
func markAll(t *testing.T, m *measured, sources ...string) {
	t.Helper()
	for _, s := range sources {
		if err := m.st.MarkRead(context.Background(), s, m.r.now().Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

// One failed read inside a guard's watch, of a command source its plan counts, does not hold its close: a capacity
// run that failed on day 10, and a sessions scan that stopped for a night (two runs nine hours apart, which the store
// records as one incomplete run), each leave the watch to close as guarded once its sources read past its end.
func TestAGuardClosesThroughAFailedReadInsideItsWatch(t *testing.T) {
	ctx := context.Background()
	t.Run("a failed capacity run", func(t *testing.T) {
		m := guarded(t, time.Hour, "capacity", nil)
		markAll(t, m, toolSignal.Source, "capacity")
		m.measure(t, Options{})
		start := m.clock
		weekly(t, m.st, "capacity", "w", start)
		if err := m.st.MarkIncomplete(ctx, "capacity", "", start.Add(10*aDay)); err != nil {
			t.Fatal(err)
		}
		m.clock = start.Add(10*aDay + 2*time.Hour)
		markAll(t, m, toolSignal.Source, "capacity")
		m.measure(t, Options{})
		m.clock = start.Add(30*aDay + time.Hour)
		markAll(t, m, toolSignal.Source, "capacity")
		if got := m.measure(t, Options{}); got.Status != "guarded" {
			t.Fatalf("a watch with a failed capacity run on day 10: %+v", got)
		}
	})
	t.Run("a night asleep", func(t *testing.T) {
		m := guarded(t, time.Hour, store.Sessions, nil)
		markAll(t, m, toolSignal.Source, store.Sessions)
		m.measure(t, Options{})
		start := m.clock
		weekly(t, m.st, store.Sessions, "w", start)
		for at := start; at.Before(start.Add(31 * aDay)); at = at.Add(10 * time.Minute) {
			if at.After(start.Add(10*aDay)) && at.Before(start.Add(10*aDay+9*time.Hour)) {
				continue // asleep
			}
			if err := m.st.MarkRunRead(ctx, store.Sessions, "claude", at, 15*time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := m.st.MarkRead(ctx, store.Sessions, at.Add(-15*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		m.clock = start.Add(30*aDay + time.Hour)
		markAll(t, m, toolSignal.Source)
		if got := m.measure(t, Options{}); got.Status != "guarded" {
			t.Fatalf("a watch with a night asleep on day 10: %+v", got)
		}
	})
}

// A recurrence is acted on whatever a wait for a source would hold: with a source of the plan switched off, and a
// command source's failed run between the plan and the change's effect, a failure in the watch's first hour brings the
// case back (its rollback held, the loop not being live) on the first pass after it.
func TestARecurrenceIsActedOnBeforeAnyWaitForASource(t *testing.T) {
	ctx := context.Background()
	m := guarded(t, 3*aDay, "capacity", func(st *store.Store, planAt, useUntil time.Time) store.Case {
		c, _ := recurring(t, st, "capacity", planAt, useUntil, planAt, fourBursts(planAt)...)
		var evs []store.Event
		for i, u := 0, planAt.Add(-20*aDay); u.Before(planAt); i, u = i+1, u.Add(time.Hour) {
			e := toolResult(fmt.Sprint("third-", i), u.Add(5*time.Minute), fmt.Sprint("third-", i), false)
			e.Source = "zz-third"
			evs = append(evs, e)
		}
		began := toolResult("third-began", planAt.Add(-60*aDay), "third-began", false)
		began.Source = "zz-third"
		put(t, st, append(evs, began))
		if err := st.MarkRead(ctx, "zz-third", planAt); err != nil {
			t.Fatal(err)
		}
		return c
	})
	if err := m.st.MarkIncomplete(ctx, "capacity", "", m.planAt.Add(aDay)); err != nil {
		t.Fatal(err)
	}
	m.r.Ship.SourceLive = func(source string) bool { return source != "zz-third" }
	markAll(t, m, toolSignal.Source, "capacity")
	m.measure(t, Options{})
	start := m.clock
	put(t, m.st, []store.Event{toolResult("again", start.Add(time.Hour), "again", true)})
	m.clock = start.Add(2 * time.Hour)
	markAll(t, m, toolSignal.Source, "capacity")
	if got := m.measure(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "regressed") || m.theCase(t).Measurement.After == 0 {
		t.Fatalf("a recurrence with a source switched off and a failed run before the change: %+v", got)
	}
}

// A change with none of its files in the checkout (it only deletes) has no file to date its checkout by: its watch
// starts when the measure step first sees it in effect, two days after its landing here, and a failure between the
// landing and then is no recurrence.
func TestADeletionOnlyChangeWatchesFromItsSighting(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1") // the checkout does not hold it yet
	landed := m.deployed
	m.clock = landed.Add(time.Hour)
	if got := m.pass(t, Options{}); got.Status != "waiting" {
		t.Fatalf("before the checkout holds it: %+v", got)
	}
	put(t, m.st, []store.Event{toolResult("before-effect", landed.Add(2*time.Hour), "before-effect", true)})
	run(t, m.repo, "git", "checkout", "-q", "main")
	m.clock = landed.Add(2 * aDay)
	m.pass(t, Options{})
	m.clock = m.clock.Add(time.Hour)
	got := m.pass(t, Options{})
	if p, _, _ := m.r.Ship.Load(m.c.ID); !p.GuardFrom.Equal(landed.Add(2*aDay)) || got.Status == "held" || m.theCase(t).Measurement.After != 0 {
		t.Fatalf("its watch's start %s (landed %s): %+v", p.GuardFrom, landed, got)
	}
}

// The close counts a guard's history again from where its proposal's count began, and drops the incident its source
// began with as the proposal did: a failure stored after the proposal, six and a half days after that burst ended,
// belongs to the burst, so the history still holds no incident after its first and the report gives no rate.
func TestTheClosesRecountDropsTheIncidentItsSourceBeganWith(t *testing.T) {
	m := guarded(t, 3*aDay, toolSignal.Source, func(st *store.Store, planAt, useUntil time.Time) store.Case {
		return stoppedProblem(t, st, planAt.Add(-8*aDay), 60, useUntil, planAt)
	})
	put(t, m.st, []store.Event{toolResult("burst-late", m.planAt.Add(aDay), "burst-late", true)})
	got := closeGuarded(t, m)
	if got.Result == nil || !strings.Contains(got.Result.Effect, "no rate to compare") || m.theCase(t).Measurement.Before != 0 {
		t.Fatalf("a burst's late failure counted as one more incident: %+v", got.Result)
	}
}

// The guard's liveness over randomized store histories (SPEC, Measurement, Guards, Liveness). Each history starts at a
// guard plan of four bursts, lands its change within three days (its file dated between the landing and the first pass
// that sees it), and runs the measure step every three hours while the machine is awake. Its sources: the signal's (an
// ingest source, whose scans now and then read in part, which a rebuild may drop and read again, to which an older
// build writes some events without a write sequence, which may be switched off for days, and which may read only in
// part from the watch's end on, its failed files newer than the watch's start), the sessions scan (a
// command source, read every twelve minutes while awake: a night or days asleep leave one incomplete run, and it may
// stop for good), one switched off at some point, and one that began inside the plan's span. The change may go out of
// effect for a while; the unit may go unused for two weeks, or the whole watch; the loop may be live. A recurrence, in
// some histories, of the signal or only of the case's evidence, is dated in the watch, before the pass that first sees
// the change, or after the watch's close, and stored up to two days late. On every history, stated from what it did:
//   - Closes: the case never closes before each source of its plan has shown everything dated in its 30 days (a scan
//     that read whole showed everything dated before it, the sessions scan everything a stamp's keep before it, and
//     an ingest scan that failed only on files older than the watch's start counts as whole; a source switched off
//     shows nothing more; a command source that stopped is left out a window, the plan's week, after its last read;
//     the signal's source is left out a window after the end once it has read there only in part), and it is closed
//     by the first pass after that with its change in effect and its signal's source on;
//   - Acts: a stored recurrence brings the case back, rolls its change back, or holds its rollback with the case
//     marked recurred, on the first pass after it is stored;
//   - Never improved: the case never closes as improved, and no shipped note is put.
func TestTheGuardsLivenessOverSimulatedHistories(t *testing.T) {
	histories := []simHistory{
		{}, {signalStop: true}, {idle: true}, {unused: true}, {},
		{recur: "during"}, {recur: "during", live: true}, {recur: "during", signalStop: true},
		{recur: "after"}, {recur: "after", live: true}, {recur: "after", signalStop: true},
		{recur: "evidence"}, {recur: "evidence", live: true}, {recur: "sighting"}, {partial: true},
		{late: true}, {recur: "evidence", late: true}, {recur: "during", late: true},
	}
	for i, h := range histories {
		h.seed = uint64(8000 + i)
		t.Run(h.String(), func(t *testing.T) {
			t.Parallel()
			simulateGuard(t, h)
		})
	}
}

// simHistory is what a simulated history holds beyond what its seed draws.
type simHistory struct {
	seed       uint64
	recur      string // "", "during", "after", "evidence" (only the case's evidence holds it), "sighting" (before the first pass)
	live       bool   // the loop is live: a recurrence during the watch rolls the change back
	idle       bool   // the unit unused for two weeks inside the watch
	unused     bool   // the unit unused from the landing on
	signalStop bool   // the signal's own source switched off for one to six days
	partial    bool   // the signal's own source reads only in part from the watch's end on, its failed files in the watch
	late       bool   // the detector files late: an "evidence" recurrence up to three days after it is stored, and a failure
	// dated between the plan and the landing, of the signal's tool or another, as the case's evidence inside the watch,
	// which is history and no recurrence
}

func (h simHistory) String() string {
	name := fmt.Sprint(h.seed, "-", map[string]string{"": "quiet"}[h.recur], h.recur)
	for flag, on := range map[string]bool{"live": h.live, "idle": h.idle, "unused": h.unused, "signal-stop": h.signalStop, "partial": h.partial, "late": h.late} {
		if on {
			name += "-" + flag
		}
	}
	return name
}

func simulateGuard(t *testing.T, h simHistory) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(h.seed, 80))
	between := func(lo, hi time.Duration) time.Duration { return lo + time.Duration(rng.Int64N(int64(hi-lo))) }
	const lag, tick, sessionsWindow = 15 * time.Minute, 12 * time.Minute, 7 * aDay
	st, dbPath := testStoreAt(t)
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var clock time.Time
	otherOff, signalOff := false, false
	plan0 := time.Now().Add(-365 * aDay).Truncate(time.Hour)
	landed := plan0.Add(between(6*time.Hour, 3*aDay))

	// the repository the change lands in: a checkout and, for a live loop, its origin and a host clone to roll back from
	var ship *Ship
	var repo, host, base string
	if h.live {
		f := newRepo(t, nil)
		ship, repo, host = shipWithSandbox(st, f, Sandbox{}), f.main, filepath.Join(f.dir, "host")
		base = strings.TrimSpace(run(t, repo, "git", "rev-parse", "HEAD"))
		write(t, filepath.Join(repo, "a.txt"), "b\n")
		run(t, repo, "git", "add", "-A")
		run(t, repo, "git", "commit", "-q", "-m", "the change")
		run(t, repo, "git", "push", "-q", "origin", "HEAD:main")
		run(t, f.dir, "git", "clone", "-q", f.origin, host)
	} else {
		dir := t.TempDir()
		ship = &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}
		repo = filepath.Join(t.TempDir(), "repo")
		run(t, t.TempDir(), "git", "init", "-q", "-b", "main", repo)
		write(t, filepath.Join(repo, "a.txt"), "a\n")
		run(t, repo, "git", "add", "-A")
		run(t, repo, "git", "commit", "-q", "-m", "before the change")
		write(t, filepath.Join(repo, "a.txt"), "b\n")
		run(t, repo, "git", "commit", "-q", "-am", "the change")
	}
	head := strings.TrimSpace(run(t, repo, "git", "rev-parse", "HEAD"))
	r := &Runner{Store: st, Dir: ship.Dir, Budgets: DefaultBudgets, Ship: ship}
	r.Now = func() time.Time { return clock }
	ship.Now = r.Now
	ship.SourceLive = func(source string) bool {
		return !(source == "zz-other" && otherOff) && !(source == toolSignal.Source && signalOff)
	}

	other := func(source, key string, at time.Time) store.Event {
		return store.Event{Source: source, SourceKey: key, TS: at, Session: key, Harness: "claude", Actor: store.ActorAgent, Kind: "text", Summary: "a session's text"}
	}
	phase := func(key string, at time.Time) store.Event {
		return store.Event{Source: store.Sessions, SourceKey: key, TS: at, Session: key, Harness: "claude", Actor: store.ActorHarness, Kind: "phase", Summary: key + ": working -> idle"}
	}
	hourly := func(at time.Time, n int) []store.Event {
		key := fmt.Sprint("h", n)
		evs := []store.Event{toolResult("use-"+key, at, "use-"+key, false), phase("phase-"+key, at), other("zz-other", "other-"+key, at)}
		if !at.Before(plan0.Add(-10 * aDay)) {
			evs = append(evs, other("zz-late", "late-"+key, at))
		}
		return evs
	}

	// the history before the plan: hourly use, four bursts, every source read just before the plan
	history := []store.Event{other(toolSignal.Source, "tool-began", plan0.Add(-60*aDay)), phase("sessions-began", plan0.Add(-60*aDay)),
		other("zz-other", "other-began", plan0.Add(-60*aDay))}
	hour := 0
	for at := plan0.Add(-28 * aDay); at.Before(plan0); at = at.Add(time.Hour) {
		history = append(history, hourly(at, hour)...)
		hour++
	}
	var failures []store.Event
	for b, from := range fourBursts(plan0) {
		for j := 0; j < 3; j++ {
			failures = append(failures, toolResult(fmt.Sprintf("b%d-%d", b, j), from.Add(time.Duration(j)*time.Hour+30*time.Minute), fmt.Sprintf("b%d-%d", b, j), true))
		}
	}
	put(t, st, append(history, failures...))
	signalStored := slices.DeleteFunc(slices.Clone(append(history, failures...)), func(e store.Event) bool { return e.Source != toolSignal.Source })
	markRead := func(source string, at time.Time) {
		t.Helper()
		if err := st.MarkRead(ctx, source, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{toolSignal.Source, "zz-other", "zz-late"} {
		markRead(source, plan0.Add(-10*time.Minute))
	}
	if err := st.MarkRunRead(ctx, store.Sessions, "claude", plan0.Add(-10*time.Minute), lag); err != nil {
		t.Fatal(err)
	}
	markRead(store.Sessions, plan0.Add(-10*time.Minute-lag))
	signature := fmt.Sprint("failures/v2:zz-sim-", h.seed)
	c := testingCaseOf(t, st, signature, failures)
	plan, err := ship.validPlan(ctx, c, toolSignal, 7*aDay, "", plan0)
	if g, _ := guardOf(t, plan); err != nil || g == nil {
		t.Fatalf("the proposal's plan: %+v %v", plan, err)
	}
	if !slices.Contains(plan.Sources, store.Sessions) || !slices.Contains(plan.Sources, "zz-other") || slices.Contains(plan.Sources, "zz-late") {
		t.Fatalf("the plan's sources: %v, left out %v", plan.Sources, plan.Omitted)
	}
	// a failure dated between the plan and the landing, stored then: history, which the detector files inside the watch
	var historyLate store.Event
	if h.late {
		historyLate = toolResult("history-late", plan0.Add(between(time.Hour, landed.Sub(plan0)-time.Hour)), "history-late", true)
		if rng.IntN(2) == 0 {
			historyLate.Tool = "Edit"
		}
		put(t, st, []store.Event{historyLate})
		signalStored = append(signalStored, historyLate)
	}
	pkg := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", Dir: repo, Host: host, BaseCommit: base, PR: "https://github.com/agent-sh/fake/pull/7",
		State: Landed, Landed: []string{head}, LandedAt: landed, Changed: []string{"a.txt"}, Plan: &plan}
	if err := ship.save(&pkg); err != nil {
		t.Fatal(err)
	}

	// what happens to the machine and the store from the plan on
	var sleeps [][2]time.Time
	for n := 1 + rng.IntN(3); n > 0; n-- {
		from := plan0.Add(between(time.Hour, 40*aDay))
		sleeps = append(sleeps, [2]time.Time{from, from.Add(between(2*time.Hour, 3*aDay))})
	}
	if rng.IntN(2) == 0 && landed.Sub(plan0) > 10*time.Hour { // a night between the plan and the landing
		from := plan0.Add(between(time.Hour, landed.Sub(plan0)-9*time.Hour))
		sleeps = append(sleeps, [2]time.Time{from, from.Add(between(2*time.Hour, 8*time.Hour))})
	}
	within := func(spans [][2]time.Time, at time.Time) bool {
		return slices.ContainsFunc(spans, func(s [2]time.Time) bool { return !at.Before(s[0]) && at.Before(s[1]) })
	}
	var outOfEffect, idle, signalStop [][2]time.Time
	if !h.live && rng.IntN(10) < 6 {
		from := landed.Add(between(aDay, 25*aDay))
		outOfEffect = append(outOfEffect, [2]time.Time{from, from.Add(between(2*time.Hour, 2*aDay))})
	}
	if h.idle {
		from := landed.Add(between(aDay, 12*aDay))
		idle = append(idle, [2]time.Time{from, from.Add(between(14*aDay, 15*aDay))})
	}
	if h.unused {
		idle = append(idle, [2]time.Time{landed, plan0.Add(100 * aDay)})
	}
	if h.signalStop {
		from := landed.Add(between(2*aDay, 33*aDay))
		signalStop = append(signalStop, [2]time.Time{from, from.Add(between(aDay, 6*aDay))})
	}
	var otherStop, sessionsStop, rebuildAt time.Time
	if rng.IntN(2) == 0 {
		otherStop = plan0.Add(between(time.Hour, 30*aDay))
	}
	if rng.IntN(4) == 0 {
		sessionsStop = plan0.Add(between(20*aDay, 38*aDay))
	}
	if rng.IntN(2) == 0 {
		rebuildAt = landed.Add(between(2*aDay, 45*aDay))
	}
	t.Logf("landed +%s, asleep %v, out of effect %v, idle %v, signal off %v, other off %v, sessions stop %v, rebuild %v", landed.Sub(plan0), sleeps,
		outOfEffect, idle, signalStop, otherStop, sessionsStop, rebuildAt)

	// what the history did, as the oracle states it: when each source of the plan last read, and when it has shown
	// everything dated in the watch
	lastRead := map[string]time.Time{}
	shown := map[string]time.Time{}
	var pending, deferred []store.Event
	raw, evidence := map[string]bool{}, map[string]bool{}
	var guardFrom, end, closedAt, recurDated, recurStoreAt, recurStored, took, partialAfterEnd, recurFileAt, historyFileAt time.Time
	var lateRecur store.Event // an "evidence" recurrence stored and not yet filed
	recurMade := false        // the recurrence is stored
	recurRaw, deployed, effect, woke, closed := false, false, true, false, false
	acted := "" // the case's status on the first pass after its recurrence was stored
	// scanned records a scan of a source that read all it reads, at the time it shows everything dated before (for the
	// sessions scan, a stamp's keep before its start: a session gone from a scan is written vanished that much later)
	scanned := func(source string, at time.Time) {
		lastRead[source] = at
		if !end.IsZero() && at.After(end) && shown[source].IsZero() {
			shown[source] = at
		}
	}
	storeEvents := func(at time.Time) {
		t.Helper()
		if !recurStoreAt.IsZero() && !recurMade && !at.Before(recurStoreAt) && !signalOff {
			recurMade = true
			e := toolResult("recurrence", recurDated, "recurrence", true)
			late := h.recur == "evidence" && h.late
			if h.recur == "evidence" {
				e.Summary = "Exit code 1 zz-tool-crashed: the tool failed" // a form the signal does not count
				evidence[e.ID()] = !late
			}
			pending, raw[e.ID()], recurStored = append(pending, e), recurRaw, at
			if late { // filed later: what the watch acts on is the filing
				lateRecur, recurStored, recurFileAt = e, time.Time{}, at.Add(between(0, 3*aDay))
			}
			t.Logf("a recurrence dated +%s stored at +%s", recurDated.Sub(plan0), at.Sub(plan0))
		}
		var evs []store.Event
		var filed []store.Evidence
		for _, e := range pending {
			if e.Source == toolSignal.Source && signalOff {
				deferred = append(deferred, e) // its source is off: read when it is on again
				continue
			}
			if evidence[e.ID()] {
				filed = append(filed, store.Evidence{Ref: "ev:" + e.ID()})
			}
			if e.Source == toolSignal.Source {
				signalStored = append(signalStored, e)
			}
			if raw[e.ID()] {
				if _, err := db.ExecContext(ctx, `INSERT INTO events (id, source, source_key, ts, session, agent, harness, model, cwd, actor, kind, tool, summary, meta, ref, heuristic, experiment, ingested)
				  VALUES (?,?,?,?,?,'',?,'','',?,?,?,?,?,'',0,'',?)`, e.ID(), e.Source, e.SourceKey, e.TS.UnixMilli(), e.Session, e.Harness, string(e.Actor), e.Kind, e.Tool,
					e.Summary, e.Meta, time.Now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
				continue
			}
			evs = append(evs, e)
		}
		put(t, st, evs)
		pending = nil
		if lateRecur.Source != "" && !at.Before(recurFileAt) { // the detector files the recurrence late
			filed, recurStored = append(filed, store.Evidence{Ref: "ev:" + lateRecur.ID()}), at
			t.Logf("the recurrence filed at +%s", at.Sub(plan0))
			lateRecur = store.Event{}
		}
		if historyLate.Source != "" && !historyFileAt.IsZero() && !at.Before(historyFileAt) { // and history late
			filed = append(filed, store.Evidence{Ref: "ev:" + historyLate.ID()})
			t.Logf("history of %s dated +%s filed at +%s", historyLate.Tool, historyLate.TS.Sub(plan0), at.Sub(plan0))
			historyLate = store.Event{}
		}
		if len(filed) > 0 { // the detector files it by the case's signature
			if _, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: signature, Title: "the tool fails", Evidence: filed}, "detector"); err != nil {
				t.Fatal(err)
			}
		}
		if signalOff {
			// its source is off: nothing of it is read
		} else {
			if !rebuildAt.IsZero() && !at.Before(rebuildAt) { // the signal's source dropped and read again
				if _, err := st.Rebuild(ctx, toolSignal.Source, false); err != nil {
					t.Fatal(err)
				}
				put(t, st, signalStored)
				rebuildAt = time.Time{}
				delete(lastRead, toolSignal.Source)
				delete(shown, toolSignal.Source) // its events must be read again
				partialAfterEnd = time.Time{}    // and its partial runs go with its reads
			}
			partialOnly := h.partial && !end.IsZero() && at.After(end)
			if partialOnly || rng.IntN(100) < 8 { // a scan of the signal's source that read in part
				failed := at.Add(-time.Hour)
				if !partialOnly && rng.IntN(2) == 0 {
					failed = landed.Add(-aDay)
				}
				if err := st.MarkPartialRead(ctx, toolSignal.Source, at, failed); err != nil {
					t.Fatal(err)
				}
				if !guardFrom.IsZero() && failed.Before(guardFrom) && !end.IsZero() && at.After(end) && shown[toolSignal.Source].IsZero() {
					shown[toolSignal.Source] = at // its failed files hold nothing of the watch
				}
				if !end.IsZero() && at.After(end) && partialAfterEnd.IsZero() {
					partialAfterEnd = at
				}
			} else {
				markRead(toolSignal.Source, at)
				scanned(toolSignal.Source, at)
			}
		}
		if !otherOff {
			markRead("zz-other", at)
			scanned("zz-other", at)
		}
		markRead("zz-late", at)
		if sessionsStop.IsZero() || at.Before(sessionsStop) {
			if err := st.MarkHarnessRead(ctx, store.Sessions, "claude", at.Add(-lag)); err != nil {
				t.Fatal(err)
			}
			markRead(store.Sessions, at.Add(-lag))
			scanned(store.Sessions, at.Add(-lag))
		}
	}
	// allShown says whether every source of the plan has shown everything dated in the watch by at
	allShown := func(at time.Time) bool {
		for _, source := range plan.Sources {
			switch {
			case source == "zz-other" && otherOff:
			case source == store.Sessions && !sessionsStop.IsZero() && !at.Before(sessionsStop) && at.Sub(lastRead[source]) > sessionsWindow:
			case source == toolSignal.Source && !partialAfterEnd.IsZero() && at.Sub(end) > plan.Window: // read only in part past the end, a window on
			case shown[source].IsZero() || shown[source].After(at):
				return false
			}
		}
		return true
	}
	horizon := plan0.Add(60 * aDay)
	for n, at := 0, plan0; at.Before(horizon); n, at = n+1, at.Add(tick) {
		if within(sleeps, at) {
			woke = true
			continue
		}
		if !otherStop.IsZero() && !at.Before(otherStop) {
			otherOff = true
		}
		if off := within(signalStop, at); off != signalOff {
			signalOff = off
			if !off { // on again: its ingest reads what it missed
				pending, deferred = append(pending, deferred...), nil
			}
		}
		if want := !within(outOfEffect, at); want != effect {
			if want {
				run(t, repo, "git", "checkout", "-q", "main")
			} else {
				run(t, repo, "git", "checkout", "-q", "HEAD~1")
			}
			effect = want
		}
		if woke && (sessionsStop.IsZero() || at.Before(sessionsStop)) { // the first scan after a sleep
			if err := st.MarkRunRead(ctx, store.Sessions, "claude", at, lag); err != nil {
				t.Fatal(err)
			}
			markRead(store.Sessions, at.Add(-lag))
			scanned(store.Sessions, at.Add(-lag))
		}
		woke = false
		if at.Minute() == 0 {
			for _, e := range hourly(at, hour) {
				switch {
				case e.Source == "zz-other" && otherOff, e.Source == store.Sessions && !sessionsStop.IsZero() && !at.Before(sessionsStop):
					continue
				case e.Source == toolSignal.Source && !at.Before(landed) && within(idle, at):
					continue // the unit not used
				case e.Source == toolSignal.Source && rng.IntN(10) == 0:
					raw[e.ID()] = true // an older build's write
				}
				pending = append(pending, e)
			}
			hour++
		}
		pass := n%15 == 0
		if pass || within(sleeps, at.Add(tick)) {
			storeEvents(at)
		}
		if !pass {
			continue
		}
		if !deployed && !at.Before(landed) {
			// the checkout took the change between its landing and this pass: its file says when
			took = landed.Add(at.Sub(landed) / 3)
			if err := os.Chtimes(filepath.Join(repo, "a.txt"), took, took); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DeployCase(ctx, c.ID, ship.pkgDir(c.ID), landed, "landed", "test"); err != nil {
				t.Fatal(err)
			}
			deployed = true
			if h.recur == "sighting" && effect && !signalOff && at.After(took) {
				// a failure after the change took effect, stored before the pass that first sees it in effect
				recurDated, recurStoreAt = took.Add(at.Sub(took)/2), at
				storeEvents(at)
			}
		}
		clock = at
		mayClose := !guardFrom.IsZero() && !at.Before(end) && effect && !signalOff && allShown(at)
		ms, err := r.Measure(ctx, Options{})
		if err != nil {
			t.Fatal(err)
		}
		why := ""
		for _, x := range ms {
			if x.Status == "failed" {
				t.Fatalf("+%s: the measure step failed: %s", at.Sub(plan0), x.Why)
			}
			why += x.Status + ": " + x.Why + " | "
		}
		got, err := st.CaseByID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Reason == store.Improved {
			t.Fatalf("+%s: closed as improved", at.Sub(plan0))
		}
		if guardFrom.IsZero() && got.Status != store.CaseTesting && got.Status != store.CaseDeployed {
			p, _, _ := ship.Load(c.ID)
			guardFrom, end = p.GuardFrom, p.GuardFrom.Add(guardRetention)
			if got.Status == store.CaseMeasuring && guardFrom.IsZero() || guardFrom.After(at) {
				t.Fatalf("+%s: its watch's start %s", at.Sub(plan0), guardFrom)
			}
			if !took.IsZero() && at.Sub(landed) < 3*time.Hour && !guardFrom.Equal(took) { // seen on the pass after its landing
				t.Fatalf("+%s: its watch starts at %s, not at its file's time %s", at.Sub(plan0), guardFrom, took)
			}
			switch h.recur {
			case "during", "evidence":
				recurDated = guardFrom.Add(between(time.Hour, 29*aDay))
				recurStoreAt, recurRaw = recurDated.Add(between(0, 2*aDay)), h.recur == "during" && rng.IntN(10) < 3
			}
			if h.late {
				historyFileAt = guardFrom.Add(between(aDay, 25*aDay))
			}
		}
		switch {
		case !recurStored.IsZero(): // Acts
			if !(got.Status == store.CaseOpen || got.Status == store.CaseTesting || got.Status == store.CaseMeasuring && got.Measurement.After > 0) {
				t.Fatalf("+%s: a recurrence stored at +%s was not acted on: case %s %s (%s)", at.Sub(plan0), recurStored.Sub(plan0), got.Status, got.Reason, why)
			}
			// during the watch a live loop rolls its change back, and one that is not holds the rollback
			if h.live && got.Status == store.CaseMeasuring || !h.live && got.Status == store.CaseTesting {
				t.Fatalf("+%s: a recurrence during the watch, the loop live %v: case %s (%s)", at.Sub(plan0), h.live, got.Status, why)
			}
			if acted == "" {
				acted = string(got.Status)
			}
		case !closed && got.Status == store.CaseClosed: // Closes, not before
			if got.Reason != guardedReason || !mayClose {
				t.Fatalf("+%s: closed as %s before its sources had shown its watch (from +%s): %v", at.Sub(plan0), got.Reason, guardFrom.Sub(plan0), shown)
			}
			closed, closedAt = true, at
			if h.unused && (got.Measurement.Watched != 0 || !strings.Contains(got.Measurement.Effect, "not watched")) {
				t.Fatalf("an unused unit's close: %+v", got.Measurement)
			}
			if h.recur == "after" {
				recurDated = closedAt.Add(between(time.Hour, 3*aDay))
				if rng.IntN(2) == 0 {
					recurDated = guardFrom.Add(between(aDay, 29*aDay)) // dated in the watch, stored after its close
				}
				recurStoreAt, recurRaw = latest(recurDated, closedAt.Add(time.Minute)).Add(between(0, aDay)), rng.IntN(10) < 3
			}
		case !closed && mayClose: // Closes, by the first pass allowed
			t.Fatalf("+%s: not closed by the first pass after its sources showed its watch (from +%s): case %s (%s)", at.Sub(plan0), guardFrom.Sub(plan0), got.Status, why)
		case closed && (got.Status != store.CaseClosed || got.Reason != guardedReason):
			t.Fatalf("+%s: a guarded close changed with no recurrence: %s %s", at.Sub(plan0), got.Status, got.Reason)
		}
		if closed && h.recur == "" && at.Sub(closedAt) > 2*aDay {
			break
		}
		if !recurStored.IsZero() && at.Sub(recurStored) > 2*aDay {
			break
		}
	}
	switch {
	case h.recur != "" && recurStored.IsZero():
		t.Fatalf("no recurrence was stored (watch from +%s, closed %v)", guardFrom.Sub(plan0), closed)
	case h.recur == "" && !closed:
		t.Fatalf("never closed (watch from +%s)", guardFrom.Sub(plan0))
	}
	t.Logf("watch from +%s; closed %v at +%s; a recurrence stored at +%s acted on as %q", guardFrom.Sub(plan0), closed, closedAt.Sub(plan0), recurStored.Sub(plan0), acted)
	notes, err := st.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.Kind == store.NoteShipped {
			t.Fatalf("a shipped note: %+v", n)
		}
	}
}

// latest is the later of two times.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// A watch may start before the pass that first sees its change in effect, when the change's own file dates it: a
// failure stored before that pass and dated after that file's time is acted on at that pass, not the one after it.
func TestARecurrenceBeforeTheSightingPassIsActedOnThen(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Changed = []string{"a.txt"}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	took := m.deployed.Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), took, took); err != nil {
		t.Fatal(err)
	}
	put(t, m.st, []store.Event{toolResult("between", took.Add(30*time.Minute), "between", true)})
	m.clock = m.deployed.Add(aDay)
	got := m.pass(t, Options{})
	if p, _, _ = m.r.Ship.Load(m.c.ID); !p.GuardFrom.Equal(took) || got.Status != "held" || !strings.Contains(got.Why, "regressed") {
		t.Fatalf("the sighting pass (watch from %s, its file %s): %+v", p.GuardFrom, took, got)
	}
}

// A store whose write sequence went below a guarded close's cursor (its file restored from a copy older than the
// package, or made anew) is read once more from the watch's start: a recurrence stored then is read.
func TestAGuardedCloseReadsAStoreWhoseSequenceWentBelowItsCursor(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	closeGuarded(t, m)
	c := m.theCase(t)
	p, _, _ := m.r.Ship.Load(c.ID)
	seq, _ := m.st.Seq(context.Background())
	p.GuardSeq = seq + 1000 // as after a restore of the store file from before later writes
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.clock = c.Closed.Add(time.Hour)
	put(t, m.st, []store.Event{toolResult("back", m.clock.Add(-30*time.Minute), "back", true)})
	m.clock = m.clock.Add(time.Hour)
	if _, err := m.passes(t, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := m.theCase(t); got.Status != store.CaseOpen {
		t.Fatalf("a recurrence in a store below the cursor: case %s %s", got.Status, got.Reason)
	}
}

// A guard's exposure watched leaves out the stretches its change was out of effect: seen in effect after its first
// week's use, then out of effect for the rest of its 30 days, it closes as guarded with only the 25 hours it watched
// before that. The closing pass is the first to see the change in effect again, and a dry one says the same: the
// stretch that pass ends is left out of its copy of the package too.
func TestAGuardsExposureWatchedLeavesOutItsStretchesOutOfEffect(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	start := m.clock
	weekly(t, m.st, toolSignal.Source, "w", start)
	m.clock = start.Add(aDay)
	m.pass(t, Options{}) // in effect through the first week's use
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1")
	for _, d := range []time.Duration{2 * aDay, 10 * aDay, 29 * aDay} {
		m.clock = start.Add(d)
		m.pass(t, Options{})
	}
	run(t, m.repo, "git", "checkout", "-q", "main")
	m.clock = start.Add(30*aDay + time.Hour)
	for _, o := range []Options{{Dry: true}, {}} {
		if got := m.pass(t, o); got.Status != "guarded" || got.Result == nil || !strings.Contains(got.Result.Effect, "no recurrence in 25 session hours watched") {
			t.Fatalf("a watch out of effect for 28 of its days, dry %v: %+v %+v", o.Dry, got, got.Result)
		}
	}
}

// The case's own evidence is a recurrence too: the problem back in a form the signal does not count, which the
// detector files by the case's signature, brings the case back during the watch; and after a guarded close, evidence
// dated inside the watch, filed only then, brings it back regressed.
func TestTheCasesOwnEvidenceInTheWatchIsARecurrence(t *testing.T) {
	ctx := context.Background()
	newForm := func(start time.Time) []store.Event {
		var back []store.Event
		for j := 0; j < 6; j++ {
			e := toolResult(fmt.Sprint("new-form-", j), start.Add(10*aDay+time.Duration(j)*time.Hour), fmt.Sprint("new-form-", j), true)
			e.Summary = "Exit code 1 zz-tool-crashed: the tool failed"
			back = append(back, e)
		}
		return back
	}
	file := func(t *testing.T, m *measured, evs []store.Event) {
		t.Helper()
		put(t, m.st, evs)
		f := store.Finding{Family: "failures", Signature: "failures/v2:zz-recurring-zz-tool", Title: "the tool fails"}
		for _, e := range evs {
			f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
		}
		if _, _, err := m.st.ObserveCase(ctx, f, "detector"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("during the watch", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		m.pass(t, Options{})
		start := m.clock
		weekly(t, m.st, toolSignal.Source, "w", start)
		file(t, m, newForm(start))
		m.clock = start.Add(10*aDay + 7*time.Hour)
		if got := m.pass(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "regressed") || m.theCase(t).Measurement.After == 0 {
			t.Fatalf("the case's evidence in the watch: %+v", got)
		}
	})
	t.Run("after the close", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		start := m.clock
		closeGuarded(t, m)
		file(t, m, newForm(start))
		m.clock = m.theCase(t).Closed.Add(time.Hour)
		if _, err := m.passes(t, Options{}); err != nil {
			t.Fatal(err)
		}
		if c := m.theCase(t); c.Status != store.CaseOpen || lastLog(t, m.st, c.ID).Action != "regressed" {
			t.Fatalf("the case's evidence dated in the watch, filed after its close: %+v", c)
		}
	})
}

// loopRun is a failure of the tool in a run of the loop's own, under dir; with form, in a form the signal does not count.
func loopRun(key string, at time.Time, dir string, form bool) store.Event {
	e := toolResult(key, at, key, true)
	e.Cwd = filepath.Join(dir, "runs", key)
	if form {
		e.Summary = "Exit code 1 zz-tool-crashed: the tool failed"
	}
	return e
}

// fileEvidence files events as the case's evidence, as the detector does by its signature.
func fileEvidence(t *testing.T, m *measured, evs ...store.Event) {
	t.Helper()
	put(t, m.st, evs)
	c := m.theCase(t)
	f := store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title}
	for _, e := range evs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	if _, _, err := m.st.ObserveCase(context.Background(), f, "detector"); err != nil {
		t.Fatal(err)
	}
}

// One rule leaves the loop's own activity out of a guard, its directory known by its path as given and as its link
// resolves: the loop configured at a link, its runs recording the resolved path. Its runs' use is no exposure, its
// failures of the signal's form and of another, filed as the case's evidence too, are no recurrence during the watch,
// and after a guarded close neither its failures nor its evidence bring the case back.
func TestTheLoopsOwnActivityIsLeftOutOfAGuardByOneRule(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	real := m.r.Dir
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	m.r.Dir, m.r.Ship.Dir = link, link
	m.pass(t, Options{})
	start := m.clock
	weekly(t, m.st, toolSignal.Source, "w", start)
	var uses []store.Event // the loop's runs using the tool: no exposure
	for i := 0; i < 40; i++ {
		e := toolResult(fmt.Sprint("loop-use-", i), start.Add(time.Duration(i)*7*time.Hour), fmt.Sprint("loop-use-", i), false)
		e.Cwd = filepath.Join(real, "runs", fmt.Sprint("u", i))
		uses = append(uses, e)
	}
	put(t, m.st, uses)
	put(t, m.st, []store.Event{loopRun("loop-signal", start.Add(3*aDay), real, false)})
	fileEvidence(t, m, loopRun("loop-form", start.Add(4*aDay), real, true))
	m.clock = start.Add(4*aDay + 2*time.Hour)
	if got := m.pass(t, Options{}); got.Status == "held" || got.Status == "rolled back" {
		t.Fatalf("the loop's own runs recurred during the watch: %s", got.Why)
	}
	m.clock = start.Add(30*aDay + time.Hour)
	got := m.pass(t, Options{})
	if got.Status != "guarded" || got.Result == nil || !strings.Contains(got.Result.Effect, "no recurrence in 125 session hours watched") {
		t.Fatalf("the close, its exposure without the loop's runs: %+v %+v", got, got.Result)
	}
	put(t, m.st, []store.Event{loopRun("loop-after", m.clock.Add(time.Hour), real, false)})
	fileEvidence(t, m, loopRun("loop-after-form", m.clock.Add(2*time.Hour), real, true))
	if c := m.theCase(t); c.Status != store.CaseClosed {
		t.Fatalf("the loop's evidence filed after the close brought the case back: %s", lastLog(t, m.st, c.ID).Detail)
	}
	m.clock = m.clock.Add(3 * time.Hour)
	if ms, err := m.passes(t, Options{}); err != nil || len(ms) != 0 || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("the watch after the close counted the loop's own runs: %+v %v", ms, err)
	}
}

// One evidence rule for a guard: the case's own evidence of a loop run, filed after the close, does not bring it back
// (the reviewer's scenario A); filed during the watch, it is no recurrence, and when an old event, from before the
// watch, is filed after the close, the case stays closed: nothing counts the loop's evidence then either (scenario B). And the watch after the close reads the case's evidence on every
// pass: evidence filed after the closing pass read the store, its write sequence already at the cursor, is read on the
// next pass and brings the case back.
func TestAGuardsEvidenceIsDecidedByOneRule(t *testing.T) {
	t.Run("the loop's evidence after the close", func(t *testing.T) { // the reviewer's scenario A
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		closeGuarded(t, m)
		fileEvidence(t, m, loopRun("loop-run", m.clock.Add(time.Hour), m.r.Dir, false))
		m.clock = m.clock.Add(2 * time.Hour)
		if _, err := m.passes(t, Options{}); err != nil {
			t.Fatal(err)
		}
		if c := m.theCase(t); c.Status != store.CaseClosed {
			t.Fatalf("a loop run's failure filed after a guarded close brought the case back: %s", lastLog(t, m.st, c.ID).Detail)
		}
	})
	t.Run("the loop's evidence, then an old event after the close", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		m.pass(t, Options{})
		start := m.clock
		fileEvidence(t, m, loopRun("loop-run", start.Add(3*aDay), m.r.Dir, false))
		weekly(t, m.st, toolSignal.Source, "w", start)
		m.clock = start.Add(30*aDay + time.Hour)
		if got := m.pass(t, Options{}); got.Status != "guarded" {
			t.Fatalf("the close: %+v", got)
		}
		fileEvidence(t, m, toolResult("old", m.planAt.Add(-2*aDay), "old", true))
		m.clock = m.clock.Add(time.Hour)
		if _, err := m.passes(t, Options{}); err != nil {
			t.Fatal(err)
		}
		if c := m.theCase(t); c.Status != store.CaseClosed {
			t.Fatalf("an old event filed after the close brought the case back: %s", lastLog(t, m.st, c.ID).Detail)
		}
	})
	t.Run("between the closing read and the close", func(t *testing.T) {
		m := guarded(t, time.Hour, toolSignal.Source, nil)
		start := m.clock
		closeGuarded(t, m)
		c := m.theCase(t)
		late := toolResult("late-form", start.Add(12*aDay), "late-form", true)
		late.Summary = "Exit code 1 zz-tool-crashed: the tool failed"
		put(t, m.st, []store.Event{late})
		// the evidence row as a filing between the closing pass's read and its close leaves it: on the case, with no
		// comeback decided, since the case still measured then
		db, err := sql.Open("sqlite", m.db)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO case_evidence (case_id, ref, at, kind, summary, added) VALUES (?, ?, ?, 'tool_result', ?, ?)`, c.ID, "ev:"+late.ID(),
			late.TS.UnixMilli(), late.Summary, c.Closed.Add(-time.Second).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		p, _, _ := m.r.Ship.Load(c.ID)
		seq, _ := m.st.Seq(context.Background())
		p.GuardSeq = seq // the closing pass read the sequence after this evidence was written
		if err := m.r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		m.clock = c.Closed.Add(time.Hour)
		if _, err := m.passes(t, Options{}); err != nil {
			t.Fatal(err)
		}
		if got := m.theCase(t); got.Status != store.CaseOpen {
			t.Fatalf("evidence filed between the closing read and the close: case %s %s", got.Status, got.Reason)
		}
	})
}

// An ingest source that has read only in part since a watch's end, its failed files newer than the watch's start, is
// left out a window (the plan's) after the end: before that the watch waits for it, after it the watch closes, the
// signal's source still in its counts, and the close's own log line names it so. A pass after the 30 days whose
// signal's newest complete read started before the end waits.
func TestAGuardWaitsForItsSignalsReadPastTheEndForAWindow(t *testing.T) {
	ctx := context.Background()
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	start := m.clock
	weekly(t, m.st, toolSignal.Source, "w", start)
	end := start.Add(30 * aDay)
	if err := m.st.MarkRead(ctx, toolSignal.Source, end.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	partial := func(at time.Time) {
		t.Helper()
		if err := m.st.MarkPartialRead(ctx, toolSignal.Source, at, start.Add(5*aDay)); err != nil {
			t.Fatal(err)
		}
	}
	m.clock = end.Add(2 * time.Hour)
	partial(m.clock.Add(-10 * time.Minute))
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, toolSignal.Source) {
		t.Fatalf("a pass after the end, the signal's newest complete read before it: %+v", got)
	}
	m.clock = end.Add(6 * aDay)
	partial(m.clock.Add(-10 * time.Minute))
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("within a window of the end, read only in part: %+v", got)
	}
	m.clock = end.Add(7*aDay + time.Hour)
	partial(m.clock.Add(-10 * time.Minute))
	// left out of the close, the signal's source stays in its counts: its 125 hours of use are watched
	if got := m.measure(t, Options{}); got.Status != "guarded" || got.Result == nil || got.Result.Watched != 125 {
		t.Fatalf("a window past the end, read only in part since it: %+v %+v", got, got.Result)
	}
	log, err := m.st.CaseLog(ctx, m.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, l := range log {
		if strings.Contains(l.Detail, toolSignal.Source+", the signal's own source, kept in the counts, read only in part since the watch's end") {
			named++
			if l.Action != "closed" {
				t.Fatalf("named in a line other than the close's: %+v", l)
			}
		}
	}
	if named != 1 {
		t.Fatalf("the close's line does not name the signal's source read in part: %+v", log)
	}
}

// A guard's recurrence stored while its change is out of effect is acted on at the next pass, still out of effect. Its
// exposure watched leaves out the stretch still out of effect, from the last pass that saw the change in effect: of
// 25 hours of use before that pass, 10 after it and the recurrence's own hour, it watched 25.
func TestAGuardsRecurrenceWhileOutOfEffectIsActedOnAtTheNextPass(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	start := m.clock
	usesIn(t, m.st, toolSignal.Source, "in-", start.Add(time.Hour), 25, 5)
	m.clock = start.Add(aDay)
	m.pass(t, Options{}) // the last pass that sees it in effect
	usesIn(t, m.st, toolSignal.Source, "away-", start.Add(aDay+time.Hour), 10, 5)
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1")
	m.clock = start.Add(2 * aDay)
	if got := m.pass(t, Options{}); got.Status != "waiting" {
		t.Fatalf("out of effect: %+v", got)
	}
	put(t, m.st, []store.Event{toolResult("again", start.Add(2*aDay+time.Hour), "again", true)})
	m.clock = start.Add(2*aDay + 2*time.Hour)
	if got := m.pass(t, Options{}); got.Status != "held" || got.Result == nil || got.Result.Watched != 25 || !strings.Contains(got.Why, "1 of 25 session hours watched") {
		t.Fatalf("a recurrence while out of effect, at the next pass: %+v %+v", got, got.Result)
	}
}

// A deployed guard's change not in effect yet: case show says its watch starts once it is, with the incidents and
// exposure its plan was proposed with.
func TestADeployedGuardShowsTheWatchItWillStart(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1")
	m.clock = m.deployed.Add(time.Hour)
	if got := m.pass(t, Options{}); got.Status != "waiting" {
		t.Fatalf("before it is in effect: %+v", got)
	}
	if out := shown(t, m.theCase(t)); !strings.Contains(out, "its watch starts once its change is in effect") || !strings.Contains(out, "after 4 incidents in 684 units") {
		t.Fatalf("a deployed guard's case show:\n%s", out)
	}
}

// A watch whose change was out of effect through all of its unit's use says so: it watched nothing because the change
// was out of effect, not because the unit was not used.
func TestAWatchOutOfEffectThroughItsUseSaysWhy(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	start := m.clock
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1")
	m.clock = start.Add(time.Hour)
	m.pass(t, Options{})
	weekly(t, m.st, toolSignal.Source, "w", start.Add(2*time.Hour))
	m.clock = start.Add(29 * aDay)
	m.pass(t, Options{})
	run(t, m.repo, "git", "checkout", "-q", "main")
	m.clock = start.Add(30*aDay + time.Hour)
	got := m.pass(t, Options{})
	if got.Status != "guarded" || got.Result == nil || !strings.Contains(got.Result.Effect, "not watched: the change was out of effect while its unit was used") {
		t.Fatalf("a watch out of effect through its use: %+v %+v", got, got.Result)
	}
	c := m.theCase(t)
	if s := evidenceview.StatusLine(c); !strings.Contains(s, "out of effect") || !strings.Contains(shown(t, c), "out of effect") {
		t.Fatalf("its views: %q %s", s, shown(t, c))
	}
}
