// Package proc reads the Linux process table once per scan, so every session source sees the same processes.
// It reads a proc filesystem at any root, which lets tests build a synthetic one.
package proc

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TicksPerSecond is USER_HZ, the unit of a process start time in stat. It is 100 on every mainstream Linux
// architecture.
const TicksPerSecond = 100

// Proc is one process.
type Proc struct {
	PID        int
	PPID       int
	Argv       []string
	StartTicks uint64    // field 22 of stat: clock ticks after boot; with PID it names one process
	Start      time.Time // StartTicks as a time, from the boot time
}

// Table is a snapshot of the processes under Root. It is not changed after Read, so goroutines can share it.
type Table struct {
	Root  string
	Boot  time.Time
	ByPID map[int]*Proc
	kids  map[int][]int
}

// maxTicks is the largest start a time.Duration can hold; stat never reports one, so a larger value is corrupt.
const maxTicks = uint64(math.MaxInt64 / int64(time.Second/TicksPerSecond))

// Read walks root (normally /proc). Kernel threads, which have no command line, are left out, and so is any
// process that ends during the walk.
func Read(root string) (*Table, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	t := &Table{Root: root, Boot: bootTime(root), ByPID: map[int]*Proc{}}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		p := &Proc{PID: pid, Argv: strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")}
		if st, err := os.ReadFile(filepath.Join(root, e.Name(), "stat")); err == nil {
			p.PPID, p.StartTicks = parseStat(string(st))
			if p.StartTicks <= maxTicks {
				p.Start = t.Boot.Add(time.Duration(p.StartTicks) * (time.Second / TicksPerSecond))
			}
		}
		t.ByPID[pid] = p
	}
	t.kids = map[int][]int{}
	for _, p := range t.ByPID {
		t.kids[p.PPID] = append(t.kids[p.PPID], p.PID)
	}
	for _, k := range t.kids {
		sort.Ints(k)
	}
	return t, nil
}

// parseStat returns the parent pid and start ticks from a stat line. The command name in parentheses may hold
// spaces and parentheses, so fields count from the last ')'.
func parseStat(s string) (ppid int, start uint64) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return 0, 0
	}
	f := strings.Fields(s[i+2:])
	if len(f) > 19 {
		ppid, _ = strconv.Atoi(f[1])
		start, _ = strconv.ParseUint(f[19], 10, 64)
	}
	return ppid, start
}

func bootTime(root string) time.Time {
	b, _ := os.ReadFile(filepath.Join(root, "stat"))
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

// Children returns the pids whose parent is pid, lowest first.
func (t *Table) Children(pid int) []int { return t.kids[pid] }

// Alive reports whether the snapshot holds pid, and when start is not zero, whether it is the same process
// (a recycled pid has a different start).
func (t *Table) Alive(pid int, start uint64) bool {
	p := t.ByPID[pid]
	return p != nil && (start == 0 || p.StartTicks == start)
}

// Cwd returns a process's working directory, or "" when it cannot be read.
func (t *Table) Cwd(pid int) string {
	c, _ := os.Readlink(filepath.Join(t.Root, strconv.Itoa(pid), "cwd"))
	return c
}

// Files returns what a process's open file descriptors point at.
func (t *Table) Files(pid int) []string {
	dir := filepath.Join(t.Root, strconv.Itoa(pid), "fd")
	fds, _ := os.ReadDir(dir)
	var out []string
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil {
			out = append(out, target)
		}
	}
	return out
}

// Env returns one variable from a process's starting environment.
func (t *Table) Env(pid int, key string) string {
	raw, err := os.ReadFile(filepath.Join(t.Root, strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// Slot is a process, its working directory, and the start of the next newer process in the same directory. A
// session created between the two belongs to this process.
type Slot struct {
	Proc *Proc
	Cwd  string
	Next time.Time // zero when no newer process shares the directory
}

// Slots orders processes newest first and gives each its slot.
func (t *Table) Slots(ps []*Proc) []Slot {
	ps = append([]*Proc(nil), ps...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Start.After(ps[j].Start) })
	newer := map[string]time.Time{}
	out := make([]Slot, 0, len(ps))
	for _, p := range ps {
		cwd := t.Cwd(p.PID)
		out = append(out, Slot{Proc: p, Cwd: cwd, Next: newer[cwd]})
		newer[cwd] = p.Start
	}
	return out
}

// Exe is the base name of the program, from argv[0].
func (p *Proc) Exe() string {
	if len(p.Argv) == 0 {
		return ""
	}
	return filepath.Base(p.Argv[0])
}

// Arg returns argv[i], or "".
func (p *Proc) Arg(i int) string {
	if i < len(p.Argv) {
		return p.Argv[i]
	}
	return ""
}

// Is reports whether the program or the script it runs (argv[0] or argv[1]) is named name.
func (p *Proc) Is(name string) bool {
	for _, a := range p.Argv[:min(2, len(p.Argv))] {
		if filepath.Base(a) == name {
			return true
		}
	}
	return false
}

// Has reports whether any argument equals one of flags.
func (p *Proc) Has(flags ...string) bool {
	for _, a := range p.Argv {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}
