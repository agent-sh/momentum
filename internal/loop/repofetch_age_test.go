package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// ageMirror makes the mirror's record say it was last used ago before now.
func ageMirror(t *testing.T, mirror string, ago time.Duration) {
	t.Helper()
	path := filepath.Join(mirror, mirrorMetaFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record["used"] = time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)
	if raw, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// usedAt is when the mirror's record says it was last used, zero when it says nothing.
func usedAt(t *testing.T, mirror string) time.Time {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mirror, mirrorMetaFile))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Used time.Time `json:"used"`
	}
	_ = json.Unmarshal(raw, &record)
	return record.Used
}

// A pass removes a mirror nothing has used for a week, the loop's shortest measurement window, and keeps one used
// within it; a mirror the pass reads is used now, whenever it was before.
func TestAMirrorUnusedForAWeekIsPruned(t *testing.T) {
	ctx := context.Background()
	r, _ := inputRunner(t)
	a, old, recent := newRepo(t, nil), newRepo(t, nil), newRepo(t, nil)
	mirrorOf := func(f repoFixture) string { return identityMirror(r, "url:"+f.origin) }
	for _, f := range []repoFixture{a, old, recent} {
		if _, why := fetchTip(ctx, f.main, mirrorOf(f), f.origin, ""); why != "" {
			t.Fatal(why)
		}
	}
	ageMirror(t, mirrorOf(a), 8*24*time.Hour)
	ageMirror(t, mirrorOf(old), 8*24*time.Hour)
	ageMirror(t, mirrorOf(recent), 6*24*time.Hour)
	r.Inputs.ReadDirs = []string{a.main}
	newPass(t, r)
	if _, err := os.Stat(mirrorOf(old)); !os.IsNotExist(err) {
		t.Fatalf("a pass kept a mirror unused for 8 days: %v", err)
	}
	if _, err := os.Stat(mirrorOf(recent)); err != nil {
		t.Fatalf("a pass removed a mirror used 6 days ago: %v", err)
	}
	if _, err := os.Stat(mirrorOf(a)); err != nil {
		t.Fatalf("a pass removed the mirror it read: %v", err)
	}
	if used := usedAt(t, mirrorOf(a)); time.Since(used) > time.Minute {
		t.Fatalf("the mirror the pass read was last used at %v", used)
	}
}

// The owner's preparation of a repository outside loop.read_dirs, while a pass fetches, leaves its mirror: the pass's
// prune reads when the mirror was last used once it holds the mirror's lock, after the preparation used it.
func TestAPreparationDuringAPassKeepsItsMirror(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	ctx := context.Background()
	r, c, f := buildCase(t)
	g := newRepo(t, nil) // a repository outside loop.read_dirs
	bin := t.TempDir()
	paused, resume := filepath.Join(bin, "paused"), filepath.Join(bin, "resume")
	// the pass's fetch of its own repository waits until the preparation is done
	script(t, bin, "git", `case " $* " in *" fetch "*'`+f.origin+`'*) if [ ! -e '`+resume+`' ]; then : > '`+paused+`'; i=0; while [ ! -e '`+resume+`' ] && [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done; fi;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	passed := make(chan error, 1)
	go func() { passed <- r.prepareInputs(ctx) }()
	if !comesTrue(2*time.Minute, func() bool { _, err := os.Stat(paused); return err == nil }) {
		t.Fatal("the pass never fetched")
	}
	_, prepared := r.Ship.Prepare(ctx, c.ID, g.main)
	write(t, resume, "")
	if err := <-passed; err != nil {
		t.Fatal(err)
	}
	if prepared != nil {
		t.Fatalf("the preparation during the pass: %v", prepared)
	}
	if _, err := os.Stat(identityMirror(r, "url:"+g.origin)); err != nil {
		t.Fatalf("the pass pruned the mirror a preparation used during it: %v", err)
	}
}

// A file or a link at a mirror's name is no mirror: a prune unlinks it, never what a link names.
func TestAPruneUnlinksAFileOrALinkAtAMirrorsName(t *testing.T) {
	for _, kind := range []string{"a file", "a link to a file"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			at := filepath.Join(r.Dir, "mirrors", "0123456789abcdef.git")
			target := filepath.Join(t.TempDir(), "target")
			write(t, target, "keep\n")
			if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "a file" {
				write(t, at, "not a mirror\n")
			} else if err := os.Symlink(target, at); err != nil {
				t.Fatal(err)
			}
			passRepository(t, r, f.main)
			if _, err := os.Lstat(at); !os.IsNotExist(err) {
				t.Fatalf("a prune left %s at a mirror's name: %v", kind, err)
			}
			if raw, err := os.ReadFile(target); err != nil || string(raw) != "keep\n" {
				t.Fatalf("a prune changed what a link named: %q %v", raw, err)
			}
		})
	}
}

// A lock taken from an orphaned fetch is the mirror's only while its file is still the one at the mirror's path: once
// the mirror moved while the lock was being taken, the lock is another's, and the mirror counts as busy.
func TestALockTakenFromAMovedMirrorIsBusy(t *testing.T) {
	f := newRepo(t, nil)
	mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
	lock, err := openMirror(context.Background(), mirror, f.main)
	if err != nil {
		t.Fatal(err)
	}
	// a fetch's process group that outlived its pass and ignores SIGTERM, so taking its lock lasts gitWaitDelay
	holder := exec.Command("sh", "-c", "trap '' TERM; exec sleep 60")
	holder.ExtraFiles = []*os.File{lock}
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	pid := holder.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = holder.Wait() })
	_, start, err := procStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.WriteAt([]byte(fmt.Sprintf("%d %d\n", pid, start)), 0); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if !comesTrue(5*time.Second, func() bool { age, err := procAge(start); return err == nil && age > 0 }) {
		t.Fatal("the holder's age cannot be read")
	}
	was := orphanAfter
	t.Cleanup(func() { orphanAfter = was })
	orphanAfter = 0
	taken := make(chan error, 1)
	go func() {
		l, err := lockMirror(mirror)
		if l != nil {
			l.Close()
		}
		taken <- err
	}()
	time.Sleep(time.Second) // the lock's file is open, and its holder has ignored the SIGTERM
	if err := os.Rename(mirror, mirror+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mirror, 0o700); err != nil { // another mirror at its name
		t.Fatal(err)
	}
	if err := <-taken; !errors.Is(err, errFetchRunning) {
		t.Fatalf("a lock taken from a mirror that moved meanwhile: %v", err)
	}
}

// A mirror made again keeps its old directory's lock until the new one takes its place, so no prune or fetch takes the
// old one for free meanwhile.
func TestAMirrorMadeAgainKeepsItsLockUntilTheSwap(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	ctx := context.Background()
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	mirror := identityMirror(r, "url:"+f.origin)
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, filepath.Dir(mirror), nil, "init", "-q", mirror) // a work tree, which the host makes again
	bin := t.TempDir()
	paused, resume := filepath.Join(bin, "paused"), filepath.Join(bin, "resume")
	script(t, bin, "git", `case " $* " in *" init "*) for a; do last=$a; done
  case "$last" in */.new-*) : > '`+paused+`'; i=0; while [ ! -e '`+resume+`' ] && [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done;; esac;; esac
exec '`+real+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r.Inputs.ReadDirs = []string{f.main}
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	passed := make(chan error, 1)
	go func() { passed <- r.prepareInputs(ctx) }()
	if !comesTrue(2*time.Minute, func() bool { _, err := os.Stat(paused); return err == nil }) {
		t.Fatal("the mirror was not made again")
	}
	release, free := lockForRemoval(mirror)
	if free {
		release()
	}
	write(t, resume, "")
	if err := <-passed; err != nil {
		t.Fatal(err)
	}
	if free {
		t.Fatal("the old mirror's lock was free while its replacement was made")
	}
}
