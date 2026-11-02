package loop

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agent-sh/momentum/internal/jail"
)

// testJail fills in an agent's sandbox for a test: bubblewrap (the test is skipped without it), the test binary as
// momentum's (its init and its door's client, TestMain), the home, a route, and the fake harness's directory,
// writable, which it keeps its records in.
func testJail(t *testing.T, a Agent) Agent {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bubblewrap on this machine")
	}
	if out, err := exec.Command(bwrap, "--unshare-all", "--ro-bind", "/", "/", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot make a sandbox here: %v: %s", err, out)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a.Bwrap, a.Self, a.Home = bwrap, exe, os.Getenv("HOME")
	if a.Bedrock == nil {
		a.Bedrock = testBedrock(t)
	}
	if len(a.Runner) > 0 {
		a.Mounts = append(a.Mounts, jail.Mount{Path: filepath.Dir(a.Runner[0]), Write: true})
	}
	return a
}
