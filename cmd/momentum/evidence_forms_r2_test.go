package main

import (
	"database/sql"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/store"
)

// evidence show takes every id after --, as search takes its phrase there, and a lone - as a word that names no
// event: no id is left out without a word.
func TestEvidenceShowReadsTheIdsAfterTheEndOfItsFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	transcript := filepath.Join(home, "t.jsonl")
	first, second := `{"text":"the first record"}`+"\n", `{"text":"the second record"}`+"\n"
	os.WriteFile(transcript, []byte(first+second), 0o600)
	a := store.Event{Source: store.Transcripts, SourceKey: "a", TS: at, Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "one", Ref: transcript + "@0#0"}
	b := store.Event{Source: store.Transcripts, SourceKey: "b", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "two",
		Ref: transcript + "@" + strconv.Itoa(len(first)) + "#0"}
	putEvents(t, a, b)
	for _, args := range [][]string{{"--", a.ID(), b.ID()}, {a.ID(), "--", b.ID()}, {a.ID(), "--bytes", "40", "--", b.ID()}} {
		code, out, diag := momentum(append([]string{"evidence", "show"}, args...)...)
		if code != 0 || !strings.Contains(out, "the first record") || !strings.Contains(out, "the second record") {
			t.Errorf("%v: exit %d %q %s", args, code, out, diag)
		}
	}
	code, out, diag := momentum("evidence", "show", a.ID(), "-", b.ID())
	if code != 0 || !strings.Contains(out, "the first record") || !strings.Contains(out, "the second record") || !strings.Contains(out, "-: ") {
		t.Errorf("a lone dash: exit %d %q %s", code, out, diag)
	}
}

// A record is scrubbed whole before --bytes cuts it: a secret the cut would split is masked as the scrubber sees it
// whole, so no first part of it is printed.
func TestEvidenceShowScrubsARecordWholeBeforeItsCut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	dir := t.TempDir()
	scrub := filepath.Join(dir, "scrub")
	os.WriteFile(scrub, []byte("#!/bin/sh\nsed 's/alpha-value-0001/****************/g'\n"), 0o755) // only the whole word
	was := loop.Scrubber
	loop.Scrubber = []string{scrub}
	t.Cleanup(func() { loop.Scrubber = was })
	transcript := filepath.Join(home, "t.jsonl")
	os.WriteFile(transcript, []byte(`{"text":"the value is alpha-value-0001 and more words after it"}`+"\n"), 0o600)
	e := store.Event{Source: store.Transcripts, SourceKey: "e", TS: time.Now().Add(-time.Hour), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text",
		Summary: "the value", Ref: transcript + "@0#0"}
	putEvents(t, e)
	code, out, diag := momentum("evidence", "show", e.ID(), "--bytes", "31")
	if code != 0 || strings.Contains(out, "alpha-val") || !strings.Contains(out, "the value is ***") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A Hermes row whose timestamp is no time (not a number, past any date, before 1970) is withheld, before any
// conversion that could wrap it under an evaluation's end, and the call's other ids are still shown.
func TestAHermesRowDatedNoTimeIsWithheld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	before := end.Add(-time.Hour)
	db := hermesStore(t, home, [4]any{"h1", "a row of 1e19 seconds", nil, 1e19}, [4]any{"h1", "a row of infinite time", nil, math.Inf(1)},
		[4]any{"h1", "a row before the end", nil, hermesTS(before)})
	far := store.Event{Source: store.Transcripts, SourceKey: "f", TS: before, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x", Ref: db + "@1#0"}
	inf := store.Event{Source: store.Transcripts, SourceKey: "i", TS: before, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x", Ref: db + "@2#0"}
	own := store.Event{Source: store.Transcripts, SourceKey: "o", TS: before, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x", Ref: db + "@3#0"}
	putEvents(t, far, inf, own)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_EVAL", "fixture")
	code, out, diag := momentum("evidence", "show", far.ID(), inf.ID(), own.ID())
	if code != 0 || strings.Contains(out, "1e19 seconds") || strings.Contains(out, "infinite time") || !strings.Contains(out, "a row before the end") ||
		strings.Count(out, "its timestamp is no time") != 2 {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A Hermes store is read read-only: a WAL store with a row still in its log keeps its database file and its log as
// they were, as a writer would not (it checkpoints the row into the file as it closes, and removes the log).
func TestEvidenceShowLeavesAHermesStoreAsItWas(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	live := filepath.Join(t.TempDir(), "live.db")
	w, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: live}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL)`,
		`INSERT INTO messages VALUES (1, 'h1', 'assistant', 'the row still in the log', NULL, NULL, ` + strconv.FormatFloat(hermesTS(at), 'f', -1, 64) + `)`} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(home, "state.db")
	for _, ext := range []string{"", "-wal"} { // a copy taken while the writer holds the row in its log
		raw, err := os.ReadFile(live + ext)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(db+ext, raw, 0o600)
	}
	before, _ := os.ReadFile(db)
	log, _ := os.ReadFile(db + "-wal")
	if len(log) == 0 {
		t.Fatal("the copy holds no log")
	}
	e := store.Event{Source: store.Transcripts, SourceKey: "h", TS: at, Session: "h1", Harness: "hermes", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the row", Ref: db + "@1#0"}
	putEvents(t, e)
	if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || !strings.Contains(out, "the row still in the log") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
	after, _ := os.ReadFile(db)
	logAfter, err := os.ReadFile(db + "-wal")
	if err != nil || string(after) != string(before) || string(logAfter) != string(log) {
		t.Fatalf("the store changed: file %d to %d bytes, log %d to %d bytes (%v)", len(before), len(after), len(log), len(logAfter), err)
	}
}
