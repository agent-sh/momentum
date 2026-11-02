package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
)

// A vanished loop run keeps its directory through the saved stamp and contributes no activity.
func TestAVanishedLoopRunKeepsItsDirectory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	loopDir := "/synthetic/loop"
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	run := fixed{{Key: "loop-run-1", Tool: "claude", Auto: true, TurnOpen: true, Cwd: filepath.Join(loopDir, "runs", "r1")}}
	if _, _, err := board(ctx, &sessions.World{Now: t0}, nil, st, "test", []sessions.Source{run}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{t0.Add(5 * time.Minute), t0.Add(40 * time.Minute)} {
		if _, _, err := board(ctx, &sessions.World{Now: at}, nil, st, "test", []sessions.Source{fixed{}}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := st.Events(ctx, store.Query{Since: t0.Add(-time.Hour), Until: t0.Add(2 * time.Hour), Source: store.Sessions, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		t.Logf("%s %s cwd=%q at %s", e.Kind, e.Session, e.Cwd, e.TS.Format(time.TimeOnly))
	}
	units, err := st.Activity(ctx, store.ActivityQuery{Since: t0.Add(-time.Hour), Until: t0.Add(2 * time.Hour), ExcludeCwd: loopDir, BySession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("activity with the loop's directory excluded: %v", units)
	if len(units) != 0 {
		t.Fatalf("the vanished run is no unit: %v", units)
	}
}
