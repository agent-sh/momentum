package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A package prepared for one repository never builds another's tree: a checkout whose origin now names another
// repository gives the pass that repository's mirror, and the build refuses it rather than mount its tree, unscanned,
// as the public package's base.
func TestABuildRefusesAnotherRepositorysMirror(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	write(t, filepath.Join(f.dir, "public"), "")
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	other := newRepo(t, map[string]string{"private.txt": "marker-7f3a\n"})
	run(t, f.main, "git", "remote", "set-url", "origin", other.origin)
	newPass(t, r)
	built, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err == nil || !strings.Contains(err.Error(), "the case's repository changed after preparation") {
		t.Fatalf("a build took another repository's mirror: %v", err)
	}
	if built.Dir != "" {
		if _, err := os.Stat(filepath.Join(built.Dir, "work", "private.txt")); err == nil {
			t.Fatal("another repository's tree was mounted")
		}
	}
}

// A public base rests on the branch as the remote serves it now, which only this pass's fetch says: with the mirror's
// last fetch alone (a force-push purged the base since, and the fetch fails), the build waits rather than mount the
// purged tree unscanned.
func TestAPublicBaseWaitsForThisPassesFetch(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	write(t, filepath.Join(f.dir, "public"), "")
	pushOther(t, f, "main", map[string]string{"leak.txt": "marker-7f3a\n"})
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	// the leak is purged from origin's branch, and origin then cannot be fetched
	purge := t.TempDir()
	run(t, f.dir, "git", "clone", "-q", f.origin, purge)
	run(t, purge, "git", "checkout", "-q", "--orphan", "purged")
	run(t, purge, "git", "rm", "-q", "--cached", "leak.txt")
	if err := os.Remove(filepath.Join(purge, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	run(t, purge, "git", "commit", "-q", "-m", "purged")
	run(t, purge, "git", "push", "-q", "--force", "origin", "HEAD:main")
	if err := os.Rename(f.origin, f.origin+".away"); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if fallback := r.pass.manifest.Repositories[0].Fallback; !strings.HasPrefix(fallback, "origin's main as fetched at") {
		t.Fatalf("the pass did not read the mirror's last fetch: %q", fallback)
	}
	built, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if !held(err) || !strings.Contains(err.Error(), "cannot be fetched now") {
		t.Fatalf("a public base from a last fetch alone did not wait: %v (held %v)", err, held(err))
	}
	if built.Dir != "" {
		if _, err := os.Stat(filepath.Join(built.Dir, "work", "leak.txt")); err == nil {
			t.Fatal("the purged tree was mounted")
		}
	}
}

// A preparation whose proposal branch cannot be read now waits before it clones or removes anything, pass after pass,
// and the pass's log gets Git's line.
func TestAHeldProposalReadClonesNothing(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	var mu sync.Mutex
	var logged []string
	r.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	newPass(t, r) // the mirror holds a fetch
	if err := os.Rename(f.origin, f.origin+".away"); err != nil {
		t.Fatal(err)
	}
	clones := clonesCounted(t)
	for range 3 {
		newPass(t, r)
		_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
		if !held(err) || !strings.Contains(err.Error(), "the proposal branch of") {
			t.Fatalf("an unreadable proposal branch did not hold the preparation: %v", err)
		}
		r.buildSkipped(ctx, c, "prepare", err)
	}
	if got := clones(); len(got) != 0 {
		t.Fatalf("three held preparations made %d clones: %v", len(got), got)
	}
	if _, err := os.Stat(filepath.Join(r.Ship.Dir, "host", c.ID)); !os.IsNotExist(err) {
		t.Fatalf("a held preparation left a host clone: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "waits to read its proposal branch") && strings.Contains(line, "does not appear to be a git repository") {
			return
		}
	}
	t.Fatalf("the pass's log lacks git's line:\n%s", strings.Join(logged, "\n"))
}

// A note that says its hold ended but still stands (a pass that stopped between its two writes) gives way to the next
// hold of its class: the note stands again without the end it named.
func TestAnEndedNoteThatStandsGivesWayToTheNextHold(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	unreachableOrigin(t, f, closedConnection)
	newPass(t, r)
	hold := func() {
		t.Helper()
		_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
		if !held(err) {
			t.Fatalf("a mirror with no fetch did not hold the build: %v", err)
		}
		r.buildSkipped(ctx, c, "prepare", err)
	}
	hold()
	open, _ := baseNotesOf(t, r, c.ID)
	if len(open) != 1 {
		t.Fatalf("the hold's notes: %+v", open)
	}
	n := open[0]
	n.Detail += " " + baseFetched // the first of endBaseNotes's two writes, alone
	if _, err := r.Store.PutNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	hold()
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 1 || strings.Contains(open[0].Detail, "It ended") {
		t.Fatalf("the next hold kept a note that says its hold ended: %+v", open)
	}
}

// Once the pass's mirror holds a fetch, the notes of builds that waited for their base end, though the build then
// stops for another reason.
func TestBaseNotesEndOnceTheMirrorHoldsAFetch(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	unreachableOrigin(t, f, closedConnection)
	newPass(t, r)
	_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if !held(err) {
		t.Fatalf("a mirror with no fetch did not hold the build: %v", err)
	}
	r.buildSkipped(ctx, c, "prepare", err)
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 1 {
		t.Fatalf("the hold's notes: %+v", open)
	}
	run(t, f.main, "git", "remote", "set-url", "origin", f.origin)
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) { return "a session works in it", nil }
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err == nil || !strings.Contains(err.Error(), "is in use") {
		t.Fatalf("a repository in use did not stop the build: %v", err)
	}
	if open, done := baseNotesOf(t, r, c.ID); len(open) != 0 || len(done) != 1 || !strings.HasSuffix(done[0].Detail, baseFetched) {
		t.Fatalf("a fetched mirror left the hold's notes open: open %+v, done %+v", open, done)
	}
}

// A checkout with no origin/HEAD, whose origin's default branch is trunk, is built: the base branch is the one the
// mirror's record names, and nothing reads the checkout's remote-tracking HEAD.
func TestABuildOfACheckoutWithoutOriginHEAD(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	run(t, f.dir, "git", "--git-dir", f.origin, "branch", "-m", "main", "trunk")
	run(t, f.main, "git", "fetch", "-q", "--prune", "origin")
	run(t, f.main, "git", "remote", "set-head", "origin", "-d")
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatalf("the build of a checkout without origin/HEAD: %v (held %v)", err, held(err))
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil || p.Base != "trunk" || p.BaseCommit != originHead(t, f, "trunk") {
		t.Fatalf("the package's base is %s at %s (%v)", p.Base, p.BaseCommit, err)
	}
}
