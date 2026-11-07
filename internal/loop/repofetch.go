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
// fetch. A GitHub repository is fetched over https by its name, with the GitHub CLI's credentials. A mirror holds the
// default branch's whole history: its first fetch carries all of it, a later one only what is new, and the log and
// the diffs read the window from it. Every call has the input pins and ends with its context, whatever its ssh or
// https helper does (endWithContext). When the fetch fails, the run reads the branch as the mirror last fetched it,
// and without a mirror, or an origin to read, the checkout's own default branch; the input summary and the run's
// prompt say which and why.

// fetchLimit bounds a repository's fetch (origin's HEAD and the branch's new objects) once its mirror holds a complete
// fetch: an input step's limit, as a local read of a large repository has.
const fetchLimit = gitStepTimeout

// firstFetchLimit bounds a mirror's first fetch, which carries the branch's whole history, once. The low-speed limit
// (remotePins) already ends a transfer that stalls, so this bounds only a slow live one. 30 minutes carries about
// 1 GiB at 600 KiB/s, well past the history of the repositories a pass reads; the pass waits for it once per
// repository. A first fetch that does not finish leaves no complete record, so the next pass's fetch is a first one
// again.
const firstFetchLimit = 30 * time.Minute

// fetchBudget is what a mirror's next fetch is and its limit: a first fetch with firstFetchLimit while the mirror holds
// no complete fetch (none yet, or only one that did not finish, whose record names no commit), else a fetch with
// fetchLimit.
func fetchBudget(mirror string) (string, time.Duration) {
	if m, err := readMirrorMeta(mirror); err != nil || m.Commit == "" {
		return "first fetch", firstFetchLimit
	}
	return "fetch", fetchLimit
}

// gitWaitDelay is how long a Git call's process group has to end once its context ended and it was sent SIGTERM
// (which lets git remove its lock files) before SIGKILL, and then how long the wait for its output may last.
const gitWaitDelay = 5 * time.Second

// remoteSSH is the ssh every Git call of the host's that reaches a remote runs: it never prompts, gives up on a server
// that does not answer within 30 seconds, and ends a connection the server leaves silent for 45 (three keepalives 15
// seconds apart).
const remoteSSH = "ssh -o BatchMode=yes -o ConnectTimeout=30 -o ServerAliveInterval=15 -o ServerAliveCountMax=3"

// remotePins are the transfer limits of every Git call of the host's that reaches a remote: remoteSSH, and an https
// transfer that moves under 1 byte a second for 60 seconds ends. The limit is 1, not more: a quiet fetch (--quiet)
// gets no progress while the server counts and compresses objects, only its keepalive, about 1 byte a second, and a
// higher limit would end such a healthy transfer of a large repository.
func remotePins() []string {
	return []string{"-c", "core.sshCommand=" + remoteSSH, "-c", "http.lowSpeedLimit=1", "-c", "http.lowSpeedTime=60"}
}

// githubURL is where a GitHub repository is fetched from: over https with the GitHub CLI's credentials, never over
// ssh, so no ssh agent is asked to confirm a key. A test points it at a local repository.
var githubURL = func(repo string) string { return "https://github.com/" + repo + ".git" }

// mirrorMetaFile, in a mirror, says which checkouts read it and when its branch was last fetched.
const mirrorMetaFile = "momentum-mirror.json"

// mirrorLockFile, in a mirror, is the lock of the fetch that writes it (lockMirror).
const mirrorLockFile = "momentum-mirror.lock"

// errFetchRunning is a mirror whose lock a fetch still holds: one an earlier pass started, whose process group outlived
// it.
var errFetchRunning = errors.New("a fetch an earlier pass started still runs in the mirror")

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
func sourceTip(ctx context.Context, source, mirrors, gh string) (repoTip, string, error) {
	org, why := readOrigin(ctx, source)
	mirror := ""
	if why == "" {
		mirror = mirrorPath(mirrors, org.identity)
		var tip repoTip
		if tip, why = fetchTip(ctx, source, mirror, org.url, gh); why == "" {
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
	if scheme, rest, ok := strings.Cut(url, "://"); ok {
		switch scheme = strings.ToLower(scheme); scheme {
		case "https", "ssh", "file":
		default:
			return "", refused("origin's URL is not https, ssh or a local path, all the host fetches")
		}
		// a user or token in an https URL, or a password in an ssh one, would stand on the helper's command line,
		// which every account on the machine can read
		authority, _, _ := strings.Cut(rest, "/")
		if user, _, ok := strings.Cut(authority, "@"); ok && (scheme == "https" || strings.Contains(user, ":")) {
			return "", refused("origin's URL holds credentials, which the host never puts on a command line")
		}
		return url, nil
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

// fetchTip fetches the default branch of source's origin into its mirror, within fetchLimit (firstFetchLimit when the
// mirror holds no complete fetch yet), and returns its tip, or why it could not (a text without a host path or a URL).
func fetchTip(ctx context.Context, source, mirror, url, gh string) (repoTip, string) {
	failure := func(err error) string { return "fetch failed: " + reason(err) }
	lock, err := openMirror(ctx, mirror, source)
	if err != nil {
		return repoTip{}, failure(err)
	}
	defer lock.Close()
	what, limit := fetchBudget(mirror)
	fctx, cancel := withLimit(ctx, what, limit)
	defer cancel()
	branch, err := originBranch(fctx, mirror, url, gh, lock)
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
	if _, err := fetchGit(fctx, mirror, gh, lock, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		url, "+"+ref+":"+ref); err != nil {
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
	packMirror(ctx, mirror, lock)
	return repoTip{dir: mirror, branch: branch, commit: commit}, ""
}

// openMirror takes the mirror's lock (lockMirror) and makes sure dir is a usable mirror (usableMirror), making it again
// without a template (no hooks, no sample files) when it is not, removes what a fetch the system ended left behind
// (clearStale), and records source among the checkouts that read it. It returns the lock, which the caller holds, and
// passes to every Git call that writes the mirror, until its fetch ends. A mirror whose lock a fetch still holds is
// neither cleared nor made again: errFetchRunning.
func openMirror(ctx context.Context, dir, source string) (*os.File, error) {
	mirrors := filepath.Dir(dir)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		return nil, err
	}
	if err := noInputLinks(mirrors); err != nil {
		return nil, err
	}
	lock, err := lockMirror(dir)
	if errors.Is(err, errFetchRunning) {
		return nil, err
	}
	// a mirror whose lock cannot be taken (none yet, or a link where the lock is) is not one the host made
	if err != nil || !usableMirror(ctx, dir) {
		if lock != nil {
			lock.Close()
		}
		if lock, err = makeMirror(ctx, dir); err != nil {
			return nil, err
		}
	} else if err := clearStale(dir); err != nil {
		lock.Close()
		return nil, err
	}
	if err := updateMirrorMeta(dir, func(m *mirrorMeta) {
		if !slices.Contains(m.Sources, source) {
			m.Sources = append(m.Sources, source)
			slices.Sort(m.Sources)
		}
	}); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// makeMirror makes dir again: a bare repository without a template, with the files ref format, whatever Git's default
// is (clearStale and the record read refs as files), made under a name of its own and locked before it takes dir's.
func makeMirror(ctx context.Context, dir string) (*os.File, error) {
	if err := removeAll(dir); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".new-")
	if err != nil {
		return nil, err
	}
	if _, err := hostInputGit(ctx, "", "init", "--quiet", "--bare", "--template=", "--ref-format=files", tmp); err != nil {
		_ = removeAll(tmp)
		return nil, err
	}
	lock, err := lockMirror(tmp)
	if err != nil {
		_ = removeAll(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		lock.Close()
		_ = removeAll(tmp)
		return nil, err
	}
	return lock, nil
}

// lockMirror takes the mirror's lock: a flock on mirrorLockFile, held on a descriptor every Git call that writes the
// mirror inherits (fetchGit, mirrorGit). The lock lasts as long as any process holds that descriptor: a fetch's process
// group that outlives the pass that started it (the loop's own lock goes with its process) still holds it, and git
// never removes the file. errFetchRunning is a lock a fetch holds. A dir that is no directory has no lock to take.
func lockMirror(dir string) (*os.File, error) {
	if info, err := os.Lstat(dir); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, errors.New("the mirror is not a directory")
	}
	f, err := os.OpenFile(filepath.Join(dir, mirrorLockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errFetchRunning
		}
		return nil, err
	}
	return f, nil
}

// fetchRuns says whether a fetch holds the mirror's lock, without making the lock.
func fetchRuns(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, mirrorLockFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

// clearStale removes the lock files and temporary packs a fetch leaves when it is killed before it can remove them
// itself (SIGTERM lets git remove its lock files, never index-pack's temporary pack). It runs only under the mirror's
// lock (openMirror), so no fetch that could still write them runs: not this pass's (the loop runs one pass at a time),
// and not one an earlier pass started whose process group outlived it.
func clearStale(dir string) error {
	var stale []string
	for _, name := range []string{"HEAD.lock", "config.lock", "packed-refs.lock", "shallow.lock", "FETCH_HEAD.lock"} {
		stale = append(stale, filepath.Join(dir, name))
	}
	if err := filepath.WalkDir(filepath.Join(dir, "refs"), func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
			stale = append(stale, p)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "tmp_*"))
	if err != nil {
		return err
	}
	for _, p := range append(stale, packs...) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// usableMirror says whether dir is a mirror as the host makes it: a bare repository with the files ref format that is
// not shallow, of plain files and directories, whose configuration names no program, rewrite or include
// (CheckGitConfigWith, on its own file).
func usableMirror(ctx context.Context, dir string) bool {
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dir, "shallow")); !errors.Is(err, fs.ErrNotExist) {
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
	out, err := hostInputGit(ctx, "", "--git-dir="+dir, "rev-parse", "--is-bare-repository", "--show-ref-format")
	if err != nil || strings.TrimSpace(string(out)) != "true\nfiles" {
		return false
	}
	return CheckGitConfigWith(func(args ...string) ([]byte, error) { return mirrorConfig(ctx, dir, args...) }) == nil
}

// mirrorPackLimit and mirrorLooseLimit are the counts of packs and of loose objects past which a mirror is repacked
// (packMirror): Git's own defaults for the same decision (gc.autoPackLimit and gc.auto), at which `git gc --auto`
// repacks any repository. Every Git call of the host's pins gc.auto=0 and maintenance.auto=false (GitSafety), so Git
// never repacks a mirror on the side of a fetch, and without a repack every fetch adds a pack or loose objects that
// each later pass's usableMirror walks.
const (
	mirrorPackLimit  = 50
	mirrorLooseLimit = 6700
)

var looseObject = regexp.MustCompile(`^[0-9a-f]{38}([0-9a-f]{24})?$`)

// mirrorObjects counts the mirror's packs and its loose objects.
func mirrorObjects(mirror string) (packs, loose int) {
	objects := filepath.Join(mirror, "objects")
	if found, err := filepath.Glob(filepath.Join(objects, "pack", "*.pack")); err == nil {
		packs = len(found)
	}
	dirs, _ := os.ReadDir(objects)
	for _, d := range dirs {
		if len(d.Name()) != 2 || !d.IsDir() {
			continue
		}
		names, _ := os.ReadDir(filepath.Join(objects, d.Name()))
		for _, n := range names {
			if looseObject.MatchString(n.Name()) {
				loose++
			}
		}
	}
	return packs, loose
}

// packMirror repacks the mirror once it holds more than mirrorPackLimit packs or mirrorLooseLimit loose objects:
// every object the branch reaches goes into one pack (repack -a -d -l), and the loose objects no ref reaches (a branch
// that was pushed over) are pruned at once, so they do not keep the mirror past its limit. Git's own prune waits two
// weeks for an object a concurrent writer may still be about to name; here none writes: it runs after a complete
// fetch, under the pass's lock and the mirror's, which every call holds. A repack that fails leaves the mirror as it
// was, and the next pass tries again.
func packMirror(ctx context.Context, mirror string, lock *os.File) {
	if packs, loose := mirrorObjects(mirror); packs <= mirrorPackLimit && loose <= mirrorLooseLimit {
		return
	}
	// the mirror serves no fetch, so a bitmap would only cost the repack time
	if _, err := mirrorGit(ctx, mirror, lock, "-c", "repack.writeBitmaps=false", "repack", "-a", "-d", "-l", "-q"); err != nil {
		return
	}
	_, _ = mirrorGit(ctx, mirror, lock, "prune", "--expire=now")
}

// mirrorGit runs a local Git call that writes the mirror, as hostInputGit does, holding the mirror's lock.
func mirrorGit(ctx context.Context, mirror string, lock *os.File, args ...string) ([]byte, error) {
	out, _, err := hostInputGitFiles(ctx, "", ScrubBatchBytes, []*os.File{lock}, append([]string{"--git-dir=" + mirror}, args...)...)
	return out, err
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
// may have moved the branch's ref, never the record. A mirror is never read through a link in the path to the mirrors,
// which would make it some other directory's.
func lastFetch(ctx context.Context, mirror string) (repoTip, time.Time, bool) {
	if noInputLinks(filepath.Dir(mirror)) != nil || !usableMirror(ctx, mirror) {
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

var (
	commitName = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	mirrorName = regexp.MustCompile(`^[0-9a-f]{16}\.git$`)
)

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
// stays (used), and so does one that records a source whose origin could not be read this pass (unread). Only a
// mirror's name (mirrorPath) or a mirror being made (.new-) is removed, never one a fetch still writes (fetchRuns), and
// nothing when the path to mirrors holds a link.
func pruneMirrors(mirrors string, used map[string]bool, unread []string) {
	if noInputLinks(mirrors) != nil {
		return // none yet, or not the loop's own: the next pass looks again
	}
	entries, err := os.ReadDir(mirrors)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(mirrors, e.Name())
		if used[dir] || !mirrorName.MatchString(e.Name()) && !strings.HasPrefix(e.Name(), ".new-") {
			continue
		}
		if m, err := readMirrorMeta(dir); err == nil && slices.ContainsFunc(m.Sources, func(s string) bool { return slices.Contains(unread, s) }) {
			continue
		}
		if fetchRuns(dir) {
			continue // a fetch an earlier pass started still writes it: the next pass looks again
		}
		_ = removeAll(dir) // what cannot be removed now is tried again by the next pass
	}
}

// originBranch is the branch origin's HEAD names, or "" when origin does not say.
func originBranch(ctx context.Context, mirror, url, gh string, lock *os.File) (string, error) {
	out, err := fetchGit(ctx, mirror, gh, lock, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return "", err
	}
	return symrefBranch(string(out)), nil
}

// symrefBranch is the branch `ls-remote --symref URL HEAD` says HEAD names, or "" when it names none.
func symrefBranch(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if branch, ok := strings.CutSuffix(rest, "\tHEAD"); ok && branch != "" {
				return branch
			}
		}
	}
	return ""
}

// fetchGit runs a Git call that reads a remote, in the mirror, with the input pins, the remote limits (remotePins) and
// no prompt of any kind: no terminal, no stdin, no askpass program, ssh in batch mode. Only https, ssh and local paths
// are fetched, and GitHub's credentials come from the GitHub CLI, as the ship tool's do. The call ends with ctx
// (endWithContext). The call inherits the mirror's lock (lockMirror), when there is one, so a fetch whose process group
// outlives the pass still holds it. Its error names the operation and which limit ended it, never Git's diagnostics,
// which can print a URL or a host path.
func fetchGit(ctx context.Context, mirror, gh string, lock *os.File, args ...string) ([]byte, error) {
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
	if lock != nil {
		cmd.ExtraFiles = []*os.File{lock}
	}
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

// remoteFailure is a failed remote call's cause as a fixed text: the deadline that ended it, or what of the transport
// did (transportCause). Git's diagnostics are matched, never shown.
func remoteFailure(ctx context.Context, verb string, err error, stderr []byte) error {
	if ctx.Err() != nil {
		return fmt.Errorf("input git %s: %s", verb, endedBy(ctx))
	}
	if cause := transportCause(stderr); cause != "" {
		return fmt.Errorf("input git %s: %s", verb, cause)
	}
	return fmt.Errorf("input git %s failed: %w", verb, err)
}

// transportCauses are the transport's own failures, as git, curl and ssh word them, each with the fixed text the host
// gives for it: a transfer limit (remotePins), a server that cannot be reached, a connection that broke off, a server
// that failed. A repository the server does not have, or a branch it does not hold, is an answer, not one of them.
var transportCauses = []struct {
	words []string
	cause string
}{
	{[]string{"operation too slow"}, "the transfer moved under 1 byte a second for 60s"},
	{[]string{"not responding"}, "the ssh server was silent for 45s"},
	{[]string{"timed out"}, "the connection timed out"},
	{[]string{"could not resolve host"}, "the server's name did not resolve"},
	{[]string{"connection refused", "could not connect to server", "couldn't connect to server", "failed to connect to",
		"network is unreachable", "no route to host"}, "no connection to the server could be made"},
	{[]string{"connection reset", "early eof", "unexpected disconnect", "not closed cleanly", "ssl_error_syscall"}, "the connection broke off"},
	{[]string{"the requested url returned error: 5"}, "the server failed (an HTTP 5xx answer)"},
}

// transportCause is the fixed text of the transport failure Git's diagnostics name, or "" for any other failure.
func transportCause(stderr []byte) string {
	text := strings.ToLower(string(stderr))
	for _, c := range transportCauses {
		for _, w := range c.words {
			if strings.Contains(text, w) {
				return c.cause
			}
		}
	}
	return ""
}

// endWithContext makes a Git call end with its context, whatever its children do: git starts in a process group of
// its own, and the context's end stops the whole group (git and the ssh or https helper it started, which would hold
// its output open past git's own end). The group gets SIGTERM, on which git removes its lock files (index-pack's
// temporary pack stays, for clearStale), and SIGKILL when it has not ended after gitWaitDelay; the wait for its output
// is bounded the same.
func endWithContext(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		group := -cmd.Process.Pid
		if err := syscall.Kill(group, syscall.SIGTERM); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else if err != nil {
			return err
		}
		// the group's number is not reused while any member lives, so the probe and the kill reach only this group
		for end := time.Now().Add(gitWaitDelay); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if errors.Is(syscall.Kill(group, 0), syscall.ESRCH) {
				return nil
			}
		}
		if err := syscall.Kill(group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
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
