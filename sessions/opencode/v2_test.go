package opencode

import (
	"context"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
)

func TestOpenCodeV2PhasesAndLog(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	start := time.Unix(fake.Boot, 0).Add(10 * time.Second)
	d := newDB(t, data)
	d.exec(`CREATE TABLE session_v2 (id TEXT, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER, time_archived INTEGER);
CREATE TABLE session_message (id TEXT, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT)`)
	d.exec(`INSERT INTO session_v2 VALUES ('s',NULL,?,'marker-7f3a',?,?,NULL)`, cwd, start.Add(time.Second).UnixMilli(), start.Add(time.Minute).UnixMilli())
	d.exec(`INSERT INTO session_message VALUES ('m1','s','user',1,?,?,'{"text":"marker-7f3a"}')`, start.Add(2*time.Second).UnixMilli(), start.Add(2*time.Second).UnixMilli())
	d.exec(`INSERT INTO session_message VALUES ('m2','s','assistant',2,?,?,?)`,
		start.Add(3*time.Second).UnixMilli(), start.Add(3*time.Second).UnixMilli(),
		js(map[string]any{"model": map[string]any{"id": "neutral"}, "time": map[string]any{"created": start.Add(3 * time.Second).UnixMilli()},
			"content": []any{map[string]any{"type": "tool", "name": "bash", "state": map[string]any{"status": "running", "input": map[string]any{"command": "cat neutral.txt"}}}}}))
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Start: 1000, Cwd: cwd}))
	if err != nil {
		t.Fatal(err)
	}
	src := New(sessions.Env{DataHome: data})
	defer src.Close()
	w := &sessions.World{Now: start.Add(2 * time.Minute), Procs: tb}
	got, err := sessions.Scan(context.Background(), w, src)
	if err != nil || len(got) != 1 || got[0].Phase != sessions.Working || got[0].Model != "neutral" {
		t.Fatalf("v2 working session: %v %+v", err, got)
	}
	d.exec(`UPDATE session_message SET data=? WHERE id='m2'`,
		js(map[string]any{"model": map[string]any{"id": "neutral"}, "time": map[string]any{"created": start.Add(3 * time.Second).UnixMilli(), "completed": start.Add(4 * time.Second).UnixMilli()},
			"error": map[string]any{"type": "APIError", "message": "marker-7f3a timeout"}}))
	got, err = sessions.Scan(context.Background(), w, src)
	if err != nil || len(got) != 1 || got[0].Phase != sessions.NeedsYou || got[0].AskKind != sessions.AskError {
		t.Fatalf("v2 failed turn: %v %+v", err, got)
	}
}
