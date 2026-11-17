package loop

import (
	"flag"
	"fmt"
	"github.com/agent-sh/momentum/internal/sandbox"
	"os"
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
	code := m.Run()
	if err := testLogBound(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// testLogMax bounds Go's test log, should it be written after all (TestMain turns it off). When the sandbox walked
// its sources through package os, a full run of this package wrote a log of several GB, and the go command grew
// to tens of GB of memory. A run whose log passes the bound fails here, before the go command reads it.
const testLogMax = 64 << 20

func testLogBound() error {
	f := flag.Lookup("test.testlogfile")
	if f == nil || f.Value.String() == "" {
		return nil
	}
	st, err := os.Stat(f.Value.String())
	if err != nil {
		return nil // no log was written
	}
	if st.Size() > testLogMax {
		// the go command would read all of it: leave it nothing to grow on
		if err := os.Truncate(f.Value.String(), 0); err != nil {
			return fmt.Errorf("go's test log is %d MiB (the bound is %d) and could not be emptied: %v", st.Size()>>20, testLogMax>>20, err)
		}
		return fmt.Errorf("go's test log grew to %d MiB (the bound is %d): a test walks a large tree through os.Stat or os.Open, and the go command "+
			"holds and hashes that log after the run. TestMain turns the log off: see why it was written", st.Size()>>20, testLogMax>>20)
	}
	return nil
}

// The suite runs with Go's test log off: the go command reads that log whole, and this suite fills it.
func TestGoTestLogIsOff(t *testing.T) {
	if f := flag.Lookup("test.testlogfile"); f != nil && f.Value.String() != "" {
		t.Fatalf("go's test log is on (%s): TestMain turns it off, so a run of this suite leaves the go command nothing to read", f.Value)
	}
}
