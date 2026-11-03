package loop

import (
	"context"

	"fmt"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The week's spend holds the runs as the day's does: a run starts only when the week's spend by the live runs, those
// in flight at their whole budget, leaves room for its own, whatever room the day has.

// The week's spend holds the runs as the day's does: a run starts only when the week's spend by the live runs, those
// in flight at their whole budget, leaves room for its own, whatever room the day has.
func TestTheWeeksSpendHoldsTheRuns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	r.Budgets.Day, r.Budgets.Week = 100, 10
	openCase(t, st, "failures", "a", 2)
	// 7 dollars three days ago: the day is empty, and the week has room for 3 more
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "r0", Case: "c0", Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-72 * time.Hour),
		Ended: time.Now().Add(-71 * time.Hour), Status: store.RunOK, USD: 7}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunRefused || !strings.Contains(done[0].Skipped, "week budget") {
		t.Fatalf("over the week's budget: %+v %v", done, err)
	}
	// a week and a day later those 7 dollars are out of the week
	r.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if why, err := r.affordable(ctx); err != nil || why != "" {
		t.Fatalf("a week later: %q %v", why, err)
	}
}

// localRunner exercises selection and accounting without starting an agent or requiring a sandbox.
func localRunner(t *testing.T, st *store.Store) *Runner {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "loop")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return &Runner{Store: st, Dir: dir,
		Families: []string{"failures"}, Budgets: DefaultBudgets}
}

// A known primary result stays chargeable when a fallback's directory check prevents it from starting.
func TestAFallbackStartupErrorKeepsKnownAccounting(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "marker-7f3a", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	// This unit fixture supplies an observer's known accounting, not stream claims.
	primary := Result{Started: true, Status: store.RunFailed, Failure: "the model's safeguards", CostKnown: true, USD: .25, Tokens: 30, Turns: 1, Model: "primary"}
	primary.Model = "primary"
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("marker-7f3a", filepath.Join(run.Dir, "work")); err != nil {
		t.Fatal(err)
	}
	startErr := runDirectories(run.Dir)
	if startErr == nil {
		t.Fatal("fallback accepted the planted directory link")
	}
	r.finish(ctx, c, &run, primary, startErr)
	got, err := st.LoopRunByID(ctx, run.ID)
	if err != nil || got.Status != store.RunFailed || got.Failure != "run" || !strings.Contains(got.Summary, startErr.Error()) || got.USD != 0.25 ||
		got.Tokens != 30 || got.Turns != 1 || got.Model != "primary" {
		t.Errorf("known primary accounting was lost: %+v %v", got, err)
	}
	if got, err := st.CaseByID(ctx, c.ID); err != nil || got.Cost.USD != 0.25 || got.Cost.Tokens != 30 || got.Cost.Runs != 1 {
		t.Errorf("known primary accounting was lost on the case: %+v %v", got.Cost, err)
	}
}

// The real sandbox returns the primary's known spend alongside an error when its fallback cannot start.
func TestAFallbackThatNeverStartsChargesOnlyThePrimary(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	harness := script(t, t.TempDir(), "harness", `python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.bind("/work/planted.sock")'
`+fixtureResult(`{"type":"result","subtype":"success","is_error":true,"result":"the safeguards blocked it","total_cost_usd":0.25,"num_turns":1,"usage":{"input_tokens":10,"output_tokens":20}}`))
	r := runnerFor(t, st, []string{harness})
	r.Agent.Model, r.Agent.Fallback = "primary", "fallback"
	c := openCase(t, st, "failures", "marker-7f3a", 1)
	done, err := r.Once(ctx, Options{Families: []string{c.Family}})
	if err != nil || len(done) != 1 {
		t.Fatalf("fallback run: %+v %v", done, err)
	}
	run := done[0].Run
	if run.Status != store.RunFailed || run.Failure != "run" || !strings.Contains(run.Summary, "source directory holds a socket") ||
		run.USD != fixtureCharge(1) || run.Tokens != fixtureTokens(1) || run.Turns != 1 || run.Model != "primary" {
		t.Errorf("fallback startup lost known primary accounting: %+v", run)
	}
	if got, err := st.CaseByID(ctx, c.ID); err != nil || got.Cost.USD != fixtureCharge(1) || got.Cost.Tokens != fixtureTokens(1) || got.Cost.Runs != 1 {
		t.Errorf("fallback startup lost the case's charge: %+v %v", got.Cost, err)
	}
}

// A running reservation is stale only after its allowed duration, with no extra grace period.
func TestARunIsStaleOnlyPastTheTimeARunMayLast(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	now := time.Now().Truncate(time.Millisecond)
	r.Now = func() time.Time { return now }
	r.Budgets.RunTime = 30 * time.Minute
	for id, age := range map[string]time.Duration{
		"young": 29 * time.Minute, "boundary": 30 * time.Minute,
		"just stale": 30*time.Minute + time.Millisecond, "old": 40 * time.Minute,
	} {
		c := openCase(t, st, "failures", id, 1)
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: "failures", Step: StepInvestigate, Started: now.Add(-age),
			Status: store.RunRunning, USD: 4}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"young", "boundary"} {
		if got, err := st.LoopRunByID(ctx, id); err != nil || got.Status != store.RunRunning {
			t.Errorf("%s was ended within its duration: %+v %v", id, got, err)
		}
	}
	for _, id := range []string{"just stale", "old"} {
		if got, err := st.LoopRunByID(ctx, id); err != nil || got.Status != store.RunFailed || got.Failure != "killed" || got.USD != 4 {
			t.Errorf("%s was not ended and charged past its duration: %+v %v", id, got, err)
		}
	}
}

// The stop rule reads the last two runs of one step that ran: a build and a review that failed alike are two steps,
// not the same way twice.
func TestTheStopRuleReadsOneStepAtATime(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	for _, c := range []struct {
		name  string
		steps [2]string
		stop  bool
		gap   int
	}{{"mixed", [2]string{"fixture-other", StepInvestigate}, false, 0}, {"one step", [2]string{StepInvestigate, StepInvestigate}, true, 0},
		{"history gap", [2]string{StepInvestigate, StepInvestigate}, true, 20}} {
		cs := openCase(t, st, "failures", c.name, 1)
		now := time.Now()
		for i, step := range c.steps {
			if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint(c.name, i), Case: cs.ID, Family: "failures", Step: step,
				Started: now.Add(-time.Duration(3-i) * time.Minute), Ended: now, Status: store.RunTurns, Failure: "turns"}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < c.gap; i++ {
			if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint(c.name, "-refused-", i), Case: cs.ID, Family: "failures",
				Step: StepInvestigate, Started: now.Add(-3*time.Minute + time.Duration(i+1)*time.Second),
				Ended: now, Status: store.RunRefused, Failure: "day budget"}); err != nil {
				t.Fatal(err)
			}
		}
		why, err := r.stopped(ctx, cs, true)
		if err != nil || (why != "") != c.stop {
			t.Errorf("%s: stopped %q %v, want %v", c.name, why, err, c.stop)
		}
		if c.gap > 0 {
			got, _, why, err := r.pick(ctx, cs.Family, cs.ID, false)
			if err != nil || got.ID != "" || !strings.Contains(why, "stopped") {
				t.Errorf("history gap admitted a third run: %+v %q %v", got, why, err)
			}
		}
	}
}

// What a run of a case that went into another case costs is that case's: it counts in the room of the case that
// holds it, and holds it while it runs.
func TestAMergedCasesRunsCountInTheRoomOfTheCaseThatHoldsIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	a, b := openCase(t, st, "failures", "a", 1), openCase(t, st, "failures", "b", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCaseSignature(ctx, a.ID, "root:fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "merged", Case: a.ID, Family: "failures", Step: StepInvestigate, Started: time.Now(), Status: store.RunRunning, USD: 3}); err != nil {
		t.Fatal(err)
	}
	if spent, running, err := r.caseRoom(ctx, b); err != nil || !running || spent != 3 {
		t.Fatalf("the holder's room: %v %v %v", spent, running, err)
	}
}
