package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/signal"
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

// The uv and docker rules prune with each tool's own command: the tool knows its cache. The uv rule deletes entries of
// uv's cache itself only around what uv's prune cannot keep: processes that run from it, places scheduled jobs name in
// it, and overlays a command may run in unseen (uvaround.go).

// Tools runs uv and docker.
type Tools interface {
	UV(ctx context.Context, timeout time.Duration, args ...string) (string, error)
	Docker(ctx context.Context, timeout time.Duration, args ...string) (string, error)
}

// errNoTool means the tool is not installed.
var errNoTool = errors.New("not installed")

type execTools struct{ uv, docker string }

// UV runs uv with a short wait on its cache lock: a prune that another uv process blocks fails at once instead of
// waiting its default 5 minutes. Its output is what uv wrote to stderr (its report, such as what a prune removed),
// then what it wrote to stdout (an answer, such as the cache's place).
func (t execTools) UV(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return t.UVWith(ctx, timeout, nil, args...)
}

// UVWith is UV with open files uv inherits, from fd 3 on.
func (t execTools) UVWith(ctx context.Context, timeout time.Duration, files []*os.File, args ...string) (string, error) {
	out, report, err := runToolBoth(ctx, timeout, t.uv, "uv", files, append([]string{"UV_LOCK_TIMEOUT=10"}, args...)...)
	if report != "" && err == nil {
		out = strings.TrimRight(report, "\n") + "\n" + out
	}
	return out, err
}

func (t execTools) Docker(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return runTool(ctx, timeout, t.docker, "docker", args...)
}

func runTool(ctx context.Context, timeout time.Duration, path, name string, args ...string) (string, error) {
	return runToolWith(ctx, timeout, path, name, nil, args...)
}

// runToolWith is runTool with open files the tool inherits, from fd 3 on.
func runToolWith(ctx context.Context, timeout time.Duration, path, name string, files []*os.File, args ...string) (string, error) {
	out, _, err := runToolBoth(ctx, timeout, path, name, files, args...)
	return out, err
}

// runToolBoth is runToolWith that gives what the tool wrote to stderr too.
func runToolBoth(ctx context.Context, timeout time.Duration, path, name string, files []*os.File, args ...string) (string, string, error) {
	if path == "" {
		p, err := exec.LookPath(name)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", name, errNoTool)
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
		return out.String(), errb.String(), fmt.Errorf("%s %s: %w after %s", name, strings.Join(args[:min(len(args), 2)], " "),
			errTimeout, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 { // its head says what failed, its tail where
			msg = msg[:300] + " ... " + msg[len(msg)-300:]
		}
		return out.String(), errb.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args[:min(len(args), 2)], " "), err, msg)
	}
	return out.String(), errb.String(), nil
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
// container that mounts the cache, a holder of the lock it cannot read, a process that waits for the lock or holds it
// alone (uv's own clean or prune), and a process whose use of the cache it cannot tell. Otherwise it prunes: with
// uv's own lock when nobody holds it, or, when every holder is a uv process past its sync that runs outside the
// cache, with --force under a lock of its own put in the lock file's place (lockInPlace). When processes run from
// the cache (a path in it, an environment in it or linked into it), a scheduled job names a place in one of its
// environments, or builds-v0 holds an overlay a command may run in unseen (uvBuildsOf), it prunes around them instead
// (uvReachOf, uvRemove), under uv's lock or its own in the lock file's place, the old file put back after (restore).
func (r *runner) uv(ctx context.Context, live bool) ([]Finding, []string, error) {
	out, err := r.tools.UV(ctx, 30*time.Second, "cache", "dir")
	if errors.Is(err, errNoTool) {
		return nil, []string{"uv is not installed"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dir := strings.TrimSpace(lastLine(out))
	var trashed string
	if live {
		// what an earlier prune around running environments moved aside and did not delete: nothing reaches it
		if err := uvEmptyTrash(dir); err != nil {
			trashed = "what an earlier prune moved aside could not be deleted: " + err.Error()
		}
	}
	size, counted := r.uvSize(ctx, dir)
	last, err := r.mark(ctx, "prune:uv")
	if err != nil {
		return nil, nil, err
	}
	max := int64(r.cfg.UV.MaxGiB * (1 << 30))
	every := time.Duration(r.cfg.UV.EveryDays * 24 * float64(time.Hour))
	notes := []string{fmt.Sprintf("cache %s at %s", gib(size), short(dir))}
	if trashed != "" {
		notes = append(notes, trashed)
	}
	f := Finding{Rule: RuleUV, Path: dir, canon: r.m.Canon(dir), Bytes: size}
	// the cache as uv spells it and as the kernel does: /proc shows paths with their links resolved
	c := uvCache{canon: f.canon, real: r.m.resolve(dir), name: r.m.Canon, named: filepath.Join(dir, ".lock"), dir: dir}
	c.lock, c.lockReal = r.m.Canon(c.named), filepath.Join(c.real, ".lock")
	if p, err := filepath.EvalSymlinks(dir); err == nil {
		c.dir = p
	}
	look := func() *Sample {
		return TakeSample(r.opt.ProcRoot, r.m, func(p string) bool { return c.in(p) })
	}
	report := func(why string) ([]Finding, []string, error) {
		f.Action, f.class, f.Why = ActReport, "busy", "it waits: "+why
		return []Finding{f}, notes, nil
	}
	// an old lock file a killed run set aside goes back at every scan of the rule live, a prune due or not, and one
	// that may not prune (the jobs could not all be read): until it does, uv's own clean or prune does not wait for
	// the processes that hold it. Putting it back deletes nothing.
	if why := uvPutBack(r.opt.ProcRoot, c, r.m, !r.opt.DryRun && r.cfg.IsLive(RuleUV)); why != "" {
		return report(why)
	}
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
	hides := r.m.procHides(r.opt.ProcRoot)
	envs := &uvEnvs{in: map[string]bool{}, ns: map[string]bool{}, resolve: true, hidden: hides == procInvisible, hides: hides != procShown}
	using := func(users string) string {
		why := "these run from the cache or hold its lock unseen, and a prune deletes what they use (" + users + ")"
		if len(envs.slow) > 0 {
			why += fmt.Sprintf("; %s did not answer in %s, so what is there is not known", short(envs.slow[0]), uvPlaceWait)
		}
		if len(envs.unread) > 0 {
			why += fmt.Sprintf("; %s came after the lock went in, and is not read under it", short(envs.unread[0]))
		}
		return why
	}
	// the locks are read before the look, so a holder the look does not show has ended or is one it cannot read
	lk := uvLock(r.opt.ProcRoot, c.named, c.lockReal, r.m, c.isLock)
	up, now := uptimeTicks(r.opt.ProcRoot), look()
	// a quiet wait for one uv process that has gone on for a day is reported: no install takes that long, and the
	// rule cannot tell what such a process runs (a command that changed its environment, a stuck sync)
	keep := func(pid int, why string) ([]Finding, []string, error) {
		if p, ok := now.Table[pid]; ok && up > 0 && p.Start > 0 && up-p.Start >= int64(uvQuietMax.Seconds())*userHZ {
			return report(fmt.Sprintf("%s, which has run for %s, looks like a uv process that may write the cache, and the "+
				"rule cannot tell what it runs", why, (time.Duration(up-p.Start) * time.Second / userHZ).Round(time.Hour)))
		}
		f.Action, f.Why = ActKeep, "waits for an install: "+why
		return []Finding{f}, notes, nil
	}
	lk.locks.ended(now, r.opt.ProcRoot, envs.hidden)
	if pid, busy := uvBusy(now, r.opt.ProcRoot); busy != "" {
		return keep(pid, busy)
	}
	// processes that run from the cache: uv's prune would delete what they use, so the rule prunes around it, when the
	// look can tell what that is. So it does around what a scheduled job names in the cache, as every rule keeps what a
	// job names, and around an overlay of builds-v0 a command may run in that names nothing (uvBuildsOf).
	found := uvUsers(now, r.opt.ProcRoot, c, lk.locks.holders, envs)
	if r.named != nil {
		jobs := map[string]bool{}
		for _, p := range sortedKeys(r.named.Paths) {
			if q, ok := uvJobPlace(c, resolveLinks, p); ok {
				found.places = append(found.places, uvPlace{path: q, who: "the job " + r.named.Paths[p]})
				jobs[r.named.Paths[p]] = true
			}
		}
		found.jobs = len(jobs)
	}
	builds := uvBuildsOf(c, now, r.opt.ProcRoot)
	var around *uvReach
	if found.names != "" || found.jobs > 0 || len(builds.live) > 0 || builds.blind {
		switch {
		case found.blind != "":
			return report("the look cannot tell what these use of the cache, and a prune may delete it (" + found.blind + ")")
		case len(envs.slow) > 0:
			return report(using(found.names))
		}
		if around = uvReachOf(c, found, builds); around.why != "" {
			return report(around.why)
		}
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
				return keep(pid, fmt.Sprintf("%s (pid %d)", argvHead(p.Argv), pid))
			}
		}
		holders := fmt.Sprintf("%d uv processes hold", len(lk.locks.holders))
		if len(lk.locks.holders) == 1 {
			holders = "1 uv process holds"
		}
		if around == nil {
			f.Detail += "; " + holders + " its lock and none runs from the cache, so with --force under a lock of capacity's own"
		} else {
			f.Detail += "; " + holders + " its lock, so under a lock of capacity's own"
		}
	}
	if why, err := r.uvContainers(ctx, c); err != nil || why != "" {
		if err != nil {
			why = "Docker could not be asked which containers mount the cache: " + err.Error()
		}
		return report(why)
	}
	if around != nil {
		f.Detail += "; " + around.says(found)
	}
	f.Action = ActDelete
	if !live {
		return []Finding{f}, notes, nil
	}
	start := time.Now()
	var removal uvRemoval
	var restored, printed string
	if !forced && around == nil {
		// uv's own lock decides: a process that started since the look keeps the cache, and the prune waits
		printed, err = r.uvPrune(ctx, nil)
		if err != nil && uvInUse(err) {
			return report("a uv process took the cache's lock since the look")
		}
	} else {
		cmds := map[int]Proc{} // the command each holder started, as the look saw it
		for pid := range lk.locks.holders {
			cmds[pid], _ = uvStarted(now, r.opt.ProcRoot, pid)
		}
		// a look at the processes: each holder that still runs runs the command the first look saw, and none uses the
		// cache, or, around running environments, none uses what the first look did not keep and builds-v0 holds
		// nothing it did not see
		type verdict struct {
			pid              int
			keep, why, users string
		}
		lookAgain := func() verdict {
			again := look()
			if pid, why := uvSameCommands(now, again, r.opt.ProcRoot, cmds); why != "" {
				return verdict{pid: pid, keep: why}
			}
			found := uvUsers(again, r.opt.ProcRoot, c, lk.locks.holders, envs)
			if around == nil {
				return verdict{users: found.names}
			}
			if users := around.outside(found, envs); users != "" {
				return verdict{users: users}
			}
			return verdict{why: around.newBuilds()}
		}
		answer := func(v verdict) ([]Finding, []string, error) {
			switch {
			case v.why != "":
				return report(v.why)
			case v.keep != "":
				return keep(v.pid, v.keep)
			}
			return report(using(v.users))
		}
		var held *os.File
		swap := uvSwap{lk: lk, look: now, procRoot: r.opt.ProcRoot, c: c, hidden: envs.hidden, after: r.opt.afterUVLock,
			stat: statLock}
		var hold *stopHold
		if forced {
			// it looks before the rename, so what it finds keeps the lock file in place, every holder on it. Around
			// running environments the old file is set aside to be put back (restore): its holders may run from the
			// cache, and a stop waits until it is back. uv's own prune would delete it there, and its holders run
			// outside the cache.
			var before verdict
			var why string
			held, hold, why = swap.lockInPlace(func() bool {
				before = lookAgain()
				return before.keep == "" && before.users == "" && before.why == ""
			}, around != nil)
			if before.keep != "" || before.users != "" || before.why != "" {
				return answer(before)
			}
			if held == nil {
				return report(why)
			}
		} else {
			// nobody holds uv's lock: capacity takes it, as uv's own prune does, and a uv process that starts waits
			var why string
			if held, why = uvTakeLock(lk.file, lk.id); held == nil {
				return report(why)
			}
			if r.opt.afterUVLock != nil {
				r.opt.afterUVLock()
			}
		}
		lockedAt := time.Now()
		// and after it, under capacity's lock, for what came in the moments between: that look reads /proc, and of
		// other filesystems only the lock files /proc names; a place or a namespace no look read counts as in use. A
		// process stuck on a filesystem that does not answer can still hold up a read of its /proc files, so the look
		// has a deadline too.
		envs.resolve = false
		done := make(chan verdict, 1)
		go func() {
			if forced {
				if why := swap.afterRename(); why != "" {
					done <- verdict{why: why}
					return
				}
			}
			done <- lookAgain()
		}()
		var v verdict
		select {
		case v = <-done:
		case <-time.After(uvUnderLockWait):
			v.why = fmt.Sprintf("the look under capacity's lock did not end in %s: a process's files in /proc do not answer", uvUnderLockWait)
		}
		// let capacity's lock go: the old lock file back in its place, when it took the lock file's place
		letGo := func() {
			if forced && around != nil {
				restored = swap.restore(held, hold)
			} else {
				hold.end()
			}
			held.Close() // a uv process that waited for it goes on
		}
		if v.why != "" || v.keep != "" || v.users != "" {
			letGo()
			got, n, e := answer(v)
			if restored != "" && len(got) > 0 {
				got[0].Why += "; " + restored
			}
			return got, n, e
		}
		if around == nil {
			printed, err = r.uvPrune(ctx, held, "--force")
		} else {
			// what a job names was a place of the walk; an entry that holds a mount point stays, as with every rule
			stay := func(p string) string {
				if mp, ok := r.m.mountWithin(p, r.m.Canon(p)); ok {
					return "it holds the mount point " + short(mp)
				}
				return ""
			}
			removal = uvRemove(c, around, lockedAt.Add(uvAroundFor), hold.came, stay)
		}
		letGo()
		if around != nil {
			// what went is what was moved aside, the files of files-v0 and the links it removed: what it frees counts
			// as every deletion's does, so a wheel an environment outside the cache still links frees nothing
			freed, terr := countFreed(uvTrashed(c.dir), func() error { return uvEmptyTrash(c.dir) })
			if terr != nil {
				removal.trash = terr.Error()
			}
			removal.freed += freed
			if removal.removed == 0 && len(removal.failures) > 0 {
				err = errors.New(removal.failures[0])
			}
		}
	}
	took := time.Since(start).Round(time.Second)
	after, counted2 := r.uvSize(ctx, dir)
	if size >= 0 && after >= 0 {
		f.Out = max64(0, size-after)
	}
	switch {
	case around != nil:
		f.Freed = removal.freed
	case err == nil: // uv's own prune: what uv says it freed, as uv's
		var said bool
		f.Freed, said, f.AtLeast = uvReclaimed(printed)
		f.By, f.Unsaid = "uv", !said
	}
	// nothing went only when uv's own count is the same before and after: a walk, or a cache that grew meanwhile,
	// cannot tell
	f.none = counted && counted2 && after == size
	// an entry the prune around running environments could not remove is a failed deletion of its own
	var failed []Finding
	for _, x := range removal.failures {
		rel, why, _ := strings.Cut(x, ": ")
		p := filepath.Join(dir, rel)
		failed = append(failed, Finding{Rule: RuleUV, Path: p, canon: r.m.Canon(p), Action: ActDelete, Err: why})
	}
	if restored != "" {
		f.Detail += "; " + restored
	}
	if err != nil {
		f.Err = err.Error()
		f.Detail += fmt.Sprintf("; it failed after %s", took)
		return append([]Finding{f}, failed[min(1, len(failed)):]...), notes, nil
	}
	f.Done = true
	if around != nil {
		f.Detail += "; " + removal.says()
	}
	switch {
	case around != nil:
		f.Detail += fmt.Sprintf("; it freed %s, as it counts what went: a file still linked from outside it stays", gib(f.Freed))
	case f.Unsaid:
		f.Detail += "; uv printed no total of what it freed"
	case f.AtLeast:
		f.Detail += fmt.Sprintf("; uv says it freed at least %s", gib(f.Freed))
	default:
		f.Detail += fmt.Sprintf("; uv says it freed %s", gib(f.Freed))
	}
	f.Detail += fmt.Sprintf("; %s to %s in %s", gib(size), gib(after), took)
	if around != nil && after >= 0 {
		f.Detail += fmt.Sprintf(", of which %s is in builds-v0 and the buckets it leaves to uv's own prune",
			gib(max64(0, after-(around.bytes-around.builds))))
	}
	return append([]Finding{f}, failed...), notes, r.st.SetMark(ctx, "prune:uv", r.now)
}

// uvUnderLockWait is how long the look under capacity's lock may take before the rule lets the lock go: long beside
// what a look at every process takes, and short beside the UV_LOCK_TIMEOUT a new uv process waits.
var uvUnderLockWait = 10 * time.Second

// uvQuietMax is how long the rule waits quietly for one uv process that may write the cache before it reports it: a
// day, longer than any install (an extension built from source takes hours).
var uvQuietMax = 24 * time.Hour

// uvHolder is a Tools whose uv can be handed open files: the forced prune gets the file that holds capacity's lock,
// so the lock lasts while either of them runs, and a run that dies does not let it go under the prune. The prune
// opens the lock file on its own, finds it locked and, with --force, goes on.
type uvHolder interface {
	UVWith(ctx context.Context, timeout time.Duration, files []*os.File, args ...string) (string, error)
}

// uvPrune runs `uv cache prune --ci` with these arguments, handing it held when there is one and the tools can, and
// gives what it printed.
func (r *runner) uvPrune(ctx context.Context, held *os.File, args ...string) (string, error) {
	args = append([]string{"cache", "prune", "--ci"}, args...)
	if h, ok := r.tools.(uvHolder); ok && held != nil {
		return h.UVWith(ctx, 20*time.Minute, []*os.File{held}, args...)
	}
	return r.tools.UV(ctx, 20*time.Minute, args...)
}

// uvRemovedRe is the summary uv's prune prints: "Removed 412 files (48.0KiB)", or "(at least 1.0MiB)" where uv could
// not measure every entry.
var uvRemovedRe = regexp.MustCompile(`Removed \d+ (?:files?|director(?:y|ies)) \((at least )?(\d+(?:\.\d+)?) ?([KMGTPE]iB|B)\)`)

// uvReclaimed is what uv's prune says it freed, by its report, and whether uv said that is at least what it freed:
// since 0.12.6 uv counts a file as it removes it, when that removes its last link. A prune that found nothing to
// remove freed nothing; a report this build cannot read says nothing.
func uvReclaimed(report string) (n int64, said, atLeast bool) {
	report = ansiRe.ReplaceAllString(report, "")
	m := uvRemovedRe.FindStringSubmatch(report)
	if m == nil {
		return 0, strings.Contains(report, "No unused entries found"), false
	}
	v, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return 0, false, false
	}
	unit := map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50, "EiB": 1 << 60}
	return int64(v * unit[m[3]]), true, m[1] != ""
}

// ansiRe is a terminal's color sequence, which a tool may print around a figure.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// uvCache is the cache's place as uv names it and with its links resolved (real), both canonical, its lock file in
// both spellings and as uv names it, and how a path is given its canonical name (a bind mount's path is its
// filesystem's). dir is where the run reads and deletes in it: the place as uv names it, its links resolved.
type uvCache struct {
	canon, real, lock, lockReal, named, dir string
	name                                    func(string) string
}

// in reports whether a path, as a process, its environment or /proc gives it, is in the cache, other than its lock
// file and the one capacity puts in its place (the run itself has it open while it looks).
func (c uvCache) in(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	if c.name != nil {
		p = c.name(p)
	}
	if p == c.lock || p == c.lockReal || p == c.lock+".capacity" || p == c.lockReal+".capacity" {
		return false
	}
	return c.canon != "" && under(p, c.canon) || c.real != "" && under(p, c.real)
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
	return Measure(dir).Bytes, false
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
func uvBusy(s *Sample, procRoot string) (int, string) {
	if s == nil {
		return 0, ""
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
			return pid, fmt.Sprintf("%s (pid %d)", argvHead(p.Argv), pid)
		}
	}
	return 0, ""
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
	cmd := uvCommand(s.Table[pid].Argv)
	run := len(cmd) > 0 && cmd[0] == "run"
	for _, q := range sortedPIDs(s.Table) {
		p := s.Table[q]
		if p.PPID != pid {
			continue
		}
		env, err := readEnviron(procRoot, q)
		if errors.Is(err, fs.ErrPermission) || err == nil && (!ownRead || uvGave(env, own, run)) {
			return p, true
		}
	}
	return Proc{}, false
}

// uvGave reports whether a process's environment is the one uv gives the command it starts. uv run counts
// UV_RUN_RECURSION_DEPTH up for it, and uv tool run puts the bin of its environment first on uv's own PATH. A helper
// uv runs on the way (the libc probe, keyring, git, nvidia-smi) gets uv's environment as it is, and a build backend
// gets an environment's bin first on uv's PATH too, with UV_INTERNAL__BUILD_DIR. One with no PATH where uv has one is
// the command, its environment since replaced (a command that set its process title over it, an env -i): no helper
// or backend runs without it.
func uvGave(env, own []string, run bool) bool {
	if b, ok := envValue(env, "UV_INTERNAL__BUILD_DIR"); ok {
		if o, _ := envValue(own, "UV_INTERNAL__BUILD_DIR"); b != o {
			return false
		}
	}
	path, ok := envValue(env, "PATH")
	if !ok {
		_, had := envValue(own, "PATH")
		return had
	}
	if run {
		d, _ := envValue(env, "UV_RUN_RECURSION_DEPTH")
		o, _ := envValue(own, "UV_RUN_RECURSION_DEPTH")
		return d != o
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
func uvSameCommands(first, again *Sample, procRoot string, cmds map[int]Proc) (int, string) {
	for _, pid := range sortedPIDs(cmds) {
		h, ok := again.Table[pid]
		if !ok || h.Start != first.Table[pid].Start {
			continue // it ended, and its lock went with it
		}
		if p, ok := uvStarted(again, procRoot, pid); !ok || p.PID != cmds[pid].PID || p.Start != cmds[pid].Start {
			return pid, fmt.Sprintf("%s (pid %d), whose command is not the one the look saw", argvHead(h.Argv), pid)
		}
	}
	return 0, ""
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

// uvEnvs are the answers, by place, to whether an environment is in the cache or linked into it, and, by mount
// namespace, to whether a process in it sees the cache's lock file where the run does; both are kept across the
// rule's looks. Without resolve, a place or namespace no look answered counts as in use without being read: the look
// under capacity's lock reads no filesystem but /proc. slow are the places that did not answer in uvPlaceWait,
// unread those that look did not read.
type uvEnvs struct {
	in, ns       map[string]bool
	resolve      bool
	slow, unread []string
	hidden       bool // /proc shows no directory for other users' processes (hidepid=invisible)
	hides        bool // /proc hides other users' processes or what is in their directories (any hidepid)
}

// uvPlaceWait is how long the look waits for one place to be resolved and read; a place on a filesystem that does not
// answer (a dead network or FUSE mount) counts as one in use. On local disks a place takes a few milliseconds.
var uvPlaceWait = 2 * time.Second

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
		e.unread = append(e.unread, p)
		return true
	}
	if !v {
		answer := make(chan bool, 1)
		resolve := resolveLinks // read here: a place that does not answer leaves the read behind
		go func() {
			real, err := resolve(p)
			answer <- err == nil && c.in(real) || linksInto(p, c)
		}()
		select {
		case v = <-answer:
		case <-time.After(uvPlaceWait):
			v = true
			e.slow = append(e.slow, p)
		}
	}
	e.in[p] = v
	return v
}

// sameView reports whether a process in another mount namespace sees the cache's lock file at a path the run knows
// it by: then the paths /proc gives for it name the cache as the run's do. Without resolve, a namespace no look
// answered does not.
func (e *uvEnvs) sameView(procRoot string, pid int, ns string, c uvCache) bool {
	if v, ok := e.ns[ns]; ok || !e.resolve {
		return v
	}
	answer := make(chan bool, 1)
	stat := statView // read here: a view that does not answer leaves the read behind
	go func() {
		lock, err := os.Stat(c.lockReal)
		v := false
		for _, p := range []string{c.named, c.lockReal} {
			if st, err2 := stat(filepath.Join(procRoot, strconv.Itoa(pid), "root", p)); err == nil && err2 == nil && os.SameFile(lock, st) {
				v = true
			}
		}
		answer <- v
	}()
	v := false
	select {
	case v = <-answer:
	case <-time.After(uvPlaceWait):
	}
	e.ns[ns] = v
	return v
}

// resolveLinks resolves an environment's links, and statView stats a path in another process's view: tests' ways to
// see which places the rule reads.
var (
	resolveLinks = filepath.EvalSymlinks
	statView     = os.Stat
)

// uvFound is what a look found of the processes that use the cache. names names them as a note does: at most three,
// each by the uv process it runs under, then how many more. places are what they use of it, each with the name of its
// process. blind names those the look cannot tell what they use of it by: a holder of the lock it cannot read, a
// process under one it may not read or that /proc hides, one in another mount namespace with another view of the
// cache, and one with UV_LINK_MODE=symlink (its packages may be links into the cache wherever its environment is).
// users is how many processes it names, each by the uv process it runs under; jobs is how many of the places a
// scheduled job names (the rule adds those).
type uvFound struct {
	names, blind string
	users, jobs  int
	places       []uvPlace
}

// uvPlace is a place in the cache a process uses, as it gives it (a path /proc shows, the root of its environment),
// and the process, named as a note names it; a place the walk found has no process. resolved is whether the kernel
// gave the path with its links resolved (an open or mapped file, a working directory, a program), unlike a path on a
// command line or in an environment.
type uvPlace struct {
	path, who string
	resolved  bool
}

// uvUsers finds the processes a prune would break, each named by the uv process it runs under, and the holders of the
// lock the look cannot read. A process uses the cache by a path in it other than its lock file (a program, a mapped or
// open file, a working directory, an argument), by the program it was started as (a cached environment's bin/python
// is a link to an interpreter outside the cache), or by its environment: uv starts a command with the environment's
// bin first on PATH, and a script's with VIRTUAL_ENV, and the command's children inherit them; PYTHONPATH may name a
// place in the cache too. An environment counts as in the cache when it, with its links resolved, is there, or when
// its packages are links into it (uv's symlink link mode). A holder, or a process under one, whose open files or
// environment the run may not read is one the look cannot read; a holder that ended since the locks were read is
// none.
func uvUsers(s *Sample, procRoot string, c uvCache, holders map[int]bool, envs *uvEnvs) uvFound {
	var out uvFound
	if s == nil {
		return out
	}
	all, blind := &pidNames{}, &pidNames{}
	byUV := func(pid int, what string) (int, string) { // the uv process it runs under names it best
		for p, hops := s.Table[pid], 0; hops < 8; hops++ {
			if len(uvCommand(p.Argv)) > 0 {
				return p.PID, argvHead(p.Argv)
			}
			q, ok := s.Table[p.PPID]
			if !ok {
				break
			}
			p = q
		}
		return pid, what
	}
	use := func(pid int, what, place string, resolved bool) {
		pid, what = byUV(pid, what)
		all.add(pid, what)
		out.places = append(out.places, uvPlace{place, fmt.Sprintf("%s (pid %d)", what, pid), resolved})
	}
	cannot := func(pid int, what string) {
		all.add(pid, what)
		blind.add(pid, what)
	}
	uses := append([]Use(nil), s.Uses...)
	sort.Slice(uses, func(i, j int) bool { return uses[i].PID < uses[j].PID })
	for _, u := range uses {
		if c.in(u.Path) {
			use(u.PID, u.Comm, u.Path, u.How != "arg")
		}
	}
	for _, pid := range sortedPIDs(s.Table) {
		p := s.Table[pid]
		if len(p.Argv) > 0 && filepath.IsAbs(p.Argv[0]) {
			switch a := p.Argv[0]; {
			case c.in(a):
				use(pid, p.Comm, a, false)
			case strings.HasPrefix(filepath.Base(a), "python") && filepath.Base(filepath.Dir(a)) == "bin" &&
				envs.has(filepath.Dir(filepath.Dir(a)), c):
				// started as an environment's interpreter, by a name outside the cache: a project's .venv that links
				// into it, or whose packages do
				use(pid, p.Comm, filepath.Dir(filepath.Dir(a)), false)
			}
		}
		for _, kv := range environ(procRoot, pid) {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "VIRTUAL_ENV":
				if envs.has(v, c) {
					use(pid, p.Comm, v, false)
				}
			case "PATH", "PYTHONPATH":
				for _, b := range strings.Split(v, ":") {
					switch {
					case c.in(b):
						use(pid, p.Comm, b, false)
					case k == "PATH" && filepath.Base(b) == "bin" && envs.has(filepath.Dir(b), c):
						use(pid, p.Comm, filepath.Dir(b), false)
					case k == "PYTHONPATH" && filepath.IsAbs(b) && envs.has(b, c):
						use(pid, p.Comm, b, false)
					}
				}
			case "UV_LINK_MODE":
				if v == "symlink" {
					cannot(byUV(pid, p.Comm))
				}
			}
		}
	}
	own := mountNS(procRoot, os.Getpid())
	for _, pid := range sortedPIDs(holders) {
		h, ok := s.Table[pid]
		if !ok {
			if _, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid))); envs.hidden || !errors.Is(err, fs.ErrNotExist) {
				cannot(pid, "a process the look cannot read holds the lock")
			}
			continue
		}
		layers := uvLayers(h.Argv)
		for _, q := range append([]int{pid}, descendants(s, pid)...) {
			switch {
			case unreadable(procRoot, q):
				cannot(q, "a process the look cannot read runs under a holder of the lock")
			case envs.hides && hiddenChild(s, procRoot, q):
				cannot(q, "a process /proc hides runs under this one, under a holder of the lock")
			case q != pid && layers && noPath(procRoot, q):
				// its imports come from the cache's layer with no file of it open or mapped, and its environment, which
				// would name the layer, is gone (setproctitle wrote over it). The overlay it runs in names the layer,
				// and a prune around it keeps that overlay and what it names (uvBuildsOf).
				all.add(q, "a command whose environment holds no PATH runs under a uv run that layers one of the cache's environments")
			default:
				if ns := mountNS(procRoot, q); own != "" && ns != "" && ns != own && !envs.sameView(procRoot, q, ns, c) {
					cannot(q, "a process that sees the cache by other paths (another mount namespace) runs under a holder of the lock")
				}
			}
		}
	}
	out.names, out.blind, out.users = all.String(), blind.String(), len(all.seen)
	return out
}

// uvLayers reports whether a uv process is a run that layers an environment of the cache over its command's (--with,
// -w, --with-editable, --with-requirements): the command then imports from the cache with no file of it open or
// mapped, while what it maps may come from the environment beneath. It reads uv's options up to uv's command, the
// first word that is no option or an option's value; every word after it is the command's own (gunicorn's -w,
// Celery's --without-gossip). An option uvRunFlags does not know is read as one that takes a value, so a miss reads
// on into the command's words, which only holds a prune.
func uvLayers(argv []string) bool {
	cmd := uvCommand(argv)
	if len(cmd) == 0 || cmd[0] != "run" {
		return false
	}
	for i := 1; i < len(cmd); i++ {
		switch a := cmd[i]; {
		case a == "--" || a == "-" || !strings.HasPrefix(a, "-"):
			return false // the command (- is a script on stdin)
		case strings.HasPrefix(a, "--"):
			name, _, valued := strings.Cut(a, "=")
			if name == "--with" || name == "--with-editable" || name == "--with-requirements" {
				return true
			}
			if !valued && !uvRunFlags[name] {
				i++ // its value
			}
		default: // short options, one or more: -qw six, -qwsix
			for j := 1; j < len(a); j++ {
				o := "-" + a[j:j+1]
				if o == "-w" {
					return true
				}
				if !uvRunFlags[o] { // it takes the rest of the word as its value, or the next word
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return false
}

// uvRunFlags are the options of uv run, and uv's global ones, that take no value, in uv 0.12 and 0.13, as uv's own
// command line definitions have them.
var uvRunFlags = map[string]bool{
	"--active": true, "--all-extras": true, "--all-groups": true, "--all-packages": true,
	"--allow-python-downloads": true, "--binary": true, "--build": true, "--build-isolation": true,
	"--compile": true, "--compile-bytecode": true, "--dev": true, "--editable": true, "--exact": true,
	"--force-reinstall": true, "--frozen": true, "--gui-script": true, "--help": true, "--inexact": true,
	"--isolated": true, "--locked": true, "--managed-python": true, "--module": true, "--native-tls": true,
	"--no-active": true, "--no-all-extras": true, "--no-binary": true, "--no-build": true,
	"--no-build-isolation": true, "--no-cache": true, "--no-cache-dir": true, "--no-color": true,
	"--no-compile": true, "--no-compile-bytecode": true, "--no-config": true, "--no-default-groups": true,
	"--no-dev": true, "--no-editable": true, "--no-env-file": true, "--no-exact": true, "--no-frozen": true,
	"--no-index": true, "--no-installer-metadata": true, "--no-locked": true, "--no-managed-python": true,
	"--no-native-tls": true, "--no-offline": true, "--no-preview": true, "--no-progress": true, "--no-project": true,
	"--no-python-downloads": true, "--no-refresh": true, "--no-reinstall": true, "--no-require-build-hashes": true,
	"--no-sources": true, "--no-sync": true, "--no-system-certs": true, "--no-upgrade": true, "--no_workspace": true,
	"--offline": true, "--only-dev": true, "--pre": true, "--preview": true, "--quiet": true, "--refresh": true,
	"--reinstall": true, "--require-build-hashes": true, "--script": true, "--show-resolution": true,
	"--show-settings": true, "--system-certs": true, "--upgrade": true, "--verbose": true, "--version": true,
	"-U": true, "-V": true, "-h": true, "-m": true, "-n": true, "-q": true, "-s": true, "-v": true,
}

// noPath reports whether a process's environment, as /proc reads it, holds no PATH: what uvGave counts a command by
// whose environment was replaced (setproctitle wrote its title over it, maybe with an = in it, or an env -i).
func noPath(procRoot string, pid int) bool {
	env, err := readEnviron(procRoot, pid)
	_, ok := envValue(env, "PATH")
	return err == nil && !ok
}

// hiddenChild reports whether a process has a child the look does not have: another user's, which a /proc mounted with
// hidepid hides or leaves unreadable. The kernel lists every child in task/TID/children whatever hidepid hides; a
// process without those files (a kernel without them) may have such a child too.
func hiddenChild(s *Sample, procRoot string, pid int) bool {
	files, _ := filepath.Glob(filepath.Join(procRoot, strconv.Itoa(pid), "task", "*", "children"))
	if len(files) == 0 {
		return true
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, w := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(w); err == nil {
				if _, ok := s.Table[c]; !ok {
					return true
				}
			}
		}
	}
	return false
}

// mountNS is a process's mount namespace, as /proc names it, or "".
func mountNS(procRoot string, pid int) string {
	l, _ := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "ns", "mnt"))
	return l
}

// descendants are the processes under one in a sample, each once.
func descendants(s *Sample, pid int) []int {
	kids := map[int][]int{}
	for _, q := range sortedPIDs(s.Table) {
		kids[s.Table[q].PPID] = append(kids[s.Table[q].PPID], q)
	}
	seen := map[int]bool{pid: true}
	var out []int
	for todo := kids[pid]; len(todo) > 0; {
		q := todo[0]
		todo = todo[1:]
		if seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
		todo = append(todo, kids[q]...)
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

// uvContainers names a running Docker container that mounts the cache, a directory in it or one that holds it, or "":
// its processes are root's, which the look cannot read.
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
// how many wait for it and which (waiting), and those of them that wait to hold it alone. known is false when
// /proc/locks cannot be read.
type fileLocks struct {
	holders, writers, waiting map[int]bool
	waiters                   int
	alone                     []int
	known                     bool
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
// and /proc has them no more. Their locks went with them. When /proc hides other users' processes, one it does not
// show may run, and stays.
func (l fileLocks) ended(s *Sample, procRoot string, hidden bool) {
	for pid := range l.holders {
		if _, ok := s.table()[pid]; ok || hidden {
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
func uvLock(procRoot, file, real string, m *Mounts, isLock func(string) bool) uvLockState {
	l := uvLockState{file: file, locks: fileLocks{holders: map[int]bool{}, writers: map[int]bool{}}}
	id, err := fileID(file)
	if err != nil {
		return l
	}
	l.id, l.keys = id, []lockID{id}
	maj, mnr, byMount := parseDev(m.devOf(real))
	if byMount && (maj != id.major || mnr != id.minor) {
		l.keys = append(l.keys, lockID{maj, mnr, id.ino})
	}
	// the lock files an earlier swap replaced, which their holders keep: they hold uv's lock as much as the file in
	// place's holders do, and their process trees are judged the same way
	for _, old := range replaced(procRoot, isLock, id) {
		l.keys = append(l.keys, old)
		if byMount && (maj != old.major || mnr != old.minor) {
			l.keys = append(l.keys, lockID{maj, mnr, old.ino})
		}
	}
	l.locks = lockHolders(procRoot, l.keys)
	return l
}

// keysOf are the names /proc/locks may give another file of the lock file's filesystem, by its inode: the
// devices the lock file is known by.
func (l uvLockState) keysOf(ino uint64) []lockID {
	var out []lockID
	for _, k := range l.keys {
		if id := (lockID{k.major, k.minor, ino}); !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// replaced are the lock files other than id that processes still have open where /proc names the lock file with
// " (deleted)" after it: files an earlier swap renamed over. Only such a link is stat'ed.
func replaced(procRoot string, isLock func(string) bool, id lockID) []lockID {
	ents, _ := os.ReadDir(procRoot)
	seen := map[lockID]bool{id: true}
	var out []lockID
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		fds := filepath.Join(procRoot, e.Name(), "fd")
		names, _ := os.ReadDir(fds)
		for _, n := range names {
			l, err := os.Readlink(filepath.Join(fds, n.Name()))
			if p, deleted := strings.CutSuffix(l, " (deleted)"); err != nil || !deleted || !isLock(p) {
				continue
			}
			if got, err := fileID(filepath.Join(fds, n.Name())); err == nil && !seen[got] {
				seen[got] = true
				out = append(out, got)
			}
		}
	}
	return out
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
	if _, ok := st.Sys().(*syscall.Stat_t); !ok {
		return lockID{}, fmt.Errorf("%s: no inode", p)
	}
	return idOf(st), nil
}

// idOf is a file's identity as /proc/locks names it, from its stat; none when it has no inode.
func idOf(st os.FileInfo) lockID {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return lockID{}
	}
	return lockID{unix.Major(uint64(s.Dev)), unix.Minor(uint64(s.Dev)), s.Ino}
}

// lockHolders are the locks on a file, by /proc/locks ("1: FLOCK ADVISORY READ 4242 fc:00:1234 0 EOF"; a waiter's
// line has "->" after its number), under any of its names.
func lockHolders(procRoot string, keys []lockID) fileLocks {
	out := fileLocks{holders: map[int]bool{}, writers: map[int]bool{}, waiting: map[int]bool{}}
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
			if pid, err := strconv.Atoi(f[4]); err == nil {
				out.waiting[pid] = true
				if f[3] == "WRITE" {
					out.alone = append(out.alone, pid)
				}
			}
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

// uvSwap puts capacity's lock in uv's lock file's place while processes hold the old file shared: a new file, locked
// exclusively, is renamed over the lock file, and the old one kept beside it (uvAsideSuffix). A uv process that starts
// opens the new file and waits, as it waits for uv's own prune, up to its UV_LOCK_TIMEOUT; the holders keep the old
// file, whose lock protects nothing the prune removes. When capacity is done, restore puts the old file back, so uv's
// own clean or prune waits for its holders again: they may run from the cache. stat is statLock, read once by the
// caller.
type uvSwap struct {
	lk       uvLockState
	look     *Sample // the processes as the rule's look saw them
	procRoot string
	c        uvCache
	hidden   bool   // /proc hides other users' processes
	after    func() // a test's way into the moment after the rename
	stat     func(string) (os.FileInfo, error)
}

// uvAsideSuffix names the cache's old lock file while capacity's lock is in its place.
const uvAsideSuffix = ".capacity-old"

// uvCleanWait is how long capacity keeps its lock at most, its own file no longer in the lock file's place, while a
// uv process that opened that file waits for it alone (a clean or a prune that came while capacity held it): that one
// would run while the old file's holders run from the cache. uv waits UV_LOCK_TIMEOUT for a lock, 300 s by default,
// and then gives up; the run waits as long and uvLockMargin more, and no longer than uv's default: it holds every rule
// of the run, the disk's among them, while it waits.
var uvCleanWait = 5*time.Minute + 10*time.Second

// restore puts the old lock file back in the lock file's place, its holders on it, waits out a uv clean or prune that
// waits for capacity's file (uvWaitOut), then ends the hold on stops, and says what it could not do, or "". A uv
// process that starts then shares the lock with the holders, and uv's own clean or prune waits for them. One that
// opened capacity's file meanwhile goes on under it once capacity lets it go, a lock no later clean waits for; the uv
// processes that came to capacity's file before a clean that waits for it alone wait as long as that one is waited
// out, and may give up.
func (s uvSwap) restore(held *os.File, hold *stopHold) string {
	defer hold.end()
	aside := s.lk.file + uvAsideSuffix
	if st, err := os.Stat(aside); err != nil || idOf(st) != s.lk.id {
		return "the cache's old lock file is not beside it, so its holders keep a lock no later clean or prune waits for"
	}
	if err := os.Rename(aside, s.lk.file); err != nil {
		return "the cache's old lock file could not be put back, so its holders keep a lock no later clean or prune " +
			"waits for: " + err.Error()
	}
	return uvWaitOut(s.procRoot, s.lk, held, s.c.isLock, s.look, s.stat)
}

// uvWaitOut holds capacity's file, no longer in the lock file's place, while a uv process waits for it alone (a clean
// or a prune that came to it, which would run while the old file's holders run from the cache), and says why it let
// go before that one gave up, or "". uv opens the file, tries it without waiting, then waits for it, and between the
// two is on no line of /proc/locks: so the run waits uvLockSettle before each read, and while a process that has the
// file open neither holds it nor waits for it. No process opens it by its name any more, so once every one that has
// it open is on a line, the open files are not read again. A waiter is waited out by its UV_LOCK_TIMEOUT (uvWaits),
// uvCleanWait at most.
func uvWaitOut(procRoot string, lk uvLockState, held *os.File, isLock func(string) bool, look *Sample,
	stat func(string) (os.FileInfo, error)) string {
	st, err := held.Stat()
	if err != nil {
		return ""
	}
	id := idOf(st)
	keys := lk.keysOf(id.ino)
	var deadline time.Time
	scan := true
	for start := time.Now(); ; {
		time.Sleep(uvLockSettle)
		l := lockHolders(procRoot, keys)
		var between []int
		if scan {
			open, _ := openers(procRoot, id, isLock, true, look, stat)
			for _, pid := range open {
				if pid != os.Getpid() && !l.holders[pid] && !l.waiting[pid] {
					between = append(between, pid)
				}
			}
			scan = len(between) > 0
		}
		if len(l.alone) == 0 && len(between) == 0 {
			return ""
		}
		if deadline.IsZero() {
			var wait time.Duration
			for _, pid := range append(slices.Clone(l.alone), between...) {
				wait = max(wait, uvWaits(procRoot, pid))
			}
			deadline = start.Add(min(wait, uvCleanWait))
		}
		if time.Now().After(deadline) {
			if len(l.alone) > 0 {
				return fmt.Sprintf("a uv clean or prune (pid %d) that came while capacity held the lock waits for it past "+
					"%s, as long as the run waits, so it may run while processes run from the cache", l.alone[0],
					deadline.Sub(start))
			}
			return fmt.Sprintf("a process (pid %d) has capacity's lock file open, and neither holds it nor waits for it, "+
				"past %s; a clean that waits for it after capacity lets go may run while processes run from the cache",
				between[0], deadline.Sub(start))
		}
	}
}

// uvLockMargin is how long past its UV_LOCK_TIMEOUT a waiter is waited out: it gives up as its timeout ends.
var uvLockMargin = 10 * time.Second

// uvWaits is how long a uv process waits for a lock, and uvLockMargin more: the UV_LOCK_TIMEOUT its environment sets,
// in whole seconds as uv reads it (a u64, an optional + first), and 300 s, uv's default, when it sets none, sets
// another value (uv warns, and waits that long), or the run cannot read its environment.
func uvWaits(procRoot string, pid int) time.Duration {
	wait := 300 * time.Second
	env, err := readEnviron(procRoot, pid)
	if v, ok := envValue(env, "UV_LOCK_TIMEOUT"); err == nil && ok {
		if n, err := strconv.ParseUint(strings.TrimPrefix(v, "+"), 10, 64); err == nil {
			if n > math.MaxInt64/uint64(time.Second) {
				return math.MaxInt64 // longer than a Duration holds: a wait with no end, to the run
			}
			wait = time.Duration(n) * time.Second
		}
	}
	if wait > math.MaxInt64-uvLockMargin {
		return math.MaxInt64
	}
	return wait + uvLockMargin
}

// stopHold keeps a stop (SIGINT, SIGTERM, SIGHUP) from ending the run while capacity's lock file is in the cache's
// lock file's place and the old file is aside, and then while a clean waits for capacity's file (uvWaitOut): the old
// file's holders would keep a lock no clean waits for, and the clean would run while they run from the cache. end
// ends the hold, and a stop that came meanwhile then ends the run; came reports whether one came. A nil hold holds
// nothing.
type stopHold struct {
	ch   chan os.Signal
	got  os.Signal
	done bool
}

func holdStops() *stopHold {
	h := &stopHold{ch: make(chan os.Signal, 1)}
	signal.Notify(h.ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return h
}

// came reports whether a stop came during the hold.
func (h *stopHold) came() bool {
	if h == nil {
		return false
	}
	if h.got == nil && !h.done {
		select {
		case h.got = <-h.ch:
		default:
		}
	}
	return h.got != nil
}

// end ends the hold, once: a stop that came meanwhile ends the run now.
func (h *stopHold) end() {
	if h == nil || h.done {
		return
	}
	signal.Stop(h.ch) // no stop comes to the channel after this, and one that came is in it
	h.came()
	h.done = true
	if holdEnded != nil {
		holdEnded()
	}
	if sig, ok := h.got.(syscall.Signal); ok {
		syscall.Kill(os.Getpid(), sig)
	}
}

// holdEnded is a test's way into the moment a hold on stops ends, and uvAfterPutBack into the moment after the
// put-back's rename.
var holdEnded, uvAfterPutBack func()

// uvPutBack puts back the cache's old lock file that a run set aside and did not put back (it was killed under its
// lock): the file in place is that run's, and the old one's holders keep a lock no clean waits for. It takes the file
// in place alone without waiting, so no process gets it meanwhile, puts the old one back, and waits out a uv clean or
// prune that waits for the file it took (uvWaitOut), a stop held meanwhile. It says why it waits instead (a process
// holds the file in place), why it did not, or why the old file stays on a run that may not put it back, or "".
func uvPutBack(procRoot string, c uvCache, m *Mounts, live bool) string {
	aside := c.named + uvAsideSuffix
	if _, err := os.Lstat(aside); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	why := "an earlier run set the cache's lock file aside and did not put it back"
	if !live {
		return why + "; a run with the rule live puts it back"
	}
	l := uvLock(procRoot, c.named, c.lockReal, m, c.isLock)
	if l.id == (lockID{}) {
		return why + ", and the lock file in its place cannot be read"
	}
	if st, err := os.Stat(aside); err == nil && idOf(st) == l.id {
		// a second link to the file in place: the run was killed before its rename, and nothing was swapped
		if err := os.Remove(aside); err != nil {
			return why + ": " + err.Error()
		}
		return ""
	}
	if !l.locks.known {
		return why + ", and the locks cannot be read"
	}
	f, err := os.OpenFile(c.named, os.O_RDWR, 0)
	if err != nil {
		return why + ": " + err.Error()
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || idOf(st) != l.id {
		return why + ", and the lock file in its place changed as it looked"
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return why + ": a uv process holds the one in its place, so it goes back once that ends"
	}
	hold := holdStops()
	defer hold.end()
	if err := os.Rename(aside, c.named); err != nil {
		return why + ": " + err.Error()
	}
	if uvAfterPutBack != nil {
		uvAfterPutBack()
	}
	if w := uvWaitOut(procRoot, l, f, c.isLock, nil, statLock); w != "" {
		return why + "; it is back, but " + w
	}
	return ""
}

// lockInPlace returns the file that holds capacity's lock and the func that ends the hold on stops, or nil and why it
// gave up. It gives up without renaming when the lock file is not the one the look saw, when looked, the caller's look
// at the processes, finds a reason, or when, with capacity's lock taken on the new file, the old file has a holder the
// look did not see, a process that waits for it or holds it alone, or one that has it open and was no holder (uv opens
// the file, then locks it). With aside, the old file stays beside the new one, a second link to it, for restore to put
// back, and a stop is held until it is back. afterRename looks the same way after the rename; what it finds then only
// keeps capacity's own prune from running, and a process that came to the old file in the instant between the last
// look and the rename keeps a lock no later clean or prune waits for. The caller closes the file it returns, which lets
// the lock go.
func (s uvSwap) lockInPlace(looked func() bool, aside bool) (*os.File, *stopHold, string) {
	var none *stopHold
	lockFile := s.lk.file
	if id, err := fileID(lockFile); err != nil || id != s.lk.id {
		return nil, none, "the cache's lock file changed since the look"
	}
	st, err := os.Stat(lockFile)
	if err != nil {
		return nil, none, "the lock file cannot be read: " + err.Error()
	}
	tmp := lockFile + ".capacity"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, none, err.Error()
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, none, err.Error()
	}
	fail := func(why string) (*os.File, *stopHold, string) {
		f.Close()
		os.Remove(tmp)
		return nil, none, why
	}
	if err := f.Chmod(st.Mode().Perm()); err != nil {
		return fail(err.Error())
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail("its new lock file could not be locked: " + err.Error())
	}
	if looked != nil && !looked() {
		return fail("")
	}
	if why := s.check(false, "since the look"); why != "" {
		return fail(why)
	}
	hold, kept := none, lockFile+uvAsideSuffix
	if aside {
		hold = holdStops()
		if err := os.Link(lockFile, kept); err != nil {
			hold.end()
			return fail("the cache's lock file could not be set aside: " + err.Error())
		}
		if st, err := os.Stat(kept); err != nil || idOf(st) != s.lk.id {
			os.Remove(kept)
			hold.end()
			return fail("the cache's lock file changed since the look")
		}
	}
	if err := os.Rename(tmp, lockFile); err != nil {
		if aside {
			os.Remove(kept)
		}
		hold.end()
		return fail(err.Error())
	}
	if s.after != nil {
		s.after()
	}
	return f, hold, ""
}

// afterRename looks at the old lock file again once it was renamed over, a moment after (uv opens the file, then
// locks it), and says why capacity lets its lock go, or "".
func (s uvSwap) afterRename() string {
	time.Sleep(uvLockSettle)
	return s.check(true, "as capacity put its own in place")
}

// check says why the old lock file keeps capacity's lock out of its place, or "": a holder the look did not see, a
// process that waits for it or holds it alone, a process that has it open and was no holder, or a uv process the
// look did not see whose open files the run may not read. renamed is whether the file was renamed over.
func (s uvSwap) check(renamed bool, when string) string {
	// the open files first and the locks last: uv locks the file as it opens it, so the read of the locks, just before
	// the rename, sees a uv process that came during the scan
	open, unseen := openers(s.procRoot, s.lk.id, s.c.isLock, renamed, s.look, s.stat)
	now := lockHolders(s.procRoot, s.lk.keys)
	if !now.known {
		return "the locks cannot be read again"
	}
	now.ended(nil, s.procRoot, s.hidden)
	for _, pid := range sortedPIDs(now.holders) {
		if !s.wasHolder(pid) {
			return fmt.Sprintf("a uv process (pid %d) took the cache's lock %s", pid, when)
		}
	}
	if why := now.blocks(); why != "" {
		return why
	}
	for _, pid := range open {
		if !s.wasHolder(pid) {
			return fmt.Sprintf("a process (pid %d) opened the cache's lock %s", pid, when)
		}
	}
	if len(unseen) > 0 {
		return fmt.Sprintf("a uv process (pid %d) that started %s may have the cache's lock open; the run may not read "+
			"its open files", unseen[0], when)
	}
	return ""
}

// wasHolder reports whether a process is one the look saw hold the lock: the same pid, and the same start.
func (s uvSwap) wasHolder(pid int) bool {
	p, ok := s.look.Table[pid]
	if !ok || !s.lk.locks.holders[pid] {
		return false
	}
	st, err := os.ReadFile(filepath.Join(s.procRoot, strconv.Itoa(pid), "stat"))
	return err == nil && startOf(string(st)) == p.Start
}

// openers are the processes that have the lock file open, by their /proc fds: a link /proc names as the lock file
// (with " (deleted)" after it once the file was renamed over) whose file is id. Only such a link is stat'ed: a stat
// goes into the file's filesystem, and one that does not answer would hold the look, while a readlink reads /proc
// alone. unseen are the uv processes the look did not see whose open files the run may not read: nothing says one
// has not opened the file. stat stats a link (statLock, read by the caller: this may run in a look that gave up).
func openers(procRoot string, id lockID, isLock func(string) bool, renamed bool, look *Sample,
	stat func(string) (os.FileInfo, error)) (open, unseen []int) {
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
			if st, err := stat(fd); err == nil && idOf(st) == id {
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
	p, ok := look.table()[pid]
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
				f.Done, f.By = true, "Docker" // what Docker says it reclaimed, as Docker's: its store is root's
				var said bool
				if f.Freed, said = parseSizeAfter(res, "Total:"); !said {
					f.Freed, said = parseSizeAfter(res, "Total reclaimed space:")
				}
				f.none = said && f.Freed == 0 && removedNothing(res)
				f.Unsaid = !said
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
