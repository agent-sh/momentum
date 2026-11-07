package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A loop directory given as a relative path (a relative MOMENTUM_HOME) is the process's working directory's: the
// loop's runs, which record absolute working directories, are its own by InDir and by the counts' SQL alike.
func TestARelativeLoopDirectoryIsItsAbsolutePath(t *testing.T) {
	ctx := context.Background()
	st := caseStore(t)
	home := t.TempDir()
	t.Chdir(home)
	abs := filepath.Join(home, "loop")
	if got := DirPaths("loop"); !slices.Contains(got, abs) {
		t.Fatalf("the paths of a relative loop directory: %v, not %s", got, abs)
	}
	run := filepath.Join(abs, "runs", "r1")
	if !InDir(run, "loop") {
		t.Fatalf("a run under the loop's directory %s is not its own by a relative path", run)
	}
	e := Event{Source: "zz", SourceKey: "run", TS: time.Now(), Actor: ActorTool, Kind: "tool_result", Cwd: run}
	if _, err := st.Put(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	if evs, err := st.Events(ctx, Query{Source: "zz", ExcludeCwd: "loop", Limit: 10}); err != nil || len(evs) != 0 {
		t.Fatalf("the counts' SQL kept the loop's run by a relative path: %+v %v", evs, err)
	}
}

// A working directory is stored cleaned, so the counts' SQL compares the form InDir compares: a run's directory
// recorded with a doubled slash, a dot and a trailing slash is the loop's own in both.
func TestAWorkingDirectoryIsCleanedAtIngest(t *testing.T) {
	ctx := context.Background()
	st := caseStore(t)
	e := Event{Source: "zz", SourceKey: "run", TS: time.Now(), Actor: ActorTool, Kind: "tool_result", Cwd: "/zz/state//loop/./runs/r1/"}
	if _, err := st.Put(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.EventByID(ctx, e.ID())
	if err != nil || !found || got.Cwd != "/zz/state/loop/runs/r1" {
		t.Fatalf("the stored working directory: %q %v %v", got.Cwd, found, err)
	}
	if !InDir(got.Cwd, "/zz/state/loop") {
		t.Fatal("InDir does not count the run as the loop's own")
	}
	if evs, err := st.Events(ctx, Query{Source: "zz", ExcludeCwd: "/zz/state/loop", Limit: 10}); err != nil || len(evs) != 0 {
		t.Fatalf("the counts' SQL kept a run InDir counts as the loop's own: %+v %v", evs, err)
	}
}

// changed is a case in testing whose change went in an hour after it was opened.
func changed(t *testing.T, st *Store, sig string) Case {
	t.Helper()
	ctx := context.Background()
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: sig, Title: "t", Evidence: refsAt(sig, 2, time.Now().Add(-48*time.Hour))})
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: ptr("m"), NextTest: ptr("n")}, "agent"); err != nil {
		t.Fatal(err)
	}
	c, err := st.DeployCase(ctx, c.ID, "/l/packages/"+sig, time.Now().Add(-40*24*time.Hour), "landed", "loop:ship")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Evidence that is not an event never brings a case closed as guarded back: the measure step's watch reads only its
// events. The case's log says so, and names case reopen, which brings it back by hand.
func TestAGuardedCaseSaysOnlyEventsBringItBack(t *testing.T) {
	ctx := context.Background()
	st := caseStore(t)
	c := changed(t, st, "g")
	if _, err := st.StartGuarding(ctx, c.ID, 30*24*time.Hour, 4, 684, c.Deployed, "a watch", "loop:measure"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseGuarded(ctx, c.ID, 4, 684, 125, 0.5, "guarded, not measured", "loop:measure"); err != nil {
		t.Fatal(err)
	}
	got, obs, err := st.AddCaseEvidence(ctx, c.ID, refsAt("later", 1, time.Now().Add(-time.Minute)), "agent")
	if err != nil || obs != KeptShut || got.Status != CaseClosed {
		t.Fatalf("a file reference after a guarded close: %s %s %v", obs, got.Status, err)
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l := log[len(log)-1]; l.Action != "kept closed" || !strings.Contains(l.Detail, "evidence that is no event does not") || !strings.Contains(l.Detail, "case reopen") {
		t.Fatalf("its log line: %+v", l)
	}
}

// A case closed as improved that comes back keeps its last measurement, which case show shows: regressed by the
// measure tool, closed by hand, then reopened by hand, and through a next change rolled back while it was deployed,
// before its own measurement started. A guarded case that comes back loses its guard's columns, and its incidents,
// watch and words with them, which would read as a measurement; a rollback of a measuring change takes every
// measurement column, its own.
func TestAComebackKeepsTheLastMeasurementButNoGuard(t *testing.T) {
	ctx := context.Background()
	st := caseStore(t)
	c := changed(t, st, "i")
	if _, err := st.StartMeasuring(ctx, c.ID, 7*24*time.Hour, 6, c.Deployed, "loop:measure"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseMeasured(ctx, c.ID, 1, "6 of 100 before, 1 of 100 after", "loop:measure"); err != nil {
		t.Fatal(err)
	}
	want := Measurement{Window: 7 * 24 * time.Hour, Before: 6, After: 1, Effect: "6 of 100 before, 1 of 100 after"}
	got, err := st.RegressCase(ctx, c.ID, "back at its old share", "loop:measure")
	if err != nil || got.Measurement != want {
		t.Fatalf("regressed: %+v %v", got.Measurement, err)
	}
	if got, err = st.CloseCase(ctx, c.ID, NotWorthIt, "", "not worth another change", "agent"); err != nil || got.Measurement != want {
		t.Fatalf("closed by hand: %+v %v", got.Measurement, err)
	}
	if got, err = st.ReopenCase(ctx, c.ID, refsAt("again", 1, time.Now().Add(-time.Minute)), "it happened again", "agent"); err != nil || got.Measurement != want {
		t.Fatalf("reopened: %+v %v", got.Measurement, err)
	}
	// its next change, rolled back before its measurement starts, takes nothing of the last one with it
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: ptr("m"), NextTest: ptr("n")}, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeployCase(ctx, c.ID, "/l/packages/i2", time.Now().Add(-2*time.Hour), "landed", "loop:ship"); err != nil {
		t.Fatal(err)
	}
	if got, err = st.RollBackCase(ctx, c.ID, "its revert went in", "loop:ship"); err != nil || got.Measurement != want {
		t.Fatalf("its next change rolled back while deployed: %+v %v", got.Measurement, err)
	}
	// the change after that starts a watch of its own: nothing of the last measurement reads as a recurrence
	if _, err := st.DeployCase(ctx, c.ID, "/l/packages/i3", time.Now().Add(-time.Hour), "landed", "loop:ship"); err != nil {
		t.Fatal(err)
	}
	if got, err = st.StartGuarding(ctx, c.ID, 30*24*time.Hour, 4, 684, time.Now().Add(-time.Hour), "a watch", "loop:measure"); err != nil ||
		got.Measurement != (Measurement{Window: 30 * 24 * time.Hour, Before: 4, Exposure: 684}) {
		t.Fatalf("the next change's watch: %+v %v", got.Measurement, err)
	}

	g := changed(t, st, "g")
	if _, err := st.StartGuarding(ctx, g.ID, 30*24*time.Hour, 4, 684, g.Deployed, "a watch", "loop:measure"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseGuarded(ctx, g.ID, 4, 684, 125, 0.5, "guarded, not measured", "loop:measure"); err != nil {
		t.Fatal(err)
	}
	if got, err = st.RegressCase(ctx, g.ID, "its problem came back", "loop:measure"); err != nil || got.Measurement != (Measurement{}) {
		t.Fatalf("a guarded comeback: %+v %v", got.Measurement, err)
	}

	r := changed(t, st, "r")
	if _, err := st.StartMeasuring(ctx, r.ID, 7*24*time.Hour, 6, r.Deployed, "loop:measure"); err != nil {
		t.Fatal(err)
	}
	if got, err = st.RollBackCase(ctx, r.ID, "no measured effect", "loop:ship"); err != nil || got.Measurement != (Measurement{}) {
		t.Fatalf("rolled back: %+v %v", got.Measurement, err)
	}
}
