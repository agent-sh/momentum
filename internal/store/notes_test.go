package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

// someNotes stores one note of each kind, the decided one done, and returns them as a listing.
func someNotes(t *testing.T, st *Store) string {
	t.Helper()
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_000_000)
	for _, n := range []Note{
		{ID: "ask-1", Kind: NoteNeeds, Title: "Merge the parser fix?", Importance: ImportanceHigh, At: at},
		{ID: "dec-1", Kind: NoteDecided, Title: "Kept the old cache", Detail: "Say revert to drop it.", At: at.Add(time.Minute)},
		{ID: "shp-1", Kind: NoteShipped, Title: "Resumed scans", Effect: "scan 2.1 s -> 0.2 s (7 d)",
			Links: []Link{{Label: "PR #3", URL: "https://example.com/pr/3"}}, At: at.Add(2 * time.Minute)},
		{ID: "prb-1", Kind: NoteProblem, Title: "A test session keeps failing", Source: "supervise", Importance: ImportanceLow, At: at.Add(3 * time.Minute)},
	} {
		if _, err := st.PutNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.DoneNote(ctx, "dec-1", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return listNotes(t, st)
}

// listNotes is every note, field by field, newest first.
func listNotes(t *testing.T, st *Store) string {
	t.Helper()
	ns, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		out = append(out, fmt.Sprintf("%s %s %q %q %q %v %s %d %d %d", n.ID, n.Kind, n.Title, n.Detail, n.Effect, n.Links, n.Source,
			n.Importance, n.At.UnixMilli(), n.Done.UnixMilli()))
	}
	return strings.Join(out, "\n")
}

// The notes are kept: nothing derives them, so no schema change drops them. Not a step that reads every source
// again, nor the visit of a build from before version 3, nor a store from an older schema.
func TestNotesSurviveEverySchemaChange(t *testing.T) {
	st, path := open(t)
	want := someNotes(t, st)
	if strings.Count(want, "\n") != 3 || !strings.HasPrefix(want, "prb-1 ") || !strings.Contains(want, "\nask-1 ") {
		t.Fatalf("setup, newest first: %s", want)
	}
	st.Close()
	reopen := func(name, visit string) {
		t.Helper()
		if visit != "" {
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(visit); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if got := listNotes(t, st); got != want {
			t.Fatalf("%s changed the notes:\n%s\nwant\n%s", name, got, want)
		}
	}
	// A build from before version 3 drops every event but the sessions' and sets user_version 2: the step that
	// reads again runs again, and drops the derived tables.
	reopen("a build from before version 3", `DELETE FROM events WHERE source != 'sessions'; PRAGMA user_version = 2`)
	reopen("a store from an older schema", `PRAGMA user_version = 1; DROP TABLE schema_steps`)
	// The next step that reads again.
	was := steps
	t.Cleanup(func() { steps = was })
	steps = append(slices.Clone(was), step{version: latest() + 1, reread: true})
	reopen("the next step that reads again", "")
}

// A store from before the notes gains their table at its next open, and keeps the rest as it was: its events, its
// stamps and its cursors (nothing is read again). It takes no step, so a build from before the notes, which never
// reads them, keeps working on it.
func TestAStoreFromBeforeTheNotesGainsThemAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	at := time.UnixMilli(1_700_000_000_000)
	if _, _, err := st.PutFiles(ctx, []FileWrite{{Events: []Event{{Source: Transcripts, SourceKey: "a@000000000000#000", TS: at, Actor: ActorAgent, Kind: "agent_text"}},
		Cursor: FileCursor{Source: Transcripts, Path: "/s/a.jsonl", Offset: 10}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessionStamps(ctx, state.Stamps{"s": {Phase: sessions.Idle, Since: at, Seen: at}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE notes`); err != nil { // the store as the build before the notes left it
		t.Fatal(err)
	}
	var stepsBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_steps`).Scan(&stepsBefore); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.PutNote(ctx, Note{Kind: NoteNeeds, Title: "Tag the release?"}); err != nil {
		t.Fatalf("a note in a store from before the notes: %v", err)
	}
	if evs, err := st.Events(ctx, Query{Since: time.UnixMilli(0)}); err != nil || len(evs) != 1 {
		t.Fatalf("events: %d %v", len(evs), err)
	}
	if cs, _ := st.FileCursors(ctx, Transcripts); len(cs) != 1 {
		t.Fatalf("the cursors were dropped: %+v", cs)
	}
	if got, err := st.SessionStamps(ctx); err != nil || len(got) != 1 {
		t.Fatalf("stamps: %+v %v", got, err)
	}
	// The notes take no step, so a build from before them, which refuses a store at a step it does not know, keeps
	// working on this store.
	var uv, stepsAfter int
	rawQuery(t, path, `PRAGMA user_version`, &uv)
	rawQuery(t, path, `SELECT COUNT(*) FROM schema_steps`, &stepsAfter)
	if uv != legacyVersion || stepsAfter != stepsBefore {
		t.Fatalf("user_version %d, %d steps before the open and %d after: the notes take no step", uv, stepsBefore, stepsAfter)
	}
	if ok, err := current(st.db); err != nil || !ok {
		t.Fatalf("the store is not current: %v", err)
	}
}

// evidence rebuild never touches the notes, whatever source it drops, a source a note names among them.
func TestRebuildNeverTouchesTheNotes(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	want := someNotes(t, st)
	if _, err := st.Put(ctx, []Event{{Source: "mistakes", SourceKey: "k", TS: time.Now(), Actor: ActorTool, Kind: "tool-error"}}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"momentum", "supervise", "notes", "mistakes", Transcripts, Sessions, ""} {
		if _, err := st.Rebuild(ctx, src, true); err != nil && !errors.Is(err, ErrObserved) {
			t.Fatalf("rebuild %q: %v", src, err)
		}
	}
	if got := listNotes(t, st); got != want {
		t.Fatalf("a rebuild changed the notes:\n%s\nwant\n%s", got, want)
	}
}

// An id names one item: a note added again with it replaces it and stands again, also after it was done. done
// takes a note out of the report once; a note done already keeps its time. A note without an id gets a new one.
func TestANoteAddedAgainReplacesItAndStandsAgain(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if _, err := st.PutNote(ctx, Note{ID: "q", Kind: NoteNeeds, Title: "Rent a GPU for the eval?", At: t0}); err != nil {
		t.Fatal(err)
	}
	n, stood, err := st.DoneNote(ctx, "q", t0.Add(time.Minute))
	if err != nil || !stood || !n.Done.Equal(t0.Add(time.Minute)) || n.Standing(t0) {
		t.Fatalf("done: %+v %v %v", n, stood, err)
	}
	if n, stood, err = st.DoneNote(ctx, "q", t0.Add(2*time.Minute)); err != nil || stood || !n.Done.Equal(t0.Add(time.Minute)) {
		t.Fatalf("done again: %+v %v %v", n, stood, err)
	}
	if _, err := st.PutNote(ctx, Note{ID: "q", Kind: NoteDecided, Title: "Rented no GPU", Detail: "Say rent to rent one.", At: t0.Add(3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	ns, err := st.Notes(ctx)
	if err != nil || len(ns) != 1 {
		t.Fatalf("notes: %+v %v", ns, err)
	}
	if n := ns[0]; n.Kind != NoteDecided || n.Title != "Rented no GPU" || !n.Done.IsZero() || !n.Standing(t0) || !n.At.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("added again: %+v", n)
	}
	if _, _, err := st.DoneNote(ctx, "nope", time.Now()); !errors.Is(err, ErrNoNote) {
		t.Fatalf("done of an unknown id: %v", err)
	}
	a, err := st.PutNote(ctx, Note{Kind: NoteProblem, Title: "Disk fills"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.PutNote(ctx, Note{Kind: NoteProblem, Title: "Disk fills"})
	if err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`^n[0-9a-f]{8}$`)
	if !id.MatchString(a.ID) || !id.MatchString(b.ID) || a.ID == b.ID || a.Source != DefaultNoteSource || time.Since(a.At) > time.Minute {
		t.Fatalf("new ids: %+v %+v", a, b)
	}
	// supervise's note for a session it gave up on: one per session.
	for range 2 {
		if _, err := st.PutNote(ctx, SessionProblem("sess-a", "Gave up on fix the parser", "It stops on the same error.", t0)); err != nil {
			t.Fatal(err)
		}
	}
	if ns, _ := st.Notes(ctx); len(ns) != 4 {
		t.Fatalf("%d notes, want 4: %+v", len(ns), ns)
	}
}

// A note is stored as plain text on one line each, with a title of at most TitleMax characters; a wrong kind,
// importance, id or link is refused.
func TestANoteIsPlainTextAndAWrongOneIsRefused(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	n, err := st.PutNote(ctx, Note{Kind: NoteShipped, Title: strings.Repeat("long ", 30) + "\nend", Detail: "two\nlines \x1b[31mred\x07",
		Effect: "a\u202e\u200bb\x00", Source: "sup\x1bervise", Links: []Link{{Label: " PR\t#4\u2066 ", URL: " https://example.com/pr/4?a=b "}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := []rune(n.Title); len(r) != TitleMax || r[len(r)-1] != '…' || strings.Contains(n.Title, "\n") || n.Detail != "two lines [31mred" ||
		n.Effect != "ab" || n.Source != "supervise" || n.Links[0] != (Link{Label: "PR #4", URL: "https://example.com/pr/4?a=b"}) {
		t.Fatalf("stored: %+v", n)
	}
	for _, bad := range []Note{
		{Kind: "fyi", Title: "x"},
		{Kind: NoteNeeds, Title: " \n "},
		{Kind: NoteNeeds, Title: "x", Importance: 2},
		{Kind: NoteNeeds, Title: "x", ID: "two words"},
		{Kind: NoteNeeds, Title: "x", ID: "rlo\u202eid"},
		{Kind: NoteNeeds, Title: "x", ID: "-x"},
		{Kind: NoteNeeds, Title: "x", ID: "session:w"},
		{Kind: NoteNeeds, Title: "x", ID: "task:w/bg1"},
		{Kind: NoteNeeds, Title: "\x1b\x07\u202e"},
		{Kind: NoteNeeds, Title: "x", Links: []Link{{Label: "pr", URL: "https://example.com/\u202epr"}}},
		{Kind: NoteNeeds, Title: "x", Links: []Link{{Label: "", URL: "https://example.com"}}},
		{Kind: NoteNeeds, Title: "x", Links: []Link{{Label: "run", URL: "javascript:alert(1)"}}},
		{Kind: NoteNeeds, Title: "x", Links: []Link{{Label: "file", URL: "file:///etc/passwd"}}},
		{Kind: NoteNeeds, Title: "x", Links: []Link{{Label: "rel", URL: "/pr/4"}}},
	} {
		if _, err := st.PutNote(ctx, bad); !errors.Is(err, ErrBadNote) {
			t.Errorf("%+v: %v, want ErrBadNote", bad, err)
		}
	}
	if ns, _ := st.Notes(ctx); len(ns) != 1 {
		t.Fatalf("a refused note was stored: %d notes", len(ns))
	}
}

// A needs or problem note stands until done; a decided or shipped one from the window on.
func TestWhichNotesStand(t *testing.T) {
	since := time.UnixMilli(1_700_000_000_000)
	old, recent := since.Add(-time.Hour), since.Add(time.Hour)
	for _, c := range []struct {
		n    Note
		want bool
	}{
		{Note{Kind: NoteNeeds, At: old}, true},
		{Note{Kind: NoteProblem, At: old}, true},
		{Note{Kind: NoteDecided, At: old}, false},
		{Note{Kind: NoteShipped, At: old}, false},
		{Note{Kind: NoteDecided, At: since}, true},
		{Note{Kind: NoteShipped, At: recent}, true},
		{Note{Kind: NoteNeeds, At: recent, Done: recent}, false},
		{Note{Kind: NoteShipped, At: recent, Done: recent}, false},
	} {
		if got := c.n.Standing(since); got != c.want {
			t.Errorf("%s at %v done %v: standing %v, want %v", c.n.Kind, c.n.At.Sub(since), !c.n.Done.IsZero(), got, c.want)
		}
	}
}

// A store opened to read creates, migrates and writes nothing: a missing store stays missing, a store that is not
// this build's schema is refused as it is, and a current one reads while a sessions run holds the write lock.
func TestAReadOnlyOpenWritesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "none", "m.db")
	if _, err := OpenReadOnly(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing store: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the open made its directory: %v", err)
	}

	st, path := open(t)
	if _, err := st.PutNote(ctx, Note{ID: "q", Kind: NoteNeeds, Title: "Tag it?"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Its connection cannot write: SQLite refuses the write itself, with no lock held anywhere.
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.PutNote(ctx, Note{Kind: NoteNeeds, Title: "x"}); err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("a write on a read-only store: %v, want SQLite's readonly error", err)
	}
	ro.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	writer, err := db.Begin() // a sessions run holds the write lock
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if ro, err = OpenReadOnly(path); err != nil {
		t.Fatal(err)
	}
	ns, err := ro.Notes(ctx)
	if err != nil || len(ns) != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("read under a writer: %+v %v after %v", ns, err, time.Since(start))
	}
	ro.Close()
	writer.Rollback()

	if _, err := db.Exec(`DROP TABLE notes`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) || !strings.Contains(err.Error(), "migrates it") {
		t.Fatalf("a store from before the notes: %v", err)
	}
	var n int
	rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'notes'`, &n)
	if n != 0 {
		t.Fatal("a read-only open migrated the store")
	}
	if _, err := db.Exec(`INSERT INTO schema_steps VALUES (1000, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a store from a newer build: %v", err)
	}
}

// supervise can add its note inside a sessions run, in the run's transaction: the run holds the write lock.
func TestANoteGoesInASessionsRun(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	done := make(chan error, 1)
	go func() {
		done <- st.SessionRun(ctx, func(r *Run) error {
			_, err := r.PutNote(SessionProblem("sess-a", "Gave up on fix the parser", "", time.Now()))
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a note inside a sessions run waited on the run's own lock")
	}
	if ns, err := st.Notes(ctx); err != nil || len(ns) != 1 || ns[0].ID != "supervise:sess-a" {
		t.Fatalf("notes: %+v %v", ns, err)
	}
}

// A report scans the harnesses at once, so their sources ask one Trackers for checkpoints at once.
func TestTrackersHandOutCheckpointsAtOnce(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	tr := st.Trackers("b1")
	var logs []sessions.Log
	for i := range 4 {
		var l sessions.Log
		l.Add(sessions.Entry{Role: sessions.RoleTool, Name: "Bash", Text: fmt.Sprint("step ", i)})
		logs = append(logs, l)
	}
	for i, tool := range []string{"claude", "codex", "pi", "hermes"} {
		saveRun(t, st, tr, time.UnixMilli(1_700_000_000_000), sessions.Checkpoint{Tool: tool, Key: "/t/" + tool, State: []byte(`{}`), Log: &logs[i]})
	}
	tr = st.Trackers("b1") // the report's, for one look
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, tool := range []string{"claude", "codex", "pi", "hermes"} {
		wg.Go(func() {
			if cp, err := tr.Checkpoint(ctx, tool, "/t/"+tool); err != nil || cp == nil {
				errs <- fmt.Errorf("%s: %+v %v", tool, cp, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
