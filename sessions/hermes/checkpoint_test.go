package hermes

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

func TestAResumedRunDerivesWhatAColdRunDerives(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(fake.Boot+86400, 0)
	sec := float64(now.Unix())
	db := stateDB(t, home, true)
	exec(t, db, `INSERT INTO sessions VALUES ('c', 'cli', 'Fix the backup', '', '/w', 'model-a', ?, NULL, ?, '', 0, 0)`, sec-3600, sec-60)
	at := sec - 3000
	msg := func(db *sql.DB, role, content, calls string) {
		t.Helper()
		at++
		exec(t, db, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES ('c', ?, ?, ?, ?)`, role, content, calls, at)
	}
	tb, _ := proc.Read(fake.ProcRoot(t))
	run := func(mem *saved.Mem) []*sessions.Session {
		t.Helper()
		src := New(sessions.Env{HermesHome: home})
		defer src.Close()
		if mem != nil {
			src.Resume(mem)
		}
		ss, err := sessions.Scan(context.Background(), &sessions.World{Now: now, Procs: tb}, src)
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

	msg(db, "user", "back it up", "")
	msg(db, "assistant", "Running it.", `[{"function":{"name":"terminal","arguments":"{\"command\":\"restic backup\"}"}}]`)
	if ss := round("a call runs"); ss[0].Phase != sessions.Working {
		t.Fatalf("got %s", ss[0].Phase)
	}
	msg(db, "tool", "snapshot saved", "")
	msg(db, "assistant", "Backed up.", "[]")
	round("the turn ends")
	exec(t, db, `DELETE FROM messages WHERE content = 'snapshot saved'`) // a message read before is taken back
	msg(db, "user", "and the photos", "")
	round("a message read before is gone")

	// the database is replaced by another file: a copy that tells one message read before differently
	path := filepath.Join(home, "state.db")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fake.File(t, path+".new", string(raw))
	other, err := sql.Open("sqlite", "file:"+path+".new")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, other, `UPDATE messages SET content = 'Running the backup.' WHERE content = 'Running it.'`)
	other.Close()
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	msg(db, "assistant", "Both are backed up.", "[]")
	round("a new database file")

	for i := 0; i < 5; i++ {
		msg(db, "user", fmt.Sprintf("step %d", i), "")
	}
	round("more")
	if mem.Loads == 0 {
		t.Fatal("no run resumed from a checkpoint")
	}
	exec(t, db, `UPDATE messages SET content = 'EDITED' WHERE content = 'step 2'`)
	logText := func(ss []*sessions.Session) string { items, _ := ss[0].Log.Since(0); return fmt.Sprint(items) }
	if resumed, cold := run(mem), run(nil); strings.Contains(logText(resumed), "EDITED") || !strings.Contains(logText(cold), "EDITED") {
		t.Fatal("the resumed run read the conversation again from the start")
	}
}
