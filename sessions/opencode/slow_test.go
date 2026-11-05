package opencode

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"

	"modernc.org/sqlite"
)

// slowReads counts the rows of the slow legacy messages a query reads; each costs slowRow.
var slowReads atomic.Int64

const slowRow = 2 * time.Millisecond

func init() {
	sqlite.MustRegisterScalarFunction("zz_slow_row", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		slowReads.Add(1)
		time.Sleep(slowRow)
		return args[0], nil
	})
}

// A big database must not slow down the scan of one live session: it reads that session's rows, not every
// session's. Each legacy message of the other sessions here costs slowRow to read, so a read of all of them runs
// past the session scan's deadline.
func TestALiveSessionIsReadWithoutTheRestOfTheDatabase(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	start := time.Unix(fake.Boot, 0).Add(10 * time.Second)
	d := newDB(t, data)
	d.exec(`DROP TABLE message;
CREATE TABLE message_rows (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX message_rows_session ON message_rows (session_id, time_created);
CREATE VIEW message AS SELECT id, session_id, time_created, zz_slow_row(data) AS data FROM message_rows;
CREATE TABLE session_v2 (id TEXT, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER, time_archived INTEGER);
CREATE TABLE session_message (id TEXT, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
CREATE INDEX session_message_session ON session_message (session_id, time_created)`)
	tx, err := d.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	const others = 3000 // 6 s of reading at slowRow each
	for i := range others {
		if _, err := tx.Exec(`INSERT INTO message_rows VALUES (?,?,?,?)`, fmt.Sprintf("old%d", i), "ses_old",
			start.Add(-time.Hour).UnixMilli()+int64(i), `{"role":"assistant","time":{"created":1,"completed":2}}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d.exec(`INSERT INTO session_v2 VALUES ('s',NULL,?,'live',?,?,NULL)`, cwd, start.Add(time.Second).UnixMilli(), start.Add(time.Minute).UnixMilli())
	d.exec(`INSERT INTO session_message VALUES ('m1','s','user',1,?,?,'{"text":"go"}')`, start.Add(2*time.Second).UnixMilli(), start.Add(2*time.Second).UnixMilli())
	d.exec(`INSERT INTO session_message VALUES ('m2','s','assistant',2,?,?,?)`, start.Add(3*time.Second).UnixMilli(), start.Add(3*time.Second).UnixMilli(),
		js(map[string]any{"time": map[string]any{"created": start.Add(3 * time.Second).UnixMilli()},
			"content": []any{map[string]any{"type": "tool", "name": "bash", "state": map[string]any{"status": "running", "input": map[string]any{"command": "go test"}}}}}))
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Start: 1000, Cwd: cwd}))
	if err != nil {
		t.Fatal(err)
	}
	src := New(sessions.Env{DataHome: data})
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // the busy check's scan deadline
	defer cancel()
	slowReads.Store(0)
	got, err := sessions.Scan(ctx, &sessions.World{Now: start.Add(2 * time.Minute), Procs: tb}, src)
	if err != nil || len(got) != 1 || got[0].Phase != sessions.Working {
		t.Fatalf("the live session: %v %+v", err, got)
	}
	if n := slowReads.Load(); n > 0 {
		t.Errorf("the scan read %d rows of another session", n)
	}
}
