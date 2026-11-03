package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHeldBuildChargeSurvivesReopenWithoutRunCounts(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	c, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "marker-7f3a", Title: "One cause"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	for _, entry := range []struct {
		id         string
		started    time.Time
		experiment string
		usd        float64
	}{
		{"live-hold", now.Add(-time.Minute), "", .125},
		{"old-hold", now.Add(-48 * time.Hour), "", .5},
		{"experiment-hold", now.Add(-time.Minute), "fixture-evaluation", 1},
	} {
		run := LoopRun{ID: entry.id, Case: c.ID, Family: c.Family, Step: "build",
			Started: entry.started, Status: RunRunning, USD: 4, Experiment: entry.experiment}
		if err := st.PutLoopRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		run.Ended, run.USD, run.Tokens = now, entry.usd, 12
		if err := st.HoldLoopRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := st.HoldLoopRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if _, err := st.LoopRunByID(ctx, run.ID); !errors.Is(err, ErrNoRun) {
			t.Fatalf("held build retained its run: %v", err)
		}
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.Runs != 0 || got.Cost.USD != 1.625 || got.Cost.Tokens != 36 {
		t.Fatalf("hold charged more than once or counted runs: %+v %v", got.Cost, err)
	}
	// Filtering uses the original start, not the recent completion receipt.
	if spent, err := st.LoopSpend(ctx, now.Add(-24*time.Hour), 4); err != nil || spent != .125 {
		t.Fatalf("live spend lost a charge or included an old/experiment hold: %g %v", spent, err)
	}
	if spent, err := st.LoopSpend(ctx, now.Add(-7*24*time.Hour), 4); err != nil || spent != .625 {
		t.Fatalf("week spend lost its old live hold: %g %v", spent, err)
	}
}
