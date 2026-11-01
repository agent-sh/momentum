package capacity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The scratch rule. Agents keep a task's scratch in a folder at the top of the cache directory, and the task that
// made it is to remove it when it closes; many do not. The folders there are tools' caches and tasks' folders. A
// folder is a task's when an agent made it (madeBy). These are never a task's: a folder tagged as a cache
// (CACHEDIR.TAG at its top), one named like an installed command, a root of a rule or one that holds one, a linked
// worktree (another rule's item), a link and an empty folder. An idle task folder (no write and no use for the idle
// time, in the sampler's watched time) is offloaded with evidence-offload, which backs it up to restic and leaves
// NAME.restic.txt in its place. Its model weights, checkpoints and datasets are not offloaded: they are deleted,
// after the models rule's idle time, since the owner ruled they are downloaded or made again. A folder beside its
// pointer is only reported: an offload left it, or it was made again, and another offload would overwrite the
// pointer.

// pointerSuffix ends the name of the file evidence-offload leaves in a folder's place.
const pointerSuffix = ".restic.txt"

// scratchBudget is how long a run starts offloads for: the timer starts a run every 5 minutes and the next run
// waits 5 minutes for this one's lock, so a backlog goes over several runs.
var scratchBudget = 4 * time.Minute

// offloadTimeout is the longest one offload may take, and so the longest a run's sampler waits on one: restic backs
// the folder up before anything is removed, so a timeout leaves the folder whole, or backed up with its pointer.
// restic backs up new data to a local repository at a few hundred MiB/s, so 10 minutes hold a folder of over 100
// GiB; a bigger one fails, and is reported, until it is smaller or kept.
var offloadTimeout = 10 * time.Minute

// modelFile is one file of model data in a task folder, by its path in the folder and its inode when judged.
type modelFile struct {
	rel   string
	ino   inode
	bytes int64
}

// folderWalk is what a walk of a task folder found: its size and newest write (as Measure counts them), its model
// data, the links of its model data and of the rest (what deleting each frees), and what git records that ties it
// to a repository outside it.
type folderWalk struct {
	Tree
	models     []modelFile
	modelBytes int64
	modelLinks links
	ties       []string
	gitDirs    []string // the git directories in it
}

// scratch lists the tasks' folders of the scratch roots and decides on each. found is the shared walk's, for the
// repositories whose remotes or alternates may name a folder.
func (r *runner) scratch(ctx context.Context, found Found) ([]Finding, []string, error) {
	minBytes := int64(r.cfg.MinMiB * (1 << 20))
	refs := repoRefs(found.Repos, r.m)
	var err error
	if r.scratchRepos, err = r.folderMarks(ctx, repoMark); err != nil {
		return nil, nil, err
	}
	if r.scratchSlow, err = r.folderMarks(ctx, slowMark); err != nil {
		return nil, nil, err
	}
	var out []Finding
	tools, unmade, tasks := 0, 0, 0
	for _, x := range r.scratchRoots {
		ents, err := os.ReadDir(x.shown)
		if err != nil {
			return out, nil, err
		}
		for _, e := range ents {
			if !e.IsDir() { // files, pointers and links: evidence-offload resolves a link and would remove its target
				continue
			}
			name := e.Name()
			shown, canon := filepath.Join(x.shown, name), filepath.Join(x.canon, name)
			if r.otherRules(x, canon, shown) {
				continue
			}
			if why := r.toolCache(name, shown); why != "" {
				tools++
				continue
			}
			if inner, err := os.ReadDir(shown); err != nil || len(inner) == 0 {
				continue // an empty folder holds nothing to offload
			}
			born, ok := birth(shown)
			if !ok { // without a birth on record, no agent can be shown to have made it
				unmade++
				continue
			}
			makers, err := r.madeBy(ctx, r.spellings(x, name), born)
			if err != nil {
				return out, nil, err
			}
			if len(makers) == 0 {
				unmade++
				continue
			}
			tasks++
			if err := r.makerRepos(ctx, canon, born, makers); err != nil {
				return out, nil, err
			}
			w := walkFolder(shown, r.m, minBytes)
			f := Finding{Rule: RuleScratch, Path: shown, canon: canon, root: x.shown, Bytes: w.Bytes, Newest: w.Newest,
				files: w.models, modelBytes: w.modelBytes, makers: makers, links: w.links, modelLinks: w.modelLinks}
			f.ino, _ = inodeOf(shown)
			if pst, err := os.Lstat(filepath.Join(x.shown, name+pointerSuffix)); err == nil {
				f.Action, f.class = ActReport, "pointer"
				f.Why = "beside the pointer of its offload: it was made again, and another offload would overwrite the pointer"
				if born.Before(pst.ModTime()) {
					f.Why = "beside the pointer of its offload, which left it: what is still in it was not removed"
				}
				out = append(out, f)
				continue
			}
			out = append(out, r.decideScratch(ctx, f, x, w, refs))
		}
	}
	if err := r.forgetGone(ctx); err != nil {
		return out, nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, []string{fmt.Sprintf("%d tasks' folders, %d tools' caches, %d folders no agent is on record making", tasks, tools,
		unmade)}, nil
}

// decideScratch decides on one task folder of root x.
func (r *runner) decideScratch(ctx context.Context, f Finding, x root, w folderWalk, refs map[string]string) Finding {
	window := time.Duration(r.cfg.Scratch.IdleDays * 24 * float64(time.Hour))
	if len(f.files) > 0 { // model data waits for the models rule's idle time, and the folder's offload with it
		window = max(window, time.Duration(r.cfg.Models.IdleDays*24*float64(time.Hour)))
	}
	seen, by := r.lastSeen(f.canon, nil)
	f.Seen = seen
	watchedFrom := r.watchStart[x.canon]
	f.Idle = latest(f.Newest, seen, watchedFrom)
	watched := r.watchedFor(f.Idle)
	f.Eligible = r.now.Add(window - watched)
	f.Detail = scratchDetail(f)
	keep := func(why string) Finding { f.Action, f.Why = ActKeep, why; return f }
	if r.keptScratch(f.canon) {
		return keep("in keep")
	}
	for _, mf := range f.files {
		if c := filepath.Join(f.canon, mf.rel); r.kept(c) {
			return keep("a model file of it is in the models rule's keep: " + short(filepath.Join(f.Path, mf.rel)))
		}
	}
	if u, ok := r.sample.InUse(f.canon); ok {
		return keep("in use now: " + useText(u))
	}
	if j, ok := r.named.Protects(f.canon); ok {
		return keep("named by " + j)
	}
	if why := referenced(f.canon, w.ties, refs); why != "" {
		return keep(why)
	}
	if watched < window {
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
		return f
	}
	if w.Errs > 0 {
		f.Action, f.class = ActReport, "unreadable"
		f.Why = fmt.Sprintf("%d of its directories cannot be read, so its last write is not known", w.Errs)
		return f
	}
	class, why := "", ""
	if ex, ok := r.excludedScratch(ctx, f.canon, w, f.makers); ok {
		class, why = "excluded", "an excluded project's: "+ex
	} else if d := r.unknownMaker(f.canon); d != "" {
		class, why = "unknown", "made in "+short(d)+", which was gone when the rule first looked, so its project cannot be "+
			"known: offload it by hand (evidence-offload) if it is no excluded project's"
	}
	if class != "" {
		if len(f.files) == 0 {
			f.Action, f.class, f.Why = ActReport, class, why
			return f
		}
		// the owner ruled on models directly: an excluded project's model data goes too, and the rest stays
		f.modelOnly, f.class = true, class
		f.Detail = fmt.Sprintf("delete %d model files (%s); %s, so the rest is only reported", len(f.files),
			gib(f.modelBytes), why)
	}
	if d := stuckDir(f.Path, f.files); d != "" { // its model data cannot go, so neither can the rest: restic would keep it
		f.Action, f.class = ActReport, "stuck"
		f.Why = "its model data cannot be removed by this user: " + short(d) + " is not writable"
		return f
	}
	for _, v := range r.scratchSlow[f.canon] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && !f.modelOnly && f.Bytes-f.modelBytes >= n {
			f.Action, f.class = ActReport, "too-big"
			f.Why = fmt.Sprintf("its offload of %s ran out of its %s, so it waits until it is smaller: offload it by hand, "+
				"or list it in keep", gib(n), durationText(offloadTimeout))
			return f
		}
	}
	// a session only keeps what would go: a look at them that is not whole holds the run
	if why := r.sessionIn(ctx, f.canon, f.makers); why != "" {
		return keep(why)
	}
	f.Action = ActDelete
	return f
}

// The marks the scratch rule keeps per task folder, "<prefix><folder>|<value>". Its repository marks: "b <birth>",
// the folder they were taken for (one made again under its name has another birth, and starts again); "s
// <harness>/<session>", a maker looked up; "r <remote>" and "c <repository's place>", the repositories the makers
// worked in; "u <directory>", a maker whose working directory was gone when it was looked up. Its slow marks: the
// sizes whose offload ran out of its time.
const (
	repoMark = "scratch-repo:"
	slowMark = "scratch-slow:"
)

// folderMarks are a prefix's marks, by task folder.
func (r *runner) folderMarks(ctx context.Context, prefix string) (map[string][]string, error) {
	out := map[string][]string{}
	if r.st == nil {
		return out, nil
	}
	ms, err := r.st.Marks(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for k := range ms {
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.LastIndex(rest, "|"); i >= 0 {
			out[rest[:i]] = append(out[rest[:i]], rest[i+1:])
		}
	}
	return out, nil
}

// makerRepos looks up the repositories a task folder's makers work in, each maker once, when the rule first sees it,
// and puts them on record: a lane removes its checkout when it closes, and an excluded project's folder stays its
// after that. A folder made again under its name starts again. A dry run keeps what it finds in memory.
func (r *runner) makerRepos(ctx context.Context, canon string, born time.Time, makers []maker) error {
	vals := r.scratchRepos[canon]
	put := func(v string) error {
		vals = append(vals, v)
		if r.opt.DryRun || r.st == nil {
			return nil
		}
		return r.st.SetMark(ctx, repoMark+canon+"|"+v, r.now)
	}
	if b := "b " + strconv.FormatInt(born.UnixNano(), 10); !slices.Contains(vals, b) {
		if err := r.forget(ctx, canon); err != nil {
			return err
		}
		vals = nil
		if err := put(b); err != nil {
			return err
		}
	}
	for _, mk := range makers {
		s := "s " + mk.harness + "/" + mk.session
		if slices.Contains(vals, s) {
			continue
		}
		add := []string{s}
		if mk.cwd != "" && filepath.IsAbs(mk.cwd) {
			cwd := r.m.Canon(mk.cwd)
			if _, err := os.Lstat(cwd); errors.Is(err, fs.ErrNotExist) {
				add = append(add, "u "+cwd)
			} else if rp := r.git.repoOf(ctx, cwd); rp != nil {
				for _, u := range rp.remotes {
					add = append(add, "r "+u)
				}
				if rp.common != "" {
					add = append(add, "c "+r.m.Canon(repoPlace(rp.common)))
				}
			}
		}
		for _, v := range add {
			if !slices.Contains(vals, v) {
				if err := put(v); err != nil {
					return err
				}
			}
		}
	}
	r.scratchRepos[canon] = vals
	return nil
}

// forget clears a task folder's marks.
func (r *runner) forget(ctx context.Context, folder string) error {
	for prefix, marks := range map[string]map[string][]string{repoMark: r.scratchRepos, slowMark: r.scratchSlow} {
		if !r.opt.DryRun && r.st != nil {
			for _, v := range marks[folder] {
				if err := r.st.ClearMark(ctx, prefix+folder+"|"+v); err != nil {
					return err
				}
			}
		}
		delete(marks, folder)
	}
	return nil
}

// forgetGone clears the marks of task folders that are gone.
func (r *runner) forgetGone(ctx context.Context) error {
	gone := map[string]bool{}
	for _, marks := range []map[string][]string{r.scratchRepos, r.scratchSlow} {
		for folder := range marks {
			if _, err := os.Lstat(folder); errors.Is(err, fs.ErrNotExist) {
				gone[folder] = true
			}
		}
	}
	for folder := range gone {
		if err := r.forget(ctx, folder); err != nil {
			return err
		}
	}
	return nil
}

// stuckDir is the first directory of a task folder's model data the run cannot unlink a file in, or "". The walk
// found the files, so the run can search the directories; it must be able to write them too.
func stuckDir(p string, files []modelFile) string {
	seen := map[string]bool{}
	for _, mf := range files {
		d := filepath.Join(p, filepath.Dir(mf.rel))
		if !seen[d] {
			seen[d] = true
			if unix.Access(d, unix.W_OK) != nil {
				return d
			}
		}
	}
	return ""
}

// durationText is a duration in words: minutes, or seconds and less as Go prints them.
func durationText(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	}
	return d.String()
}

// scratchDetail says what an act on a task folder does.
func scratchDetail(f Finding) string {
	d := fmt.Sprintf("offload %s to restic", gib(f.Bytes-f.modelBytes))
	if len(f.files) > 0 {
		d = fmt.Sprintf("delete %d model files (%s), then %s", len(f.files), gib(f.modelBytes), d)
	}
	var by []string
	for _, mk := range f.makers[:min(len(f.makers), 3)] {
		by = append(by, fmt.Sprintf("%s session %s on %s", mk.harness, mk.session, mk.at.Format("2006-01-02")))
	}
	if len(f.makers) > 3 {
		by = append(by, fmt.Sprintf("%d more", len(f.makers)-3))
	}
	if len(by) > 0 {
		d += "; made by " + strings.Join(by, ", ")
	}
	return d
}

// otherRules reports whether a folder at the top of scratch root x is another rule's: a configured root, or one
// that holds one; under a models root; a linked worktree. (A Cargo target directory is tagged as a cache.)
func (r *runner) otherRules(x root, canon, shown string) bool {
	for _, rs := range [][]root{r.modelRoots, r.targetRoot, r.wtRoots, r.scratchRoots} {
		for _, o := range rs {
			if o.canon != x.canon && under(o.canon, canon) {
				return true
			}
		}
	}
	if r.inModelRoots(canon) {
		return true
	}
	_, linked := isLinkedWorktree(shown)
	return linked
}

// toolCache says why a folder at the top of a scratch root is a tool's cache and not a task's, or "".
func (r *runner) toolCache(name, shown string) string {
	if b := readHead(filepath.Join(shown, "CACHEDIR.TAG"), len(cargoTag)); string(b) == cargoTag {
		return "it is tagged as a cache (CACHEDIR.TAG)"
	}
	if r.command(name) {
		return "it is named like an installed command"
	}
	return ""
}

// command reports whether a name is an installed command's: in a links directory (names.links), or on PATH.
func (r *runner) command(name string) bool {
	for _, d := range r.cfg.Names.Links {
		if exists(filepath.Join(d, name)) {
			return true
		}
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// birth is a path's birth time (statx), and false where its filesystem keeps none.
func birth(p string) (time.Time, bool) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, p, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &st); err != nil ||
		st.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec)), true
}

// spellings are the ways a command or its output names a folder at the top of root x: through the root as
// configured, through its canonical path, and through the home directory as ~, $HOME and ${HOME}.
func (r *runner) spellings(x root, name string) []string {
	out := []string{filepath.Join(x.shown, name)}
	if x.canon != x.shown {
		out = append(out, filepath.Join(x.canon, name))
	}
	if h := homeDir(); h != "/" && under(x.shown, h) {
		rel := strings.TrimPrefix(filepath.Join(x.shown, name), h)
		out = append(out, "~"+rel, "$HOME"+rel, "${HOME}"+rel)
	}
	return out
}

// keptScratch reports whether a folder is in scratch.keep, holds a path in it, or is inside one.
func (r *runner) keptScratch(canon string) bool {
	for k := range r.scratchKeep {
		if under(k, canon) || under(canon, k) {
			return true
		}
	}
	return false
}

// sessionIn says which live agent session works in a folder (its working directory is in it), or made it and is
// still open, or "". The scheduled-AI policy leaves a task's folder to the task while it lasts. A session is the
// one that made it when its harness's id for it is the id the making call's transcript carries.
func (r *runner) sessionIn(ctx context.Context, canon string, makers []maker) string {
	ss, err := r.liveSessions(ctx)
	if err != nil {
		return r.sessionsUnread(err)
	}
	for _, s := range ss {
		name := firstNonEmpty(s.Name, s.Key)
		if s.Cwd != "" && under(r.m.Canon(s.Cwd), canon) {
			return fmt.Sprintf("session %s (%s) works in it", name, s.Tool)
		}
		for _, mk := range makers {
			if mk.session != "" && (s.ID == mk.session || s.Key == mk.session) {
				return fmt.Sprintf("the session that made it, %s (%s), is still open", name, s.Tool)
			}
		}
	}
	return ""
}

// excludedScratch reports whether a task folder is an excluded project's: by its place, by the remote of a
// repository in it, or by where a session that made it worked then (an excluded project's lane keeps its scratch
// in the cache too), by that place, or by the repository there as the rule first saw it or as it is now.
func (r *runner) excludedScratch(ctx context.Context, canon string, w folderWalk, makers []maker) (string, bool) {
	if why, ok := r.excluded(canon); ok {
		return why, true
	}
	for _, g := range w.gitDirs {
		if why, ok := r.ex.byRemote(&repo{remotes: gitURLs(g)}); ok {
			return "it holds " + why, true
		}
	}
	for _, v := range r.scratchRepos[canon] {
		kind, val, _ := strings.Cut(v, " ")
		switch {
		case kind == "r":
			if why, ok := r.ex.byRemote(&repo{remotes: []string{val}}); ok {
				return "made in " + why, true
			}
		case kind == "c":
			if why, ok := r.excluded(val); ok {
				return "made in a worktree of a repository " + why, true
			}
		}
	}
	for _, mk := range makers {
		if mk.cwd == "" || !filepath.IsAbs(mk.cwd) {
			continue
		}
		cwd := r.m.Canon(mk.cwd)
		why, ok := r.excluded(cwd)
		if !ok {
			if rp := r.git.repoOf(ctx, cwd); rp != nil {
				why, ok = r.excludedRepo(rp)
			}
		}
		if ok {
			return "made in " + short(cwd) + ", " + why, true
		}
	}
	return "", false
}

// unknownMaker names a maker's working directory that was gone when the rule first looked it up, or "": the project
// of a folder made there cannot be known, and it may be an excluded one's.
func (r *runner) unknownMaker(canon string) string {
	for _, v := range r.scratchRepos[canon] {
		if kind, val, _ := strings.Cut(v, " "); kind == "u" {
			return val
		}
	}
	return ""
}

// referenced says what ties a folder to a repository outside it (from its own git links, or a repository's remote,
// alternates or worktree that names a path in it), or "".
func referenced(canon string, links []string, refs map[string]string) string {
	if len(links) > 0 {
		return "a repository references it: " + strings.Join(links[:min(len(links), 2)], "; ")
	}
	keys := make([]string, 0, len(refs))
	for p := range refs {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if repo := refs[p]; under(p, canon) && !under(repo, canon) {
			return "a repository references it: " + short(repo) + " names " + short(p)
		}
	}
	return ""
}

// repoRefs are the paths the repositories the shared walk found name outside themselves (a remote that is a path,
// their alternates, their worktrees), each with its repository's place, both canonical.
func repoRefs(gitDirs []string, m *Mounts) map[string]string {
	out := map[string]string{}
	for _, g := range gitDirs {
		place := m.Canon(repoPlace(g))
		for _, p := range gitRefs(g) {
			if c := m.Canon(p); !under(c, place) {
				out[c] = place
			}
		}
	}
	return out
}

var urlRe = regexp.MustCompile(`^\s*url\s*=\s*(.+?)\s*$`)

// gitURLs are the remote URLs in a git directory's config.
func gitURLs(g string) []string {
	b, err := os.ReadFile(filepath.Join(g, "config"))
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if m := urlRe.FindStringSubmatch(sc.Text()); m != nil {
			out = append(out, strings.Trim(m[1], `"`))
		}
	}
	return out
}

// gitRefs are the paths a git directory names: the remotes that are paths (file://, or absolute), its alternates,
// and its linked worktrees.
func gitRefs(g string) []string {
	var out []string
	for _, u := range gitURLs(g) {
		if u = strings.TrimPrefix(u, "file://"); filepath.IsAbs(u) {
			out = append(out, filepath.Clean(u))
		}
	}
	if b, err := os.ReadFile(filepath.Join(g, "objects", "info", "alternates")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); filepath.IsAbs(l) {
				out = append(out, filepath.Clean(l))
			}
		}
	}
	for _, wt := range worktreeFiles(g) {
		out = append(out, filepath.Dir(wt))
	}
	return out
}

// worktreeFiles are the .git files of a git directory's linked worktrees that still exist, from worktrees/*/gitdir.
func worktreeFiles(g string) []string {
	ents, err := os.ReadDir(filepath.Join(g, "worktrees"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(g, "worktrees", e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		p := strings.TrimSpace(string(b))
		if !filepath.IsAbs(p) {
			p = filepath.Join(g, "worktrees", e.Name(), p)
		}
		if exists(p) {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

// walkFolder walks a task folder without following symlinks or leaving its filesystem: its size and newest write,
// as Measure counts them; its files of model data of at least minBytes, but in a package tree (an environment's
// packages, node_modules, git's store, a Cargo target, a tool's tagged cache), which are no model a task kept; and
// what git records that ties it to a repository outside it (a worktree in it of a repository outside, or a worktree
// outside of a repository in it), each path taken by its canonical name: git writes the one it was given.
func walkFolder(p string, m *Mounts, minBytes int64) folderWalk {
	var w folderWalk
	cp := m.Canon(p)
	st, err := os.Lstat(p)
	if err != nil {
		w.Errs++
		return w
	}
	rootStat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return w
	}
	seen := map[inode]bool{}
	// count counts an entry, each inode's bytes once, and its link in the links of the model data or of the rest
	count := func(fi os.FileInfo, l *links) int64 {
		s, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return 0
		}
		w.Files++
		if t := newest(s); t.After(w.Newest) {
			w.Newest = t
		}
		k := inode{uint64(s.Dev), s.Ino}
		first := !seen[k]
		l.add(s, first)
		if !first {
			return 0
		}
		seen[k] = true
		w.Bytes += s.Blocks * 512
		return s.Blocks * 512
	}
	count(st, &w.links)
	type dir struct {
		path    string
		noModel bool
	}
	stack := []dir{{p, false}}
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ents, err := os.ReadDir(d.path)
		if err != nil {
			w.Errs++
			continue
		}
		if isGitDir(d.path, ents) {
			w.gitDirs = append(w.gitDirs, d.path)
			for _, wt := range worktreeFiles(d.path) {
				if !under(m.Canon(wt), cp) {
					w.ties = append(w.ties, "a worktree at "+short(filepath.Dir(wt))+" belongs to the repository at "+short(repoPlace(d.path)))
				}
			}
		}
		for _, e := range ents {
			fi, err := e.Info()
			if err != nil { // a directory it may list but not search hides what is in it; an idle folder loses nothing
				w.Errs++
				continue
			}
			ep := filepath.Join(d.path, e.Name())
			gitFile := e.Name() == ".git" && fi.Mode().IsRegular()
			model := !gitFile && fi.Mode().IsRegular() && !d.noModel && fi.Size() >= minBytes && modelFormat(ep, e.Name(), fi.Size())
			l := &w.links
			if model {
				l = &w.modelLinks
			}
			b := count(fi, l)
			switch {
			case e.IsDir():
				if s, ok := fi.Sys().(*syscall.Stat_t); ok && s.Dev == rootStat.Dev {
					stack = append(stack, dir{ep, d.noModel || packageTree(ep, e.Name())})
				}
			case gitFile:
				// a linked worktree, or a checkout whose git directory is kept apart: the repository names it
				if g, _ := isLinkedWorktree(d.path); g != "" && !under(m.Canon(g), cp) && exists(g) {
					w.ties = append(w.ties, "it holds a checkout of the repository at "+short(repoPlace(gitCommon(g))))
				}
			case model:
				ino, _ := inodeOf(ep)
				rel, _ := filepath.Rel(p, ep)
				w.models = append(w.models, modelFile{rel: rel, ino: ino, bytes: b})
				w.modelBytes += b
			}
		}
	}
	w.links.settle()
	w.modelLinks.settle()
	return w
}

// isGitDir reports whether a directory is a git directory (a .git directory or a bare repository): HEAD, objects
// and refs.
func isGitDir(d string, ents []os.DirEntry) bool {
	var head, objects, refs bool
	for _, e := range ents {
		switch e.Name() {
		case "HEAD":
			head = e.Type().IsRegular()
		case "objects":
			objects = e.IsDir()
		case "refs":
			refs = e.IsDir()
		}
	}
	return head && objects && refs
}

// gitCommon is a linked worktree's common git directory, from its administrative directory's commondir.
func gitCommon(admin string) string {
	b, err := os.ReadFile(filepath.Join(admin, "commondir"))
	if err != nil {
		return admin
	}
	c := strings.TrimSpace(string(b))
	if !filepath.IsAbs(c) {
		c = filepath.Join(admin, c)
	}
	return filepath.Clean(c)
}

// packageTree reports whether a directory holds what a tool installed or builds, whose files are no model a task
// kept: an environment's packages, node_modules, git's store, a Python environment, or a tool's cache tagged as one
// (a Cargo target is).
func packageTree(d, name string) bool {
	switch name {
	case "site-packages", "dist-packages", "node_modules", ".git":
		return true
	}
	if exists(filepath.Join(d, "pyvenv.cfg")) {
		return true
	}
	b := readHead(filepath.Join(d, "CACHEDIR.TAG"), len(cargoTag))
	return string(b) == cargoTag && !hfHub(d)
}

// hfHub reports whether a tagged directory is a Hugging Face hub cache, whose repositories are model data.
func hfHub(d string) bool {
	ents, err := os.ReadDir(d)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if isRepoDir(e.Name()) {
			return true
		}
	}
	return false
}

// modelExts are the names of model weights', checkpoints' and datasets' files: TensorRT's engines, TFRecord and
// MosaicML shards among them. tfShard is a TensorFlow checkpoint's shard (model.ckpt.data-00000-of-00001).
var (
	modelExts = map[string]bool{".safetensors": true, ".gguf": true, ".ggml": true, ".pt": true, ".pth": true,
		".ckpt": true, ".distcp": true, ".onnx": true, ".tflite": true, ".parquet": true, ".arrow": true, ".engine": true,
		".trt": true}
	tfShard = regexp.MustCompile(`\.data-\d{5}-of-\d{5}$`)
	// shardExt is a dataset's shard: TFRecord (as TensorFlow Datasets names them, NAME.tfrecord-00000-of-00001)
	// and MosaicML's, compressed or not
	shardExt = regexp.MustCompile(`(?i)\.(?:tfrecords?(?:-\d{5}-of-\d{5})?|mds)(?:\.(?:gz|zst|zstd|bz2|xz|lz4|snappy))?$`)
)

// modelFormat reports whether a file holds model data: by its name, or by its first bytes (GGUF and legacy GGML,
// safetensors, a PyTorch archive, Parquet, Arrow, a Megatron dataset), which is how a blob named by its hash
// (Ollama's, Hugging Face's) is known. What
// a run's results are kept in as often (JSON lines, CSV, NumPy and HDF5 arrays, pickles) is left to the offload:
// restic keeps it.
func modelFormat(p, name string, size int64) bool {
	if modelExts[strings.ToLower(filepath.Ext(name))] || tfShard.MatchString(name) || shardExt.MatchString(name) {
		return true
	}
	if strings.HasSuffix(name, ".bin") { // a Megatron dataset's data, beside its index
		if bytes.HasPrefix(readHead(strings.TrimSuffix(p, ".bin")+".idx", 9), []byte("MMIDIDX\x00\x00")) {
			return true
		}
	}
	b := readHead(p, 256)
	// GGUF, legacy GGML (ggml, ggmf, ggjt, little-endian), Parquet, Arrow, a Megatron dataset's index
	for _, magic := range []string{"GGUF", "lmgg", "fmgg", "tjgg", "PAR1", "ARROW1", "MMIDIDX\x00\x00"} {
		if bytes.HasPrefix(b, []byte(magic)) {
			return true
		}
	}
	// safetensors: a little-endian length of the JSON header that follows
	if len(b) >= 9 && b[8] == '{' {
		if n := binary.LittleEndian.Uint64(b[:8]); n > 1 && n < uint64(size) {
			return true
		}
	}
	// a PyTorch archive is a zip whose first entry is its data.pkl
	if len(b) >= 30 && bytes.HasPrefix(b, []byte("PK\x03\x04")) {
		if n := int(binary.LittleEndian.Uint16(b[26:28])); 30+n <= len(b) {
			entry := string(b[30 : 30+n])
			return entry == "data.pkl" || strings.HasSuffix(entry, "/data.pkl")
		}
	}
	return false
}

// removeScratch acts on a task folder: under an exclusive BSD lock on it, the directory opened checked to be the one
// judged, it deletes its model data, each file through an os.Root of the folder and only while it is the inode
// judged, and has recorded run once they are gone (a deletion is on record before the offload starts). Then it
// offloads the rest with evidence-offload, unless only the model data goes, with offloading run just before: what the
// folder holds then is what the offload removes. An offload that failed but left the pointer and no folder went:
// restic keeps the folder.
func (r *runner) removeScratch(ctx context.Context, f *Finding, recorded func() error, offloading func()) error {
	if !f.modelOnly { // without the offload, nothing of the folder goes: its model data waits for it
		if err := haveTool(r.cfg.Scratch.Offload, "evidence-offload"); err != nil {
			return err
		}
	}
	dir, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if st, err := dir.Stat(); err != nil || !st.IsDir() {
		return errChanged
	} else if s, ok := st.Sys().(*syscall.Stat_t); !ok || (inode{uint64(s.Dev), s.Ino}) != f.ino {
		return errChanged // another directory in its place since the judgment
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errLocked
	}
	defer syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
	if len(f.files) > 0 {
		rt, err := os.OpenRoot(f.Path)
		if err != nil {
			return err
		}
		defer rt.Close()
		if r.opt.beforeModelDelete != nil {
			r.opt.beforeModelDelete(*f)
		}
		// every file of its model data is the one judged, or none goes: one put in a file's place since would go to
		// restic with the rest
		for _, mf := range f.files {
			fi, err := rt.Lstat(mf.rel)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if s, ok := fi.Sys().(*syscall.Stat_t); !ok || (inode{uint64(s.Dev), s.Ino}) != mf.ino {
				return errChanged
			}
		}
		var failed error
		for _, mf := range f.files {
			if err := rt.Remove(mf.rel); err == nil {
				f.removed += mf.bytes
			} else if !errors.Is(err, fs.ErrNotExist) {
				failed = err
				break
			}
		}
		// what went is on record before anything else happens: a model-only act's success the act records itself
		if f.removed > 0 && (failed != nil || !f.modelOnly) && recorded != nil {
			if err := recorded(); err != nil {
				return err
			}
		}
		if failed != nil {
			return failed
		}
	}
	if f.modelOnly {
		return nil
	}
	if offloading != nil {
		offloading()
	}
	out, err := runTool(ctx, offloadTimeout, r.cfg.Scratch.Offload, "evidence-offload", "EVIDENCE_TAG="+r.cfg.Scratch.Tag, f.Path)
	if m := snapshotRe.FindStringSubmatch(out); m != nil {
		f.snapshot = m[1]
	}
	pointer := f.Path + pointerSuffix
	if !exists(f.Path) && exists(pointer) {
		if f.snapshot == "" { // read it from the pointer, which evidence-offload writes before it removes the folder
			if b, rerr := os.ReadFile(pointer); rerr == nil {
				if m := snapshotRe.FindStringSubmatch(strings.Join(strings.Fields(string(b)), " ")); m != nil {
					f.snapshot = m[1]
				}
			}
		}
		f.offloaded = true
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("evidence-offload ran but did not leave %s in the folder's place: %s", short(pointer),
		strings.TrimSpace(lastLine(out)))
}

// errChanged means the item at a path is not the one judged.
var errChanged = errors.New("it changed since it was judged")

// snapshotRe reads the snapshot evidence-offload says it made.
var snapshotRe = regexp.MustCompile(`snapshot ([0-9a-f]{8,64})`)

// haveTool reports whether a tool can be run: its configured path is an executable file, or, unconfigured, its name
// is on PATH.
func haveTool(path, name string) error {
	if path == "" {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s: %w", name, errNoTool)
		}
		return nil
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s (%s): %w", name, path, errNoTool)
	}
	return nil
}

// scratchKey is the task folder a canonical path under the scratch roots is in, or false: a root itself is none.
func (r *runner) scratchKey(p string) (string, bool) {
	for _, x := range r.scratchRoots {
		if !under(p, x.canon) || p == x.canon {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(p, x.canon+"/"), "/")
		return filepath.Join(x.canon, first), true
	}
	return "", false
}
