package capacity

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A filesystem mounted noatime never updates a file's access time, so nothing says when a file was read. The
// sampler records use itself: each run reads every process's open files, its memory maps, its working directory,
// its program and the paths on its command line. A model a server mapped, a dataset a job holds open, a build a
// shell sits in: each is seen while it is in use. What a process opened and closed between two runs is not.

// Use is one path a process uses: how (fd, map, cwd, exe, arg) and which process.
type Use struct {
	Path string // canonical
	How  string
	PID  int
	Comm string
}

// Sample is one look at the processes: the paths in use that the run cares about, and the command-line words that
// may name a Hugging Face repository.
type Sample struct {
	At         time.Time
	Took       time.Duration
	Procs      int
	Unreadable int // processes whose open files could not be read (another user's)
	Uses       []Use
	RepoIDs    map[string]Use // org/name words on command lines, by word
	Table      map[int]Proc   // every process read
	byPath     map[string]Use
}

// Proc is one process the sampler read, and its working directory (canonical), wherever it is.
type Proc struct {
	PID, PPID int
	Start     int64 // clock ticks after boot (userHZ a second)
	Comm      string
	Argv      []string
	Cwd       string
}

// Children reports whether any process has pid as its parent.
func (s *Sample) Children(pid int) bool {
	for _, p := range s.Table {
		if p.PPID == pid {
			return true
		}
	}
	return false
}

// InUse reports whether the sample saw a process use p or anything under it, and the first such use. A container's
// mount of a directory holding p uses p too.
func (s *Sample) InUse(p string) (Use, bool) {
	if s == nil {
		return Use{}, false
	}
	if u, ok := s.byPath[p]; ok {
		return u, true
	}
	for _, u := range s.Uses {
		if under(u.Path, p) || (u.How == "mount" && under(p, u.Path)) {
			return u, true
		}
	}
	return Use{}, false
}

// TakeSample reads every process under procRoot. keep says which canonical paths the run cares about; only those
// are kept. Paths are made canonical with m.
func TakeSample(procRoot string, m *Mounts, keep func(string) bool) *Sample {
	start := time.Now()
	s := &Sample{At: start, RepoIDs: map[string]Use{}, Table: map[int]Proc{}, byPath: map[string]Use{}}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		s.Took = time.Since(start)
		return s
	}
	add := func(raw, how string, pid int, comm string) {
		p := strings.TrimSuffix(raw, " (deleted)")
		if !filepath.IsAbs(p) {
			return
		}
		p = m.Canon(p)
		if !keep(p) {
			return
		}
		if _, ok := s.byPath[p]; ok {
			return
		}
		u := Use{Path: p, How: how, PID: pid, Comm: comm}
		s.byPath[p] = u
		s.Uses = append(s.Uses, u)
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(raw) == 0 { // a kernel thread, or gone
			continue
		}
		s.Procs++
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		comm := filepath.Base(argv[0])
		if c, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
			comm = strings.TrimSpace(string(c))
		}
		ppid, start := 0, int64(0)
		if st, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
			ppid, start = parentOf(string(st)), startOf(string(st))
		}
		pr := Proc{PID: pid, PPID: ppid, Start: start, Comm: comm, Argv: argv}
		if l, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
			add(l, "cwd", pid, comm)
			if filepath.IsAbs(l) {
				pr.Cwd = m.Canon(strings.TrimSuffix(l, " (deleted)"))
			}
		}
		s.Table[pid] = pr
		if l, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			add(l, "exe", pid, comm)
		}
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if errors.Is(err, fs.ErrPermission) {
			s.Unreadable++
		}
		for _, fd := range fds {
			if l, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil {
				add(l, "fd", pid, comm)
			}
		}
		readMaps(filepath.Join(dir, "maps"), func(p string) { add(p, "map", pid, comm) })
		for _, a := range argv[1:] {
			for _, w := range words(a) {
				switch {
				case filepath.IsAbs(w):
					add(w, "arg", pid, comm)
				case isRepoID(w):
					if _, ok := s.RepoIDs[w]; !ok {
						s.RepoIDs[w] = Use{Path: w, How: "arg", PID: pid, Comm: comm}
					}
				}
			}
		}
	}
	s.Took = time.Since(start)
	return s
}

// parentOf is the parent pid in a stat line. The command name in parentheses may hold spaces and parentheses, so
// fields count from the last ')'.
func parentOf(stat string) int {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 > len(stat) {
		return 0
	}
	f := strings.Fields(stat[i+2:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// readMaps calls f with each file a maps file names, once each.
func readMaps(path string, f func(string)) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, '/') // the path is the last field, and the only one that starts with /
		if i < 0 {
			continue
		}
		p := line[i:]
		if !seen[p] {
			seen[p] = true
			f(p)
		}
	}
}

// words splits a command-line argument or a line of text into the words that may be paths or repository ids: at
// spaces, quotes, and the separators of options, assignments and lists (=, :, ;, ,). A word loses systemd's
// command prefixes (-, @, +, !), and ~/, $HOME/, ${HOME}/ and systemd's %h/ become the home directory.
func words(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '`', '=', ':', ';', ',', '(', ')', '[', ']', '{', '}', '<', '>', '|', '&':
			return true
		}
		return false
	})
	// a quoted string is a word too, spaces and all: a path may hold a space; so is its value, in an assignment
	for _, q := range []string{`"`, `'`} {
		parts := strings.Split(s, q)
		for i := 1; i < len(parts); i += 2 {
			if w := strings.TrimSpace(parts[i]); w != "" && !strings.ContainsAny(w, "\n") {
				f = append(f, w)
				if _, v, ok := strings.Cut(w, "="); ok && v != "" {
					f = append(f, v)
				}
			}
		}
	}
	home := homeDir()
	for i, w := range f {
		w = strings.TrimLeft(w, "-@+!") // systemd's command prefixes (ExecStart=-/x, EnvironmentFile=-/y)
		f[i] = w
		for _, pre := range []string{"~/", "$HOME/", "%h/", "${HOME}/"} {
			if strings.HasPrefix(w, pre) {
				f[i] = filepath.Join(home, w[len(pre):])
			}
		}
	}
	return f
}

// isRepoID reports whether w looks like a Hugging Face repository id: org/name, each part a name.
func isRepoID(w string) bool {
	org, name, ok := strings.Cut(w, "/")
	if !ok || strings.Contains(name, "/") {
		return false
	}
	return repoPart(org) && repoPart(name)
}

func repoPart(s string) bool {
	if s == "" || len(s) > 96 || strings.Contains(s, "--") || strings.Contains(s, "..") {
		return false
	}
	for i, r := range s {
		ok := r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !ok || i == 0 && (r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// homeDir is the user's home directory.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

// Writes reads how many bytes each process has caused to be written to storage (write_bytes in /proc/PID/io), by
// pid. A process of another user cannot be read and is left out; unreadable counts them.
func Writes(procRoot string) (out map[int]int64, unreadable int) {
	out = map[int]int64{}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return out, 0
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "io"))
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				unreadable++
			}
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "write_bytes:"); ok {
				n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				out[pid] = n
			}
		}
	}
	return out, unreadable
}
