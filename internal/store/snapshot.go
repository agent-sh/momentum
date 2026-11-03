package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"
)

// Path is the store's host filename.
func (s *Store) Path() string { return s.path }

// AsOf copies events in [since, until) and notes before until into a fresh store.
// No case, run or derived rows are copied. omitInPlace excludes mutable incident
// and radar rows for evaluations; live runs keep them as evidence.
func AsOf(ctx context.Context, src, dst string, since, until time.Time, omitInPlace bool) (err error) {
	if !since.Before(until) {
		return fmt.Errorf("snapshot window is empty")
	}
	if _, err := os.Stat(src); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(dst)
			os.Remove(dst + "-wal")
			os.Remove(dst + "-shm")
		}
	}()
	st, err := Open(dst)
	if err != nil {
		return err
	}
	defer st.Close()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	uri := (&url.URL{Scheme: "file", Path: src, RawQuery: "mode=ro"}).String()
	if _, err = conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, uri); err != nil {
		return fmt.Errorf("attach snapshot source: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE src`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cols := `id, source, source_key, ts, session, agent, harness, model, cwd, actor, kind, tool, summary, meta, ref, heuristic,
	  experiment, ingested`
	where := ""
	if omitInPlace {
		where = ` AND NOT (source = 'incidents' AND kind = 'incident') AND NOT (source = 'radar' AND kind = 'finding')`
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO main.events (`+cols+`) SELECT `+cols+` FROM src.events
	  WHERE ts >= ? AND ts < ?`+where, since.UnixMilli(), until.UnixMilli()); err != nil {
		return fmt.Errorf("copy snapshot events: %w", err)
	}
	ncols := `id, kind, title, detail, effect, links, source, importance, at, done`
	if _, err = tx.ExecContext(ctx, `INSERT INTO main.notes (`+ncols+`) SELECT id, kind, title, detail, effect, links,
	  source, importance, at, CASE WHEN done >= ? THEN 0 ELSE done END FROM src.notes WHERE at < ?`,
		until.UnixMilli(), until.UnixMilli()); err != nil {
		return fmt.Errorf("copy snapshot notes: %w", err)
	}
	return tx.Commit()
}
