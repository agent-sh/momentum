package loop

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// denied says whether a file tool's deny rule, as the harness matches it (gitignore's: a name with no slash at
// any depth, DIR/** under DIR), keeps a run from reading a path.
func denied(rules []string, p string) bool {
	p = strings.ToLower(p)
	for _, r := range rules {
		pattern, ok := strings.CutPrefix(r, "Read(**/")
		if !ok {
			continue
		}
		pattern = strings.ToLower(strings.TrimSuffix(pattern, ")"))
		if dir, ok := strings.CutSuffix(pattern, "/**"); ok {
			if strings.Contains(p, "/"+dir+"/") {
				return true
			}
			continue
		}
		if strings.Contains(pattern, "/") {
			if strings.HasSuffix(p, "/"+pattern) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pattern, path.Base(p)); ok {
			return true
		}
	}
	return false
}

var (
	// credentialFiles hold secrets wherever they lie in a run's directories
	credentialFiles = []string{"/r/.env", "/r/app/token.json", "/r/app/client_secret.json", "/r/deploy/id_ecdsa", "/r/certs/client.p12",
		"/r/infra/.kube/config", "/r/app/.docker/config.json", "/r/svc/gcp-credential.json", "/r/app/secrets.yaml", "/r/home/.ssh/config"}
	// sourceFiles are code, whatever words their names hold
	sourceFiles = []string{"/r/tokens.go", "/r/tokenizer.rs", "/r/secret_store.py", "/r/credential_cache.go", "/r/internal/auth/token_test.go"}
)

// The file tools' deny rules keep a run from every credential file momentum's own commands withhold, and from no
// source file: one list says what a credential is.
func TestTheFileToolsDenyWhatTheCommandsWithhold(t *testing.T) {
	st := testStore(t)
	r := runnerFor(t, st, nil)
	for _, step := range []string{StepInvestigate, StepBuild, StepReview} {
		deny := r.permissions(step, store.Case{ID: "c1"}, store.LoopRun{ID: "r1", Dir: filepath.Join(t.TempDir(), "runs", "r1")}).Deny
		for _, p := range credentialFiles {
			if !denied(deny, p) {
				t.Errorf("%s: %s is readable by the file tools", step, p)
			}
		}
		for _, p := range sourceFiles {
			if denied(deny, p) {
				t.Errorf("%s: %s, a source file, is denied", step, p)
			}
		}
		if denied(deny, "/home/u/.local/share/momentum/loop/runs/r1/findings.md") {
			t.Errorf("%s: the run's own directory is denied", step)
		}
	}
}

// The pathspecs that leave credentials out of a diff, a history and a search leave a source file named like one in.
func TestCredentialPathspecsKeepSourceFiles(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	for _, p := range append(append([]string{}, credentialFiles...), sourceFiles...) {
		write(t, filepath.Join(dir, strings.TrimPrefix(p, "/r/")), "changed\n")
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", "both kinds")
	out := run(t, dir, append([]string{"git", "diff", "--name-only", "HEAD~1", "HEAD", "--"}, CredentialPathspecs()...)...)
	for _, p := range sourceFiles {
		if !strings.Contains(out, strings.TrimPrefix(p, "/r/")) {
			t.Errorf("%s left out of the diff", p)
		}
	}
	for _, p := range credentialFiles {
		if strings.Contains(out, strings.TrimPrefix(p, "/r/")+"\n") {
			t.Errorf("%s shown in the diff", p)
		}
	}
}

// A check refuses a change to a credential file, so a clean review has read all of what it lands; and the test runs
// on its tree without the tracked credential files, so nothing it prints of them reaches the build or the review.
func TestACheckKeepsCredentialFilesOutOfTheChangeAndTheTest(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, map[string]string{".env": "password=zz-private-value\n"})
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	// a test that reads the tracked .env and fails on the old answer, printing what it read
	reads := "package fake\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestAnswer(t *testing.T) {\n\tb, _ := os.ReadFile(\".env\")\n\tt.Logf(\"env %q\", b)\n\tif Answer() != 42 {\n\t\tt.Fatal(Answer())\n\t}\n}\n"
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), reads)
	p, err = s.Check(ctx, c.ID, "go test -v ./...", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Check.Before.Tail+p.Check.After.Tail, "zz-private-value") {
		t.Fatalf("the test read a tracked credential file:\n%s\n%s", p.Check.Before.Tail, p.Check.After.Tail)
	}
	write(t, filepath.Join(p.Work, ".env"), "password=zz-other-value\n")
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("a change to a credential file was checked: %v", err)
	}
	os.Remove(filepath.Join(p.Work, ".env"))
}

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
	if raw, _ := os.ReadFile(filepath.Join(build.Dir, "review.md")); !strings.Contains(string(raw), "it logs ****") || strings.Contains(string(raw), "zz-private-value") {
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
