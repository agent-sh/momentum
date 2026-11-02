package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// loopEval runs the golden-set evaluation: each task's investigation, as of the moment its case could have opened, several
// times, graded by a clean-context judge against its verified root cause.
func loopEval(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("loop eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	golden := fs.String("golden", filepath.Join(paths.Data(), "golden", "golden.jsonl"), "the golden set, one task per line")
	trials := fs.Int("trials", 3, "runs of each task")
	tasks := fs.String("task", "", "comma-separated task ids (default: every task)")
	budget := fs.Float64("budget", 120, "the most the evaluation spends, in US dollars")
	parallel := fs.Int("parallel", 3, "trials at once")
	arms := fs.String("arms", "loop,native", "arms to compare: loop,native, or one alone")
	asJSON := fs.Bool("json", false, "one JSON object per trial, then one per task")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *trials < 1 || *parallel < 1 || *budget < 0 || math.IsNaN(*budget) || math.IsInf(*budget, 0) {
		return usageError{"trials and parallel must be positive; budget must not be negative"}
	}
	selected := strings.Split(*arms, ",")
	seen := map[string]bool{}
	for _, a := range selected {
		if (a != "loop" && a != "native") || seen[a] {
			return usageError{"arms must be loop,native, or one alone"}
		}
		seen[a] = true
	}
	if os.Getenv("MOMENTUM_LOOP_RUN") != "" {
		return usageError{"an evaluation does not run inside one of the loop's runs"}
	}
	all, err := loop.ReadGolden(*golden)
	if err != nil {
		return usageError{err.Error()}
	}
	var picked []loop.GoldenTask
	want := map[string]bool{}
	for _, t := range strings.Split(*tasks, ",") {
		if t = strings.TrimSpace(t); t != "" {
			want[t] = true
		}
	}
	allTasks, unknown := len(want) == 0, maps.Clone(want)
	for _, t := range all {
		if allTasks || want[t.ID] {
			picked = append(picked, t)
			delete(unknown, t.ID)
		}
	}
	for t := range unknown {
		return usageError{fmt.Sprintf("no task %q in %s", t, *golden)}
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	r, _, err := setup(st, c.Loop)
	if err != nil {
		return err
	}
	id := loop.NewEvalID()
	raw, err := os.ReadFile(*golden)
	if err != nil {
		return err
	}
	var repositories []string
	for _, t := range all {
		if t.RepoPath != "" {
			repositories = append(repositories, t.RepoPath)
		}
	}
	e := &loop.Eval{Runner: r, Source: paths.DB(), Dir: filepath.Join(loopDir(), "eval", id), Trials: *trials, Budget: *budget, Parallel: *parallel, ID: id,
		Judge: loop.Agent{Runner: r.Agent.Runner, Model: r.Agent.Model}, Arms: selected, Repositories: repositories, GoldenHash: fmt.Sprintf("%x", sha256.Sum256(raw))}
	fmt.Fprintf(stderr, "evaluation %s: %d tasks, %d trials per arm, in %s\n", id, len(picked), *trials, e.Dir)
	trs, err := e.Run(ctx, picked)
	if err != nil {
		return err
	}
	results := e.Results(trs)
	if err := e.WriteResults(results); err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(results)
	}
	printEvaluation(stdout, results)
	return nil
}

// printEvaluation uses the same counts and rates that results.json records.
func printEvaluation(out io.Writer, r loop.EvaluationResults) {
	rate := func(x *float64) string {
		if x == nil {
			return "undefined"
		}
		return fmt.Sprintf("%.3f", *x)
	}
	for _, a := range r.Arms {
		fmt.Fprintf(out, "%s: %d/%d graded trials reached; %d partial, %d missed, %d wrong, %d errors, %d skipped, %d runs cut short; %d/%d ran\n", a.Arm, a.Reached, a.Graded, a.Partial, a.Missed, a.Wrong, a.Errors, a.Skipped, a.CutShort, a.Ran, a.Planned)
		fmt.Fprintf(out, "  cost $%.2f; time %.2f min; per trial $%s, %s min; per root cause reached $%s, %s min; judge $%.2f\n", a.USD, a.Minutes, rate(a.USDPerTrial), rate(a.MinutesPerTrial), rate(a.USDPerReached), rate(a.MinutesPerReached), a.JudgeUSD)
		fmt.Fprintf(out, "  k=%d: pass@k %.3f, pass^k %.3f; %d tasks flipped\n", a.K, a.PassAtK, a.PassAllK, a.Flips)
	}
	for _, t := range r.Comparison.Tasks {
		fmt.Fprintf(out, "%s: loop %d, native %d reached; more often: %s\n", t.Task, t.LoopReached, t.NativeReached, t.MoreOften)
	}
	interval := func(x *loop.Interval) string {
		if x == nil {
			return "undefined"
		}
		return fmt.Sprintf("[%.4f, %.4f]", x.Low, x.High)
	}
	fmt.Fprintf(out, "loop minus native, task bootstrap 90%% interval (%d resamples, seed %d): reached per dollar %s; reached per minute %s\n", r.Comparison.Resamples, r.Comparison.Seed, interval(r.Comparison.ReachedPerDollar), interval(r.Comparison.ReachedPerMinute))
}
