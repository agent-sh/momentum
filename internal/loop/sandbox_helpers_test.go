package loop

import (
	"flag"
	"fmt"
	"github.com/agent-sh/momentum/internal/sandbox"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testSandboxAgent(t *testing.T, a Agent) Agent {
	t.Helper()
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	if a.Bedrock == nil {
		a.Bedrock = testBedrock(t)
	}
	if len(a.Runner) > 0 {
		// Fixture bookkeeping has its own explicitly bound directory.
		a.Binds = append(a.Binds, sandbox.Bind{Src: filepath.Dir(a.Runner[0]), Dst: "/opt/fixture", Write: true})
		a.Runner = append([]string(nil), a.Runner...)
		if raw, err := os.ReadFile(a.Runner[0]); err == nil && strings.HasPrefix(string(raw), "#!/bin/sh") {
			if err = os.WriteFile(a.Runner[0], []byte(strings.ReplaceAll(string(raw), filepath.Dir(a.Runner[0]), "/opt/fixture")), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	return a
}
func TestMain(m *testing.M) {
	// test processes run side by side (reviews, a second checkout), and a live loop may run: each stops only its own
	// leftover units, so the prefix of a test is not one that begins with the loop's
	UnitPrefix = fmt.Sprintf("momentum-run-looptest%d-", os.Getpid())
	EvalUnitPrefix = fmt.Sprintf("momentum-run-evaltest%d-", os.Getpid())
	Scrubber = []string{"sed", "-e", "s/AKIA[A-Z0-9]*EXAMPLE/[masked]/g", "-e", "s/password=marker-7f3a/password=[masked]/g"}
	// Go's test log (-test.testlogfile, which go test passes unless the run cannot be cached) records every file a
	// test stats or opens, and the go command reads it whole after the run and hashes what it names. This suite
	// works over thousands of files in repositories of its own, for half an hour: with the log on the go command
	// holds several times the log's size. Without it the run is not cached, which a suite that tests the host's
	// sandbox should not be.
	flag.Parse()
	flag.Set("test.testlogfile", "")
	os.Exit(m.Run())
}

// The suite runs with Go's test log off: the go command reads that log whole, and this suite fills it. The test
// binary, started as go test starts it for a run it may cache, writes no log.
func TestGoTestLogIsOff(t *testing.T) {
	log := filepath.Join(t.TempDir(), "testlog.txt")
	if out, err := exec.Command(os.Args[0], "-test.run=^$", "-test.testlogfile="+log).CombinedOutput(); err != nil {
		t.Fatalf("the test binary with no test to run: %v\n%s", err, out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("go's test log was written: TestMain turns it off, so a run of this suite leaves the go command nothing to read")
	}
}
