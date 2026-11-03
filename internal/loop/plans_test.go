package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

var incidentSignal = Signal{Source: store.Incidents, Kinds: []string{"incident"}, Match: []string{"synthetic failure"}}

// incident is an incident the incident reader stored: of a session, or, with none, of the machine.
func incident(key string, at time.Time, session string) store.Event {
	return store.Event{Source: store.Incidents, SourceKey: key, TS: at, Session: session, Actor: store.ActorDetector, Kind: "incident", Summary: "synthetic failure of a unit"}
}

// ordinary is activity of a reader: an event of the machine, or of a session.
func ordinary(source, key string, at time.Time, session string) store.Event {
	return store.Event{Source: source, SourceKey: key, TS: at, Session: session, Actor: store.ActorDetector, Kind: "ordinary", Summary: "ordinary activity"}
}

// incidentCase is a case in testing about the incidents of evs: its evidence is each of them.
func incidentCase(t *testing.T, st *store.Store, evs []store.Event) store.Case {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:units2", Title: "a unit fails"}
	for _, e := range evs {
		if e.Kind == "incident" {
			f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
		}
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "a mechanism", "a test"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	return c
}

// A plan is counted in the units its signal counts at the boundary the store has read to. When a reader that is behind
// moves the boundary, and the incidents there are the machine's, not a session's, the sources are those of the machine's
// hours; and a reader that began inside that span is left out there as it is anywhere, rather than hold the plan for a
// read it has not finished. A signal of the machine's own events, counted so from the start, waits on the readers of
// the machine's hours at once.
func TestAPlanCountsInTheUnitsAndSourcesOfItsBoundary(t *testing.T) {
	ctx := context.Background()
	day := 24 * time.Hour
	now := time.Now()
	// n hours of the machine, or of one session each, from a time: one incident each
	run := func(prefix string, from time.Time, n int, session bool) []store.Event {
		var evs []store.Event
		for i := 0; i < n; i++ {
			s := ""
			if session {
				s = fmt.Sprint(prefix, i)
			}
			evs = append(evs, incident(fmt.Sprint(prefix, i), from.Add(time.Duration(i)*2*time.Hour), s))
		}
		return evs
	}
	history := incident("history", now.Add(-100*day), "")
	history.Kind = "history"
	t.Run("the boundary moves what the signal counts in", func(t *testing.T) {
		st := testStore(t)
		evs := append(run("recent", now.Add(-6*day), 6, true), run("old", now.Add(-9*day), 6, false)...) // the gate's, and the window before it
		evs = append(evs, run("base", now.Add(-30*day), 12, false)...)
		evs = append(evs, history, ordinary("zz-slow", "recent", now.Add(-2*day), "s1"), ordinary("zz-slow", "old", now.Add(-90*day), "s1"),
			ordinary("zz-begun", "a", now.Add(-15*day), ""), ordinary("zz-begun", "b", now.Add(-14*day), ""))
		c := incidentCase(t, st, evs)
		for source, at := range map[string]time.Time{store.Incidents: now.Add(time.Hour), "zz-slow": now.Add(-3 * day)} { // the slow reader has read to three days ago
			if err := st.MarkRead(ctx, source, at); err != nil {
				t.Fatal(err)
			}
		}
		s := Ship{Store: st}
		plan, err := s.validPlan(ctx, c, incidentSignal, 7*day, "", now)
		t.Logf("%+v %v", plan, err)
		if err != nil || plan.Units != MachineHours || !plan.At.Equal(now.Add(-3*day).Add(-time.Millisecond).Truncate(time.Millisecond)) {
			t.Fatalf("the plan at the boundary the slow reader gives: %+v %v", plan, err)
		}
		if strings.Join(plan.Sources, ",") != store.Incidents || strings.Join(plan.Omitted, ",") != "zz-begun" {
			t.Fatalf("sources %v, left out %v", plan.Sources, plan.Omitted)
		}
	})
	t.Run("a signal of the machine waits on the readers of its hours at once", func(t *testing.T) {
		st := testStore(t)
		evs := append(run("recent", now.Add(-6*day), 6, false), run("base", now.Add(-30*day), 12, false)...)
		evs = append(evs, history, ordinary(store.Capacity, "recent", now.Add(-2*day), ""), ordinary(store.Capacity, "old", now.Add(-90*day), ""))
		c := incidentCase(t, st, evs)
		for source, at := range map[string]time.Time{store.Incidents: now.Add(time.Hour), store.Capacity: now.Add(-2 * day)} {
			if err := st.MarkRead(ctx, source, at); err != nil {
				t.Fatal(err)
			}
		}
		plan, err := (&Ship{Store: st}).validPlan(ctx, c, incidentSignal, 7*day, "", now)
		if err != nil || plan.Units != MachineHours || !plan.At.Equal(now.Add(-2*day).Add(-time.Millisecond).Truncate(time.Millisecond)) ||
			strings.Join(plan.Sources, ",") != "capacity,incidents" {
			t.Fatalf("the plan at the capacity runs' boundary: %+v %v", plan, err)
		}
	})
}

// A reader whose only events fall inside the span has begun there: it adds activity the earlier windows held in part
// or not at all, so it is left out of both sides, and it holds the plan for no read, finished or not.
func TestAReaderThatBeganInsideTheSpanHoldsNoProposal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	if _, err := st.Put(ctx, []store.Event{ordinary("zz-new-reader", "first", now.Add(-3*24*time.Hour), "s1")}); err != nil {
		t.Fatal(err)
	}
	plan, err := (&Ship{Store: st}).validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now) // it has finished no read
	if err != nil || strings.Contains(strings.Join(plan.Sources, ","), "zz-new-reader") || strings.Join(plan.Omitted, ",") != "zz-new-reader" {
		t.Fatalf("a reader begun inside the span: %+v %v", plan, err)
	}
}

// A plan whose signal's own source began inside its span is too new to measure: the baseline lacks the weeks before
// the source began, and its share is low by what it missed. It is refused, not left out as another reader is.
func TestAPlanWhoseSignalBeganInsideItsSpanIsTooNew(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	// testingCase gives the transcripts a past of years; this signal's source has none
	s := Ship{Store: st}
	var evs []store.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, store.Event{Source: "zz-fresh", SourceKey: fmt.Sprint(i), TS: now.Add(-time.Duration(i+1) * time.Hour), Session: fmt.Sprint("f", i), Harness: "claude",
			Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`})
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, "zz-fresh", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	fresh := goodSignal
	fresh.Source = "zz-fresh"
	if _, err := s.validPlan(ctx, c, fresh, 7*24*time.Hour, "", now); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "began inside the 28d before") {
		t.Fatalf("a signal's source that began inside the span: %v", err)
	}
	if _, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now); err != nil {
		t.Fatalf("a source that has run for years: %v", err)
	}
}

// The pick passes a case over for a proposal that waits, and only for one: the measure step sets a wait of its own on a
// measuring case, which a rollback by hand leaves behind, and it holds a case in testing for nothing.
func TestAMeasurementsWaitDoesNotHoldACaseRolledBackByHand(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	m.measure(t, Options{})
	m.clock = m.clock.Add(25 * time.Hour) // the window after has ended, and the transcripts reader is behind
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "has not read") {
		t.Fatalf("the measurement: %+v", got)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	if p.ReadWait.IsZero() || !p.ProposalAt.IsZero() {
		t.Fatalf("the measurement's wait: %+v", p)
	}
	// the owner rolls the change back by hand: its package rolled back, its case back to testing. (The fixture measures over a
	// day; a plan counted again for a proposal is of whole weeks.)
	p.State, p.Plan.Window = RolledBack, 7*24*time.Hour
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.RollBackCase(ctx, m.c.ID, "by hand", "owner"); err != nil {
		t.Fatal(err)
	}
	picked, step, why, err := m.r.pick(ctx, "failures", "", false)
	if err != nil || picked.ID != m.c.ID || step != StepBuild {
		t.Fatalf("picked %q %s: %s %v", picked.ID, step, why, err)
	}
}

// What a waiting proposal waits on is what a proposal would count now: a reader that has left the span holds nothing,
// though the plan that was refused named it.
func TestAWaitingProposalIsPassedOverOnlyForWhatItWaitsOnNow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := measurementRunner(t, st)
	c := testingCase(t, st)
	now := time.Now()
	r.Now = func() time.Time { return now }
	// a reader that last had activity two months ago, and has read nothing since
	if _, err := st.Put(ctx, []store.Event{ordinary("zz-gone", "a", now.Add(-60*24*time.Hour), "s1"), ordinary("zz-gone", "b", now.Add(-90*24*time.Hour), "s1")}); err != nil {
		t.Fatal(err)
	}
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", State: Checked, ProposalAt: now.Add(-time.Hour), ReadWait: now.Add(-time.Hour), ReadUntil: now.Add(-time.Hour),
		Plan: &Plan{Signal: goodSignal, Window: 7 * 24 * time.Hour, Units: SessionHours, At: now.Add(-time.Hour), Sources: []string{store.Transcripts, "zz-gone"}}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if picked, step, why, err := r.pick(ctx, c.Family, "", false); err != nil || picked.ID != c.ID || step != StepBuild {
		t.Fatalf("a reader out of the span held the case: picked %q %s: %s %v", picked.ID, step, why, err)
	}
}

// A proposal that waits names the readers that are behind, and only them: a reader that has read to within a window of
// the boundary holds nothing back, and the note a window of waiting raises says the same.
func TestAWaitingProposalNamesOnlyTheReadersBehind(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st) // the transcripts are read
	now := time.Now()
	if _, err := st.Put(ctx, []store.Event{ordinary("zz-reader", "recent", now.Add(-2*time.Hour), "s1"), ordinary("zz-reader", "old", now.Add(-60*24*time.Hour), "s1"),
		ordinary("zz-near", "recent", now.Add(-2*time.Hour), "s1"), ordinary("zz-near", "old", now.Add(-60*24*time.Hour), "s1")}); err != nil {
		t.Fatal(err)
	}
	for source, at := range map[string]time.Time{"zz-reader": now.Add(-30 * 24 * time.Hour), "zz-near": now.Add(-time.Hour)} {
		if err := st.MarkRead(ctx, source, at); err != nil {
			t.Fatal(err)
		}
	}
	_, err := (&Ship{Store: st}).validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	var wait *waitingPlan
	if !errors.As(err, &wait) || strings.Join(wait.sources, ",") != "zz-reader" || wait.title() != "the store is behind on zz-reader" {
		t.Fatalf("a proposal that waits on one reader: %v", err)
	}
}
