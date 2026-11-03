package loop

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/cfgpaths"
)

const (
	MaxConfigFileBytes = 256 << 10
	MaxConfigBytes     = 8 << 20
)

// configRoots, configAllow and the copy's denylist are cfgpaths', the one list the configuration copy and the
// configuration targets share.
var (
	configRoots = cfgpaths.Roots
	configAllow = cfgpaths.Allow
)

func configPathAllowed(root, path string, dir bool) bool {
	return cfgpaths.PathAllowed(root, path, dir)
}

func deniedConfigPath(root, path string) string { return cfgpaths.Denied(root, path) }

func excludedModDir(name string) bool { return cfgpaths.ExcludedDir(name) }

type ConfigCopyOptions struct {
	Home        string
	Destination string
	AsOf        time.Time
}

type ConfigFile struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	Root   string `json:"root"`
	SHA256 string `json:"sha256,omitempty"` // of the text the run reads
}

type ConfigSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type ConfigManifest struct {
	Files           []ConfigFile   `json:"files"`
	CountsByRoot    map[string]int `json:"counts_by_root"`
	Bytes           int            `json:"bytes"` // includes MANIFEST.txt
	Skipped         []ConfigSkip   `json:"skipped"`
	SkippedOmitted  int            `json:"skipped_omitted,omitempty"` // skipped paths beyond the listed ones
	SkippedByReason map[string]int `json:"skipped_by_reason"`
	Leaks           []string       `json:"leaks,omitempty"`
	Canary          bool           `json:"canary"`
}

type configCandidate struct {
	path, root, text string
}

// ConfigCopy makes /ro/config from the named roots, never from live mounts.
// Reads are confined to Home and reject links. An as-of Git root uses only
// tracked blobs at its last HEAD commit before the cutoff; other roots are leaks.
func ConfigCopy(ctx context.Context, o ConfigCopyOptions) (m ConfigManifest, err error) {
	if err = CheckScrubber(ctx); err != nil {
		return m, err
	}
	if err = absentInput(o.Destination); err != nil {
		return m, err
	}
	if o.Home == "" {
		return m, fmt.Errorf("configuration copy needs a home")
	}
	o.Home, err = filepath.Abs(o.Home)
	if err != nil {
		return m, err
	}
	if err = noInputLinks(o.Home); err != nil {
		return m, err
	}
	home, err := os.OpenRoot(o.Home)
	if err != nil {
		return m, err
	}
	defer home.Close()
	m = ConfigManifest{CountsByRoot: map[string]int{}, SkippedByReason: map[string]int{}, Canary: true}
	var candidates []configCandidate
	capped := false
	for _, root := range configRoots {
		m.CountsByRoot[root] = 0
		rootBytes := 0 // what this root has collected: a root cannot fill memory, whatever it holds
		// Validate every live path component before Git may enter the root.
		link := false
		for p := root; p != "."; p = filepath.Dir(p) {
			info, statErr := home.Lstat(p)
			if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				link = true
				break
			}
		}
		if link {
			m.skip(root, "link")
			continue
		}
		names, repo, commit, historical, listErr := configAsOf(ctx, o, root)
		if listErr != nil {
			return m, listErr
		}
		if !o.AsOf.IsZero() && !historical {
			m.Leaks = append(m.Leaks, root)
		}
		if !historical {
			names, err = liveConfigNames(home, root, &m)
			if err != nil {
				return m, err
			}
		}
		sort.Strings(names)
		cleanNames, nameErr := RedactAll(ctx, names)
		if nameErr != nil {
			return m, nameErr
		}
		for i, name := range names {
			if cleanNames[i] != name {
				m.skip(cleanNames[i], "path needs scrubbing")
				continue
			}
			if reason := deniedConfigPath(root, name); reason != "" {
				m.skip(name, reason)
				continue
			}
			if !configPathAllowed(root, name, false) {
				m.skip(name, "not configuration")
				continue
			}
			if capped {
				m.skip(name, "total bytes")
				continue
			}
			var b []byte
			reason := ""
			if historical {
				rel, relErr := filepath.Rel(repo, filepath.Join(o.Home, name))
				if relErr != nil {
					return m, relErr
				}
				b, err = hostInputGit(ctx, repo, "show", commit+":"+filepath.ToSlash(rel))
				if err != nil {
					return m, err
				}
			} else {
				b, reason, err = readConfigText(home, name)
				if err != nil {
					if ctx.Err() != nil {
						return m, ctx.Err()
					}
					m.skip(name, "unreadable")
					continue
				}
			}
			if reason == "" {
				reason = configTextReason(b)
			}
			if reason == "" && root == ".local/bin" && !scriptShebang(b) {
				reason = "not a script"
			}
			if reason != "" {
				m.skip(name, reason)
				continue
			}
			if rootBytes+len(b) > MaxConfigBytes {
				m.skip(name, "total bytes")
				continue
			}
			rootBytes += len(b)
			candidates = append(candidates, configCandidate{name, root, string(b)})
		}
	}
	// The cap is shared by the roots, not taken by the first: a root that wants less than its equal share keeps all it
	// wants and the rest is divided among the others, smallest demand first (a prefix in the fixed order would let one
	// big tree, a harness's skills, leave the later roots, the agent-sync rules among them, with nothing).
	candidates = shareConfigCap(candidates, &m)
	texts := make([]string, len(candidates))
	for i, candidate := range candidates {
		texts[i] = candidate.text
	}
	clean, err := RedactAll(ctx, texts)
	if err != nil {
		return m, err
	}
	skipPaths := make([]string, len(m.Skipped))
	for i := range m.Skipped {
		skipPaths[i] = m.Skipped[i].Path
	}
	cleanPaths, err := RedactAll(ctx, skipPaths)
	if err != nil {
		return m, err
	}
	for i := range m.Skipped {
		m.Skipped[i].Path = cleanPaths[i]
	}
	// Keep a prefix in the fixed root/path order, including the manifest in
	// the byte cap. Scrubbing can change sizes, so account after it runs.
	dataBytes := 0
	keep := 0
	for i, candidate := range candidates {
		size := len(clean[i])
		if capped = size > MaxConfigFileBytes || dataBytes+size > MaxConfigBytes; capped {
			for _, left := range candidates[i:] {
				m.skip(left.path, "total bytes")
			}
			break
		}
		m.Files = append(m.Files, ConfigFile{candidate.path, size, candidate.root, hashOf([]byte(clean[i]))})
		m.CountsByRoot[candidate.root]++
		dataBytes += size
		keep++
	}
	var manifest string
	for {
		manifest = configIndex(m)
		scrubbed, scrubErr := RedactAll(ctx, []string{manifest})
		if scrubErr != nil {
			return m, scrubErr
		}
		manifest = scrubbed[0]
		if dataBytes+len(manifest) <= MaxConfigBytes {
			break
		}
		if keep == 0 {
			return m, fmt.Errorf("configuration manifest exceeds the input limit")
		}
		keep--
		file := m.Files[keep]
		m.Files = m.Files[:keep]
		m.CountsByRoot[file.Root]--
		dataBytes -= file.Bytes
		m.skip(file.Path, "total bytes")
	}
	m.Bytes = dataBytes + len(manifest)
	tmp, err := os.MkdirTemp(filepath.Dir(o.Destination), ".config-input-")
	if err != nil {
		return m, err
	}
	defer removeInputTree(tmp)
	for i, file := range m.Files {
		path := filepath.Join(tmp, file.Path)
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return m, err
		}
		if err = writeReadonlyInput(path, []byte(clean[i])); err != nil {
			return m, err
		}
	}
	if err = writeReadonlyInput(filepath.Join(tmp, "MANIFEST.txt"), []byte(manifest)); err != nil {
		return m, err
	}
	if err = readonlyInputDirs(tmp); err != nil {
		return m, err
	}
	if err = os.Rename(tmp, o.Destination); err != nil {
		return m, err
	}
	return m, nil
}

// MaxConfigSkips is how many skipped paths the manifest lists; the counts by reason cover all of them. A tree of
// hundreds of thousands of skipped files (logs, caches) would otherwise make the manifest larger than the cap.
const MaxConfigSkips = 500

func (m *ConfigManifest) skip(path, reason string) {
	if len(m.Skipped) < MaxConfigSkips {
		m.Skipped = append(m.Skipped, ConfigSkip{path, reason})
	} else {
		m.SkippedOmitted++
	}
	m.SkippedByReason[reason]++
}

func liveConfigNames(home *os.Root, root string, m *ConfigManifest) ([]string, error) {
	var names []string
	err := fs.WalkDir(home.FS(), root, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err != nil {
			// a directory or file that cannot be read now (a permission, a file removed since the listing) is left out
			// and counted; the copy goes on
			m.skip(path, "unreadable")
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			m.skip(path, "link")
			return nil
		}
		if entry.IsDir() && excludedModDir(entry.Name()) {
			m.skip(path, "excluded directory")
			return fs.SkipDir
		}
		if reason := deniedConfigPath(root, path); reason != "" {
			m.skip(path, reason)
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !configPathAllowed(root, path, entry.IsDir()) {
			// not skipped by name one by one: a harness's sessions and logs are thousands of files
			if entry.IsDir() {
				return fs.SkipDir
			}
			m.skip(path, "not configuration")
			return nil
		}
		if entry.IsDir() {
			if root == ".local/bin" && path != root {
				m.skip(path, "not a direct script")
				return fs.SkipDir
			}
			return nil
		}
		names = append(names, path)
		return nil
	})
	return names, err
}

func readConfigText(home *os.Root, path string) ([]byte, string, error) {
	// O_NOFOLLOW applies to each component, including directory components
	// replaced after WalkDir. A substituted FIFO never waits for a writer.
	f, err := openConfigText(home, path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !st.Mode().IsRegular() {
		return nil, "not regular", nil
	}
	if st.Size() > MaxConfigFileBytes {
		return nil, "file bytes", nil
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	return b, "", err
}

func openConfigText(home *os.Root, path string) (*os.File, error) {
	if !filepath.IsLocal(path) {
		return nil, fmt.Errorf("configuration path escapes Home")
	}
	current, err := home.Open(".")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i, part := range parts {
		flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		fd, err := syscall.Openat(int(current.Fd()), part, flags, 0)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = os.NewFile(uintptr(fd), part)
	}
	return current, nil
}

func configTextReason(b []byte) string {
	if len(b) > MaxConfigFileBytes {
		return "file bytes"
	}
	if bytes.IndexByte(b[:min(len(b), 8<<10)], 0) >= 0 || !utf8.Valid(b) {
		return "not text"
	}
	return ""
}

func scriptShebang(b []byte) bool {
	line, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(line, "#!") {
		return false
	}
	words := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(words) == 0 {
		return false
	}
	program := filepath.Base(words[0])
	if program == "env" {
		words = words[1:]
		if len(words) > 0 && words[0] == "-S" {
			words = words[1:]
		}
		if len(words) == 0 {
			return false
		}
		program = words[0]
	}
	switch program {
	case "sh", "bash", "dash", "ash", "zsh", "ksh", "csh", "tcsh", "yash", "fish", "node":
		return true
	}
	if strings.HasPrefix(program, "python") {
		return strings.Trim(strings.TrimPrefix(program, "python"), "0123456789.") == ""
	}
	return false
}

// configAsOf keeps historical lookup small: find the repository, choose HEAD's
// last commit at the cutoff, and list only regular blobs below the named root.
func configAsOf(ctx context.Context, o ConfigCopyOptions, root string) ([]string, string, string, bool, error) {
	if o.AsOf.IsZero() {
		return nil, "", "", false, nil
	}
	probe := filepath.Join(o.Home, root)
	for {
		info, err := os.Lstat(probe)
		if err == nil && info.IsDir() {
			break
		}
		if probe == o.Home {
			return nil, "", "", false, nil
		}
		probe = filepath.Dir(probe)
	}
	top, err := hostInputGit(ctx, probe, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, "", "", false, nil
	}
	repo := strings.TrimSpace(string(top))
	relHome, err := filepath.Rel(o.Home, repo)
	if err != nil || relHome != "." && !filepath.IsLocal(relHome) {
		return nil, "", "", false, fmt.Errorf("as-of configuration repository lies outside Home")
	}
	head, err := branchAsOf(ctx, repo, "HEAD", o.AsOf)
	if err != nil {
		return nil, "", "", false, err
	}
	commit := strings.TrimSpace(string(head))
	if commit == "" {
		return nil, repo, "", true, nil
	}
	rel, _ := filepath.Rel(repo, filepath.Join(o.Home, root))
	tree, err := hostInputGit(ctx, repo, "ls-tree", "-r", "-z", commit, "--", filepath.ToSlash(rel))
	if err != nil {
		return nil, "", "", false, err
	}
	var names []string
	for _, record := range strings.Split(string(tree), "\x00") {
		header, path, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		if !strings.HasPrefix(header, "100644 blob ") && !strings.HasPrefix(header, "100755 blob ") {
			continue
		}
		name, relErr := filepath.Rel(o.Home, filepath.Join(repo, path))
		if relErr != nil || !filepath.IsLocal(name) {
			return nil, "", "", false, fmt.Errorf("as-of configuration path escapes Home")
		}
		names = append(names, name)
	}
	return names, repo, commit, true, nil
}

func configIndex(m ConfigManifest) string {
	var b strings.Builder
	for _, file := range m.Files {
		fmt.Fprintf(&b, "%s\t%d\n", file.Path, file.Bytes)
	}
	for _, skip := range m.Skipped {
		fmt.Fprintf(&b, "skip\t%s\t%s\n", skip.Path, skip.Reason)
	}
	if m.SkippedOmitted > 0 {
		fmt.Fprintf(&b, "skip\t(%d more paths, not listed)\t-\n", m.SkippedOmitted)
	}
	reasons := make([]string, 0, len(m.SkippedByReason))
	for reason := range m.SkippedByReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(&b, "skipped\t%s\t%d\n", reason, m.SkippedByReason[reason])
	}
	for _, root := range m.Leaks {
		fmt.Fprintf(&b, "leak\t%s\n", root)
	}
	return b.String()
}

func readonlyInputDirs(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

func writeReadonlyInput(path string, text []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(text); err == nil {
		err = f.Chmod(0o444)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func removeInputTree(dir string) {
	// Failed staging may already have read-only directories.
	filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			os.Chmod(path, 0o700)
		}
		return err
	})
	os.RemoveAll(dir)
}

// configManifestReserve is set aside of the cap for MANIFEST.txt.
const configManifestReserve = 256 << 10

// shareConfigCap keeps, per root, the prefix of its candidates (paths sorted) that fits its share of the cap, and counts the
// rest as skipped for "total bytes". The order of the kept candidates is the fixed root and path order.
func shareConfigCap(candidates []configCandidate, m *ConfigManifest) []configCandidate {
	demand := map[string]int{}
	for _, c := range candidates {
		demand[c.root] += len(c.text)
	}
	var roots []string
	for _, root := range configRoots {
		if demand[root] > 0 {
			roots = append(roots, root)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return demand[roots[i]] < demand[roots[j]] })
	remaining := MaxConfigBytes - configManifestReserve
	allowed := map[string]int{}
	for i, root := range roots {
		share := remaining / (len(roots) - i)
		allowed[root] = min(demand[root], share)
		remaining -= allowed[root]
	}
	used := map[string]int{}
	var kept []configCandidate
	for _, c := range candidates {
		if used[c.root]+len(c.text) <= allowed[c.root] {
			used[c.root] += len(c.text)
			kept = append(kept, c)
			continue
		}
		m.skip(c.path, "total bytes")
	}
	return kept
}
