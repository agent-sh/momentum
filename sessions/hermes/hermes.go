// Package hermes reads Hermes Agent conversations from its state database: turn leases, the latest messages,
// and async delegations as subagents.
package hermes

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/text"

	_ "modernc.org/sqlite"
)

// Human are the sources where a person converses. Their conversations stay listed for Keep after their last
// activity; an automated run (cron, one-shot, kanban, ...) is listed only while a turn of its runs.
var Human = map[string]bool{"cli": true, "tui": true, "desktop": true, "mobile": true, "telegram": true, "webui": true, "whatsapp": true}

// Keep is how long a conversation stays listed after its last activity.
const Keep = 24 * time.Hour

// Retain is how long the activity log of a conversation that left keeps its place.
const Retain = 15 * time.Minute

// Source reads Hermes conversations from $HERMES_HOME/state.db.
type Source struct {
	Env  sessions.Env
	db   *sql.DB
	logs map[string]*dbLog
	seen map[string]time.Time
}

type dbLog struct {
	log    sessions.Log
	cursor int64
}

// New reads Hermes conversations.
func New(env sessions.Env) *Source { return &Source{Env: env} }

func (src *Source) Tool() string { return "hermes" }

func (src *Source) Close() error {
	if src.db != nil {
		return src.db.Close()
	}
	return nil
}

// open opens the state database read-only once it exists; nil while Hermes is not installed here.
func (src *Source) open() *sql.DB {
	if src.db == nil {
		path := filepath.Join(src.Env.HermesHome, "state.db")
		if _, err := os.Stat(path); err == nil {
			src.db, _ = sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(500)")
		}
	}
	return src.db
}

type lease struct {
	pid      int
	live     bool
	terminal bool
}

// Scan returns the conversations to list.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.logs == nil {
		src.logs, src.seen = map[string]*dbLog{}, map[string]time.Time{}
	}
	db := src.open()
	if db == nil {
		return nil, nil
	}
	// A turn lease names the process that runs a turn ("pid=N:...:platform=cli"). A live lease means a turn is
	// running; the last lease of a cli or tui chat also names the terminal process between turns.
	leases := map[string]lease{}
	if rows, err := db.QueryContext(ctx, `select conversation_id, holder, expires_at from session_turn_leases`); err == nil {
		for rows.Next() {
			var id, holder string
			var exp float64
			if rows.Scan(&id, &holder, &exp) != nil {
				continue
			}
			pid := 0
			if v, ok := strings.CutPrefix(holder, "pid="); ok {
				pid, _ = strconv.Atoi(strings.SplitN(v, ":", 2)[0])
			}
			alive := pid > 0 && w.Proc(pid) != nil
			leases[id] = lease{pid: pid, live: exp > float64(w.Now.Unix()) && (pid == 0 || alive),
				terminal: alive && (strings.HasSuffix(holder, "platform=cli") || strings.HasSuffix(holder, "platform=tui"))}
		}
		rows.Close()
	}
	rows, err := db.QueryContext(ctx, `select id, source, coalesce(title, ''), coalesce(display_name, ''), coalesce(cwd, ''),
		coalesce(model, ''), started_at, coalesce(last_activity_at, started_at), coalesce(last_activity_description, '')
		from sessions where ended_at is null and coalesce(archived, 0) = 0 and coalesce(hidden, 0) = 0
		and coalesce(last_activity_at, started_at) > ? and source != 'subagent'`, float64(w.Now.Add(-Keep).Unix()))
	if err != nil {
		return nil, err
	}
	type row struct {
		id, source, title, display, cwd, model, act string
		started, last                               float64
	}
	var hs []row
	for rows.Next() {
		var h row
		if rows.Scan(&h.id, &h.source, &h.title, &h.display, &h.cwd, &h.model, &h.started, &h.last, &h.act) == nil {
			hs = append(hs, h)
		}
	}
	rows.Close()
	var out []*sessions.Session
	for _, h := range hs {
		l := leases[h.id]
		if !Human[h.source] && !l.live {
			continue
		}
		s := &sessions.Session{Key: "hermes:" + h.id, Tool: "hermes", Auto: !Human[h.source],
			Name: text.FirstNonEmpty(text.OneLine(h.title, 60), text.OneLine(h.display, 60), "hermes "+h.source), Cwd: h.cwd,
			Project: h.source, Where: h.source, Model: h.model, Started: epoch(h.started), LastActive: epoch(h.last), Tasks: []sessions.Task{}}
		if l.terminal && (h.source == "cli" || h.source == "tui") {
			s.PID = l.pid // only a terminal chat can be opened or typed into
		}
		s.TurnOpen = src.facts(ctx, h.id, s) || l.live
		lg := src.logOf(s.Key)
		src.readLog(ctx, h.id, lg)
		s.Log = &lg.log
		if s.TurnOpen && s.Now == "" && h.act != "" {
			s.Now, s.NowSince = text.OneLine(h.act, 140), s.LastActive
		}
		s.Size = int64(h.last * 1000)
		s.Tasks = append(s.Tasks, src.delegations(ctx, h.id, w)...)
		out = append(out, s)
		src.seen[s.Key] = w.Now
	}
	for k, at := range src.seen {
		if w.Now.Sub(at) > Retain {
			delete(src.logs, k)
			delete(src.seen, k)
		}
	}
	return out, nil
}

func (src *Source) logOf(key string) *dbLog {
	l := src.logs[key]
	if l == nil {
		l = &dbLog{}
		src.logs[key] = l
	}
	return l
}

// facts reads the latest messages; it reports whether a turn is open.
func (src *Source) facts(ctx context.Context, id string, s *sessions.Session) bool {
	rows, err := src.db.QueryContext(ctx, `select role, coalesce(content, ''), coalesce(tool_calls, ''), timestamp
		from messages where session_id = ? and role in ('user', 'assistant', 'tool') order by id desc limit 12`, id)
	if err != nil {
		return false
	}
	defer rows.Close()
	first, open := true, false
	for rows.Next() {
		var role, content, calls string
		var at float64
		if rows.Scan(&role, &content, &calls, &at) != nil {
			continue
		}
		hasCalls := calls != "" && calls != "[]"
		if first {
			open = role == "user" || role == "tool" || (role == "assistant" && hasCalls)
			if role == "assistant" && hasCalls {
				for _, c := range toolCalls(calls) {
					s.Now, s.NowSince = text.OneLine(c[0]+": "+c[1], 140), epoch(at)
				}
			}
			first = false
		}
		if role == "assistant" && strings.TrimSpace(content) != "" && s.LastText == "" {
			s.LastText = strings.TrimSpace(content)
		}
	}
	return open
}

func toolCalls(raw string) [][2]string {
	var calls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	json.Unmarshal([]byte(raw), &calls)
	var out [][2]string
	for _, c := range calls {
		in := map[string]any{}
		json.Unmarshal([]byte(c.Function.Arguments), &in)
		out = append(out, [2]string{c.Function.Name, text.Describe(c.Function.Name, in)})
	}
	return out
}

// readLog appends the messages written since the last read.
func (src *Source) readLog(ctx context.Context, id string, l *dbLog) {
	rows, err := src.db.QueryContext(ctx, `select id, role, coalesce(content, ''), coalesce(tool_calls, '') from messages
		where session_id = ? and id > ? and role in ('user', 'assistant', 'tool') order by id limit 3000`, id, l.cursor)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var mid int64
		var role, content, calls string
		if rows.Scan(&mid, &role, &content, &calls) != nil {
			continue
		}
		switch role {
		case "user":
			l.log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text.OneLine(content, 1200)})
		case "assistant":
			if strings.TrimSpace(content) != "" {
				l.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(content, 900)})
			}
			for _, c := range toolCalls(calls) {
				l.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: c[0], Text: text.OneLine(c[1], 200)})
			}
		case "tool":
			l.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(content, 160)})
		}
		l.cursor = mid
	}
}

// delegations returns a conversation's async delegations still in flight. One whose owner process is gone (or
// whose pid now names another process: the start time differs) will never complete, so it is left out.
func (src *Source) delegations(ctx context.Context, id string, w *sessions.World) []sessions.Task {
	const q = `select delegation_id, coalesce(task_json, ''), dispatched_at, updated_at, coalesce(owner_pid, 0), %s
		from async_delegations where (origin_session_id = ? or origin_session = ? or parent_session_id = ?)
		and state not in ('completed', 'failed', 'cancelled', 'canceled', 'error', 'timeout')`
	rows, err := src.db.QueryContext(ctx, fmt.Sprintf(q, "coalesce(owner_started_at, 0)"), id, id, id)
	if err != nil { // a database from before owner start times
		rows, err = src.db.QueryContext(ctx, fmt.Sprintf(q, "0"), id, id, id)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []sessions.Task
	for rows.Next() {
		var did, task string
		var disp, upd float64
		var owner int
		var ownerStart int64
		if rows.Scan(&did, &task, &disp, &upd, &owner, &ownerStart) != nil {
			continue
		}
		if owner > 0 && !w.Procs.Alive(owner, uint64(max(ownerStart, 0))) {
			continue
		}
		var tj struct {
			Goal string `json:"goal"`
			Role string `json:"role"`
		}
		json.Unmarshal([]byte(task), &tj)
		out = append(out, sessions.Task{ID: did, Kind: "agent", Label: text.OneLine(tj.Goal, 90), Type: tj.Role, Started: epoch(disp), LastActive: epoch(upd)})
	}
	return out
}

// epoch reads Hermes's float Unix seconds.
func epoch(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(f * 1000))
}
