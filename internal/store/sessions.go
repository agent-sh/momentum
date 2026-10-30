package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

// SessionStamps loads when each live session entered its phase, as the last run saved it.
func (s *Store) SessionStamps(ctx context.Context) (state.Stamps, error) {
	return loadStamps(ctx, s.db)
}

// SaveSessionStamps replaces the saved stamps with these, in one transaction.
func (s *Store) SaveSessionStamps(ctx context.Context, st state.Stamps) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveStamps(ctx, tx, st); err != nil {
		return err
	}
	return tx.Commit()
}

func loadStamps(ctx context.Context, q querier) (state.Stamps, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, phase, since, seq, seen, tool, name, auto, ask, ask_kind FROM session_stamps`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := state.Stamps{}
	for rows.Next() {
		var key, phase, askKind string
		var since, seen int64
		var r state.Stamp
		if err := rows.Scan(&key, &phase, &since, &r.Seq, &seen, &r.Tool, &r.Name, &r.Auto, &r.Ask, &askKind); err != nil {
			return nil, err
		}
		r.Phase, r.AskKind, r.Since, r.Seen = sessions.Phase(phase), sessions.AskKind(askKind), time.UnixMilli(since), time.UnixMilli(seen)
		out[key] = r
	}
	return out, rows.Err()
}

func saveStamps(ctx context.Context, q querier, st state.Stamps) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM session_stamps`); err != nil {
		return err
	}
	for k, r := range st {
		if _, err := q.ExecContext(ctx, `INSERT INTO session_stamps (key, phase, since, seq, seen, tool, name, auto, ask, ask_kind)
		  VALUES (?,?,?,?,?,?,?,?,?,?)`, k, string(r.Phase), r.Since.UnixMilli(), r.Seq, r.Seen.UnixMilli(), r.Tool, r.Name, r.Auto,
			r.Ask, string(r.AskKind)); err != nil {
			return err
		}
	}
	return nil
}

// LockSessions takes the lock that makes sessions runs on this store happen one after another: a run holds it
// from before its scan until its transaction committed, so each run reads the stamps and checkpoints the
// previous run saved, and observes the sessions after it. Another run waits for it. unlock releases it.
func (s *Store) LockSessions() (unlock func(), err error) {
	f, err := os.OpenFile(s.path+".sessions-lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("sessions lock: %w", err)
	}
	return func() { f.Close() }, nil
}

// Run is one sessions run's write transaction: what it reads in it is what the previous run saved, and what it
// saves lands at once or not at all.
type Run struct {
	ctx   context.Context
	tx    *sql.Tx
	after []func()
}

// SessionRun runs fn in one write transaction and commits when fn returns nil. A concurrent run waits for this
// one, so it reads the stamps this one saves.
func (s *Store) SessionRun(ctx context.Context, fn func(*Run) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r := &Run{ctx: ctx, tx: tx}
	if err := fn(r); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, f := range r.after {
		f()
	}
	return nil
}

// Stamps loads the stamps the last run saved.
func (r *Run) Stamps() (state.Stamps, error) { return loadStamps(r.ctx, r.tx) }

// SaveStamps replaces the saved stamps with these.
func (r *Run) SaveStamps(st state.Stamps) error { return saveStamps(r.ctx, r.tx, st) }

// Put stores events, as Store.Put does, and returns how many were new.
func (r *Run) Put(evs []Event) (int, error) { return put(r.ctx, r.tx, evs) }

// Trackers is the store's side of the session trackers' checkpoints for one build of momentum. It hands a
// source the checkpoint an earlier run of the same build saved; a checkpoint of another build is not handed out,
// since its state may mean something else to this code. It remembers what it handed out, so a run rewrites only
// the checkpoints that moved.
type Trackers struct {
	s     *Store
	build string
	held  map[string][]byte // tool and key -> the state the store holds
}

// Trackers returns the checkpoints of one build.
func (s *Store) Trackers(build string) *Trackers {
	return &Trackers{s: s, build: build, held: map[string][]byte{}}
}

// Checkpoint returns the checkpoint a run of this build saved for a tracker, or nil. The state and its log come
// from one statement, so from one snapshot: a run saving meanwhile cannot pair this state with its newer log. The
// statement returns the state once, ahead of the log, and every entry the snapshot holds: a save drops the entries
// below the base in the transaction that moves it.
func (t *Trackers) Checkpoint(ctx context.Context, tool, key string) (*sessions.Checkpoint, error) {
	rows, err := t.s.db.QueryContext(ctx, `SELECT 0, build, state, log_base, log_seq, 0, '', '', '', '' FROM trackers WHERE tool = ? AND key = ?
	  UNION ALL SELECT 1, '', NULL, 0, 0, seq, role, name, origin, text FROM tracker_log WHERE tool = ? AND key = ?
	  ORDER BY 1, 6`, tool, key, tool, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var build string
	var st []byte
	var base, end int
	var items []sessions.Entry
	found := false
	for rows.Next() {
		var part, seq, b, e int
		var bd, role, name, origin, text string
		var blob []byte
		if err := rows.Scan(&part, &bd, &blob, &b, &e, &seq, &role, &name, &origin, &text); err != nil {
			return nil, err
		}
		if part == 0 {
			found, build, st, base, end = true, bd, blob, b, e
			continue
		}
		if !found {
			continue // entries without their checkpoint: there is none
		}
		if seq != base+len(items) {
			return nil, fmt.Errorf("its log lacks entry %d", base+len(items))
		}
		items = append(items, sessions.Entry{Role: sessions.Role(role), Name: name, Origin: origin, Text: text})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found || build != t.build {
		return nil, nil
	}
	if base+len(items) != end {
		return nil, fmt.Errorf("its log holds %d entries, not %d", len(items), end-base)
	}
	t.held[tool+"\x00"+key] = st
	l := sessions.Kept(base, items)
	return &sessions.Checkpoint{Tool: tool, Key: key, State: st, Log: &l}, nil
}

// SaveCheckpoints keeps the checkpoints of the trackers a run used. A checkpoint that did not move since the
// store handed it out only records that it was seen; one that moved replaces its state and brings its log up to
// date entry by entry. When the store no longer holds what it handed out (another run saved or dropped it since),
// the log is written whole. Once the run commits, the logs count as saved.
func (r *Run) SaveCheckpoints(t *Trackers, cps []sessions.Checkpoint, now time.Time) error {
	for _, cp := range cps {
		id := cp.Tool + "\x00" + cp.Key
		from, items := cp.Log.Unsaved()
		held, ok := t.held[id]
		if ok {
			var cur []byte
			err := r.tx.QueryRowContext(r.ctx, `SELECT state FROM trackers WHERE tool = ? AND key = ? AND build = ?`, cp.Tool, cp.Key, t.build).Scan(&cur)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				ok = false
			case err != nil:
				return err
			default:
				ok = bytes.Equal(cur, held)
			}
		}
		if ok && len(items) == 0 && bytes.Equal(held, cp.State) {
			if _, err := r.tx.ExecContext(r.ctx, `UPDATE trackers SET seen = ? WHERE tool = ? AND key = ?`, now.UnixMilli(), cp.Tool, cp.Key); err != nil {
				return err
			}
			continue
		}
		base := cp.Log.Base()
		if !ok {
			from = base
			items, _ = cp.Log.Since(base)
		}
		if _, err := r.tx.ExecContext(r.ctx, `INSERT INTO trackers (tool, key, build, state, log_base, log_seq, seen) VALUES (?,?,?,?,?,?,?)
		  ON CONFLICT(tool, key) DO UPDATE SET build = excluded.build, state = excluded.state, log_base = excluded.log_base,
		  log_seq = excluded.log_seq, seen = excluded.seen`, cp.Tool, cp.Key, t.build, cp.State, base, cp.Log.Seq(), now.UnixMilli()); err != nil {
			return err
		}
		if _, err := r.tx.ExecContext(r.ctx, `DELETE FROM tracker_log WHERE tool = ? AND key = ? AND (seq < ? OR seq >= ?)`,
			cp.Tool, cp.Key, base, from); err != nil {
			return err
		}
		for i, e := range items {
			if _, err := r.tx.ExecContext(r.ctx, `INSERT INTO tracker_log (tool, key, seq, role, name, origin, text) VALUES (?,?,?,?,?,?,?)`,
				cp.Tool, cp.Key, from+i, string(e.Role), e.Name, e.Origin, e.Text); err != nil {
				return err
			}
		}
		cp := cp
		r.after = append(r.after, func() { cp.Log.Saved(); t.held[id] = cp.State })
	}
	return nil
}

// DropCheckpoints drops the checkpoints of trackers no run has used since before.
func (r *Run) DropCheckpoints(before time.Time) error {
	if _, err := r.tx.ExecContext(r.ctx, `DELETE FROM tracker_log WHERE (tool, key) IN (SELECT tool, key FROM trackers WHERE seen < ?)`,
		before.UnixMilli()); err != nil {
		return err
	}
	_, err := r.tx.ExecContext(r.ctx, `DELETE FROM trackers WHERE seen < ?`, before.UnixMilli())
	return err
}
