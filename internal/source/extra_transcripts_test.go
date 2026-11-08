package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/pkg/evidence"
	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
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

// kimiState writes the state.json of the Kimi session that holds the agent wire at path.
func kimiState(t *testing.T, wire, state string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(wire))), "state.json"), []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
}

func kimiPrompt(text string) map[string]any {
	return kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": []any{map[string]any{"type": "text", "text": text}}})
}

func appendJSONLines(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, row := range rows {
		if err := json.NewEncoder(f).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

// promptActors maps each user prompt's text to the actor and kind it was stored with.
func promptActors(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range all(t, st, "transcripts") {
		if strings.HasPrefix(e.Summary, "marker-7f3a") {
			out[e.Summary] = string(e.Actor) + " " + e.Kind
		}
	}
	return out
}

// `kimi -p` gives its prompt a user origin, as a person's typing has, and records no lastPrompt in the session's
// metadata. The prompt that started such a run is a program's; a session whose metadata cannot be read credits it
// to no one.
func TestKimiHeadlessPromptIsNotTheOwners(t *testing.T) {
	for _, tc := range []struct {
		name, state, want string
	}{
		{"headless", `{"version":2,"cwd":"/w"}`, "harness harness_text"},
		{"no metadata", "", "unknown harness_text"},
		{"unreadable metadata", "{", "unknown harness_text"},
		// Kimi reads a lastPrompt that is not a string as none.
		{"lastPrompt not a string", `{"version":2,"lastPrompt":7}`, "harness harness_text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiPrompt("marker-7f3a review"), kimiRecord("turn.ended", map[string]any{}))
			if tc.state != "" {
				kimiState(t, path, tc.state)
			}
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := promptActors(t, st); len(got) != 1 || got["marker-7f3a review"] != tc.want {
				t.Fatalf("launcher prompt stored as %v, want %q", got, tc.want)
			}
		})
	}
}

// A person who resumes a program's run in Kimi's TUI types into it: his prompts from then on are his, and the
// prompt that started the run stays the program's.
func TestKimiInteractivePromptsStayTheOwners(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a launcher"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `{"version":2}`)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	appendJSONLines(t, path, kimiPrompt("marker-7f3a typed"),
		kimiRecord("turn.steer", map[string]any{"origin": map[string]any{"kind": "user", "inTurn": true}, "input": []any{map[string]any{"type": "text", "text": "marker-7f3a steered"}}}),
		kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
	ingest(t, src, st)
	want := map[string]string{"marker-7f3a launcher": "harness harness_text", "marker-7f3a typed": "owner owner_text",
		"marker-7f3a steered": "owner owner_text"}
	if got := promptActors(t, st); !maps.Equal(got, want) {
		t.Fatalf("prompts stored as %v, want %v", got, want)
	}
}

// A cursor saved before the rule holds a run's launcher prompt as the owner's: the file is read again once, its
// records replaced, and an interactive session keeps its one owner turn.
func TestKimiCursorFromBeforeTheRuleReadsAgain(t *testing.T) {
	home := t.TempDir()
	run := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
	typed := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
	writeJSONLines(t, run, kimiPrompt("marker-7f3a launcher"), kimiRecord("turn.ended", map[string]any{}))
	writeJSONLines(t, typed, kimiPrompt("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, typed, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
	// What the reader before the rule stored: the launcher prompt as the owner's, under a cursor with no rule.
	kimiState(t, run, `{"version":2,"lastPrompt":"marker-7f3a launcher"}`)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	ctx := context.Background()
	cursors, err := st.FileCursors(ctx, src.Name())
	if err != nil || len(cursors) != 2 {
		t.Fatalf("cursors %d, %v", len(cursors), err)
	}
	var old []store.FileWrite
	for _, cur := range cursors {
		var state map[string]any
		if err := json.Unmarshal(cur.State, &state); err != nil {
			t.Fatal(err)
		}
		delete(state, "Rule")
		cur.State, _ = json.Marshal(state)
		old = append(old, store.FileWrite{Cursor: cur})
	}
	if _, _, err := st.PutFiles(ctx, old); err != nil {
		t.Fatal(err)
	}
	kimiState(t, run, `{"version":2}`)
	// The files read again are counted, as a file read again for any other reason is.
	if r := ingest(t, src, st); !slices.Contains(r.Notes, "files read again from the start (replaced, cut, rewritten or read under an older rule): 2") {
		t.Fatalf("the read again is silent: %q", r.Notes)
	}
	want := map[string]string{"marker-7f3a launcher": "harness harness_text", "marker-7f3a typed": "owner owner_text"}
	if got := promptActors(t, st); !maps.Equal(got, want) {
		t.Fatalf("prompts stored as %v, want %v", got, want)
	}
	if n := len(all(t, st, "transcripts")); n != 4 {
		t.Fatalf("reading again stored %d events, want 4", n)
	}
	if r := ingest(t, src, st); r.Read != 0 {
		t.Fatalf("a cursor under the rule read again: %+v", r)
	}
}

// Undoing every prompt takes a session's lastPrompt away. A session with none whose wire holds an undo may be a
// person's, so its prompts are credited to no one.
func TestKimiUndoneSessionIsUnknown(t *testing.T) {
	for _, undo := range []string{"context.undo", "context.undone"} {
		t.Run(undo, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiPrompt("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}),
				kimiRecord(undo, map[string]any{"agentId": "main", "count": 1}))
			kimiState(t, path, `{"version":2}`)
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := promptActors(t, st); len(got) != 1 || got["marker-7f3a typed"] != "unknown harness_text" {
				t.Fatalf("an undone session's prompt stored as %v", got)
			}
		})
	}
}

// A wire the undo check cannot read may hold an undo: it is not taken for a wire without one. Only a race (the
// file gone or replaced between the read of its prompt and the check) gets here, so the check is tested alone.
func TestKimiUnreadableWireIsNotNoUndo(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.jsonl")
	writeJSONLines(t, plain, kimiPrompt("marker-7f3a"), kimiRecord("turn.ended", map[string]any{}))
	undone := filepath.Join(dir, "undone.jsonl")
	writeJSONLines(t, undone, kimiPrompt("marker-7f3a"), kimiRecord("context.undo", map[string]any{"agentId": "main", "count": 1}))
	for path, want := range map[string]bool{plain: false, undone: true, filepath.Join(dir, "gone.jsonl"): true, dir: true} {
		if got := kimiUndone(path); got != want {
			t.Fatalf("%s: undone %v, want %v", filepath.Base(path), got, want)
		}
	}
}

// Kimi keeps an older session's metadata in its session-meta directory. Its lastPrompt counts when the session has
// no state.json, and when its state.json holds none.
func TestKimiLegacySessionMetaIsRead(t *testing.T) {
	for _, state := range []string{"", `{"version":2}`} {
		t.Run(fmt.Sprintf("state.json %q", state), func(t *testing.T) {
			home := t.TempDir()
			session := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed")
			path := filepath.Join(session, "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiPrompt("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}))
			if state != "" {
				kimiState(t, path, state)
			}
			if err := os.MkdirAll(filepath.Join(session, "session-meta"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(session, "session-meta", "state.json"), []byte(`{"version":1,"lastPrompt":"marker-7f3a typed"}`), 0600); err != nil {
				t.Fatal(err)
			}
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := promptActors(t, st); len(got) != 1 || got["marker-7f3a typed"] != "owner owner_text" {
				t.Fatalf("a prompt of a session with older metadata stored as %v", got)
			}
		})
	}
}

// The undo check reads a wire held only by its compressed copy: a program's run stays the program's, and an undone
// session is still unknown.
func TestKimiCompressedWireIsCheckedForUndo(t *testing.T) {
	for _, tc := range []struct {
		name string
		undo bool
		want string
	}{{"headless", false, "harness harness_text"}, {"undone", true, "unknown harness_text"}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
			rows := []map[string]any{kimiPrompt("marker-7f3a launcher"), kimiRecord("turn.ended", map[string]any{})}
			if tc.undo {
				rows = append(rows, kimiRecord("context.undo", map[string]any{"agentId": "main", "count": 1}))
			}
			writeJSONLines(t, path, rows...)
			kimiState(t, path, `{"version":2}`)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			enc, err := zstd.NewWriter(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".zst", enc.EncodeAll(raw, nil), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := promptActors(t, st); len(got) != 1 || got["marker-7f3a launcher"] != tc.want {
				t.Fatalf("a compressed wire's launcher prompt stored as %v, want %q", got, tc.want)
			}
		})
	}
}

// A native file replaced or cut since its cursor is read from its start, and the ingest counts it, as it counts
// tacit's.
func TestNativeRestartIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]any
	}{
		{"cut", []map[string]any{kimiPrompt("marker-7f3a typed")}},
		{"replaced", []map[string]any{kimiPrompt("marker-7f3a other"), kimiRecord("turn.ended", map[string]any{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiPrompt("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}),
				kimiRecord("context.append_loop_event", map[string]any{"event": map[string]any{"type": "content.part", "part": map[string]any{"type": "text", "text": "marker-7f3a done"}}}))
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			st := openStore(t)
			src := transcripts(home)
			if r := ingest(t, src, st); len(r.Notes) != 0 {
				t.Fatalf("a first read noted %q", r.Notes)
			}
			writeJSONLines(t, path, tc.rows...)
			if r := ingest(t, src, st); !slices.Contains(r.Notes, "files read again from the start (replaced, cut, rewritten or read under an older rule): 1") {
				t.Fatalf("a file %s since its cursor was read again silently: %q", tc.name, r.Notes)
			}
		})
	}
}

// Without metadata a user-origin prompt is credited to no one; a task's notice is the agent's own input still.
func TestKimiTaskPromptStaysGeneratedWithoutMetadata(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a launcher"),
		kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "task", "taskId": "task-1"},
			"input": []any{map[string]any{"type": "text", "text": "marker-7f3a task"}}}),
		kimiRecord("turn.ended", map[string]any{}))
	st := openStore(t)
	ingest(t, transcripts(home), st)
	want := map[string]string{"marker-7f3a launcher": "unknown harness_text", "marker-7f3a task": "harness harness_text"}
	if got := promptActors(t, st); !maps.Equal(got, want) {
		t.Fatalf("prompts stored as %v, want %v", got, want)
	}
}

// Each read reads the metadata again: once a session's state.json is gone, a prompt read later is credited to no
// one, though an earlier read credited the owner.
func TestKimiProvenanceIsReadAgainEachRead(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	if err := os.Remove(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "state.json")); err != nil {
		t.Fatal(err)
	}
	appendJSONLines(t, path, kimiPrompt("marker-7f3a later"), kimiRecord("turn.ended", map[string]any{}))
	ingest(t, src, st)
	want := map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a later": "unknown harness_text"}
	if got := promptActors(t, st); !maps.Equal(got, want) {
		t.Fatalf("prompts stored as %v, want %v", got, want)
	}
}

// The verdict is fixed at a read's first user-origin prompt: a session whose metadata gains a lastPrompt while it is
// read (a person resumed a kimi -p run) is the program's for the rest of that read.
func TestKimiProvenanceIsReadOncePerRead(t *testing.T) {
	old := chunkBytes
	chunkBytes = 1 // a part a line, so the metadata changes between the two prompts
	defer func() { chunkBytes = old }()
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a launcher"), kimiPrompt("marker-7f3a typed"))
	kimiState(t, path, `{"version":2}`)
	j, ok := fileJob(path, evidence.Claude, nil)
	if !ok {
		t.Fatal("no job for the wire")
	}
	j.native = &nativeJob{harness: "kimi"}
	var kinds []evidence.Kind
	tally := &tally{failed: map[string]bool{}}
	transcripts("").readNativeJob(context.Background(), j, tally, func(p part) bool {
		for _, it := range p.items {
			kinds = append(kinds, it.e.Kind)
		}
		kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
		return true
	})
	if want := []evidence.Kind{evidence.KindHarnessText, evidence.KindHarnessText}; !slices.Equal(kinds, want) || len(tally.failed) != 0 {
		t.Fatalf("prompts read as %v, want %v (%v)", kinds, want, tally.failed)
	}
}

// A cursor that read nothing (a first line still being written) credited nothing: completing the line under the new
// rule is a first read, not one counted as read again.
func TestKimiCursorThatReadNothingIsNotCounted(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
	raw, err := json.Marshal(kimiPrompt("marker-7f3a typed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0600); err != nil {
		t.Fatal(err)
	}
	kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	ctx := context.Background()
	cursors, err := st.FileCursors(ctx, src.Name())
	if err != nil || len(cursors) != 1 {
		t.Fatalf("cursors %d, %v", len(cursors), err)
	}
	var old []store.FileWrite
	for _, cur := range cursors {
		if cur.Offset != 0 {
			t.Fatalf("a partial first line moved the cursor to %d", cur.Offset)
		}
		var state map[string]any
		if err := json.Unmarshal(cur.State, &state); err != nil {
			t.Fatal(err)
		}
		delete(state, "Rule")
		cur.State, _ = json.Marshal(state)
		old = append(old, store.FileWrite{Cursor: cur})
	}
	if _, _, err := st.PutFiles(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	r := ingest(t, src, st)
	if r.New != 1 || len(r.Notes) != 0 {
		t.Fatalf("a first read of the completed line: %+v", r)
	}
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
				kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a"}`)
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
