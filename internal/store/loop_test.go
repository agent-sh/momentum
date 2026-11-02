package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

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
