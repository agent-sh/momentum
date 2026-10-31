package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// environment outside the cache (a project's .venv, an installed tool's) is untouched, its files hardlinks of the
// cache's. Every uv process holds uv's lock shared for its life, so a prune that takes the lock waits for all of
// them. The rule looks first: it waits quietly for an install, a sync or a venv being made, which end soon, and
// waits for, and names, a process that uses a path in the cache, or a holder of the lock it cannot read. Otherwise
// it prunes: with uv's own lock when nobody holds it, or, when every holder runs outside the cache, with --force
// under a lock of its own put in the lock file's place (uvLockInPlace).
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
	// a look at the processes now, for the uv processes and the uses of the cache, which may be under no other root
	look := func() *Sample {
		return TakeSample(r.opt.ProcRoot, r.m, func(p string) bool { return under(p, f.canon) })
	}
	now := look()
	if busy := uvBusy(now); busy != "" {
		f.Action, f.Why = ActKeep, "waits for an install: "+busy
		return []Finding{f}, notes, nil
	}
	busyWhy := func(users string) string {
		return "it waits: these run from the cache or hold its lock unseen, and a prune deletes what they use (" + users + ")"
	}
	lockFile := filepath.Join(dir, ".lock")
	lock, holders := uvLock(r.opt.ProcRoot, lockFile)
	if users := uvUsers(now, r.opt.ProcRoot, f.canon, r.m.Canon(lockFile), r.m.Canon, holders); users != "" {
		f.Action, f.class, f.Why = ActReport, "busy", busyWhy(users)
		return []Finding{f}, notes, nil
	}
	f.Action = ActDelete
	forced := len(holders) > 0
	if forced {
		f.Detail += fmt.Sprintf("; %d uv processes hold its lock and none runs from the cache, so with --force under a lock "+
			"of capacity's own", len(holders))
	}
	if !live {
		return []Finding{f}, notes, nil
	}
	if !forced {
		// uv's own lock decides: a process that started since the look keeps the cache, and the prune waits
		_, err = r.tools.UV(ctx, 20*time.Minute, "cache", "prune", "--ci")
		if err != nil && uvInUse(err) {
			f.Action, f.class, f.Why = ActReport, "busy", "it waits: a uv process took the cache's lock since the look"
			return []Finding{f}, notes, nil
		}
	} else {
		held, why := uvLockInPlace(lockFile, lock, holders, r.opt.ProcRoot, r.opt.afterUVLock)
		if held == nil {
			f.Action, f.class, f.Why = ActReport, "busy", "it waits: "+why
			return []Finding{f}, notes, nil
		}
		if users := uvUsers(look(), r.opt.ProcRoot, f.canon, r.m.Canon(lockFile), r.m.Canon, nil); users != "" {
			held.Close()
			f.Action, f.class, f.Why = ActReport, "busy", busyWhy(users)
			return []Finding{f}, notes, nil
		}
		_, err = r.tools.UV(ctx, 20*time.Minute, "cache", "prune", "--ci", "--force")
		held.Close() // a uv process that waited for it goes on
	}
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
	f.Detail += fmt.Sprintf("; %s to %s", gib(size), gib(after))
	return []Finding{f}, notes, r.st.SetMark(ctx, "prune:uv", r.now)
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

// uvBusy names a uv process that installs into the cache now, or "". An install, a sync, an add or remove, a lock,
// a venv, a tool or python install. A `uv run` or `uv tool run` (uvx) that has started its command (it has a child)
// is past its sync and only reads the cache; one without a child may still be installing.
func uvBusy(s *Sample) string {
	if s == nil {
		return ""
	}
	for _, p := range s.Table {
		if len(p.Argv) == 0 {
			continue
		}
		exe := filepath.Base(p.Argv[0])
		var args []string
		for _, a := range p.Argv[1:] {
			if !strings.HasPrefix(a, "-") {
				args = append(args, a)
			}
		}
		sub := func(i int) string {
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		busy := false
		switch exe {
		case "uvx":
			busy = !s.Children(p.PID)
		case "uv":
			switch sub(0) {
			case "sync", "add", "remove", "lock", "venv", "build", "export", "init":
				busy = true
			case "pip":
				busy = !contains([]string{"list", "show", "freeze", "check", "tree"}, sub(1))
			case "tool":
				switch sub(1) {
				case "install", "upgrade":
					busy = true
				case "run":
					busy = !s.Children(p.PID)
				}
			case "python":
				busy = sub(1) == "install"
			case "run":
				busy = !s.Children(p.PID)
			}
		}
		if busy {
			return fmt.Sprintf("%s (pid %d)", strings.Join(p.Argv[:min(len(p.Argv), 4)], " "), p.PID)
		}
	}
	return ""
}

// uvUsers names, at most three, the processes a prune would break, each by the uv process it runs under, and the
// holders of the lock the look did not read. A process uses the cache by a path in it other than its lock file (a
// program, a mapped or open file, a working directory, an argument), by the program it was started as (a cached
// environment's bin/python is a link to an interpreter outside the cache), or by its environment: uv starts a
// command with the environment's bin first on PATH, and a script's with VIRTUAL_ENV, and the command's children
// inherit them. "" is none.
func uvUsers(s *Sample, procRoot, cache, lock string, canon func(string) string, holders map[int]bool) string {
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
			if len(p.Argv) > 0 && (filepath.Base(p.Argv[0]) == "uv" || filepath.Base(p.Argv[0]) == "uvx") {
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
	in := func(p string) bool { return filepath.IsAbs(p) && under(canon(p), cache) }
	uses := append([]Use(nil), s.Uses...)
	sort.Slice(uses, func(i, j int) bool { return uses[i].PID < uses[j].PID })
	for _, u := range uses {
		if cache != "" && under(u.Path, cache) && u.Path != lock {
			byUV(u.PID, u.Comm)
		}
	}
	pids := make([]int, 0, len(s.Table))
	for pid := range s.Table {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		p := s.Table[pid]
		if cache == "" || seen[pid] {
			continue
		}
		if len(p.Argv) > 0 && in(p.Argv[0]) {
			byUV(pid, p.Comm)
			continue
		}
		for _, kv := range environ(procRoot, pid) {
			k, v, _ := strings.Cut(kv, "=")
			if (k == "VIRTUAL_ENV" && in(v)) || (k == "PATH" && slices.ContainsFunc(strings.Split(v, ":"), in)) {
				byUV(pid, p.Comm)
				break
			}
		}
	}
	held := make([]int, 0, len(holders))
	for pid := range holders {
		held = append(held, pid)
	}
	sort.Ints(held)
	for _, pid := range held {
		if _, ok := s.Table[pid]; !ok {
			add(pid, "a process the look cannot read holds the lock")
		}
	}
	if len(seen) > len(names) {
		names = append(names, fmt.Sprintf("%d more", len(seen)-len(names)))
	}
	return strings.Join(names, ", ")
}

// lockID is a file as /proc/locks names it: its device's major and minor numbers, and its inode.
type lockID struct {
	major, minor uint32
	ino          uint64
}

// uvLock is the cache's lock file's identity and the processes that hold a lock on it, from /proc/locks. None is
// known when the file or /proc/locks cannot be read, and then only uv's own lock may decide. A process waiting for
// the lock holds nothing.
func uvLock(procRoot, lockFile string) (lockID, map[int]bool) {
	st, err := os.Stat(lockFile)
	if err != nil {
		return lockID{}, nil
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return lockID{}, nil
	}
	id := lockID{unix.Major(uint64(s.Dev)), unix.Minor(uint64(s.Dev)), s.Ino}
	holders, _ := lockHolders(procRoot, id)
	return id, holders
}

// lockHolders are the processes that hold a lock on a file, by /proc/locks ("1: FLOCK ADVISORY READ 4242
// fc:00:1234 0 EOF"; a waiter's line has "->" after its number).
func lockHolders(procRoot string, id lockID) (map[int]bool, bool) {
	out := map[int]bool{}
	b, err := os.ReadFile(filepath.Join(procRoot, "locks"))
	if err != nil {
		return out, false
	}
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
			continue // a process waiting for the lock holds nothing
		}
		if pid, err := strconv.Atoi(f[4]); err == nil {
			out[pid] = true
		}
	}
	return out, true
}

// uvLockSettle is how long the rule waits after its lock file takes the old one's place before it looks for a uv
// process that opened the old file just then: uv locks the file the moment it opens it.
var uvLockSettle = 100 * time.Millisecond

// uvLockInPlace takes the cache's lock while processes hold it shared, none of which runs from the cache: a new
// file, locked exclusively, is renamed over the lock file. A uv process that starts opens it and waits, as it waits
// for uv's own prune; the holders keep the old file, whose lock protects nothing the prune removes. A uv process
// that took the old file meanwhile (a holder not seen before) makes it let go, with why. The caller closes the file
// it returns, which lets the lock go.
func uvLockInPlace(lockFile string, old lockID, before map[int]bool, procRoot string, after func()) (*os.File, string) {
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
	now, known := lockHolders(procRoot, old)
	for pid := range now {
		if !before[pid] {
			f.Close()
			return nil, fmt.Sprintf("a uv process (pid %d) took the cache's lock as capacity put its own in place", pid)
		}
	}
	if !known {
		f.Close()
		return nil, "the locks cannot be read again"
	}
	return f, ""
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
