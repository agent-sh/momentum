package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// WeeklyUnitPrefix names the units of the weekly observation's model pass. Its sweep of leftovers stops only units
// that begin with it.
var WeeklyUnitPrefix = "momentum-run-weekly-"

// Advice is one model pass with no tools over a prompt alone, as the evaluation's judge runs: the agent's sandbox,
// its Bedrock relay and its accounting, a report by the schema. Its run is kept among the loop's runs under its step
// and family with no case, so the loop's budgets count what it spent.
type Advice struct {
	Agent          Agent
	Store          *store.Store
	Dir            string // the runs' directory; each pass takes a directory of its own in it
	System, Prompt string
	Schema         string
	BudgetUSD      float64
	MaxTurns       int
	Timeout        time.Duration
	Family, Step   string
}

// Run runs the pass once, then once more on the fallback model when the model's safeguards refused it, and returns
// its report and its kept run.
func (a Advice) Run(ctx context.Context) (json.RawMessage, store.LoopRun, error) {
	if err := sandbox.StopLeftover(ctx, WeeklyUnitPrefix); err != nil {
		return nil, store.LoopRun{}, err
	}
	id := newRunID(time.Now())
	dir := filepath.Join(a.Dir, id)
	inv := Invocation{ID: id, Dir: dir, System: a.System, Schema: a.Schema, MaxTurns: a.MaxTurns, BudgetUSD: a.BudgetUSD,
		Timeout: a.Timeout, Prompt: a.Prompt, Tools: "none", unitPrefix: WeeklyUnitPrefix, inputOnly: true}
	if err := hostDir(HostDir(dir)); err != nil {
		return nil, store.LoopRun{}, err
	}
	if err := writeExclusive(HostDir(dir), "prompt.txt", []byte(a.Prompt)); err != nil {
		return nil, store.LoopRun{}, err
	}
	agent := a.Agent
	agent.Env, agent.Binds = nil, nil
	start := time.Now()
	res, err := agent.RunOrFallback(ctx, inv)
	run := store.LoopRun{ID: id, Family: a.Family, Step: a.Step, Started: start, Model: res.Model, Dir: dir}
	run, _ = settleRun(run, res, err, a.BudgetUSD)
	if recordErr := a.Store.PutLoopRun(context.WithoutCancel(ctx), run); recordErr != nil {
		return nil, run, recordErr
	}
	if err != nil {
		return nil, run, err
	}
	if run.Status != store.RunOK || len(res.Report) == 0 {
		return nil, run, fmt.Errorf("no report: %s %s", run.Status, run.Failure)
	}
	return res.Report, run, nil
}

// AdviceBudgetUSD is a pass's budget: a tool-free answer over one prompt, priced as the evaluation's judge is.
const AdviceBudgetUSD = judgeBudget
