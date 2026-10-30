package source

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
)

// bumpSchema puts a store back at an older schema version, one whose builds read every source again on a version
// change (user_version 2, no steps): the next open moves it up through the steps, and one of them reads again.
func bumpSchema(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA user_version = 2; DROP TABLE IF EXISTS schema_steps`); err != nil {
		t.Fatal(err)
	}
}

func openPath(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The transcripts' events are kept across a schema change that reads again. The change drops the cursors, so the
// next ingest reads every transcript still there again; what it reads replaces what was read of it, record by
// record, so nothing is stored twice, not even a turn the reader now splits otherwise. A transcript deleted in
// between keeps its events.
func TestTranscriptsKeepTheirEventsAcrossASchemaChange(t *testing.T) {
	ctx := context.Background()
	home := testHome(t)
	// The prompt history records a paste, so the reader cuts it from his turn into an event of its own.
	paste := "panic: runtime error: index out of range [3] with length 3 in cache.Key"
	history := writeLines(t, filepath.Join(home, ".claude", "history.jsonl"),
		`{"display":"fix this: [Pasted text #1]","pastedContents":{"1":{"id":1,"type":"text","content":"`+paste+`"}},"timestamp":1792490820000,"project":"/home/u/projects/shop"}`)
	lines := append(claudeSession(), claudeLine(7, `"type":"user","uuid":"u7","promptId":"p7",`+typed+`,"message":{"role":"user","content":"fix this: `+paste+`"}`))
	writeLines(t, claudePath(home), lines...)
	xpath := writeLines(t, codexPath(home), codexRollout()...)
	writeLines(t, piPath(home), piSession()...)
	path := filepath.Join(t.TempDir(), "m.db")
	st := openPath(t, path)
	ingest(t, transcripts(home), st)
	before := all(t, st, "transcripts")
	find(t, before, "agent_text", "index out of range") // the paste, cut from his turn
	codex := 0
	for _, e := range before {
		if e.Harness == "codex" {
			codex++
		}
	}
	if codex == 0 {
		t.Fatal("setup: no Codex events")
	}
	st.Close()

	// In between, Codex deletes the rollout, and the prompt history no longer records the paste: his turn now reads
	// as one event.
	if err := os.Remove(xpath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(history); err != nil {
		t.Fatal(err)
	}
	bumpSchema(t, path)
	st = openPath(t, path)
	if cs, err := st.FileCursors(ctx, "transcripts"); err != nil || len(cs) != 0 {
		t.Fatalf("the schema change kept the cursors: %d, %v", len(cs), err)
	}
	if r := ingest(t, transcripts(home), st); r.Read == 0 {
		t.Fatalf("after the schema change nothing was read again: %+v", r)
	}
	after := all(t, st, "transcripts")

	// No loss: every event of the deleted rollout is there, as it was.
	for k, e := range before {
		if e.Harness != "codex" {
			continue
		}
		if got, ok := after[k]; !ok || fmt.Sprint(got) != fmt.Sprint(e) {
			t.Fatalf("an event of the deleted rollout was lost or changed:\nwas %+v\nnow %+v", e, got)
		}
	}
	// No duplicates: the transcripts still there hold exactly what a first read of them stores.
	fresh := openStore(t)
	ingest(t, transcripts(home), fresh)
	want := all(t, fresh, "transcripts")
	for k, e := range after {
		if e.Harness == "codex" {
			continue
		}
		if w, ok := want[k]; !ok || fmt.Sprint(w) != fmt.Sprint(e) {
			t.Fatalf("an event a first read does not store: %+v", e)
		}
	}
	for k, w := range want {
		if _, ok := after[k]; !ok {
			t.Fatalf("an event a first read stores is missing: %+v", w)
		}
	}
	if len(after) != len(want)+codex {
		t.Fatalf("%d events, want %d read again and %d kept", len(after), len(want), codex)
	}
	if e := find(t, after, "owner_text", "fix this:"); !strings.Contains(e.Summary, "index out of range") {
		t.Fatalf("his turn, read again without the paste record: %+v", e)
	}
}

// Gone counts the transcripts' events whose source is gone: a transcript deleted, and a Hermes session its store
// no longer holds. A rollout whose compressed copy is still there is not gone, and nor is one Codex archived: the
// next ingest finds it by its name.
func TestGoneCountsTheEventsWhoseSourceIsGone(t *testing.T) {
	ctx := context.Background()
	home := testHome(t)
	cpath := writeLines(t, claudePath(home), claudeSession()...)
	xpath := writeLines(t, codexPath(home), codexRollout()...)
	bare := writeLines(t, codexUnknownPath(home), codexUnknown()...)
	hpath := filepath.Join(home, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(hpath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_HERMES_DB", hpath)
	db, err := sql.Open("sqlite", hpath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(hermesSchema + `
	  insert into sessions (id, source, created_source, model, started_at) values ('phone', 'mobile', 'mobile', 'gpt-6.1-sol', 100);
	  insert into sessions (id, source, created_source, model, started_at) values ('desk', 'mobile', 'mobile', 'gpt-6.1-sol', 200);
	  insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'prune the docker images', 101);
	  insert into messages (session_id, role, content, timestamp) values ('desk', 'user', 'rotate the logs', 201);`); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	ingest(t, transcripts(home), st)
	want, desk := 0, 0
	for _, e := range all(t, st, "transcripts") {
		switch {
		case e.Harness == "claude":
			want++
		case e.Session == "desk":
			want++
			desk++
		}
	}
	if want == 0 || desk == 0 {
		t.Fatalf("setup: %d events to go, %d of the desk session", want, desk)
	}
	// The Claude Code transcript is deleted, one rollout compressed in place and the other archived, the desk
	// session deleted from its store.
	if err := os.Remove(cpath); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(home, ".codex", "archived_sessions", filepath.Base(bare))
	if err := os.MkdirAll(filepath.Dir(archived), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bare, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(xpath)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	if err := os.WriteFile(xpath+".zst", enc.EncodeAll(raw, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	enc.Close()
	if err := os.Remove(xpath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`delete from messages where session_id = 'desk'; delete from sessions where id = 'desk'`); err != nil {
		t.Fatal(err)
	}
	if n, err := transcripts(home).Gone(ctx, st); err != nil || n != want {
		t.Fatalf("gone %d, %v; want %d", n, err, want)
	}
}

// A harness has one name in every source, so a query by it finds the events of each: here Claude Code's in the
// mistakes log and in its transcripts.
func TestAQueryByHarnessFindsTheEventsOfEverySource(t *testing.T) {
	ctx := context.Background()
	home := testHome(t)
	writeLines(t, claudePath(home), claudeSession()...)
	writeLines(t, codexPath(home), codexRollout()...)
	writeLines(t, piPath(home), piSession()...)
	log := writeLines(t, filepath.Join(t.TempDir(), "events.jsonl"),
		`{"ts":"2026-10-20T10:02:00Z","session":"`+claudeSID+`","kind":"tool-error","tool":"Bash","detail":"Exit code 2"}`)
	st := openStore(t)
	ingest(t, Mistakes{Path: log}, st)
	ingest(t, transcripts(home), st)
	evs, err := st.Events(ctx, store.Query{Since: time.Unix(0, 0), Harness: "claude", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int{}
	for _, e := range evs {
		if e.Harness != "claude" {
			t.Fatalf("another harness's event: %+v", e)
		}
		by[e.Source]++
	}
	if by["mistakes"] != 1 || by["transcripts"] == 0 {
		t.Fatalf("Claude Code's events by source: %v, want the mistakes log's and the transcripts'", by)
	}
	for _, src := range []string{"mistakes", "transcripts"} {
		for _, e := range all(t, st, src) {
			if !slices.Contains(store.Harnesses, e.Harness) {
				t.Fatalf("a harness by a name of its own: %+v", e)
			}
		}
	}
}
