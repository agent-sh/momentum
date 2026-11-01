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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/capacity"
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
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		bwrap = "bwrap" // a check then fails to start, and says so
	}
	exe, _ := os.Executable()
	bins := []string{filepath.Dir(exe), filepath.Join(home, ".local", "bin")}
	ro, toolPath := toolchains(home)
	sb := loop.Sandbox{Bwrap: bwrap, Home: home, ReadOnly: append(ro, c.SandboxRead...), Cache: filepath.Join(loopDir(), "cache"),
		Scratch: filepath.Join(loopDir(), "tmp"), Path: toolPath}
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
		Gate: loop.Gate{Owners: c.Owners, Exclude: c.Exclude, RepoGuard: c.RepoGuard, Own: loopDir(),
			Home: home, Busy: func(ctx context.Context, dir string) (string, error) { return repoBusy(ctx, st, dir) }}}
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
	ds, err := listDetectors()
	if err != nil {
		return nil, nil, err
	}
	_, scope := exec.LookPath("systemd-run")
	r := &loop.Runner{Store: st, Agent: loop.Agent{Runner: c.Runner, Model: model, Fallback: fallback, Scope: scope == nil}, Ship: ship,
		Detectors: ds, Families: families, Budgets: b, Rules: c.Rules, Constraints: c.Constraints, MCP: c.MCP, ReadDirs: c.ReadDirs,
		Dir: loopDir(), Home: home, Bins: bins,
		Post: func(ctx context.Context, repo, pr, body string) (string, error) {
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			out, err := exec.CommandContext(cctx, "gh", "pr", "comment", pr, "--repo", repo, "--body", body).Output()
			return strings.TrimSpace(string(out)), err
		}}
	return r, ship, nil
}

// toolchains are what a check's test reads under the home, read-only (the toolchains of the test runners a check
// may run, and their module and crate caches), and the directories of their programs, first on its path.
func toolchains(home string) (ro, path []string) {
	add := func(dir string) {
		if dir != "" && within(dir, home) && !slices.Contains(ro, dir) {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				ro = append(ro, dir)
			}
		}
	}
	if out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output(); err == nil {
		f := strings.Fields(string(out))
		if len(f) == 2 {
			add(f[0])
			add(f[1])
			path = append(path, filepath.Join(f[0], "bin"))
		}
	}
	for _, d := range []string{".cargo/bin", ".cargo/registry", ".cargo/git", ".rustup", ".nvm"} {
		add(filepath.Join(home, d))
	}
	path = append(path, filepath.Join(home, ".cargo", "bin"))
	if p, err := exec.LookPath("node"); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			path = append(path, filepath.Dir(real))
		}
	}
	return ro, path
}

func gitGlobal(key string) string {
	cmd := exec.Command("git", append(loop.GitSafety(), "config", "--global", key)...)
	cmd.Env = append(os.Environ(), "GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_ATTR_NOSYSTEM=1")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// repoBusy names what works in a repository now, or "": a live agent session (any phase but done) in one of its
// work trees, or above one whose calls of the last hour name it; a process with its working directory in one; a
// change to its tracked files not committed; or a commit, a checkout or a staging in one in the last hour. The
// loop's own trees and processes are its own.
func repoBusy(ctx context.Context, st *store.Store, dir string) (string, error) {
	ss, err := sessionsByPID(ctx, st)
	if err != nil {
		return "", err
	}
	return busyIn(ctx, st, dir, ss)
}

// busyIn is repoBusy over the live sessions given.
func busyIn(ctx context.Context, st *store.Store, dir string, ss map[int]capacity.Session) (string, error) {
	out, err := gitRead(ctx, dir, "worktree", "list", "--porcelain")
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
	home := paths.Home()
	for _, t := range trees {
		for _, s := range ss {
			switch {
			case s.Cwd == "" || within(s.Cwd, own):
			case within(s.Cwd, t):
				return fmt.Sprintf("session %s (%s) works in %s", firstText(s.Name, s.Key), s.Tool, t), nil
			case within(t, s.Cwd) && s.ID != "":
				// a session above the tree: its last hour's calls say whether it works in it
				calls, err := st.Events(ctx, store.Query{Since: time.Now().Add(-time.Hour), Session: s.ID, Kinds: []string{"tool_call"},
					Source: store.Transcripts, Limit: 2000})
				if err != nil {
					return "", err
				}
				rel, _ := filepath.Rel(s.Cwd, t)
				names := []string{t, "~/" + strings.TrimPrefix(strings.TrimPrefix(t, home), "/"), rel}
				for _, e := range calls {
					for _, n := range names {
						if n != "" && n != "." && strings.Contains(e.Summary, n) {
							return fmt.Sprintf("session %s (%s) works in %s from %s", firstText(s.Name, s.Key), s.Tool, t, s.Cwd), nil
						}
					}
				}
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
		// his tree's status, read without the lock that would rewrite its index
		status, err := gitRead(ctx, t, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(status) != "" {
			return fmt.Sprintf("uncommitted changes in %s", t), nil
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
	if len(args) > 0 && args[0] == "lesson" {
		return loopLesson(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "eval" {
		return loopEval(ctx, args[1:], stdout, stderr)
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
	if err == nil {
		r.Ship.SourceLive = c.SourceLive
	}
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
				"failure": d.Run.Failure, "usd": d.Run.USD, "summary": d.Run.Summary, "skipped": d.Skipped, "did": d.Did})
			continue
		}
		switch {
		case d.Did != "":
			fmt.Fprintf(stdout, "%-14s %-10s %s %s\n", firstText(d.Family, "-"), d.Case, d.Step, d.Did)
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

// loopLesson publishes a case's validated lesson to tacit: a case closed as improved, its lesson one line,
// scrubbed, written as the loop's observation (never the owner's words) in the scope of the repository its change
// went to. In a loop run it is the run's own case's.
func loopLesson(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("loop lesson", flag.ContinueOnError)
	fs.SetOutput(stderr)
	caseID := fs.String("case", os.Getenv("MOMENTUM_LOOP_CASE"), "the case (in a loop run: its own)")
	text := fs.String("text", "", "the lesson, one line")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if run := os.Getenv("MOMENTUM_LOOP_RUN"); run != "" && *caseID != os.Getenv("MOMENTUM_LOOP_CASE") {
		return usageError{fmt.Sprintf("this loop run works case %s, not %s", os.Getenv("MOMENTUM_LOOP_CASE"), *caseID)}
	}
	line := strings.TrimSpace(*text)
	switch {
	case *caseID == "":
		return usageError{"which case: --case ID"}
	case line == "" || strings.ContainsAny(line, "\n\r") || len(line) > 300:
		return usageError{"a lesson is one line of at most 300 characters"}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := st.CaseByID(ctx, *caseID)
	if err != nil {
		return usageError{err.Error()}
	}
	if c.Status != store.CaseClosed || c.Reason != store.Improved {
		return usageError{fmt.Sprintf("case %s is %s: a lesson is published only for a case closed as improved", c.ID, statusLine(c))}
	}
	scrubbed, err := loop.Redact(ctx, line)
	if err != nil {
		return err
	}
	scope := "projects/momentum"
	if p, ok, _ := (&loop.Ship{Dir: loopDir()}).Load(c.ID); ok && p.Repo != "" {
		scope = "projects/" + p.Repo[strings.IndexByte(p.Repo, '/')+1:]
	}
	// the text reserved first (scrubbed) is the one written, its mentions and references made inert, under a name of
	// the case's own: tacit takes the same name and text again as the same fact, so a write tried again after one
	// that failed partway writes nothing twice
	reserved, start, err := st.BeginLesson(ctx, c.ID, scrubbed)
	if err != nil {
		return err
	}
	if !start {
		fmt.Fprintf(stdout, "%s: its lesson is published already\n", c.ID)
		return nil
	}
	pub := loop.Neutral(reserved)
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, "tacit", "add", "--harness", "claude", "--kind", "observed", "--scope", scope, "--desc", pub,
		"--body", fmt.Sprintf("Learned by momentum's improvement loop on case %s (%s): %s", c.ID, c.Family, c.Title), lessonName(c.ID)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tacit add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(stdout, "%s: published to %s: %s\n", c.ID, scope, pub)
	return st.CaseNote(ctx, c.ID, store.LessonPublished, scope+": "+pub, caseActor())
}

// lessonName is the name of a case's lesson in tacit: one per case, so its write is the same fact each time.
func lessonName(caseID string) string {
	return "momentum-lesson-" + strings.ToLower(caseID)
}
