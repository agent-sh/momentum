package capacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// act deletes what the live judging rules (models, targets, worktrees) found. Nothing is deleted when the scheduled
// jobs could not all be read, or when the run cannot trust its look at the processes. Before each deletion the
// item is judged again: the same inode as when it was judged, no process using it (one fresh look at the
// processes, Docker's mounts among them), no write since, and for a worktree the same commit, still clean and still
// a leftover. An item that holds a mount point is only reported: a deletion would reach into another filesystem,
// or through a bind mount into another place. Each deletion is recorded at once, so a run stopped midway leaves
// what it did on record.
func (r *runner) act(ctx context.Context) error {
	var todo []*Finding
	for i := range r.res.Rules {
		rr := &r.res.Rules[i]
		if !rr.Live || (rr.Rule != RuleModels && rr.Rule != RuleTargets && rr.Rule != RuleWorktrees) {
			continue
		}
		for j := range rr.Findings {
			if rr.Findings[j].Action == ActDelete {
				todo = append(todo, &rr.Findings[j])
			}
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if r.opt.beforeAct != nil {
		r.opt.beforeAct()
	}
	hold := ""
	switch {
	case r.namedErr != nil:
		hold = "the scheduled jobs could not all be read"
	case r.blind != "":
		hold = r.blind
	case r.dockerErr != "":
		hold = "Docker could not be asked which containers run"
	}
	if hold != "" {
		for _, f := range todo {
			f.Action, f.Why = ActKeep, hold
		}
		return nil
	}
	r.fresh = TakeSample(r.opt.ProcRoot, r.m, r.watched)
	if err := r.addContainerUses(ctx, r.fresh); err != nil {
		for _, f := range todo {
			f.Action, f.Why = ActKeep, "Docker could not be asked which containers run"
		}
		return nil
	}
	r.sessions = nil // the sessions read again, for the worktrees' second look
	for _, f := range todo {
		if why := r.recheck(ctx, f); why != "" {
			f.Action, f.Why = ActKeep, why
			continue
		}
		if mp, ok := r.m.mountWithin(f.Path, f.canon); ok {
			f.Action, f.class, f.Why = ActReport, "mount", "it holds a mount point ("+mp+"): a deletion would reach past it"
			continue
		}
		before := r.free(f.Path)
		var err error
		switch f.Rule {
		case RuleWorktrees:
			err = r.removeWorktree(ctx, f)
		case RuleModels:
			err = r.removeModel(f)
		default:
			err = r.removeTarget(f)
		}
		if after := r.free(f.Path); before >= 0 && after >= 0 {
			f.Freed = max(0, after-before)
		}
		if errors.Is(err, errLocked) {
			f.Action, f.Why = ActKeep, err.Error()
			continue
		}
		if err != nil {
			f.Err = err.Error()
			if _, statErr := os.Lstat(f.Path); statErr == nil {
				continue // still there: a failure, reported
			}
		}
		f.Done = true
		if !r.opt.DryRun {
			if err := r.recordDone(ctx, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// recheck judges an item again just before its deletion, and says why it stays, or "".
func (r *runner) recheck(ctx context.Context, f *Finding) string {
	if ino, err := inodeOf(f.Path); err != nil || ino != f.ino {
		return "it changed since it was judged"
	}
	if u, ok := r.fresh.InUse(f.canon); ok {
		return "in use now: " + useText(u)
	}
	switch f.Rule {
	case RuleModels:
		if u, ok := r.repoInUse(r.fresh, f.canon); ok {
			return "in use now: " + useText(u)
		}
		for _, b := range f.linked {
			if u, ok := r.fresh.InUse(b); ok {
				return "in use now: " + useText(u)
			}
		}
		if t := Measure(f.Path, nil); t.Newest.After(f.Newest) {
			return "written since it was judged"
		}
	case RuleTargets:
		if newerThan(f.Path, f.Newest) {
			return "built since it was judged"
		}
	case RuleWorktrees:
		g, ok := r.worktree(ctx, f.Path, f.canon, r.heads, r.fresh)
		switch {
		case !ok:
			return "no longer a leftover"
		case g.Action != ActDelete:
			return "since it was judged: " + g.Why
		case g.head != f.head:
			return "its branch moved since it was judged"
		}
	}
	return ""
}

// inodeOf is a path's own inode (a symlink's, not its target's).
func inodeOf(p string) (inode, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return inode{}, err
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return inode{}, fmt.Errorf("%s: no inode", p)
	}
	return inode{uint64(s.Dev), s.Ino}, nil
}

// mountWithin names a mount point at or under any of these paths, as the mount table shows it.
func (m *Mounts) mountWithin(paths ...string) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, mt := range m.list {
		for _, p := range paths {
			if p != "" && under(mt.point, p) {
				return mt.point, true
			}
		}
	}
	return "", false
}

// errLocked means another process holds a lock on the directory: systemd-tmpfiles's convention for "do not age
// this tree". The item stays.
var errLocked = errors.New("another process holds a lock on it")

// removeIn deletes rel under dir through an os.Root, so no symlink in the path leads the deletion out of dir, while
// holding an exclusive BSD lock on it when it is a directory: another holder of a lock keeps it.
func removeIn(dir, rel string) error {
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer rt.Close()
	fi, err := rt.Lstat(rel)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		fh, err := rt.Open(rel)
		if err != nil {
			return err
		}
		defer fh.Close()
		if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errLocked
		}
	}
	return rt.RemoveAll(rel)
}

// removeItem deletes an item under the root it was found in.
func (r *runner) removeItem(f *Finding) error {
	rel, err := filepath.Rel(f.root, f.Path)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == "../" {
		return fmt.Errorf("%s is not under %s", f.Path, f.root)
	}
	return removeIn(f.root, rel)
}

// removeTarget deletes a target dir, or only Cargo's own entries of one that holds others.
func (r *runner) removeTarget(f *Finding) error {
	if len(f.parts) == 0 {
		return r.removeItem(f)
	}
	rt, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := syscall.Flock(int(rt.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errLocked
	}
	for _, p := range f.parts {
		if err := removeIn(f.Path, p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// waitLock takes an exclusive BSD lock on a file, trying for up to wait.
func waitLock(f *os.File, wait time.Duration) bool {
	for deadline := time.Now().Add(wait); ; time.Sleep(100 * time.Millisecond) {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}
