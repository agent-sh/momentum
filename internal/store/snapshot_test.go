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
