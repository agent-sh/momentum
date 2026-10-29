package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Radar reads a findings scout's SQLite store read-only, in two streams:
//   - findings: what the detector wrote (kind "finding", actor detector). Its title and type, not its state:
//     state changes are other people's actions and arrive as feedback events.
//   - feedback: every action taken on a finding (like, dismiss, note, ...) as its own event, at the time it
//     was taken. Actions from a phone page carry a device and are the owner's; actions from the scout's
//     verification device are tagged as an experiment; actions without a device came from its CLI, which
//     agents and scripts run, so they are an agent's.
//
// The findings cursor is the newest `updated` seen, re-read with a margin: the scout's writers take their
// timestamp before waiting on its 15 s busy timeout, so commits can land out of order by that much.
// The feedback cursor is the AUTOINCREMENT id, which SQLite assigns under the write lock.
type Radar struct {
	Path string
	// TestDevice is the scout's verification device; its actions are tagged as an experiment.
	TestDevice string
}

func (r Radar) Name() string { return "radar" }

// radarMargin is twice the scout's busy timeout.
const radarMargin = 30.0

func (r Radar) Ingest(ctx context.Context, st *store.Store) (Result, error) {
	var res Result
	if _, err := os.Stat(r.Path); errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	db, err := sql.Open("sqlite", "file:"+r.Path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return res, err
	}
	defer db.Close()
	if err := r.findings(ctx, db, st, &res); err != nil {
		return res, err
	}
	return res, r.feedback(ctx, db, st, &res)
}

func (r Radar) findings(ctx context.Context, db *sql.DB, st *store.Store, res *Result) error {
	cur, err := st.Cursor(ctx, r.Name())
	if err != nil {
		return err
	}
	since, _ := strconv.ParseFloat(cur, 64)
	rows, err := db.QueryContext(ctx, `SELECT id, ts, type, key, title, updated FROM findings WHERE updated > ? ORDER BY updated`,
		since-radarMargin)
	if err != nil {
		return fmt.Errorf("radar findings: %w", err)
	}
	defer rows.Close()
	var evs []store.Event
	newest := since
	for rows.Next() {
		var id, typ, key, title string
		var ts, updated float64
		if err := rows.Scan(&id, &ts, &typ, &key, &title, &updated); err != nil {
			return err
		}
		res.Read++
		evs = append(evs, store.Event{
			Source:    r.Name(),
			SourceKey: "finding:" + id,
			TS:        unixFloat(ts),
			Actor:     store.ActorDetector,
			Kind:      "finding",
			Summary:   clip(fmt.Sprintf("[%s] %s", typ, title), 240),
			Meta:      meta(map[string]any{"key": key, "finding": id}),
			Ref:       fmt.Sprintf("%s:findings:%s", r.Path, id),
		})
		newest = math.Max(newest, updated)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	n, err := st.Put(ctx, evs)
	if err != nil {
		return err
	}
	res.New += n
	return st.SetCursor(ctx, r.Name(), strconv.FormatFloat(newest, 'g', -1, 64))
}

func (r Radar) feedback(ctx context.Context, db *sql.DB, st *store.Store, res *Result) error {
	var has int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='feedback'`).Scan(&has); err != nil {
		return fmt.Errorf("radar feedback: %w", err)
	}
	if has == 0 {
		return nil // an older scout without a feedback table
	}
	name := r.Name() + "-feedback"
	cur, err := st.Cursor(ctx, name)
	if err != nil {
		return err
	}
	last, _ := strconv.ParseInt(cur, 10, 64)
	rows, err := db.QueryContext(ctx, `SELECT id, finding_id, ts, kind, COALESCE(text, ''), COALESCE(meta, '') FROM feedback
	  WHERE id > ? ORDER BY id`, last)
	if err != nil {
		return fmt.Errorf("radar feedback: %w", err)
	}
	defer rows.Close()
	var evs []store.Event
	for rows.Next() {
		var id int64
		var finding, kind, text, metaText string
		var ts float64
		if err := rows.Scan(&id, &finding, &ts, &kind, &text, &metaText); err != nil {
			return err
		}
		res.Read++
		device := ""
		if metaText != "" {
			var m struct {
				Device string `json:"device"`
			}
			if json.Unmarshal([]byte(metaText), &m) == nil {
				device = m.Device
			}
		}
		ev := store.Event{
			Source:    name,
			SourceKey: strconv.FormatInt(id, 10),
			TS:        unixFloat(ts),
			Actor:     store.ActorAgent, // the scout's CLI: agents and scripts run it
			Kind:      "feedback:" + kind,
			Summary:   clip(text, 240),
			Meta:      meta(map[string]any{"finding": finding, "device": device}),
			Ref:       fmt.Sprintf("%s:feedback:%d", r.Path, id),
		}
		switch {
		case device != "" && device == r.TestDevice:
			ev.Experiment = device
		case device != "":
			ev.Actor = store.ActorOwner // a phone-page action carries the device it came from
		}
		evs = append(evs, ev)
		last = max(last, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	n, err := st.Put(ctx, evs)
	if err != nil {
		return err
	}
	res.New += n
	return st.SetCursor(ctx, name, strconv.FormatInt(last, 10))
}

func unixFloat(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9))
}
