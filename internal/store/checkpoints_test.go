package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

func TestStampsFromBeforeGainTheirColumnsAndKeepTheirRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// session_stamps as the first sessions release made it
	if _, err := old.Exec(`CREATE TABLE session_stamps (key TEXT PRIMARY KEY, phase TEXT NOT NULL, since INTEGER NOT NULL,
	  seq INTEGER NOT NULL, seen INTEGER NOT NULL);
	  INSERT INTO session_stamps VALUES ('a', 'idle', 5, 3, 6);
	  PRAGMA user_version = 2;`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	got, err := st.SessionStamps(ctx)
	if err != nil || len(got) != 1 || got["a"].Phase != sessions.Idle || got["a"].Seq != 3 || got["a"].Tool != "" {
		t.Fatalf("the old stamp is gone: %+v %v", got, err)
	}
	want := state.Stamp{Phase: sessions.NeedsYou, Since: time.UnixMilli(7), Seq: 4, Seen: time.UnixMilli(8), Tool: "codex", Name: "fix it",
		Auto: true, Ask: "Delete the old data?", AskKind: sessions.AskQuestion}
	if err := st.SaveSessionStamps(ctx, state.Stamps{"a": want}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SessionStamps(ctx); err != nil || !reflect.DeepEqual(got["a"], want) {
		t.Fatalf("got %+v %v", got["a"], err)
	}
}

// saveRun saves checkpoints as a sessions run does.
func saveRun(t *testing.T, st *Store, tr *Trackers, now time.Time, cps ...sessions.Checkpoint) {
	t.Helper()
	if err := st.SessionRun(context.Background(), func(r *Run) error { return r.SaveCheckpoints(tr, cps, now) }); err != nil {
		t.Fatal(err)
	}
}

// sameLog fails unless a log the store gave back holds what the live log holds.
func sameLog(t *testing.T, what string, got *sessions.Checkpoint, want *sessions.Log, state string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no checkpoint", what)
	}
	gi, _ := got.Log.Since(0)
	wi, _ := want.Since(0)
	if string(got.State) != state || got.Log.Seq() != want.Seq() || got.Log.Base() != want.Base() || !reflect.DeepEqual(gi, wi) {
		t.Fatalf("%s: state %q seq %d/%d base %d/%d entries %d/%d", what, got.State, got.Log.Seq(), want.Seq(), got.Log.Base(),
			want.Base(), len(gi), len(wi))
	}
}

func TestCheckpointsKeepTheirLogEntryByEntry(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	open := func() *Store {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	st := open()
	tr := st.Trackers("b1")
	if cp, err := tr.Checkpoint(ctx, "claude", "/t/a.jsonl"); cp != nil || err != nil {
		t.Fatalf("a checkpoint before any run: %+v %v", cp, err)
	}
	var live sessions.Log
	add := func(n int) {
		for i := 0; i < n; i++ {
			live.Add(sessions.Entry{Role: sessions.RoleTool, Name: "Bash", Origin: "", Text: fmt.Sprint("step ", live.Seq())})
		}
	}
	t0 := time.UnixMilli(1_700_000_000_000)
	add(10)
	saveRun(t, st, tr, t0, sessions.Checkpoint{Tool: "claude", Key: "/t/a.jsonl", State: []byte(`{"off":10}`), Log: &live})
	// the next run, in a new process: a new store handle
	st = open()
	tr = st.Trackers("b1")
	cp, err := tr.Checkpoint(ctx, "claude", "/t/a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	sameLog(t, "after a run", cp, &live, `{"off":10}`)
	// it resumes with the stored log and adds past LogKeep, so the oldest quarter goes
	live = *cp.Log
	add(sessions.LogKeep)
	saveRun(t, st, tr, t0.Add(time.Minute), sessions.Checkpoint{Tool: "claude", Key: "/t/a.jsonl", State: []byte(`{"off":20}`), Log: &live})
	cp, _ = open().Trackers("b1").Checkpoint(ctx, "claude", "/t/a.jsonl")
	sameLog(t, "after a trim", cp, &live, `{"off":20}`)
	if live.Base() == 0 {
		t.Fatal("setup: the log did not trim")
	}
	// the transcript was rewritten: the tracker starts a new log, and the store forgets the old one
	var fresh sessions.Log
	fresh.Add(sessions.Entry{Role: sessions.RoleUser, Origin: "human/typed", Text: "fresh start"})
	saveRun(t, st, tr, t0.Add(2*time.Minute), sessions.Checkpoint{Tool: "claude", Key: "/t/a.jsonl", State: []byte(`{"off":1}`), Log: &fresh})
	cp, _ = open().Trackers("b1").Checkpoint(ctx, "claude", "/t/a.jsonl")
	sameLog(t, "after a restart", cp, &fresh, `{"off":1}`)
	if cp, err := open().Trackers("b2").Checkpoint(ctx, "claude", "/t/a.jsonl"); cp != nil || err != nil {
		t.Fatalf("another build's checkpoint was handed out: %+v %v", cp, err)
	}
	// unused for longer than the window: dropped with its log
	if err := st.SessionRun(ctx, func(r *Run) error { return r.DropCheckpoints(t0.Add(3 * time.Minute)) }); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := st.db.QueryRow(`SELECT (SELECT COUNT(*) FROM trackers) + (SELECT COUNT(*) FROM tracker_log)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d rows left, %v", rows, err)
	}
}

func TestAnUnchangedCheckpointIsOnlyMarkedSeen(t *testing.T) {
	st, _ := open(t)
	tr := st.Trackers("b1")
	var l sessions.Log
	l.Add(sessions.Entry{Role: sessions.RoleOut, Text: "ok"})
	t0 := time.UnixMilli(1_700_000_000_000)
	saveRun(t, st, tr, t0, sessions.Checkpoint{Tool: "pi", Key: "k", State: []byte("s1"), Log: &l})
	if _, err := st.db.Exec(`UPDATE tracker_log SET text = 'marker'`); err != nil { // a rewrite would replace it
		t.Fatal(err)
	}
	saveRun(t, st, tr, t0.Add(time.Hour), sessions.Checkpoint{Tool: "pi", Key: "k", State: []byte("s1"), Log: &l})
	var text string
	var seen int64
	if err := st.db.QueryRow(`SELECT text, seen FROM tracker_log JOIN trackers USING (tool, key)`).Scan(&text, &seen); err != nil {
		t.Fatal(err)
	}
	if text != "marker" || seen != t0.Add(time.Hour).UnixMilli() {
		t.Fatalf("text %q seen %d", text, seen)
	}
}

func TestObservedEventsSurviveASchemaChange(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1_700_000_000_000)
	if _, err := st.Put(ctx, []Event{
		{Source: Sessions, SourceKey: "s1@1", TS: at, Session: "s1", Harness: "codex", Actor: ActorHarness, Kind: "phase", Meta: `{"from":"needs_you"}`},
		{Source: "mistakes", SourceKey: "m1", TS: at, Actor: ActorTool, Kind: "tool-error"},
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil { // a store from another derived schema
		t.Fatal(err)
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(ctx, Query{Since: at.Add(-time.Hour)})
	if err != nil || len(evs) != 1 || evs[0].Source != Sessions || evs[0].Meta != `{"from":"needs_you"}` || evs[0].Harness != "codex" {
		t.Fatalf("the derived events go, the observed ones stay: %+v %v", evs, err)
	}
}

func TestObservedEventsMoveIntoANewEventsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// an events table with fewer columns than today's, and no ingested column
	if _, err := old.Exec(`CREATE TABLE events (id TEXT PRIMARY KEY, source TEXT, source_key TEXT, ts INTEGER, actor TEXT, kind TEXT);
	  INSERT INTO events VALUES ('x', 'sessions', 'k@1', 1700000000000, 'harness', 'vanished');
	  INSERT INTO events VALUES ('y', 'mistakes', 'k', 1700000000000, 'owner', 'correction');`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(context.Background(), Query{Since: time.UnixMilli(0)})
	if err != nil || len(evs) != 1 || evs[0].Kind != "vanished" || evs[0].SourceKey != "k@1" {
		t.Fatalf("got %+v %v", evs, err)
	}
}

func TestRebuildRefusesObservedEvents(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if _, err := st.Put(ctx, []Event{{Source: Sessions, SourceKey: "s@1", TS: time.Now(), Actor: ActorHarness, Kind: "phase"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Rebuild(ctx, Sessions); !errors.Is(err, ErrObserved) {
		t.Fatalf("err %v", err)
	}
	if evs, _ := st.Events(ctx, Query{Since: time.Unix(0, 0), Source: Sessions}); len(evs) != 1 {
		t.Fatal("the observed events were dropped")
	}
}

func TestARunThatLoadedBeforeAnotherSavedWritesItsLogWhole(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	var first sessions.Log
	for i := 0; i < 10; i++ {
		first.Add(sessions.Entry{Role: sessions.RoleOut, Text: fmt.Sprint(i)})
	}
	t0 := time.UnixMilli(1_700_000_000_000)
	saveRun(t, st, st.Trackers("b"), t0, sessions.Checkpoint{Tool: "codex", Key: "k", State: []byte("s0"), Log: &first})
	// two runs load the same checkpoint; the first to save grew its log past a trim
	trA, trB := st.Trackers("b"), st.Trackers("b")
	cpA, _ := trA.Checkpoint(ctx, "codex", "k")
	cpB, _ := trB.Checkpoint(ctx, "codex", "k")
	logA, logB := *cpA.Log, *cpB.Log
	for i := 0; i < sessions.LogKeep; i++ {
		logA.Add(sessions.Entry{Role: sessions.RoleOut, Text: "a"})
	}
	for i := 0; i < 5; i++ {
		logB.Add(sessions.Entry{Role: sessions.RoleOut, Text: "b"})
	}
	saveRun(t, st, trA, t0.Add(time.Minute), sessions.Checkpoint{Tool: "codex", Key: "k", State: []byte("sA"), Log: &logA})
	saveRun(t, st, trB, t0.Add(time.Minute), sessions.Checkpoint{Tool: "codex", Key: "k", State: []byte("sB"), Log: &logB})
	cp, err := st.Trackers("b").Checkpoint(ctx, "codex", "k")
	if err != nil {
		t.Fatal(err)
	}
	sameLog(t, "the later save", cp, &logB, "sB")
}

func TestALoadNeverMixesTwoSaves(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := Open(path) // another run, with its own handle
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tr := writer.Trackers("b")
	var live sessions.Log
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 400; i++ { // each save adds an entry; its state says how long the log is
			live.Add(sessions.Entry{Role: sessions.RoleOut, Text: fmt.Sprint(i)})
			err := writer.SessionRun(ctx, func(r *Run) error {
				return r.SaveCheckpoints(tr, []sessions.Checkpoint{{Tool: "claude", Key: "k", State: []byte(fmt.Sprint(live.Seq())), Log: &live}}, time.Now())
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	loads, torn := 0, 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		cp, err := reader.Trackers("b").Checkpoint(ctx, "claude", "k")
		if err != nil {
			t.Fatal(err)
		}
		if cp == nil {
			continue
		}
		loads++
		if string(cp.State) != fmt.Sprint(cp.Log.Seq()) {
			torn++
		}
	}
	if torn > 0 {
		t.Fatalf("%d of %d loads mixed a state and a log from two saves", torn, loads)
	}
}

func TestALogShorterThanItsCheckpointIsRefused(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	var l sessions.Log
	for i := 0; i < 3; i++ {
		l.Add(sessions.Entry{Role: sessions.RoleOut, Text: fmt.Sprint(i)})
	}
	saveRun(t, st, st.Trackers("b"), time.UnixMilli(1_700_000_000_000), sessions.Checkpoint{Tool: "pi", Key: "k", State: []byte("s"), Log: &l})
	if _, err := st.db.Exec(`DELETE FROM tracker_log WHERE seq = 2`); err != nil { // the newest entry is lost
		t.Fatal(err)
	}
	if cp, err := st.Trackers("b").Checkpoint(ctx, "pi", "k"); err == nil {
		t.Fatalf("a short log was handed out: %d entries", cp.Log.Seq())
	}
}

func TestFirstOpensOfAnOlderStoreAtOnceAllSucceed(t *testing.T) {
	for round := 0; round < 10; round++ {
		path := filepath.Join(t.TempDir(), "m.db")
		old, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		// a store as the first sessions release left it, in WAL mode as momentum opens every store
		if _, err := old.Exec(`PRAGMA journal_mode = WAL; CREATE TABLE session_stamps (key TEXT PRIMARY KEY, phase TEXT NOT NULL,
		  since INTEGER NOT NULL, seq INTEGER NOT NULL, seen INTEGER NOT NULL); PRAGMA user_version = 2;`); err != nil {
			t.Fatal(err)
		}
		old.Close()
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ { // a timer run and an evidence query starting together after an upgrade
			go func() {
				st, err := Open(path)
				if err == nil {
					st.Close()
				}
				errs <- err
			}()
		}
		for i := 0; i < 4; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}

func TestSessionsRunsTakeTurns(t *testing.T) {
	st, path := open(t)
	other, err := Open(path) // another run, with its own handle
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	unlock, err := st.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func())
	go func() {
		u, err := other.LockSessions()
		if err != nil {
			t.Error(err)
			u = func() {}
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("a second run took the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the second run never got the lock")
	}
}
