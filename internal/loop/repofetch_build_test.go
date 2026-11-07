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

// unreachableOrigin points the checkout's origin at a remote whose ssh fails as says (its diagnostics), a repository
// no mirror holds yet: a pass that reads it has no usable fetch of it.
func unreachableOrigin(t *testing.T, f repoFixture, says string) {
	t.Helper()
	bin := t.TempDir()
	script(t, bin, "ssh", says+"\nexit 255")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	run(t, f.main, "git", "remote", "set-url", "origin", "ssh://git@fixture.invalid/fixture/fake.git")
}

const (
	closedConnection   = `printf 'Connection closed by 192.0.2.1 port 22\r\n' >&2`
	repositoryNotFound = `echo "ERROR: Repository not found." >&2`
)

// baseNotesOf are a case's notes of builds waiting for their base, open and done.
func baseNotesOf(t *testing.T, r *Runner, caseID string) (open, done []store.Note) {
	t.Helper()
	notes, err := r.Store.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(caseID)+":base:") {
			if n.Done.IsZero() {
				open = append(open, n)
			} else {
				done = append(done, n)
			}
		}
	}
	return open, done
}

// A build waits only when its repository's mirror has no usable fetch: none ever completed, whatever the fetch's
// failure, and the note names its class. With a last fetch, the build goes on from the mirror as that fetch left it.
func TestABuildWaitsOnlyForAMirrorWithNoUsableFetch(t *testing.T) {
	for _, c := range []struct{ name, says, class string }{
		{"a closed connection", closedConnection, "the connection broke off"},
		{"a missing repository", repositoryNotFound, "the remote has no such repository or branch"},
		{"a last fetch", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			r, cs, f := buildCase(t)
			if c.says == "" {
				// the package is prepared while origin answers, as an episode's first build is; a later pass's fetch fails
				newPass(t, r)
				if _, err := r.prepareRun(ctx, cs, cs.Family, StepBuild); err != nil {
					t.Fatal(err)
				}
				last := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
				newPass(t, r)
				if err := os.Rename(f.origin, f.origin+".away"); err != nil {
					t.Fatal(err)
				}
				newPass(t, r)
				if _, err := r.prepareRun(ctx, cs, cs.Family, StepBuild); err != nil {
					t.Fatalf("a build with its mirror's last fetch: %v (held %v)", err, held(err))
				}
				if p, _, err := r.Ship.Load(cs.ID); err != nil || p.BaseCommit != last {
					t.Fatalf("the base is %s, want the last fetch's %s (%v)", p.BaseCommit, last, err)
				}
				return
			}
			unreachableOrigin(t, f, c.says)
			newPass(t, r)
			_, err := r.prepareRun(ctx, cs, cs.Family, StepBuild)
			if !held(err) || !strings.Contains(err.Error(), "cannot be fetched now ("+c.class+")") {
				t.Fatalf("a mirror with no fetch did not hold the build by its class: %v (held %v)", err, held(err))
			}
		})
	}
}

// A build of a GitHub repository reads its base without the network: the pass's mirror fetched it over https by its
// name, and the build's host clone is refreshed from that mirror, so no ssh agent is asked for a key: a private
// repository's base and a public one's, whose ancestry is read from the mirror.
func TestABuildReadsAGitHubBaseOverHTTPS(t *testing.T) {
	for _, kind := range []string{"private", "public"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			newPass(t, r)
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil { // the package, as an episode's first build has it
				t.Fatal(err)
			}
			if kind == "public" {
				write(t, filepath.Join(f.dir, "public"), "")
			}
			bin := t.TempDir()
			calls := filepath.Join(bin, "calls")
			script(t, bin, "ssh", `echo "$@" >> '`+calls+`'; exit 255`)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			was := githubURL
			t.Cleanup(func() { githubURL = was })
			var mu sync.Mutex
			var asked []string
			githubURL = func(repo string) string { mu.Lock(); defer mu.Unlock(); asked = append(asked, repo); return f.origin }
			p, _, err := r.Ship.Load(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{f.main, p.Host} { // the host clone's origin names the repository as the checkout's does
				run(t, dir, "git", "remote", "set-url", "origin", "git@github.com:agent-sh/fake.git")
			}
			tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			newPass(t, r)
			mu.Lock()
			fetched := len(asked)
			mu.Unlock()
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
				t.Fatalf("the build of a GitHub base: %v (held %v)", err, held(err))
			}
			p, _, err = r.Ship.Load(c.ID)
			if err != nil || p.BaseCommit != tip {
				t.Fatalf("the base is %s, want %s (%v)", p.BaseCommit, tip, err)
			}
			if raw, err := os.ReadFile(calls); err == nil {
				t.Fatalf("the build ran ssh:\n%s", raw)
			}
			mu.Lock()
			defer mu.Unlock()
			if fetched == 0 || len(asked) != fetched {
				t.Fatalf("the pass fetched %d times and the build %d", fetched, len(asked)-fetched)
			}
		})
	}
}

// No remote call of the host's has an https low-speed limit: a quiet fetch gets about a byte a second while the server
// compresses, and each call's own deadline bounds it.
func TestARemoteCallHasNoLowSpeedLimit(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script(t, bin, "git", `case "$*" in *lowSpeedLimit*) printf '%s\n' "$*" >> '`+calls+`';; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	passRepository(t, r, f.main)
	pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	passRepository(t, r, f.main)
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range lines {
		if !strings.Contains(line, "http.lowSpeedLimit=0 ") {
			t.Fatalf("a remote call with a low-speed limit: %s", line)
		}
	}
	if len(lines) != 4 { // each pass's ls-remote and fetch
		t.Fatalf("%d remote calls, want 4", len(lines))
	}
}

// clonesCounted puts on PATH a Git that counts the clones it makes, and returns them so far.
func clonesCounted(t *testing.T) func() []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin := t.TempDir()
	clones := filepath.Join(bin, "clones")
	script(t, bin, "git", `case " $* " in *" clone "*) printf '%s\n' "$*" >> '`+clones+`';; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return func() []string {
		raw, err := os.ReadFile(clones)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// A build that waits for its mirror clones nothing, pass after pass, and leaves an earlier package's host clone as it
// is; once the mirror holds a fetch, the build's host clone is a local clone of the mirror.
func TestAHeldBuildClonesNothing(t *testing.T) {
	for _, kind := range []string{"no clone yet", "an earlier package's clone"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			var old Package
			if kind == "an earlier package's clone" {
				newPass(t, r)
				if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
					t.Fatal(err)
				}
				var err error
				if old, _, err = r.Ship.Load(c.ID); err != nil {
					t.Fatal(err)
				}
				old.State = RolledBack
				if err := r.Ship.save(&old); err != nil {
					t.Fatal(err)
				}
			}
			clones := clonesCounted(t)
			unreachableOrigin(t, f, closedConnection)
			for range 3 {
				newPass(t, r)
				if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); !held(err) {
					t.Fatalf("a mirror with no fetch did not hold the build: %v", err)
				}
			}
			if got := clones(); len(got) != 0 {
				t.Fatalf("three held builds made %d clones: %v", len(got), got)
			}
			if kind == "an earlier package's clone" {
				run(t, old.Host, "git", "rev-parse", "--verify", "refs/heads/"+old.Branch)
			}
			// the remote answers again: the build's host clone comes from the mirror
			run(t, f.main, "git", "remote", "set-url", "origin", f.origin)
			newPass(t, r)
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
				t.Fatal(err)
			}
			p, _, err := r.Ship.Load(c.ID)
			if err != nil || p.State != Prepared || p.BaseCommit != originHead(t, f, "main") {
				t.Fatalf("the build after the hold: %+v %v", p, err)
			}
			if got := clones(); len(got) != 1 || !strings.Contains(got[0], identityMirror(r, "url:"+f.origin)) {
				t.Fatalf("the build's clones: %v, want one of the mirror", got)
			}
			if url := strings.TrimSpace(run(t, p.Host, "git", "remote", "get-url", "origin")); url != f.origin {
				t.Fatalf("the host clone's origin is %s, want the checkout's", url)
			}
		})
	}
}

// A base note a person took out of the report stays out while its hold lasts at the importance it was taken out at,
// and comes back once when that rises; once a build fetched its base, the next hold of its class opens it again.
func TestABaseNoteTakenOutStaysOutWhileItsHoldLasts(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	clock := time.Now()
	r.Now = func() time.Time { return clock }
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
	dismiss := func() {
		t.Helper()
		open, _ := baseNotesOf(t, r, c.ID)
		if len(open) != 1 {
			t.Fatalf("the hold's notes: %+v", open)
		}
		if _, _, err := r.Store.DoneNote(ctx, open[0].ID, r.now()); err != nil {
			t.Fatal(err)
		}
	}
	hold()
	dismiss()
	hold()
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 0 {
		t.Fatalf("a note taken out of the report came back while its hold lasts: %+v", open)
	}
	clock = clock.Add(baseHoldRaise + time.Minute)
	hold()
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 1 || open[0].Importance != store.ImportanceNormal {
		t.Fatalf("a day's hold did not bring its note back at normal importance: %+v", open)
	}
	dismiss()
	hold()
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 0 {
		t.Fatalf("a note taken out at normal importance came back: %+v", open)
	}
	// a build fetches its base: the hold ended, and the next one is a new hold
	run(t, f.main, "git", "remote", "set-url", "origin", f.origin)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	unreachableOrigin(t, f, closedConnection)
	newPass(t, r)
	hold()
	if open, _ := baseNotesOf(t, r, c.ID); len(open) != 1 || !strings.Contains(open[0].Detail, "cannot be fetched now (the connection broke off)") ||
		strings.Contains(open[0].Detail, "It ended") || open[0].Importance != store.ImportanceLow {
		t.Fatalf("a new hold did not open its note again: %+v", open)
	}
}

// A note that says its hold ended but still stands (a pass that stopped between the two writes) is only resolved: its
// text says the end once.
func TestAnEndedBaseNoteThatStandsIsResolved(t *testing.T) {
	ctx := context.Background()
	r, c := inputRunner(t)
	id := baseNotePrefix(c.ID) + "fixture"
	detail := "refused: the default branch of main cannot be fetched now (the connection broke off) " + baseFetched
	if _, err := r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: time.Now(),
		Title: "The loop's build for case " + c.ID + " waits for its base", Detail: detail}); err != nil {
		t.Fatal(err)
	}
	if err := r.endBaseNotes(ctx, c.ID, baseFetched); err != nil {
		t.Fatal(err)
	}
	open, done := baseNotesOf(t, r, c.ID)
	if len(open) != 0 || len(done) != 1 || strings.Count(done[0].Detail, "It ended") != 1 {
		t.Fatalf("an ended note that stood: open %+v, done %+v", open, done)
	}
}

// A build's host clone stands at its base, the commit the pass's snapshot read, though the mirror's branch moved past it
// since: the branch is made at that commit, not at the mirror's tip.
func TestAPreparedCloneStandsAtItsBase(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	snapshot := r.pass.manifest.Repositories[0].Commit
	pushOther(t, f, "main", map[string]string{"later.txt": "merged after the pass read it\n"})
	if _, why := fetchTip(ctx, f.main, identityMirror(r, "url:"+f.origin), f.origin, ""); why != "" {
		t.Fatal(why)
	}
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil || p.BaseCommit != snapshot {
		t.Fatalf("the base is %s, want the snapshot's %s (%v)", p.BaseCommit, snapshot, err)
	}
	if head := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", "HEAD")); head != snapshot {
		t.Fatalf("the host clone stands at %s, not at its base %s", head, snapshot)
	}
}

// A held build's note names its class, not Git's text, and the pass's log gets Git's line that shows it, not ssh's
// warning before it. After a day of waiting the note is raised to normal importance; a build that fetches its base
// resolves it.
func TestABaseHoldNotesItsClassAndIsRaisedAfterADay(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	clock := time.Now()
	r.Now = func() time.Time { return clock }
	var mu sync.Mutex
	var logged []string
	r.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	// a transport whose credentials are gone, after ssh's warning about its key exchange, which comes first
	unreachableOrigin(t, f, `echo "** WARNING: connection is not using a post-quantum key exchange algorithm." >&2
echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2`)
	newPass(t, r)
	hold := func() {
		t.Helper()
		_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
		if !held(err) {
			t.Fatalf("lost credentials did not hold the build: %v", err)
		}
		r.buildSkipped(ctx, c, "prepare", err)
	}
	hold()
	notes, _ := baseNotesOf(t, r, c.ID)
	if len(notes) != 1 || !strings.Contains(notes[0].Detail, "cannot be fetched now (no credentials)") || notes[0].Importance != store.ImportanceLow {
		t.Fatalf("the hold's notes: %+v", notes)
	}
	mu.Lock()
	found := false
	for _, line := range logged {
		found = found || strings.Contains(line, "could not read Username for 'https://github.com': terminal prompts disabled")
		if strings.Contains(line, "post-quantum") {
			t.Fatalf("the pass's log took ssh's warning for git's failure: %s", line)
		}
	}
	mu.Unlock()
	if !found {
		t.Fatalf("the pass's log lacks git's line:\n%s", strings.Join(logged, "\n"))
	}
	clock = clock.Add(baseHoldRaise / 2)
	hold()
	if notes, _ := baseNotesOf(t, r, c.ID); len(notes) != 1 || notes[0].Importance != store.ImportanceLow {
		t.Fatalf("half a day's hold: %+v", notes)
	}
	clock = clock.Add(baseHoldRaise/2 + time.Minute)
	hold()
	if notes, _ := baseNotesOf(t, r, c.ID); len(notes) != 1 || notes[0].Importance != store.ImportanceNormal {
		t.Fatalf("a day's hold was not raised: %+v", notes)
	}
	run(t, f.main, "git", "remote", "set-url", "origin", f.origin)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	if notes, _ := baseNotesOf(t, r, c.ID); len(notes) != 0 {
		t.Fatalf("a build that fetched its base left its hold's notes open: %+v", notes)
	}
}
