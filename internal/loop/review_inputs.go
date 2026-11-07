package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const reviewSchema = `{"type":"object","additionalProperties":false,"required":["summary","findings","reviewed_files"],
"properties":{"summary":{"type":"string","minLength":1},"reviewed_files":{"type":"array","items":{"type":"string"},"uniqueItems":true},"findings":{"type":"array","items":{"type":"object",
"additionalProperties":false,"required":["severity","where","problem","fix"],
"properties":{"severity":{"type":"string","enum":["high","medium","low"]},"where":{"type":"string"},
"problem":{"type":"string"},"fix":{"type":"string"},"person_path":{"type":"string"}}}}}}`

type reviewReport struct {
	Summary       *string    `json:"summary"`
	Findings      *[]Finding `json:"findings"`
	ReviewedFiles *[]string  `json:"reviewed_files"`
}

func readReviewReport(raw []byte, changed []string) (reviewReport, error) {
	var rep reviewReport
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&rep) != nil || rep.Summary == nil || strings.TrimSpace(*rep.Summary) == "" || rep.Findings == nil || rep.ReviewedFiles == nil {
		return rep, fmt.Errorf("an unreadable review")
	}
	files := append([]string(nil), (*rep.ReviewedFiles)...)
	want := append([]string(nil), changed...)
	slices.Sort(files)
	slices.Sort(want)
	if len(slices.Compact(append([]string(nil), files...))) != len(files) || !slices.Equal(files, want) {
		return rep, fmt.Errorf("a review must name every changed file once")
	}
	return rep, nil
}

// freshReviewPackage contains only the claims a reviewer needs to check.
// Prior findings, command arguments, output tails and host paths stay host-side.
func freshReviewPackage(p Package) any {
	type result struct {
		BeforeExit int    `json:"before_exit"`
		AfterExit  int    `json:"after_exit"`
		Tree       string `json:"tree"`
	}
	var check *result
	if p.Check != nil {
		check = &result{p.Check.Before.Exit, p.Check.After.Exit, p.Check.Tree}
	}
	return struct {
		Title    string  `json:"title"`
		Body     string  `json:"body"`
		Rollback string  `json:"rollback"`
		Plan     *Plan   `json:"plan,omitempty"`
		Check    *result `json:"check,omitempty"`
	}{p.Title, p.Body, p.Rollback, p.Plan, check}
}

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

func (r *Runner) invokeReview(ctx context.Context, c store.Case, p Package, run store.LoopRun) (result Result, runErr error) {
	inv, manifest, err := r.preparedInvocation(run, reviewSchema)
	if err != nil {
		return Result{}, err
	}
	export, err := r.Ship.export(ctx, p, "review", p.Head)
	if err != nil {
		return Result{}, err
	}
	defer func() { runErr = errors.Join(runErr, r.Ship.unexport(p, export)) }()
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
