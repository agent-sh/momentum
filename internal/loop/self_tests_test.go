package loop

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// selfOther is a test of the rules' package that a person wrote, in a file before the loop's.
const (
	selfOtherPath = "internal/supervise/rules_test.go"
	selfOther     = "package supervise\n\nimport \"testing\"\n\nfunc TestLimitIsAtMostTwo(t *testing.T) {\n\tif Limit() > 2 {\n\t\tt.Fatal(Limit())\n\t}\n}\n"
)

// selfFixture is a ship tool over a repository the gate names as the loop's own, holding the supervision rules,
// a person's test of them and the loop's test file, with a package prepared for a case.
func selfFixture(t *testing.T) (*Ship, *store.Store, repoFixture, store.Case, Package) {
	t.Helper()
	return selfFixtureWith(t, nil)
}

// selfFixtureWith is selfFixture over a repository that holds more files.
func selfFixtureWith(t *testing.T, more map[string]string) (*Ship, *store.Store, repoFixture, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	files := map[string]string{rulesPath: selfRules, rulesTestPath: selfRulesTest, selfOtherPath: selfOther}
	maps.Copy(files, more)
	f := newRepo(t, files)
	s := shipFor(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return SelfRepo, nil }
	c := testingCase(t, st)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	return s, st, f, c, p
}

// The loop lands a change to the supervision rules together with their test file when the change to the test only
// appends tests: the package has no hand path, and it lands, both files with it.
func TestARulesChangeWithAppendedTestsIsTheLoopsToLand(t *testing.T) {
	ctx := context.Background()
	s, _, f, c, p := selfFixture(t)
	write(t, filepath.Join(p.Work, rulesPath), strings.Replace(selfRules, "limit = 1", "limit = 2", 1))
	write(t, filepath.Join(p.Work, rulesTestPath), selfRulesTest+selfNewTest)
	p, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, nil, fixturePatch(t, s, c.ID))
	if err != nil || p.State != Checked || len(p.Hand) != 0 || !slices.Equal(p.Changed, []string{rulesPath, rulesTestPath}) {
		t.Fatalf("a rules change with an appended test: hand %v changed %v state %s: %v", p.Hand, p.Changed, p.State, err)
	}
	// by its name the test file is still a person's: only the read of its change lets it through
	if err := CheckPaths(Target{Repo: SelfRepo}, p.Changed); !errors.Is(err, ErrRefused) {
		t.Fatalf("the gate's check by names: %v", err)
	}
	if _, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "The limit is two", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "Checked every changed file.", Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || !p.Clean() || len(p.Hand) != 0 {
		t.Fatalf("a clean review: %+v %v", p, err)
	}
	s.Live = true
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed {
		t.Fatalf("the landing: %+v %v", p, err)
	}
	got := run(t, f.main, "git", "--git-dir", f.origin, "show", "main:"+rulesTestPath)
	if !strings.Contains(got, "TestLimitIsTwo") || !strings.Contains(run(t, f.main, "git", "--git-dir", f.origin, "show", "main:"+rulesPath), "limit = 2") {
		t.Fatalf("the default branch after the landing: %s", got)
	}
}

// What a change does to the loop's own files decides whose it is to land, where the paths cannot: a test file's
// change that is no append of tests stays a person's, a rules file's change that does more than change a rule
// becomes one, and in another repository nothing is read.
func TestTheLoopReadsAChangeToItsOwnFiles(t *testing.T) {
	ctx := context.Background()
	two := strings.Replace(selfRules, "limit = 1", "limit = 2", 1)
	for name, c := range map[string]struct {
		rules, test string
		mode        os.FileMode
		hand        []string
	}{
		"appended tests":                  {two, selfRulesTest + selfNewTest, 0o644, nil},
		"the rules alone":                 {two, selfRulesTest, 0o644, nil},
		"the test alone":                  {selfRules, selfRulesTest + selfNewTest, 0o644, []string{rulesTestPath}},
		"a variable that runs code":       {two + "\nvar _ = func() bool { limit = 9; return true }()\n", selfRulesTest + selfNewTest, 0o644, []string{rulesPath}},
		"a line of an old test rewritten": {two, strings.Replace(selfRulesTest, "Limit() <= 0", "Limit() < -5", 1) + selfNewTest, 0o644, []string{rulesTestPath}},
		"a skip put inside an old test":   {two, strings.Replace(selfRulesTest, "\tif Limit", "\tt.Skip()\n\tif Limit", 1) + selfNewTest, 0o644, []string{rulesTestPath}},
		"a TestMain appended":             {two, selfRulesTest + selfNewTest + "\nfunc TestMain(m *testing.M) {}\n", 0o644, []string{rulesTestPath}},
		"the test file made executable":   {two, selfRulesTest + selfNewTest, 0o755, []string{rulesTestPath}},
		"the test file deleted":           {two, "", 0o644, []string{rulesTestPath}},
		"an import added to the rules":    {strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar _ = os.Args", 1), selfRulesTest + selfNewTest, 0o644, []string{rulesPath}},
		"an initializer in the rules":     {two + "\nfunc init() { limit = 3 }\n", selfRulesTest, 0o644, []string{rulesPath}},
		"the rules deleted":               {"", selfRulesTest + selfNewTest, 0o644, []string{rulesPath}},
		"both":                            {two + "\nfunc init() {}\n", selfRulesTest + "\nvar x = 1\n", 0o644, []string{rulesPath, rulesTestPath}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, cs, p := selfFixture(t)
			for path, body := range map[string]string{rulesPath: c.rules, rulesTestPath: c.test} {
				if body == "" {
					if err := os.Remove(filepath.Join(p.Work, path)); err != nil {
						t.Fatal(err)
					}
					continue
				}
				write(t, filepath.Join(p.Work, path), body)
			}
			if c.test != "" {
				if err := os.Chmod(filepath.Join(p.Work, rulesTestPath), c.mode); err != nil {
					t.Fatal(err)
				}
			}
			tree, paths, err := s.applyPatch(ctx, p, []Patch{fixturePatch(t, s, cs.ID)})
			if err != nil {
				t.Fatal(err)
			}
			byName, err := PathScope(Target{Repo: SelfRepo}, paths)
			if err != nil {
				t.Fatal(err)
			}
			hand, err := s.selfScope(ctx, p, p.BaseCommit, tree, paths, byName)
			if err != nil || !slices.Equal(hand, c.hand) {
				t.Fatalf("hand paths %v, want %v (by name %v): %v", hand, c.hand, byName, err)
			}
			// the landing reads the same change the same way
			p.Changed = paths
			if err := s.landPaths(ctx, p, p.BaseCommit, tree); (err == nil) != (c.hand == nil) || err != nil && !errors.Is(err, ErrRefused) {
				t.Fatalf("the landing's paths: %v", err)
			}
			// another repository's files of the same names are not read: its hand paths stand as the gate gave them
			p.Repo = "agent-sh/other"
			if hand, err := s.selfScope(ctx, p, p.BaseCommit, tree, paths, byName); err != nil || !slices.Equal(hand, byName) {
				t.Fatalf("another repository: %v %v", hand, err)
			}
		})
	}
	// The loop's tests run last only while the package's other test files leave the order alone. In a package
	// where one does not, the loop's test file is a person's again, though its change only appends.
	for name, more := range map[string]map[string]string{
		"a test file that sorts after the loop's": {"internal/supervise/zzz_later_test.go": selfOther},
		"a test that runs in parallel":            {"internal/supervise/par_test.go": "package supervise\n\nimport \"testing\"\n\nfunc TestPar(t *testing.T) { t.Parallel() }\n"},
		"a file of the external test package":     {"internal/supervise/ext_test.go": "package supervise_test\n\nimport \"testing\"\n\nfunc TestExt(t *testing.T) {}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, cs, p := selfFixtureWith(t, more)
			write(t, filepath.Join(p.Work, rulesPath), two)
			write(t, filepath.Join(p.Work, rulesTestPath), selfRulesTest+selfNewTest)
			tree, paths, err := s.applyPatch(ctx, p, []Patch{fixturePatch(t, s, cs.ID)})
			if err != nil {
				t.Fatal(err)
			}
			if hand, err := s.selfScope(ctx, p, p.BaseCommit, tree, paths, []string{rulesTestPath}); err != nil || !slices.Equal(hand, []string{rulesTestPath}) {
				t.Fatalf("hand paths %v: %v", hand, err)
			}
		})
	}
}

// The host runs the whole suite of the rules' package on the change. A change whose own test passes and that breaks
// a test the package had is refused, and so is one whose command rewrites the tree it ran in to hide that.
func TestTheRulesPackagesOwnTestsMustStillPass(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		rules, added string
		args         []string
	}{
		// the old test wants a positive limit; the build's command names only its new test
		"an old test broken": {strings.Replace(selfRules, "limit = 1", "limit = 0", 1),
			"\nfunc TestLimitIsZero(t *testing.T) {\n\tif Limit() != 0 {\n\t\tt.Fatal(Limit())\n\t}\n}\n", []string{"go", "test", "-run", "TestLimitIsZero", "./..."}},
		// the build's command puts the old limit back in the tree it ran in, once its own test has passed: the suite
		// runs on an export of its own, where the change is as it was
		"a command that rewrites the tree it ran in": {strings.Replace(selfRules, "limit = 1", "limit = 0", 1),
			"\nfunc TestLimitIsZero(t *testing.T) {\n\tif Limit() != 0 {\n\t\tt.Fatal(Limit())\n\t}\n}\n", []string{"go", "test", "-run", "TestLimitIsZero", "-exec",
				`/bin/sh -c '"$@" && sed -i "s/limit = 0/limit = 1/" /work/internal/supervise/rules.go' sh`, "./internal/supervise"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, cs, p := selfFixture(t)
			write(t, filepath.Join(p.Work, rulesPath), c.rules)
			write(t, filepath.Join(p.Work, rulesTestPath), selfRulesTest+c.added)
			p, err := s.CheckArgs(ctx, cs.ID, c.args, nil, fixturePatch(t, s, cs.ID))
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "own tests do not pass") || p.State == Checked {
				t.Fatalf("want the suite's refusal: state %s: %v", p.State, err)
			}
		})
	}
}

// A rebase can put the change on a base whose tests it breaks. The landing runs the package's suite on the rebased
// change too, and nothing reaches the default branch when it fails.
func TestARebasedRulesChangeIsReadAndRunAgain(t *testing.T) {
	ctx := context.Background()
	s, _, f, c, p := selfFixture(t)
	write(t, filepath.Join(p.Work, rulesPath), strings.Replace(selfRules, "limit = 1", "limit = 2", 1))
	write(t, filepath.Join(p.Work, rulesTestPath), selfRulesTest+selfNewTest)
	if _, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "-run", "TestLimitIsTwo", "./..."}, nil, fixturePatch(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "The limit is two", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Reviewed(ctx, c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "Checked every changed file.", Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || !p.Clean() {
		t.Fatalf("a clean review: %+v %v", p, err)
	}
	// the base gains a test that holds on it and not with the change
	pushOther(t, f, "main", map[string]string{"internal/supervise/y_new_test.go": "package supervise\n\nimport \"testing\"\n\nfunc TestLimitIsOne(t *testing.T) {\n\tif Limit() != 1 {\n\t\tt.Fatal(Limit())\n\t}\n}\n"})
	base := originHead(t, f, "main")
	s.Live = true
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "own tests do not pass") || originHead(t, f, "main") != base {
		t.Fatalf("a rebased change that breaks the base's test: %v", err)
	}
}

// The landing reads the change again, whatever the package says of its hand paths: a package saved with none over a
// change that rewrites an old test does not land, started or resumed, and the read after a rebase refuses it too.
func TestALandingReadsTheChangeAgain(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{Proposed, Landing} {
		s, _, f, c, p := selfFixture(t)
		write(t, filepath.Join(p.Work, rulesPath), strings.Replace(selfRules, "limit = 1", "limit = 2", 1))
		write(t, filepath.Join(p.Work, rulesTestPath), strings.Replace(selfRulesTest, "Limit() <= 0", "Limit() < -5", 1)+selfNewTest)
		p, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, nil, fixturePatch(t, s, c.ID))
		if err != nil || !slices.Equal(p.Hand, []string{rulesTestPath}) {
			t.Fatalf("setup: a hand landing: %v %v", p.Hand, err)
		}
		if _, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "The limit is two", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		if p, err = s.Reviewed(ctx, c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "Checked every changed file.", Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || !p.Clean() {
			t.Fatalf("setup: a clean review: %+v %v", p, err)
		}
		p.Hand, p.State = nil, state
		if state == Landing { // a head journaled and published, with the base where it was: the resumed landing's push is next
			p.LandingHead, p.PublishedHead = p.Head, p.Head
		}
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		s.Live = true
		base := originHead(t, f, "main")
		calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
		if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), rulesTestPath) || originHead(t, f, "main") != base {
			t.Fatalf("a %s package with no hand path over a rewritten test: %v", state, err)
		}
		// the landing's own read refused it, before the request or the CI was read and before any rebase
		if after, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls")); string(after) != string(calls) {
			t.Fatalf("a %s package reached the forge before its change was read:\n%s", state, after[len(calls):])
		}
		if kept, _, _ := s.Load(c.ID); state == Proposed && kept.LandingHead != "" {
			t.Fatalf("a %s package journaled a head: %s", state, kept.LandingHead)
		}
		if err := s.selfLanding(ctx, p, p.Head); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), rulesTestPath) {
			t.Fatalf("the read after a rebase: %v", err)
		}
	}
}
