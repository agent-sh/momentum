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

// The disk rule. Every run records each disk's use, and each process's written bytes (a snapshot the next run
// compares with). When a disk fills faster than the threshold over the window, or would reach the full percent
// within the configured hours at that rate, the run names the processes that write: fast at a look of a few
// seconds, or since the run before. It names each one's working directory and the agent session it runs under, and
// leaves a problem note: never a notification. Every judging rule then scans on that run, whatever its cadence. A
// disk above its full percent has its own note.

// userHZ is the clock ticks a second of a process's start in /proc/PID/stat and of a snapshot's uptime: the
// kernel's USER_HZ (sysconf(_SC_CLK_TCK)), which is 100 on every architecture this runs on.
const userHZ = 100

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

// writers are the processes that wrote fastest over the writer wait, or since the run before, with their working
// directory, whether it is on the disk, and their session. Another user's processes cannot be read (root's among
// them).
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
	if r.iv == nil { // once a run, for every fast disk
		r.iv = between(r.prevWrites, r.writes)
	}
	iv := r.iv
	// a writer since the run before counts from the slowest fill the rule reports, which a writer alone may make
	slowest := r.cfg.Fill.MinGiBPerHour * 1024 / 3600
	pids := map[int]bool{}
	for pid := range b {
		pids[pid] = true
	}
	for pid := range r.writes.ws {
		pids[pid] = true
	}
	var out []Writer
	for pid := range pids {
		w := Writer{PID: pid, Start: r.writes.ws[pid].Start}
		if before, had := a[pid]; had {
			if n, ok := b[pid]; ok {
				w.Rate = float64(n-before) / (1 << 20) / secs
			}
		}
		if d, ok := iv[pid]; ok {
			w.Written, w.Over = d.written, d.over
		}
		if w.Rate < 1 && (w.Over <= 0 || w.since() < slowest) {
			continue
		}
		if p, ok := r.sample.Table[pid]; ok { // the sample's names, for a process that ended since
			w.Comm, w.Cwd = p.Comm, p.Cwd
		}
		dir := filepath.Join(r.opt.ProcRoot, strconv.Itoa(pid))
		if c, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
			w.Comm = strings.TrimSpace(string(c))
		}
		if l, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
			w.Cwd = l
		}
		if w.Cwd != "" && diskDev != 0 {
			if st, err := os.Stat(w.Cwd); err == nil {
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

// writesSnap is each process's written bytes at one look, with its parent and start from the run's sample, and
// when the look was.
type writesSnap struct {
	snap store.Snapshot
	ws   map[int]store.ProcWrite
}

// clockOf is the time of a look at the processes: the wall clock, and the boot's own count of clock ticks, read
// before the look lists /proc, so a process that starts after it is past it; and which boot.
func clockOf(procRoot string, now time.Time) store.Snapshot {
	return store.Snapshot{At: now, Uptime: uptimeTicks(procRoot), Boot: bootID(procRoot)}
}

// readWrites reads each sampled process's written bytes. A process's start and the snapshot's uptime are on one
// count, so the wall clock drops out.
func readWrites(procRoot string, table map[int]Proc, snap store.Snapshot) writesSnap {
	out := writesSnap{snap: snap, ws: map[int]store.ProcWrite{}}
	for pid, p := range table {
		if p.Start <= 0 {
			continue
		}
		io, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "io"))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(io), "\n") {
			if v, ok := strings.CutPrefix(l, "write_bytes:"); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					out.ws[pid] = store.ProcWrite{PID: pid, PPID: p.PPID, Start: p.Start, Written: n}
				}
			}
		}
	}
	return out
}

// uptimeTicks is the time since boot in clock ticks, from a /proc's uptime, or 0.
func uptimeTicks(procRoot string) int64 {
	b, err := os.ReadFile(filepath.Join(procRoot, "uptime"))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v * userHZ)
}

// bootID names the boot, from a /proc's kernel boot_id, or "".
func bootID(procRoot string) string {
	b, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// wrote is what a process wrote between two snapshots, and over how many seconds.
type wrote struct {
	written int64
	over    float64
}

// between is what each process wrote since the snapshot before. The same process (pid and start, on the same boot)
// wrote the difference. One that started since wrote all it wrote, and on another boot every process started
// since. A process takes in all a waited-for child ever wrote, and a launcher passes its children's on to its own
// parent when it ends; so for a process on record that ended, the nearest recorded ancestor still running loses
// what the record held of it, and keeps what it wrote since. A counter that went down, or a process there before
// but not on record (unreadable then), is not known.
func between(prev, cur writesSnap) map[int]wrote {
	out := map[int]wrote{}
	if prev.snap.At.IsZero() || cur.snap.Uptime <= 0 {
		return out
	}
	sameBoot := prev.snap.Boot == cur.snap.Boot && cur.snap.Uptime >= prev.snap.Uptime
	took := map[int]int64{}
	if sameBoot {
		for pid, c := range prev.ws {
			if now, ok := cur.ws[pid]; ok && now.Start == c.Start {
				continue
			}
			for a, hops := c.PPID, 0; hops < 64; hops++ { // up past the ended ones, to the ancestor still running
				p, ok := prev.ws[a]
				if !ok {
					break
				}
				if q, ok := cur.ws[a]; ok && q.Start == p.Start {
					took[a] += c.Written
					break
				}
				a = p.PPID
			}
		}
	}
	for pid, w := range cur.ws {
		p, ok := prev.ws[pid]
		switch {
		case sameBoot && ok && p.Start == w.Start:
			if w.Written < p.Written {
				continue
			}
			out[pid] = wrote{max(0, w.Written-p.Written-took[pid]), float64(cur.snap.Uptime-prev.snap.Uptime) / userHZ}
		case !sameBoot || w.Start >= prev.snap.Uptime: // a start in the tick of the look before is after it
			out[pid] = wrote{w.Written, float64(max(0, cur.snap.Uptime-w.Start)) / userHZ}
		}
	}
	return out
}

// carry keeps on record a process on record that is still there (its pid and start, by its stat, which this user
// can read) but whose written bytes cannot be read now: a zombie its parent has not waited for yet, or one that
// became non-dumpable. It has not ended: its parent has taken in nothing of it.
func carry(procRoot string, prev writesSnap, cur *writesSnap) {
	if prev.snap.At.IsZero() || prev.snap.Boot != cur.snap.Boot {
		return
	}
	for pid, c := range prev.ws {
		if now, ok := cur.ws[pid]; ok && now.Start == c.Start {
			continue
		}
		st, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if err != nil || startOf(string(st)) != c.Start {
			continue
		}
		cur.ws[pid] = store.ProcWrite{PID: pid, PPID: parentOf(string(st)), Start: c.Start, Written: c.Written}
	}
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
