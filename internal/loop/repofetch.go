package loop

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"strconv"
	"strings"
	"sync/atomic"
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

// firstFetchLimit bounds a mirror's first fetch, which carries the branch's whole history, once, and a stalled one
// too: no low-speed limit ends a transfer (remotePins), and ssh's keepalive ends only a server gone silent. 30 minutes
// carries about 1 GiB at 600 KiB/s, well past the history of the repositories a pass reads; the pass waits for it
// once per repository. A first fetch that does not finish leaves no complete record, so the next pass's fetch is a
// first one again.
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

// remotePins are the transfer limits of every Git call of the host's that reaches a remote: remoteSSH, and no https
// low-speed limit. A quiet fetch (--quiet) gets no progress while the server counts and compresses objects, only its
// keepalive, about 1 byte a second, so any rate a limit names can end a healthy transfer of a large repository; each
// call's own deadline bounds it instead (fetchBudget, the ship's git call limit).
func remotePins() []string {
	return []string{"-c", "core.sshCommand=" + remoteSSH, "-c", "http.lowSpeedLimit=0"}
}

// githubURL is where a GitHub repository is fetched from: over https with the GitHub CLI's credentials, never over
// ssh, so no ssh agent is asked to confirm a key. A test points it at a local repository.
var githubURL = func(repo string) string { return "https://github.com/" + repo + ".git" }

// mirrorMetaFile, in a mirror, says which checkouts read it and when its branch was last fetched.
const mirrorMetaFile = "momentum-mirror.json"

// mirrorLockFile, in a mirror, is the lock of the fetch that writes it (lockMirror).
const mirrorLockFile = "momentum-mirror.lock"

// makersLockFile, beside the mirrors, serializes the makers of mirrors (makeMirror): no name a prune removes.
const makersLockFile = ".make.lock"

// makersWait bounds how long a maker waits for another to finish its swap (lockMakers): a swap is a lock, a check and
// two renames.
const makersWait = time.Minute

// mirrorTombstone begins the name a prune or a remake gives a mirror it removes (pruneMirrors, makeMirror): renamed
// under its lock, the mirror is out of every reader's way before any of it goes, and one stopped midway leaves only a
// tombstone, which the next prune removes.
const mirrorTombstone = ".gone-"

// tombstone is a name of its own under mirrors for a mirror taken out of the way (mirrorTombstone).
func tombstone(mirrors string) string {
	return filepath.Join(mirrors, mirrorTombstone+strings.ToLower(rand.Text()))
}

// mirrorUnused is how long a mirror nothing used stays (pruneMirrors): a week, the loop's shortest measurement window.
// A case in testing gets a build attempt every pass, so a mirror a case still needs is used well within it.
const mirrorUnused = 7 * 24 * time.Hour

// errFetchRunning is a mirror whose lock another holds: a fetch (one an earlier pass started, whose process group
// outlived it), or the owner's preparation, from its fetch until its package is saved.
var errFetchRunning = errors.New("another fetch, or the owner's preparation, holds the mirror")

type mirrorMeta struct {
	Sources []string  `json:"sources"`
	Branch  string    `json:"branch,omitempty"` // origin's default branch at the last complete fetch
	Commit  string    `json:"commit,omitempty"` // its tip then
	Fetched time.Time `json:"fetched,omitzero"`
	// Used is when a pass or the owner's preparation last opened the mirror to fetch into it (recordSource), under its
	// lock. A prune removes a mirror unused for mirrorUnused.
	Used time.Time `json:"used,omitzero"`
}

// repoTip is where a snapshot reads a repository: the Git directory that holds its objects (origin's mirror, or the
// checkout), the branch and its commit, and, when that is not origin's branch fetched now, which it is and why. repack
// is why the mirror's repack after the fetch failed, for the pass's log; class and line name a fetch that failed.
type repoTip struct {
	dir, branch, commit string
	fallback            string
	repack              string
	class, line         string // a failed fetch's class (holdClass) and the line of Git's that shows it
	identity            string // the repository the mirror holds (origin's identity), for a tip in the mirror
	busy                bool   // the fetch found the mirror's lock held by another (errFetchRunning)
}

// origin is the repository a checkout's origin names, by its identity (OWNER/REPO on GitHub, else url: and its URL),
// and the URL the mirror fetches it from.
type origin struct{ identity, url string }

// sourceTip is the tip a snapshot of source reads, and the mirror of its origin ("" when its origin could not be
// read): origin's default branch fetched now; when the fetch fails, the branch as the mirror last fetched it; when
// there is no such mirror, or no origin to read, the checkout's own default branch. A fallback says which and why. An
// error is a checkout without a default branch, or a pass told to stop.
func sourceTip(ctx context.Context, source, mirrors, gh string) (repoTip, string, error) {
	tip, mirror, lock, err := lockedSourceTip(ctx, source, mirrors, gh)
	if lock != nil {
		lock.Close()
	}
	return tip, mirror, err
}

// lockedSourceTip is sourceTip that returns the mirror's lock, still held, when the tip is in the mirror and the fetch
// took the lock, the fetch done or not; else nil (a fetch an earlier pass started holds it, the tip is the checkout's,
// or an error). The caller reads the mirror under it and closes it.
func lockedSourceTip(ctx context.Context, source, mirrors, gh string) (repoTip, string, *os.File, error) {
	org, why := readOrigin(ctx, source)
	mirror, class, line := "", "", ""
	var lock *os.File
	noMirror := func(tip repoTip, err error) (repoTip, string, *os.File, error) {
		if lock != nil {
			lock.Close() // the caller reads no mirror
		}
		return tip, mirror, nil, err
	}
	if why == "" {
		mirror = mirrorPath(mirrors, org.identity)
		var tip repoTip
		if tip, lock, why = lockedFetchTip(ctx, source, mirror, org.url, gh); why == "" {
			tip.identity = org.identity
			return tip, mirror, lock, nil
		}
		class, line = tip.class, tip.line
		if err := ctx.Err(); err != nil {
			return noMirror(repoTip{}, err)
		}
		if last, at, ok := lastFetch(ctx, mirror); ok {
			last.fallback = fmt.Sprintf("origin's %s as fetched at %s: %s", last.branch, at.UTC().Format("2006-01-02 15:04Z"), why)
			last.class, last.line, last.identity, last.busy = class, line, org.identity, tip.busy
			return last, mirror, lock, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return noMirror(repoTip{}, err)
	}
	branch, commit, err := defaultCommit(ctx, source)
	if err != nil {
		return noMirror(repoTip{}, err)
	}
	return noMirror(repoTip{dir: source, branch: branch, commit: commit, fallback: fmt.Sprintf("checkout's %s: %s", branch, why),
		class: firstOf(class, why), line: line}, nil)
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
	tip, lock, why := lockedFetchTip(ctx, source, mirror, url, gh)
	if lock != nil {
		lock.Close()
	}
	return tip, why
}

// lockedFetchTip is fetchTip that returns the mirror's lock, still held, whenever it took it, the fetch done or not:
// the caller reads the mirror under it and closes it.
func lockedFetchTip(ctx context.Context, source, mirror, url, gh string) (tip repoTip, lock *os.File, why string) {
	var class, line string // a failed call's, for the tip a failure returns
	busy := false
	defer func() {
		if why != "" {
			tip = repoTip{class: firstOf(class, why), line: line, busy: busy}
		}
	}()
	failure := func(err error) string {
		var failed *remoteError
		if errors.As(err, &failed) {
			class, line = failed.class, failed.line
		}
		return "fetch failed: " + reason(err)
	}
	lock, err := openMirror(ctx, mirror, source)
	if err != nil {
		busy = errors.Is(err, errFetchRunning)
		return repoTip{}, nil, failure(err)
	}
	what, limit := fetchBudget(mirror)
	fctx, cancel := withLimit(ctx, what, limit)
	defer cancel()
	call := mirrorCall{mirror: mirror, gh: gh, lock: lock}
	branch, err := originBranch(fctx, call, url)
	if err != nil {
		return repoTip{}, lock, failure(err)
	}
	if branch == "" {
		// a remote that does not name its HEAD: the branch the checkout takes for origin's default
		if branch, err = cloneDefaultBranch(fctx, source); err != nil {
			return repoTip{}, lock, failure(err)
		}
	}
	if _, err := hostInputGit(fctx, "", "check-ref-format", "refs/heads/"+branch); err != nil {
		return repoTip{}, lock, "fetch refused: origin's default branch has no valid name"
	}
	ref := "refs/heads/" + branch
	if _, err := fetchGit(fctx, call, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		url, "+"+ref+":"+ref); err != nil {
		return repoTip{}, lock, failure(err)
	}
	out, err := hostInputGit(fctx, "", "--git-dir="+mirror, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return repoTip{}, lock, failure(err)
	}
	commit := strings.TrimSpace(string(out))
	if err := updateMirrorMeta(mirror, func(m *mirrorMeta) { m.Branch, m.Commit, m.Fetched = branch, commit, time.Now() }); err != nil {
		return repoTip{}, lock, failure(err)
	}
	tip = repoTip{dir: mirror, branch: branch, commit: commit}
	if err := packMirror(ctx, mirror, lock); err != nil {
		tip.repack = "the mirror's repack failed: " + reason(err)
	}
	return tip, lock, ""
}

// mirrorCall is a remote Git call in a mirror: the mirror, the GitHub CLI for GitHub's credentials, and the mirror's
// lock, which the call inherits.
type mirrorCall struct {
	mirror, gh string
	lock       *os.File
}

// openMirror takes the mirror's lock (lockMirror) and makes sure dir is a usable mirror (usableMirror), making it again
// without a template (no hooks, no sample files) when it is not, removes what a fetch the system ended left behind
// (clearStale), and records source among the checkouts that read it. It returns the lock, which the caller holds, and
// passes to every Git call that writes the mirror, until its fetch ends. A mirror is made again only when it is
// definitely not one the host made: none yet, something else in its place, a link where its lock is, or a usable
// check that came out no. A lock another holds (errFetchRunning: a fetch, or the owner's preparation), any other error
// and a pass told to stop leave it as it is.
func openMirror(ctx context.Context, dir, source string) (*os.File, error) {
	mirrors := filepath.Dir(dir)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		return nil, err
	}
	if err := noInputLinks(mirrors); err != nil {
		return nil, err
	}
	lock, err := lockMirror(dir)
	switch {
	case err == nil:
		usable, err := usableMirror(ctx, dir)
		if err == nil && usable {
			err = clearStale(dir)
		}
		if err != nil || usable {
			if err == nil {
				err = recordSource(dir, source)
			}
			if err != nil {
				lock.Close()
				return nil, err
			}
			return lock, nil
		}
		// made again: the old mirror's lock stays held until the new one takes its place (makeMirror)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
	default:
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	if lock, err = makeMirror(ctx, dir, lock); err != nil {
		return nil, err
	}
	if err := recordSource(dir, source); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// recordSource records source among the checkouts that read the mirror, and that the mirror is used now.
func recordSource(dir, source string) error {
	return updateMirrorMeta(dir, func(m *mirrorMeta) {
		if !slices.Contains(m.Sources, source) {
			m.Sources = append(m.Sources, source)
			slices.Sort(m.Sources)
		}
		m.Used = time.Now()
	})
}

// lastUsed is when the mirror was last used: its record's time; for a record written before it had one, the record's
// own last change; without a record, the directory's.
func lastUsed(dir string) time.Time {
	if m, err := readMirrorMeta(dir); err == nil && !m.Used.IsZero() {
		return m.Used
	}
	for _, p := range []string{filepath.Join(dir, mirrorMetaFile), dir} {
		if info, err := os.Lstat(p); err == nil {
			return info.ModTime()
		}
	}
	return time.Time{}
}

// makeMirror makes dir again: a bare repository without a template, with the files ref format, whatever Git's default
// is (clearStale and the record read refs as files). The format is set by init.defaultRefFormat, which a Git before
// 2.45, that has no other format and no --ref-format option, ignores. The new mirror is made under a name of its own,
// locked before Git initializes it, so a prune never takes it for one nothing holds (lockForRemoval), and takes dir's
// place only once it exists, so an init that fails leaves dir as it was. The swap runs under the makers' lock
// (lockMakers), so two makers never both swap: once it holds that lock, a maker looks at dir again. A mirror another
// holds there (another maker made it meanwhile, or a fetch runs) leaves the new one dropped and errFetchRunning; a
// usable one, which another maker made and let go, is the one used. Otherwise dir goes to a tombstone, the new mirror
// takes its name, and only then is the tombstone removed. The old mirror's lock (old, nil when there is none) stays
// held until that swap; makeMirror closes old.
func makeMirror(ctx context.Context, dir string, old *os.File) (*os.File, error) {
	if old != nil {
		defer old.Close()
	}
	mirrors := filepath.Dir(dir)
	tmp, err := os.MkdirTemp(mirrors, ".new-")
	if err != nil {
		return nil, err
	}
	lock, err := lockMirror(tmp)
	if err != nil {
		_ = removeAll(tmp)
		return nil, err
	}
	drop := func(err error) (*os.File, error) {
		lock.Close()
		_ = removeAll(tmp)
		return nil, err
	}
	if _, err := hostInputGit(ctx, "", "-c", "init.defaultRefFormat=files", "init", "--quiet", "--bare", "--template=", tmp); err != nil {
		return drop(err)
	}
	unlock, err := lockMakers(ctx, mirrors, makersWait)
	if err != nil {
		return drop(err)
	}
	held := old // the lock of what stands at dir now, held until the swap
	if held == nil || !sameLock(held, dir) {
		held = nil
		there, err := lockMirror(dir)
		switch {
		case err == nil:
			usable, err := usableMirror(ctx, dir)
			if err == nil && usable {
				err = clearStale(dir)
			}
			if err != nil || usable {
				unlock()
				if err != nil {
					there.Close()
					return drop(err)
				}
				lock.Close()
				_ = removeAll(tmp)
				return there, nil // another maker's mirror, which it let go
			}
			held = there
			defer there.Close()
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
		default: // errFetchRunning: another maker made it and holds it, or a fetch runs there
			unlock()
			return drop(err)
		}
	}
	tomb := tombstone(mirrors)
	if err := os.Rename(dir, tomb); err != nil && !errors.Is(err, fs.ErrNotExist) {
		unlock()
		return drop(err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.Rename(tomb, dir) // the old one back at its name; a prune removes a tombstone left
		unlock()
		return drop(err)
	}
	unlock()
	if held != nil {
		held.Close()
	}
	_ = removeAll(tomb) // what cannot be removed now the next prune removes
	return lock, nil
}

// lockMakers takes the makers' lock (makersLockFile), waiting at most wait or until ctx ends: one maker swaps a mirror
// at a time (makeMirror waits makersWait). A maker that cannot take it in time finds the mirror busy
// (errFetchRunning); with no wait, the lock is tried once.
func lockMakers(ctx context.Context, mirrors string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(filepath.Join(mirrors, makersLockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	for end := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return func() { f.Close() }, nil
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, err
		case ctx.Err() != nil:
			f.Close()
			return nil, ctx.Err()
		case time.Now().After(end):
			f.Close()
			return nil, errFetchRunning
		}
	}
}

// lockMirror takes the mirror's lock: a flock on mirrorLockFile, held on a descriptor every Git call that writes the
// mirror inherits (fetchGit, mirrorGit). The lock lasts as long as any process holds that descriptor: a fetch's process
// group that outlives the pass that started it (the loop's own lock goes with its process) still holds it, and git
// never removes the file. The lock's file names its taker, this process (writeLockRecord). A lock whose taker is gone
// and that a group has held past orphanAfter is taken from that group (takeOrphan); errFetchRunning is one another
// holds: a fetch, or the owner's preparation, until its package is saved. A dir that is no directory has no lock to
// take (ENOTDIR), and neither has a link where the lock is (ELOOP).
func lockMirror(dir string) (*os.File, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "lock", Path: dir, Err: syscall.ENOTDIR}
	}
	f, err := os.OpenFile(filepath.Join(dir, mirrorLockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) && takeOrphan(f) {
		err = nil
	}
	if err == nil && !sameLock(f, dir) { // the mirror moved, or was made again, while its lock was being taken
		err = syscall.EWOULDBLOCK
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errFetchRunning
		}
		return nil, err
	}
	writeLockRecord(f, lockRecord{taker: thisTaker()}) // a previous holder's record goes
	return f, nil
}

// sameLock says whether f, a lock file taken, is still the one at dir's lock path, by its device and inode: a mirror
// moved (a prune's tombstone, a remake) while its lock was being taken has another file there, or none.
func sameLock(f *os.File, dir string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	at, err := os.Lstat(filepath.Join(dir, mirrorLockFile))
	return err == nil && os.SameFile(held, at)
}

// orphanAfter is how long one Git call's process group may hold a mirror's lock before a pass takes the lock from it,
// once the lock's taker is gone: the longest a call of a live pass lasts (a first fetch, then SIGTERM and the wait for
// its output). A group whose taker is gone and that holds it longer is one nothing else ends: its deadline was the
// taker's. A test shortens it.
var orphanAfter = firstFetchLimit + 2*gitWaitDelay

// lockHolder is a process a mirror's lock record names: its number and its start time (procStat). The zero one names
// none.
type lockHolder struct {
	pid   int
	start uint64
}

// lockRecord is what a mirror's lock file says of who holds the lock: its taker, the process that took it (lockMirror),
// and the process group of the last Git call that inherited it, by its leader (recordHolder).
type lockRecord struct{ taker, group lockHolder }

// taker is this process as a lock's taker, once a read of /proc has found it (thisTaker).
var taker atomic.Pointer[lockHolder]

// thisTaker is this process as a lock's taker, from /proc: the zero one while it cannot be read, and a lock whose
// record names no taker is never taken from its group (takeOrphan). Only a read that succeeds is kept: one that fails
// (no descriptor free, say) is tried again at the next lock taken or call recorded (recordHolder).
func thisTaker() lockHolder {
	if h := taker.Load(); h != nil {
		return *h
	}
	pid := os.Getpid()
	_, start, err := procStat(pid)
	if err != nil {
		return lockHolder{}
	}
	h := lockHolder{pid, start}
	taker.Store(&h)
	return h
}

// writeLockRecord writes r over the lock file's record, as one line per holder it names (`taker PID START`, then
// `group PID START`). The text is written first and the file then cut to its length, never emptied first: the taker's
// line, which comes first, stands unchanged while the taker records each of its calls.
func writeLockRecord(lock *os.File, r lockRecord) {
	var text strings.Builder
	for _, h := range []struct {
		name string
		lockHolder
	}{{"taker", r.taker}, {"group", r.group}} {
		if h.pid > 0 {
			fmt.Fprintf(&text, "%s %d %d\n", h.name, h.pid, h.start)
		}
	}
	if _, err := lock.WriteAt([]byte(text.String()), 0); err == nil {
		_ = lock.Truncate(int64(text.Len()))
	}
}

// readLockRecord is the lock file's record; a line it cannot read names no holder.
func readLockRecord(lock *os.File) lockRecord {
	raw := make([]byte, 128)
	n, _ := lock.ReadAt(raw, 0)
	var r lockRecord
	for _, line := range strings.Split(string(raw[:n]), "\n") {
		var h lockHolder
		var name string
		if _, err := fmt.Sscanf(line, "%s %d %d", &name, &h.pid, &h.start); err != nil || h.pid <= 0 {
			continue
		}
		switch name {
		case "taker":
			r.taker = h
		case "group":
			r.group = h
		}
	}
	return r
}

// recordHolder writes into the mirror's lock file the process group of a Git call that inherited the lock: its number
// and its leader's start time (procStat), beside this process as the lock's taker, for takeOrphan. A call that already
// ended leaves the record as it was.
func recordHolder(lock *os.File, pid int) {
	_, start, err := procStat(pid)
	if err != nil {
		return
	}
	writeLockRecord(lock, lockRecord{taker: thisTaker(), group: lockHolder{pid, start}})
}

// gone says whether the process h names has ended: no process has its number, the one that has it started at another
// time, or it has ended and waits for its parent (a zombie, which holds no file). One whose status cannot be read for
// another reason, and the zero holder, count as running.
func (h lockHolder) gone() bool {
	if h.pid <= 0 {
		return false
	}
	p, err := readProc(h.pid)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		return false
	}
	return p.start != h.start || p.state == 'Z' || p.state == 'X'
}

// takeOrphan ends the process group the lock's record names and takes the lock, when the record's taker is gone and
// that group has held it past orphanAfter. A group whose taker still runs is never ended, however old: its taker's
// deadline still bounds it, and the start times count from boot, so a suspend ages a live call past orphanAfter. The
// record's leader must still run, lead its group and have the start time the record gives, so a number another
// process took since is never signalled; a group whose leader is gone is left alone. The group gets SIGTERM, then
// SIGKILL after gitWaitDelay, as a deadline ends a call (endWithContext). It says whether f holds the lock now.
func takeOrphan(f *os.File) bool {
	r := readLockRecord(f)
	pgid, start := r.group.pid, r.group.start
	if pgid <= 1 || !r.taker.gone() {
		return false
	}
	group, started, err := procStat(pgid)
	if err != nil || group != pgid || started != start {
		return false
	}
	if age, err := procAge(started); err != nil || age <= orphanAfter {
		return false
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		// the leader is read again before each signal: one that ended on SIGTERM leaves a group that cannot be told
		if group, now, err := procStat(pgid); err != nil || group != pgid || now != start {
			return false
		}
		if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return false
		}
		for end := time.Now().Add(gitWaitDelay); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				return true
			}
		}
	}
	return false
}

// procStat is a process's group and its start time, in clock ticks after boot (readProc).
func procStat(pid int) (int, uint64, error) {
	p, err := readProc(pid)
	return p.group, p.start, err
}

// procStatus is what /proc/PID/stat says of a process: its state, its group and its start time, in clock ticks after
// boot.
type procStatus struct {
	state byte
	group int
	start uint64
}

// readProc reads fields 3, 5 and 22 of /proc/PID/stat, counted after the command's name, which can hold spaces and
// parentheses.
func readProc(pid int) (procStatus, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStatus{}, err
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return procStatus{}, errors.New("unreadable process status")
	}
	fields := strings.Fields(string(raw[i+1:])) // from field 3
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStatus{}, errors.New("unreadable process status")
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return procStatus{}, err
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStatus{}, err
	}
	return procStatus{state: fields[0][0], group: group, start: start}, nil
}

// userHZ is the clock tick /proc counts in (USER_HZ): 100 on Linux, whatever the kernel's own rate.
const userHZ = 100

// procAge is how long ago a process that started at start (procStat) did, by the system's uptime.
func procAge(start uint64) (time.Duration, error) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, errors.New("unreadable uptime")
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration((up - float64(start)/userHZ) * float64(time.Second)), nil
}

// lockForRemoval takes the mirror's lock for its removal (pruneMirrors) as a fetch takes it (lockMirror), without
// waiting: a missing lock file is made, and a lock an orphaned fetch holds is taken from it (takeOrphan). ok is false
// while anything else holds it, a fetch's process group (one whose taker runs, or one an earlier pass started that has
// not held it past orphanAfter) or the owner's preparation, from its fetch until its package is saved; when the lock
// file taken is no longer the one at the mirror's path (sameLock); when nothing, or no directory, is at dir; and when
// the lock file cannot be opened for another reason (no access): the next pass looks again. A link where the lock is
// has no lock to take. A mirror there is one only a maker touches, to make it again (openMirror), and a maker swaps
// only under the makers' lock, so a prune holds that lock instead (lockLinkedForRemoval).
func lockForRemoval(dir string) (release func(), ok bool) {
	lock, err := lockMirror(dir)
	if err == nil {
		return func() { lock.Close() }, true
	}
	if !errors.Is(err, syscall.ELOOP) {
		return nil, false
	}
	return lockLinkedForRemoval(dir)
}

// lockLinkedForRemoval takes, for its removal, a mirror whose lock path held a link (lockForRemoval): the makers' lock,
// tried once, and under it the mirror's lock again, since a maker may have swapped a mirror in before the prune took
// the makers' lock. A link still there is held by the makers' lock alone; a mirror swapped in is held by its own lock
// as well. ok is false while a maker holds the makers' lock, while another holds the mirror's, and when no mirror can
// be locked at dir.
func lockLinkedForRemoval(dir string) (release func(), ok bool) {
	unlock, err := lockMakers(context.Background(), filepath.Dir(dir), 0)
	if err != nil {
		return nil, false
	}
	switch lock, err := lockMirror(dir); {
	case err == nil: // a maker swapped a mirror in before the prune took the makers' lock
		return func() { lock.Close(); unlock() }, true
	case errors.Is(err, syscall.ELOOP):
		return unlock, true
	}
	unlock()
	return nil, false
}

// clearStale removes the lock files, temporary files and keep files a fetch or a repack leaves when it is killed before
// it can remove them itself (SIGTERM lets git remove its lock files, never index-pack's temporary pack or its keep
// file, a loose object half written or a repack's new pack). It runs only under the mirror's lock (openMirror), so no
// call that could still write them runs: not this pass's (the loop runs one pass at a time), and not one an earlier
// pass started whose process group outlived it.
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
	// a record's temporary file a writer killed before its rename left (updateMirrorMeta)
	if found, err := filepath.Glob(filepath.Join(dir, "."+mirrorMetaFile+".*")); err == nil {
		stale = append(stale, found...)
	}
	// index-pack's temporary pack, a repack's new pack before its rename, a loose object before its rename
	for _, pattern := range []string{filepath.Join("pack", "tmp_*"), filepath.Join("pack", ".tmp-*"), filepath.Join("[0-9a-f][0-9a-f]", "tmp_obj_*")} {
		found, err := filepath.Glob(filepath.Join(dir, "objects", pattern))
		if err != nil {
			return err
		}
		stale = append(stale, found...)
	}
	// the keep file index-pack writes for fetch-pack, which the fetch removes once its refs name the pack: one a killed
	// fetch left would keep its pack out of every repack
	keeps, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.keep"))
	if err != nil {
		return err
	}
	for _, k := range keeps {
		if f, err := os.Open(k); err == nil {
			head := make([]byte, len("fetch-pack "))
			n, _ := f.Read(head)
			f.Close()
			if string(head[:n]) == "fetch-pack " {
				stale = append(stale, k)
			}
		}
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// usableMirror says whether dir is a mirror as the host makes it: a bare repository with the files ref format that is
// not shallow, of plain files and directories, whose configuration names no program, rewrite or include
// (CheckGitConfigWith, on its own file). It answers no only on a definite answer: nothing there, a shallow mirror or a
// file that is not plain, git saying the directory is no repository (bareMirror), another ref format in the mirror's
// own configuration, a setting the host never makes. Every other failure (a pass told to stop, a read that failed, a
// Git that ran out of memory or descriptors) is an error, and a mirror is never made again on one.
func usableMirror(ctx context.Context, dir string) (bool, error) {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.IsDir():
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, "shallow")); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	plain := true
	if err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && !e.Type().IsRegular() {
			plain = false
			return fs.SkipAll
		}
		return err
	}); err != nil {
		return false, err
	}
	if !plain {
		return false, nil
	}
	if bare, err := bareMirror(ctx, dir); err != nil || !bare {
		return false, err
	}
	// the ref format, from the mirror's own configuration: a Git before 2.45 has no --show-ref-format, and its rev-parse
	// prints the flag back
	format, err := mirrorConfig(ctx, dir, "--get", "extensions.refStorage")
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.As(err, &exit) && exit.Exited() && exit.ExitCode() == 1: // not set: files
	case err != nil:
		return false, err
	case !strings.EqualFold(strings.TrimSpace(string(format)), "files"):
		return false, nil
	}
	err = CheckGitConfigWith(func(args ...string) ([]byte, error) { return mirrorConfig(ctx, dir, args...) })
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case err == nil:
		return true, nil
	case errors.Is(err, ErrRefused):
		return false, nil // a setting the host never makes
	}
	return false, err
}

// bareMirror is git's answer to whether dir is a bare repository: true; false, for one with a work tree; or no
// repository at all, as git says when the directory is none, its configuration cannot be read, or it needs a
// repository extension this Git does not have (one a later Git made). Any other failure is an error.
func bareMirror(ctx context.Context, dir string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append(append([]string{"--git-dir=" + dir}, GitSafety()...), "rev-parse", "--is-bare-repository")...)
	cmd.Env = GitEnv()
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	answer := strings.TrimSpace(out.String())
	text := strings.ToLower(diag.String())
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case err == nil && answer == "true":
		return true, nil
	case err == nil && answer == "false":
		return false, nil
	case gitSaidNo(err) && (strings.Contains(text, "not a git repository") || strings.Contains(text, "bad config") ||
		strings.Contains(text, "unknown repository extension")):
		return false, nil
	case err == nil:
		return false, errors.New("input git rev-parse gave no answer")
	}
	return false, fmt.Errorf("input git rev-parse failed: %w", err)
}

// gitSaidNo says whether a Git call failed by exiting with an error of its own, not by a signal or before it ran.
func gitSaidNo(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.Exited()
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

// repackLimit bounds a mirror's repack. A geometric repack (packMirror) rewrites only the packs and loose objects it
// rolls up, never the largest pack while the others stay under half its size, so it costs about what came since the
// last one. 10 minutes would still move the 1 GiB a first fetch is sized for (firstFetchLimit) at under 2 MiB/s each
// way, far below any disk's rate, and it stays under orphanAfter, so a pass never takes the lock from a live repack.
const repackLimit = 10 * time.Minute

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

// packMirror repacks the mirror once it holds more than mirrorPackLimit packs or mirrorLooseLimit loose objects, into
// a geometric progression (repack -d -l --geometric=2): each pack holds at least twice the objects of the next smaller,
// so a mirror keeps at most about log2 of its object count in packs, well under mirrorPackLimit. Its loose objects
// go into the new pack whether a ref reaches them or not, so the count of loose objects is 0 after it. A geometric
// repack rewrites only what it rolls up, not the whole history, as repack -a does: so a repack that fails costs little
// when the next pass tries it again, and needs no record of its own or a wait. It runs after a complete fetch, under
// the mirror's lock, which the call holds: a pass's fetch holds the pass's lock too, the owner's preparation
// (Ship.Prepare) only the mirror's. A repack that fails leaves the mirror as it was, says why, and the next fetch
// tries again.
func packMirror(ctx context.Context, mirror string, lock *os.File) error {
	if packs, loose := mirrorObjects(mirror); packs <= mirrorPackLimit && loose <= mirrorLooseLimit {
		return nil
	}
	// the mirror serves no fetch, so a bitmap would only cost the repack time
	_, err := mirrorGit(ctx, mirror, lock, "-c", "repack.writeBitmaps=false", "repack", "-d", "-l", "--geometric=2", "-q")
	return err
}

// mirrorGit runs a mirror's repack, as hostInputGit does, within repackLimit, holding the mirror's lock.
func mirrorGit(ctx context.Context, mirror string, lock *os.File, args ...string) ([]byte, error) {
	out, _, err := hostInputGitHolding(ctx, "", ScrubBatchBytes, repackLimit, lock, append([]string{"--git-dir=" + mirror}, args...)...)
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
	if noInputLinks(filepath.Dir(mirror)) != nil {
		return repoTip{}, time.Time{}, false
	}
	if usable, err := usableMirror(ctx, mirror); err != nil || !usable {
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

// updateMirrorMeta changes the mirror's record in one rename, from a temporary file of its own.
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
	tmp, err := os.CreateTemp(mirror, "."+mirrorMetaFile+".*") // a name of this writer's own
	if err != nil {
		return err
	}
	_, err = tmp.Write(raw)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(mirror, mirrorMetaFile))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// pruneMirrors removes the mirrors nothing has used for mirrorUnused: neither a pass (used: the mirrors this pass read,
// whose records say so too) nor the owner's preparation, each of which records the time in the mirror's record under
// its lock (recordSource). A use in the future (a clock set back) is written as now. Only a mirror's name (mirrorPath)
// or a mirror being made (.new-, whose maker holds its lock while it runs, and one without a lock file yet only once it
// is older than orphanAfter) is removed, only under its lock, taken as a fetch takes it, or the makers' lock where a
// link stands at its lock (lockForRemoval), never one whose lock another holds, and nothing when the path to mirrors
// holds a link. A mirror's last use is read once its lock is taken, so a preparation that used it while the pass ran
// is seen. Under its lock a mirror is renamed to a tombstone (mirrorTombstone) and only then removed, so a prune
// stopped midway leaves no half mirror at the mirror's name; a tombstone left is removed. A file or a link at a
// mirror's name is no mirror: it is unlinked, never what a link names.
func pruneMirrors(mirrors string, used map[string]bool) {
	if noInputLinks(mirrors) != nil {
		return // none yet, or not the loop's own: the next pass looks again
	}
	entries, err := os.ReadDir(mirrors)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(mirrors, e.Name())
		if strings.HasPrefix(e.Name(), mirrorTombstone) {
			_ = removeAll(dir) // a mirror taken out of the way: nothing reads it
			continue
		}
		made := strings.HasPrefix(e.Name(), ".new-")
		if used[dir] || !mirrorName.MatchString(e.Name()) && !made {
			continue
		}
		if !e.IsDir() {
			_ = os.Remove(dir) // a file or a link: the link itself goes
			continue
		}
		if made {
			// one with no lock file yet is its maker's, which is about to lock it, unless no maker lasts so long
			_, err := os.Lstat(filepath.Join(dir, mirrorLockFile))
			if info, ierr := e.Info(); errors.Is(err, fs.ErrNotExist) && (ierr != nil || time.Since(info.ModTime()) < orphanAfter) {
				continue
			}
		}
		release, ok := lockForRemoval(dir)
		if !ok {
			continue // a fetch or a preparation still holds it: the next pass looks again
		}
		if !made {
			used := lastUsed(dir)
			if used.After(time.Now()) { // a clock set back since: it was used now, and ages from now
				_ = updateMirrorMeta(dir, func(m *mirrorMeta) { m.Used = time.Now() })
			}
			if time.Since(used) < mirrorUnused {
				release()
				continue
			}
		}
		tomb := tombstone(mirrors)
		err := os.Rename(dir, tomb)
		release()
		if err == nil {
			_ = removeAll(tomb) // what cannot be removed now the next pass removes
		}
	}
}

// originBranch is the branch origin's HEAD names, or "" when origin does not say.
func originBranch(ctx context.Context, call mirrorCall, url string) (string, error) {
	out, err := fetchGit(ctx, call, "ls-remote", "--symref", url, "HEAD")
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
// (endWithContext). The call inherits the mirror's lock (lockMirror), when there is one, and records its process group
// there (recordHolder), so a fetch whose process group outlives the pass still holds it, and a later pass can tell
// that group. Its error names the operation and which limit ended it, never Git's diagnostics, which can print a URL
// or a host path.
func fetchGit(ctx context.Context, call mirrorCall, args ...string) ([]byte, error) {
	pins := append([]string{"--git-dir=" + call.mirror}, GitSafety()...)
	pins = append(pins, "-c", "uploadpack.packObjectsHook=",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "protocol.file.allow=always",
		"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!"+ShellQuote(firstOf(call.gh, "gh"))+" auth git-credential")
	pins = append(pins, remotePins()...)
	cmd := exec.CommandContext(ctx, "git", append(pins, args...)...)
	for _, kv := range GitEnv() {
		if !strings.HasPrefix(kv, "SSH_ASKPASS") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "SSH_ASKPASS_REQUIRE=never")
	if call.lock != nil {
		cmd.ExtraFiles = []*os.File{call.lock}
	}
	endWithContext(cmd)
	out, stderr := Capture{Limit: ScrubBatchBytes}, Capture{Limit: ScrubBatchBytes}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Start()
	if err == nil {
		if call.lock != nil {
			recordHolder(call.lock, cmd.Process.Pid)
		}
		err = cmd.Wait()
	}
	if err != nil {
		return nil, remoteFailure(ctx, gitVerb(args), err, stderr.Bytes())
	}
	if out.Over() || stderr.Over() {
		return nil, fmt.Errorf("input git output exceeds the input limit")
	}
	return out.Bytes(), nil
}

// remoteFailure is a failed remote call's cause as a fixed text: the deadline that ended it, or its class (holdClass).
// Git's diagnostics are matched, never shown; the line that shows the class goes, scrubbed, only to the pass's log.
func remoteFailure(ctx context.Context, verb string, err error, stderr []byte) error {
	if ctx.Err() != nil {
		cause := endedBy(ctx)
		return &remoteError{text: fmt.Sprintf("input git %s: %s", verb, cause), class: cause}
	}
	class, line := holdClass(stderr)
	if class == otherFailure {
		return &remoteError{text: fmt.Sprintf("input git %s failed: %v", verb, err), class: class, line: line}
	}
	return &remoteError{text: fmt.Sprintf("input git %s: %s", verb, class), class: class, line: line}
}

// remoteError is a remote call that failed: its text, its class (holdClass) and the line of Git's that shows it.
type remoteError struct{ text, class, line string }

func (e *remoteError) Error() string { return e.text }

// transportCauses are the transport's own failures, as git, curl (OpenSSL or GnuTLS) and ssh word them, each with the
// fixed text the host gives for it: an ssh server gone silent (remoteSSH), a server that cannot be reached, a
// connection that broke off, a server that failed.
var transportCauses = []struct {
	words []string
	cause string
}{
	{[]string{"not responding"}, "the ssh server was silent for 45s"},
	{[]string{"timed out"}, "the connection timed out"},
	{[]string{"could not resolve host"}, "the server's name did not resolve"},
	{[]string{"connection refused", "could not connect to server", "couldn't connect to server", "failed to connect to",
		"network is unreachable", "no route to host"}, "no connection to the server could be made"},
	{[]string{"connection reset", "connection closed by", "early eof", "unexpected disconnect", "not closed cleanly",
		"transfer closed with outstanding read data", "empty reply from server", "framing layer", "handshake failed",
		"recv error", "send error", "non-properly terminated", "ssl_read", "ssl_write", "ssl_connect", "ssl_error_syscall"},
		"the connection broke off"},
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

// holdClass is the class of a remote call's failure, as the pass's summary and a held build's note name it, and the
// line of Git's diagnostics that shows it, for the pass's log. The class is one of a fixed few, so a note stands for
// a class, not for each text Git printed: no credentials (credentialFailure); a certificate or ssh host key not
// trusted; a transport cause (transportCauses); a remote that has no such repository or branch; a server's refusal
// (another 4xx); another failure. A call its deadline ended has that deadline for its class instead (remoteFailure).
// The line is the one that matched, else Git's last fatal: or error: line: warnings (ssh's about its key exchange, a
// redirect) come before what failed.
func holdClass(diag []byte) (string, string) {
	var lines []string
	for _, l := range strings.Split(string(diag), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	last := ""
	for _, l := range lines {
		if lower := strings.ToLower(l); strings.HasPrefix(lower, "fatal:") || strings.HasPrefix(lower, "error:") || last == "" {
			last = l
		}
	}
	match := func(f func(string) bool) (string, bool) {
		for _, l := range lines {
			if f(strings.ToLower(l)) {
				return l, true
			}
		}
		return "", false
	}
	if l, ok := match(credentialFailure.MatchString); ok {
		return "no credentials", l
	}
	if l, ok := match(func(l string) bool {
		return strings.Contains(l, "certificate") || strings.Contains(l, "host key verification failed")
	}); ok {
		return "a certificate or host key not trusted", l
	}
	for _, l := range lines {
		if cause := transportCause([]byte(l)); cause != "" {
			return cause, l
		}
	}
	if l, ok := match(noSuchRepository.MatchString); ok {
		return "the remote has no such repository or branch", l
	}
	if l, ok := match(serverRefused); ok {
		return "the server refused (an HTTP 4xx answer)", l
	}
	return otherFailure, last
}

// otherFailure is the class of a failure no other class names.
const otherFailure = "another failure"

// credentialFailure is Git with no credentials it can use: none it can get without a prompt, which the host never
// allows; ones the server refused (the only credentials a host call has are the machine's own: gh's token, an ssh key
// it offers, a token an organization's SAML SSO has not authorized); a key the ssh agent would not sign with.
var credentialFailure = regexp.MustCompile(`could not read (username|password)|terminal prompts disabled|authentication failed|` +
	`invalid username or (password|token)|returned error: 401|permission denied \(publickey|agent refused operation|` +
	`signing failed|saml sso`)

// noSuchRepository is a remote that has no such repository or branch: GitHub's "Repository not found", git's
// "repository 'URL' not found" for an https 404, a path that is no repository, a branch it does not hold, a 404.
var noSuchRepository = regexp.MustCompile(`repository not found|repository '[^']*' not found|does not appear to be a git repository|` +
	`couldn't find remote ref|returned error: 404`)

var httpAnswer = regexp.MustCompile(`returned error: (4[0-9][0-9])`)

// serverRefused is any other HTTP 4xx answer but a 408 and a 429, which ask the client to try again later (another
// failure, then).
func serverRefused(l string) bool {
	m := httpAnswer.FindStringSubmatch(l)
	return m != nil && m[1] != "408" && m[1] != "429"
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
