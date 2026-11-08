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

// outOfReachFix is the change the fixture's builds say a person makes.
const outOfReachFix = "Run go test ./... in the workflow's test step."

// outOfReachRunner is a runner whose builds answer that the fix needs where, which no change of theirs may touch, and
// change nothing, over a case in testing (testingCase). It returns the file that counts the builds.
func outOfReachRunner(t *testing.T, where string) (*Runner, store.Case, string) {
	t.Helper()
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	st := testStore(t)
	f := newRepo(t, nil)
	c := testingCase(t, st)
	raw, err := json.Marshal(map[string]any{"summary": "The fix is the workflow's: its test step runs no test.", "test_command": []string{},
		"title": "", "body": "", "rollback": "", "window": "", "lesson": "", "signal": map[string]any{"source": "", "kinds": []string{}, "match": []string{}},
		"out_of_reach": map[string]string{"path": where, "fix": outOfReachFix}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	harness := script(t, dir, "harness", `n=$(cat /opt/fixture/builds 2>/dev/null || echo 0)
echo "$((n+1))" > /opt/fixture/builds
`+fixtureResult(`{"type":"result","subtype":"success","structured_output":`+string(raw)+`}`))
	r := runnerFor(t, st, []string{harness})
	r.Ship = shipFor(t, st, f)
	r.Ship.Dir = r.Dir
	r.Ship.Live = false
	r.Toolchains = Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path}
	r.Inputs.ReadDirs = []string{f.main}
	return r, c, filepath.Join(dir, "builds")
}

// onePass runs a pass over the case's family and returns what it did.
func onePass(t *testing.T, r *Runner, c store.Case) Done {
	t.Helper()
	done, err := r.Once(context.Background(), Options{Families: []string{c.Family}})
	if err != nil || len(done) != 1 {
		t.Fatalf("the pass: %+v %v", done, err)
	}
	return done[0]
}

// builds is how many builds the fixture's harness ran.
func builds(t *testing.T, counter string) string {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return "0"
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// heldFor checks that a pass's build answered and held its case for a person: the run ended as answered, with no
// failure, the case is still in testing with no stop, a standing raised note names the path and the fix, and no
// component-health case is filed.
func heldFor(t *testing.T, r *Runner, c store.Case, d Done, where string) {
	t.Helper()
	ctx := context.Background()
	if d.Skipped != "" || d.Run.ID == "" || d.Run.Step != StepBuild {
		t.Fatalf("the build did not run: %+v", d)
	}
	run, err := r.Store.LoopRunByID(ctx, d.Run.ID)
	if err != nil || run.Status != store.RunOK || run.Failure != "" || run.Cause != "" {
		t.Fatalf("an out-of-reach answer failed its build: %+v %v", run, err)
	}
	n, ok, err := r.Store.Note(ctx, "loop-build:"+store.IDPart(c.ID)+":person")
	if err != nil || !ok || !n.Done.IsZero() || n.Kind != store.NoteProblem || n.Importance != store.ImportanceNormal ||
		!strings.Contains(n.Title, where) || !strings.Contains(n.Detail, where) || !strings.Contains(n.Detail, outOfReachFix) {
		t.Fatalf("the hold's note: %+v %v %v", n, ok, err)
	}
	now, err := r.Store.CaseByID(ctx, c.ID)
	if err != nil || now.Status != store.CaseTesting {
		t.Fatalf("the held case: %+v %v", now, err)
	}
	if why, err := r.stopped(ctx, now, true); err != nil || why != "" {
		t.Fatalf("an out-of-reach answer counts for the stop rule: %q %v", why, err)
	}
	if cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: store.HealthFamily, All: true}); err != nil || len(cs) != 0 {
		t.Fatalf("an out-of-reach answer filed a component-health case: %+v %v", cs, err)
	}
}

// A build whose fix needs a path no change may touch answers so with the path and the change a person makes there, and
// changes nothing: the host checks the path, and the case holds for a person with a raised note and a line in its log
// that name them. The run is no failed build: the stop rule counts nothing, and no component-health case is filed.
func TestAnOutOfReachFixHoldsTheCaseForAPerson(t *testing.T) {
	where := ".github/workflows/ci.yml"
	r, c, _ := outOfReachRunner(t, where)
	heldFor(t, r, c, onePass(t, r, c), where)
	log, err := r.Store.CaseLog(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	held := false
	for _, e := range log {
		held = held || e.Action == "held for a person" && strings.Contains(e.Detail, where) && strings.Contains(e.Detail, outOfReachFix)
	}
	if !held {
		t.Fatalf("the case's log keeps no hold for a person: %+v", log)
	}
}

// A case held for a person is not built again, and takes no build slot: the family's next workable case builds instead.
func TestACaseHeldForAPersonIsNotBuiltAgain(t *testing.T) {
	ctx := context.Background()
	where := ".github/workflows/ci.yml"
	r, c, counter := outOfReachRunner(t, where)
	other := openCase(t, r.Store, c.Family, "out-of-reach-other", 1)
	mech, next := "the workflow runs no test", "run the tests in the workflow"
	other, err := r.Store.UpdateCase(ctx, other.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, other, "main")
	if d := onePass(t, r, c); d.Case != c.ID || d.Run.ID == "" {
		t.Fatalf("the first pass did not build the case with the most evidence: %+v", d)
	}
	d := onePass(t, r, c)
	if d.Case != other.ID || d.Run.ID == "" || builds(t, counter) != "2" {
		t.Fatalf("the held case took the family's slot: %+v, %s builds", d, builds(t, counter))
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("the held case was built again: %+v %v", runs, err)
	}
	if d := onePass(t, r, c); d.Run.ID != "" || !strings.Contains(d.Skipped, c.ID+" held for a person: its fix needs "+where) || builds(t, counter) != "2" {
		t.Fatalf("a pass built a held case: %+v, %s builds", d, builds(t, counter))
	}
}

// A case held for a person builds again once it holds new evidence: the hold ends, and its note with it. A build that
// answers the same again holds it again, and two such answers stop nothing.
func TestAHeldCaseBuildsAgainOnNewEvidence(t *testing.T) {
	ctx := context.Background()
	where := ".github/workflows/ci.yml"
	r, c, counter := outOfReachRunner(t, where)
	heldFor(t, r, c, onePass(t, r, c), where)
	if d := onePass(t, r, c); d.Run.ID != "" || builds(t, counter) != "1" {
		t.Fatalf("a pass built a held case: %+v", d)
	}
	if _, _, err := r.Store.AddCaseEvidence(ctx, c.ID, []store.Evidence{{Ref: "file:/t/out-of-reach-again", At: time.Now()}}, "test"); err != nil {
		t.Fatal(err)
	}
	d := onePass(t, r, c)
	if builds(t, counter) != "2" {
		t.Fatalf("new evidence did not build the held case again: %+v", d)
	}
	heldFor(t, r, c, d, where)
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	for _, e := range log {
		ended = ended || e.Action == "person hold ended" && e.Detail == "new evidence came in"
	}
	if !ended {
		t.Fatalf("the case's log keeps no end of its hold: %+v", log)
	}
}

// An out-of-reach answer whose path a change may touch is the build's own refusal, as a build that changes nothing was
// before: no hold, and two for that cause stop the case.
func TestAnOutOfReachPathInReachIsTheBuildsRefusal(t *testing.T) {
	ctx := context.Background()
	r, c, counter := outOfReachRunner(t, "fake.go")
	for i := 1; i <= 2; i++ {
		d := onePass(t, r, c)
		run, err := r.Store.LoopRunByID(ctx, d.Run.ID)
		if err != nil || run.Status != store.RunFailed || run.Failure != "build report" || run.Cause == "" || run.Cause == hostCause ||
			!strings.Contains(run.Summary, "build report: refused: the out-of-reach path fake.go is one a change may touch") {
			t.Fatalf("build %d of an in-reach path was not refused for it: %+v %v", i, run, err)
		}
	}
	if _, ok, err := r.Store.Note(ctx, "loop-build:"+store.IDPart(c.ID)+":person"); err != nil || ok {
		t.Fatalf("an in-reach path held its case: %v %v", ok, err)
	}
	if d := onePass(t, r, c); d.Run.ID != "" || !strings.Contains(d.Skipped, "stopped: two of its build runs were refused for one cause") || builds(t, counter) != "2" {
		t.Fatalf("two refusals of an in-reach path did not stop the case: %+v", d)
	}
}

// A person ends a hold: closing the case resolves its note, and the case, reopened, builds again; a case sent back to
// its investigation and returned to testing in the same episode builds again too.
func TestAPersonEndsAHold(t *testing.T) {
	where := ".github/workflows/ci.yml"
	backToTesting := func(t *testing.T, r *Runner, c store.Case) store.Case {
		t.Helper()
		mech, next := "Answer is off by one", "assert Answer() == 42"
		c, err := r.Store.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("closed", func(t *testing.T) {
		ctx := context.Background()
		r, c, counter := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
		if _, err := r.Store.CloseCase(ctx, c.ID, store.Solved, "", "the workflow runs its tests now", "test"); err != nil {
			t.Fatal(err)
		}
		onePass(t, r, c)
		if n, ok, err := r.Store.Note(ctx, "loop-build:"+store.IDPart(c.ID)+":person"); err != nil || !ok || n.Done.IsZero() {
			t.Fatalf("the close did not resolve the hold's note: %+v %v %v", n, ok, err)
		}
		c, err := r.Store.ReopenCase(ctx, c.ID, []store.Evidence{{Ref: "file:/t/out-of-reach-back", At: time.Now()}}, "it came back", "test")
		if err != nil {
			t.Fatal(err)
		}
		c = backToTesting(t, r, c)
		setCaseRepo(t, r.Store, c, "main")
		heldFor(t, r, c, onePass(t, r, c), where)
		if builds(t, counter) != "2" {
			t.Fatalf("the reopened case did not build: %s builds", builds(t, counter))
		}
	})
	t.Run("investigating", func(t *testing.T) {
		ctx := context.Background()
		r, c, counter := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
		investigating := store.CaseInvestigating
		c, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: investigating}, "test")
		if err != nil {
			t.Fatal(err)
		}
		// the pass's start, before the case's investigation would run
		if err := r.releaseHolds(ctx, []store.Case{c}); err != nil {
			t.Fatal(err)
		}
		if n, ok, err := r.Store.Note(ctx, "loop-build:"+store.IDPart(c.ID)+":person"); err != nil || !ok || n.Done.IsZero() {
			t.Fatalf("leaving testing did not resolve the hold's note: %+v %v %v", n, ok, err)
		}
		c = backToTesting(t, r, c)
		heldFor(t, r, c, onePass(t, r, c), where)
		if builds(t, counter) != "2" {
			t.Fatalf("the case back in testing did not build: %s builds", builds(t, counter))
		}
	})
}
