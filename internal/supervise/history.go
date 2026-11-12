package supervise

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
	"github.com/agent-sh/momentum/sessions/codex"
)

// Point is a session at one stop in the store's history, and what came next.
type Point struct {
	Session string // the store's session id
	Stop    Stop
	Next    Next
	Entry   string // where the owner's turns came from (the harness's entrypoint), when the store says
	// Waiting marks a turn that waited on its model's reply at least stallFloor, where no input ended the wait:
	// the reply came (Next is the session going on by itself), or the session was reopened, which closed the turn
	// (Next is its owner's restart). Only the stalled rule reads it (PlanOnly).
	Waiting bool
	// Controls are the owner's local commands after the stop, before what came next, oldest first. Each shows him
	// at the session from then, so it holds a rule for the owner window, but only one that came before the rule
	// would have acted (Replay): by then a live run had acted already.
	Controls []time.Time
}

// stallFloor is how long a turn must have waited on its model's reply to be a point of its own: no wait the
// stalled rule could use is shorter, and every turn waits on its model for a moment.
const stallFloor = time.Minute

// Next is the input that ended a stop: the owner's (a turn, a slash command, a shell command, an interruption),
// the session's own (a task's notification, a scheduled wakeup, another session's message), momentum's own
// message, or none before the window ended.
type Next struct {
	At        time.Time
	Kind      string // owner, self, momentum, none
	InputKind string // the owner's: typed, queued, slash, shell, answer, interrupt, exit; restart for a reopening
	Text      string
}

// The store keeps an agent's text by its ends, a tool's result by its start (Transcripts source). What these
// say about work outliving the call that started it fits in what is kept.
var (
	hBgBash    = regexp.MustCompile(`running in background with ID: (\w+)|moved to the background \(ID: (\w+)\)`)
	hAgent     = regexp.MustCompile(`Async agent launched.*?agentId: (\w+)`)
	hWorkflow  = regexp.MustCompile(`Workflow launched in background\. Task ID: (\w+)`)
	hMonitor   = regexp.MustCompile(`Monitor started \(task (\w+), expires in (\d+)(m|s|h)`)
	hWake      = regexp.MustCompile(`Next wakeup scheduled for [0-9:]+ \(in (\d+)s\)`)
	hCron      = regexp.MustCompile(`Scheduled (recurring|one-shot) (?:job|task) (\w+) \(([^)]*)\)`)
	hWakeGone  = regexp.MustCompile(`so that wakeup will not fire`)
	hCronExpr  = regexp.MustCompile(`"cron":"([^"]*)"`)
	hTaskID    = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	hStatus    = regexp.MustCompile(`<status>(\w+)</status>`)
	hTaskStop  = regexp.MustCompile(`"task_id":"(\w+)"`)
	hCronDel   = regexp.MustCompile(`"id":"(\w+)"`)
	hReason    = regexp.MustCompile(`"reason":"((?:[^"\\]|\\.)*)`)
	hPrompt    = regexp.MustCompile(`"prompt":"((?:[^"\\]|\\.)*)`)
	hCutCall   = regexp.MustCompile(`^\[Tool call interrupted: the session ended before`)
	terminalSt = map[string]bool{"completed": true, "failed": true, "killed": true, "stopped": true, "error": true, "timeout": true, "cancelled": true}
)

// A Claude Code local command's echo is its command tags and nothing else, with the tags the live tracker strips
// (sessions/claude); a prompt it expands to has text of its own.
var (
	echoTags      = []string{"system-reminder", "local-command-stdout", "local-command-caveat", "command-name", "command-message", "command-args"}
	hCommandTag   = regexp.MustCompile(`(?s)<(?:` + strings.Join(echoTags, "|") + `)>.*?</(?:` + strings.Join(echoTags, "|") + `)>`)
	hCommandName  = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	hSkillRunning = regexp.MustCompile(`<command-message>[^<]*\bis running`)
)

// localEcho reports a local command's echo: a record the store marks so whose text is only the command's tags. The
// store keeps 240 bytes of a record (a caveat or a reminder alone can be longer) and marks a cut one with "…":
// then the record is read whole from its transcript while it is there, else a cut that ends inside one of the tags
// still reads as an echo.
// It returns the text it judged: the record whole when it read it, else the summary, cut or not.
func localEcho(e store.Event) (string, bool) {
	cut, ok := strings.CutSuffix(e.Summary, "…")
	if !ok {
		return e.Summary, echoOnly(e.Summary)
	}
	if text, ok := recordText(e.Ref); ok {
		return text, echoOnly(text)
	}
	rest := strings.TrimSpace(hCommandTag.ReplaceAllString(cut, ""))
	if rest == "" {
		return cut, true // the cut came between two of the tags
	}
	for _, tag := range echoTags {
		open := "<" + tag + ">"
		if strings.HasPrefix(rest, open) && !strings.Contains(rest, "</"+tag+">") || strings.HasPrefix(open, rest) {
			return cut, true // the cut came inside the tag, or inside its name
		}
	}
	return cut, false
}

// echoOnly reports a text that holds a command's name and nothing but the echo's tags. A skill's echo ("<command-
// message>NAME is running…") is its prompt's, which opens a turn, whatever the record next to it.
func echoOnly(s string) bool {
	return strings.Contains(s, "<command-name>") && !hSkillRunning.MatchString(s) && strings.TrimSpace(hCommandTag.ReplaceAllString(s, "")) == ""
}

// recordText is the text of a user record in its transcript (Ref: path@offset#index): its message's content, a
// string or text blocks. False when the transcript is gone or the record holds no text.
func recordText(ref string) (string, bool) {
	line := recordAt(ref)
	if line == nil {
		return "", false
	}
	var r struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil {
		return "", false
	}
	content := r.Message.Content
	if len(content) == 0 {
		return "", false
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text, text != ""
	}
	var blocks []struct {
		Type, Text string
	}
	if json.Unmarshal(content, &blocks) != nil {
		return "", false
	}
	// the event is one block of the record (Ref's #index): judged by its own text, not the record's joined
	if i := blockIndex(ref); i >= 0 && i < len(blocks) && blocks[i].Type == "text" {
		return blocks[i].Text, blocks[i].Text != ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	if i := blockIndex(ref); i >= 0 && i < len(parts) {
		return parts[i], parts[i] != ""
	}
	return strings.Join(parts, "\n"), len(parts) > 0
}

// blockIndex is the index of an event's item in its record (Ref: path@offset#index), or -1.
func blockIndex(ref string) int {
	i := strings.LastIndexByte(ref, '#')
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// role is what one event is in a session's turns.
func role(e store.Event, m map[string]any) string {
	why, _ := m["why"].(string)
	ik, _ := m["input_kind"].(string)
	switch e.Kind {
	case "owner_text":
		return "owner"
	case "agent_text":
		if why == "origin peer" || why == "harness line" {
			// another session's message, which tacit credits to an agent when it does not know the tag: an input of the
			// session's own that the model answers, not the model's words. The store marks it by its origin (origin
			// peer), which the live tracker reads too, or a harness line when Claude Code queued it as meta, whatever
			// tag it opens with. tacit gives that mark only to a line the harness wrote (a meta queued command, a line
			// shown in the transcript only; a meta line it marks a meta line), never to the model's words.
			return "self"
		}
		return "agent"
	case "tool_call":
		return "call"
	case "tool_result":
		return "result"
	case "compaction":
		return "compaction"
	}
	isErr, _ := m["is_error"].(bool)
	s := e.Summary
	switch {
	case isErr && why == "api error":
		return "apierr"
	case why == "synthetic message":
		return "synthetic"
	case why == "momentum supervise":
		return "momentum"
	case claude.Notification(s):
		return "self"
	case strings.HasPrefix(s, "# Autonomous loop tick"), m["prior"] == "scheduled_task_fire":
		return "self"
	case claude.PeerMessage(s), why == "origin peer", peerOrigin(e, why):
		return "self"
	case strings.HasPrefix(why, "hermes notice "):
		if strings.Contains(why, "steer") {
			return "owner"
		}
		if strings.Contains(why, "notification") || strings.Contains(why, "complete") {
			return "self"
		}
	case why == "local command":
		// a Claude Code local command's echo (/login, /model), which never reaches the model: its user was at the
		// session, and no turn opened (the live layer's ControlAt). /exit and /quit end the process. Its name is read
		// from the text the echo was judged by, the record whole when the store cut it.
		text, echo := localEcho(e)
		if !echo {
			if !strings.Contains(text, "<command-name>") && strings.TrimSpace(hCommandTag.ReplaceAllString(text, "")) == "" {
				return "" // a block of the record that is only a reminder or a caveat: no command, and no prompt
			}
			if ik == "slash" || ik == "shell" {
				return "owner"
			}
			return ""
		}
		if g := hCommandName.FindStringSubmatch(text); g != nil && (g[1] == "/exit" || g[1] == "/quit") {
			return "exit"
		}
		return "control"
	case ik == "slash", ik == "shell":
		return "owner"
	case strings.HasPrefix(s, "[Request interrupted"):
		return "interrupt"
	case e.Harness == "codex" && strings.HasPrefix(s, "<turn_aborted>"):
		return "aborted" // Codex's record that its user interrupted the turn: it is over, and no input came yet
	}
	return ""
}

// fromPeer reports an event of another session's message: by its text, by the store's mark of a record Claude Code
// says came from a peer (tacit's origin peer), as an agent's text the store marks a harness line, a message another
// session sent that Claude Code queued as meta, whatever tag it opens with, or by the origin of a meta record's
// line in its transcript (peerOrigin).
func fromPeer(e store.Event, m map[string]any) bool {
	why, _ := m["why"].(string)
	return why == "origin peer" || e.Kind == "agent_text" && why == "harness line" || claude.PeerMessage(e.Summary) || peerOrigin(e, why)
}

// peerOrigin reports a Claude Code meta record that came from another session, as the live tracker reads it: its
// origin, peer, read from its line in the transcript while the transcript is there (a user record's origin, a queued
// command's attachment's). tacit marks a meta line (meta line) and a meta queued command (harness line) before it
// reads the origin, so the store does not keep it; without the transcript such a record is read by its text alone.
func peerOrigin(e store.Event, why string) bool {
	if e.Harness != "claude" || why != "meta line" && why != "harness line" {
		return false
	}
	line := recordAt(e.Ref)
	if line == nil {
		return false
	}
	type origin struct {
		Kind string `json:"kind"`
	}
	var r struct {
		Origin     origin `json:"origin"`
		Attachment struct {
			Origin origin `json:"origin"`
		} `json:"attachment"`
	}
	return json.Unmarshal(line, &r) == nil && (r.Origin.Kind == "peer" || r.Attachment.Origin.Kind == "peer")
}

type replayTask struct {
	kind  string
	until time.Time // a monitor's expiry
}

// replayCron is a job CronCreate scheduled, as the store keeps it: its expression, when it was made and last ran,
// what it runs, and whether its harness records its runs.
type replayCron struct {
	expr      string
	label     string // what its result calls it, which the live tracker shows as a stop's loop
	at, fired time.Time
	oneShot   bool
	what      string
	seen      bool
}

// replayCall is what a ScheduleWakeup or a CronCreate call asked for: what it is for (its reason, else its prompt,
// unless the prompt is a loop's own sentinel), and a CronCreate call's expression.
type replayCall struct {
	what, cron string
}

// callOf reads a ScheduleWakeup or a CronCreate call's input from its record in the transcript while it is there
// (the store keeps the start of it), else from the store's summary.
func callOf(e store.Event, id string) replayCall {
	var in map[string]any
	if raw := recordAt(e.Ref); raw != nil {
		var rec struct {
			Message struct {
				Content []struct {
					Type  string         `json:"type"`
					ID    string         `json:"id"`
					Input map[string]any `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &rec) == nil {
			for _, b := range rec.Message.Content {
				if b.Type == "tool_use" && b.ID == id {
					in = b.Input
				}
			}
		}
	}
	if in == nil {
		in = map[string]any{}
		for k, re := range map[string]*regexp.Regexp{"reason": hReason, "prompt": hPrompt, "cron": hCronExpr} {
			if g := re.FindStringSubmatch(e.Summary); g != nil {
				in[k] = strings.ReplaceAll(strings.ReplaceAll(g[1], `\"`, `"`), `\\`, `\`)
			}
		}
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	c := replayCall{cron: str("cron"), what: str("reason")}
	if c.what == "" && !strings.HasPrefix(strings.TrimSpace(str("prompt")), "<<") {
		c.what = str("prompt")
	}
	return c
}

// recordAt is the line of an event's record in the transcript (Ref: path@offset#index), nil when it is gone.
func recordAt(ref string) []byte {
	path, off, ok := refPlace(ref)
	if !ok {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	line, err := bufio.NewReaderSize(f, 1<<16).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil
	}
	return line
}

// replayFire is Claude Code's record of a scheduled run starting, as the session layer reads it.
type replayFire struct {
	TaskID   string `json:"taskId"`
	TaskKind string `json:"taskKind"`
	Content  string `json:"content"`
}

// fireBetween is the last scheduled_task_fire record in the transcript between two events' places; false when the
// transcript is gone or holds none there.
func fireBetween(from, to string) (replayFire, bool) {
	path, off, ok := refPlace(from)
	p2, end, ok2 := refPlace(to)
	if !ok || !ok2 || p2 != path || end < off {
		return replayFire{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return replayFire{}, false
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return replayFire{}, false
	}
	rd := bufio.NewReaderSize(io.LimitReader(f, min(end-off, requestLimit)), 1<<16)
	var fire replayFire
	found := false
	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 && strings.Contains(string(raw), "scheduled_task_fire") {
			var e struct {
				replayFire
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Subtype == "scheduled_task_fire" {
				fire, found = e.replayFire, true
			}
		}
		if err != nil {
			return fire, found
		}
	}
}

// abortedBy is when a Codex turn the store marked turn_aborted on a later record (to) was aborted: the rollout's
// event_msg turn_aborted between the record before it (from) and that record, read forward while the transcript is
// there, else the marked record's own time, the latest the abort can have come. A time outside the two records' is
// not taken.
func abortedBy(from, to string, after, latest time.Time) time.Time {
	path, off, ok := refPlace(from)
	p2, end, ok2 := refPlace(to)
	if !ok || !ok2 || p2 != path || end < off {
		return latest
	}
	f, err := evidence.OpenAt(path, off) // the file, or the copy Codex compressed it into
	if err != nil {
		return latest
	}
	defer f.Close()
	rd := bufio.NewReaderSize(io.LimitReader(f, min(end-off, requestLimit)), 1<<16)
	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 && strings.Contains(string(raw), "turn_aborted") {
			var e struct {
				Timestamp string `json:"timestamp"`
				Type      string `json:"type"`
				Payload   struct {
					Type string `json:"type"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Type == "event_msg" && e.Payload.Type == "turn_aborted" {
				if at, perr := time.Parse(time.RFC3339Nano, e.Timestamp); perr == nil && !at.Before(after) && !at.After(latest) {
					return at
				}
			}
		}
		if err != nil {
			return latest
		}
	}
}

// codexTurnEnd is how the turn that a Codex session's last record (from) belongs to ended, read forward in the
// rollout after that record while the transcript is there, its compressed copy among them: the first task_complete
// or turn_aborted after it, with its time and task_complete's last_agent_message, in the rollout's order, as the
// live tracker reads it. An agent message the store keeps no event of is read past, and so is the first task_started
// when opened says the last record is an input that opened its own turn, which Codex may start after writing the
// input. "" when the transcript is gone, or the turn is still open by the store's records: another turn's
// task_started, or a message or call the store has not read yet (an ingest behind the rollout), comes first, or the
// end is at or after until.
func codexTurnEnd(from string, opened bool, until time.Time) (end string, at time.Time, last string) {
	path, off, ok := refPlace(from)
	if !ok {
		return "", time.Time{}, ""
	}
	f, err := evidence.OpenAt(path, off)
	if err != nil {
		return "", time.Time{}, ""
	}
	defer f.Close()
	rd := bufio.NewReaderSize(io.LimitReader(f, requestLimit), 1<<16)
	if _, err := rd.ReadBytes('\n'); err != nil { // the record itself
		return "", time.Time{}, ""
	}
	for {
		raw, err := rd.ReadBytes('\n')
		var e struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type             string          `json:"type"`
				Role             string          `json:"role"`
				Content          json.RawMessage `json:"content"`
				LastAgentMessage string          `json:"last_agent_message"`
			} `json:"payload"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &e) == nil {
			switch {
			case e.Type == "response_item" && e.Payload.Type == "message" && e.Payload.Role == "assistant" && !codexKept(e.Payload.Content):
				// an agent message the store keeps no event of: no record of the turn is missing from it
			case e.Type == "response_item" && codexItems[e.Payload.Type]:
				return "", time.Time{}, "" // a record of the turn the store has not read: its records decide
			case e.Type == "event_msg" && e.Payload.Type == "task_started":
				if opened { // the start of the turn the input opened, which Codex wrote after the input
					opened = false
					break
				}
				return "", time.Time{}, ""
			case e.Type == "event_msg" && (e.Payload.Type == "task_complete" || e.Payload.Type == "turn_aborted"):
				if at, perr := time.Parse(time.RFC3339Nano, e.Timestamp); perr == nil && at.Before(until) {
					return e.Payload.Type, at, e.Payload.LastAgentMessage
				}
				return "", time.Time{}, ""
			}
		}
		if err != nil {
			return "", time.Time{}, ""
		}
	}
}

// codexStartedBetween reports whether a Codex rollout has a task_started between two events' records (from, to),
// read while the transcript is there, its compressed copy among them: the turn of the input at to had started before
// it. With sinceEnd, only a start after the stretch's last task_complete or turn_aborted counts: one before it is a
// turn that ended before the input (an earlier input's, aborted before the store kept anything of it). With no from
// (the input is the window's first event), the stretch is the one before to, as far back as it is read, read so.
// True when that cannot be read (the transcript is gone, the two are not in one rollout in that order, too far
// apart, or with no from no end in that stretch), so a start after the input is not taken for its own.
func codexStartedBetween(from, to string, sinceEnd bool) bool {
	p2, end, ok2 := refPlace(to)
	path, off, ok := refPlace(from)
	back := from == ""
	sinceEnd = sinceEnd || back
	if back {
		path, off, ok = p2, max(0, end-requestLimit), ok2
	}
	if !ok || !ok2 || p2 != path || end < off || end-off > requestLimit {
		return true
	}
	f, err := evidence.OpenAt(path, off)
	if err != nil {
		return true
	}
	defer f.Close()
	rd := bufio.NewReaderSize(io.LimitReader(f, end-off), 1<<16)
	if back && off > 0 {
		rd.ReadBytes('\n') // the rest of the line the stretch starts in
	}
	started, ended := false, false
	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 && (strings.Contains(string(raw), "task_started") || sinceEnd && (strings.Contains(string(raw), "task_complete") ||
			strings.Contains(string(raw), "turn_aborted"))) {
			var e struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Type == "event_msg" {
				switch e.Payload.Type {
				case "task_started":
					if !sinceEnd {
						return true
					}
					started = true
				case "task_complete", "turn_aborted":
					started, ended = false, true
				}
			}
		}
		if err != nil {
			return started || back && !ended
		}
	}
}

// codexKept reports whether the store keeps an event of a Codex agent message, by its content as tacit reads it: a
// text block that is not blank and is not Codex's "[Recorded tool history]", which the store does not keep. A shape
// it cannot read counts as kept, so the store's records decide.
func codexKept(content json.RawMessage) bool {
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return true
	}
	for _, b := range blocks {
		if strings.TrimSpace(b.Text) != "" && !strings.HasPrefix(b.Text, "[Recorded tool history]") {
			return true
		}
	}
	return false
}

// codexItems are the rollout's response items the store keeps as events: a message, and a call or its output.
var codexItems = map[string]bool{"message": true, "agent_message": true, "function_call": true, "custom_tool_call": true,
	"tool_search_call": true, "local_shell_call": true, "web_search_call": true, "function_call_output": true,
	"custom_tool_call_output": true, "tool_search_output": true}

// replayWake is a self-paced loop's pending wakeup, as the session layer reads it.
type replayWake struct {
	at, set time.Time
	what    string
	sign    string // why it may have been lost
	seen    bool   // its harness records its run
}

// recordsRunsAt reports whether the Claude Code that wrote an event's record records the scheduled runs it starts:
// by the record's version while the transcript is there; when it is gone, whether the session showed a run's
// record already, else yes.
func recordsRunsAt(e store.Event, firesSeen bool) bool {
	if v := versionAt(e.Ref); v != "" {
		return claude.RecordsRuns(v)
	}
	return true
}

// versionAt is the Claude Code version that wrote an event's record (Ref: path@offset#index), "" when its
// transcript is gone or the records do not say. Each transcript's versions are read once.
func versionAt(ref string) string {
	path, off, ok := refPlace(ref)
	if !ok {
		return ""
	}
	vs := versionsOf(path)
	i := sort.Search(len(vs), func(i int) bool { return vs[i].off > off }) - 1
	if i < 0 {
		return ""
	}
	return vs[i].version
}

// versionChange is where a transcript's records start to carry a version.
type versionChange struct {
	off     int64
	version string
}

var (
	versionsMu sync.Mutex
	versions   = map[string][]versionChange{}
)

// versionsOf is where the version in a transcript's records changes, in order, read once per transcript.
func versionsOf(path string) []versionChange {
	versionsMu.Lock()
	defer versionsMu.Unlock()
	if vs, ok := versions[path]; ok {
		return vs
	}
	var vs []versionChange
	if f, err := os.Open(path); err == nil {
		rd := bufio.NewReaderSize(f, 1<<16)
		var off int64
		for {
			line, err := rd.ReadBytes('\n')
			if bytes.Contains(line, []byte(`"version":"`)) { // the record's own field: a tool's result can hold one too
				var rec struct {
					Version string `json:"version"`
				}
				if json.Unmarshal(line, &rec) == nil && rec.Version != "" && (len(vs) == 0 || vs[len(vs)-1].version != rec.Version) {
					vs = append(vs, versionChange{off: off, version: rec.Version})
				}
			}
			off += int64(len(line))
			if err != nil {
				break
			}
		}
		f.Close()
	}
	versions[path] = vs
	return vs
}

// newestFirst is the jobs' ids, the newest first and the larger id at a tie: the order the live tracker picks a stop's
// loop by (claude.Tracker.Facts).
func newestFirst(crons map[string]replayCron) []string {
	ids := make([]string, 0, len(crons))
	for id := range crons {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if a, b := crons[ids[i]].at, crons[ids[j]].at; !a.Equal(b) {
			return a.After(b)
		}
		return ids[i] > ids[j]
	})
	return ids
}

// timeline turns one session's events into its points: one for each input, with the stop before it, and one
// for the last stop, whose Next is none.
func timeline(id string, evs []store.Event, until time.Time) []Point {
	var out []Point
	tasks := map[string]replayTask{}
	crons := map[string]replayCron{}
	calls := map[string]replayCall{} // a ScheduleWakeup or CronCreate call's id -> what it asked for
	var wake replayWake
	var ownerAt, resumed, turnBegan time.Time
	firesSeen := false      // a run's record showed in the session: its harness records them
	var restarted time.Time // the last reopening: what ran died with the process, and Claude Code restored its jobs
	waitedThrough := func(sign string, at time.Time) {
		if !wake.at.IsZero() && !at.Before(wake.set) && wake.sign == "" {
			wake.sign = sign
		}
	}
	// restart is a new process: a restart in the foreground loses a pending wakeup; Claude Code restores the jobs,
	// each from its first run after the restart (JobRuns).
	restart := func(at time.Time) {
		restarted, wake = at, replayWake{}
	}
	reopened := func(at time.Time) {
		resumed, tasks = at, map[string]replayTask{}
		restart(at)
	}
	version := "" // the Claude Code version of the last record read: a new one is a new process
	var words, entry, harness, prevRole, prevPrior string
	var carried string       // a turn's end recorded on a record passed over since the last stop: a local command's, an abort
	var controls []time.Time // the owner's local commands since the last stop
	controlRef := ""         // the place of the first of them: its echo is a user record, which answers a request from then on
	aborted := false         // a Codex abort ended the turn since the last stop, and dated its stop
	exited := false          // an /exit ended the process: the next record the replay reads is a new one
	var prevAt time.Time
	var owes time.Time           // the model owes its reply since: an input, or the result of a turn's last call
	var owesRef string           // where the transcript is read from for its request: the event before the one that owes
	var lastRef string           // the place of the event before this one
	var lastSeen string          // the place of the last event read, one passed over among them
	pending := map[string]bool{} // calls without a result yet
	lastMainUser := false
	// the input that opened the last turn when its record says the turn before it was over, and the event the replay
	// read before it
	var openRef, openFrom string
	// the owner's request the last turn served (Stop.TurnFor): his last input, until another input opens a turn
	// that is not the transient message that quoted it. An exit keeps it, as the live layer keeps the store's turn.
	var request string
	stopOf := func(at time.Time, nextRef string) Stop {
		s := Stop{Key: id, Tool: harness, At: prevAt, Words: words, OwnerAt: ownerAt, ResumedAt: resumed, TurnFor: request}
		for k, t := range tasks {
			if t.kind == "monitor" && !t.until.IsZero() && t.until.Before(at) {
				delete(tasks, k)
			}
		}
		s.Tasks = len(tasks)
		if !wake.at.IsZero() && wake.at.After(prevAt.Add(-sessions.WakeGrace)) {
			s.WakeAt = wake.at
		}
		s.TurnBegan, s.IdleAt, s.Exited, s.Reopened = turnBegan, prevAt, prevRole == "exit", prevRole == "reopened"
		if !s.Exited { // nothing runs while the process is gone: an exited stop has no schedule
			// the newest job's label, as the live tracker shows it (claude.Tracker.Facts), its expression where it has none
			if ids := newestFirst(crons); len(ids) > 0 {
				s.Loop = firstNonEmpty(crons[ids[0]].label, crons[ids[0]].expr)
			}
			// Its own runs its harness should start, as the session layer lists them, a reopening for a restart.
			if !wake.at.IsZero() && wake.seen {
				s.Runs = append(s.Runs, claude.WakeRun(wake.at, wake.set, wake.what, wake.sign, time.Time{}))
			}
			for id, c := range crons {
				if c.seen {
					s.Runs = append(s.Runs, claude.JobRuns(id, c.expr, c.oneShot, c.what, c.at, c.fired, restarted, at)...)
				}
			}
		}
		claude.SortRuns(s.Runs)
		switch prevRole {
		case "call":
			s.Working, s.Busy = len(pending) > 0, len(pending) > 0
		case "result", "owner", "self", "interrupt", "momentum":
			// the model owes a reply, unless the harness said the turn was over
			s.Working = prevRole != "interrupt" && !turnOver(harness, prevPrior)
		case "apierr":
			s.Error = words
		}
		if prevRole == "interrupt" {
			s.Working = false
		}
		if s.Working && !s.Busy && !owes.IsZero() && harness == "claude" {
			s.RequestAt = requestBetween(owesRef, nextRef, until)
		}
		s.Phase = phaseOf(s)
		return s
	}
	// notified follows a task's notification: a wakeup it says was lost, a sign for a pending one, the tasks it
	// closes
	notified := func(summary string, at time.Time) {
		if hWakeGone.MatchString(summary) {
			wake = replayWake{}
		} else {
			waitedThrough("a task's notification came in while it waited", at)
		}
		st := hStatus.FindStringSubmatch(summary)
		for _, tm := range hTaskID.FindAllStringSubmatch(summary, -1) {
			t, ok := tasks[tm[1]]
			if !ok {
				continue
			}
			if t.kind != "monitor" || st != nil && terminalSt[st[1]] {
				delete(tasks, tm[1])
			}
		}
	}
	evs, queued, enqueued := placeQueued(evs)
	for _, e := range evs {
		m := map[string]any{}
		if e.Meta != "" {
			json.Unmarshal([]byte(e.Meta), &m)
		}
		if harness == "" {
			harness = e.Harness
		}
		lastSeen = e.Ref
		r := role(e, m)
		if why, _ := m["why"].(string); harness == "codex" && r == "agent" && why != "" && why != "assistant" {
			// a subagent's message to the session, or an agent's text in the owner's turn: Codex's live tracker reads
			// only the agent's own messages, and passes over the rest
			r = ""
		}
		if enqueued[e.SourceKey] {
			// a notification at the time it was queued, delivered as an input later: it closes its task now, as the
			// live tracker reads the queue. Another session's message is no notification of its own tasks, and an input
			// that only quotes one is none.
			if r == "self" && !fromPeer(e, m) && claude.Notification(e.Summary) {
				if !strings.Contains(e.Summary, "</task-notification>") {
					e.Summary = whole(e)
				}
				notified(e.Summary, e.TS)
			}
			continue
		}
		// a queued command delivered with the input before it, before anything of that input's turn, is part of that
		// input: the turn it opens is the one the input opened
		input := r == "owner" || r == "self" || r == "momentum"
		merged := input && queued[e.SourceKey] && (prevRole == "owner" || prevRole == "self" || prevRole == "momentum")
		// one delivered after a result or a call is delivered into the turn they ran in, which was open: it begins no
		// turn, as the live tracker dates a turn's start only when a closed turn opens
		intoTurn := input && queued[e.SourceKey] && (prevRole == "result" || prevRole == "call")
		// the owner's input that opens a Codex turn clears its words, read as the live tracker reads its message
		opens := r == "owner" && !merged && harness == "codex" && codexOpens(m, carried) && codex.OpensTurn(e.Summary)
		if e.Harness == "codex" && m["prior"] == "turn_aborted" {
			pending = map[string]bool{} // the abort ended the turn and its calls, whatever record the store marked it on
			if r == "" {
				r = "aborted" // the store marked Codex's abort of the turn on the record after it
			} else if !aborted && !prevAt.IsZero() {
				// marked on an input with no record of its own: the turn ran until the rollout's turn_aborted event
				// while the transcript is there, else until then at the latest, as Codex's live tracker keeps a turn
				// open until its abort
				prevAt, aborted = abortedBy(lastRef, e.Ref, prevAt, e.TS), true
			}
		}
		if v := versionAt(e.Ref); v != "" {
			if version != "" && v != version {
				restart(e.TS)
			}
			version = v
		}
		if r == "compaction" {
			waitedThrough("the conversation was compacted while it waited", e.TS)
		}
		if r == "" || r == "compaction" { // a compaction runs as a turn starts or mid-turn: no stop of its own
			// the store puts the record of a turn's end on the first record after the turn, which may be one passed
			// over (a local command's caveat, a meta line): the next input's point reads it. Another block of the
			// record just read (an image's blocks after the owner's text, a paste the store cut out) carries that
			// record's mark, which its own event read already.
			if prior, _ := m["prior"].(string); carried == "" && turnOver(harness, prior) && !sameRecord(e.Ref, lastRef) {
				carried = prior
			}
			continue
		}
		switch r {
		case "control":
			// its user was there, and the stop before it goes on to the next input: no turn opened, and nothing the
			// model owes, its words or its error changes. It holds the rules only from when it came (Point.Controls).
			// The harness's record that the turn was over sits on the first record after the turn, maybe this one,
			// so the next input's point reads it.
			controls = append(controls, e.TS)
			if controlRef == "" {
				controlRef = e.Ref
			}
			if prior, _ := m["prior"].(string); carried == "" && turnOver(harness, prior) {
				carried = prior
			}
			if exited { // a local command after /exit: the session was opened again, and its jobs and rules run from here
				restart(e.TS)
				exited, prevRole, prevAt = false, "reopened", e.TS
			}
			continue
		case "aborted":
			// Codex ended the turn on its user's interruption, as its live tracker reads turn_aborted, with no call
			// running: the stop before it goes on to the next input, which reads the turn as over. The live tracker
			// keeps the turn open until the abort, so the stop starts there: at the abort's own record, else at the
			// first record the store marked it on, the latest it can have come. Like the live tracker, the replay
			// does not count it as his presence.
			if carried == "" {
				carried = "turn_aborted"
			}
			if !aborted && !prevAt.IsZero() {
				at := e.TS
				if !strings.HasPrefix(e.Summary, "<turn_aborted>") { // only marked: the rollout's own event, while it is there
					at = abortedBy(lastRef, e.Ref, prevAt, e.TS)
				}
				prevAt, aborted = at, true
			}
			pending, owes, owesRef = map[string]bool{}, time.Time{}, ""
			continue
		case "exit":
			// /exit or /quit: the process ends, and what ran dies with it. The stop before it ends here; the stop after
			// it is one no rule acts on (Stop.Exited) until the session runs again: its next input, turn or local
			// command, from which its jobs run again. The records the exit itself writes after it (its output, its
			// caveat) and other records the replay passes over do not end it.
			if !prevAt.IsZero() {
				prevPrior = priorOf(m, carried)
				out = append(out, Point{Session: id, Stop: stopOf(e.TS, firstNonEmpty(controlRef, e.Ref)), Next: Next{At: e.TS, Kind: "owner", InputKind: "exit", Text: e.Summary},
					Entry: entry, Controls: controls})
			}
			ownerAt, controls, controlRef, carried, words, resumed = e.TS, nil, "", "", "", time.Time{}
			tasks, pending, wake = map[string]replayTask{}, map[string]bool{}, replayWake{}
			prevAt, prevRole, prevPrior, owes, owesRef, exited = e.TS, "exit", "", time.Time{}, "", true
			// the old process's turn ended with it, as the live tracker reads the exit: a "No response requested." the
			// next one writes is no sign that the session ended mid-task
			lastMainUser = false
			continue
		}
		if exited { // the session runs again from this record: its jobs run from their first run after it
			restart(e.TS)
			exited = false
		}
		if r == "self" && strings.HasPrefix(e.Summary, "<task-notification>") && !strings.Contains(e.Summary, "</task-notification>") {
			e.Summary = whole(e) // the store cut it: read what it says from the transcript, while it is there
		}
		if r == "self" && prevRole == "apierr" && harness == "claude" && !fromPeer(e, m) && claude.Notification(e.Summary) &&
			!claude.Orphaned(e.Summary) && unsent(lastRef, e.Ref, until) {
			// A task's notification Claude Code delivered after the turn ended on an API error, and sent its model no
			// request for (a usage limit it knows of), opens no turn, as the live tracker reads it: the stop on the error
			// goes on to the next input, with its error, its words and its time, so the rule that reads the error decides
			// as it would without the notification. What it says of the session's tasks and its wakeup still counts.
			notified(e.Summary, e.TS)
			if prior, _ := m["prior"].(string); carried == "" && turnOver(harness, prior) {
				carried = prior
			}
			continue
		}
		if r == "owner" || r == "self" || r == "interrupt" || r == "momentum" {
			ik, _ := m["input_kind"].(string)
			next := Next{At: e.TS, Kind: "owner", InputKind: ik, Text: e.Summary}
			if r == "self" || r == "momentum" {
				next.Kind = r
			}
			if r == "interrupt" {
				next.InputKind = "interrupt"
			}
			if !prevAt.IsZero() && !merged {
				prevPrior = priorOf(m, carried)
				// the request the stop waited on is read up to the first local command: its echo answers it from then on,
				// as the live tracker reads it, and Replay drops a stalled decision a command came before
				out = append(out, Point{Session: id, Stop: stopOf(e.TS, firstNonEmpty(controlRef, e.Ref)), Next: next, Entry: entry, Controls: controls})
			}
		}
		if (r == "agent" || r == "call" || r == "apierr" || r == "synthetic") && !owes.IsZero() && e.TS.Sub(owes) >= stallFloor {
			// the wait ended after a long time without an input: the model's reply, or a reopening that closed the
			// turn. What the stalled rule would have done is the point.
			st := stopOf(e.TS, firstNonEmpty(controlRef, e.Ref))
			st.Working, st.Busy, st.Error = true, false, ""
			st.Phase = phaseOf(st)
			next := Next{At: e.TS, Kind: "self", Text: "the model's reply"}
			if r == "synthetic" {
				next = Next{At: e.TS, Kind: "owner", InputKind: "restart", Text: "the session was reopened"}
			}
			if r != "synthetic" || !st.RequestAt.IsZero() {
				// a local command during the wait answers its request from then on, as in the live layer: Replay drops
				// the stalled decision it came before
				out = append(out, Point{Session: id, Stop: st, Next: next, Entry: entry, Waiting: true, Controls: controls})
			}
		}
		why, _ := m["why"].(string)
		switch r {
		case "owner":
			ownerAt = e.TS
			if !strings.HasPrefix(strings.TrimSpace(e.Summary), Marker) { // a message of momentum's the store credited to him
				request = e.Summary
			}
			if e.Kind == "owner_text" {
				if v, ok := strings.CutPrefix(why, "entrypoint "); ok {
					entry = v
				} else if v, ok := strings.CutPrefix(why, "originator "); ok {
					entry = v
				}
			}
			resumed = time.Time{} // a planned wakeup still fires after a prompt: it stays
			lastMainUser = true
		case "self":
			request = ""
			if prior, _ := m["prior"].(string); prior == "scheduled_task_fire" { // a scheduled run started
				firesSeen = true
				f, ok := fireBetween(lastRef, e.Ref)
				if !ok { // the transcript is gone: the run whose prompt this is, the newest job's where two prompts start it
					for _, k := range newestFirst(crons) {
						if c := crons[k]; c.what != "" && strings.HasPrefix(strings.TrimSpace(e.Summary), strings.TrimSpace(c.what)) {
							f.TaskID = k
							break
						}
					}
					if f.TaskID == "" {
						f.TaskKind = "loop"
					}
				}
				if c, job := crons[f.TaskID]; job {
					if c.oneShot {
						delete(crons, f.TaskID)
					} else {
						c.fired = e.TS
						crons[f.TaskID] = c
					}
				} else if (f.TaskKind == "loop" || strings.Contains(f.Content, "wakeup")) && !e.TS.Before(wake.set) {
					wake = replayWake{}
				}
			}
			// another session's message is an input, not a notice of this session's tasks or of its reopening, and
			// neither is an input that only quotes a notification (a loop's prompt), as the live tracker reads them
			if !fromPeer(e, m) && claude.Notification(e.Summary) {
				notified(e.Summary, e.TS)
				if claude.Orphaned(e.Summary) { // reopened: what ran died with the process
					reopened(e.TS)
				}
			}
			lastMainUser = true
		case "momentum": // a message momentum typed: it opens a turn, and is not the owner's
			if !quotes(fold(e.Summary), request) { // only a transient retry serves the request it quoted
				request = ""
			}
			resumed = time.Time{}
			lastMainUser = true
		case "interrupt": // its owner pressed Esc or turned a call down: he was there, as the live layer counts it
			ownerAt = e.TS
			lastMainUser = false
			if !wake.at.IsZero() && !e.TS.Before(wake.set) {
				wake = replayWake{} // Claude Code cancels a pending loop wakeup on an Esc
			}
		case "call":
			id, _ := m["tool_id"].(string)
			pending[id] = true
			if e.Tool == "ScheduleWakeup" || e.Tool == "CronCreate" {
				calls[id] = callOf(e, id)
			}
			switch e.Tool {
			case "TaskStop":
				if g := hTaskStop.FindStringSubmatch(e.Summary); g != nil {
					delete(tasks, g[1])
				}
			case "CronDelete":
				if g := hCronDel.FindStringSubmatch(e.Summary); g != nil {
					delete(crons, g[1])
				}
			}
			resumed = time.Time{}
		case "result":
			id, _ := m["tool_id"].(string)
			delete(pending, id)
			isErr, _ := m["is_error"].(bool)
			s := e.Summary
			if isErr && hCutCall.MatchString(s) {
				reopened(e.TS)
			}
			if !isErr {
				if g := hBgBash.FindStringSubmatch(s); g != nil && e.Tool == "Bash" {
					tasks[g[1]+g[2]] = replayTask{kind: "bash"}
				}
				if g := hAgent.FindStringSubmatch(s); g != nil && (e.Tool == "Agent" || e.Tool == "Task") {
					tasks[g[1]] = replayTask{kind: "agent"}
				}
				if g := hWorkflow.FindStringSubmatch(s); g != nil {
					tasks[g[1]] = replayTask{kind: "workflow"}
				}
				if g := hMonitor.FindStringSubmatch(s); g != nil && e.Tool == "Monitor" {
					n, _ := strconv.Atoi(g[2])
					unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[g[3]]
					tasks[g[1]] = replayTask{kind: "monitor", until: e.TS.Add(time.Duration(n) * unit)}
				}
				if g := hWake.FindStringSubmatch(s); g != nil && e.Tool == "ScheduleWakeup" {
					n, _ := strconv.Atoi(g[1])
					wake = replayWake{at: e.TS.Add(time.Duration(n) * time.Second), set: e.TS, what: calls[id].what, seen: recordsRunsAt(e, firesSeen)}
				}
				if (strings.HasPrefix(s, "Loop stopped") || strings.HasPrefix(s, "Wakeup not scheduled")) && e.Tool == "ScheduleWakeup" {
					wake = replayWake{}
				}
				if g := hCron.FindStringSubmatch(s); g != nil && e.Tool == "CronCreate" {
					c := calls[id]
					crons[g[2]] = replayCron{expr: firstNonEmpty(c.cron, g[3]), label: g[3], at: e.TS, oneShot: g[1] == "one-shot", what: c.what, seen: recordsRunsAt(e, firesSeen)}
				}
			}
			lastMainUser = true
		case "agent":
			words, pending, resumed = e.Summary, map[string]bool{}, time.Time{}
			lastMainUser = false
		case "apierr":
			words = e.Summary
			if strings.TrimSpace(words) == "No response requested." {
				words = claude.RateLimit // a rate limit Claude Code had no words for, as the session layer reads it
			}
			lastMainUser = false
		case "synthetic":
			if lastMainUser && strings.Contains(e.Summary, "No response requested") {
				resumed = e.TS
			}
			lastMainUser = false
		}
		inputPrior := priorOf(m, carried) // an input's record of the turn before it ending, its own or one carried to it
		prevRole, prevAt, carried, aborted, controlRef = r, e.TS, "", false, ""
		for _, at := range controls { // the stop they came after is over: they show him at the session before the next
			if at.After(ownerAt) {
				ownerAt = at
			}
		}
		controls = nil
		if r == "owner" || r == "self" || r == "interrupt" || r == "momentum" {
			prevPrior = ""
		}
		if input && !merged && !intoTurn {
			turnBegan = e.TS // an input opens the turn the next stop ends
			// the turn before it was over by its own record or one passed over just before it (Codex's abort record,
			// harness text): Codex may write its turn's task_started after it
			openRef, openFrom = "", ""
			if turnOver(harness, inputPrior) {
				openRef, openFrom = e.Ref, lastRef
			}
		}
		if opens {
			words = "" // a Codex turn its input opens starts with no last words of its own, as its live tracker reads it
		}
		switch {
		case merged: // the model owes its reply from the input it came with
		case input, r == "result" && len(pending) == 0:
			// Claude Code may write the request before the record of the input it carries, dated before it
			owes, owesRef = e.TS, firstNonEmpty(lastRef, e.Ref)
		case r == "agent" || r == "call" || r == "apierr" || r == "interrupt" || r == "synthetic":
			owes, owesRef = time.Time{}, ""
		}
		lastRef = e.Ref
	}
	if !prevAt.IsZero() {
		inputLast := prevRole == "owner" || prevRole == "self" || prevRole == "momentum"
		if carried == "" && harness == "codex" && (inputLast || prevRole == "result" || prevRole == "call" && len(pending) > 0) {
			// the store marks a Codex turn's end on the record after it, and none came after the last record: the
			// rollout's own end of the turn after it, while the transcript is there, as the live tracker reads it.
			// With nothing carried, no abort since the last record dated this stop. An input that opened its turn
			// after the turn before it was over has its turn's start still to come when no task_started came after the
			// last turn end between the event the replay read before it (a record passed over, as a developer message
			// or another block of the input's own record, is not one; with none in the window, the turn before's end)
			// and the input, nor any between the input and the last record read. The end's last_agent_message is the
			// turn's last words.
			opened := inputLast && openRef != "" && !codexStartedBetween(openFrom, openRef, true) &&
				(lastSeen == openRef || !codexStartedBetween(openRef, lastSeen, false))
			if end, at, last := codexTurnEnd(lastSeen, opened, until); end != "" {
				carried, pending = end, map[string]bool{}
				if end == "turn_aborted" {
					prevAt = at // the live tracker keeps the turn open until its abort
				}
				// a last_agent_message is the turn's last words, as the live tracker reads it, a whitespace-only one
				// among them: the store's words stay where they say the same
				if last != "" && Words(last) != Words(words) {
					words = Words(last)
				}
			}
		}
		if carried != "" {
			prevPrior = carried // a record passed over after the last stop said its turn was over
		}
		out = append(out, Point{Session: id, Stop: stopOf(until, controlRef), Next: Next{Kind: "none"}, Entry: entry, Controls: controls})
	}
	return out
}

// requestLimit bounds how much of a transcript requestBetween reads between two events.
const requestLimit = 8 << 20

// requestBetween is the model request a turn still waited on between two of its events, by Claude Code's
// api-request records in the transcript between their places (Ref: path@offset#index): the last request, when
// no record of the conversation answered it, as the session layer reads them. Zero when the transcript is gone,
// predates those records, or holds no request there. to "" reads to the transcript's end, and no record dated
// after until counts.
func requestBetween(from, to string, until time.Time) time.Time {
	path, off, ok := refPlace(from)
	if !ok {
		return time.Time{}
	}
	end := int64(-1)
	if to != "" {
		p2, o2, ok := refPlace(to)
		if !ok || p2 != path || o2 < off {
			return time.Time{}
		}
		end = o2
	}
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return time.Time{}
	}
	var r io.Reader = f
	if end >= 0 {
		r = io.LimitReader(f, end-off)
	}
	rd := bufio.NewReaderSize(io.LimitReader(r, requestLimit), 1<<16)
	var req time.Time
	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 {
			var e struct {
				Type      string `json:"type"`
				Subtype   string `json:"subtype"`
				Timestamp string `json:"timestamp"`
				IsMeta    bool   `json:"isMeta"`
				Sidechain bool   `json:"isSidechain"`
			}
			if json.Unmarshal(raw, &e) == nil && !e.Sidechain {
				at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
				if !until.IsZero() && at.After(until) {
					return req
				}
				switch {
				case e.Type == "api-request":
					req = at
				case req.IsZero(), !at.IsZero() && at.Before(req):
				case e.Type == "assistant", e.Type == "user" && !e.IsMeta, e.Type == "system" && e.Subtype != "informational":
					req = time.Time{}
				}
			}
		}
		if err != nil {
			return req
		}
	}
}

// unsent reports that Claude Code sent its model no request for an input (its record's place), by the transcript
// while it is there: no api-request record after the record of the event before the input (from), up to the next
// record of the conversation after the input's own (a reply or an error, any assistant record, or another input, a
// user record that is not meta nor a local command's echo), the transcript's end, or the first record dated past
// until. Claude Code may write the request before the record of the input it carries, so one between the two counts.
// A reply with no request before it is no sign either way (a synthetic message, a request it did not record, a
// transcript older than those records), and neither is one after a local command (a /login that let the request
// go): the input is not taken as unsent. False when the transcript is gone, the two are not in one transcript in that
// order, or the read runs past requestLimit.
func unsent(from, input string, until time.Time) bool {
	path, off, ok := refPlace(from)
	p2, at, ok2 := refPlace(input)
	if !ok || !ok2 || p2 != path || at < off {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return false
	}
	rd := bufio.NewReaderSize(io.LimitReader(f, requestLimit), 1<<16)
	own, err := rd.ReadBytes('\n') // the event's own record
	if err != nil {
		return false
	}
	pos := off + int64(len(own))
	for {
		start := pos
		raw, err := rd.ReadBytes('\n')
		pos += int64(len(raw))
		if len(raw) > 0 {
			var e struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				IsMeta    bool   `json:"isMeta"`
				Sidechain bool   `json:"isSidechain"`
			}
			if json.Unmarshal(raw, &e) == nil && !e.Sidechain {
				if t, perr := time.Parse(time.RFC3339Nano, e.Timestamp); perr == nil && !until.IsZero() && t.After(until) {
					return true
				}
				switch {
				case e.Type == "api-request":
					return false
				case start > at && e.Type == "assistant":
					return false
				case start > at && e.Type == "user" && !e.IsMeta && !echoLine(raw):
					return true
				}
			}
		}
		if err != nil {
			return err == io.EOF && pos-off < requestLimit
		}
	}
}

// echoLine reports a user record whose text is only the tags of a local command's echo (its name, its output, a
// caveat, a reminder), which opens no turn: not a skill's, whose echo is its prompt's.
func echoLine(raw []byte) bool {
	var r struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	var text string
	if json.Unmarshal(r.Message.Content, &text) != nil {
		var blocks []struct{ Type, Text string }
		if json.Unmarshal(r.Message.Content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			if b.Type != "text" {
				return false
			}
			text += b.Text
		}
	}
	return strings.TrimSpace(text) != "" && !hSkillRunning.MatchString(text) && strings.TrimSpace(hCommandTag.ReplaceAllString(text, "")) == ""
}

// queuedCommand reports what Claude Code queued while a turn ran and delivered into the turn later, not its user's: a
// task's notification, which the store marks by its command mode, or a harness line when it is meta, and another
// session's message, which the store marks a harness line when it is meta, else by its origin (origin peer), as the
// harness's text or an agent's, as tacit's digest reads a queued command (meta before its mode). Either mark can
// also be on a record of another kind (a harness line on a record shown in the transcript only, origin peer on a
// user record), so while the transcript is there its record says: a queued command is an attachment of type
// queued_command. Without the transcript the store's mark stands, and tacit marks both kinds of record alike (the
// same why, no input kind), so a record of another kind reads as queued: dated where it sits, it is not moved, but
// right after an input it is read as part of that input, and after a call or its result it begins no turn. That is
// a limit of the store (SPEC). A message its user typed meanwhile is queued too, but its record's time is when he
// typed it, which his labelled steps and his turns are found by.
func queuedCommand(e store.Event) bool {
	if e.Harness != "claude" || e.Kind != "harness_text" && e.Kind != "agent_text" {
		return false
	}
	var m struct {
		Why string `json:"why"`
	}
	json.Unmarshal([]byte(e.Meta), &m)
	switch {
	case strings.HasPrefix(m.Why, "commandMode "):
		return e.Kind == "harness_text"
	case m.Why != "harness line" && m.Why != "origin peer":
		return false
	}
	line := recordAt(e.Ref)
	if line == nil {
		return true
	}
	var r struct {
		Type       string `json:"type"`
		Attachment struct {
			Type string `json:"type"`
		} `json:"attachment"`
	}
	return json.Unmarshal(line, &r) == nil && r.Type == "attachment" && r.Attachment.Type == "queued_command"
}

// placeQueued reads each queued command where it sits in its transcript. Claude Code writes one as an attachment
// that carries the time it was queued, not the time it was delivered, so by its time it would come before the
// records it followed (the result or the input it rode in with) and read as an input of its own. It is delivered at
// the event before it in its transcript (by Ref) when that is later. Anchoring steps back over the queued commands
// before it: each is already placed where it was delivered, so a run of them is read at the record they followed,
// and one queued after that record was written keeps its own time, before which nothing after it was delivered. A
// prompt its user typed meanwhile keeps the time he typed it, so what follows it is delivered with it. Events are
// read in the store's order: by time, then source key. What its queueing did stays at its own time, as the live
// tracker reads the queue: a copy there, under its key with enqueuedKey after it, so in the store's order where the
// queued command itself sat, closes the task a notification reports. It returns the events in that order, the
// queued commands and the copies, by source key.
func placeQueued(evs []store.Event) ([]store.Event, map[string]bool, map[string]bool) {
	type place struct {
		path string
		off  int64
		idx  int
	}
	before := func(a, b place) bool {
		if a.path != b.path {
			return a.path < b.path
		}
		if a.off != b.off {
			return a.off < b.off
		}
		return a.idx < b.idx
	}
	out := append([]store.Event(nil), evs...)
	places := make([]place, len(out))
	var order []int // the events with a place, in transcript order
	for i, e := range out {
		if path, off, ok := refPlace(e.Ref); ok && e.Harness == "claude" {
			places[i], order = place{path: path, off: off, idx: blockIndex(e.Ref)}, append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return before(places[order[a]], places[order[b]]) })
	queued, enqueued := map[string]bool{}, map[string]bool{}
	var copies []store.Event
	for k, i := range order {
		if !queuedCommand(out[i]) {
			continue
		}
		queued[out[i].SourceKey] = true
		if k > 0 && places[order[k-1]].path == places[i].path {
			// the event before it as placed: a queued command before it was delivered at its own anchor
			if prev := out[order[k-1]].TS; prev.After(out[i].TS) {
				c := out[i]
				c.SourceKey += enqueuedKey
				copies, enqueued[c.SourceKey] = append(copies, c), true
				out[i].TS = prev
			}
		}
	}
	if len(copies) == 0 {
		return evs, queued, enqueued
	}
	// The store's order, by construction (Store.Timeline): time, then source key, which starts with the transcript's
	// name and holds the record's offset and the item's index at fixed widths. A moved event shares its anchor's time
	// and follows it in their transcript, so it never lands before it, and the rest stay as the store reads them.
	out = append(out, copies...)
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].TS.Equal(out[b].TS) {
			return out[a].TS.Before(out[b].TS)
		}
		return out[a].SourceKey < out[b].SourceKey
	})
	return out, queued, enqueued
}

// enqueuedKey marks the copy of a queued command at the time it was queued (placeQueued).
const enqueuedKey = "~enqueued"

// sameRecord reports whether two events' Refs are blocks of one transcript record.
func sameRecord(a, b string) bool {
	pa, oa, ok := refPlace(a)
	pb, ob, ok2 := refPlace(b)
	return ok && ok2 && pa == pb && oa == ob
}

// refPlace splits an event's Ref (path@offset#index) into its transcript and the offset of its record.
func refPlace(ref string) (string, int64, bool) {
	at := strings.LastIndexByte(ref, '@')
	if at < 0 {
		return "", 0, false
	}
	path, rest := ref[:at], ref[at+1:]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	off, err := strconv.ParseInt(rest, 10, 64)
	return path, off, err == nil
}

// whole is an event's summary with its record's line in the transcript (Ref: path@offset#index) after it, read
// whole while the transcript is there; the summary alone when it is not.
func whole(e store.Event) string {
	at := strings.LastIndexByte(e.Ref, '@')
	if at < 0 {
		return e.Summary
	}
	path, rest := e.Ref[:at], e.Ref[at+1:]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	off, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return e.Summary
	}
	f, err := os.Open(path)
	if err != nil {
		return e.Summary
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return e.Summary
	}
	line, err := bufio.NewReaderSize(f, 1<<16).ReadString('\n')
	if err != nil && line == "" {
		return e.Summary
	}
	return e.Summary + " " + line
}

// priorOf is the record of the turn's end an input's point reads: the input's own prior, or, when that says nothing
// of it, the one carried from a record passed over after the stop (a local command's, an abort).
func priorOf(m map[string]any, carried string) string {
	prior, _ := m["prior"].(string)
	if carried != "" && !turnOver("", prior) {
		return carried
	}
	return prior
}

// codexOpens reports whether a Codex input opens a turn by the store's marks, as Codex's live tracker reads its
// rollout: any but one steered into the running turn, which the store marks queued, or marks an answer while the
// turn before it was not over.
func codexOpens(m map[string]any, carried string) bool {
	switch ik, _ := m["input_kind"].(string); ik {
	case "queued":
		return false
	case "answer":
		return turnOver("codex", priorOf(m, carried))
	}
	return true
}

// turnOver reports the harness's record that a turn ended, as the store keeps the record before an input. Claude
// Code's agents_killed says the background agents were killed, as an Escape kills them: it can come while a turn
// goes on, but tacit keeps it as an input's prior only when no record of the conversation (a reply, a tool's result,
// another input) came between the two, so the turn was over by then.
func turnOver(harness, prior string) bool {
	switch prior {
	case "turn_duration", "stop_hook_summary", "away_summary", "task_complete", "turn_aborted", "stop", "agents_killed":
		return true
	}
	return false
}

// phaseOf is the phase rule (sessions.PhaseOf) over a stop's facts.
func phaseOf(s Stop) sessions.Phase {
	switch {
	case s.Ask == sessions.AskQuestion || s.Ask == sessions.AskPlan || s.Ask == sessions.AskPermission:
		return sessions.NeedsYou
	case s.Working:
		return sessions.Working
	case s.Tasks > 0:
		return sessions.Background
	case s.Error != "":
		return sessions.NeedsYou
	case !s.WakeAt.IsZero() || s.Loop != "":
		return sessions.Scheduled
	}
	return sessions.Idle
}

// History reads the points of every owner session in a window from the store.
func History(ctx context.Context, st *store.Store, since, until time.Time) ([]Point, error) {
	ids, err := st.OwnerSessions(ctx, since, until)
	if err != nil {
		return nil, err
	}
	var out []Point
	for _, id := range ids {
		evs, err := st.Timeline(ctx, id, since, until)
		if err != nil {
			return nil, err
		}
		out = append(out, timeline(id, evs, until)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Stop.At.Before(out[j].Stop.At) })
	return out, nil
}

// AccessProbe tells when access came back after an access error: the first turn that worked, on the Anthropic
// API, in a session that had met an error of the same class before it. Access stops a whole account, so one
// session that works again shows it works for all; a session that never met the error proves nothing, since it
// may run on another credential.
type AccessProbe struct {
	Calls  []store.Mark // FirstPartyCalls, oldest first
	Errors []store.Mark // APIErrors, oldest first
}

// AccessBack is the first call after since in a session that was blocked by an error of the class until that
// call: its last error of the class came after its last call that worked. A session that met the class once and
// has worked since, on another login perhaps, was not blocked any more, so its calls say nothing of a new block.
func (p AccessProbe) AccessBack(class string, since time.Time) (time.Time, bool) {
	blocked := map[string]bool{}
	e := 0
	for _, c := range p.Calls {
		for e < len(p.Errors) && !p.Errors[e].At.After(c.At) {
			if ErrorClass(p.Errors[e].Summary) == class {
				blocked[p.Errors[e].Session] = true
			}
			e++
		}
		if blocked[c.Session] && c.At.After(since) {
			return c.At, true
		}
		blocked[c.Session] = false // the call worked: access worked for this session then
	}
	return time.Time{}, false
}

// StoreProbe loads, for a window, the calls and the errors AccessBack reads.
func StoreProbe(ctx context.Context, st *store.Store, since, until time.Time) (AccessProbe, error) {
	calls, err := st.FirstPartyCalls(ctx, since, until, 0)
	if err != nil {
		return AccessProbe{}, err
	}
	errs, err := st.APIErrors(ctx, since, until)
	return AccessProbe{Calls: calls, Errors: errs}, err
}
