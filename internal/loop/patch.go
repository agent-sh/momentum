package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Patch is a file of the run's directory, never a path of its clone.
type Patch struct {
	Dir, File string
	data      []byte
}

// Limits bound input capture and the changed blobs. Binary expansion is checked before Git applies it.
const (
	PatchMax = 16 << 20
	FileMax  = 8 << 20
	TotalMax = 32 << 20
)

func readPatch(p Patch) ([]byte, error) {
	if p.data != nil {
		return p.data, nil
	}
	if p.Dir == "" {
		return nil, refused("a patch needs the run's directory")
	}
	if p.File == "" {
		p.File = "change.patch"
	}
	if filepath.IsAbs(p.File) {
		rel, err := filepath.Rel(p.Dir, p.File)
		if err != nil {
			return nil, err
		}
		p.File = rel
	}
	f, err := OpenFile(p.Dir, p.File)
	if err != nil {
		return nil, refused("patch: %v", err)
	}
	defer f.Close()
	b, err := ReadFile(f, PatchMax)
	if err != nil {
		return nil, refused("patch: %v", err)
	}
	return b, nil
}

// applyPatch consumes the run's file once. Every later operation takes these bytes or host-owned objects.
func (s *Ship) applyPatch(ctx context.Context, p Package, inputs []Patch) (string, []string, error) {
	if p.Host == "" || p.BaseCommit == "" {
		return "", nil, refused("the package has no host clone and base commit; prepare it again")
	}
	in := Patch{}
	if len(inputs) > 1 {
		return "", nil, refused("one patch per step")
	}
	if len(inputs) == 1 {
		in = inputs[0]
	}
	data, err := readPatch(in)
	if err != nil {
		return "", nil, err
	}
	if s.AfterPatchRead != nil {
		s.AfterPatchRead()
	}
	names, err := patchPaths(data)
	if err != nil {
		return "", nil, err
	}
	base, err := s.treeEntries(ctx, p, p.BaseCommit)
	if err != nil {
		return "", nil, err
	}
	if err := checkBasePaths(base, names); err != nil {
		return "", nil, err
	}
	// Reset the index and files without moving the package's proposal branch.
	if _, err := s.git(ctx, p.Host, "read-tree", "--reset", "-u", p.BaseCommit); err != nil {
		return "", nil, err
	}
	if len(data) != 0 {
		if _, err := s.gitInput(ctx, p.Host, data, "-c", "protocol.allow=never", "apply", "--index", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return "", nil, refused("patch does not apply cleanly to its base: %v", err)
		}
	}
	// Write once. Gates and size checks below compare immutable objects, never file names of the run.
	out, err := s.git(ctx, p.Host, "write-tree")
	if err != nil {
		return "", nil, err
	}
	tree := strings.TrimSpace(out)
	changed, err := s.git(ctx, p.Host, "diff", "--name-only", "--no-renames", "-z", p.BaseCommit, tree, "--")
	if err != nil {
		return "", nil, err
	}
	paths := strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00")
	if changed == "" {
		paths = nil
	}
	result, err := s.treeEntries(ctx, p, tree)
	if err != nil {
		return "", nil, err
	}
	if err := checkChangedFiles(base, result, paths); err != nil {
		return "", nil, err
	}
	slices.Sort(paths)
	if err := os.WriteFile(filepath.Join(s.pkgDir(p.Case), "candidate.patch"), data, 0600); err != nil {
		return "", nil, err
	}
	return tree, paths, nil
}

func checkBasePaths(base map[string]treeEntry, names []string) error {
	for _, name := range names {
		parts := strings.Split(name, "/")
		for i := range parts {
			if e := base[strings.Join(parts[:i+1], "/")]; e.mode == "120000" || e.mode == "160000" {
				return refused("patch path %s has a symlink or gitlink in the base", name)
			}
		}
	}
	return nil
}

func checkChangedFiles(base, result map[string]treeEntry, paths []string) error {
	total := int64(0)
	for _, name := range paths {
		if err := patchPath(name); err != nil {
			return err
		}
		e, exists := result[name]
		if !exists {
			continue
		}
		if e.mode != "100644" && e.mode != "100755" {
			return refused("patch result %s has refused mode %s", name, e.mode)
		}
		if old, exists := base[name]; exists && old.mode != "100644" && old.mode != "100755" {
			return refused("patch changes a nonregular base file: %s", name)
		}
		total += e.size
		if e.size > FileMax || total > TotalMax {
			return refused("patch result exceeds %d bytes per file or %d bytes total", FileMax, TotalMax)
		}
	}
	return nil
}

func (s *Ship) keepPatch(p Package) error {
	return os.Rename(filepath.Join(s.pkgDir(p.Case), "candidate.patch"), filepath.Join(s.pkgDir(p.Case), "change.patch"))
}

type treeEntry struct {
	mode, oid string
	size      int64
}

func (s *Ship) treeEntries(ctx context.Context, p Package, tree string) (map[string]treeEntry, error) {
	out, err := s.git(ctx, p.Host, "ls-tree", "-r", "-l", "-z", tree)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]treeEntry)
	for _, row := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if row == "" {
			continue
		}
		meta, name, ok := strings.Cut(row, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return nil, fmt.Errorf("unreadable tree entry")
		}
		n, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil && f[1] != "commit" {
			return nil, err
		}
		entries[name] = treeEntry{f[0], f[2], n}
	}
	return entries, nil
}

func patchPath(name string) error {
	if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || path.IsAbs(name) || path.Clean(name) != name {
		return refused("patch path is not clean and relative: %q", name)
	}
	for _, c := range strings.Split(name, "/") {
		if c == "." || c == ".." || strings.EqualFold(c, ".git") {
			return refused("patch path has a refused component: %q", name)
		}
	}
	return nil
}

func unquotePath(s string) (string, error) {
	if strings.HasPrefix(s, `"`) {
		v, err := strconv.Unquote(s)
		if err != nil {
			return "", refused("invalid quoted patch path")
		}
		return v, nil
	}
	return s, nil
}

// patchPaths accepts Git's patch form and validates every extended-header path and mode before application.
// Ambiguous diff headers are refused rather than guessed. Hunk contents remain opaque data.
func patchPaths(data []byte) ([]string, error) {
	if len(data) > PatchMax || bytes.IndexByte(data, 0) >= 0 {
		return nil, refused("patch exceeds its limit or contains NUL")
	}
	if len(data) == 0 {
		return nil, nil
	}
	lines := strings.Split(string(data), "\n")
	var names []string
	var binaryTotal int64
	header, found := false, false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "diff --git ") {
			header, found = true, true
			rest := strings.TrimPrefix(line, "diff --git ")
			var left, right string
			if strings.HasPrefix(rest, `"`) {
				end := 1
				for end < len(rest) {
					if rest[end] == '\\' {
						end += 2
						continue
					}
					if rest[end] == '"' {
						break
					}
					end++
				}
				if end+1 >= len(rest) || rest[end+1] != ' ' {
					return nil, refused("invalid diff header")
				}
				left, right = rest[:end+1], rest[end+2:]
			} else {
				sep := " b/"
				if strings.Contains(rest, ` "b/`) {
					sep = ` "b/`
				}
				var ok bool
				left, right, ok = strings.Cut(rest, sep)
				if !ok || strings.Contains(right, " b/") || strings.Contains(right, ` "b/`) {
					return nil, refused("ambiguous diff header")
				}
				right = strings.TrimPrefix(sep, " ") + right
			}
			for j, raw := range []string{left, right} {
				name, err := unquotePath(raw)
				if err != nil {
					return nil, err
				}
				prefix := []string{"a/", "b/"}[j]
				if !strings.HasPrefix(name, prefix) {
					return nil, refused("diff paths need a/ and b/ prefixes")
				}
				name = strings.TrimPrefix(name, prefix)
				if err := patchPath(name); err != nil {
					return nil, err
				}
				names = append(names, name)
			}
			continue
		}
		if line == "GIT binary patch" {
			header = false
			var err error
			var size int64
			i, size, err = checkBinary(lines, i+1)
			if err != nil {
				return nil, err
			}
			binaryTotal += size
			if binaryTotal > TotalMax {
				return nil, refused("binary patch expands beyond the total file limit")
			}
			continue
		}
		if strings.HasPrefix(line, "@@") {
			header = false
		}
		if !header {
			continue
		}
		for _, prefix := range []string{"--- ", "+++ ", "rename from ", "rename to ", "copy from ", "copy to "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			name, err := unquotePath(strings.TrimPrefix(line, prefix))
			if err != nil {
				return nil, err
			}
			if (prefix == "--- " || prefix == "+++ ") && name == "/dev/null" {
				continue
			}
			if prefix == "--- " || prefix == "+++ " {
				pre := "a/"
				if prefix == "+++ " {
					pre = "b/"
				}
				if !strings.HasPrefix(name, pre) {
					return nil, refused("patch file path lacks its prefix")
				}
				name = strings.TrimPrefix(name, pre)
			}
			if err := patchPath(name); err != nil {
				return nil, err
			}
			names = append(names, name)
		}
		for _, prefix := range []string{"new file mode ", "deleted file mode ", "old mode ", "new mode "} {
			if strings.HasPrefix(line, prefix) {
				mode := strings.TrimPrefix(line, prefix)
				if mode != "100644" && mode != "100755" {
					return nil, refused("patch has refused mode %s", mode)
				}
			}
		}
		if strings.HasPrefix(line, "index ") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[2] != "100644" && fields[2] != "100755" {
				return nil, refused("patch index has refused mode %s", fields[2])
			}
		}
	}
	if !found {
		return nil, refused("expected a Git diff against the recorded base")
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}
