package codex

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
	"github.com/agent-sh/momentum/sessions/term"
)

var t0 = time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)

// rollout builds a synthetic Codex rollout, one event a second.
type rollout struct {
	t    *testing.T
	path string
	at   time.Time
}

func newRollout(t *testing.T, dir, id string) *rollout {
	t.Helper()
	path := filepath.Join(dir, "sessions", "2026", "05", "06", "rollout-2026-05-06T09-00-00-"+id+".jsonl")
	fake.File(t, path, "")
	return &rollout{t: t, path: path, at: t0}
}

func (r *rollout) ev(typ string, payload map[string]any) *rollout {
	r.t.Helper()
	r.at = r.at.Add(time.Second)
	fake.Append(r.t, r.path, map[string]any{"timestamp": r.at.Format(time.RFC3339Nano), "type": typ, "payload": payload})
	return r
}

func (r *rollout) start() *rollout { return r.ev("event_msg", map[string]any{"type": "task_started"}) }
func (r *rollout) done(last string) *rollout {
	return r.ev("event_msg", map[string]any{"type": "task_complete", "last_agent_message": last})
}
func (r *rollout) user(text string) *rollout {
	return r.ev("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}})
}
func (r *rollout) call(id, name, args string) *rollout {
	return r.ev("response_item", map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": args})
}
func (r *rollout) output(id, out string) *rollout {
	return r.ev("response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": out})
}

func facts(tr *Tracker, now time.Time) *sessions.Session {
	s := &sessions.Session{Tasks: []sessions.Task{}}
	tr.Facts(s, now)
	s.TurnOpen = tr.Open()
	sessions.Decide(s, now)
	return s
}

func TestATurnRunsFromStartedToComplete(t *testing.T) {
	r := newRollout(t, t.TempDir(), "00000000-0000-0000-0000-000000000001").start().user("fix the test").
		call("c1", "exec_command", `{"cmd":["go","test","./..."]}`)
	tr := NewTracker(r.path)
	s := facts(tr, r.at)
	if s.Phase != sessions.Working || s.Now != "exec_command: go test ./..." {
		t.Fatalf("got %s %q", s.Phase, s.Now)
	}
	r.output("c1", "ok").done("All tests pass.")
	if s := facts(tr, r.at); s.Phase != sessions.Idle || s.LastText != "All tests pass." || s.Now != "" {
		t.Fatalf("got %s %q %q", s.Phase, s.LastText, s.Now)
	}
	r.start().ev("event_msg", map[string]any{"type": "turn_aborted"})
	if s := facts(tr, r.at); s.Phase != sessions.Idle {
		t.Fatalf("an aborted turn is over: %s", s.Phase)
	}
}

func TestAnAsyncQuestionBlocksOnlyOnceTheTurnEnds(t *testing.T) {
	r := newRollout(t, t.TempDir(), "00000000-0000-0000-0000-000000000002").start().
		call("q1", "request_user_input_async", `{"questions":[{"title":"Which branch?"}]}`).output("q1", "asked")
	tr := NewTracker(r.path)
	s := facts(tr, r.at)
	if s.Phase != sessions.Working || s.Ask != "Which branch?" || !s.AskAsync {
		t.Fatalf("while the turn runs: %s %q async %v", s.Phase, s.Ask, s.AskAsync)
	}
	r.done("I asked which branch.")
	if s := facts(tr, r.at); s.Phase != sessions.NeedsYou {
		t.Fatalf("after the turn: %s", s.Phase)
	}
	r.user("main")
	if s := facts(tr, r.at); s.Ask != "" {
		t.Fatalf("a user message answers it: %q", s.Ask)
	}
}

func TestABlockingQuestionNeedsItsOwnerWhileItHoldsTheTurn(t *testing.T) {
	r := newRollout(t, t.TempDir(), "00000000-0000-0000-0000-000000000003").start().
		call("q1", "request_user_input", `{"questions":[{"question":"Delete the old data?"}]}`)
	tr := NewTracker(r.path)
	s := facts(tr, r.at)
	if s.Phase != sessions.NeedsYou || s.AskAsync || !s.TurnOpen {
		t.Fatalf("waiting for the answer: %s async %v open %v", s.Phase, s.AskAsync, s.TurnOpen)
	}
	r.output("q1", "yes")
	if s := facts(tr, r.at); s.Phase != sessions.Working || s.Ask != "" {
		t.Fatalf("answered: %s %q", s.Phase, s.Ask)
	}
}

func TestDescribeReadsCodexArguments(t *testing.T) {
	for args, want := range map[string]string{
		`{"cmd":"ls -la"}`:                                           "ls -la",
		`{"command":["bash","-lc","make"]}`:                          "bash -lc make",
		`{"duration_ms":30000}`:                                      "30s",
		`{"prompt":"gAAAAsecret","path":"/x"}`:                       "/x",
		`await tools.exec({cmd: "git status", yield_time_ms: 1000})`: "git status",
	} {
		if got := describe("x", args); got != want {
			t.Errorf("describe(%s) = %q, want %q", args, got, want)
		}
	}
}

func TestUserMessagesAreLoggedAsUserRoleText(t *testing.T) {
	r := newRollout(t, t.TempDir(), "00000000-0000-0000-0000-000000000004").start().
		user("<environment_context>cwd</environment_context>").user("# AGENTS.md instructions").user("ship it").done("Shipped.")
	tr := NewTracker(r.path)
	tr.Update()
	items, _ := tr.Log().Since(0)
	if len(items) != 1 || items[0].String() != "USER: ship it" {
		t.Fatalf("log %+v", items)
	}
}

// state writes a synthetic Codex state database.
func state(t *testing.T, home string, threads [][]any, edges [][]any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL, source TEXT NOT NULL, cwd TEXT NOT NULL, title TEXT NOT NULL, agent_nickname TEXT, agent_role TEXT,
		model TEXT, created_at_ms INTEGER, updated_at_ms INTEGER, thread_source TEXT, name TEXT);
		CREATE TABLE thread_spawn_edges (parent_thread_id TEXT NOT NULL, child_thread_id TEXT NOT NULL PRIMARY KEY, status TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	for _, r := range threads {
		if _, err := db.Exec(`INSERT INTO threads VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r...); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range edges {
		if _, err := db.Exec(`INSERT INTO thread_spawn_edges VALUES (?,?,?)`, e...); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	mainID  = "01a2ffff-0000-7000-8000-000000000001"
	childID = "01a10000-0000-7000-8000-000000000002" // sorts before the main thread, and has no role
	doneID  = "01a10000-0000-7000-8000-000000000003"
)

func TestDiscoveryPicksTheMainThreadAndItsRunningSubagents(t *testing.T) {
	home := t.TempDir()
	main := newRollout(t, home, mainID).start().user("refactor").call("c1", "spawn_agent", `{"task_name":"review"}`)
	child := newRollout(t, home, childID).start().call("k1", "exec_command", `{"cmd":"rg TODO"}`)
	finished := newRollout(t, home, doneID).start().done("done")
	ms := t0.UnixMilli()
	state(t, home, [][]any{
		{mainID, main.path, ms / 1000, ms / 1000, "cli", "/w/app", "refactor the store", nil, nil, "gpt-test", ms, ms + 5000, "user", nil},
		{childID, child.path, ms / 1000, ms / 1000, `{"subagent":{"thread_spawn":{"parent_thread_id":"` + mainID + `"}}}`, "/w/app", "", "Kant", nil, "gpt-test", ms, ms + 9000, "subagent", nil},
		{doneID, finished.path, ms / 1000, ms / 1000, `{"subagent":{}}`, "/w/app", "", "Hume", "explorer", "gpt-test", ms, ms, "subagent", nil},
	}, [][]any{{mainID, childID, "open"}, {mainID, doneID, "open"}})
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 10, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 11, PPID: 10, Argv: []string{"/opt/codex/codex", "--profile", "p"}, Start: 1100, Files: []string{child.path, main.path, finished.path}},
		fake.Proc{PID: 12, PPID: 11, Argv: []string{"/usr/bin/systemd-inhibit", "--what=idle"}},
		fake.Proc{PID: 13, PPID: 11, Argv: []string{"/opt/codex/codex", "exec"}}, // started by Codex
		fake.Proc{PID: 20, PPID: 1, Argv: []string{"/opt/codex/codex", "app-server"}, Files: []string{main.path}},
		fake.Proc{PID: 30, PPID: 1, Argv: []string{"/opt/codex/codex"}, Start: 3000}, // outside tmux, no thread yet
		fake.Proc{PID: 40, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 41, PPID: 40, Argv: []string{"/opt/codex/codex"}, Start: 4100}, // in a pane, no thread yet
	))
	if err != nil {
		t.Fatal(err)
	}
	env := sessions.Env{CodexHome: home}
	w := &sessions.World{Now: main.at, Procs: tb, Panes: []term.Pane{
		{ID: "%7", Session: "dev", Window: "2", Index: "0", WindowName: "codex-win", PID: 10, Path: "/w/app"},
		{ID: "%8", Session: "dev", Window: "3", Index: "0", WindowName: "fresh", PID: 40, Path: "/w/new"},
	}}
	src := New(env)
	defer src.Close()
	ss, err := sessions.Scan(context.Background(), w, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 {
		t.Fatalf("sessions %+v", ss)
	}
	s := ss[0]
	if s.Key != mainID || s.Where != "dev:2.0" || s.Pane != "%7" || s.Name != "refactor the store" || s.Model != "gpt-test" || s.Phase != sessions.Working {
		t.Fatalf("main thread: %+v", s)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].ID != childID || s.Tasks[0].Label != "Kant" || s.Tasks[0].Now != "exec_command: rg TODO" {
		t.Fatalf("a finished subagent with an open edge is not running; tasks %+v", s.Tasks)
	}
	fresh := ss[1]
	if fresh.Key != "codex:proc:41:4100" || fresh.Name != "fresh" || fresh.Phase != sessions.Idle {
		t.Fatalf("a pane Codex without a thread yet: %+v", fresh)
	}
}

func TestACodexWithoutARolloutTakesItsTurnFromTheInhibitor(t *testing.T) {
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 10, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 11, PPID: 10, Argv: []string{"/opt/codex/codex"}, Start: 1100},
		fake.Proc{PID: 12, PPID: 11, Argv: []string{"systemd-inhibit"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	env := sessions.Env{CodexHome: t.TempDir()}
	w := &sessions.World{Now: t0, Procs: tb, Panes: []term.Pane{{ID: "%1", PID: 10}}}
	ss, err := sessions.Scan(context.Background(), w, New(env))
	if err != nil || len(ss) != 1 || ss[0].Phase != sessions.Working {
		t.Fatalf("got %v %+v", err, ss)
	}
}

func TestAfterNewTheThreadUpdatedLastIsCurrent(t *testing.T) {
	home := t.TempDir()
	// thread ids are time-ordered, so the thread /new made sorts after the one it replaced
	const oldID, newID = "01a20000-0000-7000-8000-00000000000a", "01a30000-0000-7000-8000-00000000000b"
	old := newRollout(t, home, oldID).start().done("old work done")
	cur := newRollout(t, home, newID).start()
	ms := t0.UnixMilli()
	state(t, home, [][]any{
		{oldID, old.path, ms / 1000, ms / 1000, "cli", "/w", "old thread", nil, nil, nil, ms, ms + 1000, "user", nil},
		{newID, cur.path, ms / 1000, ms / 1000, "cli", "/w", "new thread", nil, nil, nil, ms, ms + 9000, "user", "named"},
	}, nil)
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, PPID: 1, Argv: []string{"codex"}, Files: []string{old.path, cur.path}}))
	if err != nil {
		t.Fatal(err)
	}
	env := sessions.Env{CodexHome: home}
	w := &sessions.World{Now: cur.at, Procs: tb}
	src := New(env)
	defer src.Close()
	ss, err := sessions.Scan(context.Background(), w, src)
	if err != nil || len(ss) != 1 || ss[0].Key != newID || ss[0].Where != "tty" || !ss[0].Named || ss[0].Name != "named" || ss[0].Phase != sessions.Working {
		t.Fatalf("got %v %+v", err, ss)
	}
}

type listed []sessions.Found

func (l listed) List(context.Context, *sessions.World) ([]sessions.Found, error) { return l, nil }

func TestASourceWithAnotherListerReadsRolloutsAndSubagents(t *testing.T) {
	home := t.TempDir()
	main := newRollout(t, home, mainID).start().call("c1", "spawn_agent", `{"task_name":"x"}`)
	child := newRollout(t, home, childID).start()
	ms := t0.UnixMilli()
	state(t, home, [][]any{{childID, child.path, ms / 1000, ms / 1000, `{"subagent":{}}`, "/w", "", "Kant", nil, nil, ms, ms, "subagent", nil}},
		[][]any{{mainID, childID, "open"}})
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"codex"}, Start: 500}))
	if err != nil {
		t.Fatal(err)
	}
	// a struct literal, as a board that brings its own list builds it
	src := &Source{Env: sessions.Env{CodexHome: home}, Lister: listed{
		{Tool: "codex", Thread: mainID, PID: 5, Transcript: main.path},
		{Tool: "codex", PID: 5, Name: "no thread yet"}, // the start comes from the process table
		{Tool: "codex", Thread: "x", Host: "elsewhere"},
	}}
	ss, err := sessions.Scan(context.Background(), &sessions.World{Now: main.at, Procs: tb}, src)
	if err != nil || len(ss) != 2 {
		t.Fatalf("got %v %+v", err, ss)
	}
	if ss[0].Phase != sessions.Background && ss[0].Phase != sessions.Working || len(ss[0].Tasks) != 1 {
		t.Fatalf("main: %+v", ss[0])
	}
	if ss[1].Key != "codex:proc:5:500" {
		t.Fatalf("a row without a start is keyed by the process's real start: %q", ss[1].Key)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (&Source{}).Close(); err != nil {
		t.Fatal("closing a Source that never opened its database is fine")
	}
}

func TestAProcessWhoseThreadCannotBeLookedUpIsReadInPart(t *testing.T) {
	home := t.TempDir()
	main := newRollout(t, home, mainID).start()
	ms := t0.UnixMilli()
	state(t, home, [][]any{{mainID, main.path, ms / 1000, ms / 1000, "cli", "/w", "work", nil, nil, nil, ms, ms, "user", nil}}, nil)
	fake.File(t, filepath.Join(home, "state_6.sqlite"), "") // Codex just made a newer database, still without tables
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 10, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 11, PPID: 10, Argv: []string{"/opt/codex/codex"}, Start: 1100, Files: []string{main.path}},
	))
	if err != nil {
		t.Fatal(err)
	}
	w := &sessions.World{Now: t0, Procs: tb, Panes: []term.Pane{{ID: "%1", PID: 10}}}
	src := New(sessions.Env{CodexHome: home})
	defer src.Close()
	ss, err := sessions.Scan(context.Background(), w, src)
	if err == nil || len(ss) != 1 || ss[0].Key != "codex:proc:11:1100" || !ss[0].Partial {
		t.Fatalf("got %v %+v", err, ss)
	}
}
