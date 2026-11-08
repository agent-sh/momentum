package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/sessions/state"
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
	Sources      []string // nil counts every source; otherwise only these sources
	ExcludeCwd   string   // exclude this directory and its descendants
	BySession    bool     // the sessions' hours; otherwise the machine's
	Harness      string   // when set, only that harness's events
	Actor        Actor    // when set, only the hours in which it did something (the owner's turns, say)
	Tools        []string // when set, only the hours in which one of these tools was called or answered: a guard's exposure
	// Server, when set, keeps only the hours in which a tool of this MCP server (OfServer) was called or answered,
	// whichever of its tools: a guard's exposure.
	Server string
	// Cwds, when set, keeps only the hours of events in one of these directories or below it: a guard's exposure. Each
	// row's directory is compared cleaned (CleanCwd), so a row stored before the store cleaned working directories
	// (`/a//b`, `/a/./b`) is in its directory as one stored after is. Activity alone narrows by it.
	Cwds []string
}

// Activity returns the units of activity over a span: the hours in which the sessions picked (an event of a
// session's own), or the machine, did anything the store recorded, from any source.
func (s *Store) Activity(ctx context.Context, q ActivityQuery) (map[Unit]bool, error) {
	if !q.Until.After(q.Since) {
		return nil, errors.New("an activity span ends after it starts")
	}
	where, args, session := activityWhere(q)
	cwd := "''"
	if len(q.Cwds) > 0 {
		cwd = "cwd" // compared in Go, cleaned: SQL would miss a row stored in another form of its directory
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+session+`, ts / 3600000, `+cwd+` FROM events WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Unit]bool{}
	for rows.Next() {
		var u Unit
		var dir string
		if err := rows.Scan(&u.Session, &u.Hour, &dir); err != nil {
			return nil, err
		}
		if len(q.Cwds) > 0 && !InAnyDir(dir, q.Cwds) {
			continue
		}
		out[u] = true
	}
	return out, rows.Err()
}

// MCPServer is the MCP server a tool is of, by the harness's name for an MCP tool, mcp__SERVER__TOOL, as Claude Code
// names one: its server is what comes between the first two separators. A tool named otherwise (Bash, Read, any other
// built-in) is of none, and gives "".
//
// Codex serves every ChatGPT app connector from one server, codex_apps, under a namespace of the connector's own,
// codex_apps__CONNECTOR, so its tools are mcp__codex_apps__CONNECTOR__TOOL; where a tool's name began with its
// connector's, Codex cut that off, and TOOL begins with _ (mcp__codex_apps__gmail___search). A tool of codex_apps is
// of its connector, codex_apps__CONNECTOR, the field before the first __ of what follows the server: the whole server
// would count every connector's hours as the problem's unit in use. It is still of codex_apps as a whole (OfServer). A
// tool Codex serves with no connector is in the server's own namespace, mcp__codex_apps__TOOL: it is of no connector,
// so MCPServer gives "", and of codex_apps as a whole alone.
func MCPServer(tool string) string {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return ""
	}
	server, name, ok := strings.Cut(rest, "__")
	if !ok || server == "" || name == "" {
		return ""
	}
	if server == CodexApps {
		connector, name, ok := strings.Cut(name, "__")
		if !ok || connector == "" || name == "" {
			return ""
		}
		return CodexApps + "__" + connector
	}
	return server
}

// CodexApps is the server Codex serves its app connectors from (MCPServer).
const CodexApps = "codex_apps"

// OfServer says whether a tool is of a server as its exposure counts it (ActivityQuery.Server): named from the server's
// prefix (mcpPrefix), with a name after it. A server MCPServer gives has as its tools exactly those MCPServer gives it;
// codex_apps as a whole has every tool named from mcp__codex_apps__, a connector's or not.
func OfServer(tool, server string) bool {
	prefix := mcpPrefix(server)
	return server != "" && len(tool) > len(prefix) && strings.HasPrefix(tool, prefix)
}

// HoldsServer says whether every tool of other is of server (OfServer), by their names alone: a server holds itself,
// and codex_apps holds each of its connectors.
func HoldsServer(server, other string) bool {
	return server != "" && other != "" && strings.HasPrefix(mcpPrefix(other), mcpPrefix(server))
}

// mcpPrefix is what every tool of an MCP server is named from (MCPServer), a codex_apps connector's in either of
// Codex's forms: its name and __.
func mcpPrefix(server string) string {
	return "mcp__" + server + "__"
}

// InAnyDir says whether a working directory, cleaned (CleanCwd), is one of dirs or below one of them. An empty one is
// in none.
func InAnyDir(cwd string, dirs []string) bool {
	if cwd == "" {
		return false
	}
	cwd = CleanCwd(cwd)
	for _, dir := range dirs {
		dir = CleanCwd(dir)
		if dir != "" && (cwd == dir || strings.HasPrefix(cwd, strings.TrimRight(dir, "/")+"/")) {
			return true
		}
	}
	return false
}

// activityWhere gives activity and its sources the same bounds.
func activityWhere(q ActivityQuery) ([]string, []any, string) {
	where, args := []string{"ts >= ?", "ts < ?"}, []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	where, args = excludeDirectory(where, args, q.ExcludeCwd)
	if q.Harness != "" {
		where, args = append(where, "harness = ?"), append(args, q.Harness)
	}
	if q.Actor != "" {
		where, args = append(where, "actor = ?"), append(args, string(q.Actor))
	}
	if len(q.Tools) > 0 {
		marks := make([]string, len(q.Tools))
		for i, tool := range q.Tools {
			marks[i] = "?"
			args = append(args, tool)
		}
		where = append(where, "tool IN ("+strings.Join(marks, ",")+")")
	}
	if q.Server != "" {
		// instr, not LIKE: a server's name may hold an underscore, which LIKE reads as any character; and a name after
		// the prefix, as OfServer and MCPServer want one
		prefix := mcpPrefix(q.Server)
		where, args = append(where, "instr(tool, ?) = 1 AND length(tool) > length(?)"), append(args, prefix, prefix)
	}
	if q.Sources != nil {
		marks := make([]string, len(q.Sources))
		for i, source := range q.Sources {
			marks[i] = "?"
			args = append(args, source)
		}
		where = append(where, "source IN ("+strings.Join(marks, ",")+")")
	}
	session := "''"
	if q.BySession {
		session = "session"
		where = append(where, "session != ''")
	}
	return where, args, session
}

// DirPaths are the paths a directory is known by: as given, made absolute (a relative one is the process's working
// directory's), and as its links resolve. The loop's own activity is what ran in its directory by either (InDir, and
// every count's ExcludeCwd). Only the directory is resolved: a working directory is compared as its session recorded
// it, cleaned at ingest, so one that reached the loop's directory through a link of its own is not told apart.
func DirPaths(dir string) []string {
	if dir == "" {
		return nil
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	paths := []string{filepath.Clean(dir)}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != paths[0] {
		paths = append(paths, resolved)
	}
	return paths
}

// InDir says whether a working directory is dir or below it, by either of its paths (DirPaths): the one rule that
// tells the loop's own activity from the rest, which the store's counts apply in SQL (excludeDirectory).
func InDir(cwd, dir string) bool {
	if cwd == "" {
		return false
	}
	cwd = filepath.Clean(cwd)
	for _, path := range DirPaths(dir) {
		if cwd == path || strings.HasPrefix(cwd, strings.TrimRight(path, "/")+"/") {
			return true
		}
	}
	return false
}

// excludeDirectory leaves out a directory by both its paths (DirPaths), as InDir tells them.
func excludeDirectory(where []string, args []any, dir string) ([]string, []any) {
	for _, path := range DirPaths(dir) {
		where = append(where, "cwd != ? AND instr(cwd, ?) != 1")
		args = append(args, path, strings.TrimRight(path, "/")+"/")
	}
	return where, args
}

// Sources names the signal and every source with an event in the selected span.
func (s *Store) Sources(ctx context.Context, signal string, q ActivityQuery) ([]string, error) {
	where, args, _ := activityWhere(q)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT source FROM events WHERE `+strings.Join(where, " AND ")+` UNION SELECT ? ORDER BY 1`, append(args, signal)...)
	if err != nil {
		return nil, err
	}
	var sources []string
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return sources, nil
}

// UnreadSources checks recorded sources even when a window has none of their events. A source that reads each harness
// on its own (a sessions scan, supervision) is read for the query's harness alone.
func (s *Store) UnreadSources(ctx context.Context, signal string, q ActivityQuery) ([]string, error) {
	sources := q.Sources
	if sources == nil {
		var err error
		sources, err = s.Sources(ctx, signal, q)
		if err != nil {
			return nil, err
		}
	}
	var unread []string
	for _, source := range sources {
		at, err := s.HarnessReadWindowThrough(ctx, source, q.Harness, q.Since)
		if err != nil {
			return nil, err
		}
		if !at.After(q.Until) {
			unread = append(unread, source)
		}
	}
	return unread, nil
}

// FirstEvent is when a source's first event in the store happened, or zero when it has none.
func (s *Store) FirstEvent(ctx context.Context, source string) (time.Time, error) {
	return s.FirstHarnessEvent(ctx, source, "")
}

// FirstHarnessEvent is when a source's first event of a harness happened (a source that reads each harness on its own;
// any other source's first event, whatever the harness), or zero when it has none.
func (s *Store) FirstHarnessEvent(ctx context.Context, source, harness string) (time.Time, error) {
	q, args := `SELECT MIN(ts) FROM events WHERE source = ?`, []any{source}
	if harness = harnessOf(source, harness); harness != "" {
		q, args = q+` AND harness = ?`, append(args, harness)
	}
	var first sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&first); err != nil || !first.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(first.Int64), nil
}

// A command (a sessions scan, supervision, a capacity run) records every run in the cursors table, under its source:
//
//	SOURCE:read                    the start of the newest run that read all of what it reads
//	SOURCE:read:h:HARNESS          the same for one harness, for a source that reads each on its own
//	SOURCE:read:incomplete:H:HOUR  the runs of an hour that did not (H a harness, or none for the source as a whole):
//	                               the time of the first in updated, and of the last in position; a stretch in which no
//	                               run started (MarkRunRead) is one record, keyed by the hour it ends in
//
// An ingest source records only SOURCE:read, and a run that read in part, on its own terms, SOURCE:read:run:START
// (MarkPartialRead). A command's failed run loses what it should have seen, and no later run sees it again, so a
// window that holds one is never read: ReadWindowThrough stops at it.

// perHarness are the sources whose runs read each harness on their own, so one harness's failing scan leaves the
// others' sessions read.
var perHarness = map[string]bool{Sessions: true, Supervise: true}

// harnessOf is the harness a source's records are kept for: the one asked for when the source reads each harness on
// its own, none for another source.
func harnessOf(source, harness string) string {
	if perHarness[source] {
		return harness
	}
	return ""
}

// ReadThrough reads only the complete run start, never a cursor update after the read. It is the start of the newest
// run that read all of what the source reads.
func (s *Store) ReadThrough(ctx context.Context, source string) (time.Time, error) {
	return s.readTime(ctx, source+":read")
}

// HarnessReadThrough is the start of the newest run that read one harness of a source completely; the whole source's
// when the source keeps none for the harness.
func (s *Store) HarnessReadThrough(ctx context.Context, source, harness string) (time.Time, error) {
	if harness = harnessOf(source, harness); harness != "" {
		if at, err := s.readTime(ctx, source+":read:h:"+harness); err != nil || !at.IsZero() {
			return at, err
		}
	}
	return s.ReadThrough(ctx, source)
}

func (s *Store) readTime(ctx context.Context, key string) (time.Time, error) {
	return readTimeIn(ctx, s.db, key)
}

func readTimeIn(ctx context.Context, q querier, key string) (time.Time, error) {
	var updated int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated),0) FROM cursors WHERE source = ?`, key).Scan(&updated)
	if updated == 0 {
		return time.Time{}, err
	}
	return time.UnixMilli(updated), err
}

// MarkRead records the start of a successful complete read, after all its events have been stored.
// An overlapping older run cannot move the record back. A partial run that began before it is of no use any more
// (it could only raise a window's read time to its own start), and goes with it.
func (s *Store) MarkRead(ctx context.Context, source string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := markReadIn(ctx, tx, source, at); err != nil {
		return err
	}
	return tx.Commit()
}

func markReadIn(ctx context.Context, q querier, source string, at time.Time) error {
	if err := markTime(ctx, q, source+":read", at); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `DELETE FROM cursors WHERE source LIKE ? AND updated <= (SELECT updated FROM cursors WHERE source = ?)`,
		source+":read:run:%", source+":read")
	return err
}

// MarkHarnessRead records the start of a run that read one harness of a source completely. A command's own runs record
// through MarkRunRead, which also leaves the stretch since the run before as an incomplete run when it is long.
func (s *Store) MarkHarnessRead(ctx context.Context, source, harness string, at time.Time) error {
	if harnessOf(source, harness) == "" {
		return s.MarkRead(ctx, source, at)
	}
	return markHarnessReadIn(ctx, s.db, source, harness, at)
}

func markHarnessReadIn(ctx context.Context, q querier, source, harness string, at time.Time) error {
	if harness = harnessOf(source, harness); harness == "" {
		return markReadIn(ctx, q, source, at)
	}
	return markTime(ctx, q, source+":read:h:"+harness, at)
}

func markTime(ctx context.Context, q querier, key string, at time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(source,position,updated) VALUES (?, '', ?)
 ON CONFLICT(source) DO UPDATE SET updated=MAX(updated, excluded.updated)`, key, at.UnixMilli())
	return err
}

// MarkIncomplete records a run that did not read a harness of a source (the whole source, for none): a scan that
// failed, a session read in part, a run that stopped. What such a run should have seen is not in the store, and
// no later run will put it there. A run of the same hour is the same record, which keeps the first and the last of
// them, so a failure that goes on is one record an hour, the unit of a measurement.
func (s *Store) MarkIncomplete(ctx context.Context, source, harness string, at time.Time) error {
	return markIncompleteIn(ctx, s.db, source, harness, at)
}

func markIncompleteIn(ctx context.Context, q querier, source, harness string, at time.Time) error {
	return markIncompleteSpanIn(ctx, q, source, harness, at, at)
}

// markIncompleteSpanIn records the incomplete runs from the first to the last, which a record keeps as its two ends,
// keyed by the hour the last falls in.
func markIncompleteSpanIn(ctx context.Context, q querier, source, harness string, first, last time.Time) error {
	harness = harnessOf(source, harness)
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(source,position,updated) VALUES (?, ?, ?)
 ON CONFLICT(source) DO UPDATE SET updated=MIN(updated, excluded.updated),
 position=CAST(MAX(CAST(position AS INTEGER), CAST(excluded.position AS INTEGER)) AS TEXT)`,
		fmt.Sprintf("%s:read:incomplete:%s:%d", source, harness, last.UnixMilli()/3_600_000), strconv.FormatInt(last.UnixMilli(), 10), first.UnixMilli())
	return err
}

// RunGap is the longest two complete runs of a command that reads each harness on its own may start apart before
// the stretch between them counts as unread: the time a session's stamp outlives its last sighting (state.Keep). A
// session unseen for longer is gone, so a run that starts further from the one before it than this missed what the
// sessions did in between, and nothing puts that in the store.
const RunGap = state.Keep

// MarkRunRead records a complete run of a command that reads each harness of its source on its own (a sessions scan,
// supervision): the harness is read through the run's start less lag, the time its sightings trail the run by, as
// MarkHarnessRead records it. When the run starts more than RunGap after the previous complete run of that harness,
// the stretch between the two (a timer stopped, a run that gave up before it recorded anything, a machine that was off)
// is recorded as an incomplete run, as a run that failed is: no run read it, and a window that holds it is never read.
func (s *Store) MarkRunRead(ctx context.Context, source, harness string, start time.Time, lag time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := markRunReadIn(ctx, tx, source, harness, start, lag); err != nil {
		return err
	}
	return tx.Commit()
}

func markRunReadIn(ctx context.Context, q querier, source, harness string, start time.Time, lag time.Duration) error {
	if h := harnessOf(source, harness); h != "" {
		if read, err := readTimeIn(ctx, q, source+":read:h:"+h); err != nil {
			return err
		} else if before := read.Add(lag); !read.IsZero() && start.Sub(before) > RunGap {
			// strictly between the two runs: the window that ends at the first, and the one that starts at the second, are read
			if err := markIncompleteSpanIn(ctx, q, source, harness, before.Add(time.Millisecond), start.Add(-time.Millisecond)); err != nil {
				return err
			}
		}
	}
	return markHarnessReadIn(ctx, q, source, harness, start.Add(-lag))
}

// Incomplete is the time of the newest incomplete run of a source's harness (any harness, for none) that falls in
// [since, until], or zero when none does. A record of an hour holds its first and its last run: one that begins
// before the span and ends in it counts, and so does one that goes on past it.
func (s *Store) Incomplete(ctx context.Context, source, harness string, since, until time.Time) (time.Time, error) {
	where, args := incompleteRows(source, harness)
	var last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(CAST(position AS INTEGER)) FROM cursors WHERE (`+where+`)
 AND updated <= ? AND CAST(position AS INTEGER) >= ?`, append(args, until.UnixMilli(), since.UnixMilli())...).Scan(&last)
	if err != nil || !last.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(last.Int64), nil
}

// incompleteRows selects the incomplete records that bear on a harness: its own and the whole source's, or, for no
// harness, every one.
func incompleteRows(source, harness string) (string, []any) {
	prefix := source + ":read:incomplete:"
	if harness = harnessOf(source, harness); harness == "" {
		return `source LIKE ?`, []any{prefix + "%"}
	}
	return `source LIKE ? OR source LIKE ?`, []any{prefix + harness + ":%", prefix + ":%"}
}

// ReadWindowThrough is the time a window that starts at since is read through: the complete read, stopped at the
// first incomplete run that falls in or after the window, and a partial run counted only when its failed files
// predate the window.
func (s *Store) ReadWindowThrough(ctx context.Context, source string, since time.Time) (time.Time, error) {
	return s.HarnessReadWindowThrough(ctx, source, "", since)
}

// HarnessReadWindowThrough is ReadWindowThrough for the harness a signal counts: a failure of another harness's
// scan neither stops nor holds it.
func (s *Store) HarnessReadWindowThrough(ctx context.Context, source, harness string, since time.Time) (time.Time, error) {
	at, err := s.HarnessReadPast(ctx, source, harness, since)
	if err != nil {
		return at, err
	}
	// a run that read less than it should have, in or after the window: the window reads through the first of them
	where, args := incompleteRows(source, harness)
	var first sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(MAX(updated, ?)) FROM cursors WHERE (`+where+`) AND CAST(position AS INTEGER) >= ?`,
		append([]any{since.UnixMilli()}, append(args, since.UnixMilli())...)...).Scan(&first); err != nil {
		return at, err
	}
	if first.Valid && !at.IsZero() && time.UnixMilli(first.Int64).Before(at) {
		at = time.UnixMilli(first.Int64)
	}
	return at, nil
}

// HarnessReadPast is the start of the newest read of a source's harness that read all it reads, or, for an ingest
// source, that read in part with every failed file older than since. Unlike HarnessReadWindowThrough it does not stop
// at an incomplete run: a watch that asks only whether the store has read past a moment, and loses nothing it waits on
// to a stretch a failed run lost, reads by it.
func (s *Store) HarnessReadPast(ctx context.Context, source, harness string, since time.Time) (time.Time, error) {
	at, err := s.HarnessReadThrough(ctx, source, harness)
	if err != nil {
		return at, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT position, updated FROM cursors WHERE source LIKE ?`, source+":read:run:%")
	if err != nil {
		return at, err
	}
	defer rows.Close()
	for rows.Next() {
		var failed string
		var started int64
		if err := rows.Scan(&failed, &started); err != nil {
			return at, err
		}
		newest, err := strconv.ParseInt(failed, 10, 64)
		if err != nil {
			return at, err
		}
		if time.UnixMilli(newest).Before(since) && time.UnixMilli(started).After(at) {
			at = time.UnixMilli(started)
		}
	}
	return at, rows.Err()
}

// PartialReadAfter is the start of the newest run of an ingest source that read in part and started after since, or
// zero when none did since its newest complete read (a complete read drops the partial runs before it).
func (s *Store) PartialReadAfter(ctx context.Context, source string, since time.Time) (time.Time, error) {
	var started sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(updated) FROM cursors WHERE source LIKE ? AND updated > ?`, source+":read:run:%",
		since.UnixMilli()).Scan(&started); err != nil || !started.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(started.Int64), nil
}

// MarkPartialRead records a listed run and the newest modification among files it failed to read.
func (s *Store) MarkPartialRead(ctx context.Context, source string, started, failed time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO cursors(source,position,updated) VALUES (?,?,?)`,
		source+":read:run:"+strconv.FormatInt(started.UnixNano(), 10), strconv.FormatInt(failed.UnixMilli(), 10), started.UnixMilli())
	return err
}

// ErrIngestHeld is an ingest lock another ingest or a rebuild holds.
var ErrIngestHeld = errors.New("holds the store")

// LockIngest takes the lock that keeps an ingest and a rebuild of a source from running together: a rebuild drops what
// a source contributed with the records of its reads, and an ingest that finished after it would record a read of
// events the rebuild took away. It fails at once, naming the process that holds it (ErrIngestHeld). unlock releases
// it; so does the end of the process.
func (s *Store) LockIngest() (unlock func(), err error) {
	path := s.path + ".ingest-lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holder, _ := os.ReadFile(path)
			return nil, fmt.Errorf("an ingest or a rebuild (pid %s) %w", strings.TrimSpace(string(holder)), ErrIngestHeld)
		}
		return nil, fmt.Errorf("ingest lock: %w", err)
	}
	if err := f.Truncate(0); err == nil { // who holds it, for the next that finds it taken
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() { f.Close() }, nil
}
