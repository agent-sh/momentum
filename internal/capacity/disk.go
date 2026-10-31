package capacity

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The disk rule. Every run records each disk's use. When a disk fills faster than the threshold over the window, or
// would reach the full percent within the configured hours at that rate, the run measures which processes write
// (their write_bytes over a few seconds), names each one's working directory and the agent session it runs under,
// and leaves a problem note: never a notification. Every judging rule then scans on that run, whatever its
// cadence. A disk above its full percent has its own note.

// minWindow is the shortest span of samples a rate is taken over: two runs a minute apart say little.
const minWindow = 10 * time.Minute

// disk fills in each disk's rate and, when one fills fast, its writers. It reports whether any does.
func (r *runner) disk(ctx context.Context, disks []DiskState) bool {
	fast := false
	window := time.Duration(r.cfg.Fill.WindowMin * float64(time.Minute))
	for i := range disks {
		d := &disks[i]
		if d.Err != "" {
			r.res.Disks = append(r.res.Disks, *d)
			continue
		}
		if d.Used+d.Avail > 0 {
			d.pctFull = 100 * float64(d.Used) / float64(d.Used+d.Avail)
		}
		samples, err := r.diskHistory(ctx, d.Disk, r.now.Add(-window))
		if err != nil {
			d.Err = err.Error()
		}
		for _, s := range samples { // the oldest sample in the window that is far enough back
			if span := r.now.Sub(s.At); span >= minWindow {
				d.Window = span
				d.Rate = float64(d.Used-s.Used) / (1 << 30) / span.Hours()
				break
			}
		}
		left := r.cfg.Fill.Full/100*float64(d.Used+d.Avail) - float64(d.Used)
		if d.Rate > 0 && left > 0 {
			d.FullIn = time.Duration(left / (1 << 30) / d.Rate * float64(time.Hour))
		}
		// A disk already above its full percent is full, not filling fast: it has a note of its own (recordFull).
		d.Fast = d.Rate >= r.cfg.Fill.GiBPerHour ||
			(d.Rate >= r.cfg.Fill.MinGiBPerHour && d.FullIn > 0 && d.FullIn <= time.Duration(r.cfg.Fill.WithinHours*float64(time.Hour)))
		if d.Fast {
			fast = true
			d.Writers = r.writers(ctx, d.mountPath)
		}
		d.WindowMS, d.FullInMS = d.Window.Milliseconds(), d.FullIn.Milliseconds()
		r.res.Disks = append(r.res.Disks, *d)
	}
	return fast
}

// writers are the processes that wrote fastest over the writer wait, with their working directory, whether it is
// on the disk, and their session. Another user's processes cannot be read (root's among them).
func (r *runner) writers(ctx context.Context, mountPath string) []Writer {
	a := r.opt.Writes()
	time.Sleep(r.opt.WriterWait)
	b := r.opt.Writes()
	secs := r.opt.WriterWait.Seconds()
	var diskDev uint64
	if st, err := os.Stat(mountPath); err == nil {
		diskDev = uint64(st.Sys().(*syscall.Stat_t).Dev)
	}
	sessions := r.liveSessions(ctx)
	boot := time.Time{}
	if up := uptimeOf(r.opt.ProcRoot); up > 0 {
		boot = r.now.Add(-time.Duration(up * float64(time.Second)))
	}
	pids := map[int]bool{}
	for pid := range b {
		pids[pid] = true
	}
	for pid := range r.snap {
		pids[pid] = true
	}
	var out []Writer
	for pid := range pids {
		w := Writer{PID: pid}
		if before, had := a[pid]; had {
			if n, ok := b[pid]; ok {
				w.Rate = float64(n-before) / (1 << 20) / secs
			}
		}
		if cur, ok := r.snap[pid]; ok && !r.prevAt.IsZero() {
			if p, ok := r.prev[pid]; ok && p.Start == cur.Start {
				w.Written, w.Over = cur.Written-p.Written, r.now.Sub(r.prevAt).Seconds()
			} else if started := boot.Add(time.Duration(cur.Start) * 10 * time.Millisecond); !boot.IsZero() && started.After(r.prevAt) {
				w.Written, w.Over = cur.Written, r.now.Sub(started).Seconds() // it started since: all it wrote
			}
		}
		if w.Rate < 1 && w.since() < 1 {
			continue
		}
		dir := filepath.Join(r.opt.ProcRoot, strconv.Itoa(pid))
		if c, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
			w.Comm = strings.TrimSpace(string(c))
		}
		if l, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
			w.Cwd = l
			if st, err := os.Stat(l); err == nil && diskDev != 0 {
				w.OnDisk = uint64(st.Sys().(*syscall.Stat_t).Dev) == diskDev
			}
		}
		for p, hops := pid, 0; p > 1 && hops < 64; hops++ { // up the parents to a session's process
			if s, ok := sessions[p]; ok {
				w.Session = firstNonEmpty(s.Name, s.Key) + " (" + s.Tool + ")"
				break
			}
			pp, ok := r.sample.Table[p]
			if !ok {
				break
			}
			p = pp.PPID
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pace() > out[j].pace() })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// pace is the faster of a writer's rate at the look and its rate since the run before.
func (w Writer) pace() float64 { return max(w.Rate, w.since()) }

// uptimeOf is the seconds since boot, from a /proc's uptime, or 0.
func uptimeOf(procRoot string) float64 {
	b, err := os.ReadFile(filepath.Join(procRoot, "uptime"))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// procWrites is each readable process's written bytes and its start (the 22nd field of its stat, in clock ticks
// after boot), from a /proc.
func procWrites(procRoot string) map[int]store.ProcWrite {
	out := map[int]store.ProcWrite{}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		io, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "io"))
		if err != nil {
			continue
		}
		st, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		w := store.ProcWrite{PID: pid, Start: startOf(string(st)), Written: -1}
		for _, l := range strings.Split(string(io), "\n") {
			if v, ok := strings.CutPrefix(l, "write_bytes:"); ok {
				w.Written, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			}
		}
		if w.Written >= 0 && w.Start > 0 {
			out[pid] = w
		}
	}
	return out
}

// startOf is a process's start from its stat, in clock ticks after boot, or 0.
func startOf(stat string) int64 {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 20 {
		return 0
	}
	n, _ := strconv.ParseInt(f[19], 10, 64)
	return n
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
