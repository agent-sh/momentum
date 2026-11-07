package loop

import (
	"context"
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

// proposalReadFails puts on PATH a Git whose read of a proposal branch (ls-remote --refs) fails as a server name that
// does not resolve does, while the pass's fetch (ls-remote --symref, fetch) goes through.
func proposalReadFails(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin := t.TempDir()
	script(t, bin, "git", `case " $* " in *" ls-remote --refs "*) echo "fatal: unable to access 'https://github.com/agent-sh/fake.git/': Could not resolve host: github.com" >&2; exit 128;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// proposalNotesOf are a case's notes of builds waiting to read their proposal branch, open and done.
func proposalNotesOf(t *testing.T, r *Runner, caseID string) (open, done []store.Note) {
	t.Helper()
	notes, err := r.Store.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(caseID)+":proposal:") {
			if n.Done.IsZero() {
				open = append(open, n)
			} else {
				done = append(done, n)
			}
		}
	}
	return open, done
}

// A build whose mirror holds this pass's fetch but whose proposal branch cannot be read now waits with a note of its
// own. The fetch does not end it: a note a person took out of the report stays out while the hold lasts, and a day's
// hold raises it. Only a read of the proposal branch that goes through ends it.
func TestAProposalReadHoldHasANoteOfItsOwn(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	clock := time.Now()
	r.Now = func() time.Time { return clock }
	path := os.Getenv("PATH")
	proposalReadFails(t)
	hold := func() {
		t.Helper()
		newPass(t, r)
		if fallback := r.pass.manifest.Repositories[0].Fallback; fallback != "" {
			t.Fatalf("the pass did not fetch: %s", fallback)
		}
		_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
		if !held(err) || !strings.Contains(err.Error(), "the proposal branch of") {
			t.Fatalf("an unreadable proposal branch did not hold the build: %v (held %v)", err, held(err))
		}
		r.buildSkipped(ctx, c, "prepare", err)
	}
	// the notes that name the proposal branch, under any id the case's builds use
	standing := func() []store.Note {
		t.Helper()
		notes, err := r.Store.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var open []store.Note
		for _, n := range notes {
			if strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(c.ID)+":") && strings.Contains(n.Detail, "the proposal branch of") && n.Done.IsZero() {
				open = append(open, n)
			}
		}
		return open
	}
	hold()
	open := standing()
	if len(open) != 1 {
		t.Fatalf("the hold's notes: %+v", open)
	}
	if _, _, err := r.Store.DoneNote(ctx, open[0].ID, r.now()); err != nil {
		t.Fatal(err)
	}
	hold()
	if open := standing(); len(open) != 0 {
		t.Fatalf("a note taken out of the report came back while its hold lasts: %+v", open)
	}
	clock = clock.Add(baseHoldRaise + time.Minute)
	hold()
	open = standing()
	if len(open) != 1 || open[0].Importance != store.ImportanceNormal {
		t.Fatalf("a day's hold did not bring its note back at normal importance: %+v", open)
	}
	if !strings.HasPrefix(open[0].ID, "loop-build:"+store.IDPart(c.ID)+":proposal:") ||
		open[0].Title != "The loop's build for case "+c.ID+" waits to read its proposal branch" {
		t.Fatalf("the hold's note is not the proposal read's own: %s %q", open[0].ID, open[0].Title)
	}
	if open, done := baseNotesOf(t, r, c.ID); len(open)+len(done) != 0 {
		t.Fatalf("a proposal read's hold wrote base notes: %+v %+v", open, done)
	}
	// the proposal branch is read: the hold ended
	t.Setenv("PATH", path)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatalf("the build once its proposal branch is read: %v", err)
	}
	if open, done := proposalNotesOf(t, r, c.ID); len(open) != 0 || len(done) != 1 || !strings.HasSuffix(done[0].Detail, "It ended: its proposal branch was read.") {
		t.Fatalf("a read of the proposal branch did not end its hold's note: open %+v, done %+v", open, done)
	}
}

// A case that leaves testing ends its proposal read's hold and its note.
func TestAProposalReadHoldEndsWhenItsCaseLeavesTesting(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	proposalReadFails(t)
	newPass(t, r)
	_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if !held(err) {
		t.Fatalf("an unreadable proposal branch did not hold the build: %v", err)
	}
	r.buildSkipped(ctx, c, "prepare", err)
	if open, _ := proposalNotesOf(t, r, c.ID); len(open) != 1 {
		t.Fatalf("the hold's notes: %+v", open)
	}
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	cs, err := r.Store.Cases(ctx, store.CaseFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.releaseHolds(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if open, done := proposalNotesOf(t, r, c.ID); len(open) != 0 || len(done) != 1 || !strings.HasSuffix(done[0].Detail, caseLeftTesting) {
		t.Fatalf("the case left testing and its proposal read's note: open %+v, done %+v", open, done)
	}
}

// A build held for its mirror puts Git's line in the pass's log once: the pass's fetch logged it already.
func TestAMirrorHoldLogsGitsLineOnce(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	var mu sync.Mutex
	var logged []string
	r.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	unreachableOrigin(t, f, closedConnection)
	newPass(t, r)
	_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if !held(err) {
		t.Fatalf("a mirror with no fetch did not hold the build: %v", err)
	}
	r.buildSkipped(ctx, c, "prepare", err)
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, line := range logged {
		if strings.Contains(line, "Connection closed by 192.0.2.1 port 22") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("git's line is in the pass's log %d times:\n%s", n, strings.Join(logged, "\n"))
	}
}

// A build that goes on from its mirror's last fetch, past the public check, ends the notes of builds that waited for
// their base; a public base, which waits for this pass's fetch, leaves them open.
func TestBaseNotesEndOnceABuildGoesOnFromALastFetch(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(f.dir, "public")
	write(t, public, "")
	if err := os.Rename(f.origin, f.origin+".away"); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if !held(err) || !strings.Contains(err.Error(), "cannot be fetched now") {
		t.Fatalf("a public base from a last fetch alone did not wait: %v (held %v)", err, held(err))
	}
	r.buildSkipped(ctx, c, "prepare", err)
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 1 {
		t.Fatalf("the hold's notes: %+v", open)
	}
	if err := os.Remove(public); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if fallback := r.pass.manifest.Repositories[0].Fallback; !strings.HasPrefix(fallback, "origin's main as fetched at") {
		t.Fatalf("the pass did not read the mirror's last fetch: %q", fallback)
	}
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatalf("a private base from the mirror's last fetch: %v (held %v)", err, held(err))
	}
	if open, done := baseNotesOf(t, r, c.ID); len(open) != 0 || len(done) != 1 || !strings.HasSuffix(done[0].Detail, baseFetched) {
		t.Fatalf("a build that went on from the last fetch left the hold's notes open: open %+v, done %+v", open, done)
	}
}

// A GitHub repository's mirror must hold the package's repository, though the checkout's origin and the host clone's
// both name another: at preparation, and at a later build.
func TestAGitHubMirrorMustHoldThePackagesRepository(t *testing.T) {
	for _, when := range []string{"at preparation", "at a later build"} {
		t.Run(when, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			was := githubURL
			t.Cleanup(func() { githubURL = was })
			githubURL = func(string) string { return f.origin } // every GitHub repository is served from the fixture's origin
			run(t, f.main, "git", "remote", "set-url", "origin", "git@github.com:agent-sh/fake.git")
			if when == "at a later build" {
				newPass(t, r)
				if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
					t.Fatal(err)
				}
				p, _, err := r.Ship.Load(c.ID)
				if err != nil {
					t.Fatal(err)
				}
				run(t, p.Host, "git", "remote", "set-url", "origin", "git@github.com:agent-sh/other.git")
			}
			// the gate still names the package's repository, agent-sh/fake, while origin names another
			run(t, f.main, "git", "remote", "set-url", "origin", "git@github.com:agent-sh/other.git")
			newPass(t, r)
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err == nil || !strings.Contains(err.Error(), "the case's repository changed after preparation") {
				t.Fatalf("a build took another GitHub repository's mirror: %v", err)
			}
		})
	}
}

// A package's checkout that the gate now names as another repository refuses the build, though its origin's URL, and
// so its mirror, stayed the same.
func TestABuildRefusesACheckoutThatNowNamesAnotherRepository(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	r.Ship.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/other", nil }
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err == nil || !strings.Contains(err.Error(), "the case's repository changed after preparation") {
		t.Fatalf("a build of a checkout that names another repository: %v", err)
	}
}

// The owner's preparation holds its mirror from the fetch through the clone: a pass that prunes the mirrors in between
// leaves it.
func TestTheOwnersPreparationHoldsItsMirrorThroughTheClone(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	ctx := context.Background()
	r, c, _ := buildCase(t)
	g := newRepo(t, nil) // a repository outside loop.read_dirs
	bin := t.TempDir()
	cloning, resume := filepath.Join(bin, "cloning"), filepath.Join(bin, "resume")
	script(t, bin, "git", `case " $* " in *" clone "*) : > '`+cloning+`'; i=0; while [ ! -e '`+resume+`' ] && [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	prepared := make(chan error, 1)
	go func() {
		_, err := r.Ship.Prepare(ctx, c.ID, g.main)
		prepared <- err
	}()
	if !comesTrue(2*time.Minute, func() bool { _, err := os.Stat(cloning); return err == nil }) {
		t.Fatal("the preparation never cloned")
	}
	pruneMirrors(filepath.Join(r.Dir, "mirrors"), nil, nil) // a pass that reads no checkout of the repository
	write(t, resume, "")
	if err := <-prepared; err != nil {
		t.Fatalf("a pass's prune between the preparation's fetch and its clone: %v", err)
	}
	if _, err := os.Stat(identityMirror(r, "url:"+g.origin)); err != nil {
		t.Fatalf("a pass pruned the mirror the preparation held: %v", err)
	}
}

// A pass keeps the mirror of a repository a package names, though no loop.read_dirs checkout reads it, so the owner's
// next preparation there fetches only what is new; a mirror nothing names still goes.
func TestAPassKeepsTheMirrorOfAPackagesRepository(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	g, h := newRepo(t, nil), newRepo(t, nil) // repositories outside loop.read_dirs
	if _, err := r.Ship.Prepare(ctx, c.ID, g.main); err != nil {
		t.Fatal(err)
	}
	unnamed := identityMirror(r, "url:"+h.origin)
	if _, why := fetchTip(ctx, h.main, unnamed, h.origin, ""); why != "" {
		t.Fatal(why)
	}
	newPass(t, r)
	if _, err := os.Stat(identityMirror(r, "url:"+g.origin)); err != nil {
		t.Fatalf("a pass removed the mirror of a package's repository: %v", err)
	}
	if _, err := os.Stat(unnamed); !os.IsNotExist(err) {
		t.Fatalf("a pass kept a mirror no checkout or package names: %v", err)
	}
}
