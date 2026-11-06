package loop

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// errNeedsScrubbing says the scrubber would change something a clone holds (an object, a ref or its configuration).
// An evaluation's as-of clones keep history, so it still leaves such a repository out; a live run gets a scrubbed
// snapshot of files and history as text instead (snapshot.go) and masks what the scrubber changes.
var errNeedsScrubbing = errors.New("clone object needs scrubbing")

func cloneSource(ctx context.Context, source string, own []string) error {
	if err := noInputLinks(source); err != nil {
		return fmt.Errorf("source is not a plain work tree: %w", err)
	}
	for _, root := range own {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, source)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return fmt.Errorf("source lies inside loop input storage")
		}
	}
	worktree, err := hostInputGit(ctx, source, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(worktree)) != "true" {
		return fmt.Errorf("source is not a git work tree")
	}
	return nil
}

// checkCloneObjects preserves commits by refusing an object that needs masking.
// It checks all objects, including unreachable objects copied by --local.
// Trees expose names, not binary object ids. Other objects are scanned whole,
// including binary blobs; an object too large to scrub is refused.
func checkCloneObjects(ctx context.Context, dir string) error {
	if err := checkObjects(ctx, dir); err != nil {
		return err
	}
	// Names of refs and the generated Git configuration are exported too.
	refs, err := hostInputGit(ctx, dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		return err
	}
	clean, err := RedactAll(ctx, []string{string(refs), string(config)})
	if err != nil {
		return err
	}
	if clean[0] != string(refs) || clean[1] != string(config) {
		return errNeedsScrubbing
	}
	return nil
}

// checkObjects is the scan of every object of the repository at dir (see checkCloneObjects), without its
// configuration: a build mounts a clone of a work tree whose own configuration is not part of the clone.
func checkObjects(ctx context.Context, dir string) (result error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	defer func() {
		if result != nil && (ctx.Err() != nil || cctx.Err() == context.DeadlineExceeded) {
			result = fmt.Errorf("the object scan ran out of time or was cancelled")
		}
	}()
	cmd := exec.CommandContext(cctx, "git", append(append([]string{"-C", dir}, GitSafety()...),
		"cat-file", "--batch-all-objects", "--batch")...)
	cmd.Env = GitEnv()
	stderr := Capture{Limit: ScrubBatchBytes}
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("clone object reader failed")
	}
	// On an early refusal, close and cancel before waiting for the producer.
	defer func() { cancel(); pipe.Close(); cmd.Wait() }()
	reader := bufio.NewReader(pipe)
	var batch []string
	size := 0
	flush := func() error {
		clean, err := RedactAll(cctx, batch)
		if err != nil {
			if cctx.Err() != nil {
				return fmt.Errorf("the object scan ran out of time or was cancelled")
			}
			return err
		}
		for i, text := range batch {
			if clean[i] != strings.ReplaceAll(text, "\x1e", " ") {
				return errNeedsScrubbing
			}
		}
		batch, size = nil, 0
		return nil
	}
	for {
		header, err := reader.ReadString('\n')
		if err == io.EOF && header == "" {
			break
		}
		if err != nil || len(header) > 128 {
			return fmt.Errorf("invalid clone object header")
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			return fmt.Errorf("invalid clone object header")
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || n < 0 || n > ScrubBatchBytes {
			return fmt.Errorf("a repository object over %d MiB cannot be scrubbed in one piece, so the repository is left out: %w", ScrubBatchBytes>>20, errNeedsScrubbing)
		}
		data := make([]byte, n)
		if _, err = io.ReadFull(reader, data); err != nil {
			return fmt.Errorf("incomplete clone object")
		}
		if end, err := reader.ReadByte(); err != nil || end != '\n' {
			return fmt.Errorf("invalid clone object boundary")
		}
		text := string(data)
		if fields[1] == "tree" {
			var names []string
			for len(data) > 0 {
				space := bytes.IndexByte(data, ' ')
				nul := bytes.IndexByte(data, 0)
				hashBytes := len(fields[0]) / 2
				if (hashBytes != 20 && hashBytes != 32) || space < 0 || nul < space || len(data) < nul+1+hashBytes {
					return fmt.Errorf("invalid clone tree")
				}
				names = append(names, string(data[space+1:nul]))
				data = data[nul+1+hashBytes:]
			}
			text = strings.Join(names, "\n")
		}
		if size+len(text)+1 > ScrubBatchBytes && len(batch) > 0 {
			if err = flush(); err != nil {
				return err
			}
		}
		batch = append(batch, text)
		size += len(text) + 1
	}
	if err = flush(); err != nil {
		return err
	}
	if err = cmd.Wait(); err != nil || stderr.Over() {
		return fmt.Errorf("clone object reader failed")
	}
	return nil
}

func cloneDefaultBranch(ctx context.Context, source string) (string, error) {
	if head, err := hostInputGit(ctx, source, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		branch := strings.TrimPrefix(strings.TrimSpace(string(head)), "refs/remotes/origin/")
		if _, err = hostInputGit(ctx, source, "rev-parse", "--verify", "refs/remotes/origin/"+branch+"^{commit}"); err == nil {
			return branch, nil
		}
		return "", fmt.Errorf("source's default branch is not available")
	}
	for _, branch := range []string{"main", "master"} {
		if _, err := hostInputGit(ctx, source, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			return branch, nil
		}
	}
	head, err := hostInputGit(ctx, source, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", fmt.Errorf("source has no default branch")
	}
	return strings.TrimPrefix(strings.TrimSpace(string(head)), "refs/heads/"), nil
}

// gitStepTimeout bounds one git step of an input copy: a large repository's clone takes minutes.
const gitStepTimeout = 5 * time.Minute

func hostInputGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return hostInputGitLimit(ctx, dir, ScrubBatchBytes, args...)
}

// hostInputGitLimit is hostInputGit for output that may exceed one scrub batch (a listing of a large tree).
func hostInputGitLimit(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	out, over, err := hostInputGitCapped(ctx, dir, limit, args...)
	if err == nil && over {
		return nil, fmt.Errorf("input git output exceeds the input limit")
	}
	return out, err
}

// hostInputGitCapped returns at most limit bytes of output and says whether there was more.
func hostInputGitCapped(ctx context.Context, dir string, limit int, args ...string) ([]byte, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, gitStepTimeout)
	defer cancel()
	pins := append(append([]string(nil), GitSafety()...), "-c", "uploadpack.packObjectsHook=", "-c", "protocol.file.allow=always")
	if dir != "" {
		pins = append([]string{"-C", dir}, pins...)
	}
	cmd := exec.CommandContext(cctx, "git", append(pins, args...)...)
	cmd.Env = GitEnv()
	endWithContext(cmd)
	out, stderr := Capture{Limit: limit}, Capture{Limit: ScrubBatchBytes}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		// Git can print source configuration and object text in diagnostics.
		// Return only the operation and status, never its raw stderr.
		if cctx.Err() == context.DeadlineExceeded {
			return nil, false, fmt.Errorf("input git %s timed out after %s", gitVerb(args), gitStepTimeout)
		}
		return nil, false, fmt.Errorf("input git %s failed: %w", gitVerb(args), err)
	}
	if stderr.Over() {
		return nil, false, fmt.Errorf("input git output exceeds the input limit")
	}
	if out.Over() {
		return out.Bytes()[:limit], true, nil
	}
	return out.Bytes(), false, nil
}

func noInputLinks(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains a link")
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// gitVerb is the subcommand of an argument list, past the global options.
func gitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return "command"
}
