package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/text"
	"github.com/agent-sh/momentum/sessions/proc"
)

// Native finds Claude Code sessions from what Claude Code itself records:
//   - a tmux pane whose nearest agent process is Claude Code, with that process's probe file
//     (<config>/sessions/<pid>.json: session id, name, status);
//   - an interactive session outside tmux, from its probe file; one-shot -p runs are scripts, not sessions;
//   - a background session, from `claude agents --json`, the list Claude Code documents for scripts.
//
// It skips Claude Code's own agent view, the parked front end of a backgrounded session, a background
// session's host process, and a probe that a recycled pid left behind.
type Native struct {
	Env sessions.Env
	// Agents returns the output of `claude agents --json`. nil runs the claude binary; when there is none, there
	// are no background sessions.
	Agents func(ctx context.Context) ([]byte, error)
	// Every bounds how long a background-session list is reused while the Claude Code processes stay the same.
	// Zero reads it on every List.
	Every time.Duration

	cache   []agentRow
	sig     string
	at      time.Time
	failed  error // the last run's failure, reused like a list until Every passes
	failSig string
	failAt  time.Time
}

// NewNative returns a Native lister that reuses the background-session list for up to two minutes while the
// processes do not change: a board scanning every few seconds then runs the claude binary rarely.
func NewNative(env sessions.Env) *Native { return &Native{Env: env, Every: 2 * time.Minute} }

// probe is Claude Code's per-process state file.
type probe struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	StartedAt       any    `json:"startedAt"`
	ProcStart       any    `json:"procStart"`
	Kind            string `json:"kind"` // interactive, bg
	Name            string `json:"name"`
	NameSource      string `json:"nameSource"` // user, derived, auto, ...
	Status          string `json:"status"`     // busy, shell, idle, waiting
	WaitingFor      string `json:"waitingFor"`
	UpdatedAt       any    `json:"updatedAt"`
	StatusUpdatedAt any    `json:"statusUpdatedAt"`
	ParkedJobID     any    `json:"parkedJobId"`
}

// parked reports a front end whose session moved to the background; its status is frozen there.
func (p probe) parked() bool { s, _ := p.ParkedJobID.(string); return s != "" }

// agentRow is one entry of `claude agents --json`.
type agentRow struct {
	ID         string `json:"id"`
	PID        int    `json:"pid"`
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"` // interactive, background
	StartedAt  any    `json:"startedAt"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	State      string `json:"state"`  // background: working, blocked, done, failed, stopped
	Status     string `json:"status"` // live process: busy, waiting, idle
	WaitingFor string `json:"waitingFor"`
}

// List finds the live Claude Code sessions.
func (n *Native) List(ctx context.Context, w *sessions.World) ([]sessions.Found, error) {
	probes := n.probes(w)
	var out []sessions.Found
	attached := map[string]string{} // background job id -> the pane attached to it
	for _, pa := range w.PaneAgents() {
		if pa.Kind != "claude" {
			continue
		}
		p := w.Proc(pa.PID)
		sub := p.Arg(1)
		if sub == "attach" && p.Arg(2) != "" {
			attached[p.Arg(2)] = pa.Pane.ID
			continue
		}
		meta := probes[pa.PID]
		if sub == "agents" || meta.parked() || meta.Kind == "bg" {
			continue // Claude Code's own agent view, or a background session's host
		}
		out = append(out, n.live(meta, p, pa.Pane.Target(), pa.Pane.ID, pa.Pane.WindowName, pa.Pane.Path))
	}
	pids := make([]int, 0, len(probes))
	for pid := range probes {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		meta, p := probes[pid], w.Proc(pid)
		if w.InPane(pid) || meta.Kind != "interactive" || meta.parked() || sessions.KindOf(p.Argv) != "claude" {
			continue
		}
		if sub := p.Arg(1); sub == "agents" || sub == "attach" || p.Has("-p", "--print") {
			continue // the agent view, an attach client, or a scripted one-shot
		}
		out = append(out, n.live(meta, p, "tty", "", "", ""))
	}
	rows, err := n.agents(ctx, w)
	for _, e := range rows {
		if e.Kind != "background" || e.PID <= 0 {
			continue
		}
		p := w.Proc(e.PID)
		if p == nil {
			continue // its process has exited; the session is not live
		}
		meta, job := probes[e.PID], n.job(e.ID)
		// The list can be up to Every old; the probe and the state file are read now, so they win.
		status, waitingFor := e.Status, e.WaitingFor
		if meta.Status != "" {
			status, waitingFor = meta.Status, meta.WaitingFor
		}
		f := sessions.Found{Tool: "claude", Name: text.FirstNonEmpty(e.Name, e.ID), Named: true, Cwd: e.Cwd, Where: "bg",
			Pane: attached[e.ID], Job: e.ID, Session: e.SessionID, PID: e.PID, ProcStart: p.StartTicks,
			Started: epoch(e.StartedAt), Model: job.model(), PRs: job.prs(),
			Transcript: TranscriptPath(n.Env.ClaudeDir, e.SessionID, e.Cwd), Status: status, WaitingFor: waitingFor,
			JobState: text.FirstNonEmpty(job.State, e.State), Detail: text.OneLine(job.Detail, 240)}
		f.LastActive = latest(epoch(job.UpdatedAt), epoch(meta.StatusUpdatedAt), mtime(filepath.Join(n.Env.ClaudeDir, "jobs", e.ID, "timeline.jsonl")))
		out = append(out, f)
	}
	return out, err
}

// probes reads the probe file of every live Claude Code process. A probe whose process start differs from the
// live process with its pid belongs to an earlier process and is left out.
func (n *Native) probes(w *sessions.World) map[int]probe {
	out := map[int]probe{}
	files, _ := filepath.Glob(filepath.Join(n.Env.ClaudeDir, "sessions", "*.json"))
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			continue
		}
		p := w.Proc(pid)
		if p == nil {
			continue
		}
		var m probe
		if raw, err := os.ReadFile(f); err != nil || json.Unmarshal(raw, &m) != nil {
			continue
		}
		if s := numString(m.ProcStart); s != "" && s != strconv.FormatUint(p.StartTicks, 10) {
			continue
		}
		out[pid] = m
	}
	return out
}

// live describes an interactive session from its probe; a process without one still counts, by its pane.
func (n *Native) live(meta probe, p *proc.Proc, where, pane, fallback, cwd string) sessions.Found {
	f := sessions.Found{Tool: "claude", Name: text.FirstNonEmpty(meta.Name, fallback, "claude"), Cwd: text.FirstNonEmpty(meta.Cwd, cwd),
		Where: where, Pane: pane, PID: p.PID, ProcStart: p.StartTicks, Session: meta.SessionID, Started: epoch(meta.StartedAt),
		LastActive: latest(epoch(meta.StatusUpdatedAt), epoch(meta.UpdatedAt)), Status: meta.Status, WaitingFor: meta.WaitingFor}
	if meta.NameSource != "" {
		f.Named = meta.NameSource == "user"
	} else {
		f.Named = meta.Name != ""
	}
	f.Transcript = TranscriptPath(n.Env.ClaudeDir, meta.SessionID, f.Cwd)
	return f
}

// agents returns the background-session list, reusing the previous one while the Claude Code processes are the
// same and it is younger than Every. A failure keeps the previous list, and is reused the same way: a hung or
// missing command is not run again on every scan.
func (n *Native) agents(ctx context.Context, w *sessions.World) ([]agentRow, error) {
	var pids []string
	for _, p := range w.Procs.ByPID {
		if sessions.KindOf(p.Argv) == "claude" {
			pids = append(pids, strconv.Itoa(p.PID))
		}
	}
	sort.Strings(pids)
	sig := strings.Join(pids, ",")
	if n.cache != nil && sig == n.sig && n.Every > 0 && w.Now.Sub(n.at) < n.Every {
		return n.cache, nil
	}
	if n.failed != nil && sig == n.failSig && n.Every > 0 && w.Now.Sub(n.failAt) < n.Every {
		return n.cache, n.failed
	}
	run := n.Agents
	if run == nil {
		run = func(ctx context.Context) ([]byte, error) { return claudeAgents(ctx, n.Env.Home) }
	}
	out, err := run(ctx)
	var rows []agentRow
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		err = json.Unmarshal(out, &rows)
	}
	if err != nil {
		n.failed, n.failSig, n.failAt = fmt.Errorf("claude agents --json: %w", err), sig, w.Now
		return n.cache, n.failed
	}
	n.failed = nil
	n.cache, n.sig, n.at = rows, sig, w.Now
	if n.cache == nil {
		n.cache = []agentRow{}
	}
	return rows, nil
}

// claudeAgents runs `claude agents --json`, which only reads: it lists sessions and does not start the
// background supervisor. No claude binary means no background sessions.
func claudeAgents(ctx context.Context, home string) ([]byte, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		bin = filepath.Join(home, ".local", "bin", "claude") // where Claude Code's installer puts it
		if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
			return nil, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agents", "--json")
	cmd.WaitDelay = time.Second // a child that keeps the pipe open does not hold the scan
	return cmd.Output()
}

// jobFile is the part of a background session's state file used for what `claude agents --json` leaves out:
// its pull requests, model and detail line. Claude Code documents the file as unstable, so every field is
// optional.
type jobFile struct {
	State        string `json:"state"`
	Detail       string `json:"detail"`
	UpdatedAt    any    `json:"updatedAt"`
	RespawnFlags []any  `json:"respawnFlags"`
	Children     []struct {
		ID   any    `json:"id"`
		Kind string `json:"kind"`
	} `json:"children"`
}

func (n *Native) job(id string) jobFile {
	var j jobFile
	if raw, err := os.ReadFile(filepath.Join(n.Env.ClaudeDir, "jobs", id, "state.json")); err == nil {
		json.Unmarshal(raw, &j)
	}
	return j
}

func (j jobFile) prs() []string {
	var out []string
	for _, c := range j.Children {
		if c.Kind == "pr" {
			if id := numString(c.ID); id != "" {
				out = append(out, "#"+id)
			}
		}
	}
	return out
}

func (j jobFile) model() string {
	for i := 0; i+1 < len(j.RespawnFlags); i++ {
		if text.Str(j.RespawnFlags[i]) == "--model" {
			return text.Str(j.RespawnFlags[i+1])
		}
	}
	return ""
}

var reSlug = regexp.MustCompile(`[^A-Za-z0-9]`)

// TranscriptPath is where Claude Code keeps a session's transcript: <config>/projects/<cwd with every
// non-alphanumeric as ->/<session>.jsonl, or wherever a project directory holds that session. "" when none does.
func TranscriptPath(claudeDir, session, cwd string) string {
	if session == "" {
		return ""
	}
	p := filepath.Join(claudeDir, "projects", reSlug.ReplaceAllString(cwd, "-"), session+".jsonl")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if m, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", session+".jsonl")); len(m) > 0 {
		return m[0]
	}
	return ""
}

// epoch reads a time Claude Code wrote as Unix milliseconds, Unix seconds or an ISO string.
func epoch(v any) time.Time {
	switch x := v.(type) {
	case float64:
		if x <= 0 {
			return time.Time{}
		}
		if x > 1e11 {
			return time.UnixMilli(int64(x))
		}
		return time.UnixMilli(int64(x * 1000))
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t
		}
	}
	return time.Time{}
}

// numString renders a JSON number or string id.
func numString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func mtime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
