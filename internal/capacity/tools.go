package capacity

import (
	"bytes"
	"context"
	"encoding/json"
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

// The uv and docker rules prune with each tool's own command, never by deleting its files: the tool knows what in
// its cache is in use.

// Tools runs uv and docker.
type Tools interface {
	UV(ctx context.Context, timeout time.Duration, args ...string) (string, error)
	Docker(ctx context.Context, timeout time.Duration, args ...string) (string, error)
}

// errNoTool means the tool is not installed.
var errNoTool = errors.New("not installed")

type execTools struct{ uv, docker string }

// UV runs uv with a short wait on its cache lock: a prune that another uv process blocks fails at once instead of
// waiting its default 5 minutes.
func (t execTools) UV(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return runTool(ctx, timeout, t.uv, "uv", append([]string{"UV_LOCK_TIMEOUT=10"}, args...)...)
}

func (t execTools) Docker(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return runTool(ctx, timeout, t.docker, "docker", args...)
}

func runTool(ctx context.Context, timeout time.Duration, path, name string, args ...string) (string, error) {
	if path == "" {
		p, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, errNoTool)
		}
		path = p
	}
	var env []string
	for len(args) > 0 && envAssign.MatchString(args[0]) {
		env, args = append(env, args[0]), args[1:] // NAME=value before the arguments: the command's environment
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// the timeout ends the tool and every process it started (a script's children, which would keep its output
	// open), and bounds the wait for its output after that. The tool stays in the run's process group, so a stop
	// from the terminal reaches it as it reaches the run.
	cmd.Cancel = func() error { return stopTree(cmd.Process.Pid) }
	cmd.WaitDelay = toolWaitDelay
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if cctx.Err() != nil {
		return out.String(), fmt.Errorf("%s %s: %w after %s", name, strings.Join(args[:min(len(args), 2)], " "), errTimeout, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 { // its head says what failed, its tail where
			msg = msg[:300] + " ... " + msg[len(msg)-300:]
		}
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args[:min(len(args), 2)], " "), err, msg)
	}
	return out.String(), nil
}

// toolWaitDelay is how long a tool and its children have to end, and its output to close, once its timeout passed:
// a child that ignores the stop may hold the output open.
var toolWaitDelay = 5 * time.Second

// stopTree sends SIGTERM to a process and to every process it started, from the top down, so a parent stopped
// first starts no more. The descendants are read from /proc once; one the run may not signal (a command sudo runs)
// gets the stop from its parent (sudo relays it).
func stopTree(pid int) error {
	children := map[int][]int{}
	if ents, err := os.ReadDir("/proc"); err == nil {
		for _, e := range ents {
			p, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			if i := bytes.LastIndexByte(b, ')'); i >= 0 {
				if f := strings.Fields(string(b[i+1:])); len(f) > 1 {
					if pp, err := strconv.Atoi(f[1]); err == nil {
						children[pp] = append(children[pp], p)
					}
				}
			}
		}
	}
	tree := []int{pid}
	for i := 0; i < len(tree) && len(tree) < 4096; i++ {
		tree = append(tree, children[tree[i]]...)
	}
	err := syscall.Kill(pid, syscall.SIGTERM)
	for _, p := range tree[1:] {
		syscall.Kill(p, syscall.SIGTERM)
	}
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// uv prunes the uv cache with `uv cache prune --ci` when it is over its size, or when the last prune is older than
// the cadence (or there was none). --ci keeps the wheels uv built and drops the ones it can download again. Every
// prune also deletes every environment uv keeps in its cache (a `uv run --with` layer, a `uv run --script` script's,
// an uninstalled tool's that uvx runs), and a process running from one fails its next import; one running from an
// environment outside the cache whose files are no links into it (a project's .venv, an installed tool's, their
// files hardlinks or copies of the cache's) is untouched. Every uv process holds uv's lock shared for its life, so a
// prune that takes the lock waits for all of them. The rule looks first. It waits quietly for a uv process that may
// write the cache now (an install, a sync, a run that has not started its command), and waits for, and names, a
// process that uses the cache: a path in it, an environment in it or linked into it, a container that mounts it, a
// holder of the lock it cannot read, a process that waits for the lock or holds it alone (uv's own clean or prune).
// Otherwise it prunes: with uv's own lock when nobody holds it, or, when every holder is a uv process past its sync
// that runs outside the cache, with --force under a lock of its own put in the lock file's place (uvLockInPlace).
func (r *runner) uv(ctx context.Context, live bool) ([]Finding, []string, error) {
	out, err := r.tools.UV(ctx, 30*time.Second, "cache", "dir")
	if errors.Is(err, errNoTool) {
		return nil, []string{"uv is not installed"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dir := strings.TrimSpace(lastLine(out))
	size, counted := r.uvSize(ctx, dir)
	last, err := r.mark(ctx, "prune:uv")
	if err != nil {
		return nil, nil, err
	}
	max := int64(r.cfg.UV.MaxGiB * (1 << 30))
	every := time.Duration(r.cfg.UV.EveryDays * 24 * float64(time.Hour))
	notes := []string{fmt.Sprintf("cache %s at %s", gib(size), short(dir))}
	f := Finding{Rule: RuleUV, Path: dir, canon: r.m.Canon(dir), Bytes: size}
	switch {
	case size > max:
		f.Detail = fmt.Sprintf("over %s", gib(max))
	case last.IsZero():
		f.Detail = "no prune on record"
	case r.now.Sub(last) >= every:
		f.Detail = "last pruned " + last.Format("2006-01-02")
	default:
		notes = append(notes, "last pruned "+last.Format("2006-01-02 15:04")+", under "+gib(max))
		return nil, notes, nil
	}
	// the cache as uv spells it and as the kernel does: /proc shows paths with their links resolved
	c := uvCache{canon: f.canon, real: r.m.resolve(dir), lock: r.m.Canon(filepath.Join(dir, ".lock")), name: r.m.Canon}
	look := func() *Sample {
		return TakeSample(r.opt.ProcRoot, r.m, func(p string) bool { return c.in(p) })
	}
	now := look()
	if busy := uvBusy(now); busy != "" {
		f.Action, f.Why = ActKeep, "waits for an install: "+busy
		return []Finding{f}, notes, nil
	}
	report := func(why string) ([]Finding, []string, error) {
		f.Action, f.class, f.Why = ActReport, "busy", "it waits: "+why
		return []Finding{f}, notes, nil
	}
	using := func(users string) string {
		return "these run from the cache or hold its lock unseen, and a prune deletes what they use (" + users + ")"
	}
	lockFile := filepath.Join(dir, ".lock")
	lock, locks := uvLock(r.opt.ProcRoot, lockFile)
	if users := uvUsers(now, r.opt.ProcRoot, c, locks.holders); users != "" {
		return report(using(users))
	}
	forced := len(locks.holders) > 0
	if forced {
		if why := locks.blocks(); why != "" {
			return report(why)
		}
		for _, pid := range sortedPIDs(locks.holders) {
			// a holder that is no run past its sync (uvBusy waited for those) may write the cache now: uv's lock is
			// all that keeps it whole
			if p := now.Table[pid]; !uvReads(p.Argv) {
				f.Action, f.Why = ActKeep, fmt.Sprintf("waits for an install: %s (pid %d)", strings.Join(p.Argv[:min(len(p.Argv), 4)], " "), pid)
				return []Finding{f}, notes, nil
			}
		}
		f.Detail += fmt.Sprintf("; %d uv processes hold its lock and none runs from the cache, so with --force under a lock "+
			"of capacity's own", len(locks.holders))
	}
	if why, err := r.uvContainers(ctx, c); err != nil || why != "" {
		if err != nil {
			why = "Docker could not be asked which containers mount the cache: " + err.Error()
		}
		return report(why)
	}
	f.Action = ActDelete
	if !live {
		return []Finding{f}, notes, nil
	}
	start := time.Now()
	if !forced {
		// uv's own lock decides: a process that started since the look keeps the cache, and the prune waits
		_, err = r.tools.UV(ctx, 20*time.Minute, "cache", "prune", "--ci")
		if err != nil && uvInUse(err) {
			return report("a uv process took the cache's lock since the look")
		}
	} else {
		held, why := uvLockInPlace(lockFile, lock, locks.holders, r.opt.ProcRoot, r.opt.afterUVLock)
		if held == nil {
			return report(why)
		}
		if users := uvUsers(look(), r.opt.ProcRoot, c, nil); users != "" {
			held.Close()
			return report(using(users))
		}
		_, err = r.tools.UV(ctx, 20*time.Minute, "cache", "prune", "--ci", "--force")
		held.Close() // a uv process that waited for it goes on
	}
	took := time.Since(start).Round(time.Second)
	after, counted2 := r.uvSize(ctx, dir)
	if size >= 0 && after >= 0 {
		f.Freed = max64(0, size-after)
	}
	// nothing went only when uv's own count is the same before and after: a walk, or a cache that grew meanwhile,
	// cannot tell
	f.none = counted && counted2 && after == size
	if err != nil {
		f.Err = err.Error()
		return []Finding{f}, notes, nil
	}
	f.Done = true
	f.Detail += fmt.Sprintf("; %s to %s in %s", gib(size), gib(after), took)
	return []Finding{f}, notes, r.st.SetMark(ctx, "prune:uv", r.now)
}

// uvCache is the cache's place as uv names it and with its links resolved (real), both canonical, its lock file, and
// how a path is given its canonical name (a bind mount's path is its filesystem's).
type uvCache struct {
	canon, real, lock string
	name              func(string) string
}

// in reports whether a path, as a process, its environment or /proc gives it, is in the cache, other than its lock
// file.
func (c uvCache) in(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	if c.name != nil {
		p = c.name(p)
	}
	return p != c.lock && (c.canon != "" && under(p, c.canon) || c.real != "" && under(p, c.real))
}

// uvInUse reports whether uv refused a prune because another uv process holds its cache.
func uvInUse(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "in-use") || strings.Contains(m, "in use") || strings.Contains(m, "timed out waiting") ||
		strings.Contains(m, "when waiting for lock")
}

// uvSize is the cache's size: uv's own count, else a walk.
func (r *runner) uvSize(ctx context.Context, dir string) (int64, bool) {
	if out, err := r.tools.UV(ctx, 2*time.Minute, "cache", "size", "--preview-features", "cache-size"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if n, err := strconv.ParseInt(strings.TrimSpace(l), 10, 64); err == nil {
				return n, true
			}
		}
	}
	if dir == "" {
		return -1, false
	}
	return Measure(dir, nil).Bytes, false
}

// uvCommands are uv's commands, which come after its global options (and their values) on its command line.
var uvCommands = map[string]bool{"run": true, "tool": true, "sync": true, "add": true, "remove": true, "lock": true,
	"venv": true, "build": true, "export": true, "init": true, "pip": true, "python": true, "tree": true, "format": true,
	"cache": true, "self": true, "version": true, "publish": true, "auth": true, "help": true}

// uvCommand is a uv process's command and the words after it (uvx's is tool uvx), or nil for a process that is not
// uv's: its program is uv or uvx, with a version after an @ or not (uv@0.13.5).
func uvCommand(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	exe, _, _ := strings.Cut(filepath.Base(argv[0]), "@")
	switch exe {
	case "uvx":
		return append([]string{"tool", "uvx"}, argv[1:]...)
	case "uv":
		for i, a := range argv[1:] {
			if uvCommands[a] {
				return argv[1+i:]
			}
		}
	}
	return nil
}

// subcommand is the first word after a command that is no option, or "".
func subcommand(cmd []string) string {
	for _, a := range cmd[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// uvReads reports whether a uv process is one that only reads the cache once it has started its command: a run, a
// tool run or a uvx (uvBusy waits for one that has not started it yet). Anything else (an install, a sync, a tree,
// a clean, a program that is not uv's) may write the cache.
func uvReads(argv []string) bool {
	cmd := uvCommand(argv)
	switch {
	case len(cmd) == 0:
		return false
	case cmd[0] == "run":
		return true
	case cmd[0] == "tool":
		sub := subcommand(cmd)
		return sub == "run" || sub == "uvx"
	}
	return false
}

// uvBusy names a uv process that writes the cache now, or "": an install, a sync, an add or remove, a lock, a venv,
// a tool or python install, a clean or a prune, or a run or uvx that has not started its command (it may still
// sync). One that only reads the cache, or lists what is there, is not.
func uvBusy(s *Sample) string {
	if s == nil {
		return ""
	}
	for _, pid := range sortedPIDs(s.Table) {
		p := s.Table[pid]
		cmd := uvCommand(p.Argv)
		if len(cmd) == 0 {
			continue
		}
		busy := false
		switch sub := subcommand(cmd); cmd[0] {
		case "sync", "add", "remove", "lock", "venv", "build", "export", "init":
			busy = true
		case "pip":
			busy = !contains([]string{"list", "show", "freeze", "check", "tree"}, sub)
		case "tool":
			busy = contains([]string{"install", "upgrade"}, sub) || (sub == "run" || sub == "uvx") && !s.Children(pid)
		case "python":
			busy = sub == "install"
		case "cache":
			busy = sub == "clean" || sub == "prune"
		case "run":
			busy = !s.Children(pid)
		}
		if busy {
			return fmt.Sprintf("%s (pid %d)", strings.Join(p.Argv[:min(len(p.Argv), 4)], " "), pid)
		}
	}
	return ""
}

// sortedPIDs are a map's process ids in order.
func sortedPIDs[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for pid := range m {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

// uvUsers names, at most three, the processes a prune would break, each by the uv process it runs under, and the
// holders of the lock the look cannot read. A process uses the cache by a path in it other than its lock file (a
// program, a mapped or open file, a working directory, an argument), by the program it was started as (a cached
// environment's bin/python is a link to an interpreter outside the cache), or by its environment: uv starts a
// command with the environment's bin first on PATH, and a script's with VIRTUAL_ENV, and the command's children
// inherit them. An environment counts as in the cache when it, with its links resolved, is there, or when its
// packages are links into it (uv's symlink link mode). A holder, or a process under one, whose open files or
// environment the run may not read is one the look cannot read. A container's mount of the cache, or of what holds
// it, is a use. "" is none.
func uvUsers(s *Sample, procRoot string, c uvCache, holders map[int]bool) string {
	if s == nil {
		return ""
	}
	seen := map[int]bool{}
	var names []string
	add := func(pid int, what string) {
		if !seen[pid] && len(names) < 3 {
			names = append(names, fmt.Sprintf("%s (pid %d)", what, pid))
		}
		seen[pid] = true
	}
	byUV := func(pid int, what string) { // the uv process it runs under names it best
		for p, hops := s.Table[pid], 0; hops < 8; hops++ {
			if len(uvCommand(p.Argv)) > 0 {
				pid, what = p.PID, strings.Join(p.Argv[:min(len(p.Argv), 4)], " ")
				break
			}
			q, ok := s.Table[p.PPID]
			if !ok {
				break
			}
			p = q
		}
		add(pid, what)
	}
	envIn := map[string]bool{} // an environment's place: whether it is in the cache or linked into it
	inEnv := func(p string) bool {
		if !filepath.IsAbs(p) {
			return false
		}
		if v, ok := envIn[p]; ok {
			return v
		}
		v := c.in(p)
		if !v {
			if real, err := filepath.EvalSymlinks(p); err == nil && c.in(real) {
				v = true
			} else {
				v = linksInto(p, c)
			}
		}
		envIn[p] = v
		return v
	}
	uses := append([]Use(nil), s.Uses...)
	sort.Slice(uses, func(i, j int) bool { return uses[i].PID < uses[j].PID })
	for _, u := range uses {
		if c.in(u.Path) {
			byUV(u.PID, u.Comm)
		}
	}
	for _, pid := range sortedPIDs(s.Table) {
		p := s.Table[pid]
		if seen[pid] {
			continue
		}
		if len(p.Argv) > 0 && filepath.IsAbs(p.Argv[0]) && c.in(p.Argv[0]) {
			byUV(pid, p.Comm)
			continue
		}
		for _, kv := range environ(procRoot, pid) {
			k, v, _ := strings.Cut(kv, "=")
			switch {
			case k == "VIRTUAL_ENV" && inEnv(v),
				k == "PATH" && slices.ContainsFunc(strings.Split(v, ":"), func(b string) bool { return inEnv(filepath.Dir(b)) && filepath.Base(b) == "bin" || c.in(b) }),
				k == "UV_LINK_MODE" && v == "symlink":
				byUV(pid, p.Comm)
			}
			if seen[pid] {
				break
			}
		}
	}
	for _, pid := range sortedPIDs(holders) {
		if _, ok := s.Table[pid]; !ok {
			add(pid, "a process the look cannot read holds the lock")
			continue
		}
		for _, q := range append([]int{pid}, descendants(s, pid)...) {
			if unreadable(procRoot, q) {
				add(q, "a process the look cannot read runs under a holder of the lock")
			}
		}
	}
	if len(seen) > len(names) {
		names = append(names, fmt.Sprintf("%d more", len(seen)-len(names)))
	}
	return strings.Join(names, ", ")
}

// descendants are the processes under one in a sample, at most 256.
func descendants(s *Sample, pid int) []int {
	kids := map[int][]int{}
	for _, q := range sortedPIDs(s.Table) {
		kids[s.Table[q].PPID] = append(kids[s.Table[q].PPID], q)
	}
	var out []int
	for todo := kids[pid]; len(todo) > 0 && len(out) < 256; {
		q := todo[0]
		todo = append(todo[1:], kids[q]...)
		out = append(out, q)
	}
	return out
}

// unreadable reports whether the run may not read a process's open files or environment: another user's (root's in
// a container, a uv run under sudo).
func unreadable(procRoot string, pid int) bool {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if _, err := os.ReadDir(filepath.Join(dir, "fd")); errors.Is(err, fs.ErrPermission) {
		return true
	}
	_, err := os.ReadFile(filepath.Join(dir, "environ"))
	return errors.Is(err, fs.ErrPermission)
}

// linksInto reports whether an environment's packages are links into the cache, as uv's symlink link mode makes
// them: an entry of its site-packages, or one inside such an entry.
func linksInto(env string, c uvCache) bool {
	sites, _ := filepath.Glob(filepath.Join(env, "lib", "python*", "site-packages"))
	for _, site := range sites {
		ents, _ := os.ReadDir(site)
		for i, e := range ents {
			if i >= 256 {
				break
			}
			p := filepath.Join(site, e.Name())
			if e.Type()&fs.ModeSymlink != 0 {
				if real, err := filepath.EvalSymlinks(p); err == nil && c.in(real) {
					return true
				}
				continue
			}
			if !e.IsDir() {
				continue
			}
			inner, _ := os.ReadDir(p)
			for j, f := range inner {
				if j >= 16 {
					break
				}
				if f.Type()&fs.ModeSymlink != 0 {
					if real, err := filepath.EvalSymlinks(filepath.Join(p, f.Name())); err == nil && c.in(real) {
						return true
					}
				}
			}
		}
	}
	return false
}

// uvContainers names a running container that mounts the cache, or a directory that holds it, or "": its processes
// are root's, which the look cannot read.
func (r *runner) uvContainers(ctx context.Context, c uvCache) (string, error) {
	ids, err := r.tools.Docker(ctx, 15*time.Second, "ps", "-q")
	if errors.Is(err, errNoTool) || (err != nil && dockerDown(err)) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ids) == "" {
		return "", nil
	}
	out, err := r.tools.Docker(ctx, 30*time.Second, append([]string{"inspect", "--format",
		"{{.Name}}{{range .Mounts}}\t{{.Source}}{{end}}"}, strings.Fields(ids)...)...)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "\t")
		for _, src := range f[1:] {
			if !filepath.IsAbs(src) {
				continue
			}
			if p := r.m.Canon(src); c.in(p) || under(c.canon, p) || under(c.real, p) {
				return fmt.Sprintf("container %s mounts %s", strings.TrimPrefix(f[0], "/"), short(src)), nil
			}
		}
	}
	return "", nil
}

// lockID is a file as /proc/locks names it: its device's major and minor numbers, and its inode.
type lockID struct {
	major, minor uint32
	ino          uint64
}

// fileLocks are the locks on a file, by /proc/locks: the processes that hold one, whether one holds it alone, and
// how many wait for it. known is false when /proc/locks cannot be read.
type fileLocks struct {
	holders   map[int]bool
	exclusive bool
	waiters   int
	known     bool
}

// blocks says why the locks on uv's lock file keep capacity from putting its own in place, or "": a process that
// waits for it (a clean or a prune, which would get the old file after the holders end, while new processes run on
// the new one), or one that holds it alone (uv's own clean or prune, at work now).
func (l fileLocks) blocks() string {
	switch {
	case l.waiters > 0:
		return "a uv process waits for the cache's lock (a clean or a prune), which it gets once the holders end"
	case l.exclusive:
		return "a uv process holds the cache's lock alone (a clean or a prune)"
	}
	return ""
}

// uvLock is the cache's lock file's identity and the locks on it. None is known when the file or /proc/locks cannot
// be read, and then only uv's own lock may decide.
func uvLock(procRoot, lockFile string) (lockID, fileLocks) {
	id, err := fileID(lockFile)
	if err != nil {
		return lockID{}, fileLocks{}
	}
	return id, lockHolders(procRoot, id)
}

// fileID is a file's identity as /proc/locks names it.
func fileID(p string) (lockID, error) {
	st, err := os.Stat(p)
	if err != nil {
		return lockID{}, err
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return lockID{}, fmt.Errorf("%s: no inode", p)
	}
	return lockID{unix.Major(uint64(s.Dev)), unix.Minor(uint64(s.Dev)), s.Ino}, nil
}

// lockHolders are the locks on a file, by /proc/locks ("1: FLOCK ADVISORY READ 4242 fc:00:1234 0 EOF"; a waiter's
// line has "->" after its number).
func lockHolders(procRoot string, id lockID) fileLocks {
	out := fileLocks{holders: map[int]bool{}}
	b, err := os.ReadFile(filepath.Join(procRoot, "locks"))
	if err != nil {
		return out
	}
	out.known = true
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		waiter := len(f) > 1 && f[1] == "->"
		if waiter {
			f = append(f[:1:1], f[2:]...)
		}
		if len(f) < 6 {
			continue
		}
		parts := strings.Split(f[5], ":")
		if len(parts) != 3 {
			continue
		}
		maj, err1 := strconv.ParseUint(parts[0], 16, 32)
		mnr, err2 := strconv.ParseUint(parts[1], 16, 32)
		ino, err3 := strconv.ParseUint(parts[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || (lockID{uint32(maj), uint32(mnr), ino}) != id {
			continue
		}
		if waiter {
			out.waiters++ // a process waiting for the lock holds nothing yet
			continue
		}
		if f[3] == "WRITE" {
			out.exclusive = true
		}
		if pid, err := strconv.Atoi(f[4]); err == nil {
			out.holders[pid] = true
		}
	}
	return out
}

// uvLockSettle is how long the rule waits after its lock file takes the old one's place before it looks for a uv
// process that opened the old file just then.
var uvLockSettle = 100 * time.Millisecond

// uvLockInPlace takes the cache's lock while processes hold it shared, none of which runs from the cache: a new
// file, locked exclusively, is renamed over the lock file. A uv process that starts opens it and waits, as it waits
// for uv's own prune, up to its UV_LOCK_TIMEOUT; the holders keep the old file, whose lock protects nothing the prune
// removes. It lets go, with why, when the lock file is not the one the look saw, or when after the rename a process
// it did not see before has the old file open (uv opens it, then locks it), holds a lock on it, or waits for it. The
// caller closes the file it returns, which lets the lock go.
func uvLockInPlace(lockFile string, old lockID, before map[int]bool, procRoot string, after func()) (*os.File, string) {
	if id, err := fileID(lockFile); err != nil || id != old {
		return nil, "the cache's lock file changed since the look"
	}
	st, err := os.Stat(lockFile)
	if err != nil {
		return nil, "the lock file cannot be read: " + err.Error()
	}
	tmp := lockFile + ".capacity"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err.Error()
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err.Error()
	}
	fail := func(why string) (*os.File, string) {
		f.Close()
		os.Remove(tmp)
		return nil, why
	}
	if err := f.Chmod(st.Mode().Perm()); err != nil {
		return fail(err.Error())
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail("its new lock file could not be locked: " + err.Error())
	}
	if err := os.Rename(tmp, lockFile); err != nil {
		return fail(err.Error())
	}
	if after != nil {
		after()
	}
	time.Sleep(uvLockSettle)
	letGo := func(why string) (*os.File, string) {
		f.Close()
		return nil, why
	}
	now := lockHolders(procRoot, old)
	if !now.known {
		return letGo("the locks cannot be read again")
	}
	for pid := range now.holders {
		if !before[pid] {
			return letGo(fmt.Sprintf("a uv process (pid %d) took the cache's lock as capacity put its own in place", pid))
		}
	}
	if why := now.blocks(); why != "" {
		return letGo(why)
	}
	// nothing can open the old file now; a process that opened it before, and has not locked it yet, may still
	for _, pid := range openers(procRoot, old) {
		if !before[pid] {
			return letGo(fmt.Sprintf("a uv process (pid %d) opened the cache's lock as capacity put its own in place", pid))
		}
	}
	return f, ""
}

// openers are the processes that have a file open, by their /proc fds.
func openers(procRoot string, id lockID) []int {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds := filepath.Join(procRoot, e.Name(), "fd")
		names, err := os.ReadDir(fds)
		if err != nil {
			continue
		}
		for _, n := range names {
			if got, err := fileID(filepath.Join(fds, n.Name())); err == nil && got == id {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// docker prunes Docker's build cache older than the configured age and its dangling images, once per cadence.
// Volumes are only reported: one may hold a database.
func (r *runner) docker(ctx context.Context, live bool) ([]Finding, []string, error) {
	if _, err := r.tools.Docker(ctx, 15*time.Second, "info", "--format", "{{.ServerVersion}}"); err != nil {
		if errors.Is(err, errNoTool) {
			return nil, []string{"docker is not installed"}, nil
		}
		return nil, []string{"docker is not reachable: " + firstLine(err.Error())}, nil
	}
	until := r.cfg.Docker.Until
	var out []Finding
	var notes []string
	// What the prune removes is counted record by record, on the builder the prune runs on: `docker builder` is
	// buildx with the context's builder, which `docker buildx` alone may not be. buildx du ignores an age filter, and
	// a count of every reclaimable record lists what the prune keeps. A size that cannot be read runs the prune
	// anyway: it is Docker's own, limited by age, and says what it freed.
	cache := Finding{Rule: RuleDocker, Path: "docker build cache", Action: ActDelete}
	sized := false
	if keep, err := time.ParseDuration(until); err == nil {
		prefix := "builder prune --filter until=" + until
		if du, err := r.tools.Docker(ctx, 2*time.Minute, "builder", "du", "--format", "json"); err != nil {
			cache.Detail = prefix + ": size unknown, `docker builder du` failed: " + firstLine(err.Error())
		} else if n, size, ok := prunable(du, keep); !ok {
			cache.Detail = prefix + ": size unknown, `docker builder du` printed no record this build reads"
		} else {
			sized = true
			cache.Bytes = size
			cache.Detail = fmt.Sprintf("%s: %d %s unused for %s", prefix, n, plural("record", n), until)
		}
	}
	images := Finding{Rule: RuleDocker, Path: "docker dangling images", Action: ActDelete, Detail: "image prune"}
	if ls, err := r.tools.Docker(ctx, time.Minute, "image", "ls", "--filter", "dangling=true", "--format", "{{.ID}}\t{{.Size}}"); err == nil {
		n := 0
		for _, l := range strings.Split(strings.TrimSpace(ls), "\n") {
			if f := strings.Split(l, "\t"); len(f) == 2 {
				n++
				images.Bytes += parseSize(f[1])
			}
		}
		images.Detail = fmt.Sprintf("image prune: %d dangling", n)
		if n == 0 {
			images.Action = ""
		}
	}
	if v, err := r.tools.Docker(ctx, time.Minute, "system", "df", "-v", "--format", "{{json .Volumes}}"); err == nil {
		if f, ok := unusedVolumes(v); ok {
			out = append(out, f)
		}
	}
	cache.unsized = !sized
	for _, f := range []*Finding{&cache, &images} {
		if f.Action == "" || (f.Bytes == 0 && (f != &cache || sized)) {
			continue
		}
		if live {
			args := []string{"builder", "prune", "--force", "--filter", "until=" + until}
			if f == &images {
				args = []string{"image", "prune", "--force"}
			}
			res, err := r.tools.Docker(ctx, 30*time.Minute, args...)
			if err != nil {
				f.Err = err.Error()
			} else {
				f.Done = true
				var said bool
				if f.Freed, said = parseSizeAfter(res, "Total:"); !said {
					f.Freed, said = parseSizeAfter(res, "Total reclaimed space:")
				}
				f.none = said && f.Freed == 0 && removedNothing(res)
				f.unsaid = !said
			}
		}
		out = append(out, *f)
	}
	if live {
		return out, notes, r.st.SetMark(ctx, "prune:docker", r.now)
	}
	return out, notes, nil
}

// prunable counts the build cache records a prune limited by age removes, from `docker builder du --format json`
// (one record per line), and whether it could read every line. A record goes when no build uses it, no image shares it, it is
// not BuildKit's own (internal and frontend records, which a prune without --all keeps), and it was last used
// longer ago than keep (a record never used goes), and when every record above it goes too: a child holds its
// parent, whose own last use stays old while the child is used. Docker prints the last use as a rough age.
func prunable(du string, keep time.Duration) (n int, size int64, ok bool) {
	type record struct {
		ID                     string
		Parents                []string
		Reclaimable, Shared    bool
		Type, Size, LastUsedAt string
	}
	var recs []record
	for _, l := range strings.Split(du, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var rec record
		if json.Unmarshal([]byte(l), &rec) != nil || rec.ID == "" {
			return 0, 0, false // a record this build cannot read may be the child that holds a parent
		}
		recs = append(recs, rec)
	}
	byID := map[string]*record{}
	for i := range recs {
		byID[recs[i].ID] = &recs[i]
	}
	kept := map[string]bool{}
	var hold func(id string)
	hold = func(id string) { // a kept record holds every record below it
		if kept[id] {
			return
		}
		kept[id] = true
		if rec := byID[id]; rec != nil {
			for _, p := range rec.Parents {
				hold(p)
			}
		}
	}
	for _, rec := range recs {
		goes := rec.Reclaimable && !rec.Shared && rec.Type != "internal" && rec.Type != "frontend"
		if goes && rec.LastUsedAt != "" {
			age, known := humanAge(rec.LastUsedAt)
			goes = known && age >= keep
		}
		if !goes {
			hold(rec.ID)
		}
	}
	for _, rec := range recs {
		if !kept[rec.ID] {
			n++
			size += parseSize(rec.Size)
		}
	}
	return n, size, true
}

var ageRe = regexp.MustCompile(`^(\d+) (second|minute|hour|day|week|month|year)s?$`)

// humanAge reads an age as Docker prints it (go-units' HumanDuration, with " ago") at the least it can be.
// Seconds, minutes and years are cut down to the unit; hours, days, weeks and months come from the hours rounded to
// the nearest, so "7 days" can be half an hour short of 7 days.
func humanAge(s string) (time.Duration, bool) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), " ago")
	switch s {
	case "less than a second":
		return 0, true
	case "about a minute":
		return time.Minute, true
	case "about an hour": // the hours round to 1, from a full hour of minutes on
		return time.Hour, true
	}
	m := ageRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	n, day, half := time.Duration(v), 24*time.Hour, 30*time.Minute
	switch m[2] {
	case "second":
		return n * time.Second, true
	case "minute":
		return n * time.Minute, true
	case "hour":
		return n*time.Hour - half, true
	case "day":
		return n*day - half, true
	case "week":
		return n*7*day - half, true
	case "month":
		return n*30*day - half, true
	}
	return n * 365 * day, true
}

// removedNothing reports whether a prune's output lists nothing it removed: no row under buildx's table header and
// no "Deleted" section (Docker's own form). A line this build does not know counts as something removed.
func removedNothing(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "", strings.HasPrefix(t, "Total"):
		case strings.HasPrefix(t, "ID") && strings.Contains(t, "RECLAIMABLE"):
		default:
			return false
		}
	}
	return true
}

// unusedVolumes is a report-only finding for the volumes no container uses, from `docker system df -v`'s JSON: a
// list of objects whose Links and Size are text.
func unusedVolumes(js string) (Finding, bool) {
	f := Finding{Rule: RuleDocker, Path: "docker volumes", Action: ActReport, class: "volumes"}
	var vols []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(js)), &vols); err != nil {
		return f, false
	}
	n := 0
	for _, v := range vols {
		if fmt.Sprint(v["Links"]) == "0" {
			n++
			f.Bytes += parseSize(fmt.Sprint(v["Size"]))
		}
	}
	if n == 0 {
		return f, false
	}
	f.Why = fmt.Sprintf("%d volumes no container uses; a volume may hold data, so they are only reported", n)
	return f, true
}

var sizeRe = regexp.MustCompile(`(?i)([0-9.]+)\s*([kmgtp]?i?b)\b`)

// parseSize reads a size as Docker prints it (1.785GB, 4.096kB, 512B), in bytes.
func parseSize(s string) int64 {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(m[2])
	mult := map[string]float64{"b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, "pb": 1e15,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40}[unit]
	return int64(v * mult)
}

// parseSizeAfter reads the size on the line that starts with a label, and whether there is such a line.
func parseSizeAfter(out, label string) (int64, bool) {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), label); ok {
			return parseSize(rest), true
		}
	}
	return 0, false
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return ls[len(ls)-1]
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// gib is a size in GiB, or MiB below one GiB.
func gib(n int64) string {
	if n < 0 {
		return "?"
	}
	if n < 1<<30 {
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

// environ is a process's environment, NAME=value each, or none when it cannot be read.
func environ(procRoot string, pid int) []string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}
