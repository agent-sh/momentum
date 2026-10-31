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

// first is a session's earliest own run, zero when it has none.
func first(s *sessions.Session) sessions.Run {
	if len(s.Runs) == 0 {
		return sessions.Run{}
	}
	return s.Runs[0]
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
	if s := x.facts(tr, x.at); s.Phase != sessions.Scheduled || !first(s).Due.Equal(due) || first(s).For != "check the soak run" || first(s).Job ||
		!strings.HasPrefix(first(s).Key, "wakeup@") || first(s).Sign != "" || !s.IdleSince.Equal(ended) {
		t.Fatalf("pending: phase %s due %v for %q key %q sign %q idle %v", s.Phase, first(s).Due, first(s).For, first(s).Key, first(s).Sign, s.IdleSince)
	}
	// Past its time and the grace, it is no longer scheduled; it is still due, and has not fired.
	if s := x.facts(tr, due.Add(time.Hour)); s.Phase != sessions.Idle || !first(s).Due.Equal(due) {
		t.Fatalf("an hour late: phase %s due %v", s.Phase, first(s).Due)
	}
	x.at = due
	x.fire("Claude resuming /loop wakeup (Mar 4 10:30am)", "w1").
		add(map[string]any{"type": "user", "isMeta": true, "promptSource": "system", "message": map[string]any{"content": "# Autonomous loop tick"}})
	if s := x.facts(tr, x.at); !first(s).Due.IsZero() || !s.WakeAt.IsZero() {
		t.Fatalf("after it fired: due %v wake %v", first(s).Due, s.WakeAt)
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
		if s := y.facts(NewTracker(y.path), y.at); !first(s).Due.IsZero() || !s.WakeAt.IsZero() {
			t.Errorf("%s: due %v wake %v", name, first(s).Due, s.WakeAt)
		}
	}
	// A record dated before the wakeup was set (Claude Code writes some late) does not end it.
	for name, end := range map[string]func(x *script){
		"a run": func(x *script) {
			x.add(map[string]any{"type": "system", "subtype": "scheduled_task_fire", "taskKind": "loop", "timestamp": t0.Format(time.RFC3339Nano)})
		},
		"an Esc": func(x *script) {
			x.add(map[string]any{"type": "user", "timestamp": t0.Format(time.RFC3339Nano), "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
		},
	} {
		y, due := soak(t)
		end(y)
		if s := y.facts(NewTracker(y.path), y.at); !first(s).Due.Equal(due) {
			t.Errorf("%s dated before it: due %v", name, first(s).Due)
		}
	}
	// A run of a task it does not know is not the wakeup's.
	w, due := soak(t)
	w.at = due
	w.fire("Running scheduled task (Mar 4 10:30am)", "r9")
	if s := w.facts(NewTracker(w.path), w.at); !first(s).Due.Equal(due) {
		t.Fatalf("another task's run: due %v", first(s).Due)
	}
	// A restart in the foreground loses a pending wakeup; a background session, whose start is not known, keeps it:
	// its process arms it again.
	z, due := soak(t)
	for _, c := range []struct {
		started time.Time
		kept    bool
	}{{time.Time{}, true}, {due.Add(-20 * time.Minute), false}} {
		s := &sessions.Session{Started: c.started, Tasks: []sessions.Task{}}
		NewTracker(z.path).Facts(s, due.Add(2*time.Minute))
		if first(s).Due.IsZero() == c.kept {
			t.Errorf("started %v: due %v", c.started, first(s).Due)
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

func TestAVersionThatRecordsNoRunsListsNone(t *testing.T) {
	x := newScript(t)
	x.add(map[string]any{"type": "user", "version": "2.1.284", "message": map[string]any{"content": "watch the soak"}})
	x.add(map[string]any{"type": "assistant", "version": "2.1.284", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1",
		"name": "ScheduleWakeup", "input": map[string]any{"delaySeconds": 1800, "reason": "check the soak run"}}}}})
	x.add(map[string]any{"type": "user", "version": "2.1.284", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
		"content": "Next wakeup scheduled for 10:30:03 (in 1800s)."}}}})
	if s := x.facts(NewTracker(x.path), x.at.Add(time.Hour)); len(s.Runs) != 0 {
		t.Fatalf("runs %+v", s.Runs)
	}
	// A session that moved to a version that records them lists the runs it sets since.
	x.add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu2",
		"name": "ScheduleWakeup", "input": map[string]any{"delaySeconds": 1800, "reason": "check the soak run"}}}}})
	x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu2",
		"content": "Next wakeup scheduled for 10:30:06 (in 1800s)."}}}})
	if s := x.facts(NewTracker(x.path), x.at); len(s.Runs) != 1 {
		t.Fatalf("after the upgrade: runs %+v", s.Runs)
	}
	for v, want := range map[string]bool{"2.1.290": false, "2.1.291": true, "2.2.0": true, "3.0.0-beta": true, "": false, "x": false} {
		if RecordsRuns(v) != want {
			t.Errorf("%q: %v", v, !want)
		}
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
		"a notice after its time": {func(x *script) { x.at = x.at.Add(time.Hour); x.notify("bt1", "completed") }, "a task's notification came in while it waited"},
		"two signs: the first stays": {func(x *script) {
			x.add(map[string]any{"type": "system", "subtype": "compact_boundary"}).notify("bt1", "completed")
		}, "the conversation was compacted while it waited"},
		"a sign dated before it was set": {func(x *script) {
			x.add(map[string]any{"type": "system", "subtype": "compact_boundary", "timestamp": t0.Format(time.RFC3339Nano)})
		}, ""},
		"a new wakeup after it": {func(x *script) {
			x.notify("bt1", "completed").call("tu2", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).result("tu2", "Next wakeup scheduled for 10:41:00 (in 600s)")
		}, ""},
	} {
		x, _ := soak(t)
		c.came(x)
		if s := x.facts(NewTracker(x.path), x.at); first(s).Sign != c.sign {
			t.Errorf("%s: sign %q, want %q", name, first(s).Sign, c.sign)
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
	if s := x.facts(tr, x.at); !first(s).Due.Equal(run.Add(5*time.Minute)) || first(s).For != "check the queue" || s.Phase != sessions.Scheduled || !first(s).Job ||
		first(s).Key != "job:j1@"+strconv.FormatInt(run.UnixMilli(), 10) || s.Loop != "Every 10 minutes" {
		t.Fatalf("due %v for %q phase %s job %v key %q loop %q, want %v", first(s).Due, first(s).For, s.Phase, first(s).Job, first(s).Key, s.Loop, run.Add(5*time.Minute))
	}
	x.at = run.Add(3 * time.Minute)
	x.fire("Running scheduled task (Mar 4 10:13am)", "j1")
	if s := x.facts(tr, x.at); !first(s).Due.Equal(run.Add(15 * time.Minute)) {
		t.Fatalf("after its run: due %v, want the 10:20 run's latest start", first(s).Due)
	}
	// Another job's run is not this job's, and an Esc does not stop a job.
	x.fire("Running scheduled task (Mar 4 10:25am)", "other").user("and the logs").interrupted()
	if s := x.facts(tr, x.at); !first(s).Due.Equal(run.Add(15 * time.Minute)) {
		t.Fatalf("another job, an Esc: due %v", first(s).Due)
	}
	// A restart restores it, session-only as it is, from its first run after the restart: it does not run for the
	// runs it missed.
	s := &sessions.Session{Started: run.Add(2 * time.Hour), Tasks: []sessions.Task{}}
	NewTracker(x.path).Facts(s, run.Add(2*time.Hour))
	if !first(s).Due.Equal(run.Add(2*time.Hour+15*time.Minute)) || s.Loop != "Every 10 minutes" {
		t.Fatalf("after a restart: due %v loop %q", first(s).Due, s.Loop)
	}
	// It expires 7 days after it was made.
	if s := x.facts(tr, t0.Add(7*24*time.Hour+time.Minute)); !first(s).Due.IsZero() {
		t.Fatalf("expired: due %v", first(s).Due)
	}
	// An older version wrote the expression in its result, and no input names it.
	y := newScript(t).call("tu1", "CronCreate", map[string]any{"prompt": "check the queue"}).
		result("tu1", "Scheduled recurring job j1 (*/10 * * * *). Session-only (not written to disk, dies when Claude exits).")
	if s := y.facts(NewTracker(y.path), y.at); !first(s).Due.Equal(run.Add(5 * time.Minute)) {
		t.Fatalf("an older result: due %v", first(s).Due)
	}
}

func TestAJobThatStopsRunningListsEachRunItMissed(t *testing.T) {
	inUTC(t)
	x := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/10 * * * *", "prompt": "check the queue"}).
		result("tu1", "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10.")
	tr := NewTracker(x.path)
	// At 10:42 the 10:10, 10:20 and 10:30 runs never started, and the 10:40 run may start until 10:45.
	s := x.facts(tr, time.Date(2026, 3, 4, 10, 42, 0, 0, time.UTC))
	var got []string
	for _, r := range s.Runs {
		got = append(got, r.Due.Format("15:04"))
	}
	if strings.Join(got, " ") != "10:15 10:25 10:35 10:45" {
		t.Fatalf("runs %v", got)
	}
	// A job made days ago that never ran lists its runs of the last hours, up to its next.
	old := x.facts(NewTracker(x.path), time.Date(2026, 3, 6, 10, 42, 0, 0, time.UTC))
	if n := len(old.Runs); n == 0 || !old.Runs[n-1].Due.Equal(time.Date(2026, 3, 6, 10, 45, 0, 0, time.UTC)) {
		t.Fatalf("days later: %d runs, the last %v", n, old.Runs[n-1].Due)
	}
	// Two jobs on one schedule: their runs are in the order of their keys, on every scan.
	z := newScript(t)
	for _, id := range []string{"j2", "j1"} {
		z.call("t"+id, "CronCreate", map[string]any{"cron": "*/10 * * * *", "prompt": "check " + id}).
			result("t"+id, "Scheduled recurring job "+id+" (Every 10 minutes). Next run: 10:10.")
	}
	if s := z.facts(NewTracker(z.path), z.at); len(s.Runs) != 2 || !strings.HasPrefix(s.Runs[0].Key, "job:j1@") {
		t.Fatalf("a tie: %+v", s.Runs)
	}
	// A job every two minutes that stopped running seven hours ago still lists its runs of the last hours.
	two := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/2 * * * *", "prompt": "poll"}).
		result("tu1", "Scheduled recurring job j2 (Every 2 minutes). Next run: 10:02.")
	late := two.facts(NewTracker(two.path), t0.Add(7*time.Hour))
	if n := len(late.Runs); n == 0 || late.Runs[n-1].Due.Before(t0.Add(7*time.Hour)) || late.Runs[0].Due.Before(t0.Add(7*time.Hour-runsBack)) {
		t.Fatalf("seven hours on: %d runs %+v", n, late.Runs)
	}
	// The list reaches as far back as a rule can act, and up to now, though the job runs every minute.
	one := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "* * * * *", "prompt": "poll"}).
		result("tu1", "Scheduled recurring job j1m (Every minute). Next run: 10:01.")
	now := t0.Add(7 * time.Hour)
	minute := one.facts(NewTracker(one.path), now)
	if n := len(minute.Runs); n == 0 || minute.Runs[n-1].Due.Before(now) || minute.Runs[0].Due.After(now.Add(-100*time.Minute)) {
		t.Fatalf("every minute, seven hours on: %d runs %v to %v", n, minute.Runs[0].Due, minute.Runs[n-1].Due)
	}
	// A job every five minutes keeps its runs' keys from scan to scan, though it has not run for hours.
	still := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/5 * * * *", "prompt": "poll"}).
		result("tu1", "Scheduled recurring job j5s (Every 5 minutes). Next run: 10:05.")
	k1 := still.facts(NewTracker(still.path), now).Runs
	k2 := still.facts(NewTracker(still.path), now.Add(15*time.Second)).Runs
	if len(k1) == 0 || len(k2) == 0 || k1[len(k1)-1].Key != k2[len(k2)-1].Key {
		t.Fatalf("five minutes, two scans: %+v %+v", k1[len(k1)-1], k2[len(k2)-1])
	}
	// A job made under a version that writes no record of its runs is not listed, though the session runs a newer one.
	aged := newScript(t)
	aged.add(map[string]any{"type": "assistant", "version": "2.1.284", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1",
		"name": "CronCreate", "input": map[string]any{"cron": "*/10 * * * *", "prompt": "poll"}}}}})
	aged.add(map[string]any{"type": "user", "version": "2.1.284", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
		"content": "Scheduled recurring job jo (*/10 * * * *). Session-only (not written to disk, dies when Claude exits)."}}}})
	aged.say("Upgraded.") // a record of the newer version
	if s := aged.facts(NewTracker(aged.path), aged.at.Add(time.Hour)); len(s.Runs) != 0 {
		t.Fatalf("an old version's job: runs %+v", s.Runs)
	}
	// A job every five minutes runs 4 minutes 45 seconds after it was made, and after a run it missed.
	five := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/5 * * * *", "prompt": "poll"}).
		result("tu1", "Scheduled recurring job j5 (Every 5 minutes). Next run: 10:05.")
	made := five.at
	if s := five.facts(NewTracker(five.path), made.Add(15*time.Minute)); len(s.Runs) < 2 ||
		!s.Runs[0].Due.Equal(made.Add(4*time.Minute+45*time.Second)) || !s.Runs[1].Due.Equal(made.Add(9*time.Minute+30*time.Second)) {
		t.Fatalf("five minutes from its making: %+v", s.Runs)
	}
	// And after a restart.
	restarted := made.Add(time.Hour)
	rs := &sessions.Session{Started: restarted, Tasks: []sessions.Task{}}
	NewTracker(five.path).Facts(rs, restarted)
	if !first(rs).Due.Equal(restarted.Add(4*time.Minute + 45*time.Second)) {
		t.Fatalf("five minutes from a restart: %v", first(rs).Due)
	}
	// A job every five minutes runs 4 minutes 45 seconds after its last run, on time.
	y := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "*/5 * * * *", "prompt": "poll"}).
		result("tu1", "Scheduled recurring job j5 (Every 5 minutes). Next run: 10:05.")
	y.at = time.Date(2026, 3, 4, 10, 5, 0, 0, time.UTC)
	y.fire("Running scheduled task (Mar 4 10:05am)", "j5")
	ran := y.at
	if s := y.facts(NewTracker(y.path), y.at); !first(s).Due.Equal(ran.Add(4*time.Minute + 45*time.Second)) {
		t.Fatalf("five minutes: due %v", first(s).Due)
	}
	// A scheduled run begins a turn, though its prompt is the harness's.
	y.add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "poll"}}).say("Polled.")
	if s := y.facts(NewTracker(y.path), y.at); !s.TurnBegan.Equal(ran) {
		t.Fatalf("the run's turn began %v, want %v", s.TurnBegan, ran)
	}
}

func TestAOneShotIsDueOnTime(t *testing.T) {
	inUTC(t)
	x := newScript(t).call("tu1", "CronCreate", map[string]any{"cron": "20 10 4 3 *", "prompt": "remind me of the review", "recurring": false}).
		result("tu1", "Scheduled one-shot task r1 (Mar 4 at 10:20 AM). Next run: 10:20. Session-only (not written to disk, dies when Claude exits). It will fire once then auto-delete.")
	tr := NewTracker(x.path)
	run := time.Date(2026, 3, 4, 10, 20, 0, 0, time.UTC)
	if s := x.facts(tr, x.at); !first(s).Due.Equal(run) || !first(s).Job || first(s).For != "remind me of the review" {
		t.Fatalf("pending: due %v job %v for %q", first(s).Due, first(s).Job, first(s).For)
	}
	// A restart restores it only if its time had not come: Claude Code restores no one-shot whose time passed.
	for _, c := range []struct {
		started time.Time
		kept    bool
	}{{run.Add(-time.Minute), true}, {run.Add(time.Minute), false}} {
		s := &sessions.Session{Started: c.started, Tasks: []sessions.Task{}}
		NewTracker(x.path).Facts(s, run.Add(time.Hour))
		if first(s).Due.IsZero() == c.kept {
			t.Errorf("restarted at %v: due %v", c.started, first(s).Due)
		}
	}
	x.at = run
	x.fire("Running scheduled task (Mar 4 10:20am)", "r1")
	if s := x.facts(tr, x.at); !first(s).Due.IsZero() || s.Loop != "" {
		t.Fatalf("after it ran: due %v loop %q", first(s).Due, s.Loop)
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
		{"0 9 1 * 1-7", at(10, 0), time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local), 30 * time.Minute}, // 7 is Sunday: every day
		{"0 9 1 * 1-6", at(10, 0), day(5, 9, 0), 30 * time.Minute},                                  // not Sunday: the 1st, or Monday to Saturday
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
	for _, bad := range []string{"", "* * * *", "0 9 * * MON", "61 * * * *", "*/0 * * * *", "5-1 * * * *", "5/15 * * * *", "5/1 * * * *", "+5 * * * *"} {
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
	// A daily run Claude Code delayed into the repeated hour's second pass does not run again that night.
	daily, _ := parseSchedule("45 1 * * *")
	late := time.Date(2026, 11, 1, 6, 15, 0, 0, time.UTC) // 01:15 standard time, the hour's second pass
	if n := daily.next(late); !n.Equal(time.Date(2026, 11, 2, 6, 45, 0, 0, time.UTC)) {
		t.Fatalf("after a run in the second pass: next %v", n)
	}
	// Spring forward: the hour that never comes does not run.
	sc, _ = parseSchedule("30 2 * * *")
	if n := sc.next(time.Date(2026, 3, 8, 1, 0, 0, 0, ny)); !n.Equal(time.Date(2026, 3, 9, 2, 30, 0, 0, ny)) {
		t.Fatalf("the skipped hour: next %v", n)
	}
}
