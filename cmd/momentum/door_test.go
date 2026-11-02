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

// A run's git is held to the repositories it reads by every place git reads one from, not only the work tree it was
// pointed at: a git directory outside the roots, a common directory outside them (a work tree's .git file names a
// directory of its own inside the root, whose commondir file names another repository's git directory), and a work
// tree that its repository's configuration moves outside are each refused; a link that stays inside the roots is
// followed.
func TestARunsGitIsHeldToItsRootsByEveryPlaceGitReadsFrom(t *testing.T) {
	needEvidenceBwrap(t)
	fakeScrubber(t)
	root := t.TempDir()
	outside := evidenceRepo(t)
	outsideGit := strings.TrimSpace(gitIn(t, outside, "rev-parse", "--absolute-git-dir"))
	t.Setenv("MOMENTUM_LOOP_RUN", "r1")
	t.Setenv("MOMENTUM_LOOP_READ", root)
	// the work tree's .git file names a git directory inside the root, whose commondir names the outside repository's
	notes := filepath.Join(root, "notes")
	gd := filepath.Join(root, "gd")
	os.MkdirAll(notes, 0o700)
	os.MkdirAll(gd, 0o700)
	os.WriteFile(filepath.Join(notes, ".git"), []byte("gitdir: "+gd+"\n"), 0o600)
	os.WriteFile(filepath.Join(gd, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	os.WriteFile(filepath.Join(gd, "commondir"), []byte(outsideGit+"\n"), 0o600)
	if code, out, diag := momentum("evidence", "git", notes, "log", "--oneline"); code == 0 || strings.Contains(out, "synthetic") {
		t.Errorf("a common directory outside the roots: %d %q %s", code, out, diag)
	}
	// a repository inside the root whose configuration moves its work tree outside
	inside := filepath.Join(root, "repo")
	os.MkdirAll(inside, 0o700)
	gitIn(t, inside, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(inside, "a.txt"), []byte("inside\n"), 0o600)
	gitIn(t, inside, "add", ".")
	gitIn(t, inside, "commit", "-qm", "inside")
	// the reverse: the work tree's git directory is outside the root and its common directory is a repository inside it
	gd2 := filepath.Join(t.TempDir(), "gd2")
	notes2 := filepath.Join(root, "notes2")
	os.MkdirAll(gd2, 0o700)
	os.MkdirAll(notes2, 0o700)
	os.WriteFile(filepath.Join(notes2, ".git"), []byte("gitdir: "+gd2+"\n"), 0o600)
	os.WriteFile(filepath.Join(gd2, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	os.WriteFile(filepath.Join(gd2, "commondir"), []byte(filepath.Join(inside, ".git")+"\n"), 0o600)
	if code, out, diag := momentum("evidence", "git", notes2, "log", "--oneline"); code == 0 || strings.Contains(out, "inside") {
		t.Errorf("a git directory outside the roots: %d %q %s", code, out, diag)
	}
	moved := filepath.Join(root, "moved")
	os.MkdirAll(moved, 0o700)
	gitIn(t, moved, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(moved, ".git"), []byte("gitdir: "+filepath.Join(inside, ".git")+"\n"), 0o600)
	gitIn(t, inside, "config", "core.worktree", filepath.Dir(root))
	if code, out, _ := momentum("evidence", "git", inside, "log", "--oneline"); code == 0 || strings.Contains(out, "inside") {
		t.Errorf("a work tree moved outside the roots: %d %q", code, out)
	}
	gitIn(t, inside, "config", "--unset", "core.worktree")
	// a link under the root to a repository under it is followed; a link out of it is not
	link := filepath.Join(root, "link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	if code, out, diag := momentum("evidence", "git", link, "log", "--oneline"); code != 0 || !strings.Contains(out, "inside") {
		t.Errorf("a link within the roots: %d %q %s", code, out, diag)
	}
	away := filepath.Join(root, "away")
	if err := os.Symlink(outside, away); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := momentum("evidence", "git", away, "log", "--oneline"); code == 0 || strings.Contains(out, "synthetic") {
		t.Errorf("a link out of the roots: %d %q", code, out)
	}
}

// The sandbox mounts checked directories at their resolved paths. The command must use the pinned path too,
// since a link beside that directory is not among the mounts. This records the command without a namespace.
func TestEvidenceGitUsesItsCheckedDirectoryForTheSandboxCommand(t *testing.T) {
	fakeScrubber(t)
	repo := evidenceRepo(t)
	link := filepath.Join(filepath.Dir(repo), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "bwrap"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_READ", filepath.Dir(repo))
	code, out, diag := momentum("evidence", "git", link, "log", "--oneline")
	if code != 0 || !strings.Contains(out, "--chdir\n"+repo+"\n--\ngit\n-C\n"+repo+"\n") {
		t.Fatalf("Git did not use its checked directory: %d %q %s", code, out, diag)
	}
	if strings.Contains(out, "\n"+link+"\n") {
		t.Fatalf("the command retained the unmounted link: %s", out)
	}
}
