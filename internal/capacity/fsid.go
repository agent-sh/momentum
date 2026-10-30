package capacity

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A path reached through a bind mount names the same file as the path through the filesystem's own mount: on a
// machine that binds /data/cache onto ~/.cache, ~/.cache/x and /data/cache/x are one directory, and /proc shows
// whichever a process used. Mounts gives every path one name, its canonical path, so a root, a process's open file
// and a unit's argument compare as strings.

// mount is one line of mountinfo: the filesystem's device, the directory of the filesystem it shows, and where.
type mount struct {
	dev, root, point string
}

// Mounts is the machine's mount table.
type Mounts struct {
	list    []mount
	primary map[string]mount // per device, the mount that shows the most of it (the shortest root), earliest first
}

// ReadMounts reads a mountinfo file (normally /proc/self/mountinfo). Without one, every path is its own name.
func ReadMounts(path string) *Mounts {
	m := &Mounts{primary: map[string]mount{}}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 5 {
			continue
		}
		mt := mount{dev: fs[2], root: unescapeMount(fs[3]), point: unescapeMount(fs[4])}
		m.list = append(m.list, mt)
		if p, ok := m.primary[mt.dev]; !ok || len(mt.root) < len(p.root) {
			m.primary[mt.dev] = mt
		}
	}
	return m
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space, \011, \012, \134).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// Canon is p's canonical name: the place on its filesystem, shown through the mount that shows the most of that
// filesystem. A path no mount holds, or one that mount does not show, keeps its name. p must be absolute; it is
// cleaned, and symlinks are not resolved (see resolve).
func (m *Mounts) Canon(p string) string {
	p = filepath.Clean(p)
	if m == nil || !filepath.IsAbs(p) {
		return p
	}
	var at mount
	found := false
	for _, mt := range m.list { // the last of the longest: a later mount on a point hides the earlier
		if under(p, mt.point) && (!found || len(mt.point) >= len(at.point)) {
			at, found = mt, true
		}
	}
	if !found {
		return p
	}
	prim, ok := m.primary[at.dev]
	if !ok || prim == at {
		return p
	}
	rest := strings.TrimPrefix(p, at.point)
	if at.point == "/" {
		rest = p
	}
	inFS := filepath.Join(at.root, rest)
	if !under(inFS, prim.root) {
		return p
	}
	return filepath.Join(prim.point, strings.TrimPrefix(inFS, prim.root))
}

// resolve is the canonical name of a path that may hold symlinks or not exist: its longest existing ancestor with
// symlinks resolved, then what follows it, through Canon.
func (m *Mounts) resolve(p string) string {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return p
	}
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return m.Canon(filepath.Join(r, rest))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return m.Canon(p)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
