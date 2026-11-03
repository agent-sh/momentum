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

// evidenceCase is a case in testing about a signal that counts eight session hours in the week before at (and six in the
// baseline before that), whose evidence is some of those events and some others that it does not count: some of the
// first kind, and some of the second in the week, after it and before it.
func evidenceCase(t *testing.T, at time.Time, counted, otherInWindow, otherAfter, otherBefore int) (*Ship, store.Case) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedHistory(t, st)
	event := func(key string, when time.Time, text string) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: key, TS: when, Session: key, Harness: "claude", Actor: store.ActorTool, Kind: "tool_result",
			Tool: "Bash", Summary: text, Meta: `{"is_error":true}`}
	}
	var evs, evidence []store.Event
	for i := 0; i < 8; i++ {
		e := event(fmt.Sprint("counted", i), at.Add(-time.Duration(i+1)*time.Hour), "Exit code 1 zz-signal-text: the thing broke")
		evs = append(evs, e)
		if i < counted {
			evidence = append(evidence, e)
		}
	}
	for i := 0; i < 6; i++ {
		evs = append(evs, event(fmt.Sprint("older", i), at.Add(-8*24*time.Hour-time.Duration(i)*time.Hour), "Exit code 1 zz-signal-text: the thing broke"))
	}
	for i := 0; i < otherInWindow; i++ {
		e := event(fmt.Sprint("other", i), at.Add(-time.Duration(i+1)*time.Hour-30*time.Minute), "Exit code 1 something else")
		evs, evidence = append(evs, e), append(evidence, e)
	}
	for i := 0; i < otherBefore; i++ {
		e := event(fmt.Sprint("before", i), at.Add(-7*24*time.Hour-time.Duration(i+1)*time.Minute), "Exit code 1 something else")
		evs, evidence = append(evs, e), append(evidence, e)
	}
	for i := 0; i < otherAfter; i++ {
		e := event(fmt.Sprint("after", i), at.Add(time.Duration(i+1)*time.Minute), "Exit code 1 something else")
		evs, evidence = append(evs, e), append(evidence, e)
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, store.Transcripts, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:zz", Title: "the thing breaks"}
	for _, e := range evidence {
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
	return &Ship{Store: st}, c
}

// A signal measures the case's problem when it counts at least half of the case's events in the window before the plan,
// and only the events in that window: the half is not a third, and an event of the case after the plan's time is not in
// the window it is counted against.
func TestASignalMustCountHalfOfTheCasesEventsInTheWindowBeforeThePlan(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-2 * time.Hour)
	for _, c := range []struct {
		name                                            string
		counted, otherInWindow, otherAfter, otherBefore int
		refused                                         bool
	}{
		{"all of them", 4, 0, 0, 0, false},
		{"exactly half", 2, 2, 0, 0, false},
		{"more than half", 3, 2, 0, 0, false},
		{"two of five: less than half, more than a third", 2, 3, 0, 0, true},
		{"one of four", 1, 3, 0, 0, true},
		{"the case's events after the plan are not in the window", 3, 0, 4, 0, false},
		{"nor are those before it", 3, 0, 0, 4, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, cs := evidenceCase(t, at, c.counted, c.otherInWindow, c.otherAfter, c.otherBefore)
			_, err := s.validPlan(ctx, cs, goodSignal, 7*24*time.Hour, "", at)
			switch {
			case c.refused && (!errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "does not measure the case's problem")):
				t.Fatalf("the signal counts %d of %d: %v", c.counted, c.counted+c.otherInWindow, err)
			case !c.refused && err != nil:
				t.Fatalf("the signal counts %d of %d: %v", c.counted, c.counted+c.otherInWindow, err)
			}
		})
	}
}

// The step decides when the window after the change has ended, and not before: a run an hour or a minute short of it
// waits, whatever the sources have read, and the run at its end decides. The same holds for the second window of a
// quiet one.
func TestTheStepDecidesAtTheEndOfTheWindowAndNotBefore(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprint("quiet ", quiet), func(t *testing.T) {
			m := measuredCase(t)
			m.measure(t, Options{})
			start := m.clock
			if quiet {
				hours(t, m.st, "a", start.Add(time.Minute), 23*time.Hour, 60, 0)   // too quiet to tell: it waits one more window
				hours(t, m.st, "b", start.Add(25*time.Hour), 22*time.Hour, 100, 0) // and the second adds the activity
			} else {
				hours(t, m.st, "a", start.Add(time.Minute), 23*time.Hour, 100, 0)
			}
			end := start.Add(24 * time.Hour)
			if quiet {
				m.clock = end.Add(time.Minute)
				if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "too quiet") {
					t.Fatalf("a quiet first window: %+v", got)
				}
				end = start.Add(48 * time.Hour)
			}
			for _, before := range []time.Duration{time.Hour, 45 * time.Minute, 30 * time.Minute, time.Minute} {
				m.clock = end.Add(-before)
				if got := m.measure(t, Options{}); got.Status != "waiting" || got.Result != nil || m.theCase(t).Status != store.CaseMeasuring {
					t.Fatalf("%v before the window ends: %+v", before, got)
				}
			}
			m.clock = end
			if got := m.measure(t, Options{}); got.Status != "improved" || got.Result == nil {
				t.Fatalf("at the window's end: %+v", got)
			}
		})
	}
}
