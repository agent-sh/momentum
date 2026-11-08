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

// A prune that found a link where a mirror's lock is looks at the mirror's lock again once it holds the makers' lock: a
// maker may have swapped a mirror in before then. A mirror swapped in that another holds is not taken, and the makers'
// lock goes with it; one nothing holds is taken by its own lock as well; a link still there is held by the makers'
// lock alone.
func TestAPruneLooksAtTheMirrorsLockAgainUnderTheMakersLock(t *testing.T) {
	for _, kind := range []string{"a mirror another holds", "a mirror nothing holds", "the link still there"} {
		t.Run(kind, func(t *testing.T) {
			mirrors := filepath.Join(t.TempDir(), "mirrors")
			mirror := filepath.Join(mirrors, "0123456789abcdef.git")
			if err := os.MkdirAll(mirror, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target")
			write(t, target, "keep\n")
			switch kind {
			case "a mirror another holds":
				other, err := lockMirror(mirror) // a fetch in the mirror a maker swapped in
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			case "the link still there":
				if err := os.Symlink(target, filepath.Join(mirror, mirrorLockFile)); err != nil {
					t.Fatal(err)
				}
			}
			release, ok := lockLinkedForRemoval(mirror)
			if ok {
				defer release()
			}
			makersFree := func() bool {
				unlock, err := lockMakers(context.Background(), mirrors, 0)
				if err != nil {
					return false
				}
				unlock()
				return true
			}
			if kind == "a mirror another holds" {
				if ok {
					t.Fatal("a prune took a mirror another holds")
				}
				if !makersFree() {
					t.Fatal("a prune that took no mirror kept the makers' lock")
				}
				return
			}
			if !ok {
				t.Fatal("the prune took no lock")
			}
			if makersFree() {
				t.Fatal("a maker could swap the mirror a prune was removing")
			}
			switch kind {
			case "a mirror nothing holds":
				if l, err := lockMirror(mirror); !errors.Is(err, errFetchRunning) {
					if l != nil {
						l.Close()
					}
					t.Fatalf("a fetch took the lock of a mirror a prune was removing: %v", err)
				}
			case "the link still there":
				if raw, err := os.ReadFile(target); err != nil || string(raw) != "keep\n" {
					t.Fatalf("a prune changed what the lock's link named: %q %v", raw, err)
				}
			}
		})
	}
}

// A prune tries the makers' lock once: while a maker holds it, a mirror with a link where its lock is stays, and the
// prune goes on at once, never waiting the makersWait a maker waits.
func TestAPruneTriesTheMakersLockOnce(t *testing.T) {
	mirrors := filepath.Join(t.TempDir(), "mirrors")
	mirror := filepath.Join(mirrors, "0123456789abcdef.git")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), filepath.Join(mirror, mirrorLockFile)); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockMakers(context.Background(), mirrors, 0) // a maker's swap
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	began := time.Now()
	release, ok := lockForRemoval(mirror)
	waited := time.Since(began)
	if ok {
		release()
		t.Fatal("a prune took a mirror while a maker held the makers' lock")
	}
	if waited > makersWait/6 {
		t.Fatalf("a prune waited %v for the makers' lock", waited.Round(time.Millisecond))
	}
}

// A pass takes a mirror's lock from an orphan's group only when the record's taker is gone: its number is another
// process's now (one that started at another time), or it has ended and waits for its parent (a zombie). A record that
// names no taker, its group alone, is never taken.
func TestAnOrphanIsTakenOnlyWhenItsRecordsTakerIsGone(t *testing.T) {
	for _, c := range []struct {
		name  string
		taken bool
	}{{"another process under the taker's number", true}, {"a taker not yet reaped", true}, {"a group-only record", false}} {
		t.Run(c.name, func(t *testing.T) {
			mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
			if err := os.MkdirAll(mirror, 0o700); err != nil {
				t.Fatal(err)
			}
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
			t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = holder.Wait() })
			_, start, err := procStat(pid)
			if err != nil {
				t.Fatal(err)
			}
			var recorded lockHolder
			switch c.name {
			case "another process under the taker's number":
				other := exec.Command("sleep", "60")
				if err := other.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
				_, started, err := procStat(other.Process.Pid)
				if err != nil {
					t.Fatal(err)
				}
				recorded = lockHolder{other.Process.Pid, started - 1} // the taker that had the number started earlier
			case "a taker not yet reaped":
				ended := exec.Command("sleep", "0")
				if err := ended.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ended.Wait() })
				zpid := ended.Process.Pid
				if !comesTrue(5*time.Second, func() bool { p, err := readProc(zpid); return err == nil && p.state == 'Z' }) {
					t.Fatal("the taker never ended")
				}
				p, err := readProc(zpid)
				if err != nil {
					t.Fatal(err)
				}
				recorded = lockHolder{zpid, p.start}
			}
			writeLockRecord(lock, lockRecord{taker: recorded, group: lockHolder{pid, start}})
			lock.Close()
			// /proc counts in hundredths of a second: the holder is older than orphanAfter once it is any age at all
			if !comesTrue(5*time.Second, func() bool { age, err := procAge(start); return err == nil && age > 0 }) {
				t.Fatal("the holder's age cannot be read")
			}
			was := orphanAfter
			t.Cleanup(func() { orphanAfter = was })
			orphanAfter = 0
			lock, err = lockMirror(mirror)
			if c.taken {
				if err != nil {
					t.Fatalf("the pass did not take the lock: %v", err)
				}
				lock.Close()
				done := make(chan error, 1)
				go func() { done <- holder.Wait() }()
				select {
				case <-done:
				case <-time.After(3 * gitWaitDelay):
					t.Fatal("the group that held the lock still runs")
				}
				return
			}
			if !errors.Is(err, errFetchRunning) {
				if lock != nil {
					lock.Close()
				}
				t.Fatalf("a lock whose record names no taker: %v", err)
			}
			if p, err := readProc(pid); err != nil || p.state == 'Z' {
				t.Fatal("the group of a record that names no taker was signalled")
			}
		})
	}
}

// takerChild marks the process of its own that TestATakerReadsItsStatusAgainAfterAFailedRead and
// TestATakerReadsItsStatusAgainWhenItRecordsACall run in.
const takerChild = "MOMENTUM_TEST_TAKER_CHILD"

// A taker whose read of its own status failed (no descriptor was free) reads it again at the next lock it takes: a
// failed read is never kept, so a lock taken once /proc can be read names its taker. The check runs in a process of its
// own, whose first read is the one that fails. The next call it records reads it again too;
// TestATakerReadsItsStatusAgainWhenItRecordsACall covers that path.
func TestATakerReadsItsStatusAgainAfterAFailedRead(t *testing.T) {
	if os.Getenv(takerChild) == "" {
		child := exec.Command(os.Args[0], "-test.run=^TestATakerReadsItsStatusAgainAfterAFailedRead$", "-test.v")
		child.Env = append(os.Environ(), takerChild+"=1")
		out, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestATakerReadsItsStatusAgainAfterAFailedRead") {
			t.Fatalf("the taker's process: %v\n%s", err, out)
		}
		return
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	lowest := probe.Fd() // the lowest descriptor free, once probe is closed
	probe.Close()
	none := limit
	none.Cur = uint64(lowest)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &none); err != nil {
		t.Fatal(err)
	}
	failed := thisTaker() // no descriptor free: the read of /proc fails
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	if failed != (lockHolder{}) {
		t.Fatalf("a read with no descriptor free found %v", failed)
	}
	_, start, err := procStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	want := lockHolder{os.Getpid(), start}
	lock, err := lockMirror(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if got := readLockRecord(lock).taker; got != want {
		t.Fatalf("a lock taken after a failed read names the taker %v, want %v", got, want)
	}
}

// A taker whose read of its own status failed as it took a lock (no descriptor was free) reads it again when it records
// a call under that lock: the record names its taker from then on. The check runs in a process of its own, whose first
// read is the one that fails.
func TestATakerReadsItsStatusAgainWhenItRecordsACall(t *testing.T) {
	if os.Getenv(takerChild) == "" {
		child := exec.Command(os.Args[0], "-test.run=^TestATakerReadsItsStatusAgainWhenItRecordsACall$", "-test.v")
		child.Env = append(os.Environ(), takerChild+"=1")
		out, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestATakerReadsItsStatusAgainWhenItRecordsACall") {
			t.Fatalf("the taker's process: %v\n%s", err, out)
		}
		return
	}
	mirror := t.TempDir()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	lowest := probe.Fd() // the lowest descriptor free, once probe is closed
	probe.Close()
	one := limit
	one.Cur = uint64(lowest) + 1 // one descriptor free: the lock file's, and none for the read of /proc
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &one); err != nil {
		t.Fatal(err)
	}
	lock, err := lockMirror(mirror)
	if rerr := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if got := readLockRecord(lock).taker; got != (lockHolder{}) {
		t.Fatalf("a lock taken with no descriptor free for /proc names the taker %v", got)
	}
	_, start, err := procStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	want := lockHolder{os.Getpid(), start}
	recordHolder(lock, os.Getpid())
	if r := readLockRecord(lock); r.taker != want || r.group != want {
		t.Fatalf("a call recorded after a failed read leaves the record %v, want the taker %v", r, want)
	}
}
