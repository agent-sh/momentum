package loop

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const reviewSchema = `{"type":"object","additionalProperties":false,"required":["summary","findings"],
"properties":{"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object",
"additionalProperties":false,"required":["severity","where","problem","fix"],
"properties":{"severity":{"type":"string","enum":["high","medium","low"]},"where":{"type":"string"},
"problem":{"type":"string"},"fix":{"type":"string"}}}}}}`

func (r *Runner) preparedInvocation(run store.LoopRun, schema string) (Invocation, InputsManifest, error) {
	system, err := readHostInput(HostDir(run.Dir), "input/system.md")
	if err != nil {
		return Invocation{}, InputsManifest{}, err
	}
	raw, err := readHostInput(HostDir(run.Dir), "prompt.json")
	if err != nil {
		return Invocation{}, InputsManifest{}, err
	}
	var prompt string
	if err := json.Unmarshal(raw, &prompt); err != nil {
		return Invocation{}, InputsManifest{}, err
	}
	binds, manifest, err := r.runInputs(run)
	return Invocation{ID: run.ID, Dir: run.Dir, System: string(system), Prompt: prompt, Schema: schema,
		MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run, Timeout: r.Budgets.RunTime, Binds: binds}, manifest, err
}

func (r *Runner) invokeReview(ctx context.Context, c store.Case, p Package, run store.LoopRun) (Result, error) {
	inv, manifest, err := r.preparedInvocation(run, reviewSchema)
	if err != nil {
		return Result{}, err
	}
	export, err := r.Ship.export(ctx, p, "review", p.Head)
	if err != nil {
		return Result{}, err
	}
	defer r.Ship.unexport(p, export)
	// This plain export belongs to the host and is stable for both attempts.
	inv.Binds = append(inv.Binds, sandbox.Bind{Src: export, Dst: "/ro/change"})
	res, err := r.Agent.RunOrFallback(ctx, inv)
	res.InputSummary = manifest.Summary()
	return res, err
}

// reviewInputPath is kept host-side. A review receives only its read-only mount.
func reviewInputPath(run store.LoopRun) string {
	return filepath.Join(HostDir(run.Dir), "input", "change.patch")
}
