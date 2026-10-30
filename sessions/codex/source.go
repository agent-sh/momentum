package codex

import (
	"context"
	"database/sql"
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
// database names their subagents.
type Source struct {
	Env    sessions.Env
	Lister sessions.Lister
	db     *stateDB

	trackers map[string]*Tracker
	seen     map[string]time.Time
}

// New reads Codex sessions with native discovery.
func New(env sessions.Env) *Source {
	db := &stateDB{home: env.CodexHome}
	return &Source{Env: env, Lister: &Native{Env: env, db: db}, db: db}
}

func (src *Source) Tool() string { return "codex" }

func (src *Source) Close() error { return src.db.close() }

// Scan lists the live sessions and reads each rollout.
func (src *Source) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	if src.trackers == nil {
		src.trackers, src.seen = map[string]*Tracker{}, map[string]time.Time{}
	}
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
		s := &sessions.Session{Key: key, Tool: "codex", Name: f.Name, Named: f.Named, Cwd: f.Cwd, Project: w.Env.Project(f.Cwd),
			Where: f.Where, Pane: f.Pane, PID: f.PID, Model: f.Model, Started: f.Started, LastActive: f.LastActive,
			Transcript: f.Transcript, Tasks: []sessions.Task{}}
		if f.Transcript == "" {
			s.TurnOpen = f.Status == "busy" // no rollout yet: only Codex's own state says anything
			out = append(out, s)
			continue
		}
		if fi, err := os.Stat(f.Transcript); err == nil && fi.ModTime().After(s.LastActive) {
			s.LastActive = fi.ModTime()
		}
		t := src.tracker(f.Transcript, w.Now)
		t.Facts(s, w.Now)
		s.TurnOpen = t.Open()
		s.Tasks = append(s.Tasks, src.children(ctx, f.Thread, w.Now)...)
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

func (src *Source) tracker(path string, now time.Time) *Tracker {
	t := src.trackers[path]
	if t == nil {
		t = NewTracker(path)
		src.trackers[path] = t
	}
	src.seen[path] = now
	return t
}

// children returns the subagent threads of a thread whose turn is running. A spawn edge stays open after its
// child finished, so the child's own rollout decides.
func (src *Source) children(ctx context.Context, thread string, now time.Time) []sessions.Task {
	db := src.db.get()
	if db == nil || thread == "" {
		return nil
	}
	rows, err := db.QueryContext(ctx, `select t.id, coalesce(t.agent_nickname, ''), coalesce(t.title, ''), coalesce(t.agent_role, ''),
		t.rollout_path, coalesce(t.created_at_ms, t.created_at*1000) from thread_spawn_edges e join threads t on t.id = e.child_thread_id
		where e.parent_thread_id = ? and e.status = 'open'`, thread)
	if err != nil {
		return nil
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
	rows.Close()
	var out []sessions.Task
	for _, c := range kids {
		ct := src.tracker(c.path, now)
		ct.Update()
		if !ct.Open() {
			continue
		}
		k := sessions.Task{ID: c.id, Kind: "agent", Label: text.OneLine(text.FirstNonEmpty(c.nick, c.title), 90), Type: c.role, Started: time.UnixMilli(c.created)}
		if fi, err := os.Stat(c.path); err == nil {
			k.LastActive = fi.ModTime()
		}
		if p := ct.pending(); p != nil {
			k.Now = text.OneLine(p.name+": "+p.desc, 110)
		}
		out = append(out, k)
	}
	return out
}

// Native finds Codex sessions the way they show on the machine: the native Codex process nearest a tmux pane's
// shell, and any other Codex process outside tmux. It skips the app server (the desktop app and tool hosts) and
// a Codex that another Codex started. A process's thread is the rollout file it holds open.
type Native struct {
	Env sessions.Env
	db  *stateDB
}

// NewNative returns a Native lister over the state database under env.CodexHome.
func NewNative(env sessions.Env) *Native { return &Native{Env: env, db: &stateDB{home: env.CodexHome}} }

// List finds the live Codex sessions.
func (n *Native) List(ctx context.Context, w *sessions.World) ([]sessions.Found, error) {
	var out []sessions.Found
	for _, pa := range w.PaneAgents() {
		if pa.Kind != "codex" {
			continue
		}
		f := n.agent(ctx, w, pa.PID)
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
		f := n.agent(ctx, w, pid)
		f.Where = "tty"
		if f.Thread != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

var reRollout = regexp.MustCompile(`rollout-.*-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// agent describes the session behind a Codex process: the main thread among the rollouts it holds open.
func (n *Native) agent(ctx context.Context, w *sessions.World, pid int) sessions.Found {
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
	th := n.db.main(ctx, ids)
	if th == nil {
		return f
	}
	f.Thread, f.Cwd, f.Model = th.id, th.cwd, th.model
	f.Named = th.name != ""
	f.Name = text.FirstNonEmpty(text.OneLine(text.FirstNonEmpty(th.name, th.title), 60), f.Name)
	f.Transcript, f.Started, f.LastActive = th.rollout, th.created, th.updated
	if fi, err := os.Stat(th.rollout); err == nil && fi.ModTime().After(f.LastActive) {
		f.LastActive = fi.ModTime()
	}
	return f
}

// stateDB is Codex's state database, opened read-only once it exists: the newest state_<n>.sqlite under the
// Codex home.
type stateDB struct {
	home string
	db   *sql.DB
}

func (s *stateDB) get() *sql.DB {
	if s.db != nil {
		return s.db
	}
	files, _ := filepath.Glob(filepath.Join(s.home, "state_*.sqlite"))
	best, bestN := "", -1
	for _, f := range files {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "state_"), ".sqlite"))
		if err == nil && n > bestN {
			best, bestN = f, n
		}
	}
	if best != "" {
		s.db, _ = sql.Open("sqlite", "file:"+best+"?mode=ro&_pragma=busy_timeout(500)")
	}
	return s.db
}

func (s *stateDB) close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

type thread struct {
	id, name, title, cwd, model, rollout string
	created, updated                     time.Time
	subagent                             bool
}

// main returns the thread among ids that is not a subagent's; after /new or /resume a process can hold two, and
// the one updated last is current. A subagent thread can have no role, so the role alone does not tell; its
// source and thread_source do.
func (s *stateDB) main(ctx context.Context, ids []string) *thread {
	db := s.get()
	if db == nil || len(ids) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `select * from threads where id in (?`+strings.Repeat(",?", len(ids)-1)+`)`, anys(ids)...)
	if err != nil {
		return nil
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
	return best
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
