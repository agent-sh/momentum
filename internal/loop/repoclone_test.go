package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func inputTestGit(t *testing.T, dir string, extra []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append(append([]string{"-C", dir}, GitSafety()...), args...)...)
	cmd.Env = append(GitEnv(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture",
		"GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_COMMITTER_EMAIL=fixture@example.com")
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fixtures contain neutral text only. Never show command environments.
		t.Fatalf("fixture git %s: %v: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func inputRepo(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, dir, nil, "init", "-b", "main")
	write(t, filepath.Join(dir, "file"), "first\n")
	inputTestGit(t, dir, nil, "add", "file")
	inputTestGit(t, dir, nil, "commit", "-m", "first")
	write(t, filepath.Join(dir, "file"), "second\n")
	inputTestGit(t, dir, nil, "add", "file")
	inputTestGit(t, dir, nil, "commit", "-m", "second")
	inputTestGit(t, dir, nil, "checkout", "-b", "feature")
	write(t, filepath.Join(dir, "file"), "feature\n")
	inputTestGit(t, dir, nil, "add", "file")
	inputTestGit(t, dir, nil, "commit", "-m", "feature")
	inputTestGit(t, dir, nil, "remote", "add", "origin", filepath.Join(parent, "absent"))
	return dir
}

func TestInputsRepositoryClonesDefaultHistoryAndRefusals(t *testing.T) {
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
	dest := filepath.Join(parent, "copies")
	got, err := CloneRepos(context.Background(), RepoCloneOptions{Sources: []string{a, b, a, link, notRepo, own},
		Destination: dest, LoopDirs: []string{filepath.Join(parent, "loop")}})
	if err != nil || len(got) != 5 {
		t.Fatalf("clones %d: %v", len(got), err)
	}
	for i, clone := range got {
		if i >= 2 {
			if clone.Error == "" || clone.Path != "" {
				t.Fatalf("refused source cloned: %+v", clone)
			}
			continue
		}
		if clone.Error != "" || clone.Branch != "main" || !strings.HasPrefix(clone.Name, "sample-") || clone.Name == got[1-i].Name {
			t.Fatalf("clone metadata %+v", clone)
		}
		if remotes := inputTestGit(t, clone.Path, nil, "remote"); remotes != "" {
			t.Fatal("clone kept a remote")
		}
		if _, err := os.Lstat(filepath.Join(clone.Path, ".git/hooks")); !os.IsNotExist(err) {
			t.Fatal("clone kept hooks")
		}
		if branch := inputTestGit(t, clone.Path, nil, "branch", "--show-current"); branch != "main" {
			t.Fatal("clone checked out the source's feature branch")
		}
		if commit := inputTestGit(t, clone.Path, nil, "rev-parse", "HEAD"); commit != clone.Commit {
			t.Fatal("clone manifest commit differs")
		}
		if history := inputTestGit(t, clone.Path, nil, "rev-list", "--count", "HEAD"); history != "2" {
			t.Fatal("clone lost history")
		}
		text, err := os.ReadFile(filepath.Join(clone.Path, "file"))
		if err != nil || string(text) != "second\n" {
			t.Fatal("default branch contents differ")
		}
		// Non-local clones never retain source hardlinks or object alternates.
		if _, err := os.Stat(filepath.Join(clone.Path, ".git/objects/info/alternates")); !os.IsNotExist(err) {
			t.Fatal("clone depends on source alternates")
		}
		sourceObject := filepath.Join(clone.Source, ".git/objects", clone.Commit[:2], clone.Commit[2:])
		cloneObject := filepath.Join(clone.Path, ".git/objects", clone.Commit[:2], clone.Commit[2:])
		srcInfo, srcErr := os.Stat(sourceObject)
		copyInfo, copyErr := os.Stat(cloneObject)
		if srcErr != nil || copyErr == nil && os.SameFile(srcInfo, copyInfo) {
			t.Fatal("repository clone retained a source hardlink")
		}
	}
}

func TestInputsTacitCloneAndNonstandardDefault(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := inputRepo(t, parent, "facts")
	inputTestGit(t, src, nil, "branch", "-m", "main", "stable")
	inputTestGit(t, src, nil, "update-ref", "refs/remotes/origin/stable", inputTestGit(t, src, nil, "rev-parse", "stable"))
	inputTestGit(t, src, nil, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/stable")
	dest := filepath.Join(parent, "copy")
	clone, err := CloneTacit(context.Background(), src, dest)
	if err != nil || clone.Branch != "stable" || clone.Commit == "" || clone.Path != dest {
		t.Fatalf("tacit clone %+v: %v", clone, err)
	}
	if inputTestGit(t, dest, nil, "remote") != "" {
		t.Fatal("tacit kept origin")
	}
	if _, err := os.Lstat(filepath.Join(dest, ".git/hooks")); !os.IsNotExist(err) {
		t.Fatal("tacit kept hooks")
	}
	srcInfo, srcErr := os.Stat(filepath.Join(src, ".git/objects", clone.Commit[:2], clone.Commit[2:]))
	copyInfo, copyErr := os.Stat(filepath.Join(dest, ".git/objects", clone.Commit[:2], clone.Commit[2:]))
	if srcErr != nil || copyErr != nil || !os.SameFile(srcInfo, copyInfo) {
		t.Fatal("tacit clone did not use local objects")
	}
}

func TestInputsClonesRefuseSecretsInHistoryAndMissingCleaner(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := inputRepo(t, parent, "sample")
	write(t, filepath.Join(src, "file"), "marker-7f3a\n")
	inputTestGit(t, src, nil, "add", "file")
	inputTestGit(t, src, nil, "commit", "-m", "marked")
	write(t, filepath.Join(src, "file"), "clean\n")
	inputTestGit(t, src, nil, "add", "file")
	inputTestGit(t, src, nil, "commit", "-m", "clean")
	got, err := CloneRepos(context.Background(), RepoCloneOptions{Sources: []string{src}, Destination: filepath.Join(parent, "copies")})
	if err != nil || len(got) != 1 || got[0].Error != "clone object needs scrubbing" || got[0].Path != "" {
		t.Fatalf("historical secret accepted: %v %v", got, err)
	}
	dest := filepath.Join(parent, "tacit")
	if _, err := CloneTacit(context.Background(), src, dest); err == nil {
		t.Fatal("tacit history bypassed scrubbing")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("refused tacit clone published")
	}
	Scrubber = nil
	missing := filepath.Join(parent, "missing-cleaner")
	if _, err := CloneRepos(context.Background(), RepoCloneOptions{Sources: []string{src}, Destination: missing}); err == nil {
		t.Fatal("repository cloned without cleaner")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("missing cleaner wrote repository destination")
	}
}
