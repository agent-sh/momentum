package store

import (
	"context"
	"errors"
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
	BySession    bool   // the sessions' hours; otherwise the machine's
	Harness      string // when set, only that harness's events
	Actor        Actor  // when set, only the hours in which it did something (the owner's turns, say)
}

// Activity returns the units of activity over a span: the hours in which the sessions picked (an event of a
// session's own), or the machine, did anything the store recorded, from any source.
func (s *Store) Activity(ctx context.Context, q ActivityQuery) (map[Unit]bool, error) {
	if !q.Until.After(q.Since) {
		return nil, errors.New("an activity span ends after it starts")
	}
	where, args := []string{"ts >= ?", "ts < ?"}, []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	if q.Harness != "" {
		where, args = append(where, "harness = ?"), append(args, q.Harness)
	}
	if q.Actor != "" {
		where, args = append(where, "actor = ?"), append(args, string(q.Actor))
	}
	session := "''"
	if q.BySession {
		session = "session"
		where = append(where, "session != ''")
	}
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
