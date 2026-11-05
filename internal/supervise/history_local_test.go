package supervise

import (
	"context"
	"strings"
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

// replayAll replays a store's history over the test's window.
func replayAll(t *testing.T, st *store.Store) ([]Point, []Result) {
	t.Helper()
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	return points, Replay(points, nil, DefaultParams, probeAt(time.Time{}), until)
}

// replayed returns the replayed stop of a session whose input is the one at next.
func replayed(t *testing.T, st *store.Store, session string, next time.Time) Result {
	t.Helper()
	_, rs := replayAll(t, st)
	for _, r := range rs {
		if r.Point.Session == session && r.Point.Next.At.Equal(next) {
			return r
		}
	}
	t.Fatalf("no stop of %s ends at %v", session, next)
	return Result{}
}

// A Claude Code local command such as /login never reaches the model: its user was at the session, and no turn
// opened. The stop before it goes on to the next input, which reads it as it was: a usage limit, a question, a turn
// its hook ended.
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
	// a prompt slash command, and a prompt the store marks as a local command, do open turns
	skill := newTimeline(t, "skill").owner(0, "watch the run", "").say(10*time.Second, "Watching.").
		add(time.Minute, "harness_text", store.ActorHarness, "", "<command-message>loop</command-message> <command-name>/loop</command-name>",
			`{"input_kind":"slash","why":"harness text"}`).
		add(time.Minute, "harness_text", store.ActorHarness, "", "Summarize the digest below as facts.", `{"input_kind":"slash","why":"local command"}`)
	skill.put(st)

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
	points, _ := replayAll(t, st)
	opened := 0
	for _, p := range points {
		if strings.Contains(p.Next.Text, "<command-name>/login") || strings.Contains(p.Next.Text, "<command-name>/effort") {
			t.Fatalf("a local command ended a stop: %+v", p)
		}
		if p.Session == "skill" && p.Next.Kind == "owner" && p.Next.InputKind == "slash" {
			opened++
		}
	}
	if opened != 2 {
		t.Fatalf("prompts that open turns ended %d stops, not 2", opened)
	}
}

// A local command holds a rule for the owner window only when it came before the rule would have acted: by then a
// live run had acted already.
func TestALocalCommandHoldsOnlyARuleThatHadNotActed(t *testing.T) {
	st := testStore(t)
	late := newTimeline(t, "late").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	errAt := late.at
	late.localCommand(30*time.Minute, "login", "").owner(time.Minute, ".", "")
	lateNext := late.at
	late.put(st)
	early := newTimeline(t, "early").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	early.localCommand(5*time.Minute, "model", "")
	command := early.at.Add(-time.Millisecond)
	early.owner(7*time.Minute, ".", "")
	earlyNext := early.at
	early.put(st)

	r := replayed(t, st, "late", lateNext)
	if r.Decision.Rule != RuleTransient || !r.Fired || r.Outcome != Unlabelled || !r.Decision.At.Before(errAt.Add(30*time.Minute)) {
		t.Fatalf("a command after the rule's time: %s %v %q at %v", r.Decision.Rule, r.Fired, r.Outcome, r.Decision.At)
	}
	r = replayed(t, st, "early", earlyNext)
	if r.Decision.Rule != RuleTransient || r.Fired || !r.Point.Stop.OwnerAt.Equal(command) || Why(r) != "transient: the owner came first" {
		t.Fatalf("a command before the rule's time: %s %v owner at %v, %q", r.Decision.Rule, r.Fired, r.Point.Stop.OwnerAt, Why(r))
	}
}

// /exit ends the process: the stop before it ends there, and no stop follows it, so no rule acts on a session that
// is gone.
func TestAnExitEndsTheStop(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "exit").owner(0, "run the suite", "turn_duration").say(10*time.Second, "The suite is running. I'll report when it finishes.")
	stoppedAt := b.at
	b.localCommand(5*time.Minute, "exit", "")
	b.put(st)
	points, rs := replayAll(t, st)
	var last Point
	for _, p := range points {
		if p.Session == "exit" {
			last = p
		}
	}
	if !last.Stop.At.Equal(stoppedAt) || last.Next.InputKind != "exit" {
		t.Fatalf("the session's last stop: at %v, next %+v", last.Stop.At, last.Next)
	}
	for _, r := range rs {
		if r.Point.Session == "exit" && r.Outcome == Left {
			t.Fatalf("a rule acted on an exited session: %+v", r)
		}
	}
}

// Codex records its user's interruption of a turn as a turn_aborted record, or marks it on the record after it:
// either ends the turn, so the stop from the aborted call's result to his next input owes no reply, and it stays
// one stop.
func TestACodexAbortedTurnIsAnInterruption(t *testing.T) {
	st := testStore(t)
	abort := func(b *timelineBuilder) *timelineBuilder {
		return b.add(time.Second, "harness_text", store.ActorHarness, "", "<turn_aborted> The previous turn was interrupted on purpose. Any running unified exec processes may still be running.", `{"why":"developer message"}`)
	}
	mark := func(b *timelineBuilder, after time.Duration) *timelineBuilder {
		return b.add(after, "harness_text", store.ActorHarness, "", "# AGENTS.md instructions", `{"input_kind":"typed","prior":"turn_aborted","why":"harness text"}`)
	}
	type shape struct {
		session          string
		stopAt, nudgedAt time.Time
	}
	var shapes []shape
	for _, name := range []string{"record", "marked", "both"} {
		b := newTimeline(t, name).owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`).
			result(12*time.Second, "exec", "c1", "aborted by user after 12.5s")
		stopAt := b.at
		switch name {
		case "record":
			abort(b).add(20*time.Minute, "compaction", store.ActorHarness, "", "", `{"why":"compacted"}`)
		case "marked":
			mark(b, 10*time.Minute)
		case "both":
			mark(abort(b), 10*time.Minute)
		}
		b.owner(time.Second, ".", "")
		shapes = append(shapes, shape{name, stopAt, b.at})
		b.codex().put(st)
	}
	points, _ := replayAll(t, st)
	for _, c := range shapes {
		r := replayed(t, st, c.session, c.nudgedAt)
		if r.Point.Stop.Working || !r.Point.Stop.At.Equal(c.stopAt) || Why(r) == owedReply {
			t.Fatalf("%s: the stop before the owner's nudge: working %v, at %v (the aborted result at %v), %q", c.session,
				r.Point.Stop.Working, r.Point.Stop.At, c.stopAt, Why(r))
		}
		n := 0
		for _, p := range points {
			if p.Session == c.session && p.Stop.At.After(c.stopAt.Add(-time.Millisecond)) && p.Next.Kind != "none" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: the aborted turn's stop is %d stops", c.session, n)
		}
	}
}
