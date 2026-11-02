package loop

import (
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// Usage is a run's tokens by kind (store.Usage).
type Usage = store.Usage

// rate is a model's price on Bedrock's global cross-region inference, in US dollars per million tokens of each kind.
type rate struct {
	input, output, cacheRead, cacheWrite5m, cacheWrite1h float64
}

// bedrockRates are Bedrock's on-demand prices for the global inference profiles (global.anthropic.MODEL) of the
// models the loop runs, from the Amazon Bedrock pricing page (Anthropic, global cross-region inference) as read on
// 2026-11-02. A geographic or in-region profile costs 10% more, and the loop runs none. The harness prices a run at
// the provider's list prices (its stream's costBasis is "list"), which for Sonnet 5.5's cache reads are twice
// Bedrock's.
var bedrockRates = map[string]rate{
	"claude-opus-5-5":   {input: 4, output: 20, cacheRead: 0.2, cacheWrite5m: 5, cacheWrite1h: 8},
	"claude-sonnet-5-5": {input: 2, output: 10, cacheRead: 0.1, cacheWrite5m: 2.5, cacheWrite1h: 4},
}

// bedrockPrice is what a global inference profile's usage costs at Bedrock's rates. ok is false for a model with no
// rate here, or one that is not a global profile: the run keeps the harness's own price for that model's part.
func bedrockPrice(model string, u Usage) (usd float64, ok bool) {
	name, global := strings.CutPrefix(model, "global.anthropic.")
	if !global {
		return 0, false
	}
	name = strings.TrimSuffix(name, "[1m]") // the 1M-context name: Bedrock prices it as the model
	r, ok := bedrockRates[name]
	if !ok {
		return 0, false
	}
	write5m := u.CacheWrite - u.CacheWrite1h
	return (float64(u.Input)*r.input + float64(u.Output)*r.output + float64(u.CacheRead)*r.cacheRead +
		float64(write5m)*r.cacheWrite5m + float64(u.CacheWrite1h)*r.cacheWrite1h) / 1e6, true
}
