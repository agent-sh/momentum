package claude

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
)

type listed []sessions.Found

func (l listed) List(context.Context, *sessions.World) ([]sessions.Found, error) { return l, nil }

// scan runs a Source over the rows a lister found and applies the phase rule.
func scan(t *testing.T, src *Source, now time.Time) map[string]*sessions.Session {
	t.Helper()
	w := &sessions.World{Now: now, Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}
	ss, err := sessions.Scan(context.Background(), w, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss { // a session is named by its source's tool, as every source of momentum names it
		if s.Tool != src.Tool() {
			t.Fatalf("session %s is named %q, not %q", s.Key, s.Tool, src.Tool())
		}
	}
	out := map[string]*sessions.Session{}
	for _, s := range ss {
		out[s.Key] = s
	}
	return out
}

func TestRowsFromAnotherMachineAreDropped(t *testing.T) {
	x := newScript(t).user("go")
	src := &Source{Lister: listed{
		{Tool: "claude", Session: "here", Transcript: x.path},
		{Tool: "claude", Session: "there", Transcript: x.path, Host: "build-box"}, // the same path, another machine's file
	}}
	got := scan(t, src, x.at)
	if _, ok := got["there"]; ok || len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestWithoutATranscriptOnlyTheHarnessStatusCounts(t *testing.T) {
	src := &Source{Lister: listed{
		{Tool: "claude", Session: "busy", Status: "busy"},
		{Tool: "claude", Session: "quiet"}, // nothing the harness said: nothing is running
		{Tool: "claude", Session: "shell", Status: "shell"},
		{Tool: "claude", Session: "asks", Status: "waiting", WaitingFor: "permission prompt"},
	}}
	got := scan(t, src, t0)
	for key, want := range map[string]sessions.Phase{"busy": sessions.Working, "quiet": sessions.Idle, "shell": sessions.Background, "asks": sessions.NeedsYou} {
		if got[key].Phase != want {
			t.Errorf("%s: %s, want %s", key, got[key].Phase, want)
		}
	}
	if got["asks"].AskKind != sessions.AskPermission {
		t.Fatalf("ask %+v", got["asks"])
	}
}

func TestTheHarnessReportsAPermissionDialog(t *testing.T) {
	x := newScript(t).user("clean up").call("tu1", "Bash", map[string]any{"command": "rm -rf build", "description": "Remove the build"})
	screens := 0
	src := &Source{Lister: listed{{Tool: "claude", Session: "s", Transcript: x.path, Status: "waiting", WaitingFor: "permission prompt"}},
		Screen: func(context.Context, *sessions.World, *sessions.Session) string { screens++; return "" }}
	s := scan(t, src, x.at.Add(time.Minute))["s"]
	if s.Phase != sessions.NeedsYou || s.AskKind != sessions.AskPermission || s.Ask != "permission prompt: Bash: Remove the build" {
		t.Fatalf("got %s %q %q", s.Phase, s.AskKind, s.Ask)
	}
	if screens != 0 {
		t.Fatal("a session that reports its status is not screen-scraped")
	}
}

func TestTheScreenIsReadOnlyWhenTheHarnessReportsNoStatus(t *testing.T) {
	x := newScript(t).user("clean up").call("tu1", "Bash", map[string]any{"command": "rm -rf build"})
	dialog := "Do you want to proceed with rm -rf build?\n❯ 1. Yes\n  2. No\n"
	screens := 0
	screen := func(context.Context, *sessions.World, *sessions.Session) string { screens++; return dialog }
	src := &Source{Lister: listed{{Tool: "claude", Session: "s", Transcript: x.path}}, Screen: screen}
	if s := scan(t, src, x.at.Add(stalledAfter/2))["s"]; s.Phase != sessions.Working || screens != 0 {
		t.Fatalf("a fresh call is not checked: %s, %d screens", s.Phase, screens)
	}
	if s := scan(t, src, x.at.Add(time.Minute))["s"]; s.Phase != sessions.NeedsYou || s.Ask != "Do you want to proceed with rm -rf build?" {
		t.Fatalf("a stalled call shows its dialog: %s %q", s.Phase, s.Ask)
	}
	busy := &Source{Lister: listed{{Tool: "claude", Session: "s", Transcript: x.path, Status: "busy"}}, Screen: screen}
	screens = 0
	if s := scan(t, busy, x.at.Add(time.Minute))["s"]; s.Phase != sessions.Working || screens != 0 {
		t.Fatalf("busy by its own status: %s, %d screens", s.Phase, screens)
	}
}

func TestAShellTheTranscriptCannotSeeKeepsItInTheBackground(t *testing.T) {
	x := newScript(t).user("watch it").say("Watching in the background.")
	src := &Source{Lister: listed{{Tool: "claude", Session: "s", Transcript: x.path, Status: "shell"}}}
	s := scan(t, src, x.at)["s"]
	if s.Phase != sessions.Background || len(s.Tasks) != 1 || s.Tasks[0].Kind != "bash" {
		t.Fatalf("got %s %+v", s.Phase, s.Tasks)
	}
}

func TestAFailedBackgroundJobNeedsItsOwner(t *testing.T) {
	x := newScript(t).user("nightly").say("Tests failed.")
	src := &Source{Lister: listed{{Tool: "claude", Job: "j1", Where: "bg", Transcript: x.path, JobState: "failed", Detail: "tests failed twice"}}}
	s := scan(t, src, x.at)["j1"]
	if s.Phase != sessions.NeedsYou || s.AskKind != sessions.AskError || s.Ask != "tests failed twice" {
		t.Fatalf("got %s %q %q", s.Phase, s.AskKind, s.Ask)
	}
}

func TestDerivedNamesAreNotNames(t *testing.T) {
	src := &Source{Lister: listed{
		{Tool: "claude", Session: "a", Name: "projects-ce", Named: true},
		{Tool: "claude", Session: "b", Name: "parser fix", Named: true},
		{Tool: "claude", Session: "b", Name: "duplicate"},
	}}
	got := scan(t, src, t0)
	if got["a"].Named || !got["b"].Named || got["b"].Name != "parser fix" || len(got) != 2 {
		t.Fatalf("got %+v %+v", got["a"], got["b"])
	}
}

func TestABackgroundJobKeepsTasksAcrossItsRespawn(t *testing.T) {
	x := newScript(t).call("tu1", "Bash", map[string]any{"command": "train"}).
		result("tu1", "Command running in background with ID: tr1").say("Training.")
	later := x.at.Add(time.Hour)
	src := &Source{Lister: listed{{Tool: "claude", Job: "j", Where: "bg", Transcript: x.path, Started: later}}}
	if s := scan(t, src, later)["j"]; s.Phase != sessions.Background || !strings.Contains(taskIDs(s), "tr1") {
		t.Fatalf("got %s %s", s.Phase, taskIDs(s))
	}
}

func TestARowWithoutAStartIsKeyedByTheProcesssRealStart(t *testing.T) {
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 7, Argv: []string{"claude"}, Start: 4321}))
	if err != nil {
		t.Fatal(err)
	}
	src := &Source{Lister: listed{{Tool: "claude", PID: 7, Pane: "%2"}}}
	ss, err := sessions.Scan(context.Background(), &sessions.World{Now: t0, Procs: tb}, src)
	if err != nil || len(ss) != 1 || ss[0].Key != "claude:proc:7:4321" {
		t.Fatalf("got %v %+v", err, ss)
	}
}
