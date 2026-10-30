package claude

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
	"github.com/agent-sh/momentum/sessions/term"
)

// machine is a synthetic machine with Claude Code sessions in tmux, outside it and in the background.
type machine struct {
	env   sessions.Env
	w     *sessions.World
	calls int // runs of `claude agents --json`
}

const claudeBin = "/home/u/.local/bin/claude"

func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{env: sessions.Env{Home: "/home/u", ClaudeDir: t.TempDir()}}
	probe := func(pid int, v map[string]any) {
		raw, _ := json.Marshal(v)
		fake.File(t, filepath.Join(m.env.ClaudeDir, "sessions", strconv.Itoa(pid)+".json"), string(raw))
	}
	cwd := "/home/u/code/app"
	fake.File(t, filepath.Join(m.env.ClaudeDir, "projects", "-home-u-code-app", "s1.jsonl"), "")
	probe(101, map[string]any{"pid": 101, "sessionId": "s1", "cwd": cwd, "kind": "interactive", "name": "fix-parser", "nameSource": "user",
		"status": "busy", "procStart": "1010", "startedAt": 1_700_000_000_000, "statusUpdatedAt": 1_700_000_100_000})
	probe(401, map[string]any{"pid": 401, "sessionId": "s4", "kind": "interactive", "parkedJobId": "job1", "status": "busy"})
	probe(500, map[string]any{"pid": 500, "sessionId": "s5", "cwd": "/home/u", "kind": "interactive", "name": "home-3f", "nameSource": "derived", "status": "idle"})
	probe(501, map[string]any{"pid": 501, "sessionId": "s51", "kind": "interactive", "status": "busy"})
	probe(600, map[string]any{"pid": 600, "sessionId": "stale", "kind": "interactive", "procStart": "1"})
	probe(700, map[string]any{"pid": 700, "sessionId": "s7", "kind": "bg", "status": "waiting", "waitingFor": "permission prompt", "statusUpdatedAt": 1_700_000_500_000})
	fake.File(t, filepath.Join(m.env.ClaudeDir, "jobs", "job1", "state.json"),
		`{"state":"failed","detail":"tests failed twice","children":[{"id":"12","kind":"pr"},{"id":"x","kind":"issue"}],"respawnFlags":["--model","opus-test"]}`)
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 100, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 101, PPID: 100, Argv: []string{claudeBin, "--resume", "s1"}, Start: 1010},
		fake.Proc{PID: 200, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 201, PPID: 200, Argv: []string{claudeBin, "attach", "job1"}},
		fake.Proc{PID: 300, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 301, PPID: 300, Argv: []string{claudeBin, "agents"}},
		fake.Proc{PID: 400, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 401, PPID: 400, Argv: []string{claudeBin}},
		fake.Proc{PID: 500, PPID: 1, Argv: []string{claudeBin}},              // interactive outside tmux
		fake.Proc{PID: 501, PPID: 1, Argv: []string{claudeBin, "-p", "x"}},   // a script
		fake.Proc{PID: 600, PPID: 1, Argv: []string{claudeBin}, Start: 9999}, // its probe is from an earlier process
		fake.Proc{PID: 700, PPID: 1, Argv: []string{"/home/u/.local/share/claude/versions/9.9.9", "--bg"}, Start: 7000},
		fake.Proc{PID: 800, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 801, PPID: 800, Argv: []string{claudeBin}, Start: 8010}, // no probe: an older Claude Code
	))
	if err != nil {
		t.Fatal(err)
	}
	panes := []term.Pane{
		{ID: "%1", Session: "main", Window: "0", Index: "0", WindowName: "app", PID: 100, Path: cwd},
		{ID: "%2", Session: "main", Window: "1", Index: "0", PID: 200},
		{ID: "%3", Session: "main", Window: "2", Index: "0", PID: 300},
		{ID: "%4", Session: "main", Window: "3", Index: "0", PID: 400},
		{ID: "%5", Session: "old", Window: "0", Index: "0", WindowName: "legacy", PID: 800, Path: "/srv/legacy"},
	}
	m.w = &sessions.World{Now: time.Unix(fake.Boot+3600, 0), Env: m.env, Procs: tb, Panes: panes}
	return m
}

func (m *machine) native() *Native {
	n := NewNative(m.env)
	n.Agents = func(context.Context) ([]byte, error) {
		m.calls++
		return []byte(`[
		 {"id":"job1","pid":700,"cwd":"/home/u/code/app","kind":"background","sessionId":"s7","name":"nightly checks","state":"failed","status":"waiting","waitingFor":"permission prompt","startedAt":1700000000000},
		 {"id":"job2","pid":701,"cwd":"/x","kind":"background","sessionId":"s8","state":"blocked"},
		 {"pid":500,"cwd":"/home/u","kind":"interactive","sessionId":"s5","status":"idle"}]`), nil
	}
	return n
}

func byKey(fs []sessions.Found) map[string]sessions.Found {
	out := map[string]sessions.Found{}
	for _, f := range fs {
		out[f.Key()] = f
	}
	return out
}

func TestNativeDiscoveryFindsWhatClaudeCodeRecords(t *testing.T) {
	m := newMachine(t)
	found, err := m.native().List(context.Background(), m.w)
	if err != nil {
		t.Fatal(err)
	}
	got := byKey(found)
	if len(got) != 4 {
		t.Fatalf("found %d: %+v", len(got), found)
	}
	s1 := got["s1"]
	if s1.Where != "main:0.0" || s1.Pane != "%1" || s1.Name != "fix-parser" || !s1.Named || s1.Status != "busy" ||
		s1.Transcript != filepath.Join(m.env.ClaudeDir, "projects", "-home-u-code-app", "s1.jsonl") || s1.ProcStart != 1010 {
		t.Fatalf("pane session %+v", s1)
	}
	if s5 := got["s5"]; s5.Where != "tty" || s5.Named || s5.Pane != "" {
		t.Fatalf("a derived name is not a name of its own; tty session %+v", s5)
	}
	job := got["job1"]
	if job.Where != "bg" || job.Pane != "%2" || job.JobState != "failed" || job.Detail != "tests failed twice" || job.Model != "opus-test" ||
		len(job.PRs) != 1 || job.PRs[0] != "#12" || job.Status != "waiting" || job.WaitingFor != "permission prompt" || !job.Named {
		t.Fatalf("background job %+v", job)
	}
	legacy := got["claude:proc:801:8010"]
	if legacy.Where != "old:0.0" || legacy.Name != "legacy" || legacy.Cwd != "/srv/legacy" {
		t.Fatalf("a pane session without a probe is keyed by its process: %+v", byKey(found))
	}
	for _, k := range []string{"s4", "s51", "stale", "job2", "%3", "%4"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not be listed", k)
		}
	}
}

func TestNativeReusesTheBackgroundListWhileProcessesStay(t *testing.T) {
	m := newMachine(t)
	n := m.native()
	ctx := context.Background()
	n.List(ctx, m.w)
	m.w.Now = m.w.Now.Add(time.Minute)
	n.List(ctx, m.w)
	if m.calls != 1 {
		t.Fatalf("claude agents ran %d times for unchanged processes", m.calls)
	}
	m.w.Now = m.w.Now.Add(2 * time.Minute)
	n.List(ctx, m.w)
	if m.calls != 2 {
		t.Fatalf("a list older than Every is read again; ran %d times", m.calls)
	}
	m.w.Procs.ByPID[900] = &proc.Proc{PID: 900, Argv: []string{claudeBin}}
	n.List(ctx, m.w)
	if m.calls != 3 {
		t.Fatalf("a new Claude Code process reads the list again; ran %d times", m.calls)
	}
}

func TestNativeKeepsTheLastBackgroundListWhenTheCommandFails(t *testing.T) {
	m := newMachine(t)
	n := m.native()
	n.List(context.Background(), m.w)
	n.Agents = func(context.Context) ([]byte, error) { return nil, errors.New("timed out") }
	n.Every = 0
	found, err := n.List(context.Background(), m.w)
	if err == nil {
		t.Fatal("the failure is reported")
	}
	if _, ok := byKey(found)["job1"]; !ok {
		t.Fatal("the last list stays on the board")
	}
}

func TestTranscriptPathFallsBackToAnyProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	fake.File(t, filepath.Join(dir, "projects", "-elsewhere", "s9.jsonl"), "")
	if got := TranscriptPath(dir, "s9", "/home/u/x"); got != filepath.Join(dir, "projects", "-elsewhere", "s9.jsonl") {
		t.Fatalf("got %q", got)
	}
	if TranscriptPath(dir, "none", "/x") != "" || TranscriptPath(dir, "", "/x") != "" {
		t.Fatal("no transcript")
	}
}
