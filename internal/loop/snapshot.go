package loop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A run gets a repository as a snapshot of its files, not as a clone. The files are the default branch's tracked
// text files at one commit, read from Git's object store without a checkout (no filter, hook or attribute runs),
// each scrubbed in place: what the scrubber would change is masked, never a reason to leave the repository out.
// The run gets no object database. History comes as text: a bounded commit log, and per case the diffs of the
// commits that touched the paths the case's evidence names.
const (
	SnapshotFileMax  = 1 << 20   // a larger tracked file is left out of a snapshot
	SnapshotRepoMax  = 256 << 20 // bytes of tracked files in one snapshot
	HistoryCommits   = 500       // commits in a log
	HistoryWindow    = StoreWindow
	DiffCommits      = 30        // commits whose diffs a run gets, per repository
	DiffMax          = 128 << 10 // bytes of one commit's diff
	DiffsMax         = 2 << 20   // bytes of all diffs of a run
	DiffPaths        = 25        // evidence paths followed, per repository
	snapshotChunk    = 4 << 20   // bytes of files scrubbed in one call
	snapshotChunkLen = 4000
	treeListMax      = 128 << 20
	logOutputMax     = 32 << 20
	logFilesPerEntry = 100
)

// RepoInput is what one repository (or tacit's store) gave a pass, for the manifest. Counts only: what the scrubber
// masked is never listed.
type RepoInput struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
	Error  string `json:"error,omitempty"`

	Files  int   `json:"files,omitempty"`  // tracked files in the snapshot
	Bytes  int64 `json:"bytes,omitempty"`  // their size after scrubbing
	Masked int   `json:"masked,omitempty"` // files whose text the scrubber changed
	// Skipped counts tracked files left out, by reason: binary, large, link, path, total.
	Skipped map[string]int  `json:"skipped,omitempty"`
	History HistoryManifest `json:"history"`

	tracked []string // the snapshot's paths, to match a case's evidence paths against
}

// HistoryManifest says what the commit log gave a run: the commits, its size, and how many entries the scrubber changed.
type HistoryManifest struct {
	Commits   int    `json:"commits"`
	Bytes     int64  `json:"bytes,omitempty"`
	Masked    int    `json:"masked,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DiffsManifest says what a run got of history for its case: the diffs of commits that touched its evidence paths.
type DiffsManifest struct {
	Paths   int    `json:"paths"`
	Commits int    `json:"commits"`
	Bytes   int64  `json:"bytes,omitempty"`
	Masked  int    `json:"masked,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SnapshotOptions names the host sources of a repository snapshot. Destination holds one directory per repository
// and LogDir one NAME.log; both must be absent or empty. LoopDirs are the caller's host-only loop storage roots.
type SnapshotOptions struct {
	Sources     []string
	Destination string
	LogDir      string
	LoopDirs    []string
	Cache       *ScrubCache
	Since       time.Time // the log's window starts here
}

// SnapshotRepos makes the scrubbed snapshot of each distinct source for /ro/repos/NAME and its log for
// /ro/history/repos/NAME.log. A source that cannot be read (missing, no work tree, a link, inside loop storage, no
// default branch) is a result with its reason, not a failure. Only a scrubber that does not run stops the pass.
func SnapshotRepos(ctx context.Context, o SnapshotOptions) ([]RepoInput, error) {
	if err := CheckScrubber(ctx); err != nil {
		return nil, err
	}
	if o.Destination == "" || o.LogDir == "" {
		return nil, fmt.Errorf("repository destination is empty")
	}
	clean, err := RedactAll(ctx, []string{o.Destination})
	if err != nil {
		return nil, err
	}
	if clean[0] != o.Destination {
		return nil, fmt.Errorf("repository destination needs scrubbing")
	}
	for _, dir := range []string{o.Destination, o.LogDir} {
		if err := noInputLinks(filepath.Dir(dir)); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	var sources []string
	seen, counts := map[string]bool{}, map[string]int{}
	for _, source := range o.Sources {
		p, err := filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			sources = append(sources, p)
			counts[filepath.Base(p)]++
		}
	}
	var results []RepoInput
	for _, source := range sources {
		name := filepath.Base(source)
		if counts[name] > 1 {
			hash := sha256.Sum256([]byte(source))
			name += fmt.Sprintf("-%x", hash[:4])
		}
		result := RepoInput{Name: name, Source: source}
		cleanPaths, err := RedactAll(ctx, []string{name, source})
		if err != nil {
			return results, err
		}
		if cleanPaths[0] != name || cleanPaths[1] != source {
			result.Name, result.Source = cleanPaths[0], cleanPaths[1]
			result.Error = "repository paths need scrubbing"
			results = append(results, result)
			continue
		}
		if err := cloneSource(ctx, source, append(append([]string(nil), o.LoopDirs...), o.Destination, o.LogDir)); err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result, err = snapshotOne(ctx, source, filepath.Join(o.Destination, name), filepath.Join(o.LogDir, name+".log"), SnapshotRepoMax, o.Cache, o.Since)
		result.Name = name
		if err != nil {
			if isScrubberError(err) {
				return results, err
			}
			result.Error = reason(err)
		}
		results = append(results, result)
	}
	return results, nil
}

// SnapshotFacts is SnapshotRepos for tacit's store: one source, at dst, its log at logPath.
func SnapshotFacts(ctx context.Context, src, dst, logPath string, cache *ScrubCache, since time.Time) (RepoInput, error) {
	result := RepoInput{Source: src, Name: filepath.Base(src)}
	if err := CheckScrubber(ctx); err != nil {
		return result, err
	}
	if src == "" || dst == "" {
		return result, fmt.Errorf("tacit snapshot needs source and destination paths")
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return result, err
	}
	dst, err = filepath.Abs(dst)
	if err != nil {
		return result, err
	}
	result.Source, result.Name = src, filepath.Base(src)
	cleanPaths, err := RedactAll(ctx, []string{src, dst})
	if err != nil {
		return result, err
	}
	if cleanPaths[0] != src || cleanPaths[1] != dst {
		result.Source = cleanPaths[0]
		result.Name = filepath.Base(cleanPaths[0])
		result.Error = "tacit paths need scrubbing"
		return result, fmt.Errorf("%s", result.Error)
	}
	if err := cloneSource(ctx, src, []string{dst, logPath}); err != nil {
		result.Error = err.Error()
		return result, err
	}
	result, err = snapshotOne(ctx, src, dst, logPath, SnapshotRepoMax, cache, since)
	if err != nil {
		result.Error = reason(err)
	}
	return result, err
}

func isScrubberError(err error) bool {
	return errors.Is(err, ErrNoScrubber)
}

// defaultCommit resolves the source's default branch to one commit.
func defaultCommit(ctx context.Context, source string) (branch, commit string, err error) {
	branch, err = cloneDefaultBranch(ctx, source)
	if err != nil {
		return "", "", err
	}
	out, err := hostInputGit(ctx, source, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		out, err = hostInputGit(ctx, source, "rev-parse", "--verify", "refs/remotes/origin/"+branch+"^{commit}")
	}
	if err != nil {
		return "", "", err
	}
	return branch, strings.TrimSpace(string(out)), nil
}

type snapEntry struct {
	path, sha string
	size      int64
}

// snapshotOne writes the snapshot of source's default branch at dst (published at the end, in one rename) and its
// log at logPath. Files are counted as they are written; r describes what was written even when err is not nil.
func snapshotOne(ctx context.Context, source, dst, logPath string, maxBytes int64, cache *ScrubCache, since time.Time) (r RepoInput, err error) {
	r = RepoInput{Source: source, Name: filepath.Base(dst), Skipped: map[string]int{}}
	if err = absentInput(dst); err != nil {
		return r, err
	}
	if err = noInputLinks(filepath.Dir(dst)); err != nil {
		return r, err
	}
	branch, commit, err := defaultCommit(ctx, source)
	if err != nil {
		return r, err
	}
	r.Branch, r.Commit = branch, commit
	entries, err := listTree(ctx, source, commit, cache, &r)
	if err != nil {
		return r, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), ".repo-input-")
	if err != nil {
		return r, err
	}
	defer removeInputTree(tmp)
	tree := filepath.Join(tmp, "tree")
	if err = os.Mkdir(tree, 0o755); err != nil {
		return r, err
	}
	if err = writeSnapshot(ctx, source, tree, entries, maxBytes, cache, &r); err != nil {
		return r, err
	}
	if err = os.Rename(tree, dst); err != nil {
		return r, err
	}
	r.Path = dst
	for _, k := range []string{"binary", "large", "link", "path", "total"} {
		if r.Skipped[k] == 0 {
			delete(r.Skipped, k)
		}
	}
	if len(r.Skipped) == 0 {
		r.Skipped = nil
	}
	r.History = snapshotLog(ctx, source, commit, logPath, since, cache)
	return r, nil
}

// listTree lists the commit's regular files. Links and submodules, and paths the scrubber would change or that
// could leave the tree, are counted and left out; the size cap is applied here, the total cap while writing.
func listTree(ctx context.Context, source, commit string, cache *ScrubCache, r *RepoInput) ([]snapEntry, error) {
	out, err := hostInputGitLimit(ctx, source, treeListMax, "ls-tree", "-r", "-z", "--long", commit)
	if err != nil {
		return nil, err
	}
	var entries []snapEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		fields := strings.Fields(string(rec[:max(tab, 0)]))
		if tab < 0 || len(fields) != 4 {
			return nil, fmt.Errorf("invalid tree listing")
		}
		mode, typ, sha, sizeText := fields[0], fields[1], fields[2], fields[3]
		switch {
		case typ != "blob" || mode == "120000":
			r.Skipped["link"]++
			continue
		case mode != "100644" && mode != "100755":
			r.Skipped["link"]++
			continue
		}
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid tree listing")
		}
		if size > SnapshotFileMax {
			r.Skipped["large"]++
			continue
		}
		entries = append(entries, snapEntry{path: string(rec[tab+1:]), sha: sha, size: size})
	}
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	clean, err := redactGroups(ctx, cache, "path", paths, 1)
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for i, e := range entries {
		if clean[i] != e.path || !filepath.IsLocal(e.path) || hasGitComponent(e.path) {
			r.Skipped["path"]++
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

func hasGitComponent(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

type pendingFile struct{ path, text string }

// writeSnapshot streams the blobs from one git process and scrubs and writes them in chunks, so memory stays at a
// chunk whatever the repository's size.
func writeSnapshot(ctx context.Context, source, tree string, entries []snapEntry, maxBytes int64, cache *ScrubCache, r *RepoInput) error {
	if len(entries) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*gitStepTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append(append([]string{"-C", source}, GitSafety()...), "cat-file", "--batch")...)
	cmd.Env = GitEnv()
	stderr := Capture{Limit: ScrubBatchBytes}
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("repository object reader failed")
	}
	go func() {
		w := bufio.NewWriter(stdin)
		for _, e := range entries {
			fmt.Fprintln(w, e.sha)
		}
		w.Flush()
		stdin.Close()
	}()
	defer func() { cancel(); stdout.Close(); cmd.Wait() }()
	reader := bufio.NewReader(stdout)
	var chunk []pendingFile
	size := 0
	var total int64
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		texts := make([]string, len(chunk))
		for i, f := range chunk {
			texts[i] = f.text
		}
		clean, err := redactGroups(ctx, cache, "file", texts, 1)
		if err != nil {
			return err
		}
		for i, f := range chunk {
			if clean[i] != f.text {
				r.Masked++
			}
			if err := writeSnapshotFile(tree, f.path, clean[i]); err != nil {
				return err
			}
			r.Files++
			r.Bytes += int64(len(clean[i]))
			r.tracked = append(r.tracked, f.path)
		}
		chunk, size = nil, 0
		return nil
	}
	for _, e := range entries {
		header, err := reader.ReadString('\n')
		if err != nil || len(header) > 256 {
			return fmt.Errorf("invalid repository object header")
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return fmt.Errorf("repository object is missing")
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || n < 0 || n > SnapshotFileMax {
			return fmt.Errorf("invalid repository object size")
		}
		data := make([]byte, n)
		if _, err = io.ReadFull(reader, data); err != nil {
			return fmt.Errorf("incomplete repository object")
		}
		if end, err := reader.ReadByte(); err != nil || end != '\n' {
			return fmt.Errorf("invalid repository object boundary")
		}
		switch {
		case bytes.IndexByte(data, 0) >= 0 || bytes.IndexByte(data, 0x1e) >= 0 || !utf8.Valid(data):
			r.Skipped["binary"]++
			continue
		case total+int64(n) > maxBytes:
			r.Skipped["total"]++
			continue
		}
		total += int64(n)
		chunk = append(chunk, pendingFile{e.path, string(data)})
		size += n
		if size >= snapshotChunk || len(chunk) >= snapshotChunkLen {
			if err = flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func writeSnapshotFile(tree, rel, text string) error {
	full := filepath.Join(tree, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(f, text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// snapshotLog writes the commit log of the window: one entry per commit with its date, author, subject and, per
// file, the lines added and removed. Entries are scrubbed one by one. A log that cannot be made is reported in the
// manifest and leaves the snapshot as it is.
func snapshotLog(ctx context.Context, source, commit, logPath string, since time.Time, cache *ScrubCache) (h HistoryManifest) {
	args := []string{"-c", "core.quotepath=false", "log", "-n", strconv.Itoa(HistoryCommits), "--no-renames", "--numstat", "--no-color",
		"--no-ext-diff", "--no-textconv", "--date=iso-strict", "--format=%x01%H %aI %an%n%s"}
	if !since.IsZero() {
		args = append(args, "--since="+since.UTC().Format(time.RFC3339))
	}
	args = append(args, commit, "--")
	out, over, err := hostInputGitCapped(ctx, source, logOutputMax, args...)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	records := strings.Split(string(out), "\x01")[1:]
	if over && len(records) > 0 {
		records, h.Truncated = records[:len(records)-1], true // the last entry is cut
	}
	texts := make([]string, 0, len(records))
	for _, rec := range records {
		texts = append(texts, logEntry(rec))
	}
	clean, err := redactGroups(ctx, cache, "log", texts, 1)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	var b strings.Builder
	for i, t := range clean {
		if t != texts[i] {
			h.Masked++
		}
		b.WriteString(t)
	}
	h.Commits, h.Bytes = len(clean), int64(b.Len())
	if err = writeLog(logPath, b.String()); err != nil {
		return HistoryManifest{Error: err.Error()}
	}
	return h
}

func writeLog(logPath, text string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(f, text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// logEntry renders one git log record (header, subject, blank line, numstat lines) as one entry.
func logEntry(rec string) string {
	lines := strings.Split(strings.TrimRight(rec, "\n"), "\n")
	var b strings.Builder
	subject := ""
	if len(lines) > 1 {
		subject = lines[1]
	}
	var files []string
	for _, l := range lines[min(2, len(lines)):] {
		if l == "" {
			continue
		}
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "-" {
			files = append(files, "  binary "+parts[2])
		} else {
			files = append(files, fmt.Sprintf("  +%s -%s %s", parts[0], parts[1], parts[2]))
		}
	}
	header := lines[0]
	if len(header) >= 40 {
		header = header[:12] + header[40:] // the first twelve characters of the hash
	}
	fmt.Fprintf(&b, "%s: %s\n  %d files changed\n", header, subject, len(files))
	for i, f := range files {
		if i == logFilesPerEntry {
			fmt.Fprintf(&b, "  ... and %d more files\n", len(files)-i)
			break
		}
		b.WriteString(f + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

var pathToken = regexp.MustCompile(`[A-Za-z0-9_.@+~/\-]{3,}`)

// EvidencePaths are the tracked files of each snapshot that the case's texts name: by absolute path under the
// repository, by a path ending the file's path, or by a file name only one file has. It returns paths by repository
// name, at most DiffPaths each, in the order first named.
func EvidencePaths(repos []RepoInput, texts []string) map[string][]string {
	found := map[string][]string{}
	for _, repo := range repos {
		if repo.Error != "" || len(repo.tracked) == 0 {
			continue
		}
		set := map[string]bool{}
		byBase := map[string][]string{}
		for _, p := range repo.tracked {
			set[p] = true
			byBase[path.Base(p)] = append(byBase[path.Base(p)], p)
		}
		seen := map[string]bool{}
		add := func(p string) {
			if !seen[p] && len(found[repo.Name]) < DiffPaths {
				seen[p] = true
				found[repo.Name] = append(found[repo.Name], p)
			}
		}
		for _, text := range texts {
			for _, tok := range pathToken.FindAllString(text, -1) {
				tok = strings.TrimPrefix(tok, "./")
				tok = strings.TrimRight(tok, ".-")
				if rest, ok := strings.CutPrefix(tok, repo.Source+"/"); ok {
					tok = rest
				}
				if tok == "" {
					continue
				}
				if set[tok] {
					add(tok)
					continue
				}
				cands := byBase[path.Base(tok)]
				if strings.Contains(tok, "/") {
					for _, c := range cands {
						if strings.HasSuffix(c, "/"+tok) {
							add(c)
						}
					}
				} else if len(cands) == 1 && strings.Contains(tok, ".") && len(tok) >= 4 {
					add(cands[0])
				}
			}
		}
	}
	return found
}

// SnapshotDiffs writes, per repository, the diffs of the recent commits that touched the paths the case's texts name,
// scrubbed, as dst/NAME/NNN-COMMIT.diff (newest first). A diff over DiffMax is cut. The host reads the source's Git objects with
// the program pins; the run gets only these files.
func SnapshotDiffs(ctx context.Context, repos []RepoInput, texts []string, dst string, since time.Time, cache *ScrubCache) (m DiffsManifest, err error) {
	if err = os.MkdirAll(dst, 0o755); err != nil {
		return m, err
	}
	paths := EvidencePaths(repos, texts)
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		var repo RepoInput
		for _, r := range repos {
			if r.Name == name {
				repo = r
			}
		}
		m.Paths += len(paths[name])
		args := []string{"--literal-pathspecs", "log", "-n", strconv.Itoa(DiffCommits), "--no-renames", "--format=%H"}
		if !since.IsZero() {
			args = append(args, "--since="+since.UTC().Format(time.RFC3339))
		}
		args = append(args, repo.Commit, "--")
		listed, lerr := hostInputGit(ctx, repo.Source, append(args, paths[name]...)...)
		if lerr != nil {
			m.Error = lerr.Error()
			continue
		}
		var texts, labels []string
		for _, hash := range strings.Fields(string(listed)) {
			show := []string{"--literal-pathspecs", "-c", "core.quotepath=false", "show", "--no-ext-diff", "--no-textconv", "--no-color", "--patch", "--date=iso-strict",
				"--format=commit %H%n%aI %an%n%s", hash, "--"}
			diff, over, derr := hostInputGitCapped(ctx, repo.Source, DiffMax, append(show, paths[name]...)...)
			if derr != nil {
				m.Error = derr.Error()
				continue
			}
			text := string(diff)
			if over {
				if i := strings.LastIndexByte(text, '\n'); i > 0 {
					text = text[:i+1]
				}
				text += fmt.Sprintf("[cut: this diff is larger than %d KiB]\n", DiffMax>>10)
			}
			if total+int64(len(text)) > DiffsMax {
				break
			}
			total += int64(len(text))
			texts, labels = append(texts, text), append(labels, hash[:12])
		}
		if len(texts) == 0 {
			continue
		}
		clean, cerr := redactGroups(ctx, cache, "diff", texts, 1)
		if cerr != nil {
			return m, cerr
		}
		if err = os.MkdirAll(filepath.Join(dst, name), 0o755); err != nil {
			return m, err
		}
		for i, t := range clean {
			if t != texts[i] {
				m.Masked++
			}
			if err = writeLog(filepath.Join(dst, name, fmt.Sprintf("%03d-%s.diff", i+1, labels[i])), t); err != nil {
				return m, err
			}
			m.Commits++
			m.Bytes += int64(len(t))
		}
	}
	return m, nil
}

// reason is an error's text for a manifest: an operating-system error names its operation and cause, never the host
// path it failed on.
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	return err.Error()
}
