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
