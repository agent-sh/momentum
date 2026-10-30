package capacity

import (
	"bufio"
	"context"
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
			if r.modelSet[canon] || r.keep[canon] || (r.stores[x.canon] && e.Name() == "blobs") {
				continue
			}
			shown := filepath.Join(x.shown, e.Name())
			t := Measure(shown, nil)
			f := Finding{Rule: RuleModels, Path: shown, canon: canon, Bytes: t.Bytes, Newest: t.Newest}
			if r.stores[x.canon] && isRepoDir(e.Name()) {
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
			if d, ok := r.decideModel(f, idle); ok {
				out = append(out, d)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// decideModel decides on one item; false leaves it out of the run's list (it has had a write within the idle
// time).
func (r *runner) decideModel(f Finding, idle time.Duration) (Finding, bool) {
	writeIdle := r.now.Sub(f.Newest) >= idle
	if !writeIdle {
		return f, false
	}
	seen, by := r.seen[f.canon].Last, r.seen[f.canon].Proc
	for _, b := range f.linked {
		if s := r.seen[b]; s.Last.After(seen) {
			seen, by = s.Last, s.Proc
		}
	}
	f.Seen = seen
	f.Idle = latest(f.Newest, seen, r.samplerStart)
	f.Eligible = f.Idle.Add(idle)
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
	if name := filepath.Base(f.canon); isRepoDir(name) {
		if id := repoIDOf(name); id != "" {
			if j, ok := r.named.RepoIDs[id]; ok {
				f.Action, f.Why = ActKeep, "named by "+j
				return f, true
			}
		}
	}
	if r.now.Before(f.Eligible) {
		f.Action = ActPending
		switch {
		case !seen.IsZero() && !seen.Before(f.Newest) && !seen.Before(r.samplerStart):
			f.Why = "seen in use " + seen.Format("2006-01-02") + " (" + by + ")"
		case !r.samplerStart.Before(f.Newest):
			f.Why = "the sampler's record starts " + r.samplerStart.Format("2006-01-02")
		}
		return f, true
	}
	f.Action = ActDelete
	return f, true
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
// still links it: the last of its repositories to go takes it. A blob whose manifest cannot be read stays: Hugging
// Face's own rule.
func (r *runner) removeModel(f *Finding) error {
	if err := removeLocked(f.Path); err != nil {
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
		blob := r.shownBlob(x, b)
		if stillLinked(blob, x.shown) {
			continue
		}
		for _, p := range []string{blob, blob + ".refs", blob + ".lock"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// stillLinked reports whether a shared blob's manifest (<blob>.refs: one repository blob per line, relative to the
// cache) names a link that still resolves to it, or cannot be read.
func stillLinked(blob, cache string) bool {
	f, err := os.Open(blob + ".refs")
	if err != nil {
		return true
	}
	defer f.Close()
	want, err := filepath.EvalSymlinks(blob)
	if err != nil {
		return true
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" {
			continue
		}
		got, err := filepath.EvalSymlinks(filepath.Join(cache, l))
		if err == nil && got == want {
			return true
		}
	}
	return sc.Err() != nil
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
