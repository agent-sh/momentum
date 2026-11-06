package loop

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// A repository the loop reads is read at its origin's default branch as the host fetches it at the pass, not at the
// checkout's own branch: nothing pulls a checkout, so it can lag its origin by many merged changes. The host fetches
// into a bare mirror of its own under the loop's storage, never into the checkout: the checkout's work tree, index,
// HEAD, refs and objects are not written, so the busy check, which reads a change there as someone at work, finds it
// as it was. The checkout's storage, configuration and origin pass the gate's own checks first (CheckGitConfig,
// originRepository), and every call has the input pins: no hooks, file-system monitor, optional locks or prompts.
// A checkout without an origin, or whose origin cannot be fetched now, is read at its own default branch, and the
// run's input summary says so.

// repoTip is where a snapshot reads a repository: the Git directory that holds its objects (origin's mirror, or the
// checkout), the branch and its commit, and, when the checkout's own branch was read instead of origin's, why.
type repoTip struct {
	dir, branch, commit string
	fallback            string
}

// sourceTip is the tip a snapshot of source reads: origin's default branch fetched into its mirror under mirrors, or,
// when that cannot be had, the checkout's own default branch with the reason. An error is a checkout without a
// default branch.
func sourceTip(ctx context.Context, source, mirrors, gh string) (repoTip, error) {
	tip, why := fetchTip(ctx, source, mirrors, gh)
	if why == "" {
		return tip, nil
	}
	if err := ctx.Err(); err != nil {
		return repoTip{}, err
	}
	branch, commit, err := defaultCommit(ctx, source)
	if err != nil {
		return repoTip{}, err
	}
	return repoTip{dir: source, branch: branch, commit: commit, fallback: why}, nil
}

// fetchTip fetches the default branch of source's origin into the origin's mirror and returns its tip, or why it
// could not (a text without a host path).
func fetchTip(ctx context.Context, source, mirrors, gh string) (repoTip, string) {
	refusal := func(err error) string {
		return "fetch refused: " + strings.TrimPrefix(reason(err), ErrRefused.Error()+": ")
	}
	failure := func(err error) string { return "fetch failed: " + reason(err) }
	if mirrors == "" {
		return repoTip{}, "fetch refused: no mirror storage"
	}
	if err := CheckGitConfig(ctx, source); err != nil {
		return repoTip{}, refusal(err)
	}
	remotes, err := gitOutput(ctx, source, "remote")
	if err != nil {
		return repoTip{}, refusal(err)
	}
	if !slices.Contains(strings.Fields(remotes), "origin") {
		return repoTip{}, "no origin"
	}
	if _, err := originRepository(ctx, source); err != nil {
		return repoTip{}, refusal(err)
	}
	raw, err := gitOutput(ctx, source, "remote", "get-url", "origin")
	if err != nil {
		return repoTip{}, refusal(err)
	}
	url, err := fetchURL(source, strings.TrimSpace(raw))
	if err != nil {
		return repoTip{}, refusal(err)
	}
	mirror, err := openMirror(ctx, mirrors, url)
	if err != nil {
		return repoTip{}, failure(err)
	}
	branch, err := originBranch(ctx, mirror, url, gh)
	if err != nil {
		return repoTip{}, failure(err)
	}
	if branch == "" {
		// a remote that does not name its HEAD: the branch the checkout takes for origin's default
		if branch, err = cloneDefaultBranch(ctx, source); err != nil {
			return repoTip{}, failure(err)
		}
	}
	if _, err := hostInputGit(ctx, "", "check-ref-format", "refs/heads/"+branch); err != nil {
		return repoTip{}, "fetch refused: origin's default branch has no valid name"
	}
	ref := "refs/heads/" + branch
	if _, err := fetchGit(ctx, mirror, gh, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		url, "+"+ref+":"+ref); err != nil {
		return repoTip{}, failure(err)
	}
	out, err := hostInputGit(ctx, "", "--git-dir="+mirror, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return repoTip{}, failure(err)
	}
	return repoTip{dir: mirror, branch: branch, commit: strings.TrimSpace(string(out))}, ""
}

// fetchURL is origin's URL as the mirror fetches it: a relative local path is made absolute from the checkout, and a
// URL Git could read as an option is refused.
func fetchURL(source, url string) (string, error) {
	switch {
	case url == "":
		return "", refused("origin names no URL")
	case strings.HasPrefix(url, "-") || strings.ContainsAny(url, "\x00\r\n"):
		return "", refused("origin's URL is not one the host fetches")
	}
	if localURL(url) && !filepath.IsAbs(url) {
		url = filepath.Join(source, url)
	}
	return url, nil
}

// localURL says whether a remote URL is a path on this machine: no scheme, and no host before a colon (Git's
// scp-like host:path form).
func localURL(url string) bool {
	if strings.Contains(url, "://") {
		return false
	}
	colon, slash := strings.IndexByte(url, ':'), strings.IndexByte(url, '/')
	return colon < 0 || slash >= 0 && slash < colon
}

// openMirror is the bare mirror of url under mirrors, made the first time without a template (no hooks, no sample
// files): only the configuration git init writes. One that is not a bare repository is made again.
func openMirror(ctx context.Context, mirrors, url string) (string, error) {
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		return "", err
	}
	if err := noInputLinks(mirrors); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(url))
	dir := filepath.Join(mirrors, fmt.Sprintf("%x.git", sum[:8]))
	info, err := os.Lstat(dir)
	switch {
	case err == nil && info.IsDir():
		if out, err := hostInputGit(ctx, "", "--git-dir="+dir, "rev-parse", "--is-bare-repository"); err == nil && strings.TrimSpace(string(out)) == "true" {
			return dir, nil
		}
		fallthrough
	case err == nil:
		if err := removeAll(dir); err != nil {
			return "", err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	tmp, err := os.MkdirTemp(mirrors, ".new-")
	if err != nil {
		return "", err
	}
	if _, err := hostInputGit(ctx, "", "init", "--quiet", "--bare", "--template=", tmp); err != nil {
		_ = removeAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = removeAll(tmp)
		return "", err
	}
	return dir, nil
}

// originBranch is the branch origin's HEAD names, or "" when origin does not say.
func originBranch(ctx context.Context, mirror, url, gh string) (string, error) {
	out, err := fetchGit(ctx, mirror, gh, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if branch, ok := strings.CutSuffix(rest, "\tHEAD"); ok && branch != "" {
				return branch, nil
			}
		}
	}
	return "", nil
}

// fetchGit runs a Git call that reads a remote, in the mirror, with the input pins and no prompt of any kind: no
// terminal, no askpass program and ssh in batch mode. Only https, ssh and local paths are fetched, and GitHub's
// credentials come from the GitHub CLI, as the ship tool's do. Its error names the operation and status, never Git's
// diagnostics, which can print a URL or a host path.
func fetchGit(ctx context.Context, mirror, gh string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, gitStepTimeout)
	defer cancel()
	pins := append([]string{"--git-dir=" + mirror}, GitSafety()...)
	pins = append(pins, "-c", "uploadpack.packObjectsHook=",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "protocol.file.allow=always",
		"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!"+shellQuote(firstOf(gh, "gh"))+" auth git-credential",
		"-c", "core.sshCommand=ssh -o BatchMode=yes")
	cmd := exec.CommandContext(cctx, "git", append(pins, args...)...)
	for _, kv := range GitEnv() {
		if !strings.HasPrefix(kv, "SSH_ASKPASS") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "SSH_ASKPASS_REQUIRE=never")
	out, stderr := Capture{Limit: ScrubBatchBytes}, Capture{Limit: ScrubBatchBytes}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("input git %s timed out after %s", gitVerb(args), gitStepTimeout)
		}
		return nil, fmt.Errorf("input git %s failed: %w", gitVerb(args), err)
	}
	if out.Over() || stderr.Over() {
		return nil, fmt.Errorf("input git output exceeds the input limit")
	}
	return out.Bytes(), nil
}
