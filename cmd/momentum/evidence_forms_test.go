package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// evidence git takes the forms agents write as git takes them: -N for the most commits, grep's first word as its
// pattern, and git's own no (a grep that matched nothing, an ancestry that does not hold) as an answer, exit 1. A
// refused option names the options the command runs with.
func TestEvidenceGitTakesGitsOwnForms(t *testing.T) {
	fakeScrubber(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	for i, text := range []string{"alpha\n", "alpha\nneedle here\n", "alpha\nneedle here\nomega\n"} {
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte(text), 0o600)
		gitIn(t, dir, "add", "a.txt")
		gitIn(t, dir, "commit", "-q", "-m", []string{"first", "second", "third"}[i])
	}
	if code, out, errOut := momentum("evidence", "git", dir, "log", "--oneline", "-2"); code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 1 || strings.Contains(out, "first") {
		t.Errorf("log -2: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "rev-list", "-1", "HEAD"); code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("rev-list -1: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "grep", "-n", "needle"); code != 0 || !strings.Contains(out, "a.txt:2:needle here") {
		t.Errorf("grep's first word is its pattern: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "grep", "-n", "needle", "HEAD~1", "--", "a.txt"); code != 0 || !strings.Contains(out, "needle here") {
		t.Errorf("a pattern, then a revision and a path: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "grep", "-i", "absent-word"); code != 1 || out != "" || !strings.Contains(errOut, "no match") || strings.Contains(errOut, "[ERROR]") {
		t.Errorf("a grep with no match: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "git", dir, "merge-base", "--is-ancestor", "HEAD", "HEAD~1"); code != 1 || !strings.Contains(errOut, "not an ancestor") {
		t.Errorf("an ancestry that does not hold: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("evidence", "git", dir, "log", "--frobnicate"); code != 2 || !strings.Contains(errOut, "--oneline") || !strings.Contains(errOut, "-N") {
		t.Errorf("a refused option names the options: exit %d %s", code, errOut)
	}
	for _, args := range [][]string{{"log", "-2x"}, {"show", "-2"}, {"grep", "-n", "needle", "not-a-revision"}} {
		if code, _, _ := momentum(append([]string{"evidence", "git", dir}, args...)...); code != 2 {
			t.Errorf("git %v: exit %d, want 2", args, code)
		}
	}
}

// evidence events takes an actor, as search does, and prints each event's id, which evidence show takes; a kind no
// event has is refused with the kinds the store holds, by events and search alike.
func TestEvidenceEventsTakesAnActorAndNamesEachEvent(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	owner := store.Event{Source: store.Transcripts, SourceKey: "o", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "why did the export stall"}
	agent := store.Event{Source: store.Transcripts, SourceKey: "a", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the export stalls on its lock"}
	putEvents(t, owner, agent)
	code, out, errOut := momentum("evidence", "events", "--since", "1d", "--actor", "owner")
	if code != 0 || !strings.Contains(out, "ev:"+owner.ID()) || strings.Contains(out, agent.ID()) {
		t.Fatalf("one actor's events, with their ids: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "events", "--actor", "person"); code != 2 || !strings.Contains(errOut, "owner, agent") {
		t.Errorf("an unknown actor: exit %d %s", code, errOut)
	}
	for _, args := range [][]string{{"evidence", "events", "--kind", "owner_prompt"}, {"evidence", "search", "export", "--kind", "owner_prompt"}} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "agent_text, owner_text") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := momentum("evidence", "events", "export", "--since", "1d"); code != 2 || !strings.Contains(errOut, "evidence search") {
		t.Errorf("a phrase given to events: exit %d %s", code, errOut)
	}
}

// evidence search takes a phrase of two characters (a program's name), and names the form for a phrase that starts
// with a dash.
func TestEvidenceSearchTakesShortPhrasesAndNamesTheDashForm(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "c", TS: time.Now().Add(-time.Hour), Session: "s1", Actor: store.ActorTool,
		Kind: "tool_result", Summary: "uv: prune --force refused"})
	if code, out, errOut := momentum("evidence", "search", "uv"); code != 0 || !strings.Contains(out, "prune") {
		t.Errorf("a two-letter phrase: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "search", "--force"); code != 2 || !strings.Contains(errOut, "-- PHRASE") {
		t.Errorf("a dash phrase: exit %d %s", code, errOut)
	}
	if code, out, errOut := momentum("evidence", "search", "--", "--force"); code != 0 || !strings.Contains(out, "prune") {
		t.Errorf("a dash phrase after --: exit %d %q %s", code, out, errOut)
	}
}

// evidence show prints several events in one call, each record scrubbed whole and then cut to --bytes; an id that
// is a session's or a record's place says what reads it; a Hermes event's record is its message row.
func TestEvidenceShowTakesSeveralEventsAndReadsHermesRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	transcript := filepath.Join(home, "t.jsonl")
	first := `{"type":"user","timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","text":"first record secret-value and a long tail of words"}` + "\n"
	second := `{"type":"assistant","timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","text":"second record"}` + "\n"
	os.WriteFile(transcript, []byte(first+second), 0o600)
	db := filepath.Join(home, "state.db")
	hermes, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: db}).String())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL)`,
		fmt.Sprintf(`INSERT INTO messages VALUES (7, 'h1', 'assistant', 'the gateway kept retrying the dead host', NULL, NULL, %f)`, float64(at.UnixMilli())/1000)} {
		if _, err := hermes.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	hermes.Close()
	a := store.Event{Source: store.Transcripts, SourceKey: "a", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "first", Ref: transcript + "@0#0"}
	b := store.Event{Source: store.Transcripts, SourceKey: "b", TS: at, Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "second", Ref: transcript + "@" + strconv.Itoa(len(first)) + "#0"}
	h := store.Event{Source: store.Transcripts, SourceKey: "h", TS: at, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the gateway", Ref: db + "@7#0"}
	putEvents(t, a, b, h)
	code, out, errOut := momentum("evidence", "show", a.ID(), "ev:"+b.ID())
	if code != 0 || !strings.Contains(out, "ev:"+a.ID()) || !strings.Contains(out, "ev:"+b.ID()) || !strings.Contains(out, "second record") || strings.Contains(out, "secret-value") {
		t.Fatalf("two events: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "show", a.ID(), "--bytes", "40"); code != 0 || strings.Contains(out, "long tail") || !strings.Contains(out, "its first 40 of") {
		t.Errorf("a cut record: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "show", h.ID()); code != 0 || !strings.Contains(out, "the gateway kept retrying the dead host") {
		t.Errorf("a Hermes row: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "show", "0f6b2c1e-1a2b-4c3d-8e9f-001122334455"); code != 2 || !strings.Contains(errOut, "--session") {
		t.Errorf("a session's id: exit %d %s", code, errOut)
	}
	if code, out, errOut := momentum("evidence", "show", a.ID(), "0f6b2c1e-1a2b-4c3d-8e9f-001122334455@000012#000"); code != 0 || !strings.Contains(out, "first record") || !strings.Contains(out, "a record's place") {
		t.Errorf("an event and a place: exit %d %q %s", code, out, errOut)
	}
}
