package claude

import (
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
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
	x.notice("wait-guard: reloaded").notice("rulebook: reloaded")
	tr := NewTracker(x.path)
	s := x.facts(tr, x.at.Add(4*time.Hour))
	if !s.TurnOpen || s.Phase != sessions.Working || !s.RequestAt.Equal(sent) || s.Queued != 0 {
		t.Fatalf("phase %s open %v request %v queued %d, want working on the request at %v", s.Phase, s.TurnOpen, s.RequestAt, s.Queued, sent)
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
}
