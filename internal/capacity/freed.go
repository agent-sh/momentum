package capacity

import (
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// What a deletion frees. A file's blocks come back when its last link goes: a wheel a project's environment still
// links, a clone's objects its source still links, a file another item links, free nothing when this link goes. So a
// deletion's freed bytes are those of the paths it removes that only those paths hold: each directory, each file with
// one link, and each file whose every link is among them, each inode once. A live run counts the paths just before
// each deletion and what is left of them after (countFreed), so a link an earlier deletion of the run took is gone by
// then, and only what went counts. A run that only says what it would do counts them as it judged them, in the order
// it would delete them (estimate).

// links are what a set of paths holds on disk, by link. own is the bytes only the set holds; shared are its files
// with links outside it, by inode: what another deletion of the run may take the rest of.
type links struct {
	own    int64
	shared map[inode]share
}

// share is a file with links outside a set: its bytes on disk, its links in the set, and all its links (st_nlink).
type share struct {
	blocks int64
	in, n  uint64
}

// add counts one entry a walk found: each path is one link of its inode, and a directory has one name. first is
// false for an inode the walk saw already, whose second sight of a directory or of a file with one link is the same
// name again (through a bind mount).
func (l *links) add(s *syscall.Stat_t, first bool) {
	b := s.Blocks * 512
	if s.Mode&syscall.S_IFMT == syscall.S_IFDIR || s.Nlink <= 1 {
		if first {
			l.own += b
		}
		return
	}
	if l.shared == nil {
		l.shared = map[inode]share{}
	}
	k := inode{uint64(s.Dev), s.Ino}
	c, ok := l.shared[k]
	if !ok {
		c = share{blocks: b, n: uint64(s.Nlink)}
	}
	c.in++
	l.shared[k] = c
}

// settle moves into own each file whose every link is in the set.
func (l *links) settle() {
	for k, c := range l.shared {
		if c.in >= c.n {
			l.own += c.blocks
			delete(l.shared, k)
		}
	}
}

// merge adds another set's links, of paths apart from this set's.
func (l *links) merge(o links) {
	l.own += o.own
	for k, c := range o.shared {
		if l.shared == nil {
			l.shared = map[inode]share{}
		}
		if x, ok := l.shared[k]; ok {
			c.in += x.in
		}
		l.shared[k] = c
	}
	l.settle()
}

// linksOf counts the links of these paths, each a file or a directory's tree, walked as Measure walks; skip passes over
// a path and what is under it. A path that is gone counts nothing.
func linksOf(paths []string, skip func(string) bool) links {
	var l links
	for _, p := range paths {
		l.merge(measure(p, skip).links)
	}
	return l
}

// countFreed runs a deletion of these paths and says what it freed: what only they held just before it, less what only
// they hold of what is left after it.
func countFreed(paths []string, del func() error) (int64, error) {
	before := linksOf(paths, nil)
	err := del()
	return max(0, before.own-linksOf(paths, nil).own), err
}

// deletes are the paths a model's, a target dir's or a worktree's deletion removes: a model with its shared-store
// blobs and their manifests (only the last repository to go takes one), a target dir's own entries, a worktree with
// its administrative directory, which git removes with it.
func (r *runner) deletes(f *Finding) []string {
	switch f.Rule {
	case RuleModels:
		out := []string{f.Path}
		for _, b := range r.blobPaths(f) {
			out = append(out, b, b+".refs")
		}
		return out
	case RuleTargets:
		if len(f.parts) == 0 {
			return []string{f.Path}
		}
		var out []string
		for _, p := range f.parts {
			out = append(out, filepath.Join(f.Path, p))
		}
		return out
	case RuleWorktrees:
		if f.admin != "" {
			return []string{f.Path, f.admin}
		}
	}
	return []string{f.Path}
}

// blobPaths are the shared-store blobs a model links, under the roots as configured.
func (r *runner) blobPaths(f *Finding) []string {
	var out []string
	for _, b := range f.linked {
		if p, ok := r.blobPath(b); ok {
			out = append(out, p)
		}
	}
	return out
}

// blobPath is a canonical shared blob's path under the root as configured, in the store of a root that has one.
func (r *runner) blobPath(b string) (string, bool) {
	for _, x := range r.modelRoots {
		if under(b, x.canon) && r.stores[x.canon] {
			return r.shownBlob(x, b), true
		}
	}
	return "", false
}

// modelPaths are the files of a task folder's model data.
func modelPaths(f *Finding) []string {
	out := make([]string, len(f.files))
	for i, mf := range f.files {
		out[i] = filepath.Join(f.Path, mf.rel)
	}
	return out
}

// estimate says, for each item a rule that is not live would delete, what its deletion would free (WouldFree): its
// links as the rule judged them, in the order act deletes, a file counting with the deletion that takes its last
// link. A shared blob counts with the last repository linking it to go. An item inside one deleted before it frees
// nothing more, and one that holds one is counted again without it.
func (r *runner) estimate() {
	var todo []*Finding
	for i := range r.res.Rules {
		rr := &r.res.Rules[i]
		if rr.Live || !judging(rr.Rule) {
			continue
		}
		for j := range rr.Findings {
			if rr.Findings[j].Action == ActDelete {
				todo = append(todo, &rr.Findings[j])
			}
		}
	}
	left := map[inode]uint64{} // the links left of each file a deletion before took some of
	take := func(l links) int64 {
		n := l.own
		for k, c := range l.shared {
			have, ok := left[k]
			if !ok {
				have = c.n
			}
			switch {
			case have == 0: // its last link went with a deletion before
			case c.in >= have:
				n, left[k] = n+c.blocks, 0
			default:
				left[k] = have - c.in
			}
		}
		return n
	}
	var taken []string        // the places the deletions before took, by their canonical names
	users := map[string]int{} // how many repositories gone so far link each blob they share
	for _, f := range todo {
		canon := f.canon
		if canon == "" {
			canon = r.m.Canon(f.Path)
		}
		inside, inner := false, []string(nil)
		for _, t := range taken {
			inside = inside || under(canon, t)
			if under(t, canon) { // as the walk of this item spells it
				inner = append(inner, filepath.Join(f.Path, strings.TrimPrefix(t, canon)))
			}
		}
		skip := func(p string) bool {
			for _, t := range inner {
				if under(p, t) {
					return true
				}
			}
			return false
		}
		var n int64
		switch {
		case inside:
		case f.Rule == RuleScratch && len(inner) > 0:
			models := modelPaths(f)
			n = take(linksOf(models, skip))
			if !f.modelOnly {
				n += take(linksOf([]string{f.Path}, func(p string) bool { return skip(p) || slices.Contains(models, p) }))
			}
		case f.Rule == RuleScratch:
			n = take(f.modelLinks)
			if !f.modelOnly {
				n += take(f.links)
			}
		case len(inner) > 0:
			n = take(linksOf(r.deletes(f), skip))
		default:
			n = take(f.links)
		}
		for _, b := range sortedKeys(f.shares) { // a blob other repositories link too goes with the last of them
			if users[b]++; users[b] == f.shares[b] {
				if p, ok := r.blobPath(b); ok {
					n += take(linksOf([]string{p, p + ".refs"}, nil))
				}
			}
		}
		f.WouldFree = &n
		switch {
		case inside:
		case f.Rule == RuleTargets && len(f.parts) > 0:
			for _, p := range f.parts {
				taken = append(taken, filepath.Join(canon, p))
			}
		case f.Rule == RuleScratch && f.modelOnly:
			for _, mf := range f.files {
				taken = append(taken, filepath.Join(canon, mf.rel))
			}
		default:
			taken = append(taken, canon)
		}
	}
}

// byCount says whose count a freed size is, for a note: nothing for capacity's own.
func byCount(tool string) string {
	if tool == "" {
		return ""
	}
	return " by " + tool + "'s count"
}
