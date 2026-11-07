package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// The loop's runs are a kept table: a store from before them gains the table at its next open and keeps its events,
// the report's read-only open refuses such a store until then, and nothing drops the runs after.
func TestTheLoopsRunsAreKept(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	if _, err := st.Put(ctx, []Event{{Source: "mistakes", SourceKey: "k", TS: time.Now(), Actor: ActorTool, Kind: "tool-error"}}); err != nil {
		t.Fatal(err)
	}
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "kept", Evidence: refsAt("a", 1, time.Now())})
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range loopTableNames {
		if _, err := db.Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("a store without the loop's runs read as current: %v", err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if evs, err := st.Events(ctx, Query{Source: "mistakes"}); err != nil || len(evs) != 1 {
		t.Fatalf("the events of a store from before the loop's runs: %d %v", len(evs), err)
	}
	started := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if err := st.PutLoopRun(ctx, LoopRun{ID: "r1", Case: c.ID, Family: "failures", Step: "investigate", Started: started, Status: RunRunning, USD: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Rebuild(ctx, "mistakes", false); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.LoopRunByID(ctx, "r1")
	if err != nil || got.Case != c.ID || got.Status != RunRunning || got.USD != 4 || !got.Started.Equal(started) {
		t.Fatalf("the run after a rebuild and a reopen: %+v %v", got, err)
	}
}

// What the loop has spent is what its live runs spent: an evaluation's runs are its own, and a run still running
// counts what is reserved for it.
func TestOnlyLiveRunsCountInTheLoopsSpend(t *testing.T) {
	ctx := context.Background()
	st := caseStore(t)
	now := time.Now()
	for _, r := range []LoopRun{
		{ID: "live", Status: RunOK, USD: 3, Started: now.Add(-time.Hour)},
		{ID: "evaluation", Status: RunOK, USD: 50, Started: now.Add(-time.Hour), Experiment: "eval:x:task:1"},
		{ID: "running", Status: RunRunning, USD: 0, Started: now.Add(-time.Minute)},
		{ID: "old", Status: RunOK, USD: 20, Started: now.Add(-48 * time.Hour)},
	} {
		r.Case, r.Family, r.Step = "c0", "failures", "investigate"
		if err := st.PutLoopRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if spent, err := st.LoopSpend(ctx, now.Add(-24*time.Hour), 4); err != nil || spent != 7 {
		t.Fatalf("the day's spend: %v %v, want the live run's 3 and the running run's reserved 4", spent, err)
	}
}

// A run's tokens by kind are kept with it, when it starts, ends and is read again.
func TestALoopRunKeepsItsTokensByKind(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	defer st.Close()
	c, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "a", Title: "a"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.UnixMilli(1_700_000_000_000)
	if err := st.PutLoopRun(ctx, LoopRun{ID: "r1", Case: c.ID, Family: "failures", Step: "investigate", Started: t0, Status: RunRunning, USD: 4}); err != nil {
		t.Fatal(err)
	}
	u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 5, CacheWrite1h: 4}
	if err := st.FinishLoopRun(ctx, LoopRun{ID: "r1", Ended: t0.Add(time.Minute), Status: RunOK, USD: 0.5, Tokens: u.Total(), Usage: u}); err != nil {
		t.Fatal(err)
	}
	if r, err := st.LoopRunByID(ctx, "r1"); err != nil || r.Usage != u || r.Tokens != 11 {
		t.Fatalf("finished: %+v %v", r, err)
	}
	v := Usage{Input: 6, Output: 7, CacheRead: 8, CacheWrite: 9}
	if err := st.PutLoopRun(ctx, LoopRun{ID: "r2", Case: c.ID, Family: "failures", Step: "judge", Started: t0, Ended: t0, Status: RunOK, Usage: v, Experiment: "eval:x"}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.LoopRuns(ctx, RunFilter{})
	got := map[string]Usage{}
	for _, r := range rs {
		got[r.ID] = r.Usage
	}
	if err != nil || len(rs) != 2 || got["r1"] != u || got["r2"] != v {
		t.Fatalf("listed: %+v %v", rs, err)
	}
}

// A refused build's cause is kept with its run, when it is put and when it ends. A loop_runs table from before the
// cause gains its column at the next open and keeps its rows, which hold no cause; until then the read-only open
// refuses the store.
func TestALoopRunKeepsItsCauseAndAnOlderTableGainsIt(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	c, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "a", Title: "a"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.UnixMilli(1_700_000_000_000)
	if err := st.PutLoopRun(ctx, LoopRun{ID: "old", Case: c.ID, Family: "failures", Step: "build", Started: t0, Ended: t0.Add(time.Second),
		Status: RunFailed, Failure: "build propose", Summary: "build propose: refused: an older reason"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE loop_runs DROP COLUMN cause`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("a store without the cause read as current: %v", err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if r, err := st.LoopRunByID(ctx, "old"); err != nil || r.Failure != "build propose" || r.Cause != "" || r.Summary != "build propose: refused: an older reason" {
		t.Fatalf("the run from before the cause: %+v %v", r, err)
	}
	cause := "refused: the patch holds Q"
	if err := st.PutLoopRun(ctx, LoopRun{ID: "put", Case: c.ID, Family: "failures", Step: "build", Started: t0, Ended: t0.Add(time.Second),
		Status: RunFailed, Failure: "build propose", Cause: cause}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLoopRun(ctx, LoopRun{ID: "finished", Case: c.ID, Family: "failures", Step: "build", Started: t0, Status: RunRunning, USD: 4}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishLoopRun(ctx, LoopRun{ID: "finished", Ended: t0.Add(time.Minute), Status: RunFailed, Failure: "build check", Cause: cause, USD: 1}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.LoopRuns(ctx, RunFilter{Case: c.ID})
	got := map[string]string{}
	for _, r := range rs {
		got[r.ID] = r.Cause
	}
	if err != nil || len(rs) != 3 || got["old"] != "" || got["put"] != cause || got["finished"] != cause {
		t.Fatalf("the causes kept: %v %v", got, err)
	}
}

// A loop_runs table from before the tokens by kind gains their columns and keeps its rows: the columns are the
// table's next ones in keptColumns, not in the CREATE TABLE.
func TestALoopRunsTableFromBeforeTheTokensByKindGainsThem(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	if err := st.PutLoopRun(ctx, LoopRun{ID: "old", Case: "c1", Family: "failures", Step: "investigate", Started: t0, Ended: t0, Status: RunOK, USD: 1, Tokens: 9}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loopRunColumns {
		if _, err := db.Exec(`ALTER TABLE loop_runs DROP COLUMN ` + c[0]); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if r, err := st.LoopRunByID(ctx, "old"); err != nil || r.Tokens != 9 || r.Usage != (Usage{}) {
		t.Fatalf("the old run: %+v %v", r, err)
	}
}

// The turns a pass moved are written together or not at all: a row the store refuses leaves the others as they were.
func TestSetLoopTurnsWritesAllOrNone(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	if err := st.SetLoopTurns(ctx, map[string]string{"build": "a", "run": "a"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER refuse_single BEFORE INSERT ON loop_turns WHEN NEW.step = 'single'
	  BEGIN SELECT RAISE(ABORT, 'marker-7f3a'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLoopTurns(ctx, map[string]string{"build": "b", "run": "b", "single": "build"}); err == nil {
		t.Fatal("the refused row was written")
	}
	for _, step := range []string{"build", "run"} {
		if got, err := st.LoopTurn(ctx, step); err != nil || got != "a" {
			t.Errorf("the %s turn is %q after a refused write, want %q: %v", step, got, "a", err)
		}
	}
}
