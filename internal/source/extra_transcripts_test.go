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

// kimiOldRule makes every saved cursor of src look as a build before the Kimi rule saved it: no Rule in its state.
// It returns the cursors.
func kimiOldRule(t *testing.T, st *store.Store, src Transcripts) map[string]store.FileCursor {
	t.Helper()
	ctx := context.Background()
	cursors, err := st.FileCursors(ctx, src.Name())
	if err != nil {
		t.Fatal(err)
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
	return cursors
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
	if cursors := kimiOldRule(t, st, src); len(cursors) != 2 {
		t.Fatalf("cursors %d, want 2", len(cursors))
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

// A native file replaced, cut or rewritten since its cursor is read from its start, and the ingest counts it, as it
// counts tacit's.
func TestNativeRestartIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]any
	}{
		{"cut", []map[string]any{kimiPrompt("marker-7f3a typed")}},
		{"rewritten", []map[string]any{kimiPrompt("marker-7f3a other"), kimiRecord("turn.ended", map[string]any{})}},
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

// The metadata is read after the line of the read's first user-origin prompt, not as the read starts: a client
// records a prompt's lastPrompt before it writes the prompt, so a person who types while a read runs is credited
// with the prompt the read then reaches.
func TestKimiProvenanceIsReadAfterThePromptsLine(t *testing.T) {
	old := chunkBytes
	chunkBytes = 1 // a part a line, so the person types after the read has started
	defer func() { chunkBytes = old }()
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiRecord("profile.bind", map[string]any{"modelAlias": "neutral"}))
	kimiState(t, path, `{"version":2}`)
	j, ok := fileJob(path, evidence.Claude, nil)
	if !ok {
		t.Fatal("no job for the wire")
	}
	j.native = &nativeJob{harness: "kimi"}
	var kinds []evidence.Kind
	typed := false
	tally := &tally{failed: map[string]bool{}}
	transcripts("").readNativeJob(context.Background(), j, tally, func(p part) bool {
		for _, it := range p.items {
			kinds = append(kinds, it.e.Kind)
		}
		if !typed {
			typed = true
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			appendJSONLines(t, path, kimiPrompt("marker-7f3a typed"))
		}
		return true
	})
	if want := []evidence.Kind{evidence.KindOwnerText}; !slices.Equal(kinds, want) || len(tally.failed) != 0 {
		t.Fatalf("the prompt typed during the read read as %v, want %v (%v)", kinds, want, tally.failed)
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
	cursors := kimiOldRule(t, st, src)
	if len(cursors) != 1 {
		t.Fatalf("cursors %d, want 1", len(cursors))
	}
	for _, cur := range cursors {
		if cur.Offset != 0 {
			t.Fatalf("a partial first line moved the cursor to %d", cur.Offset)
		}
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	r := ingest(t, src, st)
	if r.New != 1 || len(r.Notes) != 0 {
		t.Fatalf("a first read of the completed line: %+v", r)
	}
}

// A file that cannot be opened is not read, so it is not counted as read again, though its cursor is from before
// the rule.
func TestKimiFileThatCannotOpenIsNotCountedAsReadAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file of any mode")
	}
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	kimiOldRule(t, st, src)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0600) })
	r, err := src.Ingest(context.Background(), st)
	if err == nil {
		t.Fatal("a wire that cannot be opened did not fail the ingest")
	}
	for _, n := range r.Notes {
		if strings.Contains(n, "read again from the start") {
			t.Fatalf("a wire that was not read counted as read again: %q", r.Notes)
		}
	}
}

// An unchanged compressed wire is not read again: its final cursor keeps no state, so the rule's check leaves it
// alone.
func TestKimiCompressedWireIsNotReadAgain(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a launcher"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a launcher"}`)
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
	src := transcripts(home)
	ingest(t, src, st)
	if r := ingest(t, src, st); r.Read != 0 || len(r.Notes) != 0 {
		t.Fatalf("an unchanged compressed wire read again: %+v", r)
	}
}

// A state.json that holds JSON null is no metadata: the session's provenance is unknown.
func TestKimiNullStateIsUnknown(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "run", "agents", "main", "wire.jsonl")
	writeJSONLines(t, path, kimiPrompt("marker-7f3a launcher"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `null`)
	st := openStore(t)
	ingest(t, transcripts(home), st)
	if got := promptActors(t, st); len(got) != 1 || got["marker-7f3a launcher"] != "unknown harness_text" {
		t.Fatalf("a prompt of a session whose state.json is null stored as %v", got)
	}
}

// kimiSkillPart is a skill's rendered body as Kimi's promptWithSkills puts it in a prompt's input: a text part
// marked with meta.source "skill activation".
func kimiSkillPart(text string) map[string]any {
	return map[string]any{"type": "text", "text": text, "meta": map[string]any{"source": "skill activation", "activationId": "activation-1"}}
}

// kimiSkillOrigin is the user origin of a prompt that bundles one skill activation.
func kimiSkillOrigin(fields map[string]any) map[string]any {
	fields["kind"] = "user"
	fields["skillActivations"] = []any{map[string]any{"activationId": "activation-1", "skillName": "review"}}
	return fields
}

// ownerTexts are the texts of the owner turns stored, sorted.
func ownerTexts(t *testing.T, st *store.Store) []string {
	t.Helper()
	var out []string
	for _, e := range all(t, st, "transcripts") {
		if e.Kind == "owner_text" {
			out = append(out, e.Summary)
		}
	}
	slices.Sort(out)
	return out
}

// A skill a client sends inline with a prompt (Kimi's promptWithSkills: the TUI's inline skill mention, the web UI's
// skills field) renders its body into the prompt's input, ahead of what was typed. The body is generated input, as
// a slash command's activation is, whoever sent the prompt; only the rest is the prompt's words, credited as before.
// A prompt of the owner's that the skill leaves none of his words is still his turn, with empty text, and his turns
// keep the marker of the turn before them. A prompt with nothing generated in it is read as before, but one that
// holds no text at all (an image alone) is his turn with empty text too.
func TestKimiInlineSkillIsGeneratedInput(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	image := map[string]any{"type": "image_url", "imageUrl": map[string]any{"url": "file:///neutral.png"}}
	split := map[string]string{"marker-7f3a skill body": "harness harness_text", "marker-7f3a typed": "owner owner_text"}
	body := map[string]string{"marker-7f3a skill body": "harness harness_text"}
	typed, headless := `{"version":2,"lastPrompt":"marker-7f3a typed"}`, `{"version":2}`
	inline := kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
		"input": []any{kimiSkillPart("marker-7f3a skill body"), text("marker-7f3a typed")}})
	only := kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
		"input": []any{kimiSkillPart("marker-7f3a skill body"), image}})
	for _, tc := range []struct {
		name   string
		state  string // the session's state.json; "" for none
		record map[string]any
		want   map[string]string
		owner  []string
	}{
		{"inline skill", typed, inline, split, []string{"marker-7f3a typed"}},
		{"steered with a skill", typed, kimiRecord("turn.steer", map[string]any{"origin": kimiSkillOrigin(map[string]any{"inTurn": true}),
			"input": []any{kimiSkillPart("marker-7f3a skill body"), text("marker-7f3a typed")}}), split, []string{"marker-7f3a typed"}},
		{"skill only", typed, only, body, []string{""}},
		// A program's run has no turn of the owner's, and in a session of unknown provenance the words are no one's;
		// the skill's body is generated in both.
		{"skill only in a program's run", headless, only, body, nil},
		{"skill only with no metadata", "", only, body, nil},
		{"inline skill with no metadata", "", inline,
			map[string]string{"marker-7f3a skill body": "harness harness_text", "marker-7f3a typed": "unknown harness_text"}, nil},
		// A wire from before Kimi marked a skill's part holds the body as the prompt's first unmarked text part.
		{"unmarked skill of an older wire", typed, kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
			"input": []any{text("marker-7f3a skill body"), text("marker-7f3a typed")}}), split, []string{"marker-7f3a typed"}},
		// The fallback takes the first unmarked text parts, after any image or marked part before them.
		{"unmarked skill after an image", typed, kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
			"input": []any{image, text("marker-7f3a skill body"), text("marker-7f3a typed")}}), split, []string{"marker-7f3a typed"}},
		{"unmarked skill after a marked hook", typed, kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
			"input": []any{map[string]any{"type": "text", "text": "hook output", "meta": map[string]any{"source": "user prompt submit hook"}},
				text("marker-7f3a skill body"), text("marker-7f3a typed")}}), split, []string{"marker-7f3a typed"}},
		// Words tacit's own rule takes from him (a turn that opens with a tag is the harness's) are read as on main:
		// the skill does not add an empty turn of his for them.
		{"skill and words tacit takes", typed, kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
			"input": []any{kimiSkillPart("marker-7f3a skill body"), text("<neutral>marker-7f3a tagged</neutral>")}}), body, nil},
		// The older-wire fallback reads a list of parts: input that is not one is his words.
		{"input that is not a list", typed, kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
			"input": "marker-7f3a typed"}), map[string]string{"marker-7f3a typed": "owner owner_text"}, []string{"marker-7f3a typed"}},
		// Nothing generated and no text at all: his turn still, with empty text.
		{"media only", typed, kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": []any{image}}),
			map[string]string{}, []string{""}},
		{"media only, steered", typed, kimiRecord("turn.steer", map[string]any{"origin": map[string]any{"kind": "user", "inTurn": true}, "input": []any{image}}),
			map[string]string{}, []string{""}},
		{"plain prompt", typed, kimiPrompt("marker-7f3a typed"), map[string]string{"marker-7f3a typed": "owner owner_text"}, []string{"marker-7f3a typed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiRecord("turn.ended", map[string]any{}), tc.record, kimiRecord("turn.ended", map[string]any{}))
			if tc.state != "" {
				kimiState(t, path, tc.state)
			}
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, "marker-7f3a skill body", "marker-7f3a typed"); !maps.Equal(got, tc.want) {
				t.Fatalf("prompt stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, tc.owner) {
				t.Fatalf("owner turns %q, want %q", got, tc.owner)
			}
			for _, e := range all(t, st, "transcripts") {
				if e.Summary == "marker-7f3a skill body" && !strings.Contains(e.Meta, `"why":"generated input"`) {
					t.Fatalf("the skill's body stored without its reason: %q", e.Meta)
				}
				if e.Kind == "owner_text" && !strings.Contains(e.Meta, `"prior":"task_complete"`) {
					t.Fatalf("an owner turn after a turn's end lost its marker: %q", e.Meta)
				}
			}
		})
	}
}

// actorsOf maps each label to the actor and kind of the stored event whose text holds it.
func actorsOf(t *testing.T, st *store.Store, labels ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range all(t, st, "transcripts") {
		for _, label := range labels {
			if strings.Contains(e.Summary, label) {
				out[label] = string(e.Actor) + " " + e.Kind
			}
		}
	}
	return out
}

// Kimi marks each text part of a prompt that does not come from the person with a meta.source. A UserPromptSubmit
// hook's output is one, at the input's start ("user prompt submit hook"): generated input, as a skill's body is, and
// so is a part with a meta.source Kimi may add later. Only the parts with none are the owner's.
func TestKimiHookOutputIsGeneratedInput(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	sourced := func(s, source string) map[string]any {
		return map[string]any{"type": "text", "text": s, "meta": map[string]any{"source": source}}
	}
	hook := map[string]any{"type": "text", "text": "<hook_result hook_event=\"UserPromptSubmit\">\nmarker-7f3a hook output\n</hook_result>",
		"meta": map[string]any{"contentType": "text/xml", "source": "user prompt submit hook"}}
	labels := []string{"marker-7f3a hook output", "marker-7f3a skill body", "marker-7f3a other", "marker-7f3a typed"}
	for _, tc := range []struct {
		name    string
		origin  map[string]any
		input   []any
		want    map[string]string
		strange int // parts whose meta.source Kimi is not known to write, counted in a note
	}{
		{"hook output", map[string]any{"kind": "user"}, []any{hook, text("marker-7f3a typed")},
			map[string]string{"marker-7f3a hook output": "harness harness_text", "marker-7f3a typed": "owner owner_text"}, 0},
		{"hook output and an inline skill", kimiSkillOrigin(map[string]any{}), []any{hook, kimiSkillPart("marker-7f3a skill body"), text("marker-7f3a typed")},
			map[string]string{"marker-7f3a hook output": "harness harness_text", "marker-7f3a skill body": "harness harness_text",
				"marker-7f3a typed": "owner owner_text"}, 0},
		{"another meta.source", map[string]any{"kind": "user"}, []any{sourced("marker-7f3a other", "a later source"), text("marker-7f3a typed")},
			map[string]string{"marker-7f3a other": "harness harness_text", "marker-7f3a typed": "owner owner_text"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiRecord("turn.prompt", map[string]any{"origin": tc.origin, "input": tc.input}),
				kimiRecord("turn.ended", map[string]any{}))
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			st := openStore(t)
			r := ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, labels...); !maps.Equal(got, tc.want) {
				t.Fatalf("prompt stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, []string{"marker-7f3a typed"}) {
				t.Fatalf("owner turns %q, want only the typed words", got)
			}
			var counted, want []string
			for _, n := range r.Notes {
				if strings.HasPrefix(n, "Kimi prompt text parts") {
					counted = append(counted, n)
				}
			}
			if tc.strange > 0 {
				want = []string{fmt.Sprintf("Kimi prompt text parts with a meta.source it is not known to write, read as generated input: %d", tc.strange)}
			}
			if !slices.Equal(counted, want) {
				t.Fatalf("notes %q, want %q", counted, want)
			}
		})
	}
}

// Kimi's clients write two blocks for the model into a person's prompt, tagged system: the image compression note, a
// text part of its own before its image, and VS Code's editor context, at the start of a list prompt's first text
// part or at the end of a string prompt, which reaches the wire as one text part. Where they write them they are
// generated input; the rest of the part is his. Any other system block, and a known one anywhere else, is text he
// typed or pasted and stays his. A prompt the blocks leave none of his words is still his turn, with empty text.
// The tags are built from parts so this file holds none.
func TestKimiSystemBlocksAreGeneratedInput(t *testing.T) {
	open, end := "<"+"system>", "</"+"system>"
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	image := map[string]any{"type": "image_url", "imageUrl": map[string]any{"url": "file:///neutral.png"}}
	noteText := open + "Image compressed to fit model limits: marker-7f3a note, original 4000x3000 -> sent 2000x1500. Fine detail may be lost." + end
	note := text(noteText)
	context := open + "Editor context (use only if relevant to user's query): marker-7f3a context.go:12." + end + "\n"
	pasted := open + "You are marker-7f3a pasted." + end
	pastedContext := open + "Editor context (use only if relevant to user's query): marker-7f3a pasted.go:3." + end + "\n"
	labels := []string{"marker-7f3a note", "marker-7f3a context", "marker-7f3a typed", "marker-7f3a pasted", "marker-7f3a after"}
	note2 := map[string]string{"marker-7f3a note": "harness harness_text", "marker-7f3a typed": "owner owner_text"}
	split := map[string]string{"marker-7f3a context": "harness harness_text", "marker-7f3a typed": "owner owner_text"}
	hook := map[string]any{"type": "text", "text": "hook output", "meta": map[string]any{"source": "user prompt submit hook"}}
	pastedNote := map[string]string{"marker-7f3a note": "harness harness_text", "marker-7f3a after": "harness harness_text"}
	for _, tc := range []struct {
		name  string
		input []any
		want  map[string]string
		owner []string
	}{
		{"image note part", []any{text("marker-7f3a typed"), note, image}, note2, []string{"marker-7f3a typed"}},
		{"image note only", []any{note, image}, map[string]string{"marker-7f3a note": "harness harness_text"}, []string{""}},
		// VS Code sends a list only with media attached, and puts the editor context at the start of its first text part.
		{"editor context at the start of a part", []any{text(context + "marker-7f3a typed"), image}, split, []string{"marker-7f3a typed"}},
		{"editor context at the end of a part", []any{text("marker-7f3a typed\n" + context)}, split, []string{"marker-7f3a typed"}},
		// A hook's part ahead of a list prompt does not move the editor context off its first text part.
		{"editor context after a hook's part", []any{hook, text(context + "marker-7f3a typed"), image}, split, []string{"marker-7f3a typed"}},
		// Only the editor context is split off the start of the first text part: a note there is no client's, so the
		// part is not split and is left to tacit, as on main, whose rule makes a turn that opens with a tag the
		// harness's whole. Split, the words after the note would be an owner turn.
		{"a note opening his first part", []any{text(noteText + " marker-7f3a after")}, pastedNote, nil},
		{"a note opening his first part before an image", []any{text(noteText + " marker-7f3a after"), image}, pastedNote, nil},
		// A block runs to the next closing tag: a later one on the line is his.
		{"his words with a closing tag after an editor context", []any{text(open + "Editor context (use only if relevant to user's query): marker-7f3a context.go:12." +
			end + "marker-7f3a typed " + end), image}, split, []string{"marker-7f3a typed " + end}},
		// A string prompt gets the editor context only at its end: one at its start is his paste. It is not split off, and
		// is left to tacit as any block opening his first part is. VS Code's own at the end is still generated.
		{"an editor context pasted at the start of a string prompt", []any{text(pastedContext + "marker-7f3a typed")},
			map[string]string{"marker-7f3a pasted": "harness harness_text", "marker-7f3a typed": "harness harness_text"}, nil},
		{"a pasted editor context and VS Code's closing a string prompt", []any{text(pastedContext + "marker-7f3a typed\n" + context)},
			map[string]string{"marker-7f3a pasted": "harness harness_text", "marker-7f3a typed": "harness harness_text",
				"marker-7f3a context": "harness harness_text"}, nil},
		{"a pasted note at the end of his part", []any{text("marker-7f3a typed " + noteText)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text"}, []string{"marker-7f3a typed " + noteText}},
		{"an editor context opening his second part", []any{text("marker-7f3a typed"), text(context + "marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a context": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + strings.TrimSpace(context) + " marker-7f3a after"}},
		{"a note opening his second part", []any{text("marker-7f3a typed"), text(noteText + " marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + noteText + " marker-7f3a after"}},
		// A list prompt gets the editor context at its start, so one closing a part of a list is his.
		{"an editor context closing a part of a list prompt", []any{text("marker-7f3a typed\n" + context), image},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a context": "owner owner_text"},
			[]string{"marker-7f3a typed " + strings.TrimSpace(context)}},
		// A string prompt is the input's one part with no meta.source: a hook's part beside it does not count.
		{"an editor context closing a string prompt with a hook's part", []any{map[string]any{"type": "text", "text": "hook output",
			"meta": map[string]any{"source": "user prompt submit hook"}}, text("marker-7f3a typed\n" + context)}, split, []string{"marker-7f3a typed"}},
		// A note is a client's only right before its image.
		{"a note part with no image after it", []any{text("marker-7f3a typed"), note},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text"}, []string{"marker-7f3a typed " + noteText}},
		{"a note part before his words", []any{text("marker-7f3a typed"), note, text("marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + noteText + " marker-7f3a after"}},
		{"a note opening his part before an image", []any{text("marker-7f3a typed"), text(noteText + " marker-7f3a after"), image},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + noteText + " marker-7f3a after"}},
		{"a note part before a video", []any{text("marker-7f3a typed"), note, map[string]any{"type": "video_url", "videoUrl": map[string]any{"url": "file:///neutral.mp4"}}},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text"}, []string{"marker-7f3a typed " + noteText}},
		{"a note part after his image", []any{text("marker-7f3a typed"), image, note},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text"}, []string{"marker-7f3a typed " + noteText}},
		// A note part whose closing tag is on the next line is no note of a client's: they write theirs on one.
		{"a note part closed on the next line", []any{text("marker-7f3a typed"),
			text(open + "Image compressed to fit model limits: marker-7f3a note\n" + end), image},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text"},
			[]string{"marker-7f3a typed " + open + "Image compressed to fit model limits: marker-7f3a note " + end}},
		{"a system block he typed or pasted", []any{text("marker-7f3a typed " + pasted)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a pasted": "owner owner_text"}, []string{"marker-7f3a typed " + pasted}},
		// A paste that is a block alone is left to tacit, as on main: its rule makes a turn that opens with a tag the
		// harness's whole. It is not split out as a client's block, which would leave an empty turn of his.
		{"a pasted system block alone", []any{text(pasted)}, map[string]string{"marker-7f3a pasted": "harness harness_text"}, nil},
		{"a known block in the middle of his words", []any{text("marker-7f3a typed " + noteText + " marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a note": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + noteText + " marker-7f3a after"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": tc.input}),
				kimiRecord("turn.ended", map[string]any{}))
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, labels...); !maps.Equal(got, tc.want) {
				t.Fatalf("prompt stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, tc.owner) {
				t.Fatalf("owner turns %q, want %q", got, tc.owner)
			}
		})
	}
}

// The TUI joins the text parts of a steered or queued prompt (combineSteerInput): an image's note, written as a text
// part of its own right before the image, then ends the text part before it, after his words. It is generated input
// there too.
func TestKimiSteeredNoteIsGeneratedInput(t *testing.T) {
	open, end := "<"+"system>", "</"+"system>"
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	image := map[string]any{"type": "image_url", "imageUrl": map[string]any{"url": "file:///neutral.png"}}
	note := open + "Image compressed to fit model limits: marker-7f3a note, original 4000x3000 -> sent 2000x1500. Fine detail may be lost." + end
	labels := []string{"marker-7f3a note", "marker-7f3a typed"}
	want := map[string]string{"marker-7f3a note": "harness harness_text", "marker-7f3a typed": "owner owner_text"}
	notes := map[string]string{"marker-7f3a note": "harness harness_text"}
	words := []string{"marker-7f3a typed"}
	for _, tc := range []struct {
		name  string
		input []any
		want  map[string]string
		owner []string
	}{
		{"his words and the note joined before its image", []any{text("marker-7f3a typed " + note), image}, want, words},
		{"two items joined, the second's note before its image", []any{text("marker-7f3a typed\n\n" + note), image}, want, words},
		// Two image-only items joined: the separator lands in the second note's part, and only whitespace is left.
		{"two image-only items joined", []any{text(note), image, text("\n\n" + note), image}, notes, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiRecord("turn.steer", map[string]any{"origin": map[string]any{"kind": "user", "inTurn": true}, "input": tc.input}),
				kimiRecord("turn.ended", map[string]any{}))
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, labels...); !maps.Equal(got, tc.want) {
				t.Fatalf("steer stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, tc.owner) {
				t.Fatalf("owner turns %q, want %q", got, tc.owner)
			}
		})
	}
}

// Kimi's web server writes a notice for each file a prompt attaches, a whole text part built from the name, type, size
// and path its origin's attachments keep, and Kimi writes a fixed notice in place of an image the model cannot take.
// Both are generated input when they can be rebuilt exactly; a notice its origin does not list, and words that only
// look like one, stay his.
func TestKimiClientNoticesAreGeneratedInput(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	notice := "Attached file \"marker-7f3a a.txt\" (text/plain, 2097152 bytes): /neutral/a.txt \u2014 open it with the Read tool"
	listed := map[string]any{"kind": "user", "attachments": []any{map[string]any{"name": "marker-7f3a a.txt", "mediaType": "text/plain", "size": 2097152,
		"path": "/neutral/a.txt"}}}
	plain := map[string]any{"kind": "user"}
	unsupported := "[Image omitted: \"marker-7f3a shot.heic\" uses unsupported image format image/heic. The current model provider accepts only " +
		"PNG, JPEG, GIF, and WebP \u2014 convert it to PNG or JPEG and try again.]"
	malformed := "[Image omitted: \"data:image/png;base64,marker-7f3a bad\u2026\" is not a valid data URL (its header or payload could not be parsed). " +
		"Re-encode the image as PNG or JPEG and try again.]"
	// The engine names no file for a data URL's image, the main path for the TUI, ACP and VS Code.
	unnamed := "[Image omitted: unsupported image format image/heic. The current model provider accepts only PNG, JPEG, GIF, and WebP " +
		"\u2014 convert it to PNG or JPEG and try again.]"
	// Kimi writes the image's type as declared, a space in it too.
	spaced := "[Image omitted: \"marker-7f3a shot.svg\" uses unsupported image format image/svg+xml; charset=utf-8. The current model provider " +
		"accepts only PNG, JPEG, GIF, and WebP \u2014 convert it to PNG or JPEG and try again.]"
	spacedUnnamed := "[Image omitted: unsupported image format image/svg+xml; charset=utf-8. The current model provider accepts only PNG, JPEG, " +
		"GIF, and WebP \u2014 convert it to PNG or JPEG and try again.]"
	// A notice, his words, then a copy of the notice's own fixed words after what Kimi fills in: the type, the name or
	// an accepted format would have to hold his words for the part to be one notice.
	tail := ". The current model provider accepts only PNG, JPEG, GIF, and WebP \u2014 convert it to PNG or JPEG and try again.]"
	typeRunOn := unnamed + " marker-7f3a after" + tail
	nameRunOn := unsupported + " marker-7f3a after \"marker-7f3a mine.heic\" uses unsupported image format image/heic" + tail
	urlRunOn := malformed + " marker-7f3a after \"data:image/png;base64,marker-7f3a mine\" is not a valid data URL (its header or payload could " +
		"not be parsed). Re-encode the image as PNG or JPEG and try again.]"
	labels := []string{"marker-7f3a typed", "marker-7f3a a.txt", "marker-7f3a shot", "marker-7f3a bad", "marker-7f3a mine", "Image omitted: unsupported",
		"marker-7f3a after"}
	for _, tc := range []struct {
		name   string
		origin map[string]any
		input  []any
		want   map[string]string
		owner  []string
	}{
		{"an attached-file notice its origin lists", listed, []any{text("marker-7f3a typed"), text(notice)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a a.txt": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"an attached-file notice its origin does not list", plain, []any{text("marker-7f3a typed"), text(notice)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a a.txt": "owner owner_text"}, []string{"marker-7f3a typed " + notice}},
		{"an unsupported image notice", plain, []any{text("marker-7f3a typed"), text(unsupported)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a shot": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"an unsupported image notice with no name", plain, []any{text("marker-7f3a typed"), text(unnamed)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "Image omitted: unsupported": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"a malformed image notice", plain, []any{text("marker-7f3a typed"), text(malformed)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a bad": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"words that look like a notice", plain, []any{text("marker-7f3a typed"), text("[Image omitted: marker-7f3a mine]")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a mine": "owner owner_text"},
			[]string{"marker-7f3a typed [Image omitted: marker-7f3a mine]"}},
		{"an image-omitted notice after his words", plain, []any{text("marker-7f3a typed " + unsupported)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a shot": "owner owner_text"}, []string{"marker-7f3a typed " + unsupported}},
		{"an image-omitted notice before his words", plain, []any{text("marker-7f3a typed"), text(unsupported + " marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a shot": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + unsupported + " marker-7f3a after"}},
		{"an unsupported image notice whose type holds a space", plain, []any{text("marker-7f3a typed"), text(spaced)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a shot": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"an unsupported image notice with no name whose type holds a space", plain, []any{text("marker-7f3a typed"), text(spacedUnnamed)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "Image omitted: unsupported": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"a notice, his words and its fixed words after the type again", plain, []any{text("marker-7f3a typed"), text(typeRunOn)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "Image omitted: unsupported": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + typeRunOn}},
		{"a named notice, his words and its fixed words after the name again", plain, []any{text("marker-7f3a typed"), text(nameRunOn)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a shot": "owner owner_text", "marker-7f3a after": "owner owner_text",
				"marker-7f3a mine": "owner owner_text"}, []string{"marker-7f3a typed " + nameRunOn}},
		{"a malformed image notice, his words and its fixed words after the url again", plain, []any{text("marker-7f3a typed"), text(urlRunOn)},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a bad": "owner owner_text", "marker-7f3a after": "owner owner_text",
				"marker-7f3a mine": "owner owner_text"}, []string{"marker-7f3a typed " + urlRunOn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiRecord("turn.prompt", map[string]any{"origin": tc.origin, "input": tc.input}),
				kimiRecord("turn.ended", map[string]any{}))
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, labels...); !maps.Equal(got, tc.want) {
				t.Fatalf("prompt stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, tc.owner) {
				t.Fatalf("owner turns %q, want %q", got, tc.owner)
			}
		})
	}
}

// Kimi's migration of a session from its older CLI writes a marker part where it could not carry content over: a
// media part with no url or whose local file is gone, and a part of a kind it does not know, as that part's JSON. It
// writes them only into the prompts it synthesizes, which carry no turnId, in a session whose metadata it marks as
// imported. There they are generated input, and a prompt they leave none of his words is still his turn, with empty
// text. The same words in a prompt Kimi took itself, which carries a turnId, in a steer, or in a session no migration
// made, are his, and so are words in the unsupported marker's brackets that are no JSON.
func TestKimiMigrationMarkersAreGeneratedInput(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	prompt := func(kind string, numbered bool, input ...any) map[string]any {
		r := kimiRecord(kind, map[string]any{"origin": map[string]any{"kind": "user"}, "input": input})
		if numbered {
			r["turnId"] = 3
		}
		return r
	}
	unknownPart := "[unsupported content: {\"type\":\"marker-7f3a widget\",\"data\":[1,2]}]"
	notJSON := "[unsupported content: marker-7f3a mine]"
	typed := `{"version":2,"lastPrompt":"marker-7f3a typed"}`
	imported := `{"version":2,"lastPrompt":"marker-7f3a typed","custom":{"imported_from_kimi_cli":true}}`
	labels := []string{"marker-7f3a typed", "[image expired]", "[video missing url]", "marker-7f3a widget", "marker-7f3a mine", "marker-7f3a after"}
	his := map[string]string{"[image expired]": "owner owner_text"}
	for _, tc := range []struct {
		name   string
		state  string
		record map[string]any
		want   map[string]string
		owner  []string
	}{
		{"a migration's media markers", imported, prompt("turn.prompt", false, text("marker-7f3a typed"), text("[image expired]"), text("[video missing url]")),
			map[string]string{"marker-7f3a typed": "owner owner_text", "[image expired]": "harness harness_text", "[video missing url]": "harness harness_text"},
			[]string{"marker-7f3a typed"}},
		{"a migration's unsupported part", imported, prompt("turn.prompt", false, text("marker-7f3a typed"), text(unknownPart)),
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a widget": "harness harness_text"}, []string{"marker-7f3a typed"}},
		{"a migrated prompt of a marker alone", imported, prompt("turn.prompt", false, text("[image expired]")),
			map[string]string{"[image expired]": "harness harness_text"}, []string{""}},
		{"a marker after his words", imported, prompt("turn.prompt", false, text("marker-7f3a typed [image expired]")),
			map[string]string{"marker-7f3a typed": "owner owner_text", "[image expired]": "owner owner_text"}, []string{"marker-7f3a typed [image expired]"}},
		{"an unsupported marker after his words", imported, prompt("turn.prompt", false, text("marker-7f3a typed "+unknownPart)),
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a widget": "owner owner_text"}, []string{"marker-7f3a typed " + unknownPart}},
		{"a marker before his words", imported, prompt("turn.prompt", false, text("marker-7f3a typed"), text("[image expired] marker-7f3a after")),
			map[string]string{"marker-7f3a typed": "owner owner_text", "[image expired]": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed [image expired] marker-7f3a after"}},
		{"an unsupported marker before his words", imported, prompt("turn.prompt", false, text("marker-7f3a typed"), text(unknownPart+" marker-7f3a after")),
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a widget": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed " + unknownPart + " marker-7f3a after"}},
		{"words that are no JSON in an unsupported marker", imported, prompt("turn.prompt", false, text("marker-7f3a typed"), text(notJSON)),
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a mine": "owner owner_text"}, []string{"marker-7f3a typed " + notJSON}},
		{"a marker he typed", typed, prompt("turn.prompt", true, text("[image expired]")), his, []string{"[image expired]"}},
		{"an unsupported marker he typed", typed, prompt("turn.prompt", true, text(unknownPart)),
			map[string]string{"marker-7f3a widget": "owner owner_text"}, []string{unknownPart}},
		{"a marker he typed into a migrated session", imported, prompt("turn.prompt", true, text("[image expired]")), his, []string{"[image expired]"}},
		{"a marker he steered into a migrated session", imported, prompt("turn.steer", false, text("[image expired]")), his, []string{"[image expired]"}},
		{"a prompt with no turnId in a session no migration made", typed, prompt("turn.prompt", false, text("[image expired]")), his, []string{"[image expired]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, tc.record, kimiRecord("turn.ended", map[string]any{}))
			kimiState(t, path, tc.state)
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, labels...); !maps.Equal(got, tc.want) {
				t.Fatalf("prompt stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, tc.owner) {
				t.Fatalf("owner turns %q, want %q", got, tc.owner)
			}
		})
	}
}

// The ACP server writes a resource a client attached as a whole text part: an embedded text resource as a resource
// element, and a link that is not a local file as a resource_link element. Both are generated input. A local file
// link becomes a bare path, which cannot be told from a path he typed, and stays in his words. A prompt the
// resources leave none of his words is still his turn, with empty text.
func TestKimiACPResourcesAreGeneratedInput(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	resource := text("<resource uri=\"file:///neutral/a.go\">marker-7f3a file body\nline two</resource>")
	link := text("<resource_link uri=\"https://neutral.example/doc\" name=\"marker-7f3a link\" />")
	labels := []string{"marker-7f3a typed", "marker-7f3a file body", "marker-7f3a link", "marker-7f3a-linked.go", "marker-7f3a after"}
	for _, tc := range []struct {
		name  string
		input []any
		want  map[string]string
		owner []string
	}{
		{"resources beside his words", []any{text("marker-7f3a typed"), resource, link, text("/neutral/marker-7f3a-linked.go:3-9")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a file body": "harness harness_text",
				"marker-7f3a link": "harness harness_text", "marker-7f3a-linked.go": "owner owner_text"},
			[]string{"marker-7f3a typed /neutral/marker-7f3a-linked.go:3-9"}},
		{"a resource alone", []any{resource}, map[string]string{"marker-7f3a file body": "harness harness_text"}, []string{""}},
		{"a resource element after his words", []any{text("marker-7f3a typed <resource uri=\"file:///neutral/a.go\">marker-7f3a file body</resource>")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a file body": "owner owner_text"},
			[]string{"marker-7f3a typed <resource uri=\"file:///neutral/a.go\">marker-7f3a file body</resource>"}},
		{"a resource link after his words", []any{text("marker-7f3a typed <resource_link uri=\"https://neutral.example/doc\" name=\"marker-7f3a link\" />")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a link": "owner owner_text"},
			[]string{"marker-7f3a typed <resource_link uri=\"https://neutral.example/doc\" name=\"marker-7f3a link\" />"}},
		{"a resource element before his words", []any{text("marker-7f3a typed"),
			text("<resource uri=\"file:///neutral/a.go\">marker-7f3a file body</resource> marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a file body": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed <resource uri=\"file:///neutral/a.go\">marker-7f3a file body</resource> marker-7f3a after"}},
		{"a resource link before his words", []any{text("marker-7f3a typed"),
			text("<resource_link uri=\"https://neutral.example/doc\" name=\"marker-7f3a link\" /> marker-7f3a after")},
			map[string]string{"marker-7f3a typed": "owner owner_text", "marker-7f3a link": "owner owner_text", "marker-7f3a after": "owner owner_text"},
			[]string{"marker-7f3a typed <resource_link uri=\"https://neutral.example/doc\" name=\"marker-7f3a link\" /> marker-7f3a after"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
			writeJSONLines(t, path, kimiRecord("turn.prompt", map[string]any{"origin": map[string]any{"kind": "user"}, "input": tc.input}),
				kimiRecord("turn.ended", map[string]any{}))
			kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
			st := openStore(t)
			ingest(t, transcripts(home), st)
			if got := actorsOf(t, st, labels...); !maps.Equal(got, tc.want) {
				t.Fatalf("prompt stored as %v, want %v", got, tc.want)
			}
			if got := ownerTexts(t, st); !slices.Equal(got, tc.owner) {
				t.Fatalf("owner turns %q, want %q", got, tc.owner)
			}
		})
	}
}

// How a prompt's generated text is split from his words is no new rule: a record read before the split keeps what it
// was stored with, and its file is not read again, since a read from the start would decide its provenance again from
// the current metadata. New records get the new split.
func TestKimiSplitIsNoNewRule(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "sessions", "neutral", "typed", "agents", "main", "wire.jsonl")
	skilled := func(typed string) map[string]any {
		return kimiRecord("turn.prompt", map[string]any{"origin": kimiSkillOrigin(map[string]any{}),
			"input": []any{kimiSkillPart("marker-7f3a skill body"), map[string]any{"type": "text", "text": typed}}})
	}
	writeJSONLines(t, path, skilled("marker-7f3a typed"), kimiRecord("turn.ended", map[string]any{}))
	kimiState(t, path, `{"version":2,"lastPrompt":"marker-7f3a typed"}`)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	// What a reader before the split stored: the record's one event, the body and the typed words joined as the
	// owner's, at the record's first key, under a cursor of the current rule.
	var turn store.Event
	for _, e := range all(t, st, "transcripts") {
		if e.Summary == "marker-7f3a typed" {
			turn = e
		}
	}
	if len(turn.SourceKey) < 3 {
		t.Fatal("no owner turn stored")
	}
	place := turn.SourceKey[:len(turn.SourceKey)-3]
	old := turn
	old.SourceKey, old.Summary = place+"000", "marker-7f3a skill body marker-7f3a typed"
	cursors, err := st.FileCursors(context.Background(), src.Name())
	if err != nil || len(cursors) != 1 {
		t.Fatalf("cursors %d %v, want 1", len(cursors), err)
	}
	for _, cur := range cursors {
		var state map[string]any
		if err := json.Unmarshal(cur.State, &state); err != nil {
			t.Fatal(err)
		}
		state["Rule"] = 1 // the rule the cursors before the split were saved under
		cur.State, _ = json.Marshal(state)
		if _, _, err := st.PutFiles(context.Background(), []store.FileWrite{{Events: []store.Event{old}, Cursor: cur, Places: []string{place}}}); err != nil {
			t.Fatal(err)
		}
	}
	appendJSONLines(t, path, skilled("marker-7f3a later"), kimiRecord("turn.ended", map[string]any{}))
	r := ingest(t, src, st)
	for _, n := range r.Notes {
		if strings.Contains(n, "read again from the start") {
			t.Fatalf("a record read before the split was read again: %q", r.Notes)
		}
	}
	want := map[string]string{"marker-7f3a skill body marker-7f3a typed": "owner owner_text", "marker-7f3a skill body": "harness harness_text",
		"marker-7f3a later": "owner owner_text"}
	if got := promptActors(t, st); !maps.Equal(got, want) {
		t.Fatalf("prompts stored as %v, want %v", got, want)
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
