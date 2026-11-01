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

// fakeAgent is a runner that ends with the result line given, as Claude Code's stream does, and keeps the
// arguments it was given in a file beside it.
func fakeAgent(t *testing.T, result string) []string {
	t.Helper()
	dir := t.TempDir()
	p := script(t, dir, "agent", `printf '%s\n' "$@" > "`+filepath.Join(dir, "args")+`"
env > "`+filepath.Join(dir, "env")+`"
echo '{"type":"system","subtype":"init"}'
echo '`+result+`'`)
	return []string{p}
}

func agentArgs(t *testing.T, runner []string) (string, string) {
	t.Helper()
	dir := filepath.Dir(runner[0])
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	return string(args), string(env)
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":0.75,"session_id":"s1",` +
	`"structured_output":{"case":"x","step":"investigate","outcome":"investigated","summary":"looked at it"},` +
	`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}`

func runnerFor(t *testing.T, st *store.Store, agent []string) *Runner {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.md")
	write(t, rules, "Delete with literal paths.")
	return &Runner{Store: st, Agent: Agent{Runner: agent, Model: "model-x"}, Ship: &Ship{Store: st, Dir: filepath.Join(dir, "loop")},
		Families: []string{"failures", "owner-burden"}, Budgets: Budgets{Run: 4, Case: 20, Day: 40, Week: 200, RunTime: time.Minute, MaxTurns: 30},
		Rules: []string{rules}, Constraints: []string{"never wake him"}, Dir: filepath.Join(dir, "loop"), Home: dir}
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

// A run takes each family's case one step: the one with the most evidence, in testing before investigating before
// open. The agent runs headless with the family's skill and the machine's rules in its system prompt, refused
// what no rule allows, with nothing of the caller's environment, and its cost goes on the case and the run.
func TestTheLoopTakesEachFamilysCaseOneStep(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, okResult)
	r := runnerFor(t, st, agent)
	small := openCase(t, st, "failures", "a", 1)
	big := openCase(t, st, "failures", "b", 3)
	openCase(t, st, "sessions", "c", 5) // a family the loop does not run here
	t.Setenv("CLAUDE_CODE_PLUGIN_DIRS", "/should/not/pass")
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0].Case != big.ID || done[0].Step != StepInvestigate || done[0].Run.Status != store.RunOK || done[0].Run.USD != 0.75 {
		t.Fatalf("done %+v", done)
	}
	args, env := agentArgs(t, agent)
	for _, want := range []string{"--bare", "--permission-mode\ndontAsk", "--model\nmodel-x", "--max-budget-usd\n4.00", "--max-turns\n30",
		"--strict-mcp-config", "--append-system-prompt-file"} {
		if !strings.Contains(args, want) {
			t.Errorf("the agent's arguments lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(env, "CLAUDE_CODE_PLUGIN_DIRS") || !strings.Contains(env, "MOMENTUM_LOOP_CASE="+big.ID) {
		t.Errorf("the agent's environment:\n%s", env)
	}
	system, _ := os.ReadFile(filepath.Join(done[0].Run.Dir, "system.md"))
	for _, want := range []string{"# One run on one case", "# Failures", "never wake him", "Delete with literal paths."} {
		if !strings.Contains(string(system), want) {
			t.Errorf("the system prompt lacks %q", want)
		}
	}
	if strings.Contains(string(system), "# Owner burden") {
		t.Error("the system prompt holds another family's skill")
	}
	settings, _ := os.ReadFile(filepath.Join(done[0].Run.Dir, "settings.json"))
	if !strings.Contains(string(settings), `"Bash(momentum case *)"`) || strings.Contains(string(settings), "momentum ship propose") {
		t.Errorf("an investigation's permissions: %s", settings)
	}
	if got, _ := st.CaseByID(ctx, big.ID); got.Cost.USD != 0.75 || got.Cost.Runs != 1 || got.Cost.Tokens != 100 {
		t.Fatalf("the case's cost %+v", got.Cost)
	}
	runs, _ := st.LoopRuns(ctx, store.RunFilter{})
	if len(runs) != 1 || runs[0].Case != big.ID || runs[0].Summary != "investigated: looked at it" || runs[0].Turns != 4 {
		t.Fatalf("runs %+v", runs)
	}
	if _, err := st.UpdateCase(ctx, small.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "test"); err != nil {
		t.Fatal(err)
	}
	c, step, _, _ := r.pick(ctx, "failures", "")
	if c.ID != small.ID || step != StepInvestigate {
		t.Fatalf("an investigating case comes before an open one: %s %s", c.ID, step)
	}
}

// A case whose last two runs failed the same way is not run a third time: the loop opens a case of its own, in
// component health, and goes on with the family's next case.
func TestALoopThatFailsTheSameWayTwiceStops(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1.5}`)
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
}

// No run starts when the day's spend, with the runs in flight counted at their whole budget, would pass the day's
// budget; a case over its own budget is passed over.
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
	if _, err := st.AddCaseCost(ctx, c.ID, store.Cost{USD: 25}, "test"); err != nil {
		t.Fatal(err)
	}
	if got, _, why, _ := r.pick(ctx, "failures", ""); got.ID != "" || !strings.Contains(why, "spent $25.00") {
		t.Fatalf("a case over its budget picked: %s %s", got.ID, why)
	}
}

// How a run ended comes from its stream's result: ok with its report, its limit, or failed by its kind.
func TestARunsResultSaysHowItEnded(t *testing.T) {
	for _, c := range []struct {
		subtype           string
		isError, reported bool
		status, failure   string
	}{
		{"success", false, true, store.RunOK, ""},
		{"success", false, false, store.RunFailed, "no report"},
		{"error_max_turns", true, false, store.RunTurns, "turns"},
		{"error_max_budget_usd", true, false, store.RunBudget, "budget"},
		{"success", true, false, store.RunFailed, "error"},
		{"error_during_execution", true, false, store.RunFailed, "error_during_execution"},
	} {
		st, f := resultStatus(c.subtype, c.isError, c.reported)
		if st != c.status || f != c.failure {
			t.Errorf("%s %v %v: %s %q, want %s %q", c.subtype, c.isError, c.reported, st, f, c.status, c.failure)
		}
	}
}
