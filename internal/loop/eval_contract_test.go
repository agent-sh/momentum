package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A delayed pre-fix test and the fix itself are absent from every historical checkout, including its objects.
func TestRepositoryHistoryEndsBeforeTheInvestigation(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	var heads, later []string
	for i := 0; i < 2; i++ {
		src := filepath.Join(base, fmt.Sprint("source", i))
		run(t, base, "git", "init", "-q", "-b", "main", src)
		heads = append(heads, commitAt(t, src, end.Add(-time.Hour), "job", "before", "before"))
		later = append(later, commitAt(t, src, end.Add(48*time.Hour), "job_test", "take the lock before reading", "a discriminating test"))
		commitAt(t, src, end.Add(72*time.Hour), "job", "take lock first", "fix")
		e.Repositories = append(e.Repositories, src)
	}
	task.RepoPath, task.PreFix = e.Repositories[0], later[0]
	task.Cutoff = end.Add(72*time.Hour - time.Minute)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		for i, head := range heads {
			dst := filepath.Join(tr.Run.Dir+"-repos", fmt.Sprint("repo-", i+1))
			if got := strings.TrimSpace(run(t, dst, "git", "rev-parse", "HEAD")); got != head {
				t.Fatalf("head %s want %s", got, head)
			}
			if _, err := os.Stat(filepath.Join(dst, "job_test")); !os.IsNotExist(err) {
				t.Fatal("later test retained")
			}
			if exec.Command("git", "-C", dst, "cat-file", "-e", later[i]).Run() == nil {
				t.Fatal("later object retained")
			}
		}
	}
}

// A fetched but unpulled default branch selects its local head, which a no-local clone carries.
func TestHistoricalCloneUsesTheLocalDefaultBranch(t *testing.T) {
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	for _, branch := range []string{"main", "master", "trunk"} {
		t.Run(branch, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source")
			run(t, dir, "git", "init", "-q", "-b", branch, src)
			local := commitAt(t, src, end.Add(-3*time.Hour), "job", "local", "local")
			run(t, src, "git", "checkout", "-qb", "upstream")
			remote := commitAt(t, src, end.Add(-2*time.Hour), "job", "upstream", "fetched but not pulled")
			run(t, src, "git", "checkout", "-q", branch)
			run(t, src, "git", "update-ref", "refs/remotes/origin/"+branch, remote)
			run(t, src, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
			run(t, src, "git", "branch", "-D", "upstream")
			repos, err := cloneRepositories(context.Background(), []string{src}, end, filepath.Join(dir, "repos"))
			if err != nil || len(repos) != 1 {
				t.Fatalf("%v %v", repos, err)
			}
			if got := strings.TrimSpace(run(t, repos[0], "git", "rev-parse", "HEAD")); got != local {
				t.Fatalf("head %s want local %s", got, local)
			}
			if exec.Command("git", "-C", repos[0], "cat-file", "-e", remote).Run() == nil {
				t.Fatal("remote-only object retained")
			}
		})
	}
}

// Every command a trial's door runs for an arm is explained in its prompt, both prompts end with the same shared
// reference, and the arms have one system prompt: an investigation keeps no case, so no case command is among them.
func TestTrialPromptsEndWithTheSameCommandReference(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	var blocks, systems []string
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		raw, err := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if err != nil {
			t.Fatal(err)
		}
		prompt := string(raw)
		start := strings.Index(prompt, "\n\n## Commands available in this trial")
		if start < 0 {
			t.Fatal("command reference absent")
		}
		blocks = append(blocks, prompt[start:])
		for _, command := range []string{"momentum evidence git", "Your working directory is ."} {
			if !strings.Contains(prompt, command) {
				t.Fatalf("missing %s", command)
			}
		}
		for _, cmd := range trialReads {
			if !strings.Contains(prompt[start:], strings.Join(cmd, " ")) {
				t.Errorf("%s: the door runs %q and the reference does not explain it", tr.Arm, strings.Join(cmd, " "))
			}
			if cmd[0] == "momentum" && cmd[1] == "case" {
				t.Fatalf("%s: an investigation keeps no case, yet %s is allowed", tr.Arm, strings.Join(cmd, " "))
			}
		}
		raw, err = os.ReadFile(filepath.Join(HostDir(tr.Run.Dir), "primary", "system.md"))
		if err != nil {
			t.Fatal(err)
		}
		systems = append(systems, string(raw))
	}
	if len(blocks) != 2 || blocks[0] != blocks[1] {
		t.Fatalf("references differ: %v", blocks)
	}
	if len(systems) != 2 || systems[0] != systems[1] {
		t.Fatalf("the arms' system prompts differ: %q", systems)
	}
}

// The loop's final case and the native report yield identical judge text without a cause slug or an arm in its path.
func TestJudgeInputsAndDirectoriesDoNotIdentifyAnArm(t *testing.T) {
	e, task := evalFixture(t)
	c := store.Case{Mechanism: "the read precedes the lock", Alternatives: []string{"disk latency ruled out"}, NextTest: "take lock first", Signatures: []string{"root:lock-order"}}
	l := loopFinding(c, reportedFinding(t, Result{Report: json.RawMessage(`{"mechanism":"discarded report hypothesis","summary":"found it","root":"root:lock-order"}`)}))
	n := nativeFinding(Result{Report: json.RawMessage(`{"mechanism":"the read precedes the lock","alternatives":["disk latency ruled out"],"fix":"take lock first","summary":"found it","root_cause":"a different shape"}`)})
	if judgePrompt(task, l) != judgePrompt(task, n) {
		t.Fatalf("loop %s native %s", judgeFinding(l), judgeFinding(n))
	}
	if strings.Contains(judgeFinding(l), "RootCause") || strings.Contains(judgeFinding(l), "discarded") {
		t.Fatal("judge got a second cause or hypothesis")
	}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tr := range trs {
		rel, err := filepath.Rel(e.Dir, tr.JudgeDir)
		if err != nil || filepath.Dir(rel) != "judge" || seen[rel] || strings.Contains(rel, "loop") || strings.Contains(rel, "native") {
			t.Fatalf("judge directory %q: %v", rel, err)
		}
		seen[rel] = true
		raw, err := os.ReadFile(filepath.Join(tr.JudgeDir, "args.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), tr.Run.Dir) {
			t.Fatal("trial path in judge input")
		}
	}
}

// A budget with room for three arms runs one pair, and unmatched trials contribute no comparison or error.
func TestEvaluationBudgetKeepsBothArmsTogether(t *testing.T) {
	e, a := evalFixture(t)
	b := a
	b.ID = "another-task"
	e.Budget = 15
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "costly", modelResult(`{"type":"result","subtype":"success","total_cost_usd":4,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	e.Judge.Runner = []string{script(t, harness, "judge", modelResult(`{"type":"result","subtype":"success","total_cost_usd":1,"structured_output":{"verdict":"reached","facts":[],"reason":"r"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 4 {
		t.Fatalf("trials %d", len(trs))
	}
	for _, tr := range trs {
		if tr.Task == b.ID && (tr.Skipped == "" || tr.Err != "" || !tr.Run.Started.IsZero()) {
			t.Fatalf("split pair %+v", tr)
		}
	}
	r := e.Results(trs)
	for _, arm := range r.Arms {
		if arm.Ran != 1 || arm.Skipped != 1 || arm.Errors != 0 || arm.PassAtK != 1 {
			t.Fatalf("summary %+v", arm)
		}
	}
	// A stopped scheduler can leave only one arm on disk. Neither that verdict nor its error is compared.
	trs = trs[:0]
	at := time.Now()
	trs = append(trs, Trial{Task: "unpaired", N: 1, Arm: "loop", Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 4}, Grade: Grade{Verdict: "reached"}}, Trial{Task: "unpaired", N: 1, Arm: "native", Err: "stopped before start"})
	r = e.Results(trs)
	if len(r.Comparison.Tasks) != 0 || r.Comparison.ReachedPerDollar != nil {
		t.Fatalf("unpaired comparison %+v", r.Comparison)
	}
	for _, arm := range r.Arms {
		if arm.Skipped != 1 || arm.Errors != 0 || arm.Graded != 0 {
			t.Fatalf("unpaired summary %+v", arm)
		}
	}
	if r.Arms[0].USD != 4 {
		t.Fatal("unpaired cost lost")
	}
}

// Git's inherited variables cannot redirect a historical checkout into a different repository or work tree.
func TestHistoricalCloneClearsGitEnvironment(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	run(t, dir, "git", "init", "-q", "-b", "main", src)
	head := commitAt(t, src, time.Now().Add(-time.Hour), "job", "before", "before")
	t.Setenv("GIT_DIR", filepath.Join(dir, "absent.git"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(dir, "wrong-tree"))
	dst := filepath.Join(dir, "clone")
	if err := cloneAsOf(context.Background(), src, head, dst); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dst, "job"))
	if err != nil || string(raw) != "before" {
		t.Fatalf("checkout %s %v", raw, err)
	}
}

// Any bootstrap draw with a zero denominator leaves that interval undefined, including a mixture of tasks.
func TestBootstrapLeavesZeroDenominatorsUndefined(t *testing.T) {
	pairs := map[string]map[string]Summary{
		"positive": {"loop": {Reached: 1, USD: 1, TotalMinutes: 1}, "native": {Reached: 1, USD: 1, TotalMinutes: 1}},
		"zero":     {"loop": {Reached: 1}, "native": {Reached: 1, USD: 1, TotalMinutes: 1}},
	}
	c := compareTasks(pairs)
	if c.ReachedPerDollar != nil || c.ReachedPerMinute != nil {
		t.Fatalf("undefined intervals %+v", c)
	}
}

// Repeating a trial refuses its existing directory and preserves the files already in it.
func TestTrialNeverReusesItsDirectory(t *testing.T) {
	e, task := evalFixture(t)
	if err := e.pin(); err != nil {
		t.Fatal(err)
	}
	pair, done := e.makePair(context.Background(), task, 1, nil)
	defer done()
	first := e.trial(context.Background(), pair, task, 1, "native", 1)
	if first.Err != "" {
		t.Fatal(first.Err)
	}
	marker := filepath.Join(first.Run.Dir, "keep")
	write(t, marker, "unchanged")
	saved, err := os.ReadFile(filepath.Join(first.Run.Dir, "trial.json"))
	if err != nil {
		t.Fatal(err)
	}
	next := e.trial(context.Background(), pair, task, 1, "native", 1)
	if next.Err != "trial directory already exists" || !next.Run.Started.IsZero() {
		t.Fatalf("reused: %+v", next)
	}
	got, _ := os.ReadFile(filepath.Join(first.Run.Dir, "trial.json"))
	kept, _ := os.ReadFile(marker)
	if string(got) != string(saved) || string(kept) != "unchanged" {
		t.Fatal("existing trial changed")
	}
}

// TMPDIR reaches what a run's door runs unchanged, so its temporary files stay in the scratch directory chosen.
func TestRunEnvironmentKeepsItsTemporaryDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if !strings.Contains(strings.Join(CleanEnv(nil), "\n"), "TMPDIR="+dir+"\n") && !strings.HasSuffix(strings.Join(CleanEnv(nil), "\n"), "TMPDIR="+dir) {
		t.Fatal("TMPDIR omitted")
	}
}

// Saved results bind each exact investigation and judge prompt to the golden file hash.
func TestResultsRecordEveryTrialPromptHash(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.WriteResults(e.Results(trs)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.Dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result EvaluationResults
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	for _, tr := range result.Trials {
		for _, pair := range []struct{ dir, hash string }{{tr.Run.Dir, tr.PromptHash}, {tr.JudgeDir, tr.JudgePromptHash}} {
			prompt, err := os.ReadFile(filepath.Join(pair.dir, "prompt.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if pair.hash != digest(string(prompt)) {
				t.Fatalf("prompt hash %q", pair.hash)
			}
		}
		if tr.Provenance.GoldenHash != e.GoldenHash {
			t.Fatal("golden hash lost")
		}
	}
}

// Preparation failures in both arms remain errors, rather than disappearing as a budget skip.
func TestEvaluationPreparationFailuresRemainErrors(t *testing.T) {
	e, task := evalFixture(t)
	e.Source = filepath.Join(t.TempDir(), "missing.db")
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	result := e.Results(trials)
	for _, arm := range result.Arms {
		if arm.Errors != 1 || arm.Skipped != 0 {
			t.Fatalf("preparation failure hidden: %+v", arm)
		}
	}
}

// An unmatched run remains in accounting but cannot inflate either arm's cost per cause or bootstrap rate.
func TestComparisonCostsUseOnlyMatchedTrials(t *testing.T) {
	e, _ := evalFixture(t)
	e.Trials = 2
	at := time.Now()
	trials := []Trial{
		{Task: "task", N: 1, Arm: "loop", Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 1}, Grade: Grade{Verdict: "reached"}},
		{Task: "task", N: 1, Arm: "native", Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 1}, Grade: Grade{Verdict: "reached"}},
		{Task: "task", N: 2, Arm: "loop", Run: store.LoopRun{Started: at, Ended: at.Add(10 * time.Minute), USD: 10}, Grade: Grade{Verdict: "reached"}},
		{Task: "task", N: 2, Arm: "native", Skipped: "stopped"},
	}
	r := e.Results(trials)
	for _, arm := range r.Arms {
		if arm.USDPerReached == nil || *arm.USDPerReached != 1 || arm.MinutesPerReached == nil || *arm.MinutesPerReached != 1 || arm.Reached != 1 || arm.Skipped != 1 || arm.PassAllK != 0 {
			t.Fatalf("unmatched rates: %+v", arm)
		}
	}
	if r.Arms[0].USD != 11 || r.Arms[0].Ran != 2 {
		t.Fatal("accounting lost the unmatched run")
	}
	if r.Comparison.ReachedPerDollar == nil || r.Comparison.ReachedPerDollar.Low != 0 || r.Comparison.ReachedPerDollar.High != 0 {
		t.Fatalf("unmatched interval: %+v", r.Comparison)
	}
}
