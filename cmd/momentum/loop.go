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

// loopDir is where the loop keeps its runs, packages and work trees: next to the store.
func loopDir() string { return filepath.Join(paths.Data(), "loop") }

// setup builds the loop's parts from the machine's configuration: its runner and its ship tool, both under the
// gates the configuration gives.
func setup(st *store.Store, c config.Loop) (*loop.Runner, *loop.Ship, error) {
	home := paths.Home()
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
	name, email := gitGlobal("user.name"), gitGlobal("user.email")
	bwrap, _ := exec.LookPath("bwrap")
	exe, _ := os.Executable()
	bins := []string{filepath.Dir(exe), filepath.Join(home, ".local", "bin")}
	sb := loop.Sandbox{Bwrap: bwrap, Writable: []string{filepath.Join(home, ".cache", "go-build"), filepath.Join(home, ".cache", "uv")}}
	if len(c.CapacityGate) > 0 {
		gate := c.CapacityGate
		sb.Gate = func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, gate[0], gate[1:]...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("the machine has no room for the test now (%s): %w", strings.TrimSpace(string(out)), err)
			}
			return nil
		}
	}
	ship := &loop.Ship{Store: st, Dir: loopDir(), Live: c.Live, Actor: caseActor(), Identity: [2]string{name, email}, Sandbox: sb,
		Env: loop.CleanEnv(nil, bins...),
		Gate: loop.Gate{Owners: c.Owners, Exclude: c.Exclude, ConfigAllow: c.ConfigAllow, RepoGuard: c.RepoGuard, Own: loopDir(),
			Home: home, Busy: func(ctx context.Context, dir string) (string, error) { return repoBusy(ctx, st, dir) }}}
	if bwrap == "" {
		ship.Sandbox.Bwrap = "bwrap" // a check then fails to start, and says so
	}
	model, fallback := c.Model, c.FallbackModel
	if model == "" {
		model = defaultModel
	}
	if fallback == "" {
		fallback = defaultFallback
	}
	families := c.Families
	if len(families) == 0 {
		families = defaultFamilies
	}
	r := &loop.Runner{Store: st, Agent: loop.Agent{Runner: c.Runner, Model: model, Fallback: fallback}, Ship: ship, Detectors: detectors(),
		Families: families, Budgets: b, Rules: c.Rules, Constraints: c.Constraints, MCP: c.MCP, Dir: loopDir(), Home: home, Bins: bins,
		Post: func(ctx context.Context, repo, pr, body string) (string, error) {
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			out, err := exec.CommandContext(cctx, "gh", "pr", "comment", pr, "--repo", repo, "--body", body).Output()
			return strings.TrimSpace(string(out)), err
		}}
	return r, ship, nil
}

func gitGlobal(key string) string {
	out, _ := exec.Command("git", "config", "--global", key).Output()
	return strings.TrimSpace(string(out))
}

// repoBusy names what works in a repository now, or "": a live agent session (any phase but done) in one of its
// work trees, a process with its working directory in one, or a change in one in the last hour. The loop's own
// trees and processes are its own.
func repoBusy(ctx context.Context, st *store.Store, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", fmt.Errorf("git worktree list: %w", err)
	}
	own := loopDir()
	var trees []string
	for _, l := range strings.Split(string(out), "\n") {
		if t, ok := strings.CutPrefix(l, "worktree "); ok && !within(t, own) {
			trees = append(trees, t)
		}
	}
	ss, err := sessionsByPID(ctx, st)
	if err != nil {
		return "", err
	}
	for _, t := range trees {
		for _, s := range ss {
			if s.Cwd != "" && within(s.Cwd, t) && !within(s.Cwd, own) {
				return fmt.Sprintf("session %s (%s) works in %s", firstText(s.Name, s.Key), s.Tool, t), nil
			}
		}
	}
	self := os.Getpid()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == self {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || within(cwd, own) {
			continue
		}
		for _, t := range trees {
			if within(cwd, t) {
				comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
				return fmt.Sprintf("%s (pid %d) in %s", strings.TrimSpace(string(comm)), pid, t), nil
			}
		}
	}
	for _, t := range trees {
		gitdir := filepath.Join(t, ".git")
		if b, err := os.ReadFile(gitdir); err == nil && strings.HasPrefix(string(b), "gitdir: ") {
			gitdir = strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir: "))
		}
		for _, f := range []string{"index", "HEAD", "logs/HEAD"} {
			if st, err := os.Stat(filepath.Join(gitdir, f)); err == nil && time.Since(st.ModTime()) < time.Hour {
				return fmt.Sprintf("a change in %s in the last hour", t), nil
			}
		}
	}
	return "", nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// loopCmd runs the improvement loop once, or lists its runs.
func loopCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
	if os.Getenv("MOMENTUM_LOOP_RUN") != "" {
		return usageError{"the loop does not run inside one of its own runs"}
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
	unlock, err := lockLoop(paths.DB())
	if err != nil {
		return err
	}
	defer unlock()
	r, _, err := setup(st, c.Loop)
	if err != nil {
		return err
	}
	r.Log = func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	var fams []string
	for _, f := range strings.Split(*families, ",") {
		if f = strings.TrimSpace(f); f != "" {
			fams = append(fams, f)
		}
	}
	done, err := r.Once(ctx, loop.Options{Dry: *dry, Families: fams, Case: *only})
	enc := json.NewEncoder(stdout)
	for _, d := range done {
		if *asJSON {
			enc.Encode(map[string]any{"family": d.Family, "case": d.Case, "step": d.Step, "run": d.Run.ID, "status": d.Run.Status,
				"failure": d.Run.Failure, "usd": d.Run.USD, "summary": d.Run.Summary, "skipped": d.Skipped})
			continue
		}
		switch {
		case d.Skipped != "":
			fmt.Fprintf(stdout, "%-14s %-10s %s\n", d.Family, firstText(d.Case, "-"), d.Skipped)
		default:
			fmt.Fprintf(stdout, "%-14s %-10s %s %s: %s $%.2f %s\n", d.Family, d.Case, d.Step, d.Run.ID, d.Run.Status, d.Run.USD,
				firstText(d.Run.Failure, d.Run.Summary))
		}
	}
	return err
}

// lockLoop takes the loop's lock beside the store: one loop at a time, since runs of two would pick the same
// cases and spend the same budget. A second one fails at once and says so.
func lockLoop(db string) (func(), error) {
	f, err := os.OpenFile(db+".loop-lock", os.O_CREATE|os.O_RDWR, 0o600)
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
