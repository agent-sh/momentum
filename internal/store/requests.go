package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// requestTables keep the owner's checks of requests (SPEC, Requests): a check names a request by its id and the
// result it checked by its key. Nothing derives them and nothing deletes one; an undo is dated.
const requestTables = `
CREATE TABLE IF NOT EXISTS request_checks (id INTEGER PRIMARY KEY, request TEXT NOT NULL, result TEXT NOT NULL,
  at INTEGER NOT NULL, undone INTEGER NOT NULL DEFAULT 0);
`

var requestTableNames = []string{"request_checks"}

// RequestCheck is the owner's check of one result of a request. Undone is when he undid it; zero while it stands.
type RequestCheck struct {
	Request string
	Result  string
	At      time.Time
	Undone  time.Time
}

// ErrNoCheck is an undo with no check to undo.
var ErrNoCheck = errors.New("no check to undo")

// PutRequestCheck records a check.
func (s *Store) PutRequestCheck(ctx context.Context, c RequestCheck) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO request_checks (request, result, at) VALUES (?,?,?)`, c.Request, c.Result,
		c.At.UnixMilli())
	return err
}

// UndoRequestCheck undoes the newest check of any of ids that stands, and returns it.
func (s *Store) UndoRequestCheck(ctx context.Context, ids []string, at time.Time) (RequestCheck, error) {
	if len(ids) == 0 {
		return RequestCheck{}, ErrNoCheck
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RequestCheck{}, err
	}
	defer tx.Rollback()
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var row int64
	var c RequestCheck
	var ms int64
	err = tx.QueryRowContext(ctx, `SELECT id, request, result, at FROM request_checks WHERE undone = 0 AND request IN (?`+
		strings.Repeat(",?", len(ids)-1)+`) ORDER BY at DESC, id DESC LIMIT 1`, args...).Scan(&row, &c.Request, &c.Result, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestCheck{}, ErrNoCheck
	}
	if err != nil {
		return RequestCheck{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE request_checks SET undone = ? WHERE id = ?`, at.UnixMilli(), row); err != nil {
		return RequestCheck{}, err
	}
	c.At, c.Undone = time.UnixMilli(ms), at
	return c, tx.Commit()
}

// RequestChecks are the checks of any of ids, oldest first.
func (s *Store) RequestChecks(ctx context.Context, ids []string) ([]RequestCheck, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request, result, at, undone FROM request_checks WHERE request IN (?`+
		strings.Repeat(",?", len(ids)-1)+`) ORDER BY at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestCheck
	for rows.Next() {
		var c RequestCheck
		var at, undone int64
		if err := rows.Scan(&c.Request, &c.Result, &at, &undone); err != nil {
			return nil, err
		}
		c.At = time.UnixMilli(at)
		if undone != 0 {
			c.Undone = time.UnixMilli(undone)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ThreadsSince are the sessions with transcript events since a time.
func (s *Store) ThreadsSince(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session FROM events WHERE ts >= ? AND source = ? AND session != ''
	  ORDER BY session`, since.UnixMilli(), Transcripts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ThreadsByPrefix are the sessions with transcript events whose id is prefix or starts with it, at most two.
func (s *Store) ThreadsByPrefix(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session FROM events WHERE session >= ? AND session < ? AND source = ?
	  ORDER BY session LIMIT 2`, prefix, prefix+"\uffff", Transcripts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ThreadFirst is when a session's first transcript event happened; ok is false for a session with none.
func (s *Store) ThreadFirst(ctx context.Context, session string) (time.Time, bool, error) {
	var ms sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM events WHERE session = ? AND source = ?`, session, Transcripts).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, false, err
	}
	return time.UnixMilli(ms.Int64), true, nil
}

// requestKinds are the kinds a request is read from: the turns, the replies, the calls and their results.
var requestKinds = []string{"owner_text", "agent_text", "tool_call", "tool_result"}

// ThreadEvents passes a session's transcript events of the kinds a request reads, its subagents' among them,
// oldest first, to fn, one at a time: a long session is never held whole.
func (s *Store) ThreadEvents(ctx context.Context, session string, fn func(Event) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE session = ? AND source = ? AND kind IN (?,?,?,?)
	  ORDER BY ts, source_key`, session, Transcripts, requestKinds[0], requestKinds[1], requestKinds[2], requestKinds[3])
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var ts int64
		var actor string
		if err := rows.Scan(&e.Source, &e.SourceKey, &ts, &e.Session, &e.Agent, &e.Harness, &e.Model, &e.Cwd, &actor,
			&e.Kind, &e.Tool, &e.Summary, &e.Meta, &e.Ref, &e.Heuristic, &e.Experiment); err != nil {
			return err
		}
		e.TS, e.Actor = time.UnixMilli(ts), Actor(actor)
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// OwnerTurnsBetween are the main-thread owner turns of the transcripts from from to through, both included,
// oldest first: what a turn's copies and twins are found among.
func (s *Store) OwnerTurnsBetween(ctx context.Context, from, through time.Time) ([]Event, error) {
	return scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE kind = 'owner_text' AND ts >= ? AND ts <= ?
	  AND source = ? AND actor = ? AND agent = '' ORDER BY ts, source_key`, from.UnixMilli(), through.UnixMilli(), Transcripts,
		string(ActorOwner)))
}

// LastPhase is a session's newest event of source sessions: the last change a sessions run saw. ok is false when
// none saw it.
func (s *Store) LastPhase(ctx context.Context, session string) (Event, bool, error) {
	evs, err := scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor,
	  kind, tool, summary, meta, ref, heuristic, experiment FROM events WHERE session = ? AND source = ?
	  ORDER BY ts DESC, source_key DESC LIMIT 1`, session, Sessions))
	if err != nil || len(evs) == 0 {
		return Event{}, false, err
	}
	return evs[0], true, nil
}
