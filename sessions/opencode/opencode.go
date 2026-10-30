// Package opencode reads opencode sessions: an opencode terminal process, its session in opencode's database,
// open turns, running tools, and subagents as child sessions.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/text"
	"github.com/agent-sh/momentum/sessions/proc"

	_ "modernc.org/sqlite"
)

// Retain is how long the activity log of a session that left keeps its place.
const Retain = 15 * time.Minute

// childWindow is how far back a child session's last update may be for it to count as a running subagent.
const childWindow = 6 * time.Hour

// Source finds opencode terminal processes and reads their sessions from the database.
//
// opencode updates messages and parts in place (each row carries the time it was last updated), so a log resumed
// from a cursor in another process would not be what the database holds. The Source keeps no checkpoints: each
// process reads every message once.
type Source struct {
	Env  sessions.Env
	db   *sql.DB
	logs map[string]*dbLog
	seen map[string]time.Time
}

// dbLog is a session's activity log and how far into its messages it has read.
type dbLog struct {
	log    sessions.Log
	cursor int64
	atCur  map[string]bool // messages read whose time equals cursor: times are milliseconds and can tie
}

// New reads opencode sessions from $XDG_DATA_HOME/opencode/opencode.db.
func New(env sessions.Env) *Source { return &Source{Env: env} }

func (src *Source) Tool() string { return "opencode" }

func (src *Source) Close() error {
	if src.db != nil {
		return src.db.Close()
	}
	return nil
}

// open opens the database read-only once it exists; nil while opencode is not installed here.
func (src *Source) open() *sql.DB {
	if src.db == nil {
		path := filepath.Join(src.Env.DataHome, "opencode", "opencode.db")
		if _, err := os.Stat(path); err == nil {
			src.db, _ = sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(500)")
		}
	}
	return src.db
}

type message struct {
	Role string `json:"role"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	ModelID string `json:"modelID"`
}

type part struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Status string         `json:"status"`
		Input  map[string]any `json:"input"`
		Title  string         `json:"title"`
		Time   struct {
			Start int64 `json:"start"`
		} `json:"time"`
	} `json:"state"`
}

// Scan returns the session of every opencode terminal (not its server, runner or MCP host, and not an opencode
// another one started). A process's session is the one created in its window, else the newest it touched.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.logs == nil {
		src.logs, src.seen = map[string]*dbLog{}, map[string]time.Time{}
	}
	var ps []*proc.Proc
	for _, p := range w.Procs.ByPID {
		if p.Has("-p", "--print") || !p.Is("opencode") || p.Has("serve", "run", "mcp", "--port") {
			continue
		}
		if parent := w.Proc(p.PPID); parent != nil && parent.Is("opencode") {
			continue
		}
		ps = append(ps, p)
	}
	if len(ps) == 0 || src.open() == nil {
		return nil, nil
	}
	used := map[string]bool{}
	var out []*sessions.Session
	var errs []error
	for _, slot := range w.Procs.Slots(ps) {
		s, err := src.session(ctx, slot, used, w)
		errs = append(errs, err)
		if s != nil {
			out = append(out, s)
			src.seen[s.Key] = w.Now
		}
	}
	for k, at := range src.seen {
		if w.Now.Sub(at) > Retain {
			delete(src.logs, k)
			delete(src.seen, k)
		}
	}
	return out, errors.Join(errs...)
}

// session finds and reads the session of one opencode process. A query that fails after the session is found
// still returns what was read, with the error.
func (src *Source) session(ctx context.Context, slot proc.Slot, used map[string]bool, w *sessions.World) (*sessions.Session, error) {
	p, db := slot.Proc, src.db
	start := p.Start.UnixMilli() - 1000
	rows, err := db.QueryContext(ctx, `select id, title, time_created, time_updated from session where directory = ? and parent_id is null
		and time_archived is null and time_updated >= ? order by time_updated desc limit 20`, slot.Cwd, start)
	if err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	var id, title, fid, ftitle string
	var updated, fupdated int64
	for rows.Next() {
		var i, t string
		var c, u int64
		if rows.Scan(&i, &t, &c, &u) != nil || used[i] {
			continue
		}
		inWindow := c >= start && (slot.Next.IsZero() || c < slot.Next.UnixMilli()-1000)
		if inWindow && id == "" {
			id, title, updated = i, t, u
		} else if !inWindow && fid == "" {
			fid, ftitle, fupdated = i, t, u
		}
	}
	err = rows.Err()
	rows.Close()
	if id == "" {
		id, title, updated = fid, ftitle, fupdated
	}
	if id == "" {
		return nil, err
	}
	used[id] = true
	s := &sessions.Session{Key: "opencode:" + id, Tool: src.Tool(), Name: text.FirstNonEmpty(text.OneLine(title, 60), "opencode"),
		Cwd: slot.Cwd, Project: src.Env.Project(slot.Cwd), Where: "tty", PID: p.PID, Started: p.Start,
		LastActive: time.UnixMilli(updated), Tasks: []sessions.Task{}}
	errs := []error{err}
	open, err := src.facts(ctx, id, s)
	s.TurnOpen = open
	errs = append(errs, err)
	l := src.logOf(s.Key)
	errs = append(errs, src.readLog(ctx, id, l))
	s.Log = &l.log
	s.Size = updated
	// subagents are child sessions with a turn still open
	crs, err := db.QueryContext(ctx, `select id, title, time_created, time_updated from session where parent_id = ? and time_updated > ?`,
		id, w.Now.Add(-childWindow).UnixMilli())
	if err != nil {
		s.Partial = true
		return s, errors.Join(append(errs, fmt.Errorf("subagents: %w", err))...)
	}
	type child struct {
		id, title string
		c, u      int64
	}
	var kids []child
	for crs.Next() {
		var k child
		if crs.Scan(&k.id, &k.title, &k.c, &k.u) == nil {
			kids = append(kids, k)
		}
	}
	errs = append(errs, crs.Err())
	crs.Close()
	for _, k := range kids {
		cs := &sessions.Session{}
		open, err := src.facts(ctx, k.id, cs)
		errs = append(errs, err)
		if open {
			s.Tasks = append(s.Tasks, sessions.Task{ID: k.id, Kind: "agent", Label: text.OneLine(k.title, 90),
				Started: time.UnixMilli(k.c), LastActive: time.UnixMilli(k.u), Now: cs.Now})
		}
	}
	err = errors.Join(errs...)
	s.Partial = err != nil // a query of its own failed: its facts may be short
	return s, err
}

func (src *Source) logOf(key string) *dbLog {
	l := src.logs[key]
	if l == nil {
		l = &dbLog{}
		src.logs[key] = l
	}
	return l
}

// facts reads a session's last messages; it reports whether a turn is open. A session without messages has none.
func (src *Source) facts(ctx context.Context, id string, s *sessions.Session) (bool, error) {
	db := src.db
	var data string
	switch err := db.QueryRowContext(ctx, `select data from message where session_id = ? order by time_created desc limit 1`, id).Scan(&data); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("messages: %w", err)
	}
	var last message
	json.Unmarshal([]byte(data), &last)
	open := last.Role == "user" || (last.Role == "assistant" && last.Time.Completed == 0 && last.Error == nil)
	if last.Error != nil && last.Error.Name != "MessageAbortedError" {
		s.Ask, s.AskKind = "stopped on an error: "+text.OneLine(text.FirstNonEmpty(last.Error.Data.Message, last.Error.Name), 200), sessions.AskError
	}
	s.Model = last.ModelID
	rows, err := db.QueryContext(ctx, `select p.data, coalesce(m.data, '') from part p left join message m on m.id = p.message_id
		where p.session_id = ? order by p.time_created desc limit 40`, id)
	if err != nil {
		return open, fmt.Errorf("parts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d, md string
		if rows.Scan(&d, &md) != nil {
			continue
		}
		var p part
		var m message
		json.Unmarshal([]byte(d), &p)
		json.Unmarshal([]byte(md), &m)
		if p.Type == "tool" && s.Now == "" && open && (p.State.Status == "running" || p.State.Status == "pending") {
			s.Now, s.NowSince = text.OneLine(p.Tool+": "+text.FirstNonEmpty(p.State.Title, text.Describe(p.Tool, p.State.Input)), 140), time.UnixMilli(p.State.Time.Start)
		}
		// the last message is the assistant's: a user's text is never evidence of what the agent said
		if p.Type == "text" && m.Role == "assistant" && s.LastText == "" && strings.TrimSpace(p.Text) != "" {
			s.LastText = strings.TrimSpace(p.Text)
		}
	}
	return open, rows.Err()
}

// pageMessages is how many messages one query reads; the log reads page after page until it has them all.
var pageMessages = 2000

// readLog appends the messages finished since the last read, all of them: a log that stopped at one page
// would make its length mean how much was read, not what the session did.
func (src *Source) readLog(ctx context.Context, id string, l *dbLog) error {
	for {
		n, more, err := src.readPage(ctx, id, l)
		if err != nil || !more || n < pageMessages {
			return err
		}
	}
}

// readPage reads one page of messages. more is false when it stopped at a message still streaming, or when the
// page brought nothing new.
func (src *Source) readPage(ctx context.Context, id string, l *dbLog) (n int, more bool, err error) {
	rows, err := src.db.QueryContext(ctx, `select id, time_created, data from message where session_id = ? and time_created >= ?
		order by time_created, id limit ?`, id, l.cursor, pageMessages)
	if err != nil {
		return 0, false, fmt.Errorf("log: %w", err)
	}
	type msg struct {
		id string
		at int64
		m  message
	}
	var ms []msg
	for rows.Next() {
		var x msg
		var data string
		if rows.Scan(&x.id, &x.at, &data) == nil {
			json.Unmarshal([]byte(data), &x.m)
			ms = append(ms, x)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, fmt.Errorf("log: %w", err)
	}
	read := 0
	for _, x := range ms {
		if x.at == l.cursor && l.atCur[x.id] {
			continue
		}
		if x.m.Role == "assistant" && x.m.Time.Completed == 0 && x.m.Error == nil {
			return len(ms), false, nil // still streaming: read it once it is done
		}
		prs, err := src.db.QueryContext(ctx, `select data from part where message_id = ? order by time_created`, x.id)
		if err != nil {
			return len(ms), false, fmt.Errorf("log parts: %w", err)
		}
		for prs.Next() {
			var data string
			if prs.Scan(&data) != nil {
				continue
			}
			var p part
			json.Unmarshal([]byte(data), &p)
			switch {
			case p.Type == "text" && strings.TrimSpace(p.Text) != "" && x.m.Role == "assistant":
				l.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(p.Text, 900)})
			case p.Type == "text" && strings.TrimSpace(p.Text) != "":
				l.log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text.OneLine(p.Text, 1200)})
			case p.Type == "tool":
				l.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: p.Tool, Text: text.OneLine(text.FirstNonEmpty(p.State.Title, text.Describe(p.Tool, p.State.Input)), 200)})
			}
		}
		prs.Close()
		if x.at != l.cursor || l.atCur == nil {
			l.cursor, l.atCur = x.at, map[string]bool{}
		}
		l.atCur[x.id] = true
		read++
	}
	return len(ms), read > 0, nil
}
