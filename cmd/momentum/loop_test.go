package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// evidence git runs only the git commands that read, and none of their arguments that write a file, run a program
// or read past the repository: the loop's agent reads history through it and runs no git of its own.
func TestEvidenceGitOnlyReads(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "first"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if code, out, errOut := momentum("evidence", "git", dir, "log", "--oneline"); code != 0 || !strings.Contains(out, "first") {
		t.Fatalf("log: exit %d %q %s", code, out, errOut)
	}
	if code, out, _ := momentum("evidence", "git", dir, "branch", "--show-current"); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("branch --show-current: exit %d %q", code, out)
	}
	secret := filepath.Join(dir, "secret")
	os.WriteFile(secret, []byte("hunter2"), 0o600)
	for _, args := range [][]string{
		{"push", "origin"}, {"commit", "-m", "x"}, {"log", "--output=" + filepath.Join(dir, "out")}, {"diff", "--no-index", "/dev/null", secret},
		{"blame", "--contents", secret, "x"}, {"grep", "-O", "x"}, {"branch", "new-branch"}, {"branch", "-D", "main"}, {"tag", "v1"},
		{"config", "user.email", "x@y"}, {"remote", "add", "x", "y"}, {"worktree", "add", "x"}, {"log", "-c", "core.pager=x"},
	} {
		if code, _, _ := momentum(append([]string{"evidence", "git", dir}, args...)...); code != 2 {
			t.Errorf("git %v: exit %d, want 2", args, code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Fatal("a refused --output wrote its file")
	}
}

// Inside one of the loop's runs, momentum lands nothing, rolls nothing back, runs no loop, works only the run's
// own case, and writes a replay's points only in the run's own directory.
func TestALoopRunKeepsToItsOwnCase(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	run := t.TempDir()
	t.Setenv("MOMENTUM_LOOP_RUN", "r1")
	t.Setenv("MOMENTUM_LOOP_CASE", "c00000001")
	t.Setenv("MOMENTUM_LOOP_DIR", run)
	for _, args := range [][]string{{"ship", "land", "c00000001"}, {"ship", "rollback", "c00000001", "--why", "x"}, {"loop"}} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "loop") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := momentum("ship", "status", "--case", "c00000002"); code != 2 || !strings.Contains(errOut, "works case c00000001") {
		t.Errorf("another case: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("supervise", "replay", "--since", "1d", "--points", filepath.Join(t.TempDir(), "p.jsonl")); code != 2 ||
		!strings.Contains(errOut, "writes only under") {
		t.Errorf("points outside the run: exit %d %s", code, errOut)
	}
}
