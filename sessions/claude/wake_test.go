package claude

import (
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones the clock-change cases read, on any machine

	"github.com/agent-sh/momentum/sessions"
)

func (x *script) fire(content, task string) *script {
	return x.add(map[string]any{"type": "system", "subtype": "scheduled_task_fire", "content": content, "taskId": task, "isMeta": false})
}

func (x *script) interrupted() *script {
	return x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
}

// inUTC runs a test on a machine whose zone is UTC: schedules read in local time.
func inUTC(t *testing.T) {
	l := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = l })
}

// soak is a self-paced loop's turn: it sets a wakeup 30 minutes on, due at the time it returns.
func soak(t *testing.T) (*script, time.Time) {
	x := newScript(t).user("watch the soak").
		call("tu1", "ScheduleWakeup", map[string]any{"delaySeconds": 1800, "prompt": "<<autonomous-loop-dynamic>>", "reason": "check the soak run"}).
		result("tu1", "Next wakeup scheduled for 10:30:03 (in 1800s). If you owe the user a status update, write it now.").
		say("The soak runs. Next check at 10:30.")
	return x, t0.Add(3*time.Second + 1800*time.Second)
}

func TestAWakeupIsDueUntilItFires(t *testing.T) {
	x, due := soak(t)
	ended := x.at
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); s.Phase != sessions.Scheduled || !s.WakeDue.Equal(due) || s.WakeFor != "check the soak run" || s.WakeJob ||
		!strings.HasPrefix(s.WakeKey, "wakeup@") || s.WakeSign != "" || !s.IdleSince.Equal(ended) {
		t.Fatalf("pending: phase %s due %v for %q key %q sign %q idle %v", s.Phase, s.WakeDue, s.WakeFor, s.WakeKey, s.WakeSign, s.IdleSince)
	}
	// Past its time and the grace, it is no longer scheduled; it is still due, and has not fired.
	if s := x.facts(tr, due.Add(time.Hour)); s.Phase != sessions.Idle || !s.WakeDue.Equal(due) {
		t.Fatalf("an hour late: phase %s due %v", s.Phase, s.WakeDue)
	}
	x.at = due
	x.fire("Claude resuming /loop wakeup (Mar 4 10:30am)", "w1").
		add(map[string]any{"type": "user", "isMeta": true, "promptSource": "system", "message": map[string]any{"content": "# Autonomous loop tick"}})
	if s := x.facts(tr, x.at); !s.WakeDue.IsZero() || !s.WakeAt.IsZero() {
		t.Fatalf("after it fired: due %v wake %v", s.WakeDue, s.WakeAt)
	}
	// What ends it: a run of a loop's kind, the loop stopped or past its age, an Esc, a restart past its time.
	for name, end := range map[string]func(x *script){
		"a loop task's run": func(x *script) {
			x.add(map[string]any{"type": "system", "subtype": "scheduled_task_fire", "taskKind": "loop", "taskId": "w2"})
		},
		"the loop stopped": func(x *script) {
			x.call("tu2", "ScheduleWakeup", map[string]any{"stop": true}).result("tu2", "Loop stopped: cancelled 1 pending wakeup(s).")
		},
		"the loop at its limit": func(x *script) {
			x.call("tu2", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).result("tu2", "Wakeup not scheduled. The loop reached its maximum duration.")
		},
		"an Esc": func(x *script) { x.user("look at the logs").interrupted() },
		"a restart past its time": func(x *script) {
			x.add(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification><task-id>tu1</task-id><status>stopped</status>" +
				"<summary>This session restarted 5 minutes after its next /loop wakeup was due, so that wakeup will not fire.</summary></task-notification>"}})
		},
	} {
		y, _ := soak(t)
		end(y)
		if s := y.facts(NewTracker(y.path), y.at); !s.WakeDue.IsZero() || !s.WakeAt.IsZero() {
			t.Errorf("%s: due %v wake %v", name, s.WakeDue, s.WakeAt)
		}
	}
	// A run of a task it does not know is not the wakeup's.
	w, due := soak(t)
	w.at = due
	w.fire("Running scheduled task (Mar 4 10:30am)", "r9")
	if s := w.facts(NewTracker(w.path), w.at); !s.WakeDue.Equal(due) {
		t.Fatalf("another task's run: due %v", s.WakeDue)
	}
	// A restart keeps a wakeup due after it, which Claude Code arms again, and loses one due before it.
	z, due := soak(t)
	for _, c := range []struct {
		started time.Time
		kept    bool
	}{{due.Add(-time.Minute), true}, {due.Add(time.Minute), false}} {
		s := &sessions.Session{Started: c.started, Tasks: []sessions.Task{}}
		NewTracker(z.path).Facts(s, due.Add(2*time.Minute))
		if s.WakeDue.IsZero() == c.kept {
			t.Errorf("started %v: due %v", c.started, s.WakeDue)
		}
	}
}

func TestTheLastTurnBeginsAtAnInputAndEndsAtItsLastReply(t *testing.T) {
	x := newScript(t).user("build it")
	began := x.at
	// A reply streamed in parts: its text closes nothing for good, its call goes on.
	x.say("Building.").call("tu1", "Bash", map[string]any{"command": "make"}).result("tu1", "ok").say("Built.")
	ended := x.at
	x.add(map[string]any{"type": "system", "subtype": "turn_duration"})
	if s := x.facts(NewTracker(x.path), x.at); !s.TurnBegan.Equal(began) || !s.IdleSince.Equal(ended) {
		t.Fatalf("began %v ended %v, want %v %v", s.TurnBegan, s.IdleSince, began, ended)
	}
}

func TestWhatCameWhileAWakeupWaitedIsASignItWasLost(t *testing.T) {
	for name, c := range map[string]struct {
		came func(x *script)
		sign string
	}{
		"nothing":         {func(*script) {}, ""},
		"a task's notice": {func(x *script) { x.notify("bt1", "completed").say("Noted.") }, "a task's notification came in while it waited"},
		"a queued notice": {func(x *script) {
			x.add(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "<task-notification><task-id>bt1</task-id></task-notification>"})
		}, "a task's notification came in while it waited"},
		"a compaction": {func(x *script) { x.add(map[string]any{"type": "system", "subtype": "compact_boundary"}) }, "the conversation was compacted while it waited"},
		"a compaction's text": {func(x *script) {
			x.add(map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"content": "summary"}})
		}, "the conversation was compacted while it waited"},
		"a notice after its time": {func(x *script) { x.at = x.at.Add(time.Hour); x.notify("bt1", "completed") }, ""},
		"a new wakeup after it": {func(x *script) {
			x.notify("bt1", "completed").call("tu2", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).result("tu2", "Next wakeup scheduled for 10:41:00 (in 600s)")
		}, ""},
	} {
		x, _ := soak(t)
		c.came(x)
		if s := x.facts(NewTracker(x.path), x.at); s.WakeSign != c.sign {
			t.Errorf("%s: sign %q, want %q", name, s.WakeSign, c.sign)
		}
	}
}

func TestALoopsNextRunIsDueWithItsDelay(t *testing.T) {
	inUTC(t)
	// Recent versions write the schedule in words: the expression is the call's.
	x := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/10 * * * *", "prompt": "check the queue"}).
		result("tu1", "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10. Session-only (not written to disk, dies when Claude exits). Auto-expires after 7 days. Use CronDelete to cancel sooner.").
		say("Looping every 10 minutes.")
	tr := NewTracker(x.path)
	// Created at 10:00:02: its next run is 10:10, and Claude Code may start it up to 5 minutes late.
	run := time.Date(2026, 3, 4, 10, 10, 0, 0, time.UTC)
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run.Add(5*time.Minute)) || s.WakeFor != "check the queue" || s.Phase != sessions.Scheduled || !s.WakeJob ||
		s.WakeKey != "job:j1@"+strconv.FormatInt(run.UnixMilli(), 10) || s.Loop != "Every 10 minutes" {
		t.Fatalf("due %v for %q phase %s job %v key %q loop %q, want %v", s.WakeDue, s.WakeFor, s.Phase, s.WakeJob, s.WakeKey, s.Loop, run.Add(5*time.Minute))
	}
	x.at = run.Add(3 * time.Minute)
	x.fire("Running scheduled task (Mar 4 10:13am)", "j1")
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run.Add(15 * time.Minute)) {
		t.Fatalf("after its run: due %v, want the 10:20 run's latest start", s.WakeDue)
	}
	// Another job's run is not this job's, and an Esc does not stop a job.
	x.fire("Running scheduled task (Mar 4 10:25am)", "other").user("and the logs").interrupted()
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run.Add(15 * time.Minute)) {
		t.Fatalf("another job, an Esc: due %v", s.WakeDue)
	}
	// A restart restores it, from its first run after the restart: it does not run for the runs it missed.
	s := &sessions.Session{Started: run.Add(2 * time.Hour), Tasks: []sessions.Task{}}
	NewTracker(x.path).Facts(s, run.Add(2*time.Hour))
	if !s.WakeDue.Equal(run.Add(2*time.Hour + 15*time.Minute)) {
		t.Fatalf("after a restart: due %v", s.WakeDue)
	}
	// It expires 7 days after it was made.
	if s := x.facts(tr, t0.Add(7*24*time.Hour+time.Minute)); !s.WakeDue.IsZero() {
		t.Fatalf("expired: due %v", s.WakeDue)
	}
	// An older version wrote the expression in its result, and no input names it.
	y := newScript(t).call("tu1", "CronCreate", map[string]any{"prompt": "check the queue"}).
		result("tu1", "Scheduled recurring job j1 (*/10 * * * *). Session-only (not written to disk, dies when Claude exits).")
	if s := y.facts(NewTracker(y.path), y.at); !s.WakeDue.Equal(run.Add(5 * time.Minute)) {
		t.Fatalf("an older result: due %v", s.WakeDue)
	}
}

func TestAOneShotIsDueOnTime(t *testing.T) {
	inUTC(t)
	x := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "20 10 4 3 *", "prompt": "remind me of the review", "recurring": false}).
		result("tu1", "Scheduled one-shot task r1 (Mar 4 at 10:20 AM). Next run: 10:20. Session-only (not written to disk, dies when Claude exits). It will fire once then auto-delete.")
	tr := NewTracker(x.path)
	run := time.Date(2026, 3, 4, 10, 20, 0, 0, time.UTC)
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run) || !s.WakeJob || s.WakeFor != "remind me of the review" {
		t.Fatalf("pending: due %v job %v for %q", s.WakeDue, s.WakeJob, s.WakeFor)
	}
	// A restart after its time loses it: Claude Code restores no one-shot whose time passed.
	s := &sessions.Session{Started: run.Add(time.Minute), Tasks: []sessions.Task{}}
	NewTracker(x.path).Facts(s, run.Add(time.Hour))
	if !s.WakeDue.IsZero() {
		t.Fatalf("restarted after it: due %v", s.WakeDue)
	}
	x.at = run
	x.fire("Running scheduled task (Mar 4 10:20am)", "r1")
	if s := x.facts(tr, x.at); !s.WakeDue.IsZero() || s.Loop != "" {
		t.Fatalf("after it ran: due %v loop %q", s.WakeDue, s.Loop)
	}
}

func TestSchedulesReadAsCronDoes(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 3, 4, h, m, 30, 0, time.Local) } // a Wednesday
	day := func(d, h, m int) time.Time { return time.Date(2026, 3, d, h, m, 0, 0, time.Local) }
	cases := []struct {
		expr     string
		after    time.Time
		next     time.Time
		lateness time.Duration
	}{
		{"*/5 * * * *", at(10, 1), day(4, 10, 5), 0},
		{"*/10 * * * *", at(10, 1), day(4, 10, 10), 5 * time.Minute},
		{"3,13,23,33,43,53 * * * *", at(10, 13), day(4, 10, 23), 5 * time.Minute},
		{"4-59/10 * * * *", at(10, 55), day(4, 11, 4), 5 * time.Minute},
		{"0 9 * * *", at(10, 0), day(5, 9, 0), 30 * time.Minute},
		{"13 9,15,21 * * *", at(9, 30), day(4, 15, 13), 30 * time.Minute},
		{"0 9 * * 1", at(10, 0), day(9, 9, 0), 30 * time.Minute},
		{"0 9 15 * 1", at(10, 0), day(9, 9, 0), 30 * time.Minute},      // either the 15th or a Monday
		{"0 9 1-31 * 1", at(10, 0), day(9, 9, 0), 30 * time.Minute},    // every day of the month is no restriction: Mondays
		{"0 9 1,15 * 0-6", at(10, 0), day(15, 9, 0), 30 * time.Minute}, // every day of the week is none: the 1st and the 15th
		{"0 9 1 * 0-7", at(10, 0), time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local), 30 * time.Minute},
	}
	for _, c := range cases {
		sc, ok := parseSchedule(c.expr)
		if !ok {
			t.Fatalf("%s: not read", c.expr)
		}
		n := sc.next(c.after)
		if !n.Equal(c.next) || sc.lateness(n) != c.lateness {
			t.Errorf("%s after %v: next %v lateness %v, want %v %v", c.expr, c.after, n, sc.lateness(n), c.next, c.lateness)
		}
	}
	for _, bad := range []string{"", "* * * *", "0 9 * * MON", "61 * * * *", "*/0 * * * *", "5-1 * * * *", "5/15 * * * *"} {
		if _, ok := parseSchedule(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
}

func TestAClockChangeRunsAMinuteOnce(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	l := time.Local
	time.Local = ny
	t.Cleanup(func() { time.Local = l })
	sc, _ := parseSchedule("30 * * * *")
	ran := time.Date(2026, 11, 1, 5, 30, 30, 0, time.UTC)                           // the first 01:30 there, before the clock goes back an hour
	if n := sc.next(ran); !n.Equal(time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)) { // 02:30, an hour after standard time began
		t.Fatalf("after the repeated hour's first pass: next %v", n)
	}
	// Spring forward: the hour that never comes does not run.
	sc, _ = parseSchedule("30 2 * * *")
	if n := sc.next(time.Date(2026, 3, 8, 1, 0, 0, 0, ny)); !n.Equal(time.Date(2026, 3, 9, 2, 30, 0, 0, ny)) {
		t.Fatalf("the skipped hour: next %v", n)
	}
}
