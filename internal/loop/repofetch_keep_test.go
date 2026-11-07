package loop

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A package keeps its repository's mirror only while it can still prepare or build: once its case leaves testing, a
// pass that no longer reads the repository removes the mirror.
func TestAPassLetsGoOfAMirrorNoPackageCanBuildFrom(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	mirror := identityMirror(r, "url:"+f.origin)
	r.Inputs.ReadDirs = []string{newRepo(t, nil).main} // the repository leaves loop.read_dirs
	newPass(t, r)
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("a pass removed the mirror of a package that can still build: %v", err)
	}
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("a pass kept the mirror of a package whose case left testing: %v", err)
	}
}

// A package keeps the mirror of its checkout's origin as the checkout names it now: once the origin names another
// repository, a pass removes the old repository's mirror.
func TestAPassLetsGoOfTheMirrorOfAPackagesOldOrigin(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	old := identityMirror(r, "url:"+f.origin)
	r.Inputs.ReadDirs = []string{newRepo(t, nil).main} // only the package names the checkout
	run(t, f.main, "git", "remote", "set-url", "origin", newRepo(t, nil).origin)
	newPass(t, r)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("a pass kept the mirror of the origin a package's checkout named before: %v", err)
	}
}

// A preparation that fails after its fetch keeps its mirror while its case is in testing, so the next one fetches only
// what is new; once the case leaves testing, the mirror goes.
func TestAFailedPreparationKeepsItsMirrorWhileItsCaseIsInTesting(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	g := newRepo(t, nil) // a repository outside loop.read_dirs
	path := os.Getenv("PATH")
	proposalReadFails(t)
	if _, err := r.Ship.Prepare(ctx, c.ID, g.main); !held(err) || !strings.Contains(err.Error(), "the proposal branch of") {
		t.Fatalf("an unreadable proposal branch did not hold the preparation: %v", err)
	}
	t.Setenv("PATH", path)
	mirror := identityMirror(r, "url:"+g.origin)
	newPass(t, r)
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("a pass removed the mirror of a preparation whose case is in testing: %v", err)
	}
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("a pass kept the mirror of a preparation whose case left testing: %v", err)
	}
}

// The owner's preparation never reads a mirror whose lock another holds: it waits for the owner to run it again, and
// clones nothing.
func TestTheOwnersPreparationWaitsForAMirrorAnotherHolds(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	g := newRepo(t, nil)
	mirror := identityMirror(r, "url:"+g.origin)
	if _, why := fetchTip(ctx, g.main, mirror, g.origin, ""); why != "" {
		t.Fatal(why)
	}
	lock, err := lockMirror(mirror) // another preparation, or a fetch
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	clones := clonesCounted(t)
	if _, err := r.Ship.Prepare(ctx, c.ID, g.main); !held(err) || !strings.Contains(err.Error(), "another preparation or fetch holds the mirror") {
		t.Fatalf("a preparation beside another holder of its mirror: %v (held %v)", err, held(err))
	}
	if got := clones(); len(got) != 0 {
		t.Fatalf("the preparation cloned a mirror another holds: %v", got)
	}
}

// A mirror being made is locked before Git initializes it, so a prune never takes it for one nothing holds.
func TestANewMirrorIsLockedBeforeItsInit(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	bin := t.TempDir()
	inits := filepath.Join(bin, "inits")
	script(t, bin, "git", `case " $* " in *" init "*) for a; do last=$a; done
  case "$last" in */.new-*) if [ -e "$last/`+mirrorLockFile+`" ]; then echo locked >> '`+inits+`'; else echo unlocked >> '`+inits+`'; fi;; esac;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	passRepository(t, r, f.main)
	if raw, err := os.ReadFile(inits); err != nil || strings.TrimSpace(string(raw)) != "locked" {
		t.Fatalf("the new mirror's init: %q %v", raw, err)
	}
}

// A mirror whose lock file cannot be opened, for any reason but its absence, counts as held: a prune leaves it.
func TestAMirrorWhoseLockCannotBeOpenedStays(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	r, _ := inputRunner(t)
	a, b := newRepo(t, nil), newRepo(t, nil)
	r.Inputs.ReadDirs = []string{a.main}
	newPass(t, r)
	mirror := identityMirror(r, "url:"+a.origin)
	if err := os.Chmod(filepath.Join(mirror, mirrorLockFile), 0); err != nil {
		t.Fatal(err)
	}
	r.Inputs.ReadDirs = []string{b.main}
	newPass(t, r)
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("a prune removed a mirror whose lock it could not open: %v", err)
	}
}

// TestPruneInAnotherProcess is a pass's prune in a process of its own, which TestAPruneStoppedMidwayLeavesNoHalfMirror
// kills outright.
func TestPruneInAnotherProcess(t *testing.T) {
	mirrors := os.Getenv("MOMENTUM_TEST_PRUNE_MIRRORS")
	if mirrors == "" {
		t.Skip("the prune process of TestAPruneStoppedMidwayLeavesNoHalfMirror")
	}
	pruneMirrors(mirrors, nil, nil, nil)
}

// A prune stopped while it removes a mirror leaves no half mirror at the mirror's name: under its lock the mirror is
// renamed to a tombstone first, and the next prune removes what the stopped one left.
func TestAPruneStoppedMidwayLeavesNoHalfMirror(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	passRepository(t, r, f.main)
	mirrors := filepath.Join(r.Dir, "mirrors")
	mirror := identityMirror(r, "url:"+f.origin)
	bulk := filepath.Join(mirror, "objects", "bulk")
	if err := os.Mkdir(bulk, 0o700); err != nil {
		t.Fatal(err)
	}
	const files = 60000
	for i := range files {
		if err := os.WriteFile(filepath.Join(bulk, fmt.Sprintf("f%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a sample of the files, wherever the mirror stands: all there until the removal begins
	sample := func() int {
		dirs, _ := filepath.Glob(filepath.Join(mirrors, "*", "objects", "bulk"))
		left := 0
		for i := 0; i < files; i += files / 64 {
			for _, d := range dirs {
				if _, err := os.Lstat(filepath.Join(d, fmt.Sprintf("f%05d", i))); err == nil {
					left++
					break
				}
			}
		}
		return left
	}
	all := sample()
	prune := exec.Command(os.Args[0], "-test.run=^TestPruneInAnotherProcess$")
	prune.Env = append(os.Environ(), "MOMENTUM_TEST_PRUNE_MIRRORS="+mirrors)
	if err := prune.Start(); err != nil {
		t.Fatal(err)
	}
	began := false
	for end := time.Now().Add(time.Minute); time.Now().Before(end) && !began; time.Sleep(time.Millisecond) {
		began = sample() < all
	}
	_ = prune.Process.Kill()
	_ = prune.Wait()
	if !began {
		t.Fatal("the prune never began removing the mirror")
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("a prune stopped midway left a half mirror at the mirror's name: %v", err)
	}
	pruneMirrors(mirrors, nil, nil, nil)
	if entries, err := os.ReadDir(mirrors); err != nil || len(entries) != 0 {
		t.Fatalf("the next prune left %d entries (%v)", len(entries), err)
	}
}

// A preparation whose proposal notes cannot be ended (the store refuses to write them) goes on, and the log says why.
func TestAPreparationGoesOnWhenItsProposalNotesCannotEnd(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	var mu sync.Mutex
	var logged []string
	r.Ship.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	db, err := sql.Open("sqlite", "file:"+r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// a proposal note of a kind the store does not know, which it refuses to write again
	if _, err := db.Exec(`INSERT INTO notes (id, kind, title, at) VALUES (?, 'unknown', 'a note', 1)`,
		"loop-build:"+store.IDPart(c.ID)+":proposal:fixture"); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatalf("a preparation stopped on its proposal notes: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "proposal notes of "+c.ID) {
			return
		}
	}
	t.Fatalf("the log lacks the proposal notes' failure:\n%s", strings.Join(logged, "\n"))
}
