package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The improvement loop's record of its own runs: one row for each run of its agent on a case, what the run was
// for, how it ended, what it cost and where its files are. It is kept: what a run spent cannot be read again, and
// the loop's budgets and its stop rule read it.
const loopTables = `
CREATE TABLE IF NOT EXISTS loop_runs (id TEXT PRIMARY KEY, case_id TEXT NOT NULL, family TEXT NOT NULL, step TEXT NOT NULL,
  started INTEGER NOT NULL, ended INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, failure TEXT NOT NULL DEFAULT '',
  cost_usd REAL NOT NULL DEFAULT 0, tokens INTEGER NOT NULL DEFAULT 0, turns INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '', dir TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
  experiment TEXT NOT NULL DEFAULT '');
`

var loopTableNames = []string{"loop_runs"}

const loopIndexes = `
CREATE INDEX IF NOT EXISTS loop_runs_case ON loop_runs(case_id, started);
CREATE INDEX IF NOT EXISTS loop_runs_started ON loop_runs(started);
`

var loopIndexNames = []string{"loop_runs_case", "loop_runs_started"}

// LoopRun statuses.
const (
	RunRunning = "running" // started, not ended: a run killed midway stays so, and counts its budget in full
	RunOK      = "ok"      // the agent finished and reported
	RunFailed  = "failed"  // the agent or a tool failed; Failure says how
	RunBudget  = "budget"  // it reached its budget
	RunTurns   = "turns"   // it reached its turns
	RunTimeout = "timeout" // it ran out of its time and was stopped
	RunRefused = "refused" // a gate refused to start it (no budget left, a case blocked)
)

// Usage is what a run's model calls took, by the kind of token Bedrock prices: input, output, cache reads and cache
// writes, the writes at the one-hour lifetime among them (a write lives five minutes otherwise).
type Usage struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h int64
}

// Total is every token of the usage.
func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Add sums two usages.
func (u Usage) Add(v Usage) Usage {
	return Usage{u.Input + v.Input, u.Output + v.Output, u.CacheRead + v.CacheRead, u.CacheWrite + v.CacheWrite, u.CacheWrite1h + v.CacheWrite1h}
}

// LoopRun is one run of the loop's agent on a case.
type LoopRun struct {
	CaseEpisode            int64 // case episode captured when this reservation is inserted
	ID, Case, Family, Step string
	Started, Ended         time.Time
	Status, Failure        string
	USD                    float64 // reservation while running; charged cost after completion
	Tokens                 int64
	Usage                  Usage // the tokens by kind; none for a run whose stream gave no result
	Turns                  int
	Model, Dir, Summary    string
	Experiment             string // "" for a live run; otherwise what it was (an evaluation's task and trial)
}

// ErrNoRun means no run has the id.
var ErrNoRun = errors.New("no loop run has this id")

// PutLoopRun stores a run, or replaces the run of its id: a run is put when it starts, and again when it ends.
func (s *Store) PutLoopRun(ctx context.Context, r LoopRun) error {
	if r.ID == "" || r.Step == "" || r.Status == "" || r.Started.IsZero() && (r.Status == RunRunning || r.Ended.IsZero()) {
		return fmt.Errorf("a loop run needs an id, a step, a status and a start")
	}
	var ended int64
	var started int64
	if !r.Started.IsZero() {
		started = r.Started.UnixMilli()
	}
	if !r.Ended.IsZero() {
		ended = r.Ended.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO loop_runs (id, case_id, family, step, started, ended, status, failure, cost_usd,
	  tokens, turns, model, dir, summary, experiment, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	  cache_write_1h_tokens, case_episode) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,CASE WHEN ? > 0 THEN ? ELSE COALESCE((SELECT episode FROM cases WHERE id = ?), 0) END)
	  ON CONFLICT(id) DO UPDATE SET case_id = excluded.case_id, family = excluded.family, step = excluded.step,
	    started = excluded.started, ended = excluded.ended, status = excluded.status, failure = excluded.failure,
	    cost_usd = excluded.cost_usd, tokens = excluded.tokens, turns = excluded.turns, model = excluded.model,
	    dir = excluded.dir, summary = excluded.summary, experiment = excluded.experiment, input_tokens = excluded.input_tokens,
	    output_tokens = excluded.output_tokens, cache_read_tokens = excluded.cache_read_tokens,
	    cache_write_tokens = excluded.cache_write_tokens, cache_write_1h_tokens = excluded.cache_write_1h_tokens`,
		r.ID, r.Case, r.Family, r.Step, started, ended, r.Status, caseText(r.Failure, 400), r.USD, r.Tokens, r.Turns,
		r.Model, r.Dir, caseText(r.Summary, caseTextMax), r.Experiment, r.Usage.Input, r.Usage.Output, r.Usage.CacheRead,
		r.Usage.CacheWrite, r.Usage.CacheWrite1h, r.CaseEpisode, r.CaseEpisode, r.Case)
	return err
}

const loopColumns = `id, case_id, family, step, started, ended, status, failure, cost_usd, tokens, turns, model, dir, summary, experiment,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cache_write_1h_tokens, case_episode`

// loopRunColumns are the columns loop_runs gained after it was first made (keptColumns): a run's tokens by kind.
var loopRunColumns = [][2]string{{"input_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"output_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_read_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"cache_write_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_write_1h_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"case_episode", "INTEGER NOT NULL DEFAULT 0"}}

func scanRuns(rows *sql.Rows, err error) ([]LoopRun, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LoopRun
	for rows.Next() {
		var r LoopRun
		var started, ended int64
		if err := rows.Scan(&r.ID, &r.Case, &r.Family, &r.Step, &started, &ended, &r.Status, &r.Failure, &r.USD, &r.Tokens,
			&r.Turns, &r.Model, &r.Dir, &r.Summary, &r.Experiment, &r.Usage.Input, &r.Usage.Output, &r.Usage.CacheRead,
			&r.Usage.CacheWrite, &r.Usage.CacheWrite1h, &r.CaseEpisode); err != nil {
			return nil, err
		}
		if started != 0 {
			r.Started = time.UnixMilli(started)
		}
		if ended != 0 {
			r.Ended = time.UnixMilli(ended)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoopRunByID reads one run.
func (s *Store) LoopRunByID(ctx context.Context, id string) (LoopRun, error) {
	rs, err := scanRuns(s.db.QueryContext(ctx, `SELECT `+loopColumns+` FROM loop_runs WHERE id = ?`, id))
	if err != nil {
		return LoopRun{}, err
	}
	if len(rs) == 0 {
		return LoopRun{}, fmt.Errorf("%w: %q", ErrNoRun, id)
	}
	return rs[0], nil
}

// RunFilter selects runs: of one case, since a time, live ones or one experiment's, at most Limit of them.
type RunFilter struct {
	Case       string
	Since      time.Time
	Experiment *string // nil: every run; "": the live ones
	Ran        bool    // only started, completed runs; exclude refusals and sandbox failures before applying Limit
	Judged     bool    // not the runs the host ended (stopped by a signal, killed by a restart): they say nothing of the agent
	Limit      int
}

// LoopRuns lists runs, the newest first.
func (s *Store) LoopRuns(ctx context.Context, f RunFilter) ([]LoopRun, error) {
	where, args := []string{"started >= ?"}, []any{f.Since.UnixMilli()}
	if f.Case != "" {
		where, args = append(where, "case_id = ?"), append(args, f.Case)
	}
	if f.Experiment != nil {
		where, args = append(where, "experiment = ?"), append(args, *f.Experiment)
	}
	if f.Ran {
		where, args = append(where, "started > 0 AND status NOT IN (?, ?) AND failure <> ?"), append(args, RunRefused, RunRunning, "sandbox")
	}
	if f.Judged {
		where = append(where, "failure NOT IN ('stopped', 'killed')")
	}
	q := `SELECT ` + loopColumns + ` FROM loop_runs WHERE ` + strings.Join(where, " AND ") + ` ORDER BY started DESC, id DESC`
	if f.Limit > 0 {
		q, args = q+` LIMIT ?`, append(args, f.Limit)
	}
	return scanRuns(s.db.QueryContext(ctx, q, args...))
}

// LoopSpend is what the live runs that started since a time spent, in US dollars. A run still running counts its
// budget in full (reserved), since what it spends is known only when it ends.
func (s *Store) LoopSpend(ctx context.Context, since time.Time, reserved float64) (float64, error) {
	var spent float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status = ? AND cost_usd = 0 THEN ? ELSE cost_usd END), 0)
 FROM loop_runs WHERE experiment = '' AND started >= ?`, RunRunning, reserved, since.UnixMilli()).Scan(&spent)
	return spent, err
}

// FinishLoopRun ends a reservation and charges its canonical case in one transaction, exactly once.
func (s *Store) FinishLoopRun(ctx context.Context, r LoopRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, status string
	var started int64
	if err := tx.QueryRowContext(ctx, `SELECT case_id, status, started FROM loop_runs WHERE id = ?`, r.ID).Scan(&id, &status, &started); err != nil {
		return err
	}
	if status != RunRunning {
		return nil
	}
	if r.Status == RunRunning || r.Ended.IsZero() || r.USD < 0 {
		return fmt.Errorf("invalid run completion")
	}
	c, err := live(ctx, tx, id)
	if err != nil {
		return err
	}
	duration := max(int64(0), r.Ended.UnixMilli()-started)
	if r.Started.IsZero() {
		started, duration = 0, 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET cost_usd = cost_usd + ?, cost_tokens = cost_tokens + ?, cost_ms = cost_ms + ?,
 runs = runs + 1, updated = ? WHERE id = ?`, r.USD, r.Tokens, duration, r.Ended.UnixMilli(), c.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE loop_runs SET started = ?, ended = ?, status = ?, failure = ?, cost_usd = ?, tokens = ?, turns = ?, model = ?, summary = ?,
 input_tokens = ?, output_tokens = ?, cache_read_tokens = ?, cache_write_tokens = ?, cache_write_1h_tokens = ? WHERE id = ?`,
		started, r.Ended.UnixMilli(), r.Status, caseText(r.Failure, 400), r.USD, r.Tokens, r.Turns, r.Model, caseText(r.Summary, caseTextMax),
		r.Usage.Input, r.Usage.Output, r.Usage.CacheRead, r.Usage.CacheWrite, r.Usage.CacheWrite1h, r.ID); err != nil {
		return err
	}
	if err := caseLog(ctx, tx, c.ID, "loop:"+r.ID, "run", r.Step+" "+r.ID+": "+r.Status+" "+r.Failure+" "+r.Summary, r.Ended); err != nil {
		return err
	}
	return tx.Commit()
}
