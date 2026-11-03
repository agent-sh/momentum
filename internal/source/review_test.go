package source

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/store"
)

func TestNativeMalformedLinesAreCountedAndSkipped(t *testing.T) {
	for _, harness := range []string{"grok", "kimi"} {
		t.Run(harness, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "marker", "updates.jsonl")
			row := grokUpdate("agent_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a"}})
			if harness == "kimi" {
				path = filepath.Join(t.TempDir(), "marker", "agents", "main", "wire.jsonl")
				row = kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "content.part", "part": map[string]any{"type": "text", "text": "marker-7f3a"}}})
			}
			raw, _ := json.Marshal(row)
			data := string(raw) + "\n\n[]\nnull\n7\n\"marker-7f3a\"\n{broken}\n" + string(raw) + "\n"
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			src := transcripts("")
			src.Homes = nil
			src.Sources = []TranscriptSource{{Harness: harness, Glob: path}}
			st := openStore(t)
			r, err := src.Ingest(context.Background(), st)
			if err != nil || r.New != 2 || r.Skipped != 6 {
				t.Fatalf("malformed line stalled source: new=%d skipped=%d error=%v", r.New, r.Skipped, err)
			}
			if n, err := st.Skipped(context.Background(), store.Transcripts); err != nil || n != 6 {
				t.Fatalf("skipped records not kept: %d %v", n, err)
			}
			if r := ingest(t, src, st); r.Read != 0 || r.Skipped != 0 {
				t.Fatal("unchanged file retried malformed lines")
			}
		})
	}
}

func TestGrokSearchStatusAndShellOutput(t *testing.T) {
	for _, tc := range []struct {
		name, status, typ, output string
		code                      int
		failed, keyed             bool
	}{
		{"grep", "completed", "GrepSearch", "", 1, false, false},
		{"grep", "failed", "GrepSearch", "marker-7f3a: permission denied", 2, true, true},
		{"run_terminal_command", "completed", "Bash", "cat: notes.txt: No such file or directory", 1, true, true},
		{"run_terminal_command", "completed", "Bash", "--- FAIL: TestMarker (0.01s) exit status 1", 1, true, false},
	} {
		t.Run(tc.name+"/"+tc.status+"/"+tc.typ, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".grok", "sessions", "neutral", "marker", "updates.jsonl")
			writeJSONLines(t, path,
				grokUpdate("tool_call", map[string]any{"toolCallId": "call-1", "_meta": map[string]any{"x.ai/tool": map[string]any{"name": tc.name}}, "rawInput": map[string]any{"command": "cat notes.txt"}}),
				grokUpdate("tool_call_update", map[string]any{"toolCallId": "call-1", "status": tc.status, "rawOutput": map[string]any{"type": tc.typ, "exit_code": tc.code, "match_count": 0, "error": nil, "output": tc.output}}))
			st := openStore(t)
			ingest(t, transcripts(home), st)
			evs := all(t, st, store.Transcripts)
			if len(evs) != 2 {
				t.Fatalf("call/result missing: %d events", len(evs))
			}
			for _, e := range evs {
				if e.Kind != "tool_result" {
					continue
				}
				if strings.Contains(e.Meta, `"is_error":true`) != tc.failed {
					t.Fatalf("native status wrong: %s", e.Meta)
				}
				if tc.typ == "Bash" && e.Summary != tc.output {
					t.Fatalf("shell output was not extracted: %q", e.Summary)
				}
				k, ok := detect.FailureOf(e.Harness, e.Tool, e.Summary, `{"command":"cat notes.txt"}`)
				if ok != tc.keyed || ok && tc.typ == "Bash" && k.Origin != "exec" {
					t.Fatalf("wrong key: %+v %t", k, ok)
				}
			}
		})
	}
}

func TestClaudeNativeFlagSurvivesContent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "neutral", "marker-7f3a.jsonl")
	writeJSONLines(t, path,
		map[string]any{"type": "assistant", "timestamp": "2026-05-29T10:00:00Z", "sessionId": "marker-7f3a",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "Read", "input": map[string]any{"file_path": "notes.txt"}}}}},
		map[string]any{"type": "user", "timestamp": "2026-05-29T10:00:01Z", "sessionId": "marker-7f3a",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "Error: marker-7f3a says exit code 1", "is_error": false}}}})
	st := openStore(t)
	ingest(t, transcripts(home), st)
	evs := all(t, st, store.Transcripts)
	if len(evs) != 2 {
		t.Fatalf("call/result missing: %d events", len(evs))
	}
	for _, e := range evs {
		if e.Kind == "tool_result" && strings.Contains(e.Meta, `"is_error":true`) {
			t.Fatal("content overrode Claude's native success flag")
		}
	}
}

func TestOpenCodeCompletedContentKeepsNativeStatus(t *testing.T) {
	home := t.TempDir()
	db, _ := openCodeFixture(t, home)
	if _, err := db.Exec(`UPDATE session_message SET data='{"content":[{"type":"tool","id":"call-1","name":"read","state":{"status":"completed","input":{"marker":{"exit_code":1}},"output":"Error: marker-7f3a says exit code 1"}}]}' WHERE id='message-2'`); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	ingest(t, transcripts(home), st)
	evs := all(t, st, store.Transcripts)
	if len(evs) != 3 {
		t.Fatalf("user/call/result missing: %d events", len(evs))
	}
	for _, e := range evs {
		if e.Kind == "tool_result" && strings.Contains(e.Meta, `"is_error":true`) {
			t.Fatal("input or content overrode completed status")
		}
	}
}

func TestKimiNativeErrorWithoutText(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "marker", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path,
		kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "tool.call", "toolCallId": "call-1", "name": "ReadFile", "args": map[string]any{"path": "notes.txt"}}}),
		kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "tool.result", "toolCallId": "call-1", "result": map[string]any{"isError": true}}}))
	st := openStore(t)
	ingest(t, transcripts(home), st)
	evs := all(t, st, store.Transcripts)
	if len(evs) != 2 {
		t.Fatal("call/result missing")
	}
	for _, e := range evs {
		if e.Kind == "tool_result" && !strings.Contains(e.Meta, `"is_error":true`) {
			t.Fatal("Kimi native flag lost")
		}
	}
}

func TestNativeUnknownInputActor(t *testing.T) {
	for _, harness := range []string{"grok", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			home := t.TempDir()
			if harness == "opencode" {
				db, _ := openCodeFixture(t, home)
				if _, err := db.Exec(`UPDATE session_v2 SET directory='/neutral-project'`); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(home, ".grok", "sessions", "neutral", "marker", "updates.jsonl")
				writeJSONLines(t, path, grokUpdate("user_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "marker-7f3a"}}))
			}
			st := openStore(t)
			ingest(t, transcripts(home), st)
			found := false
			for _, e := range all(t, st, store.Transcripts) {
				if e.Kind == "harness_text" && e.Summary == "marker-7f3a" {
					found = true
					if e.Actor != store.ActorUnknown || !strings.Contains(e.Meta, "input origin unknown") {
						t.Fatalf("unproven input attributed: actor=%s meta=%s", e.Actor, e.Meta)
					}
				}
			}
			if !found {
				t.Fatal("unknown input missing")
			}
		})
	}
}

func TestOpenCodeFingerprintAvoidsPayloadReads(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "legacy"}[legacy], func(t *testing.T) {
			db, path := openCodeFixture(t, t.TempDir())
			query := `ALTER TABLE session_message RENAME TO marker_messages;
CREATE VIEW session_message AS SELECT id,session_id,type,seq,time_created,time_updated,json_extract('marker-7f3a','$') AS data FROM marker_messages;`
			if legacy {
				query = `DROP TABLE session_message; DROP TABLE session_v2;
CREATE TABLE session (id TEXT,parent_id TEXT,directory TEXT,time_updated INTEGER);
CREATE TABLE message (id TEXT,session_id TEXT,time_updated INTEGER,data TEXT);
CREATE TABLE marker_parts (id TEXT,session_id TEXT,time_updated INTEGER);
CREATE VIEW part AS SELECT id,session_id,time_updated,json_extract('marker-7f3a','$') AS data FROM marker_parts;
INSERT INTO session VALUES ('marker','','',1);
INSERT INTO message VALUES ('message-1','marker',1,'{"role":"assistant"}');
INSERT INTO marker_parts VALUES ('part-1','marker',1);`
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			// The views make body access fail while metadata stays readable.
			// Exercise fingerprinting independently of schema discovery.
			reader := &openCodeDB{DB: db, path: path, v2: !legacy, legacy: legacy}
			rows, err := reader.sessions(context.Background())
			if err != nil || len(rows) != 1 {
				t.Fatalf("fingerprint evaluated body column: %v", err)
			}
			before := rows[0].fingerprint
			query = `UPDATE marker_messages SET time_updated=time_updated+1 WHERE id='message-2'`
			if legacy {
				query = `UPDATE marker_parts SET time_updated=time_updated+1`
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			rows, err = reader.sessions(context.Background())
			if err != nil || rows[0].fingerprint == before {
				t.Fatalf("in-place update not detected: %v", err)
			}
			before = rows[0].fingerprint
			query = `INSERT INTO marker_messages VALUES ('message-3','session-marker-7f3a','user',3,1,1,'{"text":"marker-7f3a"}')`
			if legacy {
				query = `INSERT INTO marker_parts VALUES ('part-2','marker',1)`
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			rows, err = reader.sessions(context.Background())
			if err != nil || rows[0].fingerprint == before {
				t.Fatalf("added row not detected: %v", err)
			}
		})
	}
}

func TestOpenCodeChunkFlush(t *testing.T) {
	old := chunkBytes
	chunkBytes = 100
	defer func() { chunkBytes = old }()
	db, path := openCodeFixture(t, t.TempDir())
	for i := 3; i <= 8; i++ {
		_, err := db.Exec(`INSERT INTO session_message VALUES (?, 'session-marker-7f3a','user',?,1,1,?)`, "message-"+string(rune('0'+i)), i, `{"text":"`+strings.Repeat("marker-7f3a ", 20)+`"}`)
		if err != nil {
			t.Fatal(err)
		}
	}
	reader, err := openOpenCode(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ss, err := reader.sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j := &job{path: "opencode:" + path + "#" + ss[0].id, native: &nativeJob{harness: "opencode", db: reader, session: ss[0]}}
	var parts []part
	tally := &tally{failed: map[string]bool{}}
	transcripts("").readOpenCodeJob(context.Background(), j, tally, func(p part) bool {
		parts = append(parts, p)
		return true
	})
	if len(parts) < 3 || len(tally.failed) != 0 {
		t.Fatalf("session was not streamed: %d parts", len(parts))
	}
	items := 0
	for i, p := range parts {
		items += len(p.items)
		if (p.cur.FP != "") != (i == len(parts)-1) {
			t.Fatal("fingerprint committed before final chunk")
		}
	}
	if items != 9 {
		t.Fatalf("chunk lost items: %d", items)
	}
	// A writer that refuses the first chunk must stop the reader there.
	callbacks := 0
	transcripts("").readOpenCodeJob(context.Background(), j, tally, func(p part) bool {
		callbacks++
		return false
	})
	if callbacks != 1 {
		t.Fatalf("reader continued after refused chunk: %d callbacks", callbacks)
	}
}

func TestNativeCallNamesAndCompletedState(t *testing.T) {
	for _, harness := range []string{"grok", "kimi"} {
		t.Run(harness, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".grok", "sessions", "neutral", "marker", "updates.jsonl")
			call := grokUpdate("tool_call", map[string]any{"toolCallId": "call-1", "rawOutput": map[string]any{"name": "run_terminal_command"}, "rawInput": map[string]any{"command": "cat notes.txt"}})
			result := grokUpdate("tool_call_update", map[string]any{"toolCallId": "call-1", "status": "completed", "rawOutput": map[string]any{"type": "Bash", "exit_code": 0, "output": "marker-7f3a"}})
			want := "run_terminal_command"
			if harness == "kimi" {
				path = filepath.Join(home, ".kimi-code", "sessions", "neutral", "marker", "agents", "main", "wire.jsonl")
				call = kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "tool.call", "toolCallId": "call-1", "name": "Shell"}})
				result = kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "tool.result", "toolCallId": "call-1", "result": map[string]any{"output": "marker-7f3a"}}})
				want = "Shell"
			}
			writeJSONLines(t, path, call)
			st := openStore(t)
			src := transcripts(home)
			ingest(t, src, st)
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(f).Encode(result); err != nil {
				t.Fatal(err)
			}
			f.Close()
			ingest(t, src, st)
			for _, e := range all(t, st, store.Transcripts) {
				if e.Tool != want {
					t.Fatalf("call name lost across cursor: %q", e.Tool)
				}
			}
			cursors, err := st.FileCursors(context.Background(), store.Transcripts)
			if err != nil {
				t.Fatal(err)
			}
			for _, cur := range cursors {
				var saved nativeState
				if err := json.Unmarshal(cur.State, &saved); err != nil || len(saved.Calls) != 0 {
					t.Fatalf("completed call retained in cursor: %d %v", len(saved.Calls), err)
				}
			}
		})
	}
}
