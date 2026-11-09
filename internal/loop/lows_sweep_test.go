package loop

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// refuseReservations makes the store refuse every reservation's row from now on.
func refuseReservations(t *testing.T, r *Runner) {
	t.Helper()
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER refuse_reservation BEFORE INSERT ON loop_runs WHEN NEW.status = 'running'
	  BEGIN SELECT RAISE(ABORT, 'marker-7f3a refused'); END`); err != nil {
		t.Fatal(err)
	}
}

// A leftover build whose package names a case the store does not know is passed over like an unreadable package: its
// run directory goes, and the package is logged once in the sweep. returnWork would need the case for its lock.
func TestTheSweepPassesOverAPackageWhoseCaseTheStoreDoesNotKnow(t *testing.T) {
	r, _, in, _ := sweepFixture(t)
	var logs []string
	r.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	orphan := in(newRunID(time.Now()))
	treeIn(t, r, "marker-7f3a-gone", orphan)
	if err := os.MkdirAll(filepath.Join(HostDir(orphan), "input"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.sweepRuns(context.Background())
	for _, dir := range []string{orphan, HostDir(orphan)} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the sweep kept %s, whose package's case the store does not know: %v", filepath.Base(dir), err)
		}
	}
	passed := 0
	for _, line := range logs {
		if strings.Contains(line, "-gone") && strings.Contains(line, "passed over") { // the log masks the marker
			passed++
		}
	}
	if passed != 1 {
		t.Errorf("the package was logged as passed over %d times in the sweep, want once: %q", passed, logs)
	}
}

// A review whose reservation is refused leaves nothing on disk: the refused row names no directory, so what its
// preparation made goes at once, not at the next pass's sweep.
func TestARefusedReviewReservationLeavesNothingOnDisk(t *testing.T) {
	r, c, _ := reviewFixture(t)
	refuseReservations(t, r)
	before := runDirs(t, r) // the fixture's own
	done := r.reviews(context.Background(), "")
	if len(done) != 1 || done[0].Case != c.ID || !strings.Contains(done[0].Skipped, "marker-7f3a refused") {
		t.Fatalf("the review's reservation was not refused: %+v", done)
	}
	if left := runDirs(t, r); len(left) != len(before) {
		t.Fatalf("the refused review left %v on disk, beside the fixture's %v", left, before)
	}
}

// A build whose preparation failed after its tree moved into its run directory, and whose reservation (which would
// record the failure) is refused, gives the tree back to the case's work directory and leaves no run directory.
func TestARefusedFailedBuildGivesItsTreeBack(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	id := cs[0].ID
	// the previous patch is a link: the preparation fails at it, after buildPackage moved the tree
	if err := os.MkdirAll(r.Ship.pkgDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("marker-7f3a-elsewhere", filepath.Join(r.Ship.pkgDir(id), "change.patch")); err != nil {
		t.Fatal(err)
	}
	refuseReservations(t, r)
	done, err := r.Once(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, id, StepBuild); built || !strings.Contains(why, "marker-7f3a refused") {
		t.Fatalf("the failed build's reservation was not refused: %v %q: %+v", built, why, done)
	}
	if left := runDirs(t, r); len(left) != 0 {
		t.Errorf("the refused build left %v on disk", left)
	}
	p, ok, err := r.Ship.Load(id)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	if p.Work != r.Ship.workDir(id) {
		t.Errorf("the package names %s, not the case's work directory", p.Work)
	} else if _, err := os.Stat(p.Work); err != nil {
		t.Errorf("the package names a build tree that is gone: %v", err)
	}
}

// A pass whose turns do not move writes none of them: a store that refuses their writes counts no unwritten turn.
func TestAPassWritesNoTurnThatDidNotMove(t *testing.T) {
	r, _ := turnRunner(t, "sessions")
	ctx := context.Background()
	if err := r.Store.SetLoopTurns(ctx, map[string]string{turnBuild: "sessions", turnRun: "sessions"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err := db.Exec(`CREATE TRIGGER refuse_turn_` + event + ` BEFORE ` + event + ` ON loop_turns
		  BEGIN SELECT RAISE(ABORT, 'marker-7f3a refused'); END`); err != nil {
			t.Fatal(err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{"outside"}}) // a family with no case: no run, no turn moves
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range done {
		if d.Step == "turn" {
			t.Fatalf("a pass whose turns did not move wrote them: %+v", d)
		}
	}
}

// Two families whose picks fail: the pass returns the first one's error, in the families' configured order.
func TestAPassReturnsTheFirstOfTwoPickErrors(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden", "sessions")
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range cs {
		if _, err := db.Exec(`UPDATE cases SET alternatives = 'marker-7f3a' WHERE id = ?`, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, err = r.Once(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), cs[0].ID) || strings.Contains(err.Error(), cs[1].ID) {
		t.Fatalf("the pass did not return the first family's pick error (case %s): %v", cs[0].ID, err)
	}
}

// A run directory without a build tree goes with no package read: a package that cannot be read is not even logged.
func TestARunDirectoryWithoutATreeGoesWithNoPackageRead(t *testing.T) {
	r, _, in, _ := sweepFixture(t)
	var logs []string
	r.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	write(t, filepath.Join(r.Ship.pkgDir("marker-7f3a-broken"), "package.json"), "{")
	bare := in(newRunID(time.Now())) // an investigation's leftover: a host directory with its input, no tree
	for _, dir := range []string{bare, filepath.Join(HostDir(bare), "input")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.sweepRuns(context.Background())
	if _, err := os.Stat(bare); !os.IsNotExist(err) {
		t.Fatalf("the sweep kept a run directory no run names: %v", err)
	}
	for _, line := range logs {
		if strings.Contains(line, "-broken") { // the log masks the marker
			t.Fatalf("the sweep read a package for a directory without a tree: %s", line)
		}
	}
}
