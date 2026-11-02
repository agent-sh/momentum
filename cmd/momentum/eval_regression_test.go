package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/agent-sh/momentum/internal/loop"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// Search consumes flag values before its one phrase, validates filters, and keeps session matches newest first.
func TestEvidenceSearchParsesFlagsAnywhereAndOrdersSessionMatches(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "old", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "queue stalled older"},
		store.Event{Source: store.Transcripts, SourceKey: "new", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "queue stalled newer"})
	for _, args := range [][]string{{"--actor", "owner", "queue stalled"}, {"--kind=owner_text", "queue stalled", "--actor", "owner"}, {"queue stalled", "--session", "s1", "--limit", "1"}} {
		code, out, diag := momentum(append([]string{"evidence", "search"}, args...)...)
		if code != 0 || !strings.Contains(out, "newer") || strings.Contains(strings.Join(args, " "), "--limit") && strings.Contains(out, "older") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
	for _, args := range [][]string{{"queue stalled", "extra"}, {"--actor", "typo", "queue stalled"}, {"--kind", "typo", "queue stalled"}, {"--actor", "owner"}} {
		code, _, _ := momentum(append([]string{"evidence", "search"}, args...)...)
		if code != 2 {
			t.Fatalf("accepted %v", args)
		}
	}
}

// Default evidence windows follow a snapshot forty days back. Replay cannot read today's Hermes database or later requests.
func TestEvidenceCommandsUseTheSnapshotClock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_EVAL", "fixture")
	hermes := t.TempDir()
	t.Setenv("HERMES_HOME", hermes)
	if err := os.WriteFile(filepath.Join(hermes, "state.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	before := end.Add(-2 * time.Hour)
	first := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"queue stalled"}}`+"\n", before.Format(time.RFC3339Nano))
	later := fmt.Sprintf(`{"type":"api-request","timestamp":%q}`+"\n", end.Add(time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(transcript, []byte(first+later), 0600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Put(context.Background(), []store.Event{
		{Source: store.Transcripts, SourceKey: "owner", TS: before, Session: "fixture", Actor: store.ActorOwner, Kind: "owner_text", Harness: "claude", Summary: "queue stalled", Ref: transcript + "@0#0"},
		{Source: store.Transcripts, SourceKey: "future", TS: end.Add(time.Hour), Session: "fixture", Actor: store.ActorAgent, Kind: "agent_text", Harness: "claude", Summary: "future answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err = store.AsOf(context.Background(), src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"evidence", "search", "queue stalled"}, {"evidence", "events"}, {"evidence", "status"}, {"evidence", "status", "--since", end.Add(-24 * time.Hour).Format(time.RFC3339)}, {"supervise", "replay"}} {
		code, out, diag := momentum(args...)
		if code != 0 || out == "" || strings.Contains(out+diag, "future answer") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
		if args[1] == "search" || args[1] == "events" {
			if !strings.Contains(out, "queue stalled") {
				t.Fatalf("historical evidence lost: %s", out)
			}
		}
		if args[1] == "status" && !strings.Contains(out, "owner_text 1") {
			t.Fatalf("historical status lost: %s", out)
		}
		if args[1] == "replay" && !strings.Contains(out, " to "+end.Local().Format("2006-01-02 15:04")) {
			t.Fatalf("replay window lost: %s", out)
		}
	}
	from, err := parseWhen("1d")
	if err != nil || !from.Equal(end.Add(-24*time.Hour)) {
		t.Fatalf("parseWhen %s %v", from, err)
	}
	t.Setenv("MOMENTUM_NOW", "invalid")
	if _, err = commandNow(); err == nil {
		t.Fatal("bad clock accepted")
	}
}

// The CLI compares both arms by default and persists an empty synthetic evaluation without starting a runner.
func TestEvaluationCLIWritesAnEmptyPairedResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	if err := os.WriteFile(filepath.Join(os.Getenv("MOMENTUM_CONFIG"), "sources.json"), []byte(`{"loop":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join(t.TempDir(), "golden.jsonl")
	if err := os.WriteFile(golden, nil, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("loop", "eval", "--golden", golden, "--budget", "0", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	var result loop.EvaluationResults
	if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Arms) != 2 || result.K != 3 || result.Provenance.GoldenHash == "" {
		t.Fatalf("%s %v", out, err)
	}
	raw, err := os.ReadFile(filepath.Join(loopDir(), "eval", result.ID, "results.json"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("saved result %v", err)
	}
	for _, arms := range []string{"loop", "native"} {
		code, out, diag = momentum("loop", "eval", "--golden", golden, "--budget", "0", "--arms", arms, "--json")
		if code != 0 {
			t.Fatalf("%d %s %s", code, out, diag)
		}
		if err = json.Unmarshal([]byte(out), &result); err != nil || len(result.Arms) != 1 || result.Arms[0].Arm != arms {
			t.Fatalf("%s %v", out, err)
		}
	}
}

// Search matches incident and mistakes summaries by default, and a source or recorded actor narrows the matches.
func TestEvidenceSearchReadsEverySource(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: "incidents", SourceKey: "seen", TS: at, Actor: store.ActorDetector, Kind: "incident-seen", Summary: "backup volume not mounted"},
		store.Event{Source: "mistakes", SourceKey: "result", TS: at, Actor: store.ActorTool, Kind: "result", Summary: "lockfile busy"})
	for _, phrase := range []string{"not mounted", "lockfile busy"} {
		code, out, diag := momentum("evidence", "search", phrase)
		if code != 0 || !strings.Contains(out, phrase) {
			t.Fatalf("search %q: %d %s %s", phrase, code, out, diag)
		}
	}
	for _, args := range [][]string{{"not mounted", "--source", "incidents", "--actor", "detector"}, {"--source=mistakes", "lockfile busy", "--actor", "tool"}} {
		code, out, diag := momentum(append([]string{"evidence", "search"}, args...)...)
		if code != 0 || out == "" {
			t.Fatalf("filter %v: %d %s %s", args, code, out, diag)
		}
	}
	code, out, diag := momentum("evidence", "search", "lockfile busy", "--source", "incidents")
	if code != 0 || out != "" {
		t.Fatalf("source ignored: %d %s %s", code, out, diag)
	}
	for _, actor := range []string{"owner", "agent", "tool", "harness", "detector", "unknown"} {
		code, _, diag = momentum("evidence", "search", "lockfile busy", "--actor", actor)
		if code != 0 {
			t.Fatalf("recorded actor %s: %s", actor, diag)
		}
	}
	code, _, _ = momentum("evidence", "search", "lockfile busy", "--actor", "invented")
	if code != 2 {
		t.Fatal("unknown actor accepted")
	}
}

// An explicit upper bound cannot move search beyond the trial clock, even in a store with later rows.
func TestEvidenceSearchClampsUntilToTheTrialClock(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339))
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "before", TS: end.Add(-time.Hour), Actor: store.ActorTool, Kind: "tool_result", Summary: "signal before"},
		store.Event{Source: store.Transcripts, SourceKey: "later", TS: end.Add(time.Hour), Actor: store.ActorTool, Kind: "tool_result", Summary: "signal later"})
	code, out, diag := momentum("evidence", "search", "signal", "--until", end.Add(2*time.Hour).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "signal before") || strings.Contains(out, "signal later") {
		t.Fatalf("clock escaped: %d %s %s", code, out, diag)
	}
}

// Live evidence windows stop at the wall clock, so future-dated rows do not appear or count by default.
func TestLiveEvidenceStopsAtTheWallClock(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	fakeScrubber(t)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "future", TS: time.Now().Add(90 * time.Minute), Actor: store.ActorOwner, Kind: "owner_text", Summary: "future signal"})
	for _, args := range [][]string{{"evidence", "events", "--since", "1d"}, {"evidence", "status", "--since", "1d"}, {"evidence", "search", "future signal"}} {
		code, out, diag := momentum(args...)
		if code != 0 || strings.Contains(out, "future signal") || strings.Contains(out, "owner_text 1") {
			t.Fatalf("future row: %v %d %s %s", args, code, out, diag)
		}
	}
}
