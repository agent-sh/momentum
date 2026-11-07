package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

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
