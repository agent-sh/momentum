package loop

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A pass told to stop at a ready build's reservation gives the build tree back to the case's work directory and leaves
// no run directory, though the pass's own context is done; the next family's investigation then stops too.
func TestAStoppedPassGivesTheReadyBuildsTreeBack(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"sessions", "owner-burden"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared := false
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
		if stackHas("buildPackage") {
			prepared = true // the build's preparation is under way
		}
		return "", nil
	}
	r.Now = func() time.Time {
		if prepared && stackHas("(*Runner).reservePrepared") && ctx.Err() == nil {
			cancel() // the build's reservation: the first after the build is ready
		}
		return time.Now()
	}
	done, err := r.Once(ctx, Options{})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !prepared {
		t.Fatalf("the build was never prepared: %+v", done)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); built || !strings.Contains(why, "context canceled") {
		t.Fatalf("the build was not stopped at its reservation: %v %q: %+v", built, why, done)
	}
	if left := runDirs(t, r); len(left) != 0 {
		t.Errorf("the stopped pass left %v on disk", left)
	}
	p, ok, err := r.Ship.Load(cs[0].ID)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	if p.Work != r.Ship.workDir(cs[0].ID) {
		t.Errorf("the package names %s, not the case's work directory", p.Work)
	}
	if _, err := os.Stat(p.Work); err != nil {
		t.Errorf("the package names a build tree that is gone: %v", err)
	}
}

// A ready build left unreserved whose tree cannot go back, since another ship step holds the case's lock, keeps its
// run directory with the tree the package names; its host directory goes. The next pass gives the tree back (the run
// directory has no run) and builds.
func TestAReadyBuildWhoseTreeCannotGoBackKeepsItsRunDirectory(t *testing.T) {
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
	var unlock func()
	r.Now = func() time.Time {
		if unlock == nil && stackHas("(*Runner).reservePrepared") { // another step takes the case's lock now
			if u, err := r.Ship.lock(ctx, cs[0].ID); err == nil {
				unlock = u
			}
		}
		return time.Now()
	}
	done, err := r.Once(ctx, Options{})
	if unlock == nil {
		t.Fatal("the fixture never held the case's lock")
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); built || !strings.Contains(why, "marker-7f3a refused") {
		t.Fatalf("the build's reservation was not refused: %v %q", built, why)
	}
	p, ok, err := r.Ship.Load(cs[0].ID)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	runs := filepath.Join(r.Dir, "runs")
	if !within(runs, p.Work) {
		t.Fatalf("the package names %s, not the tree in its run directory", p.Work)
	}
	if _, err := os.Stat(p.Work); err != nil {
		t.Fatalf("the package names a build tree that is gone: %v", err)
	}
	kept := filepath.Dir(p.Work)
	if _, err := os.Stat(HostDir(kept)); !os.IsNotExist(err) {
		t.Fatalf("the host directory stayed: %v", err)
	}
	r.Now = nil
	if _, err := db.Exec(`DROP TRIGGER refuse_reservation`); err != nil {
		t.Fatal(err)
	}
	done, err = r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("the next pass kept the run directory no run names: %v", err)
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

// treeIn makes a build tree in a run directory and a package that names it.
func treeIn(t *testing.T, r *Runner, caseID, runDir string) string {
	t.Helper()
	tree := filepath.Join(runDir, "work")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "marker-7f3a"), []byte("tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Ship.save(&Package{Case: caseID, Work: tree, State: Prepared}); err != nil {
		t.Fatal(err)
	}
	return tree
}

// returnWork takes the case's lock: while another ship step holds it, the tree stays where the package names it.
func TestReturnWorkTakesTheCasesLock(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	id := cs[0].ID
	runDir := filepath.Join(r.Dir, "runs", "marker-7f3a-run")
	tree := treeIn(t, r, id, runDir)
	unlock, err := r.Ship.lock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stays, err := r.Ship.returnWork(ctx, id, runDir)
	unlock()
	if !stays || err == nil {
		t.Fatalf("the tree went back while another step held the case's lock: stays %v, %v", stays, err)
	}
	if p, _, _ := r.Ship.Load(id); p.Work != tree {
		t.Fatalf("the package names %s, not %s", p.Work, tree)
	}
	if stays, err := r.Ship.returnWork(ctx, id, runDir); stays || err != nil {
		t.Fatalf("the tree did not go back once the lock was free: stays %v, %v", stays, err)
	}
	if p, _, _ := r.Ship.Load(id); p.Work != r.Ship.workDir(id) {
		t.Fatalf("the package names %s, not the case's work directory", p.Work)
	}
	if _, err := os.Stat(filepath.Join(r.Ship.workDir(id), "marker-7f3a")); err != nil {
		t.Fatalf("the tree is not in the case's work directory: %v", err)
	}
}

// A tree that went back to the case's work directory while its package could not be saved says where it is now.
func TestATreeWhosePackageCannotBeSavedSaysWhereItIs(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	id := cs[0].ID
	runDir := filepath.Join(r.Dir, "runs", "marker-7f3a-run")
	treeIn(t, r, id, runDir)
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET merged_into = 'marker-7f3a-other' WHERE id = ?`, id); err != nil { // no save is authorized
		t.Fatal(err)
	}
	stays, err := r.Ship.returnWork(ctx, id, runDir)
	if stays || err == nil || !strings.Contains(err.Error(), r.Ship.workDir(id)) {
		t.Fatalf("the unsaved package's tree: stays %v, %v; want it named in %s", stays, err, r.Ship.workDir(id))
	}
	if _, err := os.Stat(filepath.Join(r.Ship.workDir(id), "marker-7f3a")); err != nil {
		t.Fatalf("the tree is not where the error says: %v", err)
	}
}

// A pass sweeps the run directories no run names, which a pass that ended between a build's preparation and its
// reservation leaves: the build tree a package names in one goes back to its case first, and run directories a run or a
// hold's receipt names stay. A pass that builds nothing sweeps too, and the next pass builds.
func TestAPassSweepsRunDirectoriesNoRunNames(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	id := cs[0].ID
	if _, err := r.Once(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := r.Ship.Load(id)
	if err != nil || !ok || !within(filepath.Join(r.Dir, "runs"), p.Work) {
		t.Fatalf("the first build's package: %+v %v %v", p, ok, err)
	}
	first := filepath.Dir(p.Work)
	// a crash after the next build's preparation: its tree in a run directory with no run, and its host directory
	orphan := filepath.Join(r.Dir, "runs", newRunID(time.Now()))
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Work, filepath.Join(orphan, "work")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "work", "marker-7f3a"), []byte("tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Work = filepath.Join(orphan, "work")
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(HostDir(orphan), "input"), 0o700); err != nil {
		t.Fatal(err)
	}
	// a held build's run: its row gave way to a hold's charge receipt, and its directory keeps what the run did
	held := store.LoopRun{ID: newRunID(time.Now()), Case: id, Family: cs[0].Family, Step: StepBuild, Started: time.Now(), Status: store.RunRunning,
		USD: r.Budgets.Run}
	held.Dir = filepath.Join(r.Dir, "runs", held.ID)
	if err := r.Store.PutLoopRun(ctx, held); err != nil {
		t.Fatal(err)
	}
	held.Ended, held.USD = time.Now(), 0.5
	if err := r.Store.HoldLoopRun(ctx, held); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(HostDir(held.Dir), "input"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"outside"}}); err != nil { // a family with no case: no build
		t.Fatal(err)
	}
	for _, dir := range []string{orphan, HostDir(orphan)} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the pass kept %s, which no run names: %v", filepath.Base(dir), err)
		}
	}
	if _, err := os.Stat(first); err != nil {
		t.Errorf("the pass removed the first run's directory: %v", err)
	}
	if _, err := os.Stat(HostDir(held.Dir)); err != nil {
		t.Errorf("the pass removed the held run's directory: %v", err)
	}
	if p, _, _ = r.Ship.Load(id); p.Work != r.Ship.workDir(id) {
		t.Errorf("the package names %s, not the case's work directory", p.Work)
	} else if _, err := os.Stat(filepath.Join(p.Work, "marker-7f3a")); err != nil {
		t.Errorf("the tree did not go back to the case's work directory: %v", err)
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	built := false
	for _, d := range done {
		if d.Case == id && d.Step == StepBuild && d.Run.ID != "" {
			built = true
			if d.Run.Failure == "build prepare" {
				t.Errorf("the build failed to prepare: %s", d.Run.Summary)
			}
		}
	}
	if !built {
		t.Errorf("the pass did not build: %+v", done)
	}
	if p, _, _ = r.Ship.Load(id); p.Work == "" {
		t.Errorf("the package names no tree")
	} else if _, err := os.Stat(p.Work); err != nil {
		t.Errorf("the package names a build tree that is gone: %v", err)
	}
}

// An investigation prepared and not reserved leaves nothing on disk.
func TestAnUnreservedInvestigationLeavesNothingOnDisk(t *testing.T) {
	r, _ := turnRunner(t)
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"sessions"}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER refuse_reservation BEFORE INSERT ON loop_runs WHEN NEW.status = 'running'
	  BEGIN SELECT RAISE(ABORT, 'marker-7f3a refused'); END`); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ran, why := turnStep(done, open.ID, StepInvestigate); ran || !strings.Contains(why, "marker-7f3a refused") {
		t.Fatalf("the investigation's reservation was not refused: %v %q", ran, why)
	}
	if left := runDirs(t, r); len(left) != 0 {
		t.Fatalf("the unreserved investigation left %v on disk", left)
	}
}

// sweepFixture is a runner with a known run whose directories hold a transcript, and the path of a run directory in
// the runs directory.
func sweepFixture(t *testing.T) (*Runner, store.LoopRun, func(string) string, string) {
	t.Helper()
	r, cs := turnRunner(t, "sessions")
	known := store.LoopRun{ID: newRunID(time.Now()), Case: cs[0].ID, Family: cs[0].Family, Step: StepInvestigate, Started: time.Now(),
		Ended: time.Now(), Status: store.RunOK}
	known.Dir = filepath.Join(r.Dir, "runs", known.ID)
	if err := r.Store.PutLoopRun(context.Background(), known); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{known.Dir, HostDir(known.Dir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r, known, func(name string) string { return filepath.Join(r.Dir, "runs", name) }, cs[0].ID
}

// The sweep looks only at entries in a run id's form: one named ".-host" or "..-host" names no run, and removing the
// run it seems to name would remove the runs directory.
func TestTheSweepLooksOnlyAtRunIDs(t *testing.T) {
	for _, odd := range []string{".-host", "..-host", "marker-7f3a", ".marker-7f3a-host"} {
		r, known, in, _ := sweepFixture(t)
		if err := os.MkdirAll(in(odd), 0o700); err != nil {
			t.Fatal(err)
		}
		r.sweepRuns(context.Background())
		if _, err := os.Stat(filepath.Join(HostDir(known.Dir), "transcript.jsonl")); err != nil {
			t.Errorf("entry %q: the sweep removed a known run's transcript: %v", odd, err)
		}
		if _, err := os.Stat(in(odd)); err != nil {
			t.Errorf("entry %q: the sweep removed an entry that is no run's: %v", odd, err)
		}
	}
}

// A lone host directory no run names goes; when the store cannot say which runs it knows, every directory stays.
func TestTheSweepRemovesALoneHostDirectoryAndNothingWhenTheStoreCannotSay(t *testing.T) {
	r, known, in, _ := sweepFixture(t)
	lone := HostDir(in(newRunID(time.Now())))
	if err := os.MkdirAll(filepath.Join(lone, "input"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.sweepRuns(context.Background())
	if _, err := os.Stat(lone); !os.IsNotExist(err) {
		t.Errorf("the sweep kept a lone host directory no run names: %v", err)
	}
	if _, err := os.Stat(known.Dir); err != nil {
		t.Errorf("the sweep removed a known run's directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(lone, "input"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE case_log`); err != nil { // the hold receipts cannot be read
		t.Fatal(err)
	}
	r.sweepRuns(context.Background())
	if _, err := os.Stat(lone); err != nil {
		t.Errorf("the sweep removed a directory while the store could not say which runs it knows: %v", err)
	}
}

// A run directory no run names whose tree cannot go back, since another ship step holds the case's lock, stays with
// the tree the package names.
func TestTheSweepKeepsADirectoryWhoseTreeCannotGoBack(t *testing.T) {
	r, _, in, id := sweepFixture(t)
	ctx := context.Background()
	orphan := in(newRunID(time.Now()))
	tree := treeIn(t, r, id, orphan)
	unlock, err := r.Ship.lock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.sweepRuns(ctx)
	unlock()
	if _, err := os.Stat(filepath.Join(tree, "marker-7f3a")); err != nil {
		t.Fatalf("the sweep removed a tree that could not go back: %v", err)
	}
	if p, _, _ := r.Ship.Load(id); p.Work != tree {
		t.Fatalf("the package names %s, not %s", p.Work, tree)
	}
}

// A reservation the budgets refuse names no directory in its row, so a run directory a crash leaves of it is the
// sweep's to remove.
func TestARefusedReservationNamesNoDirectory(t *testing.T) {
	r, _ := turnRunner(t)
	ctx := context.Background()
	open := openCase(t, r.Store, "sessions", "marker-7f3a-open", 1)
	r.Families = []string{"sessions"}
	spent := false
	r.Now = func() time.Time {
		if !spent && stackHas("(*Runner).reservePrepared") { // the day's budget is spent between the check and the reservation
			spent = true
			if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "marker-7f3a-spend", Case: open.ID, Family: open.Family, Step: StepInvestigate,
				Started: time.Now(), Ended: time.Now(), Status: store.RunOK, USD: r.Budgets.Day}); err != nil {
				t.Error(err)
			}
		}
		return time.Now()
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var refused store.LoopRun
	for _, d := range done {
		if d.Case == open.ID && d.Step == StepInvestigate {
			refused = d.Run
		}
	}
	if refused.Status != store.RunRefused {
		t.Fatalf("the investigation's reservation was not refused: %+v", done)
	}
	stored, err := r.Store.LoopRunByID(ctx, refused.ID)
	if err != nil || stored.Dir != "" {
		t.Fatalf("the refused reservation's row names %q (%v)", stored.Dir, err)
	}
	dir := filepath.Join(r.Dir, "runs", refused.ID)
	for _, d := range []string{dir, HostDir(dir)} { // what a crash before the cleanup leaves
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.Now = nil
	r.sweepRuns(ctx)
	for _, d := range []string{dir, HostDir(dir)} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("the sweep kept %s of a refused reservation: %v", filepath.Base(d), err)
		}
	}
}

// When the tree a package names in a run directory is gone and the case's work directory holds one, an earlier move
// put it there: returnWork only saves the package, and the tree stays.
func TestReturnWorkOnlySavesWhenTheTreeAlreadyWentBack(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	id := cs[0].ID
	runDir := filepath.Join(r.Dir, "runs", newRunID(time.Now()))
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	work := r.Ship.workDir(id)
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "marker-7f3a"), []byte("tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Ship.save(&Package{Case: id, Work: filepath.Join(runDir, "work"), State: Prepared}); err != nil {
		t.Fatal(err)
	}
	if stays, err := r.Ship.returnWork(ctx, id, runDir); stays || err != nil {
		t.Fatalf("returnWork: stays %v, %v", stays, err)
	}
	if p, _, _ := r.Ship.Load(id); p.Work != work {
		t.Fatalf("the package names %s, not the case's work directory", p.Work)
	}
	if _, err := os.Stat(filepath.Join(work, "marker-7f3a")); err != nil {
		t.Fatalf("the tree in the case's work directory is gone: %v", err)
	}
}
