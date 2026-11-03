package loop

import (
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
	Scrubber = []string{"sed", "-e", "s/AKIA[A-Z0-9]*EXAMPLE/[masked]/g", "-e", "s/password=marker-7f3a/password=[masked]/g"}
	os.Exit(m.Run())
}
