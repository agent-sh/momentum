package capacity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The worktrees rule. A linked worktree is left over when its branch, pushed under its own name, is merged into its
// remote's default branch or is gone from its remote. One that is clean (no change, no untracked file) and fully
// pushed (every commit on it is on a remote) is removed with `git worktree remove`, and its branch with it. One
// with uncommitted or unpushed work is only reported. A worktree a process is in, one changed in the last
// quietTime (an agent finishing after its merge), a locked one and an excluded project's are left; the excluded
// project's leftovers are reported. A branch never pushed under its own name (a new branch that still tracks the
// default branch) is work in progress, and a detached worktree has no branch: neither is a leftover.

// quietTime is how long a leftover worktree must be unchanged before it goes: an agent often runs its last steps in
// the worktree right after its branch merged.
const quietTime = time.Hour

// remoteHeads caches each remote's branches for a run: one ls-remote per repository and remote.
type remoteHeads struct {
	mu   sync.Mutex
	byID map[string]map[string]bool
	errs map[string]error
}

func (r *runner) worktrees(ctx context.Context, paths []string) ([]Finding, error) {
	heads := &remoteHeads{byID: map[string]map[string]bool{}, errs: map[string]error{}}
	var out []Finding
	for _, w := range paths {
		canon := r.m.Canon(w)
		if _, inUse := r.sample.InUse(canon); inUse {
			continue
		}
		f, ok := r.worktree(ctx, w, canon, heads)
		if ok {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// worktree decides on one linked worktree; false leaves it out (not a leftover).
func (r *runner) worktree(ctx context.Context, w, canon string, heads *remoteHeads) (Finding, bool) {
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
	branch, _ := gitOut(ctx, w, "symbolic-ref", "--quiet", "--short", "HEAD")
	if f.branch = strings.TrimSpace(branch); f.branch == "" {
		return f, false // detached: no branch to be merged
	}
	remote, rbranch := r.upstream(ctx, w, f.branch)
	if remote == "" {
		return f, false // never pushed: work in progress
	}
	merged, into := r.mergedInto(ctx, w, remote)
	if into == "" || strings.TrimPrefix(into, remote+"/") == rbranch {
		return f, false // it tracks the default branch itself: not pushed under its own name
	}
	landed := !merged && r.landed(ctx, w, into)
	gone := false
	if !merged && !landed {
		hs, err := heads.of(ctx, w, f.repo, remote)
		if err != nil {
			return f, false // the remote cannot be asked: nothing is known
		}
		gone = !hs[rbranch]
	}
	switch {
	case merged:
		f.Detail = "merged into " + into
	case landed:
		f.Detail = "its changes are in " + into
	case gone:
		f.Detail = "its branch is gone from " + remote
	default:
		return f, false
	}
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
	status, err := gitOut(ctx, w, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		f.Action, f.class, f.Why = ActReport, "unreadable", "git status failed: "+err.Error()
		return f, true
	}
	if n := countLines(status); n > 0 {
		f.Action, f.class, f.Why = ActReport, "uncommitted", fmt.Sprintf("%d uncommitted or untracked files", n)
		return f, true
	}
	ahead, err := gitOut(ctx, w, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		f.Action, f.class, f.Why = ActReport, "unreadable", "git rev-list failed: "+err.Error()
		return f, true
	}
	if n := strings.TrimSpace(ahead); n != "0" && !merged && !landed {
		f.Action, f.class, f.Why = ActReport, "unpushed", n+" commits on no remote, and their changes are not in "+into
		return f, true
	}
	f.Action = ActDelete
	return f, true
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
func (h *remoteHeads) of(ctx context.Context, w, common, remote string) (map[string]bool, error) {
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
	hs := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			hs[strings.TrimPrefix(f[1], "refs/heads/")] = true
		}
	}
	h.byID[id] = hs
	return hs, nil
}

// removeWorktree removes a worktree with git, which refuses one that is not clean, and then its branch.
func (r *runner) removeWorktree(ctx context.Context, f *Finding) error {
	main := f.repo
	if filepath.Base(main) == ".git" {
		main = filepath.Dir(main)
	}
	if _, err := gitRun(ctx, 2*time.Minute, main, "worktree", "remove", f.Path); err != nil {
		return err
	}
	if f.branch != "" {
		if _, err := gitRun(ctx, 30*time.Second, main, "branch", "-D", f.branch); err != nil {
			return fmt.Errorf("the worktree is removed, its branch %s is not: %w", f.branch, err)
		}
	}
	return nil
}

func countLines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
