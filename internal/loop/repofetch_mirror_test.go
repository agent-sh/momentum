package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// origin renamed its default branch, and the checkout's origin/HEAD, which nothing updates, still names the old one:
// a build follows the branch origin's HEAD names now, for a package prepared before the rename and for one prepared
// after it.
func TestABuildBaseFollowsARenamedDefaultBranch(t *testing.T) {
	for _, kind := range []string{"prepared before", "prepared after"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			newPass(t, r)
			if kind == "prepared before" {
				if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
					t.Fatal(err)
				}
			}
			run(t, f.dir, "git", "--git-dir", f.origin, "branch", "-m", "main", "trunk")
			tip := pushOther(t, f, "trunk", map[string]string{"later.txt": "merged after the rename\n"})
			if head := strings.TrimSpace(run(t, f.main, "git", "symbolic-ref", "refs/remotes/origin/HEAD")); head != "refs/remotes/origin/main" {
				t.Fatalf("the checkout's origin/HEAD moved: %s", head)
			}
			newPass(t, r)
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
				t.Fatalf("the build of a renamed default branch: %v (held %v)", err, held(err))
			}
			p, _, err := r.Ship.Load(c.ID)
			if err != nil || p.Base != "trunk" || p.BaseCommit != tip {
				t.Fatalf("the package's base is %s at %s, want trunk at %s (%v)", p.Base, p.BaseCommit, tip, err)
			}
			if text, err := os.ReadFile(filepath.Join(p.Work, "later.txt")); err != nil || string(text) != "merged after the rename\n" {
				t.Fatalf("the build tree is not at the renamed branch's tip: %q %v", text, err)
			}
		})
	}
}

// groupGone says whether no process of a group runs: none is left, or only zombies their parent has not reaped yet (a
// container's first process may never reap them), by state Z in /proc.
func groupGone(group int) bool {
	if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		i := strings.LastIndexByte(string(raw), ')')
		if err != nil || i < 0 {
			continue
		}
		fields := strings.Fields(string(raw[i+1:])) // from field 3, the state; field 5 is the group
		if len(fields) > 2 && fields[2] == strconv.Itoa(group) && fields[0] != "Z" {
			return false
		}
	}
	return true
}

// randomText is n random bytes as hex: text no pack compresses or deltifies.
func randomText(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// TestFetchInAnotherProcess is a pass's fetch in a process of its own, which TestAFetchThatOutlivesItsPassKeepsItsFiles
// kills outright.
func TestFetchInAnotherProcess(t *testing.T) {
	mirror := os.Getenv("MOMENTUM_TEST_FETCH_MIRROR")
	if mirror == "" {
		t.Skip("the fetch process of TestAFetchThatOutlivesItsPassKeepsItsFiles")
	}
	_, why := fetchTip(context.Background(), os.Getenv("MOMENTUM_TEST_FETCH_SOURCE"), mirror, os.Getenv("MOMENTUM_TEST_FETCH_URL"), "")
	t.Log(why)
}

// A pass's fetch whose process group outlives the pass (killed outright, its loop lock gone with it) still writes the
// mirror: the next pass neither removes its files nor fetches beside it. Once the group has held the mirror's lock
// longer than any call of a live pass lasts, a pass ends the group, removes what it left and fetches.
func TestAFetchThatOutlivesItsPassKeepsItsFiles(t *testing.T) {
	files := map[string]string{"noise.txt": randomText(t, 4<<20)}
	for i := range 150 { // more objects than a fetch unpacks loose: index-pack writes a temporary pack
		files[fmt.Sprintf("f%03d.txt", i)] = randomText(t, 200)
	}
	f := newRepo(t, files)
	// an ssh that runs the origin's upload-pack here, and stalls the first fetch after 1024 reads of at most 1 KiB: past
	// the advertisement (a few dozen small reads), within the pack of about 2 MiB. dd passes each read on as it gets it,
	// where GNU head holds what it read in its own buffer, and one read a byte is too slow on a busy machine.
	bin := t.TempDir()
	count, pidFile := filepath.Join(bin, "count"), filepath.Join(bin, "pid")
	script(t, bin, "ssh", `for last; do :; done
n=$(cat '`+count+`' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '`+count+`'
echo "$n $PPID $last" >> '`+count+`.log'
case "$last" in git-upload-pack*) ;; *) exit 255;; esac
if [ "$n" = 2 ]; then echo $$ > '`+pidFile+`'; sh -c "$last" | { dd bs=1024 count=1024 2>/dev/null; exec sleep 60; }; else exec sh -c "$last"; fi`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	url := "ssh://git@fixture.invalid" + f.origin
	mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
	pass := exec.Command(os.Args[0], "-test.run=^TestFetchInAnotherProcess$")
	pass.Env = append(os.Environ(), "MOMENTUM_TEST_FETCH_MIRROR="+mirror, "MOMENTUM_TEST_FETCH_SOURCE="+f.main, "MOMENTUM_TEST_FETCH_URL="+url)
	passLog := filepath.Join(bin, "pass.log")
	out, err := os.Create(passLog)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	pass.Stdout, pass.Stderr = out, out
	if err := pass.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pass.Process.Kill(); _ = pass.Wait() })
	tmpPacks := func() []string {
		found, _ := filepath.Glob(filepath.Join(mirror, "objects", "pack", "tmp_*"))
		return found
	}
	if !comesTrue(30*time.Second, func() bool { return len(tmpPacks()) > 0 }) {
		calls, _ := os.ReadFile(count)
		said, _ := os.ReadFile(passLog)
		var files []string
		_ = filepath.WalkDir(mirror, func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				files = append(files, strings.TrimPrefix(p, mirror))
			}
			return nil
		})
		procs, _ := exec.Command("ps", "-ww", "-o", "pid,ppid,pgid,stat,args", "-e").CombinedOutput()
		var mine []string
		for _, l := range strings.Split(string(procs), "\n") {
			if strings.Contains(l, filepath.Base(filepath.Dir(filepath.Dir(mirror)))) || strings.Contains(l, "upload-pack") || strings.Contains(l, " dd ") {
				mine = append(mine, l)
			}
		}
		sshLog, _ := os.ReadFile(count + ".log")
		procs = []byte(strings.Join(mine, "\n") + "\nssh calls:\n" + string(sshLog))
		t.Fatalf("the pass's fetch did not start its transfer: %q ssh calls; the mirror holds %v; the pass runs\n%s\nthe pass said:\n%s",
			strings.TrimSpace(string(calls)), files, procs, said)
	}
	// the pass is killed outright; its fetch's process group goes on
	_ = pass.Process.Kill()
	_ = pass.Wait()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("the fetch did not outlive its pass: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	live := tmpPacks()
	_, why := fetchTip(context.Background(), f.main, mirror, url, "")
	for _, p := range live {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the next pass removed the live fetch's %s: %v", filepath.Base(p), err)
		}
	}
	if !strings.Contains(why, "another fetch, or the owner's preparation, holds the mirror") {
		t.Fatalf("the next pass fetched beside a live fetch: %q", why)
	}
	// nor does a pass that no longer reads the repository remove the mirror under it
	pruneMirrors(filepath.Dir(mirror), nil)
	if _, err := os.Stat(live[0]); err != nil {
		t.Fatalf("a pass pruned the mirror of a live fetch: %v", err)
	}
	// once the group has held the lock longer than a live pass's call lasts, a pass ends it, takes the lock, removes
	// what it left and fetches
	was := orphanAfter
	t.Cleanup(func() { orphanAfter = was })
	orphanAfter = 0
	tip, why := fetchTip(context.Background(), f.main, mirror, url, "")
	if why != "" || tip.commit != originHead(t, f, "main") || len(tmpPacks()) != 0 {
		t.Fatalf("the pass did not take the lock from the orphaned fetch, clear and fetch: %q %+v %v", why, tip, tmpPacks())
	}
	if !comesTrue(10*time.Second, func() bool { return groupGone(group) }) {
		t.Fatal("the orphaned fetch's process group outlived the pass that took its lock")
	}
}

// When the fetch fails, a pass never reads the mirror's last fetch through a link in the path to the mirrors: it reads
// the checkout's own branch.
func TestAFailedFetchNeverReadsAMirrorThroughALink(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
		t.Fatalf("the first fetch failed: %s", summary)
	}
	mirrors, elsewhere := filepath.Join(r.Dir, "mirrors"), filepath.Join(t.TempDir(), "mirrors")
	if err := os.Rename(mirrors, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, mirrors); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.origin, f.origin+".away"); err != nil {
		t.Fatal(err)
	}
	repo, summary := passRepository(t, r, f.main)
	if repo.Commit != local || !strings.Contains(summary, "(checkout's main: fetch failed") {
		t.Fatalf("the failed fetch read a mirror through a link: %s", summary)
	}
}

// mirrorGitIn runs git in a mirror with stdin, for a fixture.
func mirrorGitIn(t *testing.T, mirror, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + mirror}, args...)...)
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %s: %v: %s", args[0], err, out)
	}
	return string(out)
}

// countObjects counts a mirror's packs and loose objects, as a test reads them.
func countObjects(t *testing.T, mirror string) (packs, loose int) {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(mirror, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := filepath.Glob(filepath.Join(mirror, "objects", "[0-9a-f][0-9a-f]", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(found), len(dirs)
}

// A mirror past Git's own repack counts (more than 50 packs, or more than 6700 loose objects) is repacked after its
// fetch: one pack, no loose object, and the pass reads it as before. One at the counts is left as it is.
func TestAMirrorPastItsObjectCountsIsRepacked(t *testing.T) {
	for _, c := range []struct {
		name         string
		packs, loose int
		repacked     bool
	}{{"packs", 51, 0, true}, {"loose objects", 0, 6701, true}, {"at the counts", 50, 0, false}} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			passRepository(t, r, f.main)
			mirror := identityMirror(r, "url:"+f.origin)
			if packs, _ := countObjects(t, mirror); packs != 0 {
				t.Fatalf("the first fetch of a small repository left %d packs", packs)
			}
			// objects of their own, each of the first c.packs packed alone too
			dir := t.TempDir()
			var paths []string
			for i := range max(c.packs, c.loose) {
				p := filepath.Join(dir, strconv.Itoa(i))
				write(t, p, fmt.Sprintf("object %d of the fixture\n", i))
				paths = append(paths, p)
			}
			ids := strings.Fields(mirrorGitIn(t, mirror, strings.Join(paths, "\n")+"\n", "hash-object", "-w", "--stdin-paths"))
			for _, id := range ids[:c.packs] {
				mirrorGitIn(t, mirror, id+"\n", "pack-objects", "-q", filepath.Join(mirror, "objects", "pack", "pack"))
			}
			tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			repo, summary := passRepository(t, r, f.main)
			if repo.Commit != tip || repo.Fallback != "" || repo.History.Commits != 2 {
				t.Fatalf("the pass did not read the mirror: %s", summary)
			}
			packs, loose := countObjects(t, mirror)
			if c.repacked && (packs != 1 || loose != 0) || !c.repacked && packs != c.packs {
				t.Fatalf("a mirror with %d packs and %d loose objects has %d packs and %d loose objects after its fetch", c.packs, len(ids), packs, loose)
			}
		})
	}
}

// A mirror has the files ref format whatever Git's default is (the mirror's lock files and record read refs as files),
// and one with another format is made again.
func TestAMirrorHasTheFilesRefFormat(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	if exec.Command("git", "init", "-q", "--bare", "--ref-format=reftable", filepath.Join(t.TempDir(), "probe.git")).Run() != nil {
		t.Skip("this Git has one ref format (before 2.45)")
	}
	for _, kind := range []string{"a new mirror", "a reftable mirror"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			mirror := identityMirror(r, "url:"+f.origin)
			if kind == "a reftable mirror" {
				if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
					t.Fatal(err)
				}
				inputTestGit(t, filepath.Dir(mirror), nil, "init", "-q", "--bare", "--ref-format=reftable", mirror)
			}
			// a Git whose default ref format is reftable, as Git 3.0's is to be
			bin := t.TempDir()
			script(t, bin, "git", `exec '`+real+`' -c init.defaultRefFormat=reftable "$@"`)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			if repo, summary := passRepository(t, r, f.main); repo.Commit != tip || repo.Fallback != "" {
				t.Fatalf("the pass did not fetch: %s", summary)
			}
			if format := inputTestGit(t, mirror, nil, "rev-parse", "--show-ref-format"); format != "files" {
				t.Fatalf("the mirror's ref format is %s", format)
			}
			if raw, _ := os.ReadFile(filepath.Join(mirror, "config")); strings.Contains(strings.ToLower(string(raw)), "refstorage") {
				t.Fatalf("the mirror's configuration names a ref storage:\n%s", raw)
			}
		})
	}
}

// A shallow mirror is made again: the pass reads the branch's whole history from it.
func TestAShallowMirrorIsMadeAgain(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	mirror := identityMirror(r, "url:"+f.origin)
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, filepath.Dir(mirror), nil, "clone", "-q", "--bare", "--depth", "1", "file://"+f.origin, mirror)
	if _, err := os.Stat(filepath.Join(mirror, "shallow")); err != nil {
		t.Fatalf("the fixture mirror is not shallow: %v", err)
	}
	repo, summary := passRepository(t, r, f.main)
	if repo.Commit != tip || repo.Fallback != "" || repo.History.Commits != 2 {
		t.Fatalf("the pass did not read the whole history: %d commits: %s", repo.History.Commits, summary)
	}
	if _, err := os.Stat(filepath.Join(mirror, "shallow")); !os.IsNotExist(err) {
		t.Fatalf("the mirror is still shallow: %v", err)
	}
}

// A Git call its deadline ends gets SIGTERM, on which git removes its own lock files, and SIGKILL only gitWaitDelay
// later, for what ignores SIGTERM. A commit holds the index's lock while its editor runs: the call ends without leaving
// the lock, in a repository no cleanup of the host's ever reads (clearStale runs only on a mirror, before its fetch).
func TestADeadlineSendsSIGTERMBeforeSIGKILL(t *testing.T) {
	for _, kind := range []string{"an editor that ends on SIGTERM", "an editor that ignores it"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			inputTestGit(t, repo, nil, "init", "-q")
			inputTestGit(t, repo, nil, "commit", "-q", "--allow-empty", "-m", "first")
			marks := t.TempDir()
			pidFile, termFile := filepath.Join(marks, "pid"), filepath.Join(marks, "term")
			ends := "exit 1"
			if kind == "an editor that ignores it" {
				ends = ":"
			}
			editor := script(t, marks, "editor", `echo $$ > '`+pidFile+`'
trap 'touch '"'`+termFile+`'"'; `+ends+`' TERM
while :; do sleep 0.05; done`)
			var mu sync.Mutex
			var died time.Time
			go func() {
				var pid int
				if !comesTrue(10*time.Second, func() bool {
					raw, err := os.ReadFile(pidFile)
					pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
					return err == nil
				}) {
					return
				}
				comesTrue(30*time.Second, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
				mu.Lock()
				died = time.Now()
				mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := hostInputGit(ctx, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "core.editor="+editor,
				"commit", "-q", "-a", "--allow-empty"); err == nil {
				t.Fatal("the commit outlived its deadline")
			}
			returned := time.Now()
			info, err := os.Stat(termFile)
			if err != nil {
				t.Fatalf("the editor got no SIGTERM: %v", err)
			}
			if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); !os.IsNotExist(err) {
				t.Fatalf("git left its lock: %v", err)
			}
			if !comesTrue(5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return !died.IsZero() }) {
				t.Fatal("the editor outlived the call")
			}
			mu.Lock()
			lived := died.Sub(info.ModTime())
			mu.Unlock()
			if kind == "an editor that ignores it" && lived < gitWaitDelay-time.Second {
				t.Fatalf("SIGKILL came %s after SIGTERM, before the %s the group has to end", lived.Round(time.Millisecond), gitWaitDelay)
			}
			if kind == "an editor that ends on SIGTERM" && returned.Sub(info.ModTime()) > gitWaitDelay-time.Second {
				t.Fatalf("a group that ended on SIGTERM was waited on for %s", returned.Sub(info.ModTime()).Round(time.Millisecond))
			}
		})
	}
}
