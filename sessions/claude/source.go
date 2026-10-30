package claude

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/text"
	"github.com/agent-sh/momentum/sessions/term"
)

// Source reads Claude Code sessions: a Lister finds them, a Tracker per transcript reads what they did.
type Source struct {
	Env    sessions.Env
	Lister sessions.Lister
	// Screen returns the visible screen of a session's terminal, for the permission dialog of a Claude Code that
	// does not report its own status. nil reads the tmux pane, else the kitty window.
	Screen func(ctx context.Context, w *sessions.World, s *sessions.Session) string

	trackers map[string]*Tracker
	seen     map[string]time.Time
}

// Retain is how long the tracker of a session that left keeps its place: one back within it is not read again.
const Retain = 15 * time.Minute

// stalledAfter is how long a tool call waits before the terminal is checked for a permission dialog.
const stalledAfter = 8 * time.Second

// New reads Claude Code sessions with native discovery.
func New(env sessions.Env) *Source { return &Source{Env: env, Lister: NewNative(env)} }

func (src *Source) Tool() string { return "claude" }

func (src *Source) Close() error { return nil }

// Claude Code names an unnamed session after its directory plus two hex digits ("projects-ce").
var reAutoName = regexp.MustCompile(`^[\w.-]+-[0-9a-f]{2}$`)

// Scan lists the live sessions and reads each transcript.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.trackers == nil {
		src.trackers, src.seen = map[string]*Tracker{}, map[string]time.Time{}
	}
	found, err := src.Lister.List(ctx, w)
	var out []*sessions.Session
	keys := map[string]bool{}
	for _, f := range found {
		if f.Host != "" {
			continue // a session on another machine: its transcript is not here
		}
		if f.ProcStart == 0 && f.PID > 0 {
			if p := w.Proc(f.PID); p != nil {
				f.ProcStart = p.StartTicks
			}
		}
		key := f.Key()
		if key == "" || keys[key] {
			continue
		}
		keys[key] = true
		s := &sessions.Session{Key: key, Tool: "claude", Name: f.Name, Named: f.Named && !reAutoName.MatchString(f.Name),
			Cwd: f.Cwd, Project: w.Env.Project(f.Cwd), Where: f.Where, Pane: f.Pane, Job: f.Job, PID: f.PID, Model: f.Model,
			PRs: f.PRs, Started: f.Started, LastActive: f.LastActive, JobState: f.JobState, Tasks: []sessions.Task{}}
		if f.JobState == "failed" {
			s.Failed = text.FirstNonEmpty(text.OneLine(f.Detail, 200), "the job failed")
		}
		src.facts(ctx, w, s, f)
		out = append(out, s)
	}
	for path, at := range src.seen {
		if w.Now.Sub(at) > Retain {
			delete(src.trackers, path)
			delete(src.seen, path)
		}
	}
	return out, err
}

// facts fills what the transcript and the harness's own status say.
func (src *Source) facts(ctx context.Context, w *sessions.World, s *sessions.Session, f sessions.Found) {
	path := f.Transcript
	if path == "" && f.Session != "" {
		path = TranscriptPath(src.Env.ClaudeDir, f.Session, f.Cwd)
	}
	s.Transcript = path
	if path == "" {
		// No transcript yet: only the harness's own status says anything, never a value a board wrote back.
		switch f.Status {
		case "busy":
			s.TurnOpen = true
		case "shell":
			s.Tasks = append(s.Tasks, shellTask())
		case "waiting":
			s.Ask, s.AskKind = waiting(f.WaitingFor, "")
		}
		return
	}
	src.seen[path] = w.Now
	if fi, err := os.Stat(path); err == nil && fi.ModTime().After(s.LastActive) {
		s.LastActive = fi.ModTime()
	}
	t := src.trackers[path]
	if t == nil {
		t = NewTracker(path)
		src.trackers[path] = t
	}
	if f.Where == "bg" {
		s.Started = time.Time{} // a background job restarts its process on respawn; its tasks outlive that
	}
	t.Facts(s, w.Now)
	s.TurnOpen = t.TurnOpen()
	switch f.Status {
	case "waiting": // a dialog only its owner can answer: a permission, a sandbox request, input
		if s.AskKind != sessions.AskQuestion && s.AskKind != sessions.AskPlan {
			s.Ask, s.AskKind = waiting(f.WaitingFor, s.Now)
		}
	case "shell": // its own count of running shells (commands and monitors), which outlive a process restart
		if !hasKind(s.Tasks, "bash") && !hasKind(s.Tasks, "monitor") {
			s.Tasks = append(s.Tasks, shellTask())
		}
	case "":
		// A Claude Code that reports no status: look for a permission dialog on its screen once a call stalls.
		if s.TurnOpen && !s.NowSince.IsZero() && w.Now.Sub(s.NowSince) > stalledAfter {
			if q := term.PermissionPrompt(src.screen(ctx, w, s)); q != "" {
				s.Ask, s.AskKind = q, sessions.AskPermission
			}
		}
	}
}

func (src *Source) screen(ctx context.Context, w *sessions.World, s *sessions.Session) string {
	switch {
	case src.Screen != nil:
		return src.Screen(ctx, w, s)
	case s.Pane != "":
		return term.CapturePane(ctx, s.Pane)
	case s.PID > 0:
		return term.KittyScreen(ctx, w.Env.Proc, s.PID)
	}
	return ""
}

// waiting turns Claude Code's waitingFor into an ask. Every value means its owner must act; permission,
// sandbox, worker and goal requests are permissions, anything else is a question.
func waiting(what, now string) (string, sessions.AskKind) {
	kind := sessions.AskQuestion
	switch w := strings.ToLower(what); {
	case strings.Contains(w, "permission"), strings.HasPrefix(w, "approve "), w == "sandbox request", w == "worker request", w == "goal proposal":
		kind = sessions.AskPermission
	}
	ask := text.FirstNonEmpty(what, "waiting for input")
	if now != "" {
		ask += ": " + now
	}
	return text.OneLine(ask, 300), kind
}

func shellTask() sessions.Task {
	return sessions.Task{ID: "shell", Kind: "bash", Label: "background shell (from Claude Code's status)"}
}

func hasKind(ts []sessions.Task, kind string) bool {
	for _, t := range ts {
		if t.Kind == kind {
			return true
		}
	}
	return false
}
