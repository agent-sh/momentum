package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

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
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
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
// and the scripts the ship tool calls: a scope check that allows it, and a GitHub CLI that opens a pull request.
type repoFixture struct {
	origin, main, dir string
	guard, gh         string
}

func newRepo(t *testing.T) repoFixture {
	t.Helper()
	dir := t.TempDir()
	f := repoFixture{dir: dir, origin: filepath.Join(dir, "origin.git"), main: filepath.Join(dir, "main")}
	run(t, dir, "git", "init", "-q", "--bare", "-b", "main", f.origin)
	run(t, dir, "git", "clone", "-q", f.origin, f.main)
	write(t, filepath.Join(f.main, "go.mod"), "module example.com/fake\n\ngo 1.21\n")
	write(t, filepath.Join(f.main, "fake.go"), "package fake\n\n// Answer is the answer.\nfunc Answer() int { return 41 }\n\n// Note says zz-signal-text.\nfunc Note() string { return \"zz-signal-text\" }\n")
	run(t, f.main, "git", "add", "-A")
	run(t, f.main, "git", "commit", "-q", "-m", "init")
	run(t, f.main, "git", "push", "-q", "origin", "HEAD:main")
	run(t, f.main, "git", "remote", "set-head", "origin", "main")
	f.guard = script(t, dir, "guard", `if [ -e "`+filepath.Join(dir, "report-only")+`" ]; then echo '{"allowed": false, "disposition": "report_only", "reasons": ["excluded"]}'; exit 1; fi
echo '{"allowed": true, "disposition": "allowed_scope_only", "reasons": []}'`)
	f.gh = script(t, dir, "gh", `echo "https://github.com/agent-sh/fake/pull/7"`)
	return f
}

// shipFor is a ship tool over the fixture: live, its gate allowing agent-sh, its sandbox a bubblewrap one with a
// Go build cache of the test's own.
func shipFor(t *testing.T, st *store.Store, f repoFixture) *Ship {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bubblewrap here")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go here")
	}
	cache := filepath.Join(f.dir, "gocache")
	os.MkdirAll(cache, 0o700)
	home, _ := os.UserHomeDir()
	return &Ship{Store: st, Dir: filepath.Join(f.dir, "loop"), Live: true, Actor: "loop:test", Identity: [2]string{"Owner Name", "owner@example.com"},
		GH:      f.gh,
		Sandbox: Sandbox{Bwrap: bwrap, Writable: []string{cache}},
		Env: []string{"HOME=" + home, "PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "GOCACHE=" + cache, "GOTOOLCHAIN=local", "GOTELEMETRY=off",
			"GOFLAGS=-buildvcs=false"},
		Gate: Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{f.guard}, Home: home, Own: filepath.Join(f.dir, "loop"),
			Resolve: func(context.Context, string) (string, error) { return "agent-sh/fake", nil },
			Busy:    func(context.Context, string) (string, error) { return "", nil }}}
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

// A change goes in only through its package: prepared in a work tree of the loop's own, checked by a test that
// fails without it and passes with it, proposed with a measurement of the case's own problem and a rollback,
// committed by the user's own identity, reviewed clean, and landed live by a fast-forward of the default branch;
// its rollback reverts it and takes the case back to testing.
func TestAChangeShipsOnlyThroughItsPackage(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, c.ID, f.main, ""); !errors.Is(err, ErrRefused) {
		t.Fatalf("a second package: %v", err)
	}
	test := "go test ./..."
	// no test file yet
	write(t, filepath.Join(p.Work, "fake.go"), "package fake\n\n// Answer is the answer.\nfunc Answer() int { return 42 }\n\n// Note says zz-signal-text.\nfunc Note() string { return \"zz-signal-text\" }\n")
	if _, err := s.Check(ctx, c.ID, test, nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no test file") {
		t.Fatalf("a check without a test: %v", err)
	}
	// a test that passes without the change does not discriminate it
	write(t, filepath.Join(p.Work, "note_test.go"), "package fake\n\nimport \"testing\"\n\nfunc TestNote(t *testing.T) {\n\tif Note() == \"\" {\n\t\tt.Fatal(1)\n\t}\n}\n")
	if _, err := s.Check(ctx, c.ID, test, []string{"note_test.go"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "passes without the change") {
		t.Fatalf("a test that passes on the base: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "note_test.go"))
	write(t, filepath.Join(p.Work, "fake_test.go"), "package fake\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(Answer())\n\t}\n}\n")
	if _, err := s.Check(ctx, c.ID, "go test ./... | tee x", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("a shell in the test command: %v", err)
	}
	if p, err = s.Check(ctx, c.ID, test, nil); err != nil || p.State != Checked || p.Check.Before.Exit == 0 || p.Check.After.Exit != 0 {
		t.Fatalf("check: %+v %v", p.Check, err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Answer was off by one.", Rollback: "revert the commit", Lesson: "Answers are 42.", Signal: goodSignal,
		Window: 7 * 24 * time.Hour}
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
	write(t, filepath.Join(p.Work, "fake.go"), "package fake\n\n// Answer is the answer.\nfunc Answer() int { return 42 }\n\n// Note says nothing.\nfunc Note() string { return \"quiet\" }\n")
	if _, err := s.Check(ctx, c.ID, test, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, pr); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "edits") {
		t.Fatalf("a change that edits its signal's text: %v", err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), "package fake\n\n// Answer is the answer.\nfunc Answer() int { return 42 }\n\n// Note says zz-signal-text.\nfunc Note() string { return \"zz-signal-text\" }\n")
	if _, err := s.Check(ctx, c.ID, test, nil); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Propose(ctx, c.ID, pr); err != nil || p.State != Proposed || p.PR != "https://github.com/agent-sh/fake/pull/7" || p.Plan.Before != 4 {
		t.Fatalf("propose: %+v %v", p, err)
	}
	if who := run(t, p.Work, "git", "log", "-1", "--format=%an <%ae> / %cn <%ce>"); strings.TrimSpace(who) != "Owner Name <owner@example.com> / Owner Name <owner@example.com>" {
		t.Fatalf("committed as %q", who)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "review") {
		t.Fatalf("landed unreviewed: %v", err)
	}
	if _, err := s.Reviewed(ctx, c.ID, Review{High: 0, Medium: 1, Low: 2, Text: "M1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("landed on a medium: %v", err)
	}
	if _, err := s.Reviewed(ctx, c.ID, Review{Low: 1, Text: "fine"}); err != nil {
		t.Fatal(err)
	}
	s.Live = false
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not live") {
		t.Fatalf("landed while not live: %v", err)
	}
	s.Live = true
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed || len(p.Landed) != 1 {
		t.Fatalf("land: %+v %v", p, err)
	}
	if head := strings.TrimSpace(run(t, f.dir, "git", "--git-dir", f.origin, "rev-parse", "main")); head != p.Landed[0] {
		t.Fatalf("origin's main is %s, not the landed %s", head, p.Landed[0])
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
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case after the rollback: %s", got.Status)
	}
}

// The gate is the loop's authority: a repository of an owner it may write to, not excluded, that the machine's
// scope check allows and nobody works in; a configuration directory it was given and none its own judgment rests
// on; never the loop's own detectors, measurement, gates or skills.
func TestTheGateKeepsTheLoopsAuthority(t *testing.T) {
	ctx := context.Background()
	f := newRepo(t)
	home := t.TempDir()
	g := Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{f.guard}, Home: home,
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
	mods := filepath.Join(home, ".claude", "mods")
	os.MkdirAll(filepath.Join(mods, "guard"), 0o700)
	os.MkdirAll(filepath.Join(mods, "mistakes"), 0o700)
	os.MkdirAll(filepath.Join(home, ".config", "momentum"), 0o700)
	g.ConfigAllow = []string{mods, filepath.Join(home, ".config")}
	if _, err := g.Config(filepath.Join(mods, "guard")); err != nil {
		t.Errorf("an allowed directory: %v", err)
	}
	for _, d := range []string{filepath.Join(mods, "mistakes"), filepath.Join(home, ".config", "momentum"), filepath.Join(home, "elsewhere")} {
		if _, err := g.Config(d); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", d, err)
		}
	}
	for _, p := range []string{"internal/detect/failures.go", "internal/loop/gate.go", "internal/store/cases.go", "cmd/momentum/ship.go"} {
		if err := CheckPaths(Target{Repo: SelfRepo}, []string{"README.md", p}); !errors.Is(err, ErrRefused) {
			t.Errorf("momentum's own %s: %v", p, err)
		}
	}
	if err := CheckPaths(Target{Repo: SelfRepo}, []string{"internal/supervise/rules.go"}); err != nil {
		t.Errorf("supervision's rules are no evaluator: %v", err)
	}
	if err := CheckPaths(Target{Repo: "agent-sh/fake"}, []string{"internal/loop/x.go"}); err != nil {
		t.Errorf("another repository's paths: %v", err)
	}
}

// A test command is one program and its words, run without a shell.
func TestATestCommandRunsWithoutAShell(t *testing.T) {
	if w, err := Words(`go test ./internal/x -run 'TestA|TestB' -count=1`); err != nil || strings.Join(w, "|") != "go|test|./internal/x|-run|TestA|TestB|-count=1" {
		t.Fatalf("%q %v", w, err)
	}
	for _, bad := range []string{"go test ./... | tee x", "go test && rm -rf /", "go test $(echo x)", "rm -rf x", "go test 'unclosed", "", "go test > out"} {
		if _, err := Words(bad); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
