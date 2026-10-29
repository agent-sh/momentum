package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func TestPutCountsNewRowsAndRefreshesEveryDerivedColumn(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	old := time.Unix(1_700_000_000, 0)
	e := Event{Source: "s", SourceKey: "k1", TS: old, Actor: ActorTool, Kind: "tool-error", Summary: "first"}
	if n, err := st.Put(ctx, []Event{e}); err != nil || n != 1 {
		t.Fatalf("first put: n=%d err=%v", n, err)
	}
	// a recurrence moves the time, and a corrected adapter changes who produced it
	e.TS, e.Actor, e.Summary, e.Meta, e.Experiment, e.Heuristic = old.Add(time.Hour), ActorHarness, "refreshed", `{"cause":"x"}`, "trial-1", true
	if n, err := st.Put(ctx, []Event{e, {Source: "s", SourceKey: "k2", TS: old, Actor: ActorOwner, Kind: "correction"}}); err != nil || n != 1 {
		t.Fatalf("second put: n=%d err=%v, want 1 new", n, err)
	}
	evs, err := st.Events(ctx, Query{Since: old.Add(30 * time.Minute)})
	if err != nil || len(evs) != 1 {
		t.Fatalf("a refreshed time must move the row into a later window: %+v, %v", evs, err)
	}
	got := evs[0]
	if got.Actor != ActorHarness || got.Summary != "refreshed" || got.Meta != `{"cause":"x"}` || got.Experiment != "trial-1" || !got.Heuristic {
		t.Fatalf("derived columns not refreshed: %+v", got)
	}
}

func TestPutRefusesAnEventWithoutIdentity(t *testing.T) {
	st, _ := open(t)
	if _, err := st.Put(context.Background(), []Event{{Source: "s", Kind: "x", Actor: ActorTool}}); err == nil {
		t.Fatal("an event without a source key was stored")
	}
}

func TestOpenKeepsTheStorePrivateEvenWhenItAlreadyExisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "momentum")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Put(context.Background(), []Event{{Source: "s", SourceKey: "k", TS: time.Now(), Actor: ActorOwner, Kind: "x"}}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600, path + "-wal": 0o600} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode %o, want %o", p, got, want)
		}
	}
}

func TestConcurrentWritersWaitInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	_, path := open(t)
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for w := 0; w < 8; w++ {
		st, err := Open(path) // separate handles, like two runs of the tool
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		wg.Add(1)
		go func(w int, st *Store) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				e := Event{Source: "s", SourceKey: fmt.Sprintf("%d-%d", w, i), TS: time.Now(), Actor: ActorTool, Kind: "x"}
				if _, err := st.Put(ctx, []Event{e}); err != nil {
					errs <- err
				}
			}
		}(w, st)
	}
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if n == 0 {
			t.Log(err)
		}
		n++
	}
	if n > 0 {
		t.Fatalf("%d of 400 concurrent puts failed", n)
	}
}

func TestRebuildDropsOneSourceAndItsSubstreamsOnly(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	put := func(src string) {
		if _, err := st.Put(ctx, []Event{{Source: src, SourceKey: "k", TS: time.Now(), Actor: ActorDetector, Kind: "x"}}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetCursor(ctx, src, "9"); err != nil {
			t.Fatal(err)
		}
	}
	put("radar")
	put("radar-feedback")
	put("mistakes")
	if err := st.MarkFile(ctx, "radar", "/x", 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.Skip(ctx, "radar", "/y", "bad"); err != nil {
		t.Fatal(err)
	}
	if err := st.Rebuild(ctx, "radar"); err != nil {
		t.Fatal(err)
	}
	for src, want := range map[string]int{"radar": 0, "radar-feedback": 0, "mistakes": 1} {
		evs, _ := st.Events(ctx, Query{Since: time.Unix(0, 0), Source: src})
		if len(evs) != want {
			t.Fatalf("%s: %d events after rebuild, want %d", src, len(evs), want)
		}
	}
	if c, _ := st.Cursor(ctx, "radar-feedback"); c != "" {
		t.Fatalf("substream cursor kept: %q", c)
	}
	if _, _, ok, _ := st.FileSeen(ctx, "radar", "/x"); ok {
		t.Fatal("file record kept")
	}
	if n, _ := st.Skipped(ctx, "radar"); n != 0 {
		t.Fatal("skipped records kept")
	}
	if c, _ := st.Cursor(ctx, "mistakes"); c != "9" {
		t.Fatal("another source's cursor was dropped")
	}
}

func TestAStoreFromAnotherSchemaIsDroppedAndRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// the first schema: no meta or experiment columns, user_version 0
	if _, err := old.Exec(`CREATE TABLE events (id TEXT PRIMARY KEY, source TEXT, source_key TEXT, ts INTEGER, actor TEXT, kind TEXT);
	  INSERT INTO events VALUES ('x', 'mistakes', 'k', 1, 'owner', 'correction');
	  CREATE TABLE cursors (source TEXT PRIMARY KEY, position TEXT, updated INTEGER);
	  INSERT INTO cursors VALUES ('mistakes', '1:2', 3);`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if c, _ := st.Cursor(ctx, "mistakes"); c != "" {
		t.Fatalf("an old cursor survived the schema change: %q", c)
	}
	if _, err := st.Put(ctx, []Event{{Source: "s", SourceKey: "k", TS: time.Now(), Actor: ActorOwner, Kind: "x", Meta: `{"a":1}`}}); err != nil {
		t.Fatalf("the new schema is not in place: %v", err)
	}
}
