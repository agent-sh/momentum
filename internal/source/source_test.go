package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func all(t *testing.T, st *store.Store, src string) []store.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.Query{Since: time.Unix(0, 0), Source: src, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

const (
	rowErr  = `{"ts":"2026-01-02T10:00:00","session":"s1","kind":"tool-error","tool":"Bash","input":"x","detail":"Exit code 1\nmore"}` + "\n"
	rowDeny = `{"ts":"2026-01-02T10:01:00","session":"s1","kind":"deny","tool":"Bash","detail":"guard: no"}` + "\n"
	rowFix  = `{"ts":"2026-01-02T10:02:00","session":"s1","kind":"correction","heuristic":true,"input":"no, the other one"}` + "\n"
)

func TestMistakesReadsCompleteLinesOnceAndResumes(t *testing.T) {
	ctx, st := context.Background(), openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr+rowDeny+rowFix[:30]) // the last row is still being written
	m := Mistakes{Path: log}
	if n, err := m.Ingest(ctx, st); err != nil || n != 2 {
		t.Fatalf("first ingest: n=%d err=%v, want 2", n, err)
	}
	appendFile(t, log, rowFix[30:])
	if n, err := m.Ingest(ctx, st); err != nil || n != 1 {
		t.Fatalf("second ingest: n=%d err=%v, want the completed row only", n, err)
	}
	byKind := map[string]store.Event{}
	for _, e := range all(t, st, "mistakes") {
		byKind[e.Kind] = e
	}
	if byKind["deny"].Actor != store.ActorHarness || byKind["tool-error"].Actor != store.ActorTool {
		t.Fatalf("actors: %+v", byKind)
	}
	if c := byKind["correction"]; c.Actor != store.ActorOwner || !c.Heuristic {
		t.Fatalf("a heuristic correction must stay the owner's and stay flagged: %+v", c)
	}
	if s := byKind["tool-error"].Summary; strings.Contains(s, "more") {
		t.Fatalf("summary should keep the first line of the detail only: %q", s)
	}
}

func TestMistakesRereadsARotatedLogWithoutDuplicates(t *testing.T) {
	ctx, st := context.Background(), openStore(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	appendFile(t, log, rowErr+rowDeny)
	m := Mistakes{Path: log}
	if n, _ := m.Ingest(ctx, st); n != 2 {
		t.Fatalf("first ingest n=%d", n)
	}
	// rotation: a new file (new inode) holding one old row and one new row
	if err := os.Rename(log, filepath.Join(dir, "old.jsonl")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, log, rowDeny+rowFix)
	if n, err := m.Ingest(ctx, st); err != nil || n != 1 {
		t.Fatalf("after rotation n=%d err=%v, want only the new row", n, err)
	}
}

func TestMistakesMissingLogIsNotAnError(t *testing.T) {
	if n, err := (Mistakes{Path: filepath.Join(t.TempDir(), "none.jsonl")}).Ingest(context.Background(), openStore(t)); n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestRadarStoresStateChangesAgain(t *testing.T) {
	ctx, st := context.Background(), openStore(t)
	path := filepath.Join(t.TempDir(), "radar.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE findings (id TEXT PRIMARY KEY, ts REAL, type TEXT, key TEXT, title TEXT, state TEXT, updated REAL);
	  INSERT INTO findings VALUES ('f-1', 100, 'infra', 'infra:a', 'disk fills up', 'new', 100), ('f-2', 101, 'research', 'research:b', 'a paper', 'new', 101);`); err != nil {
		t.Fatal(err)
	}
	r := Radar{Path: path}
	if n, err := r.Ingest(ctx, st); err != nil || n != 2 {
		t.Fatalf("first ingest n=%d err=%v", n, err)
	}
	if _, err := db.Exec(`UPDATE findings SET state='dismissed', updated=200 WHERE id='f-1'`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Ingest(ctx, st); err != nil || n != 0 {
		t.Fatalf("second ingest n=%d err=%v, want no new rows", n, err)
	}
	for _, e := range all(t, st, "radar") {
		if e.SourceKey == "f-1" && !strings.Contains(e.Summary, "dismissed") {
			t.Fatalf("the state change was not stored: %q", e.Summary)
		}
	}
}

func TestIncidentsReadsOpenAndClosedAndOnlyChangedFiles(t *testing.T) {
	ctx, st := context.Background(), openStore(t)
	dir := t.TempDir()
	write := func(sub, id, status string, mtime time.Time) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, sub, id+".json")
		raw, _ := json.Marshal(map[string]any{"incident_id": id, "status": status, "summary": "a failure", "last_seen": "2026-01-02T10:00:00", "repeat_count": 3})
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-time.Hour)
	write("open", "inc-1", "reported", t0)
	write("closed", "inc-2", "closed", t0)
	inc := Incidents{Dir: dir}
	if n, err := inc.Ingest(ctx, st); err != nil || n != 2 {
		t.Fatalf("first ingest n=%d err=%v", n, err)
	}
	write("open", "inc-1", "resolved", t0.Add(time.Minute))
	if n, err := inc.Ingest(ctx, st); err != nil || n != 0 {
		t.Fatalf("second ingest n=%d err=%v", n, err)
	}
	for _, e := range all(t, st, "incidents") {
		if e.SourceKey == "inc-1" && !strings.HasPrefix(e.Summary, "[resolved x3]") {
			t.Fatalf("changed incident not refreshed: %q", e.Summary)
		}
	}
}
