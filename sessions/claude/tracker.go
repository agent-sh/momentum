// Package claude reads Claude Code sessions: native discovery from processes, tmux panes and Claude Code's own
// state, and an incremental transcript tracker that knows which background commands, subagents, workflows and
// monitors a session started and which ones a task notification has closed.
package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/jsonl"
	"github.com/agent-sh/momentum/sessions/internal/text"
)

// What tool results say when work outlives the call that started it.
var (
	reBgBash    = regexp.MustCompile(`running in background with ID: (\w+)`)
	reAgentID   = regexp.MustCompile(`agentId: (\w+)`)
	reWorkflow  = regexp.MustCompile(`Workflow launched in background\. Task ID: (\w+)`)
	reMonitor   = regexp.MustCompile(`Monitor started \(task (\w+), expires in (\d+)(m|s|h)`)
	reWake      = regexp.MustCompile(`Next wakeup scheduled for [0-9:]+ \(in (\d+)s\)`)
	reCron      = regexp.MustCompile(`Scheduled (?:recurring|one-shot) job (\w+) \(([^)]*)\)`)
	reTaskID    = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	reStatus    = regexp.MustCompile(`<status>(\w+)</status>`)
	reOutFile   = regexp.MustCompile(`Output is being written to: (\S+?)\.output`)
	reSystemTag = regexp.MustCompile(`(?s)<(system-reminder|local-command-stdout|local-command-caveat|command-name|command-message|command-args)>.*?</(system-reminder|local-command-stdout|local-command-caveat|command-name|command-message|command-args)>`)
)

// terminal are the task statuses after which a task is over.
var terminal = map[string]bool{"completed": true, "failed": true, "killed": true, "stopped": true, "error": true, "timeout": true, "cancelled": true}

// Durations Claude Code gives scheduled jobs: session-only jobs die with the process, recurring ones expire after
// 7 days, and a one-shot has fired by the next day at the latest.
const (
	cronExpiry    = 7 * 24 * time.Hour
	oneShotExpiry = 24 * time.Hour
)

type task struct {
	sessions.Task
	until  time.Time // a monitor's expiry
	output string    // a background command's output file
}

type cron struct {
	label   string
	at      time.Time
	oneShot bool
}

type call struct {
	name  string
	desc  string
	input map[string]any
	at    time.Time
}

// Tracker follows one transcript. It reads the file once from the start and then only what was appended.
type Tracker struct {
	rd jsonl.Reader
	st track
}

// track is what the tracker knows; a rewritten transcript resets it.
type track struct {
	tasks    map[string]*task
	closed   map[string]*task // finished subagents a SendMessage can wake again
	calls    map[string]*call // tool_use id -> call, until its result arrives
	group    []string         // tool_use ids of the latest assistant message
	lastMain string           // "user", "assistant" or "ended"
	lastText string
	errText  string
	wakeAt   time.Time
	crons    map[string]cron
	log      sessions.Log
	metaSeen time.Time
	byUse    map[string]string // subagent tool_use id -> agent id, from the subagents' meta files
	model    string
}

func newTrack() track {
	return track{tasks: map[string]*task{}, closed: map[string]*task{}, calls: map[string]*call{}, crons: map[string]cron{}, byUse: map[string]string{}}
}

// NewTracker follows the transcript at path.
func NewTracker(path string) *Tracker {
	return &Tracker{rd: jsonl.Reader{Path: path}, st: newTrack()}
}

// Path is the transcript.
func (t *Tracker) Path() string { return t.rd.Path }

// Log is the session's activity log.
func (t *Tracker) Log() *sessions.Log { return &t.st.log }

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type line struct {
	Type         string `json:"type"`
	IsSidechain  bool   `json:"isSidechain"`
	IsMeta       bool   `json:"isMeta"`
	Compact      bool   `json:"isCompactSummary"`
	Timestamp    string `json:"timestamp"`
	APIError     bool   `json:"isApiErrorMessage"`
	PromptSource string `json:"promptSource"`
	Origin       *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func (e line) origin() string {
	if e.Origin == nil {
		return ""
	}
	return e.Origin.Kind
}

// Update reads what was appended since the last call and returns the transcript's size, or -1 when it cannot be
// read.
func (t *Tracker) Update() int64 {
	return t.rd.Read(func() { t.st = newTrack() }, t.line)
}

func (t *Tracker) line(raw []byte) {
	if !bytes.Contains(raw, []byte(`"type"`)) {
		return // not a record; where the field sits in a line is up to Claude Code, so it is not assumed
	}
	var e line
	if json.Unmarshal(raw, &e) != nil || e.IsSidechain {
		return
	}
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	if e.Message == nil {
		// Notifications that arrive mid-turn are queued: recorded as queue operations and attachments.
		if (e.Type == "queue-operation" || e.Type == "attachment") && bytes.Contains(raw, []byte("<task-notification>")) {
			var q struct {
				Content    string `json:"content"`
				Attachment struct {
					Prompt string `json:"prompt"`
				} `json:"attachment"`
			}
			json.Unmarshal(raw, &q)
			t.notified(text.FirstNonEmpty(q.Content, q.Attachment.Prompt), at)
		}
		return
	}
	switch e.Type {
	case "assistant":
		t.assistant(e, at)
	case "user":
		t.user(e, at)
	}
}

func (t *Tracker) assistant(e line, at time.Time) {
	var bs []block
	if json.Unmarshal(e.Message.Content, &bs) != nil {
		return
	}
	st := &t.st
	if st.lastMain != "assistant" {
		st.group = st.group[:0]
	}
	st.lastMain = "assistant"
	st.errText = ""
	if m := e.Message.Model; m != "" && !strings.HasPrefix(m, "<") {
		st.model = m
	}
	for _, b := range bs {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			st.lastText = b.Text
			if e.APIError {
				st.errText = text.OneLine(b.Text, 200)
			}
			st.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(b.Text, 900)})
		case "tool_use":
			in := map[string]any{}
			json.Unmarshal(b.Input, &in)
			c := &call{name: b.Name, input: in, at: at, desc: text.Describe(b.Name, in)}
			st.calls[b.ID] = c
			st.group = append(st.group, b.ID)
			st.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: b.Name, Text: text.OneLine(c.desc, 200)})
			switch b.Name {
			case "CronDelete":
				delete(st.crons, text.Str(in["id"]))
			case "TaskStop":
				t.close(text.Str(in["task_id"]))
			case "SendMessage": // a message to a finished subagent wakes it again
				if k, ok := st.closed[text.Str(in["to"])]; ok {
					k.LastActive = at
					st.tasks[k.ID] = k
					delete(st.closed, k.ID)
				}
			}
		}
	}
}

func (t *Tracker) user(e line, at time.Time) {
	if e.Compact { // the summary written at compaction: context for the log, not a turn
		t.st.log.Add(sessions.Entry{Role: sessions.RoleCompacted, Text: text.OneLine(text.BlocksText(e.Message.Content), 1500)})
		return
	}
	origin := originLabel(e.origin(), e.PromptSource)
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		t.userText(s, at, e.IsMeta, e.origin(), origin)
		return
	}
	var bs []block
	if json.Unmarshal(e.Message.Content, &bs) != nil {
		return
	}
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			t.result(b, at)
		case "text":
			t.userText(b.Text, at, e.IsMeta, e.origin(), origin)
		}
	}
}

// originLabel is Claude Code's own record of where user-role text came from: the origin kind (human, peer, ...)
// and the prompt source (typed, queued, sdk, ...). A program typing into the terminal also records as typed, so
// this labels the text; it does not attribute it.
func originLabel(kind, source string) string {
	switch {
	case kind != "" && source != "":
		return kind + "/" + source
	case kind != "":
		return kind
	}
	return source
}

func (t *Tracker) userText(s string, at time.Time, meta bool, kind, origin string) {
	st := &t.st
	if strings.Contains(s, "<task-notification>") {
		t.notified(s, at)
		st.lastMain = "user"
		return
	}
	// Another session's message is logged as such, never as user text. Claude Code marks it meta, which does not
	// open a turn; the reply that follows does.
	if kind == "peer" || strings.HasPrefix(s, "Another Claude session sent a message") {
		st.log.Add(sessions.Entry{Role: sessions.RolePeer, Text: text.OneLine(reSystemTag.ReplaceAllString(s, ""), 400)})
		if !meta {
			st.lastMain = "user"
		}
		return
	}
	if meta {
		return
	}
	clean := strings.TrimSpace(reSystemTag.ReplaceAllString(s, ""))
	if strings.HasPrefix(clean, "[Request interrupted") {
		st.lastMain = "ended"
		st.group = st.group[:0]
		return
	}
	if clean == "" {
		return
	}
	st.lastMain = "user"
	st.wakeAt = time.Time{} // a new prompt: any planned wakeup belongs to its turn now
	st.log.Add(sessions.Entry{Role: sessions.RoleUser, Origin: origin, Text: text.OneLine(clean, 1200)})
}

func (t *Tracker) notified(s string, at time.Time) {
	status := ""
	if m := reStatus.FindStringSubmatch(s); m != nil {
		status = m[1]
	}
	for _, m := range reTaskID.FindAllStringSubmatch(s, -1) {
		if k, ok := t.st.tasks[m[1]]; ok {
			k.LastActive = at
			if terminal[status] {
				t.close(m[1])
			}
		}
	}
}

func (t *Tracker) result(b block, at time.Time) {
	st := &t.st
	st.lastMain = "user"
	c := st.calls[b.ToolUseID]
	delete(st.calls, b.ToolUseID)
	out := text.BlocksText(b.Content)
	if c == nil {
		return
	}
	add := func(id, kind string) *task {
		k := &task{Task: sessions.Task{ID: id, Kind: kind, Label: c.desc, Started: at, LastActive: at}}
		if kind == "agent" {
			k.Label, k.Type = text.Str(c.input["description"]), text.Str(c.input["subagent_type"])
		}
		st.tasks[id] = k
		return k
	}
	switch {
	case b.IsError:
	case c.name == "Bash" && strings.HasPrefix(out, "Command running in background") && reBgBash.MatchString(out):
		k := add(reBgBash.FindStringSubmatch(out)[1], "bash")
		if m := reOutFile.FindStringSubmatch(out); m != nil {
			k.output = m[1] + ".output"
		}
	case c.name == "Agent" || c.name == "Task":
		if m := reAgentID.FindStringSubmatch(out); m != nil && strings.HasPrefix(out, "Async agent launched") {
			add(m[1], "agent")
		}
	case c.name == "Workflow" && reWorkflow.MatchString(out):
		k := add(reWorkflow.FindStringSubmatch(out)[1], "workflow")
		if i := strings.Index(out, "Summary: "); i >= 0 {
			k.Label = text.OneLine(strings.SplitN(out[i+9:], "\n", 2)[0], 120)
		}
	case c.name == "Monitor" && reMonitor.MatchString(out):
		m := reMonitor.FindStringSubmatch(out)
		k := add(m[1], "monitor")
		n, _ := strconv.Atoi(m[2])
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[m[3]]
		k.until = at.Add(time.Duration(n) * unit)
	case c.name == "ScheduleWakeup" && reWake.MatchString(out):
		n, _ := strconv.Atoi(reWake.FindStringSubmatch(out)[1])
		st.wakeAt = at.Add(time.Duration(n) * time.Second)
	case c.name == "CronCreate" && reCron.MatchString(out):
		m := reCron.FindStringSubmatch(out)
		st.crons[m[1]] = cron{label: m[2], at: at, oneShot: strings.Contains(out, "one-shot")}
	}
	st.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(out, 160)})
}

func (t *Tracker) close(id string) {
	if k, ok := t.st.tasks[id]; ok && k.Kind == "agent" {
		t.st.closed[id] = k
	}
	delete(t.st.tasks, id)
}

// TurnOpen reports whether the model owes a response, or a tool it called has not returned.
func (t *Tracker) TurnOpen() bool {
	switch t.st.lastMain {
	case "user":
		return true
	case "assistant":
		for _, id := range t.st.group {
			if _, ok := t.st.calls[id]; ok {
				return true
			}
		}
	}
	return false
}

// pending returns the unresolved tool call of the current turn, if any.
func (t *Tracker) pending() (string, *call) {
	if t.st.lastMain != "assistant" {
		return "", nil
	}
	for i := len(t.st.group) - 1; i >= 0; i-- {
		if c, ok := t.st.calls[t.st.group[i]]; ok {
			return t.st.group[i], c
		}
	}
	return "", nil
}

// live returns the tasks still running, refreshing subagent activity from their own transcripts. Tasks from a
// process older than started died with it; a zero started keeps them (background jobs restart their process).
func (t *Tracker) live(started, now time.Time) []sessions.Task {
	dir := strings.TrimSuffix(t.rd.Path, ".jsonl")
	t.loadMeta(dir)
	out := []sessions.Task{}
	for id, k := range t.st.tasks {
		if !started.IsZero() && k.Started.Before(started.Add(-2*time.Second)) {
			delete(t.st.tasks, id)
			continue
		}
		if k.Kind == "monitor" && !k.until.IsZero() && now.After(k.until) {
			delete(t.st.tasks, id)
			continue
		}
		switch k.Kind {
		case "bash":
			if fi, err := os.Stat(k.output); err == nil && fi.ModTime().After(k.LastActive) {
				k.LastActive = fi.ModTime()
			}
		case "agent":
			subagentActivity(filepath.Join(dir, "subagents", "agent-"+id+".jsonl"), &k.Task)
		case "workflow":
			if fi, err := os.Stat(filepath.Join(dir, "subagents", "workflow")); err == nil && fi.ModTime().After(k.LastActive) {
				k.LastActive = fi.ModTime()
			}
		}
		out = append(out, k.Task)
	}
	// A foreground Agent call is a subagent too, running inside the turn.
	if id, c := t.pending(); c != nil && (c.name == "Agent" || c.name == "Task") {
		k := sessions.Task{ID: id, Kind: "agent", Label: text.Str(c.input["description"]), Type: text.Str(c.input["subagent_type"]), Started: c.at, Sync: true}
		if aid := t.st.byUse[id]; aid != "" {
			subagentActivity(filepath.Join(dir, "subagents", "agent-"+aid+".jsonl"), &k)
		}
		out = append(out, k)
	}
	return out
}

func (t *Tracker) loadMeta(dir string) {
	sub := filepath.Join(dir, "subagents")
	fi, err := os.Stat(sub)
	if err != nil || !fi.ModTime().After(t.st.metaSeen) {
		return
	}
	t.st.metaSeen = fi.ModTime()
	metas, _ := filepath.Glob(filepath.Join(sub, "agent-*.meta.json"))
	for _, m := range metas {
		var meta struct {
			ToolUseID string `json:"toolUseId"`
		}
		if raw, err := os.ReadFile(m); err == nil && json.Unmarshal(raw, &meta) == nil && meta.ToolUseID != "" {
			t.st.byUse[meta.ToolUseID] = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".meta.json")
		}
	}
}

// subagentActivity sets a subagent's last activity and latest action from the tail of its transcript.
func subagentActivity(path string, k *sessions.Task) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	k.LastActive = fi.ModTime()
	for _, raw := range jsonl.Tail(path, 96<<10) {
		var e line
		if json.Unmarshal(raw, &e) != nil || e.Message == nil || e.Type != "assistant" {
			continue
		}
		var bs []block
		if json.Unmarshal(e.Message.Content, &bs) != nil {
			continue
		}
		for _, b := range bs {
			if b.Type == "tool_use" {
				in := map[string]any{}
				json.Unmarshal(b.Input, &in)
				k.Now = text.OneLine(b.Name+": "+text.Describe(b.Name, in), 110)
			} else if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				k.Now = text.OneLine(b.Text, 110)
			}
		}
	}
}

// Facts reads what was appended and fills the session's deterministic part: tasks, wakeups and loops, the
// pending call and ask, an API error, the last message and the model.
func (t *Tracker) Facts(s *sessions.Session, now time.Time) {
	s.Size = t.Update()
	s.Log = &t.st.log
	s.Tasks = t.live(s.Started, now)
	s.LastText = strings.TrimSpace(t.st.lastText)
	if s.Model == "" {
		s.Model = t.st.model
	}
	if !t.st.wakeAt.IsZero() && t.st.wakeAt.After(now.Add(-sessions.WakeGrace)) {
		s.WakeAt = t.st.wakeAt
	}
	var newest time.Time
	for id, c := range t.st.crons {
		if (!s.Started.IsZero() && c.at.Before(s.Started)) || now.Sub(c.at) > cronExpiry || (c.oneShot && now.Sub(c.at) > oneShotExpiry) {
			delete(t.st.crons, id)
			continue
		}
		if c.at.After(newest) {
			newest, s.Loop = c.at, c.label
		}
	}
	if _, c := t.pending(); c != nil {
		s.Now, s.NowSince = text.OneLine(c.name+": "+c.desc, 140), c.at
		switch c.name {
		case "AskUserQuestion":
			s.Ask, s.AskKind = text.OneLine(c.desc, 300), sessions.AskQuestion
		case "ExitPlanMode":
			s.Ask, s.AskKind = text.OneLine(c.desc, 300), sessions.AskPlan
		}
	}
	if t.st.errText != "" && !t.TurnOpen() {
		s.Ask, s.AskKind = "stopped on an error: "+t.st.errText, sessions.AskError
	}
}
