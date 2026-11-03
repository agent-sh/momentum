package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func inputStore(t *testing.T, events []store.Event) string {
	t.Helper()
	for i := range events {
		if events[i].Actor == "" {
			events[i].Actor = store.ActorUnknown
		}
	}
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Put(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func inputPlainDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteInputURI(path, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestInputsStoreCopyScrubsWindowAndPlainReadOnlyFile(t *testing.T) {
	goodInputScrubber(t)
	ctx := context.Background()
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	start := end.Add(-StoreWindow)
	src := inputStore(t, []store.Event{
		{Source: "fixture", SourceKey: "old", TS: start.Add(-time.Millisecond), Kind: "record"},
		{Source: "fixture", SourceKey: "inside", TS: start, Kind: "record", Summary: "marker-7f3a", Meta: `{"text":"marker-7f3a"}`, Ref: "marker-7f3a"},
		{Source: "fixture", SourceKey: "end", TS: end, Kind: "record"},
	})
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutNote(ctx, store.Note{Kind: store.NoteNeeds, Title: "marker-7f3a", Detail: "marker-7f3a", At: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	dir := t.TempDir()
	dst := filepath.Join(dir, "store.sqlite")
	m, err := StoreCopy(ctx, StoreCopyOptions{Source: src, Destination: dst, Until: end})
	if err != nil {
		t.Fatal(err)
	}
	if m.Events != 1 || m.Notes != 1 || !m.Canary || !m.Since.Equal(start) || !m.Until.Equal(end) {
		t.Fatalf("manifest %+v", m)
	}
	db := inputPlainDB(t, dst)
	var summary, meta, ref, title, detail, journal string
	if err = db.QueryRow("SELECT summary,meta,ref FROM events").Scan(&summary, &meta, &ref); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT title,detail FROM notes").Scan(&title, &detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary+meta+ref+title+detail, "marker-7f3a") || summary != "[masked]" || title != "[masked]" || !json.Valid([]byte(meta)) {
		t.Fatal("private marker or invalid JSON in copy")
	}
	if err = db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "delete" {
		t.Fatalf("copy journal %s: %v", journal, err)
	}
	var extra int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('events','notes')`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("non-input tables %d: %v", extra, err)
	}
	if _, err = db.Exec("UPDATE events SET summary='changed'"); err == nil {
		t.Fatal("read-only connection wrote")
	}
	info, err := os.Stat(dst)
	if err != nil || info.Mode().Perm() != 0o444 || info.Size() != m.Bytes {
		t.Fatalf("copy mode/bytes: %v", err)
	}
	header, err := os.ReadFile(dst)
	if err != nil || len(header) < 20 || header[18] != 1 || header[19] != 1 {
		t.Fatal("copy header still requires WAL mode")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("scratch or sidecar left: %d %v", len(entries), err)
	}
}

func TestInputsStoreCopyRepairsScrubbedJSON(t *testing.T) {
	inputScrubber(t, `sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/"/g'`)
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	src := inputStore(t, []store.Event{{Source: "fixture", SourceKey: "one", TS: end.Add(-time.Hour), Kind: "record", Meta: `{"value":"marker-7f3a"}`}})
	dst := filepath.Join(t.TempDir(), "copy.db")
	if _, err := StoreCopy(context.Background(), StoreCopyOptions{Source: src, Destination: dst, Until: end}); err != nil {
		t.Fatal(err)
	}
	var meta string
	if err := inputPlainDB(t, dst).QueryRow("SELECT meta FROM events").Scan(&meta); err != nil || meta != "{}" {
		t.Fatalf("invalid JSON retained: %q %v", meta, err)
	}
}

func TestInputsStoreCopyScrubberFailurePublishesNothing(t *testing.T) {
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	src := inputStore(t, []store.Event{{Source: "fixture", SourceKey: "one", TS: end.Add(-time.Hour), Kind: "record"}})
	inputScrubber(t, `sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g' | tr -d '\036'`)
	dir := t.TempDir()
	if _, err := StoreCopy(context.Background(), StoreCopyOptions{Source: src, Destination: filepath.Join(dir, "copy.db"), Until: end}); err == nil {
		t.Fatal("bad batch published")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed copy left output: %d %v", len(entries), err)
	}
}

func TestInputsStoreCopyInPlaceOption(t *testing.T) {
	goodInputScrubber(t)
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	src := inputStore(t, []store.Event{
		{Source: "incidents", SourceKey: "one", TS: end.Add(-time.Hour), Kind: "incident"},
		{Source: "radar", SourceKey: "two", TS: end.Add(-time.Hour), Kind: "finding"},
	})
	for _, omit := range []bool{false, true} {
		dst := filepath.Join(t.TempDir(), "copy.db")
		m, err := StoreCopy(context.Background(), StoreCopyOptions{Source: src, Destination: dst, Until: end, OmitInPlace: omit})
		if err != nil || omit && m.Events != 0 || !omit && m.Events != 2 {
			t.Fatalf("in-place option %v: %d %v", omit, m.Events, err)
		}
	}
}

func TestStoreCopyNotesUseTheSameThirtyDayWindow(t *testing.T) {
	goodInputScrubber(t)
	ctx := context.Background()
	until := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	source := inputStore(t, nil)
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Note{{ID: "old", Kind: store.NoteDecided, Title: "old", At: until.Add(-StoreWindow - time.Second)},
		{ID: "inside", Kind: store.NoteDecided, Title: "inside", At: until.Add(-StoreWindow)},
		{ID: "after", Kind: store.NoteDecided, Title: "after", At: until}} {
		if _, err := st.PutNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	target := filepath.Join(t.TempDir(), "store.sqlite")
	manifest, err := StoreCopy(ctx, StoreCopyOptions{Source: source, Destination: target, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	db := inputPlainDB(t, target)
	if err := db.QueryRowContext(ctx, "SELECT id FROM notes").Scan(&id); err != nil || id != "inside" || manifest.Notes != 1 {
		t.Fatalf("notes window: %s count %d %v", id, manifest.Notes, err)
	}
}

// A scrubber start costs most of a second: a store of a thousand events is copied in a few starts, and a note whose
// id the scrubber would change is left out of the copy.
func TestStoreCopyScrubsInFewStartsAndScrubsNoteIds(t *testing.T) {
	ctx := context.Background()
	counter := filepath.Join(t.TempDir(), "starts")
	inputScrubber(t, "echo x >> '"+counter+"'\nsed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'")
	until := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	var events []store.Event
	for i := 0; i < 1000; i++ {
		events = append(events, store.Event{Source: "fixture", SourceKey: "k" + strconv.Itoa(i), TS: until.Add(-time.Hour), Kind: "record", Summary: "neutral summary " + strconv.Itoa(i)})
	}
	source := inputStore(t, events)
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Note{{ID: "plain-id", Kind: store.NoteDecided, Title: "kept", At: until.Add(-time.Hour)},
		{ID: "id-marker-7f3a", Kind: store.NoteDecided, Title: "left out", At: until.Add(-time.Hour)}} {
		if _, err := st.PutNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	target := filepath.Join(t.TempDir(), "store.sqlite")
	if _, err := StoreCopy(ctx, StoreCopyOptions{Source: source, Destination: target, Until: until}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(counter)
	if starts := strings.Count(string(b), "x"); starts > 8 {
		t.Fatalf("%d scrubber starts for 1000 events", starts)
	}
	var notes int
	if err := inputPlainDB(t, target).QueryRow("SELECT count(*) FROM notes").Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("notes in the copy: %d %v", notes, err)
	}
}
