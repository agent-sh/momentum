package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
)

// readGit are the git commands evidence git runs: the ones that read a repository's history and state. Each
// takes the arguments it is given but those that write or run something (gitWrites); the commands that also
// write unless told to list (branch, tag, remote, config, worktree, reflog) take only their listing forms.
var readGit = map[string]bool{"log": true, "show": true, "diff": true, "blame": true, "status": true, "grep": true, "ls-files": true,
	"rev-parse": true, "shortlog": true, "describe": true, "ls-tree": true, "cat-file": true, "rev-list": true, "merge-base": true,
	"name-rev": true, "whatchanged": true}

var listingGit = map[string][]string{"branch": {"-a", "-r", "-v", "-vv", "--list", "--contains", "--merged", "--no-merged", "--all", "--show-current"},
	"tag": {"-l", "--list", "--contains", "--points-at", "-n"}, "remote": {"-v", "get-url", "show"},
	"config": {"--get", "--get-all", "--list", "-l", "--get-regexp"}, "worktree": {"list", "--porcelain"}, "reflog": {"show", "-n"}}

// takesValue are the listing flags and words whose next argument is their value.
var takesValue = []string{"--contains", "--merged", "--no-merged", "--points-at", "get-url", "show", "--get", "--get-all", "--get-regexp", "-n"}

// gitWrites are the arguments that would make a reading command write a file, run a program, reach a remote, or
// read a file outside the repository (--no-index, blame's --contents), past the harness's rules for reading.
var gitWrites = []string{"--output", "-o", "--ext-diff", "--textconv", "--exec", "--upload-pack", "--receive-pack", "--open-files-in-pager",
	"-O", "--git-dir", "--work-tree", "-c", "--config-env", "--no-index", "--contents", "--file"}

// evidenceGit runs a reading git command in a repository: the improvement loop reads history through it, since
// its harness lets it run no git of its own.
func evidenceGit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return usageError{"evidence git takes a repository's directory, then a git command that reads (log, show, diff, blame, ...)"}
	}
	dir, sub, rest := paths.Expand(args[0]), args[1], args[2:]
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return usageError{fmt.Sprintf("%s is no directory", dir)}
	}
	switch {
	case readGit[sub]:
		for _, a := range rest {
			for _, w := range gitWrites {
				if a == w || strings.HasPrefix(a, w+"=") || (len(w) == 2 && strings.HasPrefix(a, w) && !strings.HasPrefix(a, "--")) {
					return usageError{fmt.Sprintf("git %s %s writes or runs something: evidence git only reads", sub, a)}
				}
			}
		}
	case listingGit[sub] != nil:
		// only the listing forms: the flags that list, and a value after one that takes it
		allowed := listingGit[sub]
		for i, a := range rest {
			flag, _, _ := strings.Cut(a, "=")
			if slices.Contains(allowed, flag) {
				continue
			}
			if i > 0 && !strings.HasPrefix(a, "-") && slices.Contains(takesValue, rest[i-1]) {
				continue
			}
			return usageError{fmt.Sprintf("git %s %s is not a listing: evidence git only reads", sub, a)}
		}
	default:
		return usageError{fmt.Sprintf("git %s is not a command evidence git runs: it runs the ones that read", sub)}
	}
	if sub == "log" || sub == "show" || sub == "diff" || sub == "whatchanged" {
		rest = append([]string{"--no-ext-diff", "--no-textconv"}, rest...)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"--no-pager", "-C", dir, sub}, rest...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_EXTERNAL_DIFF=")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &capped{w: &out, n: 1 << 20}, stderr
	err := cmd.Run()
	stdout.Write(out.Bytes())
	if out.Len() >= 1<<20 {
		fmt.Fprintln(stderr, "momentum: the output was cut at 1 MiB; narrow the command")
	}
	return err
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
