package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
)

func msg(at time.Time, role, stop string, content ...map[string]any) map[string]any {
	return map[string]any{"type": "message", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": role, "stopReason": stop, "content": content}}
}

func txt(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func TestTrackerFollowsRolesAndStopReasons(t *testing.T) {
	at := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fake.Lines(t, path,
		map[string]any{"type": "session", "cwd": "/w"},
		map[string]any{"type": "model_change", "modelId": "local-model"},
		msg(at, "user", "", txt("list the files")),
		msg(at.Add(time.Second), "assistant", "toolUse", map[string]any{"type": "toolCall", "id": "c1", "name": "bash", "arguments": map[string]any{"command": "ls"}}),
	)
	tr := NewTracker(path)
	tr.Update()
	if !tr.st.Open || tr.st.Model != "local-model" || tr.st.Calls["c1"] == nil {
		t.Fatalf("mid-turn: %+v", tr.st)
	}
	fake.Append(t, path,
		map[string]any{"type": "message", "message": map[string]any{"role": "toolResult", "toolCallId": "c1", "content": []any{txt("a b")}}},
		msg(at.Add(2*time.Second), "assistant", "stop", txt("Two files.")),
	)
	tr.Update()
	if tr.st.Open || tr.st.LastText != "Two files." || len(tr.st.Calls) != 0 {
		t.Fatalf("after the turn: %+v", tr.st)
	}
	fake.Append(t, path, msg(at.Add(3*time.Second), "user", "", txt("again")),
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "rate limited"}})
	tr.Update()
	if tr.st.Open || tr.st.ErrText != "rate limited" {
		t.Fatalf("error: %+v", tr.st)
	}
}

func TestTrackerStartsOverWhenTheFileShrinks(t *testing.T) {
	at := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fake.Lines(t, path, msg(at, "user", "", txt("one")), msg(at, "assistant", "stop", txt("first answer, a long one to make the file long")))
	tr := NewTracker(path)
	tr.Update()
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	fake.Append(t, path, msg(at, "user", "", txt("two")))
	tr.Update()
	if !tr.st.Open || tr.st.LastText != "" || tr.Log().Seq() != 1 {
		t.Fatalf("the shrunk file is read again from its start: %+v seq %d", tr.st, tr.Log().Seq())
	}
}

func TestSessionDirEncodesTheWorkingDirectoryAsPiDoes(t *testing.T) {
	if got := SessionDir("/a", "/home/u/my:proj/x"); got != filepath.Join("/a", "sessions", "--home-u-my-proj-x--") {
		t.Fatalf("got %q", got)
	}
}

func TestCreatedReadsTheFileName(t *testing.T) {
	if got := Created("/x/2026-09-20T20-34-02-152Z_01a0.jsonl"); !got.Equal(time.Date(2026, 9, 20, 20, 34, 2, 152e6, time.UTC)) {
		t.Fatalf("got %v", got)
	}
	if !Created("/x/other.jsonl").IsZero() {
		t.Fatal("no time")
	}
}

func TestEachPiProcessGetsTheSessionItCreated(t *testing.T) {
	agent, cwd := t.TempDir(), t.TempDir()
	boot := time.Unix(fake.Boot, 0).UTC()
	older := boot.Add(10 * time.Second) // the first pi started here
	newer := boot.Add(20 * time.Second) // the second, in the same directory
	dir := SessionDir(agent, cwd)
	name := func(at time.Time, id string) string { // pi's own format; real names rarely have zero milliseconds
		at = at.Add(152 * time.Millisecond)
		return filepath.Join(dir, at.Format("2006-01-02T15-04-05")+"-"+at.Format(".000")[1:]+"Z_"+id+".jsonl")
	}
	first, second, resumed := name(older.Add(time.Second), "a"), name(newer.Add(time.Second), "b"), name(boot.Add(-time.Hour), "c")
	for _, f := range []string{first, second, resumed} {
		fake.Lines(t, f, msg(boot, "user", "", txt("hi")))
		os.Chtimes(f, newer.Add(time.Minute), newer.Add(time.Minute))
	}
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 1, Argv: []string{"node", "/usr/lib/node_modules/pi-coding-agent/dist/cli.js"}, Start: 1000, Cwd: cwd},
		fake.Proc{PID: 2, Argv: []string{"pi"}, Start: 2000, Cwd: cwd},
		fake.Proc{PID: 3, Argv: []string{"pi", "-p", "one-shot"}, Start: 2500, Cwd: cwd},
	))
	if err != nil {
		t.Fatal(err)
	}
	env := sessions.Env{PiDir: agent}
	w := &sessions.World{Now: newer.Add(2 * time.Minute), Procs: tb}
	ss, err := sessions.Scan(context.Background(), w, New(env))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, s := range ss {
		got[s.PID] = s.Transcript
	}
	if len(ss) != 2 || got[1] != first || got[2] != second {
		t.Fatalf("sessions %v", got)
	}
	if ss[0].Phase != sessions.Working || ss[0].Key[:3] != "pi:" {
		t.Fatalf("a user message opens the turn: %+v", ss[0])
	}
}
