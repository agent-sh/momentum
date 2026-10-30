package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Supervise is the source of the events `momentum supervise` writes: what it sent to a session and why, what it
// decided not to send, what came of each message, and the cases it opened. They are its own record, so
// Rebuild refuses them.
const Supervise = "supervise"

// OwnerSessions are the sessions in which the owner wrote in a window, by the transcripts' events.
func (s *Store) OwnerSessions(ctx context.Context, since, until time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session FROM events WHERE source = ? AND kind = 'owner_text'
	  AND agent = '' AND session != '' AND ts >= ? AND ts < ? ORDER BY session`, Transcripts, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Timeline is one session's own transcript events in a window, oldest first: not its subagents'.
func (s *Store) Timeline(ctx context.Context, session string, since, until time.Time) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE session = ? AND agent = '' AND source = ?
	  AND ts >= ? AND ts < ? ORDER BY ts, source_key`, session, Transcripts, since.UnixMilli(), until.UnixMilli())
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

// Mark is one event's session and time, and its summary.
type Mark struct {
	Session string
	At      time.Time
	Summary string
}

// FirstPartyCalls are Claude Code's tool calls that the Anthropic API answered, in a window, oldest first: a
// call's id names who answered it, and Amazon Bedrock's start toolu_bdrk_, Vertex's toolu_vrtx_. A call is a turn that worked, on the
// account a login, a usage limit or an organization block stops. limit bounds how many (0: all).
func (s *Store) FirstPartyCalls(ctx context.Context, since, until time.Time, limit int) ([]Mark, error) {
	return s.marks(ctx, `kind = 'tool_call' AND harness = 'claude' AND meta LIKE '%"tool_id":"toolu_%'
	  AND meta NOT LIKE '%"tool_id":"toolu_bdrk_%' AND meta NOT LIKE '%"tool_id":"toolu_vrtx_%'`, since, until, limit)
}

// APIErrors are the API errors Claude Code ended turns on, in a window, oldest first.
func (s *Store) APIErrors(ctx context.Context, since, until time.Time) ([]Mark, error) {
	return s.marks(ctx, `kind = 'harness_text' AND harness = 'claude' AND meta LIKE '%"is_error":true%' AND meta LIKE '%"why":"api error"%'`,
		since, until, 0)
}

func (s *Store) marks(ctx context.Context, where string, since, until time.Time, limit int) ([]Mark, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session, ts, summary FROM events WHERE source = ? AND agent = '' AND ts > ? AND ts < ?
	  AND `+where+` ORDER BY ts LIMIT ?`, Transcripts, since.UnixMilli(), until.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mark
	for rows.Next() {
		var m Mark
		var ts int64
		if err := rows.Scan(&m.Session, &ts, &m.Summary); err != nil {
			return nil, err
		}
		m.At = time.UnixMilli(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// OwnerTurns are the owner's last turns in a session, newest first, at most limit, by the transcripts' events.
func (s *Store) OwnerTurns(ctx context.Context, session string, limit int) ([]Mark, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, ts, summary FROM events WHERE session = ? AND kind = 'owner_text' AND source = ?
	  AND agent = '' ORDER BY ts DESC LIMIT ?`, session, Transcripts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mark
	for rows.Next() {
		var m Mark
		var ts int64
		if err := rows.Scan(&m.Session, &ts, &m.Summary); err != nil {
			return nil, err
		}
		m.At = time.UnixMilli(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AskAnswered is when a session last left needs_you on a question, a plan or a permission, by the sessions
// events: its owner answered it. Zero when it never did.
func (s *Store) AskAnswered(ctx context.Context, session string) (time.Time, error) {
	var ts *int64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(ts) FROM events WHERE session = ? AND source = ? AND kind = 'phase'
	  AND meta LIKE '%"from":"needs_you"%' AND (meta LIKE '%"from_ask_kind":"question"%' OR meta LIKE '%"from_ask_kind":"plan"%'
	  OR meta LIKE '%"from_ask_kind":"permission"%')`, session, Sessions).Scan(&ts)
	if err != nil || ts == nil {
		return time.Time{}, err
	}
	return time.UnixMilli(*ts), nil
}

// SuperviseEvents are one session's supervise events since a time, oldest first.
func (s *Store) SuperviseEvents(ctx context.Context, session string, since time.Time) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE session = ? AND source = ? AND ts >= ?
	  ORDER BY ts, source_key`, session, Supervise, since.UnixMilli())
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

// SentTexts are the texts `momentum supervise` typed into sessions, whitespace folded, by the events it recorded:
// sent, being sent when its run ended, or pasted and not known to be submitted.
func (s *Store) SentTexts(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT meta FROM events WHERE source = ? AND kind = 'act' AND experiment = ''
		AND (meta LIKE '%"sent":true%' OR meta LIKE '%"sending":true%' OR meta LIKE '%"unstable":true%')`, Supervise)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var meta string
		if err := rows.Scan(&meta); err != nil {
			return nil, err
		}
		var m struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(meta), &m) == nil && strings.TrimSpace(m.Text) != "" {
			out[strings.Join(strings.Fields(m.Text), " ")] = true
		}
	}
	return out, rows.Err()
}
