package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Only a remote's answer fails a build as its own; every other failure, as git, curl (OpenSSL or GnuTLS) and ssh print
// it, is an outage that holds the build. The samples are Git's diagnostics as they are printed.
func TestOnlyARemotesAnswerFailsABuild(t *testing.T) {
	const access = "fatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n"
	for _, c := range []struct {
		name, stderr string
		answered     bool
	}{
		{"GnuTLS handshake", "fatal: unable to access 'https://github.com/o/r.git/': GnuTLS, handshake failed: The TLS connection was non-properly terminated.\n", false},
		{"GnuTLS receive", "fatal: unable to access 'https://github.com/o/r.git/': GnuTLS recv error (-110): The TLS connection was non-properly terminated.\n", false},
		{"GnuTLS receive, another code", "error: RPC failed; curl 56 GnuTLS recv error (-9): Error decoding the received TLS packet.\n", false},
		{"OpenSSL read", "error: RPC failed; curl 56 OpenSSL SSL_read: SSL_ERROR_SYSCALL, errno 104\nfatal: early EOF\n", false},
		{"OpenSSL connect", "fatal: unable to access 'https://github.com/o/r.git/': OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443 \n", false},
		{"ssh closed", "Connection closed by 192.0.2.1 port 22\r\n" + access, false},
		{"ssh reset", "kex_exchange_identification: read: Connection reset by peer\r\nConnection reset by 192.0.2.1 port 22\r\n" + access, false},
		{"ssh refused", "ssh: connect to host github.com port 22: Connection refused\r\n" + access, false},
		{"ssh timed out", "ssh: connect to host github.com port 22: Connection timed out\r\n" + access, false},
		{"ssh name", "ssh: Could not resolve hostname github.com: Temporary failure in name resolution\r\n" + access, false},
		{"https name", "fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com\n", false},
		{"https refused", "fatal: unable to access 'https://github.com/o/r.git/': Failed to connect to github.com port 443 after 3 ms: Could not connect to server\n", false},
		{"5xx", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 502\n", false},
		{"too many requests", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 429\n", false},
		{"early EOF", "remote: Enumerating objects: 9, done.\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n", false},
		{"empty reply", "fatal: unable to access 'https://github.com/o/r.git/': Empty reply from server\n", false},
		{"outstanding read data", "error: RPC failed; curl 18 transfer closed with outstanding read data remaining\nfatal: expected flush after ref listing\n", false},
		{"http2 framing", "error: RPC failed; curl 16 Error in the HTTP2 framing layer\nfatal: expected flush after ref listing\n", false},
		{"http2 stream", "error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL (err 8)\n", false},
		{"too slow", "error: RPC failed; curl 28 Operation too slow. Less than 1 bytes/sec transferred the last 60 seconds\n", false},
		{"no diagnostics", "", false},
		{"GitHub https, no such repository", "remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found\n", true},
		{"GitHub ssh, no such repository", "ERROR: Repository not found.\n" + access, true},
		{"no such path", "fatal: '/srv/gone.git' does not appear to be a git repository\n" + access, true},
		{"no such branch", "fatal: couldn't find remote ref main\n", true},
		{"local http, no such repository", "fatal: repository 'http://127.0.0.1:8080/r.git/' not found\n", true},
		{"https 404 by curl", "fatal: unable to access 'https://git.example.org/r.git/': The requested URL returned error: 404\n", true},
		{"credentials gh gave, refused", "remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/o/r.git/'\n", false},
		{"no credentials", "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n", false},
		{"401", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 401\n", false},
		{"a local file the call cannot write", "error: cannot open '.git/FETCH_HEAD': Permission denied\n", false},
		{"ssh, key refused with methods", "git@example.org: Permission denied (publickey,password).\r\n" + access, true},
		{"key refused", "git@github.com: Permission denied (publickey).\r\n" + access, true},
		{"forbidden", "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403\n", true},
		{"not found", "fatal: unable to access 'https://git.example.org/r.git/': The requested URL returned error: 404\n", true},
	} {
		if got := remoteAnswered([]byte(c.stderr)); got != c.answered {
			t.Errorf("%s: answered %v, want %v", c.name, got, c.answered)
		}
	}
}

// A pass takes a mirror's lock from a process group that has held it past orphanAfter only when the lock's record still
// names that group: its leader runs, with the start time the record gives. A record whose number another process has
// now is never signalled.
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
			if err := lock.Truncate(0); err != nil {
				t.Fatal(err)
			}
			if _, err := lock.WriteAt([]byte(fmt.Sprintf("%d %d\n", pid, start+c.shift)), 0); err != nil {
				t.Fatal(err)
			}
			lock.Close() // the pass that started it is gone; its group holds the lock
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
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("a process the record does not name was signalled")
			}
		})
	}
}
