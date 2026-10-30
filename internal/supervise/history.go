package supervise

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
}

// Next is the input that ended a stop: the owner's (a turn, a slash command, a shell command, an interruption),
// the session's own (a task's notification, a scheduled wakeup, another session's message), momentum's own
// message, or none before the window ended.
type Next struct {
	At        time.Time
	Kind      string // owner, self, momentum, none
	InputKind string // the owner's: typed, queued, slash, shell, answer, interrupt
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
	hCron      = regexp.MustCompile(`Scheduled (recurring|one-shot) job (\w+) \(([^)]*)\)`)
	hTaskID    = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	hStatus    = regexp.MustCompile(`<status>(\w+)</status>`)
	hTaskStop  = regexp.MustCompile(`"task_id":"(\w+)"`)
	hCronDel   = regexp.MustCompile(`"id":"(\w+)"`)
	hCutCall   = regexp.MustCompile(`^\[Tool call interrupted: the session ended before`)
	hOrphan    = regexp.MustCompile(`didn't finish before|__orphan_summary__`)
	terminalSt = map[string]bool{"completed": true, "failed": true, "killed": true, "stopped": true, "error": true, "timeout": true, "cancelled": true}
)

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
	case ik == "slash", ik == "shell":
		return "owner"
	case strings.HasPrefix(s, "[Request interrupted"):
		return "interrupt"
	}
	return ""
}

type replayTask struct {
	kind  string
	until time.Time // a monitor's expiry
}

// timeline turns one session's events into its points: one for each input, with the stop before it, and one
// for the last stop, whose Next is none.
func timeline(id string, evs []store.Event, until time.Time) []Point {
	var out []Point
	tasks := map[string]replayTask{}
	crons := map[string]string{}
	var wakeAt, ownerAt, resumed time.Time
	var words, entry, harness, prevRole, prevPrior string
	var prevAt time.Time
	pending := map[string]bool{} // calls without a result yet
	lastMainUser := false
	stopOf := func(at time.Time) Stop {
		s := Stop{Key: id, Tool: harness, At: prevAt, Words: words, OwnerAt: ownerAt, ResumedAt: resumed}
		for k, t := range tasks {
			if t.kind == "monitor" && !t.until.IsZero() && t.until.Before(at) {
				delete(tasks, k)
			}
		}
		s.Tasks = len(tasks)
		if !wakeAt.IsZero() && wakeAt.After(prevAt.Add(-sessions.WakeGrace)) {
			s.WakeAt = wakeAt
		}
		for _, label := range crons {
			s.Loop = label
		}
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
		if r == "" || r == "compaction" { // a compaction runs as a turn starts or mid-turn: no stop of its own
			continue
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
				prior, _ := m["prior"].(string)
				prevPrior = prior
				out = append(out, Point{Session: id, Stop: stopOf(e.TS), Next: next, Entry: entry})
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
			wakeAt, resumed = time.Time{}, time.Time{}
			lastMainUser = true
		case "self":
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
				resumed = e.TS
				tasks, crons = map[string]replayTask{}, map[string]string{}
			}
			lastMainUser = true
		case "momentum": // a message momentum typed: it opens a turn, and is not the owner's
			wakeAt, resumed = time.Time{}, time.Time{}
			lastMainUser = true
		case "interrupt": // its owner pressed Esc or turned a call down: he was there, as the live layer counts it
			ownerAt = e.TS
			lastMainUser = false
		case "call":
			id, _ := m["tool_id"].(string)
			pending[id] = true
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
				resumed = e.TS
				tasks, crons = map[string]replayTask{}, map[string]string{}
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
					wakeAt = e.TS.Add(time.Duration(n) * time.Second)
				}
				if g := hCron.FindStringSubmatch(s); g != nil && e.Tool == "CronCreate" {
					crons[g[2]] = g[3]
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
		prevRole, prevAt = r, e.TS
		if r == "owner" || r == "self" || r == "interrupt" || r == "momentum" {
			prevPrior = ""
		}
	}
	if !prevAt.IsZero() {
		out = append(out, Point{Session: id, Stop: stopOf(until), Next: Next{Kind: "none"}, Entry: entry})
	}
	return out
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
