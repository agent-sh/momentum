package loop

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
// guard plan of four bursts, lands its change within three days, and runs the measure step every three hours while the
// machine is awake. Its sources: the signal's (an ingest source, whose scans now and then read in part, which a rebuild
// may drop and read again, and to which an older build writes some events without a write sequence), the sessions scan
// (a command source, read every twelve minutes while awake: a night or days asleep leave one incomplete run, and it may
// stop for good), one switched off at some point, and one that began inside the plan's span. The change may go out of
// effect for a while. A recurrence, in some histories, is dated in the watch or after its close and stored up to two
// days late. On every history:
//   - Closes: with no recurrence stored, the case closes as guarded on the first pass at which its change is in effect,
//     the watch's 30 days are over, and each live source of its plan has read past their end, and not before;
//   - Acts: a stored recurrence brings the case back, or holds its rollback with the case marked recurred, on the first
//     pass after it is stored;
//   - Never improved: the case never closes as improved, and no shipped note is put.
func TestTheGuardsLivenessOverSimulatedHistories(t *testing.T) {
	for i, kind := range []string{"quiet", "quiet", "quiet", "quiet", "during", "during", "during", "after", "after", "after", "quiet", "during"} {
		seed := uint64(8000 + i)
		t.Run(fmt.Sprint(kind, "-", seed), func(t *testing.T) {
			t.Parallel()
			simulateGuard(t, seed, kind)
		})
	}
}

func simulateGuard(t *testing.T, seed uint64, kind string) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(seed, 80))
	between := func(lo, hi time.Duration) time.Duration { return lo + time.Duration(rng.Int64N(int64(hi-lo))) }
	const lag, tick, sessionsWindow = 15 * time.Minute, 12 * time.Minute, 7 * aDay
	st, dbPath := testStoreAt(t)
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	var clock time.Time
	otherOff := false
	r := &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets, Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}
	r.Now = func() time.Time { return clock }
	r.Ship.Now = r.Now
	r.Ship.SourceLive = func(source string) bool { return source != "zz-other" || !otherOff }
	plan0 := time.Now().Add(-365 * aDay).Truncate(time.Hour)
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
	var history []store.Event
	for _, source := range []string{toolSignal.Source, store.Sessions, "zz-other"} {
		history = append(history, other(source, source+"-began", plan0.Add(-60*aDay)))
	}
	history[1] = phase("sessions-began", plan0.Add(-60*aDay))
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
	read := map[string]time.Time{}
	type partialRun struct{ started, failed time.Time }
	var partials []partialRun
	markRead := func(source string, at time.Time) {
		t.Helper()
		if err := st.MarkRead(ctx, source, at); err != nil {
			t.Fatal(err)
		}
		read[source] = at
		if source == toolSignal.Source {
			partials = slices.DeleteFunc(partials, func(p partialRun) bool { return !p.started.After(at) })
		}
	}
	for _, source := range []string{toolSignal.Source, "zz-other", "zz-late"} {
		markRead(source, plan0.Add(-10*time.Minute))
	}
	if err := st.MarkRunRead(ctx, store.Sessions, "claude", plan0.Add(-10*time.Minute), lag); err != nil {
		t.Fatal(err)
	}
	markRead(store.Sessions, plan0.Add(-10*time.Minute-lag))
	c := testingCaseOf(t, st, fmt.Sprint("failures/v2:zz-sim-", seed), failures)
	plan, err := r.Ship.validPlan(ctx, c, toolSignal, 7*aDay, "", plan0)
	if g, _ := guardOf(t, plan); err != nil || g == nil {
		t.Fatalf("the proposal's plan: %+v %v", plan, err)
	}
	if !slices.Contains(plan.Sources, store.Sessions) || !slices.Contains(plan.Sources, "zz-other") || slices.Contains(plan.Sources, "zz-late") {
		t.Fatalf("the plan's sources: %v, left out %v", plan.Sources, plan.Omitted)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", repo)
	write(t, filepath.Join(repo, "a.txt"), "a\n")
	run(t, repo, "git", "add", "-A")
	run(t, repo, "git", "commit", "-q", "-m", "before the change")
	write(t, filepath.Join(repo, "a.txt"), "b\n")
	run(t, repo, "git", "commit", "-q", "-am", "the change")
	head := strings.TrimSpace(run(t, repo, "git", "rev-parse", "HEAD"))
	landed := plan0.Add(between(6*time.Hour, 3*aDay))
	if err := r.Ship.save(&Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: repo, PR: "https://github.com/agent-sh/fake/pull/7", State: Landed,
		Landed: []string{head}, LandedAt: landed, Plan: &plan}); err != nil {
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
	asleep := func(at time.Time) bool {
		return slices.ContainsFunc(sleeps, func(s [2]time.Time) bool { return !at.Before(s[0]) && at.Before(s[1]) })
	}
	var outOfEffect [2]time.Time
	if rng.IntN(10) < 6 {
		from := landed.Add(between(aDay, 25*aDay))
		outOfEffect = [2]time.Time{from, from.Add(between(2*time.Hour, 2*aDay))}
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
	t.Logf("landed +%s, asleep %v, out of effect %v, other off %v, sessions stop %v, rebuild %v", landed.Sub(plan0), sleeps, outOfEffect, otherStop, sessionsStop, rebuildAt)

	var pending []store.Event
	raw := map[string]bool{}
	var guardFrom, end, closedAt, recurDated, recurStoreAt, recurStored time.Time
	recurRaw, deployed, effect, woke, closed := false, false, true, false, false
	storeEvents := func(at time.Time) {
		t.Helper()
		if !recurStoreAt.IsZero() && recurStored.IsZero() && !at.Before(recurStoreAt) {
			e := toolResult("recurrence", recurDated, "recurrence", true)
			pending, raw[e.ID()], recurStored = append(pending, e), recurRaw, at
			t.Logf("a recurrence dated %s stored at %s", recurDated.Sub(plan0), at.Sub(plan0))
		}
		var evs []store.Event
		for _, e := range pending {
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
		for _, e := range pending {
			if e.Source == toolSignal.Source {
				signalStored = append(signalStored, e)
			}
		}
		pending = nil
		if !rebuildAt.IsZero() && !at.Before(rebuildAt) { // the signal's source dropped and read again
			if _, err := st.Rebuild(ctx, toolSignal.Source, false); err != nil {
				t.Fatal(err)
			}
			put(t, st, signalStored)
			rebuildAt = time.Time{}
			partials = nil
			delete(read, toolSignal.Source) // a rebuild drops the source's reads with its events
		}
		if rng.IntN(100) < 8 { // a scan of the signal's source that read in part
			failed := at.Add(-time.Hour)
			if rng.IntN(2) == 0 {
				failed = landed.Add(-aDay)
			}
			if err := st.MarkPartialRead(ctx, toolSignal.Source, at, failed); err != nil {
				t.Fatal(err)
			}
			partials = append(partials, partialRun{at, failed})
		} else {
			markRead(toolSignal.Source, at)
		}
		if !otherOff {
			markRead("zz-other", at)
		}
		markRead("zz-late", at)
		if sessionsStop.IsZero() || at.Before(sessionsStop) {
			if err := st.MarkHarnessRead(ctx, store.Sessions, "claude", at.Add(-lag)); err != nil {
				t.Fatal(err)
			}
			markRead(store.Sessions, at.Add(-lag))
		}
	}
	readPast := func(source string) time.Time {
		at := read[source]
		if source == toolSignal.Source {
			for _, p := range partials {
				if p.failed.Before(guardFrom) && p.started.After(at) {
					at = p.started
				}
			}
		}
		return at
	}
	mayClose := func(at time.Time) bool {
		if guardFrom.IsZero() || at.Before(end) || !effect {
			return false
		}
		for _, source := range plan.Sources {
			switch {
			case source == "zz-other" && otherOff:
			case source == store.Sessions && at.Sub(read[source]) > sessionsWindow: // stopped, left out
			case !readPast(source).After(end):
				return false
			}
		}
		return true
	}
	horizon := plan0.Add(55 * aDay)
	for n, at := 0, plan0; at.Before(horizon); n, at = n+1, at.Add(tick) {
		if asleep(at) {
			woke = true
			continue
		}
		if !otherStop.IsZero() && !at.Before(otherStop) {
			otherOff = true
		}
		if want := outOfEffect[0].IsZero() || at.Before(outOfEffect[0]) || !at.Before(outOfEffect[1]); want != effect {
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
		}
		woke = false
		if at.Minute() == 0 {
			for _, e := range hourly(at, hour) {
				switch {
				case e.Source == "zz-other" && otherOff, e.Source == store.Sessions && !sessionsStop.IsZero() && !at.Before(sessionsStop):
					continue
				case e.Source == toolSignal.Source && rng.IntN(10) == 0:
					raw[e.ID()] = true // an older build's write
				}
				pending = append(pending, e)
			}
			hour++
		}
		pass := n%15 == 0
		if pass || asleep(at.Add(tick)) {
			storeEvents(at)
		}
		if !pass {
			continue
		}
		if !deployed && !at.Before(landed) {
			if _, err := st.DeployCase(ctx, c.ID, r.Ship.pkgDir(c.ID), landed, "landed", "test"); err != nil {
				t.Fatal(err)
			}
			deployed = true
		}
		clock = at
		allowed := mayClose(at)
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
		if guardFrom.IsZero() && got.Status == store.CaseMeasuring {
			p, _, _ := r.Ship.Load(c.ID)
			guardFrom, end = p.GuardFrom, p.GuardFrom.Add(guardRetention)
			if kind == "during" {
				recurDated = guardFrom.Add(between(time.Hour, 29*aDay))
				recurStoreAt, recurRaw = recurDated.Add(between(0, 2*aDay)), rng.IntN(10) < 3
			}
		}
		switch {
		case !recurStored.IsZero(): // Acts
			if !(got.Status == store.CaseOpen || got.Status == store.CaseMeasuring && got.Measurement.After > 0) {
				t.Fatalf("+%s: a recurrence stored at +%s was not acted on: case %s %s (%s)", at.Sub(plan0), recurStored.Sub(plan0), got.Status, got.Reason, why)
			}
		case !closed && got.Status == store.CaseClosed: // Closes, not before
			if got.Reason != guardedReason || !allowed {
				t.Fatalf("+%s: closed as %s before its sources allowed it (watch from +%s)", at.Sub(plan0), got.Reason, guardFrom.Sub(plan0))
			}
			closed, closedAt = true, at
			if kind == "after" {
				recurDated = closedAt.Add(between(time.Hour, 3*aDay))
				if rng.IntN(2) == 0 {
					recurDated = guardFrom.Add(between(aDay, 29*aDay)) // dated in the watch, stored after its close
				}
				recurStoreAt, recurRaw = latest(recurDated, closedAt.Add(time.Minute)).Add(between(0, aDay)), rng.IntN(10) < 3
			}
		case !closed && allowed: // Closes, on the first pass allowed
			t.Fatalf("+%s: not closed on the first pass its sources allowed (watch from +%s): case %s (%s)", at.Sub(plan0), guardFrom.Sub(plan0), got.Status, why)
		case closed && (got.Status != store.CaseClosed || got.Reason != guardedReason):
			t.Fatalf("+%s: a guarded close changed with no recurrence: %s %s", at.Sub(plan0), got.Status, got.Reason)
		}
		if closed && kind == "quiet" && at.Sub(closedAt) > 2*aDay {
			break
		}
		if !recurStored.IsZero() && at.Sub(recurStored) > 2*aDay {
			break
		}
	}
	switch {
	case kind != "quiet" && recurStored.IsZero():
		t.Fatalf("no recurrence was stored (watch from +%s, closed %v)", guardFrom.Sub(plan0), closed)
	case kind == "quiet" && !closed:
		t.Fatalf("never closed (watch from +%s)", guardFrom.Sub(plan0))
	}
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
