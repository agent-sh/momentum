package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The worktrees rule. A linked worktree is left over when its branch, pushed under its own name, is merged into its
// remote's default branch, its changes are all there already, or it is gone from its remote. One that is clean
// (no change, no untracked file, no ignored file that is not a rebuildable cache, no file whose edits git hides)
// and whose commits are on a branch its remote still has, or merged, is removed with `git worktree remove`, and its
// branch with it, if the branch is still where it was judged. One with uncommitted or unpushed work is only
// reported. A worktree a process is in, one changed in the last quietTime (an agent finishing after its merge), a
// locked one and an excluded project's are left; the excluded project's leftovers are reported. A branch never
// pushed under its own name (a new branch that still tracks the default branch), one never committed to, and a
// detached worktree are work in progress, not leftovers.

// quietTime is how long a leftover worktree must be unchanged before it goes: an agent often runs its last steps in
// the worktree right after its branch merged.
const quietTime = time.Hour

// remoteHeads caches each remote's branches and their commits for a run: one ls-remote per repository and remote.
type remoteHeads struct {
	mu   sync.Mutex
	byID map[string]map[string]string
	errs map[string]error
}

func newRemoteHeads() *remoteHeads {
	return &remoteHeads{byID: map[string]map[string]string{}, errs: map[string]error{}}
}

func (r *runner) worktrees(ctx context.Context, paths []string) ([]Finding, error) {
	var out []Finding
	for _, w := range paths {
		canon := r.m.Canon(w)
		if _, inUse := r.sample.InUse(canon); inUse {
			continue
		}
		f, ok := r.worktree(ctx, w, canon, r.heads, r.sample)
		if ok {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// worktree decides on one linked worktree, by a look at the processes; false leaves it out (not a leftover).
func (r *runner) worktree(ctx context.Context, w, canon string, heads *remoteHeads, look *Sample) (Finding, bool) {
	f := Finding{Rule: RuleWorktrees, Path: w, canon: canon}
	gitdir, ok := isLinkedWorktree(w)
	if !ok {
		return f, false
	}
	common, err := gitOut(ctx, w, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return f, false // git cannot read it: not ours to judge
	}
	f.repo = strings.TrimSpace(common)
	f.ino, _ = inodeOf(w)
	branch, _ := gitOut(ctx, w, "symbolic-ref", "--quiet", "--short", "HEAD")
	if f.branch = strings.TrimSpace(branch); f.branch == "" {
		return f, false // detached: no branch to be merged
	}
	head, err := gitOut(ctx, w, "rev-parse", "HEAD")
	if err != nil {
		return f, false
	}
	f.head = strings.TrimSpace(head)
	remote, rbranch := r.upstream(ctx, w, f.branch)
	if remote == "" {
		return f, false // never pushed: work in progress
	}
	merged, into := r.mergedInto(ctx, w, remote)
	if into == "" || strings.TrimPrefix(into, remote+"/") == rbranch {
		return f, false // it tracks the default branch itself: not pushed under its own name
	}
	landed := !merged && r.landed(ctx, w, into)
	var hs map[string]string
	gone := false
	if !merged && !landed {
		var err error
		if hs, err = heads.of(ctx, w, f.repo, remote); err != nil {
			return f, false // the remote cannot be asked: nothing is known
		}
		_, live := hs[rbranch]
		gone = !live
	}
	switch {
	case merged:
		f.Detail = "merged into " + into
	case landed:
		f.Detail = "its changes are in " + into
	case gone:
		f.Detail = "its branch is gone from " + remote
	default:
		return f, false // its branch is still on its remote
	}
	f.Detail += fmt.Sprintf("; its branch %s was at %s", f.branch, shortSHA(f.head))
	if newBranch(ctx, w, f.branch) {
		return f, false // made and never committed to: work about to begin
	}
	if r.changedIn(ctx, canon, r.now.Add(-quietTime), look) {
		f.Action, f.Why = ActKeep, "changed in the last "+quietTime.String()
		return f, true
	}
	t := Measure(w, nil)
	f.Bytes, f.Newest, f.Idle = t.Bytes, t.Newest, t.Newest
	if why, ok := r.excluded(canon); ok {
		f.Action, f.class, f.Why = ActReport, "excluded", "an excluded project's: "+why
		return f, true
	}
	if why, ok := r.ex.byPath("/" + f.branch); ok { // a lane's branch carries its project's name
		f.Action, f.class, f.Why = ActReport, "excluded", "an excluded project's branch: "+why
		return f, true
	}
	if rp := r.git.repoOf(ctx, w); rp != nil {
		if why, ok := r.excludedRepo(rp); ok {
			f.Action, f.class, f.Why = ActReport, "excluded", "an excluded project's: "+why
			return f, true
		}
	}
	if j, ok := r.named.Protects(canon); ok {
		f.Action, f.Why = ActKeep, "named by "+j
		return f, true
	}
	if b, err := os.ReadFile(filepath.Join(gitdir, "locked")); err == nil {
		f.Action, f.Why = ActKeep, "locked: "+strings.TrimSpace(string(b))
		return f, true
	}
	if busy := r.repoBusy(ctx, w, canon, look); busy != "" {
		f.Action, f.Why = ActKeep, "its repository is in use: "+busy
		return f, true
	}
	if class, why := r.unsaved(ctx, w); class != "" {
		f.Action, f.class, f.Why = ActReport, class, why
		return f, true
	}
	if !merged && !landed {
		// gone: its commits must be on a branch the remote still has, at the commit the remote has it at; a
		// remote-tracking branch this clone kept after the remote deleted or rewrote the branch proves nothing
		n, err := aheadOfLive(ctx, w, hs)
		if err != nil {
			f.Action, f.class, f.Why = ActReport, "unreadable", "git rev-list failed: "+err.Error()
			return f, true
		}
		if n > 0 {
			f.Action, f.class = ActReport, "unpushed"
			f.Why = fmt.Sprintf("%d commits on no branch %s still has, and their changes are not in %s", n, remote, into)
			return f, true
		}
	}
	f.Action = ActDelete
	return f, true
}

// unsaved names work in a worktree that git status does not count and git worktree remove would delete: a change,
// an untracked file, an ignored file or directory that is not a rebuildable cache, or a file marked skip-worktree
// or assume-unchanged, whose edits status hides. It returns the report class and why, or "".
func (r *runner) unsaved(ctx context.Context, w string) (string, string) {
	status, err := gitOut(ctx, w, "status", "--porcelain=v1", "--untracked-files=normal", "--ignored=matching")
	if err != nil {
		return "unreadable", "git status failed: " + err.Error()
	}
	var changed int
	var ignored []string
	for _, l := range strings.Split(status, "\n") {
		switch {
		case strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, "!! "):
			p := strings.Trim(strings.TrimPrefix(l, "!! "), "\"")
			if !rebuildable(filepath.Join(w, p)) {
				ignored = append(ignored, strings.TrimSuffix(p, "/"))
			}
		default:
			changed++
		}
	}
	if changed > 0 {
		return "uncommitted", fmt.Sprintf("%d uncommitted or untracked files", changed)
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		names := strings.Join(ignored[:min(len(ignored), 3)], ", ")
		if len(ignored) > 3 {
			names += fmt.Sprintf(" and %d more", len(ignored)-3)
		}
		return "ignored", fmt.Sprintf("ignored files that are not a rebuildable cache, which removing it would delete: %s", names)
	}
	files, err := gitOut(ctx, w, "ls-files", "-v")
	if err != nil {
		return "unreadable", "git ls-files failed: " + err.Error()
	}
	hidden := 0
	for _, l := range strings.Split(files, "\n") {
		if l != "" && (l[0] == 'S' || (l[0] >= 'a' && l[0] <= 'z')) {
			hidden++
		}
	}
	if hidden > 0 {
		return "uncommitted", fmt.Sprintf("%d files marked skip-worktree or assume-unchanged, whose edits git status hides", hidden)
	}
	return "", ""
}

// rebuildableNames are ignored directories that hold only what a tool makes again.
var rebuildableNames = map[string]bool{"node_modules": true, "__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	".ruff_cache": true, ".venv": true, ".tox": true, ".nox": true, ".gradle": true, ".next": true, ".turbo": true,
	".parcel-cache": true, ".svelte-kit": true}

// rebuildable reports whether an ignored path holds only what a tool makes again: a directory tagged as a cache
// (CACHEDIR.TAG), one of rebuildableNames, or a compiled Python file. A Cargo target directory is rebuildable only
// when it holds nothing but what Cargo makes (Criterion's saved baselines are not).
func rebuildable(p string) bool {
	p = strings.TrimSuffix(p, "/")
	if strings.HasSuffix(p, ".pyc") {
		return true
	}
	if rebuildableNames[filepath.Base(p)] {
		return true
	}
	b := readHead(filepath.Join(p, "CACHEDIR.TAG"), 64)
	if !strings.HasPrefix(string(b), cargoTag) { // the tag's signature, the same for every tool
		return false
	}
	if isTarget(p) {
		_, foreign := cargoEntries(p)
		return len(foreign) == 0
	}
	return true
}

// aheadOfLive counts HEAD's commits that are on no branch the remote has now, by the commits ls-remote gave: not
// by this clone's remote-tracking branches, which may hold what the remote since deleted or rewrote. A remote
// commit this clone does not have is passed over, so what only it would hold counts as unpushed.
func aheadOfLive(ctx context.Context, w string, heads map[string]string) (int, error) {
	in := "HEAD\n"
	for _, sha := range heads {
		in += "^" + sha + "\n"
	}
	n, err := gitIn(ctx, w, in, "rev-list", "--count", "--ignore-missing", "--stdin")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(n))
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// repoBusy names what works in a worktree's repository now, or "": a process (other than this one) whose working
// directory is in any of the repository's work trees, or a live agent session that works in one. The scheduled-AI
// policy leaves a repository someone works on alone; a branch of it is not deleted under them.
func (r *runner) repoBusy(ctx context.Context, w, canon string, look *Sample) string {
	out, err := gitOut(ctx, w, "worktree", "list", "--porcelain")
	if err != nil {
		return "git worktree list failed: " + err.Error()
	}
	var trees []string
	bare := map[string]bool{}
	for _, block := range strings.Split(out, "\n\n") {
		var tree string
		for _, l := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(l, "worktree "); ok {
				tree = r.m.Canon(p)
			} else if l == "bare" && tree != "" {
				bare[tree] = true
			}
		}
		if tree != "" && tree != canon {
			trees = append(trees, tree)
		}
	}
	self := os.Getpid()
	for _, t := range trees {
		for _, p := range look.Table {
			if p.PID != self && p.Cwd != "" && under(p.Cwd, t) {
				return fmt.Sprintf("%s (pid %d) in %s", p.Comm, p.PID, short(t))
			}
		}
		ss, err := r.liveSessions(ctx)
		if err != nil {
			return r.sessionsUnread(err)
		}
		for _, s := range ss {
			if s.Cwd != "" && under(r.m.Canon(s.Cwd), t) {
				return fmt.Sprintf("session %s (%s) in %s", firstNonEmpty(s.Name, s.Key), s.Tool, short(t))
			}
		}
	}
	for _, t := range trees {
		if !bare[t] && r.changedIn(ctx, t, r.now.Add(-quietTime), look) {
			return fmt.Sprintf("a change in %s in the last %s", short(t), quietTime)
		}
	}
	return ""
}

// changedIn reports whether someone changed a work tree since a time: an entry made, removed or renamed in one of
// its directories, or git's own record of it (its index and HEAD, and the last entry of HEAD's log: an add, a
// commit, a checkout). An agent between two commands runs no process there, but leaves these. A fetch writes only
// under .git's objects and remote refs, and a gc rewrites HEAD's log with its entries kept, so neither is a change.
// A change capacity's own deletion made is no change, unless someone changed the directory in the window before it
// (own). Each tree is looked at once per look.
func (r *runner) changedIn(ctx context.Context, tree string, since time.Time, look *Sample) bool {
	key := changeKey{look, tree}
	if v, ok := r.changed[key]; ok {
		return v
	}
	gitdir := filepath.Join(tree, ".git")
	if g, _ := isLinkedWorktree(tree); g != "" {
		gitdir = g // a linked worktree's, or a main checkout's kept apart (clone --separate-git-dir)
	}
	v := false
	for _, f := range []string{"index", "HEAD"} {
		if st, err := os.Lstat(filepath.Join(gitdir, f)); err == nil {
			if s, ok := st.Sys().(*syscall.Stat_t); ok && newest(s).After(since) {
				v = true
			}
		}
	}
	if t, ok := reflogTime(filepath.Join(gitdir, "logs", "HEAD")); !v && ok && t.After(since) {
		v = true
	}
	if !v {
		own := r.ownDeletions(ctx)
		v = newerThanBut(tree, since, func(d string, at time.Time) bool {
			// its last change is capacity's: someone's is the one before, which may still be in the window
			o, ok := own[d]
			return ok && !at.After(o.at.Add(time.Second)) && !o.prev.After(since)
		})
	}
	if r.changed == nil {
		r.changed = map[changeKey]bool{}
	}
	r.changed[key] = v
	return v
}

var zoneRe = regexp.MustCompile(`^[+-][0-9]{4}$`)

// reflogTime is the time of a reflog's last entry ("<old> <new> <name> <<email>> <unix time> <zone>\t<message>"),
// or the file's own time when its tail cannot be read as one.
func reflogTime(p string) (time.Time, bool) {
	f, err := os.Open(p)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	off := max(0, st.Size()-64<<10)
	b := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(b, off); err == nil {
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		// the last entry whole (its start in the window), and its header as git writes it
		if len(lines) > 1 || off == 0 {
			head, _, _ := strings.Cut(lines[len(lines)-1], "\t")
			fs := strings.Fields(head)
			if n := len(fs); n >= 6 && strings.HasSuffix(fs[n-3], ">") && zoneRe.MatchString(fs[n-1]) {
				if sec, err := strconv.ParseInt(fs[n-2], 10, 64); err == nil {
					return time.Unix(sec, 0), true
				}
			}
		}
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return newest(s), true
	}
	return time.Time{}, false
}

// ownChange is capacity's change to a directory: when it deleted something in it, and the directory's last change
// before that which was not capacity's. After the deletion the directory's time is capacity's; whether someone
// changed it lately is what prev says, judged by each run against its own window.
type ownChange struct{ at, prev time.Time }

// ownDir is a directory a deletion changes, with its last change before the deletion that was not capacity's.
type ownDir struct {
	Dir  string    `json:"dir"`
	Prev time.Time `json:"prev"`
}

// ownDeletions are the directories capacity's deletions and offloads changed: this run's, as it makes them, and the
// last hour's, from their events (an event without the list, from an older build, explains nothing).
func (r *runner) ownDeletions(ctx context.Context) map[string]ownChange {
	if r.own != nil {
		return r.own
	}
	r.own = map[string]ownChange{}
	if r.st == nil {
		return r.own
	}
	evs, err := r.st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindDelete, KindOffload}, Since: r.now.Add(-quietTime),
		Limit: math.MaxInt32})
	if err != nil {
		return r.own // none known: a repository looks busier, never idler
	}
	for _, e := range evs {
		var m struct {
			Own []ownDir `json:"own"`
		}
		if json.Unmarshal([]byte(e.Meta), &m) == nil {
			for _, d := range m.Own {
				r.addOwn(d, e.TS)
			}
		}
	}
	return r.own
}

func (r *runner) addOwn(d ownDir, at time.Time) {
	if at.After(r.own[d.Dir].at) {
		r.own[d.Dir] = ownChange{at: at, prev: d.Prev}
	}
}

// ownBefore are the directories a deletion of p will change (p, the directory it is in, and more that a deletion in it
// changes), each with its last change that was not capacity's: its time now, or, when that is capacity's own
// deletion, what that one recorded.
func (r *runner) ownBefore(ctx context.Context, p string, more ...string) []ownDir {
	own := r.ownDeletions(ctx)
	var out []ownDir
	seen := map[string]bool{}
	for _, d := range append([]string{p, filepath.Dir(p)}, more...) {
		if seen[d] {
			continue
		}
		seen[d] = true
		st, err := os.Lstat(d)
		if err != nil {
			continue
		}
		s, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		prev := newest(s)
		if o, ok := own[d]; ok && !prev.After(o.at.Add(time.Second)) {
			prev = o.prev
		}
		out = append(out, ownDir{Dir: d, Prev: prev})
	}
	return out
}

// changeKey is a work tree, as one look judged it.
type changeKey struct {
	look *Sample
	tree string
}

// liveSessions are the agent sessions on the machine, read once per run (and again for each act's second look), or
// why the look at them was not whole.
func (r *runner) liveSessions(ctx context.Context) (map[int]Session, error) {
	if r.sessions == nil {
		r.sessions, r.sessionsErr = map[int]Session{}, nil
		if r.opt.Sessions != nil {
			ss, err := r.opt.Sessions(ctx)
			switch {
			case err != nil:
				r.sessionsErr = err
			case ss != nil:
				r.sessions, r.sessionsWhole = ss, true
			default:
				r.sessionsWhole = true
			}
		}
	}
	return r.sessions, r.sessionsErr
}

// sessionsUnread says why an item a session may protect stays when the look at the sessions was not whole, and
// holds the run for it: its hold note.
func (r *runner) sessionsUnread(err error) string {
	r.sessionsHeld = err.Error()
	return "the live agent sessions could not all be read (" + err.Error() + ")"
}

// upstream is a branch's remote and its name there: its upstream, else a remote-tracking branch of the same name.
// None means it was never pushed.
func (r *runner) upstream(ctx context.Context, w, branch string) (remote, name string) {
	if rm, err := gitOut(ctx, w, "config", "--get", "branch."+branch+".remote"); err == nil {
		if mg, err := gitOut(ctx, w, "config", "--get", "branch."+branch+".merge"); err == nil {
			rm, mg = strings.TrimSpace(rm), strings.TrimPrefix(strings.TrimSpace(mg), "refs/heads/")
			if rm != "" && rm != "." && mg != "" {
				return rm, mg
			}
		}
	}
	out, err := gitOut(ctx, w, "for-each-ref", "--format=%(refname)", "refs/remotes/")
	if err != nil {
		return "", ""
	}
	for _, l := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(l), "refs/remotes/")
		if !ok {
			continue
		}
		if rm, nm, ok := strings.Cut(rest, "/"); ok && nm == branch {
			return rm, nm
		}
	}
	return "", ""
}

// mergedInto reports whether HEAD is merged into the remote's default branch (its HEAD, else main or master), as
// the remote-tracking branch knows it.
func (r *runner) mergedInto(ctx context.Context, w, remote string) (bool, string) {
	def := ""
	if out, err := gitOut(ctx, w, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD"); err == nil {
		def = strings.TrimSpace(out)
	}
	if def == "" {
		for _, b := range []string{"main", "master"} {
			if ok, _ := gitOK(ctx, w, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+b); ok {
				def = "refs/remotes/" + remote + "/" + b
				break
			}
		}
	}
	if def == "" {
		return false, ""
	}
	ok, _ := gitOK(ctx, w, "merge-base", "--is-ancestor", "HEAD", def)
	return ok, strings.TrimPrefix(def, "refs/remotes/")
}

// landed reports whether HEAD's changes are all in the default branch already, though its commits are not (a
// squash merge, a cherry-pick): merging it into the default branch would change nothing.
func (r *runner) landed(ctx context.Context, w, into string) bool {
	out, err := gitOut(ctx, w, "merge-tree", "--write-tree", "refs/remotes/"+into, "HEAD")
	if err != nil {
		return false // a conflict, or a git without merge-tree: not known to have landed
	}
	tree, err := gitOut(ctx, w, "rev-parse", "refs/remotes/"+into+"^{tree}")
	return err == nil && firstLine(strings.TrimSpace(out)) == strings.TrimSpace(tree)
}

// newBranch reports whether a branch's reflog holds only its creation: nothing was ever committed to it. Without a
// reflog it cannot be told, and is not new.
func newBranch(ctx context.Context, w, branch string) bool {
	out, err := gitOut(ctx, w, "reflog", "show", "--format=%gs", "refs/heads/"+branch, "--")
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return len(lines) == 1 && strings.HasPrefix(lines[0], "branch: Created from")
}

// of is a remote's branches, asked once per repository and remote.
func (h *remoteHeads) of(ctx context.Context, w, common, remote string) (map[string]string, error) {
	id := common + "\x00" + remote
	h.mu.Lock()
	if hs, ok := h.byID[id]; ok {
		h.mu.Unlock()
		return hs, nil
	}
	if err, ok := h.errs[id]; ok {
		h.mu.Unlock()
		return nil, err
	}
	h.mu.Unlock()
	out, err := gitRun(ctx, 45*time.Second, w, "ls-remote", "--heads", remote)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.errs[id] = err
		return nil, err
	}
	hs := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			hs[strings.TrimPrefix(f[1], "refs/heads/")] = f[0]
		}
	}
	h.byID[id] = hs
	return hs, nil
}

// removeWorktree removes a worktree with git, which refuses one that is not clean, and then its branch, only if
// the branch is still at the commit it was judged at.
func (r *runner) removeWorktree(ctx context.Context, f *Finding) error {
	main := repoPlace(f.repo)
	if _, err := gitRun(ctx, 2*time.Minute, main, "worktree", "remove", f.Path); err != nil {
		return err
	}
	if f.branch != "" {
		if _, err := gitRun(ctx, 30*time.Second, main, "update-ref", "-d", "refs/heads/"+f.branch, f.head); err != nil {
			return fmt.Errorf("the worktree is removed; its branch %s stays (it moved since it was judged, or git refused): %w", f.branch, err)
		}
	}
	return nil
}
