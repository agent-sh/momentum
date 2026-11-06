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

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
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
	hOrphan    = regexp.MustCompile(`didn't finish before|__orphan_summary__`)
	terminalSt = map[string]bool{"completed": true, "failed": true, "killed": true, "stopped": true, "error": true, "timeout": true, "cancelled": true}
)

// A Claude Code local command's echo is its command tags and nothing else, with the tags the live tracker strips
// (sessions/claude); a prompt it expands to has text of its own.
var (
	echoTags     = []string{"system-reminder", "local-command-stdout", "local-command-caveat", "command-name", "command-message", "command-args"}
	hCommandTag  = regexp.MustCompile(`(?s)<(?:` + strings.Join(echoTags, "|") + `)>.*?</(?:` + strings.Join(echoTags, "|") + `)>`)
	hCommandName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
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

// echoOnly reports a text that holds a command's name and nothing but the echo's tags.
func echoOnly(s string) bool {
	return strings.Contains(s, "<command-name>") && strings.TrimSpace(hCommandTag.ReplaceAllString(s, "")) == ""
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
	if json.Unmarshal(line, &r) != nil || len(r.Message.Content) == 0 {
		return "", false
	}
	var text string
	if json.Unmarshal(r.Message.Content, &text) == nil {
		return text, text != ""
	}
	var blocks []struct {
		Type, Text string
	}
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), len(parts) > 0
}

// role is what one event is in a session's turns.
func role(e store.Event, m map[string]any) string {
	why, _ := m["why"].(string)
	ik, _ := m["input_kind"].(string)
	switch e.Kind {
	case "owner_text":
		return "owner"
	case "agent_text":
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
	case strings.HasPrefix(s, "<task-notification>"):
		return "self"
	case strings.HasPrefix(s, "# Autonomous loop tick"), m["prior"] == "scheduled_task_fire":
		return "self"
	case strings.HasPrefix(s, "<agent-message from="), strings.HasPrefix(s, "Another Claude session sent a message"):
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

type replayTask struct {
	kind  string
	until time.Time // a monitor's expiry
}

// replayCron is a job CronCreate scheduled, as the store keeps it: its expression, when it was made and last ran,
// what it runs, and whether its harness records its runs.
type replayCron struct {
	expr      string
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
	aborted := false         // a Codex abort ended the turn since the last stop, and dated its stop
	exited := false          // an /exit ended the process: the next record the replay reads is a new one
	var prevAt time.Time
	var owes time.Time           // the model owes its reply since: an input, or the result of a turn's last call
	var owesRef string           // where the transcript is read from for its request: the event before the one that owes
	var lastRef string           // the place of the event before this one
	pending := map[string]bool{} // calls without a result yet
	lastMainUser := false
	stopOf := func(at time.Time, nextRef string) Stop {
		s := Stop{Key: id, Tool: harness, At: prevAt, Words: words, OwnerAt: ownerAt, ResumedAt: resumed}
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
			for _, c := range crons {
				s.Loop = c.expr
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
		case "result", "owner", "self", "interrupt", "compaction", "momentum":
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
	for _, e := range evs {
		m := map[string]any{}
		if e.Meta != "" {
			json.Unmarshal([]byte(e.Meta), &m)
		}
		if harness == "" {
			harness = e.Harness
		}
		r := role(e, m)
		if e.Harness == "codex" && m["prior"] == "turn_aborted" {
			pending = map[string]bool{} // the abort ended the turn and its calls, whatever record the store marked it on
			if r == "" {
				r = "aborted" // the store marked Codex's abort of the turn on the record after it
			} else if !aborted && !prevAt.IsZero() {
				// marked on an input with no record of its own: the turn ran until then at the latest, as Codex's
				// live tracker keeps a turn open until its abort
				prevAt, aborted = e.TS, true
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
			// over (a local command's caveat, a meta line): the next input's point reads it
			if prior, _ := m["prior"].(string); carried == "" && turnOver(harness, prior) {
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
				prevAt, aborted = e.TS, true
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
				out = append(out, Point{Session: id, Stop: stopOf(e.TS, e.Ref), Next: Next{At: e.TS, Kind: "owner", InputKind: "exit", Text: e.Summary},
					Entry: entry, Controls: controls})
			}
			ownerAt, controls, carried, words, resumed = e.TS, nil, "", "", time.Time{}
			tasks, pending, wake = map[string]replayTask{}, map[string]bool{}, replayWake{}
			prevAt, prevRole, prevPrior, owes, owesRef, exited = e.TS, "exit", "", time.Time{}, "", true
			continue
		}
		if exited { // the session runs again from this record: its jobs run from their first run after it
			restart(e.TS)
			exited = false
		}
		if r == "self" && strings.HasPrefix(e.Summary, "<task-notification>") && !strings.Contains(e.Summary, "</task-notification>") {
			e.Summary = whole(e) // the store cut it: read what it says from the transcript, while it is there
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
			if !prevAt.IsZero() {
				prevPrior = priorOf(m, carried)
				out = append(out, Point{Session: id, Stop: stopOf(e.TS, e.Ref), Next: next, Entry: entry, Controls: controls})
			}
		}
		if (r == "agent" || r == "call" || r == "apierr" || r == "synthetic") && !owes.IsZero() && e.TS.Sub(owes) >= stallFloor {
			// the wait ended after a long time without an input: the model's reply, or a reopening that closed the
			// turn. What the stalled rule would have done is the point.
			st := stopOf(e.TS, e.Ref)
			st.Working, st.Busy, st.Error = true, false, ""
			st.Phase = phaseOf(st)
			next := Next{At: e.TS, Kind: "self", Text: "the model's reply"}
			if r == "synthetic" {
				next = Next{At: e.TS, Kind: "owner", InputKind: "restart", Text: "the session was reopened"}
			}
			if r != "synthetic" || !st.RequestAt.IsZero() {
				// no Controls: a local command during the wait is a user record, which ends the wait on its request
				// here as in the live layer, so no stalled rule reads such a wait
				out = append(out, Point{Session: id, Stop: st, Next: next, Entry: entry, Waiting: true})
			}
		}
		why, _ := m["why"].(string)
		switch r {
		case "owner":
			ownerAt = e.TS
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
			if prior, _ := m["prior"].(string); prior == "scheduled_task_fire" { // a scheduled run started
				firesSeen = true
				f, ok := fireBetween(lastRef, e.Ref)
				if !ok { // the transcript is gone: the run whose prompt this is
					for k, c := range crons {
						if c.what != "" && strings.HasPrefix(strings.TrimSpace(e.Summary), strings.TrimSpace(c.what)) {
							f.TaskID = k
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
			if strings.HasPrefix(e.Summary, "<task-notification>") {
				if hWakeGone.MatchString(e.Summary) {
					wake = replayWake{}
				} else {
					waitedThrough("a task's notification came in while it waited", e.TS)
				}
			}
			st := hStatus.FindStringSubmatch(e.Summary)
			for _, tm := range hTaskID.FindAllStringSubmatch(e.Summary, -1) {
				t, ok := tasks[tm[1]]
				if !ok {
					continue
				}
				if t.kind != "monitor" || st != nil && terminalSt[st[1]] {
					delete(tasks, tm[1])
				}
			}
			if hOrphan.MatchString(e.Summary) { // reopened: what ran died with the process
				reopened(e.TS)
			}
			lastMainUser = true
		case "momentum": // a message momentum typed: it opens a turn, and is not the owner's
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
					crons[g[2]] = replayCron{expr: firstNonEmpty(c.cron, g[3]), at: e.TS, oneShot: g[1] == "one-shot", what: c.what, seen: recordsRunsAt(e, firesSeen)}
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
		prevRole, prevAt, carried, aborted = r, e.TS, "", false
		for _, at := range controls { // the stop they came after is over: they show him at the session before the next
			if at.After(ownerAt) {
				ownerAt = at
			}
		}
		controls = nil
		if r == "owner" || r == "self" || r == "interrupt" || r == "momentum" {
			prevPrior = ""
		}
		if r == "owner" || r == "self" || r == "momentum" {
			turnBegan = e.TS // an input opens the turn the next stop ends
		}
		switch {
		case r == "owner" || r == "self" || r == "momentum", r == "result" && len(pending) == 0:
			// Claude Code may write the request before the record of the input it carries, dated before it
			owes, owesRef = e.TS, firstNonEmpty(lastRef, e.Ref)
		case r == "agent" || r == "call" || r == "apierr" || r == "interrupt" || r == "synthetic":
			owes, owesRef = time.Time{}, ""
		}
		lastRef = e.Ref
	}
	if !prevAt.IsZero() {
		if carried != "" {
			prevPrior = carried // a record passed over after the last stop said its turn was over
		}
		out = append(out, Point{Session: id, Stop: stopOf(until, ""), Next: Next{Kind: "none"}, Entry: entry, Controls: controls})
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

// turnOver reports the harness's record that a turn ended, as the store keeps the record before an input.
func turnOver(harness, prior string) bool {
	switch prior {
	case "turn_duration", "stop_hook_summary", "away_summary", "task_complete", "turn_aborted", "stop":
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
