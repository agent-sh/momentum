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
	Name string    `json:"name"`
	Desc string    `json:"desc"`
	At   time.Time `json:"at"`
}

// Tracker follows one rollout file. It reads the file once from the start and then only what was appended, or
// resumes from a checkpoint and reads only what came after it.
type Tracker struct {
	rd jsonl.Reader
	st track
}

// track is what the rollout's lines decided, and nothing else, so a checkpoint of it fed the rest of the rollout
// ends where a full re-read ends. A checkpoint saves every exported field; the log is saved apart.
type track struct {
	Open     bool             `json:"open"`
	LastText string           `json:"last_text"`
	Calls    map[string]*call `json:"calls"`
	Order    []string         `json:"order"`
	Ask      string           `json:"ask"`
	AskCall  string           `json:"ask_call"`
	AskAsync bool             `json:"ask_async"`
	log      sessions.Log
}

// NewTracker follows the rollout at path.
func NewTracker(path string) *Tracker {
	return &Tracker{rd: jsonl.Reader{Path: path}, st: track{Calls: map[string]*call{}}}
}

// Log is the session's activity log.
func (t *Tracker) Log() *sessions.Log { return &t.st.log }

// Open reports whether a turn is running.
func (t *Tracker) Open() bool { return t.st.Open }

// checkpointState is a Tracker's checkpoint: where it stands in the rollout and what the lines so far decided.
type checkpointState struct {
	Pos   jsonl.Position `json:"pos"`
	Track track          `json:"track"`
}

// State encodes where the tracker stands and what it derived, without its log, for a Checkpoint.
func (t *Tracker) State() ([]byte, error) {
	return json.Marshal(checkpointState{Pos: t.rd.Position(), Track: t.st})
}

// Resume continues from a checkpoint of this rollout. The next Update reads what came after it, or starts over
// when the rollout is no longer the one the checkpoint read.
func (t *Tracker) Resume(cp *sessions.Checkpoint) error {
	var s checkpointState
	if err := json.Unmarshal(cp.State, &s); err != nil {
		return err
	}
	if s.Track.Calls == nil {
		s.Track.Calls = map[string]*call{}
	}
	if cp.Log != nil {
		s.Track.log = *cp.Log
	}
	t.rd.Resume(s.Pos)
	t.st = s.Track
	return nil
}

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
	return t.rd.Read(func() { t.st = track{Calls: map[string]*call{}} }, func(raw []byte) {
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
		st.Open = true
		st.Calls, st.Order = map[string]*call{}, nil
		st.Ask = ""
	case "event_msg/task_complete":
		st.Open = false
		if p.LastAgentMessage != "" {
			st.LastText = p.LastAgentMessage
		}
	case "event_msg/turn_aborted":
		st.Open = false
	case "response_item/message":
		msg := text.BlocksText(p.Content)
		if p.Role == "user" {
			st.Ask = "" // a user message answers the ask, or replaces it with a new instruction
		}
		if p.Role == "assistant" && strings.TrimSpace(msg) != "" {
			st.LastText = msg
			st.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(msg, 900)})
		} else if p.Role == "user" && !strings.HasPrefix(strings.TrimSpace(msg), "<") && !strings.HasPrefix(msg, "# AGENTS.md") {
			st.log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text.OneLine(msg, 1200)})
		}
	case "response_item/function_call", "response_item/custom_tool_call":
		desc := describe(p.Name, p.Arguments+p.Input)
		st.Calls[p.CallID] = &call{Name: p.Name, Desc: desc, At: at}
		st.Order = append(st.Order, p.CallID)
		st.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: p.Name, Text: text.OneLine(desc, 200)})
		if p.Name == "request_user_input" || p.Name == "request_user_input_async" {
			st.Ask, st.AskCall, st.AskAsync = desc, p.CallID, p.Name == "request_user_input_async"
		}
	case "response_item/function_call_output", "response_item/custom_tool_call_output":
		c := st.Calls[p.CallID]
		delete(st.Calls, p.CallID)
		if p.CallID == st.AskCall && c != nil && c.Name == "request_user_input" {
			st.Ask = "" // a blocking ask returns with the answer; an async one returns at once
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
	if !t.st.Open {
		return nil
	}
	for i := len(t.st.Order) - 1; i >= 0; i-- {
		if c, ok := t.st.Calls[t.st.Order[i]]; ok {
			return c
		}
	}
	return nil
}

// Facts reads what was appended and fills the session's deterministic part.
func (t *Tracker) Facts(s *sessions.Session, now time.Time) {
	s.Size = t.Update()
	s.Log = &t.st.log
	s.LastText = strings.TrimSpace(t.st.LastText)
	if c := t.pending(); c != nil {
		s.Now, s.NowSince = text.OneLine(c.Name+": "+c.Desc, 140), c.At
	}
	if t.st.Ask != "" {
		s.Ask, s.AskKind, s.AskAsync = text.OneLine(t.st.Ask, 300), sessions.AskQuestion, t.st.AskAsync
	}
}
