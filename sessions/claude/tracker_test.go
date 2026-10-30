package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
)

// t0 is when every synthetic transcript starts.
var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// script builds a synthetic Claude Code transcript, one line per call, each a second after the previous.
type script struct {
	t    *testing.T
	path string
	at   time.Time
}

func newScript(t *testing.T) *script {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proj", "s1.jsonl")
	fake.File(t, path, "")
	return &script{t: t, path: path, at: t0}
}

func (x *script) stamp() string {
	x.at = x.at.Add(time.Second)
	return x.at.Format(time.RFC3339Nano)
}

func (x *script) add(line map[string]any) *script {
	x.t.Helper()
	if _, ok := line["timestamp"]; !ok {
		line["timestamp"] = x.stamp()
	}
	fake.Append(x.t, x.path, line)
	return x
}

func (x *script) user(text string) *script {
	return x.add(map[string]any{"type": "user", "isSidechain": false, "origin": map[string]any{"kind": "human"}, "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": text}})
}

func (x *script) say(text string) *script {
	return x.add(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-test-1", "content": []any{
		map[string]any{"type": "text", "text": text}}}})
}

func (x *script) call(id, name string, input map[string]any) *script {
	return x.add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
}

func (x *script) result(id, content string) *script {
	return x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}}})
}

func (x *script) notify(task, status string) *script {
	return x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"},
		"message": map[string]any{"content": "<task-notification><task-id>" + task + "</task-id><status>" + status + "</status></task-notification>"}})
}

// facts reads the transcript as a fresh session of a process that started at t0.
func (x *script) facts(tr *Tracker, now time.Time) *sessions.Session {
	s := &sessions.Session{Started: t0, Tasks: []sessions.Task{}}
	tr.Facts(s, now)
	s.TurnOpen = tr.TurnOpen()
	sessions.Decide(s, now)
	return s
}

func taskIDs(s *sessions.Session) string {
	var ids []string
	for _, k := range s.Tasks {
		ids = append(ids, k.Kind+":"+k.ID)
	}
	return strings.Join(ids, ",")
}

func TestBackgroundCommandRunsUntilItsNotification(t *testing.T) {
	x := newScript(t).user("build it").
		call("tu1", "Bash", map[string]any{"command": "make", "description": "Build", "run_in_background": true}).
		result("tu1", "Command running in background with ID: b1x9. Output is being written to: /tmp/x/b1x9.output").
		say("Started the build; I will wait for it.")
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if s.Phase != sessions.Background || taskIDs(s) != "bash:b1x9" || s.Tasks[0].Label != "Build" {
		t.Fatalf("after launch: phase %s tasks %s", s.Phase, taskIDs(s))
	}
	x.notify("b1x9", "completed").say("The build passed.")
	s = x.facts(tr, x.at)
	if s.Phase != sessions.Idle || len(s.Tasks) != 0 || s.LastText != "The build passed." {
		t.Fatalf("after its notification: phase %s tasks %s last %q", s.Phase, taskIDs(s), s.LastText)
	}
}

func TestAQueuedNotificationClosesItsTask(t *testing.T) {
	x := newScript(t).
		call("tu1", "Bash", map[string]any{"command": "sleep 99"}).
		result("tu1", "Command running in background with ID: q7").
		say("waiting").
		add(map[string]any{"type": "queue-operation", "operation": "enqueue",
			"content": "<task-notification><task-id>q7</task-id><status>failed</status></task-notification>"})
	if s := x.facts(NewTracker(x.path), x.at); len(s.Tasks) != 0 {
		t.Fatalf("tasks %s", taskIDs(s))
	}
}

func TestAsyncSubagentsCloseAndAMessageWakesThemAgain(t *testing.T) {
	x := newScript(t).
		call("tu1", "Agent", map[string]any{"description": "Review the diff", "subagent_type": "reviewer", "prompt": "..."}).
		result("tu1", "Async agent launched successfully.\nagentId: ag42 (internal)").
		say("A reviewer is on it.")
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if taskIDs(s) != "agent:ag42" || s.Tasks[0].Label != "Review the diff" || s.Tasks[0].Type != "reviewer" {
		t.Fatalf("tasks %+v", s.Tasks)
	}
	x.notify("ag42", "completed").say("Review is in.")
	if s := x.facts(tr, x.at); len(s.Tasks) != 0 {
		t.Fatalf("closed: %s", taskIDs(s))
	}
	x.call("tu2", "SendMessage", map[string]any{"to": "ag42", "message": "one more"}).result("tu2", "sent").say("Asked it again.")
	if s := x.facts(tr, x.at); taskIDs(s) != "agent:ag42" {
		t.Fatalf("a message to a finished subagent wakes it: %s", taskIDs(s))
	}
}

func TestWorkflowsAndMonitors(t *testing.T) {
	x := newScript(t).
		call("tu1", "Workflow", map[string]any{"description": "fan out"}).
		result("tu1", "Workflow launched in background. Task ID: wf1\nSummary: Ten reviewers over the PR\nmore").
		call("tu2", "Monitor", map[string]any{"command": "tail"}).
		result("tu2", "Monitor started (task mon1, expires in 5m)").
		say("Both run.")
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if taskIDs(s) == "" || len(s.Tasks) != 2 {
		t.Fatalf("tasks %s", taskIDs(s))
	}
	for _, k := range s.Tasks {
		if k.Kind == "workflow" && k.Label != "Ten reviewers over the PR" {
			t.Fatalf("workflow label %q", k.Label)
		}
	}
	if s := x.facts(tr, x.at.Add(6*time.Minute)); taskIDs(s) != "workflow:wf1" {
		t.Fatalf("an expired monitor is over: %s", taskIDs(s))
	}
}

func TestWakeupsAndLoopsSchedule(t *testing.T) {
	x := newScript(t).
		call("tu1", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).
		result("tu1", "Next wakeup scheduled for 10:10:02 (in 600s)").
		say("Sleeping.")
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if s.Phase != sessions.Scheduled || !s.WakeAt.Equal(t0.Add(2*time.Second+600*time.Second)) {
		t.Fatalf("phase %s wake %v", s.Phase, s.WakeAt)
	}
	x.user("never mind, do this instead").say("ok")
	if s := x.facts(tr, x.at); s.Phase != sessions.Idle || !s.WakeAt.IsZero() {
		t.Fatalf("a new prompt takes over the wakeup: %s %v", s.Phase, s.WakeAt)
	}
	x.call("tu2", "CronCreate", map[string]any{"cron": "*/5 * * * *"}).
		result("tu2", "Scheduled recurring job c1 (*/5 * * * *). It runs until deleted.").say("Looping.")
	if s := x.facts(tr, x.at); s.Phase != sessions.Scheduled || s.Loop != "*/5 * * * *" {
		t.Fatalf("loop: %s %q", s.Phase, s.Loop)
	}
	if s := x.facts(tr, x.at.Add(cronExpiry+time.Hour)); s.Loop != "" {
		t.Fatal("a recurring job expires after 7 days")
	}
	y := newScript(t).
		call("tu1", "CronCreate", map[string]any{"cron": "0 9 * * *"}).
		result("tu1", "Scheduled one-shot job c2 (0 9 * * *).").say("Once.")
	ty := NewTracker(y.path)
	if s := y.facts(ty, y.at.Add(time.Hour)); s.Loop == "" {
		t.Fatal("a fresh one-shot schedules")
	}
	if s := y.facts(ty, y.at.Add(oneShotExpiry+time.Hour)); s.Loop != "" {
		t.Fatal("a one-shot has fired by the next day")
	}
	z := newScript(t).
		call("tu1", "CronCreate", map[string]any{}).result("tu1", "Scheduled recurring job c3 (hourly).").say("x").
		call("tu2", "CronDelete", map[string]any{"id": "c3"}).result("tu2", "deleted").say("Stopped the loop.")
	if s := z.facts(NewTracker(z.path), z.at); s.Loop != "" {
		t.Fatal("a deleted job is gone")
	}
}

func TestPendingQuestionsAndPlansBlock(t *testing.T) {
	x := newScript(t).user("plan it").
		call("tu1", "AskUserQuestion", map[string]any{"questions": []any{map[string]any{"question": "Which database?"}}})
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if s.Phase != sessions.NeedsYou || s.AskKind != sessions.AskQuestion || s.Ask != "Which database?" || !s.TurnOpen || !strings.HasPrefix(s.Now, "AskUserQuestion") {
		t.Fatalf("question: %+v", s)
	}
	x.result("tu1", "SQLite").call("tu2", "ExitPlanMode", map[string]any{"plan": "1. do it"})
	if s := x.facts(tr, x.at); s.AskKind != sessions.AskPlan || s.Phase != sessions.NeedsYou {
		t.Fatalf("plan: %+v", s)
	}
}

func TestAnAPIErrorStopsTheTurn(t *testing.T) {
	x := newScript(t).user("go").add(map[string]any{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "API Error: 529 overloaded"}}}})
	s := x.facts(NewTracker(x.path), x.at)
	if s.Phase != sessions.NeedsYou || s.AskKind != sessions.AskError || !strings.Contains(s.Ask, "529") || s.Model != "" {
		t.Fatalf("got %+v", s)
	}
}

func TestAnInterruptEndsTheTurn(t *testing.T) {
	x := newScript(t).user("go").call("tu1", "Bash", map[string]any{"command": "sleep 1000"}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user for tool use]"}}}})
	if s := x.facts(NewTracker(x.path), x.at); s.TurnOpen || s.Phase != sessions.Idle {
		t.Fatalf("got %s open %v", s.Phase, s.TurnOpen)
	}
}

func TestSidechainAndCompactionLinesAreNotTurns(t *testing.T) {
	x := newScript(t).user("go").say("Done.").
		add(map[string]any{"type": "user", "isSidechain": true, "message": map[string]any{"content": "a subagent's prompt"}}).
		add(map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"content": "Summary of the conversation so far"}})
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if s.TurnOpen || s.Phase != sessions.Idle {
		t.Fatalf("got %s open %v", s.Phase, s.TurnOpen)
	}
	items, _ := tr.Log().Since(0)
	if last := items[len(items)-1]; last.Role != sessions.RoleCompacted {
		t.Fatalf("the compaction is context in the log: %+v", last)
	}
}

func TestTasksOfAnEarlierProcessDiedWithIt(t *testing.T) {
	x := newScript(t).call("tu1", "Bash", map[string]any{"command": "serve"}).
		result("tu1", "Command running in background with ID: old1").say("Serving.")
	tr := NewTracker(x.path)
	s := &sessions.Session{Started: x.at.Add(time.Hour)} // the session was resumed by a new process later
	tr.Facts(s, x.at.Add(time.Hour))
	if len(s.Tasks) != 0 {
		t.Fatalf("tasks %s", taskIDs(s))
	}
	bg := &sessions.Session{} // a background job's tasks outlive its process
	NewTracker(x.path).Facts(bg, x.at.Add(time.Hour))
	if taskIDs(bg) != "bash:old1" {
		t.Fatalf("background job tasks %s", taskIDs(bg))
	}
}

func TestAForegroundSubagentShowsWhatItDoes(t *testing.T) {
	x := newScript(t).user("check").call("tu9", "Agent", map[string]any{"description": "Find the bug", "subagent_type": "Explore"})
	dir := strings.TrimSuffix(x.path, ".jsonl")
	fake.File(t, filepath.Join(dir, "subagents", "agent-sub7.meta.json"), `{"toolUseId":"tu9"}`)
	fake.Lines(t, filepath.Join(dir, "subagents", "agent-sub7.jsonl"), map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "x", "name": "Grep", "input": map[string]any{"pattern": "TODO"}}}}})
	s := x.facts(NewTracker(x.path), x.at)
	if len(s.Tasks) != 1 || !s.Tasks[0].Sync || s.Tasks[0].Now != "Grep: TODO" || s.Phase != sessions.Working {
		t.Fatalf("got %s %+v", s.Phase, s.Tasks)
	}
}

func TestUserRoleTextIsLabelledNotAttributed(t *testing.T) {
	x := newScript(t).user("please fix the parser").
		add(map[string]any{"type": "user", "isMeta": true, "origin": map[string]any{"kind": "peer"},
			"message": map[string]any{"content": "Another Claude session sent a message: <agent-message from=\"x\">status?</agent-message>"}}).
		add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "a skill body the harness injected"}}).
		add(map[string]any{"type": "user", "promptSource": "sdk", "message": map[string]any{"content": "The app was quit; continue."}}).
		notify("zz", "completed").
		say("ok")
	tr := NewTracker(x.path)
	x.facts(tr, x.at)
	items, _ := tr.Log().Since(0)
	var lines []string
	for _, e := range items {
		lines = append(lines, e.String())
	}
	got := strings.Join(lines, "\n")
	want := "USER [human/typed]: please fix the parser\nPEER: Another Claude session sent a message: <agent-message from=\"x\">status?</agent-message>\nUSER [sdk]: The app was quit; continue.\nASSISTANT: ok"
	if got != want {
		t.Fatalf("log:\n%s\nwant:\n%s", got, want)
	}
}

func TestARewrittenTranscriptStartsOver(t *testing.T) {
	x := newScript(t).call("tu1", "Bash", map[string]any{"command": "x"}).result("tu1", "Command running in background with ID: r1").say("bg")
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); len(s.Tasks) != 1 {
		t.Fatal("setup")
	}
	if err := os.WriteFile(x.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	x.user("fresh start").say("Hello.")
	if s := x.facts(tr, x.at); len(s.Tasks) != 0 || s.LastText != "Hello." || tr.Log().Seq() != 2 {
		t.Fatalf("tasks %s last %q seq %d", taskIDs(s), s.LastText, tr.Log().Seq())
	}
}

func TestModelComesFromTheTranscript(t *testing.T) {
	x := newScript(t).user("hi").say("hello")
	if s := x.facts(NewTracker(x.path), x.at); s.Model != "claude-test-1" {
		t.Fatalf("model %q", s.Model)
	}
}
