package loop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Two openers that both find no mirror make one between them: one makes it and holds its lock, and the other finds that
// lock held and drops its own, never moving away the mirror the first made. A maker that finds a usable mirror another
// made and let go is TestAMakerUsesTheMirrorAnotherMadeAndLetGo's.
func TestTwoOpenersOfAMissingMirrorMakeOne(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	f := newRepo(t, nil)
	mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
	bin := t.TempDir()
	paused, resume := filepath.Join(bin, "paused"), filepath.Join(bin, "resume")
	// each maker's init waits until both have come to it
	script(t, bin, "git", `case " $* " in *" init "*) for a; do last=$a; done
  case "$last" in */.new-*) echo "$last" >> '`+paused+`'; i=0; while [ ! -e '`+resume+`' ] && [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done;; esac;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	type opened struct {
		lock    *os.File
		err     error
		mirrors bool // the lock is the one at the mirror's path
	}
	results := make(chan opened, 2)
	for range 2 {
		go func() {
			lock, err := openMirror(context.Background(), mirror, f.main)
			o := opened{lock: lock, err: err}
			if lock != nil {
				o.mirrors = sameLock(lock, mirror)
			}
			results <- o // the opener keeps what it got until the test ends
		}()
	}
	both := comesTrue(2*time.Minute, func() bool {
		raw, _ := os.ReadFile(paused)
		return strings.Count(string(raw), "\n") == 2
	})
	write(t, resume, "")
	var got []opened
	for range 2 {
		o := <-results
		if o.lock != nil {
			t.Cleanup(func() { o.lock.Close() })
		}
		got = append(got, o)
	}
	if !both {
		t.Fatal("the two openers did not both come to make the mirror")
	}
	made, busy := 0, 0
	for _, o := range got {
		switch {
		case o.err == nil && o.mirrors:
			made++
		case errors.Is(o.err, errFetchRunning):
			busy++
		default:
			t.Errorf("an opener got %v, its lock the mirror's: %v", o.err, o.mirrors)
		}
	}
	if made != 1 || busy != 1 {
		t.Fatalf("%d openers hold the mirror and %d found it held", made, busy)
	}
	if usable, err := usableMirror(context.Background(), mirror); !usable || err != nil {
		t.Fatalf("the mirror made is not usable: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(mirror), ".new-*")); len(left) != 0 {
		t.Fatalf("a maker left its mirror being made: %v", left)
	}
}

// A maker that finds, under the makers' lock, a usable mirror at its name that another maker made and let go uses it:
// it holds that mirror's lock, leaves the mirror where it stands and drops the one it made.
func TestAMakerUsesTheMirrorAnotherMadeAndLetGo(t *testing.T) {
	ctx := context.Background()
	mirrors := filepath.Join(t.TempDir(), "mirrors")
	mirror := filepath.Join(mirrors, "0123456789abcdef.git")
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := makeMirror(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	made, err := os.Lstat(mirror)
	first.Close() // the first maker lets it go
	if err != nil {
		t.Fatal(err)
	}
	// the second found nothing at the mirror's name when it looked first (no old lock), as openMirror does
	second, err := makeMirror(ctx, mirror, nil)
	if err != nil {
		t.Fatalf("the second maker: %v", err)
	}
	defer second.Close()
	if now, err := os.Lstat(mirror); err != nil || !os.SameFile(made, now) {
		t.Fatalf("the second maker replaced the mirror the first made: %v", err)
	}
	if !sameLock(second, mirror) {
		t.Fatal("the second maker's lock is not the mirror's")
	}
	if l, err := lockMirror(mirror); !errors.Is(err, errFetchRunning) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("the mirror's lock was free while the second maker held it: %v", err)
	}
	for _, pattern := range []string{".new-*", mirrorTombstone + "*"} {
		if left, _ := filepath.Glob(filepath.Join(mirrors, pattern)); len(left) != 0 {
			t.Fatalf("the second maker left %v", left)
		}
	}
}

// The owner's preparation holds its mirror's lock while it clones: the mirror is busy to a prune then.
func TestTheOwnersPreparationHoldsItsMirrorAtItsClone(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	ctx := context.Background()
	r, c, _ := buildCase(t)
	g := newRepo(t, nil)
	bin := t.TempDir()
	paused, resume := filepath.Join(bin, "paused"), filepath.Join(bin, "resume")
	script(t, bin, "git", `case " $* " in *" clone "*" --no-local "*) : > '`+paused+`'; i=0; while [ ! -e '`+resume+`' ] && [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	prepared := make(chan error, 1)
	go func() {
		_, err := r.Ship.Prepare(ctx, c.ID, g.main)
		prepared <- err
	}()
	cloning := comesTrue(2*time.Minute, func() bool { _, err := os.Stat(paused); return err == nil })
	release, free := lockForRemoval(identityMirror(r, "url:"+g.origin))
	if free {
		release()
	}
	write(t, resume, "")
	if err := <-prepared; err != nil {
		t.Fatal(err)
	}
	if !cloning {
		t.Fatal("the preparation never cloned")
	}
	if free {
		t.Fatal("the mirror's lock was free while the preparation cloned it")
	}
}

// A mirror being made that has no lock file yet is its maker's, which is about to lock it: a prune leaves it until it
// is older than any maker lasts, and removes it then.
func TestAPruneLeavesAFreshMirrorBeingMade(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	mirrors := filepath.Join(r.Dir, "mirrors")
	fresh, stale := filepath.Join(mirrors, ".new-fresh"), filepath.Join(mirrors, ".new-stale")
	for _, d := range []string{fresh, stale} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	then := time.Now().Add(-orphanAfter - time.Hour)
	if err := os.Chtimes(stale, then, then); err != nil {
		t.Fatal(err)
	}
	passRepository(t, r, f.main)
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a prune removed a mirror its maker had not locked yet: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a prune kept a mirror being made that no maker locked for over %s: %v", orphanAfter, err)
	}
}

// A prune takes an unused mirror's lock from a fetch's process group that has held it longer than any call of a live
// pass lasts, as a fetch does, and removes the mirror.
func TestAPruneTakesAMirrorFromAnOrphanedFetch(t *testing.T) {
	ctx := context.Background()
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	mirrors := filepath.Join(r.Dir, "mirrors")
	mirror := identityMirror(r, "url:"+f.origin)
	if _, why := fetchTip(ctx, f.main, mirror, f.origin, ""); why != "" {
		t.Fatal(why)
	}
	ageMirror(t, mirror, 8*24*time.Hour)
	lock, err := lockMirror(mirror)
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command("sleep", "60")
	holder.ExtraFiles = []*os.File{lock}
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	pid := holder.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	ended := make(chan error, 1)
	go func() { ended <- holder.Wait() }()
	start := recordOrphan(t, lock, pid) // the record's taker, the pass, is gone; its group holds the lock
	lock.Close()
	if !comesTrue(5*time.Second, func() bool { age, err := procAge(start); return err == nil && age > 0 }) {
		t.Fatal("the holder's age cannot be read")
	}
	was := orphanAfter
	t.Cleanup(func() { orphanAfter = was })
	orphanAfter = 0
	pruneMirrors(mirrors, nil)
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("a prune left an unused mirror an orphaned fetch held: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(3 * gitWaitDelay):
		t.Fatal("the orphaned fetch's group still runs")
	}
}

// A mirror whose record says it was used in the future (a clock set back since) is used now to a prune, which writes
// that, so it ages from now and is not kept for good.
func TestAPruneMakesAFutureUseNow(t *testing.T) {
	ctx := context.Background()
	r, _ := inputRunner(t)
	a, h := newRepo(t, nil), newRepo(t, nil)
	mirror := identityMirror(r, "url:"+h.origin)
	if _, why := fetchTip(ctx, h.main, mirror, h.origin, ""); why != "" {
		t.Fatal(why)
	}
	ageMirror(t, mirror, -30*24*time.Hour)
	passRepository(t, r, a.main)
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("a prune removed a mirror whose use it read as now: %v", err)
	}
	if used := usedAt(t, mirror); used.After(time.Now()) || time.Since(used) > time.Minute {
		t.Fatalf("the mirror's use stands at %v, not now", used)
	}
}
