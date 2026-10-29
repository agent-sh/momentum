package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Radar reads the findings scout's SQLite store read-only. A finding's state changes over time (new,
// saved, dismissed), so the cursor is the newest `updated` seen and changed rows are stored again.
type Radar struct{ Path string }

func (r Radar) Name() string { return "radar" }

func (r Radar) Ingest(ctx context.Context, st *store.Store) (int, error) {
	if _, err := os.Stat(r.Path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	db, err := sql.Open("sqlite", "file:"+r.Path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	cur, err := st.Cursor(ctx, r.Name())
	if err != nil {
		return 0, err
	}
	since, _ := strconv.ParseFloat(cur, 64)
	rows, err := db.QueryContext(ctx, `SELECT id, ts, type, key, title, state, updated FROM findings
	  WHERE updated > ? ORDER BY updated`, since)
	if err != nil {
		return 0, fmt.Errorf("radar findings: %w", err)
	}
	defer rows.Close()
	var evs []store.Event
	newest := since
	for rows.Next() {
		var id, typ, key, title, state string
		var ts, updated float64
		if err := rows.Scan(&id, &ts, &typ, &key, &title, &state, &updated); err != nil {
			return 0, err
		}
		evs = append(evs, store.Event{
			Source:    r.Name(),
			SourceKey: id,
			TS:        unixFloat(ts),
			Actor:     store.ActorDetector,
			Kind:      "finding",
			Summary:   clip(fmt.Sprintf("[%s|%s] %s", typ, state, title), 240),
			Ref:       fmt.Sprintf("%s:findings:%s key=%s", r.Path, id, key),
		})
		newest = math.Max(newest, updated)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	added, err := st.Put(ctx, evs)
	if err != nil {
		return 0, err
	}
	return added, st.SetCursor(ctx, r.Name(), strconv.FormatFloat(newest, 'f', 6, 64))
}

func unixFloat(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9))
}
