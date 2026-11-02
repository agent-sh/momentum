package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
)

// gitSpec is what evidence git lets one git command take: its options by exact name (git takes an unambiguous
// prefix of a long option, so a prefix is refused, not read as the option), those of them that take a value, and
// what its other arguments may be. A revision is checked to be one (git rev-parse --verify), so no argument is read
// as a file; a path comes only after `--`, relative and inside the repository.
type gitSpec struct {
	flags  []string // options with no value
	values []string // options with a value: --opt=V, --opt V, or a short one's -xV
	revs   bool     // it takes revisions (a range A..B, REV:PATH) before `--`
	most   int      // at most this many revisions; 0: no limit
	paths  bool     // it takes paths after `--`
	need   bool     // it needs exactly one path (blame)
	revVal []string // the options whose value is a revision
}

var (
	logFlags = []string{"--oneline", "--stat", "--shortstat", "--numstat", "--name-only", "--name-status", "-p", "--patch", "--no-patch", "-s",
		"--follow", "--reverse", "--all", "--branches", "--tags", "--first-parent", "--no-merges", "--merges", "-i", "--regexp-ignore-case",
		"--decorate", "--no-decorate", "--graph", "--abbrev-commit", "--full-history", "--date-order", "--topo-order"}
	logValues = []string{"-n", "--max-count", "--skip", "--since", "--until", "--after", "--before", "--author", "--committer", "--grep",
		"--format", "--pretty", "--date", "-S", "-G", "--diff-filter", "-U", "--unified"}
	diffFlags = []string{"--stat", "--shortstat", "--numstat", "--name-only", "--name-status", "-p", "--patch", "--no-patch", "-s", "-w",
		"--ignore-all-space", "-b", "--ignore-space-change", "--word-diff"}
)

// gitSpecs are the git commands evidence git runs: the ones that read a repository's history and state, each
// with only the options that read.
var gitSpecs = map[string]gitSpec{
	"log":      {flags: logFlags, values: logValues, revs: true, paths: true},
	"show":     {flags: append(diffFlags, "--oneline", "--abbrev-commit"), values: []string{"--format", "--pretty", "--date", "-U", "--unified"}, revs: true, paths: true},
	"diff":     {flags: append(diffFlags, "--cached", "--staged"), values: []string{"-U", "--unified", "--diff-filter"}, revs: true, most: 2, paths: true},
	"blame":    {flags: []string{"-w", "-M", "-C", "--porcelain", "--line-porcelain", "-s", "-e", "--show-email", "-l", "--root", "-f", "--show-name", "-n", "--show-number"}, values: []string{"-L", "--date"}, revs: true, most: 1, paths: true, need: true},
	"grep":     {flags: []string{"-n", "--line-number", "-i", "--ignore-case", "-w", "--word-regexp", "-l", "--files-with-matches", "-c", "--count", "-E", "--extended-regexp", "-F", "--fixed-strings", "-I", "--heading", "--break", "-h", "-H"}, values: []string{"-e", "-A", "-B", "-C", "--max-depth"}, revs: true, paths: true},
	"ls-files": {flags: []string{"--stage", "-s", "--cached", "-c", "--modified", "-m", "--others", "-o", "--exclude-standard", "--deleted", "-d", "--full-name"}, paths: true},
	"ls-tree":  {flags: []string{"-r", "--name-only", "--name-status", "-l", "--long", "-t", "-d", "--full-name"}, revs: true, most: 1, paths: true},
	"rev-parse": {flags: []string{"--abbrev-ref", "--short", "--verify", "--show-toplevel", "--is-inside-work-tree", "--symbolic-full-name", "--quiet", "-q"},
		revs: true},
	"rev-list":   {flags: []string{"--count", "--all", "--reverse", "--first-parent", "--no-merges", "--merges", "--left-right"}, values: []string{"-n", "--max-count", "--since", "--until", "--after", "--before", "--author", "--grep"}, revs: true, paths: true},
	"merge-base": {flags: []string{"--is-ancestor", "--all"}, revs: true},
	"describe":   {flags: []string{"--tags", "--always", "--long", "--all", "--contains"}, values: []string{"--abbrev", "--match"}, revs: true},
	"status":     {flags: []string{"--short", "-s", "--porcelain", "-b", "--branch"}, values: []string{"--untracked-files", "-u"}, paths: true},
	"shortlog":   {flags: []string{"-s", "--summary", "-n", "--numbered", "-e", "--email"}, values: []string{"--since", "--until", "--after", "--before"}, revs: true, paths: true},
	"branch":     {flags: []string{"-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--list", "--show-current"}, values: []string{"--contains", "--no-contains", "--merged", "--no-merged", "--sort", "--format"}, revVal: []string{"--contains", "--no-contains", "--merged", "--no-merged"}},
	"tag":        {flags: []string{"-l", "--list"}, values: []string{"--contains", "--no-contains", "--points-at", "--sort", "--format"}, revVal: []string{"--contains", "--no-contains", "--points-at"}},
}

// gitSafe are the settings every git evidence git runs takes, whatever the repository's own configuration says:
// no hooks, no file-system monitor, no pager, no external diff, no remote.
var gitSafe = []string{"--no-pager", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.pager=cat", "-c", "diff.external=",
	"-c", "core.sshCommand=false", "-c", "protocol.allow=never"}

// evidenceGit runs a reading git command in a repository's work tree: the improvement loop reads history through
// it, since its harness lets it run no git of its own. It refuses any option it does not list for the command,
// any argument before `--` that is not a revision, and any path that is not inside the repository.
func evidenceGit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return usageError{"evidence git takes a repository's directory, then a git command that reads (log, show, diff, blame, ...)"}
	}
	dir, sub, rest := paths.Expand(args[0]), args[1], args[2:]
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return usageError{fmt.Sprintf("%s is no directory", dir)}
	}
	// Resolve before checking a bound or asking Git which repository it opens.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return usageError{err.Error()}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return usageError{err.Error()}
	}
	var trial string
	if os.Getenv("MOMENTUM_EVAL") != "" {
		trial, err = filepath.EvalSymlinks(os.Getenv("MOMENTUM_LOOP_DIR"))
		if err == nil {
			trial, err = filepath.Abs(trial)
		}
		if err != nil || !within(dir, trial) {
			return usageError{"in an evaluation, evidence git reads only under the resolved trial directory"}
		}
	}
	spec, ok := gitSpecs[sub]
	if !ok {
		return usageError{fmt.Sprintf("git %s is not a command evidence git runs: it runs the ones that read (%s)", sub, gitNames())}
	}
	if out, err := gitRead(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return usageError{fmt.Sprintf("%s is not a git work tree", dir)}
	}
	top, err := gitRead(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return usageError{"cannot resolve the repository's work tree"}
	}
	top, err = filepath.EvalSymlinks(strings.TrimSpace(top))
	if err != nil || trial != "" && !within(top, trial) {
		return usageError{"in an evaluation, the repository must be under the resolved trial directory"}
	}
	argv, err := gitArgs(ctx, dir, sub, spec, rest)
	if err != nil {
		return err
	}
	if err := loop.CheckGitConfig(ctx, dir); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append(append(append([]string{"-C", dir}, gitSafe...), loop.GitSafety()...), argv...)...)
	cmd.Env = gitEnv()
	var out bytes.Buffer
	capture := &capped{w: &out, n: (1 << 20) + 1}
	cmd.Stdout, cmd.Stderr = capture, capture
	err = cmd.Run()
	if out.Len() > 1<<20 {
		return usageError{"git output exceeds 1 MiB; narrow the command"}
	}
	clean, scrubErr := loop.Redact(ctx, out.String())
	if scrubErr != nil {
		return scrubErr
	}
	if _, writeErr := io.WriteString(stdout, clean); writeErr != nil {
		return writeErr
	}
	return err
}

// gitArgs checks a command's arguments against its spec and returns them as git takes them.
func gitArgs(ctx context.Context, dir, sub string, spec gitSpec, rest []string) ([]string, error) {
	refuse := func(format string, a ...any) error {
		return usageError{fmt.Sprintf("git %s: %s: evidence git only reads", sub, fmt.Sprintf(format, a...))}
	}
	out := []string{sub}
	if sub == "log" || sub == "show" || sub == "diff" {
		out = append(out, "--no-ext-diff", "--no-textconv")
	}
	revs, pathArgs, blobs := 0, 0, 0
	afterDash := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case afterDash:
			if err := repoPath(a); err != nil {
				return nil, refuse("%s", err)
			}
			if err := gitPathWithin(ctx, dir, a); err != nil {
				return nil, refuse("%s", err)
			}
			pathArgs++
			out = append(out, a)
			continue
		case a == "--":
			if !spec.paths {
				return nil, refuse("it takes no paths")
			}
			afterDash = true
			out = append(out, a)
			continue
		case strings.HasPrefix(a, "-"):
			name, val, hasVal := strings.Cut(a, "=")
			switch {
			case contains(spec.flags, a):
				out = append(out, a)
			case contains(spec.values, name) && hasVal:
				if err := gitValue(ctx, dir, spec, name, val); err != nil {
					return nil, refuse("%s", err)
				}
				out = append(out, a)
			case contains(spec.values, a) && i+1 < len(rest):
				if err := gitValue(ctx, dir, spec, a, rest[i+1]); err != nil {
					return nil, refuse("%s", err)
				}
				joined := a + rest[i+1]
				if strings.HasPrefix(a, "--") {
					joined = a + "=" + rest[i+1]
				}
				out, i = append(out, joined), i+1
			case len(a) > 2 && a[1] != '-' && contains(spec.values, a[:2]): // a short option's value attached: -n5, -U3
				if err := gitValue(ctx, dir, spec, a[:2], a[2:]); err != nil {
					return nil, refuse("%s", err)
				}
				out = append(out, a)
			default:
				return nil, refuse("%s is not an option it runs with", a)
			}
			continue
		}
		if !spec.revs {
			return nil, refuse("%q: it takes no revision; give paths after --", a)
		}
		if err := isRevision(ctx, dir, a); err != nil {
			if sub == "grep" {
				return nil, refuse("%q is no revision of the repository (a pattern goes with -e, a path after --)", a)
			}
			return nil, refuse("%q is no revision of the repository (a path goes after --)", a)
		}
		revs++
		if strings.Contains(a, ":") {
			blobs++
		}
		if spec.most > 0 && revs > spec.most {
			return nil, refuse("it takes at most %d revisions", spec.most)
		}
		out = append(out, a)
	}
	if spec.need && pathArgs != 1 {
		return nil, refuse("it takes one path, after --")
	}
	if blobs > 0 && (revs != 1 || pathArgs > 0) {
		return nil, refuse("a blob selector must be the only revision, with no paths")
	}
	if blobs == 0 && contains([]string{"log", "show", "diff", "grep"}, sub) {
		if !afterDash {
			out = append(out, "--")
		}
		out = append(out, credentialExcludes()...)
	}
	return out, nil
}

// credentialExcludes keeps implicit history and wildcard path queries from reading credential files too.
func credentialExcludes() []string { return loop.CredentialPathspecs() }

// gitValue checks an option's value: a revision for the options that take one, and a value that is no option.
func gitValue(ctx context.Context, dir string, spec gitSpec, name, val string) error {
	if val == "" || strings.HasPrefix(val, "-") {
		return fmt.Errorf("%s requires a non-option value", name)
	}
	if contains(spec.revVal, name) {
		if err := isRevision(ctx, dir, val); err != nil {
			return fmt.Errorf("%s %q is no revision", name, val)
		}
	}
	return nil
}

// isRevision checks that each side of a revision (a range's two, a REV:PATH's revision) names an object of the
// repository.
func isRevision(ctx context.Context, dir, a string) error {
	if a == "" || strings.HasPrefix(a, "-") {
		return fmt.Errorf("no revision")
	}
	sides := []string{strings.TrimPrefix(a, "^")}
	for _, sep := range []string{"...", ".."} {
		if l, r, ok := strings.Cut(a, sep); ok {
			sides = []string{l, r}
			break
		}
	}
	for _, s := range sides {
		if s == "" {
			continue // A.. is A..HEAD
		}
		if rev, path, ok := strings.Cut(s, ":"); ok {
			if rev == "" {
				return fmt.Errorf("index blob selectors are not shown")
			}
			if err := repoPath(path); err != nil {
				return err
			}
			kind, err := gitRead(ctx, dir, "cat-file", "-t", s)
			if err != nil {
				return err
			}
			if strings.TrimSpace(kind) != "blob" {
				return fmt.Errorf("a path selector must name a blob, not a tree")
			}
		}
		if _, err := gitRead(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", s); err != nil {
			return err
		}
		if !strings.Contains(s, ":") {
			kind, err := gitRead(ctx, dir, "cat-file", "-t", s+"^{}")
			if err != nil {
				return err
			}
			if strings.TrimSpace(kind) == "blob" {
				return fmt.Errorf("a blob needs a checked repository path")
			}
		}
	}
	return nil
}

// repoPath refuses a path that is not inside the repository: an absolute one, one that climbs out, a pathspec's
// magic.
func repoPath(p string) error {
	clean := path.Clean(p)
	if loop.CredentialPath(clean) {
		return fmt.Errorf("credential path is not shown")
	}
	switch {
	case p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~"):
		return fmt.Errorf("%q is not a path inside the repository", p)
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return fmt.Errorf("%q climbs out of the repository", p)
	case strings.HasPrefix(p, ":"):
		return fmt.Errorf("%q is a pathspec's magic", p)
	}
	return nil
}

// gitRead runs one git command of momentum's own, with the safe settings.
func gitRead(ctx context.Context, dir string, args ...string) (string, error) {
	if err := loop.CheckGitConfig(ctx, dir); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append(append(append([]string{"-C", dir}, gitSafe...), loop.GitSafety()...), args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	return string(out), err
}

// gitEnv is momentum's environment without git's own variables (a GIT_DIR would point it elsewhere), never
// prompting, taking no optional lock and running no external diff.
func gitEnv() []string {
	env := loop.GitEnv()
	if os.Getenv("MOMENTUM_EVAL") != "" {
		if trial, err := filepath.EvalSymlinks(os.Getenv("MOMENTUM_LOOP_DIR")); err == nil {
			if trial, err = filepath.Abs(trial); err == nil {
				env = append(env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(trial))
			}
		}
	}
	return env
}

func gitNames() string {
	var out []string
	for _, n := range []string{"log", "show", "diff", "blame", "grep", "ls-files", "ls-tree", "rev-parse", "rev-list", "merge-base", "describe",
		"status", "shortlog", "branch", "tag"} {
		if _, ok := gitSpecs[n]; ok {
			out = append(out, n)
		}
	}
	return strings.Join(out, ", ")
}

// capped writes at most n bytes, and takes the rest without writing it.
type capped struct {
	w io.Writer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n <= 0 {
		return len(p), nil
	}
	k := min(len(p), c.n)
	c.n -= k
	if _, err := c.w.Write(p[:k]); err != nil {
		return 0, err
	}
	return len(p), nil
}

// gitPathWithin bounds live paths by the resolved work tree, including a missing path's existing parents.
func gitPathWithin(ctx context.Context, dir, name string) error {
	top, err := gitRead(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(top))
	if err != nil {
		return err
	}
	candidate := filepath.Join(dir, name)
	for {
		real, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			if !within(real, root) {
				return fmt.Errorf("path resolves outside the repository")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return err
		}
		candidate = parent
	}
}
