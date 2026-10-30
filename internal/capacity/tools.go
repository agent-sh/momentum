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
	"sort"
	"strconv"
	"strings"
	"time"
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
	for len(args) > 0 && strings.Contains(args[0], "=") && !strings.HasPrefix(args[0], "-") {
		env, args = append(env, args[0]), args[1:] // NAME=value before the arguments: the command's environment
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
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

// uv prunes the uv cache with `uv cache prune --ci` when it is over its size, or when the last prune is
// older than the cadence (or there was none). --ci keeps the wheels uv built and drops the ones it can download
// again, and the unpacked wheels environments import from. A prune under a running uv process deletes files it
// may still import, so the rule never passes --force: uv's own lock, which every uv process holds while it runs,
// decides, and the rule looks first, to wait quietly for an install (a sync, a venv being made), which ends soon,
// and to report a long-lived one (a tool server run with uvx, a script run with uv run) by name.
func (r *runner) uv(ctx context.Context, live bool) ([]Finding, []string, error) {
	out, err := r.tools.UV(ctx, 30*time.Second, "cache", "dir")
	if errors.Is(err, errNoTool) {
		return nil, []string{"uv is not installed"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dir := strings.TrimSpace(lastLine(out))
	size := r.uvSize(ctx, dir)
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
	now := TakeSample(r.opt.ProcRoot, r.m, func(p string) bool { return under(p, f.canon) })
	if busy := uvBusy(now); busy != "" {
		f.Action, f.Why = ActKeep, "waits for an install: "+busy
		return []Finding{f}, notes, nil
	}
	busyWhy := func(readers string) string {
		w := "it waits: uv processes read the cache, and a prune under them would delete files they import"
		if readers != "" {
			w += " (" + readers + ")"
		}
		return w
	}
	if readers := uvReaders(now, f.canon); readers != "" {
		f.Action, f.class, f.Why = ActReport, "busy", busyWhy(readers)
		return []Finding{f}, notes, nil
	}
	f.Action = ActDelete
	if !live {
		return []Finding{f}, notes, nil
	}
	// No --force: uv's own lock, which every uv process holds shared while it runs, decides; one that started since
	// the look keeps the cache, and the prune waits for the next run.
	_, err = r.tools.UV(ctx, 20*time.Minute, "cache", "prune", "--ci")
	if err != nil && uvInUse(err) {
		f.Action, f.class, f.Why = ActReport, "busy", busyWhy(uvReaders(TakeSample(r.opt.ProcRoot, r.m,
			func(p string) bool { return under(p, f.canon) }), f.canon))
		return []Finding{f}, notes, nil
	}
	after := r.uvSize(ctx, dir)
	if size >= 0 && after >= 0 {
		f.Freed = max64(0, size-after)
		f.none = f.Freed == 0
	}
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
func (r *runner) uvSize(ctx context.Context, dir string) int64 {
	if out, err := r.tools.UV(ctx, 2*time.Minute, "cache", "size", "--preview-features", "cache-size"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if n, err := strconv.ParseInt(strings.TrimSpace(l), 10, 64); err == nil {
				return n
			}
		}
	}
	if dir == "" {
		return -1
	}
	return Measure(dir, nil).Bytes
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

// uvReaders names, at most three, the uv processes running and the processes using a file in the cache, or "".
func uvReaders(s *Sample, cache string) string {
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
	pids := make([]int, 0, len(s.Table))
	for pid := range s.Table {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		p := s.Table[pid]
		if len(p.Argv) > 0 && (filepath.Base(p.Argv[0]) == "uv" || filepath.Base(p.Argv[0]) == "uvx") {
			add(pid, strings.Join(p.Argv[:min(len(p.Argv), 4)], " "))
		}
	}
	for _, u := range s.Uses {
		if cache != "" && under(u.Path, cache) {
			add(u.PID, u.Comm)
		}
	}
	if len(seen) > len(names) {
		names = append(names, fmt.Sprintf("%d more", len(seen)-len(names)))
	}
	return strings.Join(names, ", ")
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
	// What the prune removes is counted record by record: `buildx du` takes no age filter, and an estimate that
	// counts the records an image shares (most of an old cache, often) lists gigabytes the prune cannot free.
	cache := Finding{Rule: RuleDocker, Path: "docker build cache", Action: ActDelete}
	if keep, err := time.ParseDuration(until); err == nil {
		if du, err := r.tools.Docker(ctx, 2*time.Minute, "buildx", "du", "--format", "json"); err == nil {
			n, size := prunable(du, keep)
			cache.Bytes = size
			cache.Detail = fmt.Sprintf("builder prune --filter until=%s: %d %s unused for %s", until, n, plural("record", n), until)
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
	for _, f := range []*Finding{&cache, &images} {
		if f.Action == "" || f.Bytes == 0 {
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
				f.none = said && f.Freed == 0
			}
		}
		out = append(out, *f)
	}
	if live {
		return out, notes, r.st.SetMark(ctx, "prune:docker", r.now)
	}
	return out, notes, nil
}

// prunable counts the build cache records a prune limited by age removes, from `docker buildx du --format json`
// (one record per line): those no build uses, that no image shares, that are not BuildKit's own (internal and
// frontend records, which a prune without --all keeps), and that were last used longer ago than keep. Docker prints
// the last use as a rough age ("12 days ago"); a record never used counts.
func prunable(du string, keep time.Duration) (n int, size int64) {
	for _, l := range strings.Split(du, "\n") {
		var rec struct {
			Reclaimable, Shared    bool
			Type, Size, LastUsedAt string
		}
		if json.Unmarshal([]byte(l), &rec) != nil || !rec.Reclaimable || rec.Shared || rec.Type == "internal" || rec.Type == "frontend" {
			continue
		}
		if rec.LastUsedAt != "" {
			if age, ok := humanAge(rec.LastUsedAt); !ok || age < keep {
				continue
			}
		}
		n++
		size += parseSize(rec.Size)
	}
	return n, size
}

var ageRe = regexp.MustCompile(`^(\d+) (second|minute|hour|day|week|month|year)s?$`)

// humanAge reads an age as Docker prints it (go-units' HumanDuration, with " ago"): the least it can be, since each
// unit rounds down ("7 days" is 7 to 8 days).
func humanAge(s string) (time.Duration, bool) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), " ago")
	switch s {
	case "less than a second":
		return 0, true
	case "about a minute":
		return time.Minute, true
	case "about an hour":
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
	day := 24 * time.Hour
	unit := map[string]time.Duration{"second": time.Second, "minute": time.Minute, "hour": time.Hour, "day": day,
		"week": 7 * day, "month": 30 * day, "year": 365 * day}[m[2]]
	return time.Duration(v) * unit, true
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
