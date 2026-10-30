package capacity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// worktreesFixture is a remote, a clone of it, and linked worktrees of the clone in every state.
func worktreesFixture(t *testing.T) (clone, wts string) {
	t.Helper()
	gitEnv(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	clone, wts = filepath.Join(base, "clone"), filepath.Join(base, "cache")
	gitT(t, base, "init", "-q", "--bare", remote)
	gitT(t, base, "clone", "-q", remote, clone)
	write(t, filepath.Join(clone, "README"), "one\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-qm", "one")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "remote", "set-head", "origin", "main")
	add := func(name, branch string) string {
		p := filepath.Join(wts, name)
		gitT(t, clone, "worktree", "add", "-q", "-b", branch, p, "origin/main")
		return p
	}
	commit := func(wt, file string) {
		write(t, filepath.Join(wt, file), file+"\n")
		gitT(t, wt, "add", ".")
		gitT(t, wt, "commit", "-qm", file)
	}
	// merged: pushed under its own name and fast-forwarded into main
	merged := add("merged", "feat/merged")
	commit(merged, "a")
	gitT(t, merged, "push", "-q", "-u", "origin", "feat/merged")
	gitT(t, merged, "push", "-q", "origin", "feat/merged:main")
	// gone: pushed, then its branch deleted on the remote, not merged
	gone := add("gone", "feat/gone")
	commit(gone, "b")
	gitT(t, gone, "push", "-q", "-u", "origin", "feat/gone")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/gone")
	// squashed: its changes squash-merged into main, then its branch deleted
	squashed := add("squashed", "feat/squashed")
	commit(squashed, "s1")
	commit(squashed, "s2")
	gitT(t, squashed, "push", "-q", "-u", "origin", "feat/squashed")
	gitT(t, clone, "fetch", "-q", "origin")
	gitT(t, clone, "merge", "-q", "--ff-only", "origin/main")
	gitT(t, clone, "merge", "-q", "--squash", "feat/squashed")
	gitT(t, clone, "commit", "-qm", "squashed")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/squashed")
	// pushed and fast-forwarded into main, then never committed to again: still merged
	// a new branch pushed under its own name with no commit yet: work about to begin
	pushedNew := add("pushed-new", "feat/pushed-new")
	gitT(t, pushedNew, "push", "-q", "-u", "origin", "feat/pushed-new")
	// gone, with a commit made after the push
	ahead := add("ahead", "feat/ahead")
	commit(ahead, "c")
	gitT(t, ahead, "push", "-q", "-u", "origin", "feat/ahead")
	commit(ahead, "d")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/ahead")
	// gone, with an uncommitted file
	dirty := add("dirty", "feat/dirty")
	commit(dirty, "e")
	gitT(t, dirty, "push", "-q", "-u", "origin", "feat/dirty")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/dirty")
	write(t, filepath.Join(dirty, "notes.txt"), "wip\n")
	// work in progress: a new branch that still tracks main, one never pushed, and a detached one
	add("fresh", "feat/fresh")
	gitT(t, filepath.Join(wts, "fresh"), "branch", "-q", "--set-upstream-to", "origin/main")
	local := add("local", "feat/local")
	gitT(t, local, "branch", "-q", "--unset-upstream")
	commit(local, "f")
	gitT(t, clone, "worktree", "add", "-q", "--detach", filepath.Join(wts, "detached"), "origin/main")
	// merged, in a lane whose branch names an excluded project
	lane := add("lane", "secretlane/T1")
	commit(lane, "g")
	gitT(t, lane, "push", "-q", "-u", "origin", "secretlane/T1")
	gitT(t, lane, "push", "-q", "origin", "secretlane/T1:main")
	gitT(t, clone, "fetch", "-q", "origin")
	return clone, wts
}

func TestLeftoverWorktreesGoWhenCleanAndPushed(t *testing.T) {
	clone, wts := worktreesFixture(t)
	now := time.Now().Add(2 * time.Hour)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Exclude: Exclude{Names: []string{"secretlane"}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	st := openStore(t)
	res, err := Run(context.Background(), cfg, st, options(t, procRoot(t), now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleWorktrees)
	want := map[string]string{
		"merged":   ActDelete,
		"squashed": ActDelete, // its changes are in main
		"gone":     ActReport, // deleted on the remote, its commit on no remote and not in main
		"ahead":    ActReport, // a commit on no remote
		"dirty":    ActReport, // an untracked file
		"lane":     ActReport, // an excluded project's branch
	}
	for name, a := range want {
		f := got[filepath.Join(wts, name)]
		if f.Action != a {
			t.Errorf("%s: %q (%s %s), want %q", name, f.Action, f.Detail, f.Why, a)
		}
	}
	for _, name := range []string{"fresh", "local", "detached", "pushed-new"} {
		if f, ok := got[filepath.Join(wts, name)]; ok {
			t.Errorf("%s is work in progress, not a leftover: %+v", name, f)
		}
	}
	for name, a := range want {
		_, err := os.Stat(filepath.Join(wts, name))
		if gone := os.IsNotExist(err); gone != (a == ActDelete) {
			t.Errorf("%s: gone %v, want gone %v", name, gone, a == ActDelete)
		}
	}
	branches := gitT(t, clone, "branch", "--list")
	if strings.Contains(branches, "feat/merged") || strings.Contains(branches, "feat/squashed") {
		t.Errorf("a removed worktree's branch goes with it: %s", branches)
	}
	if !strings.Contains(branches, "feat/gone") || !strings.Contains(branches, "feat/ahead") || !strings.Contains(branches, "feat/dirty") {
		t.Errorf("a reported worktree keeps its branch: %s", branches)
	}
	if got[filepath.Join(wts, "ahead")].class != "unpushed" || got[filepath.Join(wts, "gone")].class != "unpushed" ||
		got[filepath.Join(wts, "dirty")].class != "uncommitted" {
		t.Error("each report-only worktree says why")
	}
}

func TestAWorktreeChangedWithinTheHourOrInUseStays(t *testing.T) {
	_, wts := worktreesFixture(t)
	now := time.Now().Add(2 * time.Hour)
	must(t, os.Chtimes(filepath.Join(wts, "merged"), now, now.Add(-10*time.Minute)))
	proc := procRoot(t, fakeProc{pid: 3, ppid: 1, argv: []string{"bash"}, cwd: filepath.Join(wts, "gone")})
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, proc, now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleWorktrees)
	if f := got[filepath.Join(wts, "merged")]; f.Action != ActKeep {
		t.Errorf("a worktree changed 10 minutes ago stays: %+v", f)
	}
	for _, name := range []string{"merged", "gone"} {
		if _, err := os.Stat(filepath.Join(wts, name)); err != nil {
			t.Errorf("%s was removed", name)
		}
	}
}
