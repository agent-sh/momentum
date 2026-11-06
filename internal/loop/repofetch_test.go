package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// passRepository prepares a pass's inputs over the one checkout and returns what it gave the repository, with the
// pass's summary.
func passRepository(t *testing.T, r *Runner, checkout string) (RepoInput, string) {
	t.Helper()
	r.Inputs.ReadDirs = []string{checkout}
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	if err := r.prepareInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := r.pass.manifest
	if len(m.Repositories) != 1 {
		t.Fatalf("repositories: %+v", m.Repositories)
	}
	return m.Repositories[0], m.Summary()
}

// Nothing pulls a checkout. A pass reads the repository at its origin's default branch as the host fetches it, so a
// checkout behind its origin gives the run origin's files and history, and the checkout itself does not move.
func TestARepositoryIsReadAtItsOriginsFetchedTip(t *testing.T) {
	r, _ := inputRunner(t)
	f := newRepo(t, nil)
	local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	repo, summary := passRepository(t, r, f.main)
	if repo.Error != "" || repo.Branch != "main" || repo.Commit != tip {
		t.Fatalf("the repository was not read at origin's tip %s: %+v", tip, repo)
	}
	if text, err := os.ReadFile(filepath.Join(repo.Path, "later.txt")); err != nil || string(text) != "merged later\n" {
		t.Fatalf("the snapshot lacks origin's later file: %q %v", text, err)
	}
	log, err := os.ReadFile(filepath.Join(r.pass.dir, "history", "repos", repo.Name+".log"))
	if err != nil || !strings.Contains(string(log), "someone else's") || repo.History.Commits != 2 {
		t.Fatalf("the log is not origin's: %d commits %v\n%s", repo.History.Commits, err, log)
	}
	if strings.Contains(summary, "checkout's") {
		t.Fatalf("a fetched repository is summarised as a fallback: %s", summary)
	}
	if got := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD")); got != local {
		t.Fatal("the read moved the checkout")
	}
	if refs := run(t, f.main, "git", "for-each-ref", "--format=%(refname) %(objectname)"); strings.Contains(refs, tip) {
		t.Fatalf("the fetch wrote into the checkout's refs:\n%s", refs)
	}
	// the case's diffs read the same objects: a commit only origin holds
	texts := []string{"the failure is in later.txt"}
	m, err := SnapshotDiffs(context.Background(), r.pass.manifest.Repositories, texts, filepath.Join(t.TempDir(), "diffs"), defaultSince(), nil)
	if err != nil || m.Commits != 1 || m.Error != "" {
		t.Fatalf("the diffs did not read origin's commit: %+v %v", m, err)
	}
}

// A checkout whose origin cannot be fetched, or that has none, is read at its own default branch, and the run's
// input summary says so.
func TestAFetchThatFailsFallsBackToTheCheckoutAndSaysSo(t *testing.T) {
	for _, kind := range []string{"fetch failed", "no origin"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := inputRunner(t)
			f := newRepo(t, nil)
			pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
			local := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))
			if kind == "no origin" {
				run(t, f.main, "git", "remote", "remove", "origin")
			} else {
				run(t, f.main, "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
			}
			repo, summary := passRepository(t, r, f.main)
			if repo.Error != "" || repo.Commit != local {
				t.Fatalf("the fallback did not read the checkout's main at %s: %+v", local, repo)
			}
			if _, err := os.Stat(filepath.Join(repo.Path, "later.txt")); !os.IsNotExist(err) {
				t.Fatal("the fallback read a file the checkout does not have")
			}
			if !strings.Contains(summary, "at "+local[:7]+" (checkout's main: "+kind) {
				t.Fatalf("the summary does not say the checkout was read: %s", summary)
			}
			if strings.Contains(summary, f.dir) || strings.Contains(summary, "gone.git") {
				t.Fatalf("the summary names a host path: %s", summary)
			}
		})
	}
}

// A build's base is the default branch as the host fetches it at the build, not the tree the episode prepared first:
// a package prepared before origin moved, and a revision after a review, both rework the change on the new tip, with
// the earlier patch as previous.patch.
func TestABuildBaseFollowsTheFetchedTip(t *testing.T) {
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	var err error
	c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	first, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// origin moves while the package waits for its next build
	tip := pushOther(t, f, "main", map[string]string{"later.txt": "merged later\n"})
	second, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil || first.BaseCommit == tip || p.BaseCommit != tip || p.Work != filepath.Join(second.Dir, "work") {
		t.Fatalf("the build kept the base the episode prepared: %s, want %s (%v)", p.BaseCommit, tip, err)
	}
	if text, err := os.ReadFile(filepath.Join(p.Work, "later.txt")); err != nil || string(text) != "merged later\n" {
		t.Fatalf("the build tree is not at the fetched tip: %q %v", text, err)
	}
	if prompt, err := readHostInput(HostDir(second.Dir), "prompt.json"); err != nil || !strings.Contains(string(prompt), tip) {
		t.Fatalf("the build prompt does not name the fetched base: %v", err)
	}
	// a revision after a review: its base moves too, and its earlier patch reaches the run
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	previous, err := readPatch(fixturePatch(t, r.Ship, c.ID))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Ship.pkgDir(c.ID), "change.patch"), string(previous))
	p.Head, p.State = p.BaseCommit, Revising
	p.Reviews = []Review{{Round: 1, Medium: 1, Text: "Name the cause.", Head: p.Head, Comment: "posted"}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	next := pushOther(t, f, "main", map[string]string{"notes.txt": "plain notes\n"})
	third, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = r.Ship.Load(c.ID)
	if err != nil || p.BaseCommit != next || p.Head != "" || p.State != Prepared {
		t.Fatalf("the revision did not move to the fetched tip: %s %q %s (%v)", p.BaseCommit, p.Head, p.State, err)
	}
	if patch, err := readHostInput(HostDir(third.Dir), "input/previous.patch"); err != nil || string(patch) != string(previous) {
		t.Fatal("the revision lost its earlier patch")
	}
	// a base that did not move is made again from the same commit
	fourth, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	if again, _, err := r.Ship.Load(c.ID); err != nil || again.BaseCommit != next || again.Work != filepath.Join(fourth.Dir, "work") {
		t.Fatalf("an unmoved base changed: %s (%v)", again.BaseCommit, err)
	}
}

// A build whose base cannot be fetched now is held for the pass: no run, no charge.
func TestABuildBaseThatCannotBeFetchedHoldsTheBuild(t *testing.T) {
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	var err error
	c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	run(t, p.Host, "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, err = r.prepareRun(ctx, c, c.Family, StepBuild)
	if err == nil || !held(err) || !strings.Contains(err.Error(), "cannot be fetched now") {
		t.Fatalf("an unfetchable base was not a hold: %v", err)
	}
}
