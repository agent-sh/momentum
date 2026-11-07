package loop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A remote call's failure is named by one of a fixed few classes, whatever git, curl (OpenSSL or GnuTLS) and ssh
// printed: the samples are Git's diagnostics as they are printed.
func TestARemoteFailureIsNamedByItsClass(t *testing.T) {
	const access = "fatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n"
	const (
		broke     = "the connection broke off"
		noRepo    = "the remote has no such repository or branch"
		noCreds   = "no credentials"
		refusedBy = "the server refused (an HTTP 4xx answer)"
	)
	for _, c := range []struct{ name, stderr, class string }{
		{"GnuTLS handshake", "fatal: unable to access 'https://github.com/o/r.git/': GnuTLS, handshake failed: The TLS connection was non-properly terminated.\n", broke},
		{"GnuTLS receive", "fatal: unable to access 'https://github.com/o/r.git/': GnuTLS recv error (-110): The TLS connection was non-properly terminated.\n", broke},
		{"OpenSSL read", "error: RPC failed; curl 56 OpenSSL SSL_read: SSL_ERROR_SYSCALL, errno 104\nfatal: early EOF\n", broke},
		{"ssh closed", "Connection closed by 192.0.2.1 port 22\r\n" + access, broke},
		{"ssh refused", "ssh: connect to host github.com port 22: Connection refused\r\n" + access, "no connection to the server could be made"},
		{"ssh timed out", "ssh: connect to host github.com port 22: Connection timed out\r\n" + access, "the connection timed out"},
		{"ssh name", "ssh: Could not resolve hostname github.com: Temporary failure in name resolution\r\n" + access, "the server's name did not resolve"},
		{"https name", "fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com\n", "the server's name did not resolve"},
		{"5xx", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 502\n", "the server failed (an HTTP 5xx answer)"},
		{"too many requests", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 429\n", otherFailure},
		{"empty reply", "fatal: unable to access 'https://github.com/o/r.git/': Empty reply from server\n", broke},
		{"http2 framing", "error: RPC failed; curl 16 Error in the HTTP2 framing layer\nfatal: expected flush after ref listing\n", broke},
		{"no diagnostics", "", otherFailure},
		{"GitHub https, no such repository", "remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found\n", noRepo},
		{"GitHub ssh, no such repository", "ERROR: Repository not found.\n" + access, noRepo},
		{"local http, no such repository", "fatal: repository 'http://127.0.0.1:8080/r.git/' not found\n", noRepo},
		{"no such path", "fatal: '/srv/gone.git' does not appear to be a git repository\n" + access, noRepo},
		{"no such branch", "fatal: couldn't find remote ref main\n", noRepo},
		{"https 404", "fatal: unable to access 'https://git.example.org/r.git/': The requested URL returned error: 404\n", noRepo},
		{"forbidden", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403\n", refusedBy},
		{"credentials gh gave, refused", "remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/o/r.git/'\n", noCreds},
		{"no credentials", "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n", noCreds},
		{"401", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 401\n", noCreds},
		{"ssh key refused", "git@github.com: Permission denied (publickey).\r\n" + access, noCreds},
		{"ssh agent refused", "sign_and_send_pubkey: signing failed for ED25519 \"/home/u/.ssh/id_ed25519\" from agent: agent refused operation\r\ngit@github.com: Permission denied (publickey).\r\n" + access, noCreds},
		{"SAML SSO", "remote: The 'org' organization has enabled or enforced SAML SSO. To access this repository, you must use the HTTPS remote with a personal access token or SSH with an SSH key and passphrase that has been authorized for this organization.\nfatal: unable to access 'https://github.com/org/r.git/': The requested URL returned error: 403\n", noCreds},
		{"a local file the call cannot write", "error: cannot open '.git/FETCH_HEAD': Permission denied\n", otherFailure},
	} {
		if got, _ := holdClass([]byte(c.stderr)); got != c.class {
			t.Errorf("%s: class %q, want %q", c.name, got, c.class)
		}
	}
}

// A pass takes a mirror's lock from a process group whose taker is gone and that has held it past orphanAfter only when
// the lock's record still names that group: its leader runs, with the start time the record gives. A record whose
// number another process has now is never signalled.
func TestAPassTakesALockOnlyFromTheGroupItsRecordNames(t *testing.T) {
	for _, c := range []struct {
		name  string
		shift uint64
		taken bool
	}{{"the record's group", 0, true}, {"another process with its number", 1, false}} {
		t.Run(c.name, func(t *testing.T) {
			f := newRepo(t, nil)
			mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
			lock, err := openMirror(context.Background(), mirror, f.main)
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
			// the record's taker, the pass that took the lock, is gone; its group holds the lock
			writeLockRecord(lock, lockRecord{taker: endedProcess(t), group: lockHolder{pid, start + c.shift}})
			lock.Close()
			// /proc counts in hundredths of a second: the holder is older than orphanAfter once it is any age at all
			if !comesTrue(5*time.Second, func() bool { age, err := procAge(start); return err == nil && age > 0 }) {
				t.Fatal("the holder's age cannot be read")
			}
			was := orphanAfter
			t.Cleanup(func() { orphanAfter = was })
			orphanAfter = 0
			lock, err = openMirror(context.Background(), mirror, f.main)
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
				t.Fatalf("a lock whose record names another process: %v", err)
			}
			if p, err := readProc(pid); err != nil || p.state == 'Z' {
				t.Fatal("a process the record does not name was signalled")
			}
		})
	}
}

// endedProcess is a process that ran and has ended, by its number and its start time: the taker of a lock whose pass
// is gone.
func endedProcess(t *testing.T) lockHolder {
	t.Helper()
	p := exec.Command("sleep", "60")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	_, start, err := procStat(p.Process.Pid)
	_ = p.Process.Kill()
	_ = p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return lockHolder{p.Process.Pid, start}
}

// recordOrphan writes into the lock's file the record of a fetch whose pass is gone: a taker that has ended, and the
// process group pid leads. It returns the leader's start time.
func recordOrphan(t *testing.T, lock *os.File, pid int) uint64 {
	t.Helper()
	_, start, err := procStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	writeLockRecord(lock, lockRecord{taker: endedProcess(t), group: lockHolder{pid, start}})
	return start
}

// A fetch whose lock's taker still runs is never ended, however long its group has held the lock by the boot clock,
// which counts a suspend: the owner's preparation that took the lock still waits for its fetch. orphanAfter 0 stands
// for a group that clock aged past orphanAfter; this process is the taker, and holds its lock as a preparation does.
func TestAFetchWhoseTakerRunsIsNeverEnded(t *testing.T) {
	for _, by := range []string{"a fetch", "a prune"} {
		t.Run(by, func(t *testing.T) {
			f := newRepo(t, nil)
			mirror := filepath.Join(t.TempDir(), "mirrors", "0123456789abcdef.git")
			lock, err := openMirror(context.Background(), mirror, f.main)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			// the taker's fetch: its trap says whether a signal came
			termed := filepath.Join(t.TempDir(), "termed")
			holder := exec.Command("sh", "-c", "trap ': > \""+termed+"\"' TERM; while :; do sleep 1; done")
			holder.ExtraFiles = []*os.File{lock}
			holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := holder.Start(); err != nil {
				t.Fatal(err)
			}
			pid := holder.Process.Pid
			t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = holder.Wait() })
			recordHolder(lock, pid) // as a fetch's call records its group
			_, start, err := procStat(pid)
			if err != nil {
				t.Fatal(err)
			}
			if !comesTrue(5*time.Second, func() bool { age, err := procAge(start); return err == nil && age > 0 }) {
				t.Fatal("the holder's age cannot be read")
			}
			was := orphanAfter
			t.Cleanup(func() { orphanAfter = was })
			orphanAfter = 0
			switch by {
			case "a fetch":
				if l, err := lockMirror(mirror); !errors.Is(err, errFetchRunning) {
					if l != nil {
						l.Close()
					}
					t.Fatalf("a fetch beside a live taker's fetch: %v", err)
				}
			case "a prune":
				if release, ok := lockForRemoval(mirror); ok {
					release()
					t.Fatal("a prune took the lock of a live taker's fetch")
				}
			}
			if _, err := os.Stat(termed); err == nil {
				t.Fatal("the fetch of a taker that still runs was signalled")
			}
		})
	}
}
