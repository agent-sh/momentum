package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/store"
)

// hermesStore makes a synthetic Hermes messages table and returns its path.
func hermesStore(t *testing.T, dir string, rows ...[4]any) string {
	t.Helper()
	db := filepath.Join(dir, "state.db")
	h, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: db}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := h.Exec(`INSERT INTO messages VALUES (?, ?, 'assistant', ?, ?, NULL, ?)`, i+1, r[0], r[1], r[2], r[3]); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func hermesTS(at time.Time) float64 { return float64(at.UnixMilli()) / 1000 }

// A Hermes row reaches the scrubber as an agent wrote it: no HTML escapes, and each call's arguments one level deep
// as the JSON they are, so a secret the scrubber masks as written is masked there too.
func TestAHermesRowReachesTheScrubberAsWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	dir := t.TempDir()
	scrub := filepath.Join(dir, "scrub")
	// it masks a URL's password with its & as written, and a quoted value one JSON level deep, as in a transcript's line
	os.WriteFile(scrub, []byte("#!/bin/sh\nsed 's/pa&ss-value/**********/g; s/\\\\\"zz-quoted-value\\\\\"/\\\\\"**********\\\\\"/g'\n"), 0o755)
	was := loop.Scrubber
	loop.Scrubber = []string{scrub}
	t.Cleanup(func() { loop.Scrubber = was })
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	calls := `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"echo db_password = \\\"zz-quoted-value\\\"\"}"}}]`
	db := hermesStore(t, home, [4]any{"h1", "cloning: https://builder:pa&ss-value@example.com/repo.git", nil, hermesTS(at)}, [4]any{"h1", "", calls, hermesTS(at)})
	a := store.Event{Source: store.Transcripts, SourceKey: "h1", TS: at, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "cloning", Ref: db + "@1#0"}
	b := store.Event{Source: store.Transcripts, SourceKey: "h2", TS: at, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "tool_call", Summary: "terminal", Ref: db + "@2#0"}
	putEvents(t, a, b)
	code, out, diag := momentum("evidence", "show", a.ID(), b.ID())
	if code != 0 || strings.Contains(out, "pa&ss-value") || strings.Contains(out, `&`) || strings.Contains(out, "zz-quoted-value") || strings.Count(out, "**********") != 2 {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A Hermes row is the event's own only while it is of the event's session at the event's time: a store made again
// starts its ids over, and the row then at an old event's id is another's. In an evaluation, a row is shown before the
// end and withheld from it on.
func TestAHermesRowIsTheEventsOwnAndDatedAsOfTheEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	before, after := end.Add(-time.Hour), end.Add(time.Hour)
	db := hermesStore(t, home, [4]any{"h2", "a later conversation about something else", nil, hermesTS(before)},
		[4]any{"h1", "the gateway kept retrying the dead host", nil, hermesTS(before)}, [4]any{"h1", "after the end: retry the host first", nil, hermesTS(after)})
	other := store.Event{Source: store.Transcripts, SourceKey: "o", TS: before, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x", Ref: db + "@1#0"}
	own := store.Event{Source: store.Transcripts, SourceKey: "w", TS: before, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x", Ref: db + "@2#0"}
	late := store.Event{Source: store.Transcripts, SourceKey: "l", TS: after, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x", Ref: db + "@3#0"}
	putEvents(t, other, own, late)
	if _, out, _ := momentum("evidence", "show", other.ID()); strings.Contains(out, "something else") || !strings.Contains(out, "another's") {
		t.Fatalf("another session's row: %q", out)
	}
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_EVAL", "fixture")
	if _, out, _ := momentum("evidence", "show", own.ID(), late.ID()); !strings.Contains(out, "kept retrying the dead host") || strings.Contains(out, "retry the host first") || !strings.Contains(out, "withheld") {
		t.Fatalf("an evaluation's rows: %q", out)
	}
}

// --bytes backs off only a rune it splits; events reads "a, b" as two kinds and gives each JSON line its id; grep with
// -e anywhere takes every other word as a revision, as git does.
func TestEvidenceFormsReadAsTheyAreWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	transcript := filepath.Join(home, "t.jsonl")
	line := "{\"text\":\"a \xff byte, then a long tail of words\"}\n"
	os.WriteFile(transcript, []byte(line), 0o600)
	e := store.Event{Source: store.Transcripts, SourceKey: "e", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "why", Ref: transcript + "@0#0"}
	f := store.Event{Source: store.Transcripts, SourceKey: "f", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "because"}
	putEvents(t, e, f)
	if _, out, _ := momentum("evidence", "show", e.ID(), "--bytes", "20"); !strings.Contains(out, "its first 20 of") {
		t.Errorf("a cut past an invalid byte: %q", out)
	}
	if _, out, _ := momentum("evidence", "events", "--since", "1d", "--kind", "owner_text, agent_text"); !strings.Contains(out, "why") || !strings.Contains(out, "because") {
		t.Errorf("two spaced kinds: %q", out)
	}
	if _, out, _ := momentum("evidence", "events", "--since", "1d", "--json"); !strings.Contains(out, `"ID":"`+e.ID()+`"`) {
		t.Errorf("a JSON line's id: %q", out)
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("gamma\n"), 0o600)
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("delta\n"), 0o600) // the work tree no longer holds it
	if code, out, diag := momentum("evidence", "git", dir, "grep", "-n", "HEAD", "-e", "gamma"); code != 0 || !strings.Contains(out, "HEAD:a.txt:1:gamma") {
		t.Errorf("grep HEAD -e: exit %d %q %s", code, out, diag)
	}
	_ = fmt.Sprint
}
