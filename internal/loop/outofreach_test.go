package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// ciFiles are the fixture repository's CI workflows: paths no change may touch, which BASE holds.
var ciFiles = map[string]string{".github/workflows/ci.yml": "name: ci\n", ".github/workflows/release.yml": "name: release\n"}

// outOfReachAnswer is a build's step that changes nothing and answers its fix needs where.
func outOfReachAnswer(t *testing.T, where string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"summary": "The fix is the workflow's: its test step runs no test.", "test_command": []string{},
		"title": "", "body": "", "rollback": "", "window": "", "lesson": "", "signal": map[string]any{"source": "", "kinds": []string{}, "match": []string{}},
		"out_of_reach": map[string]string{"path": where, "fix": outOfReachFix}})
	if err != nil {
		t.Fatal(err)
	}
	return fixtureResult(`{"type":"result","subtype":"success","structured_output":` + string(raw) + `}`)
}

// refusedWindow is a build's step that makes the checked change and reports a window the host cannot read, which it
// refuses at build propose; it keeps the refusal.md the build was given, if any, as refusal-N.
func refusedWindow(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(BuildReport{Summary: "The answer is corrected.", Test: []string{"go", "test", "./..."}, Title: "Correct the answer",
		Body:     "Fix one cause with a discriminating test. Prior art needs a network search, which this run cannot perform.",
		Rollback: "Revert the change.", Window: "fortnight", Lesson: "Test the answer at its boundary.", Signal: goodSignal})
	if err != nil {
		t.Fatal(err)
	}
	return `if [ -e /run/input/refusal.md ]; then cp /run/input/refusal.md "/opt/fixture/refusal-$n"; fi
sed -i 's/return 41/return 42/' /work/fake.go
cat > /work/answer_test.go <<'TEST'
` + answerTest + `TEST
` + fixtureResult(`{"type":"result","subtype":"success","structured_output":`+string(raw)+`}`)
}

// stepsRunner is a runner over a case in testing (testingCase) of a repository with files, whose n-th build runs
// steps[n-1], and the last step after them. It returns the runner, the case and the directory where the builds count.
func stepsRunner(t *testing.T, files map[string]string, steps ...string) (*Runner, store.Case, string) {
	t.Helper()
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	st := testStore(t)
	f := newRepo(t, files)
	c := testingCase(t, st)
	body := "n=$(cat /opt/fixture/builds 2>/dev/null || echo 0)\nn=$((n+1))\necho \"$n\" > /opt/fixture/builds\ncase \"$n\" in\n"
	for i, step := range steps {
		label := "*"
		if i < len(steps)-1 {
			label = string(rune('1' + i))
		}
		body += label + ")\n" + step + "\n;;\n"
	}
	dir := t.TempDir()
	harness := script(t, dir, "harness", body+"esac")
	r := runnerFor(t, st, []string{harness})
	r.Ship = shipFor(t, st, f)
	r.Ship.Dir = r.Dir
	r.Ship.Live = false
	r.Toolchains = Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path}
	r.Inputs.ReadDirs = []string{f.main}
	return r, c, dir
}

// outOfReachRunner is stepsRunner whose every build answers its fix needs where, in a repository with ciFiles.
func outOfReachRunner(t *testing.T, where string) (*Runner, store.Case, string) {
	t.Helper()
	return stepsRunner(t, ciFiles, outOfReachAnswer(t, where))
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
func builds(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "builds"))
	if os.IsNotExist(err) {
		return "0"
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// personNote is the case's note of its hold for a person on where.
func personNote(t *testing.T, r *Runner, c store.Case, where string) (store.Note, bool) {
	t.Helper()
	notes, err := r.Store.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(c.ID)+":person") && strings.Contains(n.Title, where) {
			return n, true
		}
	}
	return store.Note{}, false
}

// heldFor checks that a pass's build answered and held its case for a person: the run kept its own outcome, with no
// failure, the case is still in testing with no stop, a standing raised note names the path and the fix, and no
// component-health case is filed.
func heldFor(t *testing.T, r *Runner, c store.Case, d Done, where string) {
	t.Helper()
	ctx := context.Background()
	if d.Skipped != "" || d.Run.ID == "" || d.Run.Step != StepBuild {
		t.Fatalf("the build did not run: %+v", d)
	}
	run, err := r.Store.LoopRunByID(ctx, d.Run.ID)
	if err != nil || run.Status != "out-of-reach" || run.Failure != "" || run.Cause != "" {
		t.Fatalf("an out-of-reach answer did not keep its own outcome: %+v %v", run, err)
	}
	n, ok := personNote(t, r, c, where)
	if !ok || !n.Done.IsZero() || n.Kind != store.NoteProblem || n.Importance != store.ImportanceNormal ||
		!strings.Contains(n.Detail, where) || !strings.Contains(n.Detail, outOfReachFix) {
		t.Fatalf("the hold's note: %+v %v", n, ok)
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

// stillHeld checks that a pass built nothing and named the case's hold for a person on where.
func stillHeld(t *testing.T, r *Runner, c store.Case, dir, count, where string) {
	t.Helper()
	if d := onePass(t, r, c); d.Run.ID != "" || !strings.Contains(d.Skipped, c.ID+" held for a person: its fix needs "+where) || builds(t, dir) != count {
		t.Fatalf("a pass built a held case: %+v, %s builds", d, builds(t, dir))
	}
	if n, ok := personNote(t, r, c, where); !ok || !n.Done.IsZero() {
		t.Fatalf("the held case's note does not stand: %+v %v", n, ok)
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
	r, c, dir := outOfReachRunner(t, where)
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
	if d.Case != other.ID || d.Run.ID == "" || builds(t, dir) != "2" {
		t.Fatalf("the held case took the family's slot: %+v, %s builds", d, builds(t, dir))
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("the held case was built again: %+v %v", runs, err)
	}
	if d := onePass(t, r, c); d.Run.ID != "" || !strings.Contains(d.Skipped, c.ID+" held for a person: its fix needs "+where) || builds(t, dir) != "2" {
		t.Fatalf("a pass built a held case: %+v, %s builds", d, builds(t, dir))
	}
}

// New evidence does not end a hold for a person: the problem recurring is no fix, and a case that built again on it
// would pay a build each time.
func TestNewEvidenceKeepsAHold(t *testing.T) {
	ctx := context.Background()
	where := ".github/workflows/ci.yml"
	r, c, dir := outOfReachRunner(t, where)
	heldFor(t, r, c, onePass(t, r, c), where)
	for i := 0; i < 2; i++ {
		if _, _, err := r.Store.AddCaseEvidence(ctx, c.ID, []store.Evidence{{Ref: "file:/t/out-of-reach-again-" + string(rune('a'+i)), At: time.Now()}}, "test"); err != nil {
			t.Fatal(err)
		}
		stillHeld(t, r, c, dir, "1", where)
	}
}

// A hold for a person between two refusals for one cause keeps their count: the stop rule reads past it, so the
// second refusal stops the case, and the build after the hold is given the refusal before it.
func TestAHoldBetweenTwoRefusalsKeepsTheirCount(t *testing.T) {
	ctx := context.Background()
	where := ".github/workflows/ci.yml"
	r, c, dir := stepsRunner(t, ciFiles, refusedWindow(t), outOfReachAnswer(t, where), refusedWindow(t))
	d := onePass(t, r, c)
	if run, err := r.Store.LoopRunByID(ctx, d.Run.ID); err != nil || run.Failure != "build propose" || !strings.Contains(run.Summary, `"fortnight"`) {
		t.Fatalf("the first build was not refused for its window: %+v %v", run, err)
	}
	heldFor(t, r, c, onePass(t, r, c), where)
	// a person sends the case on (it ends the hold), and the problem recurs
	next := "assert Answer() == 42 at its boundary"
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.AddCaseEvidence(ctx, c.ID, []store.Evidence{{Ref: "file:/t/out-of-reach-recurs", At: time.Now()}}, "test"); err != nil {
		t.Fatal(err)
	}
	d = onePass(t, r, c)
	if run, err := r.Store.LoopRunByID(ctx, d.Run.ID); err != nil || run.Failure != "build propose" || builds(t, dir) != "3" {
		t.Fatalf("the third build was not refused for its window: %+v %v", run, err)
	}
	given, err := os.ReadFile(filepath.Join(dir, "refusal-3"))
	if err != nil || !strings.Contains(string(given), `invalid measurement window "fortnight"`) {
		t.Fatalf("the build after the hold was not given the refusal before it: %q %v", given, err)
	}
	if d := onePass(t, r, c); d.Run.ID != "" || !strings.Contains(d.Skipped, "stopped: two of its build runs were refused for one cause") || builds(t, dir) != "3" {
		t.Fatalf("two refusals for one cause around a hold did not stop the case: %+v", d)
	}
}

// A person's change of the case's fix ends its hold: its note is resolved before the case builds again, and a build
// that names another path holds it on that one. A change of the case's status, even one undone before the next pass,
// ends it too; a change of its title or benefit alone does not, whatever their text reads as.
func TestAPersonsChangeEndsAHold(t *testing.T) {
	ctx := context.Background()
	where, other := ".github/workflows/ci.yml", ".github/workflows/release.yml"
	t.Run("fix", func(t *testing.T) {
		r, c, dir := stepsRunner(t, ciFiles, outOfReachAnswer(t, where), outOfReachAnswer(t, other))
		heldFor(t, r, c, onePass(t, r, c), where)
		next := "assert Answer() == 42 at its boundary"
		if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{NextTest: &next}, "test"); err != nil {
			t.Fatal(err)
		}
		heldFor(t, r, c, onePass(t, r, c), other)
		if builds(t, dir) != "2" {
			t.Fatalf("the changed case did not build again: %s builds", builds(t, dir))
		}
		if n, ok := personNote(t, r, c, where); !ok || n.Done.IsZero() {
			t.Fatalf("the ended hold's note stands: %+v %v", n, ok)
		}
		stillHeld(t, r, c, dir, "2", other)
	})
	t.Run("status", func(t *testing.T) {
		r, c, dir := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
		mech, next := "Answer is off by one", "assert Answer() == 42"
		for _, status := range []store.CaseStatus{store.CaseInvestigating, store.CaseTesting} {
			if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: status, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
				t.Fatal(err)
			}
		}
		heldFor(t, r, c, onePass(t, r, c), where)
		if builds(t, dir) != "2" {
			t.Fatalf("the case sent back and returned did not build again: %s builds", builds(t, dir))
		}
	})
	t.Run("title", func(t *testing.T) {
		r, c, dir := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
		// a title and a benefit whose text reads as a change of the fix and of the mechanism change neither
		title, benefit := "the thing breaks; fix: in CI", "fewer breaks; mechanism: none"
		if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Title: &title, Benefit: &benefit}, "test"); err != nil {
			t.Fatal(err)
		}
		stillHeld(t, r, c, dir, "1", where)
	})
}

// An out-of-reach answer whose path a change may touch is the build's own refusal, as a build that changes nothing was
// before: no hold, and two for that cause stop the case.
func TestAnOutOfReachPathInReachIsTheBuildsRefusal(t *testing.T) {
	ctx := context.Background()
	r, c, dir := outOfReachRunner(t, "fake.go")
	for i := 1; i <= 2; i++ {
		d := onePass(t, r, c)
		run, err := r.Store.LoopRunByID(ctx, d.Run.ID)
		if err != nil || run.Status != store.RunFailed || run.Failure != "build report" || run.Cause == "" || run.Cause == hostCause ||
			!strings.Contains(run.Summary, "build report: refused: the out-of-reach path fake.go is one a change may touch") {
			t.Fatalf("build %d of an in-reach path was not refused for it: %+v %v", i, run, err)
		}
	}
	if _, ok := personNote(t, r, c, "fake.go"); ok {
		t.Fatal("an in-reach path held its case")
	}
	if d := onePass(t, r, c); d.Run.ID != "" || !strings.Contains(d.Skipped, "stopped: two of its build runs were refused for one cause") || builds(t, dir) != "2" {
		t.Fatalf("two refusals of an in-reach path did not stop the case: %+v", d)
	}
}

// A path BASE does not hold, in a directory it does not hold, is the build's refusal, though no change may touch it
// either: a made-up path gets no hold, and neither does a new file at the root (Jenkinsfile, CI configuration a host
// would run), which BASE cannot tell from a made-up one. A new file in a directory below the root BASE holds is a path
// the fix may need.
func TestAMadeUpPathIsTheBuildsRefusal(t *testing.T) {
	ctx := context.Background()
	for _, where := range []string{".github/made-up/x.yml", "Jenkinsfile"} {
		t.Run(where, func(t *testing.T) {
			r, c, _ := outOfReachRunner(t, where)
			d := onePass(t, r, c)
			run, err := r.Store.LoopRunByID(ctx, d.Run.ID)
			if err != nil || run.Failure != "build report" || run.Cause == hostCause ||
				!strings.Contains(run.Summary, "build report: refused: the out-of-reach path "+where+" is no path of the repository at BASE") {
				t.Fatalf("a made-up path was not refused: %+v %v", run, err)
			}
			if _, ok := personNote(t, r, c, where); ok {
				t.Fatal("a made-up path held its case")
			}
		})
	}
	t.Run("new file", func(t *testing.T) {
		where := ".github/workflows/test.yml"
		r, c, _ := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
	})
}

// A path PathScope allows but no change can touch, since the patch check refuses it, is out of reach too: one in Git's
// own directory, and a submodule or a symlink of BASE (TestAnOutOfReachPathIsReadAsBASEReadsIt reads paths under them).
func TestPathsThePatchCheckRefusesAreOutOfReach(t *testing.T) {
	for _, where := range []string{".git/config", "vendor/lib", "link"} {
		t.Run(where, func(t *testing.T) {
			r, c, _ := outOfReachRunner(t, where)
			main := r.Inputs.ReadDirs[0]
			// a submodule and a symlink in BASE: the submodule's directory is in the checkout, uninitialized
			if err := os.MkdirAll(filepath.Join(main, "vendor", "lib"), 0o755); err != nil {
				t.Fatal(err)
			}
			run(t, main, "git", "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,vendor/lib")
			if err := os.Symlink("fake.go", filepath.Join(main, "link")); err != nil {
				t.Fatal(err)
			}
			run(t, main, "git", "add", "link")
			run(t, main, "git", "commit", "-q", "-m", "a submodule and a symlink")
			run(t, main, "git", "push", "-q", "origin", "HEAD:main")
			heldFor(t, r, c, onePass(t, r, c), where)
		})
	}
}

// A standing hold whose note was never put (its line went in and its note failed) gets its note at the next pick,
// and the case still takes no slot.
func TestAHoldWithNoNoteGetsItsNote(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	f := newRepo(t, ciFiles)
	r.Ship = shipWithSandbox(st, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	c := openCase(t, st, "failures", "out-of-reach-unnoted", 1)
	mech, next := "the workflow runs no test", "run the tests in the workflow"
	c, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, st, c, "main")
	where := ".github/workflows/ci.yml"
	raw, err := json.Marshal(map[string]any{"path": where, "fix": outOfReachFix, "reason": where + " is CI configuration", "episode": c.Episode,
		"mechanism_sum": hexSum(mech), "fix_sum": hexSum(next)})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CaseNote(ctx, c.ID, "held for a person", string(raw), "loop"); err != nil {
		t.Fatal(err)
	}
	cands, why, err := r.candidates(ctx, c.Family, "", false)
	if err != nil || len(cands) != 0 || !strings.Contains(why, c.ID+" held for a person: its fix needs "+where) {
		t.Fatalf("the held case is a candidate: %+v %q %v", cands, why, err)
	}
	if n, ok := personNote(t, r, c, where); !ok || !n.Done.IsZero() || !strings.Contains(n.Detail, outOfReachFix) {
		t.Fatalf("the hold's missing note was not put: %+v %v", n, ok)
	}
}

// An out-of-reach answer names a path of the repository and a fix: one with no fix, or a path that leaves the
// repository or is not clean, is the build's refusal at its report.
func TestAnOutOfReachAnswerNamesAPathAndAFix(t *testing.T) {
	report := func(where, fix string) []byte {
		raw, err := json.Marshal(map[string]any{"summary": "out of reach", "test_command": []string{}, "title": "", "body": "", "rollback": "",
			"window": "", "lesson": "", "signal": map[string]any{"source": "", "kinds": []string{}, "match": []string{}},
			"out_of_reach": map[string]string{"path": where, "fix": fix}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, fix := range []string{"", "  \t "} {
		if _, err := buildReport(report(".github/workflows/ci.yml", fix)); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has no fix") {
			t.Fatalf("an answer with the fix %q: %v", fix, err)
		}
	}
	for _, where := range []string{"", "../x", "a/../../x", "/etc/passwd", "./ci.yml", "a//b", `a\b`, "a\nb"} {
		if _, err := buildReport(report(where, outOfReachFix)); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "is no path of the repository") {
			t.Fatalf("an answer with the path %q: %v", where, err)
		}
	}
	got, err := buildReport(report(".github/workflows/", outOfReachFix))
	if err != nil || got.OutOfReach == nil || got.OutOfReach.Path != ".github/workflows" {
		t.Fatalf("a directory's path: %+v %v", got.OutOfReach, err)
	}
	got, err = buildReport(report("Jenkinsfile", outOfReachFix))
	if err != nil || got.OutOfReach == nil || got.OutOfReach.Path != "Jenkinsfile" {
		t.Fatalf("a root file's path: %+v %v", got.OutOfReach, err)
	}
}

// hexSum is the hex SHA-256 of text, as a hold keeps the sums of its case's mechanism and fix.
func hexSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A person ends a hold by leaving testing: closing the case resolves its note, and the case, reopened, builds again;
// a case sent back to its investigation has its hold ended as the pass begins, and builds again back in testing.
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
		r, c, dir := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
		if _, err := r.Store.CloseCase(ctx, c.ID, store.Solved, "", "the workflow runs its tests now", "test"); err != nil {
			t.Fatal(err)
		}
		onePass(t, r, c)
		if n, ok := personNote(t, r, c, where); !ok || n.Done.IsZero() {
			t.Fatalf("the close did not resolve the hold's note: %+v %v", n, ok)
		}
		c, err := r.Store.ReopenCase(ctx, c.ID, []store.Evidence{{Ref: "file:/t/out-of-reach-back", At: time.Now()}}, "it came back", "test")
		if err != nil {
			t.Fatal(err)
		}
		c = backToTesting(t, r, c)
		setCaseRepo(t, r.Store, c, "main")
		heldFor(t, r, c, onePass(t, r, c), where)
		if builds(t, dir) != "2" {
			t.Fatalf("the reopened case did not build: %s builds", builds(t, dir))
		}
	})
	t.Run("investigating", func(t *testing.T) {
		ctx := context.Background()
		r, c, dir := outOfReachRunner(t, where)
		heldFor(t, r, c, onePass(t, r, c), where)
		c, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "test")
		if err != nil {
			t.Fatal(err)
		}
		// the pass's start, before the case's investigation would run
		if err := r.releaseHolds(ctx, []store.Case{c}); err != nil {
			t.Fatal(err)
		}
		if n, ok := personNote(t, r, c, where); !ok || n.Done.IsZero() {
			t.Fatalf("leaving testing did not resolve the hold's note: %+v %v", n, ok)
		}
		log, err := r.Store.CaseLog(ctx, c.ID)
		if err != nil || log[len(log)-1].Action != "person hold ended" || log[len(log)-1].Detail != "the case left testing" {
			t.Fatalf("leaving testing did not end the hold: %+v %v", log, err)
		}
		c = backToTesting(t, r, c)
		heldFor(t, r, c, onePass(t, r, c), where)
		if builds(t, dir) != "2" {
			t.Fatalf("the case back in testing did not build: %s builds", builds(t, dir))
		}
	})
}

// A person who resolves the hold's note is done waiting: the next pass ends the hold and builds the case again, where a
// pass before it built nothing.
func TestAPersonWhoResolvesTheNoteEndsTheHold(t *testing.T) {
	ctx := context.Background()
	where := ".github/workflows/ci.yml"
	r, c, dir := outOfReachRunner(t, where)
	heldFor(t, r, c, onePass(t, r, c), where)
	stillHeld(t, r, c, dir, "1", where)
	n, _ := personNote(t, r, c, where)
	if _, _, err := r.Store.DoneNote(ctx, n.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	heldFor(t, r, c, onePass(t, r, c), where)
	if builds(t, dir) != "2" {
		t.Fatalf("a resolved note did not end the hold: %s builds", builds(t, dir))
	}
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	for _, e := range log {
		ended = ended || e.Action == "person hold ended" && e.Detail == "a person resolved its note"
	}
	if !ended {
		t.Fatalf("the case's log keeps no end of its hold: %+v", log)
	}
}

// A path in Git's own directory is read where Git keeps it: in a linked worktree, whose .git is a file, the
// worktree's git directory or the repository's common one. One in neither is no path of the repository.
func TestAGitPathInALinkedWorktreeIsOutOfReach(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	f := newRepo(t, ciFiles)
	r.Ship = shipWithSandbox(st, f, Sandbox{})
	r.Ship.Dir = r.Dir
	wt := filepath.Join(t.TempDir(), "main")
	run(t, f.main, "git", "worktree", "add", "-q", "--detach", wt, "HEAD")
	if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the worktree's .git is no file: %v %v", fi, err)
	}
	p := Package{Case: openCase(t, st, "failures", "out-of-reach-worktree", 1).ID, Repo: "agent-sh/fake", Dir: wt, Host: f.main, BaseCommit: strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))}
	for _, where := range []string{".git/config", ".git/refs/heads/fix"} {
		if why, refusal, err := r.unreachable(ctx, p, where); err != nil || refusal != nil || why == nil || !strings.Contains(why.Error(), "Git's own directory") {
			t.Fatalf("%s in a linked worktree: %v %v %v", where, why, refusal, err)
		}
	}
	if why, refusal, err := r.unreachable(ctx, p, ".git/made-up/x"); err != nil || why != nil || refusal == nil {
		t.Fatalf("a made-up path in Git's directory: %v %v %v", why, refusal, err)
	}
}

// The stop rule does not count a build that answered its fix is out of reach: an agent's failure on either side of
// one stops the case as two in a row do.
func TestAnOutOfReachBuildDoesNotSplitTwoAgentFailures(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "out-of-reach-turns", 1)
	at := time.Now().Add(-time.Hour)
	putBuild(t, st, c, "20261109-000001-00000001", StepBuild, store.RunTurns, "turns", "", "build run: it reached its turns", at)
	putBuild(t, st, c, "20261109-000002-00000002", StepBuild, store.RunOutOfReach, "", "", "The fix is the workflow's.", at.Add(time.Minute))
	putBuild(t, st, c, "20261109-000003-00000003", StepBuild, store.RunTurns, "turns", "", "build run: it reached its turns", at.Add(2*time.Minute))
	if why, err := r.stopped(ctx, c, true); err != nil || why != "its last two build runs failed the same way: turns" {
		t.Fatalf("turns, out of reach, turns did not stop the case: %q %v", why, err)
	}
}

// A configuration build changes its one file: an out-of-reach answer from one is its refusal at its report, before
// the host reads anything.
func TestAConfigurationBuildAnswersNoOutOfReachPath(t *testing.T) {
	r := &Runner{}
	res, err := r.outOfReach(context.Background(), store.Case{ID: "c00000000"}, store.LoopRun{}, Result{Status: store.RunOK},
		Package{Kind: KindConfig}, OutOfReach{Path: ".github/workflows/ci.yml", Fix: outOfReachFix})
	if err != nil || res.Status != store.RunFailed || res.Failure != "build report" || !strings.Contains(res.Text, "a configuration build changes its file") {
		t.Fatalf("a configuration build's out-of-reach answer: %+v %v", res, err)
	}
}

// hostPackage is a package of case caseID over the fixture's main clone as its host clone, at its HEAD.
func hostPackage(t *testing.T, f repoFixture, caseID string) Package {
	t.Helper()
	return Package{Case: caseID, Repo: "agent-sh/fake", Dir: f.main, Host: f.main, BaseCommit: strings.TrimSpace(run(t, f.main, "git", "rev-parse", "HEAD"))}
}

// An out-of-reach path is read as BASE reads it. A submodule or a symlink of BASE is out of reach as the entry itself.
// A path through a symlink that points inside the repository is its target's, out of reach or in reach, and out of
// reach when the paths rule refuses any path on the way (a CI path whose directory links elsewhere). A path under a
// submodule, or under a symlink out of the repository, one that never ends or one whose target climbs back out of a
// directory it names, and a new root file no change may create are no path of BASE: BASE cannot tell any of them from
// a made-up one. A new root file a change may create is in reach.
func TestAnOutOfReachPathIsReadAsBASEReadsIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	f := newRepo(t, map[string]string{".github/workflows/ci.yml": "name: ci\n", ".github/workflows/release.yml": "name: release\n",
		"tools/actions/setup/action.yml": "name: setup\n"})
	r.Ship = shipWithSandbox(st, f, Sandbox{})
	r.Ship.Dir = r.Dir
	run(t, f.main, "git", "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,vendor/lib")
	for link, target := range map[string]string{"ci": ".github/workflows", "chain": "ci", "here": ".", "file": "fake.go", "out": "../elsewhere", "abs": "/etc", "loop": "loop",
		".github/actions": "../tools/actions", "s": ".github/workflows/inner", "l": "s/.."} {
		if err := os.Symlink(target, filepath.Join(f.main, link)); err != nil {
			t.Fatal(err)
		}
		run(t, f.main, "git", "add", link)
	}
	run(t, f.main, "git", "commit", "-q", "-m", "a submodule and symlinks")
	p := hostPackage(t, f, openCase(t, st, "failures", "out-of-reach-links", 1).ID)
	for where, reason := range map[string]string{"vendor/lib": "vendor/lib is a symlink or a submodule", "file": "file is a symlink or a submodule",
		"ci/ci.yml": ".github/workflows/ci.yml is CI configuration", "ci/new.yml": ".github/workflows/new.yml is CI configuration",
		"chain/release.yml":                ".github/workflows/release.yml is CI configuration",
		".github/actions/setup/action.yml": ".github/actions/setup/action.yml is CI configuration"} {
		if why, refusal, err := r.unreachable(ctx, p, where); err != nil || refusal != nil || why == nil || !strings.Contains(why.Error(), reason) {
			t.Errorf("%s is not out of reach: %v %v %v", where, why, refusal, err)
		}
	}
	under, none := "is under a submodule or a symlink of BASE whose content BASE does not hold", "is no path of the repository at BASE"
	for where, reason := range map[string]string{"vendor/lib/x.go": under, "out/x": under, "abs/x": under, "loop/x": under, "l/ci.yml": under,
		"file/x": none, "ci/made-up/x.yml": none, "Jenkinsfile": none,
		"here/fake.go":      "is fake.go through a symlink of BASE, which a change may touch",
		"here/CHANGELOG.md": "is CHANGELOG.md through a symlink of BASE, which a change may touch"} {
		if why, refusal, err := r.unreachable(ctx, p, where); err != nil || why != nil || refusal == nil || !strings.Contains(refusal.Error(), where+" "+reason) {
			t.Errorf("%s is not the build's refusal: %v %v %v", where, why, refusal, err)
		}
	}
	// a new root file a change may create is in reach, as fake.go is: the build's refusal says to make the fix there
	for _, where := range []string{"fake.go", "CHANGELOG.md"} {
		if why, refusal, err := r.unreachable(ctx, p, where); err != nil || why != nil || refusal != nil {
			t.Errorf("%s is not in reach: %v %v %v", where, why, refusal, err)
		}
	}
}

// heldRunner is a runner over a host repository with ciFiles and no sandbox, with a case in testing on its main clone.
func heldRunner(t *testing.T, sig string) (*Runner, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	r := localRunner(t, st)
	f := newRepo(t, ciFiles)
	r.Ship = shipWithSandbox(st, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	c := openCase(t, st, "failures", sig, 1)
	mech, next := "the workflow runs no test", "run the tests in the workflow"
	c, err := st.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, st, c, "main")
	return r, c, hostPackage(t, f, c.ID)
}

// A person's change of the case's status after the case and its log were read for its build ends the hold that build
// answers, though the change is logged before the hold, and before the run's reservation when it comes while the
// build is prepared: the hold keeps the newest line of the log the build was given. The next pass builds the case
// again. The change that moved the case to testing, before the build, ends no hold (the other tests here).
func TestAStatusChangeAfterTheBuildReadTheCaseEndsItsHold(t *testing.T) {
	where := ".github/workflows/ci.yml"
	r, c, dir := outOfReachRunner(t, where)
	busy, changed := r.Ship.Gate.Busy, false
	r.Ship.Gate.Busy = func(ctx context.Context, main string) (string, error) {
		if !changed { // the gate runs as the build is prepared, after its inputs read the case's log
			changed = true
			for _, status := range []store.CaseStatus{store.CaseInvestigating, store.CaseTesting} {
				if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: status}, "test"); err != nil {
					return "", err
				}
			}
		}
		return busy(ctx, main)
	}
	heldFor(t, r, c, onePass(t, r, c), where)
	if !changed {
		t.Fatal("the gate did not run as the build was prepared")
	}
	heldFor(t, r, c, onePass(t, r, c), where)
	if builds(t, dir) != "2" {
		t.Fatalf("a change after the build read the case did not end its hold: %s builds", builds(t, dir))
	}
}

// A hold line the loop cannot read is a hold all the same: the case takes no slot and gets a note that says so, until
// a person changes the case; new evidence does not end it.
func TestAnUnreadableHoldLineIsAHold(t *testing.T) {
	ctx := context.Background()
	r, c, _ := heldRunner(t, "out-of-reach-unread")
	if err := r.Store.CaseNote(ctx, c.ID, "held for a person", `{"path":".github/workflows/ci.yml","fix":"Run the te`, "loop"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.AddCaseEvidence(ctx, c.ID, []store.Evidence{{Ref: "file:/t/out-of-reach-unread-again", At: time.Now()}}, "test"); err != nil {
		t.Fatal(err)
	}
	cands, why, err := r.candidates(ctx, c.Family, "", false)
	if err != nil || len(cands) != 0 || !strings.Contains(why, c.ID+" held for a person: its hold line cannot be read") {
		t.Fatalf("a case with an unreadable hold line is a candidate: %+v %q %v", cands, why, err)
	}
	n, ok := personNote(t, r, c, "its hold line cannot be read")
	if !ok || !n.Done.IsZero() || !strings.Contains(n.Detail, "Read the build's answer in the case's log") {
		t.Fatalf("the unreadable hold's note: %+v %v", n, ok)
	}
	title := "the workflow is wrong"
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Title: &title}, "test"); err != nil {
		t.Fatal(err)
	}
	cands, why, err = r.candidates(ctx, c.Family, "", false)
	if err != nil || len(cands) != 1 || strings.Contains(why, "held for a person") {
		t.Fatalf("a person's change did not end the unreadable hold: %+v %q %v", cands, why, err)
	}
	if n, ok := personNote(t, r, c, "its hold line cannot be read"); !ok || n.Done.IsZero() {
		t.Fatalf("the ended hold's note stands: %+v %v", n, ok)
	}
}
