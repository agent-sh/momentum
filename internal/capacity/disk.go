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
)

// The disk rule. Every run records each disk's use. When a disk fills faster than the threshold over the window, or
// would reach the full percent within the configured hours at that rate, the run measures which processes write
// (their write_bytes over a few seconds), names each one's working directory and the agent session it runs under,
// and leaves a problem note: never a notification. Every deleting rule then scans on that run, whatever its
// cadence.

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
		if d.Rate > 0 {
			left := r.cfg.Fill.Full/100*float64(d.Used+d.Avail) - float64(d.Used)
			d.FullIn = time.Duration(max(0, left) / (1 << 30) / d.Rate * float64(time.Hour))
		}
		d.Fast = d.Rate >= r.cfg.Fill.GiBPerHour ||
			(d.Rate > 0 && d.FullIn <= time.Duration(r.cfg.Fill.WithinHours*float64(time.Hour)))
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
	var out []Writer
	for pid, n := range b {
		rate := float64(n-a[pid]) / (1 << 20) / secs
		if _, had := a[pid]; !had || rate < 1 {
			continue
		}
		w := Writer{PID: pid, Rate: rate}
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
	sort.Slice(out, func(i, j int) bool { return out[i].Rate > out[j].Rate })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
