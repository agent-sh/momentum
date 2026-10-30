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
	"sort"
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

// What Claude Code writes as it reopens a session that ended mid-task: a background task's notice that it did not
// finish before the previous session ended, and the result of a call that the end cut off.
var (
	reOrphan   = regexp.MustCompile(`didn't finish before the previous session ended|<task-id>__orphan_summary__`)
	reCutCall  = regexp.MustCompile(`^\[Tool call interrupted: the session ended before`)
	synthetic  = "<synthetic>"
	noResponse = "No response requested."
	// RateLimit is the error a turn ended on when Claude Code reported a rate limit with no text of its own.
	RateLimit = "rate_limit: " + noResponse
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
	Until  time.Time `json:"until,omitzero"`   // a monitor's expiry
	Output string    `json:"output,omitempty"` // a background command's output file
}

type cron struct {
	Label   string    `json:"label"`
	At      time.Time `json:"at"`
	OneShot bool      `json:"one_shot,omitempty"`
}

type call struct {
	Name  string    `json:"name"`
	Desc  string    `json:"desc"`
	Label string    `json:"label,omitempty"` // an Agent call's description
	Type  string    `json:"type,omitempty"`  // an Agent call's subagent type
	At    time.Time `json:"at"`
}

// Tracker follows one transcript. It reads the file once from the start and then only what was appended, or
// resumes from a checkpoint and reads only what came after it.
type Tracker struct {
	rd jsonl.Reader
	st track

	// The subagents' meta files map a foreground Agent call to its subagent. They are other files, so they are
	// read when a scan needs them and never saved.
	metaSeen time.Time
	byUse    map[string]string
}

// track is what the transcript's lines decided, and nothing else: no scan's clock and no other file, so a
// checkpoint of it fed the rest of the transcript ends where a full re-read ends. A rewritten transcript resets
// it. A checkpoint saves every exported field; the log is saved apart, entry by entry.
type track struct {
	Tasks    map[string]*task `json:"tasks"`
	Closed   map[string]*task `json:"closed"` // finished subagents a SendMessage can wake again
	Calls    map[string]*call `json:"calls"`  // tool_use id -> call, until its result arrives
	Group    []string         `json:"group"`  // tool_use ids of the latest assistant message
	LastMain string           `json:"last_main"`
	LastText string           `json:"last_text"`
	ErrText  string           `json:"err_text"`
	WakeAt   time.Time        `json:"wake_at"`
	Crons    map[string]cron  `json:"crons"`
	Model    string           `json:"model"`
	UserAt   time.Time        `json:"user_at,omitzero"`    // when user-role text last opened a turn
	Control  time.Time        `json:"control_at,omitzero"` // when its user last ran a local command, interrupted or answered
	Resumed  time.Time        `json:"resumed,omitzero"`    // a resume record since which nothing else happened
	log      sessions.Log
}

func newTrack() track {
	var st track
	st.fill()
	return st
}

// fill makes the maps a decoded checkpoint left out.
func (st *track) fill() {
	if st.Tasks == nil {
		st.Tasks = map[string]*task{}
	}
	if st.Closed == nil {
		st.Closed = map[string]*task{}
	}
	if st.Calls == nil {
		st.Calls = map[string]*call{}
	}
	if st.Crons == nil {
		st.Crons = map[string]cron{}
	}
}

// NewTracker follows the transcript at path.
func NewTracker(path string) *Tracker {
	return &Tracker{rd: jsonl.Reader{Path: path}, st: newTrack()}
}

// Path is the transcript.
func (t *Tracker) Path() string { return t.rd.Path }

// Log is the session's activity log.
func (t *Tracker) Log() *sessions.Log { return &t.st.log }

// checkpointState is a Tracker's checkpoint: where it stands in the transcript and what the lines so far decided.
type checkpointState struct {
	Pos   jsonl.Position `json:"pos"`
	Track track          `json:"track"`
}

// State encodes where the tracker stands and what it derived, without its log, for a Checkpoint.
func (t *Tracker) State() ([]byte, error) {
	return json.Marshal(checkpointState{Pos: t.rd.Position(), Track: t.st})
}

// Resume continues from a checkpoint of this transcript. The next Update reads what came after it, or starts
// over when the transcript is no longer the one the checkpoint read.
func (t *Tracker) Resume(cp *sessions.Checkpoint) error {
	var s checkpointState
	if err := json.Unmarshal(cp.State, &s); err != nil {
		return err
	}
	s.Track.fill()
	if cp.Log != nil {
		s.Track.log = *cp.Log
	}
	t.rd.Resume(s.Pos)
	t.st = s.Track
	return nil
}

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
	Error        string `json:"error"` // an API error's kind, as Claude Code names it
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
	// Reopening a session whose turn was open (the model owed a reply), Claude Code closes the turn with a
	// synthetic "No response requested." and calls no model. After an interruption or its own retry it writes the
	// same, but the turn was not open then. An API error can carry the same text: a rate limit it has no words for.
	if !e.APIError && e.Message.Model == synthetic && bytes.Contains(e.Message.Content, []byte(noResponse)) {
		if st.LastMain == "user" {
			st.Resumed = at
		}
	} else if !e.APIError {
		st.Resumed = time.Time{}
	}
	if st.LastMain != "assistant" {
		st.Group = st.Group[:0]
	}
	st.LastMain = "assistant"
	st.ErrText = ""
	if m := e.Message.Model; m != "" && !strings.HasPrefix(m, "<") {
		st.Model = m
	}
	for _, b := range bs {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			st.LastText = b.Text
			if e.APIError {
				st.ErrText = text.OneLine(b.Text, 200)
				if e.Error == "rate_limit" && strings.TrimSpace(b.Text) == noResponse {
					st.ErrText = RateLimit // the text says nothing; the kind says what stopped it
				}
			}
			st.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(b.Text, 900)})
		case "tool_use":
			in := map[string]any{}
			json.Unmarshal(b.Input, &in)
			c := &call{Name: b.Name, At: at, Desc: text.Describe(b.Name, in)}
			if b.Name == "Agent" || b.Name == "Task" {
				c.Label, c.Type = text.Str(in["description"]), text.Str(in["subagent_type"])
			}
			st.Calls[b.ID] = c
			st.Group = append(st.Group, b.ID)
			st.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: b.Name, Text: text.OneLine(c.Desc, 200)})
			switch b.Name {
			case "CronDelete":
				delete(st.Crons, text.Str(in["id"]))
			case "TaskStop":
				t.close(text.Str(in["task_id"]))
			case "SendMessage": // a message to a finished subagent wakes it again
				if k, ok := st.Closed[text.Str(in["to"])]; ok {
					k.LastActive = at
					st.Tasks[k.ID] = k
					delete(st.Closed, k.ID)
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
		st.LastMain = "user"
		if reOrphan.MatchString(s) {
			st.Resumed = at
		}
		return
	}
	// Another session's message is logged as such, never as user text. Claude Code marks it meta, which does not
	// open a turn; the reply that follows does.
	if kind == "peer" || strings.HasPrefix(s, "Another Claude session sent a message") {
		st.log.Add(sessions.Entry{Role: sessions.RolePeer, Text: text.OneLine(reSystemTag.ReplaceAllString(s, ""), 400)})
		if !meta {
			st.LastMain = "user"
		}
		return
	}
	if meta {
		return
	}
	clean := strings.TrimSpace(reSystemTag.ReplaceAllString(s, ""))
	if strings.HasPrefix(clean, "[Request interrupted") {
		st.LastMain = "ended"
		st.Group = st.Group[:0]
		st.Control = at // its user pressed Esc, or turned a tool call down
		return
	}
	if clean == "" {
		if strings.Contains(s, "<command-name>") {
			st.Control = at // the echo of a local command its user ran: /login, /model
		}
		return
	}
	st.LastMain = "user"
	st.UserAt, st.Resumed = at, time.Time{}
	st.WakeAt = time.Time{} // a new prompt: any planned wakeup belongs to its turn now
	st.log.Add(sessions.Entry{Role: sessions.RoleUser, Origin: origin, Text: text.OneLine(clean, 1200)})
}

func (t *Tracker) notified(s string, at time.Time) {
	status := ""
	if m := reStatus.FindStringSubmatch(s); m != nil {
		status = m[1]
	}
	for _, m := range reTaskID.FindAllStringSubmatch(s, -1) {
		if k, ok := t.st.Tasks[m[1]]; ok {
			k.LastActive = at
			if terminal[status] {
				t.close(m[1])
			}
		}
	}
}

func (t *Tracker) result(b block, at time.Time) {
	st := &t.st
	st.LastMain = "user"
	c := st.Calls[b.ToolUseID]
	delete(st.Calls, b.ToolUseID)
	out := text.BlocksText(b.Content)
	if c == nil {
		return
	}
	if (c.Name == "AskUserQuestion" || c.Name == "ExitPlanMode") && !reCutCall.MatchString(out) && !strings.Contains(out, "<tool_use_error>") {
		st.Control = at // its user answered the question, or the plan: whenever the scans ran
	}
	add := func(id, kind string) *task {
		k := &task{Task: sessions.Task{ID: id, Kind: kind, Label: c.Desc, Started: at, LastActive: at}}
		if kind == "agent" {
			k.Label, k.Type = c.Label, c.Type
		}
		st.Tasks[id] = k
		return k
	}
	if b.IsError && reCutCall.MatchString(out) {
		st.Resumed = at
	}
	switch {
	case b.IsError:
	case c.Name == "Bash" && strings.HasPrefix(out, "Command running in background") && reBgBash.MatchString(out):
		k := add(reBgBash.FindStringSubmatch(out)[1], "bash")
		if m := reOutFile.FindStringSubmatch(out); m != nil {
			k.Output = m[1] + ".output"
		}
	case c.Name == "Agent" || c.Name == "Task":
		if m := reAgentID.FindStringSubmatch(out); m != nil && strings.HasPrefix(out, "Async agent launched") {
			add(m[1], "agent")
		}
	case c.Name == "Workflow" && reWorkflow.MatchString(out):
		k := add(reWorkflow.FindStringSubmatch(out)[1], "workflow")
		if i := strings.Index(out, "Summary: "); i >= 0 {
			k.Label = text.OneLine(strings.SplitN(out[i+9:], "\n", 2)[0], 120)
		}
	case c.Name == "Monitor" && reMonitor.MatchString(out):
		m := reMonitor.FindStringSubmatch(out)
		k := add(m[1], "monitor")
		n, _ := strconv.Atoi(m[2])
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[m[3]]
		k.Until = at.Add(time.Duration(n) * unit)
	case c.Name == "ScheduleWakeup" && reWake.MatchString(out):
		n, _ := strconv.Atoi(reWake.FindStringSubmatch(out)[1])
		st.WakeAt = at.Add(time.Duration(n) * time.Second)
	case c.Name == "CronCreate" && reCron.MatchString(out):
		m := reCron.FindStringSubmatch(out)
		st.Crons[m[1]] = cron{Label: m[2], At: at, OneShot: strings.Contains(out, "one-shot")}
	}
	st.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(out, 160)})
}

func (t *Tracker) close(id string) {
	if k, ok := t.st.Tasks[id]; ok && k.Kind == "agent" {
		t.st.Closed[id] = k
	}
	delete(t.st.Tasks, id)
}

// TurnOpen reports whether the model owes a response, or a tool it called has not returned.
func (t *Tracker) TurnOpen() bool {
	switch t.st.LastMain {
	case "user":
		return true
	case "assistant":
		for _, id := range t.st.Group {
			if _, ok := t.st.Calls[id]; ok {
				return true
			}
		}
	}
	return false
}

// pending returns the unresolved tool call of the current turn, if any.
func (t *Tracker) pending() (string, *call) {
	if t.st.LastMain != "assistant" {
		return "", nil
	}
	for i := len(t.st.Group) - 1; i >= 0; i-- {
		if c, ok := t.st.Calls[t.st.Group[i]]; ok {
			return t.st.Group[i], c
		}
	}
	return "", nil
}

// live returns the tasks still running, oldest first, with subagent activity from their own transcripts. Tasks
// from a process older than started died with it; a zero started keeps them (background jobs restart their
// process). It changes nothing the transcript decided: what the clock and the other files say is read again
// each time.
func (t *Tracker) live(started, now time.Time) []sessions.Task {
	dir := strings.TrimSuffix(t.rd.Path, ".jsonl")
	out := []sessions.Task{}
	for _, k := range t.st.Tasks {
		if !started.IsZero() && k.Started.Before(started.Add(-2*time.Second)) {
			continue
		}
		if k.Kind == "monitor" && !k.Until.IsZero() && now.After(k.Until) {
			continue
		}
		v := k.Task
		switch k.Kind {
		case "bash":
			if fi, err := os.Stat(k.Output); err == nil && fi.ModTime().After(v.LastActive) {
				v.LastActive = fi.ModTime()
			}
		case "agent":
			subagentActivity(filepath.Join(dir, "subagents", "agent-"+k.ID+".jsonl"), &v)
		case "workflow":
			if fi, err := os.Stat(filepath.Join(dir, "subagents", "workflow")); err == nil && fi.ModTime().After(v.LastActive) {
				v.LastActive = fi.ModTime()
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.Before(out[j].Started)
		}
		return out[i].ID < out[j].ID
	})
	// A foreground Agent call is a subagent too, running inside the turn.
	if id, c := t.pending(); c != nil && (c.Name == "Agent" || c.Name == "Task") {
		k := sessions.Task{ID: id, Kind: "agent", Label: c.Label, Type: c.Type, Started: c.At, Sync: true}
		if aid := t.agentOf(dir, id); aid != "" {
			subagentActivity(filepath.Join(dir, "subagents", "agent-"+aid+".jsonl"), &k)
		}
		out = append(out, k)
	}
	return out
}

// agentOf returns the subagent Claude Code started for a foreground Agent call, from the subagents' meta files.
// They are read again only when their directory changed.
func (t *Tracker) agentOf(dir, useID string) string {
	sub := filepath.Join(dir, "subagents")
	if fi, err := os.Stat(sub); err == nil && fi.ModTime().After(t.metaSeen) {
		t.metaSeen = fi.ModTime()
		if t.byUse == nil {
			t.byUse = map[string]string{}
		}
		metas, _ := filepath.Glob(filepath.Join(sub, "agent-*.meta.json"))
		for _, m := range metas {
			var meta struct {
				ToolUseID string `json:"toolUseId"`
			}
			if raw, err := os.ReadFile(m); err == nil && json.Unmarshal(raw, &meta) == nil && meta.ToolUseID != "" {
				t.byUse[meta.ToolUseID] = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".meta.json")
			}
		}
	}
	return t.byUse[useID]
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
	s.LastText = strings.TrimSpace(t.st.LastText)
	s.UserAt, s.ControlAt, s.ResumedAt = t.st.UserAt, t.st.Control, t.st.Resumed
	if s.Model == "" {
		s.Model = t.st.Model
	}
	if !t.st.WakeAt.IsZero() && t.st.WakeAt.After(now.Add(-sessions.WakeGrace)) {
		s.WakeAt = t.st.WakeAt
	}
	var newest time.Time
	newestID := ""
	for id, c := range t.st.Crons {
		if (!s.Started.IsZero() && c.At.Before(s.Started)) || now.Sub(c.At) > cronExpiry || (c.OneShot && now.Sub(c.At) > oneShotExpiry) {
			continue // a session-only job of an earlier process, or one that expired
		}
		if c.At.After(newest) || c.At.Equal(newest) && id > newestID {
			newest, newestID, s.Loop = c.At, id, c.Label
		}
	}
	if _, c := t.pending(); c != nil {
		s.Now, s.NowSince = text.OneLine(c.Name+": "+c.Desc, 140), c.At
		switch c.Name {
		case "AskUserQuestion":
			s.Ask, s.AskKind = text.OneLine(c.Desc, 300), sessions.AskQuestion
		case "ExitPlanMode":
			s.Ask, s.AskKind = text.OneLine(c.Desc, 300), sessions.AskPlan
		}
	}
	if t.st.ErrText != "" && !t.TurnOpen() {
		s.Ask, s.AskKind = "stopped on an error: "+t.st.ErrText, sessions.AskError
	}
}
