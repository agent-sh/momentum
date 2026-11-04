package loop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

func heldTestingCase(t *testing.T, st *store.Store, sig, repo string) store.Case {
	t.Helper()
	c := openCase(t, st, "failures", sig, 5)
	mechanism, fix := "one cause", "one fix"
	c, err := st.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, st, c, repo)
	return c
}

// A case whose build the configuration holds (builds off: loop.owners is empty) takes no slot: the pass investigates
// the family's open case instead, the held case shows its hold, and the next pass writes no new line for it.
func TestAHeldBuildLeavesItsFamilysSlotToAnOpenCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, investigatedResult))
	r.Ship = &Ship{Store: st, Dir: r.Dir} // loop.owners is empty
	held := heldTestingCase(t, st, "held", "momentum")
	open := openCase(t, st, "failures", "open", 1)

	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	for _, d := range done {
		if d.Case == held.ID && (d.Run.ID != "" || strings.Contains(d.Skipped, "refused")) {
			t.Fatalf("the held case took a turn: %+v", d)
		}
		if d.Case == open.ID && d.Step == StepInvestigate && d.Run.ID != "" {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("the open case was not investigated: %+v", done)
	}
	if got, _ := st.CaseByID(ctx, open.ID); got.Status == store.CaseOpen {
		t.Fatalf("the open case's investigation was not recorded: %+v", got)
	}
	log, err := st.CaseLog(ctx, held.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.CaseByID(ctx, held.ID)
	if why := store.HoldOf(c, log); !strings.Contains(why, "builds are off") {
		t.Fatalf("the held case does not show its hold: %q", why)
	}
	lines := len(log)

	// the next pass works the family's next case and leaves the held one as it was
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if log, _ := st.CaseLog(ctx, held.ID); len(log) != lines {
		t.Fatalf("an unchanged hold wrote again: %+v", log[lines:])
	}
	notes, _ := st.Notes(ctx)
	holds := 0
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "loop-build:"+held.ID+":held:") {
			holds++
		}
	}
	if holds != 1 {
		t.Fatalf("the hold's problem note: %d, want 1", holds)
	}
}

// An owner the loop may not write to holds the build the same way, and the hold lifts with the configuration.
func TestAnOwnerNotAdmittedHoldsTheBuildUntilTheConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	src := filepath.Join(t.TempDir(), "momentum")
	r.Inputs.ReadDirs = []string{src}
	r.Ship = &Ship{Store: st, Dir: r.Dir, Gate: Gate{Owners: []string{"someone"}, RepoGuard: []string{"true"},
		Resolve: func(context.Context, string) (string, error) { return "other/momentum", nil }}}
	held := heldTestingCase(t, st, "held", "momentum")
	open := openCase(t, st, "failures", "open", 1)
	picked, step, _, err := r.pick(ctx, "failures", "", false)
	if err != nil || picked.ID != open.ID || step != StepInvestigate {
		t.Fatalf("picked %s %s %v, want the open case", picked.ID, step, err)
	}
	log, _ := st.CaseLog(ctx, held.ID)
	c, _ := st.CaseByID(ctx, held.ID)
	if why := store.HoldOf(c, log); !strings.Contains(why, "loop.owners") {
		t.Fatalf("hold: %q", why)
	}
	r.Ship.Gate.Owners = []string{"other"}
	picked, step, _, err = r.pick(ctx, "failures", "", false)
	if err != nil || picked.ID != held.ID || step != StepBuild {
		t.Fatalf("picked %s %s %v, want the build once its owner is admitted", picked.ID, step, err)
	}
	log, _ = st.CaseLog(ctx, held.ID)
	if why := store.HoldOf(c, log); why != "" {
		t.Fatalf("the hold did not lift: %q", why)
	}
	notes, _ := st.Notes(ctx)
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "loop-build:"+held.ID+":held:") && n.Done.IsZero() {
			t.Fatalf("the hold's note still stands: %s", n.ID)
		}
	}
}
