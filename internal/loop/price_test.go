package loop

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// pricedResult is a run's result line as the harness writes it on Bedrock: its price at the provider's list rates,
// and each model's tokens by kind. Sonnet 5.5's list price for a cache read is twice Bedrock's; Opus 5.5's rates are
// Bedrock's; a model with no Bedrock rate here keeps the harness's price for its part.
const pricedResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":4.49,"session_id":"s1",` +
	`"structured_output":{"mechanism":"the export reads the lockfile before it takes the lock","alternatives":["a slow disk"],"fix":"take the lock first","summary":"looked at it"},` +
	`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40},` +
	`"modelUsage":{` +
	`"global.anthropic.claude-sonnet-5-5":{"inputTokens":1000000,"outputTokens":100000,"cacheReadInputTokens":2000000,"cacheCreationInputTokens":300000,` +
	`"cacheCreationEphemeral5mInputTokens":200000,"cacheCreationEphemeral1hInputTokens":100000,"costUSD":4.3},` +
	`"global.anthropic.claude-opus-5-5":{"inputTokens":10000,"outputTokens":1000,"cacheReadInputTokens":100000,"cacheCreationInputTokens":20000,` +
	`"cacheCreationEphemeral5mInputTokens":20000,"cacheCreationEphemeral1hInputTokens":0,"costUSD":0.18},` +
	`"global.anthropic.claude-haiku-5-5":{"inputTokens":1000,"outputTokens":100,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.01}}}`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A run is priced at Bedrock's rates for the global profiles, model by model, from its tokens of each kind: a cache
// write for an hour at its own rate, a model with no rate at the harness's price; and its tokens are kept by kind.
func TestARunIsPricedAtBedrocksRates(t *testing.T) {
	a := Agent{Bedrock: testBedrock(t), Runner: fakeAgent(t, pricedResult), Model: "global.anthropic.claude-sonnet-5-5"}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 5, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Sonnet 5.5: 1M input at $2, 0.1M output at $10, 2M cache reads at $0.10, 0.2M five-minute writes at $2.50 and
	// 0.1M hour writes at $4 is $4.10; Opus 5.5: $0.04 + $0.02 + $0.02 + $0.10 = $0.18; Haiku keeps its $0.01
	if !near(res.USD, 4.29) || !res.CostKnown {
		t.Errorf("priced at $%v (the harness's list price is $4.49), want $4.29", res.USD)
	}
	want := store.Usage{Input: 1_011_000, Output: 101_100, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	if res.Usage != want || res.Tokens != want.Total() {
		t.Errorf("tokens %+v (%d), want %+v", res.Usage, res.Tokens, want)
	}
}

// Without each model's tokens, or with a model the harness gave no price for, the run keeps the harness's price; a
// regional profile has no Bedrock rate here, since the loop runs none.
func TestARunWithoutEveryModelsPriceKeepsTheHarnesss(t *testing.T) {
	for name, result := range map[string]string{
		"no model usage": okResult,
		"no price":       `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.75,"structured_output":{"summary":"s"},"modelUsage":{"global.anthropic.claude-haiku-5-5":{"inputTokens":5}}}`,
		"regional":       `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.75,"structured_output":{"summary":"s"},"modelUsage":{"us.anthropic.claude-sonnet-5-5":{"inputTokens":5,"costUSD":0.75}}}`,
	} {
		a := Agent{Bedrock: testBedrock(t), Runner: fakeAgent(t, result), Model: "model-x"}
		res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 5, Timeout: time.Minute})
		if err != nil || !near(res.USD, 0.75) {
			t.Errorf("%s: $%v %v", name, res.USD, err)
		}
	}
	if u, ok := bedrockPrice("us.anthropic.claude-opus-5-5", store.Usage{Input: 1}); ok || u != 0 {
		t.Errorf("a regional profile priced at the global rate: %v", u)
	}
}

// A run's record keeps its tokens by kind and its price at Bedrock's rates, and its case is charged that price.
func TestARunsRecordKeepsItsTokensByKind(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, pricedResult))
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) == 0 || done[0].Run.ID == "" {
		t.Fatalf("%+v %v", done, err)
	}
	run, err := st.LoopRunByID(ctx, done[0].Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Usage{Input: 1_011_000, Output: 101_100, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	if run.Usage != want || run.Tokens != want.Total() || !near(run.USD, 4.29) {
		t.Errorf("the record: %+v", run)
	}
	if got, _ := st.CaseByID(ctx, c.ID); !near(got.Cost.USD, 4.29) || got.Cost.Tokens != want.Total() {
		t.Errorf("the case was charged %+v", got.Cost)
	}
}

// A refused first attempt and its fallback's tokens add up, as their prices do.
func TestAFallbacksTokensAddToTheRefusedRunsTokens(t *testing.T) {
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it",` +
		`"modelUsage":{"global.anthropic.claude-opus-5-5":{"inputTokens":50000,"outputTokens":2500,"costUSD":0.25}}}`
	p := script(t, dir, "agent", `case "$*" in *"--model model-a"*) echo '`+refused+`' ;; *) echo '`+pricedResult+`' ;; esac`)
	a := Agent{Bedrock: testBedrock(t), Runner: []string{p}, Model: "model-a", Fallback: "model-b"}
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 9, BudgetUSD: 9, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("%+v %v", res, err)
	}
	want := store.Usage{Input: 1_061_000, Output: 103_600, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	if res.Usage != want || res.Tokens != want.Total() || !near(res.USD, 4.29+0.25) {
		t.Errorf("$%v %+v, want %+v", res.USD, res.Usage, want)
	}
}

// An evaluation's trials and judges are priced at Bedrock's rates too, and keep their tokens by kind: the trial's
// run in results.json, every judge's run in the store, and each task's and arm's tokens summed.
func TestAnEvaluationsRunsArePricedAtBedrocksRates(t *testing.T) {
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
	judged := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.11,"structured_output":{"verdict":"reached","facts":[],"reason":"r"},` +
		`"modelUsage":{"global.anthropic.claude-sonnet-5-5":{"inputTokens":20000,"outputTokens":1000,"cacheReadInputTokens":300000,"costUSD":0.11}}}`
	judge := script(t, dir, "judge", `echo '`+judged+`'`)
	rules := filepath.Join(dir, "rules.md")
	write(t, rules, "Delete with literal paths.")
	task := GoldenTask{ID: "t1", Family: "failures", CaseTitle: "a thing keeps failing", Cutoff: cutoff, KeyFacts: []string{"f"}}
	task.Symptom.Query.EventIDs = []string{e1.ID()}
	st := testStore(t)
	e := &Eval{Runner: &Runner{Store: st, Agent: Agent{Bedrock: testBedrock(t), Runner: fakeAgent(t, pricedResult), Model: "m"}, Budgets: Budgets{Run: 5, RunTime: time.Minute, MaxTurns: 9},
		Rules: []string{rules}, Constraints: []string{"never wake him"}}, Source: source, Dir: filepath.Join(dir, "out"), Trials: 1, Budget: 20,
		Parallel: 1, ID: "priced", Judge: Agent{Bedrock: testBedrock(t), Runner: []string{judge}, Model: "j"}, Native: true, Arms: []string{"native"}}
	trs, err := e.Run(ctx, []GoldenTask{task})
	if err != nil || len(trs) != 1 || trs[0].Err != "" || trs[0].Grade.Verdict != "reached" {
		t.Fatalf("%+v %v", trs, err)
	}
	want := store.Usage{Input: 1_011_000, Output: 101_100, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	// the judge: 20k input at $2, 1k output at $10, 300k cache reads at $0.10 is $0.08 (the harness says $0.11)
	if tr := trs[0]; tr.Run.Usage != want || !near(tr.Run.USD, 4.29) || !near(tr.JudgeUSD, 0.08) {
		t.Errorf("the trial: $%v %+v, judge $%v", tr.Run.USD, tr.Run.Usage, tr.JudgeUSD)
	}
	exp := trs[0].Run.Experiment
	runs, err := st.LoopRuns(ctx, store.RunFilter{Experiment: &exp})
	judges := 0
	for _, r := range runs {
		if r.Step == "judge" {
			judges++
			if r.Usage != (store.Usage{Input: 20000, Output: 1000, CacheRead: 300000}) || !near(r.USD, 0.08) {
				t.Errorf("the judge's run: %+v", r)
			}
		}
	}
	if err != nil || judges != 1 {
		t.Errorf("%d judges' runs: %v", judges, err)
	}
	if sums := Summarize(trs, 1); len(sums) != 1 || sums[0].Usage != want {
		t.Errorf("the task's tokens: %+v", sums)
	}
	if arms := e.Results(trs).Arms; len(arms) != 1 || arms[0].Usage != want {
		t.Errorf("the arm's tokens: %+v", arms)
	}
}
