package loop

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestStartupRequiresAChildPID(t *testing.T) {
	for _, raw := range []string{"", `{"exit-code":1}`, `{"child-pid":0}`, `{"child-pid":"7"}`} {
		if sandboxStarted(strings.NewReader(raw + "\n")) {
			t.Errorf("setup status marked started: %s", raw)
		}
	}
	if !sandboxStarted(strings.NewReader("{\"future\":true}\n{\"child-pid\":7}\n{\"exit-code\":1}\n")) {
		t.Fatal("child-pid did not mark startup")
	}
}

func TestAgentDoesNotStartWhenBubblewrapSetupFails(t *testing.T) {
	// This test needs only a Unix relay socket. Its fake bubblewrap starts no harness and sends no request.
	path := filepath.Join(os.Getenv("TMPDIR"), "startup-probe.sock")
	probe, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("Unix sockets unavailable: %v", err)
	}
	probe.Close()
	os.Remove(path)
	dir := t.TempDir()
	env := filepath.Join(dir, "route.env")
	write(t, env, "CLAUDE_CODE_USE_BEDROCK=1\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=1\nANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:1/synthetic\n")
	bwrap := script(t, dir, "bwrap", `echo 'synthetic mount setup failure' >&2; exit 1`)
	exe, _ := os.Executable()
	a := Agent{Self: exe, Bwrap: bwrap, Home: "/empty", Runner: []string{"/bin/true"}, Bedrock: &Bedrock{File: env}}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), BudgetUSD: DefaultBudgets.Run, Timeout: time.Minute})
	if err != nil || res.Started || res.USD != 0 || res.Failure != "sandbox" || !strings.Contains(res.Text, "synthetic mount setup failure") {
		t.Fatalf("setup failure: %+v %v", res, err)
	}
}

func TestTheRunnerDoesNotChargeOrStopForSandboxFailures(t *testing.T) {
	ctx := context.Background()
	r := recordingRunner(t)
	c := openCase(t, r.Store, "failures", "symptom", 1)
	for i := 0; i < 2; i++ {
		run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
		if why != "" {
			t.Fatal(why)
		}
		r.finish(ctx, c, &run, Result{Status: store.RunFailed, Failure: "sandbox"}, nil)
		stored, err := r.Store.LoopRunByID(ctx, run.ID)
		if err != nil || !stored.Started.IsZero() || stored.USD != 0 || stored.Failure != "sandbox" {
			t.Fatalf("sandbox was charged as a run: %+v %v", stored, err)
		}
	}
	if why, err := r.stopped(ctx, c, false); err != nil || why != "" {
		t.Fatalf("sandbox failed the model's stop rule: %s %v", why, err)
	}
	got, _ := r.Store.CaseByID(ctx, c.ID)
	if got.Status != c.Status || got.Cost.USD != 0 || got.Cost.Duration != 0 {
		t.Fatalf("setup failure changed or charged case: %+v", got)
	}
}

func TestFailedSetupLeavesItsPairOutOfTheComparison(t *testing.T) {
	e := &Eval{Trials: 1, Runner: recordingRunner(t)}
	run, started := settleRun(store.LoopRun{Started: time.Now()}, Result{Status: store.RunFailed, Failure: "sandbox"}, nil, 4)
	if started {
		t.Fatal("sandbox started")
	}
	good := store.LoopRun{Started: time.Now(), Ended: time.Now().Add(time.Minute), USD: 1}
	r := e.Results([]Trial{{Task: "synthetic", Arm: "loop", N: 1, Run: run, Err: "sandbox"},
		{Task: "synthetic", Arm: "native", N: 1, Run: good, Grade: Grade{Verdict: "reached"}}})
	if len(r.Comparison.Tasks) != 0 {
		t.Fatalf("one-armed pair compared: %s", fmt.Sprint(r.Comparison))
	}
}
