package capacity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/agent-sh/momentum/internal/store"
)

// The scratch rule. Agents keep a task's scratch in a folder at the top of the cache directory, and the task that
// made it is to remove it when it closes; many do not. The folders there are tools' caches and tasks' folders. A
// folder is a task's when an agent made it: the store's transcripts hold an agent's tool call, or its output, that
// names the folder from a minute before its birth to ten minutes after. These are never a task's: a folder tagged
// as a cache (CACHEDIR.TAG at its top), one named like an installed command, one that came back after an offload
// (its pointer beside it), a root of a rule or one that holds one, a Cargo target directory and a linked worktree
// (other rules' items). An idle task folder (no write and no use for the idle time, in the sampler's watched time)
// is offloaded with evidence-offload, which backs it up to restic and leaves NAME.restic.txt in its place. Its
// model weights, checkpoints and datasets are not offloaded: they are deleted, after the models rule's idle time,
// since the owner ruled they are downloaded or made again.

// pointerSuffix ends the name of the file evidence-offload leaves in a folder's place.
const pointerSuffix = ".restic.txt"

// madeBefore and madeAfter bound when an agent's tool call, or its output, names a folder it made: from a minute
// before the folder's birth (a call is recorded as it is issued, before its command runs) to ten minutes after (an
// agent names what a command it ran made, or the path mktemp printed). A tool that makes its cache again after an
// agent looked at it or deleted it does so outside the window.
const (
	madeBefore = time.Minute
	madeAfter  = 10 * time.Minute
)

// scratchBudget is how long a run starts offloads for: the timer starts a run every 5 minutes and the next run
// waits 5 minutes for this one's lock, so a backlog goes over several runs and never stops the sampler.
var scratchBudget = 4 * time.Minute

// offloadTimeout is the longest one offload may take: restic backs the folder up before anything is removed, so a
// timeout leaves the folder whole, or backed up with its pointer written.
const offloadTimeout = 30 * time.Minute

// The kinds of a transcript event that carry an agent's command and its output.
const (
	toolCall   = "tool_call"
	toolResult = "tool_result"
)

// maker is the agent session a task folder's birth is on record with, and where it worked then.
type maker struct {
	session, harness, cwd string
	at                    time.Time
}

// modelFile is one file of model data in a task folder, by its path in the folder and its inode when judged.
type modelFile struct {
	rel   string
	ino   inode
	bytes int64
}

// folderWalk is what a walk of a task folder found: its size and newest write (as Measure counts them), its model
// data, and what git records that ties it to a repository outside it.
type folderWalk struct {
	Tree
	models     []modelFile
	modelBytes int64
	links      []string
	gitDirs    []string // the git directories in it
}

// scratch lists the tasks' folders of the scratch roots and decides on each. found is the shared walk's, for the
// repositories whose remotes or alternates may name a folder.
func (r *runner) scratch(ctx context.Context, found Found) ([]Finding, []string, error) {
	minBytes := int64(r.cfg.MinMiB * (1 << 20))
	refs := repoRefs(found.Repos, r.m)
	var out []Finding
	tools, unmade, tasks := 0, 0, 0
	for _, x := range r.scratchRoots {
		ents, err := os.ReadDir(x.shown)
		if err != nil {
			return out, nil, err
		}
		for _, e := range ents {
			if !e.IsDir() { // files, links and pointers are not folders
				continue
			}
			name := e.Name()
			shown, canon := filepath.Join(x.shown, name), filepath.Join(x.canon, name)
			if r.otherRules(x, canon, shown) {
				continue
			}
			if why := r.toolCache(x, name, shown); why != "" {
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
			mk, made, err := r.madeBy(ctx, r.spellings(x, name), canon, born)
			if err != nil {
				return out, nil, err
			}
			if !made {
				unmade++
				continue
			}
			tasks++
			w := walkFolder(shown, minBytes)
			f := Finding{Rule: RuleScratch, Path: shown, canon: canon, root: x.shown, Bytes: w.Bytes, Newest: w.Newest,
				files: w.models, modelBytes: w.modelBytes, maker: mk}
			f.ino, _ = inodeOf(shown)
			out = append(out, r.decideScratch(ctx, f, x, w, refs))
		}
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
	if u, ok := r.sample.InUse(f.canon); ok {
		return keep("in use now: " + useText(u))
	}
	if j, ok := r.named.Protects(f.canon); ok {
		return keep("named by " + j)
	}
	if why := r.sessionIn(ctx, f.canon, f.maker); why != "" {
		return keep(why)
	}
	if why := referenced(f.canon, w.links, refs); why != "" {
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
	if why, ok := r.excludedScratch(ctx, f.canon, w, f.maker); ok {
		if len(f.files) == 0 {
			f.Action, f.class, f.Why = ActReport, "excluded", "an excluded project's: "+why
			return f
		}
		// the owner ruled on models directly: an excluded project's model data goes too, and the rest stays
		f.modelOnly = true
		f.Detail = fmt.Sprintf("delete %d model files (%s); an excluded project's (%s), so the rest is only reported",
			len(f.files), gib(f.modelBytes), why)
	}
	f.Action = ActDelete
	return f
}

// scratchDetail says what an act on a task folder does.
func scratchDetail(f Finding) string {
	d := fmt.Sprintf("offload %s to restic", gib(f.Bytes-f.modelBytes))
	if len(f.files) > 0 {
		d = fmt.Sprintf("delete %d model files (%s), then %s", len(f.files), gib(f.modelBytes), d)
	}
	if f.maker.session != "" {
		d += fmt.Sprintf("; made by %s session %s on %s", f.maker.harness, f.maker.session, f.maker.at.Format("2006-01-02"))
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
func (r *runner) toolCache(x root, name, shown string) string {
	if b := readHead(filepath.Join(shown, "CACHEDIR.TAG"), len(cargoTag)); string(b) == cargoTag {
		return "it is tagged as a cache (CACHEDIR.TAG)"
	}
	if r.command(name) {
		return "it is named like an installed command"
	}
	if exists(filepath.Join(x.shown, name+pointerSuffix)) {
		return "it came back after an offload: " + name + pointerSuffix + " is beside it"
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
		st.Mask&unix.STATX_BTIME == 0 || st.Btime.Sec == 0 {
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

// madeBy is the agent session that made a folder: the earliest of the tool calls, and their outputs, that name it
// (or work in it) from madeBefore before its birth to madeAfter after. Without a store none is on record.
func (r *runner) madeBy(ctx context.Context, spellings []string, canon string, born time.Time) (maker, bool, error) {
	if r.st == nil {
		return maker{}, false, nil
	}
	evs, err := r.st.Events(ctx, store.Query{Source: store.Transcripts, Kinds: []string{toolCall, toolResult},
		Since: born.Add(-madeBefore), Until: born.Add(madeAfter), Limit: math.MaxInt32})
	if err != nil {
		return maker{}, false, err
	}
	var mk maker
	found := false
	for _, e := range evs {
		if !namesPath(e.Summary, spellings) && (e.Cwd == "" || !under(r.m.Canon(e.Cwd), canon)) {
			continue
		}
		if !found || e.TS.Before(mk.at) {
			mk, found = maker{session: e.Session, harness: e.Harness, cwd: e.Cwd, at: e.TS}, true
		}
	}
	return mk, found, nil
}

// namesPath reports whether a text names a path in one of its spellings: the spelling, then what cannot go on a
// name (a slash, a space, a quote, the end).
func namesPath(text string, spellings []string) bool {
	for _, s := range spellings {
		for i := 0; i < len(text); {
			j := strings.Index(text[i:], s)
			if j < 0 {
				break
			}
			end := i + j + len(s)
			if end == len(text) || !nameByte(text[end]) {
				return true
			}
			i += j + 1
		}
	}
	return false
}

// nameByte reports whether a byte may go on a file name as agents write them.
func nameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.' || b == '-' ||
		b == '@' || b == '+'
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
// still open, or "". The scheduled-AI policy leaves a task's folder to the task while it lasts.
func (r *runner) sessionIn(ctx context.Context, canon string, mk maker) string {
	for _, s := range r.liveSessions(ctx) {
		name := firstNonEmpty(s.Name, s.Key)
		switch {
		case s.Cwd != "" && under(r.m.Canon(s.Cwd), canon):
			return fmt.Sprintf("session %s (%s) works in it", name, s.Tool)
		case mk.session != "" && s.Key == mk.session:
			return fmt.Sprintf("the session that made it, %s (%s), is still open", name, s.Tool)
		}
	}
	return ""
}

// excludedScratch reports whether a task folder is an excluded project's: by its place, by the remote of a
// repository in it, or by where the session that made it worked then (an excluded project's lane keeps its scratch
// in the cache too).
func (r *runner) excludedScratch(ctx context.Context, canon string, w folderWalk, mk maker) (string, bool) {
	if why, ok := r.excluded(canon); ok {
		return why, true
	}
	for _, g := range w.gitDirs {
		if why, ok := r.ex.byRemote(&repo{remotes: gitURLs(g)}); ok {
			return "it holds " + why, true
		}
	}
	if mk.cwd != "" && filepath.IsAbs(mk.cwd) {
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
// their alternates, their worktrees), canonical, each with its repository's place.
func repoRefs(gitDirs []string, m *Mounts) map[string]string {
	out := map[string]string{}
	for _, g := range gitDirs {
		place := repoPlace(g)
		for _, p := range gitRefs(g) {
			if c := m.Canon(p); !under(c, m.Canon(place)) {
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
// outside of a repository in it).
func walkFolder(p string, minBytes int64) folderWalk {
	var w folderWalk
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
	count := func(fi os.FileInfo) (int64, bool) {
		s, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		w.Files++
		if t := newest(s); t.After(w.Newest) {
			w.Newest = t
		}
		k := inode{uint64(s.Dev), s.Ino}
		if seen[k] {
			return 0, true
		}
		seen[k] = true
		w.Bytes += s.Blocks * 512
		return s.Blocks * 512, true
	}
	count(st)
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
				if !under(wt, p) {
					w.links = append(w.links, "a worktree at "+short(filepath.Dir(wt))+" belongs to the repository at "+short(repoPlace(d.path)))
				}
			}
		}
		for _, e := range ents {
			fi, err := e.Info()
			if err != nil {
				continue // gone since the listing
			}
			b, _ := count(fi)
			ep := filepath.Join(d.path, e.Name())
			switch {
			case e.IsDir():
				if s, ok := fi.Sys().(*syscall.Stat_t); ok && s.Dev == rootStat.Dev {
					stack = append(stack, dir{ep, d.noModel || packageTree(ep, e.Name())})
				}
			case e.Name() == ".git" && fi.Mode().IsRegular():
				// a linked worktree, or a checkout whose git directory is kept apart: the repository names it
				if g, _ := isLinkedWorktree(d.path); g != "" && !under(g, p) && exists(g) {
					w.links = append(w.links, "it holds a checkout of the repository at "+short(repoPlace(gitCommon(g))))
				}
			case fi.Mode().IsRegular() && !d.noModel && fi.Size() >= minBytes && modelFormat(ep, e.Name(), fi.Size()):
				ino, _ := inodeOf(ep)
				rel, _ := filepath.Rel(p, ep)
				w.models = append(w.models, modelFile{rel: rel, ino: ino, bytes: b})
				w.modelBytes += b
			}
		}
	}
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
// kept: an environment's packages, node_modules, git's store, a Python environment, a Cargo target, or a tool's
// cache tagged as one.
func packageTree(d, name string) bool {
	switch name {
	case "site-packages", "dist-packages", "node_modules", ".git":
		return true
	}
	if exists(filepath.Join(d, "pyvenv.cfg")) || isTarget(d) {
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

// modelExts are the names of model weights', checkpoints' and datasets' files.
var modelExts = map[string]bool{".safetensors": true, ".gguf": true, ".ggml": true, ".pt": true, ".pth": true,
	".ckpt": true, ".distcp": true, ".onnx": true, ".tflite": true, ".parquet": true, ".arrow": true}

// modelFormat reports whether a file holds model data: by its name, or by its first bytes (GGUF, safetensors, a
// PyTorch archive, Parquet, Arrow), which is how a blob named by its hash (Ollama's, Hugging Face's) is known. What
// a run's results are kept in as often (JSON lines, CSV, NumPy and HDF5 arrays, pickles) is left to the offload:
// restic keeps it.
func modelFormat(p, name string, size int64) bool {
	if modelExts[strings.ToLower(filepath.Ext(name))] {
		return true
	}
	b := readHead(p, 256)
	for _, magic := range []string{"GGUF", "PAR1", "ARROW1"} {
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

// removeScratch acts on a task folder: under an exclusive BSD lock on it, it deletes its model data, each file
// through an os.Root of the folder and only while it is the inode judged, then offloads the rest with
// evidence-offload, unless only the model data goes.
func (r *runner) removeScratch(ctx context.Context, f *Finding) error {
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
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errLocked
	}
	defer syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
	if len(f.files) > 0 {
		rt, err := os.OpenRoot(f.Path)
		if err != nil {
			return err
		}
		for _, mf := range f.files {
			fi, err := rt.Lstat(mf.rel)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				rt.Close()
				return err
			}
			if s, ok := fi.Sys().(*syscall.Stat_t); !ok || (inode{uint64(s.Dev), s.Ino}) != mf.ino {
				continue // another file in its place since the judgment: it is not the one judged
			}
			if err := rt.Remove(mf.rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
				rt.Close()
				return err
			}
			f.removed += mf.bytes
		}
		rt.Close()
	}
	if f.modelOnly {
		return nil
	}
	out, err := runTool(ctx, offloadTimeout, r.cfg.Scratch.Offload, "evidence-offload", "EVIDENCE_TAG="+r.cfg.Scratch.Tag, f.Path)
	if m := snapshotRe.FindStringSubmatch(out); m != nil {
		f.snapshot = m[1]
	}
	if err != nil {
		return err
	}
	if exists(f.Path) || !exists(f.Path+pointerSuffix) {
		return fmt.Errorf("evidence-offload ran but did not leave %s in the folder's place: %s", short(f.Path)+pointerSuffix,
			strings.TrimSpace(lastLine(out)))
	}
	f.offloaded = true
	return nil
}

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
