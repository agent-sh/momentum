package loop

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
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
	seedHistory(t, st)
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
	setCaseRepo(t, st, c, "main")
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
	if _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Correct the answer", Body: "Return the expected value.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	p, err = proposePatch(t, s, ctx, c.ID, pr)
	t.Logf("first proposal, store read 5 minutes ago: state=%s err=%v", p.State, err)
	if err != nil || p.State != Proposed {
		t.Fatalf("first proposal: %+v %v", p, err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Round 1: one medium finding.", Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "no comment", Fix: "add one"}},
		Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || p.State != Revising {
		t.Fatalf("reviewed: %+v %v", p, err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is the answer, 42.", 1))
	if _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	p, err = proposePatch(t, s, ctx, c.ID, pr)
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
			// the checkout moves to a branch cut before the change, and the tool is rebuilt from it; the moves are dated on
			// the case's clock, as its builds are
			datedGitIn(t, m.repo, start, "checkout", "-q", "--orphan", "older")
			run(t, m.repo, "git", "rm", "-rqf", ".")
			write(t, filepath.Join(m.repo, "a.txt"), "the old code\n")
			run(t, m.repo, "git", "add", "-A")
			datedGitIn(t, m.repo, start, "commit", "-q", "-m", "before the change")
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
			datedGitIn(t, m.repo, m.clock, "checkout", "-q", p.Landed[len(p.Landed)-1])
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
	r := measurementRunner(t, st)
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
	p.Landed, p.Changed = []string{landed}, []string{"main.go"}
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
	write(t, filepath.Join(m.repo, "notes.txt"), "an untracked note\n")
	build()
	check(true) // Go calls the tree modified for an untracked file; the change's own files are the revision's
	write(t, filepath.Join(m.repo, "main.go"), "package main\nfunc main() { println(0) }\n")
	build()
	check(false) // the change's own file modified: the build may not hold it
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

// A plan records the sources its units can count, from its gate and baseline: a sessions' measurement records a
// source whose events hold sessions, and none whose events have no session (a capacity run, the scout's findings).
func TestAPlanRecordsEverySourceInItsSpan(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := testingCase(t, st)
	now := time.Now()
	for i, source := range []string{store.Supervise, "radar", "capacity"} {
		at, session := now.Add(-time.Hour), "s1"
		if i == 1 {
			at = now.Add(-10 * 24 * time.Hour)
		}
		if i == 2 {
			session = "" // the machine's, not a session's
		}
		// each reader ran before the span too: one that began inside it is left out (TestAReaderThatBeganInsideTheSpanIsLeftOut)
		if _, err := st.Put(ctx, []store.Event{{Source: source, SourceKey: "ordinary", TS: at, Session: session, Actor: store.ActorDetector, Kind: "ordinary", Summary: "ordinary activity"},
			{Source: source, SourceKey: "earlier", TS: now.Add(-40 * 24 * time.Hour), Session: session, Actor: store.ActorDetector, Kind: "ordinary", Summary: "earlier activity"}}); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkRead(ctx, source, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	s := Ship{Store: st}
	plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", now)
	if err != nil || plan.Units != SessionHours || strings.Join(plan.Sources, ",") != "radar,supervise,transcripts" {
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
	if _, err := st.Put(ctx, []store.Event{{Source: "zz-reader", SourceKey: "activity", TS: now.Add(-2 * time.Hour), Session: "s1", Actor: store.ActorDetector, Kind: "ordinary", Summary: "ordinary activity"},
		{Source: "zz-reader", SourceKey: "earlier", TS: now.Add(-60 * 24 * time.Hour), Session: "s1", Actor: store.ActorDetector, Kind: "ordinary", Summary: "earlier activity"}}); err != nil {
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

// A build first seen without the change stays rejected when the checkout moves back, until the file changes. The test
// keeps a clock of its own, with each build seconds after the move before it: the reflog dates a move to the second
// (TestAnInstalledFileWrittenInTheSecondItsCheckoutMoved).
func TestARejectedFirstBuildNeedsAnotherFileChange(t *testing.T) {
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Installed = filepath.Join(t.TempDir(), "tool")
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	datedGitIn(t, m.repo, base, "checkout", "-q", "--orphan", "before")
	datedGitIn(t, m.repo, base, "commit", "-qm", "before the change")
	writeDated(t, p.Plan.Installed, "old build", base.Add(2500*time.Millisecond))
	ctx := context.Background()
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why == "" {
		t.Fatalf("first absent build: %s %v", why, err)
	}
	datedGitIn(t, m.repo, base.Add(5*time.Second), "checkout", "-q", p.Landed[0])
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why == "" {
		t.Fatalf("checkout alone accepted the old build: %s %v", why, err)
	}
	if p.CheckoutAt.IsZero() {
		t.Fatal("waiting build lost the first checkout time")
	}
	writeDated(t, p.Plan.Installed, "new build", base.Add(7500*time.Millisecond))
	if _, why, err := m.r.Ship.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("new build: %s %v", why, err)
	}
}

// HEAD's reflog dates a move to the whole second, after git writes the tree, and an installed file's time is finer: a
// move dated in the file's second or the next cannot be ordered against it. When it moved between a commit with the
// change and one without it, the file is out of effect with that reason, kept while it stands, until an install in a
// later second. A move that leaves every commit holding the change, or a build a clear second away, is decided as before.
// A reflog that ends in that window, or after the file, cannot say what HEAD named then; an empty one leaves HEAD as it
// stands, and that reason, about the checkout, is not kept for the file.
func TestAnInstalledFileWrittenInTheSecondItsCheckoutMoved(t *testing.T) {
	ms := time.Millisecond
	aBefore := func(t *testing.T, repo string) { write(t, filepath.Join(repo, "a.txt"), "a, before the change\n") }
	link := func(t *testing.T, repo, to string) {
		t.Helper()
		if err := os.Remove(filepath.Join(repo, "l")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(to, filepath.Join(repo, "l")); err != nil {
			t.Fatal(err)
		}
	}
	a := []string{"a.txt"}
	for _, tc := range []struct {
		name            string
		changed         []string                        // the change's own files
		change, without func(t *testing.T, repo string) // the landed commit's edits on the case's, and the tree without the change's
		first           bool                            // no run saw the checkout hold the change before (no CheckoutAt)
		moves           func(s *sameSecond)             // the checkout's moves from the commit without the change, and their files' times
		built           time.Duration                   // the installed file's time, into the second
		why             string                          // a part of the reason it is out of effect; empty when in effect
		after           func(s *sameSecond)             // what the checkout does before the next run, with the file unchanged
		next            string                          // a part of the next run's reason, when it is not why's
		again           time.Duration                   // an install after it, into the second, which is in effect; 0 for none
	}{
		{name: "built in the second of a move onto the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, 0); s.date("a.txt", 200*ms) }, built: 600 * ms, why: movedInItsSecond, again: 5 * time.Second},
		{name: "built in the second of a move off the change, at its first sighting", changed: a, without: aBefore, first: true,
			moves: func(s *sameSecond) { s.move(s.landed, -59*time.Minute); s.move(s.without, 0) }, built: 300 * ms, why: movedInItsSecond},
		{name: "built in the second of the reflog's oldest move", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.expire(); s.move(s.landed, 0); s.date("a.txt", 200*ms) }, built: 600 * ms, why: movedInItsSecond},
		{name: "built before the reflog's oldest move", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.expire(); s.move(s.landed, 5*time.Second) }, built: 300 * ms, why: reflogShort},
		{name: "built with an empty reflog on the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, -59*time.Minute); s.expire() }, built: 300 * ms},
		{name: "built with an empty reflog off the change, and the checkout moves onto it", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.expire() }, built: 300 * ms, why: "the repository's checkout does not hold",
			after: func(s *sameSecond) { s.move(s.landed, 5*time.Second) }, next: "has not changed since a build from a checkout without the change"},
		{name: "built in the second of a move off the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, -59*time.Minute); s.move(s.without, 0) }, built: 300 * ms, why: movedInItsSecond},
		{name: "built in the second of several moves", changed: a, without: aBefore,
			moves: func(s *sameSecond) {
				s.move(s.landed, -59*time.Minute)
				s.move(s.without, 0)
				s.move(s.landed, 0)
				s.date("a.txt", 700*ms)
			}, built: 900 * ms, why: movedInItsSecond},
		{name: "built in the second before a move off the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, -59*time.Minute); s.move(s.without, time.Second) }, built: 900 * ms, why: movedInItsSecond},
		{name: "built in the second before a move onto the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, time.Second); s.date("a.txt", time.Second) }, built: 900 * ms, why: movedInItsSecond},
		{name: "built in the second of a move onto the change, at its first sighting", changed: a, without: aBefore, first: true,
			moves: func(s *sameSecond) { s.move(s.landed, 0); s.date("a.txt", 200*ms) }, built: 600 * ms, why: movedInItsSecond, again: 5 * time.Second},
		{name: "built in the second of a commit on the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, -59*time.Minute); s.commit(0) }, built: 300 * ms},
		{name: "built a second after a move onto the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, 0); s.date("a.txt", 200*ms) }, built: 2300 * ms},
		{name: "built a second after a move onto the change, at its first sighting", changed: a, without: aBefore, first: true,
			moves: func(s *sameSecond) { s.move(s.landed, 0); s.date("a.txt", 200*ms) }, built: 2300 * ms},
		{name: "built a second before a move onto the change", changed: a, without: aBefore,
			moves: func(s *sameSecond) { s.move(s.landed, 2*time.Second); s.date("a.txt", 2*time.Second) }, built: 300 * ms,
			why: "the checkout the installed file was built from did not hold"},
		{name: "built before its link of the change was written, at its first sighting", changed: []string{"l"}, first: true,
			change: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "b.txt"), "b\n")
				link(t, repo, "a.txt")
			},
			without: func(t *testing.T, repo string) { link(t, repo, "b.txt") },
			// the checkout writes the link now, after the build; its target keeps an older time
			moves: func(s *sameSecond) { s.move(s.landed, -59*time.Minute) }, built: 300 * ms, why: "has not changed since the checkout held the change"},
		{name: "built from a change that turns a directory into a file, at its first sighting", changed: []string{"a.txt", "x", "x/y"}, first: true,
			change: func(t *testing.T, repo string) { write(t, filepath.Join(repo, "x"), "x is a file\n") },
			without: func(t *testing.T, repo string) {
				aBefore(t, repo)
				if err := os.Remove(filepath.Join(repo, "x")); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(repo, "x", "y"), "y\n")
			},
			moves: func(s *sameSecond) {
				s.move(s.landed, -59*time.Minute)
				s.date("a.txt", -30*time.Minute)
				s.date("x", -30*time.Minute)
			}, built: 300 * ms},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSameSecond(t, tc.changed, tc.change, tc.without)
			if tc.first {
				s.p.CheckoutAt = time.Time{}
			}
			tc.moves(s)
			why := s.install(tc.built)
			if (why == "") != (tc.why == "") || !strings.Contains(why, tc.why) {
				t.Fatalf("in effect %v, want %v: %q", why == "", tc.why == "", why)
			}
			if tc.after != nil {
				tc.after(s)
			}
			want := firstOf(tc.next, tc.why)
			if next := s.judge(); (next == "") != (want == "") || !strings.Contains(next, want) || tc.next == "" && next != why {
				t.Fatalf("the next run, with the file unchanged: %q, want %q", next, want)
			}
			if tc.again != 0 {
				if why := s.install(tc.again); why != "" {
					t.Fatalf("an install in a later second: %q", why)
				}
			}
		})
	}
}

// sameSecond is a measured case whose checkout moves around one second of the test's clock (second), an hour after
// its base. The landed commit holds the change, with the case's edits on the measured case's commit; without is a root
// commit that does not, made at base from the landed tree with the case's edits, and the checkout is on it. Every
// regular file of the work tree is dated base.
type sameSecond struct {
	t               *testing.T
	m               *measured
	p               Package
	landed, without string
	base, second    time.Time
}

func newSameSecond(t *testing.T, changed []string, change, without func(t *testing.T, repo string)) *sameSecond {
	t.Helper()
	s := &sameSecond{t: t, m: measuredCase(t)}
	s.p, _, _ = s.m.r.Ship.Load(s.m.c.ID)
	s.base = time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	s.second = s.base.Add(time.Hour)
	repo := s.m.repo
	if change != nil {
		change(t, repo)
		run(t, repo, "git", "add", "-A")
		datedGitIn(t, repo, s.base.Add(-time.Hour), "commit", "-qm", "the change")
	}
	s.landed = strings.TrimSpace(run(t, repo, "git", "rev-parse", "HEAD"))
	s.p.Landed, s.p.Changed, s.p.Plan.Installed = []string{s.landed}, changed, filepath.Join(t.TempDir(), "tool")
	s.p.CheckoutAt = s.p.LandedAt // an earlier run saw the checkout hold the change
	datedGitIn(t, repo, s.base, "checkout", "-q", "--orphan", "without")
	if without != nil {
		without(t, repo)
	}
	run(t, repo, "git", "add", "-A")
	datedGitIn(t, repo, s.base, "commit", "-qm", "a tree without the change")
	s.without = strings.TrimSpace(run(t, repo, "git", "rev-parse", "HEAD"))
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case d.Type().IsRegular():
			return os.Chtimes(path, s.base, s.base)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// move checks out a commit, its reflog entry dated at an offset into the second.
func (s *sameSecond) move(commit string, at time.Duration) {
	s.t.Helper()
	datedGitIn(s.t, s.m.repo, s.second.Add(at), "checkout", "-q", commit)
}

// date dates a file of the work tree at an offset into the second, as the move that wrote it would have.
func (s *sameSecond) date(name string, at time.Duration) {
	s.t.Helper()
	dated(s.t, filepath.Join(s.m.repo, name), s.second.Add(at))
}

// expire empties the checkout's reflogs, as git's expiry does for a HEAD that has not moved for long.
func (s *sameSecond) expire() {
	s.t.Helper()
	run(s.t, s.m.repo, "git", "reflog", "expire", "--expire=now", "--all")
}

// commit commits on the checkout's HEAD, its reflog entry dated at an offset into the second.
func (s *sameSecond) commit(at time.Duration) {
	s.t.Helper()
	datedGitIn(s.t, s.m.repo, s.second.Add(at), "commit", "-q", "--allow-empty", "-m", "more work on the change")
}

// install installs a build dated at an offset into the second and says why it is out of effect, or nothing.
func (s *sameSecond) install(built time.Duration) string {
	s.t.Helper()
	writeDated(s.t, s.p.Plan.Installed, fmt.Sprintf("a build at %v", built), s.second.Add(built))
	return s.judge()
}

// judge says why the installed file is out of effect, or nothing.
func (s *sameSecond) judge() string {
	s.t.Helper()
	_, why, err := s.m.r.Ship.InEffect(context.Background(), &s.p)
	if err != nil {
		s.t.Fatal(err)
	}
	return why
}

// dated sets a file's modification time.
func dated(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// writeDated writes a file with a modification time.
func writeDated(t *testing.T, path, text string, at time.Time) {
	t.Helper()
	write(t, path, text)
	dated(t, path, at)
}

// goTool is a Go tool in a measured case's repository: its landed commit fixes main.go, and the case's package holds it.
// build installs it, from the repository as it stands, with its version control stamp; inEffect asks the ship tool.
type goTool struct {
	t        *testing.T
	m        *measured
	p        Package
	landed   string
	installs string
}

func newGoTool(t *testing.T) *goTool {
	t.Helper()
	g := &goTool{t: t, m: measuredCase(t)}
	g.p, _, _ = g.m.r.Ship.Load(g.m.c.ID)
	write(t, filepath.Join(g.m.repo, "go.mod"), "module example.test/tool\n\ngo 1.24\n")
	write(t, filepath.Join(g.m.repo, "main.go"), "package main\nfunc main() { println(41) }\n")
	run(t, g.m.repo, "git", "add", "-A")
	run(t, g.m.repo, "git", "commit", "-qm", "add the tool")
	write(t, filepath.Join(g.m.repo, "main.go"), "package main\nfunc main() { println(42) }\n")
	run(t, g.m.repo, "git", "commit", "-qam", "fix the tool")
	g.landed = strings.TrimSpace(run(t, g.m.repo, "git", "rev-parse", "HEAD"))
	g.installs = filepath.Join(t.TempDir(), "tool")
	g.p.Landed, g.p.Changed, g.p.Plan.Installed = []string{g.landed}, []string{"main.go"}, g.installs
	return g
}

func (g *goTool) build() {
	g.t.Helper()
	run(g.t, g.m.repo, "go", "build", "-buildvcs=true", "-o", g.installs, ".")
}

func (g *goTool) inEffect(want bool, what string) {
	g.t.Helper()
	_, why, err := g.m.r.Ship.InEffect(context.Background(), &g.p)
	if err != nil || (why == "") != want {
		g.t.Fatalf("%s: in effect %v, want %v: %q %v", what, why == "", want, why, err)
	}
}

// A Go binary built from a tree with uncommitted edits is judged once, at the first run that sees it, by the tree it
// was built from, and the decision stands while its hash and time do - as for another installed file. The checkout
// moves on without the binary (a commit of the owner's, an edit undone), and neither says what the binary holds.
func TestAGoBuildFromAModifiedTreeIsJudgedOnceAndKeptWhileItStands(t *testing.T) {
	t.Run("a later commit does not put it out of effect", func(t *testing.T) {
		g := newGoTool(t)
		write(t, filepath.Join(g.m.repo, "notes.txt"), "an untracked note\n") // Go calls the tree modified
		g.build()
		g.inEffect(true, "the build holds the change")
		// the owner commits more work on the tool; the installed tool is unchanged and holds the change
		write(t, filepath.Join(g.m.repo, "main.go"), "package main\nfunc main() { println(43) }\n")
		write(t, filepath.Join(g.m.repo, "main_test.go"), "package main\n")
		g.p.Changed = []string{"main.go", "main_test.go", "README.md"} // an edit of its test file or a document it touched does the same
		run(t, g.m.repo, "git", "add", "-A")
		run(t, g.m.repo, "git", "commit", "-qm", "more work")
		g.inEffect(true, "the owner's later commit")
		g.inEffect(true, "and the run after it")
		if !g.p.InstalledOK || !g.p.InstalledGo || g.p.InstalledHash == "" {
			t.Fatalf("the decision: %+v", g.p)
		}
	})
	t.Run("an edit undone does not put it in effect", func(t *testing.T) {
		g := newGoTool(t)
		write(t, filepath.Join(g.m.repo, "main.go"), "package main\nfunc main() { println(41) }\n") // the old code, uncommitted
		g.build()
		g.inEffect(false, "the tool built from the old code")
		run(t, g.m.repo, "git", "checkout", "--", "main.go") // the edit is discarded; the tool is not rebuilt
		g.inEffect(false, "the edit discarded")
		if _, why, _ := g.m.r.Ship.InEffect(context.Background(), &g.p); !strings.Contains(why, "modified") || g.p.InstalledOK {
			t.Fatalf("the reason kept with the decision: %q %+v", why, g.p)
		}
		g.build()
		g.inEffect(true, "a build from the tree that holds the change")
	})
	t.Run("a file of the change touched after the build says nothing of the tree it came from", func(t *testing.T) {
		g := newGoTool(t)
		write(t, filepath.Join(g.m.repo, "notes.txt"), "an untracked note\n")
		g.build()
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(g.m.repo, "main.go"), later, later); err != nil { // the same code, written again
			t.Fatal(err)
		}
		g.inEffect(false, "main.go written after the binary")
		g.build()
		g.inEffect(false, "a rebuild before the file's time") // the file is still newer than the binary
		past := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(g.m.repo, "main.go"), past, past); err != nil {
			t.Fatal(err)
		}
		g.build()
		g.inEffect(true, "a rebuild after it")
	})
}

// The first run that sees a Go build compares the change's files with the revision the build names, not with the
// checkout's HEAD: the checkout may have moved on from the revision before that run, and then it says nothing of the
// tree the binary was built from.
func TestAModifiedTreeBuildIsComparedWithItsRevisionNotTheCheckoutsHead(t *testing.T) {
	g := newGoTool(t)
	write(t, filepath.Join(g.m.repo, "notes.txt"), "an untracked note\n")
	g.build()
	// before any run sees the binary, the owner commits other work on the tool, and its files keep their old times
	write(t, filepath.Join(g.m.repo, "main.go"), "package main\nfunc main() { println(43) }\n")
	run(t, g.m.repo, "git", "commit", "-qam", "other work")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(g.m.repo, "main.go"), past, past); err != nil {
		t.Fatal(err)
	}
	g.inEffect(false, "the checkout moved on from the binary's revision")
}

// A git error that may pass decides nothing: the build is asked again at the next run, and the rejection of an answer
// that git gave is kept.
func TestAnUndecidedGoBuildIsAskedAgain(t *testing.T) {
	g := newGoTool(t)
	g.build()
	repo := g.p.Dir
	g.p.Dir = t.TempDir() // a directory git cannot read a repository from
	_, why, err := g.m.r.Ship.InEffect(context.Background(), &g.p)
	if err != nil || !strings.Contains(why, "cannot check") || g.p.InstalledHash != "" {
		t.Fatalf("a git error: %q %v, recorded %q", why, err, g.p.InstalledHash)
	}
	g.p.Dir = repo
	g.inEffect(true, "asked again")
}

// A revision that is no commit name is never handed to git.
func TestAGoBuildsRevisionIsACommitName(t *testing.T) {
	for revision, want := range map[string]bool{
		strings.Repeat("a", 40): true, strings.Repeat("0f", 32): true,
		"--output=/x": false, "HEAD": false, strings.Repeat("a", 39): false, strings.Repeat("g", 40): false, "": false,
	} {
		if commitID.MatchString(revision) != want {
			t.Errorf("%q: %v", revision, !want)
		}
	}
}

// A revision in a binary's build information that is no commit name is never handed to git, whatever the binary says:
// it is out of effect, for good, and git runs nothing for it.
func TestAGoBuildWhoseRevisionIsNoCommitIsOutOfEffect(t *testing.T) {
	g := newGoTool(t)
	g.build()
	was := readBuildInfo
	t.Cleanup(func() { readBuildInfo = was })
	for _, revision := range []string{"--output=/x", "HEAD", g.landed[:12], ""} {
		readBuildInfo = func(io.ReaderAt) (*debug.BuildInfo, error) {
			return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "false"}}}, nil
		}
		g.p.InstalledHash = "" // a state not seen before
		_, why, err := g.m.r.Ship.InEffect(context.Background(), &g.p)
		if revision == "" { // a binary with no revision is another installed file, which the checkout decides
			if err != nil || strings.Contains(why, "no commit") {
				t.Errorf("no revision: %q %v", why, err)
			}
			continue
		}
		if err != nil || !strings.Contains(why, "no commit") || !g.p.InstalledGo || g.p.InstalledOK {
			t.Errorf("revision %q: %q %v %+v", revision, why, err, g.p)
		}
	}
}

// The change's files are compared with the build's revision by their names, as literal names: the author of a change
// names its files, and a file called ":(exclude)main.go" must not take main.go out of the comparison.
func TestTheChangesFileNamesAreNoPathspecs(t *testing.T) {
	g := newGoTool(t)
	write(t, filepath.Join(g.m.repo, "notes.txt"), "an untracked note\n")
	g.build()
	write(t, filepath.Join(g.m.repo, "main.go"), "package main\nfunc main() { println(0) }\n") // the tree now differs from the build's revision
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(g.m.repo, "main.go"), past, past); err != nil {
		t.Fatal(err)
	}
	g.p.Changed = []string{"main.go", ":(exclude)main.go"}
	g.inEffect(false, "a file name that reads as a pathspec")
}
