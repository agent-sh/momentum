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

// caveatFirst is a local command as Claude Code may also write it: the caveat first, carrying the system record before
// it, then the command's echo.
func (b *timelineBuilder) caveatFirst(after time.Duration, name, prior string) *timelineBuilder {
	b.add(after, "harness_text", store.ActorHarness, "", "<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request.</local-command-caveat>",
		`{"prior":"`+prior+`","why":"meta line"}`)
	return b.add(time.Millisecond, "harness_text", store.ActorHarness, "", "<command-name>/"+name+"</command-name> <command-message>"+name+"</command-message> <command-args></command-args>",
		`{"input_kind":"slash","why":"local command"}`)
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
	// the same with the caveat first, and a session whose last record is a local command after its hook ended the turn
	caveat := newTimeline(t, "caveat").owner(0, "lint it", "").call(time.Second, "Bash", "t1", `{"command":"make lint"}`).
		result(time.Second, "Bash", "t1", "ok")
	caveat.caveatFirst(5*time.Minute, "effort", "stop_hook_summary").owner(time.Minute, "continue", "")
	caveatNext := caveat.at
	caveat.put(st)
	ends := newTimeline(t, "ends").owner(0, "lint it", "").call(time.Second, "Bash", "t1", `{"command":"make lint"}`).
		result(time.Second, "Bash", "t1", "ok").localCommand(5*time.Minute, "effort", "stop_hook_summary")
	ends.put(st)
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
	if r := replayed(t, st, "caveat", caveatNext); r.Point.Stop.Working || Why(r) == owedReply {
		t.Fatalf("a turn its hook ended, the caveat first: working %v, %q", r.Point.Stop.Working, Why(r))
	}
	if r := replayed(t, st, "ends", time.Time{}); r.Point.Stop.Working || r.Point.Next.Kind != "none" {
		t.Fatalf("the last stop, its turn ended by its hook: working %v, next %+v", r.Point.Stop.Working, r.Point.Next)
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
	early.owner(12*time.Minute, ".", "")
	earlyNext := early.at
	early.put(st)
	// a restart step the owner took with /login, after the rule's time: measured at the command, which ends no stop
	step := newTimeline(t, "step").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	step.localCommand(20*time.Minute, "login", "")
	stepAt := step.at.Add(-time.Millisecond)
	step.owner(time.Minute, "deploy it again", "")
	stepNext := step.at
	step.put(st)

	r := replayed(t, st, "late", lateNext)
	if r.Decision.Rule != RuleTransient || !r.Fired || r.Outcome != Unlabelled || !r.Decision.At.Before(errAt.Add(30*time.Minute)) {
		t.Fatalf("a command after the rule's time: %s %v %q at %v", r.Decision.Rule, r.Fired, r.Outcome, r.Decision.At)
	}
	r = replayed(t, st, "early", earlyNext)
	if r.Decision.Rule != RuleTransient || r.Decision.Verdict != Hold || !r.Decision.At.Equal(command.Add(DefaultParams.OwnerWindow)) || r.Fired ||
		!r.Point.Stop.OwnerAt.Equal(command) || Why(r) != "transient: the owner came first" {
		t.Fatalf("a command before the rule's time: %s %s until %v, fired %v, owner at %v, %q", r.Decision.Rule, r.Decision.Verdict, r.Decision.At,
			r.Fired, r.Point.Stop.OwnerAt, Why(r))
	}
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Label{{Session: "step", At: stepAt, Kind: "restart"}}
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Session == "step" && r.Point.Next.At.Equal(stepNext) {
			if r.Label == nil || r.Outcome != Covered || r.Earlier != stepAt.Sub(r.Decision.At) {
				t.Fatalf("a restart step on a local command: label %v, %q, %v earlier", r.Label, r.Outcome, r.Earlier)
			}
			return
		}
	}
	t.Fatal("no stop ends on the step's next input")
}

// /exit ends the process: the stop before it ends there, reading the turn's end recorded on the command, and the
// stop after it is one no rule acts on, so no rule acts on a session that is gone.
func TestAnExitEndsTheStop(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "exit").owner(0, "run the suite", "turn_duration").say(10*time.Second, "The suite is running. I'll report when it finishes.")
	stoppedAt := b.at
	b.localCommand(5*time.Minute, "exit", "")
	b.put(st)
	hook := newTimeline(t, "exit-hook").owner(0, "lint it", "").call(time.Second, "Bash", "t1", `{"command":"make lint"}`).
		result(time.Second, "Bash", "t1", "ok").localCommand(5*time.Minute, "exit", "stop_hook_summary")
	hook.put(st)
	_, rs := replayAll(t, st)
	var stops []Result
	for _, r := range rs {
		if r.Point.Session == "exit" {
			stops = append(stops, r)
		}
		if r.Point.Session == "exit-hook" && r.Point.Next.InputKind == "exit" && r.Point.Stop.Working {
			t.Fatalf("a turn its hook ended reads as working at its exit: %+v", r.Point.Stop)
		}
		if r.Outcome == Left {
			t.Fatalf("a rule acted on an exited session: %+v", r)
		}
	}
	if len(stops) != 2 || !stops[0].Point.Stop.At.Equal(stoppedAt) || stops[0].Point.Next.InputKind != "exit" ||
		!stops[1].Point.Stop.Exited || stops[1].Decision.Verdict != Skip || Why(stops[1]) != "skipped: its process exited" {
		t.Fatalf("the session's stops: %+v", stops)
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
	for _, name := range []string{"record", "marked", "both", "silent"} {
		b := newTimeline(t, name).owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`)
		if name != "silent" { // a call aborted before it wrote anything has no result
			b.result(12*time.Second, "exec", "c1", "aborted by user after 12.5s")
		}
		stopAt := b.at
		switch name {
		case "silent":
			abort(b)
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
		if r.Point.Stop.Working || r.Point.Stop.Busy || !r.Point.Stop.At.Equal(c.stopAt) || Why(r) == owedReply {
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
