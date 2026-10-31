package supervise

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// overdueActs replays the store's history and returns the overdue rule's acts.
func overdueActs(t *testing.T, st *store.Store, until time.Time) []Result {
	t.Helper()
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	var acted []Result
	for _, r := range Replay(points, nil, DefaultParams, probeAt(time.Time{}), until) {
		if r.Decision.Rule == RuleOverdue && r.Fired {
			acted = append(acted, r)
		}
	}
	return acted
}

func TestReplayReadsJobsAndTheirRunsFromTheTranscript(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	r := newRecorded(t, "jobs").owner(0, "watch both")
	for _, j := range []struct{ call, id, what, cron, out string }{
		{"tc1", "jA", "check A", "*/10 * * * *", "Scheduled recurring job jA (Every 10 minutes). Next run: 10:10. Session-only (not written to disk, dies when Claude exits)."},
		{"tc2", "jB", "remind B", "10 10 4 3 *", "Scheduled one-shot task jB (Mar 4 at 10:10 AM). Next run: 10:10. It will fire once then auto-delete."},
	} {
		// The store keeps the start of a call's input: here it lost the expression, which the transcript holds.
		r.event(time.Second, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": j.call,
			"name": "CronCreate", "input": map[string]any{"prompt": j.what, "cron": j.cron}}}}},
			"tool_call", store.ActorAgent, "CronCreate", fmt.Sprintf(`{"prompt":%q,"cr`, j.what), fmt.Sprintf(`{"tool_id":%q}`, j.call))
		r.event(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": j.call, "content": j.out}}}},
			"tool_result", store.ActorTool, "CronCreate", j.out, fmt.Sprintf(`{"tool_id":%q}`, j.call))
	}
	r.say(time.Second, "Watching both.")
	// Only B's run starts, at 10:10: Claude Code's record names it, and the run's prompt follows. A's never does.
	r.line(10*time.Minute+20*time.Second, map[string]any{"type": "system", "subtype": "scheduled_task_fire", "taskId": "jB", "content": "Running scheduled task"})
	r.event(time.Second, map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "remind B"}},
		"harness_text", store.ActorHarness, "", "remind B", `{"prior":"scheduled_task_fire","why":"meta line"}`)
	r.say(time.Second, "Reminded.")
	r.owner(time.Hour, "status?")
	r.put(st)
	acted := overdueActs(t, st, t0.Add(3*time.Hour))
	if len(acted) != 1 || !strings.HasPrefix(acted[0].Point.Stop.WakeKey, "job:jA@") || !strings.Contains(acted[0].Decision.Text, `"check A"`) {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayLeavesWakeupsTheTranscriptShowsEnded(t *testing.T) {
	st := testStore(t)
	wake := func(b *timelineBuilder) *timelineBuilder {
		return b.call(time.Second, "ScheduleWakeup", "tw1", `{"delaySeconds":1800,"prompt":"<<autonomous-loop-dynamic>>","reason":"check the soak run"}`).
			result(time.Second, "ScheduleWakeup", "tw1", "Next wakeup scheduled for 10:30:03 (in 1800s).").say(time.Second, "Next check at 10:30.")
	}
	notified := func(b *timelineBuilder) *timelineBuilder {
		return b.add(5*time.Minute, "harness_text", store.ActorHarness, "", "<task-notification><task-id>bt1</task-id><status>completed</status></task-notification>", `{"why":"meta line"}`).
			say(time.Second, "Noted.")
	}
	// A compaction while it waited is a sign: it acts.
	compacted := wake(newTimeline(t, "compacted").owner(0, "watch the soak", "turn_duration"))
	compacted.add(5*time.Minute, "compaction", store.ActorHarness, "", "summary", "").owner(time.Minute, "go on", "turn_duration").say(time.Second, "Going.").
		owner(time.Hour, "status?", "turn_duration")
	compacted.put(st)
	// Its owner's Esc cancelled it: nothing after is a sign.
	escaped := notified(wake(newTimeline(t, "escaped").owner(0, "watch the soak", "turn_duration")))
	escaped.owner(time.Minute, "look at the logs", "turn_duration").add(time.Second, "harness_text", store.ActorHarness, "", "[Request interrupted by user]", "")
	notified(escaped).owner(time.Hour, "status?", "turn_duration")
	escaped.put(st)
	// Background work runs past its time, so nothing acts then; when the work ends hours later, the wakeup's time
	// came long before that stop: it is past.
	late := wake(newTimeline(t, "late").owner(0, "watch the soak", "turn_duration").
		call(time.Second, "Bash", "tb1", `{"command":"./soak","run_in_background":true}`).
		result(time.Second, "Bash", "tb1", "Command running in background with ID: b1. Output is being written to: /x/b1.output"))
	late.add(5*time.Minute, "compaction", store.ActorHarness, "", "summary", "")
	late.add(4*time.Hour, "harness_text", store.ActorHarness, "", "<task-notification><task-id>b1</task-id><status>completed</status></task-notification>", `{"why":"meta line"}`).
		say(time.Second, "The soak finished.").owner(time.Hour, "status?", "turn_duration")
	late.put(st)
	acted := overdueActs(t, st, t0.Add(8*time.Hour))
	if len(acted) != 1 || acted[0].Point.Session != "compacted" {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayRestoresJobsAcrossAReopening(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	// The session ends mid-task and is reopened: the job is restored, durable or session-only, and its runs never
	// start.
	for _, j := range []struct{ session, input, out string }{
		{"durable", `{"cron":"*/10 * * * *","prompt":"check the deploy","durable":true}`, "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10. Persisted to .claude/scheduled_tasks.json."},
		{"session-only", `{"cron":"*/10 * * * *","prompt":"check the deploy"}`, "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10. Session-only (not written to disk, dies when Claude exits)."},
	} {
		b := newTimeline(t, j.session).owner(0, "check the deploy every 10 minutes", "turn_duration").
			call(time.Second, "CronCreate", "tc1", j.input).result(time.Second, "CronCreate", "tc1", j.out).say(time.Second, "Checking.")
		b.add(2*time.Minute, "harness_text", store.ActorHarness, "", "<task-notification><task-id>__orphan_summary__</task-id> didn't finish before the previous session ended</task-notification>", `{"why":"meta line"}`).
			say(time.Second, "Back.").owner(time.Hour, "status?", "turn_duration")
		b.put(st)
	}
	acted := overdueActs(t, st, t0.Add(3*time.Hour))
	if len(acted) != 2 || !strings.HasPrefix(acted[0].Point.Stop.WakeKey, "job:j1@") || !strings.HasPrefix(acted[1].Point.Stop.WakeKey, "job:j1@") {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
	// A one-shot whose time came before the reopening is not restored.
	st = testStore(t)
	o := newTimeline(t, "once").owner(0, "remind me at 10:02", "turn_duration").
		call(time.Second, "CronCreate", "tc1", `{"cron":"2 10 4 3 *","prompt":"remind me","recurring":false}`).
		result(time.Second, "CronCreate", "tc1", "Scheduled one-shot task r1 (Mar 4 at 10:02 AM). Next run: 10:02.").say(time.Second, "Will remind you.")
	o.add(5*time.Minute, "harness_text", store.ActorHarness, "", "<task-notification><task-id>__orphan_summary__</task-id> didn't finish before the previous session ended</task-notification>", `{"why":"meta line"}`).
		say(time.Second, "Back.").owner(time.Hour, "status?", "turn_duration")
	o.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 0 {
		t.Fatalf("a one-shot past its time: acted %s", fmt.Sprint(acted))
	}
}

func TestReplayCreditsARunByItsPromptWhenTheTranscriptIsGone(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	b := newTimeline(t, "ran").owner(0, "check the deploy every half hour", "turn_duration").
		call(time.Second, "CronCreate", "tc1", `{"cron":"*/30 * * * *","prompt":"check the deploy"}`).
		result(time.Second, "CronCreate", "tc1", "Scheduled recurring job j1 (Every 30 minutes). Next run: 10:30.").say(time.Second, "Checking.")
	b.add(30*time.Minute+30*time.Second, "harness_text", store.ActorHarness, "", "check the deploy", `{"prior":"scheduled_task_fire","why":"meta line"}`).
		say(time.Second, "Checked.").owner(40*time.Minute, "status?", "turn_duration") // before the 11:00 run's time is past
	b.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 0 {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayDropsAWakeupARestartLost(t *testing.T) {
	st := testStore(t)
	for name, end := range map[string]string{
		"a reopening":        "<task-notification><task-id>__orphan_summary__</task-id> didn't finish before the previous session ended</task-notification>",
		"the restart notice": "<task-notification><task-id>tw1</task-id><status>stopped</status><summary>This session restarted 5 minutes after its next /loop wakeup was due, so that wakeup will not fire.</summary></task-notification>",
	} {
		b := newTimeline(t, name).owner(0, "watch the soak", "turn_duration").
			call(time.Second, "ScheduleWakeup", "tw1", `{"delaySeconds":1800,"prompt":"<<autonomous-loop-dynamic>>","reason":"check the soak run"}`).
			result(time.Second, "ScheduleWakeup", "tw1", "Next wakeup scheduled for 10:30:03 (in 1800s).").say(time.Second, "Next check at 10:30.").
			add(5*time.Minute, "compaction", store.ActorHarness, "", "summary", "")
		b.add(time.Minute, "harness_text", store.ActorHarness, "", end, `{"why":"meta line"}`).say(time.Second, "Back.").
			owner(time.Hour, "status?", "turn_duration")
		b.put(st)
	}
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 0 {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayTakesANoticeAfterTheTimeForASign(t *testing.T) {
	st := testStore(t)
	// A background agent runs through the wakeup's time; its notification comes after it, as #74569 shows.
	b := newTimeline(t, "agent").owner(0, "watch the soak", "turn_duration").
		call(time.Second, "Agent", "ta1", `{"description":"watch the logs","run_in_background":true}`).
		result(time.Second, "Agent", "ta1", "Async agent launched successfully. agentId: a1").
		call(time.Second, "ScheduleWakeup", "tw1", `{"delaySeconds":1800,"prompt":"<<autonomous-loop-dynamic>>","reason":"check the soak run"}`).
		result(time.Second, "ScheduleWakeup", "tw1", "Next wakeup scheduled for 10:30:04 (in 1800s).").say(time.Second, "Next check at 10:30.")
	b.add(45*time.Minute, "harness_text", store.ActorHarness, "", "<task-notification><task-id>a1</task-id><status>completed</status></task-notification>", `{"why":"meta line"}`).
		say(time.Second, "The agent is done.").owner(time.Hour, "status?", "turn_duration")
	b.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 1 {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayTakesANewVersionForARestart(t *testing.T) {
	st := testStore(t)
	r := newRecorded(t, "upgraded")
	line := func(after time.Duration, v map[string]any, kind string, actor store.Actor, tool, summary, meta string) {
		v["version"] = "2.1.300"
		r.event(after, v, kind, actor, tool, summary, meta)
	}
	line(0, map[string]any{"type": "user", "message": map[string]any{"content": "watch the soak"}}, "owner_text", store.ActorOwner, "", "watch the soak", `{"input_kind":"typed"}`)
	line(time.Second, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tw1",
		"name": "ScheduleWakeup", "input": map[string]any{"delaySeconds": 1800, "reason": "check the soak run"}}}}},
		"tool_call", store.ActorAgent, "ScheduleWakeup", `{"delaySeconds":1800,"reason":"check the soak run"}`, `{"tool_id":"tw1"}`)
	out := "Next wakeup scheduled for 10:30:03 (in 1800s)."
	line(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tw1", "content": out}}}},
		"tool_result", store.ActorTool, "ScheduleWakeup", out, `{"tool_id":"tw1"}`)
	line(5*time.Minute, map[string]any{"type": "system", "subtype": "compact_boundary"}, "compaction", store.ActorHarness, "", "summary", "")
	// The owner restarts the session on a newer Claude Code: the restart in the foreground lost the wakeup.
	r.event(time.Minute, map[string]any{"type": "user", "version": "2.1.316", "message": map[string]any{"content": "status?"}},
		"owner_text", store.ActorOwner, "", "status?", `{"input_kind":"typed"}`)
	r.event(time.Second, map[string]any{"type": "assistant", "version": "2.1.316", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Fine."}}}},
		"agent_text", store.ActorAgent, "", "Fine.", "")
	r.owner(time.Hour, "anything else?")
	r.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 0 {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayListsNoRunsOfAVersionThatRecordsNone(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	r := newRecorded(t, "old")
	line := func(after time.Duration, v map[string]any, kind string, actor store.Actor, tool, summary, meta string) {
		v["version"] = "2.1.284"
		r.event(after, v, kind, actor, tool, summary, meta)
	}
	line(0, map[string]any{"type": "user", "message": map[string]any{"content": "check the deploy every 10 minutes"}}, "owner_text", store.ActorOwner, "", "check the deploy every 10 minutes", `{"input_kind":"typed"}`)
	line(time.Second, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tc1",
		"name": "CronCreate", "input": map[string]any{"cron": "*/10 * * * *", "prompt": "check the deploy"}}}}},
		"tool_call", store.ActorAgent, "CronCreate", `{"cron":"*/10 * * * *","prompt":"check the deploy"}`, `{"tool_id":"tc1"}`)
	out := "Scheduled recurring job j1 (*/10 * * * *). Session-only (not written to disk, dies when Claude exits)."
	line(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tc1", "content": out}}}},
		"tool_result", store.ActorTool, "CronCreate", out, `{"tool_id":"tc1"}`)
	line(time.Hour, map[string]any{"type": "user", "message": map[string]any{"content": "status?"}}, "owner_text", store.ActorOwner, "", "status?", `{"input_kind":"typed"}`)
	r.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 0 {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

func TestReplayReadsARecordsOwnVersion(t *testing.T) {
	st := testStore(t)
	r := newRecorded(t, "nested")
	line := func(after time.Duration, v map[string]any, kind string, actor store.Actor, tool, summary, meta string) {
		v["version"] = "2.1.316"
		r.event(after, v, kind, actor, tool, summary, meta)
	}
	line(0, map[string]any{"type": "user", "message": map[string]any{"content": "watch the soak"}}, "owner_text", store.ActorOwner, "", "watch the soak", `{"input_kind":"typed"}`)
	line(time.Second, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tw1",
		"name": "ScheduleWakeup", "input": map[string]any{"delaySeconds": 1800, "reason": "check the soak run"}}}}},
		"tool_call", store.ActorAgent, "ScheduleWakeup", `{"delaySeconds":1800,"reason":"check the soak run"}`, `{"tool_id":"tw1"}`)
	out := "Next wakeup scheduled for 10:30:03 (in 1800s)."
	line(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tw1", "content": out}}}},
		"tool_result", store.ActorTool, "ScheduleWakeup", out, `{"tool_id":"tw1"}`)
	line(5*time.Minute, map[string]any{"type": "system", "subtype": "compact_boundary"}, "compaction", store.ActorHarness, "", "summary", "")
	// A tool's result whose own data names a version, written before the record's: no restart.
	r.event(time.Minute, map[string]any{"toolUseResult": map[string]any{"version": "9.9.9"}, "type": "user", "version": "2.1.316",
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tb1", "content": "ok"}}}},
		"tool_result", store.ActorTool, "Bash", "ok", `{"tool_id":"tb1"}`)
	line(time.Second, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Noted."}}}},
		"agent_text", store.ActorAgent, "", "Noted.", "")
	r.owner(time.Hour, "status?")
	r.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 1 {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

// inUTCHere runs a test on a machine whose zone is UTC: schedules read in local time.
func inUTCHere(t *testing.T) {
	l := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = l })
}
