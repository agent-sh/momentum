package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSONLines(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func grokUpdate(kind string, fields map[string]any) map[string]any {
	fields["sessionUpdate"] = kind
	return map[string]any{"timestamp": 1780000000, "method": "session/update",
		"params": map[string]any{"sessionId": "session-marker-7f3a", "update": fields}}
}

func kimiRecord(kind string, fields map[string]any) map[string]any {
	fields["type"], fields["time"] = kind, 1780000000000
	return fields
}

func TestGrokAndKimiTranscripts(t *testing.T) {
	for _, harness := range []string{"grok", "kimi"} {
		t.Run(harness, func(t *testing.T) {
			home := t.TempDir()
			var path string
			if harness == "grok" {
				path = filepath.Join(home, ".grok", "sessions", "neutral", "session-marker-7f3a", "updates.jsonl")
				writeJSONLines(t, path,
					grokUpdate("user_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a"}}),
					grokUpdate("agent_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "Checking marker-7f3a."}}),
					grokUpdate("tool_call", map[string]any{"toolCallId": "call-1", "_meta": map[string]any{"x.ai/tool": map[string]any{"name": "bash"}}, "rawInput": map[string]any{"command": "cat neutral.txt"}}),
					grokUpdate("tool_call_update", map[string]any{"toolCallId": "call-1", "status": "failed", "rawOutput": map[string]any{"content": "cat: neutral.txt: No such file or directory"}}),
					grokUpdate("turn_completed", map[string]any{}))
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "summary.json"), []byte(`{"session_kind":"tui"}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				path = filepath.Join(home, ".kimi-code", "sessions", "neutral", "session-marker-7f3a", "agents", "main", "wire.jsonl")
				writeJSONLines(t, path,
					kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": []any{map[string]any{"type": "text", "text": "marker-7f3a"}}}),
					kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "content.part", "part": map[string]any{"type": "text", "text": "Checking marker-7f3a."}}}),
					kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "tool.call", "toolCallId": "call-1", "name": "Shell", "args": map[string]any{"command": "cat neutral.txt"}}}),
					kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "tool.result", "toolCallId": "call-1", "result": map[string]any{"isError": true, "output": "cat: neutral.txt: No such file or directory"}}}),
					kimiRecord("turn.ended", map[string]any{}))
			}
			st := openStore(t)
			src := transcripts(home)
			ingest(t, src, st)
			evs := all(t, st, "transcripts")
			if len(evs) != 5 {
				t.Fatalf("got %d events, want 5", len(evs))
			}
			kinds := map[string]int{}
			for _, e := range evs {
				if e.Harness != harness {
					t.Fatalf("harness %q", e.Harness)
				}
				kinds[e.Kind]++
				if e.Kind == "tool_result" && (!strings.Contains(e.Meta, `"is_error":true`) || e.Tool == "") {
					t.Fatalf("failed result lost metadata: %+v", e)
				}
			}
			if kinds["owner_text"] != 1 || kinds["agent_text"] != 1 || kinds["tool_call"] != 1 || kinds["tool_result"] != 1 {
				t.Fatalf("kinds %v", kinds)
			}
			if r := ingest(t, src, st); r.Read != 0 || r.New != 0 {
				t.Fatalf("unchanged source read again: %+v", r)
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			row := grokUpdate("agent_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a appended"}})
			if harness == "kimi" {
				row = kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "content.part", "part": map[string]any{"type": "text", "text": "marker-7f3a appended"}}})
			}
			if err := json.NewEncoder(f).Encode(row); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if r := ingest(t, src, st); r.New != 1 {
				t.Fatalf("append: %+v", r)
			}
			if n, err := src.Gone(context.Background(), st); err != nil || n != 0 {
				t.Fatalf("present extra transcript counted gone: %d, %v", n, err)
			}
		})
	}
}

func openCodeFixture(t *testing.T, home string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE session_v2 (id TEXT, parent_id TEXT, directory TEXT, time_updated INTEGER);
CREATE TABLE session_message (id TEXT, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
INSERT INTO session_v2 VALUES ('session-marker-7f3a','','',1780000000000);
INSERT INTO session_message VALUES ('message-1','session-marker-7f3a','user',1,1780000000000,1780000000000,'{"text":"marker-7f3a"}');
INSERT INTO session_message VALUES ('message-2','session-marker-7f3a','assistant',2,1780000001000,1780000001000,'{"content":[{"type":"text","text":"Checking marker-7f3a."},{"type":"tool","id":"call-1","name":"bash","state":{"status":"running","input":{"command":"cat neutral.txt"}}}]}');`)
	if err != nil {
		t.Fatal(err)
	}
	return db, path
}

func TestOpenCodeMutableResults(t *testing.T) {
	home := t.TempDir()
	db, _ := openCodeFixture(t, home)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	if len(all(t, st, "transcripts")) != 3 {
		t.Fatal("opencode messages and call not ingested")
	}
	if _, err := db.Exec(`UPDATE session_message SET time_updated=1780000002000,
data='{"content":[{"type":"text","text":"Checking marker-7f3a."},{"type":"tool","id":"call-1","name":"bash","state":{"status":"error","input":{"command":"cat neutral.txt"},"error":{"message":"cat: neutral.txt: No such file or directory"}}}]}'
WHERE id='message-2'`); err != nil {
		t.Fatal(err)
	}
	ingest(t, src, st)
	evs := all(t, st, "transcripts")
	if len(evs) != 4 {
		t.Fatalf("updated result not read, or doubled: %d", len(evs))
	}
	for _, e := range evs {
		if e.Kind == "tool_result" && !strings.Contains(e.Meta, `"is_error":true`) {
			t.Fatal("error state lost")
		}
	}
	if r := ingest(t, src, st); r.Read != 0 {
		t.Fatalf("unchanged DB reread: %+v", r)
	}
	if n, err := src.Gone(context.Background(), st); err != nil || n != 0 {
		t.Fatalf("present opencode rows counted gone: %d %v", n, err)
	}
}

func TestOpenCodeLegacyAndMigration(t *testing.T) {
	home := t.TempDir()
	db, _ := openCodeFixture(t, home)
	_, err := db.Exec(`CREATE TABLE session (id TEXT,parent_id TEXT,directory TEXT,time_updated INTEGER);
CREATE TABLE message (id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
CREATE TABLE part (id TEXT,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
INSERT INTO session VALUES ('session-marker-7f3a','','',1780000000000),('legacy','','',1780000000000);
INSERT INTO message VALUES ('stale','session-marker-7f3a',1780000000000,1780000000000,'{"role":"user"}'),
('old-user','legacy',1780000000000,1780000000000,'{"role":"user"}'),
('old-agent','legacy',1780000001000,1780000001000,'{"role":"assistant","modelID":"neutral"}');
INSERT INTO part VALUES ('stale-part','stale','session-marker-7f3a',1780000000000,1780000000000,'{"type":"text","text":"stale-marker"}'),
('user-part','old-user','legacy',1780000000000,1780000000000,'{"type":"text","text":"marker-7f3a"}'),
('agent-part','old-agent','legacy',1780000001000,1780000001000,'{"type":"text","text":"Checking marker-7f3a."}'),
('tool-part','old-agent','legacy',1780000001000,1780000001000,'{"type":"tool","callID":"old-call","tool":"bash","state":{"status":"error","input":{"command":"cat neutral.txt"},"error":"cat: neutral.txt: No such file or directory","time":{"start":1780000001000,"end":1780000002000}}}');`)
	if err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	ingest(t, transcripts(home), st)
	evs := all(t, st, "transcripts")
	if len(evs) != 7 {
		t.Fatalf("legacy rows absent or migrated rows duplicated: %d", len(evs))
	}
	for _, e := range evs {
		if strings.Contains(e.Summary, "stale-marker") {
			t.Fatal("legacy copy of migrated session ingested")
		}
		if e.Session == "legacy" && e.Kind == "tool_result" && e.TS.UnixMilli() != 1780000002000 {
			t.Fatal("tool result used the message's start time")
		}
	}
}

func TestNativeSourceAliasesShareOneCursor(t *testing.T) {
	home := t.TempDir()
	_, path := openCodeFixture(t, home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	src := transcripts(home)
	src.Sources = []TranscriptSource{{Harness: "opencode", DB: relative}}
	st := openStore(t)
	r := ingest(t, src, st)
	cursors, err := st.FileCursors(context.Background(), "transcripts")
	if err != nil || r.Read != 3 || len(cursors) != 1 {
		t.Fatalf("same database read through two names: %d records, %d cursors, %v", r.Read, len(cursors), err)
	}
}

func TestGrokProvenanceRefreshesAtCursor(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".grok", "sessions", "neutral", "marker", "updates.jsonl")
	writeJSONLines(t, path, grokUpdate("user_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a first"}}))
	summary := filepath.Join(filepath.Dir(path), "summary.json")
	if err := os.WriteFile(summary, []byte(`{"session_kind":"headless"}`), 0600); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	for _, kind := range []string{"tui", "headless"} {
		raw, _ := json.Marshal(map[string]any{"session_kind": kind})
		if err := os.WriteFile(summary, raw, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(f).Encode(grokUpdate("user_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a " + kind}})); err != nil {
			t.Fatal(err)
		}
		f.Close()
		ingest(t, src, st)
	}
	owners := 0
	for _, e := range all(t, st, "transcripts") {
		if e.Kind == "owner_text" {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("saved provenance overrode current session kind: %d owner events", owners)
	}
}

func TestNativeAttributionAndDistinctSessions(t *testing.T) {
	home := t.TempDir()
	for _, id := range []string{"one", "two"} {
		path := filepath.Join(home, ".grok", "sessions", "neutral", id, "updates.jsonl")
		row := grokUpdate("user_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a"}})
		row["params"].(map[string]any)["sessionId"] = id
		writeJSONLines(t, path, row)
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), "summary.json"), []byte(`{"session_kind":"headless"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "one", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "task"}, "input": []any{map[string]any{"type": "text", "text": "marker-7f3a"}}}))
	child := filepath.Join(home, ".kimi-code", "sessions", "neutral", "one", "agents", "agent-0", "wire.jsonl")
	writeJSONLines(t, child, kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": []any{map[string]any{"type": "text", "text": "marker-7f3a"}}}))
	st := openStore(t)
	ingest(t, transcripts(home), st)
	evs := all(t, st, "transcripts")
	if len(evs) != 4 {
		t.Fatalf("shared filenames collided: %d events", len(evs))
	}
	for _, e := range evs {
		if e.Kind == "owner_text" {
			t.Fatal("generated or delegated prompt counted as owner")
		}
	}
	_, _ = openCodeFixture(t, home)
	ingest(t, transcripts(home), st)
	for _, e := range all(t, st, "transcripts") {
		if e.Harness == "opencode" && strings.Contains(e.Summary, "marker-7f3a") && e.Kind == "owner_text" {
			t.Fatal("role-only opencode input counted as owner")
		}
	}
}

func TestNativePartialLineAndRedactionRetry(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "marker", "agents", "main", "wire.jsonl")
	row := kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": []any{map[string]any{"type": "text", "text": "marker-7f3a"}}})
	writeJSONLines(t, path, row)
	raw, _ := json.Marshal(kimiRecord("turn.ended", map[string]any{}))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(raw[:len(raw)/2]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	st := openStore(t)
	src := transcripts(home)
	src.records = func(context.Context, []string) ([]string, error) { return nil, fmt.Errorf("synthetic mask failure") }
	if _, err := src.Ingest(context.Background(), st); err == nil || len(all(t, st, "transcripts")) != 0 {
		t.Fatal("redaction failure stored a native event or was ignored")
	}
	src = transcripts(home)
	if r := ingest(t, src, st); r.New != 1 {
		t.Fatalf("partial line: %+v", r)
	}
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(raw[len(raw)/2:]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if r := ingest(t, src, st); r.New != 1 {
		t.Fatalf("completed line without newline: %+v", r)
	}
}

func TestFailureStatusBeforeSummaryCut(t *testing.T) {
	home := t.TempDir()
	output := `{"stdout":"` + strings.Repeat("x", 760) + ` cat: neutral.txt: No such file or directory","exit_code":1}`
	path := filepath.Join(home, ".codex", "sessions", "2026", "05", "29", "marker-7f3a.jsonl")
	record := func(kind string, payload map[string]any) map[string]any {
		return map[string]any{"timestamp": "2026-05-29T10:00:00Z", "type": kind, "payload": payload}
	}
	writeJSONLines(t, path,
		record("session_meta", map[string]any{"id": "marker-7f3a", "originator": "codex-tui", "source": "cli", "thread_source": "user"}),
		record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call-1", "arguments": `{"cmd":"cat neutral.txt"}`}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "call-1", "output": output}))
	st := openStore(t)
	ingest(t, transcripts(home), st)
	found := false
	for _, e := range all(t, st, "transcripts") {
		if e.Kind == "tool_result" {
			found = true
			if !strings.Contains(e.Meta, `"is_error":true`) || !strings.Contains(e.Summary, "No such file or directory") {
				t.Fatal("native exit beyond the normal summary cut was lost")
			}
		}
	}
	if !found {
		t.Fatal("no result")
	}
}

// A long command keeps the directory it runs in: where a call acts goes first in the stored cut, for every harness.
func TestALongCommandKeepsItsWorkdirInTheStoredCall(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "sessions", "2026", "05", "29", "marker-7f3a.jsonl")
	record := func(kind string, payload map[string]any) map[string]any {
		return map[string]any{"timestamp": "2026-05-29T10:00:00Z", "type": kind, "payload": payload}
	}
	cmd := "go test ./... -run " + strings.Repeat("Neutral", 40) + " && echo ok"
	writeJSONLines(t, path,
		record("session_meta", map[string]any{"id": "marker-7f3a", "originator": "codex-tui", "source": "cli", "thread_source": "user"}),
		record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call-1",
			"arguments": `{"cmd":"` + cmd + `","workdir":"/w/tree","yield_time_ms":1000}`}))
	st := openStore(t)
	ingest(t, transcripts(home), st)
	found := false
	for _, e := range all(t, st, "transcripts") {
		if e.Kind == "tool_call" {
			found = true
			if !strings.HasPrefix(e.Summary, `{"workdir":"/w/tree","cmd":"go test ./... -run Neutral`) || !strings.HasSuffix(e.Summary, "…") {
				t.Fatalf("the stored call lost its workdir: %q", e.Summary)
			}
		}
	}
	if !found {
		t.Fatal("no call")
	}
}
