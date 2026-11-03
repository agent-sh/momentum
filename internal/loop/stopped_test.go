package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/store"
)

// A command's source (a sessions scan, supervision, a capacity run) that has not finished a complete read for a whole
// window is stopped, one that never finished one but has events older than a window too; a source read within the
// window, or begun within it, is live. A scan and supervision are read for the harness a signal counts.
func TestACommandSourceThatHasNotReadForAWindowIsStopped(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	week := 7 * 24 * time.Hour
	for _, source := range []string{store.Sessions, store.Supervise, "capacity"} {
		t.Run(source, func(t *testing.T) {
			st := testStore(t)
			s := Ship{Store: st}
			stopped := func(harness string) bool {
				t.Helper()
				live, omitted := s.liveSources(ctx, []string{source}, week, now, harness)
				if len(live)+len(omitted) != 1 {
					t.Fatalf("%v %v", live, omitted)
				}
				return len(omitted) == 1
			}
			if stopped("") {
				t.Error("a source with no read and no event is not stopped")
			}
			if _, err := st.Put(ctx, []store.Event{ordinary(source, "first", now.Add(-2*24*time.Hour), "s1")}); err != nil {
				t.Fatal(err)
			}
			if stopped("") {
				t.Error("a source with events of two days and no read is not stopped")
			}
			if _, err := st.Put(ctx, []store.Event{ordinary(source, "older", now.Add(-8*24*time.Hour), "s1")}); err != nil {
				t.Fatal(err)
			}
			if !stopped("") {
				t.Error("a source that has run unread for eight days is stopped")
			}
			if err := st.MarkRead(ctx, source, now.Add(-6*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if stopped("") {
				t.Error("a source read six days ago is live")
			}
			if err := st.MarkRead(ctx, source, now.Add(-8*24*time.Hour)); err != nil { // an older read moves nothing back
				t.Fatal(err)
			}
			if stopped("") {
				t.Error("an older read moved the newer back")
			}
		})
	}
	// the harness a signal counts is the one that must have read: another's failing scan stops nothing of it
	st := testStore(t)
	s := Ship{Store: st}
	for _, err := range []error{st.MarkRead(ctx, store.Sessions, now.Add(-9*24*time.Hour)), st.MarkHarnessRead(ctx, store.Sessions, "claude", now),
		st.MarkHarnessRead(ctx, store.Sessions, "codex", now.Add(-9*24*time.Hour))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for harness, want := range map[string]bool{"claude": false, "codex": true, "": true} {
		if _, omitted := s.liveSources(ctx, []string{store.Sessions}, week, now, harness); (len(omitted) == 1) != want {
			t.Errorf("harness %q: stopped %v, want %v", harness, len(omitted) == 1, want)
		}
	}
	// a reader the machine's configuration no longer names is stopped whatever it read
	cfg := config.Sources{}
	s.SourceLive = cfg.SourceLive
	if _, omitted := s.liveSources(ctx, []string{"transcripts", "mistakes", store.Sessions}, week, now, "claude"); strings.Join(omitted, ",") != "transcripts,mistakes" {
		t.Errorf("readers no longer configured: %v", omitted)
	}
}

// A stopped signal makes no decision and raises a note; once the signal's source reads again, the note goes and the
// case is measured.
func TestAStoppedSignalsNoteGoesWhenItsSourceReadsAgain(t *testing.T) {
	m := measuredCase(t)
	m.measure(t, Options{}) // the case measures
	m.r.Ship.SourceLive = func(source string) bool { return source != store.Transcripts }
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "signal's source stopped") || len(m.notes(t)) != 1 {
		t.Fatalf("stopped: %+v, %d notes", got, len(m.notes(t)))
	}
	m.r.Ship.SourceLive = nil
	if got := m.measure(t, Options{}); got.Status != "waiting" || strings.Contains(got.Why, "stopped") || len(m.notes(t)) != 0 || m.theCase(t).Status != store.CaseMeasuring {
		t.Fatalf("running again: %+v, %d notes", got, len(m.notes(t)))
	}
}

// A baseline recounted without a stopped source is read like any window: it waits for the sources it counts to have
// read past it.
func TestARecountedBaselineWaitsForItsSourcesToBeRead(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Sources = []string{store.Transcripts, store.Supervise}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	cfg := config.Sources{}
	cfg.Transcripts.Homes = []string{"/synthetic"}
	cfg.Supervise.Off = true // supervision is switched off: the baseline is counted again without it
	m.r.Ship.SourceLive = cfg.SourceLive
	// the transcripts have read to a day before the plan: the baseline's last window ends at the plan less a window
	if err := m.st.MarkRead(ctx, store.Transcripts, p.Plan.At.Add(-2*p.Plan.Window)); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "has not read past") || m.theCase(t).Status != store.CaseDeployed {
		t.Fatalf("the recounted baseline unread: %+v", got)
	}
	if err := m.st.MarkRead(ctx, store.Transcripts, p.Plan.At); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("the recounted baseline read: %+v", got)
	}
}

// A source that stops after the watch read some windows takes the watch back to the close: it recounts every window
// since without the source, and the run of windows at the old share starts from nothing.
func TestTheWatchRecountsItsWindowsWhenASourceStops(t *testing.T) {
	ctx := context.Background()
	m := keptCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Sources = []string{store.Transcripts, store.Supervise}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	closed := m.theCase(t).Closed
	if err := m.st.MarkRead(ctx, store.Supervise, closed.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// the baseline counted again without supervision is the plan's own: 18 of 300 session hours, the old 6%
	hours(t, m.st, "independent", p.Plan.At.Add(-4*p.Plan.Window), 3*p.Plan.Window, 300, 18)
	// every window: ten of a hundred session hours hold the problem, the old share is six; but supervision doubles the
	// activity (a hundred more hours of sessions), so counted with it the share is five and the problem is not back
	var supervised []store.Event
	for i := 0; i < 5; i++ {
		hours(t, m.st, fmt.Sprint("watch", i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, 10)
		for j := 0; j < 100; j++ {
			supervised = append(supervised, ordinary(store.Supervise, fmt.Sprint("sv", i, "-", j), closed.Add(time.Duration(i)*24*time.Hour+time.Duration(j)*13*time.Minute), fmt.Sprint("sv", i, "-", j)))
		}
	}
	if _, err := m.st.Put(ctx, supervised); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // two windows read with supervision
		m.clock = closed.Add(time.Duration(i+1) * 24 * time.Hour)
		if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
			t.Fatalf("window %d: %+v %v", i+1, ms, err)
		}
	}
	if p, _, _ = m.r.Ship.Load(m.c.ID); p.WatchWindow != 2 || len(p.WatchRecent) != 2 {
		t.Fatalf("two windows read: %+v", p)
	}
	cfg := config.Sources{}
	cfg.Transcripts.Homes = []string{"/synthetic"}
	cfg.Supervise.Off = true // supervision stops: the two windows read are counted again without it, and only they are kept
	m.r.Ship.SourceLive = cfg.SourceLive
	if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("the two windows recounted: %+v %v", ms, err)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 2 || len(p.WatchRecent) != 2 || p.WatchRecent[0].Base != 100 || p.WatchRecent[1].Base != 100 || p.WatchRecent[0].Hits != 10 {
		t.Fatalf("the windows kept after the recount: %+v", p.WatchRecent)
	}
	m.clock = closed.Add(3 * 24 * time.Hour)
	ms, err := m.r.Measure(ctx, Options{})
	if err != nil || len(ms) != 1 || ms[0].Status != "back" || !strings.Contains(ms[0].Why, "window 3") {
		t.Fatalf("the windows recounted without supervision: %+v %v", ms, err)
	}
}

// A reader whose events are found only at the span a moved boundary reaches is counted there: a plan whose boundary the
// store's reads moved back is held for it as for any reader, if it has read less.
func TestAReaderFoundOnlyAtTheMovedBoundaryHoldsThePlan(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	day := 24 * time.Hour
	if _, err := st.Put(ctx, []store.Event{ordinary("zz-slow", "recent", now.Add(-2*day), "s1"), ordinary("zz-slow", "old", now.Add(-90*day), "s1"),
		ordinary("zz-late", "far", now.Add(-30*day), "s1"), ordinary("zz-late", "older", now.Add(-90*day), "s1")}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, "zz-slow", now.Add(-3*day)); err != nil { // the boundary moves back three days: the span reaches zz-late's event
		t.Fatal(err)
	}
	_, err := (&Ship{Store: st}).validPlan(ctx, c, goodSignal, 7*day, "", now)
	var wait *waitingPlan
	if !errors.As(err, &wait) || strings.Join(wait.sources, ",") != "zz-late" {
		t.Fatalf("a reader found only at the moved boundary: %v", err)
	}
	if err := st.MarkRead(ctx, "zz-late", now); err != nil {
		t.Fatal(err)
	}
	// once it has read, it holds nothing (the plan at the moved boundary is refused for what its gate holds there)
	if _, err := (&Ship{Store: st}).validPlan(ctx, c, goodSignal, 7*day, "", now); errors.As(err, &wait) {
		t.Fatalf("once it has read: %v", err)
	}
}
