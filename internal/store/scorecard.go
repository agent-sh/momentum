package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The scorecard's record (internal/scorecard): when it started judging, each week it judged a system in, and when a
// system was shut down or turned back on. It is kept: a week's numbers come from records that age out of their
// sources (a delivery log keeps a month), and a shutdown counts two weeks in a row.
const scorecardTables = `
CREATE TABLE IF NOT EXISTS scorecard_state (key TEXT PRIMARY KEY, value TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS scorecard_weeks (system TEXT NOT NULL, week TEXT NOT NULL, scored INTEGER NOT NULL,
  delivers INTEGER NOT NULL, why TEXT NOT NULL DEFAULT '', numbers TEXT NOT NULL DEFAULT '{}', at INTEGER NOT NULL,
  PRIMARY KEY (system, week)) WITHOUT ROWID;
`

var scorecardTableNames = []string{"scorecard_state", "scorecard_weeks"}

// ScorecardWeek is one week's verdict on one system: whether the week was scored (it began after the scorecard
// started judging the system), whether the system delivered, why, and the numbers it was judged on (a JSON object).
type ScorecardWeek struct {
	System   string
	Week     string // YYYY-Www
	Scored   bool
	Delivers bool
	Why      string
	Numbers  string
	At       time.Time
}

// ScorecardValue is a value the scorecard keeps by its key, and when it was set; ok is false when none is.
func (s *Store) ScorecardValue(ctx context.Context, key string) (value string, at time.Time, ok bool, err error) {
	var ms int64
	err = s.db.QueryRowContext(ctx, `SELECT value, at FROM scorecard_state WHERE key = ?`, key).Scan(&value, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return value, time.UnixMilli(ms), true, nil
}

// SetScorecardValue keeps a value by its key, replacing the one there.
func (s *Store) SetScorecardValue(ctx context.Context, key, value string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO scorecard_state (key, value, at) VALUES (?, ?, ?)
	  ON CONFLICT(key) DO UPDATE SET value = excluded.value, at = excluded.at`, key, value, at.UnixMilli())
	return err
}

// PutScorecardWeek keeps a week's verdict, replacing the one of the same system and week.
func (s *Store) PutScorecardWeek(ctx context.Context, w ScorecardWeek) error {
	if w.System == "" || w.Week == "" {
		return errors.New("a scorecard week needs a system and a week")
	}
	if w.Numbers == "" {
		w.Numbers = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO scorecard_weeks (system, week, scored, delivers, why, numbers, at)
	  VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(system, week) DO UPDATE SET scored = excluded.scored,
	  delivers = excluded.delivers, why = excluded.why, numbers = excluded.numbers, at = excluded.at`,
		w.System, w.Week, w.Scored, w.Delivers, w.Why, w.Numbers, w.At.UnixMilli())
	return err
}

// ScorecardWeeks are a system's kept weeks, the oldest first (a week's name sorts by its time).
func (s *Store) ScorecardWeeks(ctx context.Context, system string) ([]ScorecardWeek, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT system, week, scored, delivers, why, numbers, at FROM scorecard_weeks
	  WHERE system = ? ORDER BY week`, system)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScorecardWeek
	for rows.Next() {
		var w ScorecardWeek
		var at int64
		if err := rows.Scan(&w.System, &w.Week, &w.Scored, &w.Delivers, &w.Why, &w.Numbers, &at); err != nil {
			return nil, err
		}
		w.At = time.UnixMilli(at)
		out = append(out, w)
	}
	return out, rows.Err()
}

// CaseLogEntry is a line of any case's log, with its case and family.
type CaseLogEntry struct {
	Case, Family string
	CaseEntry
}

// CaseLogBetween are the lines of every case's log with an action in a window, the oldest first.
func (s *Store) CaseLogBetween(ctx context.Context, action string, since, until time.Time) ([]CaseLogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.case_id, COALESCE(c.family, ''), l.seq, l.at, l.actor, l.action, l.detail
	  FROM case_log l LEFT JOIN cases c ON c.id = l.case_id WHERE l.action = ? AND l.at >= ? AND l.at < ? ORDER BY l.at, l.case_id, l.seq`,
		action, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseLogEntry
	for rows.Next() {
		var e CaseLogEntry
		var at int64
		if err := rows.Scan(&e.Case, &e.Family, &e.Seq, &at, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// HasSession says whether the store holds an event of a session.
func (s *Store) HasSession(ctx context.Context, session string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE session = ? LIMIT 1`, session).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ToolCallsLike are the tool calls in a window whose tool matches a LIKE pattern, the oldest first.
func (s *Store) ToolCallsLike(ctx context.Context, pattern string, since, until time.Time) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE kind = 'tool_call' AND ts >= ? AND ts < ? AND tool LIKE ?
	  ORDER BY ts, source_key`, since.UnixMilli(), until.UnixMilli(), pattern)
	return scanEvents(rows, err)
}

// FirstOf is the time of the oldest event of a kind, zero when the store holds none.
func (s *Store) FirstOf(ctx context.Context, kind string) (time.Time, error) {
	var ms sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM events WHERE kind = ?`, kind).Scan(&ms); err != nil || !ms.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64), nil
}
