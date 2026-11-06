package supervise

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
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
	// an echo that also carries a reminder is still a local command, as the live tracker reads it
	reminded := newTimeline(t, "reminded").owner(0, "plan the cleanup", "").say(10*time.Second, "Should I start on items 1 to 4?").
		add(10*time.Minute, "harness_text", store.ActorHarness, "", "<command-name>/model</command-name> <command-args></command-args> <system-reminder>a plugin reloaded</system-reminder>",
			`{"input_kind":"slash","why":"local command"}`).
		owner(2*time.Second, "resume", "")
	remindedNext := reminded.at
	reminded.put(st)
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
	if r := replayed(t, st, "reminded", remindedNext); r.Point.Stop.Working || Why(r) != "its last words ask the owner" {
		t.Fatalf("an echo with a reminder: working %v, %q", r.Point.Stop.Working, Why(r))
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
	step.owner(time.Minute, "continue", "")
	stepNext := step.at
	step.put(st)
	tail := newTimeline(t, "tail").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	tail.localCommand(20*time.Minute, "login", "")
	tailAt := tail.at.Add(-time.Millisecond)
	tail.put(st)

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
	labels := []Label{{Session: "step", At: stepAt, Kind: "restart"}, {Session: "step", At: stepNext, Kind: "nudge"},
		{Session: "tail", At: tailAt, Kind: "restart"}}
	seen := 0
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		switch {
		case r.Point.Session == "step" && r.Point.Next.At.Equal(stepNext):
			// the earliest step is measured, the later one counts with it
			if r.Label != &labels[0] || !r.StepAt.Equal(stepAt) || r.Outcome != Covered || r.Earlier != stepAt.Sub(r.Decision.At) ||
				len(r.Also) != 1 || r.Also[0] != &labels[1] {
				t.Fatalf("two steps at one stop: label %v at %v, %q, %v earlier, also %v", r.Label, r.StepAt, r.Outcome, r.Earlier, r.Also)
			}
			seen++
		case r.Point.Session == "tail" && r.Point.Next.Kind == "none":
			if r.Label != &labels[2] || !r.StepAt.Equal(tailAt) || r.Outcome != Covered {
				t.Fatalf("a step on a local command at the last stop: label %v at %v, %q", r.Label, r.StepAt, r.Outcome)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("%d of the labelled stops replayed", seen)
	}
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
		result(time.Second, "Bash", "t1", "ok").localCommand(5*time.Minute, "quit", "stop_hook_summary")
	hook.put(st)
	_, rs := replayAll(t, st)
	var stops []Result
	quit := 0
	for _, r := range rs {
		if r.Point.Session == "exit" {
			stops = append(stops, r)
		}
		if r.Point.Session == "exit-hook" && r.Point.Next.InputKind == "exit" {
			quit++
			if r.Point.Stop.Working {
				t.Fatalf("a turn its hook ended reads as working at its exit: %+v", r.Point.Stop)
			}
		}
		if r.Outcome == Left {
			t.Fatalf("a rule acted on an exited session: %+v", r)
		}
	}
	if quit != 1 {
		t.Fatalf("/quit ended %d stops", quit)
	}
	if len(stops) != 2 || !stops[0].Point.Stop.At.Equal(stoppedAt) || stops[0].Point.Next.InputKind != "exit" ||
		!stops[1].Point.Stop.Exited || stops[1].Decision.Verdict != Skip || Why(stops[1]) != "skipped: its process exited" {
		t.Fatalf("the session's stops: %+v", stops)
	}
}

// Codex records its user's interruption of a turn as a turn_aborted record, or marks it on the record after it:
// either ends the turn, so the stop to his next input owes no reply, and it stays one stop. It starts at the abort,
// as Codex's live tracker keeps the turn open until then: at its own record, else at the first record marked.
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
	for _, name := range []string{"record", "marked", "both", "silent", "on-input"} {
		b := newTimeline(t, name).owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`)
		if name != "silent" && name != "on-input" { // a call aborted before it wrote anything has no result
			b.result(12*time.Second, "exec", "c1", "aborted by user after 12.5s")
		}
		var stopAt time.Time
		switch name {
		case "silent":
			stopAt = abort(b).at
		case "record":
			stopAt = abort(b).at
			b.add(20*time.Minute, "compaction", store.ActorHarness, "", "", `{"why":"compacted"}`)
		case "marked":
			stopAt = mark(b, 10*time.Minute).at
		case "both":
			stopAt = abort(b).at
			mark(b, 10*time.Minute)
		}
		if name == "on-input" {
			stopAt = b.owner(10*time.Minute, ".", "turn_aborted").at // the abort marked on the owner's own text
		} else {
			b.owner(time.Second, ".", "")
		}
		shapes = append(shapes, shape{name, stopAt, b.at})
		b.codex().put(st)
	}
	points, _ := replayAll(t, st)
	for _, c := range shapes {
		r := replayed(t, st, c.session, c.nudgedAt)
		if r.Point.Stop.Working || r.Point.Stop.Busy || !r.Point.Stop.At.Equal(c.stopAt) || Why(r) == owedReply || !r.Point.Stop.OwnerAt.Equal(t0) {
			t.Fatalf("%s: the stop before the owner's nudge: working %v, at %v (the abort at %v), %q", c.session,
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

// storeCut is a record's text as the store keeps it: 240 bytes on one line, a cut one marked with "…"
// (internal/source, cutHead).
func storeCut(s string) string {
	cut := len(s) > 240
	if cut {
		s = s[:240]
	}
	s = strings.Join(strings.Fields(s), " ")
	if cut {
		s += "…"
	}
	return s
}

// Claude Code's caveat as it writes it before a local command's output, longer than the store keeps.
const caveat = "<local-command-caveat>Caveat: The messages below were generated by the user while running local commands. DO NOT respond to these messages or otherwise consider them in your response unless the user explicitly asks you to.</local-command-caveat>"

// A local command's echo the store cut inside a reminder or a caveat is still a local command: read whole from its
// transcript while it is there, else by the tag the cut came inside. A prompt that reads like one when cut, but has
// text of its own in its record, still opens a turn.
func TestACutLocalCommandEchoIsStillOne(t *testing.T) {
	st := testStore(t)
	echo := "<command-name>/model</command-name> <command-message>model</command-message> <command-args></command-args>"
	reminder := "<system-reminder>" + strings.Repeat("A plugin reloaded and its tools changed. ", 8) + "</system-reminder>"
	local := `{"input_kind":"slash","why":"local command"}`
	asked := map[string]time.Time{}
	for name, text := range map[string]string{"reminder": echo + " " + reminder, "caveat-first": caveat + " " + echo} {
		b := newTimeline(t, name).owner(0, "plan the cleanup", "").say(10*time.Second, "Should I start on items 1 to 4?")
		if s := storeCut(text); !strings.HasSuffix(s, "…") {
			t.Fatalf("%s is not cut: %q", name, s)
		}
		b.add(10*time.Minute, "harness_text", store.ActorHarness, "", storeCut(text), local).owner(2*time.Second, "resume", "")
		asked[name] = b.at
		b.put(st)
	}
	whole := newRecorded(t, "whole").owner(0, "plan the cleanup").say(10*time.Second, "Should I start on items 1 to 4?")
	full := echo + " " + reminder
	whole.event(10*time.Minute, map[string]any{"type": "user", "message": map[string]any{"content": full}}, "harness_text", store.ActorHarness, "",
		storeCut(full), local).owner(2*time.Second, "resume")
	asked["whole"] = whole.at
	whole.put(st)
	prompt := newRecorded(t, "prompt").owner(0, "watch the run").say(10*time.Second, "Watching.")
	text := "<command-name>/review</command-name> <command-args></command-args> " + reminder + " Review the open pull request."
	prompt.event(time.Minute, map[string]any{"type": "user", "message": map[string]any{"content": text}}, "harness_text", store.ActorHarness, "",
		storeCut(text), local)
	prompted := prompt.at
	prompt.put(st)

	for name, next := range asked {
		if r := replayed(t, st, name, next); r.Point.Stop.Working || Why(r) != "its last words ask the owner" {
			t.Errorf("%s: the stop before the resume: working %v, %q", name, r.Point.Stop.Working, Why(r))
		}
	}
	if r := replayed(t, st, "prompt", prompted); r.Point.Next.Kind != "owner" || r.Point.Next.InputKind != "slash" {
		t.Errorf("a cut prompt with text of its own ended no stop: %+v", r.Point.Next)
	}
}

// A local command after /exit shows the session open again: the stop after it is no longer the exited one, and
// its jobs run from it. A job's runs due while the process was gone are not missed.
func TestAReopeningAfterAnExitRestartsTheSession(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	b := newTimeline(t, "model").owner(0, "run the suite", "turn_duration").say(10*time.Second, "Running.")
	b.localCommand(time.Minute, "exit", "").localCommand(40*time.Minute, "model", "")
	b.put(st)
	if r := replayed(t, st, "model", time.Time{}); r.Point.Stop.Exited || Why(r) == "skipped: its process exited" {
		t.Fatalf("the last stop after a reopening /model: exited %v, %q", r.Point.Stop.Exited, Why(r))
	}
	st = testStore(t)
	j := newTimeline(t, "job").owner(0, "check the deploy every 10 minutes", "turn_duration").
		call(time.Second, "CronCreate", "tc1", `{"cron":"*/10 * * * *","prompt":"check the deploy","durable":true}`).
		result(time.Second, "CronCreate", "tc1", "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10. Persisted to .claude/scheduled_tasks.json.").
		say(time.Second, "Checking.")
	j.localCommand(time.Minute, "exit", "").owner(40*time.Minute, "continue", "")
	reopened := j.at
	j.say(time.Second, "Back.").owner(time.Hour, "status?", "turn_duration")
	j.put(st)
	acted := overdueActs(t, st, t0.Add(3*time.Hour))
	if len(acted) == 0 {
		t.Fatal("no run after the reopening was missed")
	}
	for _, r := range acted {
		if !r.Point.Stop.WakeDue.After(reopened) {
			t.Fatalf("a run due while the process was gone read as missed: due %v, reopened %v", r.Point.Stop.WakeDue, reopened)
		}
	}
}

// After a Codex abort the model owes nothing: a record of the session later is no long wait on its reply.
func TestACodexAbortEndsTheWaitOnTheModel(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "aborted").owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`).
		result(12*time.Second, "exec", "c1", "aborted by user after 12.5s").
		add(time.Second, "harness_text", store.ActorHarness, "", "<turn_aborted> The previous turn was interrupted on purpose.", `{"why":"developer message"}`).
		say(2*time.Minute, "The checks passed.")
	b.codex().put(st)
	points, _ := replayAll(t, st)
	for _, p := range points {
		if p.Session == "aborted" && p.Waiting {
			t.Fatalf("an aborted turn read as a wait on the model: %+v", p)
		}
	}
}

// A step labelled on a local command before the rule's time is measured against the decision it did not move: the
// owner came first by the true margin, not by the owner window the command would start.
func TestALabelledLocalCommandIsMeasuredAgainstTheRuleItDidNotMove(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "early-step").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	b.localCommand(3*time.Minute, "login", "")
	stepAt := b.at.Add(-time.Millisecond)
	b.owner(time.Minute, "continue", "")
	next := b.at
	b.put(st)
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	unlabelled := replayed(t, st, "early-step", next)
	labels := []Label{{Session: "early-step", At: stepAt, Kind: "restart"}}
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Session != "early-step" || !r.Point.Next.At.Equal(next) {
			continue
		}
		rule := t0.Add(DefaultParams.OwnerWindow) // held by the owner's own prompt, not by the command
		if r.Label == nil || r.Outcome != OwnerFirst || !r.Measured.At.Equal(rule) || r.StepAt.Sub(r.Measured.At) != stepAt.Sub(rule) {
			t.Fatalf("the labelled step: %v %q, rule at %v, gap %v", r.Label, r.Outcome, r.Measured.At, r.StepAt.Sub(r.Measured.At))
		}
		if !r.Decision.At.Equal(stepAt.Add(DefaultParams.OwnerWindow)) {
			t.Fatalf("a live run would hold the rule from the command: %v", r.Decision.At)
		}
		if !unlabelled.Decision.At.Equal(stepAt.Add(DefaultParams.OwnerWindow)) {
			t.Fatalf("unlabelled, the command holds the rule: %v", unlabelled.Decision.At)
		}
		return
	}
	t.Fatal("no stop ends on the step's next input")
}

// A /exit the store cut inside the caveat before it is still an exit, by its record read whole: the stop ends there
// and the stop after it is the exited one.
func TestACutExitIsStillAnExit(t *testing.T) {
	st := testStore(t)
	r := newRecorded(t, "cut-exit").owner(0, "run the suite").say(10*time.Second, "The suite is running. I'll report when it finishes.")
	text := caveat + " <command-name>/exit</command-name> <command-message>exit</command-message> <command-args></command-args>"
	r.event(5*time.Minute, map[string]any{"type": "user", "message": map[string]any{"content": text}}, "harness_text", store.ActorHarness, "",
		storeCut(text), `{"input_kind":"slash","why":"local command"}`)
	r.put(st)
	if strings.Contains(storeCut(text), "/exit") {
		t.Fatal("the cut kept the command's name")
	}
	_, rs := replayAll(t, st)
	var stops []Result
	for _, res := range rs {
		if res.Point.Session == "cut-exit" {
			stops = append(stops, res)
		}
	}
	if len(stops) != 2 || stops[0].Point.Next.InputKind != "exit" || !stops[1].Point.Stop.Exited || stops[1].Fired {
		t.Fatalf("the cut exit's stops: %+v", stops)
	}
}

// After /exit no rule acts until the session is opened again, and a reopening local command counts the rules
// from itself: a reopening before the exit is forgotten, and a job's runs due while the process was gone are not
// missed.
func TestRulesCountFromAReopeningCommand(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	b := newTimeline(t, "orphan").owner(0, "run the soak", "turn_duration").say(10*time.Second, "Running.")
	b.add(time.Minute, "harness_text", store.ActorHarness, "", "<task-notification><task-id>__orphan_summary__</task-id> didn't finish before the previous session ended</task-notification>", `{"why":"meta line"}`)
	b.localCommand(time.Minute, "exit", "").localCommand(40*time.Minute, "model", "")
	reopened := b.at.Add(-time.Millisecond)
	b.put(st)
	_, rs := replayAll(t, st)
	var last Result
	for _, r := range rs {
		if r.Point.Session == "orphan" && r.Fired && r.Decision.At.Before(reopened) {
			t.Fatalf("%s acted at %v, before the reopening at %v", r.Decision.Rule, r.Decision.At, reopened)
		}
		if r.Point.Session == "orphan" {
			last = r
		}
	}
	if !last.Point.Stop.At.Equal(reopened) {
		t.Fatalf("the stop after the reopening counts from %v, not from the reopening at %v", last.Point.Stop.At, reopened)
	}
	st = testStore(t)
	j := newTimeline(t, "job").owner(0, "check the deploy every 10 minutes", "turn_duration").
		call(time.Second, "CronCreate", "tc1", `{"cron":"*/10 * * * *","prompt":"check the deploy","durable":true}`).
		result(time.Second, "CronCreate", "tc1", "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10. Persisted to .claude/scheduled_tasks.json.").
		say(time.Second, "Checking.")
	j.localCommand(time.Minute, "exit", "").localCommand(40*time.Minute, "model", "")
	model := j.at.Add(-time.Millisecond)
	j.owner(10*time.Minute, "status?", "").say(time.Second, "Back.").owner(time.Hour, "and now?", "turn_duration")
	j.put(st)
	acted := overdueActs(t, st, t0.Add(3*time.Hour))
	if len(acted) == 0 {
		t.Fatal("no run after the reopening was missed")
	}
	for _, r := range acted {
		if !r.Point.Stop.WakeDue.After(model) {
			t.Fatalf("a run due while the process was gone read as missed: due %v, reopened %v", r.Point.Stop.WakeDue, model)
		}
	}
}

// watchNotice is Claude Code's notice that a session's artifact watches stopped, as it sends it to the model: a task
// notification with a type and no task, longer than the store keeps.
const watchNotice = "<task-notification>\n<task-type>artifact-watch-lifecycle</task-type>\n<summary>Artifact watches stopped</summary>\n" +
	"<event>This session no longer watches the artifacts it published, because the signed-in account changed. Publish an artifact again " +
	"to watch it. Nothing here needs a reply.</event>\n</task-notification>"

// weeklyLimit is the error a turn stops on when the account's weekly usage is spent.
const weeklyLimit = "You've hit your weekly limit · resets Mar 9, 9am (Europe/Lisbon)"

func (r *recorded) apiError(after time.Duration, text string) *recorded {
	return r.event(after, map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": "rate_limit",
		"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": text}}}},
		"harness_text", store.ActorHarness, "", text, `{"is_error":true,"why":"api error"}`)
}

func (r *recorded) login(after time.Duration) *recorded {
	echo := "<command-name>/login</command-name> <command-message>login</command-message> <command-args></command-args>"
	return r.event(after, map[string]any{"type": "user", "message": map[string]any{"content": echo}}, "harness_text", store.ActorHarness, "",
		echo, `{"input_kind":"slash","why":"local command"}`)
}

// queued is a notification Claude Code queued at queuedAt and delivered after the records before it: its attachment
// sits where it was delivered and carries the time it was queued, as the store keeps it.
func (r *recorded) queued(after time.Duration, queuedAt time.Time, text string) *recorded {
	off, _ := r.line(after, map[string]any{"type": "attachment", "timestamp": queuedAt.Format(time.RFC3339Nano),
		"attachment": map[string]any{"type": "queued_command", "commandMode": "task-notification", "prompt": text}})
	r.evs = append(r.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%d", r.session, off), TS: queuedAt, Session: r.session,
		Harness: "claude", Actor: store.ActorHarness, Kind: "harness_text", Summary: storeCut(text), Meta: `{"why":"commandMode task-notification"}`,
		Ref: fmt.Sprintf("%s@%d#0", r.path, off)})
	return r
}

// Claude Code's notice that the artifact watches stopped is a notification the model answers: while the session is
// idle it opens a turn, so the stop before it ends there, and delivered alone after a usage limit it ends the limit's
// stop.
func TestAnAnsweredNoticeIsAnInput(t *testing.T) {
	st := testStore(t)
	idle := newTimeline(t, "idle").owner(0, "run the suite", "").say(10*time.Second, "The suite is running. I'll report when it finishes.")
	promised := idle.at
	idle.add(10*time.Second, "harness_text", store.ActorHarness, "", storeCut(watchNotice), `{"why":"origin task-notification"}`)
	noticed := idle.at
	idle.say(5*time.Second, "Noted: the watches stopped.").put(st)
	limit := newRecorded(t, "limit").owner(0, "run the soak").say(10*time.Second, "Running the soak.").apiError(time.Minute, weeklyLimit)
	stoppedAt := limit.at
	limit.queued(5*time.Minute, stoppedAt.Add(5*time.Minute), watchNotice)
	delivered := limit.at
	limit.login(15*time.Minute).say(5*time.Second, "Noted: the watches stopped.").put(st)

	_, rs := replayAll(t, st)
	ended := func(session string, next time.Time) (Result, bool) {
		for _, r := range rs {
			if r.Point.Session == session && r.Point.Next.At.Equal(next) {
				return r, true
			}
		}
		return Result{}, false
	}
	if r, ok := ended("idle", noticed); !ok || !r.Point.Stop.At.Equal(promised) || r.Point.Next.Kind != "self" {
		t.Errorf("the idle notice ended no stop at the promise (%v): found %v, %+v", promised, ok, r.Point)
	}
	r, ok := ended("limit", delivered)
	if s := r.Point.Stop; !ok || !s.At.Equal(stoppedAt) || s.Error == "" || r.Point.Next.Kind != "self" || r.Decision.Rule != RuleAccess {
		t.Errorf("the notice after the limit ended no access stop (stopped at %v): found %v, at %v, error %q, next %+v, %s", stoppedAt, ok,
			s.At, s.Error, r.Point.Next, r.Decision.Rule)
	}
}

// A queued command's attachment carries the time Claude Code queued it, before the records it was delivered after:
// it is read where it sits, and delivered with an input, before anything of that input's turn, it is part of that
// input. A notification queued after a usage limit and delivered with the owner's "resume" after /login leaves the
// limit's stop to the access rule, a task's notification as the watches' notice, with its transcript or without.
func TestAQueuedCommandIsReadWhereItSits(t *testing.T) {
	st := testStore(t)
	type shape struct {
		stoppedAt, login, resume time.Time
	}
	shapes := map[string]shape{}
	task := "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>toolu_1</tool-use-id>\n<status>completed</status>\n<summary>Background command \"soak\" completed (exit code 0)</summary>\n</task-notification>"
	for name, text := range map[string]string{"notice": watchNotice, "task": task} {
		for _, gone := range []bool{false, true} {
			session := name
			if gone {
				session += "-gone"
			}
			b := newRecorded(t, session).owner(0, "run the soak").say(10*time.Second, "Running the soak.").apiError(time.Minute, weeklyLimit)
			stoppedAt := b.at
			b.login(20 * time.Minute)
			login := b.at
			b.owner(2*time.Second, "resume")
			resume := b.at
			b.queued(0, stoppedAt.Add(5*time.Minute), text).say(10*time.Second, "Resuming the soak.")
			if gone {
				if err := os.Remove(b.path); err != nil {
					t.Fatal(err)
				}
			}
			b.put(st)
			shapes[session] = shape{stoppedAt, login, resume}
		}
	}
	points, _ := replayAll(t, st)
	for _, p := range points {
		if strings.Contains(p.Next.Text, "<task-notification>") {
			t.Errorf("%s: the queued notification ended a stop: %+v", p.Session, p.Next)
		}
	}
	for session, c := range shapes {
		r := replayed(t, st, session, c.resume)
		if s := r.Point.Stop; s.Working || !s.At.Equal(c.stoppedAt) || s.Error == "" || !s.OwnerAt.Equal(c.login) {
			t.Errorf("%s: the stop before the resume: working %v, at %v (stopped at %v), error %q, owner at %v (login at %v)", session,
				s.Working, s.At, c.stoppedAt, s.Error, s.OwnerAt, c.login)
		}
		if r.Decision.Rule != RuleAccess || Why(r) == owedReply {
			t.Errorf("%s: the stop before the resume: %s, %q", session, r.Decision.Rule, Why(r))
		}
	}
}

// A background command that finishes at once is queued as done before the result that launched it is recorded:
// read where it was delivered, after that result, its notification closes the task, so the session is not left
// with work in flight.
func TestANotificationQueuedBeforeItsLaunchClosesItsTask(t *testing.T) {
	st := testStore(t)
	b := newRecorded(t, "quick").owner(0, "build it").call(time.Second, "t1")
	b.event(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1"}}}},
		"tool_result", store.ActorTool, "Bash", "Command running in background with ID: b9. Output is being written to: /work/b9.output", `{"tool_id":"t1"}`)
	launched := b.at
	b.queued(0, launched.Add(-23*time.Millisecond), "<task-notification>\n<task-id>b9</task-id>\n<status>completed</status>\n<summary>Background command \"build\" completed (exit code 0)</summary>\n</task-notification>").
		say(5*time.Second, "The build passed.").owner(10*time.Minute, "status?")
	asked := b.at
	b.put(st)
	if r := replayed(t, st, "quick", asked); r.Point.Stop.Tasks != 0 || r.Point.Stop.Phase == sessions.Background {
		t.Fatalf("the stop after the build: %d tasks, phase %s", r.Point.Stop.Tasks, r.Point.Stop.Phase)
	}
}

// /exit ends the process and the turn it ran: a "No response requested." Claude Code writes as it opens the session
// again is no sign that it ended mid-task, so the reopened rule leaves a deliberate exit alone. The same record after
// a process that died with its turn open is one.
func TestAnExitEndsTheTurnItsProcessRan(t *testing.T) {
	st := testStore(t)
	for _, name := range []string{"exited", "died"} {
		b := newTimeline(t, name).owner(0, "run the suite", "").call(time.Second, "Bash", "t1", `{"command":"make test"}`).
			result(time.Second, "Bash", "t1", "ok") // the model owes its reply
		if name == "exited" {
			b.localCommand(time.Minute, "exit", "")
		}
		b.add(40*time.Minute, "harness_text", store.ActorHarness, "", "No response requested.", `{"why":"synthetic message"}`)
		b.put(st)
	}
	if r := replayed(t, st, "exited", time.Time{}); !r.Point.Stop.ResumedAt.IsZero() || r.Decision.Rule == RuleReopened {
		t.Fatalf("a session reopened after /exit: resumed at %v, %s %s, %q", r.Point.Stop.ResumedAt, r.Decision.Rule, r.Decision.Verdict, Why(r))
	}
	if r := replayed(t, st, "died", time.Time{}); r.Point.Stop.ResumedAt.IsZero() || r.Decision.Rule != RuleReopened {
		t.Fatalf("a session reopened after its process died mid-task: resumed at %v, %s", r.Point.Stop.ResumedAt, r.Decision.Rule)
	}
}

// Nothing runs while the process is gone: the stop an /exit ends has no schedule, so the owner's input that reopens
// it is no nudge to a session that would have gone on by itself.
func TestAnExitedStopHasNoSchedule(t *testing.T) {
	inUTCHere(t)
	st := testStore(t)
	j := newTimeline(t, "gone").owner(0, "check the deploy every 10 minutes", "turn_duration").
		call(time.Second, "CronCreate", "tc1", `{"cron":"*/10 * * * *","prompt":"check the deploy","durable":true}`).
		result(time.Second, "CronCreate", "tc1", "Scheduled recurring job j1 (Every 10 minutes). Next run: 10:10. Persisted to .claude/scheduled_tasks.json.").
		say(time.Second, "Checking.")
	j.localCommand(time.Minute, "exit", "").owner(40*time.Minute, "continue", "")
	next := j.at
	j.put(st)
	r := replayed(t, st, "gone", next)
	s := r.Point.Stop
	if !s.Exited || s.Loop != "" || len(s.Runs) != 0 || phaseOf(s) == sessions.Scheduled {
		t.Fatalf("the exited stop: exited %v, loop %q, %d runs, phase %v", s.Exited, s.Loop, len(s.Runs), phaseOf(s))
	}
}

// A Codex turn that hung, then was aborted, ran until the abort, as Codex's live tracker keeps it open: rule waits
// count from the abort, not from its last record, so a promise before the hang gets no act while it ran.
func TestAHungCodexTurnRunsUntilItsAbort(t *testing.T) {
	st := testStore(t)
	aborts := map[string]time.Time{}
	nexts := map[string]time.Time{}
	for _, name := range []string{"hung-record", "hung-marked"} {
		b := newTimeline(t, name).owner(0, "run the suite", "").say(5*time.Second, "I'll run the suite and report when it finishes.").
			call(time.Second, "exec", "c1", `{"cmd":"make test"}`)
		if name == "hung-record" {
			b.add(30*time.Minute, "harness_text", store.ActorHarness, "", "<turn_aborted> The user interrupted the previous turn on purpose.", `{"why":"developer message"}`)
		} else {
			b.add(30*time.Minute, "harness_text", store.ActorHarness, "", "# AGENTS.md instructions", `{"input_kind":"typed","prior":"turn_aborted","why":"harness text"}`)
		}
		aborts[name] = b.at
		b.owner(time.Minute, ".", "")
		nexts[name] = b.at
		b.codex().put(st)
	}
	for name, next := range nexts {
		r := replayed(t, st, name, next)
		if !r.Point.Stop.At.Equal(aborts[name]) || r.Fired || !r.Decision.At.IsZero() && r.Decision.At.Before(aborts[name]) {
			t.Errorf("%s: the stop at %v (the abort at %v), %s at %v, fired %v", name, r.Point.Stop.At, aborts[name], r.Decision.Rule, r.Decision.At, r.Fired)
		}
	}
}

// The first input after a local command reopened an exited session is the owner coming back, not a nudge: the stop
// it ends is marked reopened, while rules count from the command.
func TestTheInputAfterAReopeningCommandIsMarked(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "back").owner(0, "run the suite", "turn_duration").say(10*time.Second, "Running.")
	b.localCommand(time.Minute, "exit", "").localCommand(40*time.Minute, "model", "")
	model := b.at.Add(-time.Millisecond)
	b.owner(time.Minute, "continue", "")
	next := b.at
	b.say(time.Second, "Continuing.").owner(time.Hour, "continue", "")
	later := b.at
	b.put(st)
	if r := replayed(t, st, "back", next); !r.Point.Stop.Reopened || r.Point.Stop.Exited || !r.Point.Stop.At.Equal(model) {
		t.Fatalf("the stop the reopening ends: reopened %v, exited %v, at %v (the command at %v)", r.Point.Stop.Reopened, r.Point.Stop.Exited, r.Point.Stop.At, model)
	}
	if r := replayed(t, st, "back", later); r.Point.Stop.Reopened {
		t.Fatal("a later stop reads as reopened")
	}
}

// Each labelled step takes its nearest free label, whatever order the labels came in.
func TestAStepTakesItsNearestLabel(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "near").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	b.localCommand(20*time.Minute, "login", "")
	login := b.at.Add(-time.Millisecond)
	b.owner(2*time.Second, "continue", "")
	next := b.at
	b.put(st)
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Label{{Session: "near", At: next.Add(-100 * time.Millisecond), Kind: "nudge"}, {Session: "near", At: login, Kind: "restart"}}
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Session == "near" && r.Point.Next.At.Equal(next) {
			if r.Label != &labels[1] || len(r.Also) != 1 || r.Also[0] != &labels[0] {
				t.Fatalf("the steps' labels: %v, also %v", r.Label, r.Also)
			}
			return
		}
	}
	t.Fatal("no stop ends on the step's next input")
}

// Whether a rule would have acted is read with every earlier command's hold, as a live run would go, while a
// labelled step is still measured against the decision it did not move.
func TestFiredCountsEveryEarlierHold(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "held").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	b.localCommand(3*time.Minute, "login", "")
	login := b.at.Add(-time.Millisecond)
	b.owner(12*time.Minute, "continue", "") // after the rule's own time, before the command's hold ends
	next := b.at
	b.put(st)
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Label{{Session: "held", At: login, Kind: "restart"}}
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Session == "held" && r.Point.Next.At.Equal(next) {
			if r.Fired || !r.Decision.At.Equal(login.Add(DefaultParams.OwnerWindow)) || !r.Measured.At.Equal(t0.Add(DefaultParams.OwnerWindow)) || r.Outcome != OwnerFirst {
				t.Fatalf("fired %v, decision at %v, measured at %v, %q", r.Fired, r.Decision.At, r.Measured.At, r.Outcome)
			}
			return
		}
	}
	t.Fatal("no stop ends on the step's next input")
}

// A local command during a wait on the model answers its request from then on, as the live tracker reads the
// echo: before it, the request still stands. The stalled rule acts when its time came before the command, and has
// nothing to act on when the command came first.
func TestALocalCommandAnswersTheRequestOnlyFromThen(t *testing.T) {
	st := testStore(t)
	local := `{"input_kind":"slash","why":"local command"}`
	echo := "<command-name>/model</command-name> <command-message>model</command-message> <command-args></command-args>"
	nexts := map[string]time.Time{}
	for name, after := range map[string]time.Duration{"late": 45 * time.Minute, "early": 20 * time.Minute} {
		r := newRecorded(t, name).owner(0, "prove it").request(time.Second)
		r.event(after, map[string]any{"type": "user", "message": map[string]any{"content": echo}}, "harness_text", store.ActorHarness, "", echo, local)
		r.owner(time.Minute, "continue")
		nexts[name] = r.at
		r.put(st)
	}
	late := replayed(t, st, "late", nexts["late"])
	if late.Point.Stop.RequestAt.IsZero() || late.Decision.Rule != RuleStalled || !late.Fired {
		t.Fatalf("a command after the stalled rule's time: request at %v, %s, fired %v, %q", late.Point.Stop.RequestAt, late.Decision.Rule, late.Fired, Why(late))
	}
	if early := replayed(t, st, "early", nexts["early"]); early.Decision.Rule == RuleStalled || early.Fired {
		t.Fatalf("a command before the stalled rule's time: %s %s at %v, fired %v", early.Decision.Rule, early.Decision.Verdict, early.Decision.At, early.Fired)
	}
}

// One record of a local command with several text blocks is several events: a block that is only a reminder or a
// caveat opens no turn, and a cut block is judged by its own text, so an /exit's reminder is no second exit.
func TestALocalCommandRecordsBlocksAreJudgedAlone(t *testing.T) {
	st := testStore(t)
	local := `{"input_kind":"slash","why":"local command"}`
	reminder := "<system-reminder>" + strings.Repeat("A plugin reloaded and its tools changed. ", 8) + "</system-reminder>"
	shapes := map[string][2]string{
		"model": {"<command-name>/model</command-name> <command-args></command-args>", "<system-reminder>a plugin reloaded</system-reminder>"},
		"exit":  {"<command-name>/exit</command-name> <command-args></command-args>", reminder},
	}
	for name, blocks := range shapes {
		r := newRecorded(t, name).owner(0, "plan the cleanup").say(10*time.Second, "Should I start on items 1 to 4?")
		off, at := r.line(time.Minute, map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": blocks[0]}, map[string]any{"type": "text", "text": blocks[1]}}}})
		for i, b := range blocks {
			r.evs = append(r.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%d#%d", name, off, i), TS: at, Session: name,
				Harness: "claude", Actor: store.ActorHarness, Kind: "harness_text", Summary: storeCut(b), Meta: local, Ref: fmt.Sprintf("%s@%d#%d", r.path, off, i)})
		}
		if name == "model" {
			r.owner(time.Minute, "resume")
		}
		r.put(st)
	}
	points, _ := replayAll(t, st)
	exits := 0
	for _, p := range points {
		if p.Session == "model" && p.Next.InputKind == "slash" {
			t.Fatalf("a reminder block opened a turn: %+v", p.Next)
		}
		if p.Session == "exit" && p.Next.InputKind == "exit" {
			exits++
		}
	}
	if exits != 1 {
		t.Fatalf("an /exit record with a cut reminder ended %d stops", exits)
	}
}

// A label goes to the stop's step nearest it: a nudge on "continue" two seconds after /model is the nudge's.
func TestALabelGoesToItsNearestStep(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "pair").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").apiError(time.Minute, err503)
	b.localCommand(20*time.Minute, "model", "")
	b.owner(2*time.Second, "continue", "")
	next := b.at
	b.put(st)
	until := t0.Add(6 * time.Hour)
	points, err := History(context.Background(), st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Label{{Session: "pair", At: next, Kind: "nudge"}}
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Session == "pair" && r.Point.Next.At.Equal(next) {
			if r.Label != &labels[0] || !r.StepAt.Equal(next) {
				t.Fatalf("the nudge went to the step at %v, not to continue at %v", r.StepAt, next)
			}
			return
		}
	}
	t.Fatal("no stop ends on continue")
}

// A skill's echo is its prompt's, even marked as a local command next to /model: it opens a turn.
func TestASkillsEchoIsNoLocalCommand(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "skill-run").owner(0, "watch the run", "").say(10*time.Second, "Watching.")
	b.localCommand(time.Minute, "model", "").add(time.Second, "harness_text", store.ActorHarness, "",
		"<command-message>loop is running…</command-message> <command-name>/loop</command-name>", `{"input_kind":"slash","why":"local command"}`)
	skill := b.at
	b.put(st)
	points, _ := replayAll(t, st)
	for _, p := range points {
		if p.Session == "skill-run" && p.Next.At.Equal(skill) && p.Next.InputKind == "slash" {
			return
		}
	}
	t.Fatal("a skill's echo opened no turn")
}

// An older Codex rollout writes no turn_aborted record, only its event: the stop starts at that event, read from
// the rollout while it is there, not at the later record the store marked.
func TestAnOlderCodexAbortIsDatedByItsEvent(t *testing.T) {
	st := testStore(t)
	r := newRecorded(t, "old-abort").owner(0, "run the suite").say(5*time.Second, "I'll run the suite and report when it finishes.")
	r.line(10*time.Minute, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "turn_aborted", "reason": "interrupted"}})
	abortAt := r.at
	r.event(20*time.Minute, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message"}}, "harness_text", store.ActorHarness, "",
		"# AGENTS.md instructions", `{"input_kind":"typed","prior":"turn_aborted","why":"harness text"}`)
	r.owner(time.Second, ".")
	next := r.at
	for i := range r.evs {
		r.evs[i].Harness = "codex"
	}
	r.put(st)
	if res := replayed(t, st, "old-abort", next); !res.Point.Stop.At.Equal(abortAt) {
		t.Fatalf("the stop at %v, not at the abort event at %v", res.Point.Stop.At, abortAt)
	}
}

// queuedInputs are what Claude Code queues while a turn runs and delivers into it after the record it waited for,
// dating each record when it was queued: a task's notification, which the store marks by its command mode, and
// another session's message (meta, from a peer), which the store keeps as a harness line, as the harness's text or,
// for a tag tacit does not know, an agent's.
var queuedInputs = []struct {
	name, kind, text string
	actor            store.Actor
}{
	{"notification", "harness_text", "<task-notification>\n<task-id>b0</task-id>\n<status>completed</status>\n<summary>Background command \"lint\" completed (exit code 0)</summary>\n</task-notification>", store.ActorHarness},
	{"agent-message", "harness_text", `<agent-message from="worker">The build is green.</agent-message>`, store.ActorHarness},
	{"cross-session", "agent_text", `<cross-session-message from="s2" from-name="reviewer" from-mode="default">Review done, two nits.</cross-session-message>`, store.ActorAgent},
}

// queuedInput is one of queuedInputs, queued at queuedAt and delivered after the records before it.
func (r *recorded) queuedInput(after time.Duration, queuedAt time.Time, name string) *recorded {
	for _, q := range queuedInputs {
		switch {
		case q.name != name:
			continue
		case name == "notification":
			return r.queued(after, queuedAt, q.text)
		}
		off, _ := r.line(after, map[string]any{"type": "attachment", "timestamp": queuedAt.Format(time.RFC3339Nano),
			"attachment": map[string]any{"type": "queued_command", "commandMode": "prompt", "isMeta": true, "origin": map[string]any{"kind": "peer"}, "prompt": q.text}})
		r.evs = append(r.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%d", r.session, off), TS: queuedAt, Session: r.session,
			Harness: "claude", Actor: q.actor, Kind: q.kind, Summary: storeCut(q.text), Meta: `{"why":"harness line"}`, Ref: fmt.Sprintf("%s@%d#0", r.path, off)})
		return r
	}
	r.t.Fatalf("no queued input %q", name)
	return r
}

// ownerQueued is a prompt the owner typed at typedAt while the turn ran, delivered after the records before it.
func (r *recorded) ownerQueued(after time.Duration, typedAt time.Time, text string) *recorded {
	off, _ := r.line(after, map[string]any{"type": "attachment", "timestamp": typedAt.Format(time.RFC3339Nano),
		"attachment": map[string]any{"type": "queued_command", "commandMode": "prompt", "origin": map[string]any{"kind": "human"}, "prompt": text}})
	r.evs = append(r.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%d", r.session, off), TS: typedAt, Session: r.session,
		Harness: "claude", Actor: store.ActorOwner, Kind: "owner_text", Summary: text, Meta: `{"input_kind":"queued"}`, Ref: fmt.Sprintf("%s@%d#0", r.path, off)})
	return r
}

// pointOf is the point of a session whose next input is text, as the store keeps it.
func pointOf(points []Point, session, text string) (Point, bool) {
	for _, p := range points {
		if p.Session == session && p.Next.Text == storeCut(text) {
			return p, true
		}
	}
	return Point{}, false
}

// heldAsWords reports a point of a session whose stop reads text as the model's words.
func heldAsWords(points []Point, session, text string) bool {
	for _, p := range points {
		if p.Session == session && p.Stop.Words == storeCut(text) {
			return true
		}
	}
	return false
}

// Another session's message Claude Code queued during a call is delivered after the call's result, and its record is
// dated when it was queued, before that result. It is read where it was delivered: the stop before it is the
// result's, with no call running, and it is an input of the session's own, not the model's words, whichever way the
// store keeps it, with its transcript or without.
func TestAQueuedPeerMessageIsReadAfterTheResultItFollows(t *testing.T) {
	st := testStore(t)
	results := map[string]time.Time{}
	texts := map[string]string{}
	for _, q := range queuedInputs[1:] {
		for _, gone := range []bool{false, true} {
			session := q.name
			if gone {
				session += "-gone"
			}
			b := newRecorded(t, session).owner(0, "run the suite").call(time.Second, "t1")
			called := b.at
			b.result(time.Minute, "t1")
			results[session], texts[session] = b.at, q.text
			b.queuedInput(0, called.Add(30*time.Second), q.name).say(5*time.Second, "Noted. The suite passed.")
			if gone {
				if err := os.Remove(b.path); err != nil {
					t.Fatal(err)
				}
			}
			b.put(st)
		}
	}
	points, _ := replayAll(t, st)
	for session, result := range results {
		p, ok := pointOf(points, session, texts[session])
		if s := p.Stop; !ok || !s.At.Equal(result) || s.Busy || p.Next.Kind != "self" {
			t.Errorf("%s: the stop before the message: found %v, at %v (the result at %v), busy %v, next %q", session, ok, s.At, result, s.Busy, p.Next.Kind)
		}
		if heldAsWords(points, session, texts[session]) {
			t.Errorf("%s: a stop reads the message as the model's words", session)
		}
	}
}

// Claude Code delivers what it queued during a call together, after the call's result: the first is read after the
// result, and what follows it is delivered with it, not pulled back to the time it was queued.
func TestBackToBackQueuedInputsAreDeliveredTogether(t *testing.T) {
	st := testStore(t)
	results := map[string]time.Time{}
	second := "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Background command \"build\" completed (exit code 0)</summary>\n</task-notification>"
	for _, q := range queuedInputs {
		b := newRecorded(t, q.name).owner(0, "run the suite").call(time.Second, "t1")
		called := b.at
		b.result(time.Minute, "t1")
		results[q.name] = b.at
		b.queuedInput(0, called.Add(30*time.Second), q.name).queued(0, called.Add(40*time.Second), second).say(5*time.Second, "Both done.")
		b.put(st)
	}
	points, _ := replayAll(t, st)
	for _, q := range queuedInputs {
		p, ok := pointOf(points, q.name, q.text)
		if s := p.Stop; !ok || !s.At.Equal(results[q.name]) || s.Busy {
			t.Errorf("%s: the stop before the first: found %v, at %v (the result at %v), busy %v", q.name, ok, s.At, results[q.name], s.Busy)
		}
		if p, ok := pointOf(points, q.name, second); ok {
			t.Errorf("%s: the second ended a stop of its own at %v, after %v", q.name, p.Next.At, p.Stop.At)
		}
	}
}

// A prompt the owner typed during a call keeps the time he typed it. What Claude Code queued after it is delivered
// with it: part of his input, neither an input of its own nor the model's words.
func TestAQueuedInputAfterTheOwnersQueuedPromptIsPartOfIt(t *testing.T) {
	st := testStore(t)
	typed := map[string]time.Time{}
	for _, q := range queuedInputs {
		b := newRecorded(t, q.name).owner(0, "run the suite").call(time.Second, "t1")
		called := b.at
		typed[q.name] = called.Add(20 * time.Second)
		b.result(time.Minute, "t1").ownerQueued(0, typed[q.name], "and lint it").queuedInput(0, called.Add(25*time.Second), q.name).
			say(5*time.Second, "Linting too.")
		b.put(st)
	}
	points, _ := replayAll(t, st)
	for _, q := range queuedInputs {
		if p, ok := pointOf(points, q.name, "and lint it"); !ok || !p.Next.At.Equal(typed[q.name]) {
			t.Errorf("%s: no stop ends at the owner's prompt, typed at %v: found %v, %+v", q.name, typed[q.name], ok, p.Next)
		}
		if p, ok := pointOf(points, q.name, q.text); ok {
			t.Errorf("%s: what came after his prompt ended a stop of its own at %v", q.name, p.Next.At)
		}
		if heldAsWords(points, q.name, q.text) {
			t.Errorf("%s: a stop reads what came after his prompt as the model's words", q.name)
		}
	}
}

// The events of a session are read in the store's order: by time, then transcript, offset and index. A notification
// delivered after a call's result is read after it, also when another transcript of the session has a record at the
// result's time.
func TestANotificationAfterAResultFollowsItWhateverElseSharesItsTime(t *testing.T) {
	st := testStore(t)
	b := newRecorded(t, "tied").owner(0, "run the suite").call(time.Second, "t1")
	called := b.at
	b.result(time.Minute, "t1")
	result := b.at
	other := filepath.Join(filepath.Dir(b.path), "other.jsonl")
	b.evs = append(b.evs, store.Event{Source: store.Transcripts, SourceKey: "tied-other@0", TS: result, Session: "tied", Harness: "claude",
		Actor: store.ActorHarness, Kind: "harness_text", Summary: "<system-reminder>a note</system-reminder>", Meta: `{"why":"meta line"}`,
		Ref: other + "@0#0"})
	text := queuedInputs[0].text
	b.queued(0, called.Add(30*time.Second), text).say(5*time.Second, "Linted.")
	b.put(st)
	points, _ := replayAll(t, st)
	if p, ok := pointOf(points, "tied", text); !ok || !p.Stop.At.Equal(result) || p.Stop.Busy {
		t.Fatalf("the stop before the notification: found %v, at %v (the result at %v), busy %v", ok, p.Stop.At, result, p.Stop.Busy)
	}
}
