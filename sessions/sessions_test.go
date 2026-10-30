package sessions

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
	"github.com/agent-sh/momentum/sessions/term"
)

func TestKeyIsAnIDOrTheProcessNeverThePane(t *testing.T) {
	for _, tc := range []struct {
		f    Found
		want string
	}{
		{Found{Tool: "claude", Job: "j1", Session: "s1", PID: 5, Pane: "%3"}, "j1"},
		{Found{Tool: "codex", Thread: "t1", PID: 5, Pane: "%3"}, "t1"},
		{Found{Tool: "claude", Session: "s1", PID: 5, Pane: "%3"}, "s1"},
		{Found{Tool: "claude", PID: 5, ProcStart: 777, Pane: "%3", Name: "x"}, "claude:proc:5:777"},
		{Found{Tool: "claude", PID: 5, Pane: "%3"}, "claude:proc:5"},
		{Found{Tool: "claude", Pane: "%3", Name: "x"}, ""},
	} {
		if got := tc.f.Key(); got != tc.want {
			t.Errorf("%+v: key %q, want %q", tc.f, got, tc.want)
		}
	}
	// tmux numbers panes again after its server restarts: the same pane id, a new process
	before := Found{Tool: "claude", PID: 4100, ProcStart: 100, Pane: "%31"}
	after := Found{Tool: "claude", PID: 9100, ProcStart: 900, Pane: "%31"}
	if before.Key() == after.Key() {
		t.Fatal("two processes in a reused pane share a key")
	}
}

func TestEnvFromOSHonoursTheHarnessVariables(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/etc/claude-alt")
	t.Setenv("CODEX_HOME", "~/codex-alt")
	t.Setenv("XDG_DATA_HOME", "relative/data") // relative values are ignored
	t.Setenv("HERMES_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "/srv/pi")
	e := EnvFromOS()
	if e.Home != home || e.ClaudeDir != "/etc/claude-alt" || e.CodexHome != filepath.Join(home, "codex-alt") ||
		e.DataHome != filepath.Join(home, ".local", "share") || e.HermesHome != filepath.Join(home, ".hermes") ||
		e.PiDir != "/srv/pi" || e.Proc != "/proc" || len(e.Projects) != 0 {
		t.Fatalf("env %+v", e)
	}
}

func TestProjectNamesADirectory(t *testing.T) {
	e := Env{Home: "/h", Projects: []string{"/h/code", "/srv/work/"}}
	for cwd, want := range map[string]string{
		"/h/code/org/repo": "org/repo",
		"/srv/work/x":      "x",
		"/h/code":          "~/code", // the root itself is no project
		"/h":               "home",
		"/h/notes":         "~/notes",
		"/opt/x":           "/opt/x",
		"":                 "",
	} {
		if got := e.Project(cwd); got != want {
			t.Errorf("Project(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestLogKeepsASequenceAcrossTrims(t *testing.T) {
	var l Log
	for i := 0; i < LogKeep+1; i++ {
		l.Add(Entry{Role: RoleOut, Text: "x"})
	}
	if l.Seq() != LogKeep+1 {
		t.Fatalf("seq %d", l.Seq())
	}
	items, gap := l.Since(10) // already dropped
	if !gap || len(items) != LogKeep*3/4 {
		t.Fatalf("since a dropped point: %d items, gap %v", len(items), gap)
	}
	items, gap = l.Since(l.Seq() - 2)
	if gap || len(items) != 2 {
		t.Fatalf("since the tail: %d items, gap %v", len(items), gap)
	}
	if items, _ := l.Since(l.Seq()); items != nil {
		t.Fatal("nothing after the end")
	}
	var none *Log
	if none.Seq() != 0 {
		t.Fatal("a nil log is empty")
	}
}

func TestEntryLines(t *testing.T) {
	for _, tc := range []struct {
		e    Entry
		want string
	}{
		{Entry{Role: RoleUser, Text: "go on"}, "USER: go on"},
		{Entry{Role: RoleUser, Origin: "human/queued", Text: "go on"}, "USER [human/queued]: go on"},
		{Entry{Role: RoleTool, Name: "Bash", Text: "ls"}, "TOOL Bash: ls"},
		{Entry{Role: RolePeer, Text: "hi"}, "PEER: hi"},
	} {
		if got := tc.e.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestKindOf(t *testing.T) {
	for argv, want := range map[string]string{
		"/home/u/.local/bin/claude --resume":       "claude",
		"/home/u/.local/share/claude/versions/2.1": "claude",
		"/opt/codex/bin/codex --profile p":         "codex",
		"node /usr/lib/node_modules/codex/bin/x":   "",
		"bash -c codex":                            "",
		"":                                         "",
	} {
		if got := KindOf(strings.Fields(argv)); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", argv, got, want)
		}
	}
}

func world(t *testing.T, panes []term.Pane, ps ...fake.Proc) *World {
	t.Helper()
	tb, err := proc.Read(fake.ProcRoot(t, ps...))
	if err != nil {
		t.Fatal(err)
	}
	return &World{Now: time.Now(), Procs: tb, Panes: panes}
}

func TestPaneAgentsTakeTheNearestAgentUnderTheShell(t *testing.T) {
	w := world(t, []term.Pane{{ID: "%1", PID: 100}, {ID: "%2", PID: 200}, {ID: "%3", PID: 300}},
		fake.Proc{PID: 100, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 101, PPID: 100, Argv: []string{"node", "wrapper"}},
		fake.Proc{PID: 102, PPID: 101, Argv: []string{"/bin/codex"}},
		fake.Proc{PID: 103, PPID: 102, Argv: []string{"/bin/claude"}}, // started by that Codex: the pane is Codex's
		fake.Proc{PID: 200, PPID: 1, Argv: []string{"/bin/claude"}},   // the pane's own process
		fake.Proc{PID: 300, PPID: 1, Argv: []string{"bash"}},
		fake.Proc{PID: 301, PPID: 300, Argv: []string{"vim"}},
	)
	got := w.PaneAgents()
	if len(got) != 2 || got[0].Pane.ID != "%1" || got[0].Kind != "codex" || got[0].PID != 102 || got[1].Kind != "claude" || got[1].PID != 200 {
		t.Fatalf("agents %+v", got)
	}
	if !w.InPane(102) || w.InPane(103) || !w.InPane(200) {
		t.Fatal("InPane")
	}
}

type stubSource struct {
	tool string
	ss   []*Session
	err  error
}

func (s stubSource) Tool() string                                     { return s.tool }
func (s stubSource) Close() error                                     { return nil }
func (s stubSource) Scan(context.Context, *World) ([]*Session, error) { return s.ss, s.err }

func TestScanDecidesEverySessionAndNamesFailingSources(t *testing.T) {
	var l Log
	l.Add(Entry{Role: RoleUser, Text: "a"})
	w := &World{Now: time.Now()}
	ss, err := Scan(context.Background(), w,
		stubSource{tool: "one", ss: []*Session{{Key: "a", TurnOpen: true, Log: &l}}},
		stubSource{tool: "two", err: errors.New("db locked"), ss: []*Session{{Key: "b", AskKind: AskError}}},
	)
	if err == nil || !strings.Contains(err.Error(), "two: db locked") {
		t.Fatalf("err %v", err)
	}
	if len(ss) != 2 || ss[0].Phase != Working || ss[0].Seq != 1 || ss[1].Phase != NeedsYou {
		t.Fatalf("sessions %+v %+v", ss[0], ss[1])
	}
}
