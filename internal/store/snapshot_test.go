package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestInputsSnapshotWindowInPlaceAndNoDerivedRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	st, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if st.Path() != src {
		t.Fatal("source filename lost")
	}
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	var events []Event
	for _, at := range []time.Time{start.Add(-time.Millisecond), start, end.Add(-time.Millisecond), end} {
		events = append(events, Event{Source: Transcripts, SourceKey: at.String(), TS: at, Actor: ActorTool, Kind: "tool_result", Summary: "record"})
	}
	events = append(events,
		Event{Source: "incidents", SourceKey: "mutable", TS: end.Add(-time.Hour), Kind: "incident", Actor: ActorDetector},
		Event{Source: "incidents", SourceKey: "seen", TS: end.Add(-time.Hour), Kind: "incident-seen", Actor: ActorDetector},
		Event{Source: "radar", SourceKey: "mutable", TS: end.Add(-time.Hour), Kind: "finding", Actor: ActorDetector})
	if _, err = st.Put(ctx, events); err != nil {
		t.Fatal(err)
	}
	note, err := st.PutNote(ctx, Note{Kind: NoteNeeds, Title: "before", At: start.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.DoneNote(ctx, note.ID, end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutNote(ctx, Note{Kind: NoteNeeds, Title: "after", At: end}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ObserveCase(ctx, Finding{Family: "failures", Signature: "synthetic", Title: "case", Evidence: []Evidence{{Ref: "file:/fixture", At: start}}}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err = st.MarkFile(ctx, Transcripts, "/fixture.jsonl", 1, 2); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "evaluation"}[omit], func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "copy.db")
			if err := AsOf(ctx, src, dst, start, end, omit); err != nil {
				t.Fatal(err)
			}
			copy, err := Open(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer copy.Close()
			got, err := copy.Events(ctx, Query{Since: time.Unix(0, 0)})
			want := 5
			if omit {
				want = 3
			}
			if err != nil || len(got) != want {
				t.Fatalf("events %d, want %d: %v", len(got), want, err)
			}
			for _, event := range got {
				if event.TS.Before(start) || !event.TS.Before(end) || omit && (event.Kind == "incident" || event.Kind == "finding") {
					t.Fatalf("event outside snapshot: %s %s", event.Kind, event.TS)
				}
			}
			notes, err := copy.Notes(ctx)
			if err != nil || len(notes) != 1 || notes[0].Title != "before" || !notes[0].Done.IsZero() {
				t.Fatalf("notes %v: %v", notes, err)
			}
			cases, err := copy.Cases(ctx, CaseFilter{All: true})
			if err != nil || len(cases) != 0 {
				t.Fatalf("cases copied: %d %v", len(cases), err)
			}
			if _, _, ok, err := copy.FileSeen(ctx, Transcripts, "/fixture.jsonl"); err != nil || ok {
				t.Fatal("derived rows copied")
			}
			if err := AsOf(ctx, src, dst, start, end, omit); err == nil {
				t.Fatal("existing snapshot overwritten")
			}
		})
	}
}
