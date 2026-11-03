package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"path/filepath"

	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// The loop's defaults that are no machine path: the harness's main agent's models (SPEC, Architecture) and the
// families it starts with.
const (
	defaultModel    = "global.anthropic.claude-opus-5-5"
	defaultFallback = "global.anthropic.claude-sonnet-5-5"
)

var defaultFamilies = []string{"failures", "owner-burden", "sessions"}

// loopDir is where the loop keeps its runs and pass inputs: next to the store.
func loopDir() string { return filepath.Join(paths.Data(), "loop") }

// onBedrock says whether the loop's agent can start a run now: on Bedrock, by its route, with Bedrock's models. The
// commands that start a model check it first; dry selection and run listing do not.
// The relay holds every request of every run to a global inference profile; this says before any run starts that
// a configured model could never run. The model and the fallback model are each one model: a list is refused, since
// a run refused by the model's safeguards would pass the whole list as its model.
func onBedrock(a loop.Agent) error {
	if err := a.Bedrock.Check(); err != nil {
		return err
	}
	for _, c := range []struct{ name, model string }{{"model", a.Model}, {"fallback_model", a.Fallback}} {
		switch m := strings.TrimSpace(c.model); {
		case strings.ContainsAny(m, ", \t"):
			return fmt.Errorf("loop %s %q is a list: the loop takes one model, and one fallback model", c.name, m)
		case m != "" && !loop.BedrockModel(m):
			return fmt.Errorf("loop %s %q is no Bedrock model: every headless run is on Bedrock, on a global inference profile (global.anthropic.claude-...)", c.name, m)
		case m != "" && !loop.RelayPriced(m):
			return fmt.Errorf("loop %s %q has no rate in the relay, which prices only %s: every request would be refused", c.name, m, strings.Join(loop.RelayModels(), " and "))
		}
	}
	return nil
}

// expandAll is each path with its home expanded.
func expandAll(ps []string) []string {
	var out []string
	for _, p := range ps {
		out = append(out, paths.Expand(p))
	}
	return out
}

// selectionRunner reads only the store. It discovers no programs, toolchains or live sources.
func selectionRunner(st *store.Store, c config.Loop) *loop.Runner {
	b := loop.DefaultBudgets
	for _, v := range []struct {
		dst *float64
		src float64
	}{{&b.Run, c.RunUSD}, {&b.Case, c.CaseUSD}, {&b.Day, c.DayUSD}, {&b.Week, c.WeekUSD}} {
		if v.src > 0 {
			*v.dst = v.src
		}
	}
	if c.RunMinutes > 0 {
		b.RunTime = time.Duration(c.RunMinutes) * time.Minute
	}
	if c.MaxTurns > 0 {
		b.MaxTurns = c.MaxTurns
	}
	families := c.Families
	if len(families) == 0 {
		families = defaultFamilies
	}
	return &loop.Runner{Store: st, Ship: &loop.Ship{Store: st, Dir: loopDir()},
		Families: families, Budgets: b, Dir: loopDir()}
}

// setup configures the live investigation runner and its trusted input sources.
func setup(st *store.Store, sources config.Sources) (*loop.Runner, error) {
	c := sources.Loop
	r := selectionRunner(st, c)
	home := paths.Home()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	model, fallback := c.Model, c.FallbackModel
	if model == "" {
		model = defaultModel
	}
	if fallback == "" {
		fallback = defaultFallback
	}
	ds, err := listDetectors()
	if err != nil {
		return nil, err
	}
	r.Agent = loop.Agent{Runner: c.Runner, Model: model, Fallback: fallback, Bedrock: &loop.Bedrock{File: c.BedrockEnv}}
	r.Ship = setupShip(st, sources)
	r.Toolchains = loop.Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path, Cache: r.Ship.Sandbox.Cache,
		GoRoot: r.Ship.Sandbox.GoRoot, GoModules: r.Ship.Sandbox.GoModules}
	r.Tacit = "tacit"
	r.Post = func(ctx context.Context, repo, pr, body string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, "gh", "pr", "comment", pr, "--repo", repo, "--body", body)
		cmd.Env = loop.CleanEnv(nil)
		out, e := commandOutput(cmd, false)
		return strings.TrimSpace(string(out)), e
	}
	r.Detectors = ds
	r.Rules, r.Constraints = c.Rules, c.Constraints
	tacit := c.TacitStore
	if tacit == "" {
		tacit = filepath.Join(home, ".local", "share", "tacit", "store")
	}
	r.Inputs = loop.InputOptions{Momentum: exe, Home: home, StoreHome: paths.Data(), TacitStore: tacit, ReadDirs: expandAll(c.ReadDirs), Window: loop.StoreWindow}
	return r, nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// loopCmd runs the improvement loop once, or lists its runs.
func loopCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "lesson" {
		return loopLesson(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "runs" {
		return loopRuns(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("loop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry", false, "pick each family's case and step, and run nothing")
	families := fs.String("family", "", "comma-separated families (default: loop.families)")
	only := fs.String("case", "", "work this case only")
	asJSON := fs.Bool("json", false, "one JSON object per step")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("loop takes no arguments, got %q", fs.Arg(0))}
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	open := store.Open
	var unlock func()
	if *dry {
		unlock, err = loopLock(paths.DB(), true)
		if err != nil {
			return err
		}
		defer unlock()
		open = store.OpenReadOnlyExclusive
	}
	st, err := open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if !*dry {
		unlock, err = lockLoop(paths.DB())
		if err != nil {
			return err
		}
		defer unlock()
	}
	r := selectionRunner(st, c.Loop)
	if !*dry {
		r, err = setup(st, c)
		if err != nil {
			return err
		}
	}
	r.Ship.SourceLive = c.SourceLive
	r.Log = func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	if !*dry {
		if err := onBedrock(r.Agent); err != nil {
			return err // no route, no run: nothing is recovered, detected, reserved or started
		}
	}
	var fams []string
	for _, f := range strings.Split(*families, ",") {
		if f = strings.TrimSpace(f); f != "" {
			fams = append(fams, f)
		}
	}
	done, err := r.Once(ctx, loop.Options{Dry: *dry, Families: fams, Case: *only})
	enc := json.NewEncoder(stdout)
	for _, d := range done {
		if printErr := printDone(stdout, enc, d, *asJSON); printErr != nil {
			return printErr
		}
	}
	return err
}

// printDone prints one step the loop took, or why it took none: a line, or an object with --json. What the run's relay
// refused comes after the line, apart from the failure the stop rule compares.
func printDone(stdout io.Writer, enc *json.Encoder, d loop.Done, asJSON bool) error {
	// Clean fields separately so a cleaner cannot break JSON escaping or keys.
	for _, text := range []*string{&d.Run.Summary, &d.Skipped, &d.Run.Failure, &d.Did} {
		clean, err := loop.Redact(context.Background(), *text)
		if err != nil {
			return err
		}
		*text = clean
	}
	d.Refused = append([]string(nil), d.Refused...)
	for i := range d.Refused {
		clean, err := loop.Redact(context.Background(), d.Refused[i])
		if err != nil {
			return err
		}
		d.Refused[i] = clean
	}
	if asJSON {
		return enc.Encode(map[string]any{"family": d.Family, "case": d.Case, "step": d.Step, "run": d.Run.ID, "status": d.Run.Status,
			"failure": d.Run.Failure, "usd": d.Run.USD, "summary": d.Run.Summary, "skipped": d.Skipped, "did": d.Did, "relay_refused": d.Refused})
	}
	if d.Did != "" {
		_, err := fmt.Fprintf(stdout, "%-14s %-10s %s %s\n", firstText(d.Family, "-"), d.Case, d.Step, d.Did)
		return err
	}
	if d.Skipped != "" {
		_, err := fmt.Fprintf(stdout, "%-14s %-10s %s\n", d.Family, firstText(d.Case, "-"), d.Skipped)
		return err
	}
	line := fmt.Sprintf("%-14s %-10s %s %s: %s $%.2f %s", d.Family, d.Case, d.Step, d.Run.ID, d.Run.Status, d.Run.USD,
		firstText(d.Run.Failure, d.Run.Summary))
	if len(d.Refused) > 0 {
		line += " (the relay refused " + strings.Join(d.Refused, "; ") + ")"
	}
	_, err := fmt.Fprintln(stdout, line)
	return err
}

// lockLoop takes the loop's lock beside the store: one loop at a time, since runs of two would pick the same
// cases and spend the same budget. A second one fails at once and says so.
func lockLoop(db string) (func(), error) {
	return loopLock(db, false)
}

// A dry lock check reads an existing lock without creating one. No file means no loop holds it.
func loopLock(db string, dry bool) (func(), error) {
	flags := os.O_CREATE | os.O_RDWR
	if dry {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(db+".loop-lock", flags, 0o600)
	if dry && errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another loop runs now")
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// loopRuns lists the loop's runs, the newest first, and what the live ones spent today and this week.
func loopRuns(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("loop runs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "7d", "how far back")
	only := fs.String("case", "", "one case's runs")
	asJSON := fs.Bool("json", false, "one JSON object per run")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	from, err := parseWhen(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	runs, err := st.LoopRuns(ctx, store.RunFilter{Since: from, Case: *only})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	for _, r := range runs {
		// Older rows can predate summary cleaning. Clean the display copy only;
		// the stored failure key remains the stop rule's own.
		for _, text := range []*string{&r.Summary, &r.Failure} {
			clean, err := loop.Redact(ctx, *text)
			if err != nil {
				return err
			}
			*text = clean
		}
		if *asJSON {
			if err := enc.Encode(r); err != nil {
				return err
			}
			continue
		}
		tag := ""
		if r.Experiment != "" {
			tag = " [" + r.Experiment + "]"
		}
		fmt.Fprintf(stdout, "%s  %-10s %-14s %-11s %-8s $%5.2f %3d turns %s%s\n", r.Started.Local().Format("01-02 15:04"), r.Case, r.Family,
			r.Step, r.Status, r.USD, r.Turns, firstText(r.Failure, r.Summary), tag)
	}
	if !*asJSON {
		day, _ := st.LoopSpend(ctx, time.Now().Add(-24*time.Hour), 0)
		week, _ := st.LoopSpend(ctx, time.Now().Add(-7*24*time.Hour), 0)
		fmt.Fprintf(stdout, "spent by live runs: $%.2f in the last day, $%.2f in the last week\n", day, week)
	}
	return nil
}

func loopLesson(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("loop lesson", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("case", "", "the case")
	text := fs.String("text", "", "the lesson, one line")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *id == "" || fs.NArg() != 0 {
		return usageError{"loop lesson needs --case ID and --text TEXT"}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if err = loop.PublishLesson(ctx, st, &loop.Ship{Dir: loopDir()}, *id, *text, "tacit", caseActor()); err != nil {
		if errors.Is(err, loop.ErrRefused) {
			return usageError{err.Error()}
		}
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s: lesson publication recorded\n", *id)
	return err
}

func lessonName(caseID string) string { return "momentum-lesson-" + strings.ToLower(caseID) }
