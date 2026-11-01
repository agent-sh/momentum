package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// AsOf makes a store at dst that holds what src held as of a moment: its events from since up to until, and
// its notes from before until; no case, run or derived table of src's. An evaluation reads it in place of the
// store, so what came after the moment (a fix, its discussion, its lesson) is not there to be read.
func AsOf(ctx context.Context, src, dst string, since, until time.Time) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
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
	uri := "file:" + strings.ReplaceAll(src, "?", "%3f") + "?mode=ro"
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, uri); err != nil {
		return fmt.Errorf("attach %s: %w", src, err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE src`)
	cols := `id, source, source_key, ts, session, agent, harness, model, cwd, actor, kind, tool, summary, meta, ref, heuristic,
	  experiment, ingested`
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO main.events (`+cols+`) SELECT `+cols+` FROM src.events
	  WHERE ts >= ? AND ts < ?`, since.UnixMilli(), until.UnixMilli()); err != nil {
		return fmt.Errorf("copy the events: %w", err)
	}
	ncols := `id, kind, title, detail, effect, links, source, importance, at, done`
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO main.notes (`+ncols+`) SELECT id, kind, title, detail, effect, links,
	  source, importance, at, CASE WHEN done >= ? THEN 0 ELSE done END FROM src.notes WHERE at < ?`, until.UnixMilli(), until.UnixMilli()); err != nil {
		return fmt.Errorf("copy the notes: %w", err)
	}
	return nil
}
