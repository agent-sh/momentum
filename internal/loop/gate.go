package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ErrRefused is a gate's refusal: the write is outside the loop's authority, now or ever. The agent's run ends its
// try there; nothing else may route around it.
var ErrRefused = errors.New("refused")

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Gate holds the loop's authority, as the machine's configuration gives it. The loop never changes it: its
// configuration and code are outside what the ship tool may write (Protected, protectedConfig).
type Gate struct {
	Owners      []string // repository owners it may write to
	Exclude     []string // OWNER/REPO it never touches
	ConfigAllow []string // configuration directories it may change in place
	RepoGuard   []string // the machine's read-only scope check, run before any repository write
	// Busy names what works in a repository now (a live session, a process, a recent change), or "" when nothing
	// does; Own is the loop's own work directory, whose trees and processes are its own.
	Busy func(ctx context.Context, repoDir string) (string, error)
	Own  string
	Home string
	// Resolve names a checkout's repository (OWNER/REPO); RepoOf, by its GitHub origin, when nil.
	Resolve func(ctx context.Context, dir string) (string, error)
}

// Target is where a change goes: a repository (by its owner and name, and the checkout the loop starts from) or a
// configuration directory.
type Target struct {
	Kind  string // repo or config
	Repo  string // OWNER/REPO
	Dir   string // the repository's main checkout, or the configuration directory
	Base  string // a repository's default branch
	Owner string
}

var remoteRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// RepoOf is a checkout's repository on GitHub, by its origin remote.
func RepoOf(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("%s has no origin remote: %w", dir, err)
	}
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", refused("%s's origin %q is not a GitHub repository", dir, strings.TrimSpace(out))
	}
	return m[1] + "/" + m[2], nil
}

// Repository checks that the loop may change a repository now, and returns it as a target: an owner it may write
// to, a repository it does not exclude, the machine's scope check allows it, and nothing else works in it (the
// scheduled-AI policy leaves a repository another agent or a human works on alone).
func (g Gate) Repository(ctx context.Context, dir string) (Target, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Target{}, err
	}
	top, err := gitOutput(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Target{}, refused("%s is not a git checkout", dir)
	}
	main := filepath.Dir(strings.TrimSpace(top)) // the main checkout of a linked worktree too
	resolve := g.Resolve
	if resolve == nil {
		resolve = RepoOf
	}
	repo, err := resolve(ctx, main)
	if err != nil {
		return Target{}, err
	}
	owner := strings.SplitN(repo, "/", 2)[0]
	switch {
	case !slices.ContainsFunc(g.Owners, func(o string) bool { return strings.EqualFold(o, owner) }):
		return Target{}, refused("%s is not of an owner the loop may write to (loop.owners): report only", repo)
	case slices.ContainsFunc(g.Exclude, func(x string) bool { return strings.EqualFold(x, repo) }):
		return Target{}, refused("%s is excluded (loop.exclude): report only", repo)
	case len(g.RepoGuard) == 0:
		return Target{}, refused("no scope check is configured (loop.repo_guard): no repository write")
	}
	if err := g.scope(ctx, repo, main); err != nil {
		return Target{}, err
	}
	if g.Busy != nil {
		what, err := g.Busy(ctx, main)
		if err != nil {
			return Target{}, refused("cannot tell whether someone works in %s (%v): report only", repo, err)
		}
		if what != "" {
			return Target{}, refused("%s is in use: %s; the loop leaves it alone", repo, what)
		}
	}
	base, err := defaultBranch(ctx, main)
	if err != nil {
		return Target{}, err
	}
	return Target{Kind: "repo", Repo: repo, Dir: main, Base: base, Owner: owner}, nil
}

// scope runs the machine's read-only scope check for scheduled repository work: a nonzero exit or a disposition
// of report only is no write.
func (g Gate) scope(ctx context.Context, repo, dir string) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.RepoGuard[0], append(append([]string{}, g.RepoGuard[1:]...), "--repo", repo, "--path", dir)...)
	out, err := cmd.Output()
	var v struct {
		Allowed     bool     `json:"allowed"`
		Disposition string   `json:"disposition"`
		Reasons     []string `json:"reasons"`
	}
	if jerr := json.Unmarshal(out, &v); err != nil || jerr != nil || !v.Allowed || v.Disposition == "report_only" {
		why := strings.Join(v.Reasons, "; ")
		if why == "" && err != nil {
			why = err.Error()
		}
		return refused("the scope check does not allow %s (%s %s): report only", repo, v.Disposition, why)
	}
	return nil
}

func defaultBranch(ctx context.Context, dir string) (string, error) {
	if out, err := gitOutput(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(out), "origin/"), nil
	}
	for _, b := range []string{"main", "master"} {
		if _, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+b); err == nil {
			return b, nil
		}
	}
	return "", refused("cannot tell %s's default branch", dir)
}

// Config checks that the loop may change a configuration directory in place: under one loop.config_allow names,
// and none of what the loop's own judgment rests on (protectedConfig).
func (g Gate) Config(dir string) (Target, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Target{}, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	for _, p := range g.protectedConfig() {
		if under(abs, p) || under(p, abs) {
			return Target{}, refused("%s holds what the loop's own judgment rests on: never its to change", abs)
		}
	}
	for _, a := range g.ConfigAllow {
		if real, err := filepath.EvalSymlinks(a); err == nil {
			a = real
		}
		if under(abs, a) {
			return Target{Kind: "config", Dir: abs}, nil
		}
	}
	return Target{}, refused("%s is no configuration the loop may change (loop.config_allow)", abs)
}

// protectedConfig are the configuration the loop never changes, whatever loop.config_allow says: its own (its
// authority, budgets and detectors' sources), the machine's shared rules and the scheduled-AI policy, the
// harness's settings, and the mods whose records its detectors read (the mistakes log).
func (g Gate) protectedConfig() []string {
	h := g.Home
	return []string{filepath.Join(h, ".config", "momentum"), filepath.Join(h, ".local", "share", "momentum"),
		filepath.Join(h, ".config", "agents"), filepath.Join(h, ".claude", "settings.json"),
		filepath.Join(h, ".claude", "settings.local.json"), filepath.Join(h, ".claude", "mods", "mistakes"),
		filepath.Join(h, ".claude", "CLAUDE.md"), filepath.Join(h, ".config", "systemd")}
}

// Protected are the paths of momentum's own repository the loop never changes: its detectors, its measurement,
// its gates, its skills and runner, its case store and the commands over them. A change there is its evaluator's
// or its authority's, which only the owner or a session he directs makes.
var Protected = []string{"internal/detect/", "internal/loop/", "internal/measure/", "internal/store/cases.go",
	"internal/store/loop.go", "cmd/momentum/case.go", "cmd/momentum/loop.go", "cmd/momentum/ship.go",
	"cmd/momentum/measure.go", "internal/config/"}

// SelfRepo is momentum's own repository, where Protected applies.
const SelfRepo = "agent-sh/momentum"

// CheckPaths refuses a change to a repository that touches what the loop may never change there.
func CheckPaths(t Target, changed []string) error {
	if !strings.EqualFold(t.Repo, SelfRepo) {
		return nil
	}
	for _, c := range changed {
		for _, p := range Protected {
			if c == strings.TrimSuffix(p, "/") || strings.HasPrefix(c, p) {
				return refused("%s is the loop's own (its detectors, measurement, gates or skills): never its to change", c)
			}
		}
	}
	return nil
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// gitOutput runs git in a directory and returns what it printed. Git runs without optional locks and never
// prompts.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}
