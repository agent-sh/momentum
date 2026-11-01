package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// loopEval runs the golden-set evaluation: each task's investigation, as of the moment before its fix, several
// times, graded by a clean-context judge against its verified root cause.
func loopEval(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("loop eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	golden := fs.String("golden", filepath.Join(paths.Data(), "golden", "golden.jsonl"), "the golden set, one task per line")
	trials := fs.Int("trials", 3, "runs of each task")
	tasks := fs.String("task", "", "comma-separated task ids (default: every task)")
	budget := fs.Float64("budget", 120, "the most the evaluation spends, in US dollars")
	parallel := fs.Int("parallel", 3, "trials at once")
	native := fs.Bool("native", false, "run the native path instead: the harness's agent with the same evidence and limits, without the loop's procedure, skill or case")
	asJSON := fs.Bool("json", false, "one JSON object per trial, then one per task")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
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
	id := time.Now().Format("20060102-1504")
	if *native {
		id += "-native"
	}
	e := &loop.Eval{Runner: r, Source: paths.DB(), Dir: filepath.Join(loopDir(), "eval", id), Trials: *trials, Budget: *budget,
		Parallel: *parallel, ID: id, Judge: loop.Agent{Runner: r.Agent.Runner, Model: r.Agent.Model, Fallback: r.Agent.Fallback}, Native: *native}
	fmt.Fprintf(stderr, "evaluation %s: %d tasks, %d trials each, in %s\n", id, len(picked), *trials, e.Dir)
	trs, err := e.Run(ctx, picked)
	if err != nil {
		return err
	}
	sums := loop.Summarize(trs)
	raw, _ := json.MarshalIndent(map[string]any{"trials": trs, "tasks": sums}, "", "  ")
	os.WriteFile(filepath.Join(e.Dir, "results.json"), raw, 0o600)
	enc := json.NewEncoder(stdout)
	if *asJSON {
		for _, t := range trs {
			enc.Encode(t)
		}
		for _, s := range sums {
			enc.Encode(s)
		}
		return nil
	}
	var usd, judge, minutes float64
	reached, any, every, flips, n := 0, 0, 0, 0, 0
	for _, s := range sums {
		flip := ""
		if s.Flipped {
			flip, flips = " flipped", flips+1
		}
		fmt.Fprintf(stdout, "%-34s %-13s %s  %d/%d reached%s  $%.2f  %.1f min  %.0f turns\n", s.Task, s.Family, strings.Join(s.Verdicts, ","),
			s.Reached, len(s.Verdicts), flip, s.USD, s.Minutes, s.Turns)
		for _, e := range s.Errors {
			fmt.Fprintf(stdout, "  error: %s\n", e)
		}
		usd, judge, minutes = usd+s.USD, judge+s.JudgeUSD, minutes+s.Minutes*float64(len(s.Verdicts))
		reached, n = reached+s.Reached, n+len(s.Verdicts)
		if s.AnyReached {
			any++
		}
		if s.AllReached && len(s.Verdicts) > 0 {
			every++
		}
	}
	if n > 0 {
		fmt.Fprintf(stdout, "trials: %d of %d reached the root cause; tasks: %d of %d in at least one trial (pass@k), %d in every trial (pass^k), %d flipped\n",
			reached, n, any, len(sums), every, flips)
		fmt.Fprintf(stdout, "cost: $%.2f in the investigations (mean $%.2f, %.1f min a trial), $%.2f in the judge\n", usd, usd/float64(n), minutes/float64(n), judge)
	}
	return nil
}
