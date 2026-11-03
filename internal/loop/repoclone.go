package loop

import (
	"errors"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RepoCloneOptions struct {
	Sources     []string
	Destination string
	LoopDirs    []string // caller's host-only loop storage roots
}

type RepoClone struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
	Error  string `json:"error,omitempty"`
}

// errNeedsScrubbing says the scrubber would change something a clone holds (an object, a ref or its configuration):
// such a repository is left out of the inputs and counted, never published.
var errNeedsScrubbing = errors.New("the scrubber would change what the clone holds")

// CloneRepos makes independent, history-preserving clones for /ro/repos/NAME.
// Git's --no-local transport copies objects, so source maintenance cannot change
// a run's objects. Each distinct source is attempted once; a refusal is a result.
// LoopDirs must contain the caller's loop storage roots. Objects are checked
// before publication; a scrubber hit refuses the clone, preserving commits.
func CloneRepos(ctx context.Context, o RepoCloneOptions) ([]RepoClone, error) {
	if err := CheckScrubber(ctx); err != nil {
		return nil, err
	}
	if o.Destination == "" {
		return nil, fmt.Errorf("repository destination is empty")
	}
	cleanDestination, err := RedactAll(ctx, []string{o.Destination})
	if err != nil {
		return nil, err
	}
	if cleanDestination[0] != o.Destination {
		return nil, fmt.Errorf("repository destination needs scrubbing")
	}
	if err := noInputLinks(filepath.Dir(o.Destination)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(o.Destination); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("repository destination is not a plain directory")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var sources []string
	seen, counts := map[string]bool{}, map[string]int{}
	for _, source := range o.Sources {
		path, err := filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		if !seen[path] {
			seen[path] = true
			sources = append(sources, path)
			counts[filepath.Base(path)]++
		}
	}
	var results []RepoClone
	for _, source := range sources {
		name := filepath.Base(source)
		if counts[name] > 1 {
			hash := sha256.Sum256([]byte(source))
			name += fmt.Sprintf("-%x", hash[:4])
		}
		result := RepoClone{Name: name, Source: source}
		cleanPaths, err := RedactAll(ctx, []string{name, source})
		if err != nil {
			return results, err
		}
		if cleanPaths[0] != name || cleanPaths[1] != source {
			result.Name, result.Source = cleanPaths[0], cleanPaths[1]
			result.Error = "repository paths need scrubbing"
			results = append(results, result)
			continue
		}
		if err := cloneSource(ctx, source, append(append([]string(nil), o.LoopDirs...), o.Destination)); err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		if err := os.MkdirAll(o.Destination, 0o700); err != nil {
			return results, err
		}
		result, err = cloneInput(ctx, source, filepath.Join(o.Destination, name), "--no-local")
		result.Name = name
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return results, nil
}

// CloneTacit makes the host-owned tacit fact clone at /ro/clone. Local cloning
// shares immutable Git objects by hardlink; the caller mounts it read-only.
func CloneTacit(ctx context.Context, src, dst string) (RepoClone, error) {
	result := RepoClone{Source: src, Name: filepath.Base(src)}
	if err := CheckScrubber(ctx); err != nil {
		return result, err
	}
	if src == "" || dst == "" {
		return result, fmt.Errorf("tacit clone needs source and destination paths")
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return result, err
	}
	dst, err = filepath.Abs(dst)
	if err != nil {
		return result, err
	}
	result.Source, result.Name = src, filepath.Base(src)
	cleanPaths, err := RedactAll(ctx, []string{src, dst})
	if err != nil {
		return result, err
	}
	if cleanPaths[0] != src || cleanPaths[1] != dst {
		result.Source = cleanPaths[0]
		result.Name = filepath.Base(cleanPaths[0])
		result.Error = "tacit paths need scrubbing"
		return result, fmt.Errorf("%s", result.Error)
	}
	if err := cloneSource(ctx, src, []string{dst}); err != nil {
		return result, err
	}
	result, err = cloneInput(ctx, src, dst, "--local")
	if err != nil {
		result.Error = err.Error()
	}
	return result, err
}

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

func cloneInput(ctx context.Context, source, destination, mode string) (r RepoClone, err error) {
	r = RepoClone{Source: source, Name: filepath.Base(destination)}
	if err = absentInput(destination); err != nil {
		return r, err
	}
	if err = noInputLinks(filepath.Dir(destination)); err != nil {
		return r, err
	}
	branch, err := cloneDefaultBranch(ctx, source)
	if err != nil {
		return r, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), ".repo-input-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(tmp)
	clone := filepath.Join(tmp, "clone")
	if _, err = hostInputGit(ctx, "", "clone", mode, "--no-checkout", "--", source, clone); err != nil {
		return r, err
	}
	// Remove inherited remote reach and sample hooks before checkout.
	if _, err = hostInputGit(ctx, clone, "remote", "remove", "origin"); err != nil {
		return r, err
	}
	if err = os.RemoveAll(filepath.Join(clone, ".git", "hooks")); err != nil {
		return r, err
	}
	// --no-checkout may have made a local branch only for the source's HEAD.
	// Resolve the default branch in the source and detach at that immutable id,
	// then name it locally without depending on removed remote-tracking refs.
	ref := "refs/heads/" + branch
	commit, err := hostInputGit(ctx, source, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		ref = "refs/remotes/origin/" + branch
		commit, err = hostInputGit(ctx, source, "rev-parse", "--verify", ref+"^{commit}")
	}
	if err != nil {
		return r, err
	}
	r.Commit = strings.TrimSpace(string(commit))
	if _, err = hostInputGit(ctx, clone, "cat-file", "-e", r.Commit+"^{commit}"); err != nil {
		// A remote-tracking default absent from local heads may not have been
		// transferred. Fetch that ref from the same local source, with no remote.
		if _, err = hostInputGit(ctx, clone, "fetch", "--no-tags", "--no-write-fetch-head", "--", source, ref); err != nil {
			return r, err
		}
	}
	if _, err = hostInputGit(ctx, clone, "checkout", "-B", branch, r.Commit, "--"); err != nil {
		return r, err
	}
	got, err := hostInputGit(ctx, clone, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(got)) != r.Commit {
		return r, fmt.Errorf("clone commit changed during preparation")
	}
	r.Branch = branch
	// Reflogs are local bookkeeping, not repository history. Do not export
	// identities that Git generated on this host while preparing the clone.
	if err = os.RemoveAll(filepath.Join(clone, ".git", "logs")); err != nil {
		return r, err
	}
	if err = checkCloneObjects(ctx, clone); err != nil {
		return r, err
	}
	if err = os.Rename(clone, destination); err != nil {
		return r, err
	}
	r.Path = destination
	return r, nil
}

// checkCloneObjects preserves commits by refusing an object that needs masking.
// It checks all objects, including unreachable objects copied by --local.
// Trees expose names, not binary object ids. Other objects are scanned whole,
// including binary blobs; an object too large to scrub is refused.
func checkCloneObjects(ctx context.Context, dir string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
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
	cctx, cancel := context.WithTimeout(ctx, gitStepTimeout)
	defer cancel()
	pins := append(append([]string(nil), GitSafety()...), "-c", "uploadpack.packObjectsHook=", "-c", "protocol.file.allow=always")
	if dir != "" {
		pins = append([]string{"-C", dir}, pins...)
	}
	cmd := exec.CommandContext(cctx, "git", append(pins, args...)...)
	cmd.Env = GitEnv()
	out, stderr := Capture{Limit: ScrubBatchBytes}, Capture{Limit: ScrubBatchBytes}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		// Git can print source configuration and object text in diagnostics.
		// Return only the operation and status, never its raw stderr.
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("input git %s timed out after %s", args[0], gitStepTimeout)
		}
		return nil, fmt.Errorf("input git %s failed: %w", args[0], err)
	}
	if out.Over() || stderr.Over() {
		return nil, fmt.Errorf("input git output exceeds the input limit")
	}
	return out.Bytes(), nil
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
