package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/internal/saved"
	"github.com/agent-sh/momentum/sessions/proc"
)

// newDBAt opens the database already at path.
func newDBAt(t *testing.T, path string) *ocDB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &ocDB{t: t, db: db}
}

func TestAResumedRunDerivesWhatAColdRunDerives(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	start := time.Unix(fake.Boot, 0).Add(10 * time.Second)
	d := newDB(t, data)
	d.session("ses", "", cwd, "port the parser", start.Add(time.Second), start.Add(time.Hour))
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"opencode"}, Start: 1000, Cwd: cwd}))
	if err != nil {
		t.Fatal(err)
	}
	at := start.Add(2 * time.Second)
	n := 0
	add := func(role string, completed bool, text string) string { // a message and its text part, a second after the last
		t.Helper()
		at, n = at.Add(time.Second), n+1
		id := fmt.Sprintf("m%03d", n)
		m := map[string]any{"role": role, "time": map[string]any{"created": at.UnixMilli()}}
		if completed {
			m["time"] = map[string]any{"created": at.UnixMilli(), "completed": at.UnixMilli()}
		}
		d.message(id, "ses", at, m)
		d.part("p"+id, id, "ses", at, map[string]any{"type": "text", "text": text})
		return id
	}
	run := func(mem *saved.Mem) []*sessions.Session {
		t.Helper()
		src := New(sessions.Env{DataHome: data})
		defer src.Close()
		if mem != nil {
			src.Resume(mem)
		}
		ss, err := sessions.Scan(context.Background(), &sessions.World{Now: start.Add(2 * time.Hour), Procs: tb}, src)
		if err != nil || len(ss) != 1 {
			t.Fatalf("got %v %+v", err, ss)
		}
		if mem != nil {
			mem.Save(src.Checkpoints())
		}
		return ss
	}
	mem := saved.New()
	round := func(name string) []*sessions.Session {
		t.Helper()
		resumed := run(mem)
		if diff := saved.Same(resumed, run(nil)); diff != "" {
			t.Fatalf("%s: %s", name, diff)
		}
		return resumed
	}

	add("user", false, "port the parser")
	add("assistant", true, "Ported the lexer.")
	round("a turn")
	add("user", false, "now the parser")
	streaming := add("assistant", false, "porting")
	round("an answer still streaming")
	d.exec(`UPDATE message SET data = ? WHERE id = ?`, js(map[string]any{"role": "assistant", "time": map[string]any{"created": 1, "completed": 2}}), streaming)
	round("the answer is done")
	same := at.Add(time.Second)
	d.message("t1", "ses", same, map[string]any{"role": "user"})
	d.part("pt1", "t1", "ses", same, map[string]any{"type": "text", "text": "one"})
	d.message("t2", "ses", same, map[string]any{"role": "user"}) // the same millisecond
	d.part("pt2", "t2", "ses", same, map[string]any{"type": "text", "text": "two"})
	at = same
	round("two messages at once")

	d.exec(`DELETE FROM message WHERE id = 't2'`) // an undo takes back a message already read
	add("user", false, "instead, this")
	round("a message read before is gone")

	// the database is replaced by another file: a copy that tells one message read before differently
	path := filepath.Join(data, "opencode", "opencode.db")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fake.File(t, path+".new", string(raw))
	newDBAt(t, path+".new").exec(`UPDATE part SET data = ? WHERE message_id = 'm002'`, js(map[string]any{"type": "text", "text": "Ported the lexer and its tests."}))
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	d = newDBAt(t, path)
	add("assistant", true, "Done with the parser.")
	round("a new database file")

	var middle string
	for i := 0; i < 5; i++ {
		id := add("user", false, fmt.Sprintf("step %d", i))
		if i == 2 {
			middle = id
		}
	}
	round("more")
	if mem.Loads == 0 {
		t.Fatal("no run resumed from a checkpoint")
	}
	d.exec(`UPDATE part SET data = ? WHERE message_id = ?`, js(map[string]any{"type": "text", "text": "EDITED"}), middle)
	logText := func(ss []*sessions.Session) string { items, _ := ss[0].Log.Since(0); return fmt.Sprint(items) }
	if resumed, cold := run(mem), run(nil); strings.Contains(logText(resumed), "EDITED") || !strings.Contains(logText(cold), "EDITED") {
		t.Fatal("the resumed run read the session's messages again from the start")
	}
}
