// Package codex reads Codex CLI sessions: discovery from processes and the rollout files they hold open, an
// incremental rollout tracker (a turn runs from task_started to task_complete or turn_aborted), and subagents
// from the thread spawn edges in Codex's state database.
package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/jsonl"
	"github.com/agent-sh/momentum/sessions/internal/text"
)

type call struct {
	name string
	desc string
	at   time.Time
}

// Tracker follows one rollout file.
type Tracker struct {
	rd jsonl.Reader
	st track
}

type track struct {
	open     bool
	lastText string
	calls    map[string]*call
	order    []string
	log      sessions.Log
	ask      string
	askCall  string
	askAsync bool
}

// NewTracker follows the rollout at path.
func NewTracker(path string) *Tracker {
	return &Tracker{rd: jsonl.Reader{Path: path}, st: track{calls: map[string]*call{}}}
}

// Log is the session's activity log.
func (t *Tracker) Log() *sessions.Log { return &t.st.log }

// Open reports whether a turn is running.
func (t *Tracker) Open() bool { return t.st.open }

type xLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type             string          `json:"type"`
		Role             string          `json:"role"`
		Name             string          `json:"name"`
		CallID           string          `json:"call_id"`
		Arguments        string          `json:"arguments"`
		Input            string          `json:"input"`
		Output           json.RawMessage `json:"output"`
		Content          json.RawMessage `json:"content"`
		LastAgentMessage string          `json:"last_agent_message"`
	} `json:"payload"`
}

// Update reads what was appended and returns the rollout's size, or -1 when it cannot be read.
func (t *Tracker) Update() int64 {
	return t.rd.Read(func() { t.st = track{calls: map[string]*call{}} }, func(raw []byte) {
		var e xLine
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		t.line(e, at)
	})
}

func (t *Tracker) line(e xLine, at time.Time) {
	st, p := &t.st, e.Payload
	switch e.Type + "/" + p.Type {
	case "event_msg/task_started":
		st.open = true
		st.calls, st.order = map[string]*call{}, nil
		st.ask = ""
	case "event_msg/task_complete":
		st.open = false
		if p.LastAgentMessage != "" {
			st.lastText = p.LastAgentMessage
		}
	case "event_msg/turn_aborted":
		st.open = false
	case "response_item/message":
		msg := text.BlocksText(p.Content)
		if p.Role == "user" {
			st.ask = "" // a user message answers the ask, or replaces it with a new instruction
		}
		if p.Role == "assistant" && strings.TrimSpace(msg) != "" {
			st.lastText = msg
			st.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(msg, 900)})
		} else if p.Role == "user" && !strings.HasPrefix(strings.TrimSpace(msg), "<") && !strings.HasPrefix(msg, "# AGENTS.md") {
			st.log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text.OneLine(msg, 1200)})
		}
	case "response_item/function_call", "response_item/custom_tool_call":
		desc := describe(p.Name, p.Arguments+p.Input)
		st.calls[p.CallID] = &call{name: p.Name, desc: desc, at: at}
		st.order = append(st.order, p.CallID)
		st.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: p.Name, Text: text.OneLine(desc, 200)})
		if p.Name == "request_user_input" || p.Name == "request_user_input_async" {
			st.ask, st.askCall, st.askAsync = desc, p.CallID, p.Name == "request_user_input_async"
		}
	case "response_item/function_call_output", "response_item/custom_tool_call_output":
		c := st.calls[p.CallID]
		delete(st.calls, p.CallID)
		if p.CallID == st.askCall && c != nil && c.name == "request_user_input" {
			st.ask = "" // a blocking ask returns with the answer; an async one returns at once
		}
		out := text.BlocksText(p.Output)
		if out == "" {
			out = string(p.Output)
		}
		st.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(out, 160)})
	}
}

// describe returns the readable part of a Codex tool call's arguments.
func describe(name, args string) string {
	var in map[string]any
	if json.Unmarshal([]byte(args), &in) == nil {
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				return text.FirstNonEmpty(text.Str(q["title"]), text.Str(q["question"]))
			}
		}
		if ms, ok := in["duration_ms"].(float64); ok {
			return fmt.Sprintf("%ds", int(ms/1000))
		}
		for _, k := range []string{"cmd", "command", "task_name", "question", "prompt", "message", "path"} {
			if v, ok := in[k]; ok {
				if s, ok := v.(string); ok && !strings.HasPrefix(s, "gAAAA") {
					return s
				}
				if a, ok := v.([]any); ok {
					parts := make([]string, 0, len(a))
					for _, x := range a {
						parts = append(parts, text.Str(x))
					}
					return strings.Join(parts, " ")
				}
			}
		}
	}
	// exec cells carry code; the first tool call inside is the useful part
	if i := strings.Index(args, "cmd:"); i >= 0 {
		return strings.Trim(strings.SplitN(args[i+4:], ",", 2)[0], "\"{} `")
	}
	return text.OneLine(args, 120)
}

// pending is the latest call the running turn waits on.
func (t *Tracker) pending() *call {
	if !t.st.open {
		return nil
	}
	for i := len(t.st.order) - 1; i >= 0; i-- {
		if c, ok := t.st.calls[t.st.order[i]]; ok {
			return c
		}
	}
	return nil
}

// Facts reads what was appended and fills the session's deterministic part.
func (t *Tracker) Facts(s *sessions.Session, now time.Time) {
	s.Size = t.Update()
	s.Log = &t.st.log
	s.LastText = strings.TrimSpace(t.st.lastText)
	if c := t.pending(); c != nil {
		s.Now, s.NowSince = text.OneLine(c.name+": "+c.desc, 140), c.at
	}
	if t.st.ask != "" {
		s.Ask, s.AskKind, s.AskAsync = text.OneLine(t.st.ask, 300), sessions.AskQuestion, t.st.askAsync
	}
}
