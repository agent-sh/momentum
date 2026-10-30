package capacity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
	if newerThan(w, r.now.Add(-quietTime)) {
		f.Action, f.Why = ActKeep, "changed in the last "+quietTime.String()
		return f, true
	}
	t := Measure(w, nil)
	f.Bytes, f.Newest, f.Idle = t.Bytes, t.Newest, t.Newest
	if why, ok := r.ex.byPath(canon); ok {
		f.Action, f.class, f.Why = ActReport, "excluded", "an excluded project's: "+why
		return f, true
	}
	if why, ok := r.ex.byPath("/" + f.branch); ok { // a lane's branch carries its project's name
		f.Action, f.class, f.Why = ActReport, "excluded", "an excluded project's branch: "+why
		return f, true
	}
	if rp := r.git.repoOf(ctx, w); rp != nil {
		if why, ok := r.ex.repo(rp); ok {
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
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			if c := r.m.Canon(p); c != canon {
				trees = append(trees, c)
			}
		}
	}
	self := os.Getpid()
	for _, t := range trees {
		for _, p := range look.Table {
			if p.PID != self && p.Cwd != "" && under(p.Cwd, t) {
				return fmt.Sprintf("%s (pid %d) in %s", p.Comm, p.PID, short(t))
			}
		}
		for _, s := range r.liveSessions(ctx) {
			if s.Cwd != "" && under(r.m.Canon(s.Cwd), t) {
				return fmt.Sprintf("session %s (%s) in %s", firstNonEmpty(s.Name, s.Key), s.Tool, short(t))
			}
		}
	}
	return ""
}

// liveSessions are the agent sessions on the machine, read once per run.
func (r *runner) liveSessions(ctx context.Context) map[int]Session {
	if r.sessions == nil {
		r.sessions = map[int]Session{}
		if r.opt.Sessions != nil {
			if ss := r.opt.Sessions(ctx); ss != nil {
				r.sessions = ss
			}
		}
	}
	return r.sessions
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
	main := f.repo
	if filepath.Base(main) == ".git" {
		main = filepath.Dir(main)
	}
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
