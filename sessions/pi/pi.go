// Package pi reads pi coding agent sessions: a pi process, the session file it writes under the agent
// directory, and an incremental tracker of that file.
package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/jsonl"
	"github.com/agent-sh/momentum/sessions/internal/text"
	"github.com/agent-sh/momentum/sessions/proc"
)

// Retain is how long the tracker of a session that left keeps its place.
const Retain = 15 * time.Minute

type call struct {
	name string
	desc string
	at   time.Time
}

// Tracker follows one pi session file.
type Tracker struct {
	rd jsonl.Reader
	st track
}

type track struct {
	open     bool
	lastText string
	errText  string
	calls    map[string]*call
	log      sessions.Log
	model    string
}

// NewTracker follows the session file at path.
func NewTracker(path string) *Tracker {
	return &Tracker{rd: jsonl.Reader{Path: path}, st: track{calls: map[string]*call{}}}
}

// Log is the session's activity log.
func (t *Tracker) Log() *sessions.Log { return &t.st.log }

type piLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	ModelID   string `json:"modelId"`
	Message   *struct {
		Role       string `json:"role"`
		StopReason string `json:"stopReason"`
		Error      string `json:"errorMessage"`
		ToolCallID string `json:"toolCallId"`
		Content    []struct {
			Type      string         `json:"type"`
			Text      string         `json:"text"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"content"`
	} `json:"message"`
}

// Update reads what was appended; a file that shrank or was replaced is read again from the start.
func (t *Tracker) Update() int64 {
	return t.rd.Read(func() { t.st = track{calls: map[string]*call{}} }, t.line)
}

func (t *Tracker) line(raw []byte) {
	var e piLine
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	st := &t.st
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	if e.Type == "model_change" {
		st.model = e.ModelID
	}
	if e.Type != "message" || e.Message == nil {
		return
	}
	m := e.Message
	switch m.Role {
	case "user":
		st.open, st.errText = true, ""
		for _, c := range m.Content {
			if c.Type == "text" {
				st.log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text.OneLine(c.Text, 1200)})
			}
		}
	case "toolResult":
		delete(st.calls, m.ToolCallID)
		st.open = true
		for _, c := range m.Content {
			if c.Type == "text" {
				st.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(c.Text, 160)})
			}
		}
	case "assistant":
		st.calls = map[string]*call{}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					st.lastText = c.Text
					st.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(c.Text, 900)})
				}
			case "toolCall":
				d := text.Describe(c.Name, c.Arguments)
				st.calls[c.ID] = &call{name: c.Name, desc: d, at: at}
				st.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: c.Name, Text: text.OneLine(d, 200)})
			}
		}
		st.open = m.StopReason == "toolUse"
		if m.StopReason == "error" {
			st.errText = text.OneLine(m.Error, 200)
		}
	}
}

var reCreated = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2})-(\d{2})-(\d{2})-(\d{3})Z_`)

// Created reads the creation time pi puts in a session file's name (2026-09-20T20-34-02-152Z_<id>.jsonl): an ISO
// time with dashes for its colons and its decimal point.
func Created(path string) time.Time {
	m := reCreated.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, m[1]+":"+m[2]+":"+m[3]+"."+m[4]+"Z")
	if err != nil {
		return time.Time{}
	}
	return t
}

// SessionDir is where pi keeps the sessions of a working directory: the leading separator dropped, every other
// separator and colon turned into a dash, wrapped in double dashes.
func SessionDir(agentDir, cwd string) string {
	safe := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.TrimPrefix(strings.TrimPrefix(cwd, "/"), `\`))
	return filepath.Join(agentDir, "sessions", "--"+safe+"--")
}

// Source finds pi processes and reads the session file each one writes.
type Source struct {
	Env      sessions.Env
	trackers map[string]*Tracker
	seen     map[string]time.Time
}

// New reads pi sessions.
func New(env sessions.Env) *Source { return &Source{Env: env} }

func (src *Source) Tool() string { return "pi" }

func (src *Source) Close() error { return nil }

// Scan returns the session of every pi process. A pi process's session is the file created in its window
// (after it started, before the next pi in the same directory started); a resumed session was created earlier,
// so it falls back to the newest file it has written to since it started.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.trackers == nil {
		src.trackers, src.seen = map[string]*Tracker{}, map[string]time.Time{}
	}
	var ps []*proc.Proc
	for _, p := range w.Procs.ByPID {
		if p.Has("-p", "--print") {
			continue // scripted one-shots, not sessions
		}
		if p.Is("pi") || strings.Contains(strings.Join(p.Argv[:min(2, len(p.Argv))], " "), "pi-coding-agent") {
			ps = append(ps, p)
		}
	}
	used := map[string]bool{}
	var out []*sessions.Session
	for _, slot := range w.Procs.Slots(ps) {
		if s := src.session(slot, used, w); s != nil {
			out = append(out, s)
		}
	}
	for path, at := range src.seen {
		if w.Now.Sub(at) > Retain {
			delete(src.trackers, path)
			delete(src.seen, path)
		}
	}
	return out, nil
}

func (src *Source) session(slot proc.Slot, used map[string]bool, w *sessions.World) *sessions.Session {
	p := slot.Proc
	files, _ := filepath.Glob(filepath.Join(SessionDir(src.Env.PiDir, slot.Cwd), "*.jsonl"))
	best, bestT, fallback, fallbackT := "", time.Time{}, "", time.Time{}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || used[f] || fi.ModTime().Before(p.Start.Add(-time.Second)) {
			continue // a session file this process has not written to
		}
		created := Created(f)
		if !created.Before(p.Start.Add(-time.Second)) && (slot.Next.IsZero() || created.Before(slot.Next.Add(-time.Second))) {
			if fi.ModTime().After(bestT) {
				best, bestT = f, fi.ModTime()
			}
		} else if fi.ModTime().After(fallbackT) {
			fallback, fallbackT = f, fi.ModTime()
		}
	}
	if best == "" {
		best, bestT = fallback, fallbackT
	}
	if best == "" {
		return nil
	}
	used[best] = true
	t := src.trackers[best]
	if t == nil {
		t = NewTracker(best)
		src.trackers[best] = t
	}
	src.seen[best] = w.Now
	project := src.Env.Project(slot.Cwd)
	s := &sessions.Session{Key: "pi:" + strings.TrimSuffix(filepath.Base(best), ".jsonl"), Tool: "pi", Name: "pi in " + project,
		Cwd: slot.Cwd, Project: project, Where: "tty", PID: p.PID, Started: p.Start, LastActive: bestT, Tasks: []sessions.Task{},
		Transcript: best}
	s.Size = t.Update()
	s.Log = &t.st.log
	s.Model = t.st.model
	s.LastText = strings.TrimSpace(t.st.lastText)
	s.TurnOpen = t.st.open
	for _, c := range t.st.calls {
		if c.at.After(s.NowSince) {
			s.Now, s.NowSince = text.OneLine(c.name+": "+c.desc, 140), c.at
		}
	}
	if t.st.errText != "" && !t.st.open {
		s.Ask, s.AskKind = "stopped on an error: "+t.st.errText, sessions.AskError
	}
	return s
}
