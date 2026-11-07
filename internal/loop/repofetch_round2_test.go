package loop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A connection the network closes is an outage, though no transport text the host knows names it: the build is held
// for the pass, not failed as its own.
func TestABuildBaseHoldsOnAConnectionTheNetworkClosed(t *testing.T) {
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
	// an ssh whose connection the network closes, as OpenSSH says it
	bin := t.TempDir()
	script(t, bin, "ssh", `printf 'Connection closed by 192.0.2.1 port 22\r\n' >&2; exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	run(t, p.Host, "git", "remote", "set-url", "origin", "ssh://git@fixture.invalid/fixture/fake.git")
	_, err = r.prepareRun(ctx, c, c.Family, StepBuild)
	if err == nil || !held(err) || !strings.Contains(err.Error(), "cannot be fetched now") {
		t.Fatalf("a closed connection did not hold the build: %v (held %v)", err, held(err))
	}
}

// A build reads a GitHub repository's base as the mirror reads it, over https by its name with the GitHub CLI's
// credentials, never over ssh, so no ssh agent is asked for a key: a private repository's base and a public one's,
// whose fetch is read again before the mount.
func TestABuildReadsAGitHubBaseOverHTTPS(t *testing.T) {
	for _, kind := range []string{"private", "public"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			newPass(t, r)
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
				t.Fatal(err)
			}
			p, _, err := r.Ship.Load(c.ID)
			if err != nil {
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
			for _, dir := range []string{f.main, p.Host} {
				run(t, dir, "git", "remote", "set-url", "origin", "git@github.com:agent-sh/fake.git")
			}
			tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			newPass(t, r)
			mu.Lock()
			asked = nil
			mu.Unlock()
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
				t.Fatalf("the build of a GitHub base: %v (held %v)", err, held(err))
			}
			if p, _, err = r.Ship.Load(c.ID); err != nil || p.BaseCommit != tip {
				t.Fatalf("the base is %s, want %s (%v)", p.BaseCommit, tip, err)
			}
			if raw, err := os.ReadFile(calls); err == nil {
				t.Fatalf("the build ran ssh:\n%s", raw)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) == 0 || asked[0] != "agent-sh/fake" {
				t.Fatalf("the build asked for %v", asked)
			}
		})
	}
}

// A ship Git call never prompts: the ssh it runs has no askpass program in its environment, whatever the session that
// runs the loop sets, as the mirror's fetch has none.
func TestAShipCallNeverPrompts(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script(t, bin, "ssh", `env | grep -E '^(SSH_ASKPASS|SSH_ASKPASS_REQUIRE)=' >> '`+calls+`'; exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SSH_ASKPASS", filepath.Join(bin, "askpass"))
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	f := newRepo(t, nil)
	ship := shipWithSandbox(testStore(t), f, Sandbox{})
	if _, err := ship.git(context.Background(), f.main, "ls-remote", "ssh://git@fixture.invalid/fixture/fake.git", "HEAD"); err == nil {
		t.Fatal("the fixture's ssh answered")
	}
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the call did not run ssh: %v", err)
	}
	if strings.Contains(string(got), "SSH_ASKPASS=") || !strings.Contains(string(got), "SSH_ASKPASS_REQUIRE=never") {
		t.Fatalf("ssh ran with an askpass program, or one it may force:\n%s", got)
	}
}

// A mirror's first fetch has no https low-speed limit, since its 30 minutes bound it; a later fetch, and the ls-remote
// of either, end under 1 byte a second for 60 seconds.
func TestAFirstFetchHasNoLowSpeedLimit(t *testing.T) {
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
	limits := func() map[string]string {
		raw, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(calls); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			verb := "fetch"
			if strings.Contains(line, " ls-remote ") {
				verb = "ls-remote"
			}
			_, limit, _ := strings.Cut(line, "http.lowSpeedLimit=")
			limit, _, _ = strings.Cut(limit, " ")
			got[verb] = limit
		}
		return got
	}
	if repo, summary := passRepository(t, r, f.main); repo.Fallback != "" {
		t.Fatalf("the first fetch failed: %s", summary)
	}
	if got := limits(); got["fetch"] != "0" || got["ls-remote"] != "1" {
		t.Fatalf("the first fetch's low-speed limits: %v", got)
	}
	pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	if repo, summary := passRepository(t, r, f.main); repo.Fallback != "" {
		t.Fatalf("the later fetch failed: %s", summary)
	}
	if got := limits(); got["fetch"] != "1" || got["ls-remote"] != "1" {
		t.Fatalf("a later fetch's low-speed limits: %v", got)
	}
}

// A mirror is made again only when it is definitely not one the host made: a check that cannot come out (a pass told
// to stop, a lock file that cannot be opened) leaves it, and its last fetch, as they were.
func TestAMirrorIsNotMadeAgainOnACheckThatCannotComeOut(t *testing.T) {
	for _, kind := range []string{"a pass told to stop", "a lock that cannot be opened"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "a lock that cannot be opened" && os.Getuid() == 0 {
				t.Skip("root opens any file")
			}
			f := newRepo(t, nil)
			mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
			if _, why := fetchTip(context.Background(), f.main, mirror, f.origin, ""); why != "" {
				t.Fatal(why)
			}
			ctx := context.Background()
			if kind == "a pass told to stop" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				lockFile := filepath.Join(mirror, "momentum-mirror.lock")
				if err := os.Chmod(lockFile, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(lockFile, 0o600) })
			}
			if lock, err := openMirror(ctx, mirror, f.main); err == nil {
				lock.Close()
				t.Fatal("the mirror opened")
			}
			if _, _, ok := lastFetch(context.Background(), mirror); !ok {
				t.Fatal("the mirror was made again: its last fetch is gone")
			}
		})
	}
}

// A repack that fails is logged with the pass, which reads the mirror all the same.
func TestAFailedRepackIsLogged(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes any directory")
	}
	r, _ := inputRunner(t)
	var mu sync.Mutex
	var logged []string
	r.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	f := newRepo(t, nil)
	passRepository(t, r, f.main)
	mirror := identityMirror(r, "url:"+f.origin)
	dir := t.TempDir()
	var paths []string
	for i := range 51 {
		p := filepath.Join(dir, strconv.Itoa(i))
		write(t, p, fmt.Sprintf("object %d of the fixture\n", i))
		paths = append(paths, p)
	}
	for _, id := range strings.Fields(mirrorGitIn(t, mirror, strings.Join(paths, "\n")+"\n", "hash-object", "-w", "--stdin-paths")) {
		mirrorGitIn(t, mirror, id+"\n", "pack-objects", "-q", filepath.Join(mirror, "objects", "pack", "pack"))
	}
	// the repack cannot write its pack; the fetch, of a few loose objects, still can
	packs := filepath.Join(mirror, "objects", "pack")
	if err := os.Chmod(packs, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(packs, 0o700) })
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
		t.Fatalf("a failed repack stopped the pass's read: %s", summary)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "repack failed") {
			return
		}
	}
	t.Fatalf("the failed repack is not in the log:\n%s", strings.Join(logged, "\n"))
}
