package loop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// A repository the loop reads is read at its origin's default branch as the host fetches it at the pass, not at the
// checkout's own branch: nothing pulls a checkout, so it can lag its origin by many merged changes. The host fetches
// into a bare mirror of its own under the loop's storage, one per repository, never into the checkout: the checkout's
// work tree, index, HEAD, refs and objects are not written, so the busy check, which reads a change there as someone
// at work, finds it as it was. Of the checkout the host reads only its storage (CheckHostStorage) and its origin's
// URLs, from its own configuration file without includes (readOrigin); no setting of the checkout's applies to the
// fetch. A GitHub repository is fetched over https by its name, with the GitHub CLI's credentials. The fetch takes the
// history window the log and the diffs read, and every call has the input pins and ends with its context, whatever
// its ssh or https helper does (endWithContext). When the fetch fails, the run reads the branch as the mirror last
// fetched it, and without a mirror, or an origin to read, the checkout's own default branch; the input summary says
// which and why.

// mirrorMax bounds a mirror's size on disk after a fetch. A mirror holds the tree at the branch's tip and the history
// window's commits. A snapshot reads at most SnapshotRepoMax of that tree's text; the binaries and large files it
// leaves out are fetched all the same, and a mirror never collects garbage (gc.auto=0), so the objects of earlier
// windows stay in it. Four times what a snapshot can read leaves room for both; past it the mirror holds mostly what
// no run reads, so it is dropped and the next pass fetches the window into an empty one.
var mirrorMax int64 = 4 * SnapshotRepoMax

// fetchLimit bounds the whole fetch of one repository (origin's HEAD, the window and its parents): an input step's
// limit, since a large repository's first fetch takes minutes.
const fetchLimit = gitStepTimeout

// gitWaitDelay bounds the wait for a Git call's output once its context ended and its process group was killed.
const gitWaitDelay = 5 * time.Second

// remoteSSH is the ssh every Git call of the host's that reaches a remote runs: it never prompts, gives up on a server
// that does not answer within 30 seconds, and ends a connection the server leaves silent for 45 (three keepalives 15
// seconds apart).
const remoteSSH = "ssh -o BatchMode=yes -o ConnectTimeout=30 -o ServerAliveInterval=15 -o ServerAliveCountMax=3"

// remotePins are the transfer limits of every Git call of the host's that reaches a remote: remoteSSH, and an https
// transfer that stays under 1000 bytes a second for 60 seconds ends.
func remotePins() []string {
	return []string{"-c", "core.sshCommand=" + remoteSSH, "-c", "http.lowSpeedLimit=1000", "-c", "http.lowSpeedTime=60"}
}

// githubURL is where a GitHub repository is fetched from: over https with the GitHub CLI's credentials, never over
// ssh, so no ssh agent is asked to confirm a key. A test points it at a local repository.
var githubURL = func(repo string) string { return "https://github.com/" + repo + ".git" }

// mirrorMetaFile, in a mirror, says which checkouts read it and when its branch was last fetched.
const mirrorMetaFile = "momentum-mirror.json"

type mirrorMeta struct {
	Sources []string  `json:"sources"`
	Branch  string    `json:"branch,omitempty"` // origin's default branch at the last complete fetch
	Commit  string    `json:"commit,omitempty"` // its tip then
	Fetched time.Time `json:"fetched,omitzero"`
}

// repoTip is where a snapshot reads a repository: the Git directory that holds its objects (origin's mirror, or the
// checkout), the branch and its commit, and, when that is not origin's branch fetched now, which it is and why.
type repoTip struct {
	dir, branch, commit string
	fallback            string
}

// origin is the repository a checkout's origin names, by its identity (OWNER/REPO on GitHub, else url: and its URL),
// and the URL the mirror fetches it from.
type origin struct{ identity, url string }

// sourceTip is the tip a snapshot of source reads, and the mirror of its origin ("" when its origin could not be
// read): origin's default branch fetched now; when the fetch fails, the branch as the mirror last fetched it; when
// there is no such mirror, or no origin to read, the checkout's own default branch. A fallback says which and why. An
// error is a checkout without a default branch, or a pass told to stop.
func sourceTip(ctx context.Context, source, mirrors, gh string, since time.Time) (repoTip, string, error) {
	org, why := readOrigin(ctx, source)
	mirror := ""
	if why == "" {
		mirror = mirrorPath(mirrors, org.identity)
		var tip repoTip
		if tip, why = fetchTip(ctx, source, mirror, org.url, gh, since); why == "" {
			return tip, mirror, nil
		}
		if err := ctx.Err(); err != nil {
			return repoTip{}, mirror, err
		}
		if last, at, ok := lastFetch(ctx, mirror); ok {
			last.fallback = fmt.Sprintf("origin's %s as fetched at %s: %s", last.branch, at.UTC().Format("2006-01-02 15:04Z"), why)
			return last, mirror, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return repoTip{}, mirror, err
	}
	branch, commit, err := defaultCommit(ctx, source)
	if err != nil {
		return repoTip{}, mirror, err
	}
	return repoTip{dir: source, branch: branch, commit: commit, fallback: fmt.Sprintf("checkout's %s: %s", branch, why)}, mirror, nil
}

// readOrigin reads the checkout's origin from its own configuration file, without includes: no setting of the
// checkout's runs or applies, since the fetch runs in the mirror with the host's pins. Its storage passes the host's
// check first (CheckHostStorage). A configuration that rewrites URLs (url.*.insteadOf or url.*.pushInsteadOf) is
// refused, since the URLs read would not be the ones Git uses there, and so are fetch and push URLs that name different
// repositories. A reason names a setting's section, never its key or a URL, either of which can hold a token.
func readOrigin(ctx context.Context, source string) (origin, string) {
	unreadable := "fetch refused: the checkout's Git configuration cannot be read"
	if err := CheckHostStorage(ctx, source); err != nil {
		return origin{}, "fetch refused: " + strings.TrimPrefix(reason(err), ErrRefused.Error()+": ")
	}
	names, err := hostConfig(ctx, source, "--null", "--name-only", "--list")
	if err != nil {
		return origin{}, unreadable
	}
	for _, name := range strings.Split(names, "\x00") {
		key := strings.ToLower(name)
		if strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof")) {
			return origin{}, "fetch refused: the checkout's Git configuration rewrites remote URLs (its url section)"
		}
	}
	fetches, err := configValues(ctx, source, "remote.origin.url")
	if err != nil {
		return origin{}, unreadable
	}
	if len(fetches) == 0 {
		return origin{}, "no origin"
	}
	pushes, err := configValues(ctx, source, "remote.origin.pushurl")
	if err != nil {
		return origin{}, unreadable
	}
	if len(pushes) == 0 {
		pushes = fetches // a push goes to every URL
	}
	identity := remoteIdentity(fetches[0])
	for _, url := range pushes {
		if remoteIdentity(url) != identity {
			return origin{}, "fetch refused: origin's fetch and push destinations differ"
		}
	}
	if !strings.HasPrefix(identity, "url:") {
		return origin{identity: identity, url: githubURL(identity)}, ""
	}
	url, err := fetchURL(source, strings.TrimSpace(fetches[0]))
	if err != nil {
		return origin{}, "fetch refused: " + strings.TrimPrefix(reason(err), ErrRefused.Error()+": ")
	}
	return origin{identity: "url:" + url, url: url}, "" // a relative path names its repository once made absolute
}

// configValues are a setting's values in dir's own configuration file, without includes; none when it is not set.
func configValues(ctx context.Context, dir, key string) ([]string, error) {
	out, err := hostConfig(ctx, dir, "--null", "--get-all", key)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var values []string
	for _, v := range strings.Split(out, "\x00") {
		if v != "" {
			values = append(values, v)
		}
	}
	return values, nil
}

// fetchURL is a non-GitHub origin's URL as the mirror fetches it: https, ssh or a local path, with a relative path made
// absolute from the checkout. Any other protocol, a remote helper's transport::address and a URL Git could read as an
// option are refused.
func fetchURL(source, url string) (string, error) {
	switch {
	case url == "":
		return "", refused("origin names no URL")
	case strings.HasPrefix(url, "-") || strings.ContainsAny(url, "\x00\r\n"):
		return "", refused("origin's URL is not one the host fetches")
	}
	if i := strings.Index(url, "::"); i > 0 && !strings.ContainsAny(url[:i], ":/[@") {
		return "", refused("origin's URL names a remote helper, which the host does not run")
	}
	if scheme, _, ok := strings.Cut(url, "://"); ok {
		switch strings.ToLower(scheme) {
		case "https", "ssh", "file":
			return url, nil
		}
		return "", refused("origin's URL is not https, ssh or a local path, all the host fetches")
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

// mirrorPath is the mirror of a repository under mirrors, named by the repository's identity: every checkout of one
// repository, by any of its URLs, shares it.
func mirrorPath(mirrors, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(mirrors, fmt.Sprintf("%x.git", sum[:8]))
}

// fetchTip fetches the default branch of source's origin into its mirror, within fetchLimit, and returns its tip, or
// why it could not (a text without a host path or a URL).
func fetchTip(ctx context.Context, source, mirror, url, gh string, since time.Time) (repoTip, string) {
	failure := func(err error) string { return "fetch failed: " + reason(err) }
	fctx, cancel := withLimit(ctx, "fetch", fetchLimit)
	defer cancel()
	if err := openMirror(fctx, mirror, source); err != nil {
		return repoTip{}, failure(err)
	}
	branch, err := originBranch(fctx, mirror, url, gh)
	if err != nil {
		return repoTip{}, failure(err)
	}
	if branch == "" {
		// a remote that does not name its HEAD: the branch the checkout takes for origin's default
		if branch, err = cloneDefaultBranch(fctx, source); err != nil {
			return repoTip{}, failure(err)
		}
	}
	if _, err := hostInputGit(fctx, "", "check-ref-format", "refs/heads/"+branch); err != nil {
		return repoTip{}, "fetch refused: origin's default branch has no valid name"
	}
	ref := "refs/heads/" + branch
	if err := fetchWindow(fctx, mirror, url, gh, ref, since); err != nil {
		return repoTip{}, failure(err)
	}
	size, err := treeSize(mirror)
	if err == nil && size > mirrorMax {
		err = removeAll(mirror)
		if err == nil {
			return repoTip{}, fmt.Sprintf("fetch refused: the mirror passed %d MiB, so it was dropped; the next pass fetches into an empty one", mirrorMax>>20)
		}
	}
	if err != nil {
		return repoTip{}, failure(err)
	}
	out, err := hostInputGit(fctx, "", "--git-dir="+mirror, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return repoTip{}, failure(err)
	}
	commit := strings.TrimSpace(string(out))
	if err := updateMirrorMeta(mirror, func(m *mirrorMeta) { m.Branch, m.Commit, m.Fetched = branch, commit, time.Now() }); err != nil {
		return repoTip{}, failure(err)
	}
	return repoTip{dir: mirror, branch: branch, commit: commit}, ""
}

// fetchWindow fetches ref of url into the mirror with the history the log and the diffs read: the commits since the
// window's start, and the parents of its oldest, so their changes are their own and not a whole tree's. A branch
// without a commit in the window comes as its tip alone. Without a window start the whole history comes.
func fetchWindow(ctx context.Context, mirror, url, gh, ref string, since time.Time) error {
	fetch := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules"}
	spec := "+" + ref + ":" + ref
	if since.IsZero() {
		_, err := fetchGit(ctx, mirror, gh, append(fetch, url, spec)...)
		return err
	}
	if _, err := fetchGit(ctx, mirror, gh, append(fetch, "--shallow-since="+since.UTC().Format(time.RFC3339), url, spec)...); err != nil {
		if ctx.Err() != nil {
			return err
		}
		// no commit since the window's start: Git selects none, so the tip alone
		_, err = fetchGit(ctx, mirror, gh, append(fetch, "--depth=1", url, spec)...)
		return err
	}
	_, err := fetchGit(ctx, mirror, gh, append(fetch, "--deepen=1", url, spec)...)
	return err
}

// openMirror makes sure dir is a usable mirror (usableMirror), making it again without a template (no hooks, no
// sample files) when it is not, and records source among the checkouts that read it.
func openMirror(ctx context.Context, dir, source string) error {
	mirrors := filepath.Dir(dir)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		return err
	}
	if err := noInputLinks(mirrors); err != nil {
		return err
	}
	if !usableMirror(ctx, dir) {
		if err := removeAll(dir); err != nil {
			return err
		}
		tmp, err := os.MkdirTemp(mirrors, ".new-")
		if err != nil {
			return err
		}
		if _, err := hostInputGit(ctx, "", "init", "--quiet", "--bare", "--template=", tmp); err != nil {
			_ = removeAll(tmp)
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			_ = removeAll(tmp)
			return err
		}
	}
	return updateMirrorMeta(dir, func(m *mirrorMeta) {
		if !slices.Contains(m.Sources, source) {
			m.Sources = append(m.Sources, source)
			slices.Sort(m.Sources)
		}
	})
}

// usableMirror says whether dir is a mirror as the host makes it: a bare repository of plain files and directories,
// whose configuration names no program, rewrite or include (CheckGitConfigWith, on its own file).
func usableMirror(ctx context.Context, dir string) bool {
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return false
	}
	if filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && !e.Type().IsRegular() {
			err = errors.New("not a plain file")
		}
		return err
	}) != nil {
		return false
	}
	out, err := hostInputGit(ctx, "", "--git-dir="+dir, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return false
	}
	return CheckGitConfigWith(func(args ...string) ([]byte, error) { return mirrorConfig(ctx, dir, args...) }) == nil
}

// mirrorConfig reads the mirror's own configuration file and nothing else.
func mirrorConfig(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"config", "--file", filepath.Join(dir, "config"), "--no-includes"}, args...)...)
	cmd.Env = GitEnv()
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return nil, refused("configuration output exceeds its limit")
	}
	return out.Bytes(), err
}

// lastFetch is the branch as the mirror's last complete fetch left it, and when that was: a fetch that failed part way
// may have moved the branch's ref, never the record.
func lastFetch(ctx context.Context, mirror string) (repoTip, time.Time, bool) {
	if !usableMirror(ctx, mirror) {
		return repoTip{}, time.Time{}, false
	}
	m, err := readMirrorMeta(mirror)
	if err != nil || m.Branch == "" || m.Fetched.IsZero() || !commitName.MatchString(m.Commit) {
		return repoTip{}, time.Time{}, false
	}
	out, err := hostInputGit(ctx, "", "--git-dir="+mirror, "rev-parse", "--verify", m.Commit+"^{commit}")
	if err != nil || strings.TrimSpace(string(out)) != m.Commit {
		return repoTip{}, time.Time{}, false
	}
	return repoTip{dir: mirror, branch: m.Branch, commit: m.Commit}, m.Fetched, true
}

var commitName = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func readMirrorMeta(mirror string) (mirrorMeta, error) {
	var m mirrorMeta
	raw, err := os.ReadFile(filepath.Join(mirror, mirrorMetaFile))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(raw, &m)
}

// updateMirrorMeta changes the mirror's record in one rename.
func updateMirrorMeta(mirror string, change func(*mirrorMeta)) error {
	m, err := readMirrorMeta(mirror)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		m = mirrorMeta{} // a record that cannot be read is written again
	}
	change(&m)
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(mirror, "."+mirrorMetaFile+".new")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(mirror, mirrorMetaFile))
}

// pruneMirrors removes the mirrors that no checkout of the pass reads: a mirror a source's origin named this pass
// stays (used), and so does one that records a source whose origin could not be read this pass (unread). Any other
// mirror, and anything under mirrors that is not one, goes.
func pruneMirrors(mirrors string, used map[string]bool, unread []string) {
	entries, err := os.ReadDir(mirrors)
	if err != nil {
		return // none yet; or unreadable, and the next pass tries again
	}
	for _, e := range entries {
		dir := filepath.Join(mirrors, e.Name())
		if used[dir] {
			continue
		}
		if m, err := readMirrorMeta(dir); err == nil && slices.ContainsFunc(m.Sources, func(s string) bool { return slices.Contains(unread, s) }) {
			continue
		}
		_ = removeAll(dir) // what cannot be removed now is tried again by the next pass
	}
}

// treeSize is the bytes of the regular files under dir.
func treeSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}
		info, err := e.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	return n, err
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

// fetchGit runs a Git call that reads a remote, in the mirror, with the input pins, the remote limits (remotePins) and
// no prompt of any kind: no terminal, no stdin, no askpass program, ssh in batch mode. Only https, ssh and local paths
// are fetched, and GitHub's credentials come from the GitHub CLI, as the ship tool's do. The call ends with ctx
// (endWithContext). Its error names the operation and which limit ended it, never Git's diagnostics, which can print
// a URL or a host path.
func fetchGit(ctx context.Context, mirror, gh string, args ...string) ([]byte, error) {
	pins := append([]string{"--git-dir=" + mirror}, GitSafety()...)
	pins = append(pins, "-c", "uploadpack.packObjectsHook=",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "protocol.file.allow=always",
		"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!"+shellQuote(firstOf(gh, "gh"))+" auth git-credential")
	pins = append(pins, remotePins()...)
	cmd := exec.CommandContext(ctx, "git", append(pins, args...)...)
	for _, kv := range GitEnv() {
		if !strings.HasPrefix(kv, "SSH_ASKPASS") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "SSH_ASKPASS_REQUIRE=never")
	endWithContext(cmd)
	out, stderr := Capture{Limit: ScrubBatchBytes}, Capture{Limit: ScrubBatchBytes}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, remoteFailure(ctx, gitVerb(args), err, stderr.Bytes())
	}
	if out.Over() || stderr.Over() {
		return nil, fmt.Errorf("input git output exceeds the input limit")
	}
	return out.Bytes(), nil
}

// remoteFailure is a failed remote call's cause as a fixed text: the deadline that ended it, or the transfer limit
// that did. Git's diagnostics are matched, never shown.
func remoteFailure(ctx context.Context, verb string, err error, stderr []byte) error {
	if ctx.Err() != nil {
		return fmt.Errorf("input git %s: %s", verb, endedBy(ctx))
	}
	switch text := strings.ToLower(string(stderr)); {
	case strings.Contains(text, "operation too slow"):
		return fmt.Errorf("input git %s: the transfer stayed under 1000 bytes a second for 60s", verb)
	case strings.Contains(text, "not responding"):
		return fmt.Errorf("input git %s: the ssh server was silent for 45s", verb)
	case strings.Contains(text, "timed out"):
		return fmt.Errorf("input git %s: the connection timed out", verb)
	}
	return fmt.Errorf("input git %s failed: %w", verb, err)
}

// endWithContext makes a Git call end with its context, whatever its children do: git starts in a process group of
// its own, the context's end kills the whole group (git and the ssh or https helper it started, which would hold its
// output open past git's own end), and the wait for that output is bounded.
func endWithContext(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = gitWaitDelay
}

// stepLimit is the cause of a context its step's own limit ended, so an error can say which deadline fired.
type stepLimit struct {
	what  string
	limit time.Duration
}

func (l stepLimit) Error() string { return fmt.Sprintf("the %s's %s limit passed", l.what, l.limit) }

// withLimit is ctx bounded by a step's own limit.
func withLimit(ctx context.Context, what string, limit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, limit, stepLimit{what, limit})
}

// endedBy says which deadline ended a context: a step's own limit (withLimit), the caller's deadline, or the caller's
// stop.
func endedBy(ctx context.Context) string {
	var limit stepLimit
	switch {
	case errors.As(context.Cause(ctx), &limit):
		return limit.Error()
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "the caller's deadline passed"
	}
	return "the caller stopped it"
}
