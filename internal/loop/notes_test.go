package loop

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// refuseNotes makes the store refuse every new note, as a disk that fails at the wrong moment would, until allow is called.
func refuseNotes(t *testing.T, path string) (allow func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER refuse_notes BEFORE INSERT ON notes BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := db.Exec(`DROP TRIGGER refuse_notes`); err != nil {
			t.Fatal(err)
		}
	}
}

// notesOf are the notes of a kind in the report, done or not.
func notesOf(t *testing.T, st *store.Store, kind store.NoteKind) []store.Note {
	t.Helper()
	all, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Note
	for _, n := range all {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// The note of a decision is written before the decision is made, under an id of its own for the change: a run that is
// stopped or fails between the two would otherwise leave a closed case with no shipped note, and nothing writes it again.
// A note that cannot be written makes no decision, and the next run, with the note writable, makes it and writes the
// note once, however many runs follow.
func TestAKeepIsNotMadeWithoutItsNoteAndTheNoteIsWrittenOnce(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	m.measure(t, Options{})
	hours(t, m.st, "after", m.clock.Add(time.Minute), 23*time.Hour, 100, 0)
	m.clock = m.clock.Add(25 * time.Hour)
	allow := refuseNotes(t, m.db)
	got := m.measure(t, Options{})
	if got.Status != "failed" || !strings.Contains(got.Why, "the note") || m.theCase(t).Status != store.CaseMeasuring {
		t.Fatalf("a keep whose note could not be written: %+v, case %s", got, m.theCase(t).Status)
	}
	if p, _, _ := m.r.Ship.Load(m.c.ID); p.State == Kept {
		t.Fatalf("the package was kept without its note: %+v", p)
	}
	allow()
	if got := m.measure(t, Options{}); got.Status != "improved" {
		t.Fatalf("the run after: %+v", got)
	}
	for i := 0; i < 2; i++ { // and the runs after that, which watch the kept change
		if _, err := m.r.Measure(ctx, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	shipped := notesOf(t, m.st, store.NoteShipped)
	if len(shipped) != 1 || shipped[0].ID != noteID("shipped", mustLoad(t, m)) || !strings.Contains(shipped[0].Effect, "p=") {
		t.Fatalf("the shipped note: %+v", shipped)
	}
	if c := m.theCase(t); c.Status != store.CaseClosed || c.Reason != store.Improved {
		t.Fatalf("the case: %+v", c)
	}
	// a note the owner took out of the report is not written again by a run that comes back to the decision
	if _, _, err := m.st.DoneNote(ctx, shipped[0].ID, m.clock); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.st.PutNoteOnce(ctx, store.Note{ID: shipped[0].ID, Kind: store.NoteShipped, Title: "again"}); err != nil || ok {
		t.Fatalf("a note written once is written again: %v %v", ok, err)
	}
	if notes, _ := m.st.Notes(ctx); len(notes) != 1 || notes[0].Done.IsZero() || notes[0].Title == "again" {
		t.Fatalf("the owner's note: %+v", notes)
	}
}

func mustLoad(t *testing.T, m *measured) Package {
	t.Helper()
	p, ok, err := m.r.Ship.Load(m.c.ID)
	if err != nil || !ok {
		t.Fatalf("the package: %v %v", ok, err)
	}
	return p
}

// A rollback held while the loop is not live saves that it is held and writes its decided note: the note first, so a note
// that could not be written is written by the next run, and the flag that stops it from being written again is set after.
func TestAHeldRollbacksNoteIsWrittenOnce(t *testing.T) {
	m := measuredCase(t)
	m.measure(t, Options{})
	hours(t, m.st, "a", m.clock.Add(time.Minute), 23*time.Hour, 100, 6)
	m.clock = m.clock.Add(25 * time.Hour)
	allow := refuseNotes(t, m.db)
	got := m.measure(t, Options{})
	if got.Status != "failed" || !strings.Contains(got.Why, "the note") {
		t.Fatalf("a held rollback whose note could not be written: %+v", got)
	}
	if p := mustLoad(t, m); p.Held {
		t.Fatal("the rollback was marked held without its note")
	}
	allow()
	for i := 0; i < 3; i++ {
		if got := m.measure(t, Options{}); got.Status != "held" {
			t.Fatalf("run %d: %+v", i, got)
		}
	}
	decided := notesOf(t, m.st, store.NoteDecided)
	if len(decided) != 1 || decided[0].ID != noteID("held", mustLoad(t, m)) || !strings.Contains(decided[0].Detail, "momentum ship rollback "+m.c.ID) {
		t.Fatalf("the held rollback's note: %+v", decided)
	}
	if !mustLoad(t, m).Held {
		t.Fatal("the rollback is not marked held")
	}
}

// A problem a case fixed that is back at its old share brings the case back with a decided note: written first, so that
// a note that failed leaves the case closed and the next run, reading the same windows, brings it back and writes it once.
func TestABackNoteIsWrittenOnceAndTheCaseComesBackWithIt(t *testing.T) {
	m := keptCase(t)
	closed := m.theCase(t).Closed
	for i := 0; i < 3; i++ {
		hours(t, m.st, fmt.Sprint("back", i), closed.Add(time.Duration(i)*24*time.Hour), 23*time.Hour, 100, 12)
	}
	m.clock = closed.Add(72 * time.Hour)
	allow := refuseNotes(t, m.db)
	got := m.measure(t, Options{})
	if got.Status != "failed" || !strings.Contains(got.Why, "the note") || m.theCase(t).Status != store.CaseClosed {
		t.Fatalf("a comeback whose note could not be written: %+v, case %s", got, m.theCase(t).Status)
	}
	allow()
	if got := m.measure(t, Options{}); got.Status != "back" || m.theCase(t).Status != store.CaseOpen {
		t.Fatalf("the run after: %+v, case %s", got, m.theCase(t).Status)
	}
	var back []store.Note
	for _, n := range notesOf(t, m.st, store.NoteDecided) {
		if strings.Contains(n.Title, "a problem it fixed is back") {
			back = append(back, n)
		}
	}
	if len(back) != 1 || !strings.HasPrefix(back[0].ID, "back-3:"+m.c.ID+":") {
		t.Fatalf("the comeback's note: %+v", back)
	}
}

// A rollback's note is put by its receipt, before the case leaves its measurement: a revert pushed and a note that could
// not be written leaves the case measuring with its package rolled back, the next run finishes the receipt, and the note
// is written once. The revert is not made twice.
func TestARollbacksNoteIsWrittenOnceByItsReceipt(t *testing.T) {
	ctx := context.Background()
	st, db := testStoreAt(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, _ := reviewedChange(t, s, st, f)
	r := measurementRunner(t, st)
	r.Ship, r.Dir = s, s.Dir
	clock := time.Now().Truncate(time.Millisecond)
	r.Now = func() time.Time { return clock }
	s.Now = r.Now
	if done := r.reviews(ctx, c.ID); len(done) != 1 || done[0].Step != "land" || done[0].Skipped != "" {
		t.Fatalf("landing: %+v", done)
	}
	run(t, f.main, "git", "fetch", "origin", "main")
	run(t, f.main, "git", "merge", "--ff-only", "origin/main")
	p, _, _ := s.Load(c.ID)
	p.Plan.Before, p.Plan.BeforeBase, p.Plan.Window = 6, 100, 24*time.Hour
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	m := &measured{r: r, st: st, db: db, c: c}
	m.measure(t, Options{})
	hours(t, st, "after", clock.Add(time.Minute), 23*time.Hour, 100, 6)
	clock = clock.Add(25 * time.Hour)
	if err := st.MarkRead(ctx, store.Transcripts, clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reverts := func() string { return run(t, f.dir, "git", "--git-dir", f.origin, "log", "--format=%s", "main") }
	allow := refuseNotes(t, db)
	got := m.measure(t, Options{})
	if got.Status != "failed" || !strings.Contains(got.Why, "the rollback") || m.theCase(t).Status != store.CaseMeasuring {
		t.Fatalf("a rollback whose note could not be written: %+v, case %s", got, m.theCase(t).Status)
	}
	if body := run(t, f.dir, "git", "--git-dir", f.origin, "show", "main:fake.go"); !strings.Contains(body, "return 41") {
		t.Fatal("the revert was not pushed before the receipt")
	}
	pushed := strings.Count(reverts(), "Revert")
	allow()
	if got := m.measure(t, Options{}); got.Status != "rolled back" || m.theCase(t).Status != store.CaseTesting {
		t.Fatalf("the run after: %+v, case %s", got, m.theCase(t).Status)
	}
	if _, err := r.Measure(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	var made []store.Note
	for _, n := range notesOf(t, st, store.NoteDecided) {
		if strings.Contains(n.Title, "rolled back its change") {
			made = append(made, n)
		}
	}
	if len(made) != 1 || !strings.HasPrefix(made[0].ID, "rolled-back:"+c.ID+":") || !strings.Contains(made[0].Detail, "no measured effect") {
		t.Fatalf("the rollback's note: %+v", made)
	}
	if n := strings.Count(reverts(), "Revert"); n != pushed || n != 1 {
		t.Fatalf("the revert is on origin %d times after the retry, was %d", n, pushed)
	}
}

// A change whose revert is journaled but not pushed (a rollback begun by hand that failed at the push) is not a change to
// keep, whatever its window shows: the step finishes the rollback before it measures anything, as the pick does, instead of
// closing the case as improved over a package that is rolling back.
func TestAChangeWhoseRevertIsJournaledIsRolledBackNotKept(t *testing.T) {
	ctx := context.Background()
	st, db := testStoreAt(t)
	f := newRepo(t, nil)
	refuse := filepath.Join(f.dir, "refuse-pushes")
	script(t, filepath.Join(f.origin, "hooks"), "pre-receive", `if [ -e "`+refuse+`" ]; then echo "refused" >&2; exit 1; fi`)
	s := shipFor(t, st, f)
	c, _ := reviewedChange(t, s, st, f)
	r := measurementRunner(t, st)
	r.Ship, r.Dir = s, s.Dir
	clock := time.Now().Truncate(time.Millisecond)
	r.Now = func() time.Time { return clock }
	s.Now = r.Now
	if done := r.reviews(ctx, c.ID); len(done) != 1 || done[0].Step != "land" || done[0].Skipped != "" {
		t.Fatalf("landing: %+v", done)
	}
	run(t, f.main, "git", "fetch", "origin", "main")
	run(t, f.main, "git", "merge", "--ff-only", "origin/main")
	p, _, _ := s.Load(c.ID)
	p.Plan.Before, p.Plan.BeforeBase, p.Plan.Window = 6, 100, 24*time.Hour
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	m := &measured{r: r, st: st, db: db, c: c}
	m.measure(t, Options{})
	// the owner begins a rollback by hand, and its push is refused
	write(t, refuse, "")
	if _, err := s.RollBack(ctx, c.ID, "by hand"); err == nil {
		t.Fatal("the push was not refused")
	}
	if p := mustLoad(t, m); p.State != RollingBack || p.Undo == nil {
		t.Fatalf("the journaled rollback: %+v", p)
	}
	if err := os.Remove(refuse); err != nil {
		t.Fatal(err)
	}
	// the window after shows the problem gone, and the data says improved
	hours(t, st, "after", clock.Add(time.Minute), 23*time.Hour, 100, 0)
	clock = clock.Add(25 * time.Hour)
	if err := st.MarkRead(ctx, store.Transcripts, clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if dry := m.measure(t, Options{Dry: true}); dry.Status != "rolled back" || !strings.Contains(dry.Why, "begun") || mustLoad(t, m).State != RollingBack {
		t.Fatalf("a dry measure finishes nothing: %+v", dry)
	}
	got := m.measure(t, Options{})
	if got.Status != "rolled back" || m.theCase(t).Status != store.CaseTesting {
		t.Fatalf("the step: %+v, case %s", got, m.theCase(t).Status)
	}
	if p := mustLoad(t, m); p.State != RolledBack {
		t.Fatalf("the package: %+v", p)
	}
	if body := run(t, f.dir, "git", "--git-dir", f.origin, "show", "main:fake.go"); !strings.Contains(body, "return 41") {
		t.Fatal("origin did not roll back")
	}
	if n := strings.Count(run(t, f.dir, "git", "--git-dir", f.origin, "log", "--format=%s", "main"), "Revert"); n != 1 {
		t.Fatalf("the revert is on origin %d times", n)
	}
	if shipped := notesOf(t, st, store.NoteShipped); len(shipped) != 0 {
		t.Fatalf("a shipped note for a change that was rolled back: %+v", shipped)
	}
	var made []store.Note
	for _, n := range notesOf(t, st, store.NoteDecided) {
		if strings.Contains(n.Title, "rolled back its change") {
			made = append(made, n)
		}
	}
	if len(made) != 1 || !strings.Contains(made[0].Detail, "by hand") {
		t.Fatalf("the rollback's note, with the reason it was begun for: %+v", made)
	}
}
