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
// configuration lives outside every repository it may write, and its code is outside what the ship tool may
// change (Protected).
type Gate struct {
	Owners    []string // repository owners it may write to
	Exclude   []string // OWNER/REPO it never touches
	RepoGuard []string // the machine's read-only scope check, run before any repository write
	// Busy names what works in a repository now (a live session, a process, a recent change), or "" when nothing
	// does; Own is the loop's own work directory, whose trees and processes are its own.
	Busy func(ctx context.Context, repoDir string) (string, error)
	Own  string
	Home string
	// Resolve names a checkout's repository (OWNER/REPO); RepoOf, by its GitHub origin, when nil.
	Resolve func(ctx context.Context, dir string) (string, error)
}

// Target is where a change goes: a repository, by its owner and name, and the checkout the loop starts from.
type Target struct {
	Repo  string // OWNER/REPO
	Dir   string // the repository's main checkout
	Base  string // its default branch
	Owner string
}

var remoteRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// originRepository resolves the fetch URL and every expanded push URL. A custom resolver may name a
// local test repository, but its destinations must still agree.
func originRepository(ctx context.Context, dir string) (string, error) {
	fetch, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	pushes, err := gitOutput(ctx, dir, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return "", err
	}
	identity := func(url string) string {
		url = strings.TrimSpace(url)
		if m := remoteRe.FindStringSubmatch(url); m != nil {
			return strings.ToLower(m[1] + "/" + m[2])
		}
		return "url:" + url
	}
	repo := identity(fetch)
	urls := strings.Split(strings.TrimSpace(pushes), "\n")
	for _, url := range urls {
		if url == "" || identity(url) != repo {
			return "", refused("origin's fetch and push destinations differ")
		}
	}
	return repo, nil
}

// RepoOf is a checkout's repository on GitHub, shared by its fetch and all push destinations.
func RepoOf(ctx context.Context, dir string) (string, error) {
	repo, err := originRepository(ctx, dir)
	if err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[\w.-]+/[\w.-]+$`).MatchString(repo) {
		return "", refused("origin is not a GitHub repository")
	}
	return repo, nil
}

// Repository checks that the loop may change a repository now, and returns it as a target: an owner it may write
// to, a repository it does not exclude, the machine's scope check allows it, and nothing else works in it (the
// scheduled-AI policy leaves a repository another agent or a human works on alone).
func (g Gate) Repository(ctx context.Context, dir string) (Target, error) {
	if err := CheckGitFilters(ctx, dir); err != nil {
		return Target{}, err
	}
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
	if _, err := originRepository(ctx, dir); err != nil {
		return Target{}, err
	}
	repo, err := resolve(ctx, dir)
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
	return Target{Repo: repo, Dir: main, Base: base, Owner: owner}, nil
}

// scope runs the machine's read-only scope check for scheduled repository work: a nonzero exit or a disposition
// of report only is no write.
func (g Gate) scope(ctx context.Context, repo, dir string) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.RepoGuard[0], append(append([]string{}, g.RepoGuard[1:]...), "--repo", repo, "--path", dir)...)
	cmd.Env = append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
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

// Protected are the paths of momentum's own repository the loop never changes: what its judgment and its
// authority rest on. Its detectors and the events they read (the store, the sources that fill it, the masks), its
// measurement, its gates, skills and runner, its commands (the git and replay guards among them), its
// configuration, its build (go.mod, go.sum, CI) and its contract (SPEC, AGENTS). A change there is its
// evaluator's or its authority's, which only the owner or a session he directs makes. Each call returns a list
// of its own: nothing a change compiles in can empty it.
func Protected() []string {
	return []string{"internal/detect/", "internal/loop/", "internal/measure/", "internal/store/", "internal/source/", "internal/plain/",
		"internal/config/", "internal/paths/", "cmd/momentum/", "go.mod", "go.sum", ".github/", "SPEC.md", "AGENTS.md"}
}

// SelfRepo is momentum's own repository, where Protected applies.
const SelfRepo = "agent-sh/momentum"

// CheckPaths refuses a change to a repository that touches what the loop may never change there: changed holds
// every path a change adds, removes or edits (both sides of a move).
func CheckPaths(t Target, changed []string) error {
	if !strings.EqualFold(t.Repo, SelfRepo) {
		return nil
	}
	for _, c := range changed {
		for _, p := range Protected() {
			if c == strings.TrimSuffix(p, "/") || strings.HasPrefix(c, p) {
				return refused("%s is the loop's own (what its judgment or its authority rests on): never its to change", c)
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

// gitOutput runs git in a directory and returns what it printed. Git runs without optional locks, hooks or a
// file-system monitor, never prompts, and reads none of git's own variables (a GIT_DIR would point it elsewhere).
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	if err := CheckGitFilters(ctx, dir); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append(append([]string{"-C", dir}, GitSafety()...), args...)...)
	cmd.Env = append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
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

// noGitEnv removes caller Git variables and EMAIL, which Git takes for an identity, and pins tree attributes.
func noGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "EMAIL=") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_ATTR_NOSYSTEM=1")
}

// GitSafety pins execution settings for every Git call outside a sandbox. Tree attributes cannot select
// transformations, and no signature format or signing setting may run a repository's program.
func GitSafety() []string {
	return []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null", "-c", "gpg.program=false", "-c", "gpg.openpgp.program=false",
		"-c", "gpg.x509.program=false", "-c", "gpg.ssh.program=false", "-c", "log.showSignature=false",
		"-c", "push.gpgSign=false"}
}

// GitEnv removes caller Git settings and disables system and global settings and tree attributes.
func GitEnv() []string {
	return append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
}
