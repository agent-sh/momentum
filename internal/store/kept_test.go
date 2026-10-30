package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

// v3Schema is the store as the last build before the events were kept made it: user_version 3, every table
// derived but the stamps.
const v3Schema = `
CREATE TABLE events (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, source_key TEXT NOT NULL, ts INTEGER NOT NULL,
  session TEXT, agent TEXT, harness TEXT, model TEXT, cwd TEXT, actor TEXT NOT NULL, kind TEXT NOT NULL,
  tool TEXT, summary TEXT, meta TEXT, ref TEXT, heuristic INTEGER NOT NULL DEFAULT 0,
  experiment TEXT NOT NULL DEFAULT '', ingested INTEGER NOT NULL);
CREATE INDEX events_ts ON events(ts);
CREATE INDEX events_kind ON events(kind, ts);
CREATE INDEX events_source ON events(source);
CREATE TABLE cursors (source TEXT PRIMARY KEY, position TEXT NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE seen_files (source TEXT NOT NULL, path TEXT NOT NULL, mtime INTEGER NOT NULL,
  size INTEGER NOT NULL, PRIMARY KEY (source, path));
CREATE TABLE skipped (source TEXT NOT NULL, ref TEXT NOT NULL, reason TEXT NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY (source, ref));
CREATE TABLE trackers (tool TEXT NOT NULL, key TEXT NOT NULL, build TEXT NOT NULL, state BLOB NOT NULL,
  log_base INTEGER NOT NULL, log_seq INTEGER NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY (tool, key));
CREATE TABLE tracker_log (tool TEXT NOT NULL, key TEXT NOT NULL, seq INTEGER NOT NULL, role TEXT NOT NULL,
  name TEXT NOT NULL, origin TEXT NOT NULL, text TEXT NOT NULL, PRIMARY KEY (tool, key, seq)) WITHOUT ROWID;
CREATE INDEX events_session ON events(session, ts);
CREATE TABLE file_cursors (source TEXT NOT NULL, path TEXT NOT NULL, inode INTEGER NOT NULL, fp TEXT NOT NULL,
  compressed INTEGER NOT NULL, offset INTEGER NOT NULL, state BLOB, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
  updated INTEGER NOT NULL, PRIMARY KEY (source, path));
CREATE TABLE session_stamps (key TEXT PRIMARY KEY, phase TEXT NOT NULL, since INTEGER NOT NULL,
  seq INTEGER NOT NULL, seen INTEGER NOT NULL, tool TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  auto INTEGER NOT NULL DEFAULT 0, ask TEXT NOT NULL DEFAULT '', ask_kind TEXT NOT NULL DEFAULT '');
PRAGMA user_version = 3;
`

// rawQuery reads one value from a store file, past the store.
func rawQuery(t *testing.T, path, q string, dest any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(q).Scan(dest); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// A store as the build before the events were kept left it migrates in place: every event stays, Claude Code's
// gets the name the transcripts give it, nothing is read again (the cursors stay), and user_version stays at 3,
// the version that build checks before it drops anything.
func TestAStoreFromBeforeKeptEventsMigratesInPlace(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(v3Schema + `
	  INSERT INTO events (id, source, source_key, ts, session, agent, harness, model, cwd, actor, kind, tool, summary, meta, ref, ingested) VALUES
	    ('m', 'mistakes', 'k1', 1700000000000, 's1', '', 'claude-code', '', '', 'tool', 'tool-error', 'Bash', 'exit 2', '', '', 1),
	    ('s', 'sessions', 's1@1', 1700000000000, 's1', '', 'claude-code', '', '', 'harness', 'phase', '', '', '{"to":"idle"}', '', 1),
	    ('t', 'transcripts', 's1@000000000000#000', 1700000000000, 's1', '', 'claude', '', '', 'owner', 'owner_text', '', 'go on', '', '', 1),
	    ('x', 'transcripts', 'r@000000000000#000', 1700000000000, 'r1', '', 'codex', '', '', 'agent', 'agent_text', '', 'done', '', '', 1);
	  INSERT INTO file_cursors VALUES ('transcripts', '/s/s1.jsonl', 7, 'ab', 0, 42, NULL, 50, 9, 1);
	  INSERT INTO session_stamps (key, phase, since, seq, seen) VALUES ('s1', 'idle', 5, 1, 6);`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(ctx, Query{Since: time.UnixMilli(0)})
	if err != nil || len(evs) != 4 {
		t.Fatalf("events after the migration: %d, %v", len(evs), err)
	}
	var claude []string
	for _, e := range evs {
		switch e.Harness {
		case "claude":
			claude = append(claude, e.Source)
		case "codex":
		default:
			t.Fatalf("a harness by another name: %+v", e)
		}
	}
	sort.Strings(claude)
	if strings.Join(claude, " ") != "mistakes sessions transcripts" {
		t.Fatalf("Claude Code's events: %v", claude)
	}
	if cs, _ := st.FileCursors(ctx, "transcripts"); len(cs) != 1 {
		t.Fatalf("the cursors were not kept: %+v", cs)
	}
	if got, err := st.SessionStamps(ctx); err != nil || len(got) != 1 {
		t.Fatalf("the stamps: %+v %v", got, err)
	}
	var uv, key, bySource int
	rawQuery(t, path, `PRAGMA user_version`, &uv)
	rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'events_key'`, &key)
	rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'events_source'`, &bySource)
	if uv != 3 || key != 1 || bySource != 0 {
		t.Fatalf("user_version %d, events_key %d, events_source %d", uv, key, bySource)
	}
}

// A build from before the events were kept, run on a store migrated since, makes events_source again and writes
// Claude Code by its old name. The next open of this build puts both right, and the one after has nothing to do.
func TestAnOlderBuildsVisitIsPutRight(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX events_source ON events(source);
	  INSERT INTO events (id, source, source_key, ts, harness, actor, kind, ingested) VALUES
	    ('m', 'mistakes', 'k1', 1700000000000, 'claude-code', 'tool', 'tool-error', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(ctx, Query{Since: time.UnixMilli(0), Harness: "claude"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("Claude Code's events after the visit: %+v %v", evs, err)
	}
	var old int
	rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'events_source'`, &old)
	if old != 0 {
		t.Fatal("events_source is still there")
	}
	if ok, err := current(st.db); err != nil || !ok {
		t.Fatalf("after the repairs the store is not current: %v", err)
	}
}

// A store a newer build migrated is refused, and nothing in it changes: this build does not know its steps.
func TestAStoreFromANewerBuildIsRefusedUnchanged(t *testing.T) {
	st, path := open(t)
	if _, err := st.Put(context.Background(), []Event{{Source: "mistakes", SourceKey: "k", TS: time.Now(), Actor: ActorTool, Kind: "x"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_steps VALUES (?, 0)`, latest()+1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if st, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		if st != nil {
			st.Close()
		}
		t.Fatalf("a store from a newer build: %v", err)
	}
	var n int
	rawQuery(t, path, `SELECT COUNT(*) FROM events`, &n)
	if n != 1 {
		t.Fatalf("%d events left", n)
	}
}

// The next column of the events is one more entry in keptColumns, with a step that reads again for the sources to
// fill it. A store from before gains it, keeps every event and stamp with the column's default, and drops its
// derived tables, so the next ingest reads its sources again; the next open has nothing to do.
func TestTheNextColumnIsOneEntryAndAStep(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	at := time.UnixMilli(1_700_000_000_000)
	if _, _, err := st.PutFiles(ctx, []FileWrite{{Events: []Event{{Source: Transcripts, SourceKey: "a@000000000000#000", TS: at, Actor: ActorAgent, Kind: "agent_text"}},
		Cursor: FileCursor{Source: Transcripts, Path: "/s/a.jsonl", Offset: 10}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, []Event{{Source: Sessions, SourceKey: "s@1", TS: at, Actor: ActorHarness, Kind: "phase"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessionStamps(ctx, state.Stamps{"s": {Phase: sessions.Idle, Since: at, Seen: at}}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// The next build: a column, and a step.
	cols, was := keptColumns[0].columns, steps
	t.Cleanup(func() { keptColumns[0].columns, steps = cols, was })
	keptColumns[0].columns = append(slices.Clone(cols), [2]string{"tokens", "INTEGER NOT NULL DEFAULT 0"})
	steps = append(slices.Clone(was), step{version: latest() + 1, sql: `UPDATE events SET tokens = 0 WHERE tokens != 0`, reread: true})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if evs, err := st.Events(ctx, Query{Since: time.UnixMilli(0)}); err != nil || len(evs) != 2 {
		t.Fatalf("events after the step: %d %v", len(evs), err)
	}
	if got, err := st.SessionStamps(ctx); err != nil || len(got) != 1 {
		t.Fatalf("stamps after the step: %+v %v", got, err)
	}
	if cs, _ := st.FileCursors(ctx, Transcripts); len(cs) != 0 {
		t.Fatalf("the step kept the cursors: %+v", cs)
	}
	var defaulted, last int
	rawQuery(t, path, `SELECT COUNT(*) FROM events WHERE tokens = 0`, &defaulted)
	rawQuery(t, path, `SELECT MAX(version) FROM schema_steps`, &last)
	if defaulted != 2 || last != latest() {
		t.Fatalf("%d events hold the column's default, want 2; the last step %d", defaulted, last)
	}
	if ok, err := current(st.db); err != nil || !ok {
		t.Fatalf("after the step the store is not current: %v", err)
	}
}

// A column with no step is added, and nothing is read again: the cursors stay, and the events before it hold its
// default.
func TestAColumnWithNoStepKeepsTheCursors(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	if _, _, err := st.PutFiles(ctx, []FileWrite{{Events: []Event{{Source: Transcripts, SourceKey: "a@000000000000#000", TS: time.Now(), Actor: ActorAgent, Kind: "agent_text"}},
		Cursor: FileCursor{Source: Transcripts, Path: "/s/a.jsonl", Offset: 10}}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	cols := keptColumns[0].columns
	t.Cleanup(func() { keptColumns[0].columns = cols })
	keptColumns[0].columns = append(slices.Clone(cols), [2]string{"lang", "TEXT NOT NULL DEFAULT ''"})
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if cs, _ := st.FileCursors(ctx, Transcripts); len(cs) != 1 {
		t.Fatalf("a column alone dropped the cursors: %+v", cs)
	}
	var defaulted int
	rawQuery(t, path, `SELECT COUNT(*) FROM events WHERE lang = ''`, &defaulted)
	if defaulted != 1 {
		t.Fatalf("%d events hold the new column's default, want 1", defaulted)
	}
}

// A record read again holds what that reading stores: an event stored under one of a write's places that the write
// does not hold is dropped; another record's events stay, and so do another file's and another source's.
func TestARecordReadAgainHoldsOneReadingsEvents(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	ev := func(src, key string) Event {
		return Event{Source: src, SourceKey: key, TS: time.UnixMilli(1_700_000_000_000), Actor: ActorAgent, Kind: "agent_text"}
	}
	cur := FileCursor{Source: Transcripts, Path: "/s/a.jsonl", Offset: 300}
	first := FileWrite{Events: []Event{ev(Transcripts, "a@000000000100#000"), ev(Transcripts, "a@000000000100#001"),
		ev(Transcripts, "a@000000000100#002"), ev(Transcripts, "a@000000000200#000")}, Cursor: cur,
		Places: []string{"a@000000000100#", "a@000000000200#"}}
	other := FileWrite{Events: []Event{ev(Transcripts, "ab@000000000100#001")}, Cursor: FileCursor{Source: Transcripts, Path: "/s/ab.jsonl"},
		Places: []string{"ab@000000000100#"}}
	if _, _, err := st.PutFiles(ctx, []FileWrite{first, other}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, []Event{ev("elsewhere", "a@000000000100#001")}); err != nil {
		t.Fatal(err)
	}
	// Read again, record 100 gives one event.
	again := FileWrite{Events: []Event{ev(Transcripts, "a@000000000100#000")}, Cursor: cur, Places: []string{"a@000000000100#"}}
	added, dropped, err := st.PutFiles(ctx, []FileWrite{again})
	if err != nil || added != 0 || dropped != 2 {
		t.Fatalf("read again: %d added, %d dropped, %v; want 0 and 2", added, dropped, err)
	}
	evs, err := st.Events(ctx, Query{Since: time.UnixMilli(0)})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range evs {
		keys = append(keys, e.Source+" "+e.SourceKey)
	}
	sort.Strings(keys)
	want := "elsewhere a@000000000100#001,transcripts a@000000000100#000,transcripts a@000000000200#000,transcripts ab@000000000100#001"
	if strings.Join(keys, ",") != want {
		t.Fatalf("after the record was read again:\n%s\nwant\n%s", strings.Join(keys, ","), want)
	}
}
