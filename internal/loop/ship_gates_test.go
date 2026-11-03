package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestABaseTestLinkIsRefused(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(fmt.Sprint(parent), func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			f := newRepo(t, nil)
			outside := filepath.Join(f.dir, "outside")
			if err := os.MkdirAll(outside, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(outside, "probe_test.go")
			write(t, target, "KEEP")
			link, name := filepath.Join(f.main, "probe_test.go"), "probe_test.go"
			if parent {
				link, name = filepath.Join(f.main, "nested"), "nested/probe_test.go"
			}
			dest := target
			if parent {
				dest = outside
			}
			if err := os.Symlink(dest, link); err != nil {
				t.Fatal(err)
			}
			run(t, f.main, "git", "add", ".")
			run(t, f.main, "git", "commit", "-qm", "base link")
			run(t, f.main, "git", "push", "-q", "origin", "main")
			s := recoveryShip(t, st, f)
			s.Sandbox.Gate = func(context.Context) error {
				t.Fatal("a base link reached a test sandbox")
				return nil
			}
			c := testingCase(t, st)
			p, err := s.Prepare(ctx, c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			candidateLink := filepath.Join(p.Work, "probe_test.go")
			if parent {
				candidateLink = filepath.Join(p.Work, "nested")
			}
			if err := os.Remove(candidateLink); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(p.Work, name), answerTest)
			write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
			_, err = checkPatch(t, s, ctx, c.ID, "go test ./...", []string{name})
			// The cumulative patch deletes the base link. Its mode is refused before a test export is made.
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "refused mode 120000") {
				t.Errorf("base link accepted: %v", err)
			}
			if b, _ := os.ReadFile(target); string(b) != "KEEP" {
				t.Fatal("base test copy overwrote outside bytes")
			}
		})
	}
}

func TestGitFiltersNeverRunOnTheHost(t *testing.T) {
	for _, driver := range []string{"clean", "smudge", "process", "merge"} {
		t.Run(driver, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			f := newRepo(t, map[string]string{".gitattributes": "fake.go filter=fixture\n", "filter.sh": "cat\n"})
			s := recoveryShip(t, st, f)
			c := testingCase(t, st)
			p, err := s.Prepare(ctx, c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(f.dir, "outside-marker")
			// Host configuration is independent of the source and run clones. Pin the repository Git will read.
			write(t, filepath.Join(p.Host, "filter.sh"), "echo ran > '"+marker+"'\ncat\n")
			write(t, filepath.Join(p.Host, "fake.go"), fixed("42"))
			key := "filter.fixture." + driver
			if driver == "merge" {
				key = "merge.fixture.driver"
			}
			run(t, p.Host, "git", "config", key, "sh filter.sh")
			for _, args := range [][]string{{"status", "--porcelain"}, {"add", "-A"}, {"read-tree", "-u", "--reset", "HEAD"}} {
				if _, err := s.git(ctx, p.Host, args...); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), key) {
					t.Errorf("external %s not refused before %v: %v", driver, args, err)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("filter ran before sandbox")
			}
		})
	}
}

func TestABinaryDiffHidesNoSignalEdit(t *testing.T) {
	for _, attribute := range []bool{false, true} {
		t.Run(fmt.Sprint(attribute), func(t *testing.T) {
			files := map[string]string{"message.txt": "zz-signal-text\n"}
			if attribute {
				files[".gitattributes"] = "message.txt -diff\n"
			}
			f := newRepo(t, files)
			st := testStore(t)
			s := shipFor(t, st, f)
			c := testingCase(t, st)
			p, err := s.Prepare(context.Background(), c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			text := "other-message\n"
			if !attribute {
				text += "\x00\n"
			}
			write(t, filepath.Join(p.Work, "message.txt"), text)
			tree, _, err := s.applyPatch(context.Background(), p, []Patch{fixturePatch(t, s, p.Case)})
			if err != nil {
				t.Fatal(err)
			}
			p.Check = &Check{Tree: tree}
			if err := s.editsSignal(context.Background(), p, goodSignal); err == nil {
				t.Fatal("binary diff hid a removed signal")
			}
		})
	}
}

func reviewFixture(t *testing.T) (*Runner, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"summary":"clean","findings":[]}}`))
	c := testingCase(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	// a review outside Once reads the inputs of a pass of its own
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	p := reviewable(t, r.Ship, c.ID)
	r.Ship.Public = func(_ context.Context, s string) (string, error) { return Neutral(s), nil }
	r.Ship.Gate = reviewGate(t)
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	return r, c, p
}

func TestAReviewWhosePostFailedIsPostedAgain(t *testing.T) {
	ctx := context.Background()
	r, c, _ := reviewFixture(t)
	calls := 0
	r.Post = func(context.Context, string, string, string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("temporary failure")
		}
		return "https://example.invalid/comment/1", nil
	}
	r.reviews(ctx, "")
	p, _, _ := r.Ship.Load(c.ID)
	if p.Clean() {
		t.Fatal("unposted review was clean")
	}
	r.reviews(ctx, "")
	p, _, _ = r.Ship.Load(c.ID)
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if err != nil || calls != 2 || len(p.Reviews) != 1 || len(runs) != 1 || !p.Clean() {
		t.Fatalf("post stranded or spent another round: calls=%d reviews=%d runs=%d clean=%v %v", calls, len(p.Reviews), len(runs), p.Clean(), err)
	}
}

func TestALandingWhoseRebasedTestFailsCanResume(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"boom_test.go": "package fake\nimport \"testing\"\nfunc TestBoom(t *testing.T){t.Fatal(\"boom\")}\n"})
	for i := 0; i < 2; i++ {
		if _, err := s.Land(ctx, c.ID); err == nil || !strings.Contains(err.Error(), "after the rebase the test fails") {
			t.Fatalf("attempt %d cannot resume: %v", i, err)
		}
		saved, _, _ := s.Load(c.ID)
		head := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", "HEAD"))
		if saved.State != Proposed || saved.Head != head || head != p.Head {
			t.Fatalf("stranded state %+v local=%s", saved, head)
		}
	}
}

func TestTheFamiliesNarrowReviewsAndLandings(t *testing.T) {
	for _, clean := range []bool{false, true} {
		t.Run(fmt.Sprint(clean), func(t *testing.T) {
			ctx := context.Background()
			r, _, p := reviewFixture(t)
			if clean {
				p.Reviews = []Review{{Head: p.Head, Comment: "posted"}}
				if err := r.Ship.save(&p); err != nil {
					t.Fatal(err)
				}
			}
			done, err := r.Once(ctx, Options{Families: []string{"sessions"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range done {
				if d.Family != "sessions" {
					t.Errorf("excluded family acted: %+v", d)
				}
			}
			runs, err := r.Store.LoopRuns(ctx, store.RunFilter{})
			if err != nil || len(runs) != 0 {
				t.Fatalf("excluded review spent: %+v %v", runs, err)
			}
		})
	}
}

func TestALandingWhosePushFailedCanResume(t *testing.T) {
	for _, ref := range []string{"branch", "base"} {
		t.Run(ref, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			f := newRepo(t, nil)
			s := shipFor(t, st, f)
			c, _ := reviewedChange(t, s, st, f)
			pushOther(t, f, "main", map[string]string{"README": "base advanced\n"})
			marker := filepath.Join(f.dir, "refuse-once")
			write(t, marker, "")
			pattern := "refs/heads/momentum/*"
			if ref == "base" {
				pattern = "refs/heads/main"
			}
			script(t, filepath.Join(f.origin, "hooks"), "pre-receive", `while read old new ref; do
 case "$ref" in `+pattern+`)
  if [ -e '`+marker+`' ]; then rm '`+marker+`'; exit 1; fi ;;
 esac
done`)
			if _, err := s.Land(ctx, c.ID); err == nil {
				t.Fatal("fixture did not refuse the push")
			}
			p, err := s.Land(ctx, c.ID)
			if err != nil || p.State != Landed {
				t.Fatalf("push failure stranded landing: %s %v", p.State, err)
			}
		})
	}
}

func TestRepositoryProgramSettingsAreRefused(t *testing.T) {
	keys := []string{"core.fsMonitor", "core.sshCommand", "core.askPass", "core.gitProxy", "core.alternateRefsCommand", "core.pager", "core.editor", "sequence.editor", "interactive.diffFilter", "credential.helper", "credential.https://github.com.helper", "url.fixture.insteadOf", "include.path", "includeIf.onbranch:never.path", "filter.fixture.required", "merge.fixture.driver", "diff.fixture.textconv", "diff.fixture.command", "gpg.program", "remote.origin.receivePack", "remote.origin.uploadPack", "remote.origin.vcs", "uploadpack.packObjectsHook", "pager.log", "gc.recentObjectsHook", "hook.fixture.command", "trailer.fixture.command", "submodule.child.update"}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			run(t, dir, "git", "init", "-q")
			marker := filepath.Join(t.TempDir(), "ran")
			program := script(t, t.TempDir(), "program", "echo ran > '"+marker+"'")
			value := program
			if strings.HasPrefix(strings.ToLower(key), "include") {
				value = filepath.Join(t.TempDir(), "included-config")
				write(t, value, "[core]\n sshCommand = "+program+"\n")
			}
			run(t, dir, "git", "config", key, value)
			if err := CheckGitConfig(ctx, dir); !errors.Is(err, ErrRefused) {
				t.Fatalf("accepted %s: %v", key, err)
			}
			if _, err := gitOutput(ctx, dir, "status"); !errors.Is(err, ErrRefused) {
				t.Fatalf("git accepted %s: %v", key, err)
			}
			gate := Gate{RepoGuard: []string{program}}
			if err := gate.scope(ctx, "agent-sh/fixture", dir); !errors.Is(err, ErrRefused) {
				t.Fatalf("scope accepted %s: %v", key, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("refused repository ran a program: %v", err)
			}
		})
	}
	for _, value := range []string{"false", "FALSE", "0", "off"} {
		dir := t.TempDir()
		run(t, dir, "git", "init", "-q")
		run(t, dir, "git", "config", "core.fsmonitor", value)
		if err := CheckGitConfig(context.Background(), dir); err != nil {
			t.Fatalf("disabled monitor refused: %v", err)
		}
	}
}

func TestScopeAndForgeGitKeepTheirPins(t *testing.T) {
	ctx := context.Background()
	f := newRepo(t, nil)
	marker := filepath.Join(f.dir, "hook-ran")
	hooks := filepath.Join(f.dir, "hooks")
	script(t, hooks, "post-index-change", "echo ran > '"+marker+"'")
	run(t, f.main, "git", "config", "core.hooksPath", hooks)
	if err := CheckGitConfig(ctx, f.main); err != nil {
		t.Fatal(err)
	}
	// A content-preserving edit makes status refresh the index. Prove the hook would run without the pins.
	touch := func() {
		t.Helper()
		path := filepath.Join(f.main, "fake.go")
		at := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	touch()
	run(t, f.main, "git", "status", "--porcelain")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unprotected status did not exercise hook: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	touch()
	checker := script(t, f.dir, "checker", `unset GIT_CONFIG_COUNT GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM
exec '`+f.guard+`' "$@"`)
	gate := Gate{RepoGuard: []string{checker}}
	if err := gate.scope(ctx, "agent-sh/fixture", f.main); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("scope wrote the index: %v", err)
	}
	forge := script(t, f.dir, "forge", `set -eu
python3 -c 'import os; names=set(os.environ.keys()); assert "GIT_OPTIONAL_LOCKS" in names; assert "GIT_ATTR_SOURCE" in names'
[ "$(git config --get core.hooksPath)" = /dev/null ]
[ "$(git config --get core.fsmonitor)" = false ]
if git config --get fixture.global > /dev/null; then exit 9; fi
git status --porcelain`)
	home := t.TempDir()
	write(t, filepath.Join(home, ".gitconfig"), "[fixture]\n global = trusted\n")
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	t.Setenv("GIT_ATTR_SOURCE", "HEAD")
	s := &Ship{GH: forge}
	if _, err := s.gh(ctx, f.main, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("forge wrote the index: %v", err)
	}
}

func TestNoGitCallRecursesOrRunsMaintenance(t *testing.T) {
	f := newRepo(t, nil)
	st := testStore(t)
	s := shipFor(t, st, f)
	run(t, f.main, "git", "config", "submodule.recurse", "true")
	run(t, f.main, "git", "config", "fetch.recurseSubmodules", "on-demand")
	run(t, f.main, "git", "config", "push.recurseSubmodules", "on-demand")
	run(t, f.main, "git", "config", "gc.auto", "1")
	run(t, f.main, "git", "config", "maintenance.auto", "true")
	for key, want := range map[string]string{"submodule.recurse": "false", "fetch.recurseSubmodules": "false", "push.recurseSubmodules": "no",
		"gc.auto": "0", "maintenance.auto": "false"} {
		got, err := s.git(context.Background(), f.main, "config", "--get", key)
		if err != nil || strings.TrimSpace(got) != want {
			t.Errorf("%s in a ship git call: %q %v, want %s", key, got, err, want)
		}
		// a gate's own config read is raw (its checker needs the repository's settings as they are); any other call
		// carries the pins, which a -c in front of the subcommand lets the config read show
		got, err = gitOutput(context.Background(), f.main, "-c", "zz.fixture=1", "config", "--get", key)
		if err != nil || strings.TrimSpace(got) != want {
			t.Errorf("%s in a gate git call: %q %v, want %s", key, got, err, want)
		}
	}
}

func TestAReviewWithoutItsFindingsIsNoReview(t *testing.T) {
	for _, out := range []string{`null`, `{"summary":"fine"}`, `{"findings":[]}`} {
		ctx := context.Background()
		r, c, _ := reviewFixture(t)
		r.Agent = testSandboxAgent(t, Agent{Runner: fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":`+out+`}`), Model: r.Agent.Model})
		r.Post = func(context.Context, string, string, string) (string, error) {
			return "https://example.invalid/c/1", nil
		}
		r.Ship.Gate = reviewGate(t)
		done := r.reviews(ctx, "")
		p, _, _ := r.Ship.Load(c.ID)
		failure := "an unreadable review"
		if out == "null" {
			failure = "report"
		}
		if len(done) != 1 || done[0].Run.Status != store.RunFailed || done[0].Run.Failure != failure || len(p.Reviews) != 0 {
			t.Fatalf("%s: %+v %+v", out, done, p.Reviews)
		}
	}
}
