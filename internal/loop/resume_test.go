package loop

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A stop holds until its cause is fixed: closing the component-health case of the stop as already solved, naming
// what fixed it, resumes every case the stop rule held on its signature. Each keeps its status and episode, the
// stop rule counts only its runs after the resume, the next pass works it again, and a case stopped another way
// stays stopped. A fix that does not hold brings the component-health case back.
func TestAFixOfTheStopResumesTheCasesItHeld(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	n := 0
	fail := func(c store.Case, step, failure string, ago time.Duration) {
		t.Helper()
		n++
		at := time.Now().Add(-ago)
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: "run-" + string(rune('a'+n)), Case: c.ID, Family: c.Family, Step: step,
			Started: at, Ended: at.Add(time.Second), Status: store.RunFailed, Failure: failure}); err != nil {
			t.Fatal(err)
		}
	}
	a := openCase(t, st, "failures", "a", 3)
	b := openCase(t, st, "failures", "b", 2)
	other := openCase(t, st, "failures", "other", 2)
	mech, fix := "the export reads the lockfile first", "take the lock first"
	b, err := st.UpdateCase(ctx, b.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &fix}, "test")
	if err != nil {
		t.Fatal(err)
	}
	fail(a, StepInvestigate, "turns", 4*time.Minute)
	fail(a, StepInvestigate, "turns", 3*time.Minute)
	fail(b, StepInvestigate, "turns", 4*time.Minute)
	fail(b, StepInvestigate, "turns", 3*time.Minute)
	fail(other, StepInvestigate, "timeout", 4*time.Minute)
	fail(other, StepInvestigate, "timeout", 3*time.Minute)
	for _, c := range []store.Case{a, b, other} {
		if why, err := r.stopped(ctx, c, false); err != nil || why == "" {
			t.Fatalf("%s is not stopped: %q %v", c.ID, why, err)
		}
	}
	health, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(health) != 2 {
		t.Fatalf("the loop's own cases: %+v %v", health, err)
	}
	var turns store.Case
	for _, h := range health {
		if strings.Contains(h.Title, "turns") {
			turns = h
		}
	}
	if turns.ID == "" || turns.Evidence != 4 {
		t.Fatalf("the stop on turns: %+v", turns)
	}
	if got, _, why, err := r.pick(ctx, "failures", "", true); err != nil || got.ID != "" {
		t.Fatalf("a stopped family worked %s: %s %v", got.ID, why, err)
	}

	// closed without naming its fix, the stop would hold on: refused
	if _, err := st.CloseCase(ctx, turns.ID, store.Solved, "", "fixed", "cli"); !errors.Is(err, store.ErrBadCase) || !strings.Contains(err.Error(), "--fix") {
		t.Fatalf("a component-health case closed as solved without its fix: %v", err)
	}
	stopped, err := StoppedOn(ctx, st, turns)
	if err != nil || len(stopped) != 2 || !slices.Contains(stopped, a.ID) || !slices.Contains(stopped, b.ID) {
		t.Fatalf("the cases the stop on turns holds: %v %v", stopped, err)
	}
	if _, _, err := st.CloseFixed(ctx, a.ID, "git:agent-sh/momentum@4814cc4", stopped, "the cap was raised", "cli"); !errors.Is(err, store.ErrBadCase) {
		t.Fatalf("a failures case named a fix: %v", err)
	}
	if _, _, err := st.CloseFixed(ctx, turns.ID, "commit 4814cc4", stopped, "the cap was raised", "cli"); !errors.Is(err, store.ErrBadCase) {
		t.Fatalf("a fix that is no reference: %v", err)
	}
	if _, err := st.PutNote(ctx, store.Note{ID: store.ReviewNoteID(a.ID, "stopped"), Kind: store.NoteProblem, Importance: store.ImportanceLow,
		At: time.Now(), Title: "The loop's review for case " + a.ID + " waits: stopped", Detail: "its last two review runs failed the same way: report"}); err != nil {
		t.Fatal(err)
	}
	// late has failed once, and its second run is still running at the close: the close does not see it held
	late := openCase(t, st, "failures", "late", 2)
	fail(late, StepInvestigate, "turns", 3*time.Minute)
	// mixed has failed once before the close, and fails again after it
	mixed := openCase(t, st, "failures", "mixed", 2)
	fail(mixed, StepInvestigate, "turns", 3*time.Minute)
	running := time.Now().Add(-time.Minute)
	time.Sleep(5 * time.Millisecond)
	closed, resumed, err := st.CloseFixed(ctx, turns.ID, "git:agent-sh/momentum@4814cc4", stopped, "the cap was raised", "cli")
	if err != nil || closed.Status != store.CaseClosed || closed.Reason != store.Solved || len(resumed) != 2 {
		t.Fatalf("the close: %+v %v %v", closed, resumed, err)
	}
	log, _ := st.CaseLog(ctx, turns.ID)
	if last := log[len(log)-1]; !strings.Contains(last.Detail, "fixed by git:agent-sh/momentum@4814cc4; resumes ") {
		t.Fatalf("the close's line: %+v", last)
	}
	for _, c := range []store.Case{a, b} {
		got, _ := st.CaseByID(ctx, c.ID)
		if got.Status != c.Status || got.Episode != c.Episode {
			t.Errorf("%s resumed as %s, episode %d (was %s, %d)", c.ID, got.Status, got.Episode, c.Status, c.Episode)
		}
		log, _ := st.CaseLog(ctx, c.ID)
		if last := log[len(log)-1]; last.Action != store.ActionResumed || !strings.Contains(last.Detail, turns.ID) || !strings.Contains(last.Detail, "4814cc4") {
			t.Errorf("%s's resume line: %+v", c.ID, last)
		}
		if why, err := r.stopped(ctx, got, true); err != nil || why != "" {
			t.Errorf("%s is still stopped: %q %v", c.ID, why, err)
		}
	}
	if why, err := r.stopped(ctx, other, true); err != nil || why == "" {
		t.Fatalf("the case stopped another way resumed: %v", err)
	}
	if n, ok, _ := st.Note(ctx, store.ReviewNoteID(a.ID, "stopped")); !ok || n.Done.IsZero() {
		t.Fatalf("the resumed case's review note stays: %+v", n)
	}
	// its run started before the close and ends after it, failing the same way: the case resumes as of the close
	time.Sleep(5 * time.Millisecond)
	n++
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "run-late", Case: late.ID, Family: late.Family, Step: StepInvestigate,
		Started: running, Ended: time.Now(), Status: store.RunFailed, Failure: "turns"}); err != nil {
		t.Fatal(err)
	}
	if why, err := r.stopped(ctx, late, true); err != nil || why != "" {
		t.Fatalf("a dry check holds the late stop: %q %v", why, err)
	}
	if why, err := r.stopped(ctx, late, false); err != nil || why != "" {
		t.Fatalf("the late stop held its case: %q %v", why, err)
	}
	log, _ = st.CaseLog(ctx, late.ID)
	if last := log[len(log)-1]; last.Action != store.ActionResumed || !strings.Contains(last.Detail, "fixed by git:agent-sh/momentum@4814cc4, while this stop's runs were under way") ||
		last.At.UnixMilli() != closed.Closed.UnixMilli() {
		t.Fatalf("the late case's resume line, as of the close at %v: %+v", closed.Closed, last)
	}
	if h, _ := st.CaseByID(ctx, turns.ID); h.Status != store.CaseClosed || h.Evidence != turns.Evidence {
		t.Fatalf("the late stop changed the closed case: %s, %d evidence", h.Status, h.Evidence)
	}
	if why, err := r.stopped(ctx, late, false); err != nil || why != "" {
		t.Fatalf("the late case is held after its resume: %q %v", why, err)
	}
	if got, step, why, err := r.pick(ctx, "failures", "", true); err != nil || got.ID != a.ID || step != StepInvestigate {
		t.Fatalf("the next pass: %s %s: %s %v", got.ID, step, why, err)
	}
	if _, _, err := st.CloseFixed(ctx, turns.ID, "git:agent-sh/momentum@4814cc4", nil, "again", "cli"); !errors.Is(err, store.ErrBadCase) {
		t.Fatalf("a closed case closed again: %v", err)
	}

	// a resumed case's next failure does not stop it
	time.Sleep(5 * time.Millisecond)
	fail(a, StepInvestigate, "turns", 0)
	if why, err := r.stopped(ctx, a, true); err != nil || why != "" {
		t.Fatalf("one failure after the resume stopped it: %q %v", why, err)
	}
	// the fix did not hold: mixed fails after the close, resumes as of it, and its next failure is the second after
	// the fix, which stops it and brings the component-health case back
	time.Sleep(5 * time.Millisecond)
	fail(mixed, StepInvestigate, "turns", 0)
	if why, err := r.stopped(ctx, mixed, false); err != nil || why != "" {
		t.Fatalf("one failure after the fix stopped mixed: %q %v", why, err)
	}
	time.Sleep(5 * time.Millisecond)
	fail(mixed, StepInvestigate, "turns", 0)
	if why, err := r.stopped(ctx, mixed, false); err != nil || why == "" {
		t.Fatalf("two failures after the fix did not stop mixed: %v", err)
	}
	back, _ := st.CaseByID(ctx, turns.ID)
	log, _ = st.CaseLog(ctx, turns.ID)
	if back.Status != store.CaseOpen || log[len(log)-1].Action != "regressed" {
		t.Fatalf("the fix that did not hold: %s, %+v", back.Status, log[len(log)-1])
	}
	// a stop after the resume puts the review's stopped note again
	if err := r.noteReviewSkip(ctx, a, "stopped", "its last two review runs failed the same way: report"); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := st.Note(ctx, store.ReviewNoteID(a.ID, "stopped")); !ok || !n.Done.IsZero() {
		t.Fatalf("a stop after the resume left the review note done: %+v", n)
	}
}
