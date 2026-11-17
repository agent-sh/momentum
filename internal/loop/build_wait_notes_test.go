package loop

import (
	"context"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// waitingBuild is a case in testing whose build waited at its preparation for two causes.
func waitingBuild(t *testing.T, st *store.Store, r *Runner, signature string) store.Case {
	t.Helper()
	ctx := context.Background()
	c := openCase(t, st, "failures", signature, 1)
	mechanism, fix := "one cause", "one fix"
	c, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, cause := range []string{"the repository is in use", "the scope check could not decide"} {
		if err := r.noteBuildSkip(ctx, c, "prepare", cause); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// standingOf are a case's standing build notes.
func standingOf(t *testing.T, st *store.Store, caseID string) (out []store.Note) {
	t.Helper()
	notes, _ := standingNotes(t, st)
	for _, n := range notes {
		if buildWaitNote(caseID, n) || n.ID == capacityNoteID(caseID) {
			out = append(out, n)
		}
	}
	return out
}

// A build that waited at a stage leaves a note for each cause. A case that leaves testing builds no more, so its
// notes end with it: a closed case's stood in the report for good. Another case's notes stay.
func TestABuildsWaitNotesEndWhenItsCaseLeavesTesting(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	gone, stays := waitingBuild(t, st, r, "wait-leaves"), waitingBuild(t, st, r, "wait-stays")
	if n := standingOf(t, st, gone.ID); len(n) != 2 {
		t.Fatalf("the waits' notes: %+v", n)
	}
	if _, err := st.UpdateCase(ctx, gone.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.releaseHolds(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if n := standingOf(t, st, gone.ID); len(n) != 0 {
		t.Fatalf("the case left testing and its build's wait notes stand: %+v", n)
	}
	if n := standingOf(t, st, stays.ID); len(n) != 2 {
		t.Fatalf("a case still in testing lost its build's wait notes: %+v", n)
	}
}

// A build's run that ended with nothing holding it got past its waits: their notes end, and a note with an end of
// its own (the capacity gate's) is left to it.
func TestABuildsWaitNotesEndWhenItsRunEndsUnheld(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := waitingBuild(t, st, r, "wait-ran")
	if err := r.noteCapacityHold(ctx, c, "the capacity gate: no room"); err != nil {
		t.Fatal(err)
	}
	run, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
	if why != "" {
		t.Fatal(why)
	}
	done := r.completeStep(ctx, c, run, Result{Started: true, CostKnown: true, Status: store.RunFailed, Failure: "build refused"}, nil)
	if done.Skipped != "" {
		t.Fatalf("the run was held: %+v", done)
	}
	left := standingOf(t, st, c.ID)
	if len(left) != 1 || left[0].ID != capacityNoteID(c.ID) {
		t.Fatalf("after a run nothing held: %+v", left)
	}
}

// A wait the loop ended comes back for the same cause: its note stands again. One a reader resolved stays resolved.
func TestAWaitThatComesBackForTheSameCausePutsItsNoteAgain(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := waitingBuild(t, st, r, "wait-recurs")
	before := standingOf(t, st, c.ID)
	if len(before) == 0 {
		t.Fatalf("the first wait left no note: %+v", before)
	}
	cause := before[0].Detail
	wait := func() {
		t.Helper()
		if err := r.noteBuildSkip(ctx, c, "prepare", cause); err != nil {
			t.Fatal(err)
		}
	}
	// a run of the build ends unheld, and the next build waits for the same cause
	run, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
	if why != "" {
		t.Fatal(why)
	}
	r.completeStep(ctx, c, run, Result{Started: true, CostKnown: true, Status: store.RunFailed, Failure: "build run"}, nil)
	if n := standingOf(t, st, c.ID); len(n) != 0 {
		t.Fatalf("after the unheld run: %+v", n)
	}
	wait()
	if n := standingOf(t, st, c.ID); len(n) != 1 || n[0].Detail != cause {
		t.Fatalf("the same wait after an unheld run: %+v", n)
	}
	// the case leaves testing and comes back to the same wait
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.releaseHolds(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if n := standingOf(t, st, c.ID); len(n) != 0 {
		t.Fatalf("after the case left testing: %+v", n)
	}
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting}, "fixture"); err != nil {
		t.Fatal(err)
	}
	wait()
	n := standingOf(t, st, c.ID)
	if len(n) != 1 || n[0].Detail != cause {
		t.Fatalf("the same wait after the case came back: %+v", n)
	}
	// a reader resolves it: the same wait does not put it again
	if _, _, err := st.DoneNote(ctx, n[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	wait()
	if n := standingOf(t, st, c.ID); len(n) != 0 {
		t.Fatalf("a note its reader resolved came back: %+v", n)
	}
}
