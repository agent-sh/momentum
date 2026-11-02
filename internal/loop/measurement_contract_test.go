package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// A stalled transcript reader cannot turn other sources' activity, or an entirely unread span, into a decision.
// Once both sources read the same ended windows, the step can measure them and clears the passive problem note.
func TestUnreadWindowsWaitForEveryCountedSource(t *testing.T) {
	ctx := context.Background()
	for _, activity := range []bool{false, true} {
		t.Run(fmt.Sprint(activity), func(t *testing.T) {
			m := measuredCaseWithRead(t, false)
			if activity {
				p, _, _ := m.r.Ship.Load(m.c.ID)
				p.Plan.Sources = []string{store.Transcripts, store.Sessions}
				if err := m.r.Ship.save(&p); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.st.MarkRead(ctx, store.Transcripts, m.clock); err != nil {
				t.Fatal(err)
			}
			m.measure(t, Options{})
			start := m.clock
			if activity {
				var evs []store.Event
				for i := 0; i < 100; i++ {
					evs = append(evs, store.Event{Source: store.Sessions, SourceKey: fmt.Sprint(i), Session: fmt.Sprint(i), Actor: store.ActorHarness, Kind: "phase", TS: start.Add(time.Duration(i) * time.Hour / 100)})
				}
				if _, err := m.st.Put(ctx, evs); err != nil {
					t.Fatal(err)
				}
				if err := m.st.MarkRead(ctx, store.Sessions, start.Add(4*24*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range []int{1, 2, 3} {
				m.clock = start.Add(time.Duration(d)*24*time.Hour + time.Minute)
				got := m.measure(t, Options{})
				if got.Status != "waiting" || !strings.Contains(got.Why, store.Transcripts) || got.Result != nil || m.theCase(t).Status != store.CaseMeasuring {
					t.Fatalf("unread: %+v", got)
				}
			}
			ns := m.notes(t)
			if len(ns) != 1 || ns[0].ID != "measure:"+m.c.ID || !strings.Contains(ns[0].Title, "store is behind") {
				t.Fatalf("problem: %+v", ns)
			}
			// With no activity read at all, the late ingest supplies it at its original event times.
			if !activity {
				hours(t, m.st, "late", start, 23*time.Hour, 100, 0)
			}
			if err := m.st.MarkRead(ctx, store.Transcripts, m.clock); err != nil {
				t.Fatal(err)
			}
			if got := m.measure(t, Options{}); got.Status != "improved" {
				t.Fatalf("read: %+v", got)
			}
			if noteKinds(m.notes(t))[store.NoteProblem] != 0 {
				t.Fatal("read problem still stands")
			}
		})
	}
}

// A fresh signal alone is insufficient when another source contributes activity; a watch also waits without
// advancing, and only completed, read windows join its run of three.
func TestActivityAndWatchWaitForTheirSources(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Sources = []string{store.Transcripts, store.Sessions}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.measure(t, Options{})
	start := m.clock
	ev := store.Event{Source: store.Sessions, SourceKey: "phase", Session: "phase", Actor: store.ActorHarness, Kind: "phase", TS: start}
	if _, err := m.st.Put(ctx, []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	// the sessions scan reads, but not yet past the window's end: within a window of its last read it holds the decision
	if err := m.st.MarkRead(ctx, store.Sessions, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	hours(t, m.st, "after", start, 23*time.Hour, 100, 0)
	m.clock = start.Add(25 * time.Hour)
	if err := m.st.MarkRead(ctx, store.Transcripts, m.clock); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, store.Sessions) {
		t.Fatalf("activity unread: %+v", got)
	}
	if err := m.st.MarkRead(ctx, store.Sessions, m.clock); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("keep: %+v", got)
	}
	closed := m.theCase(t).Closed
	for i := 0; i < 3; i++ {
		hours(t, m.st, fmt.Sprint("return", i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, 6)
	}
	m.clock = closed.Add(3*24*time.Hour + time.Minute)
	if got := m.measure(t, Options{Dry: true}); got.Status != "waiting" {
		t.Fatalf("dry watch: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 0 || !p.ReadWait.IsZero() {
		t.Fatal("dry unread watch saved")
	}
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("watch: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 0 {
		t.Fatal("unread watch advanced")
	}
	m.clock = m.clock.Add(24 * time.Hour)
	m.measure(t, Options{})
	if noteKinds(m.notes(t))[store.NoteProblem] != 1 {
		t.Fatal("watch stall has no note")
	}
	if err := m.st.MarkRead(ctx, store.Transcripts, m.clock); err != nil {
		t.Fatal(err)
	}
	if err := m.st.MarkRead(ctx, store.Sessions, m.clock); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "back" {
		t.Fatalf("read watch: %+v", got)
	}
}

// Proposal counts require a complete read of the gate and baseline, including another source's activity.
func TestAPlanWaitsForItsBaselineSources(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	w := 7 * 24 * time.Hour
	hours(t, m.st, "baseline", m.planAt.Add(-4*w), 3*w, 100, 10)
	ev := store.Event{Source: store.Sessions, SourceKey: "baseline-phase", Session: "phase", Actor: store.ActorHarness, Kind: "phase", TS: m.planAt.Add(-2 * w)}
	first := store.Event{Source: store.Sessions, SourceKey: "first-phase", Session: "phase", Actor: store.ActorHarness, Kind: "phase", TS: m.planAt.Add(-5 * w)} // a reader from before the span
	if _, err := m.st.Put(ctx, []store.Event{ev, first}); err != nil {
		t.Fatal(err)
	}
	_, err := m.r.Ship.validPlan(ctx, m.c, goodSignal, w, "", m.planAt)
	if err == nil || !strings.Contains(err.Error(), store.Transcripts) {
		t.Fatalf("unread transcripts: %v", err)
	}
	if err := m.st.MarkRead(ctx, store.Transcripts, m.planAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// the sessions scan, a command's source, never finished a read in the five weeks since its first event: it
	// stopped, and the plan leaves it out of both sides and names it, rather than wait on it for good
	plan, err := m.r.Ship.validPlan(ctx, m.c, goodSignal, w, "", m.planAt)
	if err != nil || slices.Contains(plan.Sources, store.Sessions) || !slices.Contains(plan.Omitted, store.Sessions) {
		t.Fatalf("a stopped sessions scan: %+v %v", plan, err)
	}
	if err := m.st.MarkRead(ctx, store.Sessions, m.planAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if plan, err = m.r.Ship.validPlan(ctx, m.c, goodSignal, w, "", m.planAt); err != nil || !slices.Contains(plan.Sources, store.Sessions) {
		t.Fatalf("a sessions scan read again: %+v %v", plan, err)
	}
}

// The first observed checkout time belongs to the installed build. Later edits or branch switches cannot
// take that build out of effect, while removing the installed file still holds the measurement.
func TestAnInstalledBuildSurvivesLaterCheckoutChanges(t *testing.T) {
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	bin := filepath.Join(t.TempDir(), "tool")
	write(t, bin, "build")
	p.Plan.Installed, p.Changed = bin, []string{"a.txt"}
	if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), m.deployed, m.deployed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, m.clock, m.clock); err != nil {
		t.Fatal(err)
	}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("start: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if !p.CheckoutAt.Equal(m.deployed) {
		t.Fatalf("checkout time: %v", p.CheckoutAt)
	}
	start := m.clock
	if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), start.Add(time.Hour), start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	m.clock = start.Add(2 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || got.Why != "" {
		t.Fatalf("rewrite: %+v", got)
	}
	run(t, m.repo, "git", "checkout", "-q", "--orphan", "other")
	run(t, m.repo, "git", "commit", "-q", "--allow-empty", "-m", "another tree")
	hours(t, m.st, "after", start, 23*time.Hour, 100, 0)
	m.clock = start.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("branch switch: %+v", got)
	}
	if !m.theCase(t).Deployed.Equal(start) {
		t.Fatal("installed measurement restarted")
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if _, why, err := m.r.Ship.InEffect(context.Background(), &p); err != nil || !strings.Contains(why, "not there") {
		t.Fatalf("missing build: %s %v", why, err)
	}
}

// Plan windows are whole weeks, and both the build and review name the installed file when that is what runs.
func TestPlansAndSkillsRequireWeeksAndInstalledFiles(t *testing.T) {
	m := measuredCase(t)
	for _, w := range []time.Duration{0, 24 * time.Hour, 6 * 24 * time.Hour, 8 * 24 * time.Hour, 7*24*time.Hour + time.Hour} {
		if _, err := m.r.Ship.validPlan(context.Background(), m.c, goodSignal, w, "", m.planAt); err == nil || !strings.Contains(err.Error(), "whole number of weeks") {
			t.Fatalf("window %v: %v", w, err)
		}
	}
	for _, name := range []string{"loop", "review", "owner-burden"} {
		text, err := Skill(name)
		if err != nil {
			t.Fatal(err)
		}
		if name != "review" && (!strings.Contains(text, "a week, or two for a rarer problem") || strings.Contains(text, "shortest window")) {
			t.Fatalf("%s window guidance", name)
		}
		if name != "owner-burden" && (!strings.Contains(text, "--installed FILE") || !strings.Contains(text, "binary") || !strings.Contains(text, "installed configuration")) {
			t.Fatalf("%s omits installed file", name)
		}
	}
}

// A stray note and an unreadable package each fail only their own entry. A healthy deployed case still starts.
func TestABadMeasurementCaseDoesNotStopTheOthers(t *testing.T) {
	ctx := context.Background()
	for _, badPackage := range []bool{false, true} {
		m := measuredCase(t)
		id := "c-none"
		if badPackage {
			c, _, err := m.st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:bad-package", Title: "bad package", Evidence: []store.Evidence{{Ref: "file:/synthetic/evidence", At: m.clock}}}, "test")
			if err != nil {
				t.Fatal(err)
			}
			id = c.ID
			if err := m.r.Ship.save(&Package{Case: id, State: Kept}); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(m.r.Ship.pkgDir(id), "package.json"), "not json")
		}
		m.r.note(ctx, store.Note{ID: "measure:" + id, Kind: store.NoteProblem, Title: "a stale note"})
		ms, err := m.r.Measure(ctx, Options{})
		if err != nil || len(ms) != 2 {
			t.Fatalf("step: %+v %v", ms, err)
		}
		statuses := map[string]string{}
		for _, v := range ms {
			statuses[v.Case] = v.Status
		}
		if statuses[id] != "failed" || statuses[m.c.ID] != "measuring" {
			t.Fatalf("entries: %+v", ms)
		}
	}
}

// A git refusal says what prevented the check, rather than claiming the checkout lacks the change.
func TestAnInEffectGitErrorNamesTheFailedCheck(t *testing.T) {
	m := measuredCase(t)
	run(t, m.repo, "git", "config", "core.fsmonitor", "a-forbidden-program")
	got := m.measure(t, Options{})
	if got.Status != "waiting" || !strings.Contains(got.Why, "cannot check") || !strings.Contains(got.Why, "fsmonitor") || strings.Contains(got.Why, "does not hold") {
		t.Fatalf("git error: %+v", got)
	}
}

// Loop runs are excluded at every step that tallies: the proposal baseline, one or two windows after, and watch.
func TestLoopRunsCountInNoMeasurementWindow(t *testing.T) {
	ctx := context.Background()
	w := 24 * time.Hour
	addLoop := func(m *measured, prefix string, start time.Time, n int) {
		evs := hours(t, m.st, prefix, start, 23*time.Hour, n, n)
		for i := range evs {
			evs[i].Cwd = filepath.Join(m.r.Dir, "runs", prefix)
		}
		if _, err := m.st.Put(ctx, evs); err != nil {
			t.Fatal(err)
		}
	}
	m := measuredCase(t)
	hours(t, m.st, "baseline", m.planAt.Add(-14*w), w, 100, 6)
	addLoop(m, "baseline-loop", m.planAt.Add(-15*w), 100)
	p, err := m.r.Ship.validPlan(ctx, m.c, goodSignal, 7*w, "", m.planAt)
	if err != nil || p.Before != 6 || p.BeforeBase != 100 {
		t.Fatalf("baseline: %+v %v", p, err)
	}
	for _, two := range []bool{false, true} {
		m := measuredCase(t)
		m.measure(t, Options{})
		start := m.clock
		n := 100
		if two {
			n = 60
		}
		hours(t, m.st, "after", start, 23*time.Hour, n, 0)
		addLoop(m, "first-loop", start, 1000)
		m.clock = start.Add(25 * time.Hour)
		got := m.measure(t, Options{})
		if two {
			if got.Status != "waiting" {
				t.Fatalf("quiet first: %+v", got)
			}
			hours(t, m.st, "second", start.Add(w), 23*time.Hour, 40, 0)
			addLoop(m, "second-loop", start.Add(w), 1000)
			m.clock = start.Add(49 * time.Hour)
			got = m.measure(t, Options{})
		}
		if got.Status != "improved" || got.Result.After != (measure.Counts{Hits: 0, Base: 100}) {
			t.Fatalf("after: %+v", got)
		}
	}
	m = keptCase(t)
	closed := m.theCase(t).Closed
	for i := 0; i < 3; i++ {
		start := closed.Add(time.Duration(i) * w)
		hours(t, m.st, fmt.Sprint("quiet", i), start, 23*time.Hour, 100, 0)
		addLoop(m, fmt.Sprint("watch-loop", i), start, 100)
	}
	m.clock = closed.Add(3 * w)
	if ms, err := m.r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("watch: %+v %v", ms, err)
	}
	held, _, _ := m.r.Ship.Load(m.c.ID)
	if held.WatchWindow != 3 || len(held.WatchRecent) != 3 {
		t.Fatalf("watch progress: %+v", held)
	}
	for _, c := range held.WatchRecent {
		if c != (measure.Counts{Hits: 0, Base: 100}) {
			t.Fatalf("loop in watch: %+v", c)
		}
	}
}

// Published measurement guidance names both denominators and the watch's low-share limitation.
func TestMeasurementDocsStateTheConditionalRates(t *testing.T) {
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, text := range []string{"36.8%, 14.4% and 6.7%", "14.6% at 100 units", "five-hit gate makes admission rare", "0.41%, 1.53%, 2.76% and 4.81%", "0.052%, 1.165%, 2.563% and 5.700%", "12.9% and 29.3%", "count per window", "74.1%", "37.4%", "59.7%", "24.5%", "4.0%, 6.8% and 9.6%", "5.3%, 10.2% and 16.4%", "whole number of weeks"} {
		if !strings.Contains(spec, text) {
			t.Errorf("SPEC omits %q", text)
		}
	}
	raw, err = os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	if strings.Contains(readme, "shortest window") || !strings.Contains(readme, "three consecutive read watch windows") || !strings.Contains(readme, "Choose a week, or two for a rarer problem") {
		t.Fatal("README describes an older measurement")
	}
}

// A stale proposal waits with one passive note. A later complete read gives it a new counted boundary.
func TestAWaitingProposalTakesANewCompleteBoundary(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCaseReadAt(t, st, time.Now().Add(-30*24*time.Hour))
	clock := time.Now().Add(-20 * 24 * time.Hour).Truncate(time.Millisecond)
	s.Now = func() time.Time { return clock }
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{Title: "Correct the answer", Body: "Return the expected value.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	for i := 0; i < 2; i++ {
		p, err = s.Propose(ctx, c.ID, proposal)
		if err == nil || !strings.Contains(err.Error(), "has not read") || !p.ProposalAt.Equal(clock) || p.State != Checked {
			t.Fatalf("waiting proposal: %+v %v", p, err)
		}
		clock = clock.Add(7 * 24 * time.Hour)
	}
	ns, err := st.Notes(ctx)
	if err != nil || len(ns) != 1 || ns[0].Done.IsZero() == false || !strings.Contains(ns[0].Title, "store is behind") {
		t.Fatalf("note: %+v %v", ns, err)
	}
	r := &Runner{Store: st, Ship: s, Dir: s.Dir, Now: s.Now}
	if _, err := r.Measure(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	ns, _ = st.Notes(ctx)
	if !ns[0].Done.IsZero() {
		t.Fatal("waiting proposal's note resolved")
	}
	fresh := hours(t, st, "fresh", clock.Add(-24*time.Hour), 23*time.Hour, 100, 10)
	hours(t, st, "fresh-base", clock.Add(-28*24*time.Hour), 21*24*time.Hour, 300, 30)
	finding := store.Finding{Family: c.Family, Signature: "failures/v1:zz", Title: c.Title}
	for _, e := range fresh[:10] {
		finding.Evidence = append(finding.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	if _, _, err := st.ObserveCase(ctx, finding, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, store.Transcripts, clock); err != nil {
		t.Fatal(err)
	}
	p, err = s.Propose(ctx, c.ID, proposal)
	if err != nil || p.State != Proposed || !p.Plan.At.Equal(clock.Add(-time.Millisecond)) || !p.ProposalAt.IsZero() || !p.ReadWait.IsZero() {
		t.Fatalf("read proposal: %+v %v", p, err)
	}
	ns, _ = st.Notes(ctx)
	if ns[0].Done.IsZero() {
		t.Fatal("proposal's read note still stands")
	}
}

// Reading the first quiet window again does not reset the wait for the second window's source read.
func TestASecondUnreadWindowKeepsItsWaitTime(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	m.measure(t, Options{})
	start := m.clock
	hours(t, m.st, "quiet", start, 23*time.Hour, 60, 0)
	if err := m.st.MarkRead(ctx, store.Transcripts, start.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	m.clock = start.Add(49 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "has not read") {
		t.Fatalf("second unread: %+v", got)
	}
	first, _, _ := m.r.Ship.Load(m.c.ID)
	m.clock = m.clock.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatalf("wait: %+v", got)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	if !p.ReadWait.Equal(first.ReadWait) || noteKinds(m.notes(t))[store.NoteProblem] != 1 {
		t.Fatalf("wait restarted: %+v", p)
	}
}
