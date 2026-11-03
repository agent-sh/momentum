package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreURIKeepsFragmentAndQueryCharactersInThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture#00?copy.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ev := Event{Source: "fixture", SourceKey: "marker-7f3a", Kind: "tool_result", Actor: ActorTool, TS: time.Now()}
	if _, err := st.Put(context.Background(), []Event{ev}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := ro.EventByID(context.Background(), ev.ID())
	ro.Close()
	if err != nil || !found {
		t.Fatal("read-only open lost the literal path")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != filepath.Base(path) && name != filepath.Base(path)+"-wal" && name != filepath.Base(path)+"-shm" {
			t.Fatal("SQLite opened a truncated URI path")
		}
	}
	copy := filepath.Join(dir, "snapshot#00?.sqlite")
	if err := AsOf(context.Background(), path, copy, ev.TS.Add(-time.Hour), ev.TS.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	snap, err := OpenReadOnly(copy)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if _, found, err := snap.EventByID(context.Background(), ev.ID()); err != nil || !found {
		t.Fatal("snapshot did not escape its source or destination")
	}
}

func TestNeverStartedRunIsListedByItsEndAndDoesNotCountOnTheCase(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "fixture", Title: "marker-7f3a", Evidence: refsAt("a", 1, time.Now())})
	run := LoopRun{ID: "fixture", Case: c.ID, Step: "investigate", Started: time.Now(), Status: RunRunning, USD: 4}
	if err := st.PutLoopRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Started, run.Ended, run.Status, run.Failure, run.USD = time.Time{}, time.Now(), RunFailed, "sandbox", 0
	if err := st.FinishLoopRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	runs, err := st.LoopRuns(ctx, RunFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil || len(runs) != 1 || !runs[0].Started.IsZero() || runs[0].Failure != "sandbox" {
		t.Fatal("never-started run is absent from the window")
	}
	runs, err = st.LoopRuns(ctx, RunFilter{Since: time.Now().Add(-time.Hour), Ran: true})
	if err != nil || len(runs) != 0 {
		t.Fatal("stop rule's started filter included a failed start")
	}
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.Runs != 0 || got.Cost.USD != 0 || got.Cost.Duration != 0 {
		t.Fatalf("failed start charged the case: %+v %v", got.Cost, err)
	}
}
