// Package pi reads pi coding agent sessions: a pi process, the session file it writes under the agent
// directory, and an incremental tracker of that file.
package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/jsonl"
	"github.com/agent-sh/momentum/sessions/internal/text"
	"github.com/agent-sh/momentum/sessions/proc"
)

// Retain is how long the tracker of a session that left keeps its place.
const Retain = 15 * time.Minute

type call struct {
	Name string    `json:"name"`
	Desc string    `json:"desc"`
	At   time.Time `json:"at"`
}

// Tracker follows one pi session file. It reads the file once from the start and then only what was appended, or
// resumes from a checkpoint and reads only what came after it.
type Tracker struct {
	rd jsonl.Reader
	st track
}

// track is what the session file's lines decided, and nothing else, so a checkpoint of it fed the rest of the file
// ends where a full re-read ends. A checkpoint saves every exported field; the log is saved apart.
type track struct {
	Open     bool             `json:"open"`
	LastText string           `json:"last_text"`
	ErrText  string           `json:"err_text"`
	Calls    map[string]*call `json:"calls"`
	Model    string           `json:"model"`
	log      sessions.Log
}

// NewTracker follows the session file at path.
func NewTracker(path string) *Tracker {
	return &Tracker{rd: jsonl.Reader{Path: path}, st: track{Calls: map[string]*call{}}}
}

// Log is the session's activity log.
func (t *Tracker) Log() *sessions.Log { return &t.st.log }

// checkpointState is a Tracker's checkpoint: where it stands in the file and what the lines so far decided.
type checkpointState struct {
	Pos   jsonl.Position `json:"pos"`
	Track track          `json:"track"`
}

// State encodes where the tracker stands and what it derived, without its log, for a Checkpoint.
func (t *Tracker) State() ([]byte, error) {
	return json.Marshal(checkpointState{Pos: t.rd.Position(), Track: t.st})
}

// Resume continues from a checkpoint of this file. The next Update reads what came after it, or starts over when
// the file is no longer the one the checkpoint read.
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

type piLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	ModelID   string `json:"modelId"`
	Message   *struct {
		Role       string `json:"role"`
		StopReason string `json:"stopReason"`
		Error      string `json:"errorMessage"`
		ToolCallID string `json:"toolCallId"`
		Content    []struct {
			Type      string         `json:"type"`
			Text      string         `json:"text"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"content"`
	} `json:"message"`
}

// Update reads what was appended; a file that shrank or was replaced is read again from the start.
func (t *Tracker) Update() int64 {
	return t.rd.Read(func() { t.st = track{Calls: map[string]*call{}} }, t.line)
}

func (t *Tracker) line(raw []byte) {
	var e piLine
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	st := &t.st
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	if e.Type == "model_change" {
		st.Model = e.ModelID
	}
	if e.Type != "message" || e.Message == nil {
		return
	}
	m := e.Message
	switch m.Role {
	case "user":
		st.Open, st.ErrText = true, ""
		for _, c := range m.Content {
			if c.Type == "text" {
				st.log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text.OneLine(c.Text, 1200)})
			}
		}
	case "toolResult":
		delete(st.Calls, m.ToolCallID)
		st.Open = true
		for _, c := range m.Content {
			if c.Type == "text" {
				st.log.Add(sessions.Entry{Role: sessions.RoleOut, Text: text.OneLine(c.Text, 160)})
			}
		}
	case "assistant":
		st.Calls = map[string]*call{}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					st.LastText = c.Text
					st.log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: text.OneLine(c.Text, 900)})
				}
			case "toolCall":
				d := text.Describe(c.Name, c.Arguments)
				st.Calls[c.ID] = &call{Name: c.Name, Desc: d, At: at}
				st.log.Add(sessions.Entry{Role: sessions.RoleTool, Name: c.Name, Text: text.OneLine(d, 200)})
			}
		}
		st.Open = m.StopReason == "toolUse"
		if m.StopReason == "error" {
			st.ErrText = text.OneLine(m.Error, 200)
		}
	}
}

var reCreated = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2})-(\d{2})-(\d{2})-(\d{3})Z_`)

// Created reads the creation time pi puts in a session file's name (2026-09-20T20-34-02-152Z_<id>.jsonl): an ISO
// time with dashes for its colons and its decimal point.
func Created(path string) time.Time {
	m := reCreated.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, m[1]+":"+m[2]+":"+m[3]+"."+m[4]+"Z")
	if err != nil {
		return time.Time{}
	}
	return t
}

// SessionDir is where pi keeps the sessions of a working directory: the leading separator dropped, every other
// separator and colon turned into a dash, wrapped in double dashes.
func SessionDir(agentDir, cwd string) string {
	safe := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.TrimPrefix(strings.TrimPrefix(cwd, "/"), `\`))
	return filepath.Join(agentDir, "sessions", "--"+safe+"--")
}

// Source finds pi processes and reads the session file each one writes.
type Source struct {
	Env      sessions.Env
	saved    sessions.Saved
	trackers map[string]*Tracker
	seen     map[string]time.Time
	now      time.Time // the last scan's
}

// New reads pi sessions.
func New(env sessions.Env) *Source { return &Source{Env: env} }

func (src *Source) Tool() string { return "pi" }

func (src *Source) Close() error { return nil }

// Resume makes later scans start a session file they meet for the first time from its checkpoint in saved.
func (src *Source) Resume(saved sessions.Saved) { src.saved = saved }

// Checkpoints returns the checkpoint of every session file the last Scan read.
func (src *Source) Checkpoints() []sessions.Checkpoint {
	var out []sessions.Checkpoint
	for path, at := range src.seen {
		if !at.Equal(src.now) {
			continue
		}
		t := src.trackers[path]
		if state, err := t.State(); err == nil {
			out = append(out, sessions.Checkpoint{Tool: src.Tool(), Key: path, State: state, Log: t.Log()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// tracker returns the session file's tracker: the one kept from an earlier scan, else one resumed from its
// checkpoint, else a new one that reads from the start. A checkpoint that cannot be read is reported, and the file
// is read from the start.
func (src *Source) tracker(ctx context.Context, path string) (*Tracker, error) {
	if t := src.trackers[path]; t != nil {
		return t, nil
	}
	t := NewTracker(path)
	src.trackers[path] = t
	if src.saved == nil {
		return t, nil
	}
	cp, err := src.saved.Checkpoint(ctx, src.Tool(), path)
	if err == nil && cp != nil {
		if err = t.Resume(cp); err != nil {
			*t = *NewTracker(path)
		}
	}
	if err != nil {
		return t, fmt.Errorf("checkpoint of %s: %w", path, err)
	}
	return t, nil
}

// Scan returns the session of every pi process. A pi process's session is the file created in its window
// (after it started, before the next pi in the same directory started); a resumed session was created earlier,
// so it falls back to the newest file it has written to since it started.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.trackers == nil {
		src.trackers, src.seen = map[string]*Tracker{}, map[string]time.Time{}
	}
	src.now = w.Now
	var ps []*proc.Proc
	for _, p := range w.Procs.ByPID {
		if p.Has("-p", "--print") {
			continue // scripted one-shots, not sessions
		}
		if p.Is("pi") || strings.Contains(strings.Join(p.Argv[:min(2, len(p.Argv))], " "), "pi-coding-agent") {
			ps = append(ps, p)
		}
	}
	used := map[string]bool{}
	var out []*sessions.Session
	var errs []error
	for _, slot := range w.Procs.Slots(ps) {
		s, err := src.session(ctx, slot, used, w)
		if s != nil {
			out = append(out, s)
		}
		errs = append(errs, err)
	}
	for path, at := range src.seen {
		if w.Now.Sub(at) > Retain {
			delete(src.trackers, path)
			delete(src.seen, path)
		}
	}
	return out, errors.Join(errs...)
}

// session finds and reads the session of one pi process. The error is a checkpoint that could not be read; the
// session is read all the same.
func (src *Source) session(ctx context.Context, slot proc.Slot, used map[string]bool, w *sessions.World) (*sessions.Session, error) {
	p := slot.Proc
	files, _ := filepath.Glob(filepath.Join(SessionDir(src.Env.PiDir, slot.Cwd), "*.jsonl"))
	best, bestT, fallback, fallbackT := "", time.Time{}, "", time.Time{}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || used[f] || fi.ModTime().Before(p.Start.Add(-time.Second)) {
			continue // a session file this process has not written to
		}
		created := Created(f)
		if !created.Before(p.Start.Add(-time.Second)) && (slot.Next.IsZero() || created.Before(slot.Next.Add(-time.Second))) {
			if fi.ModTime().After(bestT) {
				best, bestT = f, fi.ModTime()
			}
		} else if fi.ModTime().After(fallbackT) {
			fallback, fallbackT = f, fi.ModTime()
		}
	}
	if best == "" {
		best, bestT = fallback, fallbackT
	}
	if best == "" {
		return nil, nil
	}
	used[best] = true
	t, err := src.tracker(ctx, best)
	src.seen[best] = w.Now
	project := src.Env.Project(slot.Cwd)
	s := &sessions.Session{Key: "pi:" + strings.TrimSuffix(filepath.Base(best), ".jsonl"), Tool: "pi", Name: "pi in " + project,
		Cwd: slot.Cwd, Project: project, Where: "tty", PID: p.PID, Started: p.Start, LastActive: bestT, Tasks: []sessions.Task{},
		Transcript: best}
	s.Size = t.Update()
	s.Log = &t.st.log
	s.Model = t.st.Model
	s.LastText = strings.TrimSpace(t.st.LastText)
	s.TurnOpen = t.st.Open
	nowID := ""
	for id, c := range t.st.Calls { // the latest call; of two at once, the first by id
		if c.At.After(s.NowSince) || c.At.Equal(s.NowSince) && id < nowID {
			s.Now, s.NowSince, nowID = text.OneLine(c.Name+": "+c.Desc, 140), c.At, id
		}
	}
	if t.st.ErrText != "" && !t.st.Open {
		s.Ask, s.AskKind = "stopped on an error: "+t.st.ErrText, sessions.AskError
	}
	return s, err
}
