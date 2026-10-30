package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Capacity is the source of the events `momentum capacity` writes: each deletion and prune it made, each item it
// could only report, and each fast fill of a disk. They are its own record, so Rebuild refuses them.
const Capacity = "capacity"

// capacityTables are the kept tables of `momentum capacity`, part of keptTables: what its runs observed, which
// nothing can read again.
//   - disk_samples: each disk's size, use and free space at each run.
//   - capacity_seen: when the process sampler last saw a path in use (open, mapped, a working directory, a
//     program, or named on a command line), and first.
//   - capacity_runs: each run, the time its sample of the processes took, how many it read and how many it could
//     not. The first run is where the sampler's record starts.
//   - capacity_marks: when each rule last ran its scan, or last pruned.
const capacityTables = `
CREATE TABLE IF NOT EXISTS disk_samples (disk TEXT NOT NULL, at INTEGER NOT NULL, size INTEGER NOT NULL,
  used INTEGER NOT NULL, avail INTEGER NOT NULL, PRIMARY KEY (disk, at)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS capacity_seen (path TEXT PRIMARY KEY, first INTEGER NOT NULL, last INTEGER NOT NULL,
  how TEXT NOT NULL DEFAULT '', proc TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS capacity_runs (at INTEGER PRIMARY KEY, sample_ms INTEGER NOT NULL, procs INTEGER NOT NULL,
  unreadable INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS capacity_marks (mark TEXT PRIMARY KEY, at INTEGER NOT NULL);
`

// capacityTableNames are the tables capacityTables makes.
var capacityTableNames = []string{"disk_samples", "capacity_seen", "capacity_runs", "capacity_marks"}

// DiskSample is one disk at one run: its size, what is used and what is free to an unprivileged writer, in bytes.
type DiskSample struct {
	Disk              string
	At                time.Time
	Size, Used, Avail int64
}

// Seen is when the sampler saw a path in use: first, last, how it was used the last time (fd, map, cwd, exe, arg)
// and by which process (its name and pid).
type Seen struct {
	Path        string
	First, Last time.Time
	How, Proc   string
}

// CapacityRun is one run's look at the processes: when, how long the sample took, how many processes it read and
// how many it could not (another user's).
type CapacityRun struct {
	At         time.Time
	Sample     time.Duration
	Procs      int
	Unreadable int
}

// PutCapacityRun records a run in one transaction: the run, each disk's sample, and what the sampler saw. A path
// seen before keeps its first sighting; its last becomes this one when it is later.
func (s *Store) PutCapacityRun(ctx context.Context, run CapacityRun, disks []DiskSample, seen []Seen) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO capacity_runs (at, sample_ms, procs, unreadable) VALUES (?,?,?,?)`,
		run.At.UnixMilli(), run.Sample.Milliseconds(), run.Procs, run.Unreadable); err != nil {
		return err
	}
	for _, d := range disks {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO disk_samples (disk, at, size, used, avail) VALUES (?,?,?,?,?)`,
			d.Disk, d.At.UnixMilli(), d.Size, d.Used, d.Avail); err != nil {
			return err
		}
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO capacity_seen (path, first, last, how, proc) VALUES (?,?,?,?,?)
	  ON CONFLICT(path) DO UPDATE SET first = MIN(first, excluded.first),
	    how = CASE WHEN excluded.last >= last THEN excluded.how ELSE how END,
	    proc = CASE WHEN excluded.last >= last THEN excluded.proc ELSE proc END,
	    last = MAX(last, excluded.last)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, v := range seen {
		first := v.First
		if first.IsZero() {
			first = v.Last
		}
		if _, err := stmt.ExecContext(ctx, v.Path, first.UnixMilli(), v.Last.UnixMilli(), v.How, v.Proc); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DiskSamples are one disk's samples from since on, oldest first.
func (s *Store) DiskSamples(ctx context.Context, disk string, since time.Time) ([]DiskSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, size, used, avail FROM disk_samples WHERE disk = ? AND at >= ? ORDER BY at`,
		disk, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiskSample
	for rows.Next() {
		d := DiskSample{Disk: disk}
		var at int64
		if err := rows.Scan(&at, &d.Size, &d.Used, &d.Avail); err != nil {
			return nil, err
		}
		d.At = time.UnixMilli(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SeenPaths is every path the sampler has seen in use, by path.
func (s *Store) SeenPaths(ctx context.Context) (map[string]Seen, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, first, last, how, proc FROM capacity_seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Seen{}
	for rows.Next() {
		var v Seen
		var first, last int64
		if err := rows.Scan(&v.Path, &first, &last, &v.How, &v.Proc); err != nil {
			return nil, err
		}
		v.First, v.Last = time.UnixMilli(first), time.UnixMilli(last)
		out[v.Path] = v
	}
	return out, rows.Err()
}

// SamplerStart is when the sampler's record starts: the first run's time, or zero before any.
func (s *Store) SamplerStart(ctx context.Context) (time.Time, error) {
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(at) FROM capacity_runs`).Scan(&at); err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(at.Int64), nil
}

// SamplerGap is the longest time between two runs from since on, and the run it ended at: while no run looked, a
// path in use went unseen.
func (s *Store) SamplerGap(ctx context.Context, since time.Time) (gap time.Duration, at time.Time, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at FROM capacity_runs WHERE at >= ? ORDER BY at`, since.UnixMilli())
	if err != nil {
		return 0, at, err
	}
	defer rows.Close()
	var prev int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return 0, at, err
		}
		if prev != 0 && time.Duration(t-prev)*time.Millisecond > gap {
			gap, at = time.Duration(t-prev)*time.Millisecond, time.UnixMilli(t)
		}
		prev = t
	}
	return gap, at, rows.Err()
}

// Mark is when a mark was last set (a rule's scan, a prune), or zero.
func (s *Store) Mark(ctx context.Context, mark string) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT at FROM capacity_marks WHERE mark = ?`, mark).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(at), nil
}

// SetMark records that a mark was set at a time.
func (s *Store) SetMark(ctx context.Context, mark string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO capacity_marks (mark, at) VALUES (?, ?)
	  ON CONFLICT(mark) DO UPDATE SET at = excluded.at`, mark, at.UnixMilli())
	return err
}

// Stored reports which of these events the store holds already.
func (s *Store) Stored(ctx context.Context, evs []Event) (map[string]bool, error) {
	out := map[string]bool{}
	stmt, err := s.db.PrepareContext(ctx, `SELECT COUNT(*) FROM events WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, e := range evs {
		var n int
		if err := stmt.QueryRowContext(ctx, e.ID()).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			out[e.ID()] = true
		}
	}
	return out, nil
}

// Note returns the note with this id, done or not, and whether there is one.
func (s *Store) Note(ctx context.Context, id string) (Note, bool, error) {
	ns, err := notes(ctx, s.db, `WHERE id = ?`, id)
	if err != nil || len(ns) == 0 {
		return Note{}, false, err
	}
	return ns[0], true, nil
}
