package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A repository as of a fix is a clone at the commit before it with nothing after it: no other ref, no reflog, and
// none of the later commits' objects.
func TestARepositoryAsOfItsFixHoldsNothingAfter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	run(t, dir, "git", "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "a.go"), "package a\n")
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-q", "-m", "before")
	before := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	write(t, filepath.Join(src, "a.go"), "package a\n\n// the fix\n")
	run(t, src, "git", "commit", "-q", "-am", "the fix")
	fix := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	run(t, src, "git", "branch", "later")
	dst := filepath.Join(dir, "as-of")
	if err := cloneAsOf(context.Background(), src, before, dst); err != nil {
		t.Fatal(err)
	}
	if log := run(t, dst, "git", "log", "--all", "--format=%s"); strings.TrimSpace(log) != "before" {
		t.Fatalf("the clone's history: %q", log)
	}
	if err := exec.Command("git", "-C", dst, "cat-file", "-e", fix).Run(); err == nil {
		t.Fatal("the fix's commit is in the clone")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.go")); strings.Contains(string(b), "the fix") {
		t.Fatal("the work tree holds the fix")
	}
}

// A clone that fails says which git step failed.
func TestACloneThatFailsNamesItsStep(t *testing.T) {
	dir := t.TempDir()
	err := cloneAsOf(context.Background(), filepath.Join(dir, "no-such-source"), "0000000000000000000000000000000000000000", filepath.Join(dir, "dst"))
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("the error does not name the clone step: %v", err)
	}
}

// A golden task is read whole: an id, a family, a symptom's title, a cutoff, its events and its key facts; its
// repository's path is expanded.
func TestGoldenTasksAreReadWhole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	write(t, p, `{"id":"a","family":"failures","case_title":"x keeps failing","cutoff":"2026-03-02T10:00:00Z","symptom":{"query":{"event_ids":["e1"]}},"root_cause":"a cause","key_facts":["f"],"repo_path":"~/projects/x"}`+"\n\n")
	ts, err := ReadGolden(p)
	if err != nil || len(ts) != 1 || ts[0].RepoPath != filepath.Join(home, "projects", "x") {
		t.Fatalf("%+v %v", ts, err)
	}
	write(t, p, `{"id":"a","family":"failures","case_title":"x","cutoff":"2026-03-02T10:00:00Z","symptom":{"query":{"event_ids":[]}},"root_cause":"a cause","key_facts":["f"]}`+"\n")
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("a task with no events was read")
	}
}

// An evaluation is summed by task: its trials' verdicts, how many reached the root cause, whether any and every
// trial did (pass@k, pass^k), whether they disagreed, what they cost, and the errors of trials that ran no
// investigation.
func TestAnEvaluationIsSummedByTask(t *testing.T) {
	at := time.Now()
	trial := func(task, verdict string, usd float64, minutes int) Trial {
		return Trial{Task: task, Family: "failures", Grade: Grade{Verdict: verdict},
			Run: store.LoopRun{USD: usd, Turns: 10, Started: at, Ended: at.Add(time.Duration(minutes) * time.Minute)}}
	}
	sums := Summarize([]Trial{trial("a", "reached", 1, 4), trial("a", "partial", 2, 6), trial("b", "reached", 1, 5), trial("b", "reached", 1, 5),
		{Task: "c", Family: "sessions", Err: "the repository as of the fix: no clone"}})
	if len(sums) != 3 {
		t.Fatalf("%+v", sums)
	}
	a, b, c := sums[0], sums[1], sums[2]
	if a.Reached != 1 || !a.AnyReached || a.AllReached || !a.Flipped || a.USD != 3 || a.Minutes != 5 {
		t.Errorf("a %+v", a)
	}
	if b.Reached != 2 || !b.AllReached || b.Flipped {
		t.Errorf("b %+v", b)
	}
	if len(c.Verdicts) != 0 || len(c.Errors) != 1 || c.AnyReached {
		t.Errorf("c %+v", c)
	}
}

// The judge grades an investigation from what it is given, with no tools, by its report; one that reports no
// grade is an error, not a verdict.
func TestAJudgeGradesFromItsReport(t *testing.T) {
	dir := t.TempDir()
	grade := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.05,` +
		`"structured_output":{"verdict":"reached","facts":[{"fact":"the guard matched its own shell","reached":true}],"reason":"it says so"}}`
	p := script(t, dir, "judge", `printf '%s\n' "$@" > "`+filepath.Join(dir, "args")+`"
`+modelResult(grade))
	st := testStore(t)
	e := &Eval{Runner: &Runner{Store: st}, Judge: testJail(t, Agent{Runner: []string{p}, Model: "judge-model"})}
	task := GoldenTask{ID: "a", Family: "failures", RootCause: "the guard matched its own shell", KeyFacts: []string{"it matched its own shell"}}
	g, usd, _, err := e.grade(context.Background(), task, finding{Mechanism: "the guard matched its own shell"}, filepath.Join(t.TempDir(), "judge"),
		store.LoopRun{Experiment: "eval:x:a:1"})
	if err != nil || g.Verdict != "reached" || len(g.Facts) != 1 || usd != 0.05 {
		t.Fatalf("%+v %v %v", g, usd, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "--tools\n\n") || !strings.Contains(string(args), "the guard matched its own shell") {
		t.Fatalf("the judge's arguments:\n%s", args)
	}
	bad := script(t, dir, "judge2", modelResult(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.05}`))
	e.Judge.Runner = []string{bad}
	if _, _, _, err := e.grade(context.Background(), task, finding{}, filepath.Join(t.TempDir(), "judge"), store.LoopRun{}); err == nil {
		t.Fatal("a judge with no grade gave one")
	}
}

// An evaluation stops starting trials when its budget has no room for another whole run.
func TestAnEvaluationStopsAtItsBudget(t *testing.T) {
	e, task := evalFixture(t)
	e.Trials, e.Budget, e.Arms = 2, 3, []string{"loop"}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if !strings.Contains(tr.Skipped, "budget") {
			t.Errorf("a trial started past the budget: %+v", tr)
		}
	}
}
