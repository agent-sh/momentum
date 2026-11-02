package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Activity and events exclude the loop's directory and every descendant, not a sibling with the same prefix.
// Prefix characters are literal. Both windows include their start and exclude their end.
func TestActivityAndEventsRespectDirectoryAndTimeBounds(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	evs := []Event{
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "start", Session: "start", TS: start, Cwd: "/work/project"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "end", Session: "end", TS: end, Cwd: "/work/project"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "loop", Session: "loop", TS: start, Cwd: "/work/loop_%"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "child", Session: "child", TS: start, Cwd: "/work/loop_%/runs/a"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "sibling", Session: "sibling", TS: start, Cwd: "/work/loop_%2"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "unknown", Session: "unknown", TS: start},
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	base, err := st.Activity(ctx, ActivityQuery{Since: start, Until: end, BySession: true, ExcludeCwd: "/work/loop_%/"})
	if err != nil || len(base) != 3 {
		t.Fatalf("activity: %+v %v", base, err)
	}
	for _, e := range evs {
		want := e.SourceKey == "start" || e.SourceKey == "sibling" || e.SourceKey == "unknown"
		if base[UnitOf(e, true)] != want {
			t.Errorf("activity for %s: %v", e.SourceKey, base)
		}
	}
	got, err := st.Events(ctx, Query{Since: start, Until: end, ExcludeCwd: "/work/loop_%"})
	if err != nil || len(got) != 3 {
		t.Fatalf("events: %+v %v", got, err)
	}
	for _, e := range got {
		if e.SourceKey != "start" && e.SourceKey != "sibling" && e.SourceKey != "unknown" {
			t.Errorf("excluded event: %+v", e)
		}
	}
}

// A loop entered through an alias records its resolved working directory. Both forms and their descendants
// are excluded from hits, activity and source checks, while a sibling is still counted.
func TestTheLoopDirectoryIsExcludedThroughItsSymlink(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	real := filepath.Join(t.TempDir(), "loop_%")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	var evs []Event
	for i, cwd := range []string{real, filepath.Join(real, "runs", "one"), alias, filepath.Join(alias, "runs", "two"), real + "-sibling"} {
		evs = append(evs, Event{Source: Transcripts, SourceKey: fmt.Sprint(i), Session: fmt.Sprint(i), Actor: ActorTool, Kind: "tool_result", TS: start, Cwd: cwd})
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{Since: start, Until: end, BySession: true, ExcludeCwd: alias}
	base, err := st.Activity(ctx, q)
	if err != nil || len(base) != 1 || !base[UnitOf(evs[4], true)] {
		t.Fatalf("activity: %+v %v", base, err)
	}
	got, err := st.Events(ctx, Query{Since: start, Until: end, ExcludeCwd: alias})
	if err != nil || len(got) != 1 || got[0].Cwd != real+"-sibling" {
		t.Fatalf("events: %+v %v", got, err)
	}
}

// Readiness uses successful run starts, not event times or write times. An equal boundary is not past it,
// a quiet successful read is sufficient, and a different source's activity has its own requirement.
func TestWindowReadinessUsesEverySourcesSuccessfulRead(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ev := Event{Source: Sessions, SourceKey: "phase", Session: "one", Actor: ActorHarness, Kind: "phase", TS: start}
	if _, err := st.Put(ctx, []Event{ev}); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{Since: start, Until: end, BySession: true}
	check := func(want string) {
		t.Helper()
		got, err := st.UnreadSources(ctx, Transcripts, q)
		if err != nil || strings.Join(got, ",") != want {
			t.Fatalf("unread: %v %v, want %s", got, err, want)
		}
	}
	check("sessions,transcripts")
	if err := st.MarkRead(ctx, Transcripts, end); err != nil {
		t.Fatal(err)
	}
	check("sessions,transcripts")
	if err := st.MarkRead(ctx, Transcripts, end.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	check("sessions")
	if err := st.SetCursor(ctx, Sessions, "complete"); err != nil {
		t.Fatal(err)
	}
	check("sessions")
	if err := st.MarkRead(ctx, Sessions, end.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	check("")
	if err := st.MarkRead(ctx, Transcripts, start); err != nil {
		t.Fatal(err)
	}
	check("")
	q.Actor = ActorOwner
	if _, err := st.db.ExecContext(ctx, `DELETE FROM cursors WHERE source=?`, Sessions); err != nil {
		t.Fatal(err)
	}
	check("") // no sessions-source event is in the owner's activity
}

// Rebuilding drops complete and per-run read records atomically with the source's events and cursors.
func TestRebuildDropsAllSourceReadRecords(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.MarkRead(ctx, "mistakes", now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPartialRead(ctx, "mistakes", now, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, "mistakes-file", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Rebuild(ctx, "mistakes", false); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"mistakes", "mistakes-file"} {
		at, err := st.ReadWindowThrough(ctx, source, now.Add(-24*time.Hour))
		if err != nil || !at.IsZero() {
			t.Fatalf("read record survived for %s: %v %v", source, at, err)
		}
	}
}
