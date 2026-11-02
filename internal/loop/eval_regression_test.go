package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// evalFixture builds only synthetic evidence. Its executables print recorded stream messages, never call a model.
func evalFixture(t *testing.T) (*Eval, GoldenTask) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	ev := store.Event{Source: store.Transcripts, SourceKey: "symptom", TS: at, Session: "s", Actor: store.ActorTool, Kind: "tool_result", Summary: "the worker failed"}
	if _, err = st.Put(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	task := GoldenTask{ID: "worker-reads-before-lock", Family: "failures", CaseTitle: "the worker failed", Cutoff: at.Add(time.Hour), RootCause: "a verified cause", KeyFacts: []string{"a verified fact"}}
	task.Symptom.Query.EventIDs = []string{ev.ID()}
	agent := script(t, dir, "fixture-run", `printf '%s\n' "$@" > args.txt
printf '%s' "$MOMENTUM_NOW" > clock.txt
printf '%s\n' '{"type":"assistant","message":{"model":"actual-model"}}' '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"a mechanism","root":"root:cause","root_cause":"root:cause","fix":"a fix","summary":"a summary"}}'`)
	judge := script(t, dir, "fixture-judge", `printf '%s\n' "$@" > args.txt
printf '%s\n' '{"type":"assistant","message":{"model":"actual-judge"}}' '{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached","facts":[],"reason":"a reason"}}'`)
	return &Eval{Runner: &Runner{Store: testStore(t), Agent: Agent{Runner: []string{agent}, Model: "requested-model"}, Budgets: Budgets{Run: 4, RunTime: time.Minute, MaxTurns: 10}},
		Source: source, Dir: filepath.Join(dir, "evaluation"), ID: "synthetic-evaluation", Trials: 1, Budget: 50, Parallel: 1, Judge: Agent{Runner: []string{judge}, Model: "requested-judge", Fallback: "unused"}, GoldenHash: digest("synthetic golden file")}, task
}

// The store ends at the symptom plus one minute, or at an earlier task cutoff, before later work on the cause.
func TestATrialEndsAtItsNewestSymptom(t *testing.T) {
	e, task := evalFixture(t)
	ctx := context.Background()
	end, err := e.snapshotEnd(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if !end.Equal(task.Cutoff.Add(-59 * time.Minute)) {
		t.Fatalf("end %s cutoff %s", end, task.Cutoff)
	}
	task.Cutoff = end.Add(-30 * time.Second)
	earlier, err := e.snapshotEnd(ctx, task)
	if err != nil || !earlier.Equal(task.Cutoff) {
		t.Fatalf("earlier %s %v", earlier, err)
	}
	task.Symptom.Query.EventIDs = []string{"absent"}
	if _, err = e.snapshotEnd(ctx, task); err == nil {
		t.Fatal("missing symptom accepted")
	}
}

// A task's answer-bearing id reaches only results.json, not either invocation, its case or its judge.
func TestATrialUsesAnOpaqueKey(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		if strings.Contains(tr.Run.Dir+tr.Run.Experiment, task.ID) || tr.Key == "" {
			t.Fatalf("not opaque: %+v", tr)
		}
		for _, f := range []string{"args.txt", "system.md", "case.json", "trial.json"} {
			raw, err := os.ReadFile(filepath.Join(tr.Run.Dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), task.ID) {
				t.Fatalf("id leaked in %s", f)
			}
		}
	}
	if trs[0].Key != trs[1].Key {
		t.Fatal("the arms lost their shared trial key")
	}
	if err = e.WriteResults(e.Results(trs)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(e.Dir, "results.json"))
	if !strings.Contains(string(raw), task.ID) || !strings.Contains(string(raw), trs[0].Key) {
		t.Fatal("mapping absent")
	}
}

// A stopped native run keeps its running findings, and an unpriced investigation costs its full budget even if ungraded.
func TestACutShortTrialKeepsItsFindingsAndCost(t *testing.T) {
	e, task := evalFixture(t)
	e.Arms = []string{"native"}
	body := `cat > findings.md <<'NOTES'
## Mechanism
the retained mechanism
## Alternatives ruled out
the other cause was tested
## Root cause
the retained root
## Fix
the retained fix
## Summary
the retained summary
NOTES
`
	for _, ending := range []string{"", `echo '{"type":"result","subtype":"error_max_budget_usd","total_cost_usd":4}'`} {
		e.Dir = filepath.Join(t.TempDir(), "eval")
		e.Runner.Agent.Runner = []string{script(t, t.TempDir(), "cut-short", body+ending)}
		trs, err := e.Run(context.Background(), []GoldenTask{task})
		if err != nil || len(trs) != 1 {
			t.Fatalf("%+v %v", trs, err)
		}
		tr := trs[0]
		if tr.Err != "" || !tr.CutShort || tr.Run.USD != 4 {
			t.Fatalf("%+v", tr)
		}
		raw, _ := os.ReadFile(filepath.Join(tr.JudgeDir, "args.txt"))
		for _, s := range []string{"the retained mechanism", "the retained fix", "the other cause was tested"} {
			if !strings.Contains(string(raw), s) {
				t.Errorf("judge lost %s: %s", s, raw)
			}
		}
		tr.Err = "judge unavailable"
		tr.Grade = Grade{}
		sums := Summarize([]Trial{tr}, 1)
		if sums[0].USD != 4 || sums[0].CutShort != 1 || sums[0].Ran != 1 || len(sums[0].Errors) != 1 {
			t.Fatalf("%+v", sums)
		}
	}
}

// Both arms set the same historical clock and frame the same investigation question.
func TestBothArmsUseTheTrialClockAndQuestion(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	end, _ := e.snapshotEnd(context.Background(), task)
	for _, tr := range trs {
		raw, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "clock.txt"))
		if string(raw) != end.Format(time.RFC3339Nano) {
			t.Fatalf("clock %q want %s", raw, end)
		}
		raw, _ = os.ReadFile(filepath.Join(tr.Run.Dir, "args.txt"))
		if !strings.Contains(string(raw), investigationQuestion) {
			t.Fatalf("question missing: %s", raw)
		}
	}
}

// The judge reads one capped and fenced shape, with the case as the loop record and the report as the native record.
func TestTheJudgeReadsTheSameFieldsFromBothArms(t *testing.T) {
	report := json.RawMessage(`{"mechanism":"mechanism","root":"root:cause","summary":"summary"}`)
	c := store.Case{Mechanism: "mechanism", Signatures: []string{"eval:answer"}, Alternatives: []string{"alternative"}, NextTest: "fix"}
	loop := caseFinding(c, Result{Report: report})
	native := nativeFinding(Result{Report: json.RawMessage(`{"mechanism":"mechanism","root_cause":"root:cause","alternatives":["alternative"],"fix":"fix","summary":"summary"}`)})
	if !reflect.DeepEqual(loop, native) {
		t.Fatalf("loop %+v native %+v", loop, native)
	}
	if judgeFinding(loop) != judgeFinding(native) {
		t.Fatal("judge inputs differ")
	}
	c.Signatures = append(c.Signatures, "root:recorded")
	if strings.Contains(judgeFinding(caseFinding(c, Result{Report: report})), "root:") {
		t.Fatal("root slug reached the judge")
	}
	long := strings.Repeat("a", 2100) + "forbidden-tail"
	raw := judgeFinding(finding{Mechanism: long, Alternatives: []string{long}, Fix: long, Report: "```\n" + long})
	if strings.Contains(raw, "forbidden-tail") || strings.Count(raw, "```") != 2 || strings.Contains(raw, "Status") {
		t.Fatalf("unclipped or unfenced: %s", raw)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(raw, "```json\n"), "\n```\n")), &fields); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if len(v) > 2000 {
			t.Fatalf("%s is %d bytes", k, len(v))
		}
	}
}

// Each active slot reserves the whole investigation and judge, and skipped trials remain in the result.
func TestTrialsInFlightHoldTheirBudget(t *testing.T) {
	e, task := evalFixture(t)
	e.Trials = 5
	e.Parallel = 3
	e.Budget = 10
	e.Runner.Agent.Runner = []string{script(t, t.TempDir(), "no-price", `sleep 0.1`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 10 {
		t.Fatalf("%d trials %v", len(trs), err)
	}
	ran := 0
	spent := 0.0
	for _, tr := range trs {
		if !tr.Run.Started.IsZero() {
			ran++
		}
		spent += tr.Run.USD + tr.JudgeUSD
	}
	if ran != 2 || spent > e.Budget {
		t.Fatalf("ran %d spent %g", ran, spent)
	}
}

// Missing trials do not pass every time, and only disagreement on reaching the cause is a flip.
func TestPairedSummariesKeepTheSameK(t *testing.T) {
	at := time.Now()
	trial := func(task, arm, v string, n int) Trial {
		return Trial{Task: task, Arm: arm, N: n, Grade: Grade{Verdict: v}, Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 2}}
	}
	trs := []Trial{trial("a", "loop", "reached", 1), trial("a", "loop", "reached", 2), {Task: "a", Arm: "loop", N: 3, Skipped: "budget"}, trial("a", "native", "missed", 1), trial("a", "native", "wrong", 2), trial("a", "native", "missed", 3)}
	e, _ := evalFixture(t)
	e.Trials = 3
	r := e.Results(trs)
	if r.Arms[0].PassAllK != 0 || r.Arms[0].PassAtK != 1 || r.Tasks[1].Flipped || r.Comparison.Tasks[0].MoreOften != "loop" {
		t.Fatalf("%+v", r)
	}
	if r.Comparison.ReachedPerDollar == nil || r.Comparison.ReachedPerDollar.Low != 0.5 || r.Comparison.ReachedPerDollar.High != 0.5 {
		t.Fatalf("interval %+v", r.Comparison)
	}
	if r.Comparison.ReachedPerMinute.Low != 1 || !reflect.DeepEqual(r.Comparison, e.Results(trs).Comparison) {
		t.Fatal("bootstrap not repeatable or wrong rate")
	}
	if r.Arms[0].USDPerReached == nil || *r.Arms[0].USDPerReached != 2 {
		t.Fatalf("rate %+v", r.Arms[0])
	}
}

// Evaluation permissions retain every live denial and permit no shell prefix that changes momentum's environment.
func TestTrialPermissionsKeepLiveDenialsAndProgramWords(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		live := e.Runner.permissions(StepInvestigate, store.Case{}, tr.Run)
		raw, err := os.ReadFile(filepath.Join(tr.Run.Dir, "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var settings struct{ Permissions Settings }
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatal(err)
		}
		got := settings.Permissions

		if !reflect.DeepEqual(got.Deny, live.Deny) {
			t.Fatalf("denials %v", got.Deny)
		}
		for _, a := range got.Allow {
			if strings.Contains(a, "tacit ") || strings.Contains(a, "evidence file ") || strings.Contains(a, "evidence unit ") || strings.Contains(a, "momentum report ") || strings.Contains(a, "supervise replay ") {
				t.Fatalf("current machine allowed: %s", a)
			}
			if strings.HasPrefix(a, "Bash(") {
				pattern := strings.TrimSuffix(strings.TrimPrefix(a, "Bash("), ")")
				if !strings.HasPrefix(pattern, "momentum ") {
					t.Fatalf("non-program rule: %s", a)
				}
				for _, cmd := range []string{"TZ=UTC LANG=C MOMENTUM_NOW=now momentum evidence events", "MOMENTUM_NOW=now momentum evidence events", "env MOMENTUM_HOME=/other momentum evidence search text", "export MOMENTUM_EVAL=; momentum evidence events"} {
					// Bash rules match from the program word, not a substring of the command.
					match := trialBashRuleMatches(pattern, cmd)
					if match {
						t.Fatalf("%s permits %s", a, cmd)
					}
				}
			}
		}
	}
}

// Trial prompts pass through the same scrubber as live investigations, in both arms.
func TestTrialPromptsAreScrubbed(t *testing.T) {
	old := Scrubber
	t.Cleanup(func() { Scrubber = old })
	Scrubber = []string{script(t, t.TempDir(), "scrub", "sed 's/private-fixture/REDACTED/g'")}
	e, task := evalFixture(t)
	task.CaseTitle = "private-fixture failed"
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		raw, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "args.txt"))
		if tr.Err != "" || strings.Contains(string(raw), "private-fixture") || !strings.Contains(string(raw), "REDACTED") {
			t.Fatalf("%s %s", tr.Err, raw)
		}
	}
}

// A result identifies its data, procedure, budgets and the models the streams actually report.
func TestTrialsRecordTheirProvenanceAndModels(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Run.Model != "actual-model" || tr.JudgeModel != "actual-judge" || tr.Provenance.GoldenHash != e.GoldenHash || tr.Provenance.RunBudget != 4 || tr.Provenance.JudgeBudget != 1 || tr.Provenance.Revision == "" {
			t.Fatalf("%+v", tr)
		}
		system, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "system.md"))
		if tr.Provenance.Systems[tr.Arm] != digest(string(system)) {
			t.Fatal("system hash differs")
		}
		if len(tr.Provenance.Skills) != 0 {
			t.Fatalf("an investigation loads no skill, yet its provenance names %v", tr.Provenance.Skills)
		}
		raw, _ := os.ReadFile(filepath.Join(tr.JudgeDir, "args.txt"))
		if strings.Contains(string(raw), "--fallback-model") {
			t.Fatal("judge fallback enabled")
		}
	}
	if err = e.WriteResults(e.Results(trs)); err != nil {
		t.Fatal(err)
	}
	var result EvaluationResults
	raw, _ := os.ReadFile(filepath.Join(e.Dir, "results.json"))
	if err = json.Unmarshal(raw, &result); err != nil || result.Provenance.GoldenHash != e.GoldenHash {
		t.Fatalf("provenance %v", err)
	}
}

// Golden ids are unique slugs and every task has a verified cause; evaluation ids separate same-second starts.
func TestGoldenAndEvaluationIDsAreUnambiguous(t *testing.T) {
	_, task := evalFixture(t)
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	for _, id := range []string{"../outside", "two words", "Upper"} {
		task.ID = id
		raw, _ := json.Marshal(task)
		write(t, p, string(raw))
		if _, err := ReadGolden(p); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	task.ID = "valid-slug"
	raw, _ := json.Marshal(task)
	write(t, p, string(raw)+"\n"+string(raw))
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("duplicate accepted")
	}
	task.RootCause = ""
	raw, _ = json.Marshal(task)
	write(t, p, string(raw))
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("missing root accepted")
	}
	seen := map[string]bool{}
	for range 100 {
		id := NewEvalID()
		if seen[id] || len(id) < 26 {
			t.Fatalf("id %s", id)
		}
		seen[id] = true
	}
}

// Even an empty evaluation writes results, and a failed destination is returned to its caller.
func TestEmptyEvaluationWritesResultsAndReportsWriteErrors(t *testing.T) {
	e, _ := evalFixture(t)
	trs, err := e.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.WriteResults(e.Results(trs)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(e.Dir, "results.json")); err != nil {
		t.Fatal(err)
	}
	e.Dir = t.TempDir()
	if err = os.Mkdir(filepath.Join(e.Dir, "results.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = e.WriteResults(e.Results(nil)); err == nil {
		t.Fatal("write failure ignored")
	}
}

// A clone neither inherits global hooks nor retains later objects through alternates or kept packs.
func TestHistoricalClonesDropAlternatesAndKeptPacks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	run(t, dir, "git", "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "file"), "before")
	run(t, src, "git", "add", "file")
	run(t, src, "git", "commit", "-qm", "before")
	before := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	write(t, filepath.Join(src, "file"), "after")
	run(t, src, "git", "commit", "-qam", "after")
	after := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	run(t, src, "git", "gc")
	packs, _ := filepath.Glob(filepath.Join(src, ".git", "objects", "pack", "*.pack"))
	for _, p := range packs {
		write(t, strings.TrimSuffix(p, ".pack")+".keep", "")
	}
	shared := filepath.Join(dir, "shared")
	run(t, dir, "git", "clone", "-q", "--shared", src, shared)
	hooks := filepath.Join(dir, "hooks")
	os.Mkdir(hooks, 0700)
	marker := filepath.Join(dir, "hook-ran")
	script(t, hooks, "post-checkout", fmt.Sprintf("touch '%s'", marker))
	config := filepath.Join(dir, "gitconfig")
	write(t, config, "[core]\n hooksPath = "+hooks+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	for i, source := range []string{src, shared} {
		dst := filepath.Join(dir, fmt.Sprint("clone", i))
		if err := cloneAsOf(context.Background(), source, before, dst); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("git", "-C", dst, "cat-file", "-e", after).Run(); err == nil {
			t.Fatal("later object retained")
		}
		if _, err := os.Stat(filepath.Join(dst, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
			t.Fatal("alternates retained")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("global hook ran")
	}
}

// Every trial sees every golden repository; the task's own revision and other default branches end at its clock.
func TestEveryTrialReadsTheSameHistoricalRepositories(t *testing.T) {
	e, task := evalFixture(t)
	dir := t.TempDir()
	end, _ := e.snapshotEnd(context.Background(), task)
	var repos, befores []string
	for i := 0; i < 2; i++ {
		src := filepath.Join(dir, fmt.Sprint("src", i))
		run(t, dir, "git", "init", "-q", "-b", "main", src)
		t.Setenv("GIT_AUTHOR_DATE", end.Add(-time.Hour).Format(time.RFC3339))
		t.Setenv("GIT_COMMITTER_DATE", end.Add(-time.Hour).Format(time.RFC3339))
		write(t, filepath.Join(src, "file"), "before")
		run(t, src, "git", "add", "file")
		run(t, src, "git", "commit", "-qm", "before")
		befores = append(befores, strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD")))
		t.Setenv("GIT_AUTHOR_DATE", end.Add(time.Hour).Format(time.RFC3339))
		t.Setenv("GIT_COMMITTER_DATE", end.Add(time.Hour).Format(time.RFC3339))
		write(t, filepath.Join(src, "file"), "future")
		run(t, src, "git", "commit", "-qam", "future")
		repos = append(repos, src)
	}
	e.Repositories = repos
	task.RepoPath = repos[0]
	task.PreFix = befores[0]
	other := task
	other.ID = "other-task"
	other.RepoPath = ""
	other.PreFix = ""
	trs, err := e.Run(context.Background(), []GoldenTask{task, other})
	if err != nil || len(trs) != 4 {
		t.Fatalf("%d %v", len(trs), err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		for i := range repos {
			dst := filepath.Join(tr.Run.Dir, fmt.Sprint("repo-", i+1))
			head := strings.TrimSpace(run(t, dst, "git", "rev-parse", "HEAD"))
			if head != befores[i] {
				t.Fatalf("%s has %s", dst, head)
			}
		}
		raw, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "args.txt"))
		if strings.Contains(string(raw), "No repository was changed") || !strings.Contains(string(raw), "repo-1, ") || !strings.Contains(string(raw), "repo-2") {
			t.Fatalf("repository hint: %s", raw)
		}
	}
}

// A repository with no commit by a trial's end of store did not exist yet: the trial runs without it.
func TestATrialRunsWithoutARepositoryThatDidNotExistYet(t *testing.T) {
	e, task := evalFixture(t)
	dir := t.TempDir()
	end, _ := e.snapshotEnd(context.Background(), task)
	var repos []string
	for i, at := range []time.Time{end.Add(-time.Hour), end.Add(time.Hour)} {
		src := filepath.Join(dir, fmt.Sprint("src", i))
		run(t, dir, "git", "init", "-q", "-b", "main", src)
		t.Setenv("GIT_AUTHOR_DATE", at.Format(time.RFC3339))
		t.Setenv("GIT_COMMITTER_DATE", at.Format(time.RFC3339))
		write(t, filepath.Join(src, "file"), "first")
		run(t, src, "git", "add", "file")
		run(t, src, "git", "commit", "-qm", "first")
		repos = append(repos, src)
	}
	e.Repositories = repos
	task.RepoPath, task.PreFix = repos[1], strings.TrimSpace(run(t, repos[1], "git", "rev-parse", "HEAD"))
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%d %v", len(trs), err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatalf("a repository born after the end of store failed the trial: %s", tr.Err)
		}
		if _, err := os.Stat(filepath.Join(tr.Run.Dir, "repo-1")); err != nil {
			t.Fatalf("the repository that existed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(tr.Run.Dir, "repo-2")); !os.IsNotExist(err) {
			t.Fatalf("a repository born after the end of store was cloned: %v", err)
		}
	}
}

// The scheduler starts both arms of a task before the next task, then repeats the same k.
func TestEvaluationJobsInterleaveTrialTaskAndArm(t *testing.T) {
	e, a := evalFixture(t)
	b := a
	b.ID = "second-task"
	e.Trials = 2
	log := filepath.Join(t.TempDir(), "order")
	runner := e.Runner.Agent.Runner[0]
	e.Runner.Agent.Runner = []string{script(t, t.TempDir(), "ordered", fmt.Sprintf("printf '%%s\\n' \"$PWD\" >> '%s'\nexec '%s' \"$@\"", log, runner))}
	trs, err := e.Run(context.Background(), []GoldenTask{a, b})
	if err != nil || len(trs) != 8 {
		t.Fatalf("%d %v", len(trs), err)
	}
	raw, _ := os.ReadFile(log)
	got := strings.Fields(string(raw))
	var want []string
	for n := 1; n <= 2; n++ {
		for _, task := range []GoldenTask{a, b} {
			arms := []string{"loop", "native"}
			if n%2 == 0 { // the arms take turns going first
				arms = []string{"native", "loop"}
			}
			for _, arm := range arms {
				tr := e.newTrial(task, n, arm)
				want = append(want, filepath.Join(e.Dir, tr.Key, digest(tr.Key + "\x00" + arm)[:16]))
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// A stopped loop run retains its case findings, including findings absent from its final report.
func TestACutShortLoopKeepsItsCaseFindings(t *testing.T) {
	c := store.Case{Mechanism: "case mechanism", Alternatives: []string{"ruled out"}, NextTest: "case fix", Signatures: []string{"eval/opaque", "root:case"}}
	f := caseFinding(c, Result{Status: store.RunTimeout})
	if f.Mechanism != "case mechanism" || f.Fix != "case fix" || len(f.Alternatives) != 1 {
		t.Fatalf("%+v", f)
	}
	f = caseFinding(c, Result{Report: json.RawMessage(`{"mechanism":"report mechanism","summary":"report summary"}`)})
	if f.Mechanism != "case mechanism" || f.Report != "report summary" {
		t.Fatalf("%+v", f)
	}
}

// The result records each family's procedure, and the system prompts the arms ran, which no family changes.
func TestResultProvenanceNamesEachFamily(t *testing.T) {
	e, task := evalFixture(t)
	task.Family = "sessions"
	tr := e.newTrial(task, 1, "loop")
	r := e.Results([]Trial{tr})
	if r.Procedures["sessions"].Systems["loop"] == "" || r.Provenance.Systems["loop"] != tr.Provenance.Systems["loop"] ||
		r.Provenance.Systems["loop"] != r.Provenance.Systems["native"] || len(r.Provenance.Skills) != 0 {
		t.Fatalf("%+v", r.Provenance)
	}
}
