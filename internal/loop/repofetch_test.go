package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// stalledListener accepts connections and never answers on them, as a server that hangs does; it lets each go after
// hold, so a call that never ends by itself still ends. count says how many it accepted and how many of them their
// client still holds open: a process that holds one is still running.
func stalledListener(t *testing.T, hold time.Duration) (port int, count func() (accepted, held int)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	accepted, held := 0, 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted, held = accepted+1, held+1
			mu.Unlock()
			release := time.AfterFunc(hold, func() { conn.Close() })
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						break
					}
				}
				if release.Stop() { // the client closed it before hold let it go
					mu.Lock()
					held--
					mu.Unlock()
					conn.Close()
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() (int, int) { mu.Lock(); defer mu.Unlock(); return accepted, held }
}

// comesTrue says whether ok comes true within d.
func comesTrue(d time.Duration, ok func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return true
		}
	}
	return ok()
}

// A remote call ends at its deadline, though its ssh or https helper waits on a server that never answers, or ignores
// SIGTERM: the whole process group goes, the helper with it, and the error says which deadline ended the call.
func TestARemoteCallEndsAtItsDeadline(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh here")
	}
	const deadline = 3 * time.Second
	bound := deadline + 2*gitWaitDelay + 2*time.Second
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	inputTestGit(t, filepath.Dir(mirror), nil, "init", "-q", "--bare", mirror)
	f := newRepo(t, nil)
	ship := shipWithSandbox(testStore(t), f, Sandbox{})
	for _, name := range []string{"ssh fetch", "https fetch", "ship", "an ssh that ignores SIGTERM"} {
		t.Run(name, func(t *testing.T) {
			port, count := stalledListener(t, 20*time.Second)
			url := fmt.Sprintf("ssh://git@127.0.0.1:%d/x.git", port)
			pidFile := filepath.Join(t.TempDir(), "pid")
			switch name {
			case "https fetch":
				url = fmt.Sprintf("https://127.0.0.1:%d/x.git", port)
			case "an ssh that ignores SIGTERM":
				bin := t.TempDir()
				script(t, bin, "ssh", "trap '' TERM\necho $$ > '"+pidFile+"'\nexec sleep 60")
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			start := time.Now()
			var err error
			if name == "ship" {
				_, err = ship.git(ctx, f.main, "ls-remote", url, "HEAD")
			} else {
				_, err = fetchGit(ctx, mirrorCall{mirror: mirror}, "ls-remote", url, "HEAD")
			}
			if took := time.Since(start); took > bound {
				t.Fatalf("the call returned %s after its %s deadline", took.Round(time.Second), deadline)
			}
			if err == nil || !strings.Contains(err.Error(), "the caller's deadline passed") {
				t.Fatalf("the error does not say which deadline ended it: %v", err)
			}
			if name == "an ssh that ignores SIGTERM" {
				raw, err := os.ReadFile(pidFile)
				pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil || perr != nil {
					t.Fatalf("the helper did not start: %v %v", err, perr)
				}
				if !comesTrue(2*time.Second, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }) {
					t.Fatal("the helper that ignores SIGTERM outlived the call")
				}
				return
			}
			if !comesTrue(2*time.Second, func() bool { accepted, held := count(); return accepted > 0 && held == 0 }) {
				accepted, held := count()
				t.Fatalf("the helper outlived the call: it holds %d of the %d connections it made", held, accepted)
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
		{"https credentials", "fetch refused: origin's URL holds credentials"},
		{"ssh password", "fetch refused: origin's URL holds credentials"},
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
			case "https credentials":
				run(t, f.main, "git", "remote", "set-url", "origin", "https://tok3n-7f3a@127.0.0.1:1/fake.git")
			case "ssh password":
				run(t, f.main, "git", "remote", "set-url", "origin", "ssh://fixture:tok3n-7f3a@127.0.0.1:1/fake.git")
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

// snapshotPass is SnapshotRepos over one checkout as a pass runs it, into inputs of its own under parent.
func snapshotPass(ctx context.Context, parent, name, source string) ([]RepoInput, error) {
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return SnapshotRepos(ctx, SnapshotOptions{Sources: []string{source}, Destination: filepath.Join(dir, "repos"),
		LogDir: filepath.Join(dir, "logs"), Mirrors: filepath.Join(parent, "mirrors"), Since: time.Now().Add(-HistoryWindow)})
}

// A fetch its deadline ends in the middle of the transfer leaves a mirror the next pass fetches into: the group gets
// SIGTERM, on which git removes its locks, and the next pass removes what a fetch killed outright left.
func TestAFetchEndedMidTransferLeavesAMirrorTheNextPassFetches(t *testing.T) {
	goodInputScrubber(t)
	noise := make([]byte, 4<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	f := newRepo(t, map[string]string{"noise.bin": string(noise)})
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	// an ssh that runs the origin's upload-pack here, and stalls the first pass's fetch after 64 KiB of its pack
	bin := t.TempDir()
	count := filepath.Join(bin, "count")
	script(t, bin, "ssh", `for last; do :; done
n=$(cat '`+count+`' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '`+count+`'
case "$last" in git-upload-pack*) ;; *) exit 255;; esac
if [ "$n" = 2 ]; then sh -c "$last" | { head -c 65536; exec sleep 60; }; else exec sh -c "$last"; fi`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	run(t, f.main, "git", "remote", "set-url", "origin", "ssh://git@fixture.invalid"+f.origin)
	parent := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := snapshotPass(ctx, parent, "first", f.main); !errors.Is(err, context.DeadlineExceeded) && err != nil {
		t.Fatal(err)
	}
	if n, _ := os.ReadFile(count); strings.TrimSpace(string(n)) != "2" {
		t.Fatalf("the first pass's fetch did not start its transfer: %q", n)
	}
	got, err := snapshotPass(context.Background(), parent, "second", f.main)
	if err != nil || len(got) != 1 || got[0].Commit != tip || got[0].Fallback != "" {
		t.Fatalf("the next pass did not fetch into the mirror: %+v %v", got, err)
	}
}

// A mirror holding the locks, temporary files and keep file of a fetch or a repack killed outright is fetched into all
// the same, and loses them; a keep file of anyone else's stays.
func TestStaleLocksOfAKilledFetchAreRemoved(t *testing.T) {
	goodInputScrubber(t)
	f := newRepo(t, nil)
	parent := t.TempDir()
	if got, err := snapshotPass(context.Background(), parent, "first", f.main); err != nil || got[0].Fallback != "" {
		t.Fatalf("the first fetch failed: %+v %v", got, err)
	}
	mirror := filepath.Join(parent, "mirrors", filepath.Base(mirrorPath("", "url:"+f.origin)))
	stale := []string{"refs/heads/main.lock", "HEAD.lock", "config.lock", "packed-refs.lock", "shallow.lock", "objects/pack/tmp_pack_x1",
		"objects/pack/.tmp-123-pack-x1.pack", "objects/ab/tmp_obj_x1"}
	for _, p := range stale {
		write(t, filepath.Join(mirror, p), "")
	}
	// index-pack's keep file for a fetch that never named its pack, and a keep file a person wrote
	stale = append(stale, "objects/pack/pack-1234.keep")
	write(t, filepath.Join(mirror, "objects/pack/pack-1234.keep"), "fetch-pack 4242 on fixture\n")
	write(t, filepath.Join(mirror, "objects/pack/pack-5678.keep"), "kept by hand\n")
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	got, err := snapshotPass(context.Background(), parent, "second", f.main)
	if err != nil || got[0].Commit != tip || got[0].Fallback != "" {
		t.Fatalf("a killed fetch's leftovers stopped the next fetch: %+v %v", got, err)
	}
	for _, p := range stale {
		if _, err := os.Lstat(filepath.Join(mirror, p)); !os.IsNotExist(err) {
			t.Errorf("%s remains", p)
		}
	}
	if _, err := os.Stat(filepath.Join(mirror, "objects/pack/pack-5678.keep")); err != nil {
		t.Errorf("a keep file a person wrote was removed: %v", err)
	}
}

// Every run is told which of its repositories are not origin's default branch as its pass fetched it, and what they
// are instead: an investigation and a review alike.
func TestARunIsToldWhichRepositoriesAreNotFetched(t *testing.T) {
	const note = "/ro/repos/main, its log and its diffs are not origin's default branch as this pass fetched it (checkout's main: "
	t.Run("investigation", func(t *testing.T) {
		r, c := inputRunner(t)
		f, fetched := newRepo(t, nil), newRepo(t, nil)
		run(t, f.main, "git", "remote", "remove", "origin")
		r.Inputs.ReadDirs = []string{f.main, fetched.main}
		newPass(t, r)
		prepared, err := r.prepareRun(context.Background(), c, c.Family, StepInvestigate)
		if err != nil {
			t.Fatal(err)
		}
		prompt, err := readHostInput(HostDir(prepared.Dir), "prompt.json")
		if err != nil {
			t.Fatal(err)
		}
		stale := strings.Replace(note, "/ro/repos/main,", "/ro/repos/"+r.pass.manifest.Repositories[0].Name+",", 1)
		if !strings.Contains(string(prompt), stale+"no origin).") || strings.Count(string(prompt), "are not origin's default branch") != 1 {
			t.Fatalf("the investigation is not told which repository is the checkout's own:\n%s", prompt)
		}
	})
	t.Run("review", func(t *testing.T) {
		ctx := context.Background()
		r, c := inputRunner(t)
		mechanism, next := "one cause", "one test"
		c, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &next}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		f := newRepo(t, nil)
		r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
		r.Ship.Dir = r.Dir
		p, err := r.Ship.Prepare(ctx, c.ID, f.main)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(p.Work, "notes.txt"), "One change\n")
		p = fixtureCandidate(t, r.Ship, p)
		p.Title, p.Body, p.Rollback = "One change", "One cause", "Revert"
		p.Check = &Check{Tree: strings.Repeat("a", 40), CommandInput: "host-only", Before: Outcome{Exit: 1}}
		if err := r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		run(t, f.main, "git", "config", "url.https://example.com/.insteadOf", "https://example.org/")
		r.Inputs.ReadDirs = []string{f.main}
		newPass(t, r)
		prepared, err := r.prepareRun(ctx, c, c.Family, StepReview)
		if err != nil {
			t.Fatal(err)
		}
		prompt, err := readHostInput(HostDir(prepared.Dir), "prompt.json")
		if err != nil || !strings.Contains(string(prompt), note+"fetch refused: the checkout's Git configuration rewrites remote URLs") {
			t.Fatalf("the review is not told its repository is the checkout's own: %v\n%s", err, prompt)
		}
	})
}

// A pass removes only mirrors: not what else lies in its mirrors directory, and nothing at all when that directory is
// a link to somewhere else.
func TestMirrorPruningRemovesOnlyMirrors(t *testing.T) {
	for _, kind := range []string{"a linked directory", "other names"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			mirrors := filepath.Join(r.Dir, "mirrors")
			kept := mirrors
			if kind == "a linked directory" {
				kept = t.TempDir()
				if err := os.Symlink(kept, mirrors); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(kept, "notes.txt"), "keep\n")
			write(t, filepath.Join(kept, "other", "file"), "keep\n")
			if kind == "a linked directory" {
				write(t, filepath.Join(kept, "0123456789abcdef.git", "HEAD"), "keep\n")
			}
			passRepository(t, r, f.main)
			for _, p := range []string{"notes.txt", "other/file", "0123456789abcdef.git/HEAD"} {
				if _, err := os.Stat(filepath.Join(kept, p)); err != nil && (kind == "a linked directory" || p != "0123456789abcdef.git/HEAD") {
					t.Errorf("the pass removed %s: %v", p, err)
				}
			}
		})
	}
}

// A build whose host clone the host refuses fails as the build's own, and is not held as an outage.
func TestABuildBaseRefusalIsNotAHold(t *testing.T) {
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
	run(t, p.Host, "git", "config", "core.sshCommand", "false")
	_, err = r.prepareRun(ctx, c, c.Family, StepBuild)
	if !errors.Is(err, ErrRefused) || held(err) || !strings.Contains(err.Error(), "names a program") {
		t.Fatalf("a refused host clone: %v (held %v)", err, held(err))
	}
}

// A mirror's fetch has 30 minutes while the mirror holds no complete fetch (none yet, or a first one that did not
// finish, whose record names the checkout and no commit), and 5 once it does.
func TestAMirrorsFirstFetchHas30MinutesAndALaterOne5(t *testing.T) {
	ctx := context.Background()
	f := newRepo(t, nil)
	mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
	budget := func(want string, limit time.Duration) {
		t.Helper()
		if what, got := fetchBudget(mirror); what != want || got != limit {
			t.Fatalf("the fetch is a %s with %s, want a %s with %s", what, got, want, limit)
		}
	}
	budget("first fetch", 30*time.Minute)
	lock, err := openMirror(ctx, mirror, f.main)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	budget("first fetch", 30*time.Minute)
	if _, why := fetchTip(ctx, f.main, mirror, f.origin, ""); why != "" {
		t.Fatal(why)
	}
	budget("fetch", 5*time.Minute)
}
