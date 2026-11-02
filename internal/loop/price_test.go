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

// pricedUsage is the tokens of pricedResult, summed over its models.
var pricedUsage = store.Usage{Input: 1_011_000, Output: 101_100, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// pricedRun runs a fake agent in its sandbox and returns what it came to.
func pricedRun(t *testing.T, a Agent) Result {
	t.Helper()
	res, err := testJail(t, a).Run(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 5, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A run is priced at Bedrock's rates for the global profiles, model by model, from its tokens of each kind: a cache
// write for an hour at its own rate, a model with no rate at the harness's price; and its tokens are kept by kind.
func TestARunIsPricedAtBedrocksRates(t *testing.T) {
	res := pricedRun(t, Agent{Runner: fakeAgent(t, pricedResult), Model: "global.anthropic.claude-sonnet-5-5"})
	// Sonnet 5.5: 1M input at $2, 0.1M output at $10, 2M cache reads at $0.10, 0.2M five-minute writes at $2.50 and
	// 0.1M hour writes at $4 is $4.10; Opus 5.5: $0.04 + $0.02 + $0.02 + $0.10 = $0.18; Haiku keeps its $0.01
	if !near(res.USD, 4.29) || !res.CostKnown {
		t.Errorf("priced at $%v (the harness's list price is $4.49), want $4.29", res.USD)
	}
	if res.Usage != pricedUsage || res.Tokens != pricedUsage.Total() {
		t.Errorf("tokens %+v (%d), want %+v", res.Usage, res.Tokens, pricedUsage)
	}
}

// Bedrock's rates are the table of its pricing page, kind by kind, for each model the loop runs (the harness's
// own check keeps a price that falls outside half to all of its list price, so a run's price alone would not show a
// wrong rate): a million tokens of each kind, the cache writes split between five minutes and an hour.
func TestBedrocksRatesAreTheTableOfItsPricingPage(t *testing.T) {
	for model, rates := range map[string][5]float64{
		"global.anthropic.claude-opus-5-5":   {4, 20, 0.2, 5, 8},
		"global.anthropic.claude-sonnet-5-5": {2, 10, 0.1, 2.5, 4},
	} {
		for i, usage := range []store.Usage{{Input: 1e6}, {Output: 1e6}, {CacheRead: 1e6}, {CacheWrite: 1e6}, {CacheWrite: 1e6, CacheWrite1h: 1e6}} {
			if got, ok := bedrockPrice(model, usage); !ok || math.Abs(got-rates[i]) > 1e-9 {
				t.Errorf("%s token kind %d: $%v (priced %v), want $%v", model, i, got, ok, rates[i])
			}
		}
	}
	if _, ok := bedrockPrice("us.anthropic.claude-opus-5-5", store.Usage{Input: 1e6}); ok {
		t.Error("a regional profile was priced at the global rates")
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
		if res := pricedRun(t, Agent{Runner: fakeAgent(t, result), Model: "model-x"}); !near(res.USD, 0.75) {
			t.Errorf("%s: $%v", name, res.USD)
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
	if run.Usage != pricedUsage || run.Tokens != pricedUsage.Total() || !near(run.USD, 4.29) {
		t.Errorf("the record: %+v", run)
	}
	if got, _ := st.CaseByID(ctx, c.ID); !near(got.Cost.USD, 4.29) || got.Cost.Tokens != pricedUsage.Total() {
		t.Errorf("the case was charged %+v", got.Cost)
	}
}

// A refused first attempt and its fallback's tokens add up, as their prices do.
func TestAFallbacksTokensAddToTheRefusedRunsTokens(t *testing.T) {
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it",` +
		`"modelUsage":{"global.anthropic.claude-opus-5-5":{"inputTokens":50000,"outputTokens":2500,"costUSD":0.25}}}`
	p := script(t, dir, "agent", `case "$*" in *"--model model-a"*) `+modelResult(refused)+` ;; *) `+modelResult(pricedResult)+` ;; esac`)
	a := testJail(t, Agent{Runner: []string{p}, Model: "model-a", Fallback: "model-b"})
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: runSchema, MaxTurns: 9, BudgetUSD: 9, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("%+v %v", res, err)
	}
	want := store.Usage{Input: 1_061_000, Output: 103_600, CacheRead: 2_100_000, CacheWrite: 320_000, CacheWrite1h: 100_000}
	if res.Usage != want || res.Tokens != want.Total() || !near(res.USD, 4.29+0.25) {
		t.Errorf("$%v %+v, want %+v", res.USD, res.Usage, want)
	}
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
		if res := pricedRun(t, Agent{Runner: fakeAgent(t, tc.result), Model: "m"}); !near(res.USD, tc.want) || !res.CostKnown {
			t.Errorf("%s: $%v, want $%v", name, res.USD, tc.want)
		}
	}
}

// A stream with no per-model usage keeps its tokens by kind from the result's usage, its hour writes among them.
func TestAStreamWithoutModelUsageKeepsItsTokensByKind(t *testing.T) {
	result := `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.5,"structured_output":{"summary":"s"},` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":15,"ephemeral_1h_input_tokens":25}}}`
	res := pricedRun(t, Agent{Runner: fakeAgent(t, result), Model: "m"})
	if want := (store.Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, CacheWrite1h: 25}); res.Usage != want || res.Tokens != 100 || !near(res.USD, 0.5) {
		t.Fatalf("$%v %+v (%d)", res.USD, res.Usage, res.Tokens)
	}
}
