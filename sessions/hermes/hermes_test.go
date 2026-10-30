package hermes

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
)

func stateDB(t *testing.T, home string, withOwnerStart bool) *sql.DB {
	t.Helper()
	path := filepath.Join(home, "state.db")
	fake.File(t, path, "")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ownerStart := ", owner_started_at INTEGER"
	if !withOwnerStart {
		ownerStart = ""
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, title TEXT, display_name TEXT, cwd TEXT, model TEXT,
		started_at REAL NOT NULL, ended_at REAL, last_activity_at REAL, last_activity_description TEXT, archived INTEGER, hidden INTEGER);
		CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_calls TEXT, timestamp REAL NOT NULL);
		CREATE TABLE session_turn_leases (conversation_id TEXT PRIMARY KEY, holder TEXT NOT NULL, acquired_at REAL NOT NULL, expires_at REAL NOT NULL);
		CREATE TABLE async_delegations (delegation_id TEXT PRIMARY KEY, origin_session TEXT NOT NULL, origin_session_id TEXT NOT NULL DEFAULT '',
		parent_session_id TEXT, state TEXT NOT NULL, dispatched_at REAL NOT NULL, updated_at REAL NOT NULL, owner_pid INTEGER` + ownerStart + `, task_json TEXT);`); err != nil {
		t.Fatal(err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestHermesListsConversationsAndRunningAutomation(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := func(d time.Duration) float64 { return float64(now.Add(d).UnixMilli()) / 1000 }
	db := stateDB(t, home, true)
	exec(t, db, `INSERT INTO sessions VALUES ('chat', 'cli', 'Fix the backup', '', '/w', 'model-a', ?, NULL, ?, 'running tests', 0, 0)`, sec(-time.Hour), sec(-time.Minute))
	exec(t, db, `INSERT INTO sessions VALUES ('phone', 'mobile', '', 'Phone chat', '', 'model-a', ?, NULL, ?, '', 0, 0)`, sec(-3*time.Hour), sec(-2*time.Hour))
	exec(t, db, `INSERT INTO sessions VALUES ('cron1', 'cron', 'nightly', '', '', '', ?, NULL, ?, '', 0, 0)`, sec(-time.Hour), sec(-time.Minute))
	exec(t, db, `INSERT INTO sessions VALUES ('cron2', 'cron', 'idle cron', '', '', '', ?, NULL, ?, '', 0, 0)`, sec(-time.Hour), sec(-time.Minute))
	exec(t, db, `INSERT INTO sessions VALUES ('stale', 'cli', 'old', '', '', '', ?, NULL, ?, '', 0, 0)`, sec(-50*time.Hour), sec(-48*time.Hour))
	exec(t, db, `INSERT INTO session_turn_leases VALUES ('chat', 'pid=42:abc:platform=cli', ?, ?)`, sec(-time.Hour), sec(-time.Minute)) // expired: between turns
	exec(t, db, `INSERT INTO session_turn_leases VALUES ('cron1', 'pid=43:def:platform=cron', ?, ?)`, sec(-time.Minute), sec(time.Minute))
	exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('chat', 'user', 'back it up', '', ?)`, sec(-2*time.Minute))
	exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('chat', 'assistant', 'Running it.', ?, ?)`,
		`[{"function":{"name":"terminal","arguments":"{\"command\":\"restic backup\"}"}}]`, sec(-time.Minute))
	exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('phone', 'assistant', 'Here is the summary.', '[]', ?)`, sec(-2*time.Hour))
	exec(t, db, `INSERT INTO async_delegations VALUES ('d1', 'phone', 'phone', NULL, 'running', ?, ?, 44, 4400, '{"goal":"Read the logs","role":"researcher"}')`, sec(-time.Hour), sec(-time.Minute))
	exec(t, db, `INSERT INTO async_delegations VALUES ('d2', 'phone', 'phone', NULL, 'running', ?, ?, 45, 1, '{"goal":"recycled pid"}')`, sec(-time.Hour), sec(-time.Minute))
	exec(t, db, `INSERT INTO async_delegations VALUES ('d3', 'phone', 'phone', NULL, 'running', ?, ?, 46, 0, '{"goal":"dead owner"}')`, sec(-time.Hour), sec(-time.Minute))
	exec(t, db, `INSERT INTO async_delegations VALUES ('d4', 'phone', 'phone', NULL, 'completed', ?, ?, 44, 4400, '{"goal":"over"}')`, sec(-time.Hour), sec(-time.Minute))
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 42, Argv: []string{"hermes"}},
		fake.Proc{PID: 43, Argv: []string{"hermes", "cron"}},
		fake.Proc{PID: 44, Argv: []string{"hermes", "gateway"}, Start: 4400},
		fake.Proc{PID: 45, Argv: []string{"something", "else"}, Start: 4500}, // the delegation's owner pid, now another process
	))
	if err != nil {
		t.Fatal(err)
	}
	env := sessions.Env{HermesHome: home}
	w := &sessions.World{Now: now, Procs: tb}
	src := New(env)
	defer src.Close()
	ss, err := sessions.Scan(context.Background(), w, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss { // a session is named by its source's tool, as every source of momentum names it
		if s.Tool != src.Tool() {
			t.Fatalf("session %s is named %q, not %q", s.Key, s.Tool, src.Tool())
		}
	}
	got := map[string]*sessions.Session{}
	for _, s := range ss {
		got[s.Key] = s
	}
	if len(got) != 3 || got["hermes:cron2"] != nil || got["hermes:stale"] != nil {
		t.Fatalf("sessions %v", got)
	}
	chat := got["hermes:chat"]
	if chat.Phase != sessions.Working || chat.PID != 42 || chat.Now != "terminal: restic backup" || chat.Name != "Fix the backup" || chat.Auto {
		t.Fatalf("chat %+v", chat)
	}
	if cron := got["hermes:cron1"]; !cron.Auto || cron.Phase != sessions.Working {
		t.Fatalf("a leased automation run %+v", cron)
	}
	phone := got["hermes:phone"]
	if phone.Phase != sessions.Background || len(phone.Tasks) != 1 || phone.Tasks[0].ID != "d1" || phone.Tasks[0].Type != "researcher" || phone.PID != 0 {
		t.Fatalf("only a delegation whose owner is the same process counts: %+v", phone)
	}
	if phone.LastText != "Here is the summary." || phone.Name != "Phone chat" {
		t.Fatalf("phone %+v", phone)
	}
}

func TestHermesBeforeOwnerStartTimes(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := float64(now.Unix())
	db := stateDB(t, home, false)
	exec(t, db, `INSERT INTO sessions VALUES ('c', 'cli', 't', '', '', '', ?, NULL, ?, '', 0, 0)`, sec-60, sec-30)
	exec(t, db, `INSERT INTO async_delegations VALUES ('d1', 'c', 'c', NULL, 'running', ?, ?, 44, '{"goal":"g"}')`, sec-60, sec-30)
	tb, _ := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 44, Argv: []string{"hermes"}, Start: 4400}))
	env := sessions.Env{HermesHome: home}
	w := &sessions.World{Now: now, Procs: tb}
	ss, err := sessions.Scan(context.Background(), w, New(env))
	if err != nil || len(ss) != 1 || len(ss[0].Tasks) != 1 {
		t.Fatalf("got %v %+v", err, ss)
	}
}

func TestNoHermesNoSessions(t *testing.T) {
	env := sessions.Env{HermesHome: t.TempDir()}
	w := &sessions.World{Now: time.Now(), Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}
	if ss, err := sessions.Scan(context.Background(), w, New(env)); err != nil || len(ss) != 0 {
		t.Fatalf("got %v %v", ss, err)
	}
}

func TestTheLogReadsAConversationLongerThanAPage(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := float64(now.Unix())
	db := stateDB(t, home, true)
	exec(t, db, `INSERT INTO sessions VALUES ('c', 'cli', 't', '', '', '', ?, NULL, ?, '', 0, 0)`, sec-3600, sec-3000)
	for i := 0; i < 7; i++ {
		exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('c', 'user', ?, '', ?)`, fmt.Sprint("m", i), sec-3500+float64(i))
	}
	exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('c', 'assistant', 'Done.', '', ?)`, sec-3400)
	old := pageRows
	pageRows = 3
	t.Cleanup(func() { pageRows = old })
	tb, _ := proc.Read(fake.ProcRoot(t))
	src := New(sessions.Env{HermesHome: home})
	defer src.Close()
	first, err := sessions.Scan(context.Background(), &sessions.World{Now: now, Procs: tb}, src)
	if err != nil || len(first) != 1 || first[0].Seq != 8 {
		t.Fatalf("one scan reads every page: %v %+v", err, first)
	}
	again, _ := sessions.Scan(context.Background(), &sessions.World{Now: now.Add(time.Minute), Procs: tb}, src)
	if again[0].Seq != 8 || again[0].Phase != sessions.Idle {
		t.Fatalf("nothing new, so the log length stays: %+v", again[0])
	}
}

func TestHermesNamesProjectsFromTheWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := float64(now.Unix())
	db := stateDB(t, home, true)
	exec(t, db, `INSERT INTO sessions VALUES ('a', 'cli', 't', '', '/w/code/app', '', ?, NULL, ?, '', 0, 0)`, sec-60, sec-30)
	exec(t, db, `INSERT INTO sessions VALUES ('b', 'mobile', 't', '', '', '', ?, NULL, ?, '', 0, 0)`, sec-60, sec-30)
	tb, _ := proc.Read(fake.ProcRoot(t))
	ss, err := sessions.Scan(context.Background(), &sessions.World{Now: now, Procs: tb}, New(sessions.Env{HermesHome: home, Projects: []string{"/w/code"}}))
	got := map[string]string{}
	for _, s := range ss {
		got[s.Key] = s.Project
	}
	if err != nil || got["hermes:a"] != "app" || got["hermes:b"] != "mobile" {
		t.Fatalf("got %v %v", err, got)
	}
}
