package capacity

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The models rule. The owner's ruling: models, datasets, model caches and run checkpoints are downloaded or made
// again when work on them resumes, so an item that has had no write and was not seen in use for a month goes. An
// item is a top-level entry of a configured root. A root inside another is a container, not an item. A Hugging
// Face cache keeps the files that Xet downloads once, in a store shared by every repository (<cache>/blobs, marked
// by hfStoreMarker), and a repository's blobs are links into it: the store is never an item, a repository's size
// counts the shared files only it links, and they go with it.

// hfStoreMarker marks a Hugging Face cache's shared blob store.
const hfStoreMarker = ".huggingface-shared-blobs"

// hfRepoKinds are the prefixes of a repository's folder in a Hugging Face cache.
var hfRepoKinds = []string{"models--", "datasets--", "spaces--", "kernels--"}

func isRepoDir(name string) bool {
	for _, k := range hfRepoKinds {
		if strings.HasPrefix(name, k) {
			return true
		}
	}
	return false
}

// modelKey is the item a canonical path under the model roots is in: the deepest root holding it, and the first
// component below that root; in a shared blob store, the blob's own path. A root itself is no item.
func (r *runner) modelKey(p string) (string, bool) {
	for _, x := range r.modelRoots {
		if !under(p, x.canon) {
			continue
		}
		if p == x.canon {
			return "", false
		}
		rest := strings.TrimPrefix(p, x.canon+"/")
		first, _, _ := strings.Cut(rest, "/")
		if r.stores[x.canon] && first == "blobs" {
			parts := strings.SplitN(rest, "/", 4)
			if len(parts) < 3 {
				return "", false
			}
			name := strings.TrimSuffix(strings.TrimSuffix(parts[2], ".refs"), ".lock")
			return filepath.Join(x.canon, "blobs", parts[1], name), true
		}
		return filepath.Join(x.canon, first), true
	}
	return "", false
}

// sampleSeen is what this run's sample saw in use under the model roots, by item: the paths processes use, and the
// Hugging Face repositories their command lines name (org/name, in any cache among the roots).
func (r *runner) sampleSeen() []store.Seen {
	byKey := map[string]store.Seen{}
	add := func(key string, u Use) {
		if _, ok := byKey[key]; !ok {
			byKey[key] = store.Seen{Path: key, Last: r.now, How: u.How, Proc: u.Comm + " " + itoa(u.PID)}
		}
	}
	for _, u := range r.sample.Uses {
		if k, ok := r.modelKey(u.Path); ok {
			add(k, u)
		}
		if u.How != "mount" {
			continue
		}
		for _, x := range r.modelRoots { // a container's mount of a whole root uses every item in it
			if !under(x.canon, u.Path) {
				continue
			}
			ents, _ := os.ReadDir(x.shown)
			for _, e := range ents {
				if c := filepath.Join(x.canon, e.Name()); !r.modelSet[c] {
					add(c, u)
				}
			}
		}
	}
	for id, u := range r.sample.RepoIDs {
		for _, k := range r.repoItems(id) {
			add(k, u)
		}
	}
	out := make([]store.Seen, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// repoInUse reports whether a process's command line names the Hugging Face repository an item holds.
func (r *runner) repoInUse(s *Sample, item string) (Use, bool) {
	id := repoIDOf(filepath.Base(item))
	if id == "" || s == nil {
		return Use{}, false
	}
	u, ok := s.RepoIDs[id]
	return u, ok
}

// repoItems are the canonical items that hold a Hugging Face repository org/name in any root.
func (r *runner) repoItems(id string) []string {
	org, name, _ := strings.Cut(id, "/")
	var out []string
	for _, x := range r.modelRoots {
		for _, k := range hfRepoKinds {
			n := k + org + "--" + name
			if _, err := os.Lstat(filepath.Join(x.shown, n)); err == nil {
				out = append(out, filepath.Join(x.canon, n))
			}
		}
	}
	return out
}

// models lists every item of the model roots that has had no write for the idle time, and decides on each.
func (r *runner) models(ctx context.Context) ([]Finding, error) {
	idle := time.Duration(r.cfg.Models.IdleDays * 24 * float64(time.Hour))
	minBytes := int64(r.cfg.MinMiB * (1 << 20))
	var out []Finding
	for _, x := range r.modelRoots {
		ents, err := os.ReadDir(x.shown)
		if err != nil {
			continue
		}
		var items []Finding
		links := map[string][]string{} // item -> the shared blobs it links
		users := map[string][]string{} // shared blob -> the items that link it
		for _, e := range ents {
			canon := filepath.Join(x.canon, e.Name())
			if r.modelSet[canon] || r.kept(canon) || ((r.stores[x.canon] || r.unknownStores[x.canon]) && e.Name() == "blobs") {
				continue
			}
			shown := filepath.Join(x.shown, e.Name())
			t := Measure(shown, nil)
			f := Finding{Rule: RuleModels, Path: shown, canon: canon, Bytes: t.Bytes, Newest: t.Newest, root: x.shown}
			f.ino, _ = inodeOf(shown)
			if (r.stores[x.canon] || r.unknownStores[x.canon]) && isRepoDir(e.Name()) {
				ls := storeLinks(shown, x)
				links[canon] = ls
				for _, b := range ls {
					users[b] = append(users[b], canon)
				}
			}
			items = append(items, f)
		}
		for _, f := range items {
			logical := f.Bytes // with the blobs it shares: an item that only shares still keeps them from going
			for _, b := range links[f.canon] {
				f.linked = append(f.linked, b)
				st, err := os.Lstat(r.shownBlob(x, b))
				if err != nil {
					continue
				}
				if s, ok := st.Sys().(*syscall.Stat_t); ok {
					if w := newest(s); w.After(f.Newest) {
						f.Newest = w
					}
					logical += s.Blocks * 512
					if len(users[b]) == 1 {
						f.blobs = append(f.blobs, b)
						f.Bytes += s.Blocks * 512
					}
				}
			}
			if logical < minBytes {
				continue
			}
			if d, ok := r.decideModel(f, x, idle); ok {
				out = append(out, d)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// kept reports whether an item is in keep, holds a path in keep, or is inside one.
func (r *runner) kept(item string) bool {
	for k := range r.keep {
		if under(k, item) || under(item, k) {
			return true
		}
	}
	return false
}

// lastSeen is the last sighting of an item: of a path in it, of a path above it (a key from before a root was
// added inside another), or of a shared blob it links.
func (r *runner) lastSeen(item string, linked []string) (time.Time, string) {
	var at time.Time
	by := ""
	for k, v := range r.seen {
		if (under(k, item) || under(item, k)) && v.Last.After(at) {
			at, by = v.Last, v.Proc
		}
	}
	for _, b := range linked {
		if v, ok := r.seen[b]; ok && v.Last.After(at) {
			at, by = v.Last, v.Proc
		}
	}
	return at, by
}

// decideModel decides on one item of root x; false leaves it out of the run's list (it has had a write within the
// idle time). An item goes once the sampler has watched it, in use by nothing, for the idle time: watched time
// counts from the latest of its newest write, its last sighting and when the sampler began to watch its root, and
// a gap between two runs counts for at most maxGap.
func (r *runner) decideModel(f Finding, x root, idle time.Duration) (Finding, bool) {
	if r.now.Sub(f.Newest) < idle {
		return f, false
	}
	seen, by := r.lastSeen(f.canon, f.linked)
	f.Seen = seen
	watchedFrom := r.watchStart[x.canon]
	f.Idle = latest(f.Newest, seen, watchedFrom)
	watched := r.watchedFor(f.Idle)
	f.Eligible = r.now.Add(idle - watched)
	if u, ok := r.sample.InUse(f.canon); ok {
		f.Action, f.Why = ActKeep, "in use now: "+useText(u)
		return f, true
	}
	for _, b := range f.linked {
		if u, ok := r.sample.InUse(b); ok {
			f.Action, f.Why = ActKeep, "in use now: "+useText(u)
			return f, true
		}
	}
	if u, ok := r.repoInUse(r.sample, f.canon); ok {
		f.Action, f.Why = ActKeep, "in use now: "+useText(u)
		return f, true
	}
	if j, ok := r.named.Protects(f.canon); ok {
		f.Action, f.Why = ActKeep, "named by "+j
		return f, true
	}
	for _, b := range f.linked { // a named or kept file of a shared-store repository resolves to its blob
		if j, ok := r.named.Protects(b); ok {
			f.Action, f.Why = ActKeep, "named by "+j
			return f, true
		}
		if r.kept(b) {
			f.Action, f.Why = ActKeep, "a file of it is in keep"
			return f, true
		}
	}
	if name := filepath.Base(f.canon); isRepoDir(name) {
		if id := repoIDOf(name); id != "" {
			if j, ok := r.named.RepoIDs[id]; ok {
				f.Action, f.Why = ActKeep, "named by "+j
				return f, true
			}
		}
	}
	if r.unknownStores[x.canon] && len(f.linked) > 0 {
		f.Action, f.Why = ActKeep, "it links into a Hugging Face blob store whose layout this build does not know"
		return f, true
	}
	if watched < idle {
		f.Action = ActPending
		switch {
		case !seen.IsZero() && f.Idle.Equal(seen):
			f.Why = "seen in use " + seen.Format("2006-01-02") + " (" + by + ")"
		case f.Idle.Equal(watchedFrom):
			f.Why = "the sampler has watched " + short(x.shown) + " since " + watchedFrom.Format("2006-01-02")
		}
		if gap := r.now.Sub(f.Idle) - watched; gap > time.Hour {
			f.Why = strings.TrimPrefix(f.Why+"; ", "; ") + "the sampler missed " + gap.Round(time.Hour).String() + " of it"
		}
		return f, true
	}
	f.Action = ActDelete
	return f, true
}

// maxGap is the most a gap between two runs counts as watched: the timer runs every few minutes, so a longer gap is
// the machine asleep or the sampler down, and use in it went unseen.
const maxGap = 30 * time.Minute

// watchedFor is how long the sampler has watched since a time: the runs since then, each gap counting for at most
// maxGap.
func (r *runner) watchedFor(since time.Time) time.Duration {
	rt := r.runTimes
	i := sort.Search(len(rt), func(i int) bool { return !rt[i].Before(since) })
	if i == len(rt) {
		return 0
	}
	d := min(rt[i].Sub(since), maxGap)
	return d + r.covered[len(rt)-1] - r.covered[i]
}

// cover sums the run times' gaps, each capped at maxGap: covered[i] is the watched time from the first run to run i.
func (r *runner) cover() {
	r.covered = make([]time.Duration, len(r.runTimes))
	for i := 1; i < len(r.runTimes); i++ {
		r.covered[i] = r.covered[i-1] + min(r.runTimes[i].Sub(r.runTimes[i-1]), maxGap)
	}
}

// repoIDOf is a repository folder's org/name (models--org--name); a name without an org gives "".
func repoIDOf(dir string) string {
	for _, k := range hfRepoKinds {
		if rest, ok := strings.CutPrefix(dir, k); ok {
			org, name, ok := strings.Cut(rest, "--")
			if ok {
				return org + "/" + name
			}
		}
	}
	return ""
}

// storeLinks are the canonical shared blobs a repository's blobs link to.
func storeLinks(repoDir string, x root) []string {
	dir := filepath.Join(repoDir, "blobs")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	store := filepath.Join(x.shown, "blobs")
	var out []string
	for _, e := range ents {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		l, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if !filepath.IsAbs(l) {
			l = filepath.Join(dir, l)
		}
		l = filepath.Clean(l)
		if under(l, store) && l != store {
			out = append(out, filepath.Join(x.canon, "blobs", strings.TrimPrefix(l, store+"/")))
		}
	}
	return out
}

// shownBlob is a canonical shared blob's path under the root as configured.
func (r *runner) shownBlob(x root, canon string) string {
	return filepath.Join(x.shown, strings.TrimPrefix(canon, x.canon+"/"))
}

// removeModel deletes an item, and then each shared blob it linked whose manifest names no repository blob that
// still links it: the last of its repositories to go takes it. As Hugging Face does, it holds the blob's lock
// (<blob>.lock) while it reads the manifest and unlinks the blob and its manifest, never deletes the lock, and
// keeps a blob on any error but a missing file.
func (r *runner) removeModel(f *Finding) error {
	if err := r.removeItem(f); err != nil {
		return err
	}
	for _, b := range f.linked {
		var x root
		for _, rr := range r.modelRoots {
			if under(b, rr.canon) && r.stores[rr.canon] {
				x = rr
				break
			}
		}
		if x.shown == "" {
			continue
		}
		if err := sweepBlob(r.shownBlob(x, b), x.shown); err != nil {
			return err
		}
	}
	return nil
}

// sweepBlob unlinks a shared blob and its manifest under the blob's lock, when no live link names it.
func sweepBlob(blob, cache string) error {
	lock, err := os.OpenFile(blob+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil // no lock to hold: the blob stays
	}
	defer lock.Close()
	if !waitLock(lock, 10*time.Second) {
		return nil // another holder: it may be linking the blob now
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if stillLinked(blob, cache) {
		return nil
	}
	for _, p := range []string{blob, blob + ".refs"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// stillLinked reports whether a shared blob's manifest (<blob>.refs: one repository blob per line, relative to the
// cache) names a link that still resolves to it. A manifest that cannot be read, a blob that cannot be resolved,
// and any error but a missing file count as linked.
func stillLinked(blob, cache string) bool {
	b, err := os.ReadFile(blob + ".refs")
	if err != nil {
		return true
	}
	want, err := filepath.EvalSymlinks(blob)
	if err != nil {
		return true
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		got, err := filepath.EvalSymlinks(filepath.Join(cache, l))
		switch {
		case err == nil && got == want:
			return true
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return true
		}
	}
	return false
}

// latest is the latest of some times.
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
