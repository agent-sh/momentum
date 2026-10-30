package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
)

type ocDB struct {
	t  *testing.T
	db *sql.DB
}

func newDB(t *testing.T, dataHome string) *ocDB {
	t.Helper()
	path := filepath.Join(dataHome, "opencode", "opencode.db")
	fake.File(t, path, "")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT NOT NULL, title TEXT NOT NULL,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	return &ocDB{t: t, db: db}
}

func (d *ocDB) exec(q string, args ...any) {
	d.t.Helper()
	if _, err := d.db.Exec(q, args...); err != nil {
		d.t.Fatal(err)
	}
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func (d *ocDB) session(id, parent, dir, title string, created, updated time.Time) {
	var p any
	if parent != "" {
		p = parent
	}
	d.exec(`INSERT INTO session VALUES (?,?,?,?,?,?,NULL)`, id, p, dir, title, created.UnixMilli(), updated.UnixMilli())
}

func (d *ocDB) message(id, session string, at time.Time, m map[string]any) {
	d.exec(`INSERT INTO message VALUES (?,?,?,?)`, id, session, at.UnixMilli(), js(m))
}

func (d *ocDB) part(id, msg, session string, at time.Time, p map[string]any) {
	d.exec(`INSERT INTO part VALUES (?,?,?,?,?)`, id, msg, session, at.UnixMilli(), js(p))
}

func TestOpencodeTerminalsAndTheirSessions(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	boot := time.Unix(fake.Boot, 0)
	start := boot.Add(10 * time.Second)
	d := newDB(t, data)
	d.session("ses_old", "", cwd, "older work", boot.Add(-time.Hour), start.Add(time.Minute)) // touched since start, created before
	d.session("ses_new", "", cwd, "port the parser", start.Add(5*time.Second), start.Add(2*time.Minute))
	d.session("ses_kid", "ses_new", cwd, "explore the tests", start.Add(30*time.Second), start.Add(2*time.Minute))
	d.message("m1", "ses_new", start.Add(6*time.Second), map[string]any{"role": "user"})
	d.part("p1", "m1", "ses_new", start.Add(6*time.Second), map[string]any{"type": "text", "text": "port it"})
	d.message("m2", "ses_new", start.Add(7*time.Second), map[string]any{"role": "assistant", "modelID": "local-x", "time": map[string]any{"created": 1}})
	d.part("p2", "m2", "ses_new", start.Add(8*time.Second), map[string]any{"type": "tool", "tool": "bash",
		"state": map[string]any{"status": "running", "title": "go test", "time": map[string]any{"start": start.Add(8 * time.Second).UnixMilli()}}})
	d.message("k1", "ses_kid", start.Add(31*time.Second), map[string]any{"role": "user"})
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 5, Argv: []string{"/opt/opencode"}, Start: 1000, Cwd: cwd},
		fake.Proc{PID: 6, Argv: []string{"/opt/opencode", "serve", "--port", "4096"}, Start: 1000, Cwd: cwd},
		fake.Proc{PID: 7, PPID: 5, Argv: []string{"/opt/opencode", "worker"}, Start: 1100, Cwd: cwd},
	))
	if err != nil {
		t.Fatal(err)
	}
	w := &sessions.World{Now: start.Add(3 * time.Minute), Env: sessions.Env{DataHome: data}, Procs: tb}
	src := New(w.Env)
	defer src.Close()
	ss, err := sessions.Scan(context.Background(), w, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("one terminal, not its server or worker: %+v", ss)
	}
	s := ss[0]
	if s.Key != "opencode:ses_new" || s.Name != "port the parser" || s.Phase != sessions.Working || s.Now != "bash: go test" || s.Model != "local-x" {
		t.Fatalf("session %+v", s)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].ID != "ses_kid" {
		t.Fatalf("a child session with an open turn is a subagent: %+v", s.Tasks)
	}
	if items, _ := s.Log.Since(0); len(items) != 1 || items[0].String() != "USER: port it" {
		t.Fatalf("the streaming answer waits; log %+v", items)
	}
}

func TestOpencodeStopsOnAnError(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	start := time.Unix(fake.Boot, 0).Add(10 * time.Second)
	d := newDB(t, data)
	d.session("ses_e", "", cwd, "x", start.Add(time.Second), start.Add(time.Minute))
	d.message("m1", "ses_e", start.Add(2*time.Second), map[string]any{"role": "assistant", "time": map[string]any{"created": 1, "completed": 2},
		"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "quota exceeded"}}})
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Start: 1000, Cwd: cwd}))
	if err != nil {
		t.Fatal(err)
	}
	w := &sessions.World{Now: start.Add(2 * time.Minute), Env: sessions.Env{DataHome: data}, Procs: tb}
	ss, err := sessions.Scan(context.Background(), w, New(w.Env))
	if err != nil || len(ss) != 1 || ss[0].Phase != sessions.NeedsYou || ss[0].Ask != "stopped on an error: quota exceeded" {
		t.Fatalf("got %v %+v", err, ss)
	}
}

func TestTheLogReadsMessagesThatShareAMillisecondOnce(t *testing.T) {
	data := t.TempDir()
	at := time.Unix(fake.Boot, 0)
	d := newDB(t, data)
	d.message("a", "s", at, map[string]any{"role": "user"})
	d.part("pa", "a", "s", at, map[string]any{"type": "text", "text": "first"})
	src := New(sessions.Env{DataHome: data})
	src.open()
	defer src.Close()
	l := &dbLog{}
	src.readLog(context.Background(), "s", l)
	d.message("b", "s", at, map[string]any{"role": "user"}) // the same millisecond
	d.part("pb", "b", "s", at, map[string]any{"type": "text", "text": "second"})
	src.readLog(context.Background(), "s", l)
	items, _ := l.log.Since(0)
	if len(items) != 2 || items[0].Text != "first" || items[1].Text != "second" {
		t.Fatalf("log %+v", items)
	}
}

func TestNoOpencodeNoSessions(t *testing.T) {
	tb, _ := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Cwd: t.TempDir()}))
	w := &sessions.World{Now: time.Now(), Env: sessions.Env{DataHome: t.TempDir()}, Procs: tb}
	if ss, err := sessions.Scan(context.Background(), w, New(w.Env)); err != nil || len(ss) != 0 {
		t.Fatalf("got %v %v", ss, err)
	}
}

func TestTheLastTextIsTheAssistantsNotTheUsers(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	start := time.Unix(fake.Boot, 0).Add(10 * time.Second)
	d := newDB(t, data)
	d.session("ses", "", cwd, "x", start.Add(time.Second), start.Add(time.Minute))
	d.message("m1", "ses", start.Add(2*time.Second), map[string]any{"role": "assistant", "time": map[string]any{"created": 1, "completed": 2}})
	d.part("p1", "m1", "ses", start.Add(2*time.Second), map[string]any{"type": "text", "text": "Done; the parser is fixed."})
	d.message("m2", "ses", start.Add(3*time.Second), map[string]any{"role": "user"})
	d.part("p2", "m2", "ses", start.Add(3*time.Second), map[string]any{"type": "text", "text": "Can you approve the restart?"})
	tb, _ := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Start: 1000, Cwd: cwd}))
	w := &sessions.World{Now: start.Add(2 * time.Minute), Env: sessions.Env{DataHome: data}, Procs: tb}
	ss, err := sessions.Scan(context.Background(), w, New(w.Env))
	if err != nil || len(ss) != 1 || ss[0].LastText != "Done; the parser is fixed." {
		t.Fatalf("got %v %+v", err, ss)
	}
}
