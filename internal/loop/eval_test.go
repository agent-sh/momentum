package loop

import (
	"context"
	"fmt"
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
echo '`+grade+`'`)
	st := testStore(t)
	e := &Eval{Runner: &Runner{Store: st}, Judge: Agent{Runner: []string{p}, Model: "judge-model"}}
	task := GoldenTask{ID: "a", Family: "failures", RootCause: "the guard matched its own shell", KeyFacts: []string{"it matched its own shell"}}
	g, usd, err := e.grade(context.Background(), task, finding{Mechanism: "the guard matched its own shell"}, t.TempDir(),
		store.LoopRun{Experiment: "eval:x:a:1"})
	if err != nil || g.Verdict != "reached" || len(g.Facts) != 1 || usd != 0.05 {
		t.Fatalf("%+v %v %v", g, usd, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "--tools\n\n") || !strings.Contains(string(args), "the guard matched its own shell") {
		t.Fatalf("the judge's arguments:\n%s", args)
	}
	bad := script(t, dir, "judge2", `echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.05}'`)
	e.Judge.Runner = []string{bad}
	if _, _, err := e.grade(context.Background(), task, finding{}, t.TempDir(), store.LoopRun{}); err == nil {
		t.Fatal("a judge with no grade gave one")
	}
}

// An evaluation stops starting trials when its budget has no room for another whole run.
func TestAnEvaluationStopsAtItsBudget(t *testing.T) {
	st := testStore(t)
	e := &Eval{Runner: &Runner{Store: st, Budgets: Budgets{Run: 4}}, Source: filepath.Join(t.TempDir(), "none.db"), Dir: t.TempDir(), Trials: 2, Budget: 3,
		Parallel: 1, ID: "x", Arms: []string{"loop"}}
	trs, err := e.Run(context.Background(), []GoldenTask{{ID: "a", Family: "failures"}})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if !strings.Contains(tr.Skipped, "budget") {
			t.Errorf("a trial started past the budget: %+v", tr)
		}
	}
}

// A trial runs the loop's investigation or the native path: the same agent, system prompt, report, permissions and
// limits, asked the question with no procedure or skill. The loop's prompt holds its case's evidence and the runner
// writes its report into the case, which the judge reads; the native arm's report is what the judge reads. Neither
// may change a case itself, and neither reads the machine's rules as they are now, which hold the lessons of later
// fixes.
func TestATrialRunsTheLoopOrTheNativePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	src, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	e1 := store.Event{Source: store.Transcripts, SourceKey: "e1", TS: cutoff.Add(-time.Hour), Session: "s1", Harness: "claude", Actor: store.ActorTool,
		Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text"}
	if _, err := src.Put(ctx, []store.Event{e1}); err != nil {
		t.Fatal(err)
	}
	src.Close()
	report := `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.5,"structured_output":%s}`
	agent := func(name, out string) []string {
		return []string{script(t, dir, name, `cat "$(printf '%s\n' "$@" | sed -n '/--append-system-prompt-file/{n;p}')" > "`+filepath.Join(dir, name+".system")+`"
printf '%s\n' "$@" > "`+filepath.Join(dir, name+".args")+`"
echo '`+fmt.Sprintf(report, out)+`'`)}
	}
	judge := script(t, dir, "judge", `printf '%s\n' "$@" > "`+filepath.Join(dir, "judge.args")+`"
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.05,"structured_output":{"verdict":"partial","facts":[],"reason":"r"}}'`)
	rules := filepath.Join(dir, "rules.md")
	write(t, rules, "Delete with literal paths.")
	task := GoldenTask{ID: "t1", Family: "failures", CaseTitle: "a thing keeps failing", Cutoff: cutoff, KeyFacts: []string{"f"}}
	task.Symptom.Query.EventIDs = []string{e1.ID()}
	for _, native := range []bool{false, true} {
		name := map[bool]string{false: "loop", true: "native"}[native]
		out := `{"mechanism":"the loop mechanism","alternatives":["another"],"fix":"the loop fix","summary":"looked"}`
		if native {
			out = `{"mechanism":"the native mechanism","alternatives":["another"],"root_cause":"the native root","summary":"found it"}`
		}
		st := testStore(t)
		e := &Eval{Runner: &Runner{Store: st, Agent: Agent{Runner: agent(name, out), Model: "m"}, Budgets: Budgets{Run: 4, RunTime: time.Minute, MaxTurns: 9},
			Rules: []string{rules}, Constraints: []string{"never wake him"}}, Source: source, Dir: filepath.Join(dir, "out", name), Trials: 1, Budget: 10,
			Parallel: 1, ID: name, Judge: Agent{Runner: []string{judge}, Model: "j"}, Native: native, Arms: []string{name}}
		trs, err := e.Run(ctx, []GoldenTask{task})
		if err != nil || len(trs) != 1 || trs[0].Err != "" || trs[0].Grade.Verdict != "partial" || trs[0].Run.USD != 0.5 {
			t.Fatalf("%s: %+v %v", name, trs, err)
		}
		system, _ := os.ReadFile(filepath.Join(dir, name+".system"))
		args, _ := os.ReadFile(filepath.Join(dir, name+".args"))
		judged, _ := os.ReadFile(filepath.Join(dir, "judge.args"))
		switch {
		case !strings.Contains(string(system), "never wake him") || strings.Contains(string(system), "Delete with literal paths."):
			t.Errorf("%s: the system prompt lacks the owner's constraints, or holds the machine's rules as they are now:\n%s", name, system)
		case strings.Contains(string(system), "momentum's improvement loop") || !strings.HasPrefix(string(system), nativeBrief):
			t.Errorf("%s: not the native brief alone:\n%s", name, system)
		case strings.Contains(string(args), "Bash(momentum case") || !strings.Contains(string(args), e1.ID()):
			t.Errorf("%s: it may change a case, or it is not given its events:\n%s", name, args)
		case !native && (!strings.Contains(string(args), "zz-signal-text") || !strings.Contains(string(judged), "the loop mechanism") ||
			!strings.Contains(string(judged), "the loop fix") || trs[0].Status != string(store.CaseTesting)):
			t.Errorf("loop: its case's evidence is not in its prompt, or its report is not on its case:\n%s\n%s\n%s", args, judged, trs[0].Status)
		case native && (!strings.Contains(string(judged), "the native mechanism")):
			t.Errorf("native: the judge did not read its report:\n%s", judged)
		case native && trs[0].Status != "native":
			t.Errorf("native: its status %q", trs[0].Status)
		}
	}
}
