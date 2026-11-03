package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

func setCaseRepo(t *testing.T, st *store.Store, c store.Case, name string) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Repo    string `json:"repo"`
		Episode int64  `json:"episode"`
	}{name, c.Episode})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CaseNote(context.Background(), c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestInvestigationRepositoryAndEpisode(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "repository", 1)
	run := store.LoopRun{Case: c.ID, CaseEpisode: c.Episode, ID: "fixture"}
	f := nativeFinding(Result{Report: json.RawMessage(`{"mechanism":"one cause","fix":"one fix","summary":"marker-7f3a","repo":"main"}`)})
	if err := r.keepFindings(ctx, run, f, true, ""); err != nil {
		t.Fatal(err)
	}
	c, _ = st.CaseByID(ctx, c.ID)
	if name, err := r.caseRepo(ctx, c); err != nil || name != "main" || c.Status != store.CaseTesting {
		t.Fatalf("repository was not recorded: %s %s %v", name, c.Status, err)
	}
	c.Episode++
	if name, err := r.caseRepo(ctx, c); err != nil || name != "" {
		t.Fatalf("old episode selected a repository: %s %v", name, err)
	}
}

func TestTestingWithoutRepositoryWaitsPassively(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	c := openCase(t, st, "failures", "no-repository", 1)
	mechanism, fix := "one cause", "one fix"
	c, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		picked, _, why, err := r.pick(ctx, c.Family, c.ID, false)
		if err != nil || picked.ID != "" || !strings.Contains(why, "waits for a repository") {
			t.Fatalf("selected a build without a repository: %s %s %v", picked.ID, why, err)
		}
	}
	notes, err := st.Notes(ctx)
	if err != nil || len(notes) != 1 || notes[0].Kind != store.NoteProblem {
		t.Fatalf("passive note was not stable: %d %v", len(notes), err)
	}
}

func TestPatchSpecHasNoRelayOrHarnessAndPinsHooks(t *testing.T) {
	run := store.LoopRun{ID: "fixture", Dir: t.TempDir()}
	p := Package{Work: filepath.Join(run.Dir, "work"), BaseCommit: strings.Repeat("a", 40)}
	spec, err := patchSpec(run, p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Relay != "" || !strings.HasPrefix(spec.Unit, "momentum-run-loop-patch-") || len(spec.Binds) != 2 {
		t.Fatal("patch job has a relay or extra input")
	}
	cmd := strings.Join(spec.Argv, " ")
	if strings.Contains(cmd, "claude") || strings.Count(cmd, "core.hooksPath=/dev/null") != 2 ||
		!strings.Contains(cmd, "--cached --binary --no-ext-diff --no-textconv") {
		t.Fatal("patch job does not pin its export")
	}
	// Names only. Values of the test's environment never enter diagnostics.
	for name := range spec.Env {
		switch name {
		case "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_ATTR_SOURCE", "GIT_ATTR_NOSYSTEM", "GIT_TERMINAL_PROMPT":
		default:
			t.Fatalf("unexpected environment name: %s", name)
		}
	}
}

func TestPatchOutputConfinesSymlinksAndSize(t *testing.T) {
	for _, kind := range []string{"file-link", "out-link", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			run, outside := t.TempDir(), t.TempDir()
			write(t, filepath.Join(outside, "change.patch"), "marker-7f3a")
			switch kind {
			case "out-link":
				if err := os.Symlink(outside, filepath.Join(run, "out")); err != nil {
					t.Fatal(err)
				}
			case "file-link":
				if err := os.Mkdir(filepath.Join(run, "out"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "change.patch"), filepath.Join(run, "out", "change.patch")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				write(t, filepath.Join(run, "out", "change.patch"), "")
				if err := os.Truncate(filepath.Join(run, "out", "change.patch"), PatchMax+1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readPatch(Patch{Dir: run, File: "out/change.patch"}); err == nil {
				t.Fatal("host accepted an unsafe patch output")
			}
		})
	}
}

func TestBuildReportRejectsMissingFields(t *testing.T) {
	valid := BuildReport{Summary: "marker-7f3a", Test: "go test ./...", Title: "Fix one cause", Body: "One change.",
		Rollback: "Revert it.", Window: "7d", Lesson: "Test the boundary.", Signal: goodSignal}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildReport(raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		delete(fields, name)
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildReport(raw); err == nil {
			t.Fatalf("accepted a report without %s", name)
		}
		fields[name] = value
	}
	for _, raw := range []string{`null`, `{"summary":"marker-7f3a"}`, `{"summary":"marker-7f3a","extra":"x"}`} {
		if _, err := buildReport([]byte(raw)); err == nil {
			t.Fatal("accepted an incomplete build report")
		}
	}
}

func TestMovedBaseKeepsPreviousPatchAndScrubbedReview(t *testing.T) {
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
	first, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	previous, err := readPatch(fixturePatch(t, r.Ship, c.ID))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Ship.pkgDir(c.ID), "change.patch"), string(previous))
	p.Head, p.State, p.RefreshBase = p.BaseCommit, Revising, true
	p.Reviews = []Review{{Round: 1, Medium: 1, Text: "Fix password=marker-7f3a in the revision.", Head: p.Head, Comment: "posted"}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	base := pushOther(t, f, "main", map[string]string{"notes.txt": "marker-7f3a\n"})
	second, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = r.Ship.Load(c.ID)
	if err != nil || p.BaseCommit != base || p.Work != filepath.Join(second.Dir, "work") || p.Head != "" {
		t.Fatalf("new build did not start at the refreshed base: %s %v", p.BaseCommit, err)
	}
	patch, err := readHostInput(HostDir(second.Dir), "input/previous.patch")
	if err != nil || string(patch) != string(previous) {
		t.Fatal("refresh lost the previous cumulative patch")
	}
	review, err := readHostInput(HostDir(second.Dir), "input/review.md")
	if err != nil || strings.Contains(string(review), "password=marker-7f3a") || !strings.Contains(string(review), "[masked]") {
		t.Fatal("revision did not receive the scrubbed review")
	}
	if first.Dir == second.Dir {
		t.Fatal("revision reused a writable run directory")
	}
}

func TestBuildPreparationFailuresUseStageKeyAndStopRule(t *testing.T) {
	fakeManager(t) // Host ordering only. No unit or model starts in this fixture.
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	// The gate allows the repository, but host clone storage is unusable.
	// Gate refusals have their own hold tests and are not agent failures.
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
		write(t, filepath.Join(r.Dir, "host"), "marker-7f3a")
		return "", nil
	}
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	var err error
	c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || done[0].Run.Status != store.RunFailed || done[0].Run.Failure != "build prepare" || done[0].Run.USD != 0 {
			t.Fatalf("preparation lost its zero-cost stage failure: steps=%d error=%v", len(done), err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
	if err != nil || len(done) != 1 || !strings.Contains(done[0].Skipped, "stopped") {
		t.Fatalf("two preparation failures did not stop the case: steps=%d error=%v", len(done), err)
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if err != nil || len(runs) != 2 {
		t.Fatalf("stopped preparation started another run: %d %v", len(runs), err)
	}
}

func TestBuildOnSandboxChecksAndProposes(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	st := testStore(t)
	f := newRepo(t, nil)
	c := testingCase(t, st)
	raw, err := json.Marshal(BuildReport{Summary: "The answer is corrected.", Test: "go test ./...", Title: "Correct the answer",
		Body:     "Fix one cause with a discriminating test. Prior art needs a network search, which this run cannot perform.",
		Rollback: "Revert the change.", Window: "7d", Lesson: "Test the answer at its boundary.", Signal: goodSignal})
	if err != nil {
		t.Fatal(err)
	}
	result := `{"type":"result","subtype":"success","structured_output":` + string(raw) + `}`
	harness := script(t, t.TempDir(), "harness", `schema=""
while [ "$#" -gt 0 ]; do
 if [ "$1" = "--json-schema" ]; then schema="$2"; shift; fi
 shift
done
case "$schema" in *findings*)
 grep -q 'return 42' /ro/change/fake.go || exit 7
 if printf x >> /ro/change/fake.go 2>/dev/null; then exit 8; fi
 `+fixtureResult(`{"type":"result","subtype":"success","structured_output":{"summary":"Checked fake.go and answer_test.go.","reviewed_files":["fake.go","answer_test.go"],"findings":[]}}`)+`
 ;;
*)
 sed -i 's/return 41/return 42/' /work/fake.go
 cat > /work/answer_test.go <<'TEST'
`+answerTest+`
TEST
 go test ./... || exit 9
 `+fixtureResult(result)+`
 ;;
esac`)
	r := runnerFor(t, st, []string{harness})
	r.Ship = shipFor(t, st, f)
	r.Ship.Dir = r.Dir
	r.Ship.Live = false
	r.Toolchains = Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path}
	r.Inputs.ReadDirs = []string{f.main}
	r.Post = func(context.Context, string, string, string) (string, error) {
		return "https://example.invalid/review/1", nil
	}
	done, err := r.Once(context.Background(), Options{Families: []string{c.Family}})
	if err != nil {
		t.Fatal(err)
	}
	p, ok, err := r.Ship.Load(c.ID)
	if err != nil || !ok || p.State != Proposed || p.Check == nil || p.Check.Before.Exit == 0 || p.Check.After.Exit != 0 || !p.Clean() {
		t.Fatalf("build did not check and propose: state=%s check=%v clean=%v steps=%d error=%v", p.State, p.Check != nil, p.Clean(), len(done), err)
	}
	if originHead(t, f, p.Branch) != p.Head {
		t.Fatal("proposal did not push the checked candidate")
	}
	patch, err := os.ReadFile(filepath.Join(r.Ship.pkgDir(c.ID), "change.patch"))
	if err != nil || !strings.Contains(string(patch), "answer_test.go") || !strings.Contains(string(patch), "+func Answer() int { return 42 }") {
		t.Fatal("patch unit did not export the cumulative change")
	}
}

func TestPatchUnitDoesNotRunPlantedHooks(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-hooks", true: "configured-hooks"}[custom], func(t *testing.T) {
			st := testStore(t)
			f := newRepo(t, nil)
			s := shipFor(t, st, f)
			p, err := s.Prepare(context.Background(), testingCase(t, st).ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			hooks := filepath.Join(p.Work, ".git", "hooks")
			if custom {
				hooks = filepath.Join(p.Work, ".hooks")
				run(t, p.Work, "git", "config", "core.hooksPath", ".hooks")
			}
			script(t, hooks, "post-index-change", "touch /work/hook-marker")
			write(t, filepath.Join(p.Work, "notes.txt"), "marker-7f3a\n")
			dir := filepath.Join(shortScratch(t), "patch-run")
			write(t, filepath.Join(dir, "out", "placeholder"), "marker-7f3a")
			r := &Runner{Budgets: Budgets{RunTime: time.Minute}}
			patch, err := r.patchUnit(context.Background(), store.LoopRun{ID: newRunID(time.Now()), Dir: dir}, p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(p.Work, "hook-marker")); !os.IsNotExist(err) {
				t.Fatal("the patch unit executed a planted hook")
			}
			b, err := readPatch(patch)
			if err != nil || !strings.Contains(string(b), "+marker-7f3a") {
				t.Fatal("patch unit did not export the new file")
			}
		})
	}
}
