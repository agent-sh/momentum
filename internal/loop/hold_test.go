package loop

import (
	"context"
	"errors"
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
	open := openCase(t, st, "failures", "open", 2)
	next := openCase(t, st, "failures", "next", 1)

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
	done, err = r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil {
		t.Fatal(err)
	}
	ran = false
	for _, d := range done {
		if d.Case == held.ID && (d.Run.ID != "" || strings.Contains(d.Skipped, "refused")) {
			t.Fatalf("the held case took a turn in the next pass: %+v", d)
		}
		if d.Case == next.ID && d.Step == StepInvestigate && d.Run.ID != "" {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("the next pass did not investigate the next case: %+v", done)
	}
	if log, _ := st.CaseLog(ctx, held.ID); len(log) != lines {
		t.Fatalf("an unchanged hold wrote again: %+v", log[lines:])
	}
	notes, _ := st.Notes(ctx)
	holds := 0
	for _, n := range notes {
		if strings.HasPrefix(n.ID, holdNotePrefix(held.ID)) {
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
	held, _ = st.CaseByID(ctx, held.ID)
	picked, step, _, err = r.pick(ctx, "failures", "", false)
	if err != nil || picked.ID != held.ID || step != StepBuild {
		t.Fatalf("picked %s %s %v, want the build once its owner is admitted", picked.ID, step, err)
	}
	log, _ = st.CaseLog(ctx, held.ID)
	if why := store.HoldOf(c, log); why != "" {
		t.Fatalf("the hold did not lift: %q", why)
	}
	if standing(t, st, held.ID) != 0 {
		t.Fatal("the hold's note still stands after the hold lifted")
	}
	// the same hold again: the case is held again and its note stands again
	r.Ship.Gate.Owners = []string{"someone"}
	if picked, _, _, err = r.pick(ctx, "failures", "", false); err != nil || picked.ID != open.ID {
		t.Fatalf("held again: picked %s %v", picked.ID, err)
	}
	log, _ = st.CaseLog(ctx, held.ID)
	if why := store.HoldOf(c, log); !strings.Contains(why, "loop.owners") || standing(t, st, held.ID) != 1 {
		t.Fatalf("held again: %q, %d standing notes", why, standing(t, st, held.ID))
	}
}

func standing(t *testing.T, st *store.Store, caseID string) int {
	t.Helper()
	notes, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, note := range notes {
		if strings.HasPrefix(note.ID, holdNotePrefix(caseID)) && note.Done.IsZero() {
			n++
		}
	}
	return n
}

// Each reason the configuration gives holds the build, with that reason; a case in testing it does not hold is the
// family's build.
func TestThePolicyReasonsThatHoldABuild(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		gate       Gate
		readDir    string
	}{
		{"builds off", "builds are off", Gate{}, "momentum"},
		{"owner", "loop.owners", Gate{Owners: []string{"someone"}, RepoGuard: []string{"true"}}, "momentum"},
		{"excluded", "loop.exclude", Gate{Owners: []string{"agent-sh"}, Exclude: []string{"agent-sh/momentum"}, RepoGuard: []string{"true"}}, "momentum"},
		{"no scope check", "loop.repo_guard", Gate{Owners: []string{"agent-sh"}}, "momentum"},
		{"not a read dir", "loop.read_dirs", Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{"true"}}, "elsewhere"},
		{"allowed", "", Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{"true"}}, "momentum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			r := localRunner(t, st)
			r.Inputs.ReadDirs = []string{filepath.Join(t.TempDir(), tc.readDir)}
			tc.gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/momentum", nil }
			r.Ship = &Ship{Store: st, Dir: r.Dir, Gate: tc.gate}
			held := heldTestingCase(t, st, "held", "momentum")
			openCase(t, st, "failures", "open", 1)
			picked, step, why, err := r.pick(ctx, "failures", "", false)
			if err != nil {
				t.Fatal(err)
			}
			log, _ := st.CaseLog(ctx, held.ID)
			hold := store.HoldOf(held, log)
			if tc.want == "" {
				if picked.ID != held.ID || step != StepBuild || hold != "" {
					t.Fatalf("an allowed build was held: %s %s %q", picked.ID, why, hold)
				}
				return
			}
			if picked.ID == held.ID || !strings.Contains(hold, tc.want) || standing(t, st, held.ID) != 1 {
				t.Fatalf("picked %s (%s), hold %q, want %q", picked.ID, why, hold, tc.want)
			}
		})
	}
}

// admitBuilds lets a fixture's builds through the configuration's policy: an admitted owner, a scope check, and a
// loop.read_dirs checkout of the name the case's investigation gives.
func admitBuilds(t *testing.T, r *Runner, name string) {
	t.Helper()
	r.Ship.Gate.Owners = []string{"agent-sh"}
	r.Ship.Gate.RepoGuard = []string{"true"}
	r.Ship.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/" + name, nil }
	r.Inputs.ReadDirs = append(r.Inputs.ReadDirs, filepath.Join(t.TempDir(), name))
}

// A changed reason replaces the hold and its note; a checkout that cannot be read leaves the hold as it is; a case
// that leaves testing ends its hold, and its return records the hold again with a standing note.
func TestAHoldFollowsItsReasonAndItsCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Inputs.ReadDirs = []string{filepath.Join(t.TempDir(), "momentum")}
	resolveErr := error(nil)
	r.Ship = &Ship{Store: st, Dir: r.Dir, Gate: Gate{RepoGuard: []string{"true"},
		Resolve: func(context.Context, string) (string, error) { return "agent-sh/momentum", resolveErr }}}
	held := heldTestingCase(t, st, "held", "momentum")
	holdNow := func() string {
		t.Helper()
		if _, _, _, err := r.pick(ctx, "failures", "", false); err != nil {
			t.Fatal(err)
		}
		c, _ := st.CaseByID(ctx, held.ID)
		log, _ := st.CaseLog(ctx, held.ID)
		return store.HoldOf(c, log)
	}
	if why := holdNow(); !strings.Contains(why, "builds are off") || standing(t, st, held.ID) != 1 {
		t.Fatalf("builds off: %q", why)
	}
	r.Ship.Gate.Owners = []string{"someone"}
	if why := holdNow(); !strings.Contains(why, "loop.owners") || standing(t, st, held.ID) != 1 {
		t.Fatalf("a changed reason: %q, %d standing", why, standing(t, st, held.ID))
	}
	resolveErr = errors.New("not a git checkout")
	if picked, _, _, err := r.pick(ctx, "failures", "", false); err != nil || picked.ID == held.ID {
		t.Fatalf("an unreadable checkout took the slot: %s %v", picked.ID, err)
	}
	if why := holdNow(); !strings.Contains(why, "cannot be read: not a git checkout") || standing(t, st, held.ID) != 1 {
		t.Fatalf("an unreadable checkout is no hold: %q, %d standing", why, standing(t, st, held.ID))
	}
	resolveErr = refused("origin is not a GitHub repository")
	if why := holdNow(); !strings.Contains(why, "not a GitHub repository") || standing(t, st, held.ID) != 1 {
		t.Fatalf("a refused origin: %q", why)
	}
	resolveErr = nil
	if _, err := st.UpdateCase(ctx, held.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if why := holdNow(); why != "" || standing(t, st, held.ID) != 0 {
		t.Fatalf("a case out of testing kept its hold: %q, %d standing", why, standing(t, st, held.ID))
	}
	if _, err := st.UpdateCase(ctx, held.ID, store.CaseUpdate{Status: store.CaseTesting}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if why := holdNow(); !strings.Contains(why, "loop.owners") || standing(t, st, held.ID) != 1 {
		t.Fatalf("the return to testing: %q, %d standing", why, standing(t, st, held.ID))
	}
}

// A case waiting for a repository is held with its own note, which goes when the hold lifts (its investigation
// names a repository the configuration allows) and when the case leaves testing.
func TestTheWaitingNoteGoesWithItsHold(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	admitBuilds(t, r, "momentum")
	c := openCase(t, st, "failures", "waiting", 1)
	mechanism, fix := "one cause", "one fix"
	c, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	waiting := func() (string, bool) {
		t.Helper()
		if _, _, _, err := r.pick(ctx, "failures", "", false); err != nil {
			t.Fatal(err)
		}
		got, _ := st.CaseByID(ctx, c.ID)
		log, _ := st.CaseLog(ctx, c.ID)
		notes, _ := st.Notes(ctx)
		for _, n := range notes {
			if n.ID == repoNoteID(c.ID) {
				return store.HoldOf(got, log), n.Done.IsZero()
			}
		}
		return store.HoldOf(got, log), false
	}
	if why, stands := waiting(); !strings.Contains(why, "waits for a repository") || !stands {
		t.Fatalf("waiting: %q, note standing %v", why, stands)
	}
	setCaseRepo(t, st, c, "momentum")
	if why, stands := waiting(); why != "" || stands {
		t.Fatalf("a repository named: %q, note standing %v", why, stands)
	}
	// waiting again, then out of testing: the note goes with the case
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting}, "fixture"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, st, c, "")
	if why, stands := waiting(); !strings.Contains(why, "waits for a repository") || !stands {
		t.Fatalf("waiting again: %q, note standing %v", why, stands)
	}
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, stands := waiting(); stands {
		t.Fatal("the waiting note stands after the case left testing")
	}
}
