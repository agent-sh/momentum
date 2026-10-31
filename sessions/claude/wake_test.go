package claude

import (
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
)

func (x *script) fire(content, task string) *script {
	return x.add(map[string]any{"type": "system", "subtype": "scheduled_task_fire", "content": content, "taskId": task, "isMeta": false})
}

func TestAWakeupIsDueUntilItFires(t *testing.T) {
	x := newScript(t).user("watch the soak").
		call("tu1", "ScheduleWakeup", map[string]any{"delaySeconds": 1800, "prompt": "<<autonomous-loop-dynamic>>", "reason": "check the soak run"}).
		result("tu1", "Next wakeup scheduled for 10:30:03 (in 1800s). If you owe the user a status update, write it now.").
		say("The soak runs. Next check at 10:30.")
	due := t0.Add(3*time.Second + 1800*time.Second)
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); s.Phase != sessions.Scheduled || !s.WakeDue.Equal(due) || s.WakeFor != "check the soak run" {
		t.Fatalf("pending: phase %s due %v for %q", s.Phase, s.WakeDue, s.WakeFor)
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
	// A run of no job it knows, at the wakeup's time, is the wakeup's, whatever its notice says.
	w := newScript(t).call("tu1", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).result("tu1", "Next wakeup scheduled for 10:10:02 (in 600s)").say("ok")
	w.at = t0.Add(602 * time.Second)
	w.fire("Resuming", "w9")
	if s := w.facts(NewTracker(w.path), w.at); !s.WakeDue.IsZero() {
		t.Fatalf("an unknown run at its time: due %v", s.WakeDue)
	}
	// A loop the agent stopped takes its wakeup with it.
	y := newScript(t).call("tu1", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).result("tu1", "Next wakeup scheduled for 10:10:02 (in 600s)").
		call("tu2", "ScheduleWakeup", map[string]any{"stop": true}).result("tu2", "Loop stopped: cancelled 1 pending wakeup(s); no further dynamic-loop wakeups.")
	if s := y.facts(NewTracker(y.path), y.at); !s.WakeDue.IsZero() || !s.WakeAt.IsZero() {
		t.Fatalf("stopped: due %v wake %v", s.WakeDue, s.WakeAt)
	}
	// A wakeup an earlier process set died with it: a self-paced loop is not restored.
	z := newScript(t).call("tu1", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).result("tu1", "Next wakeup scheduled for 10:10:02 (in 600s)").say("ok")
	s := &sessions.Session{Started: z.at.Add(time.Minute), Tasks: []sessions.Task{}}
	NewTracker(z.path).Facts(s, z.at.Add(2*time.Minute))
	if !s.WakeDue.IsZero() || !s.WakeAt.IsZero() {
		t.Fatalf("an earlier process's: due %v wake %v", s.WakeDue, s.WakeAt)
	}
}

func TestALoopsNextRunIsDueWithItsDelay(t *testing.T) {
	x := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/10 * * * *", "prompt": "check the queue", "recurring": true}).
		result("tu1", "Scheduled recurring job j1 (*/10 * * * *). Session-only (not written to disk, dies when Claude exits).").
		say("Looping every 10 minutes.")
	tr := NewTracker(x.path)
	// Created at 10:00:02: its next run is 10:10, and Claude Code may start it up to 5 minutes late.
	run := time.Date(2026, 3, 4, 10, 10, 0, 0, time.UTC)
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run.Add(5*time.Minute)) || s.WakeFor != "check the queue" || s.Phase != sessions.Scheduled {
		t.Fatalf("due %v for %q phase %s, want %v", s.WakeDue, s.WakeFor, s.Phase, run.Add(5*time.Minute))
	}
	x.at = run.Add(3 * time.Minute)
	x.fire("Running scheduled task (Mar 4 10:13am)", "j1")
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run.Add(15 * time.Minute)) {
		t.Fatalf("after its run: due %v, want the 10:20 run's latest start", s.WakeDue)
	}
	// Another job's run is not this job's.
	x.fire("Running scheduled task (Mar 4 10:25am)", "other")
	if s := x.facts(tr, x.at); !s.WakeDue.Equal(run.Add(15 * time.Minute)) {
		t.Fatalf("another job: due %v", s.WakeDue)
	}
}

func TestSchedulesReadAsCronDoes(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 3, 4, h, m, 30, 0, time.Local) } // a Wednesday
	cases := []struct {
		expr     string
		after    time.Time
		next     time.Time
		lateness time.Duration
	}{
		{"*/5 * * * *", at(10, 1), time.Date(2026, 3, 4, 10, 5, 0, 0, time.Local), 0},
		{"*/10 * * * *", at(10, 1), time.Date(2026, 3, 4, 10, 10, 0, 0, time.Local), 5 * time.Minute},
		{"3,13,23,33,43,53 * * * *", at(10, 13), time.Date(2026, 3, 4, 10, 23, 0, 0, time.Local), 5 * time.Minute},
		{"4-59/10 * * * *", at(10, 55), time.Date(2026, 3, 4, 11, 4, 0, 0, time.Local), 5 * time.Minute},
		{"0 9 * * *", at(10, 0), time.Date(2026, 3, 5, 9, 0, 0, 0, time.Local), 30 * time.Minute},
		{"13 9,15,21 * * *", at(9, 30), time.Date(2026, 3, 4, 15, 13, 0, 0, time.Local), 30 * time.Minute},
		{"0 9 * * 1", at(10, 0), time.Date(2026, 3, 9, 9, 0, 0, 0, time.Local), 30 * time.Minute},
		{"0 9 15 * 1", at(10, 0), time.Date(2026, 3, 9, 9, 0, 0, 0, time.Local), 30 * time.Minute}, // either the 15th or a Monday
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
	for _, bad := range []string{"", "* * * *", "0 9 * * MON", "61 * * * *", "*/0 * * * *", "5-1 * * * *"} {
		if _, ok := parseSchedule(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
}
