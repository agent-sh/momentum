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
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// oldGit puts on PATH a Git that answers as one before 2.45 does, which has a single ref format: init refuses
// --ref-format, and rev-parse prints --show-ref-format back as it would an argument it does not know.
func oldGit(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin := t.TempDir()
	script(t, bin, "git", `show=0
for a; do
  case "$a" in
    --ref-format=*) echo "error: unknown option" >&2; exit 129;;
    --show-ref-format) show=1;;
  esac
done
if [ $show = 0 ]; then exec '`+real+`' "$@"; fi
n=$#
for a; do [ "$a" = --show-ref-format ] || set -- "$@" "$a"; done
shift $n
'`+real+`' "$@" || exit $?
echo --show-ref-format`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// With a Git before 2.45 a mirror is made, read and fetched into again: its ref format is read from its own
// configuration, not from a rev-parse option that Git does not have.
func TestAGitBeforeRefFormatsKeepsItsMirror(t *testing.T) {
	oldGit(t)
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	if repo, summary := passRepository(t, r, f.main); repo.Fallback != "" {
		t.Fatalf("the first fetch failed: %s", summary)
	}
	mirror := identityMirror(r, "url:"+f.origin)
	marker := filepath.Join(mirror, "objects", "info", "fixture-marker")
	write(t, marker, "kept\n")
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
		t.Fatalf("the later fetch failed: %s", summary)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the mirror was made again: %v", err)
	}
}

// failingInit puts on PATH a Git whose init fails, as one does that refuses an option.
func failingInit(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin := t.TempDir()
	script(t, bin, "git", `for a; do [ "$a" = init ] && { echo "error: unknown option" >&2; exit 129; }; done
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// A mirror to make again is replaced only once its replacement exists: an init that fails leaves it as it was.
func TestAFailedInitLeavesTheMirror(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	mirror := identityMirror(r, "url:"+f.origin)
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, filepath.Dir(mirror), nil, "clone", "-q", "--bare", "--depth", "1", "file://"+f.origin, mirror)
	failingInit(t)
	if repo, summary := passRepository(t, r, f.main); !strings.Contains(summary, "(checkout's main: fetch failed") {
		t.Fatalf("a failed init did not fail the fetch: %+v %s", repo, summary)
	}
	if _, err := os.Stat(filepath.Join(mirror, "shallow")); err != nil {
		t.Fatalf("a failed init removed the mirror it was to replace: %v", err)
	}
}

// A Git that fails on its own (out of memory, out of descriptors) gives no answer about the mirror, which is not made
// again.
func TestAMirrorIsNotMadeAgainWhenGitFails(t *testing.T) {
	f := newRepo(t, nil)
	mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
	if _, why := fetchTip(context.Background(), f.main, mirror, f.origin, ""); why != "" {
		t.Fatal(why)
	}
	marker := filepath.Join(mirror, "objects", "info", "fixture-marker")
	write(t, marker, "kept\n")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin := t.TempDir()
	script(t, bin, "git", `for a; do [ "$a" = --is-bare-repository ] && { echo "fatal: Out of memory, malloc failed (tried to allocate 1048576 bytes)" >&2; exit 128; }; done
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if lock, err := openMirror(context.Background(), mirror, f.main); err == nil {
		lock.Close()
		t.Fatal("the mirror opened")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the mirror was made again: %v", err)
	}
}

// A repack rolls up the small packs and leaves the mirror's largest pack as it is: it rewrites what came since, not
// the whole history.
func TestARepackLeavesTheLargestPackAsItIs(t *testing.T) {
	files := map[string]string{}
	for i := range 150 { // more objects than a fetch unpacks loose: the first fetch keeps its pack
		files[fmt.Sprintf("f%03d.txt", i)] = randomText(t, 200)
	}
	r, _ := inputRunner(t)
	f := newRepo(t, files)
	passRepository(t, r, f.main)
	mirror := identityMirror(r, "url:"+f.origin)
	history, err := filepath.Glob(filepath.Join(mirror, "objects", "pack", "pack-*.pack"))
	if err != nil || len(history) != 1 {
		t.Fatalf("the first fetch left %d packs (%v)", len(history), err)
	}
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
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
		t.Fatalf("the pass did not read the mirror: %s", summary)
	}
	if _, err := os.Stat(history[0]); err != nil {
		t.Fatalf("the repack rewrote the mirror's largest pack: %v", err)
	}
	if packs, loose := countObjects(t, mirror); packs > 3 || loose != 0 {
		t.Fatalf("after the repack the mirror has %d packs and %d loose objects", packs, loose)
	}
}

// A build held for its base gets one note per class of cause, which names the class, not Git's text, and the pass's
// log gets the text's first line. After a day of waiting the note is raised to normal importance; a build that fetches
// its base resolves it.
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
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// a transport whose credentials are gone, as git says over https when gh is logged out
	bin := t.TempDir()
	// after ssh's warning about its key exchange, which comes first
	script(t, bin, "ssh", `echo "** WARNING: connection is not using a post-quantum key exchange algorithm." >&2
echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2; exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	run(t, p.Host, "git", "remote", "set-url", "origin", "ssh://git@fixture.invalid/fixture/fake.git")
	baseNotes := func() []store.Note {
		notes, err := r.Store.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var open []store.Note
		for _, n := range notes {
			if n.Done.IsZero() && strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(c.ID)+":base:") {
				open = append(open, n)
			}
		}
		return open
	}
	hold := func() {
		t.Helper()
		_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
		if !held(err) {
			t.Fatalf("lost credentials did not hold the build: %v", err)
		}
		r.buildSkipped(ctx, c, "prepare", err)
	}
	hold()
	notes := baseNotes()
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
		t.Fatalf("the pass's log lacks git's text:\n%s", strings.Join(logged, "\n"))
	}
	clock = clock.Add(baseHoldRaise / 2)
	hold()
	if notes := baseNotes(); len(notes) != 1 || notes[0].Importance != store.ImportanceLow {
		t.Fatalf("half a day's hold: %+v", notes)
	}
	clock = clock.Add(baseHoldRaise/2 + time.Minute)
	hold()
	if notes := baseNotes(); len(notes) != 1 || notes[0].Importance != store.ImportanceNormal {
		t.Fatalf("a day's hold was not raised: %+v", notes)
	}
	run(t, p.Host, "git", "remote", "set-url", "origin", f.origin)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	if notes := baseNotes(); len(notes) != 0 {
		t.Fatalf("a build that fetched its base left its hold's notes open: %+v", notes)
	}
}

// A hold's class is one of a fixed few, whatever Git printed, and its log line is the one that shows it, else Git's
// last fatal: or error: line: warnings come first.
func TestABaseHoldNamesItsClass(t *testing.T) {
	const pq = "** WARNING: connection is not using a post-quantum key exchange algorithm.\r\n** This session may be vulnerable to \"store now, decrypt later\" attacks.\r\n** The server may need to be upgraded. See https://openssh.com/pq.html\r\n"
	for _, c := range []struct{ stderr, ended, class, line string }{
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled\n", "", "no credentials", "fatal: could not read Username for 'https://github.com': terminal prompts disabled"},
		{"warning: redirecting to https://github.com/o/r.git/\nfatal: could not read Password for 'https://x@github.com': terminal prompts disabled\n", "", "no credentials", "fatal: could not read Password for 'https://x@github.com': terminal prompts disabled"},
		{"remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/o/r.git/'\n", "", "no credentials", "remote: Invalid username or token. Password authentication is not supported for Git operations."},
		{"fatal: unable to access 'https://github.com/o/r.git/': server certificate verification failed. CAfile: none CRLfile: none\n", "", "a certificate or host key not trusted", "fatal: unable to access 'https://github.com/o/r.git/': server certificate verification failed. CAfile: none CRLfile: none"},
		{"fatal: unable to access 'https://github.com/o/r.git/': SSL certificate problem: unable to get local issuer certificate\n", "", "a certificate or host key not trusted", "fatal: unable to access 'https://github.com/o/r.git/': SSL certificate problem: unable to get local issuer certificate"},
		{"Host key verification failed.\r\nfatal: Could not read from remote repository.\n", "", "a certificate or host key not trusted", "Host key verification failed."},
		{pq + "Connection closed by 192.0.2.1 port 22\r\nfatal: Could not read from remote repository.\n", "", "the connection broke off", "Connection closed by 192.0.2.1 port 22"},
		{"fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com\n", "", "the server's name did not resolve", "fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com"},
		{"fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 503\n", "", "the server failed (an HTTP 5xx answer)", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 503"},
		{"", "the git call's 2m0s limit passed", "the git call's 2m0s limit passed", ""},
		{pq + "error: something git has never said\nfatal: and then this\n", "", "another failure", "fatal: and then this"},
	} {
		if class, line := holdClass([]byte(c.stderr), c.ended); class != c.class || line != c.line {
			t.Errorf("%q: class %q, line %q; want %q, %q", c.stderr, class, line, c.class, c.line)
		}
	}
}
