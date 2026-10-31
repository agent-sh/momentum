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
	return t.UVWith(ctx, timeout, nil, args...)
}

// UVWith is UV with open files uv inherits, from fd 3 on.
func (t execTools) UVWith(ctx context.Context, timeout time.Duration, files []*os.File, args ...string) (string, error) {
	return runToolWith(ctx, timeout, t.uv, "uv", files, append([]string{"UV_LOCK_TIMEOUT=10"}, args...)...)
}

func (t execTools) Docker(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return runTool(ctx, timeout, t.docker, "docker", args...)
}

func runTool(ctx context.Context, timeout time.Duration, path, name string, args ...string) (string, error) {
	return runToolWith(ctx, timeout, path, name, nil, args...)
}

// runToolWith is runTool with open files the tool inherits, from fd 3 on.
func runToolWith(ctx context.Context, timeout time.Duration, path, name string, files []*os.File, args ...string) (string, error) {
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
	cmd.ExtraFiles = files
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
// that runs outside the cache, with --force under a lock of its own put in the lock file's place (lockInPlace).
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
	c := uvCache{canon: f.canon, real: r.m.resolve(dir), name: r.m.Canon}
	c.lock, c.lockReal = r.m.Canon(filepath.Join(dir, ".lock")), filepath.Join(c.real, ".lock")
	look := func() *Sample {
		return TakeSample(r.opt.ProcRoot, r.m, func(p string) bool { return c.in(p) })
	}
	keep := func(why string) ([]Finding, []string, error) {
		f.Action, f.Why = ActKeep, "waits for an install: "+why
		return []Finding{f}, notes, nil
	}
	report := func(why string) ([]Finding, []string, error) {
		f.Action, f.class, f.Why = ActReport, "busy", "it waits: "+why
		return []Finding{f}, notes, nil
	}
	using := func(users string) string {
		return "these run from the cache or hold its lock unseen, and a prune deletes what they use (" + users + ")"
	}
	// the locks are read before the look, so a holder the look does not show has ended or is one it cannot read
	lk := uvLock(r.opt.ProcRoot, filepath.Join(dir, ".lock"), c.lockReal, r.m)
	now := look()
	lk.locks.ended(now, r.opt.ProcRoot)
	if busy := uvBusy(now, r.opt.ProcRoot); busy != "" {
		return keep(busy)
	}
	envs := &uvEnvs{in: map[string]bool{}, resolve: true}
	if users := uvUsers(now, r.opt.ProcRoot, c, lk.locks.holders, envs); users != "" {
		return report(using(users))
	}
	forced := len(lk.locks.holders) > 0
	if forced {
		if why := lk.locks.blocks(); why != "" {
			return report(why)
		}
		for _, pid := range sortedPIDs(lk.locks.holders) {
			// a holder that is no run past its sync (uvBusy waited for those) may write the cache now: uv's lock is
			// all that keeps it whole
			if p := now.Table[pid]; !uvReads(p.Argv) {
				return keep(fmt.Sprintf("%s (pid %d)", argvHead(p.Argv), pid))
			}
		}
		f.Detail += fmt.Sprintf("; %d uv processes hold its lock and none runs from the cache, so with --force under a lock "+
			"of capacity's own", len(lk.locks.holders))
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
		err = r.uvPrune(ctx, nil)
		if err != nil && uvInUse(err) {
			return report("a uv process took the cache's lock since the look")
		}
	} else {
		cmds := map[int]Proc{} // the command each holder started, as the look saw it
		for pid := range lk.locks.holders {
			cmds[pid], _ = uvStarted(now, r.opt.ProcRoot, pid)
		}
		held, why := uvSwap{lk: lk, look: now, procRoot: r.opt.ProcRoot, c: c, after: r.opt.afterUVLock}.lockInPlace()
		if held == nil {
			return report(why)
		}
		// the look under capacity's lock reads /proc, and of other filesystems only what the first look read: one that
		// does not answer would keep every uv process that starts waiting
		envs.resolve = false
		again := look()
		if why := uvSameCommands(now, again, r.opt.ProcRoot, cmds); why != "" {
			held.Close()
			return keep(why)
		}
		if users := uvUsers(again, r.opt.ProcRoot, c, lk.locks.holders, envs); users != "" {
			held.Close()
			return report(using(users))
		}
		err = r.uvPrune(ctx, held, "--force")
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
		f.Detail += fmt.Sprintf("; it failed after %s", took)
		return []Finding{f}, notes, nil
	}
	f.Done = true
	f.Detail += fmt.Sprintf("; %s to %s in %s", gib(size), gib(after), took)
	return []Finding{f}, notes, r.st.SetMark(ctx, "prune:uv", r.now)
}

// uvHolder is a Tools whose uv can be handed open files: the forced prune gets the file that holds capacity's lock,
// so the lock lasts while either of them runs, and a run that dies does not let it go under the prune. The prune
// opens the lock file on its own, finds it locked and, with --force, goes on.
type uvHolder interface {
	UVWith(ctx context.Context, timeout time.Duration, files []*os.File, args ...string) (string, error)
}

// uvPrune runs `uv cache prune --ci` with these arguments, handing it held when there is one and the tools can.
func (r *runner) uvPrune(ctx context.Context, held *os.File, args ...string) error {
	args = append([]string{"cache", "prune", "--ci"}, args...)
	if h, ok := r.tools.(uvHolder); ok && held != nil {
		_, err := h.UVWith(ctx, 20*time.Minute, []*os.File{held}, args...)
		return err
	}
	_, err := r.tools.UV(ctx, 20*time.Minute, args...)
	return err
}

// uvCache is the cache's place as uv names it and with its links resolved (real), both canonical, its lock file in
// both spellings, and how a path is given its canonical name (a bind mount's path is its filesystem's).
type uvCache struct {
	canon, real, lock, lockReal string
	name                        func(string) string
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
	return p != c.lock && p != c.lockReal && (c.canon != "" && under(p, c.canon) || c.real != "" && under(p, c.real))
}

// isLock reports whether a path, as /proc gives it, names the cache's lock file.
func (c uvCache) isLock(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	if c.name != nil {
		p = c.name(p)
	}
	return p == c.lock || p == c.lockReal
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

// uvCommands are uv's commands, hidden ones and aliases among them, which come after its global options (and their
// values) on its command line.
var uvCommands = map[string]bool{"run": true, "tool": true, "sync": true, "add": true, "remove": true, "lock": true,
	"upgrade": true, "venv": true, "v": true, "virtualenv": true, "build": true, "export": true, "init": true, "pip": true,
	"python": true, "tree": true, "format": true, "check": true, "audit": true, "workspace": true, "cache": true,
	"clean": true, "self": true, "version": true, "publish": true, "auth": true, "build-backend": true,
	"generate-shell-completion": true, "help": true}

// uvProgram is which of uv's programs a process runs, "uv" or "uvx", by its file name: the name alone, or with a
// suffix after it that starts with an @ or a version (uv@0.13.5, uvx-0.13: uvx runs the uv of its own suffix). ""
// is another program (uvicorn, uv-build).
func uvProgram(argv0 string) string {
	base := filepath.Base(argv0)
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	for _, name := range []string{"uvx", "uv"} {
		rest, ok := strings.CutPrefix(base, name)
		if ok && (rest == "" || rest[0] == '@' || digit(rest[0]) ||
			len(rest) > 1 && strings.IndexByte("-._", rest[0]) >= 0 && digit(rest[1])) {
			return name
		}
	}
	return ""
}

// uvCommand is a uv process's command and the words after it (uvx's is tool uvx), or nil for a process that is not
// uv's.
func uvCommand(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	switch uvProgram(argv[0]) {
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

// argvHead is a command line's first words, enough to name it.
func argvHead(argv []string) string {
	return strings.Join(argv[:min(len(argv), 4)], " ")
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

// uvBusy names a uv process that writes the cache now, or "": an install, a sync, an add or remove, a lock or an
// upgrade, a venv, a tool or python install or upgrade, a clean or a prune, or a run or uvx that has not started its
// command (it may still sync; see uvStarted). One that only reads the cache, or lists what is there, is not.
func uvBusy(s *Sample, procRoot string) string {
	if s == nil {
		return ""
	}
	started := func(pid int) bool {
		_, ok := uvStarted(s, procRoot, pid)
		return ok
	}
	for _, pid := range sortedPIDs(s.Table) {
		p := s.Table[pid]
		cmd := uvCommand(p.Argv)
		if len(cmd) == 0 {
			continue
		}
		busy := false
		switch sub := subcommand(cmd); cmd[0] {
		case "sync", "add", "remove", "lock", "upgrade", "venv", "v", "virtualenv", "build", "export", "init", "clean":
			busy = true
		case "pip":
			busy = !contains([]string{"list", "show", "freeze", "check", "tree"}, sub)
		case "tool":
			busy = contains([]string{"install", "upgrade", "update"}, sub) || (sub == "run" || sub == "uvx") && !started(pid)
		case "python":
			busy = sub == "install" || sub == "upgrade"
		case "cache":
			busy = sub == "clean" || sub == "clear" || sub == "prune"
		case "run":
			busy = !started(pid)
		}
		if busy {
			return fmt.Sprintf("%s (pid %d)", argvHead(p.Argv), pid)
		}
	}
	return ""
}

// uvStarted is the command a uv run, tool run or uvx started: a child with the environment uv gives its command (see
// uvGave), or false. A child the run may read no environment of, or whose uv's it may not read, counts as one: the
// look cannot tell, and uvUsers names a holder's such child as one it cannot read.
func uvStarted(s *Sample, procRoot string, pid int) (Proc, bool) {
	own, err := readEnviron(procRoot, pid)
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		return Proc{}, false // it ended
	}
	ownRead := err == nil
	for _, q := range sortedPIDs(s.Table) {
		p := s.Table[q]
		if p.PPID != pid {
			continue
		}
		env, err := readEnviron(procRoot, q)
		if errors.Is(err, fs.ErrPermission) || err == nil && (!ownRead || uvGave(env, own)) {
			return p, true
		}
	}
	return Proc{}, false
}

// uvGave reports whether a process's environment is the one uv gives the command it starts: uv run and uv tool run
// put the bin of one environment or more first on uv's own PATH, and uv run counts UV_RUN_RECURSION_DEPTH up. A
// helper uv runs before (the libc probe, keyring, git, nvidia-smi) gets uv's environment as it is.
func uvGave(env, own []string) bool {
	if d, ok := envValue(env, "UV_RUN_RECURSION_DEPTH"); ok {
		if o, _ := envValue(own, "UV_RUN_RECURSION_DEPTH"); d != o {
			return true
		}
	}
	path, ok := envValue(env, "PATH")
	if !ok {
		return false
	}
	head := path
	if o, ok := envValue(own, "PATH"); ok {
		if head, ok = strings.CutSuffix(path, ":"+o); !ok {
			return false
		}
	}
	for _, b := range strings.Split(head, ":") {
		if filepath.Base(b) != "bin" {
			return false
		}
	}
	return true
}

// envValue is a variable's value in an environment, the first if it is there twice, as getenv gives it.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// uvSameCommands names a holder of the lock that still runs, but not the command the first look saw it start, or "".
func uvSameCommands(first, again *Sample, procRoot string, cmds map[int]Proc) string {
	for _, pid := range sortedPIDs(cmds) {
		h, ok := again.Table[pid]
		if !ok || h.Start != first.Table[pid].Start {
			continue // it ended, and its lock went with it
		}
		if p, ok := uvStarted(again, procRoot, pid); !ok || p.PID != cmds[pid].PID || p.Start != cmds[pid].Start {
			return fmt.Sprintf("%s (pid %d), whose command is not the one the look saw", argvHead(h.Argv), pid)
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

// uvEnvs are the answers, by place, to whether an environment is in the cache or linked into it, kept across the
// rule's looks. Without resolve, a place no look answered is judged by its name alone: the look under capacity's lock
// reads no filesystem but /proc.
type uvEnvs struct {
	in      map[string]bool
	resolve bool
}

// has reports whether an environment is in the cache or linked into it: its place is there, its place with its
// links resolved is there, or its packages are links into it (uv's symlink link mode, which a system Python's
// packages may have too).
func (e *uvEnvs) has(p string, c uvCache) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	if v, ok := e.in[p]; ok {
		return v
	}
	v := c.in(p)
	if !v && !e.resolve {
		return false
	}
	if !v {
		real, err := resolveLinks(p)
		v = err == nil && c.in(real) || linksInto(p, c)
	}
	e.in[p] = v
	return v
}

// resolveLinks resolves an environment's links: a test's way to see which places the rule reads.
var resolveLinks = filepath.EvalSymlinks

// uvUsers names, at most three, the processes a prune would break, each by the uv process it runs under, and the
// holders of the lock the look cannot read. A process uses the cache by a path in it other than its lock file (a
// program, a mapped or open file, a working directory, an argument), by the program it was started as (a cached
// environment's bin/python is a link to an interpreter outside the cache), or by its environment: uv starts a
// command with the environment's bin first on PATH, and a script's with VIRTUAL_ENV, and the command's children
// inherit them. An environment counts as in the cache when it, with its links resolved, is there, or when its
// packages are links into it (uv's symlink link mode). A holder, or a process under one, whose open files or
// environment the run may not read is one the look cannot read; a holder that ended since the locks were read is
// none. A container's mount of the cache, or of what holds it, is a use. "" is none.
func uvUsers(s *Sample, procRoot string, c uvCache, holders map[int]bool, envs *uvEnvs) string {
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
				pid, what = p.PID, argvHead(p.Argv)
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
			case k == "VIRTUAL_ENV" && envs.has(v, c),
				k == "PATH" && slices.ContainsFunc(strings.Split(v, ":"), func(b string) bool {
					return c.in(b) || filepath.Base(b) == "bin" && envs.has(filepath.Dir(b), c)
				}),
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
			if _, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid))); !errors.Is(err, fs.ErrNotExist) {
				add(pid, "a process the look cannot read holds the lock")
			}
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
// them: an entry of its site-packages, or one inside such an entry (uv makes a package's directories and links its
// files).
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

// fileLocks are the locks on a file, by /proc/locks: the processes that hold one, those of them that hold it alone,
// and how many wait for it. known is false when /proc/locks cannot be read.
type fileLocks struct {
	holders, writers map[int]bool
	waiters          int
	known            bool
}

// blocks says why the locks on uv's lock file keep capacity from putting its own in place, or "": a process that
// waits for it (a clean or a prune, which would get the old file after the holders end, while new processes run on
// the new one), or one that holds it alone (uv's own clean or prune, at work now).
func (l fileLocks) blocks() string {
	switch {
	case l.waiters > 0:
		return "a uv process waits for the cache's lock (a clean or a prune), which it gets once the holders end"
	case len(l.writers) > 0:
		return "a uv process holds the cache's lock alone (a clean or a prune)"
	}
	return ""
}

// ended drops the holders that ended since the locks were read: the look (when there is one) does not show them,
// and /proc has them no more. Their locks went with them.
func (l fileLocks) ended(s *Sample, procRoot string) {
	for pid := range l.holders {
		if _, ok := s.table()[pid]; ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid))); errors.Is(err, fs.ErrNotExist) {
			delete(l.holders, pid)
			delete(l.writers, pid)
		}
	}
}

// uvLockState is uv's lock file at the look: its path, its identity by stat, the names /proc/locks may give it, and
// the locks on it.
type uvLockState struct {
	file  string
	id    lockID
	keys  []lockID
	locks fileLocks
}

// uvLock reads uv's lock file and the locks on it; real is its place with its links resolved. None is known when the
// file or /proc/locks cannot be read, and then only uv's own lock may decide. /proc/locks names a file by its
// filesystem's device, which mountinfo gives too; stat gives the same one but on btrfs, whose stat gives every
// subvolume a device of its own, so both are names of the file.
func uvLock(procRoot, file, real string, m *Mounts) uvLockState {
	l := uvLockState{file: file, locks: fileLocks{holders: map[int]bool{}, writers: map[int]bool{}}}
	id, err := fileID(file)
	if err != nil {
		return l
	}
	l.id, l.keys = id, []lockID{id}
	if maj, mnr, ok := parseDev(m.devOf(real)); ok && (maj != id.major || mnr != id.minor) {
		l.keys = append(l.keys, lockID{maj, mnr, id.ino})
	}
	l.locks = lockHolders(procRoot, l.keys)
	return l
}

// parseDev reads mountinfo's "major:minor".
func parseDev(s string) (uint32, uint32, bool) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, false
	}
	maj, err1 := strconv.ParseUint(a, 10, 32)
	mnr, err2 := strconv.ParseUint(b, 10, 32)
	return uint32(maj), uint32(mnr), ok && err1 == nil && err2 == nil
}

// fileID is a file's identity as /proc/locks names it, by stat.
func fileID(p string) (lockID, error) {
	st, err := statLock(p)
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
// line has "->" after its number), under any of its names.
func lockHolders(procRoot string, keys []lockID) fileLocks {
	out := fileLocks{holders: map[int]bool{}, writers: map[int]bool{}}
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
		if err1 != nil || err2 != nil || err3 != nil || !slices.Contains(keys, lockID{uint32(maj), uint32(mnr), ino}) {
			continue
		}
		if waiter {
			out.waiters++ // a process waiting for the lock holds nothing yet
			continue
		}
		pid, err := strconv.Atoi(f[4])
		if err != nil {
			continue
		}
		out.holders[pid] = true
		if f[3] == "WRITE" {
			out.writers[pid] = true
		}
	}
	return out
}

// statLock stats a lock file, or a /proc fd that may be one: a test's way to see which files the rule stats.
var statLock = os.Stat

// uvLockSettle is how long the rule waits after its lock file takes the old one's place before it looks for a uv
// process that opened the old file just then.
var uvLockSettle = 100 * time.Millisecond

// uvSwap puts capacity's lock in uv's lock file's place while processes hold the old file shared, none of which
// runs from the cache: a new file, locked exclusively, is renamed over the lock file. A uv process that starts opens
// the new file and waits, as it waits for uv's own prune, up to its UV_LOCK_TIMEOUT; the holders keep the old file,
// whose lock protects nothing the prune removes.
type uvSwap struct {
	lk       uvLockState
	look     *Sample // the processes as the rule's look saw them
	procRoot string
	c        uvCache
	after    func() // a test's way into the moment after the rename
}

// lockInPlace returns the file that holds capacity's lock, or nil and why it let go. It gives up without renaming
// when the lock file is not the one the look saw, or when, with capacity's lock taken on the new file, the old file
// has a holder the look did not see, a process that waits for it or holds it alone, or one that has it open and was
// no holder (uv opens the file, then locks it). It looks the same way after the rename and lets go on what it finds
// then, which only keeps its own prune from running: the new file stays, and a process that came to the old file in
// the instant between the last look and the rename keeps a lock no later clean or prune waits for. The caller closes
// the file it returns, which lets the lock go.
func (s uvSwap) lockInPlace() (*os.File, string) {
	lockFile := s.lk.file
	if id, err := fileID(lockFile); err != nil || id != s.lk.id {
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
	if why := s.check(false, "since the look"); why != "" {
		return fail(why)
	}
	if err := os.Rename(tmp, lockFile); err != nil {
		return fail(err.Error())
	}
	if s.after != nil {
		s.after()
	}
	time.Sleep(uvLockSettle)
	if why := s.check(true, "as capacity put its own in place"); why != "" {
		f.Close()
		return nil, why
	}
	return f, ""
}

// check says why the old lock file keeps capacity's lock out of its place, or "": a holder the look did not see, a
// process that waits for it or holds it alone, a process that has it open and was no holder, or a uv process the
// look did not see whose open files the run may not read. renamed is whether the file was renamed over.
func (s uvSwap) check(renamed bool, when string) string {
	now := lockHolders(s.procRoot, s.lk.keys)
	if !now.known {
		return "the locks cannot be read again"
	}
	now.ended(nil, s.procRoot)
	for _, pid := range sortedPIDs(now.holders) {
		if !s.lk.locks.holders[pid] {
			return fmt.Sprintf("a uv process (pid %d) took the cache's lock %s", pid, when)
		}
	}
	if why := now.blocks(); why != "" {
		return why
	}
	open, unseen := openers(s.procRoot, s.lk.id, s.c.isLock, renamed, s.look)
	for _, pid := range open {
		if !s.lk.locks.holders[pid] {
			return fmt.Sprintf("a process (pid %d) opened the cache's lock %s", pid, when)
		}
	}
	if len(unseen) > 0 {
		return fmt.Sprintf("a uv process (pid %d) that started %s may have the cache's lock open; the run may not read "+
			"its open files", unseen[0], when)
	}
	return ""
}

// openers are the processes that have the lock file open, by their /proc fds: a link /proc names as the lock file
// (with " (deleted)" after it once the file was renamed over) whose file is id. Only such a link is stat'ed: a stat
// goes into the file's filesystem, and one that does not answer would hold the look, while a readlink reads /proc
// alone. unseen are the uv processes the look did not see whose open files the run may not read: nothing says one
// has not opened the file.
func openers(procRoot string, id lockID, isLock func(string) bool, renamed bool, look *Sample) (open, unseen []int) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, nil
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		names, err := os.ReadDir(filepath.Join(dir, "fd"))
		if errors.Is(err, fs.ErrPermission) {
			if newUV(look, dir, pid) {
				unseen = append(unseen, pid)
			}
			continue
		}
		for _, n := range names {
			fd := filepath.Join(dir, "fd", n.Name())
			l, err := os.Readlink(fd)
			if err != nil {
				continue
			}
			p, deleted := strings.CutSuffix(l, " (deleted)")
			if deleted != renamed || !isLock(p) {
				continue
			}
			if got, err := fileID(fd); err == nil && got == id {
				open = append(open, pid)
				break
			}
		}
	}
	return open, unseen
}

// newUV reports whether a process is a uv process the look did not see, by its command line and start.
func newUV(look *Sample, dir string, pid int) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(raw) == 0 || uvProgram(strings.SplitN(string(raw), "\x00", 2)[0]) == "" {
		return false
	}
	st, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return false
	}
	p, ok := look.Table[pid]
	return !ok || p.Start != startOf(string(st))
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
	env, _ := readEnviron(procRoot, pid)
	return env
}

// readEnviron is a process's environment, or why it cannot be read: fs.ErrPermission for another user's, or a
// program it runs set-user-ID, fs.ErrNotExist for one that ended.
func readEnviron(procRoot string, pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), nil
}
