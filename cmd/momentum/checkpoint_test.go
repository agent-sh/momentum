package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
	"github.com/agent-sh/momentum/sessions/codex"
	"github.com/agent-sh/momentum/sessions/proc"
)

type listed []sessions.Found

func (l listed) List(context.Context, *sessions.World) ([]sessions.Found, error) { return l, nil }

// jsonlFile is a synthetic transcript, one JSON value per line, each a second after the last.
type jsonlFile struct {
	t    *testing.T
	path string
	at   time.Time
}

func (f *jsonlFile) add(v map[string]any) *jsonlFile {
	f.t.Helper()
	f.at = f.at.Add(time.Second)
	if _, ok := v["timestamp"]; !ok {
		v["timestamp"] = f.at.Format(time.RFC3339Nano)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		f.t.Fatal(err)
	}
	w, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write(b.Bytes()); err != nil {
		f.t.Fatal(err)
	}
	return f
}

// Claude Code transcript lines.
func (f *jsonlFile) user(s string) *jsonlFile {
	return f.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": s}})
}
func (f *jsonlFile) say(s string) *jsonlFile {
	return f.add(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-test-1",
		"content": []any{map[string]any{"type": "text", "text": s}}}})
}
func (f *jsonlFile) call(id, name string, in map[string]any) *jsonlFile {
	return f.add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": in}}}})
}
func (f *jsonlFile) result(id, out string) *jsonlFile {
	return f.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}}}})
}

// Codex rollout lines.
func (f *jsonlFile) ev(typ string, payload map[string]any) *jsonlFile {
	return f.add(map[string]any{"type": typ, "payload": payload})
}

// sessionsRun is one `momentum sessions` run in its own process: a new store handle and new sources.
func sessionsRun(t *testing.T, path string, now time.Time, srcs ...sessions.Source) []*sessions.Session {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, scanErr, err := board(context.Background(), &sessions.World{Now: now, Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}, nil, st, "test", srcs)
	if err != nil || scanErr != nil {
		t.Fatal(err, scanErr)
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].Key < ss[j].Key })
	return ss
}

// derived is what the trackers derived for a session, without the phase-entry time, which the stamps hold.
func derived(t *testing.T, s *sessions.Session) string {
	t.Helper()
	c := *s
	c.Since = time.Time{}
	j, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	items, gap := s.Log.Since(0)
	return fmt.Sprintf("%s size=%d seq=%d base=%d gap=%v log=%v", j, s.Size, s.Log.Seq(), s.Log.Base(), gap, items)
}

func TestAResumedRunDerivesWhatAColdRunDerivesThroughTheStore(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	cl := &jsonlFile{t: t, path: filepath.Join(dir, "claude", "a.jsonl"), at: t0}
	cx := &jsonlFile{t: t, path: filepath.Join(dir, "codex", "rollout-x.jsonl"), at: t0}
	for _, p := range []string{cl.path, cx.path} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sources := func() []sessions.Source {
		return []sessions.Source{
			&claude.Source{Lister: listed{{Tool: "claude", Session: "a", Transcript: cl.path, Started: t0}},
				Screen: func(context.Context, *sessions.World, *sessions.Session) string { return "" }},
			&codex.Source{Env: sessions.Env{CodexHome: filepath.Join(dir, "codex")}, Lister: listed{{Tool: "codex", Thread: "x", PID: 7, Transcript: cx.path}}},
		}
	}
	resumedDB := filepath.Join(dir, "resumed.db")
	now := t0
	round := func(name string) []*sessions.Session {
		t.Helper()
		now = later(now, cl.at, cx.at).Add(time.Minute)
		resumed := sessionsRun(t, resumedDB, now, sources()...)
		cold := sessionsRun(t, filepath.Join(t.TempDir(), "cold.db"), now, sources()...) // a store with no checkpoints
		if len(resumed) != len(cold) {
			t.Fatalf("%s: %d sessions resumed, %d cold", name, len(resumed), len(cold))
		}
		for i := range cold {
			if r, c := derived(t, resumed[i]), derived(t, cold[i]); r != c {
				t.Fatalf("%s: %s differs\nresumed %.600s\ncold    %.600s", name, cold[i].Key, r, c)
			}
		}
		return resumed
	}

	cl.user("ship the release "+strings.Repeat("with every check, ", 30)).
		call("t1", "Bash", map[string]any{"command": "make", "run_in_background": true}).
		result("t1", "Command running in background with ID: bg1").
		call("t2", "Agent", map[string]any{"description": "Review", "subagent_type": "reviewer"}).
		result("t2", "Async agent launched successfully.\nagentId: ag1 (internal)").say("Both run.")
	cx.ev("event_msg", map[string]any{"type": "task_started"}).
		ev("response_item", map[string]any{"type": "function_call", "call_id": "c1", "name": "exec_command", "arguments": `{"cmd":"go test ./..."}`})
	if ss := round("first"); ss[0].Phase != sessions.Background || ss[1].Phase != sessions.Working {
		t.Fatalf("setup: %s %s", ss[0].Phase, ss[1].Phase)
	}
	cl.add(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification><task-id>ag1</task-id><status>completed</status></task-notification>"}}).
		call("t3", "AskUserQuestion", map[string]any{"questions": []any{map[string]any{"question": "Tag it now?"}}})
	cx.ev("response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"}).
		ev("event_msg", map[string]any{"type": "task_complete", "last_agent_message": "Tests pass."})
	round("asks")
	cl.result("t3", "yes")
	for i := 0; i < sessions.LogKeep/2+50; i++ { // past LogKeep: the store drops the oldest quarter too
		id := fmt.Sprintf("r%d", i)
		cl.call(id, "Read", map[string]any{"file_path": fmt.Sprintf("/w/f%d.go", i)}).result(id, "package x")
	}
	cl.say("Read everything.")
	if ss := round("a long log"); ss[0].Log.Base() == 0 {
		t.Fatal("the log did not trim")
	}
	if err := os.WriteFile(cx.path, nil, 0o600); err != nil { // the rollout starts over
		t.Fatal(err)
	}
	cx.ev("event_msg", map[string]any{"type": "task_started"}).
		ev("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "again"}}})
	round("a rollout written again")
	cl.user("one more thing").say("Done.")
	round("after it")

	// a line already read is edited in place: only a cold run sees it, so the resumed one read from its checkpoint
	body, _ := os.ReadFile(cl.path)
	edited := strings.Replace(string(body), "/w/f1000.go", "/w/X1000.go", 1)
	if edited == string(body) || os.WriteFile(cl.path, []byte(edited), 0o600) != nil {
		t.Fatal("setup")
	}
	now = now.Add(time.Minute)
	resumed := sessionsRun(t, resumedDB, now, sources()...)
	cold := sessionsRun(t, filepath.Join(t.TempDir(), "cold.db"), now, sources()...)
	if len(resumed) != 2 || len(cold) != 2 {
		t.Fatalf("sessions: %d resumed, %d cold", len(resumed), len(cold))
	}
	if r, c := derived(t, resumed[0]), derived(t, cold[0]); strings.Contains(r, "X1000") || !strings.Contains(c, "X1000") {
		t.Fatal("the resumed run read the transcript again from its start")
	}
}

func later(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
