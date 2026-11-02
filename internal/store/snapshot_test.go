package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A store as of a moment holds the source's events from since up to the moment and its notes from before it (a
// note done after the moment stands again), and nothing else of it: no case, no run, no source's state.
func TestAStoreAsOfAMomentHoldsOnlyWhatCameBefore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	st, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	cut := time.Now().Add(-2 * time.Hour)
	var evs []Event
	for i, at := range []time.Time{cut.Add(-48 * time.Hour), cut.Add(-time.Hour), cut.Add(-time.Minute), cut.Add(time.Minute)} {
		evs = append(evs, Event{Source: Transcripts, SourceKey: "s@" + at.String(), TS: at, Session: "s", Actor: ActorAgent, Kind: "agent_text",
			Summary: string(rune('a' + i))})
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	n, err := st.PutNote(ctx, Note{Kind: NoteNeeds, Title: "before", At: cut.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.DoneNote(ctx, n.ID, cut.Add(time.Hour)); err != nil { // done after the moment: it stood then
		t.Fatal(err)
	}
	if _, err := st.PutNote(ctx, Note{Kind: NoteNeeds, Title: "after", At: cut.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "s", Title: "a case", Evidence: []Evidence{{Ref: "file:/t/x", At: cut.Add(-time.Hour)}}}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFile(ctx, Transcripts, "/t/x.jsonl", 1, 2); err != nil {
		t.Fatal(err)
	}
	st.Close()
	dst := filepath.Join(dir, "as-of.db")
	if err := AsOf(ctx, src, dst, cut.Add(-24*time.Hour), cut); err != nil {
		t.Fatal(err)
	}
	snap, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	got, err := snap.Events(ctx, Query{Since: time.Unix(0, 0), Limit: 100})
	if err != nil || len(got) != 2 {
		t.Fatalf("events %d %v", len(got), err)
	}
	notes, _ := snap.Notes(ctx)
	if len(notes) != 1 || notes[0].Title != "before" || !notes[0].Done.IsZero() {
		t.Fatalf("notes %+v", notes)
	}
	if cs, _ := snap.Cases(ctx, CaseFilter{All: true}); len(cs) != 0 {
		t.Fatalf("cases %d", len(cs))
	}
	if _, _, ok, _ := snap.FileSeen(ctx, Transcripts, "/t/x.jsonl"); ok {
		t.Fatal("a source's state went into the snapshot")
	}
}

// A snapshot keeps immutable incident sightings, never the incident row rewritten with its later resolution.
func TestASnapshotLeavesOutRewrittenIncidents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	st, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	since := end.Add(-CaseRetention)
	var events []Event
	for i, at := range []time.Time{since.Add(-time.Millisecond), since, end.Add(-time.Millisecond), end, end.Add(time.Second)} {
		events = append(events, Event{Source: Transcripts, SourceKey: at.String(), TS: at, Actor: ActorTool, Kind: "tool_result", Summary: string(rune('a' + i))})
	}
	events = append(events, Event{Source: "incidents", SourceKey: "incident", Actor: ActorDetector, TS: end.Add(-time.Hour), Kind: "incident", Summary: "resolved: the later answer"},
		Event{Source: "incidents", SourceKey: "seen", Actor: ActorDetector, TS: end.Add(-time.Hour), Kind: "incident-seen", Summary: "the symptom"})
	if _, err = st.Put(ctx, events); err != nil {
		t.Fatal(err)
	}
	st.Close()
	dst := filepath.Join(dir, "snapshot.db")
	if err = AsOf(ctx, src, dst, since, end); err != nil {
		t.Fatal(err)
	}
	snap, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	got, err := snap.Events(ctx, Query{Since: time.Unix(0, 0), Limit: 100})
	if err != nil || len(got) != 3 {
		t.Fatalf("events %+v %v", got, err)
	}
	for _, ev := range got {
		if ev.Kind == "incident" || ev.TS.Before(since) || !ev.TS.Before(end) {
			t.Fatalf("leaked %+v", ev)
		}
	}
}

// A scout title rewritten after the cutoff cannot appear at the finding's original timestamp.
func TestASnapshotLeavesOutRewrittenScoutFindings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	st, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	ev := Event{Source: "radar", SourceKey: "finding:1", TS: at, Actor: ActorDetector, Kind: "finding", Summary: "the job fails"}
	if _, err = st.Put(ctx, []Event{ev}); err != nil {
		t.Fatal(err)
	}
	ev.Summary = "the job fails: fixed by taking the lock first"
	if _, err = st.Put(ctx, []Event{ev}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	dst := filepath.Join(dir, "snapshot.db")
	if err = AsOf(ctx, src, dst, at.Add(-time.Hour), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	snap, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	got, err := snap.Events(ctx, Query{Since: at.Add(-time.Hour), Limit: 10})
	if err != nil || len(got) != 0 {
		t.Fatalf("rewritten finding: %+v %v", got, err)
	}
}
