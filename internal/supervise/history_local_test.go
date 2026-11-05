package supervise

import (
	"context"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

const owedReply = "working: the model owed its reply"

// localCommand is a Claude Code local command as the store keeps it: its echo, marked as a local command with the
// system record before it, then its output and the caveat Claude Code writes after it.
func (b *timelineBuilder) localCommand(after time.Duration, name, prior string) *timelineBuilder {
	meta := `{"input_kind":"slash","why":"local command"}`
	if prior != "" {
		meta = `{"input_kind":"slash","prior":"` + prior + `","why":"local command"}`
	}
	b.add(after, "harness_text", store.ActorHarness, "", "<command-name>/"+name+"</command-name> <command-message>"+name+"</command-message> <command-args></command-args>", meta)
	b.add(0, "harness_text", store.ActorHarness, "", "<local-command-stdout>Done</local-command-stdout>", `{"input_kind":"typed","why":"harness text"}`)
	return b.add(time.Millisecond, "harness_text", store.ActorHarness, "", "<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request.</local-command-caveat>",
		`{"prior":"informational","why":"meta line"}`)
}

// codex makes the session's events Codex's.
func (b *timelineBuilder) codex() *timelineBuilder {
	for i := range b.evs {
		b.evs[i].Harness = "codex"
	}
	return b
}

// replayed replays a store's history and returns the result whose input is the one at next.
func replayed(t *testing.T, st *store.Store, session string, next time.Time) Result {
	t.Helper()
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Replay(points, nil, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Session == session && r.Point.Next.At.Equal(next) {
			return r
		}
	}
	t.Fatalf("no stop of %s ends at %v", session, next)
	return Result{}
}

// A Claude Code local command such as /login never reaches the model: its user was at the session, and no turn
// opened. The stop before it goes on to the next input, which reads it as it was: a usage limit, a question.
func TestALocalCommandIsTheOwnersPresenceAndOpensNoTurn(t *testing.T) {
	st := testStore(t)
	limit := newTimeline(t, "limit").owner(0, "run the soak", "turn_duration").say(10*time.Second, "Running the soak.").
		apiError(time.Minute, "You've hit your weekly limit · resets Mar 9, 9am (Europe/Lisbon)")
	stoppedAt := limit.at
	limit.localCommand(20*time.Minute, "login", "informational")
	login := limit.at.Add(-time.Millisecond)
	limit.owner(2*time.Second, "resume", "")
	resume := limit.at
	limit.put(st)
	asks := newTimeline(t, "asks").owner(0, "plan the cleanup", "").say(10*time.Second, "Should I start on items 1 to 4?")
	asked := asks.at
	asks.localCommand(10*time.Minute, "login", "turn_duration").owner(2*time.Second, "resume", "")
	asksResume := asks.at
	asks.put(st)
	// a turn that ended on a call's result, as its stop hook said on the local command's record
	hook := newTimeline(t, "hook").owner(0, "lint it", "").call(time.Second, "Bash", "t1", `{"command":"make lint"}`).
		result(time.Second, "Bash", "t1", "ok")
	hook.localCommand(5*time.Minute, "effort", "stop_hook_summary").owner(time.Minute, "continue", "")
	hookNext := hook.at
	hook.put(st)

	r := replayed(t, st, "limit", resume)
	if s := r.Point.Stop; s.Working || !s.At.Equal(stoppedAt) || s.Error == "" || !s.OwnerAt.Equal(login) {
		t.Fatalf("the stop before the limit's resume: working %v, at %v, error %q, owner at %v", s.Working, s.At, s.Error, s.OwnerAt)
	}
	if r.Decision.Rule != RuleAccess || Why(r) == owedReply {
		t.Fatalf("the limit's stop: %s, %q", r.Decision.Rule, Why(r))
	}
	if r := replayed(t, st, "asks", asksResume); r.Point.Stop.Working || !r.Point.Stop.At.Equal(asked) || Why(r) != "its last words ask the owner" {
		t.Fatalf("the question's stop: working %v, at %v, %q", r.Point.Stop.Working, r.Point.Stop.At, Why(r))
	}
	if r := replayed(t, st, "hook", hookNext); r.Point.Stop.Working || Why(r) == owedReply {
		t.Fatalf("a turn its hook ended: working %v, %q", r.Point.Stop.Working, Why(r))
	}
	points, err := History(context.Background(), st, t0.Add(-time.Hour), t0.Add(6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		if p.Next.InputKind == "slash" {
			t.Fatalf("a local command ended a stop: %+v", p)
		}
	}
}

// Codex records its user's interruption of a turn as a turn_aborted message, or marks it on the record after it:
// either is an interruption, so the stop before his next input owes no reply.
func TestACodexAbortedTurnIsAnInterruption(t *testing.T) {
	st := testStore(t)
	aborted := newTimeline(t, "aborted").owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`).
		result(12*time.Second, "exec", "c1", "aborted by user after 12.5s").
		add(time.Second, "harness_text", store.ActorHarness, "", "<turn_aborted> The previous turn was interrupted on purpose. Any running unified exec processes may still be running.", `{"why":"developer message"}`).
		add(20*time.Minute, "compaction", store.ActorHarness, "", "", `{"why":"compacted"}`).
		owner(time.Minute, "continue", "")
	next := aborted.at
	aborted.codex().put(st)
	marked := newTimeline(t, "marked").owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`).
		result(12*time.Second, "exec", "c1", "aborted by user after 12.5s").
		add(10*time.Minute, "harness_text", store.ActorHarness, "", "# AGENTS.md instructions", `{"input_kind":"typed","prior":"turn_aborted","why":"harness text"}`).
		owner(time.Second, ".", "")
	markedNext := marked.at
	marked.codex().put(st)

	for _, c := range []struct {
		session string
		next    time.Time
	}{{"aborted", next}, {"marked", markedNext}} {
		r := replayed(t, st, c.session, c.next)
		if r.Point.Stop.Working || Why(r) == owedReply {
			t.Fatalf("%s: the stop before the owner's nudge: working %v, %q", c.session, r.Point.Stop.Working, Why(r))
		}
	}
}
