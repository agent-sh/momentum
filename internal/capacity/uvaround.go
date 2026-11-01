package capacity

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// uv's prune deletes every environment in its cache, so while processes run from some, the rule prunes around them.
// A process runs from an environment uv keeps in the cache: a cached one in archive-v0 (a tool uvx runs uninstalled,
// a `uv run --with` layer), which environments-v2 links to by its interpreter and requirements; a script's (and a
// centralized project's) in environments-v2 itself; and a temporary one in builds-v0 (the overlay a `uv run --with`
// runs its command in, an isolated run's). An environment's files are clones, hardlinks or copies of the unpacked
// wheels in archive-v0 (uv's default link mode falls back from clone to hardlink to copy), so the wheels can go while
// it runs; uv takes a pointer to an archive that is gone as a cache miss, and unpacks or downloads it again. What it
// still reaches in the cache is in its links (uv's symlink link mode links its packages' files), the paths its .pth
// files name (the overlay names its layer and the environment beneath), its pyvenv.cfg (home, extends-environment)
// and its scripts' interpreter lines.

// uvBuckets are the buckets that hold environments, as uv 0.12 and 0.13 name them: its temporary environments, its
// environments and the links to cached ones, and the unpacked wheels and cached environments. The rule deletes from
// the last two (uvDeletes) when it prunes around running environments. Every other bucket (the wheels' pointers and
// metadata, the index, the built wheels, git) stays for uv's own prune, since nothing runs from it, but for the files
// of files-v0 that nothing links any more (uvFiles). builds-v0 stays too: uv removes an entry when the process that made
// it ends, and one whose uv was killed may still be the environment of the command it started (uvBuildsOf).
var (
	uvBuckets = []string{"builds-v0", "environments-v2", "archive-v0"}
	uvDeletes = []string{"environments-v2", "archive-v0"}
)

// rel is a path's place in the cache, relative to it ("" for the cache itself), or false for a path outside it.
func (c uvCache) rel(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	if c.name != nil {
		p = c.name(p)
	}
	for _, root := range []string{c.canon, c.real} {
		if root != "" && under(p, root) {
			return strings.TrimPrefix(strings.TrimPrefix(p, root), "/"), true
		}
	}
	return "", false
}

// entry is the entry of the cache a path is in, as the cache names it ("archive-v0/ID", "builds-v0/NAME",
// "environments-v2/NAME"), "" for a path in no bucket that holds environments (another bucket, a file at the top, a
// path outside the cache), and whole when the path is the cache or one of those buckets itself.
func (c uvCache) entry(p string) (e string, whole bool) {
	rel, ok := c.rel(p)
	if !ok {
		return "", false
	}
	if rel == "" {
		return "", true
	}
	bucket, rest, _ := strings.Cut(rel, "/")
	if !slices.Contains(uvBuckets, bucket) {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return "", true
	}
	return bucket + "/" + name, false
}

// uvReachWait is how long the walk of what the processes run from may take. It reads each directory of their
// environments once, which on a local disk takes milliseconds per thousand files; one that does not end in time is
// on a filesystem that does not answer, and what the processes use is not known.
var uvReachWait = 30 * time.Second

// uvReach is what the processes that use the cache keep of it: each entry a place of theirs is in, and every entry
// such an entry reaches (see walk). places are the places it answered, as the processes give them, so a later look
// knows them without reading a filesystem; bytes is the kept entries' size on disk and builds-v0's, each file once,
// and builds what of it is in builds-v0; envs is how many kept entries outside builds-v0 are environments, and
// overlays how many overlays of builds-v0 a command may run in unseen. buildsAtLook are builds-v0's entries at the
// first look. resolve resolves a path's links: resolveLinks, read once by the caller.
type uvReach struct {
	c            uvCache
	keep         map[string]bool
	places       map[string]bool
	bytes        int64
	builds       int64
	envs         int
	overlays     int
	why          string
	seen         map[inode]bool
	queue        []uvPlace
	walkedOuts   map[string]bool
	resolve      func(string) (string, error)
	buildsAtLook map[string]bool
}

// uvReachOf walks what the places reach: found's, and the overlays of builds-v0 a command may run in unseen (b's
// live). It gives up, saying why, on a builds-v0 it could not read, a place that names the cache or a bucket itself,
// a directory of a kept entry it cannot read, and a walk that does not end in uvReachWait. Then it measures the rest of
// builds-v0, which stays.
func uvReachOf(c uvCache, found uvFound, b uvBuilds) *uvReach {
	if b.err != nil {
		return &uvReach{why: "builds-v0 cannot be read, so what runs from it is not known: " + b.err.Error()}
	}
	r := &uvReach{c: c, keep: map[string]bool{}, places: map[string]bool{}, seen: map[inode]bool{},
		walkedOuts: map[string]bool{}, resolve: resolveLinks, buildsAtLook: b.names, overlays: len(b.live)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.queue = append(r.queue, found.places...)
		for _, n := range sortedKeys(b.live) {
			r.queue = append(r.queue, uvPlace{path: filepath.Join(c.dir, "builds-v0", n)})
		}
		for len(r.queue) > 0 && r.why == "" {
			pl := r.queue[0]
			r.queue = r.queue[1:]
			r.place(pl)
		}
		for _, n := range sortedKeys(b.names) {
			if !r.keep["builds-v0/"+n] && r.why == "" {
				before := r.bytes
				r.measure(filepath.Join(c.dir, "builds-v0", n))
				r.builds += r.bytes - before
			}
		}
	}()
	select {
	case <-done:
		return r
	case <-time.After(uvReachWait):
		return &uvReach{why: fmt.Sprintf("the walk of what they run from did not end in %s, so what they use is not known", uvReachWait)}
	}
}

// uvBuilds is builds-v0 at the first look: its entries, and the overlays among them a command may run in unseen
// (uvBuildsOf). err is why builds-v0 could not be read, and blind whether a command may then run in it unseen.
type uvBuilds struct {
	names, live map[string]bool
	err         error
	blind       bool
}

// uvOverlaySlack is how much earlier than an overlay's birth a command's start may read and still be one that runs in
// it: /proc gives a process's start in ticks after the boot, and the boot by the uptime, read a moment apart.
const uvOverlaySlack = 2 * time.Second

// uvBirth is a path's birth (birth): a test's way to give an overlay its age.
var uvBirth = birth

// uvBuildsOf reads builds-v0 at the first look: its entries, and the overlays a command may run in unseen. A uv run
// --with runs its command in an overlay there (its site-packages holds _uv_ephemeral_overlay.pth, its pyvenv.cfg
// names extends-environment), and removes it as the run ends, or leaves it when its uv is killed; a command that set
// its title over its environment maps nothing of the layer it imports from, and once its uv is gone nothing names it.
// So an overlay is live while such a command may run in it: a process whose environment holds no PATH, that runs the
// interpreter the overlay names (its pyvenv.cfg's home; any, under a uv run that layers an environment), and that
// started after the overlay was made, in this boot. An overlay no such process can run in holds nothing, and another
// entry (a build's environment, an isolated run's) is named by the extension a titled command maps. Where the
// filesystem keeps no birth, or /proc gives no uptime or start, the time is not asked; an entry whose pyvenv.cfg
// cannot be read counts as an overlay. The read has uvReachWait to end.
func uvBuildsOf(c uvCache, s *Sample, procRoot string) uvBuilds {
	type titled struct {
		bin     string // the directory of the interpreter it runs
		start   int64
		layered bool // it runs under a uv run that layers an environment over its command's
	}
	var procs []titled
	for _, pid := range sortedPIDs(s.table()) {
		if !noPath(procRoot, pid) {
			continue
		}
		exe, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
		if err != nil {
			continue // a kernel thread's, or one that ended
		}
		procs = append(procs, titled{c.canonical(filepath.Dir(strings.TrimSuffix(exe, " (deleted)"))), s.Table[pid].Start,
			underLayers(s, pid)})
	}
	up, now := uptimeTicks(procRoot), time.Now()
	tick := time.Second / userHZ
	boot := now.Add(-time.Duration(up) * tick)
	done := make(chan uvBuilds, 1)
	go func() {
		b := uvBuilds{names: map[string]bool{}, live: map[string]bool{}}
		dir := filepath.Join(c.dir, "builds-v0")
		ents, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			b.err, b.blind = err, len(procs) > 0
		}
		for _, e := range ents {
			b.names[e.Name()] = true
			if len(procs) == 0 {
				continue
			}
			p := filepath.Join(dir, e.Name())
			cfg, err := readSmallErr(filepath.Join(p, "pyvenv.cfg"))
			pth, _ := filepath.Glob(filepath.Join(p, "lib", "python*", "site-packages", "_uv_ephemeral_overlay.pth"))
			if (err == nil || errors.Is(err, fs.ErrNotExist)) && len(pth) == 0 && cfgValue(cfg, "extends-environment") == "" {
				continue // no overlay
			}
			born, timed := uvBirth(p)
			timed = timed && up > 0
			if timed && born.Before(boot.Add(-uvOverlaySlack)) {
				continue // made before the boot: no process that ran in it runs
			}
			home := cfgValue(cfg, "home")
			if res, err := filepath.EvalSymlinks(home); home != "" && err == nil {
				home = res
			}
			for _, q := range procs {
				started := boot.Add(time.Duration(q.start) * tick)
				if (home == "" || q.layered || q.bin == c.canonical(home)) &&
					(!timed || q.start <= 0 || !started.Before(born.Add(-uvOverlaySlack))) {
					b.live[e.Name()] = true
					break
				}
			}
		}
		done <- b
	}()
	select {
	case b := <-done:
		return b
	case <-time.After(uvReachWait):
		return uvBuilds{err: fmt.Errorf("it did not answer in %s", uvReachWait), blind: true}
	}
}

// underLayers reports whether a process runs under a uv run that layers an environment over its command's
// (uvLayers): its own overlay, made after that uv started, holds what it imports.
func underLayers(s *Sample, pid int) bool {
	p := s.Table[pid]
	for hops := 0; hops < 8; hops++ {
		q, ok := s.Table[p.PPID]
		if !ok || q.PID == p.PID {
			return false
		}
		if uvLayers(q.Argv) {
			return true
		}
		p = q
	}
	return false
}

// canonical is a path as the cache's places are named (name), or as it is.
func (c uvCache) canonical(p string) string {
	if c.name != nil {
		return c.name(p)
	}
	return p
}

// where is a place with its links resolved, as far as it names the entry it is in: the place resolved when that is in
// the cache, else the nearest directory above it that resolves into the cache, with what follows that directory (an
// environment's bin/python links to an interpreter outside it, and a project's .venv outside the cache may link into
// it). A place no directory of which resolves into the cache is the place resolved, or as given.
func (r *uvReach) where(p string) string { return uvWhere(r.c, r.resolve, p) }

// uvWhere is where, with resolve to resolve a path's links.
func uvWhere(c uvCache, resolve func(string) (string, error), p string) string {
	for q := p; ; q = filepath.Dir(q) {
		if res, err := resolve(q); err == nil {
			if _, in := c.rel(res); in {
				if q == p {
					return res
				}
				return filepath.Join(res, strings.TrimPrefix(p, q+"/"))
			}
		}
		if q == filepath.Dir(q) {
			break
		}
	}
	if res, err := resolve(p); err == nil {
		return res
	}
	return p
}

// place keeps the entry a place is in (where), and puts what that entry reaches on the queue. A process's place
// outside the cache is an environment whose packages link into it: what it reaches is walked, and nothing of it is
// kept. A place in one of environments-v2's directories of links is the link it goes through: one that leads nowhere
// keeps nothing.
func (r *uvReach) place(pl uvPlace) {
	if r.places[pl.path] {
		return
	}
	r.places[pl.path] = true
	real := r.where(pl.path)
	e, whole := r.c.entry(real)
	if whole {
		r.why = fmt.Sprintf("%s uses %s itself", whoOr(pl.who, "a process"), short(real))
		return
	}
	if e == "" {
		if _, in := r.c.rel(real); !in && pl.who != "" && !r.walkedOuts[real] {
			r.walkedOuts[real] = true
			r.walk(real, false)
		}
		return
	}
	if r.keep[e] {
		return
	}
	dir := filepath.Join(r.c.dir, e)
	if rel, _ := r.c.rel(real); strings.HasPrefix(e, "environments-v2/") {
		if parts := strings.SplitN(rel, "/", 4); len(parts) >= 3 {
			link := filepath.Join(dir, parts[2])
			if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				// a link to a cached environment, among the links of its interpreter: what it leads to is kept
				if t, err := r.resolve(link); err == nil {
					r.queue = append(r.queue, uvPlace{path: t, who: pl.who})
				}
				return
			}
		}
	}
	r.keep[e] = true
	if exists(filepath.Join(dir, "pyvenv.cfg")) && !strings.HasPrefix(e, "builds-v0/") {
		r.envs++
	}
	before := r.bytes
	r.walk(dir, true)
	if strings.HasPrefix(e, "builds-v0/") {
		r.builds += r.bytes - before
	}
}

// walk reads a directory's tree, not following its links, and puts on the queue what it reaches in the cache: each
// link's target, the paths each .pth file in a site-packages names (a path on a line, or a string in a line that
// starts with import, as the overlay of a uv run --with names its layer and the environment beneath) and an editable
// install's finder module, the paths pyvenv.cfg gives (home, extends-environment), and the interpreter line of each
// script in its bin. measure counts the size of what it reads. A .pth file, a finder or a pyvenv.cfg it cannot read
// is one whose paths are not known.
func (r *uvReach) walk(dir string, measure bool) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if measure {
			r.count(d)
		}
		read := func() ([]byte, error) {
			b, err := readSmallErr(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return b, err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if t, err := r.resolve(p); err == nil && r.c.in(t) {
				r.queue = append(r.queue, uvPlace{path: t})
			}
		case !d.Type().IsRegular():
		case filepath.Base(filepath.Dir(p)) == "site-packages" && strings.HasSuffix(p, ".pth"):
			b, err := read()
			if err != nil {
				return err
			}
			r.named(pthPaths(b, filepath.Dir(p), false))
		case filepath.Base(filepath.Dir(p)) == "site-packages" && strings.HasPrefix(d.Name(), "__editable__") &&
			strings.HasSuffix(p, ".py"):
			b, err := read()
			if err != nil {
				return err
			}
			r.named(pthPaths(b, filepath.Dir(p), true))
		case d.Name() == "pyvenv.cfg" && filepath.Dir(p) == dir:
			b, err := read()
			if err != nil {
				return err
			}
			r.named(cfgPaths(b))
		case filepath.Base(filepath.Dir(p)) == "bin" && filepath.Dir(filepath.Dir(p)) == dir:
			r.named(shebangPath(p))
		}
		return nil
	})
	if err != nil && r.why == "" {
		r.why = fmt.Sprintf("%s cannot be read, so what it reaches is not known: %v", short(dir), err)
	}
}

// count adds a file's size on disk to what the walk measured, each file once.
func (r *uvReach) count(d fs.DirEntry) {
	if info, err := d.Info(); err == nil {
		if s, ok := info.Sys().(*syscall.Stat_t); ok {
			if k := (inode{uint64(s.Dev), s.Ino}); !r.seen[k] {
				r.seen[k] = true
				r.bytes += s.Blocks * 512
			}
		}
	}
}

// measure counts the size on disk of a directory's tree that stays whole (an entry of builds-v0 no command runs in),
// and follows nothing in it. What it cannot read it does not count.
func (r *uvReach) measure(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			r.count(d)
		}
		return nil
	})
}

// named puts on the queue the paths a file names that are in the cache, by their names or with their links resolved
// (a project's .venv that is a centralized environment). Of one outside it, only an environment whose packages link
// into the cache is the rule's (the base an overlay names, by its root or its site-packages, in uv's symlink link
// mode): what they reach is walked, as a process's environment outside the cache is, and nothing of it is kept.
func (r *uvReach) named(paths []string) {
	for _, q := range paths {
		if _, in := r.c.rel(q); in {
			r.queue = append(r.queue, uvPlace{path: q})
			continue
		}
		if t, err := r.resolve(q); err == nil && t != q {
			if _, in := r.c.rel(t); in {
				r.queue = append(r.queue, uvPlace{path: t})
				continue
			}
		}
		env := q
		if filepath.Base(q) == "site-packages" && filepath.Base(filepath.Dir(filepath.Dir(q))) == "lib" {
			env = filepath.Dir(filepath.Dir(filepath.Dir(q)))
		}
		if !r.walkedOuts[env] && exists(filepath.Join(env, "pyvenv.cfg")) && linksInto(env, r.c) {
			r.walkedOuts[env] = true
			r.walk(env, false)
		}
	}
}

// outside names the processes of a later look that use what the first look did not keep, or "". A place the first
// look answered is known; any other is judged by its name alone, since the look under the lock reads no filesystem
// but /proc: one in an entry the first look kept is fine, and so is one /proc gave resolved in another bucket. One
// that names another bucket on a command line or in an environment is not: a wheel's pointer there is a link into
// archive-v0, which the name does not say. A place no look could read (envs.unread) is not either.
func (r *uvReach) outside(found uvFound, envs *uvEnvs) string {
	if found.blind != "" {
		return found.blind
	}
	names := &pidNames{}
	for _, pl := range found.places {
		if r.places[pl.path] && !slices.Contains(envs.unread, pl.path) {
			continue
		}
		e, whole := r.c.entry(pl.path)
		if !whole && !slices.Contains(envs.unread, pl.path) && (e != "" && r.keep[e] || e == "" && pl.resolved && r.c.in(pl.path)) {
			continue
		}
		names.addWho(whoOr(pl.who, short(pl.path)))
	}
	return names.String()
}

// newBuilds names an entry of builds-v0 made since the first look, or says that builds-v0 cannot be read again: what
// such an entry reaches was not walked. Under the cache's lock no uv process makes one, so this is the rule's last
// word before it deletes, read from the cache itself.
func (r *uvReach) newBuilds() string {
	ents, err := os.ReadDir(filepath.Join(r.c.dir, "builds-v0"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "builds-v0 cannot be read again: " + err.Error()
	}
	for _, e := range ents {
		if !r.buildsAtLook[e.Name()] {
			return fmt.Sprintf("%s was made since the look", short(filepath.Join(r.c.dir, "builds-v0", e.Name())))
		}
	}
	return ""
}

// says what the prune around running environments keeps, for its finding's detail: how many processes run from the
// cache (each named by the uv process it runs under, as the notes name them), the jobs that name places in it, and the
// overlays of builds-v0 a command may run in unseen, and the environments it keeps with what they reach.
func (r *uvReach) says(found uvFound) string {
	var why []string
	switch found.users {
	case 0:
	case 1:
		why = append(why, "1 process runs from it")
	default:
		why = append(why, fmt.Sprintf("%d processes run from it", found.users))
	}
	switch found.jobs {
	case 0:
	case 1:
		why = append(why, "1 job names a place in it")
	default:
		why = append(why, fmt.Sprintf("%d jobs name places in it", found.jobs))
	}
	switch r.overlays {
	case 0:
	case 1:
		why = append(why, "builds-v0 holds 1 overlay a command may run in unseen")
	default:
		why = append(why, fmt.Sprintf("builds-v0 holds %d overlays a command may run in unseen", r.overlays))
	}
	envs := fmt.Sprintf("%d environments", r.envs)
	if r.envs == 1 {
		envs = "1 environment"
	}
	said := strings.Join(why, " and ")
	if len(why) > 2 {
		said = strings.Join(why[:len(why)-1], ", ") + " and " + why[len(why)-1]
	}
	return fmt.Sprintf("%s, so the prune goes around what they use: it keeps %s and what they reach (%s), and builds-v0 "+
		"(%s); the rest of environments-v2 and archive-v0 goes, and the other buckets stay for uv's own prune",
		said, envs, gib(r.bytes-r.builds), gib(r.builds))
}

// says what a prune around running environments did, for its finding's detail.
func (u uvRemoval) says() string {
	s := fmt.Sprintf("it removed %d entries", u.removed)
	if u.removed == 1 {
		s = "it removed 1 entry"
	}
	switch u.files {
	case 0:
	case 1:
		s += " and 1 file of files-v0 no archive links any more"
	default:
		s += fmt.Sprintf(" and %d files of files-v0 no archive links any more", u.files)
	}
	if len(u.failures) > 0 {
		s += fmt.Sprintf(", %d could not be removed (%s)", len(u.failures), u.failures[0])
	}
	if len(u.stayed) > 0 {
		s += fmt.Sprintf(", %d stay (%s)", len(u.stayed), u.stayed[0])
	}
	if u.left > 0 {
		s += fmt.Sprintf(", and left %d for a later run: it moves entries aside for %s at most under the lock a new uv "+
			"process waits for", u.left, uvAroundFor)
	}
	if u.trash != "" {
		s += "; what it moved aside could not all be deleted, and each later run tries again: " + u.trash
	}
	return s
}

// whoOr is who, or else.
func whoOr(who, or string) string {
	if who == "" {
		return or
	}
	return who
}

// readSmall reads the first MiB of a file, or nothing.
func readSmall(p string) []byte {
	b, _ := readSmallErr(p)
	return b
}

// readSmallErr reads the first MiB of a file, or says why it cannot.
func readSmallErr(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// pthPaths are the paths a .pth file names: a line that is a path, relative to the file's directory, as
// site.addsitedir reads one, and in a line that starts with import, every string that is an absolute path (uv's
// overlay writes `import site; site.addsitedir(__import__("os").fsdecode(b"/path"))`). Of a module (an editable
// install's finder, which maps its packages to their sources), every such string.
func pthPaths(b []byte, dir string, module bool) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		switch t := strings.TrimSpace(sc.Text()); {
		case t == "" || strings.HasPrefix(t, "#"):
		case module || strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "import\t"):
			out = append(out, pyStrings(t)...)
		default:
			if !filepath.IsAbs(t) {
				t = filepath.Join(dir, t)
			}
			out = append(out, filepath.Clean(t))
		}
	}
	return out
}

// pyStrings are the strings of a line of Python, its escapes undone (\\, \', \", \n, \t, \r, \xNN, \NNN), each that
// is an absolute path.
func pyStrings(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		q := line[i]
		if q != '\'' && q != '"' {
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(line) && line[j] != q; j++ {
			if line[j] != '\\' || j+1 >= len(line) {
				b.WriteByte(line[j])
				continue
			}
			j++
			switch c := line[j]; c {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'x':
				if j+2 < len(line) {
					if n, err := strconv.ParseUint(line[j+1:j+3], 16, 8); err == nil {
						b.WriteByte(byte(n))
						j += 2
						continue
					}
				}
				b.WriteByte(c)
			default:
				if c >= '0' && c <= '7' && j+2 < len(line) {
					if n, err := strconv.ParseUint(line[j:j+3], 8, 8); err == nil {
						b.WriteByte(byte(n))
						j += 2
						continue
					}
				}
				b.WriteByte(c)
			}
		}
		if s := b.String(); filepath.IsAbs(s) {
			out = append(out, filepath.Clean(s))
		}
		i = j
	}
	return out
}

// cfgPaths are the paths a pyvenv.cfg gives: the interpreter's directory (home), the environment an overlay extends
// (extends-environment), and the interpreter it was made with (base-executable).
func cfgPaths(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		switch k = strings.TrimSpace(k); {
		case !ok:
		case k == "home" || k == "extends-environment" || k == "base-executable":
			if v = strings.TrimSpace(v); filepath.IsAbs(v) {
				out = append(out, filepath.Clean(v))
			}
		}
	}
	return out
}

// shebangPath is the interpreter a script's first line names, or none.
func shebangPath(p string) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, 512)
	n, _ := f.Read(b)
	line, _, _ := bytes.Cut(b[:n], []byte("\n"))
	rest, ok := bytes.CutPrefix(line, []byte("#!"))
	if !ok {
		return nil
	}
	if f := strings.Fields(string(rest)); len(f) > 0 && filepath.IsAbs(f[0]) {
		return []string{filepath.Clean(f[0])}
	}
	return nil
}

// cfgValue is what a pyvenv.cfg sets a key to, or "".
func cfgValue(b []byte, key string) string {
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// uvAroundFor is how long the rule moves entries aside under the cache's lock: a uv process that starts meanwhile
// waits for the lock up to its UV_LOCK_TIMEOUT, 5 minutes by default, and then fails. No entry starts after this.
var uvAroundFor = 4 * time.Minute

// uvTrash is where a prune around running environments moves what it deletes, under the cache's lock: an entry leaves
// uv's names whole and at once, and is deleted once the lock is gone, since nothing can reach it there. A run stopped
// midway leaves it there for the next run to delete; uv's own prune removes it too, as no bucket of its own.
const uvTrash = ".capacity-trash"

// uvRemoval is what a prune around running environments did: the entries it moved aside, the files of files-v0 it
// removed, the entries that could not go (each with why), those that stay since they hold a mount point, those it
// left when its time ran out, why what it moved aside could not all be deleted, and what it freed: each file whose
// last link it removed.
type uvRemoval struct {
	removed, files, left int
	failures, stayed     []string
	trash                string
	freed                int64
}

// uvRemove moves aside, under the cache's lock, every entry of environments-v2 and archive-v0 that k does not keep: a
// script's or a project's environment, an unpacked wheel, a cached environment. A link of environments-v2 to a cached
// environment that is not kept, or to none, goes as it is, and a directory of such links once it is empty; what is
// neither an environment nor such a link stays. A name archive-v0 holds that starts with a dot is no entry uv made,
// and stays. stay says why an entry stays, if it does (it holds a mount point). Each move goes through
// an os.Root of the cache, so no link leads it out, with an exclusive BSD lock on the entry: another holder of a lock
// keeps it. First it removes the files of files-v0 nothing links any more (an archive this rule moved aside earlier
// held their last links). No entry starts after until, or once stopped says a stop came (the run ends once the lock
// file is back).
func uvRemove(c uvCache, k *uvReach, until time.Time, stopped func() bool, stay func(string) string) uvRemoval {
	var out uvRemoval
	rt, err := os.OpenRoot(c.dir)
	if err != nil {
		out.failures = append(out.failures, short(c.dir)+": "+err.Error())
		return out
	}
	defer rt.Close()
	uvFiles(rt, c.dir, until, stopped, &out)
	aside := filepath.Join(uvTrash, strconv.FormatInt(time.Now().UnixNano(), 36))
	late := func() bool {
		if time.Now().After(until) || stopped() {
			out.left++
			return true
		}
		return false
	}
	for _, b := range uvDeletes {
		ents, err := os.ReadDir(filepath.Join(c.dir, b))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			out.failures = append(out.failures, b+": "+err.Error())
		}
		for _, e := range ents {
			rel := b + "/" + e.Name()
			switch {
			case k.keep[rel]:
				continue
			case b == "archive-v0" && strings.HasPrefix(e.Name(), "."):
				continue
			case b == "environments-v2" && e.IsDir() && !exists(filepath.Join(c.dir, rel, "pyvenv.cfg")):
				out.removeLinks(rt, c, k, rel, late)
				continue
			case b == "environments-v2" && !e.IsDir():
				continue
			}
			if why := stay(filepath.Join(c.dir, rel)); why != "" {
				out.stayed = append(out.stayed, rel+": "+why)
			} else if !late() {
				out.move(rt, rel, filepath.Join(aside, rel))
			}
		}
	}
	return out
}

// move moves an entry aside, through the cache's root, while it holds an exclusive BSD lock on it. A directory the
// user may not read cannot be opened to be locked, and one the user may not write cannot go to another parent: as uv
// does before it deletes, it is made readable and writable, and the move runs again. An entry that is gone went.
func (u *uvRemoval) move(rt *os.Root, rel, to string) {
	err := moveLocked(rt, rel, to)
	if errors.Is(err, fs.ErrPermission) {
		if fi, e := rt.Lstat(rel); e == nil && fi.IsDir() && rt.Chmod(rel, fi.Mode().Perm()|0o700) == nil {
			err = moveLocked(rt, rel, to)
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		if _, e := rt.Lstat(rel); errors.Is(e, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		u.failures = append(u.failures, rel+": "+err.Error())
		return
	}
	u.removed++
}

// moveLocked opens an entry, takes an exclusive BSD lock on it without waiting (another holder of a lock keeps it),
// and renames it to its place aside.
func moveLocked(rt *os.Root, rel, to string) error {
	fh, err := rt.Open(rel)
	if err != nil {
		return err
	}
	defer fh.Close()
	if syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errLocked
	}
	if err := rt.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return rt.Rename(rel, to)
}

// removeLinks removes the links of one of environments-v2's directories of links whose cached environment is not
// kept, or that lead nowhere, and the directory once it is empty. What is no link stays.
func (u *uvRemoval) removeLinks(rt *os.Root, c uvCache, k *uvReach, shard string, late func() bool) {
	ents, err := os.ReadDir(filepath.Join(c.dir, shard))
	if err != nil {
		return
	}
	left := 0
	for _, l := range ents {
		rel := shard + "/" + l.Name()
		if l.Type()&fs.ModeSymlink == 0 {
			left++
			continue
		}
		t, err := rt.Readlink(rel)
		if err == nil && !filepath.IsAbs(t) {
			t = filepath.Join(c.dir, shard, t)
		}
		if e, _ := c.entry(filepath.Clean(t)); err == nil && k.keep[e] {
			left++
			continue
		}
		if late() {
			left++
			continue
		}
		switch n, err := removeCounted(rt, rel); {
		case err == nil:
			u.removed++
			u.freed += n // a link with a long target holds a block
		case errors.Is(err, fs.ErrNotExist):
			u.removed++
		default:
			left++
			u.failures = append(u.failures, rel+": "+err.Error())
		}
	}
	if left == 0 {
		if n, err := removeCounted(rt, shard); err == nil { // empty, or a link came since: then it stays
			u.freed += n
		}
	}
}

// removeCounted removes one entry through the cache's root and says what that freed: its blocks, when it was a
// directory or its file's last link.
func removeCounted(rt *os.Root, rel string) (int64, error) {
	var n int64
	if fi, err := rt.Lstat(rel); err == nil {
		if s, ok := fi.Sys().(*syscall.Stat_t); ok && (fi.IsDir() || s.Nlink <= 1) {
			n = s.Blocks * 512
		}
	}
	if err := rt.Remove(rel); err != nil {
		return 0, err
	}
	return n, nil
}

// uvFiles removes the files of files-v0 that nothing links any more, and then its directories left empty. uv 0.13's
// content-addressed cache (a preview) keeps a wheel's files there and hardlinks them into the wheel's archive, and an
// environment's files are links of those, so each file only an archive that went linked has one link left: uv's own
// prune removes those. A running environment's link keeps its file's count above one. An archive moved aside still
// links its files until it is deleted, after the lock, so they go at the next prune.
func uvFiles(rt *os.Root, dir string, until time.Time, stopped func() bool, out *uvRemoval) {
	files := filepath.Join(dir, "files-v0")
	var dirs []string
	filepath.WalkDir(files, func(p string, d fs.DirEntry, err error) error {
		if err != nil || time.Now().After(until) || stopped() {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case d.IsDir() && p != files:
			dirs = append(dirs, rel)
		case d.Type().IsRegular():
			if info, err := d.Info(); err == nil {
				if s, ok := info.Sys().(*syscall.Stat_t); ok && s.Nlink == 1 && rt.Remove(rel) == nil {
					out.files++
					out.freed += s.Blocks * 512 // its last link
				}
			}
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		if n, err := removeCounted(rt, dirs[i]); err == nil { // children first; one that is not empty stays
			out.freed += n
		}
	}
}

// uvTrashed are the entries prunes around running environments moved aside, each where its move put it
// (.capacity-trash/STAMP/BUCKET/NAME): what deleting the trash deletes, but the directories the moves made.
func uvTrashed(dir string) []string {
	var out []string
	for _, stamp := range listDirs(filepath.Join(dir, uvTrash)) {
		for _, bucket := range listDirs(stamp) {
			ents, _ := os.ReadDir(bucket)
			for _, e := range ents {
				out = append(out, filepath.Join(bucket, e.Name()))
			}
		}
	}
	return out
}

// uvEmptyTrash deletes what prunes around running environments moved aside, through an os.Root of the cache: once it
// is aside nothing reaches it, so no lock is needed. A directory the user may not read or write is made so first, as
// uv does before it deletes.
func uvEmptyTrash(dir string) error {
	rt, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer rt.Close()
	err = rt.RemoveAll(uvTrash)
	if errors.Is(err, fs.ErrPermission) {
		// the walk sees a directory before it reads it, so one made readable here is walked too
		filepath.WalkDir(filepath.Join(dir, uvTrash), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				if info, err := d.Info(); err == nil {
					rel, _ := filepath.Rel(dir, p)
					rt.Chmod(rel, info.Mode().Perm()|0o700)
				}
			}
			return nil
		})
		err = rt.RemoveAll(uvTrash)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// uvJobPlace is a place a scheduled job names in an entry of a bucket that holds environments, as a process's place
// is found (where): the path with its links resolved, or the path through the nearest directory above it that
// resolves into the cache (a project's .venv that links into it). A job that names the cache, one of its buckets, or a
// path in another bucket names no environment. resolve is resolveLinks, read by the caller.
func uvJobPlace(c uvCache, resolve func(string) (string, error), p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	q := uvWhere(c, resolve, p)
	e, whole := c.entry(q)
	return q, e != "" && !whole
}

// pidNames names processes for a note: at most three, each once, then how many more.
type pidNames struct {
	seen  map[string]bool
	names []string
}

func (n *pidNames) add(pid int, what string) {
	n.addKey(strconv.Itoa(pid), fmt.Sprintf("%s (pid %d)", what, pid))
}

func (n *pidNames) addWho(who string) { n.addKey(who, who) }

func (n *pidNames) addKey(key, name string) {
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	if !n.seen[key] && len(n.names) < 3 {
		n.names = append(n.names, name)
	}
	n.seen[key] = true
}

func (n *pidNames) String() string {
	out := slices.Clone(n.names)
	if len(n.seen) > len(n.names) {
		out = append(out, fmt.Sprintf("%d more", len(n.seen)-len(n.names)))
	}
	return strings.Join(out, ", ")
}

// uvTakeLock takes uv's lock on the cache exclusively, without waiting, as uv's own prune does when no uv process
// holds it: a uv process that starts meanwhile waits for it. It gives up when the lock file is not the one the look
// saw, or a uv process holds it.
func uvTakeLock(lock string, id lockID) (*os.File, string) {
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		return nil, "the cache's lock file cannot be opened: " + err.Error()
	}
	if st, err := f.Stat(); err != nil || idOf(st) != id {
		f.Close()
		return nil, "the cache's lock file changed since the look"
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return f, ""
	case errors.Is(err, syscall.EWOULDBLOCK):
		f.Close()
		return nil, "a uv process took the cache's lock since the look"
	default:
		f.Close()
		return nil, "the cache's lock cannot be taken: " + err.Error()
	}
}
