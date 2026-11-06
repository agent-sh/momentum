package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// The write sequence never goes down: a rebuild that deletes the rows that held the highest does not lower it, and a
// row a build from before it writes, or rewrites, without one is stamped from it. A watch that read the sequence sees
// every later write above it.
func TestTheWriteSequenceNeverGoesDown(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	ev := func(source, key string) Event {
		return Event{Source: source, SourceKey: key, TS: at, Actor: ActorTool, Kind: "tool_result", Summary: "ok"}
	}
	seqOf := func(id string) int64 {
		t.Helper()
		var seq int64
		if err := st.db.QueryRowContext(ctx, `SELECT seq FROM events WHERE id = ?`, id).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	if _, err := st.Put(ctx, []Event{ev("zz-a", "1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, []Event{ev("zz-b", "1"), ev("zz-b", "2")}); err != nil {
		t.Fatal(err)
	}
	top, err := st.Seq(ctx)
	if err != nil || top < 2 {
		t.Fatalf("the sequence after two writes: %d %v", top, err)
	}
	if _, err := st.Rebuild(ctx, "zz-b", true); err != nil { // the rows that held the highest go
		t.Fatal(err)
	}
	if seq, _ := st.Seq(ctx); seq < top {
		t.Fatalf("a rebuild lowered the sequence from %d to %d", top, seq)
	}
	next := ev("zz-c", "1")
	if _, err := st.Put(ctx, []Event{next}); err != nil {
		t.Fatal(err)
	}
	if got := seqOf(next.ID()); got <= top {
		t.Fatalf("a write after the rebuild took %d, at or below the %d a watch read", got, top)
	}
	top, _ = st.Seq(ctx)

	// a build from before the sequence: a row it inserts, and a row it rewrites, without one
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := ev("zz-old", "1")
	if _, err := db.ExecContext(ctx, `INSERT INTO events (id, source, source_key, ts, actor, kind, summary) VALUES (?,?,?,?,?,?,?)`,
		old.ID(), old.Source, old.SourceKey, old.TS.UnixMilli(), string(old.Actor), old.Kind, old.Summary); err != nil {
		t.Fatal(err)
	}
	if got := seqOf(old.ID()); got <= top {
		t.Fatalf("a row an older build inserted took %d, at or below %d", got, top)
	}
	top, _ = st.Seq(ctx)
	if _, err := db.ExecContext(ctx, `UPDATE events SET summary = ? WHERE id = ?`, fmt.Sprint("failed ", 1), next.ID()); err != nil {
		t.Fatal(err)
	}
	if got := seqOf(next.ID()); got <= top {
		t.Fatalf("a row an older build rewrote kept %d, at or below %d", got, top)
	}
	if seq, _ := st.Seq(ctx); seq != seqOf(next.ID()) {
		t.Fatalf("the sequence %d is not the last write's %d", seq, seqOf(next.ID()))
	}
}

// The write sequence's counter row comes back if it is gone, above the events' highest: a write after it is deleted
// still goes through, and above every number before.
func TestTheWriteSequenceSurvivesItsCounterRowGoing(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	if _, err := st.Put(ctx, []Event{{Source: "zz", SourceKey: "1", TS: at, Actor: ActorTool, Kind: "tool_result", Summary: "ok"}}); err != nil {
		t.Fatal(err)
	}
	top, _ := st.Seq(ctx)
	if _, err := st.db.ExecContext(ctx, `DELETE FROM event_seq`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, []Event{{Source: "zz", SourceKey: "2", TS: at, Actor: ActorTool, Kind: "tool_result", Summary: "ok"}}); err != nil {
		t.Fatalf("a write after the counter's row went: %v", err)
	}
	if seq, _ := st.Seq(ctx); seq <= top {
		t.Fatalf("the sequence after the counter's row went: %d, at or below %d", seq, top)
	}
}

// A guarded measurement never closes as improved: the store refuses it.
func TestAGuardedMeasurementNeverClosesAsImproved(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	e := Event{Source: "zz", SourceKey: "1", TS: at, Actor: ActorTool, Kind: "tool_result", Summary: "failed"}
	if _, err := st.Put(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "failures/v2:zz", Title: "it fails", Evidence: []Evidence{{Ref: "ev:" + e.ID()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "m", "n"
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeployCase(ctx, c.ID, t.TempDir(), at, "landed", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartGuarding(ctx, c.ID, 30*24*time.Hour, 4, 684, at, "a guard", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseMeasured(ctx, c.ID, 0, "a drop", "test"); err == nil {
		t.Fatal("a guarded measurement closed as improved")
	}
}
