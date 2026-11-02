package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Unit is a unit of activity, what a measurement counts in: an hour of a session in which it did anything, or,
// with no session, an hour of the machine in which the store recorded anything at all.
type Unit struct {
	Session string
	Hour    int64 // hours since the epoch
}

// UnitOf is the unit an event falls in: its session's hour, or its hour of the machine when bySession is false.
func UnitOf(e Event, bySession bool) Unit {
	u := Unit{Hour: e.TS.UnixMilli() / 3_600_000}
	if bySession {
		u.Session = e.Session
	}
	return u
}

// ActivityQuery picks the units of activity over a span.
type ActivityQuery struct {
	Since, Until time.Time
	Sources      []string // nil counts every source; otherwise only these sources
	ExcludeCwd   string   // exclude this directory and its descendants
	BySession    bool     // the sessions' hours; otherwise the machine's
	Harness      string   // when set, only that harness's events
	Actor        Actor    // when set, only the hours in which it did something (the owner's turns, say)
}

// Activity returns the units of activity over a span: the hours in which the sessions picked (an event of a
// session's own), or the machine, did anything the store recorded, from any source.
func (s *Store) Activity(ctx context.Context, q ActivityQuery) (map[Unit]bool, error) {
	if !q.Until.After(q.Since) {
		return nil, errors.New("an activity span ends after it starts")
	}
	where, args, session := activityWhere(q)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+session+`, ts / 3600000 FROM events WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Unit]bool{}
	for rows.Next() {
		var u Unit
		if err := rows.Scan(&u.Session, &u.Hour); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// activityWhere gives activity and its sources the same bounds.
func activityWhere(q ActivityQuery) ([]string, []any, string) {
	where, args := []string{"ts >= ?", "ts < ?"}, []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	where, args = excludeDirectory(where, args, q.ExcludeCwd)
	if q.Harness != "" {
		where, args = append(where, "harness = ?"), append(args, q.Harness)
	}
	if q.Actor != "" {
		where, args = append(where, "actor = ?"), append(args, string(q.Actor))
	}
	if q.Sources != nil {
		marks := make([]string, len(q.Sources))
		for i, source := range q.Sources {
			marks[i] = "?"
			args = append(args, source)
		}
		where = append(where, "source IN ("+strings.Join(marks, ",")+")")
	}
	session := "''"
	if q.BySession {
		session = "session"
		where = append(where, "session != ''")
	}
	return where, args, session
}

// excludeDirectory leaves out both the configured directory and its resolved path.
func excludeDirectory(where []string, args []any, dir string) ([]string, []any) {
	if dir == "" {
		return where, args
	}
	paths := []string{filepath.Clean(dir)}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != paths[0] {
		paths = append(paths, resolved)
	}
	for _, path := range paths {
		where = append(where, "cwd != ? AND instr(cwd, ?) != 1")
		args = append(args, path, strings.TrimRight(path, "/")+"/")
	}
	return where, args
}

// Sources names the signal and every source with an event in the selected span.
func (s *Store) Sources(ctx context.Context, signal string, q ActivityQuery) ([]string, error) {
	where, args, _ := activityWhere(q)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT source FROM events WHERE `+strings.Join(where, " AND ")+` UNION SELECT ? ORDER BY 1`, append(args, signal)...)
	if err != nil {
		return nil, err
	}
	var sources []string
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return sources, nil
}

// UnreadSources checks recorded sources even when a window has none of their events.
func (s *Store) UnreadSources(ctx context.Context, signal string, q ActivityQuery) ([]string, error) {
	sources := q.Sources
	if sources == nil {
		var err error
		sources, err = s.Sources(ctx, signal, q)
		if err != nil {
			return nil, err
		}
	}
	var unread []string
	for _, source := range sources {
		at, err := s.ReadWindowThrough(ctx, source, q.Since)
		if err != nil {
			return nil, err
		}
		if !at.After(q.Until) {
			unread = append(unread, source)
		}
	}
	return unread, nil
}

// ReadThrough reads only the complete run start, never a cursor update after the read.
func (s *Store) ReadThrough(ctx context.Context, source string) (time.Time, error) {
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated),0) FROM cursors WHERE source = ?`, source+":read").Scan(&updated)
	if updated == 0 {
		return time.Time{}, err
	}
	return time.UnixMilli(updated), err
}

// MarkRead records the start of a successful complete read, after all its events have been stored.
// An overlapping older run cannot move the record back.
func (s *Store) MarkRead(ctx context.Context, source string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cursors(source,position,updated) VALUES (?, '', ?)
 ON CONFLICT(source) DO UPDATE SET updated=MAX(updated, excluded.updated)`, source+":read", at.UnixMilli())
	return err
}

// ReadWindowThrough includes a partial run only when its failed files predate the window.
func (s *Store) ReadWindowThrough(ctx context.Context, source string, since time.Time) (time.Time, error) {
	at, err := s.ReadThrough(ctx, source)
	if err != nil {
		return at, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT position, updated FROM cursors WHERE source LIKE ?`, source+":read:run:%")
	if err != nil {
		return at, err
	}
	defer rows.Close()
	for rows.Next() {
		var failed string
		var started int64
		if err := rows.Scan(&failed, &started); err != nil {
			return at, err
		}
		newest, err := strconv.ParseInt(failed, 10, 64)
		if err != nil {
			return at, err
		}
		if time.UnixMilli(newest).Before(since) && time.UnixMilli(started).After(at) {
			at = time.UnixMilli(started)
		}
	}
	return at, rows.Err()
}

// MarkPartialRead records a listed run and the newest modification among files it failed to read.
func (s *Store) MarkPartialRead(ctx context.Context, source string, started, failed time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO cursors(source,position,updated) VALUES (?,?,?)`,
		source+":read:run:"+strconv.FormatInt(started.UnixNano(), 10), strconv.FormatInt(failed.UnixMilli(), 10), started.UnixMilli())
	return err
}
