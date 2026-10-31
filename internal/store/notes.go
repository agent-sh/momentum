package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/agent-sh/momentum/internal/plain"
)

// NoteKind is what a note reports. Notes are what momentum shows of its state besides the live sessions: the
// decisions only the owner can make, the ones made on his behalf, the improvements that landed, and the problems
// momentum saw and could not fix itself. Nothing derives them, so they are kept: a schema change keeps them, and
// Rebuild never touches them.
type NoteKind string

const (
	NoteNeeds   NoteKind = "needs"   // a decision only the owner can make; its title is the question, or what momentum could not do
	NoteDecided NoteKind = "decided" // a decision made on his behalf that he may override; its detail says how
	NoteShipped NoteKind = "shipped" // an improvement that landed, with its measured effect when there is one
	NoteProblem NoteKind = "problem" // something momentum saw and could not fix itself
)

// NoteKinds are the kinds a note can have.
var NoteKinds = []NoteKind{NoteNeeds, NoteDecided, NoteShipped, NoteProblem}

// A note's importance orders it in its section before its time: high first, low last.
const (
	ImportanceLow    = -1
	ImportanceNormal = 0
	ImportanceHigh   = 1
)

// TitleMax is the longest title in characters: one plain line.
const TitleMax = 120

// DefaultNoteSource is the source of a note that names none.
const DefaultNoteSource = "momentum"

// Link is a labelled http or https address.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Note is one item momentum shows. Its text is plain (package plain): one line each, no markup, no control or
// format character.
type Note struct {
	ID         string
	Kind       NoteKind
	Title      string // for needs, the question itself, or what momentum could not do
	Detail     string // short sentences, within what the views keep (the phone's, 400 characters); for decided, how to override it
	Effect     string // for shipped, the measured effect
	Links      []Link
	Source     string // who wrote it: momentum, or another tool
	Importance int    // ImportanceHigh, ImportanceNormal or ImportanceLow
	At         time.Time
	Done       time.Time // when note done took it out of the report; zero while it stands
}

// Standing reports whether the report shows the note: it is not done, and it is a needs or problem note, which
// stands until done, or a decided or shipped one from since on.
func (n Note) Standing(since time.Time) bool {
	if !n.Done.IsZero() {
		return false
	}
	return n.Kind == NoteNeeds || n.Kind == NoteProblem || !n.At.Before(since)
}

// ErrBadNote means a note cannot be stored as it is: its kind, title, importance, id or a link is wrong.
// The ids of the report's running items (session:KEY, task:KEY/ID) are not a note's, nor is one that starts with
// a dash, which note done would read as a flag.
var ErrBadNote = errors.New("bad note")

// ErrNoNote means no note has the id.
var ErrNoNote = errors.New("no note has this id")

// SessionProblem is the note supervise adds when it gives up on a session: a problem keyed by the session, so
// giving up on it again replaces the note instead of adding a second one. Add it once, when supervise gives up,
// not on every run that finds the session still stuck: an add makes a note the owner resolved stand again. Store
// it with Store.PutNote, or Run.PutNote inside a sessions run; note done resolves it.
func SessionProblem(session, title, detail string, at time.Time) Note {
	return Note{ID: "supervise:" + session, Kind: NoteProblem, Title: title, Detail: detail, Source: DefaultNoteSource, At: at}
}

// clean checks a note and puts its text in the form it is stored in: each text on one line, a title of at most
// TitleMax characters (a longer one is clipped), the default source, and the time now when it has none.
func clean(n Note, now time.Time) (Note, error) {
	bad := func(format string, args ...any) (Note, error) {
		return Note{}, fmt.Errorf("%w: %s", ErrBadNote, fmt.Sprintf(format, args...))
	}
	known := false
	for _, k := range NoteKinds {
		known = known || n.Kind == k
	}
	if !known {
		return bad("kind %q is not needs, decided, shipped or problem", n.Kind)
	}
	if n.Importance < ImportanceLow || n.Importance > ImportanceHigh {
		return bad("importance %d is not high (1), normal (0) or low (-1)", n.Importance)
	}
	switch {
	case strings.IndexFunc(n.ID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0:
		return bad("id %q holds a space, a control or a format character", n.ID)
	case strings.HasPrefix(n.ID, "-"):
		return bad("id %q starts with -", n.ID)
	case strings.HasPrefix(n.ID, "session:") || strings.HasPrefix(n.ID, "task:"):
		return bad("id %q starts as the report's running items do (session:, task:)", n.ID)
	}
	n.Title, n.Detail, n.Effect, n.Source = plain.Line(n.Title), plain.Line(n.Detail), plain.Line(n.Effect), plain.Line(n.Source)
	if n.Title == "" {
		return bad("it has no title")
	}
	n.Title = plain.Clip(n.Title, TitleMax)
	if n.Source == "" {
		n.Source = DefaultNoteSource
	}
	links := make([]Link, 0, len(n.Links))
	for _, l := range n.Links {
		l.Label, l.URL = plain.Line(l.Label), strings.TrimSpace(l.URL)
		u, err := url.Parse(l.URL)
		switch {
		case l.Label == "":
			return bad("link %q has no label", l.URL)
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || plain.Line(l.URL) != l.URL:
			return bad("link %q is not an http or https address", l.URL)
		}
		links = append(links, l)
	}
	n.Links = links
	if n.At.IsZero() {
		n.At = now
	}
	n.Done = time.Time{}
	return n, nil
}

// newNoteID is a short random id: n and 8 hex digits.
func newNoteID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "n" + hex.EncodeToString(b)
}

// PutNote stores a note and returns it as stored. A note whose id is stored already replaces it, and stands again
// if it was done: an id names one item, and adding it again says it holds now. A note without an id gets a new one.
// Its text is made plain (package plain), a title longer than TitleMax is clipped, a note without a time is dated
// now, and one without a source is momentum's. A wrong kind, importance, id or link is ErrBadNote.
func (s *Store) PutNote(ctx context.Context, n Note) (Note, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()
	if n, err = putNote(ctx, tx, n); err != nil {
		return Note{}, err
	}
	return n, tx.Commit()
}

// PutWithNote stores events, as Put does, and a note, as PutNote does, in one transaction: a note that sums the
// events up holds what they hold, however the writer stops.
func (s *Store) PutWithNote(ctx context.Context, evs []Event, n Note) (Note, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()
	if _, err := put(ctx, tx, evs); err != nil {
		return Note{}, err
	}
	if n, err = putNote(ctx, tx, n); err != nil {
		return Note{}, err
	}
	return n, tx.Commit()
}

// PutNote stores a note in the run's transaction, as Store.PutNote does: a sessions run holds the store's write
// lock, so a note from inside it goes in its transaction.
func (r *Run) PutNote(n Note) (Note, error) { return putNote(r.ctx, r.tx, n) }

func putNote(ctx context.Context, tx querier, n Note) (Note, error) {
	n, err := clean(n, time.Now())
	if err != nil {
		return Note{}, err
	}
	links, err := json.Marshal(n.Links)
	if err != nil {
		return Note{}, err
	}
	for n.ID == "" {
		id := newNoteID()
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE id = ?`, id).Scan(&taken); err != nil {
			return Note{}, err
		}
		if taken == 0 {
			n.ID = id
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notes (id, kind, title, detail, effect, links, source, importance, at, done)
	  VALUES (?,?,?,?,?,?,?,?,?,0) ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, title = excluded.title,
	    detail = excluded.detail, effect = excluded.effect, links = excluded.links, source = excluded.source,
	    importance = excluded.importance, at = excluded.at, done = 0`,
		n.ID, string(n.Kind), n.Title, n.Detail, n.Effect, string(links), n.Source, n.Importance, n.At.UnixMilli()); err != nil {
		return Note{}, err
	}
	return n, nil
}

// DoneNote takes a note out of the report as of at: a needs or problem note is resolved, a decided or shipped one
// withdrawn. It returns the note as stored, and whether it stood until now: a note done already keeps the time it
// was done. An id no note has is ErrNoNote.
func (s *Store) DoneNote(ctx context.Context, id string, at time.Time) (Note, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE notes SET done = ? WHERE id = ? AND done = 0`, max(1, at.UnixMilli()), id)
	if err != nil {
		return Note{}, false, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return Note{}, false, err
	}
	ns, err := notes(ctx, tx, `WHERE id = ?`, id)
	if err != nil {
		return Note{}, false, err
	}
	if len(ns) == 0 {
		return Note{}, false, fmt.Errorf("%w: %q", ErrNoNote, id)
	}
	return ns[0], changed > 0, tx.Commit()
}

// Notes returns every note, done ones too, newest first.
func (s *Store) Notes(ctx context.Context) ([]Note, error) {
	return notes(ctx, s.db, "")
}

func notes(ctx context.Context, q querier, where string, args ...any) ([]Note, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, kind, title, detail, effect, links, source, importance, at, done FROM notes `+
		where+` ORDER BY at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var kind, links string
		var at, done int64
		if err := rows.Scan(&n.ID, &kind, &n.Title, &n.Detail, &n.Effect, &links, &n.Source, &n.Importance, &at, &done); err != nil {
			return nil, err
		}
		n.Kind, n.At = NoteKind(kind), time.UnixMilli(at)
		if done != 0 {
			n.Done = time.UnixMilli(done)
		}
		if err := json.Unmarshal([]byte(links), &n.Links); err != nil {
			return nil, fmt.Errorf("note %s: its links: %w", n.ID, err)
		}
		if n.Links == nil {
			n.Links = []Link{}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
