package hermes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/internal/saved"
	"github.com/agent-sh/momentum/sessions/proc"
)

// runAs is one CLI run in its own process: a new Source, resumed from the store when it can resume.
func runAs(t *testing.T, home string, now time.Time, mem *saved.Mem) []*sessions.Session {
	t.Helper()
	src := New(sessions.Env{HermesHome: home})
	defer src.Close()
	r, resumable := any(src).(sessions.Resumable)
	if resumable && mem != nil {
		r.Resume(mem)
	}
	tb, _ := proc.Read(fake.ProcRoot(t))
	ss, err := sessions.Scan(context.Background(), &sessions.World{Now: now, Procs: tb}, src)
	if err != nil || len(ss) != 1 {
		t.Fatalf("got %v %+v", err, ss)
	}
	if resumable && mem != nil {
		mem.Save(r.Checkpoints())
	}
	return ss
}

func TestARowRewrittenInPlaceShowsInTheNextRun(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := float64(now.Unix())
	db := stateDB(t, home, true)
	exec(t, db, `INSERT INTO sessions VALUES ('c', 'cli', 'notes', '', '/w', 'model-a', ?, NULL, ?, '', 0, 0)`, sec-3600, sec-60)
	exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('c', 'user', 'summarize @notes.md', '', ?)`, sec-50)
	mem := saved.New()
	runAs(t, home, now, mem) // a run between the submit and the turn's prologue
	exec(t, db, `UPDATE messages SET content = 'summarize the file notes.md: its text' WHERE role = 'user'`)
	exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('c', 'assistant', 'Summary.', '[]', ?)`, sec-40)
	next, cold := runAs(t, home, now.Add(time.Minute), mem), runAs(t, home, now.Add(time.Minute), nil)
	if diff := saved.Same(next, cold); diff != "" {
		t.Fatalf("the next run is not what the database holds: %s", diff)
	}
	if items, _ := next[0].Log.Since(0); len(items) != 2 || items[0].Text != "summarize the file notes.md: its text" {
		t.Fatalf("log %v", items)
	}
}

func TestMessagesTakenBackAreNotTheConversation(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := float64(now.Unix())
	db := stateDB(t, home, true)
	exec(t, db, `ALTER TABLE messages ADD COLUMN active INTEGER NOT NULL DEFAULT 1`)
	exec(t, db, `INSERT INTO sessions VALUES ('c', 'cli', 'notes', '', '/w', 'model-a', ?, NULL, ?, '', 0, 0)`, sec-3600, sec-60)
	for i, m := range [][3]any{{"user", "draft it", 1}, {"assistant", "Drafted.", 1}, {"user", "no, start over", 0}, {"tool", "the old output", 0}} {
		exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp, active) VALUES ('c', ?, ?, '', ?, ?)`,
			m[0], m[1], sec-50+float64(i), m[2])
	}
	s := runAs(t, home, now, nil)[0]
	items, _ := s.Log.Since(0)
	if s.Phase != sessions.Idle || s.TurnOpen || fmt.Sprint(items) != "[USER: draft it ASSISTANT: Drafted.]" {
		t.Fatalf("a turn taken back opened the session or entered its log: %s open %v log %v", s.Phase, s.TurnOpen, items)
	}
}
