package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func undecidedCase(t *testing.T) (*Store, Case) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "undecided.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, _, err := st.ObserveCase(context.Background(), Finding{Family: "failures", Signature: "undecided", Title: "marker-7f3a",
		Evidence: []Evidence{{Ref: "file:/fixture/undecided", At: time.Now()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return st, c
}

// RecordUndecided closes only a case still open or investigating: a case a person moved to testing while the run ran
// keeps its state.
func TestRecordUndecidedLeavesACaseThatMovedOn(t *testing.T) {
	ctx := context.Background()
	st, c := undecidedCase(t)
	mech, next, status := "a cause", "a test", CaseTesting
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: status, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	other := "another cause"
	got, recorded, err := st.RecordUndecided(ctx, c.ID, c.Episode, CaseUpdate{Mechanism: &other}, "undecided", "loop:r")
	if err != nil || recorded || got.Status != CaseTesting {
		t.Fatalf("a testing case: recorded %v, %s %v", recorded, got.Status, err)
	}
	if now, _ := st.CaseByID(ctx, c.ID); now.Status != CaseTesting || now.Mechanism != mech {
		t.Fatalf("the case changed: %s %q", now.Status, now.Mechanism)
	}
}

// A close for an undecided run without a reason of its own says what it was: the case log never holds an empty why.
func TestRecordUndecidedNamesItsReason(t *testing.T) {
	ctx := context.Background()
	st, c := undecidedCase(t)
	if _, recorded, err := st.RecordUndecided(ctx, c.ID, c.Episode, CaseUpdate{}, " ", "loop:r"); err != nil || !recorded {
		t.Fatalf("not recorded: %v %v", recorded, err)
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range log {
		if e.Action == "closed" {
			if !strings.Contains(e.Detail, "named no mechanism and fix") {
				t.Fatalf("the close says %q", e.Detail)
			}
			return
		}
	}
	t.Fatal("no close in the case log")
}
