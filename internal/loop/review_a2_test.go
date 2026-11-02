package loop

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/store"
)

func TestProposalRefusesAProtectedEditDuringValidation(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/momentum", nil }
	c, p, pr := recoveryChecked(t, s, st, f)
	s.Public = func(_ context.Context, text string) (string, error) {
		write(t, filepath.Join(p.Work, "internal/jail/probe.go"), "package jail // marker-7f3a\n")
		return text, nil
	}
	q, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	if tree := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", q.Head+"^{tree}")); tree != p.Check.Tree {
		t.Fatal("concurrent run write changed the commit")
	}
	if strings.Contains(run(t, p.Host, "git", "ls-tree", "-r", q.Head), "internal/jail") {
		t.Fatal("protected run file shipped")
	}
}

func TestFetchedBranchDoesNotWeakenProposalLease(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	ctx := context.Background()
	if _, err := s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "p", Fix: "f"}}, Comment: "c1"}); err != nil {
		t.Fatal(err)
	}
	theirs := pushOther(t, f, p.Branch, map[string]string{"theirs.txt": "marker-7f3a\n"})
	run(t, f.main, "git", "fetch", "origin", p.Branch)
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is 42.", 1))
	if _, err := checkPatch(t, s, ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	if _, err := proposePatch(t, s, ctx, c.ID, pr); err == nil {
		t.Fatal("revision overwrote the fetched remote change")
	}
	if originHead(t, f, p.Branch) != theirs {
		t.Fatal("other push was lost")
	}
}

func TestDoorWithholdsOversizedProduction(t *testing.T) {
	dir := t.TempDir()
	prog := script(t, dir, "output", `head -c 8388610 /dev/zero`)
	r := Runner{Momentum: prog, Budgets: Budgets{RunTime: time.Minute}}
	reply := r.door(StepReview, store.Case{ID: "fixture"}, store.LoopRun{ID: "fixture", Dir: dir}, []string{dir})(
		context.Background(), jail.Request{Argv: []string{"momentum", "case", "show", "fixture"}, Dir: dir})
	if reply.Code == 0 || !strings.Contains(string(reply.Stdout)+string(reply.Stderr), "(output withheld: over the door's limit)") {
		t.Fatalf("oversized output was not withheld: code %d, stdout %d, stderr %d", reply.Code, len(reply.Stdout), len(reply.Stderr))
	}
}

func TestProposalHookRefusesAProtectedEditBeforeCommit(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/momentum", nil }
	c, p, pr := recoveryChecked(t, s, st, f)
	s.BeforeCommit = func() { write(t, filepath.Join(p.Work, "internal/jail/probe.go"), "package jail // marker-7f3a\n") }
	q, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	if tree := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", q.Head+"^{tree}")); tree != p.Check.Tree {
		t.Fatal("concurrent run write changed the commit")
	}
	if strings.Contains(run(t, p.Host, "git", "ls-tree", "-r", q.Head), "internal/jail") {
		t.Fatal("protected run file shipped")
	}
}

func TestFetchedBranchDoesNotWeakenRebasedLandingLease(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"base.txt": "marker-7f3a\n"})
	var theirs string
	s.Sandbox.Gate = func(context.Context) error {
		theirs = pushOther(t, f, p.Branch, map[string]string{"theirs.txt": "marker-7f3a\n"})
		run(t, f.main, "git", "fetch", "origin", p.Branch)
		return nil
	}
	if _, err := s.Land(context.Background(), c.ID); err == nil {
		t.Fatal("rebased landing overwrote the fetched branch")
	}
	if theirs == "" || originHead(t, f, p.Branch) != theirs {
		t.Fatal("another push was lost")
	}
	if strings.TrimSpace(run(t, p.Host, "git", "rev-parse", "HEAD")) != p.Head {
		t.Fatal("reviewed head was not restored")
	}
}

func TestDoorCancellationKillsItsCommand(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	prog := script(t, dir, "blocked", `echo $$ > '`+pidFile+`'
exec tail -f /dev/null`)
	r := Runner{Momentum: prog, Budgets: Budgets{RunTime: time.Minute}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan jail.Reply, 1)
	go func() {
		done <- r.door(StepReview, store.Case{}, store.LoopRun{Dir: dir}, []string{dir})(ctx, jail.Request{Argv: []string{"momentum", "case", "show", "fixture"}})
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var pid int
	for pid == 0 {
		select {
		case <-tick.C:
			b, err := os.ReadFile(pidFile)
			if err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			}
		case <-deadline.C:
			t.Fatal("command did not start")
		}
	}
	cancel()
	select {
	case reply := <-done:
		if reply.Code == 0 {
			t.Fatal("cancelled command succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("command is still present: %v", err)
	}
}

func TestCaptureRetainsOnlyLimitPlusOneByte(t *testing.T) {
	c := Capture{Limit: jail.MaxStream}
	block := make([]byte, 1<<20)
	for i := 0; i < 128; i++ {
		c.Write(block)
	}
	if !c.Over() || c.Len() != jail.MaxStream+1 || cap(c.Bytes()) > jail.MaxStream+1 {
		t.Fatalf("retained %d bytes", c.Len())
	}
}

func TestHostWritesIgnorePlantedRunFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	write(t, canary, "marker-7f3a")
	for _, name := range []string{"system.md", "schema.json", "transcript.jsonl", "stderr.txt"} {
		if err := os.Symlink(canary, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	host := HostDir(dir)
	if err := hostDir(host); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []string{"primary", "fallback"} {
		input := filepath.Join(host, attempt)
		if err := hostDir(input); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"system.md", "schema.json"} {
			if err := writeExclusive(input, name, []byte("safe input")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeExclusive(host, "transcript.jsonl", []byte(okResult+"\n")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "replacement"), `{"type":"result","subtype":"success","total_cost_usd":0,"structured_output":{"summary":"fake"}}`)
	if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "transcript.jsonl")); err != nil {
		t.Fatal(err)
	}
	result, err := readStream(filepath.Join(host, "transcript.jsonl"))
	if err != nil || result.USD != .75 {
		t.Fatalf("replacement changed the result: %+v %v", result, err)
	}
	if b, _ := os.ReadFile(canary); string(b) != "marker-7f3a" {
		t.Fatal("host canary changed")
	}
	if err := writeExclusive(host, "transcript.jsonl", []byte("replacement")); err == nil {
		t.Fatal("host archive was replaced")
	}
}

func TestNonzeroExitCannotKeepAParsedSuccess(t *testing.T) {
	res := Result{Status: store.RunOK, CostKnown: true, USD: .75}
	err := exec.Command("sh", "-c", "exit 7").Run()
	reconcileExit(&res, err)
	if res.Status != store.RunFailed || res.Failure != "exit 7" || res.USD != .75 {
		t.Fatalf("exit reconciliation: %+v", res)
	}
	for _, key := range []string{"the model's safeguards", "api error", "budget", "turns"} {
		res := Result{Status: store.RunFailed, Failure: key}
		reconcileExit(&res, err)
		if res.Failure != key {
			t.Fatal("changed the stop rule's key")
		}
	}
}

func TestRunnerChargesTheHigherOfHarnessAndRelay(t *testing.T) {
	for _, tc := range []struct {
		known                bool
		harness, relay, want float64
	}{
		{true, .2, .7, .7}, {true, .7, .2, .7}, {false, 0, 6, 6},
	} {
		st := testStore(t)
		c := openCase(t, st, "failures", "fixture", 1)
		r := &Runner{Store: st, Dir: t.TempDir(), Budgets: Budgets{Run: 4, Case: 20, Day: 40, Week: 200}}
		row, why := r.reserve(context.Background(), c.Family, c.ID, StepInvestigate)
		if why != "" {
			t.Fatal(why)
		}
		r.finish(context.Background(), c, &row, Result{Status: store.RunOK, CostKnown: tc.known, USD: tc.harness, RelayUSD: tc.relay}, nil)
		got, err := st.LoopRunByID(context.Background(), row.ID)
		if err != nil || got.USD != tc.want {
			t.Fatalf("charge: %+v %v, want %f", got, err, tc.want)
		}
	}
}

func TestProposalAdoptsTheExactPushAfterALostResponse(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, _, pr := recoveryChecked(t, s, st, f)
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.dir, "lose-response")
	write(t, marker, "marker-7f3a")
	bin := filepath.Join(f.dir, "bin")
	script(t, bin, "git", `case " $* " in
 *" push "*)
  if [ -f '`+marker+`' ]; then
   rm '`+marker+`'
   '`+gitBinary+`' "$@" || exit $?
   echo 'fixture lost the push response' >&2
   exit 1
  fi ;;
esac
exec '`+gitBinary+`' "$@"`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil || p.PublishedHead != p.Head || p.State != Proposed {
		t.Fatalf("lost response was not adopted: %+v %v", p, err)
	}
	saved, _, err := s.Load(c.ID)
	if err != nil || saved.PublishedHead != p.Head || originHead(t, f, p.Branch) != p.Head {
		t.Fatal("publication receipt was not persisted")
	}
}

func TestRunRefusesAMountThatExposesItsHostDirectory(t *testing.T) {
	root := t.TempDir()
	a := Agent{Runner: []string{"true"}, Bedrock: &Bedrock{File: filepath.Join(root, "absent")},
		Mounts: []jail.Mount{{Path: root, Write: true}}}
	_, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(root, "run"), BudgetUSD: 1})
	if err == nil || !strings.Contains(err.Error(), "host-only directory") {
		t.Fatalf("unsafe mount was accepted: %v", err)
	}
}

func TestPackageReadsRefuseTraversal(t *testing.T) {
	s := Ship{Dir: t.TempDir()}
	for _, name := range []string{"", ".", "..", "../work/fixture", "/fixture"} {
		if _, _, err := s.Load(name); err == nil {
			t.Fatalf("package path %q accepted", name)
		}
	}
}

func TestFallbackComparesAggregateHarnessAndRelayCharges(t *testing.T) {
	first := Result{CostKnown: true, USD: .8, RelayUSD: .1}
	second := Result{CostKnown: true, USD: .1, RelayUSD: .8}
	got := finishFallback(first, second, .2)
	if math.Abs(got.USD-.9) > 1e-10 || math.Abs(got.RelayUSD-.9) > 1e-10 {
		t.Fatalf("summed per-attempt maxima instead of the two totals: %+v", got)
	}
	unknown := finishFallback(Result{CostKnown: true, USD: .1, RelayUSD: .7}, Result{CostKnown: false, RelayUSD: .2}, .3)
	if math.Abs(unknown.USD-1) > 1e-10 || !unknown.CostKnown {
		t.Fatalf("unknown fallback lost its reservation: %+v", unknown)
	}
}
