package loop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func pricedRun(t *testing.T, a Agent) Result {
	t.Helper()
	res, _ := a.Run(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 5, Timeout: time.Minute})
	return res
}

// The harness's own prices check Bedrock's: a model whose tokens price outside half to all of its list price (its
// tokens unread, say) keeps the harness's price, and a run whose models' prices do not add up to the run's keeps
// the harness's price whole, so a short or unreadable usage never lowers what a run counts.
func TestAModelUsageThatDoesNotAddUpKeepsTheHarnesssPrice(t *testing.T) {
	for name, tc := range map[string]struct {
		result string
		want   float64
	}{
		"tokens it cannot read": {`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":3,"structured_output":{"summary":"s"},` +
			`"modelUsage":{"global.anthropic.claude-opus-5-5":{"input_tokens":500000,"output_tokens":50000,"costUSD":3}}}`, 3},
		"a model it does not list": {`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":4.49,"structured_output":{"summary":"s"},` +
			`"modelUsage":{"global.anthropic.claude-sonnet-5-5":{"inputTokens":1000000,"outputTokens":100000,"cacheReadInputTokens":2000000,` +
			`"cacheCreationInputTokens":300000,"cacheCreationEphemeral1hInputTokens":100000,"costUSD":4.3}}}`, 4.49},
		"the 1M-context name": {`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":4.3,"structured_output":{"summary":"s"},` +
			`"modelUsage":{"global.anthropic.claude-sonnet-5-5[1m]":{"inputTokens":1000000,"outputTokens":100000,"cacheReadInputTokens":2000000,` +
			`"cacheCreationInputTokens":300000,"cacheCreationEphemeral1hInputTokens":100000,"costUSD":4.3}}}`, 4.1},
	} {
		res := pricedRun(t, Agent{Bedrock: testBedrock(t), Runner: fakeAgent(t, tc.result), Model: "m"})
		if !near(res.USD, tc.want) || !res.CostKnown {
			t.Errorf("%s: $%v, want $%v", name, res.USD, tc.want)
		}
	}
}

// A run that was not on Bedrock is charged what its stream says it spent, the harness's price: its tokens went by
// another route, at that route's prices. A live run that failed with its cost known keeps its tokens by kind.
func TestARunNotOnBedrockIsChargedTheHarnesssPrice(t *testing.T) {
	if res := pricedRun(t, Agent{Bedrock: testBedrock(t), Runner: unrouted(t, pricedResult), Model: "m"}); res.Failure != "not on Bedrock" || !near(res.USD, 4.49) {
		t.Fatalf("%s: $%v", res.Failure, res.USD)
	}
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, unrouted(t, pricedResult))
	openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.ID == "" {
		t.Fatalf("%+v %v", done, err)
	}
	run, err := st.LoopRunByID(ctx, done[0].Run.ID)
	want := store.Usage{Input: 1_011_000, Output: 101_100, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	if err != nil || run.Status != store.RunFailed || !near(run.USD, 4.49) || run.Usage != want {
		t.Fatalf("the record of a run not on Bedrock: %+v %v", run, err)
	}
}

// A stream with no per-model usage keeps its tokens by kind from the result's usage, its hour writes among them.
func TestAStreamWithoutModelUsageKeepsItsTokensByKind(t *testing.T) {
	result := `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.5,"structured_output":{"summary":"s"},` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":15,"ephemeral_1h_input_tokens":25}}}`
	res := pricedRun(t, Agent{Bedrock: testBedrock(t), Runner: fakeAgent(t, result), Model: "m"})
	if want := (store.Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, CacheWrite1h: 25}); res.Usage != want || res.Tokens != 100 || !near(res.USD, 0.5) {
		t.Fatalf("$%v %+v (%d)", res.USD, res.Usage, res.Tokens)
	}
}

// An evaluation's trial that ends in an error keeps its tokens by kind with its price, as a live run does, in the
// trial and in the store.
func TestATrialThatFailedKeepsItsTokens(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	src, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	e1 := store.Event{Source: store.Transcripts, SourceKey: "e1", TS: cutoff.Add(-time.Hour), Session: "s1", Harness: "claude", Actor: store.ActorTool,
		Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text"}
	if _, err := src.Put(ctx, []store.Event{e1}); err != nil {
		t.Fatal(err)
	}
	src.Close()
	rules := filepath.Join(dir, "rules.md")
	write(t, rules, "Delete with literal paths.")
	task := GoldenTask{ID: "t1", Family: "failures", CaseTitle: "a thing keeps failing", Cutoff: cutoff, KeyFacts: []string{"f"}}
	task.Symptom.Query.EventIDs = []string{e1.ID()}
	st := testStore(t)
	judge := script(t, dir, "judge", `echo '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.05,"structured_output":{"verdict":"partial","facts":[],"reason":"r"}}'`)
	e := &Eval{Runner: &Runner{Store: st, Agent: Agent{Bedrock: testBedrock(t), Runner: unrouted(t, pricedResult), Model: "m"}, Budgets: Budgets{Run: 5, RunTime: time.Minute, MaxTurns: 9},
		Rules: []string{rules}, Constraints: []string{"never wake him"}}, Source: source, Dir: filepath.Join(dir, "out"), Trials: 1, Budget: 20,
		Parallel: 1, ID: "failed", Judge: Agent{Bedrock: testBedrock(t), Runner: []string{judge}, Model: "j"}, Native: true, Arms: []string{"native"}}
	trs, err := e.Run(ctx, []GoldenTask{task})
	if err != nil || len(trs) != 1 || !strings.Contains(trs[0].Err, "Bedrock") {
		t.Fatalf("%+v %v", trs, err)
	}
	want := store.Usage{Input: 1_011_000, Output: 101_100, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	if tr := trs[0]; tr.Run.Usage != want || !near(tr.Run.USD, 4.49) {
		t.Errorf("the trial: $%v %+v", tr.Run.USD, tr.Run.Usage)
	}
	if run, err := st.LoopRunByID(ctx, trs[0].Run.ID); err != nil || run.Usage != want {
		t.Errorf("the trial's stored run: %+v %v", run, err)
	}
}
