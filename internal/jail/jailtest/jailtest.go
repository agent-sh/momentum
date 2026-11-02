// Package jailtest holds what the tests of the packages that run a sandbox share: the probe that says whether
// bubblewrap can make one on this machine, and a directory for sockets.
package jailtest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
)

// MaxSocketPath is the longest path of a Unix socket: sun_path holds 108 bytes and one of them ends the string.
const MaxSocketPath = 107

// socketTail is the longest path below a socket directory that a test uses, with its separator: "/new/route.sock".
const socketTail = len("/new/route.sock")

var (
	probeOnce sync.Once
	probePath string
	probeWhy  string
)

// NeedBwrap returns the bubblewrap binary, or skips the test with the reason when this machine cannot make a
// sandbox: no bubblewrap, or one that fails to make the namespaces, the /proc and the /dev a sandbox has (a host that
// restricts unprivileged user namespaces, a container that cannot mount /proc). Every test that runs a sandbox
// starts with it, in every package, so they all skip for the same reason.
func NeedBwrap(t testing.TB) string {
	t.Helper()
	probeOnce.Do(func() {
		p, err := exec.LookPath("bwrap")
		if err != nil {
			probeWhy = "no bubblewrap on this machine"
			return
		}
		prog, err := exec.LookPath("true")
		if err != nil {
			probeWhy = "no true(1) on this machine to probe bubblewrap with"
			return
		}
		out, err := exec.Command(p, "--unshare-all", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev",
			"--tmpfs", "/tmp", prog).CombinedOutput()
		if err != nil {
			probeWhy = fmt.Sprintf("bubblewrap cannot make a sandbox here: %v: %s", err, bytes.TrimSpace(out))
			return
		}
		probePath = p
	})
	if probeWhy != "" {
		t.Skip(probeWhy)
	}
	return probePath
}

// SocketDir is a new directory only the test's user can enter, for sockets, removed when the test ends. It is under
// the run time directory when there is one, else under the temporary directory, and the test skips with the reason
// when a socket path in it as long as "new/route.sock" would be over MaxSocketPath bytes (a long TMPDIR makes it so).
func SocketDir(t testing.TB) string {
	t.Helper()
	base := os.Getenv("XDG_RUNTIME_DIR")
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "mj-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if len(dir)+socketTail > MaxSocketPath {
		t.Skipf("the socket directory %s is %d bytes: a socket path in it would be over the %d bytes a Unix socket allows",
			dir, len(dir), MaxSocketPath)
	}
	return dir
}
