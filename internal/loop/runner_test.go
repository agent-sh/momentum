package loop

import (
	"context"
	"database/sql"

	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/agent-sh/momentum/internal/store"
)

// fakeAgent is a runner that ends with the result line given, as Claude Code's stream does, and keeps the
// arguments it was given in a file beside it.

// fakeAgent is a runner that ends with the result line given, as Claude Code's stream does, and keeps the
// arguments it was given in a file beside it.
func fakeAgent(t *testing.T, result string) []string {
	t.Helper()
	dir := t.TempDir()
	p := script(t, dir, "agent", `printf '%s\n' "$@" > "`+filepath.Join("/opt/fixture", "args")+`"
`+maskedEnv()+` > "`+filepath.Join("/opt/fixture", "env")+`"
echo '{"type":"system","subtype":"init"}'
`+fixtureResult(result))
	return []string{p}
}

func agentArgs(t *testing.T, runner []string) (string, string) {
	t.Helper()
	dir := filepath.Dir(runner[0])
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	return string(args), string(env)
}

func runnerFor(t *testing.T, st *store.Store, agent []string) *Runner {
	t.Helper()
	dir := shortScratch(t)
	home := t.TempDir()
	facts := inputRepo(t, t.TempDir(), "facts")
	binary := script(t, t.TempDir(), "momentum", "printf 'marker-7f3a record\\n'")
	rules := filepath.Join(dir, "rules.md")
	write(t, rules, "Delete with literal paths.")
	return &Runner{Store: st, Agent: testSandboxAgent(t, Agent{Runner: agent, Model: "model-x"}),
		Families: []string{"failures", "owner-burden"}, Budgets: Budgets{Run: 4, Case: 20, Day: 40, Week: 200, RunTime: time.Minute, MaxTurns: 30},
		Rules: []string{rules}, Constraints: []string{"never wake him"}, Dir: dir, Inputs: InputOptions{Momentum: binary, Home: home, TacitStore: facts}}
}

func openCase(t *testing.T, st *store.Store, family, sig string, n int) store.Case {
	t.Helper()
	f := store.Finding{Family: family, Signature: sig, Title: "case " + sig}
	for i := 0; i < n; i++ {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: fmt.Sprintf("file:/t/%s-%d", sig, i), At: time.Now().Add(-time.Duration(i+1) * time.Minute)})
	}
	c, _, err := st.ObserveCase(context.Background(), f, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A case whose last two runs failed the same way is not run a third time: the loop opens a case of its own, in
// component health, and goes on with the family's next case. A refusal of the budgets is no run of the case.
func TestALoopThatFailsTheSameWayTwiceStops(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":91,"total_cost_usd":1.5}`)
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 3)
	other := openCase(t, st, "failures", "b", 1)
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{"failures"}})
		if err != nil || done[0].Case != c.ID || done[0].Run.Status != store.RunTurns {
			t.Fatalf("run %d: %+v %v", i, done, err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || done[0].Case != other.ID {
		t.Fatalf("the third run worked %+v %v", done, err)
	}
	cs, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health"})
	if len(cs) != 1 || !strings.Contains(cs[0].Title, "turns") || cs[0].Evidence != 2 {
		t.Fatalf("the loop's own case %+v", cs)
	}
	// two refusals of the budgets stop nothing
	st2 := testStore(t)
	r2 := runnerFor(t, st2, agent)
	c2 := openCase(t, st2, "failures", "a", 3)
	for i := 0; i < 2; i++ {
		st2.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("r", i), Case: c2.ID, Family: "failures", Step: StepInvestigate,
			Started: time.Now().Add(-time.Duration(i+1) * time.Minute), Ended: time.Now(), Status: store.RunRefused, Failure: "the loop's day budget: $37.00 spent of $40"})
	}
	if got, _, why, _ := r2.pick(ctx, "failures", "", false); got.ID != c2.ID {
		t.Fatalf("two refusals stopped the case: %s", why)
	}
}

// No run starts when the day's spend, with the runs in flight counted at their whole budget, would pass the day's
// budget, and two families cannot both take its last room. A case whose budget has no room for a run's is passed
// over. A run whose cost is unknown counts its whole budget.
func TestTheBudgetsHoldTheRuns(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, okResult)
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 2)
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "r0", Case: "c0", Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-time.Hour),
		Ended: time.Now(), Status: store.RunOK, USD: 37}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || done[0].Run.Status != store.RunRefused || !strings.Contains(done[0].Skipped, "day budget") {
		t.Fatalf("over the day's budget: %+v %v", done, err)
	}
	if _, err := st.AddCaseCost(ctx, c.ID, store.Cost{USD: 17}, "test"); err != nil {
		t.Fatal(err)
	}
	if got, _, why, _ := r.pick(ctx, "failures", "", false); got.ID != "" || !strings.Contains(why, "spent $17.00") {
		t.Fatalf("a case with no room for a run picked: %s %s", got.ID, why)
	}
	// the day has room for one run: of three families, one starts
	st2 := testStore(t)
	r2 := runnerFor(t, st2, fakeAgent(t, okResult))
	r2.Families = []string{"failures", "owner-burden", "sessions"}
	r2.Budgets.Day = 5
	for _, fam := range r2.Families {
		openCase(t, st2, fam, fam+"/x", 1)
	}
	done, err = r2.Once(ctx, Options{})
	started := 0
	for _, d := range done {
		if d.Run.Status == store.RunOK {
			started++
		}
	}
	if err != nil || started != 1 {
		t.Fatalf("%d runs started on room for one: %+v %v", started, done, err)
	}
	// A missing result does not erase known relay accounting.
	st3 := testStore(t)
	r3 := runnerFor(t, st3, fakeAgent(t, `{"type":"system","subtype":"init"}`))
	c3 := openCase(t, st3, "failures", "a", 1)
	done, err = r3.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || done[0].Run.USD != fixtureCharge(1) {
		t.Fatalf("a run with no cost known: %+v %v", done, err)
	}
	if got, _ := st3.CaseByID(ctx, c3.ID); got.Cost.USD != fixtureCharge(1) {
		t.Fatalf("its case's cost %+v", got.Cost)
	}
}

// Harness subtypes are diagnostic claims, never host outcomes.
func TestHarnessSubtypeNeverSetsTheHostOutcome(t *testing.T) {
	for _, subtype := range []string{"success", "error_max_turns", "error_max_budget_usd", "error_during_execution"} {
		path := filepath.Join(t.TempDir(), "stream")
		write(t, path, `{"type":"result","subtype":"`+subtype+`","is_error":true,"total_cost_usd":1000,"structured_output":{"summary":"marker-7f3a"}}`)
		res, err := readStream(path)
		if err != nil || res.Status != store.RunFailed || res.Failure != "report" || res.USD != 0 || res.Harness.Subtype != subtype {
			t.Fatalf("stdout subtype installed an outcome: %+v %v", res, err)
		}
	}
}

// A run the model's safeguards refuse runs once more on the fallback model, and its result says which ran and
// what both cost.
func TestASafeguardsRefusalFallsBack(t *testing.T) {
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	p := script(t, dir, "agent", `case "$*" in *"--model model-a"*) `+fixtureResult(refused)+` ;; *) `+fixtureResult(okResult)+` ;; esac`)
	a := testSandboxAgent(t, Agent{Runner: []string{p}, Model: "model-a", Fallback: "model-b"})
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK || res.Model != "model-b" || res.USD != fixtureCharge(5) {
		t.Fatalf("%+v %v", res, err)
	}
}

// A run stopped at its time takes every process it started with it, those that ignore its TERM too.
func TestATimedOutRunLeavesNothingRunning(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join("/opt/fixture", "survived")
	p := script(t, dir, "agent", `trap '' TERM
(trap '' TERM; sleep 3; touch "`+marker+`") &
sleep 30`)
	a := testSandboxAgent(t, Agent{Runner: []string{p}, Model: "m"})
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Second})
	if err != nil || res.Status != store.RunTimeout {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Fatal("a process of the run outlived it")
	}
	// a leader that ends on its TERM leaves none of its group behind, those that ignore TERM neither
	marker2 := filepath.Join("/opt/fixture", "survived2")
	p2 := script(t, dir, "agent2", `(trap '' TERM; sleep 3; touch "`+marker2+`") &
sleep 30`)
	res, err = testSandboxAgent(t, Agent{Runner: []string{p2}, Model: "m"}).Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run2"), Schema: investigationSchema, MaxTurns: 3,
		BudgetUSD: 1, Timeout: time.Second})
	if err != nil || res.Status != store.RunTimeout {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "survived2")); err == nil {
		t.Fatal("a process of the run's group outlived its leader")
	}
}

// A run's processes end with it, those that left its process group and its session too: they share its sandbox's
// process namespace, which ends with the harness.
func TestARunsProcessesEndWithIt(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join("/opt/fixture", "survived")
	p := script(t, dir, "agent", `setsid sh -c 'sleep 3; touch "`+marker+`"' > /dev/null 2>&1 < /dev/null &
`+fixtureResult(okResult))
	a := testSandboxAgent(t, Agent{Runner: []string{p}, Model: "m"})
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3,
		BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Fatal("a process that left the run's group outlived it")
	}
}

// A dry run picks and says what it would run, and writes nothing: no run ended, no case of its own opened.
func TestADryRunWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "a", 3)
	for i := 0; i < 2; i++ {
		st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("f", i), Case: c.ID, Family: "failures", Step: StepInvestigate,
			Started: time.Now().Add(-time.Duration(i+1) * time.Minute), Ended: time.Now(), Status: store.RunTurns, Failure: "turns"})
	}
	oldCase := openCase(t, st, "sessions", "old", 1)
	st.PutLoopRun(ctx, store.LoopRun{ID: "old", Case: oldCase.ID, Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-5 * time.Hour), Status: store.RunRunning})
	done, err := r.Once(ctx, Options{Dry: true, Families: []string{"failures"}})
	if err != nil || len(done) != 1 || !strings.Contains(done[0].Skipped, "stopped") {
		t.Fatalf("dry %+v %v", done, err)
	}
	if cs, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health"}); len(cs) != 0 {
		t.Fatalf("a dry run opened %d cases", len(cs))
	}
	if run, _ := st.LoopRunByID(ctx, "old"); run.Status != store.RunRunning {
		t.Fatalf("a dry run ended a run: %+v", run)
	}
	db, _ := sql.Open("sqlite", "file:"+path)
	defer db.Close()
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM loop_runs`).Scan(&n)
	if n != 3 {
		t.Fatalf("a dry run recorded runs: %d", n)
	}
}

// A run killed with the process that ran it is ended on the next loop run, its whole budget counted; and a run the
// model's safeguards refuse runs on the fallback model, which the run records.
func TestAStaleRunCountsItsBudgetAndARefusalFallsBack(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	p := script(t, dir, "agent", `case "$*" in *"--model model-x"*) `+fixtureResult(refused)+` ;; *) `+fixtureResult(okResult)+` ;; esac`)
	r := runnerFor(t, st, []string{p})
	r.Agent.Fallback = "model-y"
	c := openCase(t, st, "failures", "a", 2)
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "stale", Case: c.ID, Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-3 * time.Hour),
		Status: store.RunRunning}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Case != c.ID || done[0].Run.Status != store.RunOK || done[0].Run.Model != "model-y" || done[0].Run.USD != fixtureCharge(5) {
		t.Fatalf("done %+v %v", done, err)
	}
	if run, _ := st.LoopRunByID(ctx, "stale"); run.Status != store.RunFailed || run.Failure != "killed" || run.USD != r.Budgets.Run {
		t.Fatalf("the stale run %+v", run)
	}
}

// A recent reservation, whoever of the loop's runners started it (one killed before stale recovery could end its
// run), blocks another run and holds its case's room until stale recovery ends it.
func TestARunningReservationHoldsItsCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "recent", 1)
	r.Budgets.Run = 4
	r.Budgets.Case = 4
	r.Budgets.Day = 20
	old := store.LoopRun{ID: "other-run", Case: c.ID, Family: c.Family, Step: StepInvestigate, Started: time.Now(), Status: store.RunRunning, USD: 4}
	if err := st.PutLoopRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	if spent, running, err := r.caseRoom(ctx, c); err != nil || spent != 4 || !running {
		t.Fatalf("room %v %v %v", spent, running, err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	runs, err := st.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("second run admitted: %d %v", len(runs), err)
	}
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.USD != 4 {
		t.Fatalf("recovery charge: %+v %v", got.Cost, err)
	}
	if c, _, _, err := r.pick(ctx, "failures", "", false); err != nil || c.ID != "" {
		t.Fatalf("spent case picked: %s %v", c.ID, err)
	}
}

// An evaluation's runs are its own: one left running (its process killed) is neither ended by the loop's stale
// recovery, whose cases they are not, nor counted against a live case, and the loop runs on.
func TestAnEvaluationsRunsAreNotTheLoops(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "live", 2)
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "eval-run", Case: "c-of-a-trial", Family: "failures", Step: StepInvestigate,
		Started: time.Now().Add(-3 * time.Hour), Status: store.RunRunning, USD: 4, Experiment: "eval:x:task:1"}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Case != c.ID || done[0].Run.Status != store.RunOK {
		t.Fatalf("the loop with an evaluation's run left running: %+v %v", done, err)
	}
	if run, err := st.LoopRunByID(ctx, "eval-run"); err != nil || run.Status != store.RunRunning {
		t.Fatalf("the evaluation's run: %+v %v", run, err)
	}
}

// Spare case and day budgets leave only the running hold to reject a pick. Publishing is held too,
// even when its case has spent its budget; ending the run admits each step again.
func TestARunningHoldIsIndependentOfBudgets(t *testing.T) {
	for _, step := range []string{StepInvestigate} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "m.db")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			r := runnerFor(t, st, nil)
			c := openCase(t, st, "failures", "held", 1)
			if got, next, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || got.ID != c.ID || next != step {
				t.Fatalf("control pick: %s %s %v", got.ID, next, err)
			}
			reservation, why := r.reserve(ctx, c.Family, c.ID, step)
			if why != "" {
				t.Fatal(why)
			}
			if why, err := r.affordable(ctx); err != nil || why != "" {
				t.Fatalf("fixture has no spare day budget: %s %v", why, err)
			}
			if step == StepInvestigate {
				if spent, _, err := r.caseRoom(ctx, c); err != nil || spent+r.Budgets.Run > r.Budgets.Case {
					t.Fatalf("fixture has no spare case budget: %v %v", spent, err)
				}
			}
			if got, _, why, err := r.pick(ctx, c.Family, c.ID, false); err != nil || got.ID != "" || !strings.Contains(why, "in flight") {
				t.Fatalf("running case admitted: %s %s %v", got.ID, why, err)
			}
			r.finish(ctx, c, &reservation, Result{Status: store.RunOK, CostKnown: true}, nil)
			if got, next, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || got.ID != c.ID || next != step {
				t.Fatalf("ended run holds case: %s %s %v", got.ID, next, err)
			}
		})
	}
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":0.75,"session_id":"s1","structured_output":{"mechanism":"lockfile read before lock","fix":"take the lock first","summary":"looked at it"},"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}`

// Two runs that failed differently do not stop a case; two that failed alike do.
func TestTheStopRuleComparesTheFailures(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "stop-rule", 2)
	put := func(id, failure string, started time.Duration) {
		t.Helper()
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: "failures", Step: StepInvestigate, Started: time.Now().Add(started),
			Ended: time.Now().Add(started + time.Second), Status: store.RunFailed, Failure: failure}); err != nil {
			t.Fatal(err)
		}
	}
	put("r1", "turns", -3*time.Minute)
	put("r2", "timeout", -2*time.Minute)
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("two different failures stopped the case: %q %v", why, err)
	}
	put("r3", "timeout", -time.Minute)
	if why, err := r.stopped(ctx, c, true); err != nil || !strings.Contains(why, "failed the same way: timeout") {
		t.Fatalf("two alike did not stop it: %q %v", why, err)
	}
}

func TestTheStopRuleSkipsTheRunsTheHostEnded(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "stop-rule-host", 2)
	put := func(id, failure string, started time.Duration) {
		t.Helper()
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: "failures", Step: StepInvestigate, Started: time.Now().Add(started),
			Ended: time.Now().Add(started + time.Second), Status: store.RunFailed, Failure: failure}); err != nil {
			t.Fatal(err)
		}
	}
	put("h1", "stopped", -6*time.Minute)
	put("h2", "stopped", -5*time.Minute)
	put("h3", "killed", -4*time.Minute)
	put("h4", "killed", -3*time.Minute)
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("runs the host ended stopped the case: %q %v", why, err)
	}
	put("h5", "timeout", -2*time.Minute)
	put("h6", "stopped", -90*time.Second)
	put("h7", "timeout", -time.Minute)
	if why, err := r.stopped(ctx, c, true); err != nil || !strings.Contains(why, "failed the same way: timeout") {
		t.Fatalf("two timeouts around a stop did not stop the case: %q %v", why, err)
	}
}
