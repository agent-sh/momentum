// Package store is momentum's local SQLite store: normalized events from every source, the notes its report
// shows, and the state that makes ingestion incremental. The events are kept: a schema change migrates them and
// never drops them, since a harness deletes its transcripts after a while and the observations of `momentum
// sessions` are nowhere else. The notes are kept too: nothing derives them.
// What the sources and the session trackers derive from what they read (where each stopped, their checkpoints)
// is a cache: a schema change drops it, and the next run reads again. A source that reads a signal store can be
// dropped and read again (Rebuild); the transcripts only when forced, the observations never. Bulky material
// stays where it is; an event keeps a reference to it.
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

// Transcripts is the source of the agents' session history. A harness deletes its transcripts after a while
// (Claude Code after about 30 days), so its events can be all that is left of them.
const Transcripts = "transcripts"

// ErrKept means a source's events may be all that is left of what it read: Rebuild drops them only when forced.
var ErrKept = errors.New("its events are kept")

// Incidents is the source of an incident detector's incidents and their sightings (source.Incidents).
const Incidents = "incidents"

// Harnesses are the names events give the harnesses, one spelling in every source and in the sessions layer:
// tacit's reader's (claude, codex, pi, hermes), and opencode, which only the sessions layer reads.
var Harnesses = []string{"claude", "codex", "pi", "opencode", "hermes"}

// querier is what a statement runs on: the database, or one transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// The store's tables are of two kinds.
//
// Kept tables hold what cannot be read again: every event, when each live session entered its phase, the notes
// (notes.go), what the capacity runs observed (capacity.go), the cases (cases.go), and the loop's runs (loop.go). A schema change migrates them and never drops
// them. A kept table that is missing is made with the columns it was first made with (keptTables), it gains each
// later column it lacks (keptColumns), and its rows change only in a step or by its triggers (keptTriggers).
//
// Derived tables hold what the sources and the session trackers derived from what they read: where each source
// stopped (with tacit's saved reader state), the files it parsed, what it could not parse, and the trackers'
// checkpoints. A step that reads again drops them, so the next ingest reads every source again from its start, and
// the next sessions run reads every transcript once (PutFiles says what reading again keeps).

// keptTables makes the kept tables as they were first made, and schema_steps, the steps a store has taken. A new
// kept table changes no row, so it takes no step: a store from before it gains it at the next open, and a build
// from before it, which never reads it, keeps working on the store (parallel change).
const keptTables = `
CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, source TEXT NOT NULL, source_key TEXT NOT NULL,
  ts INTEGER NOT NULL, actor TEXT NOT NULL, kind TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS session_stamps (key TEXT PRIMARY KEY, phase TEXT NOT NULL, since INTEGER NOT NULL,
  seq INTEGER NOT NULL, seen INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS schema_steps (version INTEGER PRIMARY KEY, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS notes (id TEXT PRIMARY KEY, kind TEXT NOT NULL, title TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '', effect TEXT NOT NULL DEFAULT '', links TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL DEFAULT '', importance INTEGER NOT NULL DEFAULT 0, at INTEGER NOT NULL,
  done INTEGER NOT NULL DEFAULT 0);
` + capacityTables + caseTables + loopTables

// keptColumns are the columns each kept table gained after it was first made, in order. A table that lacks one
// gains it and keeps its rows: SQLite's ADD COLUMN writes no row, so it costs the same on any table. The next
// column is one more entry here. A NOT NULL column needs a default other than NULL, and an added column cannot be
// a key. The events from before it hold its default; for the sources to fill it in the events whose transcripts
// are still there, add a step that reads again as well.
var keptColumns = []struct {
	table   string
	columns [][2]string
}{
	{"events", [][2]string{{"session", "TEXT NOT NULL DEFAULT ''"}, {"agent", "TEXT NOT NULL DEFAULT ''"},
		{"harness", "TEXT NOT NULL DEFAULT ''"}, {"model", "TEXT NOT NULL DEFAULT ''"}, {"cwd", "TEXT NOT NULL DEFAULT ''"},
		{"tool", "TEXT NOT NULL DEFAULT ''"}, {"summary", "TEXT NOT NULL DEFAULT ''"}, {"meta", "TEXT NOT NULL DEFAULT ''"},
		{"ref", "TEXT NOT NULL DEFAULT ''"}, {"heuristic", "INTEGER NOT NULL DEFAULT 0"},
		{"experiment", "TEXT NOT NULL DEFAULT ''"}, {"ingested", "INTEGER NOT NULL DEFAULT 0"}}},
	{"session_stamps", [][2]string{{"tool", "TEXT NOT NULL DEFAULT ''"}, {"name", "TEXT NOT NULL DEFAULT ''"},
		{"auto", "INTEGER NOT NULL DEFAULT 0"}, {"ask", "TEXT NOT NULL DEFAULT ''"}, {"ask_kind", "TEXT NOT NULL DEFAULT ''"}}},
	{"capacity_runs", [][2]string{{"blind", "INTEGER NOT NULL DEFAULT 0"}}},
}

// keptIndexes are the events' indexes. events_key finds a record's events by the prefix of their keys (PutFiles)
// and a source's events, as events_source did.
const keptIndexes = `
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind, ts);
CREATE INDEX IF NOT EXISTS events_session ON events(session, ts);
CREATE INDEX IF NOT EXISTS events_key ON events(source, source_key);
`

var keptIndexNames = slices.Concat([]string{"events_ts", "events_kind", "events_session", "events_key"}, caseIndexNames, loopIndexNames)

// keptTriggers give Claude Code its one name as an event is written, by whatever build writes it: a build from
// before the events were kept writes claude-code, also while this build has the store open.
const keptTriggers = `
CREATE TRIGGER IF NOT EXISTS events_harness_insert AFTER INSERT ON events WHEN NEW.harness = 'claude-code'
BEGIN UPDATE events SET harness = 'claude' WHERE rowid = NEW.rowid; END;
CREATE TRIGGER IF NOT EXISTS events_harness_update AFTER UPDATE OF harness ON events WHEN NEW.harness = 'claude-code'
BEGIN UPDATE events SET harness = 'claude' WHERE rowid = NEW.rowid; END;
`

var keptTriggerNames = []string{"events_harness_insert", "events_harness_update"}

// derivedSchema makes the derived tables that are missing.
const derivedSchema = `
CREATE TABLE IF NOT EXISTS cursors (source TEXT PRIMARY KEY, position TEXT NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS seen_files (source TEXT NOT NULL, path TEXT NOT NULL, mtime INTEGER NOT NULL,
  size INTEGER NOT NULL, PRIMARY KEY (source, path));
CREATE TABLE IF NOT EXISTS skipped (source TEXT NOT NULL, ref TEXT NOT NULL, reason TEXT NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY (source, ref));
CREATE TABLE IF NOT EXISTS trackers (tool TEXT NOT NULL, key TEXT NOT NULL, build TEXT NOT NULL, state BLOB NOT NULL,
  log_base INTEGER NOT NULL, log_seq INTEGER NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY (tool, key));
CREATE TABLE IF NOT EXISTS tracker_log (tool TEXT NOT NULL, key TEXT NOT NULL, seq INTEGER NOT NULL, role TEXT NOT NULL,
  name TEXT NOT NULL, origin TEXT NOT NULL, text TEXT NOT NULL, PRIMARY KEY (tool, key, seq)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS file_cursors (source TEXT NOT NULL, path TEXT NOT NULL, inode INTEGER NOT NULL, fp TEXT NOT NULL,
  compressed INTEGER NOT NULL, offset INTEGER NOT NULL, state BLOB, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
  updated INTEGER NOT NULL, PRIMARY KEY (source, path));
`

// derivedTables are the derived tables. The file cursors say how far a source's events of one file go, with the
// reader's saved state there: dropped, the next ingest reads every transcript again from its start, a few
// minutes of CPU once, and a transcript its harness deleted since is not read again: its events stay as they are.
var derivedTables = []string{"cursors", "seen_files", "skipped", "trackers", "tracker_log", "file_cursors"}

// step is a change to the schema, run once, in order, in the transaction that opens a store from before it, and
// recorded in schema_steps; sql, when there is one, changes the kept tables' rows or indexes, on a store at the
// version before it. A step that rereads changes what the sources derive from what they read (a column they fill,
// a fix in how they read): it drops the derived tables, and the next ingest reads every source again from its
// start. Reading again keeps what is gone and stores nothing twice of a transcript only ever appended to, but not
// of one rewritten in place with lines that moved, and it credits each item as tacit does when it reads again
// (PutFiles). So a step rereads only when it must, and a store that took several steps reads once.
type step struct {
	version int
	sql     string
	reread  bool
}

// steps are the schema's changes; their versions go on from legacyVersion. The next change to a kept table's rows
// is one more step here.
var steps = []step{
	// A store from before version 3 comes from builds that read every source again on a version change, and its
	// derived tables are theirs: its sources are read again, as those builds would have.
	{version: 3, reread: true},
	// The events are kept (keptTables, keptColumns), and a record's events are found by their keys (events_key,
	// which serves what events_source served). Nothing is read again. Every harness has one name, tacit's
	// reader's: Claude Code's events are renamed claude, and keptTriggers name those written later.
	{version: 4, sql: `UPDATE events SET harness = 'claude' WHERE harness = 'claude-code'`},
}

// repairs run in every migration, after the steps. They put right what a build from before the events were kept
// does to a store migrated since: it makes events_source again, an index no build since reads, which costs every
// write. current reports a store that holds events_source as not current, so the next open after such a build
// runs them. What that build writes, keptTriggers name.
const repairs = `DROP INDEX IF EXISTS events_source`

// legacyVersion is the last version user_version counted. A build of that version drops every derived table, the
// events among them, when user_version is another number, so user_version stays at it: such a build, opening a
// store migrated since, finds its own version and drops nothing. A build from before it (user_version 2) drops every
// event but the sessions' on any other number, so it must not open a store again. The versions since are the steps in schema_steps.
const legacyVersion = 3

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

// ErrNotCurrent means a store is not this build's schema. The next open of a build that writes migrates it (a
// sessions run, an ingest, a note); a store a newer build migrated is refused by every open of this build.
var ErrNotCurrent = errors.New("the store is not this build's schema")

// OpenReadOnly opens a store to read it as it is: it makes no store, migrates nothing, writes no row and takes no
// lock. SQLite may make the -wal and -shm files a reader of a WAL store needs, with the store's mode; the next writer
// removes them. A missing store is an error, and so is one that is not this build's schema (ErrNotCurrent), which
// this build would misread.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no store to read: %w; a sessions run, an ingest or a note makes it", err)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", path, BusyTimeout.Milliseconds()))
	if err != nil {
		return nil, err
	}
	ok, err := current(db)
	if err == nil && !ok {
		switch v, verr := version(db); {
		case verr != nil:
			err = verr
		case v > latest():
			err = fmt.Errorf("%w: its schema is version %d, newer than this build's %d", ErrNotCurrent, v, latest())
		default:
			err = fmt.Errorf("%w yet: the next sessions run, ingest or note of this build migrates it", ErrNotCurrent)
		}
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// busy reports SQLITE_BUSY, or one of its extended codes.
func busy(err error) bool {
	var c interface{ Code() int }
	return errors.As(err, &c) && c.Code()&0xff == 5
}

// keptTableNames are the tables keptTables makes.
var keptTableNames = slices.Concat([]string{"events", "session_stamps", "schema_steps", "notes"}, capacityTableNames, caseTableNames,
	loopTableNames)

// checkpointColumns are the columns the schema gives the checkpoint tables. They are a cache of what transcripts
// hold, so tables with other columns (a build before this one made them) are dropped and made again, and the next
// sessions run reads every transcript once; nothing else is dropped.
var checkpointColumns = map[string][]string{
	"trackers":    {"tool", "key", "build", "state", "log_base", "log_seq", "seen"},
	"tracker_log": {"tool", "key", "seq", "role", "name", "origin", "text"},
}

// latest is this build's schema version: its last step's.
func latest() int { return steps[len(steps)-1].version }

// version is a store's schema version: the last step it took, or before any, its user_version. A user_version
// below legacyVersion is the version, whatever schema_steps holds: a build from before version 3 set it, and that
// build drops every event but the sessions' and keeps the tables it does not know, so the steps since run again.
func version(q querier) (int, error) {
	ctx := context.Background()
	var v, n int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil || v < legacyVersion {
		return v, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_steps'`).Scan(&n); err != nil || n == 0 {
		return v, err
	}
	var last sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_steps`).Scan(&last); err != nil {
		return 0, err
	}
	return max(v, int(last.Int64)), nil
}

// migrate brings the store to this schema. It checks with reads first, so opening a store that is up to date
// never waits for a writer. Otherwise it changes the store in one write transaction, which takes the lock up front
// (two first opens after an upgrade take turns), and reads the version again inside it: the kept tables gain what
// they lack, the steps the store has not taken run (after the derived tables are dropped, when one of them
// rereads), the repairs run, and what is missing is made. A store at a version newer than this build's is
// refused, unchanged: this build does not know its steps.
func migrate(db *sql.DB) error {
	if ok, err := current(db); err != nil || ok {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := version(tx)
	if err != nil {
		return err
	}
	if v > latest() {
		return fmt.Errorf("its schema is version %d, newer than this build's %d: open it with a newer build", v, latest())
	}
	if _, err := tx.Exec(keptTables); err != nil {
		return err
	}
	for _, k := range keptColumns {
		if err := addColumns(tx, k.table, k.columns); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(keptIndexes + caseIndexes + loopIndexes + keptTriggers); err != nil {
		return err
	}
	var pending []step
	reread := false
	for _, s := range steps {
		if s.version > v {
			pending, reread = append(pending, s), reread || s.reread
		}
	}
	if reread {
		for _, t := range derivedTables {
			if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
				return err
			}
		}
	}
	now := time.Now().UnixMilli()
	for _, s := range pending {
		if s.sql != "" {
			if _, err := tx.Exec(s.sql); err != nil {
				return fmt.Errorf("step %d: %w", s.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_steps (version, at) VALUES (?, ?)`, s.version, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(repairs); err != nil {
		return fmt.Errorf("repairs: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, legacyVersion)); err != nil {
		return err
	}
	for t, want := range checkpointColumns {
		have, err := columns(tx, t)
		if err != nil {
			return err
		}
		if len(have) > 0 && !slices.Equal(have, want) {
			if _, err := tx.Exec(`DROP TABLE IF EXISTS trackers; DROP TABLE IF EXISTS tracker_log`); err != nil {
				return err
			}
			break
		}
	}
	if _, err := tx.Exec(derivedSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// current reports, with reads alone, whether the store already has this schema: user_version, the version of
// its steps, every table and index, and the columns of the kept tables and the checkpoint tables. A store that
// holds events_source is not current: a build from before the events were kept opened it, and the repairs put
// right what it did.
func current(q querier) (bool, error) {
	ctx := context.Background()
	var uv int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&uv); err != nil || uv != legacyVersion {
		return false, err
	}
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table', 'index', 'trigger')`)
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
	for _, n := range slices.Concat(keptTableNames, derivedTables, keptIndexNames, keptTriggerNames) {
		if !have[n] {
			return false, nil
		}
	}
	if have["events_source"] {
		return false, nil
	}
	if v, err := version(q); err != nil || v != latest() {
		return false, err
	}
	for _, k := range keptColumns {
		cols, err := columns(q, k.table)
		if err != nil {
			return false, err
		}
		for _, c := range k.columns {
			if !slices.Contains(cols, c[0]) {
				return false, nil
			}
		}
	}
	for t, want := range checkpointColumns {
		cols, err := columns(q, t)
		if err != nil || !slices.Equal(cols, want) {
			return false, err
		}
	}
	return true, nil
}

// columns lists a table's columns in order.
func columns(q querier, table string) ([]string, error) {
	rows, err := q.QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// addColumns adds to a kept table the columns it lacks.
func addColumns(tx *sql.Tx, table string, want [][2]string) error {
	have, err := columns(tx, table)
	if err != nil {
		return err
	}
	for _, c := range want {
		if !slices.Contains(have, c[0]) {
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

// FileWrite is a part of a file read: its events, the file's cursor after them, and the records it read. Places
// are the prefixes of their keys, one per record (a transcript's line, a Hermes row): what a record's events'
// keys start with, and no other record's do.
type FileWrite struct {
	Events []Event
	Cursor FileCursor
	Places []string
}

// PutFiles stores parts of files in one transaction, each part's events with the cursor after them, so a
// cursor never runs ahead of what was stored. A record read again holds what this reading of it stores, and
// nothing of an earlier one: an event stored under one of a part's places that the part does not hold is dropped
// (a reader that now splits a turn otherwise numbers its events otherwise). Places name records by their offset,
// so this holds while a record stays where it was: a file rewritten in place with lines that moved stores them
// again at their new places, and a record that now gives no item keeps the events it had. It returns how many
// events were new, and how many were dropped.
func (s *Store) PutFiles(ctx context.Context, ws []FileWrite) (added, dropped int, err error) {
	if len(ws) == 0 {
		return 0, 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	held, err := tx.PrepareContext(ctx, `SELECT id FROM events WHERE source = ? AND source_key >= ? AND source_key < ?`)
	if err != nil {
		return 0, 0, err
	}
	defer held.Close()
	drop, err := tx.PrepareContext(ctx, `DELETE FROM events WHERE id = ?`)
	if err != nil {
		return 0, 0, err
	}
	defer drop.Close()
	var evs []Event
	for _, w := range ws {
		stale := map[string]bool{}
		for _, p := range w.Places {
			if p == "" {
				continue
			}
			// The keys that start with p, by the index on them: from p up to p with its last byte one higher
			// (keys are text: no byte of theirs is 0xff).
			rows, err := held.QueryContext(ctx, w.Cursor.Source, p, p[:len(p)-1]+string([]byte{p[len(p)-1] + 1}))
			if err != nil {
				return 0, 0, err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return 0, 0, err
				}
				stale[id] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, 0, err
			}
		}
		for _, e := range w.Events {
			delete(stale, e.ID())
		}
		for id := range stale {
			if _, err := drop.ExecContext(ctx, id); err != nil {
				return 0, 0, err
			}
			dropped++
		}
		evs = append(evs, w.Events...)
	}
	if added, err = put(ctx, tx, evs); err != nil {
		return 0, 0, err
	}
	cur, err := tx.PrepareContext(ctx, `INSERT INTO file_cursors (source, path, inode, fp, compressed, offset, state,
	  size, mtime, updated) VALUES (?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(source, path) DO UPDATE SET inode=excluded.inode, fp=excluded.fp, compressed=excluded.compressed,
	    offset=excluded.offset, state=excluded.state, size=excluded.size, mtime=excluded.mtime, updated=excluded.updated`)
	if err != nil {
		return 0, 0, err
	}
	defer cur.Close()
	now := time.Now().UnixMilli()
	for _, w := range ws {
		c := w.Cursor
		if _, err := cur.ExecContext(ctx, c.Source, c.Path, int64(c.Inode), c.FP, c.Compressed, c.Offset, c.State, c.Size,
			c.MTime, now); err != nil {
			return 0, 0, err
		}
	}
	return added, dropped, tx.Commit()
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

// Rebuild drops everything a source contributed, so the next ingest reads it again from the start, and returns
// how many events it dropped. Sources share one state space by name prefix: "radar" also drops "radar-feedback".
// The observed events (Sessions, Supervise, Capacity) cannot be read again, so Rebuild refuses them with
// ErrObserved. The transcripts' events can be all that is left of transcripts their harness deleted, and the
// incidents' sightings are all there is of what an incident did over time (its file keeps only its running count),
// so without force Rebuild refuses them with ErrKept.
func (s *Store) Rebuild(ctx context.Context, source string, force bool) (int, error) {
	switch {
	case source == Sessions, source == Supervise, source == Capacity:
		return 0, fmt.Errorf("%s: %w", source, ErrObserved)
	case source == Transcripts && !force:
		return 0, fmt.Errorf("%s: %w: a transcript its harness deleted since cannot be read again", source, ErrKept)
	case source == Incidents && !force:
		return 0, fmt.Errorf("%s: %w: its sightings are what each incident did over time, and its file keeps only its running count", source, ErrKept)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	like := source + "-%"
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE source=? OR source LIKE ?`, source, like)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, q := range []string{
		`DELETE FROM cursors WHERE source=? OR source LIKE ?`,
		`DELETE FROM seen_files WHERE source=? OR source LIKE ?`,
		`DELETE FROM skipped WHERE source=? OR source LIKE ?`,
		`DELETE FROM file_cursors WHERE source=? OR source LIKE ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, source, like); err != nil {
			return 0, err
		}
	}
	return int(n), tx.Commit()
}

// CallsOf reads the calls the transcripts' results answer: for each result with a tool id, the tool call of its
// session with that id, at most reach before it. It returns each call's text by its result's id.
func (s *Store) CallsOf(ctx context.Context, results []Event, reach time.Duration) (map[string]string, error) {
	type key struct{ session, id string }
	type result struct {
		id string
		ts int64
	}
	want := map[key][]result{}
	tools := map[string]bool{}
	var lo, hi int64
	for _, r := range results {
		id := toolID(r.Meta)
		if r.Source != Transcripts || id == "" {
			continue
		}
		k := key{r.Session, id}
		want[k] = append(want[k], result{r.ID(), r.TS.UnixMilli()})
		tools[r.Tool] = true
		if ms := r.TS.UnixMilli(); lo == 0 || ms < lo {
			lo = ms
		}
		hi = max(hi, r.TS.UnixMilli())
	}
	if len(want) == 0 {
		return nil, nil
	}
	args := []any{Transcripts, lo - reach.Milliseconds(), hi}
	for t := range tools {
		args = append(args, t)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session, ts, meta, summary FROM events WHERE kind = 'tool_call' AND source = ? AND ts >= ?
	  AND ts <= ? AND tool IN (?`+strings.Repeat(",?", len(tools)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var session, meta, summary string
		var ts int64
		if err := rows.Scan(&session, &ts, &meta, &summary); err != nil {
			return nil, err
		}
		for _, r := range want[key{session, toolID(meta)}] {
			if ts <= r.ts && ts >= r.ts-reach.Milliseconds() {
				out[r.id] = summary
			}
		}
	}
	return out, rows.Err()
}

// toolID is the tool call's id an event's meta holds.
func toolID(meta string) string {
	i := strings.Index(meta, `"tool_id":"`)
	if i < 0 {
		return ""
	}
	rest := meta[i+len(`"tool_id":"`):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// EventByKey reads a source's event by its key; ok is false when the store holds none.
func (s *Store) EventByKey(ctx context.Context, source, key string) (Event, bool, error) {
	evs, err := scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE id = ?`, Event{Source: source, SourceKey: key}.ID()))
	if err != nil || len(evs) == 0 {
		return Event{}, false, err
	}
	return evs[0], true, nil
}

// Origin is where events were read from: the file or store their Ref names (all of it before its last '@'),
// with their harness, session and subagent.
type Origin struct{ Harness, Path, Session, Agent string }

// Origins counts a source's events by where they were read from.
func (s *Store) Origins(ctx context.Context, source string) (map[Origin]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT harness, ref, session, agent FROM events WHERE source = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Origin]int{}
	for rows.Next() {
		var o Origin
		var ref string
		if err := rows.Scan(&o.Harness, &ref, &o.Session, &o.Agent); err != nil {
			return nil, err
		}
		o.Path = ref
		if i := strings.LastIndexByte(ref, '@'); i >= 0 {
			o.Path = ref[:i]
		}
		out[o]++
	}
	return out, rows.Err()
}

// Query selects events newest first, or, with Session (an id or its first characters), one session's events
// with its subagents' at any depth, oldest first: a session reads as what happened, in order. Events of one
// moment keep their order in the source (SourceKey). Until, when set, is the end of the window. Harness, when
// set, is one of Harnesses: its events from every source.
type Query struct {
	Since   time.Time
	Kinds   []string
	Source  string
	Session string
	Until   time.Time
	Limit   int
	Harness string
	Actor   Actor  // when set, only its events
	Main    bool   // only a session's own events, not its subagents'
	MetaHas string // when set, only events whose meta holds it, e.g. `"is_error":true`
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
	if q.Harness != "" {
		where, args = append(where, "harness = ?"), append(args, q.Harness)
	}
	if !q.Until.IsZero() {
		where, args = append(where, "ts < ?"), append(args, q.Until.UnixMilli())
	}
	if q.Actor != "" {
		where, args = append(where, "actor = ?"), append(args, string(q.Actor))
	}
	if q.Main {
		where = append(where, "agent = ''")
	}
	if q.MetaHas != "" {
		where, args = append(where, "instr(meta, ?) > 0"), append(args, q.MetaHas)
	}
	order := "ts DESC, source_key DESC"
	if q.Session != "" {
		// An id or its first characters (a range the session index serves), and the threads under it at any
		// depth: a subagent's thread is an Agent inside its parent's session, and its own subagents' events
		// are in a session of its id.
		where = append(where, `session IN (WITH RECURSIVE tree(id) AS (
		    SELECT DISTINCT session FROM events WHERE session >= ? AND session < ?
		    UNION SELECT DISTINCT e.agent FROM events e JOIN tree ON e.session = tree.id WHERE e.agent != '' AND e.agent != e.session)
		  SELECT id FROM tree)`)
		args = append(args, q.Session, q.Session+"\uffff")
		order = "ts, source_key"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	return scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`,
		append(args, limit)...))
}

// scanEvents reads the rows of a query of every event column, in order.
func scanEvents(rows *sql.Rows, err error) ([]Event, error) {
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

// Before is a session's own transcript events of some kinds from after up to before (not its subagents'), the
// newest first, at most limit of them: what led up to a moment. The session index serves it.
func (s *Store) Before(ctx context.Context, session string, kinds []string, after, before time.Time, limit int) ([]Event, error) {
	if len(kinds) == 0 || limit <= 0 {
		return nil, nil
	}
	args := []any{session, Transcripts, after.UnixMilli(), before.UnixMilli()}
	for _, k := range kinds {
		args = append(args, k)
	}
	return scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE session = ? AND agent = '' AND source = ? AND ts > ?
	  AND ts < ? AND kind IN (?`+strings.Repeat(",?", len(kinds)-1)+`) ORDER BY ts DESC, source_key DESC LIMIT ?`,
		append(args, limit)...))
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
