package main

import (
	"testing"

	"github.com/agent-sh/momentum/internal/loop"
)

// The loop refuses to start with a model the relay cannot price, so its own defaults must have rates.
func TestLoopDefaultModelsArePriced(t *testing.T) {
	for _, m := range []string{defaultModel, defaultFallback} {
		if !loop.RelayPriced(m) {
			t.Errorf("default model %s has no relay rate", m)
		}
	}
}
