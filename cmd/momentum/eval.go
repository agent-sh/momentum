package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	asJSON := fs.Bool("json", false, "prints one results object")
	heldOut := fs.String("held-out", "", "comma-separated held-out task ids, recorded with the evaluation")
	split := fs.String("split", "all", "task split: all, development, held-out")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *trials < 1 || *parallel < 1 || *budget < 0 || math.IsNaN(*budget) || math.IsInf(*budget, 0) {
		return usageError{"trials and parallel must be positive; budget must not be negative"}
	}
	if fs.NArg() > 0 {
		return usageError{"loop eval takes flags only"}
	}
	if *split != "all" && *split != "development" && *split != "held-out" {
		return usageError{"split must be all, development, or held-out"}
	}
	selected := strings.Split(*arms, ",")
	seen := map[string]bool{}
	for _, a := range selected {
		if (a != "loop" && a != "native") || seen[a] {
			return usageError{"arms must be loop,native, or one alone"}
		}
		seen[a] = true
	}
	raw, err := os.ReadFile(*golden)
	if err != nil {
		return usageError{err.Error()}
	}
	all, err := loop.ParseGolden(raw, *golden)
	if err != nil {
		return usageError{err.Error()}
	}
	held := map[string]bool{}
	for _, id := range strings.Split(*heldOut, ",") {
		if id = strings.TrimSpace(id); id != "" {
			held[id] = true
		}
	}
	unknownHeld := maps.Clone(held)
	for _, t := range all {
		delete(unknownHeld, t.ID)
	}
	for id := range unknownHeld {
		return usageError{fmt.Sprintf("no held-out task %q in the golden set", id)}
	}
	if *split != "all" && len(held) == 0 {
		return usageError{"a selected split needs --held-out task ids"}
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
		if *split == "held-out" && !held[t.ID] || *split == "development" && held[t.ID] {
			continue
		}
		if allTasks || want[t.ID] {
			picked = append(picked, t)
			delete(unknown, t.ID)
		}
	}
	for t := range unknown {
		return usageError{fmt.Sprintf("task %q is absent from the selected split in %s", t, *golden)}
	}
	// one evaluation at a time: its sweep of leftovers stops every unit of the evaluation prefix, a running one's too
	unlock, err := lockEval(paths.DB())
	if err != nil {
		return err
	}
	defer unlock()
	c, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := setup(st, c)
	if err != nil {
		return err
	}
	if err := onBedrock(r.Agent); err != nil {
		return err // every trial and judge is on Bedrock: with no route, the evaluation does not start
	}
	id := loop.NewEvalID()
	var repositories []string
	for _, t := range all {
		if t.RepoPath != "" {
			repositories = append(repositories, t.RepoPath)
		}
	}
	// Records run a pinned copy of the evaluating binary outside every sandbox.
	r.Inputs.Momentum = selfBinary()
	judge := r.Agent
	judge.Fallback = ""
	e := &loop.Eval{Runner: r, Source: paths.DB(), Dir: filepath.Join(loopDir(), "eval", id), Trials: *trials, Budget: *budget, Parallel: *parallel, ID: id,
		Judge: judge, Arms: selected, Repositories: repositories, GoldenHash: fmt.Sprintf("%x", sha256.Sum256(raw)), Tacit: r.Inputs.TacitStore, Split: *split}
	for _, t := range all {
		if held[t.ID] {
			e.HeldOut = append(e.HeldOut, t.ID)
		}
	}
	for _, t := range picked {
		e.Selected = append(e.Selected, t.ID)
	}
	tacit := "no tacit (no store)"
	if e.Tacit != "" {
		tacit = "tacit's store as of each end"
	}
	fmt.Fprintf(stderr, "evaluation %s: %d tasks, %d trials per arm, %s, in %s\n", id, len(picked), *trials, tacit, e.Dir)
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
		fmt.Fprintf(out, "%s: %d/%d graded trials reached; %d partial, %d missed, %d wrong, %d errors, %d skipped, %d excluded, %d runs cut short; %d/%d ran\n", a.Arm, a.Reached, a.Graded, a.Partial, a.Missed, a.Wrong, a.Errors, a.Skipped, a.Excluded, a.CutShort, a.Ran, a.Planned)
		fmt.Fprintf(out, "  cost $%.2f; time %.2f min; per trial $%s, %s min; per root cause reached $%s, %s min; judge $%.2f\n", a.USD, a.Minutes, rate(a.USDPerTrial), rate(a.MinutesPerTrial), rate(a.USDPerReached), rate(a.MinutesPerReached), a.JudgeUSD)
		fmt.Fprintf(out, "  tokens: %d input, %d output, %d cache read, %d cache write (%d of them for an hour)\n", a.Usage.Input, a.Usage.Output, a.Usage.CacheRead, a.Usage.CacheWrite, a.Usage.CacheWrite1h)
		fmt.Fprintf(out, "  compared: %d tasks, %d root causes reached among them; %d trials ungraded, left out with their pairs; pass@k, pass^k and the rates per root cause read the compared trials alone\n", a.ComparedTasks, a.ComparedReached, a.Ungraded)
		fmt.Fprintf(out, "  k=%d: pass@k %s, pass^k %s; %d tasks flipped\n", a.K, rate(a.PassAtK), rate(a.PassAllK), a.Flips)
		seenErrors := map[string]bool{}
		for _, tr := range r.Trials {
			why := tr.Err
			if why == "" && tr.Run.Status == store.RunFailed {
				why = strings.TrimSpace(tr.Run.Failure + ": " + tr.Run.Summary)
			}
			if tr.Arm == a.Arm && why != "" && !seenErrors[why] {
				fmt.Fprintf(out, "  error: %s\n", why)
				seenErrors[why] = true
			}
		}
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
	compared := 0
	for _, task := range r.Comparison.Tasks {
		if task.MoreOften != "not compared" {
			compared++
		}
	}
	fmt.Fprintf(out, "loop minus native, task bootstrap 90%% interval (%d compared tasks; %d resamples, seed %d): reached per dollar %s; reached per minute %s\n", compared, r.Comparison.Resamples, r.Comparison.Seed, interval(r.Comparison.ReachedPerDollar), interval(r.Comparison.ReachedPerMinute))
}

// selfBinary is the running binary: on Linux the process's own executable, which stays the build that started even
// when its file is replaced.
func selfBinary() string {
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		return "/proc/self/exe"
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// lockEval takes the evaluation's lock beside the store. A second evaluation fails at once and says so.
func lockEval(db string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(db+".eval-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another evaluation runs now")
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
