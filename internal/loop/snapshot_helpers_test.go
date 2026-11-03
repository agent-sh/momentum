package loop

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// defaultSince is the log window of a test: the whole history.
func defaultSince() (zero time.Time) { return }
