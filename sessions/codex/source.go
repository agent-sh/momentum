package codex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/text"

	_ "modernc.org/sqlite"
)

// Retain is how long the tracker of a thread that left keeps its place.
const Retain = 15 * time.Minute

// Source reads Codex sessions: a Lister finds them, a Tracker per rollout reads what they did, and the state
// database under Env.CodexHome names their subagents. Any Lister works; the database opens on first use.
type Source struct {
	Env    sessions.Env
	Lister sessions.Lister
	db     *stateDB

	saved    sessions.Saved
	trackers map[string]*Tracker
	seen     map[string]time.Time
	now      time.Time // the last scan's
}

// New reads Codex sessions with native discovery, which shares the Source's database handle.
func New(env sessions.Env) *Source {
	db := &stateDB{home: env.CodexHome}
	return &Source{Env: env, Lister: &Native{Env: env, db: db}, db: db}
}

func (src *Source) Tool() string { return "codex" }

// Close closes the state database, if it was opened.
func (src *Source) Close() error { return src.db.close() }

// Resume makes later scans start a rollout they meet for the first time from its checkpoint in saved.
func (src *Source) Resume(saved sessions.Saved) { src.saved = saved }

// Checkpoints returns the checkpoint of every rollout the last Scan read, its sessions' and their subagents'.
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

func (src *Source) state() *stateDB {
	if src.db == nil {
		src.db = &stateDB{home: src.Env.CodexHome}
	}
	return src.db
}

// Scan lists the live sessions and reads each rollout.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.trackers == nil {
		src.trackers, src.seen = map[string]*Tracker{}, map[string]time.Time{}
	}
	src.now = w.Now
	found, err := src.Lister.List(ctx, w)
	var out []*sessions.Session
	keys := map[string]bool{}
	for _, f := range found {
		if f.Host != "" {
			continue // a session on another machine: its rollout is not here
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
		s := &sessions.Session{Key: key, Tool: "codex", Name: f.Name, Named: f.Named, Cwd: f.Cwd, Project: src.Env.Project(f.Cwd),
			Where: f.Where, Pane: f.Pane, PID: f.PID, Model: f.Model, Started: f.Started, LastActive: f.LastActive,
			Transcript: f.Transcript, Tasks: []sessions.Task{}, Partial: f.Partial}
		if f.Transcript == "" {
			s.TurnOpen = f.Status == "busy" // no rollout yet: only Codex's own state says anything
			out = append(out, s)
			continue
		}
		if fi, err := os.Stat(f.Transcript); err == nil && fi.ModTime().After(s.LastActive) {
			s.LastActive = fi.ModTime()
		}
		t, terr := src.tracker(ctx, f.Transcript, w.Now)
		err = errors.Join(err, terr)
		t.Facts(s, w.Now)
		s.TurnOpen = t.Open()
		kids, kerr := src.children(ctx, f.Thread, w.Now)
		s.Tasks = append(s.Tasks, kids...)
		s.Partial = s.Partial || kerr != nil
		err = errors.Join(err, kerr)
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

// tracker returns the rollout's tracker: the one kept from an earlier scan, else one resumed from its checkpoint,
// else a new one that reads from the start. A checkpoint that cannot be read is reported, and the rollout is read
// from the start.
func (src *Source) tracker(ctx context.Context, path string, now time.Time) (*Tracker, error) {
	src.seen[path] = now
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

// children returns the subagent threads of a thread whose turn is running. A spawn edge stays open after its
// child finished, so the child's own rollout decides.
func (src *Source) children(ctx context.Context, thread string, now time.Time) ([]sessions.Task, error) {
	if thread == "" {
		return nil, nil
	}
	db, err := src.state().get()
	if db == nil || err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `select t.id, coalesce(t.agent_nickname, ''), coalesce(t.title, ''), coalesce(t.agent_role, ''),
		t.rollout_path, coalesce(t.created_at_ms, t.created_at*1000) from thread_spawn_edges e join threads t on t.id = e.child_thread_id
		where e.parent_thread_id = ? and e.status = 'open'`, thread)
	if err != nil {
		return nil, fmt.Errorf("subagents of %s: %w", thread, err)
	}
	type child struct {
		id, nick, title, role, path string
		created                     int64
	}
	var kids []child
	for rows.Next() {
		var c child
		if rows.Scan(&c.id, &c.nick, &c.title, &c.role, &c.path, &c.created) == nil {
			kids = append(kids, c)
		}
	}
	rerr := rows.Err()
	rows.Close()
	var out []sessions.Task
	for _, c := range kids {
		ct, err := src.tracker(ctx, c.path, now)
		rerr = errors.Join(rerr, err)
		ct.Update()
		if !ct.Open() {
			continue
		}
		k := sessions.Task{ID: c.id, Kind: "agent", Label: text.OneLine(text.FirstNonEmpty(c.nick, c.title), 90), Type: c.role, Started: time.UnixMilli(c.created)}
		if fi, err := os.Stat(c.path); err == nil {
			k.LastActive = fi.ModTime()
		}
		if p := ct.pending(); p != nil {
			k.Now = text.OneLine(p.Name+": "+p.Desc, 110)
		}
		out = append(out, k)
	}
	return out, rerr
}

// Native finds Codex sessions the way they show on the machine: the native Codex process nearest a tmux pane's
// shell, and any other Codex process outside tmux. It skips the app server (the desktop app and tool hosts) and
// a Codex that another Codex started. A process's thread is the rollout file it holds open.
type Native struct {
	Env sessions.Env
	db  *stateDB
}

// NewNative returns a Native lister over the state database under env.CodexHome. Close it when done; one that
// New made shares the Source's handle, which the Source closes.
func NewNative(env sessions.Env) *Native { return &Native{Env: env} }

// Close closes the state database, if it was opened.
func (n *Native) Close() error { return n.db.close() }

func (n *Native) state() *stateDB {
	if n.db == nil {
		n.db = &stateDB{home: n.Env.CodexHome}
	}
	return n.db
}

// List finds the live Codex sessions. A failed thread lookup still lists the process, without its thread.
func (n *Native) List(ctx context.Context, w *sessions.World) ([]sessions.Found, error) {
	var out []sessions.Found
	var errs []error
	for _, pa := range w.PaneAgents() {
		if pa.Kind != "codex" {
			continue
		}
		f, err := n.agent(ctx, w, pa.PID)
		errs = append(errs, err)
		f.Partial = err != nil // its thread is not known: it shows by its process until a lookup works
		f.Cwd, f.Where, f.Pane = text.FirstNonEmpty(f.Cwd, pa.Pane.Path), pa.Pane.Target(), pa.Pane.ID
		if f.Name == "codex" {
			f.Name = text.FirstNonEmpty(pa.Pane.WindowName, f.Name)
		}
		out = append(out, f)
	}
	pids := make([]int, 0, len(w.Procs.ByPID))
	for pid := range w.Procs.ByPID {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		p := w.Procs.ByPID[pid]
		if sessions.KindOf(p.Argv) != "codex" || w.InPane(pid) || p.Has("app-server") {
			continue
		}
		if parent := w.Proc(p.PPID); parent != nil && sessions.KindOf(parent.Argv) == "codex" {
			continue
		}
		f, err := n.agent(ctx, w, pid)
		errs = append(errs, err)
		f.Where = "tty"
		if f.Thread != "" {
			out = append(out, f)
		}
	}
	return out, errors.Join(errs...)
}

var reRollout = regexp.MustCompile(`rollout-.*-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// agent describes the session behind a Codex process: the main thread among the rollouts it holds open.
func (n *Native) agent(ctx context.Context, w *sessions.World, pid int) (sessions.Found, error) {
	p := w.Proc(pid)
	f := sessions.Found{Tool: "codex", Name: "codex", PID: pid, ProcStart: p.StartTicks}
	for _, c := range w.Procs.Children(pid) {
		if k := w.Proc(c); k != nil && k.Exe() == "systemd-inhibit" {
			f.Status = "busy" // Codex holds an inhibitor only while a turn runs
		}
	}
	var ids []string
	for _, target := range w.Procs.Files(pid) {
		if m := reRollout.FindStringSubmatch(target); m != nil {
			ids = append(ids, m[1])
		}
	}
	th, err := n.state().main(ctx, ids)
	if th == nil {
		return f, err
	}
	f.Thread, f.Cwd, f.Model = th.id, th.cwd, th.model
	f.Named = th.name != ""
	f.Name = text.FirstNonEmpty(text.OneLine(text.FirstNonEmpty(th.name, th.title), 60), f.Name)
	f.Transcript, f.Started, f.LastActive = th.rollout, th.created, th.updated
	if fi, err := os.Stat(th.rollout); err == nil && fi.ModTime().After(f.LastActive) {
		f.LastActive = fi.ModTime()
	}
	return f, nil
}

// stateDB is Codex's state database: the newest state_<n>.sqlite under the Codex home, opened read-only. Each use
// looks again, so a Codex that moved to a newer file is followed.
type stateDB struct {
	home string
	path string
	db   *sql.DB
}

// get returns the database, or nil while Codex has none here.
func (s *stateDB) get() (*sql.DB, error) {
	files, _ := filepath.Glob(filepath.Join(s.home, "state_*.sqlite"))
	best, bestN := "", -1
	for _, f := range files {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "state_"), ".sqlite"))
		if err == nil && n > bestN {
			best, bestN = f, n
		}
	}
	if best != s.path {
		s.close()
		if best != "" {
			db, err := sql.Open("sqlite", "file:"+best+"?mode=ro&_pragma=busy_timeout(500)")
			if err != nil {
				return nil, err
			}
			s.db, s.path = db, best
		}
	}
	return s.db, nil
}

// close closes the database; closing again, or a nil stateDB, is a no-op.
func (s *stateDB) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db, s.path = nil, ""
	return err
}

type thread struct {
	id, name, title, cwd, model, rollout string
	created, updated                     time.Time
	subagent                             bool
}

// main returns the thread among ids that is not a subagent's; after /new or /resume a process can hold two, and
// the one updated last is current. A subagent thread can have no role, so the role alone does not tell; its
// source and thread_source do.
func (s *stateDB) main(ctx context.Context, ids []string) (*thread, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	db, err := s.get()
	if db == nil || err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `select * from threads where id in (?`+strings.Repeat(",?", len(ids)-1)+`)`, anys(ids)...)
	if err != nil {
		return nil, fmt.Errorf("threads: %w", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var best *thread
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		r := map[string]any{}
		for i, c := range cols {
			r[c] = vals[i]
		}
		th := &thread{id: str(r["id"]), name: str(r["name"]), title: str(r["title"]), cwd: str(r["cwd"]), model: str(r["model"]),
			rollout: str(r["rollout_path"]), created: ms(r["created_at_ms"], r["created_at"]), updated: ms(r["updated_at_ms"], r["updated_at"])}
		src, tsrc := str(r["source"]), str(r["thread_source"])
		th.subagent = str(r["agent_role"]) != "" || strings.HasPrefix(strings.TrimSpace(src), "{") || tsrc == "subagent" || tsrc == "guardian_review"
		if !th.subagent && (best == nil || th.updated.After(best.updated)) {
			best = th
		}
	}
	return best, rows.Err()
}

func anys(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

// ms reads a millisecond column, else a seconds column.
func ms(milli, sec any) time.Time {
	if n, ok := milli.(int64); ok && n > 0 {
		return time.UnixMilli(n)
	}
	if n, ok := sec.(int64); ok && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}
