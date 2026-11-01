package loop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// testCache is the sandbox's build cache the package's tests share: one cache of their own, so each check does not
// build the standard library again, and none of the user's.
var testCache string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loop-test-cache-")
	if err != nil {
		panic(err)
	}
	testCache = dir
	Scrubber = []string{"cat"} // the tests' own: a test that reads what the scrubber does sets one that does it
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_DIR=") && !strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	write(t, p, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// repoFixture is a repository with an origin of its own (a bare repository), a Go module whose Answer is wrong,
// and the scripts the ship tool calls: a scope check that refuses, as the machine's does, while any work tree of
// the repository holds uncommitted work, and a GitHub CLI that opens and edits a pull request (and fails once when
// told to).
type repoFixture struct {
	origin, main, dir string
	guard, gh         string
}

const fakeGo = "package fake\n\n// Answer is the answer.\nfunc Answer() int { return 41 }\n\n// Note says zz-signal-text.\nfunc Note() string { return \"zz-signal-text\" }\n"

func newRepo(t *testing.T, files map[string]string) repoFixture {
	t.Helper()
	dir := t.TempDir()
	f := repoFixture{dir: dir, origin: filepath.Join(dir, "origin.git"), main: filepath.Join(dir, "main")}
	run(t, dir, "git", "init", "-q", "--bare", "-b", "main", f.origin)
	run(t, dir, "git", "clone", "-q", f.origin, f.main)
	write(t, filepath.Join(f.main, "go.mod"), "module example.com/fake\n\ngo 1.21\n")
	write(t, filepath.Join(f.main, "fake.go"), fakeGo)
	for p, body := range files {
		write(t, filepath.Join(f.main, p), body)
	}
	run(t, f.main, "git", "add", "-A")
	run(t, f.main, "git", "commit", "-q", "-m", "init")
	run(t, f.main, "git", "push", "-q", "origin", "HEAD:main")
	run(t, f.main, "git", "remote", "set-head", "origin", "main")
	script(t, filepath.Join(f.origin, "hooks"), "post-receive", `while read old new ref; do echo "$ref $new" >> "`+filepath.Join(dir, "pushes")+`"; done`)
	f.guard = script(t, dir, "guard", `if [ -e "`+filepath.Join(dir, "report-only")+`" ]; then echo '{"allowed": false, "disposition": "report_only", "reasons": ["excluded"]}'; exit 1; fi
for wt in $(git -C "`+f.main+`" worktree list --porcelain | sed -n 's/^worktree //p'); do
  if [ -n "$(git -C "$wt" status --porcelain)" ]; then echo '{"allowed": false, "disposition": "report_only", "reasons": ["uncommitted or untracked work"]}'; exit 1; fi
done
echo '{"allowed": true, "disposition": "allowed_scope_only", "reasons": []}'`)
	f.gh = script(t, dir, "gh", `if [ -e "`+filepath.Join(dir, "gh-fails")+`" ]; then rm "`+filepath.Join(dir, "gh-fails")+`"; echo "gh: network down" >&2; exit 1; fi
echo "$@" >> "`+filepath.Join(dir, "gh-calls")+`"
if [ "$2" = "create" ]; then echo "https://github.com/agent-sh/fake/pull/7"; fi`)
	return f
}

// shipFor is a ship tool over the fixture: live, its gate allowing agent-sh, its sandbox a bubblewrap one over the
// user's home with the Go toolchain read-only and the tests' own build cache, and its posts kept as they are but
// neutral (no scrubber in a test).
func shipFor(t *testing.T, st *store.Store, f repoFixture) *Ship {
	t.Helper()
	sb := testSandbox(t)
	return &Ship{Store: st, Dir: filepath.Join(f.dir, "loop"), Live: true, Actor: "loop:test", Identity: [2]string{"Owner Name", "owner@example.com"},
		GH: f.gh, Sandbox: sb, Public: func(_ context.Context, s string) (string, error) { return Neutral(s), nil },
		Gate: Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{f.guard}, Home: sb.Home, Own: filepath.Join(f.dir, "loop"),
			Resolve: func(context.Context, string) (string, error) { return "agent-sh/fake", nil },
			Busy:    func(context.Context, string) (string, error) { return "", nil }}}
}

func testSandbox(t *testing.T) Sandbox {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bubblewrap here")
	}
	out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output()
	if err != nil {
		t.Skip("no go here")
	}
	env := strings.Fields(string(out))
	home, _ := os.UserHomeDir()
	return Sandbox{Bwrap: bwrap, Home: home, ReadOnly: env, Cache: testCache, Scratch: filepath.Join(t.TempDir(), "tmp"),
		Path: []string{filepath.Join(env[0], "bin")}}
}

// testingCase is a case in testing whose evidence the signal zz-signal-text counts: four failed results.
func testingCase(t *testing.T, st *store.Store) store.Case {
	t.Helper()
	ctx := context.Background()
	var evs []store.Event
	for i := 0; i < 4; i++ {
		evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("s%d@%d", i%2, i), TS: time.Now().Add(-time.Duration(i+1) * time.Hour),
			Session: fmt.Sprint("s", i%2), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash",
			Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`})
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:zz", Title: "the thing breaks"}
	for _, e := range evs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "Answer is off by one", "assert Answer() == 42"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	return c
}

var goodSignal = Signal{Source: store.Transcripts, Kinds: []string{"tool_result"}, Tool: "Bash", Match: []string{"zz-signal-text"}}

const answerTest = "package fake\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(Answer())\n\t}\n}\n"

func fixed(answer string) string { return strings.Replace(fakeGo, "return 41", "return "+answer, 1) }

// A change goes in only through its package: prepared in a work tree of the loop's own, checked by a test that
// fails without it and passes with it (each run on the tree the change commits), proposed with a measurement of
// the case's own problem and a rollback, committed by the user's own identity whatever the environment says,
// reviewed, revised on the same branch and pull request for what a review found, reviewed clean and posted, and
// landed live by a fast-forward of the default branch; its rollback reverts it, takes the case back to testing,
// and lets it be prepared again.
func TestAChangeShipsOnlyThroughItsPackage(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, c.ID, f.main); !errors.Is(err, ErrRefused) {
		t.Fatalf("a second package: %v", err)
	}
	test := "go test ./..."
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	if _, err := s.Check(ctx, c.ID, test, nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no test file") {
		t.Fatalf("a check without a test: %v", err)
	}
	// a test that passes without the change does not discriminate it
	write(t, filepath.Join(p.Work, "note_test.go"), "package fake\n\nimport \"testing\"\n\nfunc TestNote(t *testing.T) {\n\tif Note() == \"\" {\n\t\tt.Fatal(1)\n\t}\n}\n")
	if _, err := s.Check(ctx, c.ID, test, []string{"note_test.go"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "passes without the change") {
		t.Fatalf("a test that passes on the base: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "note_test.go"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./... | tee x", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("a shell in the test command: %v", err)
	}
	// a file the commit would not hold (ignored) is not what the check runs on
	write(t, filepath.Join(p.Work, ".gitignore"), "go.work\n")
	write(t, filepath.Join(p.Work, "go.work"), "this is no go.work file\n")
	if p, err = s.Check(ctx, c.ID, test, nil); err != nil || p.State != Checked || p.Check.Before.Exit == 0 || p.Check.After.Exit != 0 {
		t.Fatalf("check: %+v %v", p.Check, err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Answer was off by one; see agent-sh/fake#3, ask @owner.", Rollback: "revert the commit",
		Lesson: "Answers are 42.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	bad := pr
	bad.Signal = Signal{Source: store.Transcripts, Kinds: []string{"tool_result"}, Match: []string{"something else entirely"}}
	if _, err := s.Propose(ctx, c.ID, bad); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "fewer than") {
		t.Fatalf("a signal that counts nothing: %v", err)
	}
	// an edit after the check needs a new check
	write(t, filepath.Join(p.Work, "extra.go"), "package fake\n")
	if _, err := s.Propose(ctx, c.ID, pr); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "check it again") {
		t.Fatalf("a change edited after its check: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "extra.go"))
	// a change that rewrites the words its signal counts
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "\"zz-signal-text\"", "\"quiet\"", 1))
	if _, err := s.Check(ctx, c.ID, test, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, pr); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "edits") {
		t.Fatalf("a change that edits its signal's text: %v", err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	if _, err := s.Check(ctx, c.ID, test, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMITTER_NAME", "Someone Else")
	t.Setenv("EMAIL", "someone@example.com")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere")) // git's own variables do not point the ship tool elsewhere
	if p, err = s.Propose(ctx, c.ID, pr); err != nil || p.State != Proposed || p.PR != "https://github.com/agent-sh/fake/pull/7" || p.Plan.Before != 4 {
		t.Fatalf("propose: %+v %v", p, err)
	}
	if who := run(t, p.Work, "git", "log", "-1", "--format=%an <%ae> / %cn <%ce>"); strings.TrimSpace(who) != "Owner Name <owner@example.com> / Owner Name <owner@example.com>" {
		t.Fatalf("committed as %q", who)
	}
	if msg := run(t, p.Work, "git", "log", "-1", "--format=%B"); strings.Contains(msg, "#3") || strings.Contains(msg, "@owner") {
		t.Fatalf("the commit's mentions and references are live: %q", msg)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "review") {
		t.Fatalf("landed unreviewed: %v", err)
	}
	if _, err := s.Reviewed(ctx, c.ID, Review{High: 1, Findings: []Finding{{Severity: "low", Where: "x", Problem: "y", Fix: "z"}}}); !errors.Is(err, ErrRefused) ||
		!strings.Contains(err.Error(), "the counts are the findings'") {
		t.Fatalf("counts that are not the findings': %v", err)
	}
	// a review that finds a medium sends it back to its build, on the same branch and pull request
	if p, err = s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "no comment", Fix: "add one"}},
		Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || p.State != Revising || p.Reviews[0].Medium != 1 {
		t.Fatalf("reviewed: %+v %v", p, err)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("landed on a medium: %v", err)
	}
	if _, err := s.Reviewed(ctx, c.ID, Review{Medium: 0, High: 1, Findings: []Finding{{Severity: "low"}}}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a review of a package back at its build: %v", err)
	}
	first := p.Head
	if _, err := s.Check(ctx, c.ID, test, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, pr); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "changes nothing") {
		t.Fatalf("a revision that changes nothing: %v", err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is the answer, 42.", 1))
	if p, err = s.Check(ctx, c.ID, test, nil); err != nil || p.State != Checked {
		t.Fatalf("the revision's check: %+v %v", p, err)
	}
	if p, err = s.Propose(ctx, c.ID, pr); err != nil || p.State != Proposed || p.Head == first || p.PR != "https://github.com/agent-sh/fake/pull/7" {
		t.Fatalf("the revision: %+v %v", p, err)
	}
	if calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls")); !strings.Contains(string(calls), "pr edit https://github.com/agent-sh/fake/pull/7") {
		t.Fatalf("the revision did not update its pull request: %s", calls)
	}
	// a clean review that was not posted lands nothing
	if p, err = s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "low", Where: "x", Problem: "y", Fix: "z"}}}); err != nil || p.Clean() {
		t.Fatalf("an unposted review: %+v %v", p, err)
	}
	p.Reviews[len(p.Reviews)-1].Comment = "https://github.com/agent-sh/fake/pull/7#c2"
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	// a commit after the review is no reviewed commit
	write(t, filepath.Join(p.Work, "late.go"), "package fake\n")
	run(t, p.Work, "git", "add", "-A")
	run(t, p.Work, "git", "commit", "-q", "-m", "late")
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not the reviewed") {
		t.Fatalf("landed a commit after its review: %v", err)
	}
	run(t, p.Work, "git", "reset", "-q", "--hard", p.Head)
	s.Live = false
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not live") {
		t.Fatalf("landed while not live: %v", err)
	}
	s.Live = true
	// the default branch moved since: the landing rebases, checks again and pushes the pull request's head first
	other := filepath.Join(f.dir, "other")
	run(t, f.dir, "git", "clone", "-q", f.origin, other)
	write(t, filepath.Join(other, "README"), "a readme\n")
	run(t, other, "git", "add", "-A")
	run(t, other, "git", "commit", "-q", "-m", "readme")
	run(t, other, "git", "push", "-q", "origin", "HEAD:main")
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed || len(p.Landed) != 2 {
		t.Fatalf("land: %+v %v", p, err)
	}
	if head := strings.TrimSpace(run(t, f.dir, "git", "--git-dir", f.origin, "rev-parse", "main")); head != p.Landed[1] || head != p.Head {
		t.Fatalf("origin's main is %s, not the landed %v", head, p.Landed)
	}
	pushes, _ := os.ReadFile(filepath.Join(f.dir, "pushes"))
	if i, j := strings.Index(string(pushes), "refs/heads/"+p.Branch+" "+p.Head), strings.Index(string(pushes), "refs/heads/main "+p.Head); i < 0 || j < i {
		t.Fatalf("the pull request's head did not reach its branch before the base:\n%s", pushes)
	}
	if who := run(t, f.dir, "git", "--git-dir", f.origin, "log", "-1", "--format=%cn <%ce>", "main"); strings.TrimSpace(who) != "Owner Name <owner@example.com>" {
		t.Fatalf("rebased as %q", who)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseDeployed || got.Deployed.IsZero() {
		t.Fatalf("the case after landing: %+v", got)
	}
	if p, err = s.RollBack(ctx, c.ID, "it did nothing"); err != nil || p.State != RolledBack {
		t.Fatalf("rollback: %+v %v", p, err)
	}
	if body := run(t, f.dir, "git", "--git-dir", f.origin, "show", "main:fake.go"); !strings.Contains(body, "return 41") {
		t.Fatalf("the rollback left %s", body)
	}
	if who := run(t, f.dir, "git", "--git-dir", f.origin, "log", "-1", "--format=%an / %cn", "main"); strings.TrimSpace(who) != "Owner Name / Owner Name" {
		t.Fatalf("reverted as %q", who)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case after the rollback: %s", got.Status)
	}
	if p, err = s.Prepare(ctx, c.ID, f.main); err != nil || p.State != Prepared {
		t.Fatalf("prepared again after its rollback: %+v %v", p, err)
	}
}

// The scope check sees every work tree of the repository: a proposal commits first, so the loop's own tree is
// clean when it looks. A proposal whose pull request could not be opened is taken up again where it stopped,
// without a second commit.
func TestAProposalCommitsBeforeTheScopeCheckAndResumes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	write(t, filepath.Join(f.dir, "gh-fails"), "")
	if p, err = s.Propose(ctx, c.ID, pr); err == nil || p.State != Checked || p.Head == "" {
		t.Fatalf("a proposal whose pull request failed: %+v %v", p, err)
	}
	head := p.Head
	if p, err = s.Propose(ctx, c.ID, pr); err != nil || p.State != Proposed || p.Head != head {
		t.Fatalf("taken up again: %+v %v", p, err)
	}
	if n := strings.Count(run(t, p.Work, "git", "log", "--format=%s", "origin/main..HEAD"), "\n"); n != 1 {
		t.Fatalf("%d commits on the branch", n)
	}
	// a dirty work tree of another (the owner's checkout) is report only
	write(t, filepath.Join(f.main, "wip.txt"), "his work\n")
	if _, err := s.Gate.Repository(ctx, f.main); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("a dirty checkout of his: %v", err)
	}
}

// The ship tool's git runs none of the repository's hooks (a tracked hooks directory is the change's own), and a
// work tree whose link to its repository was changed is refused.
func TestTheChangesOwnGitFilesDoNotRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	f := newRepo(t, map[string]string{".githooks/post-commit": hook, ".githooks/post-checkout": hook})
	os.Chmod(filepath.Join(f.main, ".githooks", "post-commit"), 0o755)
	os.Chmod(filepath.Join(f.main, ".githooks", "post-checkout"), 0o755)
	run(t, f.main, "git", "commit", "-q", "-am", "hooks run")
	run(t, f.main, "git", "push", "-q", "origin", "HEAD:main")
	run(t, f.main, "git", "config", "core.hooksPath", ".githooks")
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	write(t, filepath.Join(p.Work, ".githooks", "post-commit"), "#!/bin/sh\ntouch "+marker+" # the change's own\n")
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, Proposal{Title: "t", Body: "b", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook of the repository ran")
	}
	c2 := testingCase2(t, st)
	p2, err := s.Prepare(ctx, c2.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p2.Work, ".git"), "gitdir: "+filepath.Join(t.TempDir(), "evil")+"\n")
	if _, err := s.Check(ctx, c2.ID, "go test ./...", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("a work tree linked elsewhere: %v", err)
	}
}

func testingCase2(t *testing.T, st *store.Store) store.Case {
	t.Helper()
	ctx := context.Background()
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:zz2", Title: "another",
		Evidence: []store.Evidence{{Ref: "file:/t/zz2", At: time.Now().Add(-time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "m", "n"
	c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// In momentum's own repository a change may not touch what the loop's judgment or authority rests on: not by an
// edit, not by moving the file away, not by a new file whose name git would quote.
func TestAChangeToTheLoopsOwnCodeIsRefusedHoweverItIsMade(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, map[string]string{"internal/loop/gate.go": "package loop\n", "internal/supervise/rules.go": "package supervise\n", "docs/x.txt": "notes\n"})
	s := shipFor(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return SelfRepo, nil }
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	run(t, p.Work, "git", "mv", "internal/loop/gate.go", "gate.go.txt")
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "internal/loop/gate.go") {
		t.Fatalf("a protected file moved away: %v", err)
	}
	run(t, p.Work, "git", "mv", "gate.go.txt", "internal/loop/gate.go")
	run(t, p.Work, "git", "mv", "docs/x.txt", "internal/loop/x.go")
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "internal/loop/x.go") {
		t.Fatalf("a file moved into a protected path: %v", err)
	}
	run(t, p.Work, "git", "mv", "internal/loop/x.go", "docs/x.txt")
	write(t, filepath.Join(p.Work, "internal", "loop", "é.go"), "package loop\n")
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "internal/loop/é.go") {
		t.Fatalf("a new protected file with a quoted name: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "internal", "loop", "é.go"))
	write(t, filepath.Join(p.Work, "internal", "supervise", "rules.go"), "package supervise\n\n// a rule\n")
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatalf("supervision's rules are the system it improves: %v", err)
	}
}

// The gate is the loop's authority: a repository of an owner it may write to, not excluded, that the machine's
// scope check allows and nobody works in; never what momentum's judgment rests on in its own repository.
func TestTheGateKeepsTheLoopsAuthority(t *testing.T) {
	ctx := context.Background()
	f := newRepo(t, nil)
	g := Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{f.guard},
		Resolve: func(context.Context, string) (string, error) { return "agent-sh/fake", nil },
		Busy:    func(context.Context, string) (string, error) { return "", nil }}
	if _, err := g.Repository(ctx, f.main); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Gate){
		"another owner":  func(g *Gate) { g.Owners = []string{"someone"} },
		"excluded":       func(g *Gate) { g.Exclude = []string{"agent-sh/fake"} },
		"no scope check": func(g *Gate) { g.RepoGuard = nil },
		"busy": func(g *Gate) {
			g.Busy = func(context.Context, string) (string, error) { return "session x works in it", nil }
		},
		"unknown work": func(g *Gate) {
			g.Busy = func(context.Context, string) (string, error) { return "", errors.New("no look") }
		},
	} {
		gg := g
		change(&gg)
		if _, err := gg.Repository(ctx, f.main); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	write(t, filepath.Join(f.dir, "report-only"), "")
	if _, err := g.Repository(ctx, f.main); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "report_only") {
		t.Errorf("the scope check's report only: %v", err)
	}
	for _, p := range []string{"internal/detect/failures.go", "internal/loop/gate.go", "internal/measure/x.go", "internal/store/events.go",
		"internal/source/transcripts.go", "internal/plain/plain.go", "internal/config/config.go", "internal/paths/paths.go",
		"cmd/momentum/git.go", "cmd/momentum/replay.go", "go.mod", "go.sum", ".github/workflows/ci.yml", "SPEC.md", "AGENTS.md"} {
		if err := CheckPaths(Target{Repo: SelfRepo}, []string{"README.md", p}); !errors.Is(err, ErrRefused) {
			t.Errorf("momentum's own %s: %v", p, err)
		}
	}
	if err := CheckPaths(Target{Repo: SelfRepo}, []string{"internal/supervise/rules.go", "internal/capacity/rules.go", "README.md"}); err != nil {
		t.Errorf("supervision's and capacity's rules are no evaluator: %v", err)
	}
	if err := CheckPaths(Target{Repo: "agent-sh/fake"}, []string{"internal/loop/x.go"}); err != nil {
		t.Errorf("another repository's paths: %v", err)
	}
	Protected()[0] = "x"
	if err := CheckPaths(Target{Repo: SelfRepo}, []string{"internal/detect/x.go"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a change to the list one call returned changed the next: %v", err)
	}
}

// A test command is one program and its words, run without a shell.
func TestATestCommandRunsWithoutAShell(t *testing.T) {
	if w, err := Words(`go test ./internal/x -run 'TestA|TestB' -count=1`); err != nil || strings.Join(w, "|") != "go|test|./internal/x|-run|TestA|TestB|-count=1" {
		t.Fatalf("%q %v", w, err)
	}
	for _, bad := range []string{"go test ./... | tee x", "go test && rm -rf /", "go test $(echo x)", "rm -rf x", "go test 'unclosed", "", "go test > out", "go test -run $NAME"} {
		if _, err := Words(bad); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// A check's test runs where it can change only its tree: it writes nothing outside it, reaches no network and no
// socket of the user's (his session's bus, an agent, a daemon), sees none of his home's files but the toolchains,
// and has nothing of his session in its environment.
func TestTheSandboxKeepsATestInItsTree(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 here")
	}
	sb := testSandbox(t)
	home, err := os.MkdirTemp("", "home-") // a home of the test's own, outside /tmp, with a file of his in it
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	sb.Home = home
	tree := t.TempDir()
	write(t, filepath.Join(tree, ".git"), "gitdir: /x\n")
	outside := t.TempDir()
	sock := filepath.Join(outside, "s.sock")
	socketDir, err := os.Open(outside)
	if err != nil {
		t.Fatal(err)
	}
	defer socketDir.Close()
	l, err := net.Listen("unix", fmt.Sprintf("/proc/self/fd/%d/s.sock", socketDir.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	control, err := net.Dial("unix", fmt.Sprintf("/proc/self/fd/%d/s.sock", socketDir.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	control.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	secret := filepath.Join(sb.Home, ".config", "x", "token")
	write(t, secret, "a secret")
	probe := fmt.Sprintf(`
import os, socket, sys
bad = []
cwd = os.getcwd()
try:
    os.chdir(%q)
    s = socket.socket(socket.AF_UNIX); s.connect("s.sock"); bad.append("unix socket")
except OSError: pass
finally: os.chdir(cwd)
try:
    s = socket.create_connection(("127.0.0.1", %d), timeout=2); bad.append("network")
except OSError: pass
try:
    open(%q, "w").write("x"); bad.append("wrote outside")
except OSError: pass
if os.path.exists(%q): bad.append("read the home")
if os.listdir("/run"): bad.append("/run holds " + " ".join(os.listdir("/run")[:5]))
try:
    open(".git", "w").write("gitdir: /elsewhere"); bad.append("wrote its .git link")
except OSError: pass
for k in ("DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK"):
    if k in os.environ: bad.append(k)
if os.environ.get("HOME") != %q: bad.append("HOME is " + os.environ.get("HOME", ""))
open("inside.txt", "w").write("ok")
print(" ".join(bad)); sys.exit(1 if bad else 0)
`, outside, tcp.Addr().(*net.TCPAddr).Port, filepath.Join(outside, "x"), secret, sb.Home)
	t.Setenv("SSH_AUTH_SOCK", sock)
	o, err := sb.Run(context.Background(), tree, []string{"python3", "-c", probe}, nil)
	if err != nil || o.Exit != 0 {
		t.Fatalf("exit %d %v: %s", o.Exit, err, o.Tail)
	}
	if b, _ := os.ReadFile(filepath.Join(tree, "inside.txt")); string(b) != "ok" {
		t.Fatal("the test could not write its own tree")
	}
}

// A change may not remove a line holding its signal's text, whatever the line starts with.
func TestAChangeMayNotRewriteItsSignalsText(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, map[string]string{"notes.md": "-- zz-signal-text is what fails\n"})
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "notes.md"), "quiet now\n")
	if _, _, err := s.staged(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.editsSignal(ctx, p, goodSignal); !errors.Is(err, ErrRefused) {
		t.Fatalf("a removed line that opens with dashes: %v", err)
	}
}

// What the loop posts is scrubbed of secrets and its mentions and references are made inert; with no scrubber
// nothing is posted.
func TestWhatTheLoopPostsIsScrubbedAndInert(t *testing.T) {
	dir := t.TempDir()
	was := Scrubber
	defer func() { Scrubber = was }()
	Scrubber = []string{script(t, dir, "scrub", "sed 's/hunter2/*******/g'")}
	got, err := Public(context.Background(), "the password hunter2; ping @owner and agent-sh/x#12, see #3, mail a@b.example")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "@owner") || strings.Contains(got, "x#12") || strings.Contains(got, " #3") ||
		!strings.Contains(got, "a@b.example") {
		t.Fatalf("%q", got)
	}
	Scrubber = []string{filepath.Join(dir, "none")}
	if _, err := Public(context.Background(), "text"); !errors.Is(err, ErrNoScrubber) {
		t.Fatalf("no scrubber: %v", err)
	}
}

// A measurement counts the case's own problem: a case that holds no event of the store has nothing of it to count.
func TestAProposalNeedsTheCasesOwnEvents(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	testingCase(t, st) // the store's events the signal counts, of another case
	c := testingCase2(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, Proposal{Title: "t", Body: "b", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour}); !errors.Is(err, ErrRefused) ||
		!strings.Contains(err.Error(), "holds no event") {
		t.Fatalf("a case with no event of its own: %v", err)
	}
}

// reviewedChange takes the fixture's case to a proposed change whose clean review was posted: what a landing needs.
func reviewedChange(t *testing.T, s *Ship, st *store.Store, f repoFixture) (store.Case, Package) {
	t.Helper()
	ctx := context.Background()
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || !p.Clean() {
		t.Fatalf("a clean review: %+v %v", p, err)
	}
	return c, p
}

// originHead is a branch's head in the fixture's origin.
func originHead(t *testing.T, f repoFixture, branch string) string {
	t.Helper()
	return strings.TrimSpace(run(t, f.dir, "git", "--git-dir", f.origin, "rev-parse", branch))
}

// pushOther commits files to the origin's branch from a clone of someone else's.
func pushOther(t *testing.T, f repoFixture, branch string, files map[string]string) string {
	t.Helper()
	other := t.TempDir()
	run(t, f.dir, "git", "clone", "-q", "-b", branch, f.origin, other)
	for p, body := range files {
		write(t, filepath.Join(other, p), body)
	}
	run(t, other, "git", "add", "-A")
	run(t, other, "git", "commit", "-q", "-m", "someone else's")
	run(t, other, "git", "push", "-q", "origin", "HEAD:"+branch)
	return strings.TrimSpace(run(t, other, "git", "rev-parse", "HEAD"))
}

// Every step after a change's check checks again what it rests on. A check refuses a change that fails its own
// test and a test file the change does not touch; a proposal, a landing and a rollback each refuse what the gate
// refuses now, before anything is pushed; a landing refuses a work tree that holds changes besides the reviewed
// commit, and one whose .git was pointed at another repository that holds the same commit.
func TestEachStepChecksAgainWhatItRestsOn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	test := "go test ./..."
	write(t, filepath.Join(p.Work, "fake.go"), fixed("43"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, test, nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "fails with the change") {
		t.Fatalf("a change that fails its own test: %v", err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	if _, err := s.Check(ctx, c.ID, test, []string{"other_test.go"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not a file the change touches") {
		t.Fatalf("a test file the change does not touch: %v", err)
	}
	if _, err := s.Check(ctx, c.ID, test, nil); err != nil {
		t.Fatal(err)
	}
	base := originHead(t, f, "main")
	stop := filepath.Join(f.dir, "report-only")
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	write(t, stop, "")
	if _, err := s.Propose(ctx, c.ID, pr); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "report_only") {
		t.Fatalf("a proposal the gate refuses now: %v", err)
	}
	if pushes, _ := os.ReadFile(filepath.Join(f.dir, "pushes")); strings.Contains(string(pushes), p.Branch) {
		t.Fatalf("a proposal the gate refused was pushed:\n%s", pushes)
	}
	os.Remove(stop)
	if _, err := s.Propose(ctx, c.ID, pr); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Comment: "https://github.com/agent-sh/fake/pull/7#c1"}); err != nil || !p.Clean() {
		t.Fatalf("a clean review: %+v %v", p, err)
	}
	write(t, stop, "")
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "report_only") || originHead(t, f, "main") != base {
		t.Fatalf("a landing the gate refuses now: %v", err)
	}
	os.Remove(stop)
	write(t, filepath.Join(p.Work, "stray.txt"), "not reviewed\n")
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "holds changes") || originHead(t, f, "main") != base {
		t.Fatalf("a landing with a change besides the reviewed commit: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "stray.txt"))
	// another repository that holds the reviewed commit, and a .git pointed at it
	elsewhere := t.TempDir()
	run(t, f.dir, "git", "clone", "-q", f.origin, elsewhere)
	run(t, elsewhere, "git", "fetch", "-q", "origin", p.Branch)
	run(t, elsewhere, "git", "checkout", "-q", "--detach", p.Head)
	link, _ := os.ReadFile(filepath.Join(p.Work, ".git"))
	write(t, filepath.Join(p.Work, ".git"), "gitdir: "+filepath.Join(elsewhere, ".git")+"\n")
	if head := strings.TrimSpace(run(t, p.Work, "git", "rev-parse", "HEAD")); head != p.Head {
		t.Fatalf("the other repository's head is %s, not the reviewed %s", head, p.Head)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), ".git points to") || originHead(t, f, "main") != base {
		t.Fatalf("a landing through a .git pointed elsewhere: %v", err)
	}
	write(t, filepath.Join(p.Work, ".git"), string(link))
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed {
		t.Fatalf("land: %+v %v", p, err)
	}
	landed := originHead(t, f, "main")
	write(t, stop, "")
	if _, err := s.RollBack(ctx, c.ID, "no effect"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "report_only") || originHead(t, f, "main") != landed {
		t.Fatalf("a rollback the gate refuses now: %v", err)
	}
	os.Remove(stop)
	if p, err = s.RollBack(ctx, c.ID, "no effect"); err != nil || p.State != RolledBack {
		t.Fatalf("rollback: %+v %v", p, err)
	}
}

// A landing tests the change as it lands: rebased on a default branch under which its test fails, it does not
// land.
func TestALandingTestsTheRebasedChange(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, _ := reviewedChange(t, s, st, f)
	moved := pushOther(t, f, "main", map[string]string{"boom_test.go": "package fake\n\nimport \"testing\"\n\nfunc TestBoom(t *testing.T) { t.Fatal(\"boom\") }\n"})
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "after the rebase the test fails") {
		t.Fatalf("a rebased change whose test fails: %v", err)
	}
	if head := originHead(t, f, "main"); head != moved {
		t.Fatalf("the default branch is %s, not %s", head, moved)
	}
}

// A landing pushes the default branch only as a fast-forward: a commit that reaches it between the landing's
// rebase and its push stays, and the landing does not go in.
func TestALandingPushesOnlyAFastForward(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, _ := reviewedChange(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"README": "a readme\n"}) // the base moved: the landing rebases and pushes its branch first
	// when the branch is pushed, someone's commit reaches the default branch
	race, raced := filepath.Join(f.dir, "race"), filepath.Join(f.dir, "raced")
	script(t, filepath.Join(f.origin, "hooks"), "post-receive", `while read old new ref; do
  echo "$ref $new" >> "`+filepath.Join(f.dir, "pushes")+`"
  case "$ref" in refs/heads/momentum/*)
    if [ -e "`+race+`" ]; then
      rm "`+race+`"
      tree=$(git rev-parse refs/heads/main^{tree})
      c=$(echo race | GIT_AUTHOR_NAME=r GIT_AUTHOR_EMAIL=r@example.com GIT_COMMITTER_NAME=r GIT_COMMITTER_EMAIL=r@example.com git commit-tree -p refs/heads/main "$tree")
      git update-ref refs/heads/main "$c"
      echo "$c" > "`+raced+`"
    fi ;;
  esac
done`)
	write(t, race, "")
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "fast-forward") {
		t.Fatalf("a landing past a commit that came between: %v", err)
	}
	other, _ := os.ReadFile(raced)
	if head := originHead(t, f, "main"); head != strings.TrimSpace(string(other)) || head == "" {
		t.Fatalf("the default branch is %s, not the commit that came between (%s)", head, other)
	}
}

// A revision does not overwrite what someone else pushed to its branch since: its push holds a lease on the branch
// as the loop last pushed it.
func TestARevisionDoesNotOverwriteAnotherPushToItsBranch(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	if p, err = s.Propose(ctx, c.ID, pr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "p", Fix: "f"}}, Comment: "c1"}); err != nil {
		t.Fatal(err)
	}
	theirs := pushOther(t, f, p.Branch, map[string]string{"theirs.txt": "a fix of someone's\n"})
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is 42.", 1))
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, pr); err == nil {
		t.Fatal("a revision overwrote another push to its branch")
	}
	if head := originHead(t, f, p.Branch); head != theirs {
		t.Fatalf("the branch is %s, not the other push %s", head, theirs)
	}
}

// A package prepared again after its rollback, in another checkout of the repository, leaves no registration of
// its old work tree in the first.
func TestAPackagePreparedAgainLeavesNoOldTreeBehind(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	if _, err := s.Land(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollBack(ctx, c.ID, "no effect"); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(f.dir, "second")
	run(t, f.dir, "git", "clone", "-q", f.origin, second)
	q, err := s.Prepare(ctx, c.ID, second)
	if err != nil || q.Dir != second || q.Work != p.Work {
		t.Fatalf("prepared again in another checkout: %+v %v", q, err)
	}
	if list := run(t, f.main, "git", "worktree", "list", "--porcelain"); strings.Contains(list, p.Work) {
		t.Fatalf("the first checkout still registers the old work tree:\n%s", list)
	}
}

// A measurement's signal counts the case's own problem: one that counts enough events, none of them the case's, is
// refused.
func TestAPlanMeasuresTheCasesOwnProblem(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	s := &Ship{Store: st}
	c := testingCase(t, st)
	var evs []store.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprint("other@", i), TS: time.Now().Add(-time.Duration(i+1) * time.Hour),
			Session: "s9", Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-other-text"})
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	other := Signal{Source: store.Transcripts, Kinds: []string{"tool_result"}, Tool: "Bash", Match: []string{"zz-other-text"}}
	if _, err := s.validPlan(ctx, c, other, 7*24*time.Hour); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "does not measure the case's problem") {
		t.Fatalf("a signal of another problem: %v", err)
	}
	if n, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour); err != nil || n != 4 {
		t.Fatalf("the case's own signal: %d %v", n, err)
	}
}

// A review is clean only of the head it read: a later commit needs its own.
func TestACleanReviewIsOfTheHeadProposed(t *testing.T) {
	p := Package{Head: "b", Reviews: []Review{{Head: "a", Comment: "c1"}}}
	if p.Clean() {
		t.Fatal("a review of an older head is clean on the new one")
	}
	p.Reviews[0].Head = "b"
	if !p.Clean() {
		t.Fatal("a clean posted review of the head")
	}
}

// The work tree's branch activates its own configuration before any file is materialized. Every later step
// refuses a driver activated there too, and none runs it on the host.
func TestBranchConditionalDriversAreRefused(t *testing.T) {
	for _, step := range []string{"prepare", "check", "propose", "land"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, map[string]string{".gitattributes": "fake.go filter=fixture\n"})
			s := shipFor(t, st, f)
			c := testingCase(t, st)
			marker := filepath.Join(f.dir, "marker")
			filter := script(t, f.dir, "filter", "echo ran > '"+marker+"'\ncat")
			cfg := filepath.Join(f.dir, "branch-config")
			write(t, cfg, "[filter \"fixture\"]\n smudge = "+filter+"\n clean = "+filter+"\n")
			var p Package
			var err error
			if step != "prepare" {
				p, err = s.Prepare(ctx, c.ID, f.main)
				if err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
				write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
				if step != "check" {
					if _, err = s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
						t.Fatal(err)
					}
				}
				if step == "land" {
					if _, err = s.Propose(ctx, c.ID, Proposal{Title: "Answer", Body: "Fix it.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
						t.Fatal(err)
					}
					if _, err = s.Reviewed(ctx, c.ID, Review{Comment: "https://example.invalid/review"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			run(t, f.main, "git", "config", "includeIf.onbranch:momentum/**.path", cfg)
			if err := CheckGitConfig(ctx, f.main); !errors.Is(err, ErrRefused) {
				t.Fatalf("conditional include accepted on main: %v", err)
			}
			switch step {
			case "prepare":
				_, err = s.Prepare(ctx, c.ID, f.main)
			case "check":
				_, err = s.Check(ctx, c.ID, "go test ./...", nil)
			case "propose":
				_, err = s.Propose(ctx, c.ID, Proposal{Title: "Answer", Body: "Fix it.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour})
			case "land":
				_, err = s.Land(ctx, c.ID)
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("%s: %v", step, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("driver ran: %v", err)
			}
			if step == "prepare" {
				if _, err := os.Stat(filepath.Join(s.workDir(c.ID), "fake.go")); !os.IsNotExist(err) {
					t.Fatal("files materialized before branch configuration check")
				}
			}
		})
	}
}

// A configured signer, verifier or transformation is refused even on a signed ancestor. With configuration
// refusal removed by an overlay, the same fixture checks the remaining pins through the ship lifecycle.
func TestShipGitRunsNoRepositoryPrograms(t *testing.T) {
	for _, surface := range []string{"signatures", "attributes"} {
		t.Run(surface, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, map[string]string{".gitattributes": "fake.go filter=fixture diff=fixture merge=fixture\n"})
			s := shipFor(t, st, f)
			marker := filepath.Join(f.dir, "marker")
			program := script(t, f.dir, "program", "echo ran > '"+marker+"'\ncat")
			tree := strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD^{tree}"))
			raw := "tree " + tree + "\nauthor Fixture <fixture@example.invalid> 1700000000 +0000\ncommitter Fixture <fixture@example.invalid> 1700000000 +0000\ngpgsig -----BEGIN PGP SIGNATURE-----\n c3ludGhldGlj\n -----END PGP SIGNATURE-----\n\nfixture\n"
			object := filepath.Join(f.dir, "object")
			write(t, object, raw)
			sha := strings.TrimSpace(run(t, f.main, "git", "hash-object", "-t", "commit", "-w", object))
			run(t, f.main, "git", "reset", "--hard", sha)
			run(t, f.main, "git", "push", "--force", "origin", "HEAD:main")
			if surface == "attributes" {
				for _, key := range []string{"filter.fixture.clean", "filter.fixture.smudge", "diff.fixture.command", "merge.fixture.driver"} {
					run(t, f.main, "git", "config", key, program)
				}

			} else {
				for _, key := range []string{"gpg.program", "gpg.openpgp.program", "gpg.x509.program", "gpg.ssh.program"} {
					run(t, f.main, "git", "config", key, program)
				}
				for _, key := range []string{"log.showSignature", "commit.gpgSign", "push.gpgSign"} {
					run(t, f.main, "git", "config", key, "true")
				}
			}
			if err := CheckGitConfig(ctx, f.main); err != nil {
				if !errors.Is(err, ErrRefused) {
					t.Fatal(err)
				}
				if _, err := s.Prepare(ctx, testingCase(t, st).ID, f.main); !errors.Is(err, ErrRefused) {
					t.Fatal(err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("repository program ran: %v", err)
				}
				return
			}
			// The scope checker is an independent program. This fixture checks only its authority receipt.
			s.Gate.RepoGuard = []string{script(t, f.dir, "allow", `echo '{"allowed":true}'`)}
			c, p := reviewedChange(t, s, st, f)
			if _, err := s.git(ctx, p.Work, "log", "--format=%G?"); err != nil {
				t.Fatal(err)
			}
			if _, err := gitOutput(ctx, p.Work, "log", "--format=%G?"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Land(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RollBack(ctx, c.ID, "fixture"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("repository program ran: %v", err)
			}
		})
	}
}

// Fetch and every expanded push URL name the same repository, including rewrites and branch-local settings.
func TestPushDestinationsMustAgree(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"pushurl", "multiple", "pushInsteadOf", "insteadOf", "branch"} {
		t.Run(mode, func(t *testing.T) {
			f := newRepo(t, nil)
			run(t, f.main, "git", "remote", "set-url", "origin", "https://github.com/agent-sh/fake.git")
			dir := f.main
			switch mode {
			case "pushurl":
				run(t, dir, "git", "config", "remote.origin.pushurl", "https://github.com/elsewhere/other.git")
			case "multiple":
				run(t, dir, "git", "config", "--add", "remote.origin.pushurl", "git@github.com:agent-sh/fake.git")
				run(t, dir, "git", "config", "--add", "remote.origin.pushurl", "https://github.com/elsewhere/other.git")
			case "pushInsteadOf":
				run(t, dir, "git", "config", "url.https://github.com/elsewhere/.pushInsteadOf", "https://github.com/agent-sh/")
			case "insteadOf":
				run(t, dir, "git", "config", "remote.origin.pushurl", "fixture:other.git")
				run(t, dir, "git", "config", "url.https://github.com/elsewhere/.insteadOf", "fixture:")
			case "branch":
				cfg := filepath.Join(f.dir, "branch-config")
				write(t, cfg, "[remote \"origin\"]\n pushurl = https://github.com/elsewhere/other.git\n")
				run(t, dir, "git", "config", "includeIf.onbranch:momentum/**.path", cfg)
				dir = filepath.Join(f.dir, "branch")
				run(t, f.main, "git", "worktree", "add", "-b", "momentum/fixture", dir, "HEAD")
			}
			if repo, err := RepoOf(ctx, dir); !errors.Is(err, ErrRefused) {
				t.Fatalf("%s: %s %v", mode, repo, err)
			}
			gate := reviewGate(t)
			if _, err := gate.Repository(ctx, dir); !errors.Is(err, ErrRefused) {
				t.Fatalf("gate: %v", err)
			}
		})
	}
	f := newRepo(t, nil)
	run(t, f.main, "git", "remote", "set-url", "origin", "https://github.com/agent-sh/fake.git")
	run(t, f.main, "git", "config", "--add", "remote.origin.pushurl", "git@github.com:agent-sh/fake.git")
	run(t, f.main, "git", "config", "--add", "remote.origin.pushurl", "ssh://git@github.com/agent-sh/fake")
	if repo, err := RepoOf(ctx, f.main); err != nil || repo != "agent-sh/fake" {
		t.Fatalf("same repository: %s %v", repo, err)
	}
}

// A changed push destination cannot take a proposal, landing, deletion or rollback. The check uses the actual
// work tree and compares its identity to the package, even when the fetch and push URLs agree with each other.
func TestEveryPushChecksItsPackageDestination(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	other := filepath.Join(f.dir, "other.git")
	run(t, f.dir, "git", "clone", "--bare", f.origin, other)
	base := originHead(t, f, "main")
	for _, rewrite := range []bool{false, true} {
		if rewrite {
			run(t, f.main, "git", "config", "--unset-all", "remote.origin.pushurl")
			run(t, f.main, "git", "config", "url."+other+".pushInsteadOf", f.origin)
		} else {
			run(t, f.main, "git", "config", "remote.origin.pushurl", other)
		}
		for _, args := range [][]string{{"--no-verify", "origin", "HEAD:refs/heads/" + p.Branch}, {"--no-verify", "origin", "HEAD:refs/heads/main"}, {"--no-verify", "origin", "--delete", p.Branch}} {
			if _, err := s.push(ctx, p, p.Work, args...); !errors.Is(err, ErrRefused) {
				t.Fatalf("push %v: %v", args, err)
			}
		}
		if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
			t.Fatalf("land: %v", err)
		}
		if got := strings.TrimSpace(run(t, f.dir, "git", "--git-dir", other, "rev-parse", "main")); got != base {
			t.Fatal("foreign repository changed")
		}
	}
	run(t, f.main, "git", "config", "--unset-all", "url."+other+".pushInsteadOf")
	s.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/other", nil }
	if _, err := s.push(ctx, p, p.Work, "origin", "HEAD:refs/heads/main"); !errors.Is(err, ErrRefused) {
		t.Fatalf("different package identity: %v", err)
	}
	if originHead(t, f, "main") != base {
		t.Fatal("origin changed")
	}
}

// A tracked transport cannot redirect a proposal or landing. The advertised origin is GitHub-shaped,
// but the synthetic wrapper only routes to local bare repositories and must never run after configuration.
func TestShipRefusesRepositoryTransport(t *testing.T) {
	for _, step := range []string{"prepare", "check", "propose", "land"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, map[string]string{"transport": "exit 1\n"})
			s := shipFor(t, st, f)
			c := testingCase(t, st)
			other := filepath.Join(f.dir, "other.git")
			run(t, f.dir, "git", "clone", "--bare", f.origin, other)
			marker := filepath.Join(f.dir, "transport-ran")
			transport := "echo ran > '" + marker + "'\ncase \"$*\" in\n *git-upload-pack*) exec git-upload-pack '" + f.origin + "';;\n *git-receive-pack*) exec git-receive-pack '" + other + "';;\n *) exit 1;;\nesac\n"
			dir := f.main
			if step != "prepare" {
				p, err := s.Prepare(ctx, c.ID, f.main)
				if err != nil {
					t.Fatal(err)
				}
				dir = p.Work
				write(t, filepath.Join(dir, "transport"), transport)
				write(t, filepath.Join(dir, "fake.go"), fixed("42"))
				write(t, filepath.Join(dir, "fake_test.go"), answerTest)
				if step != "check" {
					if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
						t.Fatal(err)
					}
				}
				if step == "land" {
					if _, err := s.Propose(ctx, c.ID, Proposal{Title: "Fixture", Body: "Synthetic change.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Reviewed(ctx, c.ID, Review{Comment: "https://example.invalid/review"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if step == "prepare" {
				write(t, filepath.Join(dir, "transport"), transport)
			}
			run(t, f.main, "git", "config", "core.sshCommand", "sh transport")
			run(t, f.main, "git", "remote", "set-url", "origin", "ssh://git@github.com/agent-sh/fake.git")
			s.Gate.Resolve = nil
			s.Gate.RepoGuard = []string{script(t, f.dir, "allow", `echo '{"allowed":true}'`)}
			var err error
			switch step {
			case "prepare":
				_, err = s.Prepare(ctx, c.ID, dir)
			case "check":
				_, err = s.Check(ctx, c.ID, "go test ./...", nil)
			case "propose":
				_, err = s.Propose(ctx, c.ID, Proposal{Title: "Fixture", Body: "Synthetic change.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour})
			case "land":
				_, err = s.Land(ctx, c.ID)
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("transport accepted: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("transport ran: %v", err)
			}
		})
	}
}

// A monitor configured after prepare cannot run through the status calls of the fixture scope checker.
func TestScopeRefusesAnEditedRepositoryMonitor(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, map[string]string{"monitor": "#!/bin/sh\nexit 1\n"})
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.dir, "monitor-ran")
	script(t, p.Work, "monitor", "echo ran > '"+marker+"'\nexit 1")
	run(t, f.main, "git", "config", "core.fsmonitor", "./monitor")
	if err := s.Gate.scope(ctx, "agent-sh/fake", p.Work); !errors.Is(err, ErrRefused) {
		t.Fatalf("scope accepted monitor: %v", err)
	}
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("check accepted monitor: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("monitor ran: %v", err)
	}
}

// Ship resets credential helpers and supplies only its own GitHub helper, including an executable path
// with shell punctuation. Caller and global configuration cannot add a helper; local helpers are refused.
func TestShipOwnsItsCredentialPath(t *testing.T) {
	ctx := context.Background()
	f := newRepo(t, nil)
	marker := filepath.Join(f.dir, "helper-ran")
	gh := script(t, f.dir, "forge's cli", `test "$1 $2" = 'auth git-credential' || exit 1
cat >/dev/null
echo ran > '`+marker+`'
printf 'username=fixture\npassword=synthetic\n'`)
	s := &Ship{GH: gh}
	run(t, f.main, "git", "config", "protocol.ext.allow", "always")
	global := filepath.Join(t.TempDir(), "global-config")
	write(t, global, "[credential]\n helper = untrusted\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "untrusted")
	t.Setenv("GIT_CONFIG_KEY_1", "protocol.ext.allow")
	t.Setenv("GIT_CONFIG_VALUE_1", "always")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper=untrusted'")
	for _, cli := range []string{gh, ""} {
		s.GH = cli
		for key, want := range map[string]string{"credential.helper": "\n", "credential.https://github.com.helper": "!" + shellQuote(firstOf(cli, "gh")) + " auth git-credential\n"} {
			out, err := s.git(ctx, f.main, "config", "--get-all", key)
			if err != nil || out != want {
				t.Fatalf("%s: %q, want %q: %v", key, out, want, err)
			}
		}
		if out, err := s.git(ctx, f.main, "config", "--get", "protocol.ext.allow"); err != nil || out != "never\n" {
			t.Fatalf("effective ext protocol policy: %q %v", out, err)
		}
	}
	s.GH = gh
	// The effective helper command invokes the configured CLI without contacting a remote or reading credentials.
	out, err := s.git(ctx, f.main, "config", "--get-urlmatch", "credential.helper", "https://github.com/agent-sh/fixture")
	if err != nil {
		t.Fatal(err)
	}
	helper := strings.TrimPrefix(strings.TrimSpace(out), "!")
	cmd := exec.Command("sh", "-c", helper+" get")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "username=fixture") {
		t.Fatalf("helper: %s %v", out, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("trusted helper did not run: %v", err)
	}
	// Set local configuration without the caller's injected settings.
	cmd = exec.Command("git", "-C", f.main, "config", "credential.helper", "untrusted")
	cmd.Env = GitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure fixture: %s %v", out, err)
	}
	if _, err := s.git(ctx, f.main, "config", "--get-all", "credential.helper"); !errors.Is(err, ErrRefused) {
		t.Fatalf("local helper accepted: %v", err)
	}
}
