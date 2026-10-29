package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPutIsIdempotentAndCountsOnlyNewRows(t *testing.T) {
	ctx, st := context.Background(), open(t)
	e := Event{Source: "s", SourceKey: "k1", TS: time.Now(), Actor: ActorTool, Kind: "tool-error", Summary: "first"}
	if n, err := st.Put(ctx, []Event{e}); err != nil || n != 1 {
		t.Fatalf("first put: n=%d err=%v", n, err)
	}
	e.Summary = "refreshed"
	if n, err := st.Put(ctx, []Event{e, {Source: "s", SourceKey: "k2", TS: time.Now(), Actor: ActorOwner, Kind: "correction"}}); err != nil || n != 1 {
		t.Fatalf("second put: n=%d err=%v, want 1 new", n, err)
	}
	evs, err := st.Events(ctx, Query{Since: time.Now().Add(-time.Hour), Kinds: []string{"tool-error"}})
	if err != nil || len(evs) != 1 || evs[0].Summary != "refreshed" || evs[0].Actor != ActorTool {
		t.Fatalf("events = %+v, err %v", evs, err)
	}
}

func TestPutRefusesAnEventWithoutIdentity(t *testing.T) {
	if _, err := open(t).Put(context.Background(), []Event{{Source: "s", Kind: "x", Actor: ActorTool}}); err == nil {
		t.Fatal("an event without a source key was stored")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	ctx, st := context.Background(), open(t)
	if c, err := st.Cursor(ctx, "s"); err != nil || c != "" {
		t.Fatalf("empty cursor = %q, %v", c, err)
	}
	if err := st.SetCursor(ctx, "s", "12:345"); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Cursor(ctx, "s"); c != "12:345" {
		t.Fatalf("cursor = %q", c)
	}
}
