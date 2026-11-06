package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// passRepository prepares a pass's inputs over the one checkout and returns what it gave the repository, with the
// pass's summary.
func passRepository(t *testing.T, r *Runner, checkout string) (RepoInput, string) {
	t.Helper()
	r.Inputs.ReadDirs = []string{checkout}
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	if err := r.prepareInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := r.pass.manifest
	if len(m.Repositories) != 1 {
		t.Fatalf("repositories: %+v", m.Repositories)
	}
	return m.Repositories[0], m.Summary()
}

// Nothing pulls a checkout. A pass reads the repository at its origin's default branch as the host fetches it, so a
// checkout behind its origin gives the run origin's files and history, and the checkout itself does not move.
func TestARepositoryIsReadAtItsOriginsFetchedTip(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	repo, summary := passRepository(t, r, f.main)
	if repo.Error != "" || repo.Branch != "main" || repo.Commit != tip {
		t.Fatalf("the repository was not read at origin's tip %s: %+v", tip, repo)
	}
	if text, err := os.ReadFile(filepath.Join(repo.Path, "later.txt")); err != nil || string(text) != "merged later\n" {
		t.Fatalf("the snapshot lacks origin's later file: %q %v", text, err)
	}
	log, err := os.ReadFile(filepath.Join(r.pass.dir, "history", "repos", repo.Name+".log"))
	if err != nil || !strings.Contains(string(log), "someone else's") || repo.History.Commits != 2 {
		t.Fatalf("the log is not origin's: %d commits %v\n%s", repo.History.Commits, err, log)
	}
	if strings.Contains(summary, "checkout's") {
		t.Fatalf("a fetched repository is summarised as a fallback: %s", summary)
	}
	if got := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD")); got != local {
		t.Fatal("the read moved the checkout")
	}
	if refs := run(t, f.main, "git", "for-each-ref", "--format=%(refname) %(objectname)"); strings.Contains(refs, tip) {
		t.Fatalf("the fetch wrote into the checkout's refs:\n%s", refs)
	}
	// the case's diffs read the same objects: a commit only origin holds
	texts := []string{"the failure is in later.txt"}
	m, err := SnapshotDiffs(context.Background(), r.pass.manifest.Repositories, texts, filepath.Join(t.TempDir(), "diffs"), defaultSince(), nil)
	if err != nil || m.Commits != 1 || m.Error != "" {
		t.Fatalf("the diffs did not read origin's commit: %+v %v", m, err)
	}
}

// A checkout whose origin cannot be fetched, or that has none, is read at its own default branch, and the run's
// input summary says so.
func TestAFetchThatFailsFallsBackToTheCheckoutAndSaysSo(t *testing.T) {
	for _, kind := range []string{"fetch failed", "no origin"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
			if kind == "no origin" {
				run(t, f.main, "git", "remote", "remove", "origin")
			} else {
				run(t, f.main, "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
			}
			repo, summary := passRepository(t, r, f.main)
			if repo.Error != "" || repo.Commit != local {
				t.Fatalf("the fallback did not read the checkout's main at %s: %+v", local, repo)
			}
			if _, err := os.Stat(filepath.Join(repo.Path, "later.txt")); !os.IsNotExist(err) {
				t.Fatal("the fallback read a file the checkout does not have")
			}
			if !strings.Contains(summary, "at "+local[:7]+" (checkout's main: "+kind) {
				t.Fatalf("the summary does not say the checkout was read: %s", summary)
			}
			if strings.Contains(summary, f.dir) || strings.Contains(summary, "gone.git") {
				t.Fatalf("the summary names a host path: %s", summary)
			}
		})
	}
}

// newPass starts a pass of the runner over its read_dirs: the inputs a pass prepares once, the repositories' snapshots
// among them.
func newPass(t *testing.T, r *Runner) {
	t.Helper()
	if r.pass != nil {
		r.pass.close()
	}
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	if err := r.prepareInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// buildCase is a runner whose case in testing names the fixture repository, the one read_dirs repository.
func buildCase(t *testing.T) (*Runner, store.Case, repoFixture) {
	t.Helper()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	c, err := r.Store.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	return r, c, f
}

// A build's base is the default branch as the host fetched it for the build's pass, not the tree the episode prepared
// first: a package prepared before origin moved, and a revision after a review, both rework the change on the new
// tip of a later pass, with the earlier patch as previous.patch.
func TestABuildBaseFollowsTheFetchedTip(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	first, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// origin moves while the package waits for its next build, in a later pass
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	newPass(t, r)
	second, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil || first.BaseCommit == tip || p.BaseCommit != tip || p.Work != filepath.Join(second.Dir, "work") {
		t.Fatalf("the build kept the base the episode prepared: %s, want %s (%v)", p.BaseCommit, tip, err)
	}
	if text, err := os.ReadFile(filepath.Join(p.Work, "later.txt")); err != nil || string(text) != "merged later\n" {
		t.Fatalf("the build tree is not at the fetched tip: %q %v", text, err)
	}
	if prompt, err := readHostInput(HostDir(second.Dir), "prompt.json"); err != nil || !strings.Contains(string(prompt), tip) || strings.Contains(string(prompt), "stop at commit") {
		t.Fatalf("the build prompt does not name the fetched base alone: %v", err)
	}
	// a revision after a review: its base moves too, and its earlier patch reaches the run
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	previous, err := readPatch(fixturePatch(t, r.Ship, c.ID))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Ship.pkgDir(c.ID), "change.patch"), string(previous))
	p.Head, p.State = p.BaseCommit, Revising
	p.Reviews = []Review{{Round: 1, Medium: 1, Text: "Name the cause.", Head: p.Head, Comment: "posted"}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	next := pushOther(t, f, "main", map[string]string{"notes.txt": "plain notes\n"})
	newPass(t, r)
	third, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = r.Ship.Load(c.ID)
	if err != nil || p.BaseCommit != next || p.Head != "" || p.State != Prepared {
		t.Fatalf("the revision did not move to the fetched tip: %s %q %s (%v)", p.BaseCommit, p.Head, p.State, err)
	}
	if patch, err := readHostInput(HostDir(third.Dir), "input/previous.patch"); err != nil || string(patch) != string(previous) {
		t.Fatal("the revision lost its earlier patch")
	}
	// a base that did not move is made again from the same commit
	newPass(t, r)
	fourth, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	if again, _, err := r.Ship.Load(c.ID); err != nil || again.BaseCommit != next || again.Work != filepath.Join(fourth.Dir, "work") {
		t.Fatalf("an unmoved base changed: %s (%v)", again.BaseCommit, err)
	}
}

// A build's base is the commit the pass's snapshot read while the fetched branch still holds it, so the run's
// history and BASE agree. When the branch no longer holds it, the base is the fetched tip and the prompt says the
// history stops at the snapshot's commit.
func TestABuildBaseIsThePassesSnapshotWhileTheBranchHoldsIt(t *testing.T) {
	for _, kind := range []string{"on the branch", "purged"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			newPass(t, r)
			snapshot := r.pass.manifest.Repositories[0].Commit
			want := snapshot
			if kind == "on the branch" {
				pushOther(t, f, "main", map[string]string{"later.txt": "merged after the pass read it\n"})
			} else {
				other := t.TempDir()
				run(t, f.dir, "git", "clone", "-q", f.origin, other)
				run(t, other, "git", "checkout", "-q", "--orphan", "purged")
				write(t, filepath.Join(other, "fake.go"), fakeGo)
				run(t, other, "git", "add", "-A")
				run(t, other, "git", "commit", "-q", "-m", "purged")
				run(t, other, "git", "push", "-q", "--force", "origin", "HEAD:main")
				want = strings.TrimSpace(run(t, other, "git", "rev-parse", "HEAD"))
			}
			built, err := r.prepareRun(ctx, c, c.Family, StepBuild)
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := r.Ship.Load(c.ID)
			if err != nil || p.BaseCommit != want {
				t.Fatalf("the base is %s, want %s (%v)", p.BaseCommit, want, err)
			}
			prompt, err := readHostInput(HostDir(built.Dir), "prompt.json")
			if err != nil {
				t.Fatal(err)
			}
			if note := strings.Contains(string(prompt), "stop at commit "+snapshot); note != (kind == "purged") {
				t.Fatalf("the prompt's note of where the history stops: %v\n%s", note, prompt)
			}
		})
	}
}

// A build whose base cannot be fetched now is held for the pass: no run, no charge.
func TestABuildBaseThatCannotBeFetchedHoldsTheBuild(t *testing.T) {
	ctx := context.Background()
	r, c, _ := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	run(t, p.Host, "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, err = r.prepareRun(ctx, c, c.Family, StepBuild)
	if err == nil || !held(err) || !strings.Contains(err.Error(), "cannot be fetched now") {
		t.Fatalf("an unfetchable base was not a hold: %v", err)
	}
}

// stalledListener accepts connections and never answers on them, as a server that hangs does; it lets each go after
// hold, so a call that never ends by itself still ends.
func stalledListener(t *testing.T, hold time.Duration) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(hold); conn.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A remote call ends at its deadline, though its ssh or https helper waits on a server that never answers: the whole
// process group goes, and the error says which deadline ended it.
func TestARemoteCallEndsAtItsDeadline(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh here")
	}
	port := stalledListener(t, 20*time.Second)
	const deadline = 3 * time.Second
	bound := deadline + gitWaitDelay + 2*time.Second
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	inputTestGit(t, filepath.Dir(mirror), nil, "init", "-q", "--bare", mirror)
	ssh := fmt.Sprintf("ssh://git@127.0.0.1:%d/x.git", port)
	f := newRepo(t, nil)
	ship := shipWithSandbox(testStore(t), f, Sandbox{})
	calls := map[string]func(context.Context) error{
		"ssh fetch": func(ctx context.Context) error {
			_, err := fetchGit(ctx, mirror, "", "ls-remote", ssh, "HEAD")
			return err
		},
		"https fetch": func(ctx context.Context) error {
			_, err := fetchGit(ctx, mirror, "", "ls-remote", fmt.Sprintf("https://127.0.0.1:%d/x.git", port), "HEAD")
			return err
		},
		"ship": func(ctx context.Context) error {
			_, err := ship.git(ctx, f.main, "ls-remote", ssh, "HEAD")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			start := time.Now()
			err := call(ctx)
			if took := time.Since(start); took > bound {
				t.Fatalf("the call returned %s after its %s deadline", took.Round(time.Second), deadline)
			}
			if err == nil || !strings.Contains(err.Error(), "the caller's deadline passed") {
				t.Fatalf("the error does not say which deadline ended it: %v", err)
			}
		})
	}
}

// The checkout's own settings never reach the fetch, which runs in the mirror with the host's pins, so settings common
// in a checkout (a file-system monitor, a credential helper, a large-file filter, an include) do not stop it.
func TestTheCheckoutsOwnSettingsDoNotStopItsFetch(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	for _, kv := range [][2]string{{"core.fsmonitor", "true"}, {"credential.helper", "store"},
		{"filter.lfs.process", "git-lfs filter-process"}, {"include.path", "extra.config"}} {
		run(t, f.main, "git", "config", kv[0], kv[1])
	}
	if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
		t.Fatalf("a checkout's own setting stopped the fetch: %s", summary)
	}
}

// A fetch the host refuses says why in the summary, and names no URL and no key, either of which can hold a token.
func TestARefusedFetchSaysWhyWithoutAURL(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"rewrite", "fetch refused: the checkout's Git configuration rewrites remote URLs (its url section)"},
		{"push elsewhere", "fetch refused: origin's fetch and push destinations differ"},
		{"protocol", "fetch refused: origin's URL is not https, ssh or a local path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
			switch c.name {
			case "rewrite":
				run(t, f.main, "git", "config", "url.https://fixture:tok3n-7f3a@example.com/.insteadOf", "https://example.com/")
			case "push elsewhere":
				run(t, f.main, "git", "config", "remote.origin.pushurl", "https://fixture:tok3n-7f3a@example.com/elsewhere.git")
			case "protocol":
				run(t, f.main, "git", "remote", "set-url", "origin", "http://fixture:tok3n-7f3a@127.0.0.1:1/fake.git")
			}
			repo, summary := passRepository(t, r, f.main)
			if repo.Commit != local || !strings.Contains(summary, "(checkout's main: "+c.want) {
				t.Fatalf("the summary does not say why the checkout was read:\n%s", summary)
			}
			if strings.Contains(summary, "tok3n") || strings.Contains(summary, "example.com") {
				t.Fatalf("the summary names a URL:\n%s", summary)
			}
		})
	}
}

// A fetch never prompts: the ssh it runs is in batch mode, with no askpass program in its environment, whatever the
// session that runs the loop sets.
func TestAFetchNeverPrompts(t *testing.T) {
	bin := t.TempDir()
	calls, prompted := filepath.Join(bin, "calls"), filepath.Join(bin, "prompted")
	script(t, bin, "askpass", "touch '"+prompted+"'; echo answer")
	// an ssh that asks the askpass program, as a real one does for a passphrase, unless it is in batch mode
	script(t, bin, "ssh", `printf '%s\n' "$@" > '`+calls+`'
env | grep -E '^(SSH_ASKPASS|SSH_ASKPASS_REQUIRE|GIT_TERMINAL_PROMPT|GIT_ASKPASS)=' >> '`+calls+`'
printf '%s\n' "$@" | grep -qx BatchMode=yes || { [ -n "$SSH_ASKPASS" ] && "$SSH_ASKPASS" passphrase; }
exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SSH_ASKPASS", filepath.Join(bin, "askpass"))
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	t.Setenv("GIT_ASKPASS", filepath.Join(bin, "askpass"))
	t.Setenv("DISPLAY", ":0")
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	run(t, f.main, "git", "remote", "set-url", "origin", "ssh://git@example.invalid/fixture/fake.git")
	_, summary := passRepository(t, r, f.main)
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the fetch did not run ssh: %v", err)
	}
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=30", "ServerAliveInterval=15", "ServerAliveCountMax=3", "SSH_ASKPASS_REQUIRE=never", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("ssh ran without %s:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "SSH_ASKPASS=") || strings.Contains(string(got), "GIT_ASKPASS=") {
		t.Fatalf("ssh had an askpass program:\n%s", got)
	}
	if _, err := os.Stat(prompted); !os.IsNotExist(err) {
		t.Fatal("the fetch prompted")
	}
	if !strings.Contains(summary, "(checkout's main: fetch failed") {
		t.Fatalf("the failed fetch is not in the summary: %s", summary)
	}
}

// A GitHub repository is fetched over https by its name, never over ssh, and every checkout of it, by any of its
// URLs, shares one mirror.
func TestAGitHubOriginIsFetchedOverHTTPSByItsName(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script(t, bin, "ssh", `echo "$@" >> '`+calls+`'; exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	behind := filepath.Join(t.TempDir(), "behind")
	run(t, f.dir, "git", "clone", "-q", f.origin, behind)
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	was := githubURL
	t.Cleanup(func() { githubURL = was })
	var asked []string
	githubURL = func(repo string) string { asked = append(asked, repo); return f.origin }
	run(t, f.main, "git", "remote", "set-url", "origin", "git@github.com:agent-sh/fake.git")
	run(t, behind, "git", "remote", "set-url", "origin", "https://github.com/Agent-SH/fake")
	r.Inputs.ReadDirs = []string{f.main, behind}
	newPass(t, r)
	for _, repo := range r.pass.manifest.Repositories {
		if repo.Commit != tip || repo.Fallback != "" {
			t.Fatalf("a GitHub origin was not fetched by its name: %+v", repo)
		}
	}
	if len(asked) != 2 || asked[0] != "agent-sh/fake" || asked[1] != "agent-sh/fake" {
		t.Fatalf("the fetch asked for %v", asked)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("the fetch ran ssh")
	}
	if entries, err := os.ReadDir(filepath.Join(r.Dir, "mirrors")); err != nil || len(entries) != 1 {
		t.Fatalf("one repository has %d mirrors (%v)", len(entries), err)
	}
}

// identityMirror is where the mirror of a repository by its identity lies, as the test reads it.
func identityMirror(r *Runner, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(r.Dir, "mirrors", fmt.Sprintf("%x.git", sum[:8]))
}

// A mirror that is not as the host makes it (not bare, or with a setting that rewrites the URL it fetches) is made
// again before the fetch.
func TestAMirrorThatIsNotAsTheHostMadeItIsMadeAgain(t *testing.T) {
	for _, kind := range []string{"a work tree", "a rewrite"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			mirror := identityMirror(r, "url:"+f.origin)
			if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "a work tree" {
				inputTestGit(t, filepath.Dir(mirror), nil, "init", "-q", mirror)
				write(t, filepath.Join(mirror, "stale.txt"), "stale\n")
			} else {
				elsewhere := newRepo(t, map[string]string{"elsewhere.txt": "elsewhere\n"})
				inputTestGit(t, filepath.Dir(mirror), nil, "init", "-q", "--bare", mirror)
				inputTestGit(t, mirror, nil, "config", "url."+elsewhere.origin+".insteadOf", f.origin)
			}
			repo, summary := passRepository(t, r, f.main)
			if repo.Commit != tip || repo.Fallback != "" {
				t.Fatalf("the fetch did not read origin through a mirror made again: %s", summary)
			}
			if bare := inputTestGit(t, mirror, nil, "rev-parse", "--is-bare-repository"); bare != "true" {
				t.Fatal("the mirror is not bare")
			}
			if _, err := os.Stat(filepath.Join(mirror, "stale.txt")); !os.IsNotExist(err) {
				t.Fatal("the old mirror's files remain")
			}
			if config, _ := os.ReadFile(filepath.Join(mirror, "config")); strings.Contains(string(config), "insteadOf") {
				t.Fatal("the old mirror's rewrite remains")
			}
		})
	}
}

// datedCommit commits files in dir at a time in the past.
func datedCommit(t *testing.T, dir string, ago time.Duration, files map[string]string) string {
	t.Helper()
	for p, body := range files {
		write(t, filepath.Join(dir, p), body)
	}
	when := "@" + strconv.FormatInt(time.Now().Add(-ago).Unix(), 10) + " +0000"
	inputTestGit(t, dir, nil, "add", "-A")
	inputTestGit(t, dir, []string{"GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_DATE=" + when}, "commit", "-q", "-m", fmt.Sprintf("%s ago", ago))
	return inputTestGit(t, dir, nil, "rev-parse", "HEAD")
}

// A fetch takes the history window the log and the diffs read, with the parents of its oldest commits, so their
// changes are their own: none of the older history. A branch without a commit in the window comes as its tip alone.
func TestAFetchTakesOnlyTheHistoryWindow(t *testing.T) {
	const day = 24 * time.Hour
	for _, recent := range []bool{true, false} {
		t.Run(fmt.Sprintf("recent commits %v", recent), func(t *testing.T) {
			r, _ := inputRunner(t)
			parent := t.TempDir()
			origin, work := filepath.Join(parent, "origin.git"), filepath.Join(parent, "work")
			inputTestGit(t, parent, nil, "init", "-q", "--bare", "-b", "main", origin)
			inputTestGit(t, parent, nil, "clone", "-q", origin, work)
			oldest := datedCommit(t, work, 90*day, map[string]string{"a.txt": "1\n", "b.txt": "1\n"})
			tip := datedCommit(t, work, 60*day, map[string]string{"a.txt": "2\n"})
			if recent {
				datedCommit(t, work, 10*day, map[string]string{"a.txt": "3\n"})
				tip = datedCommit(t, work, day, map[string]string{"b.txt": "2\n"})
			}
			inputTestGit(t, work, nil, "push", "-q", "origin", "HEAD:main")
			repo, summary := passRepository(t, r, work)
			if repo.Commit != tip || repo.Fallback != "" {
				t.Fatalf("the window's tip was not read: %s", summary)
			}
			if _, err := hostInputGit(context.Background(), "", "--git-dir="+repo.objects, "cat-file", "-e", oldest+"^{commit}"); err == nil {
				t.Fatal("the mirror holds history older than the window")
			}
			log, err := os.ReadFile(filepath.Join(r.pass.dir, "history", "repos", repo.Name+".log"))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if recent {
				want = 2
			}
			if repo.History.Commits != want || recent && strings.Count(string(log), "  1 files changed") != 2 {
				t.Fatalf("the window's log has %d commits, want %d, each with its own change:\n%s", repo.History.Commits, want, log)
			}
		})
	}
}

// A mirror over mirrorMax after its fetch is dropped, and the pass reads the checkout and says why.
func TestAMirrorOverItsCapIsDropped(t *testing.T) {
	was := mirrorMax
	t.Cleanup(func() { mirrorMax = was })
	mirrorMax = 1 << 20
	r, _ := inputRunner(t)
	noise := make([]byte, 2<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	f := newRepo(t, map[string]string{"noise.bin": string(noise)})
	pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
	repo, summary := passRepository(t, r, f.main)
	if repo.Commit != local || !strings.Contains(summary, "(checkout's main: fetch refused: the mirror passed 1 MiB, so it was dropped") {
		t.Fatalf("an oversized mirror was read: %s", summary)
	}
	if _, err := os.Stat(identityMirror(r, "url:"+f.origin)); !os.IsNotExist(err) {
		t.Fatalf("the oversized mirror remains: %v", err)
	}
}

// A pass removes the mirrors of repositories no longer in read_dirs, and keeps one whose checkout is still read though
// its origin could not be read this pass.
func TestMirrorsOfRepositoriesNoLongerReadAreRemoved(t *testing.T) {
	r, _ := inputRunner(t)
	a, b := newRepo(t, nil), newRepo(t, nil)
	mirrors := func() int {
		entries, err := os.ReadDir(filepath.Join(r.Dir, "mirrors"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	r.Inputs.ReadDirs = []string{a.main, b.main}
	newPass(t, r)
	if n := mirrors(); n != 2 {
		t.Fatalf("two repositories have %d mirrors", n)
	}
	r.Inputs.ReadDirs = []string{a.main}
	newPass(t, r)
	if _, err := os.Stat(identityMirror(r, "url:"+a.origin)); err != nil || mirrors() != 1 {
		t.Fatalf("the mirror of a repository no longer read remains, or the one still read went: %d %v", mirrors(), err)
	}
	run(t, a.main, "git", "config", "url.https://example.com/.insteadOf", "https://example.org/")
	newPass(t, r)
	if _, err := os.Stat(identityMirror(r, "url:"+a.origin)); err != nil {
		t.Fatalf("the mirror of a checkout still read went with its unreadable origin: %v", err)
	}
}

// When the fetch fails, the pass reads the branch as the mirror last fetched it, not the checkout's own, and says when
// that fetch was.
func TestAFailedFetchReadsTheMirrorsLastFetch(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
		t.Fatalf("the first fetch failed: %s", summary)
	}
	if err := os.Rename(f.origin, f.origin+".away"); err != nil {
		t.Fatal(err)
	}
	repo, summary := passRepository(t, r, f.main)
	if repo.Commit != tip || !strings.Contains(summary, "at "+tip[:7]+" (origin's main as fetched at "+time.Now().UTC().Format("2006-01-02")) ||
		!strings.Contains(summary, ": fetch failed: ") {
		t.Fatalf("the failed fetch did not read the mirror's last fetch: %s", summary)
	}
	if text, err := os.ReadFile(filepath.Join(repo.Path, "later.txt")); err != nil || string(text) != "merged later\n" {
		t.Fatalf("the snapshot is not the mirror's: %v", err)
	}
}
