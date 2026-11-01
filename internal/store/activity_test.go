package store

import (
	"context"
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
