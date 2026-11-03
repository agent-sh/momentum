package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// With one trial, which arm goes first alternates with the task: over four tasks each arm leads two pairs.
func TestArmOrderAlternatesWithTheTask(t *testing.T) {
	counts := map[string]int{}
	for task := 0; task < 4; task++ {
		counts[armOrder([]string{"loop", "native"}, 1, task)[0]]++
	}
	if counts["loop"] != 2 || counts["native"] != 2 {
		t.Fatalf("first arms over four tasks of one trial: %v", counts)
	}
}

// The judge's fields are capped at 2,000 bytes, not only at 2,000 characters: a field of multibyte text is cut at a
// character boundary within the byte cap.
func TestJudgeFieldsAreCappedInBytes(t *testing.T) {
	text := judgeFinding(finding{Mechanism: strings.Repeat("€", 2000), Fix: "f", Report: "s"})
	body := strings.TrimSuffix(strings.TrimPrefix(text, "```json\n"), "\n```\n")
	var fields struct{ Mechanism string }
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields.Mechanism) > 2000 || len(fields.Mechanism) < 1990 || !utf8.ValidString(fields.Mechanism) {
		t.Fatalf("the mechanism is %d bytes", len(fields.Mechanism))
	}
}

// The bootstrap's interval ends are the nearest-rank 5th and 95th percentiles of its 10,000 draws.
func TestTheBootstrapIntervalIsTheNearestRank(t *testing.T) {
	xs := make([]float64, 10000)
	for i := range xs {
		xs[len(xs)-1-i] = float64(i + 1) // unsorted, ranks 1..10000
	}
	got := nearestRankInterval(xs)
	if got.Low != 500 || got.High != 9500 {
		t.Fatalf("interval %+v, want the 500th and 9,500th of 10,000", got)
	}
}

// A tracked link that escapes an as-of clone or resolves nowhere is removed from its work tree; one that stays inside
// is kept.
func TestEscapingTrackedLinksAreRemoved(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "links")
	historyGit(t, filepath.Dir(repo), time.Time{}, "init", "-q", "-b", "main", repo)
	write(t, filepath.Join(repo, "inside.txt"), "inside\n")
	for name, target := range map[string]string{"absolute": "/etc/hostname", "up": "../outside", "deep": "sub/../../outside", "dangling": "absent", "kept": "inside.txt"} {
		if err := os.Symlink(target, filepath.Join(repo, name)); err != nil {
			t.Skip("no symlinks here")
		}
	}
	end := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	historyGit(t, repo, end.Add(-time.Hour), "add", "-A")
	historyGit(t, repo, end.Add(-time.Hour), "commit", "-qm", "links")
	clone := filepath.Join(t.TempDir(), "clone")
	if made, err := repositoryAsOf(context.Background(), repo, end, clone); err != nil || !made {
		t.Fatalf("clone: %v %v", made, err)
	}
	for _, name := range []string{"absolute", "up", "deep", "dangling"} {
		if _, err := os.Lstat(filepath.Join(clone, name)); !os.IsNotExist(err) {
			t.Errorf("%s stayed: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(clone, "kept")); err != nil {
		t.Errorf("the inside link was removed: %v", err)
	}
}

// A branch's reflog sets its position at the end when it covers the end: a commit dated before the end that the
// branch reached only after it is not the branch as of the end.
func TestTheReflogSetsTheBranchAsOfTheEnd(t *testing.T) {
	end := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	repo := filepath.Join(t.TempDir(), "reflog")
	historyGit(t, filepath.Dir(repo), end.Add(-3*time.Hour), "init", "-q", "-b", "main", repo)
	first := commitAt(t, repo, end.Add(-3*time.Hour), "file", "first\n", "first")
	historyGit(t, repo, end.Add(-2*time.Hour), "checkout", "-q", "-b", "side")
	commitAt(t, repo, end.Add(-time.Hour), "file", "side\n", "side")
	historyGit(t, repo, end.Add(time.Hour), "checkout", "-q", "main")
	historyGit(t, repo, end.Add(time.Hour), "merge", "-q", "--ff-only", "side")
	got, err := branchAsOf(context.Background(), repo, "main", end)
	if err != nil || strings.TrimSpace(string(got)) != first {
		t.Fatalf("main as of the end: %q %v, want %s", got, err, first)
	}
}

// A commit only the reflog holds (the branch moved elsewhere after the end, and nothing reaches the commit) is not in
// a clone, so commit dates set the position instead.
func TestAReflogCommitNoRefReachesFallsBackToDates(t *testing.T) {
	end := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	repo := filepath.Join(t.TempDir(), "rewritten")
	historyGit(t, filepath.Dir(repo), end.Add(-3*time.Hour), "init", "-q", "-b", "main", repo)
	commitAt(t, repo, end.Add(-3*time.Hour), "file", "first\n", "first")
	historyGit(t, repo, end.Add(-2*time.Hour), "checkout", "-q", "--orphan", "other")
	other := commitAt(t, repo, end.Add(-4*time.Hour), "file", "other\n", "other")
	historyGit(t, repo, end.Add(time.Hour), "branch", "-f", "main", "other")
	historyGit(t, repo, end.Add(time.Hour), "checkout", "-q", "main")
	historyGit(t, repo, end.Add(time.Hour), "branch", "-D", "other")
	got, err := branchAsOf(context.Background(), repo, "main", end)
	if err != nil || strings.TrimSpace(string(got)) != other {
		t.Fatalf("main as of the end: %q %v, want %s", got, err, other)
	}
}
