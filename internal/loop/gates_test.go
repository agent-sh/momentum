package loop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The sandbox's isolation test runs whole under a scratch path too long for a socket's address.
func TestTheSandboxHoldsUnderALongScratchPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("long", 30))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	TestTheSandboxKeepsATestInItsTree(t)
}

// A check copies the change's tests into the base's export only through no link: a link of the base's, at the
// test's own path or at a directory above it, is refused, and nothing outside the export is written.
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
			s := shipFor(t, st, f)
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
			_, err = s.Check(ctx, c.ID, "go test ./...", []string{name})
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Errorf("base link accepted: %v", err)
			}
			if b, _ := os.ReadFile(target); string(b) != "KEEP" {
				t.Fatal("base test copy overwrote outside bytes")
			}
		})
	}
}

// A repository that configures a filter or a merge driver of its own is refused before any git call of the ship
// tool's: the change's own script never runs outside the sandbox.
func TestGitFiltersNeverRunOnTheHost(t *testing.T) {
	for _, driver := range []string{"clean", "smudge", "process", "merge"} {
		t.Run(driver, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			f := newRepo(t, map[string]string{".gitattributes": "fake.go filter=fixture\n", "filter.sh": "cat\n"})
			s := shipFor(t, st, f)
			c := testingCase(t, st)
			p, err := s.Prepare(ctx, c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(f.dir, "outside-marker")
			write(t, filepath.Join(p.Work, "filter.sh"), "echo ran > '"+marker+"'\ncat\n")
			write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
			key := "filter.fixture." + driver
			if driver == "merge" {
				key = "merge.fixture.driver"
			}
			run(t, f.main, "git", "config", key, "sh filter.sh")
			for _, args := range [][]string{{"status", "--porcelain"}, {"add", "-A"}, {"read-tree", "-u", "--reset", "HEAD"}} {
				if _, err := s.git(ctx, p.Work, args...); err == nil {
					t.Errorf("external %s allowed for %v", driver, args)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("filter ran before sandbox")
			}
		})
	}
}

// A run the model's safeguards refused runs once more on the fallback model within what is left of its run's
// budget, turns and time, and not at all when what the first attempt spent is unknown.
func TestAFallbackSharesItsRunsAllowance(t *testing.T) {
	for _, cost := range []string{"3", "4", "unknown"} {
		t.Run(cost, func(t *testing.T) {
			st := testStore(t)
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "fallback-args")
			primary := `{"type":"result","subtype":"success","is_error":true,"num_turns":3,"result":"safeguards"`
			if cost != "unknown" {
				primary += `,"total_cost_usd":` + cost
			}
			primary += `}`
			fake := script(t, dir, "fixture", `model=""; budget=""; turns=""
while [ "$#" -gt 0 ]; do
 case "$1" in --model) model="$2"; shift;; --max-budget-usd) budget="$2"; shift;; --max-turns) turns="$2"; shift;; esac
 shift
done
if [ "$model" = "primary" ]; then echo '`+primary+`'; else
 printf '%s %s\n' "$budget" "$turns" > '`+argsFile+`'
 printf '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":%s,"structured_output":{"summary":"ok"}}\n' "$budget"
fi`)
			r := runnerFor(t, st, []string{fake})
			r.Agent.Model, r.Agent.Fallback = "primary", "fallback"
			r.Budgets.Run, r.Budgets.Case, r.Budgets.Day = 4, 4, 4
			r.Budgets.MaxTurns = 10
			c := openCase(t, st, "failures", "budget", 1)
			done, err := r.Once(context.Background(), Options{Families: []string{"failures"}})
			if err != nil || len(done) != 1 {
				t.Fatalf("%+v %v", done, err)
			}
			got, _ := st.CaseByID(context.Background(), c.ID)
			if got.Cost.USD > 4 {
				t.Errorf("reservation exceeded: %.2f", got.Cost.USD)
			}
			b, err := os.ReadFile(argsFile)
			if cost == "3" {
				if err != nil || string(b) != "1 7\n" {
					t.Errorf("fallback got fresh allowances: %q %v", b, err)
				}
			} else if !os.IsNotExist(err) {
				t.Error("fallback ran with no known remaining allowance")
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		dir := t.TempDir()
		fake := script(t, dir, "fixture", `case "$*" in *"--model primary"*) sleep 0.15; echo '{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":1,"result":"safeguards"}' ;; *) sleep 0.3; echo '`+okResult+`' ;; esac`)
		res, err := (Agent{Runner: []string{fake}, Model: "primary", Fallback: "fallback"}).RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), BudgetUSD: 4, MaxTurns: 10, Timeout: 350 * time.Millisecond})
		if err != nil || res.Status != store.RunTimeout || res.USD != 4 {
			t.Fatalf("fallback restarted time or lost unknown cost: %+v %v", res, err)
		}
	})
}

// A run killed midway is charged its reservation once, on the run and on its case in one transaction, whatever
// the budget says now; a recovery that fails charges nothing, and the case's budget then holds the next pick.
func TestARecoveredRunChargesItsCaseOnce(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "stale", 1)
	if _, err := st.AddCaseCost(ctx, c.ID, store.Cost{USD: 16}, "fixture"); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return time.Now().Add(-3 * time.Hour) }
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	r.Now = nil
	r.Budgets.Run = 1 // Recovery must use the original reservation.
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER stop_finish BEFORE UPDATE ON loop_runs BEGIN SELECT RAISE(ABORT, 'synthetic interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.endStale(ctx); err == nil {
		t.Error("failed finalization was ignored")
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Cost.USD != 16 {
		t.Fatal("partial charge survived rollback")
	}
	if _, err := db.Exec(`DROP TRIGGER stop_finish`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.endStale(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = st.CaseByID(ctx, c.ID)
	ended, _ := st.LoopRunByID(ctx, run.ID)
	if got.Cost.USD != 20 || got.Cost.Runs != 1 || ended.USD != 4 || ended.Status != store.RunFailed {
		t.Fatalf("case/run accounting: %+v %+v", got.Cost, ended)
	}
	picked, _, _, err := r.pick(ctx, c.Family, "", false)
	if err != nil || picked.ID != "" {
		t.Fatalf("exhausted case selected: %s %v", picked.ID, err)
	}
}

// A change that removes its signal's text is refused when git would show the file as binary too.
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
			if _, _, err := s.staged(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if err := s.editsSignal(context.Background(), p, goodSignal); err == nil {
				t.Fatal("binary diff hid a removed signal")
			}
		})
	}
}

// A GitHub address the loop posts writes into no other thread: its host is made inert, in a link's target too.
func TestAPostedURLReferencesNothing(t *testing.T) {
	for _, url := range []string{"https://github.com/example-org/example-repo/issues/123", "https://github.com/example-org/example-repo/pull/4", "HTTPS://GITHUB.COM/example-org/example-repo/issues/123", "https://github.com/example-org/example-repo/%69ssues/123"} {
		for _, text := range []string{url, "[see](" + url + ")", "<" + url + ">", "[ref]: " + url} {
			got := Neutral(text)
			if got == text || strings.Contains(strings.ToLower(got), "github.com/") || !strings.Contains(got, "github[.]com/") {
				t.Errorf("active reference %q", got)
			}
		}
	}
}

func reviewFixture(t *testing.T) (*Runner, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"summary":"clean","findings":[]}}`))
	c := testingCase(t, st)
	work, head := reviewable(t)
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", Dir: work, Work: work, State: Proposed, Head: head, PR: "https://example.invalid/pull/1"}
	r.Ship.Public = func(_ context.Context, s string) (string, error) { return Neutral(s), nil }
	r.Ship.Gate = reviewGate(t)
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	return r, c, p
}

// A review whose post failed is posted again on the next run, without another review run or round, and its change
// lands only once it is posted.
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

// A landing whose test fails after the rebase leaves its package as it was reviewed, so the next landing tries
// again.
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
		head := strings.TrimSpace(run(t, p.Work, "git", "rev-parse", "HEAD"))
		if saved.State != Proposed || saved.Head != head || head != p.Head {
			t.Fatalf("stranded state %+v local=%s", saved, head)
		}
	}
}

// A run narrowed to some families reviews and lands only their changes.
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

// A publish is done only when the lesson command left its receipt, never because its run ended well; a lesson
// reserved whose write left none is tried again a day later.
func TestAPublishIsDoneOnlyByItsReceipt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"outcome":"blocked","summary":"no lesson"}}`))
	c := openCase(t, st, "failures", "publish", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepPublish)
	if why != "" {
		t.Fatal(why)
	}
	r.step(ctx, c.Family, c, StepPublish, run)
	if r.published(ctx, c.ID) {
		t.Fatal("successful harness exit counted as a lesson")
	}
	// a lesson reserved whose write left no receipt waits a day before a publish run tries it again
	if _, _, err := st.BeginLesson(ctx, c.ID, "a lesson"); err != nil {
		t.Fatal(err)
	}
	if !r.published(ctx, c.ID) {
		t.Fatal("a lesson reserved just now is tried again at once")
	}
	r.Now = func() time.Time { return time.Now().Add(LessonRetry + time.Minute) }
	if r.published(ctx, c.ID) {
		t.Fatal("a lesson reserved a day ago is never tried again")
	}
	if err := st.CaseNote(ctx, c.ID, store.LessonPublished, "synthetic durable receipt", "fixture"); err != nil {
		t.Fatal(err)
	}
	if !r.published(ctx, c.ID) {
		t.Fatal("durable lesson receipt was ignored")
	}
}

// A landing whose push to its branch or to the default branch failed is taken up again where it stopped.
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

// A run's end is recorded once: finishing it again charges its case nothing more.
func TestARunChargesItsCaseOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "finish", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	res, err := r.invoke(ctx, c.Family, c, StepInvestigate, run)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r.finish(ctx, c, &run, res, nil)
	}
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.USD != 0.75 || got.Cost.Runs != 1 {
		t.Fatalf("duplicate completion charge: %+v %v", got.Cost, err)
	}
}

// Repository configuration names no program, including mixed-case keys and inactive includes. A refusal
// precedes the scope checker as well as direct Git; neither gets to execute the configured program.
func TestRepositoryProgramSettingsAreRefused(t *testing.T) {
	keys := []string{"core.fsMonitor", "core.sshCommand", "core.askPass", "core.gitProxy", "core.alternateRefsCommand", "core.pager", "core.editor", "sequence.editor", "interactive.diffFilter", "credential.helper", "credential.https://github.com.helper", "url.fixture.insteadOf", "include.path", "includeIf.onbranch:never.path", "filter.fixture.required", "merge.fixture.driver", "diff.fixture.textconv", "diff.fixture.command", "gpg.program", "remote.origin.receivePack", "remote.origin.uploadPack", "remote.origin.vcs", "uploadpack.packObjectsHook", "pager.log"}
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

// A repository's hook path is allowed. The fixture scope checker runs status without refreshing the index,
// even when a checker strips configuration pins; the forge's Git also has hooks and the monitor disabled.
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
[ "$GIT_OPTIONAL_LOCKS" = 0 ]
[ "$GIT_ATTR_SOURCE" = 4b825dc642cb6eb9a060e54bf8d69288fbee4904 ]
[ "$(git config --get core.hooksPath)" = /dev/null ]
[ "$(git config --get core.fsmonitor)" = false ]
[ "$(git config --get fixture.global)" = trusted ]
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

// No git call of momentum's goes into a submodule, another repository with settings of its own, whatever the
// repository's settings ask.
func TestNoGitCallGoesIntoASubmodule(t *testing.T) {
	f := newRepo(t, nil)
	st := testStore(t)
	s := shipFor(t, st, f)
	run(t, f.main, "git", "config", "submodule.recurse", "true")
	run(t, f.main, "git", "config", "fetch.recurseSubmodules", "on-demand")
	run(t, f.main, "git", "config", "push.recurseSubmodules", "on-demand")
	for key, want := range map[string]string{"submodule.recurse": "false", "fetch.recurseSubmodules": "false", "push.recurseSubmodules": "no"} {
		got, err := s.git(context.Background(), f.main, "config", "--get", key)
		if err != nil || strings.TrimSpace(got) != want {
			t.Errorf("%s in a ship git call: %q %v, want %s", key, got, err, want)
		}
		got, err = gitOutput(context.Background(), f.main, "config", "--get", key)
		if err != nil || strings.TrimSpace(got) != want {
			t.Errorf("%s in a gate git call: %q %v, want %s", key, got, err, want)
		}
	}
}
