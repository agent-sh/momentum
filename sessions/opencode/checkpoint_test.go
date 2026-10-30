package opencode

import (
	"context"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/internal/saved"
	"github.com/agent-sh/momentum/sessions/proc"
)

func TestAPartRewrittenInPlaceShowsInTheNextRun(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	start := time.Unix(fake.Boot, 0).Add(10 * time.Second)
	d := newDB(t, data)
	d.session("ses", "", cwd, "port the parser", start.Add(time.Second), start.Add(time.Hour))
	d.message("m1", "ses", start.Add(2*time.Second), map[string]any{"role": "user"})
	d.part("p1", "m1", "ses", start.Add(2*time.Second), map[string]any{"type": "text", "text": "port the lexer"})
	d.message("m2", "ses", start.Add(3*time.Second), map[string]any{"role": "assistant", "time": map[string]any{"created": 1, "completed": 2}})
	d.part("p2", "m2", "ses", start.Add(3*time.Second), map[string]any{"type": "text", "text": "Ported."})
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Start: 1000, Cwd: cwd}))
	if err != nil {
		t.Fatal(err)
	}
	mem := saved.New()
	run := func(mem *saved.Mem) []*sessions.Session { // one CLI run in its own process
		t.Helper()
		src := New(sessions.Env{DataHome: data})
		defer src.Close()
		r, resumable := any(src).(sessions.Resumable)
		if resumable && mem != nil {
			r.Resume(mem)
		}
		ss, err := sessions.Scan(context.Background(), &sessions.World{Now: start.Add(2 * time.Hour), Procs: tb}, src)
		if err != nil || len(ss) != 1 {
			t.Fatalf("got %v %+v", err, ss)
		}
		if resumable && mem != nil {
			mem.Save(r.Checkpoints())
		}
		return ss
	}
	run(mem)
	d.exec(`UPDATE part SET data = ? WHERE id = 'p2'`, js(map[string]any{"type": "text", "text": "Ported the lexer and its tests."}))
	next, cold := run(mem), run(nil)
	if diff := saved.Same(next, cold); diff != "" {
		t.Fatalf("the next run is not what the database holds: %s", diff)
	}
	if items, _ := next[0].Log.Since(0); len(items) != 2 || items[1].Text != "Ported the lexer and its tests." {
		t.Fatalf("log %v", items)
	}
}
