package claude

import (
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/saved"
)

func (x *script) request() *script {
	return x.add(map[string]any{"type": "api-request", "id": "r-" + x.stamp(), "querySource": "repl_main_thread"})
}

func (x *script) notice(text string) *script {
	return x.add(map[string]any{"type": "system", "subtype": "informational", "level": "notice", "content": text, "isMeta": true})
}

func (x *script) queue(op, content string) *script {
	line := map[string]any{"type": "queue-operation", "operation": op}
	if content != "" {
		line["content"] = content
	}
	return x.add(line)
}

func TestARequestWithNoReplyKeepsTheTurnOpen(t *testing.T) {
	x := newScript(t).user("check the deploy").request()
	sent := x.at
	x.notice("a plugin reloaded").add(map[string]any{"type": "pr-link", "prNumber": 3}).
		add(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "date"}}).
		add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "context a hook added"}})
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); !s.TurnOpen || s.Phase != sessions.Working || !s.RequestAt.Equal(sent) {
		t.Fatalf("phase %s open %v request %v, want working since %v", s.Phase, s.TurnOpen, s.RequestAt, sent)
	}
	x.say("The deploy is green.")
	if s := x.facts(tr, x.at); s.TurnOpen || !s.RequestAt.IsZero() {
		t.Fatalf("its reply came: open %v request %v", s.TurnOpen, s.RequestAt)
	}
	// Each kind of record that follows a request answers it: an error, an interruption, a retry's error.
	for name, answer := range map[string]map[string]any{
		"an api error": {"type": "assistant", "isApiErrorMessage": true, "message": map[string]any{"model": "<synthetic>",
			"content": []any{map[string]any{"type": "text", "text": "API Error: 529 overloaded"}}}},
		"an interruption": {"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}},
		"a retry's error": {"type": "system", "subtype": "api_error", "level": "error"},
	} {
		y := newScript(t).user("go").request().add(answer)
		if s := y.facts(NewTracker(y.path), y.at); !s.RequestAt.IsZero() {
			t.Errorf("%s: request %v", name, s.RequestAt)
		}
	}
	// The request's own prompt, written after its request but dated before it, is part of it.
	z := newScript(t).request()
	sent = z.at
	z.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "timestamp": sent.Add(-20 * time.Millisecond).Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": "say it again"}})
	if s := z.facts(NewTracker(z.path), z.at); !s.RequestAt.Equal(sent) || !s.TurnOpen {
		t.Fatalf("its own prompt answered it: request %v open %v", s.RequestAt, s.TurnOpen)
	}
}

// TestAnEscapeThatSendsAQueuedPromptEndsTheTurnBeforeIt: Claude Code writes the interruption of an Escape that
// sends a queued prompt, and the prompt, a few milliseconds apart in either order, then the new turn's request.
// Either way the interruption ended the turn before the prompt, and the prompt's turn runs on, dated from the
// prompt: through its request, and through a retry's backoff, when the retry's error has answered the request and
// the retry's own request is not out yet.
func TestAnEscapeThatSendsAQueuedPromptEndsTheTurnBeforeIt(t *testing.T) {
	for _, promptFirst := range []bool{false, true} {
		x := newScript(t).user("go").request() // a turn that waits on a request with no reply
		at := x.at.Add(30 * time.Minute)
		stamp := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339Nano) }
		escape := map[string]any{"type": "user", "timestamp": stamp(0),
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}}
		prompt := map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued", "timestamp": stamp(30 * time.Millisecond),
			"message": map[string]any{"role": "user", "content": "continue where you left off"}}
		escaped, prompted := at, at.Add(30*time.Millisecond)
		if promptFirst {
			escape["timestamp"], escaped = stamp(37*time.Millisecond), at.Add(37*time.Millisecond)
			x.add(prompt).add(escape)
		} else {
			x.add(escape).add(prompt)
		}
		x.add(map[string]any{"type": "api-request", "querySource": "repl_main_thread", "timestamp": stamp(61 * time.Millisecond)})
		tr := NewTracker(x.path)
		s := x.facts(tr, at.Add(time.Minute))
		ended := escaped // the turn before ended at the Escape, and no later than the prompt began
		if promptFirst {
			ended = prompted
		}
		if !s.TurnOpen || s.Phase != sessions.Working || !s.RequestAt.Equal(at.Add(61*time.Millisecond)) || !s.ControlAt.Equal(escaped) ||
			!s.TurnBegan.Equal(prompted) || !s.IdleSince.Equal(ended) {
			t.Fatalf("prompt first %v: open %v phase %s request %v control %v began %v ended %v", promptFirst, s.TurnOpen, s.Phase, s.RequestAt,
				s.ControlAt, s.TurnBegan, s.IdleSince)
		}
		x.add(map[string]any{"type": "system", "subtype": "api_error", "level": "error", "timestamp": stamp(10 * time.Minute)})
		if s := x.facts(tr, at.Add(10*time.Minute+30*time.Second)); !s.TurnOpen || s.Phase != sessions.Working || !s.RequestAt.IsZero() {
			t.Fatalf("prompt first %v, in the retry's backoff: open %v phase %s request %v", promptFirst, s.TurnOpen, s.Phase, s.RequestAt)
		}
	}
	// An interruption seconds after the queued prompt is no Escape that sent it: it ends the prompt's own turn.
	x := newScript(t).user("go").request()
	x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued",
		"message": map[string]any{"role": "user", "content": "and the tests"}})
	x.add(map[string]any{"type": "user", "timestamp": x.at.Add(5 * time.Second).Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	if s := x.facts(NewTracker(x.path), x.at.Add(time.Minute)); s.TurnOpen {
		t.Fatalf("an interruption 5 s after the queued prompt left its turn open: phase %s", s.Phase)
	}
	// Nor is one dated 10 s before the prompt though written after it.
	w := newScript(t).user("go").request()
	q := w.at.Add(30 * time.Minute)
	w.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued", "timestamp": q.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": "continue"}})
	w.add(map[string]any{"type": "user", "timestamp": q.Add(-10 * time.Second).Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	if s := w.facts(NewTracker(w.path), q.Add(time.Minute)); s.TurnOpen {
		t.Fatalf("an interruption dated 10 s before the queued prompt read as its Escape: phase %s", s.Phase)
	}
	// Nor is one after the prompt's turn sent its request, or one after a prompt its user typed: each takes back
	// the turn the prompt opened.
	for name, kind := range map[string]string{"after its request": "queued", "after a typed prompt": "typed"} {
		y := newScript(t).user("go").request()
		at := y.at.Add(30 * time.Minute)
		y.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": kind, "timestamp": at.Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": "and the tests"}})
		if kind == "queued" {
			y.add(map[string]any{"type": "api-request", "querySource": "repl_main_thread", "timestamp": at.Add(50 * time.Millisecond).Format(time.RFC3339Nano)})
		}
		y.add(map[string]any{"type": "user", "timestamp": at.Add(time.Second).Format(time.RFC3339Nano),
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
		if s := y.facts(NewTracker(y.path), at.Add(time.Minute)); s.TurnOpen {
			t.Fatalf("an interruption %s left the turn open: phase %s", name, s.Phase)
		}
	}
}

// TestAnEscapeInAQueuedPromptsTurnTakesItBack: a queued prompt no Escape sent (the turn before it ended by itself,
// or the Escape that sent it is on record before it), then its owner's Escape a moment later, before anything of
// its turn: the Escape takes the new turn back.
func TestAnEscapeInAQueuedPromptsTurnTakesItBack(t *testing.T) {
	queued := func(at time.Time, text string) map[string]any {
		return map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued",
			"timestamp": at.Format(time.RFC3339Nano), "message": map[string]any{"role": "user", "content": text}}
	}
	escape := func(at time.Time) map[string]any {
		return map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339Nano),
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}}
	}
	for name, write := range map[string]func(x *script, at time.Time){
		"sent by the turn's end": func(x *script, at time.Time) {
			x.say("done").add(queued(at, "and the tests"))
		},
		"two sent by the turn's end": func(x *script, at time.Time) {
			x.say("done").add(queued(at, "and the tests")).add(queued(at.Add(time.Millisecond), "and the docs"))
		},
		"sent by an Escape on record before it": func(x *script, at time.Time) {
			x.add(escape(at.Add(-30 * time.Millisecond))).add(queued(at, "continue"))
		},
	} {
		x := newScript(t).user("go")
		at := x.at.Add(time.Minute)
		write(x, at)
		x.add(escape(at.Add(800 * time.Millisecond)))
		if s := x.facts(NewTracker(x.path), at.Add(time.Hour)); s.TurnOpen {
			t.Errorf("%s: the owner's Escape read as the one that sent it: phase %s", name, s.Phase)
		}
	}
}

// TestAnEscapeAfterItsQueuedPromptCancelsAPendingWakeup: like the Escape whose interruption comes first, the one
// written after the queued prompt it sent cancels a self-paced wakeup that was pending.
func TestAnEscapeAfterItsQueuedPromptCancelsAPendingWakeup(t *testing.T) {
	x, _ := soak(t)
	x.user("and look at the logs").request() // a turn that waits on a request with no reply; the wakeup stays
	if s := x.facts(NewTracker(x.path), x.at); s.WakeAt.IsZero() {
		t.Fatal("no wakeup pending before the Escape")
	}
	at := x.at.Add(30 * time.Minute)
	x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": "continue"}})
	x.add(map[string]any{"type": "user", "timestamp": at.Add(7 * time.Millisecond).Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	if s := x.facts(NewTracker(x.path), at.Add(time.Minute)); !s.WakeAt.IsZero() || !s.TurnOpen {
		t.Fatalf("after the Escape: wake %v open %v", s.WakeAt, s.TurnOpen)
	}
}

// TestARunResumedBetweenAQueuedPromptAndItsEscapeDerivesWhatAColdRunDerives: a checkpoint taken after the queued
// prompt and before the Escape's interruption written after it keeps the mark.
func TestARunResumedBetweenAQueuedPromptAndItsEscapeDerivesWhatAColdRunDerives(t *testing.T) {
	x := newScript(t).user("go").request()
	found := listed{{Tool: "claude", Session: "a", Transcript: x.path, Started: t0}}
	mem := saved.New()
	at := x.at.Add(30 * time.Minute)
	x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": "continue"}})
	runOnce(t, found, mem, at.Add(time.Millisecond))
	x.add(map[string]any{"type": "user", "timestamp": at.Add(7 * time.Millisecond).Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	x.add(map[string]any{"type": "api-request", "querySource": "repl_main_thread", "timestamp": at.Add(30 * time.Millisecond).Format(time.RFC3339Nano)})
	resumed := runOnce(t, found, mem, at.Add(time.Minute))
	if diff := saved.Same(resumed, runOnce(t, found, nil, at.Add(time.Minute))); diff != "" {
		t.Fatal(diff)
	}
	if len(resumed) != 1 || !resumed[0].TurnOpen || !resumed[0].TurnBegan.Equal(at) {
		t.Fatalf("resumed: %+v", resumed)
	}
}

// TestALoopRestartedOnARequestThatNeverAnswers rebuilds a stall: a prompt typed by mistake, a /loop queued while it
// ran, an Escape that ends it and sends the /loop, whose request then gets no reply; only notices follow. The
// /loop's own records open no turn (its echo is a command's, its body the harness's), but its request does.
func TestALoopRestartedOnARequestThatNeverAnswers(t *testing.T) {
	x := newScript(t).user("x").request().request().queue("enqueue", "/loop").
		user("[Request interrupted by user]").queue("dequeue", "").
		add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"},
			"message": map[string]any{"content": "<command-message>loop</command-message>\n<command-name>/loop</command-name>"}}).
		add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "# /loop\nRun the check now, then schedule the next one."}}}}).
		request()
	sent := x.at
	x.notice("a plugin reloaded").notice("another plugin reloaded")
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at.Add(4*time.Hour))
	if !s.TurnOpen || s.Phase != sessions.Working || !s.RequestAt.Equal(sent) || s.Queued != 0 {
		t.Fatalf("phase %s open %v request %v queued %d, want working on the request at %v", s.Phase, s.TurnOpen, s.RequestAt, s.Queued, sent)
	}
	// Reopened, Claude Code closes the turn the request held open: a reopening, as for a typed prompt's turn.
	x.synthetic("No response requested.")
	if s := x.facts(tr, x.at); s.ResumedAt.IsZero() || s.TurnOpen {
		t.Fatalf("reopened: resumed %v open %v", s.ResumedAt, s.TurnOpen)
	}
}

func TestTheQueueCountsWhatItsUserTyped(t *testing.T) {
	x := newScript(t).user("long job").request().
		queue("enqueue", "<task-notification><task-id>b1</task-id><status>completed</status></task-notification>").
		queue("enqueue", "and then run the linter")
	tr := NewTracker(x.path)
	if s := x.facts(tr, x.at); s.Queued != 1 {
		t.Fatalf("queued %d", s.Queued)
	}
	x.queue("remove", "and then run the linter") // taken in mid-turn, by its text
	if s := x.facts(tr, x.at); s.Queued != 0 {
		t.Fatalf("after its removal: queued %d", s.Queued)
	}
	x.queue("enqueue", "one more").queue("dequeue", "").queue("dequeue", "")
	if s := x.facts(tr, x.at); s.Queued != 0 {
		t.Fatalf("after the dequeues: queued %d", s.Queued)
	}
	x.queue("enqueue", "a").queue("enqueue", "b").queue("popAll", "")
	if s := x.facts(tr, x.at); s.Queued != 0 {
		t.Fatalf("popAll takes them back: queued %d", s.Queued)
	}
	// An item queued with no text recorded is nearly always a scheduled run's prompt, another session's message or
	// a notification: it does not count as typed.
	x.queue("enqueue", "")
	if s := x.facts(tr, x.at); s.Queued != 0 {
		t.Fatalf("an item with no text: queued %d", s.Queued)
	}
}
