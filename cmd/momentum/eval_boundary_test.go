package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/agent-sh/momentum/internal/loop"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func datedGit(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+at.Format(time.RFC3339), "GIT_COMMITTER_DATE="+at.Format(time.RFC3339),
		"GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.com", "GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A scan place shows the next record only when that record predates the evaluation end.
func TestEvidenceShowWithholdsRecordsAtOrAfterTheEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	ctx := context.Background()
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_EVAL", "fixture")
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture-run")
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	before := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":"the job failed again; stopping"}}`+"\n", end.Add(-3*time.Minute).Format(time.RFC3339Nano))
	after := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"it reads the lockfile before it takes the lock, fix that"}}`+"\n", end.Add(20*time.Minute).Format(time.RFC3339Nano))
	if err := os.WriteFile(transcript, []byte(before+after), 0o600); err != nil {
		t.Fatal(err)
	}
	place := fmt.Sprintf("%s@%d", transcript, len(before)) // what a scan that read the whole file records
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	evs := []store.Event{
		{Source: store.Supervise, SourceKey: "s1@1#promise#hold#0", TS: end.Add(-2 * time.Minute), Session: "s1", Harness: "claude", Actor: store.ActorDetector,
			Kind: "hold", Summary: "promise: hold: the owner typed into it within the owner window", Ref: place},
		{Source: store.Sessions, SourceKey: "s1@2", TS: end.Add(-time.Minute), Session: "s1", Harness: "claude", Actor: store.ActorHarness,
			Kind: "phase", Summary: "s1: working -> idle", Ref: place},
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := store.AsOf(ctx, src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct{ line, reason string }{
		{after, "at or after"},
		{strings.ReplaceAll(after, end.Add(20*time.Minute).Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)), "at or after"},
		{`{"message":"undated answer"}` + "\n", "timestamp"},
		{`{"timestamp":"bad","message":"undated answer"}` + "\n", "timestamp"},
	} {
		if err := os.WriteFile(transcript, []byte(before+record.line), 0600); err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			code, out, diag := momentum("evidence", "show", ev.ID())
			if code != 0 || strings.Contains(out, "lockfile") || strings.Contains(out, "undated answer") || !strings.Contains(out, record.reason) {
				t.Fatalf("%s: %d %s %s", ev.Source, code, out, diag)
			}
		}
	}
	if err := os.WriteFile(transcript, []byte(before+before), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("evidence", "show", evs[0].ID())
	if code != 0 || !strings.Contains(out, "the job failed again") {
		t.Fatalf("before: %d %s %s", code, out, diag)
	}
	t.Setenv("MOMENTUM_NOW", "")
	if err := os.WriteFile(transcript, []byte(before+after), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag = momentum("evidence", "show", evs[1].ID())
	if code != 0 || !strings.Contains(out, "lockfile") {
		t.Fatalf("live place: %d %s %s", code, out, diag)
	}

}

// Replay refuses evaluation requests before help can disclose the compiled rules' waits.
func TestEvaluationRefusesReplayBeforeParsingFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	t.Setenv("MOMENTUM_EVAL", "fixture")
	for _, args := range [][]string{{"supervise", "replay"}, {"supervise", "replay", "-h"}} {
		code, out, diag := momentum(args...)
		if code != 2 || !strings.Contains(diag, "not available in an evaluation") || strings.Contains(out+diag, "stalled-wait") {
			t.Fatalf("%d %s %s", code, out, diag)
		}
	}
}

// Search accepts every stored kind, including kinds outside transcript events.
func TestEvidenceSearchAcceptsStoredKinds(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: "incidents", SourceKey: "seen", TS: at, Actor: store.ActorDetector, Kind: "incident-seen", Summary: "backup volume not mounted"},
		store.Event{Source: "mistakes", SourceKey: "result", TS: at, Actor: store.ActorTool, Kind: "result", Summary: "lockfile busy"})
	for _, args := range [][]string{
		{"evidence", "search", "not mounted", "--kind", "incident-seen"},
		{"evidence", "search", "lockfile busy", "--kind", "result"},
		{"evidence", "events", "--kind", "incident-seen", "--since", "1d"},
		{"evidence", "search", "not mounted", "--source", "incidents"},
	} {
		code, out, diag := momentum(args...)

		if code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}

// Neither an escaping link nor an enclosing repository supplies a trial's Git evidence.
func TestEvidenceGitStaysInTheResolvedTrial(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	base := t.TempDir()
	other := filepath.Join(base, "other") // another repository on the machine, as it is now
	datedGit(t, base, end, "init", "-q", "-b", "main", other)
	os.WriteFile(filepath.Join(other, "job"), []byte("before"), 0o600)
	datedGit(t, other, end.Add(-time.Hour), "add", "-A")
	datedGit(t, other, end.Add(-time.Hour), "commit", "-qm", "the job, before the end")
	os.WriteFile(filepath.Join(other, "job"), []byte("after"), 0o600)
	datedGit(t, other, end.Add(72*time.Hour), "commit", "-qam", "fix: take the lock before reading the lockfile")
	src := filepath.Join(base, "src") // a golden repository that tracks a symlink to it
	datedGit(t, base, end, "init", "-q", "-b", "main", src)
	if err := os.Symlink(other, filepath.Join(src, "peer")); err != nil {
		t.Fatal(err)
	}
	datedGit(t, src, end.Add(-2*time.Hour), "add", "-A")
	datedGit(t, src, end.Add(-2*time.Hour), "commit", "-qm", "link the peer checkout")
	trial := filepath.Join(base, "eval", "key", "loop")
	os.MkdirAll(trial, 0o700)
	datedGit(t, base, end, "clone", "-q", "--no-local", src, filepath.Join(trial, "repo-1"))
	t.Setenv("MOMENTUM_EVAL", "fixture")
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture-run")
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_LOOP_DIR", trial)
	code, out, diag := momentum("evidence", "git", filepath.Join(trial, "repo-1", "peer"), "log", "--format=%ad %s", "--date=short")
	if code != 2 || out != "" {
		t.Fatalf("escaping link: %d %s %s", code, out, diag)
	}
	// a trial directory inside an enclosing work tree
	encl := filepath.Join(base, "encl")
	datedGit(t, base, end, "init", "-q", "-b", "main", encl)
	os.WriteFile(filepath.Join(encl, "notes"), []byte("x"), 0o600)
	datedGit(t, encl, end.Add(48*time.Hour), "add", "-A")
	datedGit(t, encl, end.Add(48*time.Hour), "commit", "-qm", "a commit two days after the end")
	inner := filepath.Join(encl, "eval", "key", "native")
	os.MkdirAll(inner, 0o700)
	t.Setenv("MOMENTUM_LOOP_DIR", inner)
	code, out, diag = momentum("evidence", "git", inner, "log", "--format=%ad %s", "--date=short")
	if code != 2 || out != "" {
		t.Fatalf("enclosing repo: %d %s %s", code, out, diag)
	}
}

// An unknown source is an error naming all sources present, even outside the search window.
func TestEvidenceSearchRefusesAbsentSources(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	fakeScrubber(t)
	putEvents(t, store.Event{Source: "incidents", SourceKey: "old", TS: time.Now().Add(-90 * 24 * time.Hour), Actor: store.ActorDetector, Kind: "incident-seen", Summary: "volume not mounted"}, store.Event{Source: "mistakes", SourceKey: "result", TS: time.Now(), Actor: store.ActorTool, Kind: "result", Summary: "lockfile busy"})
	code, out, diag := momentum("evidence", "search", "not mounted", "--source", "incident")
	if code != 2 || out != "" || !strings.Contains(diag, "incidents") || !strings.Contains(diag, "mistakes") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	code, out, diag = momentum("evidence", "search", "not mounted", "--source", "incidents")
	if code != 0 || out != "" {
		t.Fatalf("existing source outside window: %d %s %s", code, out, diag)
	}
}

// Resolving a link within the bound still permits reading its repository.
func TestEvidenceGitReadsAnInternalLink(t *testing.T) {
	fakeScrubber(t)
	trial := t.TempDir()
	repo := filepath.Join(trial, "repo")
	datedGit(t, trial, time.Now(), "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "job"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	datedGit(t, repo, time.Now(), "add", "-A")
	datedGit(t, repo, time.Now(), "commit", "-qm", "inside")
	if err := os.Symlink("repo", filepath.Join(trial, "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOMENTUM_EVAL", "fixture")
	t.Setenv("MOMENTUM_LOOP_DIR", trial)
	code, out, diag := momentum("evidence", "git", filepath.Join(trial, "link"), "log", "--oneline")
	if code != 0 || !strings.Contains(out, "inside") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

// Evaluation help describes the single results object emitted by the JSON mode.
func TestEvaluationHelpNamesOneResultsObject(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	_, _, diag := momentum("loop", "eval", "--help")
	if !strings.Contains(diag, "one results object") || strings.Contains(diag, "per trial") {
		t.Fatal(diag)
	}
}

// The printed interval names only tasks with a matched comparison, including a zero-task result.
func TestEvaluationIntervalNamesItsComparedTaskCount(t *testing.T) {
	for _, tasks := range [][]loop.TaskComparison{nil, {{Task: "a", MoreOften: "tie"}, {Task: "b", MoreOften: "not compared"}}} {
		var out bytes.Buffer
		printEvaluation(&out, loop.EvaluationResults{Comparison: loop.Comparison{Tasks: tasks}})
		n := 0
		for _, task := range tasks {
			if task.MoreOften != "not compared" {
				n++
			}
		}
		if !strings.Contains(out.String(), fmt.Sprintf("interval (%d compared tasks;", n)) {
			t.Fatal(out.String())
		}
	}
}

// A live work-tree path may not follow a link beyond the repository, even when its spelling is relative.
func TestLiveGitPathsStayInTheResolvedRepository(t *testing.T) {
	fakeScrubber(t)
	repo, other := t.TempDir(), t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(other, "later"), []byte("answer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(repo, "peer")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "link")
	code, out, diag := momentum("evidence", "git", repo, "log", "--", "peer/later")
	if code != 2 || out != "" || !strings.Contains(diag, "resolves outside") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

// A Git directory inside the trial cannot redirect the work tree beyond it, even below the discovery ceiling.
func TestEvidenceGitChecksTheOpenedWorkTree(t *testing.T) {
	fakeScrubber(t)
	trial := t.TempDir()
	other := filepath.Dir(trial)
	repo := filepath.Join(trial, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "--allow-empty", "-qm", "outside work tree")
	gitIn(t, repo, "config", "core.worktree", other)
	t.Setenv("MOMENTUM_EVAL", "fixture")
	t.Setenv("MOMENTUM_LOOP_DIR", trial)
	code, out, diag := momentum("evidence", "git", repo, "log", "--oneline")
	if code != 2 || out != "" {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

// Git discovery stops at the resolved trial parent's boundary, regardless of an inherited ceiling.
func TestEvaluationGitSetsItsDiscoveryCeiling(t *testing.T) {
	trial := t.TempDir()
	alias := filepath.Join(t.TempDir(), "trial")
	if err := os.Symlink(trial, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOMENTUM_EVAL", "fixture")
	t.Setenv("MOMENTUM_LOOP_DIR", alias)
	t.Setenv("GIT_CEILING_DIRECTORIES", "/untrusted")
	var ceilings []string
	for _, item := range gitEnv() {
		if strings.HasPrefix(item, "GIT_CEILING_DIRECTORIES=") {
			ceilings = append(ceilings, item)
		}
	}
	if len(ceilings) != 1 || ceilings[0] != "GIT_CEILING_DIRECTORIES="+filepath.Dir(trial) {
		t.Fatalf("ceilings %v", ceilings)
	}
}
