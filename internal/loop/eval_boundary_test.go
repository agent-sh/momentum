package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/agent-sh/momentum/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyGit(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.com", "GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.com")
	if !at.IsZero() {
		env = append(env, "GIT_AUTHOR_DATE="+at.Format(time.RFC3339), "GIT_COMMITTER_DATE="+at.Format(time.RFC3339))
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A later fast-forward cannot admit older-dated commits that were absent from the default branch.
func TestHistoricalCloneUsesTheBranchReflog(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	task.Cutoff = end
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	historyGit(t, dir, end.Add(-2*time.Hour), "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "job"), "reads the lockfile, then takes the lock")
	historyGit(t, src, end.Add(-2*time.Hour), "add", "-A")
	stood := historyGit(t, src, end.Add(-2*time.Hour), "commit", "-qm", "the job")
	stood = historyGit(t, src, time.Time{}, "rev-parse", "HEAD")
	historyGit(t, src, end.Add(-90*time.Minute), "checkout", "-qb", "lock-order")
	write(t, filepath.Join(src, "job_test"), "the job takes its lock before it reads the lockfile")
	historyGit(t, src, end.Add(-30*time.Minute), "add", "-A")
	historyGit(t, src, end.Add(-30*time.Minute), "commit", "-qm", "a failing test: take the lock before reading the lockfile")
	historyGit(t, src, time.Time{}, "checkout", "-q", "main")
	historyGit(t, src, time.Time{}, "merge", "-q", "--ff-only", "lock-order") // today: after the end
	atEnd := historyGit(t, src, time.Time{}, "rev-parse", "main@{"+end.Format(time.RFC3339)+"}")
	e.Repositories = []string{src}
	repos, err := e.cloneRepositories(context.Background(), task, dir)
	if err != nil || len(repos) != 1 {
		t.Fatalf("%v %v", repos, err)
	}
	head := historyGit(t, repos[0], time.Time{}, "rev-parse", "HEAD")
	_, statErr := os.Stat(filepath.Join(repos[0], "job_test"))
	if head != stood || atEnd != stood || !os.IsNotExist(statErr) {
		t.Fatalf("stood %s reflog %s head %s file %v", stood, atEnd, head, statErr)
	}
}

// Skipped repositories leave no gaps in the neutral checkout names.
func TestHistoricalCloneNumbersOnlyRepositoriesMade(t *testing.T) {
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	task.Cutoff = end
	dir := t.TempDir()
	for i, at := range []time.Time{end.Add(time.Hour), end.Add(-time.Hour), end.Add(time.Hour), end.Add(-time.Hour)} {
		src := filepath.Join(dir, fmt.Sprint("source", i))
		historyGit(t, dir, at, "init", "-q", "-b", "main", src)
		commitAt(t, src, at, "job", "contents", "job")
		e.Repositories = append(e.Repositories, src)
	}
	repos, err := e.cloneRepositories(context.Background(), task, dir)
	if err != nil || len(repos) != 2 {
		t.Fatalf("%v %v", repos, err)
	}
	for i, repo := range repos {
		if filepath.Base(repo) != fmt.Sprint("repo-", i+1) {
			t.Fatalf("gap: %v", repos)
		}
	}
}

// A checkout drops escaping tracked links, keeps internal links, and retains both in history.
func TestHistoricalCloneDropsEscapingSymlinks(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "clone")
	historyGit(t, dir, time.Now(), "init", "-q", "-b", "main", src)
	outside := filepath.Join(dir, "outside")
	write(t, outside, "later answer")
	write(t, filepath.Join(src, "inside"), "historical text")
	for name, target := range map[string]string{"absolute": outside, "relative": "../outside", "internal": "inside", "chain": "relative"} {
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	historyGit(t, src, time.Now(), "add", "-A")
	historyGit(t, src, time.Now(), "commit", "-qm", "links")
	head := historyGit(t, src, time.Now(), "rev-parse", "HEAD")
	if err := cloneAsOf(context.Background(), src, head, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"absolute", "relative", "chain"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Fatalf("escaping link %s retained: %v", name, err)
		}
		if historyGit(t, dst, time.Now(), "show", "HEAD:"+name) == "" {
			t.Fatal("history lost")
		}
	}
	raw, err := os.ReadFile(filepath.Join(dst, "internal"))
	if err != nil || string(raw) != "historical text" {
		t.Fatalf("internal link %s %v", raw, err)
	}
}

// Both trials expose only shared evidence commands and neither working directory names its arm.
func TestTrialCommandsAndDirectoriesAreNeutral(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		rel, err := filepath.Rel(e.Dir, tr.Run.Dir)
		if err != nil || strings.Contains(rel, "loop") || strings.Contains(rel, "native") {
			t.Fatalf("directory identifies arm: %s %v", rel, err)
		}
		raw, err := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if err != nil {
			t.Fatal(err)
		}
		prompt := string(raw)
		start := strings.Index(prompt, "\n\n## Commands available in this trial")
		if start < 0 {
			t.Fatal("reference absent")
		}
		shared := prompt[start:]
		for _, forbidden := range []string{"loop arm", "native arm", "momentum case", "supervise replay", "source, actor and kind"} {
			if strings.Contains(shared, forbidden) {
				t.Errorf("shared reference exposes %q", forbidden)
			}
		}
		for _, required := range []string{"source and kind", "transcripts", "incidents", "mistakes", "sessions", "supervise", "radar", "capacity"} {
			if !strings.Contains(shared, required) {
				t.Errorf("reference omits %q", required)
			}
		}
		if strings.Contains(prompt, "momentum case") != (tr.Arm == "loop") {
			t.Fatal("case reference belongs only in loop prompt")
		}
	}
}

// Replay is absent from both trials' actual settings and explicitly unavailable in the loop procedure.
func TestTrialReplayIsUnavailableInSettingsAndPrompts(t *testing.T) {
	e, task := evalFixture(t)
	task.Family = "sessions"
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		raw, err := os.ReadFile(filepath.Join(tr.Run.Dir, "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var settings struct{ Permissions Settings }
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatal(err)
		}
		for _, rule := range settings.Permissions.Allow {
			if strings.Contains(rule, "supervise replay") {
				t.Fatal("replay allowed")
			}
		}
		raw, err = os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "supervise replay") {
			t.Fatal("prompt offers replay")
		}
		if tr.Arm == "loop" {
			raw, err = os.ReadFile(filepath.Join(tr.Run.Dir, "system.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "momentum supervise replay are not available") {
				t.Fatal("replay not marked unavailable")
			}
		}
	}
}

// Without a final report, both arms give the judge only mechanism, alternatives and fix.
func TestUnfinishedTrialsUseTheSameJudgeFields(t *testing.T) {
	e, task := evalFixture(t)
	e.Arms = []string{"native"}
	e.Runner.Agent.Runner = []string{script(t, t.TempDir(), "unfinished", `cat > findings.md <<'NOTES'
extra introduction
## Mechanism
cause
## Alternatives ruled out
alternative
## Fix
fix
## Summary
extra summary
NOTES
`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 1 {
		t.Fatalf("%v %v", trs, err)
	}
	if trs[0].Err != "" {
		t.Fatal(trs[0].Err)
	}
	want := judgePrompt(task, caseFinding(store.Case{Mechanism: "cause", Alternatives: []string{"alternative"}, NextTest: "fix"}, Result{}))
	if trs[0].JudgePromptHash != digest(want) {
		t.Fatal("unfinished native finding differs from the same loop case")
	}
}

// The investigation checks alternatives and reserves room for the report that prevents repeated limit stops.
func TestInvestigationKeepsRoomForItsReport(t *testing.T) {
	e, _ := evalFixture(t)
	system, err := e.system("loop", "failures")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Run the checks rather than stopping at the first explanation that fits", "Stop with room for the report", "budget or turn limit reports nothing", "two such stops pass the case over"} {
		if !strings.Contains(system, text) {
			t.Fatalf("procedure omits %q", text)
		}
	}
}

// The historical-evidence contract names place references, replay exclusion, derivation and interval limits.
func TestEvaluationContractStatesHistoricalLimits(t *testing.T) {
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first record after the scan's place", "record's own timestamp", "Sessions tasks run without replay", "reflog reaches back", "newest commit dated before", "derivation of their last read", "covers less than its nominal 90%", "reads the interval with its task count"} {
		if !strings.Contains(string(raw), text) {
			t.Errorf("contract omits %q", text)
		}
	}
}

// An expired reflog falls back to the latest commit dated strictly before the end.
func TestHistoricalCloneFallsBackWhenTheReflogDoesNotReachTheEnd(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	task.Cutoff = end
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	historyGit(t, dir, time.Now(), "init", "-q", "-b", "main", src)
	want := commitAt(t, src, end.Add(-time.Hour), "job", "before", "before")
	commitAt(t, src, end, "job", "at end", "at end")
	historyGit(t, src, time.Now(), "reflog", "expire", "--expire=now", "--all")
	e.Repositories = []string{src}
	repos, err := e.cloneRepositories(context.Background(), task, dir)
	if err != nil || len(repos) != 1 {
		t.Fatalf("%v %v", repos, err)
	}
	if got := historyGit(t, repos[0], time.Now(), "rev-parse", "HEAD"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
