// Package store is momentum's local SQLite store: normalized events from every source, and the state that
// makes ingestion incremental. Most of it is derived: any source that reads a signal store can be dropped and
// read again (Rebuild). The events of `momentum sessions` are observations instead (Sessions): nothing can read
// them again, so they survive a schema change and Rebuild refuses them. Bulky material stays where it is; an
// event keeps a reference to it.
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
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Actor says who produced an event. The owner's own words, generated text and tool outcomes never mix:
// an owner event's Summary holds only text the owner typed, or nothing.
type Actor string

const (
	ActorOwner    Actor = "owner"    // the owner: his typed words, or an action only he takes
	ActorAgent    Actor = "agent"    // an agent's text or action, including tools and scripts agents run
	ActorTool     Actor = "tool"     // a tool's outcome (an error, a result)
	ActorHarness  Actor = "harness"  // the harness or a guard in it, or an outcome nobody can be credited with
	ActorDetector Actor = "detector" // a scout's or detector's own finding
	ActorUnknown  Actor = "unknown"  // a record kind the adapter does not know; counted and reported
)

// Event is the normalized envelope. Unknown values stay empty; nothing is invented.
type Event struct {
	Source     string // the source adapter, e.g. "mistakes"
	SourceKey  string // the source's own stable identity for this event
	TS         time.Time
	Session    string
	Agent      string // a subagent's id inside Session, when there is one
	Harness    string
	Model      string
	Cwd        string
	Actor      Actor
	Kind       string // tool-error, deny, finding, incident, feedback:dismiss, ...
	Tool       string
	Summary    string // short text for listing; for owner events only the owner's own words
	Meta       string // a JSON object with structured extras (a duration, a cause, a device), or ""
	Ref        string // where the full material is: path@offset, path:table:id
	Heuristic  bool   // the source flagged it as a guess (e.g. a correction found by pattern)
	Experiment string // "" for production; otherwise what tagged it (an experiment id, a test device)
}

// ID is stable across re-ingestion: the same source event always maps to the same row.
func (e Event) ID() string {
	h := sha256.Sum256([]byte(e.Source + "\x00" + e.SourceKey))
	return hex.EncodeToString(h[:12])
}

type Store struct {
	db   *sql.DB
	path string
}

// Sessions is the source of the events `momentum sessions` writes: each phase change of a live session, as the
// runs saw it. They are observations, not read from a signal store.
const Sessions = "sessions"

// ErrObserved means a source's events are observations, which cannot be read again.
var ErrObserved = errors.New("its events are observations, not read from a source, so they cannot be read again")

// querier is what a statement runs on: the database, or one transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, source_key TEXT NOT NULL, ts INTEGER NOT NULL,
  session TEXT, agent TEXT, harness TEXT, model TEXT, cwd TEXT, actor TEXT NOT NULL, kind TEXT NOT NULL,
  tool TEXT, summary TEXT, meta TEXT, ref TEXT, heuristic INTEGER NOT NULL DEFAULT 0,
  experiment TEXT NOT NULL DEFAULT '', ingested INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind, ts);
CREATE INDEX IF NOT EXISTS events_source ON events(source);
CREATE TABLE IF NOT EXISTS cursors (source TEXT PRIMARY KEY, position TEXT NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS seen_files (source TEXT NOT NULL, path TEXT NOT NULL, mtime INTEGER NOT NULL,
  size INTEGER NOT NULL, PRIMARY KEY (source, path));
CREATE TABLE IF NOT EXISTS skipped (source TEXT NOT NULL, ref TEXT NOT NULL, reason TEXT NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY (source, ref));
CREATE TABLE IF NOT EXISTS trackers (tool TEXT NOT NULL, key TEXT NOT NULL, build TEXT NOT NULL, state BLOB NOT NULL,
  log_base INTEGER NOT NULL, log_seq INTEGER NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY (tool, key));
CREATE TABLE IF NOT EXISTS tracker_log (tool TEXT NOT NULL, key TEXT NOT NULL, seq INTEGER NOT NULL, role TEXT NOT NULL,
  name TEXT NOT NULL, origin TEXT NOT NULL, text TEXT NOT NULL, PRIMARY KEY (tool, key, seq)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS events_session ON events(session, ts);
CREATE TABLE IF NOT EXISTS file_cursors (source TEXT NOT NULL, path TEXT NOT NULL, inode INTEGER NOT NULL, fp TEXT NOT NULL,
  compressed INTEGER NOT NULL, offset INTEGER NOT NULL, state BLOB, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
  updated INTEGER NOT NULL, PRIMARY KEY (source, path));
`

// BusyTimeout is how long a statement waits for another connection's write transaction before it fails, and how
// long a sessions run waits for the one before it.
const BusyTimeout = 10 * time.Second

// Open creates the store when missing. The store holds owner text, so the database is 0600 and its
// directory 0700, also when they already existed. SQLite gives the -wal and -shm files the database's mode.
// Write transactions take the lock up front (_txlock=immediate), so a concurrent writer waits for
// busy_timeout instead of failing at the read-to-write upgrade.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_txlock=immediate",
		path, BusyTimeout.Milliseconds()))
	if err != nil {
		return nil, err
	}
	// A new file turns to WAL as its first connection opens, and that step does not wait: a second process
	// opening the same new file at that moment is told the database is busy. It tries again.
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if err = migrate(db); err == nil || !busy(err) || time.Since(start) > BusyTimeout {
			break
		}
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// busy reports SQLITE_BUSY, or one of its extended codes.
func busy(err error) bool {
	var c interface{ Code() int }
	return errors.As(err, &c) && c.Code()&0xff == 5
}

// stateSchema holds what cannot be derived from the sources: it is created when missing and never dropped,
// so a change to one of these tables needs a real migration, not a version bump.
//   - session_stamps: when each live session entered its phase (sessions/state), kept between runs, with what a
//     later run needs to report the session once it is gone.
const stateSchema = `
CREATE TABLE IF NOT EXISTS session_stamps (key TEXT PRIMARY KEY, phase TEXT NOT NULL, since INTEGER NOT NULL,
  seq INTEGER NOT NULL, seen INTEGER NOT NULL, tool TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  auto INTEGER NOT NULL DEFAULT 0, ask TEXT NOT NULL DEFAULT '', ask_kind TEXT NOT NULL DEFAULT '');
`

// stampColumns are the columns session_stamps gained after it was first made. A store from before gets them,
// and keeps its stamps.
var stampColumns = [][2]string{{"tool", "TEXT NOT NULL DEFAULT ''"}, {"name", "TEXT NOT NULL DEFAULT ''"},
	{"auto", "INTEGER NOT NULL DEFAULT 0"}, {"ask", "TEXT NOT NULL DEFAULT ''"}, {"ask_kind", "TEXT NOT NULL DEFAULT ''"}}

// schemaVersion is bumped on every change to the derived schema (adding a table is not one: it is created when
// missing). Everything in it is derived from the sources, so a store at another version drops those tables and
// the next ingest reads every source again, and the next sessions run reads every transcript. Two things are not
// touched by that: the state tables (stateSchema), and the observed events (Sessions), which move to the new
// events table column by column (keepObserved).
//   - 3: the transcripts source's file cursors (file_cursors) and the index a session is read by
//     (events_session).
const schemaVersion = 3

// derivedTables are the tables of the derived schema, and derivedIndexes its indexes. The file cursors are
// derived: each says how far a source's events of one file go, with the reader's saved state there, so they go
// with those events. Dropped, the next ingest reads every transcript again from its start, a few minutes of CPU
// once; a transcript its harness deleted since is not read again, and its events are not stored again.
var (
	derivedTables  = []string{"events", "cursors", "seen_files", "skipped", "trackers", "tracker_log", "file_cursors"}
	derivedIndexes = []string{"events_ts", "events_kind", "events_source", "events_session"}
)

// checkpointColumns are the columns the schema gives the checkpoint tables. They are a cache of what transcripts
// hold, so tables with other columns (a build before this one made them) are dropped and made again, and the next
// sessions run reads every transcript once; nothing else is dropped.
var checkpointColumns = map[string][]string{
	"trackers":    {"tool", "key", "build", "state", "log_base", "log_seq", "seen"},
	"tracker_log": {"tool", "key", "seq", "role", "name", "origin", "text"},
}

// migrate brings the store to this schema. It checks with reads first, so opening a store that is up to date
// never waits for a writer. Otherwise it changes the store in one write transaction, which takes the lock up front
// (two first opens after an upgrade take turns), and checks again inside it.
func migrate(db *sql.DB) error {
	if ok, err := current(db); err != nil || ok {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(stateSchema); err != nil {
		return err
	}
	if err := addColumns(tx, "session_stamps", stampColumns); err != nil {
		return err
	}
	var v int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == schemaVersion {
		for t, want := range checkpointColumns {
			have, err := columns(tx, "main", t)
			if err != nil {
				return err
			}
			if len(have) > 0 && !slices.Equal(have.names(), want) {
				if _, err := tx.Exec(`DROP TABLE IF EXISTS trackers; DROP TABLE IF EXISTS tracker_log`); err != nil {
					return err
				}
				break
			}
		}
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return tx.Commit()
	}
	var hadEvents int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'events'`).Scan(&hadEvents); err != nil {
		return err
	}
	if hadEvents > 0 {
		if _, err := tx.Exec(`CREATE TEMP TABLE observed AS SELECT * FROM events WHERE source = ?`, Sessions); err != nil {
			return err
		}
	}
	for _, t := range derivedTables {
		if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if hadEvents > 0 {
		if err := keepObserved(tx); err != nil {
			return fmt.Errorf("keeping the %s events: %w", Sessions, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// current reports, with reads alone, whether the store already has this schema: its version, every table and
// index, the stamps' columns and the checkpoint tables' columns.
func current(q querier) (bool, error) {
	ctx := context.Background()
	var v int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		return false, err
	}
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table', 'index')`)
	if err != nil {
		return false, err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return false, err
		}
		have[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, n := range append(append([]string{"session_stamps"}, derivedTables...), derivedIndexes...) {
		if !have[n] {
			return false, nil
		}
	}
	stamps, err := columns(q, "main", "session_stamps")
	if err != nil {
		return false, err
	}
	for _, c := range stampColumns {
		if !stamps.has(c[0]) {
			return false, nil
		}
	}
	for t, want := range checkpointColumns {
		cols, err := columns(q, "main", t)
		if err != nil || !slices.Equal(cols.names(), want) {
			return false, err
		}
	}
	return true, nil
}

// keepObserved moves the observed events from the old events table (copied to temp.observed) into the new one,
// column by column: a column the old table lacked gets its default, else an empty value as Put writes, and
// ingested gets the time of the move.
func keepObserved(tx *sql.Tx) error {
	old, err := columns(tx, "temp", "observed")
	if err != nil {
		return err
	}
	cur, err := columns(tx, "main", "events")
	if err != nil {
		return err
	}
	var names, values []string
	for _, c := range cur {
		empty := "''"
		if strings.Contains(strings.ToUpper(c.typ), "INT") {
			empty = "0"
		}
		switch {
		case old.has(c.name):
			names, values = append(names, c.name), append(values, "COALESCE("+c.name+", "+empty+")")
		case c.name == "ingested":
			names, values = append(names, c.name), append(values, fmt.Sprint(time.Now().UnixMilli()))
		case !c.defaulted:
			names, values = append(names, c.name), append(values, empty)
		}
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO events (` + strings.Join(names, ", ") + `) SELECT ` +
		strings.Join(values, ", ") + ` FROM temp.observed`); err != nil {
		return err
	}
	_, err = tx.Exec(`DROP TABLE temp.observed`)
	return err
}

type column struct {
	name, typ string
	defaulted bool
}

type columnList []column

// columns lists a table's columns in order.
func columns(q querier, schema, table string) (columnList, error) {
	rows, err := q.QueryContext(context.Background(), `SELECT name, type, dflt_value FROM pragma_table_info(?, ?)`, table, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out columnList
	for rows.Next() {
		var c column
		var dflt sql.NullString
		if err := rows.Scan(&c.name, &c.typ, &dflt); err != nil {
			return nil, err
		}
		c.defaulted = dflt.Valid
		out = append(out, c)
	}
	return out, rows.Err()
}

func (cs columnList) names() []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}

func (cs columnList) has(name string) bool {
	for _, c := range cs {
		if c.name == name {
			return true
		}
	}
	return false
}

// addColumns adds to a state table the columns it lacks.
func addColumns(tx *sql.Tx, table string, want [][2]string) error {
	have, err := columns(tx, "main", table)
	if err != nil {
		return err
	}
	for _, c := range want {
		if !have.has(c[0]) {
			if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + c[0] + ` ` + c[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// Put inserts events, or refreshes every derived column of an event already stored, in one transaction.
// It returns how many were new.
func (s *Store) Put(ctx context.Context, evs []Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added, err := put(ctx, tx, evs)
	if err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

// put stores events on q with statements prepared once: a transcript adds thousands per transaction.
func put(ctx context.Context, tx querier, evs []Event) (int, error) {
	seenStmt, err := tx.PrepareContext(ctx, `SELECT COUNT(*) FROM events WHERE id=?`)
	if err != nil {
		return 0, err
	}
	defer seenStmt.Close()
	putStmt, err := tx.PrepareContext(ctx, `INSERT INTO events (id, source, source_key, ts, session, agent, harness, model,
	  cwd, actor, kind, tool, summary, meta, ref, heuristic, experiment, ingested)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(id) DO UPDATE SET ts=excluded.ts, session=excluded.session, agent=excluded.agent,
	    harness=excluded.harness, model=excluded.model, cwd=excluded.cwd, actor=excluded.actor, kind=excluded.kind,
	    tool=excluded.tool, summary=excluded.summary, meta=excluded.meta, ref=excluded.ref,
	    heuristic=excluded.heuristic, experiment=excluded.experiment`)
	if err != nil {
		return 0, err
	}
	defer putStmt.Close()
	now := time.Now().UnixMilli()
	added := 0
	for _, e := range evs {
		if e.Source == "" || e.SourceKey == "" || e.Kind == "" || e.Actor == "" {
			return 0, fmt.Errorf("event from %q lacks source, key, kind or actor", e.Source)
		}
		id := e.ID()
		var seen int
		if err := seenStmt.QueryRowContext(ctx, id).Scan(&seen); err != nil {
			return 0, err
		}
		if _, err := putStmt.ExecContext(ctx, id, e.Source, e.SourceKey, e.TS.UnixMilli(), e.Session, e.Agent, e.Harness,
			e.Model, e.Cwd, string(e.Actor), e.Kind, e.Tool, e.Summary, e.Meta, e.Ref, e.Heuristic, e.Experiment, now); err != nil {
			return 0, err
		}
		if seen == 0 {
			added++
		}
	}
	return added, nil
}

// FileCursor is where a source stopped in one file of many, and what it needs to tell the same file from a
// new one: its inode and a fingerprint of its first line (OpenTelemetry's filelog rule), whether a compressed
// copy held it, and the size and modification time (ns) it had when it was read to the end (0 while a read
// is under way). State is what the file's reader kept at Offset, or nil: for transcripts, tacit's saved
// reader state, which holds transcript lines unredacted and is never printed or sent. Path names the file,
// or another record the source reads in parts.
type FileCursor struct {
	Source, Path string
	Inode        uint64
	FP           string
	Compressed   bool
	Offset       int64
	State        []byte
	Size, MTime  int64
}

// FileCursors are a source's saved file cursors, by path.
func (s *Store) FileCursors(ctx context.Context, source string) (map[string]FileCursor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, inode, fp, compressed, offset, state, size, mtime FROM file_cursors
	  WHERE source=?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileCursor{}
	for rows.Next() {
		c := FileCursor{Source: source}
		var ino int64
		if err := rows.Scan(&c.Path, &ino, &c.FP, &c.Compressed, &c.Offset, &c.State, &c.Size, &c.MTime); err != nil {
			return nil, err
		}
		c.Inode = uint64(ino)
		out[c.Path] = c
	}
	return out, rows.Err()
}

// FileWrite is a part of a file read: its events and the file's cursor after them.
type FileWrite struct {
	Events []Event
	Cursor FileCursor
}

// PutFiles stores parts of files in one transaction, each part's events with the cursor after them, so a
// cursor never runs ahead of what was stored. It returns how many events were new.
func (s *Store) PutFiles(ctx context.Context, ws []FileWrite) (int, error) {
	if len(ws) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var evs []Event
	for _, w := range ws {
		evs = append(evs, w.Events...)
	}
	added, err := put(ctx, tx, evs)
	if err != nil {
		return 0, err
	}
	cur, err := tx.PrepareContext(ctx, `INSERT INTO file_cursors (source, path, inode, fp, compressed, offset, state,
	  size, mtime, updated) VALUES (?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(source, path) DO UPDATE SET inode=excluded.inode, fp=excluded.fp, compressed=excluded.compressed,
	    offset=excluded.offset, state=excluded.state, size=excluded.size, mtime=excluded.mtime, updated=excluded.updated`)
	if err != nil {
		return 0, err
	}
	defer cur.Close()
	now := time.Now().UnixMilli()
	for _, w := range ws {
		c := w.Cursor
		if _, err := cur.ExecContext(ctx, c.Source, c.Path, int64(c.Inode), c.FP, c.Compressed, c.Offset, c.State, c.Size,
			c.MTime, now); err != nil {
			return 0, err
		}
	}
	return added, tx.Commit()
}

// DropFileCursors forgets the cursors of files a source no longer finds. Their events stay.
func (s *Store) DropFileCursors(ctx context.Context, source string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_cursors WHERE source=? AND path=?`, source, p); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// FileSeen returns the modification time (ns) and size a source recorded for a file it parsed.
func (s *Store) FileSeen(ctx context.Context, source, path string) (mtime, size int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT mtime, size FROM seen_files WHERE source=? AND path=?`, source, path).Scan(&mtime, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	return mtime, size, err == nil, err
}

// MarkFile records that a source parsed a file at this modification time and size.
func (s *Store) MarkFile(ctx context.Context, source, path string, mtime, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO seen_files (source, path, mtime, size) VALUES (?,?,?,?)
	  ON CONFLICT(source, path) DO UPDATE SET mtime=excluded.mtime, size=excluded.size`, source, path, mtime, size)
	return err
}

// Skip records input a source could not parse, so a format change is visible and a rebuild can retry it.
func (s *Store) Skip(ctx context.Context, source, ref, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO skipped (source, ref, reason, at) VALUES (?,?,?,?)
	  ON CONFLICT(source, ref) DO UPDATE SET reason=excluded.reason, at=excluded.at`, source, ref, reason, time.Now().UnixMilli())
	return err
}

// Skipped counts the unparsed records a source has.
func (s *Store) Skipped(ctx context.Context, source string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skipped WHERE source=?`, source).Scan(&n)
	return n, err
}

// Rebuild drops everything a source contributed, so the next ingest reads it again from the start.
// Sources share one state space by name prefix: "radar" also drops "radar-feedback". The observed events
// (Sessions) cannot be read again, so Rebuild refuses them with ErrObserved.
func (s *Store) Rebuild(ctx context.Context, source string) error {
	if source == Sessions {
		return fmt.Errorf("%s: %w", source, ErrObserved)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	like := source + "-%"
	for _, q := range []string{
		`DELETE FROM events WHERE source=? OR source LIKE ?`,
		`DELETE FROM cursors WHERE source=? OR source LIKE ?`,
		`DELETE FROM seen_files WHERE source=? OR source LIKE ?`,
		`DELETE FROM skipped WHERE source=? OR source LIKE ?`,
		`DELETE FROM file_cursors WHERE source=? OR source LIKE ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, source, like); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Query selects events newest first, or, with Session (an id or its first characters), one session's events
// oldest first: a session reads as what happened, in order. Events of one moment keep their order in the
// source (SourceKey). Until, when set, is the end of the window.
type Query struct {
	Since   time.Time
	Kinds   []string
	Source  string
	Session string
	Until   time.Time
	Limit   int
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
	if !q.Until.IsZero() {
		where, args = append(where, "ts < ?"), append(args, q.Until.UnixMilli())
	}
	order := "ts DESC, source_key DESC"
	if q.Session != "" {
		// An id or its first characters, as a range the session index serves.
		where, args = append(where, "session >= ? AND session < ?"), append(args, q.Session, q.Session+"\uffff")
		order = "ts, source_key"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`,
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
			&e.Kind, &e.Tool, &e.Summary, &e.Meta, &e.Ref, &e.Heuristic, &e.Experiment); err != nil {
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
