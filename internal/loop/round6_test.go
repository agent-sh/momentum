package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A scrubber that reads a text and prints nothing did not scrub it.
func TestAScrubberThatPrintsNothingFails(t *testing.T) {
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	Scrubber = []string{script(t, t.TempDir(), "scrub", "cat > /dev/null")}
	if _, err := Redact(context.Background(), "a text with a secret-value in it"); err == nil {
		t.Fatal("an empty scrub passed")
	}
	if out, err := Redact(context.Background(), ""); err != nil || out != "" {
		t.Fatalf("an empty text: %q %v", out, err)
	}
}

// A run that never started (its prompt could not be scrubbed) spends nothing, on its case or the loop's budgets.
func TestARunThatNeverStartedSpendsNothing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	Scrubber = []string{filepath.Join(t.TempDir(), "no-scrubber")}
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunFailed || done[0].Run.USD != 0 {
		t.Fatalf("%+v %v", done, err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Cost.USD != 0 {
		t.Fatalf("the case was charged %v", got.Cost.USD)
	}
}

// A repository whose submodule takes a mode git submodule init writes is allowed; one whose update runs a command
// is refused.
func TestSubmoduleUpdateModesAreAllowedButCommandsRefused(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "submodule.lib.update", "none")
	if err := CheckGitConfig(context.Background(), dir); err != nil {
		t.Fatalf("a mode: %v", err)
	}
	run(t, dir, "git", "config", "submodule.lib.update", "!sh ./x")
	if err := CheckGitConfig(context.Background(), dir); err == nil {
		t.Fatal("a command was allowed")
	}
}

// What a review said reaches the build that revises the change scrubbed (its review.md), and the review's own
// prompt, which holds the package's title, reaches the reviewer scrubbed.
func TestTheReviewsTextAndPromptAreScrubbed(t *testing.T) {
	ctx := context.Background()
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	Scrubber = []string{script(t, t.TempDir(), "scrub", "sed 's/zz-private-value/****************/g'")}
	st := testStore(t)
	agent := fakeAgent(t, okResult)
	r := runnerFor(t, st, agent)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	r.Ship = s
	tc := testingCase(t, st)
	p, err := s.Prepare(ctx, tc.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	p.State, p.Title = Revising, "fix the thing that printed zz-private-value"
	p.Reviews = []Review{{Round: 1, High: 1, Head: "abc", Text: "H1. it logs zz-private-value", Comment: "https://example.invalid/c/1"}}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	build, why := r.reserve(ctx, tc.Family, tc.ID, StepBuild)
	if why != "" {
		t.Fatal(why)
	}
	if _, err := r.invoke(ctx, tc.Family, tc, StepBuild, build); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(HostDir(build.Dir), "primary", "review.md")); !strings.Contains(string(raw), "it logs ****") || strings.Contains(string(raw), "zz-private-value") {
		t.Fatalf("review.md: %q", raw)
	}
	run(t, p.Work, "git", "commit", "-q", "--allow-empty", "-m", "the change")
	p.Head, p.State, p.Base = strings.TrimSpace(run(t, p.Work, "git", "rev-parse", "HEAD")), Proposed, "main"
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	review, why := r.reserve(ctx, tc.Family, tc.ID, StepReview)
	if why != "" {
		t.Fatal(why)
	}
	if _, err := r.invokeReview(ctx, tc, p, review); err != nil {
		t.Fatal(err)
	}
	if args, _ := agentArgs(t, agent); strings.Contains(args, "zz-private-value") || !strings.Contains(args, "printed ****") {
		t.Fatalf("the review's prompt:\n%s", args)
	}
}
