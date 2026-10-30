package capacity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Tree is what a walk of a file or directory found: the bytes it holds on disk (each inode once, as du counts
// them), its newest write (the later of a modification and a status change, of any entry), and how many entries.
// A status change counts as a write: a file moved or copied in with its old modification time is new here.
type Tree struct {
	Bytes  int64
	Newest time.Time
	Files  int
	Errs   int // entries that could not be read
}

type inode struct{ dev, ino uint64 }

// Measure walks p without following symlinks or leaving its filesystem. seen holds the inodes counted already, so
// a hardlink counts once across walks that share it; nil counts each walk on its own.
func Measure(p string, seen map[inode]bool) Tree {
	if seen == nil {
		seen = map[inode]bool{}
	}
	var t Tree
	st, err := os.Lstat(p)
	if err != nil {
		t.Errs++
		return t
	}
	root, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return t
	}
	count := func(fi os.FileInfo) {
		s, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return
		}
		t.Files++
		if w := newest(s); w.After(t.Newest) {
			t.Newest = w
		}
		k := inode{uint64(s.Dev), s.Ino}
		if !seen[k] {
			seen[k] = true
			t.Bytes += s.Blocks * 512
		}
	}
	count(st)
	if !st.IsDir() {
		return t
	}
	stack := []string{p}
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Errs++
			continue
		}
		for _, e := range ents {
			fi, err := e.Info()
			if err != nil {
				continue // gone since the listing
			}
			count(fi)
			if e.IsDir() {
				if s, ok := fi.Sys().(*syscall.Stat_t); ok && s.Dev == root.Dev {
					stack = append(stack, filepath.Join(d, e.Name()))
				}
			}
		}
	}
	return t
}

// newest is the later of an inode's modification and status change.
func newest(s *syscall.Stat_t) time.Time {
	m := time.Unix(s.Mtim.Sec, s.Mtim.Nsec)
	if c := time.Unix(s.Ctim.Sec, s.Ctim.Nsec); c.After(m) {
		return c
	}
	return m
}

// newerThan reports whether any directory under p (p among them) changed after t: creating, removing or renaming
// an entry changes its directory. It reads directories only, so it is the cheap first look before Measure.
func newerThan(p string, t time.Time) bool {
	stack := []string{p}
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		st, err := os.Lstat(d)
		if err != nil {
			continue
		}
		if s, ok := st.Sys().(*syscall.Stat_t); ok && newest(s).After(t) {
			return true
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				stack = append(stack, filepath.Join(d, e.Name()))
			}
		}
	}
	return false
}

// cargoTag is the start of the CACHEDIR.TAG every Cargo target directory holds (and Cargo's registry cache too,
// which a target directory is told from by its .rustc_info.json).
const cargoTag = "Signature: 8a477f597d28d172789f06886806bc55"

// isTarget reports whether dir is a Cargo target directory: Cargo's cache tag and its .rustc_info.json.
func isTarget(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "CACHEDIR.TAG"))
	if err != nil || !bytes.HasPrefix(b, []byte(cargoTag)) || !bytes.Contains(b, []byte("cargo")) {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, ".rustc_info.json"))
	return err == nil
}

// isLinkedWorktree reports whether dir is a linked git worktree: its .git is a file pointing at an administrative
// directory that names a common directory (a submodule's .git file points at no such directory).
func isLinkedWorktree(dir string) (gitdir string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", false
	}
	g, found := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !found {
		return "", false
	}
	if !filepath.IsAbs(g) {
		g = filepath.Join(dir, g)
	}
	if _, err := os.Stat(filepath.Join(g, "commondir")); err != nil {
		return g, false
	}
	return g, true
}

// skipDirs are never walked into: git's own store and package trees hold no target or worktree of their own.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "site-packages": true}

// Found is what a discovery walk found under the roots.
type Found struct {
	Targets   []string
	Worktrees []string // linked worktrees, with a .git file
	Dirs      int
	Took      time.Duration
}

// Discover walks the roots once, without following symlinks or leaving each root's filesystem, for Cargo target
// directories (it does not walk into one) and linked worktrees (it walks into them, for their targets). A path
// excluded by skip is not walked. wantTargets and wantWorktrees say under which paths each counts.
func Discover(roots []string, wantTargets, wantWorktrees func(string) bool, skip func(string) bool) Found {
	start := time.Now()
	var f Found
	seen := map[string]bool{}
	for _, r := range roots {
		st, err := os.Lstat(r)
		if err != nil || !st.IsDir() {
			continue
		}
		dev := st.Sys().(*syscall.Stat_t).Dev
		stack := []string{r}
		for len(stack) > 0 {
			d := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[d] {
				continue
			}
			seen[d] = true
			f.Dirs++
			if d != r && wantTargets(d) && isTarget(d) {
				f.Targets = append(f.Targets, d)
				continue
			}
			ents, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if e.Name() == ".git" && e.Type().IsRegular() && wantWorktrees(d) {
					if _, ok := isLinkedWorktree(d); ok {
						f.Worktrees = append(f.Worktrees, d)
					}
				}
				if !e.IsDir() || skipDirs[e.Name()] {
					continue
				}
				p := filepath.Join(d, e.Name())
				if skip != nil && skip(p) {
					continue
				}
				if fi, err := e.Info(); err == nil {
					if s, ok := fi.Sys().(*syscall.Stat_t); ok && s.Dev != dev {
						continue
					}
				}
				stack = append(stack, p)
			}
		}
	}
	f.Took = time.Since(start)
	return f
}
