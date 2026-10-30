package store

import (
	"context"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

// SessionStamps loads when each live session entered its phase, as the last run saved it.
func (s *Store) SessionStamps(ctx context.Context) (state.Stamps, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, phase, since, seq, seen FROM session_stamps`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := state.Stamps{}
	for rows.Next() {
		var key, phase string
		var since, seen int64
		var seq int
		if err := rows.Scan(&key, &phase, &since, &seq, &seen); err != nil {
			return nil, err
		}
		out[key] = state.Stamp{Phase: sessions.Phase(phase), Since: time.UnixMilli(since), Seq: seq, Seen: time.UnixMilli(seen)}
	}
	return out, rows.Err()
}

// SaveSessionStamps replaces the saved stamps with these, in one transaction.
func (s *Store) SaveSessionStamps(ctx context.Context, st state.Stamps) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_stamps`); err != nil {
		return err
	}
	for k, r := range st {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_stamps (key, phase, since, seq, seen) VALUES (?,?,?,?,?)`,
			k, string(r.Phase), r.Since.UnixMilli(), r.Seq, r.Seen.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
