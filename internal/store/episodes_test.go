package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReservationKeepsItsEpisodeAfterACaseComesBack(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	finding := Finding{Family: "failures", Signature: "symptom", Title: "marker-7f3a",
		Evidence: []Evidence{{Ref: "file:/fixture/first", At: now}}}
	c, _, err := st.ObserveCase(ctx, finding, "test")
	if err != nil {
		t.Fatal(err)
	}
	run := LoopRun{ID: "reservation", Case: c.ID, Step: "investigate", Started: now, Status: RunRunning}
	if err := st.PutLoopRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	reserved, err := st.LoopRunByID(ctx, run.ID)
	if err != nil || reserved.CaseEpisode != c.Episode {
		t.Fatalf("reservation lost the episode: %+v %v", reserved, err)
	}
	if _, err := st.CloseCase(ctx, c.ID, Solved, "", "fixture", "test"); err != nil {
		t.Fatal(err)
	}
	finding.Evidence = []Evidence{{Ref: "file:/fixture/second", At: now.Add(time.Minute)}}
	back, observed, err := st.ObserveCase(ctx, finding, "test")
	if err != nil || observed != Reopened || back.Episode != c.Episode+1 {
		t.Fatalf("comeback did not advance episode: %+v %s %v", back, observed, err)
	}
	mechanism := "old cause"
	for _, undecided := range []bool{false, true} {
		var recorded bool
		if undecided {
			_, recorded, err = st.RecordUndecided(ctx, c.ID, reserved.CaseEpisode, CaseUpdate{Mechanism: &mechanism}, "old report", "test")
		} else {
			_, recorded, err = st.RecordFindings(ctx, c.ID, reserved.CaseEpisode, CaseUpdate{Mechanism: &mechanism}, "test")
		}
		if err != nil || recorded {
			t.Fatalf("recorded old episode (undecided=%v): %v %v", undecided, recorded, err)
		}
	}
	// Updating a run's completion must preserve the episode captured by its first insert.
	run.Status, run.Ended = RunFailed, time.Now()
	if err := st.PutLoopRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoopRunByID(ctx, run.ID)
	if err != nil || got.CaseEpisode != reserved.CaseEpisode {
		t.Fatalf("completion replaced reservation's episode: %+v %v", got, err)
	}
}
