package loop

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapOptions(parent string, sources ...string) SnapshotOptions {
	return SnapshotOptions{Sources: sources, Destination: filepath.Join(parent, "snap"), LogDir: filepath.Join(parent, "logs")}
}

// treeText is every file under dir with its path and mode, to look for a marker anywhere in what a run could read.
func treeText(t *testing.T, dir string) (all string, files int) {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b.WriteString(rel + "\n")
		if d.Type().IsRegular() {
			files++
			text, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b.Write(text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String(), files
}

// markedRepo is a repository whose history and current files hold the scrubber's harmless marker, and which has a
// binary file, an oversized file, a link, a file named with the marker and a commit message with the marker: the
// shapes that made the whole repository refused when it was cloned.
func markedRepo(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, dir, nil, "init", "-b", "main")
	write(t, filepath.Join(dir, "secret.txt"), "token marker-7f3a\n")
	write(t, filepath.Join(dir, "keep.txt"), "plain\n")
	inputTestGit(t, dir, nil, "add", ".")
	inputTestGit(t, dir, nil, "commit", "-m", "first marker-7f3a")
	write(t, filepath.Join(dir, "secret.txt"), "token marker-7f3a again\n")
	write(t, filepath.Join(dir, "sub", "inner.go"), "package sub\n")
	write(t, filepath.Join(dir, "name-marker-7f3a.txt"), "plain\n")
	write(t, filepath.Join(dir, "bin.dat"), "a\x00b")
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", SnapshotFileMax+1))
	if err := os.Symlink("keep.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, dir, nil, "add", ".")
	inputTestGit(t, dir, nil, "commit", "-m", "second")
	return dir
}

func TestSnapshotMasksWhatTheScrubberChangesInsteadOfRefusing(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := markedRepo(t, parent, "sample")
	got, err := SnapshotRepos(context.Background(), snapOptions(parent, src))
	if err != nil || len(got) != 1 {
		t.Fatalf("snapshot %v: %v", got, err)
	}
	r := got[0]
	if r.Error != "" || r.Path == "" || r.Branch != "main" || r.Commit != inputTestGit(t, src, nil, "rev-parse", "HEAD") {
		t.Fatalf("repository refused or misdescribed: %+v", r)
	}
	if r.Files != 3 || r.Masked != 1 || r.Skipped["binary"] != 1 || r.Skipped["large"] != 1 || r.Skipped["link"] != 1 || r.Skipped["path"] != 1 {
		t.Fatalf("counts: files %d masked %d skipped %v", r.Files, r.Masked, r.Skipped)
	}
	if r.History.Commits != 2 || r.History.Masked != 2 || r.History.Error != "" {
		t.Fatalf("history %+v", r.History)
	}
	tree, files := treeText(t, r.Path)
	if files != 3 || strings.Contains(tree, "marker-7f3a") || !strings.Contains(tree, "token [masked] again") || strings.Contains(tree, ".git") {
		t.Fatalf("tree: %d files\n%s", files, tree)
	}
	log, err := os.ReadFile(filepath.Join(filepath.Dir(r.Path), "..", "logs", "sample.log"))
	if err != nil || strings.Contains(string(log), "marker-7f3a") || !strings.Contains(string(log), "first [masked]") || !strings.Contains(string(log), "+1 -1 secret.txt") {
		t.Fatalf("log: %v\n%s", err, log)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "marker-7f3a") {
		t.Fatalf("manifest carries a marker: %s", b)
	}
	info, err := os.Stat(filepath.Join(r.Path, "keep.txt"))
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("snapshot file is writable: %v %v", info, err)
	}
	if text, _ := os.ReadFile(filepath.Join(r.Path, "keep.txt")); string(text) != "plain\n" {
		t.Fatal("an untouched file changed")
	}
}

func TestSnapshotNamesRefusalsAndKeepsTheRest(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	a := inputRepo(t, filepath.Join(parent, "a"), "sample")
	b := inputRepo(t, filepath.Join(parent, "b"), "sample")
	notRepo := filepath.Join(parent, "plain")
	if err := os.Mkdir(notRepo, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	own := inputRepo(t, filepath.Join(parent, "loop"), "own")
	o := snapOptions(parent, a, b, a, link, notRepo, own)
	o.LoopDirs = []string{filepath.Join(parent, "loop")}
	got, err := SnapshotRepos(context.Background(), o)
	if err != nil || len(got) != 5 {
		t.Fatalf("results %d: %v", len(got), err)
	}
	for i, r := range got {
		if i >= 2 {
			if r.Error == "" || r.Path != "" {
				t.Fatalf("refused source snapshotted: %+v", r)
			}
			continue
		}
		if r.Error != "" || r.Branch != "main" || !strings.HasPrefix(r.Name, "sample-") || r.Name == got[1-i].Name || r.Files != 1 {
			t.Fatalf("metadata %+v", r)
		}
		if text, err := os.ReadFile(filepath.Join(r.Path, "file")); err != nil || string(text) != "second\n" {
			t.Fatal("default branch contents differ")
		}
		if r.History.Commits != 2 {
			t.Fatalf("log has %d commits, want 2", r.History.Commits)
		}
	}
}

func TestSnapshotFactsIsMaskedNotAbsent(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := markedRepo(t, parent, "facts")
	dst := filepath.Join(parent, "clone")
	r, err := SnapshotFacts(context.Background(), src, dst, filepath.Join(parent, "history", "facts.log"), nil, defaultSince())
	if err != nil || r.Error != "" || r.Commit == "" || r.Path != dst || r.Files != 3 || r.Masked != 1 {
		t.Fatalf("facts %+v: %v", r, err)
	}
	if tree, _ := treeText(t, dst); strings.Contains(tree, "marker-7f3a") {
		t.Fatal("facts snapshot kept a marker")
	}
	if got := (InputsManifest{Tacit: r}).Summary(); strings.Contains(got, "tacit absent") || !strings.Contains(got, "tacit 3 files") {
		t.Fatalf("summary: %s", got)
	}
}

func TestSnapshotWithoutAScrubberWritesNothing(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := markedRepo(t, parent, "sample")
	Scrubber = nil
	o := snapOptions(parent, src)
	if _, err := SnapshotRepos(context.Background(), o); err == nil {
		t.Fatal("repository snapshotted without a scrubber")
	}
	if _, err := os.Lstat(o.Destination); !os.IsNotExist(err) {
		t.Fatal("a missing scrubber wrote the destination")
	}
	if _, err := SnapshotFacts(context.Background(), src, filepath.Join(parent, "facts"), filepath.Join(parent, "facts.log"), nil, defaultSince()); err == nil {
		t.Fatal("facts snapshotted without a scrubber")
	}
}

func TestSnapshotNeverFollowsLinksOutOfTheTree(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := inputRepo(t, parent, "sample")
	outside := filepath.Join(parent, "outside.txt")
	write(t, outside, "outside\n")
	inputTestGit(t, src, nil, "checkout", "main")
	if err := os.Symlink(outside, filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, src, nil, "add", "escape")
	inputTestGit(t, src, nil, "commit", "-m", "link")
	got, err := SnapshotRepos(context.Background(), snapOptions(parent, src))
	if err != nil || len(got) != 1 || got[0].Error != "" || got[0].Skipped["link"] != 1 {
		t.Fatalf("link not skipped: %+v %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(got[0].Path, "escape")); !os.IsNotExist(err) {
		t.Fatal("a link entered the snapshot")
	}
}

func TestEvidencePathsAndDiffsFollowOnlyNamedFiles(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := filepath.Join(parent, "sample")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, src, nil, "init", "-b", "main")
	write(t, filepath.Join(src, "pkg", "a.go"), "one\n")
	write(t, filepath.Join(src, "pkg", "b.go"), "x\n")
	inputTestGit(t, src, nil, "add", ".")
	inputTestGit(t, src, nil, "commit", "-m", "add both")
	write(t, filepath.Join(src, "pkg", "a.go"), "one\ntwo marker-7f3a\n")
	inputTestGit(t, src, nil, "add", ".")
	inputTestGit(t, src, nil, "commit", "-m", "edit a")
	write(t, filepath.Join(src, "pkg", "b.go"), "x\ny\n")
	inputTestGit(t, src, nil, "add", ".")
	inputTestGit(t, src, nil, "commit", "-m", "edit b")
	write(t, filepath.Join(src, "pkg", "a.go"), "one\ntwo marker-7f3a\n"+strings.Repeat("long line of text\n", 20000))
	inputTestGit(t, src, nil, "add", ".")
	inputTestGit(t, src, nil, "commit", "-m", "grow a")
	repos, err := SnapshotRepos(context.Background(), snapOptions(parent, src))
	if err != nil || repos[0].Error != "" {
		t.Fatalf("snapshot %v: %v", repos, err)
	}
	texts := []string{"panic at pkg/a.go:42", "unrelated words and a version 1.2.3 plus https://example.com/x.html"}
	if got := EvidencePaths(repos, texts)["sample"]; len(got) != 1 || got[0] != "pkg/a.go" {
		t.Fatalf("evidence paths %v", got)
	}
	// an absolute path under the repository, and a file name only one file has
	if got := EvidencePaths(repos, []string{src + "/pkg/b.go", "see a.go"})["sample"]; len(got) != 2 {
		t.Fatalf("evidence paths %v", got)
	}
	dst := filepath.Join(parent, "diffs")
	m, err := SnapshotDiffs(context.Background(), repos, texts, dst, defaultSince(), nil)
	if err != nil || m.Paths != 1 || m.Commits != 3 || m.Masked != 2 || m.Error != "" {
		t.Fatalf("diffs %+v: %v", m, err)
	}
	all, files := treeText(t, dst)
	if files != 3 || strings.Contains(all, "marker-7f3a") || strings.Contains(all, "edit b") || !strings.Contains(all, "two [masked]") {
		t.Fatalf("diff files %d\n%.600s", files, all)
	}
	if !strings.Contains(all, "[cut: this diff is larger than") {
		t.Fatal("an oversized diff was not cut")
	}
	// no evidence path, no diff
	empty := filepath.Join(parent, "none")
	m, err = SnapshotDiffs(context.Background(), repos, []string{"nothing here"}, empty, defaultSince(), nil)
	if err != nil || m.Commits != 0 {
		t.Fatalf("diffs without evidence %+v: %v", m, err)
	}
}

func TestSummaryNamesWhatEachRunGot(t *testing.T) {
	m := InputsManifest{Store: StoreManifest{Events: 41203, Scrubbed: 5, Cached: 41198},
		Repositories: []RepoInput{{Name: "one", Commit: "abc123456789", Files: 1200, Bytes: 3 << 20, Masked: 2, Skipped: map[string]int{"binary": 3, "large": 1},
			History: HistoryManifest{Commits: 40}}, {Name: "two", Error: "source is not a git work tree"}},
		Tacit: RepoInput{Error: "input git ls-tree failed: exit status 128"}, Diffs: DiffsManifest{Paths: 2, Commits: 5, Masked: 1}}
	got := m.Summary()
	for _, want := range []string{"41,203 events (5 rows scrubbed, 41,198 cached)", "2 repositories (one 1,200 files, 3.0 MiB at abc1234, 2 masked, 4 left out, log 40 commits; two absent: source is not a git work tree)",
		"tacit absent: input git ls-tree failed: exit status 128", "diffs of 5 commits for 2 paths (1 masked)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Fatal("summary spans lines")
	}
}
