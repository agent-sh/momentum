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
	for _, name := range []string{"record", "marked", "both", "silent", "on-input"} {
		b := newTimeline(t, name).owner(0, "watch the checks", "").call(time.Second, "exec", "c1", `{"cmd":"gh pr checks 9"}`)
		if name != "silent" && name != "on-input" { // a call aborted before it wrote anything has no result
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
		if name == "on-input" {
			b.owner(10*time.Minute, ".", "turn_aborted") // the abort marked on the owner's own text
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
		if r.Label == nil || r.Outcome != OwnerFirst || !r.Decision.At.Equal(rule) || r.StepAt.Sub(r.Decision.At) != stepAt.Sub(rule) {
			t.Fatalf("the labelled step: %v %q, rule at %v, gap %v", r.Label, r.Outcome, r.Decision.At, r.StepAt.Sub(r.Decision.At))
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
	for _, r := range overdueActs(t, st, t0.Add(3*time.Hour)) {
		if !r.Point.Stop.WakeDue.After(model) {
			t.Fatalf("a run due while the process was gone read as missed: due %v, reopened %v", r.Point.Stop.WakeDue, model)
		}
	}
}
