// Package sessions is the live session layer: which agent sessions run on this machine (Claude Code, Codex, pi,
// opencode, Hermes) and what each one is doing, read exactly from processes, terminals, transcripts and the
// harnesses' own state files. No model takes part in it.
//
// A Source per harness turns what it finds into Sessions; Scan runs every source over one snapshot of the
// machine (a World) and applies the phase rule. Sources keep incremental trackers, so a caller that scans
// repeatedly (a board refreshing every few seconds) reads only what each transcript gained since the last scan.
// A caller that runs once and exits keeps them as checkpoints (Resumable): the next run resumes each tracker and
// reads only what was appended.
//
// What else persists between runs (when each session entered its phase, what changed, dismissed asks) is in
// package state; the caller stores it. Judging a finished turn with a model is in package verdict; the caller
// makes the call.
//
// User-role text in a session's Log is what the harness recorded in the user role. Typed words, a queued or
// pasted message, a program typing into the terminal and a scheduled prompt can all land there, so it is not
// attribution to the owner. Evidence about the owner comes from tacit's attribution, not from this package.
package sessions

import (
	"context"
	"strconv"
	"time"

	"github.com/agent-sh/momentum/sessions/internal/text"
)

// Phase says what a session is doing and whether it waits on its owner.
type Phase string

const (
	NeedsYou   Phase = "needs_you"  // stopped until its owner acts: it asks something, or stopped on an error
	Working    Phase = "working"    // a turn is running
	Background Phase = "background" // turn over, but its own shell jobs, subagents, workflows or monitors still run
	Scheduled  Phase = "scheduled"  // turn over, nothing in flight; a wakeup or a loop starts the next turn
	Idle       Phase = "idle"       // turn over, nothing in flight, not judged yet (it may end in needs_you or done)
	Done       Phase = "done"       // a review found a finished result that asks nothing
)

// Order is the order a board lists phases in: what needs its owner first.
var Order = map[Phase]int{NeedsYou: 0, Idle: 1, Working: 2, Background: 3, Scheduled: 4, Done: 5}

// AskKind says what a pending ask is.
type AskKind string

const (
	AskQuestion   AskKind = "question"   // a question tool, or a harness waiting for input
	AskPlan       AskKind = "plan"       // a plan waiting for approval
	AskPermission AskKind = "permission" // a tool, sandbox or worker permission
	AskError      AskKind = "error"      // stopped on an error
	AskModel      AskKind = "model"      // set only by a review (package verdict), never by a scanner
)

// Task is one piece of work a session started and has not been told is over.
type Task struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"` // bash, agent, workflow, monitor
	Label      string    `json:"label"`
	Type       string    `json:"type,omitempty"` // a subagent's type or role
	Started    time.Time `json:"started"`
	LastActive time.Time `json:"last_active,omitzero"`
	Now        string    `json:"now,omitempty"`  // a subagent's latest action
	Sync       bool      `json:"sync,omitempty"` // a subagent the current turn waits on
}

// Session is one live agent session and the facts the phase rule reads.
type Session struct {
	Key        string    `json:"key"` // stable across scans: a job, thread or session id, else the process
	Tool       string    `json:"tool"`
	Name       string    `json:"name"`
	Named      bool      `json:"named,omitempty"` // named on purpose, not a default the harness derived
	Project    string    `json:"project"`
	Cwd        string    `json:"cwd"`
	Where      string    `json:"where"` // a tmux target, "tty", "bg", or a Hermes platform
	Pane       string    `json:"pane,omitempty"`
	Job        string    `json:"job,omitempty"`
	PID        int       `json:"pid,omitempty"`
	Model      string    `json:"model,omitempty"`
	PRs        []string  `json:"prs,omitempty"`
	Phase      Phase     `json:"phase"`
	Since      time.Time `json:"since"` // when it entered Phase; set by state.Stamps
	TurnOpen   bool      `json:"turn_open,omitempty"`
	Now        string    `json:"now,omitempty"` // the tool call the current turn waits on
	NowSince   time.Time `json:"now_since,omitzero"`
	Ask        string    `json:"ask,omitempty"`
	AskKind    AskKind   `json:"ask_kind,omitempty"`
	AskAsync   bool      `json:"ask_async,omitempty"` // the ask does not hold the turn; it blocks once the turn ends
	Failed     string    `json:"failed,omitempty"`    // the harness reports this background job failed; its detail
	JobState   string    `json:"job_state,omitempty"` // a background job's state in its harness's own words
	Tasks      []Task    `json:"tasks"`
	WakeAt     time.Time `json:"wake_at,omitzero"`
	Loop       string    `json:"loop,omitempty"`
	LastText   string    `json:"last_text,omitempty"` // the last assistant message in full
	LastActive time.Time `json:"last_active"`
	// UserAt is when user-role text last opened a turn, by the harness's record. Whoever typed it: a program
	// that types into the terminal counts too.
	UserAt time.Time `json:"user_at,omitzero"`
	// ControlAt is when its user last steered the session without a prompt, by the harness's record: a local
	// command such as /login, an interruption, or an answer to its question or plan.
	ControlAt time.Time `json:"control_at,omitzero"`
	// ResumedAt is when the harness reopened the session after it ended mid-task (Claude Code's resume records:
	// a call or a background task the end cut off), when nothing has happened in it since. Zero otherwise.
	ResumedAt time.Time `json:"resumed_at,omitzero"`
	// RequestAt is when the turn sent the model request it still waits on: the last record of its conversation is
	// the request (Claude Code's api-request records), and no reply, error or input has come after it. Zero
	// otherwise, and for a harness that does not record its requests.
	RequestAt time.Time `json:"request_at,omitzero"`
	// Queued counts the messages its user typed while the turn ran, which the harness holds to send when the turn
	// allows.
	Queued     int       `json:"queued,omitempty"`
	Started    time.Time `json:"started"`
	Transcript string    `json:"transcript,omitempty"`
	Size       int64     `json:"-"` // how much of the transcript or database has been read; -1 when unreadable
	Seq        int       `json:"seq"`
	Auto       bool      `json:"auto,omitempty"` // an automated run (cron, one-shot): counted, not listed
	// Partial says the source could not read all of the session on this scan: a query failed, or its identity is
	// not known yet. Its facts may be short, so a caller that keeps history does not count it as a change.
	Partial bool `json:"partial,omitempty"`
	Log     *Log `json:"-"` // the tracker's live activity log; copy what you need before the next scan
}

// Found is one session a Lister discovered, before its transcript is read.
type Found struct {
	Tool       string
	Name       string
	Named      bool
	Cwd        string
	Where      string
	Pane       string
	PID        int
	ProcStart  uint64 // the process's start in clock ticks; with PID it names one process
	Job        string // a Claude Code background job id
	Session    string // a Claude Code session id
	Thread     string // a Codex thread id
	Started    time.Time
	LastActive time.Time
	Model      string
	PRs        []string
	Transcript string
	Status     string // the harness's own live status (busy, shell, idle, waiting); never a value a board derived
	WaitingFor string // what the harness says the session waits on, when Status is waiting
	JobState   string // a background job's state in its harness's own words
	Detail     string // a background job's own detail line
	Host       string // set for a session on another machine; sources drop those, their files are not here
	Partial    bool   // the lister could not read all of it: its identity or its facts may be missing
}

// Key is the session's identity across scans: its job, thread or session id, else its process (pid and start
// time). Never a tmux pane id, which a restarted tmux server hands out again, nor a name. "" when the row has
// no identity at all.
func (f Found) Key() string {
	if id := text.FirstNonEmpty(f.Job, f.Thread, f.Session); id != "" {
		return id
	}
	if f.PID <= 0 {
		return ""
	}
	k := f.Tool + ":proc:" + strconv.Itoa(f.PID)
	if f.ProcStart > 0 {
		k += ":" + strconv.FormatUint(f.ProcStart, 10)
	}
	return k
}

// Lister finds the live sessions of one harness. The native listers read processes, tmux and the harness's
// files; another implementation can wrap an external tool's list.
type Lister interface {
	List(ctx context.Context, w *World) ([]Found, error)
}

// Source turns one harness's live sessions into Sessions with their facts filled in. Scan applies the phase
// rule afterwards. A harness that is not installed yields no sessions and no error. A Source keeps trackers
// between scans and is not safe for concurrent use: scan from one goroutine.
type Source interface {
	Tool() string
	Scan(ctx context.Context, w *World) ([]*Session, error)
	Close() error
}
