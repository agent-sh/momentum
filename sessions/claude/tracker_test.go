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

// version is the Claude Code version the synthetic transcripts are written by: one that records scheduled runs.
const version = "2.1.316"

func (x *script) add(line map[string]any) *script {
	x.t.Helper()
	if _, ok := line["timestamp"]; !ok {
		line["timestamp"] = x.stamp()
	}
	if _, ok := line["version"]; !ok {
		line["version"] = version
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

// A task's notification closes its task by any record of it: the attachment that delivers a queued one closes it with
// no enqueue before it, whatever origin the attachment names but another session's, and blank space before its tag
// changes nothing, in a user record, in the attachment or in the enqueue. A record that only quotes a notification
// for the task closes nothing: a loop's prompt, queued or as its meta line, a headless run's user text, a prompt
// typed while the turn ran, or a snapshot of the prompt. A prompt that quotes it is read as any other: a headless
// run's opens a turn as user text (UserAt, a user entry in the log), and a loop's meta line opens none.
func TestOnlyANotificationClosesItsTask(t *testing.T) {
	failed := tag("task-notification") + tag("task-id") + "q7" + closing("task-id") + tag("status") + "failed" + closing("status") +
		closing("task-notification")
	quoted := "# Autonomous loop tick\nRead each " + failed + " as it comes."
	asked := "Why did this fail? " + failed
	queued := func(mode, origin, prompt string) map[string]any {
		return map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "commandMode": mode,
			"origin": map[string]any{"kind": origin}, "prompt": prompt}}
	}
	// userText is the headless prompt read as user text: it opened the turn at its own record, and the log holds it.
	userText := func(t *testing.T, s *sessions.Session, at, _ time.Time) {
		items, _ := s.Log.Since(0)
		last := sessions.Entry{}
		if len(items) > 0 {
			last = items[len(items)-1]
		}
		if !s.UserAt.Equal(at) || !s.TurnOpen || last.Role != sessions.RoleUser || last.Text != asked {
			t.Errorf("the headless prompt: UserAt %v (want %v), turn open %v, last log entry %s %q", s.UserAt, at, s.TurnOpen, last.Role, last.Text)
		}
	}
	// noTurn is the loop's meta line read as any other: it opened no turn, and the turn's start stayed.
	noTurn := func(t *testing.T, s *sessions.Session, _, began time.Time) {
		if s.TurnOpen || !s.TurnBegan.Equal(began) {
			t.Errorf("the loop's meta line: turn open %v, began %v (want %v)", s.TurnOpen, s.TurnBegan, began)
		}
	}
	for name, c := range map[string]struct {
		line  map[string]any
		tasks string
		check func(t *testing.T, s *sessions.Session, at, began time.Time)
	}{
		"a queued command's notification":                    {queued("task-notification", "task-notification", failed), "", nil},
		"a queued command's notification after a line break": {queued("task-notification", "task-notification", "\n"+failed), "", nil},
		"a queued notification's enqueue after a line break": {map[string]any{"type": "queue-operation", "operation": "enqueue",
			"content": "\n" + failed}, "", nil},
		"a notification after blank space": {map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"},
			"message": map[string]any{"content": " \n\t" + failed}}, "", nil},
		"a loop's prompt, queued": {map[string]any{"type": "queue-operation", "operation": "enqueue", "content": quoted}, "bash:q7", nil},
		"a loop's prompt as its meta line": {map[string]any{"type": "user", "isMeta": true, "promptSource": "system",
			"message": map[string]any{"content": quoted}}, "bash:q7", noTurn},
		"a headless run's prompt": {map[string]any{"type": "user", "promptSource": "sdk", "entrypoint": "sdk-cli",
			"message": map[string]any{"role": "user", "content": asked}}, "bash:q7", userText},
		"a prompt typed meanwhile": {queued("prompt", "human", asked), "bash:q7", nil},
		"a prompt snapshot": {map[string]any{"type": "attachment", "attachment": map[string]any{"type": "prompt_snapshot",
			"tools": []any{map[string]any{"name": "Monitor", "description": "Each event arrives as " + failed + "."}}}}, "bash:q7", nil},
	} {
		x := newScript(t).
			call("tu1", "Bash", map[string]any{"command": "sleep 99", "run_in_background": true}).
			result("tu1", "Command running in background with ID: q7").
			say("waiting")
		tr := NewTracker(x.path)
		began := x.facts(tr, x.at).TurnBegan
		x.add(c.line)
		s := x.facts(tr, x.at)
		if taskIDs(s) != c.tasks {
			t.Errorf("%s: tasks %q, want %q", name, taskIDs(s), c.tasks)
		}
		if c.check != nil {
			c.check(t, s, x.at, began)
		}
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
	if s := x.facts(tr, x.at); s.Phase != sessions.Scheduled || !s.WakeAt.Equal(t0.Add(602*time.Second)) {
		t.Fatalf("a wakeup still fires after a prompt: %s %v", s.Phase, s.WakeAt)
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

// A task's notification Claude Code delivers after the turn ended on an API error that stops access (by its kind: a
// usage limit, a failed login, an organization's block) opens no turn until it sends the model its request: under a
// limit it knows of it sends none, and the session still stops on the error. Once the request goes out, the turn is
// open as a prompt's turn, and a retry's error record that answers the request leaves it open. A transient error, a
// limit past the reset it states, and a reopening's notice open the turn at the notification, as before.
func TestANotificationAfterAnAPIErrorOpensNoTurnUntilItsRequest(t *testing.T) {
	const limit = "You've hit your weekly limit · resets Mar 7, 9am (Europe/Lisbon)"
	stop := func(kind, text string) *script {
		return newScript(t).user("run the soak").request().add(map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": kind,
			"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": text}}}}).
			add(map[string]any{"type": "system", "subtype": "turn_duration"})
	}
	retry := map[string]any{"type": "system", "subtype": "api_error", "level": "error", "retryInMs": 2000, "retryAttempt": 1}
	x := stop("rate_limit", limit).notify("b1", "completed")
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); s.TurnOpen || s.Phase != sessions.NeedsYou || s.AskKind != sessions.AskError || !strings.Contains(s.Ask, "weekly limit") {
		t.Fatalf("no request: open %v, phase %s, ask %s %q", s.TurnOpen, s.Phase, s.AskKind, s.Ask)
	}
	x.request()
	if s := x.facts(tr, x.at); !s.TurnOpen || s.Phase != sessions.Working || s.AskKind == sessions.AskError {
		t.Fatalf("its request: open %v, phase %s, ask %s", s.TurnOpen, s.Phase, s.AskKind)
	}
	x.add(retry)
	if s := x.facts(tr, x.at); !s.TurnOpen || s.Phase != sessions.Working || s.AskKind == sessions.AskError {
		t.Errorf("a retry's error after its request: open %v, phase %s, ask %s", s.TurnOpen, s.Phase, s.AskKind)
	}
	if s := stop("authentication_failed", "Login expired · Please run /login").notify("b1", "completed"); s.facts(NewTracker(s.path), s.at).AskKind != sessions.AskError {
		t.Error("a failed login: the notification opened a turn")
	}
	orphan := tag("task-notification") + tag("task-id") + "b2" + closing("task-id") + tag("status") + "stopped" + closing("status") +
		tag("summary") + "Background command didn't finish before the previous session ended" + closing("summary") + closing("task-notification")
	notice := func(text, at string) map[string]any {
		return map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"content": text}, "timestamp": at}
	}
	note := tag("task-notification") + tag("task-id") + "b1" + closing("task-id") + tag("status") + "completed" + closing("status") + closing("task-notification")
	for name, y := range map[string]*script{
		"a transient error":                stop("server_error", "API Error: 529 overloaded").notify("b1", "completed"),
		"a limit past the reset it states": stop("rate_limit", limit).add(notice(note, "2026-03-07T10:00:00Z")),
		"a reopening's notice":             stop("rate_limit", limit).add(notice(orphan, t0.Add(time.Hour).Format(time.RFC3339Nano))),
	} {
		if s := y.facts(NewTracker(y.path), y.at); !s.TurnOpen || s.Phase != sessions.Working {
			t.Errorf("%s: open %v, phase %s", name, s.TurnOpen, s.Phase)
		}
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

func (x *script) synthetic(text string) *script {
	return x.add(map[string]any{"type": "assistant", "message": map[string]any{"model": "<synthetic>", "content": []any{
		map[string]any{"type": "text", "text": text}}}})
}

func (x *script) failed(id, content string) *script {
	return x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": true}}}})
}

func TestUserAtIsWhenUserTextLastOpenedATurn(t *testing.T) {
	x := newScript(t).user("run it")
	first := x.at
	x.call("tu1", "Bash", map[string]any{"command": "make"}).result("tu1", "ok").
		notify("b9", "completed").say("Built.")
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); !s.UserAt.Equal(first) {
		t.Fatalf("a notification is not user text: %v, want %v", s.UserAt, first)
	}
	x.user("and now test it")
	if s := x.facts(tr, x.at); !s.UserAt.Equal(x.at) {
		t.Fatalf("user at %v, want %v", s.UserAt, x.at)
	}
}

func TestAReopenedSessionSaysWhenItCameBack(t *testing.T) {
	// The session ended mid-call; reopening it, Claude Code records the cut-off call and closes the turn.
	x := newScript(t).user("deploy it").
		call("tu1", "Bash", map[string]any{"command": "deploy"}).
		failed("tu1", "[Tool call interrupted: the session ended before this call's result was recorded]").
		synthetic("No response requested.")
	back := x.at
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if !s.ResumedAt.Equal(back) || s.Phase != sessions.Idle {
		t.Fatalf("resumed %v phase %s, want %v idle", s.ResumedAt, s.Phase, back)
	}
	x.say("Picking the deploy up again.")
	if s := x.facts(tr, x.at); !s.ResumedAt.IsZero() {
		t.Fatalf("it acted since, so it is no longer just back: %v", s.ResumedAt)
	}

	// Background tasks the end cut off are reported in a notification as the session reopens.
	y := newScript(t).user("watch the run").
		call("tu1", "Bash", map[string]any{"command": "watch", "run_in_background": true}).
		result("tu1", "Command running in background with ID: bw1").say("Watching it.").
		add(map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "promptSource": "system",
			"message": map[string]any{"content": "<task-notification><task-id>bw1</task-id><status>stopped</status>" +
				"<summary>Background shell command didn't finish before the previous session ended</summary></task-notification>"}})
	tr = NewTracker(y.path)
	if s := y.facts(tr, y.at); !s.ResumedAt.Equal(y.at) || len(s.Tasks) != 0 {
		t.Fatalf("resumed %v tasks %s", s.ResumedAt, taskIDs(s))
	}
	y.user("resume")
	if s := y.facts(tr, y.at); !s.ResumedAt.IsZero() {
		t.Fatalf("its owner typed since: %v", s.ResumedAt)
	}
}

// /exit ends the process and the turn it ran: the "No response requested." Claude Code writes as the session opens
// again closes no turn, so it is no reopening after a mid-task end, as the replay reads it. The same record after a
// process that died with its turn open is one.
func TestAnExitEndsTheTurnItsProcessRan(t *testing.T) {
	open := func() *script {
		return newScript(t).user("run the suite").call("tu1", "Bash", map[string]any{"command": "make test"}).result("tu1", "ok")
	}
	for _, name := range []string{"/exit", "/quit"} {
		x := open()
		x.add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "<local-command-caveat>Caveat: the messages below were generated by the user while running local commands.</local-command-caveat>"}}).
			add(map[string]any{"type": "user", "message": map[string]any{"content": "<command-name>" + name + "</command-name>\n<command-message>" + name[1:] + "</command-message>\n<command-args></command-args>"}})
		exited := x.at
		tr := NewTracker(x.path)
		if s := x.facts(tr, x.at); s.TurnOpen || !s.ControlAt.Equal(exited) {
			t.Errorf("%s: turn open %v, control at %v (the exit at %v)", name, s.TurnOpen, s.ControlAt, exited)
		}
		x.synthetic("No response requested.")
		if s := x.facts(tr, x.at); !s.ResumedAt.IsZero() {
			t.Errorf("%s: a session opened again after a deliberate exit reads as reopened mid-task at %v", name, s.ResumedAt)
		}
	}
	x := open().synthetic("No response requested.")
	if s := x.facts(NewTracker(x.path), x.at); !s.ResumedAt.Equal(x.at) {
		t.Fatalf("a process that died with its turn open: resumed at %v, want %v", s.ResumedAt, x.at)
	}
}

// /exit ends the process that came back: the reopening before it is over, as the replay forgets it at the exit, so
// the stop after the exit is not one that just came back.
func TestAnExitForgetsAReopening(t *testing.T) {
	x := newScript(t).user("deploy it").call("tu1", "Bash", map[string]any{"command": "deploy"}).
		failed("tu1", "[Tool call interrupted: the session ended before this call's result was recorded]").
		synthetic("No response requested.")
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); s.ResumedAt.IsZero() {
		t.Fatal("the session did not read as reopened")
	}
	x.add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "<local-command-caveat>Caveat: the messages below were generated by the user while running local commands.</local-command-caveat>"}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": "<command-name>/exit</command-name>\n<command-message>exit</command-message>\n<command-args></command-args>"}})
	if s := x.facts(tr, x.at); !s.ResumedAt.IsZero() {
		t.Fatalf("the exit kept the reopening at %v", s.ResumedAt)
	}
}

func TestARateLimitWithNoTextIsAnErrorNotAReopening(t *testing.T) {
	// Claude Code writes a rate limit it has no text for as an API error saying "No response requested.".
	x := newScript(t).user("go").add(map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": "rate_limit",
		"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "No response requested."}}}})
	s := x.facts(NewTracker(x.path), x.at)
	if !s.ResumedAt.IsZero() || s.AskKind != sessions.AskError || s.Ask != "stopped on an error: "+RateLimit {
		t.Fatalf("resumed %v, ask %s %q", s.ResumedAt, s.AskKind, s.Ask)
	}
}

func TestALocalCommandOrAnInterruptionIsItsUserSteering(t *testing.T) {
	x := newScript(t).user("go").add(map[string]any{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "Login expired · Please run /login"}}}})
	asked := x.at
	x.add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "<local-command-caveat>Caveat: the messages below were generated by the user while running local commands.</local-command-caveat>"}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": "<command-name>/login</command-name>\n<command-message>login</command-message>\n<command-args></command-args>"}})
	login := x.at
	x.add(map[string]any{"type": "user", "message": map[string]any{"content": "<local-command-stdout>Login successful</local-command-stdout>"}})
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at)
	if !s.ControlAt.Equal(login) || s.UserAt.After(asked) || s.AskKind != sessions.AskError {
		t.Fatalf("control %v user %v (prompt before %v) ask %s", s.ControlAt, s.UserAt, asked, s.AskKind)
	}
	x.user("go on").call("tu1", "Bash", map[string]any{"command": "make test"}).user("[Request interrupted by user for tool use]")
	if s := x.facts(tr, x.at); !s.ControlAt.Equal(x.at) {
		t.Fatalf("an interruption: control %v, want %v", s.ControlAt, x.at)
	}
}

func TestAnAnsweredQuestionOrPlanIsItsUserSteering(t *testing.T) {
	x := newScript(t).user("set up the release").
		call("tu1", "AskUserQuestion", map[string]any{"questions": []any{map[string]any{"question": "Which branch?"}}}).
		result("tu1", `User has answered your questions: "Which branch?"="main".`)
	answered := x.at
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); !s.ControlAt.Equal(answered) {
		t.Fatalf("question: control %v, want %v", s.ControlAt, answered)
	}
	x.call("tu2", "ExitPlanMode", map[string]any{"plan": "Tag and push."}).result("tu2", "User has approved your plan.").
		add(map[string]any{"type": "assistant", "isApiErrorMessage": true,
			"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "API Error: 529 overloaded"}}}})
	s := x.facts(tr, x.at)
	if s.ControlAt.Before(answered) || !s.ControlAt.Before(x.at) || s.AskKind != sessions.AskError {
		t.Fatalf("plan: control %v, ask %s", s.ControlAt, s.AskKind)
	}
	// A call the tool turned down before anyone saw it is no answer.
	y := newScript(t).user("go").call("tu1", "AskUserQuestion", map[string]any{}).
		result("tu1", "<tool_use_error>InputValidationError: questions is required</tool_use_error>")
	if s := y.facts(NewTracker(y.path), y.at); !s.ControlAt.IsZero() {
		t.Fatalf("a tool error: control %v", s.ControlAt)
	}
}

func TestAnInterruptionIsNotAReopening(t *testing.T) {
	x := newScript(t).user("go").call("tu1", "Bash", map[string]any{"command": "x"}).result("tu1", "done").
		user("[Request interrupted by user]").synthetic("No response requested.")
	if s := x.facts(NewTracker(x.path), x.at); !s.ResumedAt.IsZero() {
		t.Fatalf("resumed %v", s.ResumedAt)
	}
}

// tag and closing build a tag of Claude Code's markup from its name, so no fixture holds one whole.
func tag(name string) string     { return "<" + name + ">" }
func closing(name string) string { return "</" + name + ">" }

// Another session's message is no notice of the session's tasks or of its reopening, whatever it quotes, and no
// user text of the session's: one that quotes a whole task notification for the running command leaves it running,
// and one that quotes Claude Code's words for a reopening reopens nothing. So for each way Claude Code writes such a
// message: a user record marked by its origin (meta or not), the words it opens one with (with its origin, as
// Claude Code writes one, or alone), its tags with no origin, a queued command's attachment (by the attachment's
// origin or by its tag) and the queue's enqueue. A meta one opens no turn, so a session that ends on one waits on
// nothing; one that is not meta opens one. The replay reads it so
// (supervise.TestAPeerMessageIsNoNoticeOfTheSessionsTasks).
func TestAPeerMessageIsNoNoticeOfTheSessionsTasks(t *testing.T) {
	done := tag("task-notification") + tag("task-id") + "b9" + closing("task-id") + tag("status") + "completed" + closing("status") +
		tag("summary") + "Background command finished" + closing("summary") + closing("task-notification")
	gone := tag("task-notification") + tag("task-id") + "__orphan_summary__" + closing("task-id") + tag("summary") +
		"1 background task didn't finish " + "before the previous session ended" + closing("summary") + closing("task-notification")
	peerTag := `cross-session-message from="s2" from-name="reviewer" from-mode="default"`
	userRec := func(text string, meta bool, origin string) map[string]any {
		r := map[string]any{"type": "user", "isMeta": meta, "message": map[string]any{"role": "user", "content": text}}
		if origin != "" {
			r["origin"] = map[string]any{"kind": origin}
		}
		return r
	}
	queuedRec := func(text, origin string) map[string]any {
		a := map[string]any{"type": "queued_command", "commandMode": "prompt", "isMeta": true, "prompt": text}
		if origin != "" {
			a["origin"] = map[string]any{"kind": origin}
		}
		return map[string]any{"type": "attachment", "attachment": a}
	}
	words := "Another Claude session sent a message:\n" + done
	rows := map[string]struct {
		recs []map[string]any
		open bool // the record opens a turn
	}{
		"origin":        {[]map[string]any{userRec("Mine finished: "+done, false, "peer")}, true},
		"origin-meta":   {[]map[string]any{userRec("Mine finished: "+done, true, "peer")}, false},
		"words":         {[]map[string]any{userRec(words, true, "peer")}, false},
		"words-alone":   {[]map[string]any{userRec(words, true, "")}, false},
		"agent-message": {[]map[string]any{userRec(tag(`agent-message from="worker"`)+done+closing("agent-message"), false, "")}, true},
		"cross-session": {[]map[string]any{userRec(tag(peerTag)+done+closing("cross-session-message"), false, "")}, true},
		"queued-origin": {[]map[string]any{queuedRec(tag(`team-message from="lead"`)+done+closing("team-message"), "peer")}, false},
		"queued-tag":    {[]map[string]any{queuedRec(tag(peerTag)+done+closing("cross-session-message"), "")}, false},
		"enqueue": {[]map[string]any{
			{"type": "queue-operation", "operation": "enqueue", "content": tag(peerTag) + done + closing("cross-session-message")},
			queuedRec(tag(peerTag)+done+closing("cross-session-message"), "peer")}, false},
		"reopening": {[]map[string]any{userRec("Your log says: "+gone, false, "peer")}, true},
	}
	for name, row := range rows {
		x := newScript(t).user("run the suite in the background")
		typed := x.at
		x.call("tu1", "Bash", map[string]any{"command": "make test", "run_in_background": true}).
			result("tu1", "Command running in background with ID: b9. Output is being written to: /work/b9.output").
			say("The suite runs in the background.")
		for _, r := range row.recs {
			x.add(r)
		}
		s := x.facts(NewTracker(x.path), x.at)
		if taskIDs(s) != "bash:b9" || !s.ResumedAt.IsZero() || !s.UserAt.Equal(typed) || s.TurnOpen != row.open {
			t.Errorf("%s: tasks %q, reopened at %v, user text at %v (typed at %v), turn open %v (want %v)", name, taskIDs(s), s.ResumedAt,
				s.UserAt, typed, s.TurnOpen, row.open)
		}
	}
}
