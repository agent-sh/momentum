package capacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// act deletes what the live judging rules (models, targets, worktrees, scratch) found, and offloads the tasks'
// folders scratch found. Nothing is deleted when the scheduled jobs could not all be read, or when the run cannot
// trust its look at the processes. Before each deletion the item is judged again: the same inode as when it was
// judged, no process using it (one fresh look at the processes, Docker's mounts among them), no write since, for a
// worktree the same commit, still clean and still a leftover, and for a task's folder no live session in it or of
// it. An item that holds a mount point is only reported: a deletion would reach into another filesystem, or through
// a bind mount into another place. Each deletion counts what it frees (countAfter) and is recorded at once, so a run
// stopped midway leaves what it did on record. Offloads start for scratchBudget at most; the rest wait for the next
// run.
func (r *runner) act(ctx context.Context) error {
	var todo []*Finding
	for i := range r.res.Rules {
		rr := &r.res.Rules[i]
		if !rr.Live || !judging(rr.Rule) {
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
		r.dockerErr, r.dockerAsked = err.Error(), true
		for _, f := range todo {
			f.Action, f.Why = ActKeep, "Docker could not be asked which containers run"
		}
		return nil
	}
	r.sessions = nil // the sessions read again, for the worktrees' and task folders' second look
	start := time.Now()
	for i, f := range todo {
		if f.Rule == RuleScratch && time.Since(start) > scratchBudget {
			f.Action, f.Why = ActKeep, "waits for the next run: this run's time for offloads is spent"
			continue
		}
		if f.Rule == RuleScratch && i > 0 { // an offload takes minutes: each folder gets a look of its own
			r.fresh = TakeSample(r.opt.ProcRoot, r.m, r.watched)
			if err := r.addContainerUses(ctx, r.fresh); err != nil {
				f.Action, f.Why = ActKeep, "Docker could not be asked which containers run"
				continue
			}
			r.sessions = nil
		}
		// what its deletion frees counts from its paths' links before the recheck, so the recheck stays the last look
		// before it: a task folder's model data here, the rest as the offload finds it
		paths := r.deletes(f)
		if f.Rule == RuleScratch {
			paths = modelPaths(f)
		}
		before := linksOf(paths, nil)
		if why := r.recheck(ctx, f); why != "" {
			f.Action, f.Why = ActKeep, why
			continue
		}
		if mp, ok := r.m.mountWithin(f.Path, f.canon); ok {
			f.Action, f.class, f.Why = ActReport, "mount", "it holds a mount point ("+mp+"): a deletion would reach past it"
			continue
		}
		var inner []string // the directories a task folder's model data is in, which its deletion changes
		for _, mf := range f.files {
			inner = append(inner, filepath.Join(f.canon, filepath.Dir(mf.rel)))
		}
		dirs := r.ownBefore(ctx, f.canon, inner...)
		del := func(rm func() error) error {
			if r.opt.duringDelete != nil {
				r.opt.duringDelete(*f)
			}
			err := rm()
			var took links
			f.went, f.Freed, took = countAfter(paths, before)
			f.took = []links{took}
			return err
		}
		var err error
		switch f.Rule {
		case RuleWorktrees:
			err = del(func() error { return r.removeWorktree(ctx, f) })
		case RuleModels:
			err = del(func() error { return r.removeModel(f) })
		case RuleScratch:
			f.ownDirs = dirs
			err = r.actScratch(ctx, f, dirs, before)
		default:
			err = del(func() error { return r.removeTarget(f) })
		}
		if errors.Is(err, errLocked) || errors.Is(err, errChanged) {
			f.Action, f.Why = ActKeep, err.Error()
			continue
		}
		if f.Rule == RuleScratch && errors.Is(err, errTimeout) && !r.opt.DryRun {
			// its next look reports it, until it is smaller: another try would run out of time too
			mark := slowMark + f.canon + "|" + strconv.FormatInt(f.Bytes-f.removed, 10)
			if serr := r.st.SetMark(ctx, mark, r.now); serr != nil {
				return serr
			}
		}
		if err != nil {
			f.Err = err.Error()
			if _, statErr := os.Lstat(f.Path); statErr == nil || f.Rule == RuleScratch {
				continue // still there, or a task folder without its pointer: a failure, reported
			}
		}
		f.Done = true
		if f.Rule != RuleScratch { // its size is what went with it: what of it a deletion before took is that one's
			f.Bytes = f.went
		}
		if r.opt.afterDelete != nil {
			r.opt.afterDelete(*f)
		}
		f.ownDirs = dirs
		at := r.opt.Clock()
		for _, d := range dirs {
			r.addOwn(d, at)
		}
		if !r.opt.DryRun {
			if err := r.recordDone(ctx, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// actScratch deletes a task folder's model data and offloads the rest, and counts what each freed: the model data's
// files from their links before (counted before the recheck), and the folder as the offload found it. The model
// data's deletion is on record before the offload starts.
func (r *runner) actScratch(ctx context.Context, f *Finding, dirs []ownDir, before links) error {
	models := modelPaths(f)
	counted := false
	countModels := func() {
		if !counted {
			var took links
			f.modelWent, f.modelFreed, took = countAfter(models, before)
			f.took, counted = []links{took}, true
		}
	}
	var rest *links
	err := r.removeScratch(ctx, f, func() error {
		countModels()
		at := r.opt.Clock()
		for _, d := range dirs {
			r.addOwn(d, at)
		}
		if r.opt.DryRun {
			return nil
		}
		return r.recordModelData(ctx, f)
	}, func() {
		l := linksOf([]string{f.Path}, nil)
		rest = &l
	})
	countModels()
	f.went, f.Freed = f.modelWent, f.modelFreed
	if rest != nil {
		went, freed, took := countAfter([]string{f.Path}, *rest)
		f.went, f.Freed, f.took = f.went+went, f.Freed+freed, append(f.took, took)
	}
	if err != nil && f.removed > 0 { // its model data went; the folder stays, its failure reported
		f.Detail = fmt.Sprintf("deleted %s of its model data; then: %s", gib(f.removed), f.Detail)
	}
	return err
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
		if t := Measure(f.Path); t.Newest.After(f.Newest) {
			return "written since it was judged"
		}
	case RuleTargets:
		if newerThan(f.Path, f.Newest) {
			return "built since it was judged"
		}
	case RuleScratch:
		if t := Measure(f.Path); t.Newest.After(f.Newest) {
			return "written since it was judged"
		}
		if why := r.sessionIn(ctx, f.canon, f.makers); why != "" {
			return why
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
