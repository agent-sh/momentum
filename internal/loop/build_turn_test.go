package loop

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// turnRunner is a live runner with one testing case in each family, in that order, whose builds the host refuses
// after their reservation (a toolchain that is absent), so each build that takes the pass's slot fails.
func turnRunner(t *testing.T, families ...string) (*Runner, []store.Case) {
	t.Helper()
	fakeManager(t)
	r, _ := inputRunner(t) // its case is in failures, open, and no family here has failures in it
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	r.Toolchains.ReadOnly = []string{filepath.Join(t.TempDir(), "absent")}
	r.Families = families
	ctx := context.Background()
	mechanism, fix := "One cause", "One test"
	var cs []store.Case
	for _, fam := range families {
		c := openCase(t, r.Store, fam, "marker-7f3a-"+fam, 1)
		c, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		setCaseRepo(t, r.Store, c, "main")
		cs = append(cs, c)
	}
	return r, cs
}

// builtIn runs one pass and returns the case whose build took its slot, and the cases another case's build skipped.
func builtIn(t *testing.T, r *Runner) (built store.LoopRun, skipped []string) {
	t.Helper()
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range done {
		switch {
		case d.Step == StepBuild && d.Run.ID != "" && d.Skipped == "": // a refused reservation has a run too
			if built.ID != "" {
				t.Fatalf("two builds in one pass: %+v", done)
			}
			built = d.Run
		case d.Step == StepBuild && d.Skipped == "another case builds in this pass":
			skipped = append(skipped, d.Case)
		}
	}
	if built.ID == "" {
		t.Fatalf("no case built: %+v", done)
	}
	return built, skipped
}

// A case that built last pass and failed yields the pass's build to another family's testing case that waits.
func TestAFailedBuildYieldsToAnotherFamilysWaitingCase(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	first, second := cs[0], cs[1]
	built, skipped := builtIn(t, r)
	if built.Case != first.ID || len(skipped) != 1 || skipped[0] != second.ID {
		t.Fatalf("the first pass did not build the first family's case: %s, skipped %v", built.Case, skipped)
	}
	run, err := r.Store.LoopRunByID(context.Background(), built.ID)
	if err != nil || run.Status != store.RunFailed {
		t.Fatalf("the first build did not fail: %+v %v", run, err)
	}
	built, skipped = builtIn(t, r)
	if built.Case != second.ID || len(skipped) != 1 || skipped[0] != first.ID {
		t.Fatalf("the case that failed last pass built again while another family's case waited: %s, skipped %v", built.Case, skipped)
	}
}

// Each pass's build goes to the family after the one that built last, round the families.
func TestBuildTurnsRotateAcrossFamilies(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions", "manual-work")
	for pass, want := range []store.Case{cs[0], cs[1], cs[2], cs[0]} {
		built, skipped := builtIn(t, r)
		if built.Case != want.ID || len(skipped) != 2 {
			t.Fatalf("pass %d built %s (%s), skipped %v; want %s (%s)", pass+1, built.Case, built.Family, skipped, want.ID, want.Family)
		}
	}
	if turn, err := r.Store.LoopTurn(context.Background(), StepBuild); err != nil || turn != cs[0].Family {
		t.Fatalf("the store names %q as the family that built last, want %q: %v", turn, cs[0].Family, err)
	}
}

// A family alone takes the build of every pass: there is no other family's case to yield to.
func TestASingleFamilyBuildsEveryPass(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	for pass := range 2 {
		built, skipped := builtIn(t, r)
		if built.Case != cs[0].ID || len(skipped) != 0 {
			t.Fatalf("pass %d built %s, skipped %v", pass+1, built.Case, skipped)
		}
		if turn, err := r.Store.LoopTurn(context.Background(), StepBuild); err != nil || turn != "sessions" {
			t.Fatalf("pass %d recorded the build turn of %q: %v", pass+1, turn, err)
		}
	}
}

// A build held at its preparation takes no turn: the next family's case builds in the same pass and takes it.
func TestAHeldBuildPassesThePassBuildToTheNextFamily(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	calls := 0
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
		if calls++; calls == 1 { // the first family's preparation, in turn order
			return "marker-7f3a busy", nil
		}
		return "", nil
	}
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var built string
	held := false
	for _, d := range done {
		switch {
		case d.Step == StepBuild && d.Run.ID != "" && d.Skipped == "":
			built = d.Case
		case d.Case == cs[0].ID && d.Step == StepBuild && d.Run.ID == "" && strings.Contains(d.Skipped, "marker-7f3a busy"):
			held = true
		}
	}
	if !held || built != cs[1].ID {
		t.Fatalf("the held build did not pass the pass's build on: held %v, built %s: %+v", held, built, done)
	}
	if turn, err := r.Store.LoopTurn(context.Background(), StepBuild); err != nil || turn != cs[1].Family {
		t.Fatalf("the store names %q as the family that built last, want %q: %v", turn, cs[1].Family, err)
	}
}

// With room for one run a pass, the build and the run turn take the passes in turn, though every build fails before
// its agent starts (an absent toolchain: a run recorded unstarted at no cost, which the stop rule does not count): the
// other family's investigation runs every other pass. A store with no single turn yet serves the run turn first.
func TestARoomForOneRunAlternatesTheBuildAndTheRunTurn(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"owner-burden", "sessions"}
	r.Budgets.Day = 1.5 * r.Budgets.Run
	for pass, build := range []bool{false, true, false, true} {
		done, err := r.Once(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		built, whyBuild := turnStep(done, cs[0].ID, StepBuild)
		ran, why := turnStep(done, open.ID, StepInvestigate)
		if built != build || ran == build {
			t.Fatalf("pass %d: built %v (%q), investigated %v (%q); want the build %v", pass+1, built, whyBuild, ran, why, build)
		}
		if !build && whyBuild != runsTurn {
			t.Fatalf("pass %d: the build waits with %q, want %q", pass+1, whyBuild, runsTurn)
		}
	}
}

// With room for one run, a pass that serves the run turn and finds nothing to run gives the run to the build.
func TestARoomForOneRunWithNothingForTheRunTurnBuilds(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	r.Budgets.Day = 1.5 * r.Budgets.Run
	for pass := range 3 {
		done, err := r.Once(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
			t.Fatalf("pass %d: the build did not run though nothing else could: %q", pass+1, why)
		}
	}
}

// With room for two runs or more and three families, the run turn moves to the first family whose run, other than the
// build, a pass reserved, and the store keeps it.
func TestTheRunTurnMovesToThePassesFirstRun(t *testing.T) {
	fakeManager(t)
	r, first := inputRunner(t)
	second := openCase(t, r.Store, "sessions", "marker-7f3a-sessions", 1)
	third := openCase(t, r.Store, "manual-work", "marker-7f3a-manual", 1)
	r.Families = []string{first.Family, second.Family, third.Family}
	ctx := context.Background()
	for pass, want := range []string{first.Family, second.Family} {
		done, err := r.Once(ctx, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []store.Case{first, second, third} {
			if ran, why := turnStep(done, c.ID, StepInvestigate); !ran {
				t.Fatalf("pass %d: %s did not investigate: %q", pass+1, c.Family, why)
			}
		}
		if turn, err := r.Store.LoopTurn(ctx, turnRun); err != nil || turn != want {
			t.Fatalf("pass %d: the store names %q as the run turn, want %q: %v", pass+1, turn, want, err)
		}
	}
}

// The family whose turn it is to build takes the first place whatever the room, so a room that shrinks from pass to
// pass (3, 2, then 1 run) still passes the build from the family that built and failed to the family that waits.
func TestTheBuildTurnsFamilyBuildsWhateverTheRoom(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	ctx := context.Background()
	openCase(t, r.Store, "manual-work", "marker-7f3a-mw", 1)
	openCase(t, r.Store, "blind-spots", "marker-7f3a-bs", 1)
	r.Families = []string{"owner-burden", "sessions", "manual-work", "blind-spots"}
	// room for one run serves the build
	if err := r.Store.SetLoopTurns(ctx, map[string]string{turnBuild: "sessions", turnRun: "sessions", turnSingle: turnRun}); err != nil {
		t.Fatal(err)
	}
	builders := []store.Case{cs[0], cs[1], cs[0]}
	for pass, room := range []int{3, 2, 1} {
		spent, err := r.Store.LoopSpend(ctx, time.Now().Add(-24*time.Hour), r.Budgets.Run)
		if err != nil {
			t.Fatal(err)
		}
		r.Budgets.Day = spent + (float64(room)+0.5)*r.Budgets.Run
		if got, _, err := r.room(ctx, 4); err != nil || got != room {
			t.Fatalf("pass %d: the room is %d, want %d: %v", pass+1, got, room, err)
		}
		done, err := r.Once(ctx, Options{})
		if err != nil {
			t.Fatal(err)
		}
		want, other := builders[pass], cs[0]
		if want.ID == cs[0].ID {
			other = cs[1]
		}
		if built, why := turnStep(done, want.ID, StepBuild); !built {
			t.Fatalf("pass %d (room %d): %s (%s) did not build: %q", pass+1, room, want.ID, want.Family, why)
		}
		if built, why := turnStep(done, other.ID, StepBuild); built || why != anotherBuild {
			t.Fatalf("pass %d (room %d): %s (%s) built %v (%q) while the other built", pass+1, room, other.ID, other.Family, built, why)
		}
	}
}

// A family named twice, in the configuration or in the pass's options, is picked once.
func TestAFamilyNamedTwiceIsPickedOnce(t *testing.T) {
	r, _ := turnRunner(t)
	for _, o := range []Options{{}, {Families: []string{"outside", "outside"}}} {
		r.Families = []string{"outside", "outside"}
		done, err := r.Once(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		picked := 0
		for _, d := range done {
			if d.Family == "outside" {
				picked++
			}
		}
		if picked != 1 {
			t.Fatalf("options %+v: the family named twice has %d steps, want 1: %+v", o, picked, done)
		}
	}
}

// turnStep finds a pass's step of a case: whether a run was reserved for it, and why not.
func turnStep(done []Done, caseID, step string) (reserved bool, skipped string) {
	for _, d := range done {
		if d.Case == caseID && d.Step == step {
			if d.Run.ID != "" && d.Run.Status != store.RunRefused && d.Skipped == "" {
				return true, ""
			}
			skipped = d.Skipped
		}
	}
	return false, skipped
}

// A case whose stored record cannot be read is skipped with a low note: its family's next case runs, the other
// families' runs go on, and the pass is no failure.
func TestAnUnreadableCaseRecordSkipsTheCaseNotThePass(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	ctx := context.Background()
	if err := r.Store.CaseNote(ctx, cs[1].ID, repoAction, "marker-7f3a unreadable", "fixture"); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		done, err := r.Once(ctx, Options{})
		if err != nil {
			t.Fatalf("pass %d failed on one case's record: %v", pass, err)
		}
		if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
			t.Fatalf("pass %d: owner-burden did not build: %q", pass, why)
		}
		if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
			t.Fatalf("pass %d: sessions did not investigate its next case: %q", pass, why)
		}
	}
	recordNotes := func() (standing, all int) {
		t.Helper()
		notes, err := r.Store.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range notes {
			if strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(cs[1].ID)+":record:") && n.Importance == store.ImportanceLow {
				all++
				if n.Done.IsZero() {
					standing++
				}
			}
		}
		return standing, all
	}
	if standing, all := recordNotes(); standing != 1 || all != 1 {
		t.Fatalf("the unreadable record has %d standing low notes of %d, want 1", standing, all)
	}
	// the case leaves testing: the record's note ends with the build it held
	if _, err := r.Store.UpdateCase(ctx, cs[1].ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	if standing, all := recordNotes(); standing != 0 || all != 1 {
		t.Fatalf("the record's note stands after its case left testing: %d standing of %d", standing, all)
	}
}

// A family whose cases cannot be read fails the pass, but stops no other family's runs or reviews: the others are
// picked, built and reviewed, and the pass returns the error after them.
func TestAFamilyWhoseStoreReadFailsStopsNoOtherFamilysBuildOrReview(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	ctx := context.Background()
	// sessions' testing case cannot be read: its pick fails, and so would a read of every family's cases in testing
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET alternatives = 'marker-7f3a' WHERE id = ?`, cs[1].ID); err != nil {
		t.Fatal(err)
	}
	// owner-burden has a second testing case whose package cannot be read: the pass's reviews report it
	reviewed := openCase(t, r.Store, "owner-burden", "marker-7f3a-reviewed", 1)
	mechanism, fix := "One cause", "One test"
	if _, err := r.Store.UpdateCase(ctx, reviewed.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Ship.pkgDir(reviewed.ID), "package.json"), "marker-7f3a")
	for pass := 1; pass <= 2; pass++ {
		done, err := r.Once(ctx, Options{})
		if err == nil || !strings.Contains(err.Error(), "alternatives") {
			t.Fatalf("pass %d did not return sessions' read error: %v", pass, err)
		}
		if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
			t.Fatalf("pass %d: owner-burden did not build: %q", pass, why)
		}
		if _, why := turnStep(done, reviewed.ID, StepReview); !strings.Contains(why, "package") {
			t.Fatalf("pass %d: owner-burden's reviews did not run: %q: %+v", pass, why, done)
		}
	}
}

// A family named twice is one family: its case runs once in a pass.
func TestAFamilyListedTwiceRunsItsCaseOncePerPass(t *testing.T) {
	fakeManager(t)
	r, c := inputRunner(t)
	ctx := context.Background()
	if _, err := r.Once(ctx, Options{Families: []string{c.Family, c.Family}}); err != nil {
		t.Fatal(err)
	}
	live := ""
	if runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID, Experiment: &live}); err != nil || len(runs) != 1 {
		t.Fatalf("a family listed twice ran its case %d times: %v", len(runs), err)
	}
}

// A family whose build is held at its preparation runs its next case in the pass instead.
func TestAFamilyWhoseBuildIsHeldInvestigatesInThePass(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) { return "marker-7f3a busy", nil }
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); built || !strings.Contains(why, "marker-7f3a busy") {
		t.Fatalf("the build was not held: %v %q", built, why)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
		t.Fatalf("the family whose build is held did not investigate its next case: %q", why)
	}
}

// A turn the store cannot read leaves the pass in the configured order with all its steps, and turns it cannot write
// are counted among the pass's steps.
func TestAnUnreadableTurnLeavesThePassInConfiguredOrder(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	openCase(t, r.Store, "sessions", "marker-7f3a-open", 1) // sessions investigates while its build waits: both turns move
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE loop_turns`); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatalf("an unreadable turn failed the pass: %v", err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("the first configured family did not build: %q", why)
	}
	counted := false
	for _, d := range done {
		if d.Step == "turn" && strings.HasPrefix(d.Skipped, "2 of 2 turns not recorded") {
			counted = true
		}
	}
	if !counted {
		t.Fatalf("the pass did not count its unwritten turns: %+v", done)
	}
}

// A dry pass shows the pass's one build: a later family's build waits, and its next case is the one it would run.
// It records no turn.
func TestADryPassShowsTheOneBuildAndTheFallbackCase(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	ctx := context.Background()
	done, err := r.Once(ctx, Options{Dry: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{cs[0].ID + " " + StepBuild: "dry: build " + cs[0].ID, cs[1].ID + " " + StepBuild: "another case builds in this pass",
		open.ID + " " + StepInvestigate: "dry: investigate " + open.ID}
	for _, d := range done {
		if w, ok := want[d.Case+" "+d.Step]; ok && strings.HasPrefix(d.Skipped, w) {
			delete(want, d.Case+" "+d.Step)
		}
	}
	if len(want) != 0 {
		t.Fatalf("the dry pass lacks %v: %+v", want, done)
	}
	if turn, err := r.Store.LoopTurn(ctx, turnBuild); err != nil || turn != "" {
		t.Fatalf("a dry pass recorded the build turn %q: %v", turn, err)
	}
}

// The pass's only build held at its preparation takes no turn while another family investigates, and the next pass
// tries the held family's build first; the investigation takes its own turn.
func TestAHeldOnlyBuildTakesNoTurnWhileAnotherFamilyInvestigates(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"owner-burden", "sessions"}
	ctx := context.Background()
	busy := true
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
		if busy {
			return "marker-7f3a busy", nil
		}
		return "", nil
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); built || !strings.Contains(why, "marker-7f3a busy") {
		t.Fatalf("the build was not held: %v %q", built, why)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
		t.Fatalf("sessions did not investigate: %q", why)
	}
	if turn, err := r.Store.LoopTurn(ctx, StepBuild); err != nil || turn != "" {
		t.Fatalf("the held build took the turn: %q %v", turn, err)
	}
	if turn, err := r.Store.LoopTurn(ctx, turnRun); err != nil || turn != "sessions" {
		t.Fatalf("the run turn is %q, want sessions: %v", turn, err)
	}
	busy = false
	done, err = r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("the next pass did not build owner-burden's case: %q", why)
	}
	if turn, err := r.Store.LoopTurn(ctx, StepBuild); err != nil || turn != "owner-burden" {
		t.Fatalf("the build turn after it is %q: %v", turn, err)
	}
}

// A build the budgets refuse takes no turn: the family whose turn it was builds when the budget has room again.
func TestABuildTheBudgetRefusesTakesNoTurn(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	ctx := context.Background()
	if built, _ := builtIn(t, r); built.Case != cs[0].ID {
		t.Fatalf("the first pass built %s", built.Case)
	}
	day := r.Budgets.Day
	r.Budgets.Day = r.Budgets.Run / 2
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[1].ID, StepBuild); built || !strings.Contains(why, "day budget") {
		t.Fatalf("sessions' build was not refused on the budget: %v %q", built, why)
	}
	if turn, err := r.Store.LoopTurn(ctx, StepBuild); err != nil || turn != "owner-burden" {
		t.Fatalf("a refused build moved the turn to %q: %v", turn, err)
	}
	r.Budgets.Day = day
	if built, _ := builtIn(t, r); built.Case != cs[1].ID {
		t.Fatalf("after the refusal the pass built %s (%s), not sessions' case", built.Case, built.Family)
	}
}

// A family whose testing case waits for another family's build investigates its next case in the same pass.
func TestAFamilyWhoseBuildWaitsInvestigatesInThePass(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("owner-burden did not build: %q", why)
	}
	if built, why := turnStep(done, cs[1].ID, StepBuild); built || why != "another case builds in this pass" {
		t.Fatalf("sessions' build did not wait: %v %q", built, why)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
		t.Fatalf("sessions did not investigate its open case while its build waited: %q", why)
	}
}

// Investigations take turns too: with room for one run a pass, two families with only open cases alternate.
func TestEachPassInvestigatesTheNextFamilyUnderATightBudget(t *testing.T) {
	fakeManager(t)
	r, first := inputRunner(t)
	second := openCase(t, r.Store, "sessions", "marker-7f3a-sessions", 1)
	r.Families = []string{first.Family, "sessions"}
	r.Budgets.Day = 1.5 * r.Budgets.Run
	for pass, want := range []store.Case{first, second, first} {
		done, err := r.Once(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		other := second
		if want.ID == second.ID {
			other = first
		}
		ran, _ := turnStep(done, want.ID, StepInvestigate)
		_, why := turnStep(done, other.ID, StepInvestigate)
		if !ran || !strings.Contains(why, "day budget") {
			t.Fatalf("pass %d: %s (%s) did not investigate alone (%v), the other's: %q: %+v", pass+1, want.ID, want.Family, ran, why, done)
		}
	}
}

// runDirs are the run directories the pass left under the runner's storage, by the run each belongs to.
func runDirs(t *testing.T, r *Runner) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.Dir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(e.Name(), "."), "-host"))
	}
	return out
}

// With room for one run, the build takes the first place: another family's investigation, ahead of it in the run
// turn's order, is refused by the budget.
func TestTheBuildTakesTheFirstPlace(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"sessions", "owner-burden"}
	r.Budgets.Day = 1.5 * r.Budgets.Run
	if err := r.Store.SetLoopTurns(context.Background(), map[string]string{turnSingle: turnRun}); err != nil { // the pass serves the build
		t.Fatal(err)
	}
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	built, whyBuild := turnStep(done, cs[0].ID, StepBuild)
	ran, why := turnStep(done, open.ID, StepInvestigate)
	if !built || ran || !strings.Contains(why, "day budget") {
		t.Fatalf("built %v (%q), investigated %v (%q): %+v", built, whyBuild, ran, why, done)
	}
}

// A build with no room is not prepared: it waits, with the budget that binds, and leaves no run and nothing on disk.
func TestABuildWithNoRoomIsNotPrepared(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"sessions", "owner-burden"}
	r.Budgets.Day = 0.5 * r.Budgets.Run
	ctx := context.Background()
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ran, _ := turnStep(done, open.ID, StepInvestigate)
	built, why := turnStep(done, cs[0].ID, StepBuild)
	if ran || built || !strings.Contains(why, noRoom) || !strings.Contains(why, "day budget") {
		t.Fatalf("investigated %v, built %v (%q): %+v", ran, built, why, done)
	}
	live := ""
	if runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: cs[0].ID, Experiment: &live}); err != nil || len(runs) != 0 {
		t.Fatalf("the waiting build has runs: %+v %v", runs, err)
	}
	investigation := ""
	for _, d := range done {
		if d.Case == open.ID && d.Step == StepInvestigate {
			investigation = d.Run.ID
		}
	}
	for _, id := range runDirs(t, r) {
		if id != investigation {
			t.Fatalf("the pass left %s on disk, not the investigation's %s", id, investigation)
		}
	}
}

// The room the budgets leave is the number of runs their check lets through, one reservation after another, also
// where dividing what is left by the run's budget rounds to one run more (0.63 into 1.89) or one fewer (0.39 into
// 1.17). A run that brings the spend to the limit exactly fits.
func TestTheRoomIsTheRunsTheBudgetsLetThrough(t *testing.T) {
	for _, tc := range []struct {
		b    Budgets
		want int
	}{{Budgets{Run: 0.63, Day: 1.89, Week: 200}, 3}, {Budgets{Run: 0.39, Day: 1.17, Week: 200}, 3}, {Budgets{Run: 0.63, Day: 200, Week: 1.89}, 3},
		{Budgets{Run: 4, Day: 40, Week: 200}, 10}} {
		b := tc.b
		ctx := context.Background()
		st := testStore(t)
		c := openCase(t, st, "failures", "marker-7f3a", 1)
		r := &Runner{Store: st, Budgets: b, Dir: t.TempDir()}
		room, _, err := r.room(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		through := 0
		for ; through < 20; through++ {
			if _, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate); why != "" {
				break
			}
		}
		if room != through || through != tc.want {
			t.Errorf("run $%v, day $%v, week $%v: the room is %d runs, the budgets let %d through, want %d", b.Run, b.Day, b.Week, room,
				through, tc.want)
		}
	}
}

// A budget too large to count in runs still leaves room for the pass's build.
func TestAHugeBudgetLeavesRoomForTheBuild(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	r.Budgets.Day, r.Budgets.Week = 1e20, 1e20
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("the build did not run: %q: %+v", why, done)
	}
}

// A build prepared and then not reserved leaves no run on disk: its host and run directories go, and its build tree
// goes back to the case's work directory first, so the case's next pass builds.
func TestAReadyBuildLeftUnreservedLeavesNothingOnDisk(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER refuse_reservation BEFORE INSERT ON loop_runs WHEN NEW.status = 'running'
	  BEGIN SELECT RAISE(ABORT, 'marker-7f3a refused'); END`); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); built || !strings.Contains(why, "marker-7f3a refused") {
		t.Fatalf("the build's reservation was not refused: %v %q", built, why)
	}
	if left := runDirs(t, r); len(left) != 0 {
		t.Fatalf("the unreserved build left %v on disk", left)
	}
	p, ok, err := r.Ship.Load(cs[0].ID)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	if _, err := os.Stat(p.Work); err != nil {
		t.Fatalf("the package names a build tree that is gone: %v", err)
	}
	if _, err := db.Exec(`DROP TRIGGER refuse_reservation`); err != nil {
		t.Fatal(err)
	}
	done, err = r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range done {
		if d.Case == cs[0].ID && d.Step == StepBuild && d.Run.ID != "" {
			if d.Run.Failure == "build prepare" {
				t.Fatalf("the next pass's build failed to prepare: %s", d.Run.Summary)
			}
			return
		}
	}
	t.Fatalf("the next pass did not build: %+v", done)
}

// A package whose build tree went with its run directory gets its tree made again in the case's work directory, so
// the case's build prepares.
func TestABuildWhoseTreeWentWithItsRunDirectoryPrepares(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	if _, err := r.Once(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := r.Ship.Load(cs[0].ID)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	if !within(filepath.Join(r.Dir, "runs"), p.Work) {
		t.Fatalf("the first build's tree is not in its run directory: %s", p.Work)
	}
	if err := removeAll(filepath.Dir(p.Work)); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range done {
		if d.Case == cs[0].ID && d.Step == StepBuild && d.Run.ID != "" {
			if d.Run.Failure == "build prepare" {
				t.Fatalf("the build failed to prepare: %s", d.Run.Summary)
			}
			return
		}
	}
	t.Fatalf("the second pass did not build: %+v", done)
}

// A family whose best build is held at its preparation tries its next testing case before anything else.
func TestAHeldBuildTriesTheFamilysNextTestingCase(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	best := openCase(t, r.Store, "sessions", "marker-7f3a-best", 3) // more evidence: picked first
	mechanism, fix := "One cause", "One test"
	best, err := r.Store.UpdateCase(ctx, best.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, best, "main")
	calls := 0
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
		if calls++; calls == 1 { // the best case's preparation
			return "marker-7f3a busy", nil
		}
		return "", nil
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, best.ID, StepBuild); built || !strings.Contains(why, "marker-7f3a busy") {
		t.Fatalf("the best build was not held: %v %q", built, why)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("the family's next testing case did not build: %q: %+v", why, done)
	}
}

// An investigation whose inputs cannot be made gives its family's next case the run.
func TestAnInvestigationWhoseInputsFailTriesTheNextCaseInThePass(t *testing.T) {
	fakeManager(t)
	r, _ := inputRunner(t)
	ctx := context.Background()
	broken := openCase(t, r.Store, "sessions", "marker-7f3a-broken", 3) // more evidence: picked first
	next := openCase(t, r.Store, "sessions", "marker-7f3a-next", 1)
	r.Families = []string{"sessions"}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE case_evidence SET at = 'marker-7f3a' WHERE case_id = ?`, broken.ID); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ran, why := turnStep(done, broken.ID, StepInvestigate); ran || !strings.HasPrefix(why, "inputs: ") {
		t.Fatalf("the broken case's inputs did not fail: %v %q", ran, why)
	}
	if ran, why := turnStep(done, next.ID, StepInvestigate); !ran {
		t.Fatalf("the family's next case did not investigate: %q: %+v", why, done)
	}
}

// lessonFixture closes a case of the family as improved with a lesson in its package, and says whether the store has
// the lesson's receipt.
func lessonFixture(t *testing.T, r *Runner, db *sql.DB, family string) (store.Case, func() bool) {
	t.Helper()
	c := openCase(t, r.Store, family, "marker-7f3a-lesson-"+family, 1)
	if _, err := db.Exec("UPDATE cases SET status = ?, reason = ? WHERE id = ?", store.CaseClosed, store.Improved, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Ship.save(&Package{Case: c.ID, State: Landed, Lesson: "Check the fixture before the run."}); err != nil {
		t.Fatal(err)
	}
	return c, func() bool { return r.published(context.Background(), c.ID) }
}

// A family whose pick fails on a store read publishes no lesson in the pass, and the other family's lesson is
// published.
func TestAFamilyWhosePickFailsPublishesNoLesson(t *testing.T) {
	r, _ := turnRunner(t, "owner-burden")
	dir := t.TempDir()
	r.Tacit = script(t, dir, "tacit", "exit 0")
	r.Families = []string{"owner-burden", "sessions"}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, ownerLesson := lessonFixture(t, r, db, "owner-burden")
	_, sessionsLesson := lessonFixture(t, r, db, "sessions")
	bad := openCase(t, r.Store, "sessions", "marker-7f3a-bad", 1) // sessions' pick cannot read this row
	if _, err := db.Exec("UPDATE cases SET alternatives = 'marker-7f3a' WHERE id = ?", bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(context.Background(), Options{}); err == nil {
		t.Fatal("the pass did not return sessions' pick error")
	}
	if !ownerLesson() {
		t.Error("owner-burden's lesson was not published")
	}
	if sessionsLesson() {
		t.Error("sessions' lesson was published though its pick failed")
	}
}

// Each family's lessons read its own closed cases: a lesson of one family that cannot be read stops no other family's,
// and the pass returns the error.
func TestALessonThatCannotBeReadStopsNoOtherFamilysLesson(t *testing.T) {
	r, _ := turnRunner(t, "owner-burden")
	dir := t.TempDir()
	r.Tacit = script(t, dir, "tacit", "exit 0")
	r.Families = []string{"owner-burden", "sessions"}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, sessionsLesson := lessonFixture(t, r, db, "sessions")
	owner, _ := lessonFixture(t, r, db, "owner-burden")
	// a log line of owner-burden's closed case whose time cannot be read: its lesson's receipts cannot be read
	if _, err := db.Exec(`INSERT INTO case_log (case_id, seq, at, actor, action, detail) VALUES (?, 9999, 'marker-7f3a', 'fixture', 'note', '')`,
		owner.ID); err != nil {
		t.Fatal(err)
	}
	_, err = r.Once(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "marker-7f3a") {
		t.Fatalf("the pass did not return the lesson's error: %v", err)
	}
	if !sessionsLesson() {
		t.Error("sessions' lesson was not published")
	}
}

// With room for two runs or more, a pass builds and runs the other families' cases both, whichever turn the last pass
// took: here the last pass took only the build's, and the next builds again while another family investigates.
func TestARoomForMoreRunsBuildsAndRunsTheRestEachPass(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	ctx := context.Background()
	if built, _ := builtIn(t, r); built.Case != cs[0].ID { // no other run: the pass takes only the build's turn
		t.Fatalf("the first pass built %s, want %s", built.Case, cs[0].ID)
	}
	open := openCase(t, r.Store, "manual-work", "marker-7f3a-mw", 1)
	r.Families = append(r.Families, "manual-work")
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[1].ID, StepBuild); !built {
		t.Fatalf("the second pass did not build sessions' case: %q", why)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
		t.Fatalf("the second pass did not investigate: %q", why)
	}
}

// setRoom sets the day's budget to leave room for want runs.
func setRoom(t *testing.T, r *Runner, want int) {
	t.Helper()
	ctx := context.Background()
	spent, err := r.Store.LoopSpend(ctx, time.Now().Add(-24*time.Hour), r.Budgets.Run)
	if err != nil {
		t.Fatal(err)
	}
	r.Budgets.Day = spent + (float64(want)+0.5)*r.Budgets.Run
	if got, _, err := r.room(ctx, 4); err != nil || got != want {
		t.Fatalf("the room is %d, want %d: %v", got, want, err)
	}
}

// With room for one run, a pass answers the last pass that took exactly one turn, though a pass with room for more
// took both in between (rooms 1, 3, 1): after a pass that took only the build's turn the run turn is served, and after
// one that took only the run turn's the build. A store with no single turn serves the run turn.
func TestARoomForOneRunAnswersTheLastSingleTurn(t *testing.T) {
	for _, first := range []string{turnBuild, ""} { // the turn the first pass takes: the build's, or (no row) the run turn's
		r, cs := turnRunner(t, "owner-burden")
		ctx := context.Background()
		s := openCase(t, r.Store, "sessions", "marker-7f3a-s", 1)
		m := openCase(t, r.Store, "manual-work", "marker-7f3a-m", 1)
		r.Families = []string{"owner-burden", "sessions", "manual-work"}
		if first == turnBuild {
			if err := r.Store.SetLoopTurns(ctx, map[string]string{turnSingle: turnRun}); err != nil {
				t.Fatal(err)
			}
		}
		for pass, room := range []int{1, 3, 1} {
			setRoom(t, r, room)
			done, err := r.Once(ctx, Options{})
			if err != nil {
				t.Fatal(err)
			}
			built, whyBuild := turnStep(done, cs[0].ID, StepBuild)
			ranS, _ := turnStep(done, s.ID, StepInvestigate)
			ranM, _ := turnStep(done, m.ID, StepInvestigate)
			investigated := ranS || ranM
			wantBuild := (pass == 1) || (pass == 0) == (first == turnBuild)
			wantRuns := pass == 1 || !wantBuild
			if built != wantBuild || investigated != wantRuns || !built && whyBuild != runsTurn {
				t.Fatalf("first %q, pass %d (room %d): built %v (%q), investigated %v; want the build %v, runs %v", first, pass+1, room,
					built, whyBuild, investigated, wantBuild, wantRuns)
			}
		}
	}
}

// With room for one run, a pass whose store cannot keep the turns serves the run turn, pass after pass: the bounds hold
// only while the store reads and writes them.
func TestARoomForOneRunServesTheRunTurnWhenTheTurnsCannotBeRead(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"owner-burden", "sessions"}
	r.Budgets.Day = 1.5 * r.Budgets.Run
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE loop_turns`); err != nil {
		t.Fatal(err)
	}
	for pass := range 4 {
		setRoom(t, r, 1)
		done, err := r.Once(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		built, whyBuild := turnStep(done, cs[0].ID, StepBuild)
		ran, why := turnStep(done, open.ID, StepInvestigate)
		if built || !ran || whyBuild != runsTurn {
			t.Fatalf("pass %d: built %v (%q), investigated %v (%q); want the run turn served", pass+1, built, whyBuild, ran, why)
		}
	}
}

// A pass's lessons keep to its case: with --case naming one closed case, only that case's lesson is published.
func TestLessonsKeepToThePassesCase(t *testing.T) {
	r, _ := turnRunner(t, "owner-burden")
	r.Tacit = script(t, t.TempDir(), "tacit", "exit 0")
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	one, onePublished := lessonFixture(t, r, db, "owner-burden")
	other := openCase(t, r.Store, "owner-burden", "marker-7f3a-other", 1)
	if _, err := db.Exec("UPDATE cases SET status = ?, reason = ? WHERE id = ?", store.CaseClosed, store.Improved, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Ship.save(&Package{Case: other.ID, State: Landed, Lesson: "Name the other fixture."}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(context.Background(), Options{Case: one.ID}); err != nil {
		t.Fatal(err)
	}
	if !onePublished() {
		t.Error("the pass's case's lesson was not published")
	}
	if r.published(context.Background(), other.ID) {
		t.Error("a lesson of a case the pass does not name was published")
	}
}

// A pass with room for more runs that takes exactly one turn writes the single row too: here only the run turn's (the
// build is held), so the next pass with room for one run serves the build.
func TestAPassWithRoomForMoreThatTakesOneTurnWritesTheSingleRow(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"owner-burden", "sessions"}
	ctx := context.Background()
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) { return "marker-7f3a busy", nil }
	setRoom(t, r, 3)
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
		t.Fatalf("the first pass did not investigate: %q", why)
	}
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) { return "", nil }
	setRoom(t, r, 1)
	if done, err = r.Once(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("the pass with room for one run after a run-turn-only pass did not build: %q", why)
	}
}

// When every family's pick fails, the pass runs no reviews: with no family to read, they would read every family's
// cases at once.
func TestNoReviewsRunWhenEveryPickFails(t *testing.T) {
	r, _ := turnRunner(t, "sessions")
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bad := openCase(t, r.Store, "sessions", "marker-7f3a-bad", 1)
	if _, err := db.Exec("UPDATE cases SET status = ?, alternatives = 'marker-7f3a' WHERE id = ?", store.CaseTesting, bad.ID); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(context.Background(), Options{})
	if err == nil {
		t.Fatal("the pass did not return the pick's error")
	}
	for _, d := range done {
		if d.Step == StepReview {
			t.Fatalf("the pass ran reviews though every pick failed: %+v", d)
		}
	}
}

// A family whose cases in testing cannot be read stops no later family's reviews.
func TestAFamilyWhoseTestingCasesCannotBeReadStopsNoOtherFamilysReviews(t *testing.T) {
	r, cs := turnRunner(t, "sessions", "owner-burden")
	ctx := context.Background()
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE cases SET alternatives = 'marker-7f3a' WHERE id = ?", cs[0].ID); err != nil { // sessions' case
		t.Fatal(err)
	}
	// owner-burden's case has a run in flight: its review says so
	running := store.LoopRun{ID: newRunID(time.Now()), Case: cs[1].ID, Family: cs[1].Family, Step: StepBuild, Started: time.Now(),
		Status: store.RunRunning, USD: r.Budgets.Run}
	if err := r.Store.PutLoopRun(ctx, running); err != nil {
		t.Fatal(err)
	}
	done := r.reviews(ctx, "", "sessions", "owner-burden")
	unread, inFlight := false, false
	for _, d := range done {
		unread = unread || d.Family == "sessions" && strings.HasPrefix(d.Skipped, "cannot read the cases in testing")
		inFlight = inFlight || d.Case == cs[1].ID && d.Skipped == "a run is in flight"
	}
	if !unread || !inFlight {
		t.Fatalf("sessions unreadable %v, owner-burden's review reached %v: %+v", unread, inFlight, done)
	}
}

// A build whose run directory went, and the case's work directories with it, gets its tree made again: the parent the
// work directory needs is made too.
func TestABuildWhoseWorkDirectoriesWentPrepares(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	if _, err := r.Once(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := r.Ship.Load(cs[0].ID)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	for _, dir := range []string{filepath.Dir(p.Work), filepath.Join(r.Ship.Dir, "work")} {
		if err := removeAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range done {
		if d.Case == cs[0].ID && d.Step == StepBuild && d.Run.ID != "" {
			if d.Run.Failure == "build prepare" {
				t.Fatalf("the build failed to prepare: %s", d.Run.Summary)
			}
			return
		}
	}
	t.Fatalf("the second pass did not build: %+v", done)
}

// A pass whose pick fails returns the pick's error, though a lesson fails too.
func TestAPickErrorOutranksALessonError(t *testing.T) {
	r, _ := turnRunner(t, "owner-burden")
	r.Tacit = script(t, t.TempDir(), "tacit", "exit 0")
	r.Families = []string{"owner-burden", "sessions"}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, _ := lessonFixture(t, r, db, "owner-burden")
	if _, err := db.Exec(`INSERT INTO case_log (case_id, seq, at, actor, action, detail) VALUES (?, 9999, 'marker-7f3a', 'fixture', 'note', '')`,
		owner.ID); err != nil { // owner-burden's lesson cannot read its receipts
		t.Fatal(err)
	}
	bad := openCase(t, r.Store, "sessions", "marker-7f3a-bad", 1) // sessions' pick cannot read this row
	if _, err := db.Exec("UPDATE cases SET alternatives = 'marker-7f3a' WHERE id = ?", bad.ID); err != nil {
		t.Fatal(err)
	}
	_, err = r.Once(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Fatalf("the pass returned %v, want the pick's error", err)
	}
}

// A testing case whose investigation names a repository in no valid form is skipped like one whose record cannot be
// read: the pass is no failure, and the family's next case runs.
func TestAnInvalidRepositoryNameSkipsTheCaseNotThePass(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	setCaseRepo(t, r.Store, cs[1], "../marker-7f3a")
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatalf("the pass failed on one case's repository name: %v", err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built {
		t.Fatalf("owner-burden did not build: %q", why)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); !ran {
		t.Fatalf("sessions did not investigate its next case: %q", why)
	}
}
