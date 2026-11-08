package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// strings1 reads a query's one text column.
func strings1(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ThreadsSince are the sessions with transcript events since a time. The time's index serves it (+source keeps
// the planner off the source's, which reads every event of the source).
func (s *Store) ThreadsSince(ctx context.Context, since time.Time) ([]string, error) {
	return strings1(s.db.QueryContext(ctx, `SELECT DISTINCT session FROM events WHERE ts >= ? AND +source = ? AND session != ''
	  ORDER BY session`, since.UnixMilli(), Transcripts))
}

// ThreadsByPrefix are the sessions with transcript events whose id is prefix or starts with it, at most two. The
// session's index serves it.
func (s *Store) ThreadsByPrefix(ctx context.Context, prefix string) ([]string, error) {
	return strings1(s.db.QueryContext(ctx, `SELECT DISTINCT session FROM events WHERE session >= ? AND session < ? AND +source = ?
	  ORDER BY session LIMIT 2`, prefix, prefix+"\uffff", Transcripts))
}

// ThreadFirst is when a session's first transcript event happened; ok is false for a session with none.
func (s *Store) ThreadFirst(ctx context.Context, session string) (time.Time, bool, error) {
	var ms sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM events INDEXED BY events_session WHERE session = ? AND source = ?`, session, Transcripts).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, false, err
	}
	return time.UnixMilli(ms.Int64), true, nil
}

// Span is when an agent of a session (its main thread: "") did its first and last thing there.
type Span struct {
	Agent       string
	First, Last time.Time
}

const threadSpansSQL = `SELECT agent, MIN(ts), MAX(ts) FROM events INDEXED BY events_session WHERE session = ? AND source = ?
  GROUP BY agent ORDER BY agent`

// ThreadSpans are a session's agents' spans in its transcript events, its main thread's among them. The reads of
// one session name the session's index: for a GROUP BY, SQLite would otherwise read every transcript event by
// the source's.
func (s *Store) ThreadSpans(ctx context.Context, session string) ([]Span, error) {
	rows, err := s.db.QueryContext(ctx, threadSpansSQL, session, Transcripts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Span
	for rows.Next() {
		var sp Span
		var first, last int64
		if err := rows.Scan(&sp.Agent, &first, &last); err != nil {
			return nil, err
		}
		sp.First, sp.Last = time.UnixMilli(first), time.UnixMilli(last)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// ThreadParentMeta is the meta of a session's first main-thread event that names a parent session; "" for none.
func (s *Store) ThreadParentMeta(ctx context.Context, session string) (string, error) {
	var meta string
	err := s.db.QueryRowContext(ctx, `SELECT meta FROM events INDEXED BY events_session WHERE session = ? AND source = ? AND agent = ''
	  AND instr(meta, '"parent":"') > 0 ORDER BY ts LIMIT 1`, session, Transcripts).Scan(&meta)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return meta, err
}

// ThreadRead is what a request reads of a session's transcript events: its main thread's text (the turns, the
// replies, what was cut from a turn), every call and result of the Writers, and the calls of the Shells whose text
// holds one of CallMarks with their results that hold one of ResultMarks, and the calls of the YieldTools whose
// result holds one of YieldMarks (a Codex command left running), whatever the call's text, with those results.
// Tool names are compared in lower case.
type ThreadRead struct {
	Writers, Shells        []string
	CallMarks, ResultMarks []string
	YieldTools, YieldMarks []string
	CutMark                string // when set, also the shells' calls and results whose text ends with it: the store cut them
	// Part is what of it a read takes: "text" the main thread's text alone, "tools" the calls and results alone,
	// "" both
	Part string
}

// ThreadEvents passes a session's transcript events that read selects, its subagents' among them, oldest first,
// to fn, one at a time: a long session is never held whole, and what no request reads stays in the database. A
// shell's result is selected by its call's tool id, inside the database.
func (s *Store) ThreadEvents(ctx context.Context, session string, read ThreadRead, fn func(Event) error) error {
	in := func(names []string) string {
		if len(names) == 0 {
			return "('')"
		}
		return "(?" + strings.Repeat(",?", len(names)-1) + ")"
	}
	marks := "1"
	if len(read.CallMarks) > 0 {
		marks = "(" + strings.TrimSuffix(strings.Repeat("instr(summary, ?) > 0 OR ", len(read.CallMarks)), " OR ") + ")"
	}
	callArgs := anys(read.CallMarks)
	if read.CutMark != "" && len(read.CallMarks) > 0 {
		marks = "(" + marks + " OR substr(summary, -length(?)) = ?)"
		callArgs = append(callArgs, read.CutMark, read.CutMark)
	}
	resultMarks := "1"
	resultArgs := anys(read.ResultMarks)
	if len(read.ResultMarks) > 0 {
		resultMarks = strings.TrimSuffix(strings.Repeat("instr(summary, ?) > 0 OR ", len(read.ResultMarks)), " OR ")
		if read.CutMark != "" {
			resultMarks += " OR substr(summary, -length(?)) = ?"
			resultArgs = append(resultArgs, read.CutMark, read.CutMark)
		}
		resultMarks = "(" + resultMarks + ")"
	}
	yieldMarks := "0"
	if len(read.YieldMarks) > 0 {
		yieldMarks = "(" + strings.TrimSuffix(strings.Repeat("instr(summary, ?) > 0 OR ", len(read.YieldMarks)), " OR ") + ")"
	}
	textPart, toolPart := "1", "1"
	switch read.Part {
	case "text":
		toolPart = "0"
	case "tools":
		textPart = "0"
	}
	const toolID = `CASE WHEN json_valid(meta) THEN json_extract(meta, '$.tool_id') END`
	shellCall := `kind = 'tool_call' AND lower(tool) IN ` + in(read.Shells) + ` AND ` + marks
	var args []any
	args = append(append(append(args, session, Transcripts), anys(read.Shells)...), callArgs...)
	args = append(append(append(args, session, Transcripts), anys(read.YieldTools)...), anys(read.YieldMarks)...)
	args = append(append(args, session, Transcripts), anys(read.Writers)...)
	args = append(append(args, anys(read.Shells)...), callArgs...)
	args = append(args, anys(read.YieldTools)...)
	args = append(append(args, anys(read.Shells)...), resultArgs...)
	rows, err := s.db.QueryContext(ctx, `WITH calls(id) AS (SELECT `+toolID+` FROM events INDEXED BY events_session
	    WHERE session = ? AND source = ? AND `+shellCall+`),
	  yields(id) AS (SELECT `+toolID+` FROM events INDEXED BY events_session
	    WHERE session = ? AND source = ? AND kind = 'tool_result' AND lower(tool) IN `+in(read.YieldTools)+` AND `+yieldMarks+`)
	  SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events INDEXED BY events_session WHERE session = ? AND source = ? AND (
	    (`+textPart+` AND kind IN ('owner_text', 'agent_text') AND (agent = '' OR agent = session))
	    OR (`+toolPart+` AND (
	      (kind IN ('tool_call', 'tool_result') AND lower(tool) IN `+in(read.Writers)+`)
	      OR (`+shellCall+`)
	      OR (kind = 'tool_call' AND lower(tool) IN `+in(read.YieldTools)+` AND `+toolID+` IN (SELECT id FROM yields))
	      OR (kind = 'tool_result' AND lower(tool) IN `+in(read.Shells)+` AND `+resultMarks+`
	        AND (`+toolID+` IN (SELECT id FROM calls) OR `+toolID+` IN (SELECT id FROM yields))))))
	  ORDER BY ts, source_key`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var ts int64
		var actor string
		if err := rows.Scan(&e.Source, &e.SourceKey, &ts, &e.Session, &e.Agent, &e.Harness, &e.Model, &e.Cwd, &actor,
			&e.Kind, &e.Tool, &e.Summary, &e.Meta, &e.Ref, &e.Heuristic, &e.Experiment); err != nil {
			return err
		}
		e.TS, e.Actor = time.UnixMilli(ts), Actor(actor)
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// OwnerTurnsBetween are the main-thread owner turns of the transcripts from from to through, both included,
// oldest first: what a turn's copies and twins are found among.
func (s *Store) OwnerTurnsBetween(ctx context.Context, from, through time.Time) ([]Event, error) {
	return scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE kind = 'owner_text' AND ts >= ? AND ts <= ?
	  AND source = ? AND actor = ? AND agent = '' ORDER BY ts, source_key`, from.UnixMilli(), through.UnixMilli(), Transcripts,
		string(ActorOwner)))
}

// TextsHolding are the main-thread texts of the transcripts since a time that hold a text (a handoff's line),
// oldest first. The kind's index serves it.
func (s *Store) TextsHolding(ctx context.Context, since time.Time, text string) ([]Event, error) {
	return scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor, kind,
	  tool, summary, meta, ref, heuristic, experiment FROM events WHERE kind IN ('owner_text', 'agent_text') AND ts >= ?
	  AND source = ? AND (agent = '' OR agent = session) AND instr(summary, ?) > 0 ORDER BY ts, source_key`,
		since.UnixMilli(), Transcripts, text))
}

// AgentParent is the session that names a session as one of its agents, by the newest agent text of that agent up
// to before; "" when none does. The kind's index serves it: a subagent writes text of its own.
func (s *Store) AgentParent(ctx context.Context, agent string, before time.Time) (string, error) {
	var session string
	err := s.db.QueryRowContext(ctx, `SELECT session FROM events WHERE kind = 'agent_text' AND ts <= ? AND source = ? AND agent = ?
	  AND session != agent ORDER BY ts DESC LIMIT 1`, before.UnixMilli(), Transcripts, agent).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return session, err
}

// ChildMeta is a session whose main-thread text since a time names a parent session, with the meta of one such
// text: a session continued from another.
type ChildMeta struct {
	Session, Meta string
}

// Children are the sessions whose main-thread text since a time names a parent session. The kind's index serves
// it: a session that continues another holds text of its own.
func (s *Store) Children(ctx context.Context, since time.Time) ([]ChildMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, MIN(meta) FROM events WHERE kind IN ('owner_text', 'agent_text') AND ts >= ?
	  AND source = ? AND agent = '' AND instr(meta, '"parent":"') > 0 GROUP BY session ORDER BY session`, since.UnixMilli(), Transcripts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChildMeta
	for rows.Next() {
		var c ChildMeta
		if err := rows.Scan(&c.Session, &c.Meta); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PhasesSince are the events of source sessions since a time, oldest first: what the sessions runs saw.
func (s *Store) PhasesSince(ctx context.Context, since time.Time) ([]Event, error) {
	return scanEvents(s.db.QueryContext(ctx, `SELECT source, source_key, ts, session, agent, harness, model, cwd, actor,
	  kind, tool, summary, meta, ref, heuristic, experiment FROM events WHERE source = ? AND ts >= ? ORDER BY ts, source_key`,
		Sessions, since.UnixMilli()))
}
