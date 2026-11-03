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

// evalFixture has no host source, credential or model. Preparation tests need
// no sandbox. Agent tests explicitly opt into the real launcher and fake relay.
func evalFixture(t *testing.T) (*Eval, GoldenTask) {
	t.Helper()
	inputScrubber(t, `sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/password=marker-7f3a/password=[masked]/g'`)
	source := testStore(t)
	at := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	ev := store.Event{Source: store.Transcripts, SourceKey: "symptom", TS: at, Session: "fixture",
		Actor: store.ActorTool, Kind: "tool_result", Summary: "marker-7f3a worker failed"}
	if _, err := source.Put(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	task := GoldenTask{ID: "worker-reads-before-lock", Family: "failures", CaseTitle: "marker-7f3a worker failed",
		Cutoff: at.Add(time.Hour), RootCause: "marker-7f3a verified cause", KeyFacts: []string{"marker-7f3a fact"}}
	task.Symptom.Query.EventIDs = []string{ev.ID()}
	r := &Runner{Store: testStore(t), Budgets: Budgets{Run: 4, RunTime: time.Minute, MaxTurns: 10},
		Constraints: []string{"Build no change."}, Inputs: InputOptions{Home: t.TempDir(),
			Momentum: script(t, t.TempDir(), "records", "printf 'marker-7f3a record\\n'")}}
	return &Eval{Runner: r, Source: source.Path(), Dir: t.TempDir(), ID: "fixture-evaluation", Trials: 1,
		Budget: 50, Parallel: 1, GoldenHash: digest("fixture golden file")}, task
}

func gitRun(t *testing.T, dir, program string, args ...string) string {
	t.Helper()
	if program != "git" {
		t.Fatal("historical fixtures run only git")
	}
	return inputTestGit(t, dir, nil, args...)
}

func historyGit(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	var dates []string
	if !at.IsZero() {
		dates = []string{"GIT_AUTHOR_DATE=" + at.Format(time.RFC3339), "GIT_COMMITTER_DATE=" + at.Format(time.RFC3339)}
	}
	return inputTestGit(t, dir, dates, args...)
}

func commitAt(t *testing.T, dir string, at time.Time, file, contents, message string) string {
	t.Helper()
	write(t, filepath.Join(dir, file), contents)
	historyGit(t, dir, at, "add", "-A")
	historyGit(t, dir, at, "commit", "-qm", message)
	return historyGit(t, dir, time.Time{}, "rev-parse", "HEAD")
}

func evalRepository(t *testing.T, end time.Time, name string) (string, string, string) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, name)
	historyGit(t, base, end.Add(-time.Hour), "init", "-q", "-b", "main", repo)
	before := commitAt(t, repo, end.Add(-time.Hour), "file", "marker-7f3a before\n", "before")
	after := commitAt(t, repo, end.Add(time.Hour), "file", "marker-7f3a later answer\n", "after")
	return repo, before, after
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func absentObject(t *testing.T, repo, object string) {
	t.Helper()
	if exec.Command("git", "-C", repo, "cat-file", "-e", object).Run() == nil {
		t.Fatal("historical clone kept a later object")
	}
}

func sameTexts(t *testing.T, a, b string) {
	t.Helper()
	if strings.TrimSpace(a) != strings.TrimSpace(b) {
		t.Fatal("the two captures differ")
	}
}
