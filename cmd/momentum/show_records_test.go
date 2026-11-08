package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
)

// showOne runs evidence show on one event and fails the test on a nonzero exit.
func showOne(t *testing.T, args ...string) string {
	t.Helper()
	code, out, diag := momentum(append([]string{"evidence", "show"}, args...)...)
	if code != 0 {
		t.Fatalf("evidence show %v: exit %d %q %s", args, code, out, diag)
	}
	return out
}

// A Codex rollout that Codex compressed after it was read is still the session's record: show reads it through
// tacit's reader at the same offset of its JSONL bytes, scrubbed whole and clipped after. A record that is gone
// with no compressed copy is still gone, and a compressed copy that lies where a run can write is withheld.
func TestEvidenceShowReadsACompressedRollout(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	dir := t.TempDir()
	first := `{"timestamp":"2026-03-02T09:00:00Z","type":"session_meta","payload":{"id":"first-line-marker"}}` + "\n"
	second := `{"timestamp":"2026-03-02T09:00:01Z","type":"response_item","payload":{"type":"function_call_output","output":"secret-value packed-marker"}}` + "\n"
	third := `{"timestamp":"2026-03-02T09:00:02Z","type":"response_item","payload":{"output":"` + strings.Repeat("z", readMax) + `"}}` + "\n"
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	pack := func(path string) {
		t.Helper()
		if err := os.WriteFile(path+".zst", enc.EncodeAll([]byte(first+second+third), nil), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rollout := filepath.Join(dir, "rollout-2026-03-02T09-00-00-0000.jsonl")
	pack(rollout)
	run := filepath.Join(loopDir(), "runs", "earlier")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(run, "rollout-planted.jsonl")
	pack(planted)
	aliased := filepath.Join(dir, "rollout-aliased.jsonl")
	if err := os.Symlink(planted+".zst", aliased+".zst"); err != nil {
		t.Fatal(err)
	}
	event := func(key, path string, off int) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: key, TS: time.Now().Add(-time.Minute), Session: "s1", Harness: "codex",
			Actor: store.ActorTool, Kind: "tool_result", Summary: "fixture summary", Ref: fmt.Sprintf("%s@%d#0", path, off)}
	}
	packed := event("packed", rollout, len(first))
	long := event("long", rollout, len(first)+len(second))
	inside := event("inside", rollout, 5)
	gone := event("gone", filepath.Join(dir, "rollout-gone.jsonl"), 0)
	writable := event("writable", aliased, len(first))
	putEvents(t, packed, long, inside, gone, writable)

	out := showOne(t, packed.ID())
	if !strings.Contains(out, "packed-marker") || !strings.Contains(out, "************") || strings.Contains(out, "secret-value") ||
		strings.Contains(out, "first-line-marker") || strings.Contains(out, "its record is gone") {
		t.Fatalf("compressed rollout's record: %q", out)
	}
	if out := showOne(t, "--bytes", "60", packed.ID()); strings.Contains(out, "secret-value") || !strings.Contains(out, "(its first 60 of") {
		t.Fatalf("compressed record not scrubbed whole before its cut: %q", out)
	}
	if out := showOne(t, long.ID()); !strings.Contains(out, "(oversized record withheld)") || strings.Contains(out, "zzzz") {
		t.Fatalf("oversized compressed record: %q", out)
	}
	if code, _, diag := momentum("evidence", "show", inside.ID()); code != 2 || !strings.Contains(diag, "not a record boundary") {
		t.Fatalf("an offset inside a compressed record: %d %s", code, diag)
	}
	if out := showOne(t, gone.ID()); !strings.Contains(out, "(its record is gone)") {
		t.Fatalf("a gone record: %q", out)
	}
	if out := showOne(t, writable.ID()); !strings.Contains(out, "(record withheld: it lies where a run can write)") || strings.Contains(out, "packed-marker") {
		t.Fatalf("a compressed copy in a run's root: %q", out)
	}
}

// A Hermes event's record is its row in the Hermes store, read by the session that holds it (a delegation's own),
// scrubbed and bounded as a transcript line is. A row the store no longer holds, or holds for another session, is
// gone; an as-of child still withholds every Hermes row; a store where a run can write is withheld; and another
// source's reference into the store is not a row.
func TestEvidenceShowReadsAHermesRow(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	hermesStore := func(path string) {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`create table sessions (id text primary key, source text not null, created_source text, model text, model_config text, parent_session_id text, started_at real not null, ended_at real, end_reason text, cwd text, git_repo_root text, title text);
create table messages (id integer primary key autoincrement, session_id text not null references sessions(id), role text not null, content text, tool_call_id text, tool_calls text, tool_name text, timestamp real not null, finish_reason text, _compressed_summary integer default 0, display_kind text);
insert into sessions (id, source, created_source, started_at) values ('phone', 'mobile', 'mobile', 100);
insert into sessions (id, source, created_source, model_config, parent_session_id, started_at) values ('sub', 'mobile', 'subagent', '{"_delegate_from":"phone"}', 'phone', 101);
insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'prune the docker images', 101);
insert into messages (session_id, role, content, tool_calls, finish_reason, timestamp) values ('phone', 'assistant', 'Pruning.',
  '[{"id":"call_h1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"docker image prune <dangling> secret-value\"}"}}]', 'tool_calls', 102);
insert into messages (session_id, role, content, tool_call_id, tool_name, timestamp) values ('phone', 'tool', '{"output": "secret-value hermes-marker", "exit_code": 1}', 'call_h1', 'terminal', 103.25);
insert into messages (session_id, role, content, timestamp) values ('sub', 'assistant', 'delegated-marker', 104);
insert into messages (session_id, role, content, timestamp) values ('phone', 'tool', ?, 105);`, strings.Repeat("x", readMax+1)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "state.db")
	hermesStore(path)
	run := filepath.Join(loopDir(), "runs", "earlier")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(run, "state.db")
	hermesStore(planted)
	event := func(key, source, path string, msg int, session, agent string) store.Event {
		return store.Event{Source: source, SourceKey: key, TS: time.Now().Add(-time.Minute), Session: session, Agent: agent, Harness: "hermes",
			Actor: store.ActorTool, Kind: "tool_result", Summary: "fixture summary", Ref: fmt.Sprintf("%s@%d#0", path, msg)}
	}
	call := event("call", store.Transcripts, path, 2, "phone", "")
	result := event("result", store.Transcripts, path, 3, "phone", "")
	delegated := event("delegated", store.Transcripts, path, 4, "phone", "sub")
	other := event("other", store.Transcripts, path, 4, "phone", "")
	missing := event("missing", store.Transcripts, path, 99, "phone", "")
	long := event("long", store.Transcripts, path, 5, "phone", "")
	writable := event("writable", store.Transcripts, planted, 3, "phone", "")
	scan := event("scan", store.Supervise, path, 3, "phone", "")
	putEvents(t, call, result, delegated, other, missing, long, writable, scan)

	out := showOne(t, result.ID())
	if !strings.Contains(out, "hermes-marker") || !strings.Contains(out, "call_h1") || !strings.Contains(out, `"timestamp":"1970-01-01T00:01:43.25Z"`) ||
		!strings.Contains(out, "************") || strings.Contains(out, "secret-value") || strings.Contains(out, "withheld") {
		t.Fatalf("Hermes result row: %q", out)
	}
	if out := showOne(t, call.ID()); !strings.Contains(out, "docker image prune <dangling>") || !strings.Contains(out, `"tool_calls":[{"id":"call_h1"`) ||
		strings.Contains(out, "secret-value") {
		t.Fatalf("Hermes call row: %q", out)
	}
	if out := showOne(t, "--bytes", "30", result.ID()); strings.Contains(out, "secret-value") || !strings.Contains(out, "(its first 30 of") {
		t.Fatalf("Hermes row not scrubbed whole before its cut: %q", out)
	}
	if out := showOne(t, delegated.ID()); !strings.Contains(out, "delegated-marker") {
		t.Fatalf("a delegation's row: %q", out)
	}
	for _, e := range []store.Event{other, missing} {
		if out := showOne(t, e.ID()); !strings.Contains(out, "(its record is gone)") || strings.Contains(out, "delegated-marker") {
			t.Fatalf("%s: %q", e.SourceKey, out)
		}
	}
	if out := showOne(t, long.ID()); !strings.Contains(out, "(oversized record withheld)") || strings.Contains(out, "xxxx") {
		t.Fatalf("oversized Hermes row: %q", out)
	}
	if out := showOne(t, writable.ID()); !strings.Contains(out, "(record withheld: it lies where a run can write)") || strings.Contains(out, "hermes-marker") {
		t.Fatalf("a Hermes store in a run's root: %q", out)
	}
	if out := showOne(t, scan.ID()); !strings.Contains(out, "database rows cannot be read") || strings.Contains(out, "hermes-marker") {
		t.Fatalf("a scan's reference into the Hermes store: %q", out)
	}
	t.Setenv("MOMENTUM_NOW", time.Now().UTC().Format(time.RFC3339Nano))
	if out := showOne(t, result.ID()); !strings.Contains(out, "historical Hermes message rows are unavailable") || strings.Contains(out, "hermes-marker") {
		t.Fatalf("as-of Hermes row: %q", out)
	}
}
