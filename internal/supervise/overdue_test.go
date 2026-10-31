package supervise

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
)

func TestOverdueResyncsAWakeupThatNeverCame(t *testing.T) {
	due := t0.Add(20 * time.Minute)
	lost := "a task's notification came in while it waited"
	waiting := func(f func(*Stop)) Stop {
		return stopped(func(s *Stop) {
			s.WakeDue, s.WakeFor, s.WakeKey, s.WakeSign = due, "check the soak run", "wakeup@1", lost
			s.Words, s.Started, s.IdleAt = Words("The soak runs. Next check at 10:20."), t0.Add(-time.Hour), t0
			f(s)
		})
	}
	job := func(f func(*Stop)) Stop {
		return waiting(func(s *Stop) {
			s.WakeJob, s.WakeSign, s.WakeKey, s.WakeFor = true, "", "job:j1@1", "run the nightly report"
			f(s)
		})
	}
	check(t, overdue, []ruleCase{
		{"due after the turn ended", waiting(func(*Stop) {}), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, `"check the soak run"`},
		{"its date in the message", waiting(func(*Stop) {}), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, "due by " + due.Local().Format("Jan 2 15:04")},
		{"due while a turn ran: it counts from the turn's end", waiting(func(s *Stop) { s.IdleAt = due.Add(5 * time.Minute) }), Tries{}, nil, Wait, due.Add(15 * time.Minute), 1, ""},
		{"not from when the phase changed", waiting(func(s *Stop) { s.At = due.Add(2 * time.Minute) }), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, ""},
		{"due before the last turn began: its time came then", waiting(func(s *Stop) { s.TurnBegan, s.IdleAt = due.Add(20*time.Minute), due.Add(30*time.Minute) }), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, ""},
		{"the machine slept through it", waiting(func(s *Stop) { s.Slept, s.Woke = due.Add(-time.Minute), due.Add(time.Hour) }), Tries{}, nil, Wait, due.Add(70 * time.Minute), 1, ""},
		{"a suspend after its time does not start it again", waiting(func(s *Stop) { s.Slept, s.Woke = due.Add(time.Hour), due.Add(2*time.Hour) }), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, ""},
		{"the second try", waiting(func(*Stop) {}), Tries{RuleOverdue: 1}, nil, Wait, due.Add(10 * time.Minute), 2, ""},
		{"tries used up: the last message's turn waits as long", waiting(func(s *Stop) { s.IdleAt = due.Add(30 * time.Minute) }), Tries{RuleOverdue: 2}, nil, Wait, due.Add(40 * time.Minute), 2, ""},
		{"nothing said what it was for", waiting(func(s *Stop) { s.WakeFor = "" }), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, "Do what it was for now, and set the next one"},
		{"a resync for it went in already", waiting(func(s *Stop) { s.WakeHandled = true }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"no sign it was lost: an Esc may have stopped it", waiting(func(s *Stop) { s.WakeSign = "" }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a suspend through its time is a sign", waiting(func(s *Stop) { s.WakeSign, s.Slept, s.Woke = "", due.Add(-time.Minute), due.Add(time.Minute) }), Tries{}, nil, Wait, due.Add(11 * time.Minute), 1, ""},
		{"a job's run needs no sign", job(func(*Stop) {}), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, `nothing ran "run the nightly report". Do it now; the job keeps its schedule.`},
		{"a job's run with nothing said", job(func(s *Stop) { s.WakeFor = "" }), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, "did not start. Do what it was for now; the job keeps its schedule."},
		{"a turn runs", waiting(func(s *Stop) { s.Working, s.Phase = true, sessions.Working }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"it stopped on an error", waiting(func(s *Stop) { s.Error = err503 }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a task runs: its notification wakes it", waiting(func(s *Stop) { s.Tasks = 1 }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"its last words ask the owner", waiting(func(s *Stop) { s.Words = Words("Merge it at 10:20?") }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a stop of an earlier process", waiting(func(s *Stop) { s.Started = t0.Add(time.Minute) }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"nothing pending", waiting(func(s *Stop) { s.WakeDue = time.Time{} }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a harness whose runs it does not read", waiting(func(s *Stop) { s.Tool = "codex" }), Tries{}, nil, "", time.Time{}, 0, ""},
	})
	// It comes before the promise rule: a turn whose own wakeup never came is not one with nothing set to wake it.
	if d := Plan(waiting(func(*Stop) {}), Tries{}, DefaultParams, nil); d.Rule != RuleOverdue {
		t.Fatalf("planned %+v", d)
	}
	// With the tries used up, its time opens a case, not a third message: the turn the second message opened has
	// just ended, and a wakeup it set, or its owner, may still come.
	used, ended := Tries{RuleOverdue: 2}, waiting(func(s *Stop) { s.IdleAt = due.Add(30 * time.Minute) })
	if d := Decide(ended, used, DefaultParams, nil, due.Add(31*time.Minute)); d.Verdict != Wait {
		t.Fatalf("as the turn ends: %+v", d)
	}
	if d := Decide(ended, used, DefaultParams, nil, due.Add(40*time.Minute)); d.Verdict != Case || d.Why != "2 messages did not move it" {
		t.Fatalf("at its time: %+v", d)
	}
}

// soakLoop writes a synthetic self-paced loop: the turn sets a wakeup and ends; its owner prompts before the
// wakeup is due (a prompt does not take a wakeup over); then lost writes what came while it waited. The wakeup
// never fires. It returns the transcript, when the wakeup was due, and a scan of the session at a time, its phase
// stamped as the scheduled-to-idle change would stamp it, after the wakeup's grace.
func soakLoop(t *testing.T, lost func(*transcript)) (*transcript, time.Time, func(time.Time) *sessions.Session) {
	x := &transcript{t: t, path: filepath.Join(t.TempDir(), "s1.jsonl"), at: t0}
	x.user("watch the soak and check it every half hour").
		add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1",
			"name": "ScheduleWakeup", "input": map[string]any{"delaySeconds": 1800, "prompt": "<<autonomous-loop-dynamic>>", "reason": "check the soak run"}}}}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
			"content": "Next wakeup scheduled for 10:30:03 (in 1800s). If you owe the user a status update, write it now."}}}}).
		say("The soak runs. Next check at 10:30.").
		add(map[string]any{"type": "system", "subtype": "turn_duration"}).
		user("and keep the logs").say("Kept.").add(map[string]any{"type": "system", "subtype": "turn_duration"})
	lost(x)
	due := t0.Add(3*time.Second + 1800*time.Second)
	tr := claude.NewTracker(x.path)
	return x, due, func(now time.Time) *sessions.Session {
		s := &sessions.Session{Key: "sess-soak", Tool: "claude", Name: "soak", Where: "work:3.1", Pane: "%6", PID: 42,
			Transcript: x.path, Started: t0.Add(-time.Hour), Tasks: []sessions.Task{}}
		tr.Facts(s, now)
		s.TurnOpen = tr.TurnOpen()
		sessions.Decide(s, now)
		s.Since = due.Add(sessions.WakeGrace)
		return s
	}
}

// notification is a background task's notification coming in: the cause Claude Code is known to drop a wakeup for.
func notification(x *transcript) {
	x.add(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification><task-id>bt1</task-id><status>completed</status>" +
		"<summary>Background command finished</summary></task-notification>"}}).
		say("Noted.").add(map[string]any{"type": "system", "subtype": "turn_duration"})
}

func liveOverdue(st *store.Store, f Sender, now *time.Time) *Supervisor {
	sv := supervisor(st, &fakeSender{}, now)
	sv.Sender, sv.Live = f, map[string]bool{"overdue": true}
	sv.Sleep = func(context.Context) (time.Time, time.Time) { return time.Time{}, time.Time{} }
	return sv
}

func TestAWakeupThatNeverFiresIsResynced(t *testing.T) {
	_, due, scan := soakLoop(t, notification)
	st := testStore(t)
	f := &fakeSender{}
	now := due.Add(9 * time.Minute)
	sv := liveOverdue(st, f, &now)
	run(t, sv, scan(now))
	if len(f.sent) != 0 {
		t.Fatalf("within its slack: %q", f.sent)
	}
	now = due.Add(10 * time.Minute) // past its slack, counted from its time, not from the phase's change
	run(t, sv, scan(now))
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], `nothing woke you for "check the soak run"`) {
		t.Fatalf("past its slack: sent %q", f.sent)
	}
}

func TestAWakeupAnEscMayHaveStoppedIsLeftAlone(t *testing.T) {
	for name, lost := range map[string]func(*transcript){
		"nothing came while it waited": func(*transcript) {},
		"its owner pressed Esc mid-turn": func(x *transcript) {
			x.user("look at the logs too").add(map[string]any{"type": "user", "message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
			notification(x) // what comes after the Esc is no sign: the wakeup was cancelled
		},
	} {
		_, due, scan := soakLoop(t, lost)
		st := testStore(t)
		f := &fakeSender{}
		now := due.Add(time.Hour)
		sv := liveOverdue(st, f, &now)
		run(t, sv, scan(now))
		if len(f.sent) != 0 || len(events(t, st, KindAct)) != 0 {
			t.Errorf("%s: sent %q", name, f.sent)
		}
	}
}

func TestALostWakeupGetsOneResync(t *testing.T) {
	x, due, scan := soakLoop(t, notification)
	st := testStore(t)
	f := &fakeSender{}
	now := due.Add(10 * time.Minute)
	sv := liveOverdue(st, f, &now)
	run(t, sv, scan(now))
	if len(f.sent) != 1 {
		t.Fatalf("sent %q", f.sent)
	}
	// The resync's turn does the work and sets no wakeup; then its owner asks about something else, and leaves.
	x.at = now
	x.user(f.sent[0][strings.Index(f.sent[0], Marker):]).say("Checked: the soak finished.").add(map[string]any{"type": "system", "subtype": "turn_duration"})
	x.user("what else is open?").say("Nothing.").add(map[string]any{"type": "system", "subtype": "turn_duration"})
	for i := 0; i < 3; i++ {
		now = now.Add(time.Hour)
		run(t, sv, scan(now))
	}
	if len(f.sent) != 1 || len(events(t, st, KindCase)) != 0 {
		t.Fatalf("the same wakeup again: sent %q, cases %+v", f.sent, events(t, st, KindCase))
	}
}

// TestAJobsRunThatNeverStartsIsResynced runs a job CronCreate made, its result written as recent versions write
// it (the schedule in words), through the real tracker: its run never starts.
func TestAJobsRunThatNeverStartsIsResynced(t *testing.T) {
	x := &transcript{t: t, path: filepath.Join(t.TempDir(), "s1.jsonl"), at: t0}
	x.user("check the deploy every 10 minutes").
		add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tc1",
			"name": "CronCreate", "input": map[string]any{"cron": "*/10 * * * *", "prompt": "check the deploy"}}}}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tc1",
			"content": "Scheduled recurring job 7f3a9c21 (Every 10 minutes). Next run: 12:10. Session-only (not written to disk, dies when Claude exits). Auto-expires after 7 days. Use CronDelete to cancel sooner."}}}}).
		say("Checking every 10 minutes.").add(map[string]any{"type": "system", "subtype": "turn_duration"})
	tr := claude.NewTracker(x.path)
	scan := func(now time.Time) *sessions.Session {
		s := &sessions.Session{Key: "sess-deploy", Tool: "claude", Name: "deploy", Where: "work:4.1", Pane: "%7", PID: 42,
			Transcript: x.path, Started: t0.Add(-time.Hour), Tasks: []sessions.Task{}}
		tr.Facts(s, now)
		s.TurnOpen = tr.TurnOpen()
		sessions.Decide(s, now)
		s.Since = x.at
		return s
	}
	run10 := t0.Truncate(10 * time.Minute).Add(10 * time.Minute)
	latest := run10.Add(5 * time.Minute) // half the gap
	st := testStore(t)
	f := &fakeSender{}
	now := latest.Add(9 * time.Minute)
	sv := liveOverdue(st, f, &now)
	run(t, sv, scan(now))
	if len(f.sent) != 0 {
		t.Fatalf("within its slack: %q", f.sent)
	}
	now = latest.Add(10 * time.Minute)
	run(t, sv, scan(now))
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], `nothing ran "check the deploy"`) {
		t.Fatalf("past its slack: sent %q", f.sent)
	}
}

func TestReplayFollowsWakeupsAcrossPrompts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	wake := func(b *timelineBuilder, id string) *timelineBuilder {
		return b.call(time.Second, "ScheduleWakeup", id, `{"delaySeconds":1800,"noop":false,"prompt":"<<autonomous-loop-dynamic>>","reason":"check the soak run"}`).
			result(time.Second, "ScheduleWakeup", id, "Next wakeup scheduled for 10:30:03 (in 1800s). If you owe the user a status update, write it now.")
	}
	fire := func(b *timelineBuilder, after time.Duration) *timelineBuilder {
		return b.add(after, "harness_text", store.ActorHarness, "", "# Autonomous loop tick (dynamic pacing)", `{"prior":"scheduled_task_fire","why":"meta line"}`)
	}
	notified := func(b *timelineBuilder, after time.Duration) *timelineBuilder {
		return b.add(after, "harness_text", store.ActorHarness, "", "<task-notification><task-id>bt1</task-id><status>completed</status></task-notification>", `{"why":"meta line"}`)
	}
	// The wakeup never fires: its owner prompted before it was due, a task's notification came while it waited,
	// and he comes back long after.
	lost := wake(newTimeline(t, "lost").owner(0, "watch the soak", "turn_duration"), "tw1").say(time.Second, "Next check at 10:30.").
		owner(10*time.Minute, "and keep the logs", "turn_duration").say(time.Second, "Kept.")
	notified(lost, 5*time.Minute).say(time.Second, "Noted.").
		owner(35*time.Minute, "where is the soak at?", "turn_duration").say(time.Second, "It finished.").
		owner(2*time.Hour, "anything else?", "turn_duration")
	lost.put(st)
	// The same with nothing that drops a wakeup: an Esc at its idle prompt may have stopped the loop.
	quiet := wake(newTimeline(t, "quiet").owner(0, "watch the soak", "turn_duration"), "tw1").say(time.Second, "Next check at 10:30.").
		owner(10*time.Minute, "and keep the logs", "turn_duration").say(time.Second, "Kept.").
		owner(2*time.Hour, "where is the soak at?", "turn_duration")
	quiet.put(st)
	// The wakeup fires on time, though a task's notification came while it waited.
	kept := wake(newTimeline(t, "kept").owner(0, "watch the soak", "turn_duration"), "tw1").say(time.Second, "Next check at 10:30.")
	notified(kept, 5*time.Minute).say(time.Second, "Noted.")
	fire(kept, 25*time.Minute).say(time.Second, "Checked.")
	kept.put(st)
	until := t0.Add(5 * time.Hour)
	points, err := History(ctx, st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	var acted []Result
	for _, r := range Replay(points, nil, DefaultParams, probeAt(time.Time{}), until) {
		if r.Decision.Rule == RuleOverdue && r.Fired {
			acted = append(acted, r)
		}
	}
	if len(acted) != 1 || acted[0].Point.Session != "lost" || acted[0].Outcome != OwnerOther ||
		!strings.Contains(acted[0].Decision.Text, "check the soak run") {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestTheLastSuspendIsReadFromTheSleepTarget(t *testing.T) {
	slept, woke := parseSleep("ActiveEnterTimestamp=@1772618400\nInactiveEnterTimestamp=@1772622000\n")
	if !slept.Equal(time.Unix(1772618400, 0)) || !woke.Equal(time.Unix(1772622000, 0)) {
		t.Fatalf("slept %v woke %v", slept, woke)
	}
	if slept, woke := parseSleep("ActiveEnterTimestamp=\nInactiveEnterTimestamp=\n"); !slept.IsZero() || !woke.IsZero() {
		t.Fatalf("never slept: %v %v", slept, woke)
	}
}
