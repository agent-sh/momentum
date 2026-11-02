package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// In a loop run, evidence git reads only the repositories the loop reads (MOMENTUM_LOOP_READ, which the run's door
// sets): one under a root, not one beside it, and not a directory under a root whose .git file names a repository
// outside. Outside a run it reads any.
func TestARunsGitReadsOnlyWhereTheLoopReads(t *testing.T) {
	needEvidenceBwrap(t)
	fakeScrubber(t)
	root := t.TempDir()
	inside := filepath.Join(root, "repo")
	os.MkdirAll(inside, 0o700)
	gitIn(t, inside, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(inside, "a.txt"), []byte("inside\n"), 0o600)
	gitIn(t, inside, "add", ".")
	gitIn(t, inside, "commit", "-qm", "inside")
	outside := evidenceRepo(t)
	linked := filepath.Join(root, "linked")
	os.MkdirAll(linked, 0o700)
	gitDir := strings.TrimSpace(gitIn(t, outside, "rev-parse", "--absolute-git-dir"))
	os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600)
	if code, out, diag := momentum("evidence", "git", outside, "log", "--oneline"); code != 0 || !strings.Contains(out, "synthetic") {
		t.Fatalf("outside a run any repository reads: %d %q %s", code, out, diag)
	}
	t.Setenv("MOMENTUM_LOOP_RUN", "r1")
	t.Setenv("MOMENTUM_LOOP_CASE", "c1")
	t.Setenv("MOMENTUM_LOOP_READ", root)
	if code, out, diag := momentum("evidence", "git", inside, "log", "--oneline"); code != 0 || !strings.Contains(out, "inside") {
		t.Fatalf("a repository under the root: %d %q %s", code, out, diag)
	}
	for _, tc := range []struct{ dir, why string }{
		{outside, "evidence git reads only repositories under loop.read_dirs"},
		{linked, "not where this run reads repositories"},
	} {
		if code, out, diag := momentum("evidence", "git", tc.dir, "log", "--oneline"); code == 0 || strings.Contains(out, "synthetic") ||
			!strings.Contains(diag, tc.why) {
			t.Errorf("%s read in a run: %d %q %s", tc.dir, code, out, diag)
		}
	}
	t.Setenv("MOMENTUM_LOOP_READ", "")
	if code, _, _ := momentum("evidence", "git", inside, "log", "--oneline"); code == 0 {
		t.Error("a run with no roots read a repository")
	}
}
