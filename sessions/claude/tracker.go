// Package claude reads Claude Code sessions: native discovery from processes, tmux panes and Claude Code's own
// state, and an incremental transcript tracker that knows which background commands, subagents, workflows and
// monitors a session started and which ones a task notification has closed.
package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	reBgBash   = regexp.MustCompile(`running in background with ID: (\w+)`)
	reAgentID  = regexp.MustCompile(`agentId: (\w+)`)
	reWorkflow = regexp.MustCompile(`Workflow launched in background\. Task ID: (\w+)`)
	reMonitor  = regexp.MustCompile(`Monitor started \(task (\w+), expires in (\d+)(m|s|h)`)
	reWake     = regexp.MustCompile(`Next wakeup scheduled for [0-9:]+ \(in (\d+)s\)`)
	// reCron is CronCreate's result: a recurring job, or a one-shot task (a one-shot job in older versions), its id,
	// and its schedule, in words in recent versions ("Every 10 minutes"), as the expression in older ones.
	reCron = regexp.MustCompile(`Scheduled (recurring|one-shot) (?:job|task) (\w+) \(([^)]*)\)`)
	// reWakeGone is Claude Code's notice that a restart past a wakeup's time lost it.
	reWakeGone  = regexp.MustCompile(`so that wakeup will not fire`)
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
	Expr    string    `json:"expr,omitempty"`    // what it runs on: CronCreate's input, or the label where a version wrote it
	Version string    `json:"version,omitempty"` // the Claude Code version that made it
	At      time.Time `json:"at"`
	OneShot bool      `json:"one_shot,omitempty"`
	For     string    `json:"for,omitempty"`  // the prompt it runs
	Fired   time.Time `json:"fired,omitzero"` // its last run, by Claude Code's record of it
}

type call struct {
	Name  string    `json:"name"`
	Desc  string    `json:"desc"`
	Label string    `json:"label,omitempty"` // an Agent call's description
	Type  string    `json:"type,omitempty"`  // an Agent call's subagent type
	For   string    `json:"for,omitempty"`   // what a wakeup or a scheduled job is for: its reason, else its prompt
	Cron  string    `json:"cron,omitempty"`  // a CronCreate call's expression
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
	WakeFor  string           `json:"wake_for,omitempty"` // what the pending wakeup is for
	WakeSet  time.Time        `json:"wake_set,omitzero"`  // when it was set
	// WakeSign is why the pending wakeup may have been lost: what came while it waited that Claude Code is known to
	// drop a wakeup for (a task's notification, a compaction).
	WakeSign  string          `json:"wake_sign,omitempty"`
	TurnBegan time.Time       `json:"turn_began,omitzero"` // when the last turn began: an input, not the model's own record
	TurnEnd   time.Time       `json:"turn_end,omitzero"`   // when the last turn ended
	WakeRan   time.Time       `json:"wake_ran,omitzero"`   // when the loop's last wakeup fired
	Crons     map[string]cron `json:"crons"`
	Model     string          `json:"model"`
	UserAt    time.Time       `json:"user_at,omitzero"`    // when user-role text last opened a turn
	Control   time.Time       `json:"control_at,omitzero"` // when its user last ran a local command, interrupted or answered
	Resumed   time.Time       `json:"resumed,omitzero"`    // a resume record since which nothing else happened
	// Request is when the turn sent the model request it still waits on: the last record of the conversation is
	// Claude Code's record of that request, and no reply, error or input has come after it.
	Request time.Time `json:"request,omitzero"`
	// QueuedPrompt is when a queued prompt opened the turn, while nothing of that turn has followed it yet: an
	// interruption written right after it is the Escape that sent it (Claude Code writes the two in either order).
	QueuedPrompt time.Time `json:"queued_prompt,omitzero"`
	// Queue is what Claude Code holds to send when the turn allows, oldest first (queueKey): a message its user
	// typed by its text, "" for anything else it queued (a task's notification).
	Queue []string `json:"queue,omitempty"`
	// Version is the Claude Code version of the latest record: an older one writes no record of a scheduled run.
	Version string `json:"version,omitempty"`
	log     sessions.Log
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
	Subtype      string `json:"subtype"`   // a system record's kind: informational, turn_duration, api_error, ...
	Operation    string `json:"operation"` // a queue operation: enqueue, dequeue, remove, popAll
	Content      any    `json:"content"`   // a queue operation's message, a system record's text
	IsSidechain  bool   `json:"isSidechain"`
	IsMeta       bool   `json:"isMeta"`
	Compact      bool   `json:"isCompactSummary"`
	Timestamp    string `json:"timestamp"`
	Version      string `json:"version"`
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
	if e.Version != "" {
		t.st.Version = e.Version
	}
	open := t.TurnOpen() // before this record answers anything: a reopening closes a turn that was open
	defer func() {
		switch now := t.TurnOpen(); {
		case at.IsZero():
		case open && !now:
			t.st.TurnEnd = at
		case !open && now && e.Type != "assistant": // a reply streamed in parts reopens nothing
			t.st.TurnBegan = at
		}
	}()
	t.answered(e, at)
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
		switch e.Type {
		case "api-request":
			t.st.Request, t.st.QueuedPrompt = at, time.Time{}
		case "queue-operation":
			t.queued(e)
		case "system":
			switch e.Subtype {
			case "scheduled_task_fire":
				t.fired(raw, at)
			case "compact_boundary":
				t.waitedThrough("the conversation was compacted while it waited", at)
			}
		}
		return
	}
	switch e.Type {
	case "assistant":
		t.assistant(e, at, open)
	case "user":
		t.user(e, at, open)
	}
}

// answered clears the request the turn waits on when a record of the conversation follows it: a reply or an
// error (any assistant record), an input (a user record the harness did not inject: a prompt, a tool's result,
// an interruption, a notification), or a system record other than a notice (the turn's end, a retry's error). A
// record dated before the request was part of it, written after it: Claude Code writes the request's own prompt
// or notification so. A notice (a plugin reload, an attachment, a link) answers nothing.
func (t *Tracker) answered(e line, at time.Time) {
	if t.st.Request.IsZero() || !at.IsZero() && at.Before(t.st.Request) {
		return
	}
	switch {
	case e.Type == "assistant", e.Type == "user" && !e.IsMeta, e.Type == "system" && e.Subtype != "informational":
		t.st.Request = time.Time{}
	}
}

// fired follows Claude Code's record of a scheduled run starting: a job CronCreate scheduled, named by its task
// id, or a self-paced loop's wakeup, which says so. A run of anything else (a task scheduled elsewhere) is
// neither.
func (t *Tracker) fired(raw []byte, at time.Time) {
	var f struct {
		Content  string `json:"content"`
		TaskID   string `json:"taskId"`
		TaskKind string `json:"taskKind"`
	}
	json.Unmarshal(raw, &f)
	st := &t.st
	st.TurnBegan = at // the run's prompt opens a turn, recorded as the harness's own
	if c, job := st.Crons[f.TaskID]; job {
		if c.OneShot {
			delete(st.Crons, f.TaskID) // it fires once, then deletes itself
		} else {
			c.Fired = at
			st.Crons[f.TaskID] = c
		}
		return
	}
	if (f.TaskKind == "loop" || strings.Contains(f.Content, "wakeup")) && !st.WakeAt.IsZero() && !at.Before(st.WakeSet) {
		t.clearWake()
		st.WakeRan = at
	}
}

// clearWake ends the pending wakeup: it fired, or Claude Code cancelled it.
func (t *Tracker) clearWake() {
	t.st.WakeAt, t.st.WakeFor, t.st.WakeSign = time.Time{}, "", ""
}

// waitedThrough notes that sign came while a wakeup was pending: a cause Claude Code is known to drop one for, as
// a task's notification that comes after its time when the task ran through it.
func (t *Tracker) waitedThrough(sign string, at time.Time) {
	if st := &t.st; !st.WakeAt.IsZero() && !at.Before(st.WakeSet) && st.WakeSign == "" {
		st.WakeSign = sign
	}
}

// wakeFor is what a wakeup or a scheduled job is for: its reason, else its prompt, unless the prompt is a loop's
// own sentinel (<<autonomous-loop-dynamic>>).
func wakeFor(reason, prompt string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "<<") {
		return ""
	}
	return prompt
}

// Bounds on a job's runs a scan lists: those of the last runsBack, past which no rule acts (the stale hour, the
// slack and the most Claude Code delays a run), and at most runsMax, one a minute.
const (
	runsBack = 2 * time.Hour
	runsMax  = 150
)

// runs are the session's own runs its harness should start, earliest first and the same on every scan: the
// pending wakeup, and each job's runs since it last ran that never started (Claude Code does not start one later)
// with its next. A restart in the foreground loses a pending wakeup; a background session's process arms it
// again, and its start is not known. A restart restores the jobs: a recurring one from its first run after the
// restart, a one-shot only before its time. A recurring job expires 7 days after it was made.
func (t *Tracker) runs(started, now time.Time) []sessions.Run {
	st := &t.st
	if !RecordsRuns(st.Version) {
		return nil // a run that started looks like one that did not
	}
	var out []sessions.Run
	if t.liveWake(started) {
		out = append(out, WakeRun(st.WakeAt, st.WakeSet, st.WakeFor, st.WakeSign, st.WakeRan))
	}
	for id, c := range st.Crons {
		if RecordsRuns(text.FirstNonEmpty(c.Version, st.Version)) { // one an older version made shows none of its runs
			out = append(out, JobRuns(id, text.FirstNonEmpty(c.Expr, c.Label), c.OneShot, c.For, c.At, c.Fired, started, now)...)
		}
	}
	SortRuns(out)
	return out
}

// firstRecording is the first Claude Code version the history shows writing a record of each scheduled run
// (scheduled_task_fire); an older one starts runs without one.
var firstRecording = [3]int{2, 1, 291}

// RecordsRuns reports whether Claude Code at version v records the scheduled runs it starts. An unknown version
// does not.
func RecordsRuns(v string) bool {
	var n [3]int
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(strings.TrimRightFunc(p, func(r rune) bool { return r < '0' || r > '9' }))
		if err != nil {
			return false
		}
		n[i] = x
	}
	for i := range n {
		if n[i] != firstRecording[i] {
			return n[i] > firstRecording[i]
		}
	}
	return true
}

// WakeRun is a pending wakeup as a run, keyed by when it was set; ran is when the loop's last wakeup fired.
func WakeRun(at, set time.Time, what, sign string, ran time.Time) sessions.Run {
	return sessions.Run{Due: at, For: what, Key: fmt.Sprintf("wakeup@%d", set.UnixMilli()), Sign: sign, Ran: ran}
}

// JobRuns are a job's runs as runs does: made at made on the expression expr, last run at fired, in a process
// that started at started (zero when not known). Claude Code restores a session's jobs when it resumes it: a
// recurring one runs from its first run after the restart, a one-shot only before its time.
func JobRuns(id, expr string, oneShot bool, what string, made, fired, started, now time.Time) []sessions.Run {
	key := func(run time.Time) string { return fmt.Sprintf("job:%s@%d", id, run.UnixMilli()) }
	if oneShot {
		if run, _, ok := ScheduledRun(expr, made); ok && (started.IsZero() || !run.Before(started)) {
			return []sessions.Run{{Due: run, For: what, Key: key(run), Job: true, Once: true, Ran: fired}}
		}
		return nil
	}
	if now.Sub(made) > cronExpiry {
		return nil
	}
	after := made
	if fired.After(after) {
		after = fired
	}
	if !started.IsZero() && after.Before(started) {
		after = started // a restored job does not run for the runs it missed
	}
	if back := now.Add(-runsBack); after.Before(back) {
		after = skipTo(expr, after, back) // no rule acts on those before; the runs after keep their keys
	}
	var out []sessions.Run
	for range runsMax {
		run, last, ok := jobRun(expr, after)
		if !ok {
			break
		}
		out = append(out, sessions.Run{Due: last, For: what, Key: key(run), Job: true, Ran: fired})
		if !last.Before(now) {
			break // its next run
		}
		after = run // it never started; the next is spaced from it
	}
	return out
}

// SortRuns orders runs earliest first, a tie by key, so that the order is the same on every scan.
func SortRuns(rs []sessions.Run) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].Due.Equal(rs[j].Due) {
			return rs[i].Due.Before(rs[j].Due)
		}
		return rs[i].Key < rs[j].Key
	})
}

// skipTo moves a job's base from after to back, where the runs it lists begin: a job every five minutes by whole
// gaps, so that its runs keep their times; any other to back itself, since its runs are on its schedule.
func skipTo(expr string, after, back time.Time) time.Time {
	if fiveMinutes(expr) {
		return after.Add(back.Sub(after) / fiveMinuteGap * fiveMinuteGap)
	}
	return back
}

func fiveMinutes(expr string) bool { return strings.Join(strings.Fields(expr), " ") == "*/5 * * * *" }

// jobRun is a recurring job's run after after, and the latest it starts. A job every five minutes runs 4 minutes
// 45 seconds after its last run, or after it was made or restored, on time; any other on its schedule, delayed up
// to the most Claude Code delays it.
func jobRun(expr string, after time.Time) (run, latest time.Time, ok bool) {
	if fiveMinutes(expr) {
		run = after.Add(fiveMinuteGap)
		return run, run, true
	}
	return ScheduledRun(expr, after)
}

// fiveMinuteGap is how long after its last run Claude Code starts a job that runs every five minutes: 15 seconds
// before a five-minute prompt cache expires.
const fiveMinuteGap = 4*time.Minute + 45*time.Second

// liveWake reports a pending wakeup the session's process still holds: one this process set, or any in a session
// whose process start is not known (a background session, whose process arms it again).
func (t *Tracker) liveWake(started time.Time) bool {
	return !t.st.WakeAt.IsZero() && (started.IsZero() || !t.st.WakeSet.Before(started.Add(-2*time.Second)))
}

// queued follows Claude Code's queue of what it sends when the turn allows: a message its user typed while the turn
// ran, or a task's notification. An enqueue names what it adds; a dequeue takes the oldest; a remove names what
// it takes, or takes the oldest; popAll takes everything back to the input line.
func (t *Tracker) queued(e line) {
	st := &t.st
	content, _ := e.Content.(string)
	key := queueKey(content)
	switch e.Operation {
	case "enqueue":
		st.Queue = append(st.Queue, key)
	case "dequeue", "remove":
		i := 0
		if e.Operation == "remove" && content != "" {
			i = slices.Index(st.Queue, key)
		}
		if i >= 0 && i < len(st.Queue) {
			st.Queue = slices.Delete(st.Queue, i, i+1)
		}
	case "popAll":
		st.Queue = nil
	}
}

// queueKey is how the queue keeps an item: a message its user typed by its text, folded and cut; anything else
// Claude Code queues as "": a task's notification, and an item recorded without its text, which is nearly always a
// scheduled run's prompt, another session's message or a notification (the screen's queue hint tells the rare
// command typed meanwhile).
func queueKey(content string) string {
	if strings.TrimSpace(content) == "" || strings.Contains(content, "<task-notification>") {
		return ""
	}
	return text.OneLine(content, 200)
}

// typedQueued counts the messages its user typed that wait in the queue.
func (t *Tracker) typedQueued() int {
	n := 0
	for _, k := range t.st.Queue {
		if k != "" {
			n++
		}
	}
	return n
}

func (t *Tracker) assistant(e line, at time.Time, open bool) {
	var bs []block
	if json.Unmarshal(e.Message.Content, &bs) != nil {
		return
	}
	st := &t.st
	st.QueuedPrompt = time.Time{}
	// Reopening a session whose turn was open (the model owed a reply), Claude Code closes the turn with a
	// synthetic "No response requested." and calls no model. After an interruption or its own retry it writes the
	// same, but the turn was not open then. An API error can carry the same text: a rate limit it has no words for.
	if !e.APIError && e.Message.Model == synthetic && bytes.Contains(e.Message.Content, []byte(noResponse)) {
		if open {
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
			if b.Name == "ScheduleWakeup" || b.Name == "CronCreate" {
				c.For = text.OneLine(wakeFor(text.Str(in["reason"]), text.Str(in["prompt"])), 300)
			}
			if b.Name == "CronCreate" {
				c.Cron = text.Str(in["cron"])
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

func (t *Tracker) user(e line, at time.Time, open bool) {
	if e.Compact { // the summary written at compaction: context for the log, not a turn
		t.st.log.Add(sessions.Entry{Role: sessions.RoleCompacted, Text: text.OneLine(text.BlocksText(e.Message.Content), 1500)})
		t.waitedThrough("the conversation was compacted while it waited", at)
		return
	}
	origin := originLabel(e.origin(), e.PromptSource)
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		t.userText(s, at, e.IsMeta, e.origin(), origin, open)
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
			t.userText(b.Text, at, e.IsMeta, e.origin(), origin, open)
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

// userText reads a user record's text; open is whether a turn was open before the record.
func (t *Tracker) userText(s string, at time.Time, meta bool, kind, origin string, open bool) {
	st := &t.st
	if strings.Contains(s, "<task-notification>") {
		t.notified(s, at)
		st.LastMain, st.QueuedPrompt = "user", time.Time{}
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
			st.LastMain, st.QueuedPrompt = "user", time.Time{}
		}
		return
	}
	if meta {
		return
	}
	clean := strings.TrimSpace(reSystemTag.ReplaceAllString(s, ""))
	if strings.HasPrefix(clean, "[Request interrupted") {
		if q := st.QueuedPrompt; !q.IsZero() && st.LastMain == "user" && !at.IsZero() && at.Sub(q).Abs() <= InterruptSkew {
			// the Escape that sent the queued prompt, written after it: it ended the turn before the prompt, and the
			// prompt's turn runs on, dated from the prompt, as when the interruption comes first; the turn before
			// ended no later than the prompt began
			st.QueuedPrompt = time.Time{}
			end := at
			if at.After(q) {
				end = q
			}
			st.Control, st.TurnEnd, st.TurnBegan = at, end, q
			if !st.WakeAt.IsZero() && !at.Before(st.WakeSet) {
				t.clearWake()
			}
			return
		}
		st.LastMain = "ended"
		st.Group = st.Group[:0]
		st.Control = at // its user pressed Esc, or turned a tool call down
		if !st.WakeAt.IsZero() && !at.Before(st.WakeSet) {
			t.clearWake() // Claude Code cancels a pending loop wakeup on an Esc
		}
		return
	}
	if clean == "" {
		if strings.Contains(s, "<command-name>") {
			st.Control = at // the echo of a local command its user ran: /login, /model
		}
		return
	}
	st.LastMain = "user"
	st.UserAt, st.Resumed = at, time.Time{} // a planned wakeup still fires after a prompt: it stays
	st.QueuedPrompt = time.Time{}
	// Only a queued prompt written while the turn it waited behind still ran can have the Escape that sent it
	// after it: not one a turn's end sent, nor one after the Escape that sent it, nor the second of a batch, whose
	// turn the first opened within the skew.
	if open && at.Sub(st.TurnBegan) > InterruptSkew && strings.HasSuffix(origin, "queued") {
		st.QueuedPrompt = at
	}
	st.log.Add(sessions.Entry{Role: sessions.RoleUser, Origin: origin, Text: text.OneLine(clean, 1200)})
}

func (t *Tracker) notified(s string, at time.Time) {
	if reWakeGone.MatchString(s) {
		t.clearWake()
		return
	}
	t.waitedThrough("a task's notification came in while it waited", at)
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
	st.LastMain, st.QueuedPrompt = "user", time.Time{}
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
		st.WakeAt, st.WakeFor, st.WakeSet, st.WakeSign = at.Add(time.Duration(n)*time.Second), c.For, at, ""
	case c.Name == "ScheduleWakeup" && (strings.HasPrefix(out, "Loop stopped") || strings.HasPrefix(out, "Wakeup not scheduled")):
		t.clearWake() // the loop ended, and its pending wakeup with it
	case c.Name == "CronCreate" && reCron.MatchString(out):
		m := reCron.FindStringSubmatch(out)
		expr := c.Cron
		if expr == "" {
			expr = m[3] // a version that wrote the expression in its result
		}
		st.Crons[m[2]] = cron{Label: m[3], Expr: expr, At: at, OneShot: m[1] == "one-shot", For: c.For, Version: st.Version}
	}
	st.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(out, 160)})
}

func (t *Tracker) close(id string) {
	if k, ok := t.st.Tasks[id]; ok && k.Kind == "agent" {
		t.st.Closed[id] = k
	}
	delete(t.st.Tasks, id)
}

// TurnOpen reports whether the model owes a response, or a tool it called has not returned. A request the turn
// sent and still waits on keeps it open, whatever opened it: a slash command's prompt is no user text of its own.
func (t *Tracker) TurnOpen() bool {
	if !t.st.Request.IsZero() {
		return true
	}
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
	s.RequestAt, s.Queued = t.st.Request, t.typedQueued()
	if s.Model == "" {
		s.Model = t.st.Model
	}
	s.Runs = t.runs(s.Started, now)
	s.TurnBegan, s.IdleSince = t.st.TurnBegan, t.st.TurnEnd
	if t.liveWake(s.Started) && t.st.WakeAt.After(now.Add(-sessions.WakeGrace)) {
		s.WakeAt = t.st.WakeAt
	}
	var newest time.Time
	newestID := ""
	for id, c := range t.st.Crons {
		if now.Sub(c.At) > cronExpiry || (c.OneShot && now.Sub(c.At) > oneShotExpiry) {
			continue // one that expired: a restart restores the others
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
