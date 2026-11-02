package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

// testingCaseReadAt is testingCase with the store's last complete transcripts read at a given time.
func testingCaseReadAt(t *testing.T, st *store.Store, read time.Time) store.Case {
	t.Helper()
	ctx := context.Background()
	var evs []store.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("s%d@%d", i%2, i), TS: time.Now().Add(-time.Duration(i+1) * time.Hour),
			Session: fmt.Sprint("s", i%2), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash",
			Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`})
	}
	for _, e := range append([]store.Event(nil), evs...) {
		e.SourceKey += "-baseline"
		e.TS = e.TS.Add(-8 * 24 * time.Hour)
		evs = append(evs, e)
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, store.Transcripts, read); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:zz", Title: "the thing breaks"}
	for _, e := range evs {
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

// A complete read five minutes ago admits both a first proposal and a checked revision on the real clock.
func TestACurrentReadAdmitsAProposalAndItsRevision(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f) // s.Now is nil: the real clock
	c := testingCaseReadAt(t, st, time.Now().Add(-5*time.Minute))
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Correct the answer", Body: "Return the expected value.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	p, err = s.Propose(ctx, c.ID, pr)
	t.Logf("first proposal, store read 5 minutes ago: state=%s err=%v", p.State, err)
	if err != nil || p.State != Proposed {
		t.Fatalf("first proposal: %+v %v", p, err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "no comment", Fix: "add one"}},
		Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || p.State != Revising {
		t.Fatalf("reviewed: %+v %v", p, err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is the answer, 42.", 1))
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	p, err = s.Propose(ctx, c.ID, pr)
	t.Logf("revision, store read a moment ago: state=%s err=%v", p.State, err)
	if err != nil || p.State != Proposed {
		t.Fatalf("revision: %+v %v", p, err)
	}
}

// A recorded activity source that read no event still holds the decision until it supplies the whole window.
func TestARecordedSourceWithoutEventsHoldsTheDecision(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	if err := m.st.MarkRead(ctx, store.Transcripts, m.deployed.Add(-time.Hour)); err != nil { // stalled from before the change
		t.Fatal(err)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Signal = Signal{Source: "mistakes", Kinds: []string{"deny"}, Match: []string{"zz-signal-text"}}
	p.Plan.Before, p.Plan.BeforeBase = 10, 100
	p.Plan.Sources = []string{"mistakes", store.Sessions, store.Transcripts}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("start: %+v", got)
	}
	start := m.clock
	var tr, others []store.Event
	for i := 0; i < 100; i++ {
		at := start.Add(time.Minute + time.Duration(i)*23*time.Hour/100)
		session := fmt.Sprint("after", i)
		tr = append(tr, store.Event{Source: store.Transcripts, SourceKey: session, TS: at, Session: session, Harness: "claude", Actor: store.ActorAgent, Kind: "text", Summary: "work"})
		if i < 40 {
			others = append(others, store.Event{Source: store.Sessions, SourceKey: session, TS: at, Session: session, Harness: "claude", Actor: store.ActorHarness, Kind: "phase", Summary: "working"})
		}
		if i == 0 {
			others = append(others, store.Event{Source: "mistakes", SourceKey: session, TS: at, Session: session, Harness: "claude", Actor: store.ActorHarness, Kind: "deny", Summary: "zz-signal-text: refused"})
		}
	}
	if _, err := m.st.Put(ctx, others); err != nil {
		t.Fatal(err)
	}
	m.clock = start.Add(25 * time.Hour)
	for _, source := range []string{"mistakes", store.Sessions} {
		if err := m.st.MarkRead(ctx, source, m.clock); err != nil {
			t.Fatal(err)
		}
	}
	got := m.measure(t, Options{})
	t.Logf("transcripts stalled: %s: %s", got.Status, got.Why)
	if got.Status != "waiting" || !strings.Contains(got.Why, store.Transcripts) {
		t.Fatalf("decided on a stalled source: %+v", got)
	}
	if _, err := m.st.Put(ctx, tr); err != nil { // the reader resumes and stores the same hours at their times
		t.Fatal(err)
	}
	if err := m.st.MarkRead(ctx, store.Transcripts, m.clock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got = m.measure(t, Options{})
	t.Logf("transcripts read: %s: %+v", got.Status, got.Result)
	if got.Status != "improved" {
		t.Fatalf("read: %+v", got)
	}
}

// A missing recorded reader cannot make a fix that holds come back at the share of the remaining activity.
func TestARecordedSourceWithoutEventsHoldsTheWatch(t *testing.T) {
	ctx := context.Background()
	m := measuredCaseWithRead(t, false)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Signal = Signal{Source: "mistakes", Kinds: []string{"deny"}, Match: []string{"zz-signal-text"}}
	p.Plan.Before, p.Plan.BeforeBase = 10, 100
	p.Plan.Sources = []string{"mistakes", store.Sessions, store.Transcripts}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.measure(t, Options{})
	start := m.clock
	window := func(prefix string, from time.Time, hits int, transcripts bool) {
		var evs []store.Event
		for i := 0; i < 100; i++ {
			at := from.Add(time.Minute + time.Duration(i)*23*time.Hour/100)
			session := fmt.Sprint(prefix, i)
			if transcripts {
				evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: session, TS: at, Session: session, Harness: "claude", Actor: store.ActorAgent, Kind: "text", Summary: "work"})
			}
			if i < 40 {
				evs = append(evs, store.Event{Source: store.Sessions, SourceKey: session, TS: at, Session: session, Harness: "claude", Actor: store.ActorHarness, Kind: "phase", Summary: "working"})
			}
			if i < hits {
				evs = append(evs, store.Event{Source: "mistakes", SourceKey: session, TS: at, Session: session, Harness: "claude", Actor: store.ActorHarness, Kind: "deny", Summary: "zz-signal-text: refused"})
			}
		}
		if _, err := m.st.Put(ctx, evs); err != nil {
			t.Fatal(err)
		}
	}
	window("after", start, 0, true)
	m.clock = start.Add(25 * time.Hour)
	for _, source := range []string{store.Transcripts, "mistakes", store.Sessions} {
		if err := m.st.MarkRead(ctx, source, m.clock); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("keep: %+v", got)
	}
	closed := m.theCase(t).Closed
	for i := 0; i < 3; i++ { // the fix holds at 4 of 100; the transcripts reader read nothing since the close
		window(fmt.Sprint("watch", i, "-"), closed.Add(time.Duration(i)*24*time.Hour), 4, false)
	}
	m.clock = closed.Add(3*24*time.Hour + time.Minute)
	for _, source := range []string{"mistakes", store.Sessions} {
		if err := m.st.MarkRead(ctx, source, m.clock); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := m.r.Measure(ctx, Options{})
	t.Logf("watch with the transcripts reader stalled: %+v %v; case %s", ms, err, m.theCase(t).Status)
	if err != nil || len(ms) != 1 || ms[0].Status != "waiting" || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("%+v %v", ms, err)
	}

	for i := 0; i < 3; i++ {
		window(fmt.Sprint("watch", i, "-"), closed.Add(time.Duration(i)*24*time.Hour), 4, true)
	}
	if err := m.st.MarkRead(ctx, store.Transcripts, m.clock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ms, err = m.r.Measure(ctx, Options{})
	if err != nil || len(ms) != 0 || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("read watch: %+v %v", ms, err)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if p.WatchWindow != 3 {
		t.Fatal("read watch did not advance")
	}
	for _, window := range p.WatchRecent {
		if window.Hits != 4 || window.Base != 100 {
			t.Fatalf("partial activity counted: %+v", window)
		}
	}
}

// A changed installed file checks its build checkout again, by its hash and time, and remembers a rejected build.
func TestAChangedInstalledFileChecksItsBuildCheckout(t *testing.T) {
	for _, change := range []string{"time", "hash", "both"} {
		t.Run(change, func(t *testing.T) {
			m := measuredCase(t)
			p, _, _ := m.r.Ship.Load(m.c.ID)
			bin := filepath.Join(t.TempDir(), "tool")
			write(t, bin, "build with the change")
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
			start := m.clock
			// the checkout moves to a branch cut before the change, and the tool is rebuilt from it
			run(t, m.repo, "git", "checkout", "-q", "--orphan", "older")
			run(t, m.repo, "git", "rm", "-rqf", ".")
			write(t, filepath.Join(m.repo, "a.txt"), "the old code\n")
			run(t, m.repo, "git", "add", "-A")
			run(t, m.repo, "git", "commit", "-q", "-m", "before the change")
			if change != "time" {
				write(t, bin, "build without the change")
			}
			rebuilt := start.Add(2 * time.Hour)
			if change == "hash" {
				rebuilt = start
			}
			if err := os.Chtimes(bin, rebuilt, rebuilt); err != nil {
				t.Fatal(err)
			}
			p, _, _ = m.r.Ship.Load(m.c.ID)
			since, why, err := m.r.Ship.InEffect(context.Background(), &p)
			t.Logf("in effect after the rebuild: since=%v why=%q err=%v", since, why, err)
			if why == "" || err != nil {
				t.Fatalf("in effect: %q %v", why, err)
			}
			hours(t, m.st, "after", start.Add(time.Minute), 23*time.Hour, 100, 6) // the old build's share, unchanged
			m.clock = start.Add(25 * time.Hour)
			got := m.measure(t, Options{})
			t.Logf("decided: %s: %s", got.Status, got.Why)
			if got.Status != "waiting" || !strings.Contains(got.Why, "not in effect") {
				t.Fatalf("%+v", got)
			}
			p, _, _ = m.r.Ship.Load(m.c.ID)
			run(t, m.repo, "git", "checkout", "-q", p.Landed[len(p.Landed)-1])
			if _, why, err := m.r.Ship.InEffect(context.Background(), &p); err != nil || why == "" {
				t.Fatalf("checkout alone repaired the build: %s %v", why, err)
			}
			later := m.clock.Add(time.Hour)
			if err := os.Chtimes(bin, later, later); err != nil {
				t.Fatal(err)
			}
			if _, why, err := m.r.Ship.InEffect(context.Background(), &p); err != nil || why != "" {
				t.Fatalf("later build not accepted: %s %v", why, err)
			}
		})
	}
}

// Rebuilding the signal source holds the decision until a new read restores the events it dropped.
func TestARebuiltSourceWaitsForANewRead(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprint(rebuild), func(t *testing.T) {
			ctx := context.Background()
			m := measuredCase(t)
			p, _, _ := m.r.Ship.Load(m.c.ID)
			p.Plan.Signal = Signal{Source: "mistakes", Kinds: []string{"deny"}, Match: []string{"zz-signal-text"}}
			if err := m.r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			m.measure(t, Options{})
			start := m.clock
			evs := hours(t, m.st, "after", start.Add(time.Minute), 23*time.Hour, 100, 0)
			var hits []store.Event
			for _, e := range evs[:6] { // the problem unchanged: 6 of 100 hours, as before
				hits = append(hits, store.Event{Source: "mistakes", SourceKey: e.Session, TS: e.TS, Session: e.Session, Harness: "claude",
					Actor: store.ActorHarness, Kind: "deny", Summary: "zz-signal-text: refused"})
			}
			if _, err := m.st.Put(ctx, hits); err != nil {
				t.Fatal(err)
			}
			if err := m.st.SetCursor(ctx, "mistakes", "synthetic"); err != nil {
				t.Fatal(err)
			}
			if err := m.st.MarkRead(ctx, "mistakes", start.Add(24*time.Hour+time.Minute)); err != nil {
				t.Fatal(err)
			}
			if rebuild {
				n, err := m.st.Rebuild(ctx, "mistakes", false)
				read, rerr := m.st.ReadThrough(ctx, "mistakes")
				t.Logf("rebuild dropped %d events (%v); read through %v %v", n, err, read, rerr)
			}
			m.clock = start.Add(25 * time.Hour)
			got := m.measure(t, Options{})
			t.Logf("rebuild=%v: %s: %s %+v", rebuild, got.Status, got.Why, got.Result)
			if want := map[bool]string{false: "held", true: "waiting"}[rebuild]; got.Status != want {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// Stopping a recorded reader recounts the baseline and every later window over the same remaining sources.
func TestStoppedSourcesRecountBothSidesAndLeaveANote(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint(watch), func(t *testing.T) {
			ctx := context.Background()
			m := measuredCase(t)
			if watch {
				m = keptCase(t)
			}
			p, _, _ := m.r.Ship.Load(m.c.ID)
			p.Plan.Sources = []string{store.Transcripts, store.Supervise}
			p.Plan.Before, p.Plan.BeforeBase = 30, 500
			hours(t, m.st, "independent", p.Plan.At.Add(-4*p.Plan.Window), 3*p.Plan.Window, 300, 30)
			var evs []store.Event
			for i := 0; i < 200; i++ {
				evs = append(evs, store.Event{Source: store.Supervise, SourceKey: fmt.Sprint(i), TS: p.Plan.At.Add(-2 * p.Plan.Window), Session: fmt.Sprint("supervised", i), Actor: store.ActorDetector, Kind: "act", Summary: "ordinary decision"})
			}
			if _, err := m.st.Put(ctx, evs); err != nil {
				t.Fatal(err)
			}
			if err := m.r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			cfg := config.Sources{}
			cfg.Transcripts.Homes = []string{"/synthetic"}
			cfg.Supervise.Off = true
			m.r.Ship.SourceLive = cfg.SourceLive
			if watch {
				closed := m.theCase(t).Closed
				for i := 0; i < 3; i++ {
					hours(t, m.st, fmt.Sprint("return", i), closed.Add(time.Duration(i)*p.Plan.Window), 23*time.Hour, 100, 10)
				}
				m.clock = closed.Add(3*p.Plan.Window + time.Minute)
				if got := m.measure(t, Options{}); got.Status != "back" {
					t.Fatalf("watch: %+v", got)
				}
			} else {
				if got := m.measure(t, Options{}); got.Status != "measuring" {
					t.Fatal(got)
				}
				hours(t, m.st, "after", m.clock, 23*time.Hour, 100, 0)
				m.clock = m.clock.Add(25 * time.Hour)
				got := m.measure(t, Options{})
				if got.Status != "improved" || got.Result.Before.Base != 300 || got.Result.Before.Hits != 30 {
					t.Fatalf("recount: %+v", got)
				}
			}
			log, err := m.st.CaseLog(ctx, m.c.ID)
			if err != nil || !strings.Contains(fmt.Sprint(log), "sources left out of both counts: supervise") {
				t.Fatalf("note: %+v %v", log, err)
			}
		})
	}
}

// A stopped signal cannot decide a case even when the other readers are current.
func TestAStoppedSignalMakesNoDecision(t *testing.T) {
	m := measuredCase(t)
	m.r.Ship.SourceLive = func(source string) bool { return source != store.Transcripts }
	got := m.measure(t, Options{})
	if got.Status != "waiting" || !strings.Contains(got.Why, "signal's source stopped") {
		t.Fatal(got)
	}
	if m.theCase(t).Status != store.CaseDeployed || len(m.notes(t)) != 1 {
		t.Fatal("stopped signal decided or left no note")
	}
}

// Interrupted keeps wait for the read of the first window and, when it was quiet, the second window too.
func TestAnInterruptedKeepWaitsForItsUnreadWindow(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprint(quiet), func(t *testing.T) {
			ctx := context.Background()
			m := measuredCaseWithRead(t, false)
			m.measure(t, Options{})
			start := m.clock
			if quiet {
				hours(t, m.st, "first", start, time.Hour, 1, 0)
				hours(t, m.st, "second", start.Add(24*time.Hour), 23*time.Hour, 100, 0)
				if err := m.st.MarkRead(ctx, store.Transcripts, start.Add(25*time.Hour)); err != nil {
					t.Fatal(err)
				}
			} else {
				hours(t, m.st, "after", start, 23*time.Hour, 100, 0)
			}
			if _, err := m.st.CloseMeasured(ctx, m.c.ID, 0, "improved", "test"); err != nil {
				t.Fatal(err)
			}
			m.clock = start.Add(49 * time.Hour)
			if got := m.measure(t, Options{}); got.Status != "waiting" {
				t.Fatalf("unread recovery: %+v", got)
			}
			p, _, _ := m.r.Ship.Load(m.c.ID)
			if p.State != Landed || p.Result != nil {
				t.Fatal("unread keep completed")
			}
			if err := m.st.MarkRead(ctx, store.Transcripts, m.clock); err != nil {
				t.Fatal(err)
			}
			if _, err := m.r.Measure(ctx, Options{}); err != nil {
				t.Fatal(err)
			}
			p, _, _ = m.r.Ship.Load(m.c.ID)
			if p.State != Kept || p.Result == nil {
				t.Fatal("read keep not recovered")
			}
		})
	}
}

// An unread measurement has no problem note until a full window of waiting has passed.
func TestAnUnreadWindowHasNoEarlyWaitNote(t *testing.T) {
	m := measuredCaseWithRead(t, false)
	m.measure(t, Options{})
	m.clock = m.clock.Add(25 * time.Hour)
	m.measure(t, Options{})
	if len(m.notes(t)) != 0 {
		t.Fatal("note on first wait")
	}
	m.clock = m.clock.Add(24*time.Hour - time.Millisecond)
	m.measure(t, Options{})
	if len(m.notes(t)) != 0 {
		t.Fatal("note before a window passed")
	}
	m.clock = m.clock.Add(time.Millisecond)
	m.measure(t, Options{})
	if len(m.notes(t)) != 1 {
		t.Fatal("no note after a window")
	}
}

// Waiting for an installed build saves the checkout time across a later rewrite of the changed file.
func TestADeployedBuildWaitKeepsTheFirstCheckoutTime(t *testing.T) {
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	bin := filepath.Join(t.TempDir(), "tool")
	write(t, bin, "old build")
	p.Plan.Installed, p.Changed = bin, []string{"a.txt"}
	if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), m.deployed, m.deployed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, m.deployed.Add(-time.Hour), m.deployed.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "waiting" {
		t.Fatal(got)
	}
	write(t, bin, "build with the change")
	built := m.deployed.Add(time.Hour)
	if err := os.Chtimes(bin, built, built); err != nil {
		t.Fatal(err)
	}
	rewritten := built.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), rewritten, rewritten); err != nil {
		t.Fatal(err)
	}
	m.clock = rewritten.Add(time.Minute)
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("rewrite while waiting: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	if !p.CheckoutAt.Equal(m.deployed) {
		t.Fatalf("checkout time: %v", p.CheckoutAt)
	}
}

// A waiting proposal's note goes once the case leaves testing, even without a proposal.
func TestAProposalWaitNoteEndsWithTesting(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	r := runnerFor(t, st, nil)
	p := Package{Case: c.ID, State: Checked, ProposalAt: time.Now(), ReadWait: time.Now()}
	r.problem(ctx, &p, "the store is behind", "waiting for a read")
	if _, err := st.CloseCase(ctx, c.ID, store.Insufficient, "", "no evidence", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Measure(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	notes, err := st.Notes(ctx)
	if err != nil || len(notes) != 1 || notes[0].Done.IsZero() {
		t.Fatalf("note: %+v %v", notes, err)
	}
}

// Go's recorded revision identifies the installed build even when its checkout has moved elsewhere.
func TestAnInstalledGoRevisionMustHoldTheChange(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	write(t, filepath.Join(m.repo, "go.mod"), "module example.test/tool\n\ngo 1.24\n")
	write(t, filepath.Join(m.repo, "main.go"), "package main\nfunc main() {}\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-qm", "add the tool")
	old := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	write(t, filepath.Join(m.repo, "main.go"), "package main\nfunc main() { println(42) }\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-qm", "fix the tool")
	landed := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	p.Landed = []string{landed}
	p.Plan.Installed = filepath.Join(t.TempDir(), "tool")
	build := func() { t.Helper(); run(t, m.repo, "go", "build", "-buildvcs=true", "-o", p.Plan.Installed, ".") }
	check := func(want bool) {
		t.Helper()
		_, why, err := m.r.Ship.InEffect(ctx, &p)
		if err != nil || (why == "") != want {
			t.Fatalf("in effect %v: %s %v", want, why, err)
		}
	}
	build()
	check(true)
	run(t, m.repo, "git", "checkout", "-q", old)
	check(true) // the binary did not change
	build()
	check(false)
	run(t, m.repo, "git", "checkout", "-q", landed)
	check(false) // moving the checkout cannot repair the installed revision
	run(t, m.repo, "git", "commit", "-q", "--allow-empty", "-m", "later change")
	build()
	check(true) // a descendant holds the landed commit
	write(t, filepath.Join(m.repo, "main.go"), "package main\nfunc main() { println(0) }\n")
	build()
	check(false) // modified trees cannot certify a build
}

// Vanished loop runs carry the stamp's directory and cannot dilute an unchanged problem in the step.
func TestVanishedLoopRunsDoNotDiluteTheStep(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Sources = []string{store.Transcripts, store.Sessions}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.measure(t, Options{})
	start := m.clock
	hours(t, m.st, "after", start, 23*time.Hour, 100, 6)
	stamps := state.Stamps{}
	var live []*sessions.Session
	for i := 0; i < 250; i++ {
		live = append(live, &sessions.Session{Key: fmt.Sprint("run", i), Tool: "claude", Phase: sessions.Working, Cwd: filepath.Join(m.r.Dir, "runs", fmt.Sprint(i))})
	}
	stamps.Apply(live, start)
	if err := m.st.SaveSessionStamps(ctx, stamps); err != nil {
		t.Fatal(err)
	}
	stamps, err := m.st.SessionStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changes := stamps.Apply(nil, start.Add(time.Hour))
	var evs []store.Event
	for _, c := range changes {
		evs = append(evs, store.Event{Source: store.Sessions, SourceKey: c.Key, TS: c.At, Session: c.Key, Harness: c.Tool, Cwd: c.Cwd, Actor: store.ActorHarness, Kind: "vanished", Summary: "finished"})
	}
	if len(evs) != 250 {
		t.Fatal("missing vanished events")
	}
	if _, err := m.st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	m.clock = start.Add(25 * time.Hour)
	if err := m.st.MarkRead(ctx, store.Sessions, m.clock); err != nil {
		t.Fatal(err)
	}
	got := m.measure(t, Options{})
	if got.Status != "held" || got.Result == nil || got.Result.After.Base != 100 || got.Result.Improved {
		t.Fatalf("loop activity counted: %+v", got)
	}
}

// A no-longer-configured source is omitted at proposal time and remains named on the counted plan.
func TestAPlanLeavesOutAnUnconfiguredSource(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	if _, err := st.Put(ctx, []store.Event{{Source: "radar", SourceKey: "baseline", TS: now.Add(-10 * 24 * time.Hour), Session: "extra", Actor: store.ActorDetector, Kind: "act", Summary: "ordinary activity"}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Sources{}
	cfg.Transcripts.Homes = []string{"/synthetic"}
	s := Ship{Store: st, SourceLive: cfg.SourceLive}
	plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	if err != nil || len(plan.Omitted) != 1 || plan.Omitted[0] != "radar" || plan.BeforeBase != 6 {
		t.Fatalf("stopped baseline: %+v %v", plan, err)
	}
}

// A plan records sources from its gate and baseline, including events outside its activity units.
func TestAPlanRecordsEverySourceInItsSpan(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	for i, source := range []string{store.Supervise, "radar"} {
		at := now.Add(-time.Hour)
		if i == 1 {
			at = now.Add(-10 * 24 * time.Hour)
		}
		if _, err := st.Put(ctx, []store.Event{{Source: source, SourceKey: "ordinary", TS: at, Actor: store.ActorDetector, Kind: "ordinary", Summary: "ordinary activity"}}); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkRead(ctx, source, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	s := Ship{Store: st}
	plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	if err != nil || strings.Join(plan.Sources, ",") != "radar,supervise,transcripts" {
		t.Fatalf("recorded sources: %+v %v", plan, err)
	}
}

// Moving a plan's boundary also moves its baseline start, which can invalidate a partial read's coverage.
func TestAPlanRechecksPartialReadsAtItsNewBoundary(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Now().Truncate(time.Millisecond)
	complete := now.Add(-10 * time.Minute)
	c := testingCaseReadAt(t, st, complete)
	if err := st.MarkPartialRead(ctx, store.Transcripts, now.Add(time.Minute), now.Add(-28*24*time.Hour-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, []store.Event{{Source: "zz-reader", SourceKey: "activity", TS: now.Add(-2 * time.Hour), Actor: store.ActorDetector, Kind: "ordinary", Summary: "ordinary activity"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, "zz-reader", now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	s := Ship{Store: st}
	plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	if err != nil || !plan.At.Equal(complete.Add(-time.Millisecond)) {
		t.Fatalf("partial read at the earlier start: %+v %v", plan, err)
	}
}

// A build first seen without the change stays rejected when the checkout moves back, until the file changes.
func TestARejectedFirstBuildNeedsAnotherFileChange(t *testing.T) {
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Installed = filepath.Join(t.TempDir(), "tool")
	write(t, p.Plan.Installed, "old build")
	run(t, m.repo, "git", "checkout", "-q", "--orphan", "before")
	run(t, m.repo, "git", "commit", "-qm", "before the change")
	ctx := context.Background()
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why == "" {
		t.Fatalf("first absent build: %s %v", why, err)
	}
	run(t, m.repo, "git", "checkout", "-q", p.Landed[0])
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why == "" {
		t.Fatalf("checkout alone accepted the old build: %s %v", why, err)
	}
	if p.CheckoutAt.IsZero() {
		t.Fatal("waiting build lost the first checkout time")
	}
	write(t, p.Plan.Installed, "new build")
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("new build: %s %v", why, err)
	}
}
