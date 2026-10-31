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
	waiting := func(f func(*Stop)) Stop {
		return stopped(func(s *Stop) {
			s.WakeDue, s.WakeFor, s.Words, s.Started = due, "check the soak run", Words("The soak runs. Next check at 10:20."), t0.Add(-time.Hour)
			f(s)
		})
	}
	check(t, overdue, []ruleCase{
		{"due after the turn ended", waiting(func(*Stop) {}), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, `"check the soak run"`},
		{"due while a turn ran: it counts from the turn's end", waiting(func(s *Stop) { s.At = due.Add(5 * time.Minute) }), Tries{}, nil, Wait, due.Add(15 * time.Minute), 1, ""},
		{"the machine woke after it", waiting(func(s *Stop) { s.Woke = due.Add(time.Hour) }), Tries{}, nil, Wait, due.Add(70 * time.Minute), 1, ""},
		{"the second try", waiting(func(*Stop) {}), Tries{RuleOverdue: 1}, nil, Wait, due.Add(10 * time.Minute), 2, ""},
		{"tries used up: the last message's turn waits as long", waiting(func(s *Stop) { s.At = due.Add(30 * time.Minute) }), Tries{RuleOverdue: 2}, nil, Wait, due.Add(40 * time.Minute), 2, ""},
		{"nothing said what it was for", waiting(func(s *Stop) { s.WakeFor = "" }), Tries{}, nil, Wait, due.Add(10 * time.Minute), 1, "Do what it was for now"},
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
	used, ended := Tries{RuleOverdue: 2}, waiting(func(s *Stop) { s.At = due.Add(30 * time.Minute) })
	if d := Decide(ended, used, DefaultParams, nil, due.Add(31*time.Minute)); d.Verdict != Wait {
		t.Fatalf("as the turn ends: %+v", d)
	}
	if d := Decide(ended, used, DefaultParams, nil, due.Add(40*time.Minute)); d.Verdict != Case || d.Why != "2 messages did not move it" {
		t.Fatalf("at its time: %+v", d)
	}
}

// TestAWakeupThatNeverFiresIsResynced runs a synthetic transcript through the real tracker: the turn sets a wakeup
// and ends; its owner prompts before the wakeup is due (a prompt does not take a wakeup over); the wakeup never
// fires.
func TestAWakeupThatNeverFiresIsResynced(t *testing.T) {
	dir := t.TempDir()
	x := &transcript{t: t, path: filepath.Join(dir, "s1.jsonl"), at: t0}
	x.user("watch the soak and check it every half hour").
		add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1",
			"name": "ScheduleWakeup", "input": map[string]any{"delaySeconds": 1800, "prompt": "<<autonomous-loop-dynamic>>", "reason": "check the soak run"}}}}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
			"content": "Next wakeup scheduled for 10:30:03 (in 1800s). If you owe the user a status update, write it now."}}}}).
		say("The soak runs. Next check at 10:30.").
		add(map[string]any{"type": "system", "subtype": "turn_duration"}).
		user("and keep the logs").say("Kept.").add(map[string]any{"type": "system", "subtype": "turn_duration"})
	ended := x.at
	due := t0.Add(3*time.Second + 1800*time.Second)
	tr := claude.NewTracker(x.path)
	scan := func(now time.Time) *sessions.Session {
		s := &sessions.Session{Key: "sess-soak", Tool: "claude", Name: "soak", Where: "work:3.1", Pane: "%6", PID: 42,
			Transcript: x.path, Started: t0.Add(-time.Hour), Tasks: []sessions.Task{}}
		tr.Facts(s, now)
		s.TurnOpen = tr.TurnOpen()
		sessions.Decide(s, now)
		s.Since = ended
		return s
	}
	st := testStore(t)
	f := &fakeSender{}
	now := due.Add(9 * time.Minute)
	sv := supervisor(st, f, &now)
	sv.Live = map[string]bool{"overdue": true}
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	run(t, sv, scan(now))
	if len(f.sent) != 0 {
		t.Fatalf("within its slack: %q", f.sent)
	}
	now = due.Add(10 * time.Minute)
	run(t, sv, scan(now))
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], `nothing woke you for "check the soak run"`) {
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
	// The wakeup never fires; its owner prompted before it was due, and comes back long after.
	lost := wake(newTimeline(t, "lost").owner(0, "watch the soak", "turn_duration"), "tw1").say(time.Second, "Next check at 10:30.").
		owner(10*time.Minute, "and keep the logs", "turn_duration").say(time.Second, "Kept.").
		owner(2*time.Hour, "where is the soak at?", "turn_duration")
	lost.put(st)
	// The wakeup fires on time.
	kept := wake(newTimeline(t, "kept").owner(0, "watch the soak", "turn_duration"), "tw1").say(time.Second, "Next check at 10:30.")
	fire(kept, 30*time.Minute).say(time.Second, "Checked.")
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
