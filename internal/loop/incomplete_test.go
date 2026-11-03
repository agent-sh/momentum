package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions/state"
)

// wavesSignal counts the waits of the claude harness that the sessions scan writes.
var wavesSignal = Signal{Source: store.Sessions, Kinds: []string{"phase"}, Harness: "claude", Match: []string{"waited on"}}

// sessionsPlan makes a measured case's plan a waits plan of the claude harness: 18 of 300 session hours before.
func sessionsPlan(t *testing.T, m *measured) {
	t.Helper()
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Signal, p.Plan.Before, p.Plan.BeforeBase = wavesSignal, 18, 300
	p.Plan.Sources = []string{store.Sessions, store.Transcripts}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
}

// waitsOf puts, for the first n of the session hours that hours put (the transcripts' results of those sessions), the
// wait the sessions scan writes when a session leaves needs_you.
func waitsOf(t *testing.T, st *store.Store, evs []store.Event, n int) {
	t.Helper()
	var waits []store.Event
	for _, e := range evs[:n] {
		waits = append(waits, store.Event{Source: store.Sessions, SourceKey: e.Session + "@wait", TS: e.TS, Session: e.Session, Harness: "claude", Actor: store.ActorHarness,
			Kind: "phase", Summary: e.Session + ": needs_you -> working after 30m; waited on (permission) run the build?"})
	}
	if _, err := st.Put(context.Background(), waits); err != nil {
		t.Fatal(err)
	}
}

// reads records that every command and ingest source read through a time, as a scan that works does: the claude
// harness of the sessions scan too.
func reads(t *testing.T, m *measured, at time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, source := range []string{store.Transcripts, store.Sessions} {
		if err := m.st.MarkRead(ctx, source, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.st.MarkHarnessRead(ctx, store.Sessions, "claude", at); err != nil {
		t.Fatal(err)
	}
}

// failedScans records the claude harness's scan failing on every hour of a stretch, as a scan every five minutes would
// leave it.
func failedScans(t *testing.T, st *store.Store, from time.Time, hours int) time.Time {
	t.Helper()
	var last time.Time
	for h := 0; h < hours; h++ {
		last = from.Add(time.Duration(h) * time.Hour)
		if err := st.MarkIncomplete(context.Background(), store.Sessions, "claude", last); err != nil {
			t.Fatal(err)
		}
	}
	return last
}

// A scan failure shorter than a window leaves a hole in the window after: the waits that began and ended inside it
// are not in the store, and the scans after it are complete. The step must not decide on what the failure lost - six
// waits at the old share, five of them lost, would read as a fix - but start the window again after the failure, as it
// does after a gap out of effect, and then decide on a window that was read whole.
func TestAWindowAfterThatHoldsAFailedScanStartsAgainAfterIt(t *testing.T) {
	m := measuredCaseWithRead(t, false)
	sessionsPlan(t, m)
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("start: %+v", got)
	}
	start := m.clock
	// 100 session hours and six waits in the window after, the old share; the claude scan failed for 16 hours inside it, so
	// the scan wrote one of the waits
	waitsOf(t, m.st, hours(t, m.st, "after", start, 23*time.Hour, 100, 0), 1)
	failed := failedScans(t, m.st, start.Add(3*time.Hour), 16)
	m.clock = start.Add(25 * time.Hour)
	reads(t, m, m.clock) // the next complete run records the read past the window's end
	got := m.measure(t, Options{})
	t.Logf("%s: %s", got.Status, got.Why)
	if got.Status != "measuring" || got.Result != nil || !strings.Contains(got.Why, "starts again after it") {
		t.Fatalf("a window that held a failed scan was decided: %+v", got)
	}
	c := m.theCase(t)
	if c.Status != store.CaseMeasuring || !c.Deployed.Equal(failed.Add(time.Millisecond)) || !got.Due.Equal(c.Deployed.Add(24*time.Hour)) {
		t.Fatalf("the window after did not start again after the failure's last scan (%v): %+v due %v", failed, c, got.Due)
	}
	if log, err := m.st.CaseLog(context.Background(), m.c.ID); err != nil || log[len(log)-1].Action != "measuring restarted" {
		t.Fatalf("log: %+v %v", log, err)
	}
	// the next window is read whole, the problem at its old share, all six waits written: no effect, and the change is held
	waitsOf(t, m.st, hours(t, m.st, "again", c.Deployed.Add(time.Minute), 23*time.Hour, 100, 0), 6)
	m.clock = c.Deployed.Add(25 * time.Hour)
	reads(t, m, m.clock)
	got = m.measure(t, Options{})
	if got.Status != "held" || got.Result == nil || got.Result.Improved || !strings.Contains(got.Why, "no measured effect") {
		t.Fatalf("the window read whole: %+v", got)
	}
}

// scanned records the runs of the claude harness's scan, one every five minutes from a time to another, as the timer
// leaves them, with the transcripts' ingest read through the last.
func scanned(t *testing.T, m *measured, from, to time.Time) {
	t.Helper()
	ctx := context.Background()
	for at := from; !at.After(to); at = at.Add(5 * time.Minute) {
		if err := m.st.MarkRunRead(ctx, store.Sessions, "claude", at, state.Keep); err != nil {
			t.Fatal(err)
		}
		if err := m.st.MarkRead(ctx, store.Sessions, at.Add(-state.Keep)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.st.MarkRead(ctx, store.Transcripts, to); err != nil {
		t.Fatal(err)
	}
}

// A scan that did not run for part of the window after (a timer stopped, a run that gave up on the lock of a stuck one, a
// machine that was off) leaves no incomplete run of its own, and the waits that began and ended meanwhile are not in the
// store. The first complete scan after the stretch records it as one, so the window is not decided on what is missing -
// six waits at the old share, five of them lost, would read as a fix - and starts again after it, as it does after a
// scan that failed. The stopped rule only held the decision while the source was stopped.
func TestAWindowAfterThatHoldsAStoppedScansStretchStartsAgainAfterIt(t *testing.T) {
	m := measuredCaseWithRead(t, false)
	sessionsPlan(t, m)
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("start: %+v", got)
	}
	start := m.clock
	// 100 session hours in the window after, and the old share of waits (6) had the scan run all through it; the scan
	// stopped after the third hour, so it wrote one
	waitsOf(t, m.st, hours(t, m.st, "after", start, 23*time.Hour, 100, 0), 1)
	scanned(t, m, start.Add(-time.Hour), start.Add(3*time.Hour))
	m.clock = start.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "not read past the window's end") {
		t.Fatalf("the window's end, with the scan stopped: %+v", got)
	}
	m.clock = start.Add(60 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "the signal's source stopped") {
		t.Fatalf("a scan that has not run for a window: %+v", got)
	}
	// the scans resume at 61 h
	resumed := start.Add(61 * time.Hour)
	scanned(t, m, resumed, resumed.Add(5*time.Minute))
	m.clock = resumed.Add(5 * time.Minute)
	got := m.measure(t, Options{})
	t.Logf("%s: %s", got.Status, got.Why)
	if got.Status != "waiting" || got.Result != nil || !strings.Contains(got.Why, "starts again once it reads past") {
		t.Fatalf("a window that held the stretch was decided: %+v", got)
	}
	if c := m.theCase(t); c.Status != store.CaseMeasuring || c.Reason == store.Improved {
		t.Fatalf("the case closed on a window the scan did not read: %+v", c)
	}
	// once the scan has read past the stretch's end, the window starts again after it
	scanned(t, m, resumed.Add(10*time.Minute), resumed.Add(25*time.Minute))
	m.clock = resumed.Add(25 * time.Minute)
	got = m.measure(t, Options{})
	c := m.theCase(t)
	if got.Status != "measuring" || got.Result != nil || !strings.Contains(got.Why, "starts again after it") || !c.Deployed.Equal(resumed) {
		t.Fatalf("the window did not start again where the scans resumed (%v): %+v, deployed %v", resumed, got, c.Deployed)
	}
	// the next window is read whole, the problem at its old share, all six waits written: no effect, and the change is held
	waitsOf(t, m.st, hours(t, m.st, "again", resumed.Add(time.Hour), 23*time.Hour, 100, 0), 6)
	m.clock = resumed.Add(25 * time.Hour)
	scanned(t, m, resumed.Add(30*time.Minute), m.clock)
	got = m.measure(t, Options{})
	if got.Status != "held" || got.Result == nil || got.Result.Improved || !strings.Contains(got.Why, "no measured effect") {
		t.Fatalf("the window read whole: %+v", got)
	}
}

// A failure that goes on is waited out, not restarted at each run: the window starts again once the scan has read past it,
// and not before; a dry measure says so and changes nothing.
func TestAFailedScanThatGoesOnRestartsTheWindowOnceItReadsAgain(t *testing.T) {
	m := measuredCaseWithRead(t, false)
	sessionsPlan(t, m)
	m.measure(t, Options{})
	start := m.clock
	deployed := m.theCase(t).Deployed
	last := failedScans(t, m.st, start.Add(2*time.Hour), 6)
	m.clock = start.Add(10 * time.Hour)
	// the claude scan has not read past its last failed run
	if err := m.st.MarkRead(context.Background(), store.Transcripts, m.clock); err != nil {
		t.Fatal(err)
	}
	if err := m.st.MarkHarnessRead(context.Background(), store.Sessions, "claude", last.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.st.MarkRead(context.Background(), store.Sessions, last.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got := m.measure(t, Options{})
		if got.Status != "waiting" || !strings.Contains(got.Why, "starts again once it reads past") || !m.theCase(t).Deployed.Equal(deployed) {
			t.Fatalf("a failure that goes on: %+v, deployed %v", got, m.theCase(t).Deployed)
		}
	}
	reads(t, m, last.Add(time.Hour))
	if got := m.measure(t, Options{Dry: true}); got.Status != "measuring" || !m.theCase(t).Deployed.Equal(deployed) {
		t.Fatalf("a dry measure: %+v, deployed %v", got, m.theCase(t).Deployed)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" || !m.theCase(t).Deployed.Equal(last.Add(time.Millisecond)) {
		t.Fatalf("once it reads past the failure: %+v, deployed %v", got, m.theCase(t).Deployed)
	}
	// a scan of another harness that failed in the window is no business of a signal of the claude harness
	m2 := measuredCaseWithRead(t, false)
	sessionsPlan(t, m2)
	m2.measure(t, Options{})
	if err := m2.st.MarkIncomplete(context.Background(), store.Sessions, "codex", m2.clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	m2.clock = m2.clock.Add(10 * time.Hour)
	reads(t, m2, m2.clock)
	if got := m2.measure(t, Options{}); got.Status != "waiting" || strings.Contains(got.Why, "reads past") || !m2.theCase(t).Deployed.Equal(m2.deployed.Add(10*time.Minute)) {
		t.Fatalf("another harness's failed scan: %+v", got)
	}
}

// A window of the watch that holds a failed scan of a counted source says nothing about the problem's share: it breaks
// the run of windows at the old share, and the watch goes on past it, so three windows read whole bring the case back.
func TestAWatchWindowThatHoldsAFailedScanBreaksTheRun(t *testing.T) {
	ctx := context.Background()
	m := keptCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Sources = []string{store.Sessions, store.Transcripts}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	closed := m.theCase(t).Closed
	if err := m.st.MarkRead(ctx, store.Sessions, closed.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.st.MarkIncomplete(ctx, store.Sessions, "claude", closed.Add(36*time.Hour)); err != nil { // inside the second window
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		hours(t, m.st, fmt.Sprint("watch", i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, 10) // back at more than its old 6%
		m.clock = closed.Add(time.Duration(i+1) * 24 * time.Hour)
		ms, err := m.r.Measure(ctx, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if i < 4 && len(ms) != 0 {
			t.Fatalf("after window %d: %+v", i+1, ms)
		}
		if i == 4 && (len(ms) != 1 || ms[0].Status != "back") {
			t.Fatalf("three windows read whole after the hole: %+v", ms)
		}
	}
}

// A proposal cannot be counted over a span that holds a failed scan, whatever the store has read since: its boundary
// moves back before the failure, and a boundary more than a window old is a plan that waits, naming the source. A
// failure older than the span reaches holds nothing.
func TestAProposalWaitsWhileAFailedScanIsInItsSpan(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	if err := st.MarkRead(ctx, store.Sessions, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// the sessions scan has events in the span, and a past before it
	if _, err := st.Put(ctx, []store.Event{{Source: store.Sessions, SourceKey: "p", TS: now.Add(-10 * 24 * time.Hour), Session: "s0", Harness: "claude", Actor: store.ActorHarness, Kind: "phase", Summary: "s0: appeared, working"},
		{Source: store.Sessions, SourceKey: "old", TS: now.Add(-90 * 24 * time.Hour), Session: "s0", Harness: "claude", Actor: store.ActorHarness, Kind: "phase", Summary: "s0: appeared, working"}}); err != nil {
		t.Fatal(err)
	}
	s := Ship{Store: st}
	if err := st.MarkIncomplete(ctx, store.Sessions, "claude", now.Add(-40*24*time.Hour)); err != nil { // before the span's 28 days
		t.Fatal(err)
	}
	if plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now); err != nil || !strings.Contains(strings.Join(plan.Sources, ","), store.Sessions) {
		t.Fatalf("a failed scan out of the span's reach: %+v %v", plan, err)
	}
	if err := st.MarkIncomplete(ctx, store.Sessions, "claude", now.Add(-20*24*time.Hour)); err != nil { // inside it
		t.Fatal(err)
	}
	_, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	var wait *waitingPlan
	if !errors.As(err, &wait) || strings.Join(wait.sources, ",") != store.Sessions || !errors.Is(err, ErrRefused) {
		t.Fatalf("a failed scan inside the span: %v", err)
	}
}

// wavesCase is a case of the sessions family in testing about the waits of the claude harness: six in its gate and six in
// a baseline a week before, as testingCase has results, and a sessions scan that has run for years.
func wavesCase(t *testing.T, st *store.Store) store.Case {
	t.Helper()
	return wavesCaseFrom(t, st, 90*24*time.Hour, 0)
}

// wavesCaseFrom is wavesCase for a sessions scan whose first event is a time old, with a baseline of waits a week and
// a half further back too (older sessions' worth of them, none of the case's evidence).
func wavesCaseFrom(t *testing.T, st *store.Store, first time.Duration, older int) store.Case {
	t.Helper()
	ctx := context.Background()
	var evs []store.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, store.Event{Source: store.Sessions, SourceKey: fmt.Sprintf("s%d@%d", i%2, i), TS: time.Now().Add(-time.Duration(i+1) * time.Hour), Session: fmt.Sprint("s", i%2),
			Harness: "claude", Actor: store.ActorHarness, Kind: "phase", Summary: fmt.Sprintf("s%d: needs_you -> working after 40m; waited on (permission) run the build?", i%2)})
	}
	for _, e := range append([]store.Event(nil), evs...) {
		e.SourceKey += "-baseline"
		e.TS = e.TS.Add(-8 * 24 * time.Hour)
		evs = append(evs, e)
	}
	evidence := len(evs)
	for i := 0; i < older; i++ {
		e := evs[i%6]
		e.SourceKey += "-older"
		e.TS = e.TS.Add(-15*24*time.Hour - time.Duration(i)*time.Hour)
		evs = append(evs, e)
	}
	evs = append(evs, store.Event{Source: store.Sessions, SourceKey: "history", TS: time.Now().Add(-first), Session: "s0", Harness: "claude",
		Actor: store.ActorHarness, Kind: "appeared", Summary: "s0: appeared, working"})
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "sessions", Signature: "sessions/v1:waited", Title: "sessions wait on him"}
	for _, e := range evs[:min(12, evidence)] {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "Answer is off by one", "assert Answer() == 42"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	return c
}

// A failure inside the span moves a plan's boundary back before it, and the span with it. A signal's source that began
// before the proposal's own span is not too new for the plan the failure moved: it would be refused for it at each run,
// which is no wait and no change a build could make. One that began inside the proposal's own span is refused all the same.
func TestAFailureThatMovesTheBoundaryDoesNotMakeTheSignalTooNew(t *testing.T) {
	ctx := context.Background()
	day := 24 * time.Hour
	now := time.Now()
	// the sessions scan's first event is 31 days old: older than the span (28 days) at the proposal, not at a boundary 5 days back
	st := testStore(t)
	c := wavesCaseFrom(t, st, 31*day, 6)
	if err := st.MarkRead(ctx, store.Sessions, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := Ship{Store: st}
	plan, err := s.validPlan(ctx, c, wavesSignal, 7*day, "", now)
	if err != nil || !plan.At.Equal(now) {
		t.Fatalf("no failure: %+v %v", plan, err)
	}
	failed := now.Add(-5 * day)
	if err := st.MarkIncomplete(ctx, store.Sessions, "codex", failed); err != nil { // another harness's scan: no business of this signal
		t.Fatal(err)
	}
	if plan, err = s.validPlan(ctx, c, wavesSignal, 7*day, "", now); err != nil || !plan.At.Equal(now) {
		t.Fatalf("a failed codex scan: %+v %v", plan, err)
	}
	if err := st.MarkIncomplete(ctx, store.Sessions, "claude", failed); err != nil {
		t.Fatal(err)
	}
	plan, err = s.validPlan(ctx, c, wavesSignal, 7*day, "", now)
	if err != nil || !plan.At.Equal(failed.Add(-time.Millisecond).Truncate(time.Millisecond)) || plan.Gate < MinSignal {
		t.Fatalf("a failed claude scan five days ago: the plan counts before it: %+v %v", plan, err)
	}
	// a source that began inside the span at the proposal is too new, failure or none
	for _, fail := range []bool{false, true} {
		st := testStore(t)
		c := wavesCaseFrom(t, st, 20*day, 6)
		if err := st.MarkRead(ctx, store.Sessions, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if fail {
			if err := st.MarkIncomplete(ctx, store.Sessions, "claude", failed); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := (&Ship{Store: st}).validPlan(ctx, c, wavesSignal, 7*day, "", now); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "began inside the 28d before") {
			t.Fatalf("a signal's source 20 days old (a failure: %v): %v", fail, err)
		}
	}
}

// checked prepares the case's change in the fixture's repository and checks it, as a build does before it proposes.
func checked(t *testing.T, s *Ship, c store.Case, f repoFixture) {
	t.Helper()
	ctx := context.Background()
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
}

var wavesProposal = Proposal{Title: "Correct the answer", Body: "Return the expected value.", Rollback: "revert", Signal: wavesSignal, Window: 7 * 24 * time.Hour}

// A proposal refused because the signal's own source counts as stopped is a wait, as one refused for a read is: it is
// recorded with its plan and signal, and a build for it is passed over until the reader runs again. Otherwise each run
// of the loop spends a build, and the case's budget, on a refusal that cannot change.
func TestAProposalOnAStoppedSignalSourceWaitsAndIsPassedOver(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := wavesCase(t, st)
	now := time.Now()
	// the sessions scan's last complete read is eight days old: the scan has not run for a window
	if err := st.MarkRead(ctx, store.Sessions, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	checked(t, s, c, f)
	r := measurementRunner(t, st)
	r.Ship = s
	r.Inputs.ReadDirs = []string{f.main}
	setCaseRepo(t, st, c, filepath.Base(f.main))
	for i := 0; i < 2; i++ {
		p, err := proposePatch(t, s, ctx, c.ID, wavesProposal)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "the signal's source stopped: sessions") || p.State != Checked ||
			p.ProposalAt.IsZero() || p.Plan == nil || p.Plan.Signal.Source != store.Sessions || p.ReadWait.IsZero() {
			t.Fatalf("proposal %d: %+v %v", i, p, err)
		}
		picked, _, why, err := r.pick(ctx, "sessions", "", false)
		if err != nil || picked.ID != "" || !strings.Contains(why, "proposal waits on its signal's source, which stopped: sessions") {
			t.Fatalf("pick %d: picked %q: %s %v", i, picked.ID, why, err)
		}
	}
	// a window of waiting is a passive note in the report, once
	r.Now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	s.Now = r.Now
	if _, err := proposePatch(t, s, ctx, c.ID, wavesProposal); err == nil {
		t.Fatal("proposed on a stopped source")
	}
	if notes, err := st.Notes(ctx); err != nil || len(notes) != 1 || notes[0].ID != "measure:"+c.ID || !strings.Contains(notes[0].Title, "signal's source stopped") {
		t.Fatalf("notes: %+v %v", notes, err)
	}
	// its reader runs again: the case is taken for a build, and the proposal goes through
	r.Now, s.Now = nil, nil
	if err := st.MarkRead(ctx, store.Sessions, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if picked, step, _, err := r.pick(ctx, "sessions", "", false); err != nil || picked.ID != c.ID || step != StepBuild {
		t.Fatalf("once it reads: %q %s %v", picked.ID, step, err)
	}
	if p, err := proposePatch(t, s, ctx, c.ID, wavesProposal); err != nil || p.State != Proposed || !p.ProposalAt.IsZero() || !p.ReadWait.IsZero() {
		t.Fatalf("the proposal: %+v %v", p, err)
	}
	if notes, _ := st.Notes(ctx); len(notes) != 1 || notes[0].Done.IsZero() {
		t.Fatalf("the wait's note stands: %+v", notes)
	}
}

// The sessions scan and supervision are read harness by harness: a harness whose scan keeps failing stops nothing of
// another's plan and holds none of its reads, though the source as a whole has not read for a window.
func TestAHarnessThatFailsNeitherStopsNorHoldsAPlanOfAnother(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := wavesCase(t, st)
	now := time.Now()
	for _, err := range []error{
		st.MarkRead(ctx, store.Sessions, now.Add(-8*24*time.Hour)),              // the last run that read every harness
		st.MarkHarnessRead(ctx, store.Sessions, "claude", now.Add(time.Minute)), // the claude scan has worked since
		st.MarkIncomplete(ctx, store.Sessions, "codex", now.Add(-time.Hour)),    // the codex scan has not
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := Ship{Store: st}
	plan, err := s.validPlan(ctx, c, wavesSignal, 7*24*time.Hour, "", now)
	if err != nil || !strings.Contains(strings.Join(plan.Sources, ","), store.Sessions) || len(plan.Omitted) != 0 {
		t.Fatalf("a plan of the claude harness: %+v %v", plan, err)
	}
	// a signal that counts every harness does depend on all their scans
	all := wavesSignal
	all.Harness = ""
	var wait *waitingPlan
	if _, err := s.validPlan(ctx, c, all, 7*24*time.Hour, "", now); !errors.As(err, &wait) || !wait.stopped {
		t.Fatalf("a plan of every harness: %v", err)
	}
}
