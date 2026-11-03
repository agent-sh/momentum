package loop

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A run's PATH is the read-only reader's directory before the PATH its invocation gives (a build's toolchains), not
// in place of it. A fixture tool that lives only in a toolchain directory proves it wherever the host's go lives.
func TestARunKeepsItsToolchainPathBehindTheReader(t *testing.T) {
	tools := t.TempDir()
	script(t, filepath.Join(tools, "bin"), "zz-fixture-tool", "printf 'fixture tool\\n'")
	harness := script(t, t.TempDir(), "harness", `zz-fixture-tool >/dev/null || exit 9
case ":$PATH:" in :/opt/momentum:*) ;; *) exit 8;; esac
`+fixtureResult(okResult))
	a := testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5"})
	tc := Toolchains{ReadOnly: []string{tools}, Path: []string{filepath.Join(tools, "bin")}}
	binds, env, err := tc.inputs()
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), System: "marker-7f3a", Schema: investigationSchema,
		MaxTurns: 5, BudgetUSD: 1, Timeout: time.Minute, Binds: binds, Env: env})
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("the run's PATH lost its toolchain or the reader: %s %q %v", res.Status, res.Failure, err)
	}
}
