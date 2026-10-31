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
	b := newTimeline(t, "restored").owner(0, "check the deploy every 10 minutes", "turn_duration").
		call(time.Second, "CronCreate", "tc1", `{"cron":"*/10 * * * *","prompt":"check the deploy"}`).
		result(time.Second, "CronCreate", "tc1", "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10.").say(time.Second, "Checking.")
	// The session ends mid-task and is reopened: the job is restored, and its runs never start.
	b.add(2*time.Minute, "harness_text", store.ActorHarness, "", "<task-notification><task-id>__orphan_summary__</task-id> didn't finish before the previous session ended</task-notification>", `{"why":"meta line"}`).
		say(time.Second, "Back.").owner(time.Hour, "status?", "turn_duration")
	b.put(st)
	if acted := overdueActs(t, st, t0.Add(3*time.Hour)); len(acted) != 1 || !strings.HasPrefix(acted[0].Point.Stop.WakeKey, "job:j1@") {
		t.Fatalf("acted %s", fmt.Sprint(acted))
	}
}

// inUTCHere runs a test on a machine whose zone is UTC: schedules read in local time.
func inUTCHere(t *testing.T) {
	l := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = l })
}
