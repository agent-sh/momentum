// Package store is momentum's local SQLite store: normalized events from every source, cases, and the
// cursors that make ingestion incremental. Bulky material stays where it is; an event keeps a reference.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Actor says who produced an event. The owner's own words, generated text and tool outcomes are never mixed.
type Actor string

const (
	ActorOwner      Actor = "owner"      // the owner's own typed words
	ActorAgent      Actor = "agent"      // an agent's text or action
	ActorTool       Actor = "tool"       // a tool's outcome (an error, a result)
	ActorHarness    Actor = "harness"    // the harness or a guard in it (a deny, a hold)
	ActorDetector   Actor = "detector"   // a scout or detector's finding
	ActorExperiment Actor = "experiment" // anything an experiment produced; never evidence of the owner's preferences
)

// Event is the normalized envelope. Unknown values stay empty; nothing is invented.
type Event struct {
	Source    string // the source adapter, e.g. "mistakes"
	SourceKey string // the source's own stable identity for this event
	TS        time.Time
	Session   string
	Agent     string // a subagent's id inside Session, when there is one
	Harness   string
	Model     string
	Cwd       string
	Actor     Actor
	Kind      string // tool-error, deny, finding, incident, ...
	Tool      string
	Summary   string // short text for listing; the full material stays behind Ref
	Ref       string // where the full material is: path#line, db:table:id
	Heuristic bool   // the source flagged it as a guess (e.g. a correction found by pattern)
}

// ID is stable across re-ingestion: the same source event always maps to the same row.
func (e Event) ID() string {
	h := sha256.Sum256([]byte(e.Source + "\x00" + e.SourceKey))
	return hex.EncodeToString(h[:12])
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, source_key TEXT NOT NULL, ts INTEGER NOT NULL,
  session TEXT, agent TEXT, harness TEXT, model TEXT, cwd TEXT, actor TEXT NOT NULL, kind TEXT NOT NULL,
  tool TEXT, summary TEXT, ref TEXT, heuristic INTEGER NOT NULL DEFAULT 0, ingested INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind, ts);
CREATE TABLE IF NOT EXISTS cursors (source TEXT PRIMARY KEY, position TEXT NOT NULL, updated INTEGER NOT NULL);
`

// Open creates the store and its directory when missing.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Put inserts or refreshes events in one transaction and returns how many were new.
func (s *Store) Put(ctx context.Context, evs []Event) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	added := 0
	for _, e := range evs {
		if e.Source == "" || e.SourceKey == "" || e.Kind == "" || e.Actor == "" {
			return 0, fmt.Errorf("event from %q lacks source, key, kind or actor", e.Source)
		}
		var seen int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE id=?`, e.ID()).Scan(&seen); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events
		  (id, source, source_key, ts, session, agent, harness, model, cwd, actor, kind, tool, summary, ref, heuristic, ingested)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		  ON CONFLICT(id) DO UPDATE SET summary=excluded.summary, ref=excluded.ref, kind=excluded.kind`,
			e.ID(), e.Source, e.SourceKey, e.TS.UnixMilli(), e.Session, e.Agent, e.Harness, e.Model, e.Cwd,
			string(e.Actor), e.Kind, e.Tool, e.Summary, e.Ref, e.Heuristic, now); err != nil {
			return 0, err
		}
		if seen == 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// Cursor returns a source's saved position, "" when it has none.
func (s *Store) Cursor(ctx context.Context, source string) (string, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT position FROM cursors WHERE source=?`, source).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

// SetCursor saves a source's position after its events are stored.
func (s *Store) SetCursor(ctx context.Context, source, position string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cursors (source, position, updated) VALUES (?,?,?)
	  ON CONFLICT(source) DO UPDATE SET position=excluded.position, updated=excluded.updated`,
		source, position, time.Now().UnixMilli())
	return err
}

// Query selects events newest first.
type Query struct {
	Since  time.Time
	Kinds  []string
	Source string
	Limit  int
}

func (s *Store) Events(ctx context.Context, q Query) ([]Event, error) {
	where, args := []string{"ts >= ?"}, []any{q.Since.UnixMilli()}
	if len(q.Kinds) > 0 {
		where = append(where, "kind IN (?"+strings.Repeat(",?", len(q.Kinds)-1)+")")
		for _, k := range q.Kinds {
			args = append(args, k)
		}
	}
	if q.Source != "" {
		where, args = append(where, "source = ?"), append(args, q.Source)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, ref, heuristic FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY ts DESC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		var actor string
		if err := rows.Scan(&e.Source, &e.SourceKey, &ts, &e.Session, &e.Agent, &e.Harness, &e.Model, &e.Cwd, &actor,
			&e.Kind, &e.Tool, &e.Summary, &e.Ref, &e.Heuristic); err != nil {
			return nil, err
		}
		e.TS, e.Actor = time.UnixMilli(ts), Actor(actor)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Counts returns events per source and kind since a time, for the status view.
func (s *Store) Counts(ctx context.Context, since time.Time) (map[string]map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, kind, COUNT(*) FROM events WHERE ts >= ? GROUP BY source, kind`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var src, kind string
		var n int
		if err := rows.Scan(&src, &kind, &n); err != nil {
			return nil, err
		}
		if out[src] == nil {
			out[src] = map[string]int{}
		}
		out[src][kind] = n
	}
	return out, rows.Err()
}
