package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/act"
	"github.com/agent-sh/momentum/sessions/claude"
)

func TestStalledInterruptsATurnWaitingOnARequestWithNoReply(t *testing.T) {
	asked := t0.Add(-time.Minute)
	waiting := func(f func(*Stop)) Stop {
		return stopped(func(s *Stop) {
			s.Phase, s.Working, s.At, s.RequestAt, s.OwnerAt, s.Started = sessions.Working, true, asked, t0, asked, t0.Add(-time.Hour)
			f(s)
		})
	}
	check(t, stalled, []ruleCase{
		{"a request with no reply", waiting(func(*Stop) {}), Tries{}, nil, Wait, t0.Add(30 * time.Minute), 1, "got no reply"},
		{"the second try", waiting(func(*Stop) {}), Tries{RuleStalled: 1}, nil, Wait, t0.Add(30 * time.Minute), 2, Marker},
		{"tries used up", waiting(func(*Stop) {}), Tries{RuleStalled: 2}, nil, Case, time.Time{}, 2, ""},
		{"the machine slept after it", waiting(func(s *Stop) { s.Woke = t0.Add(2 * time.Hour) }), Tries{}, nil, Wait, t0.Add(2*time.Hour + 30*time.Minute), 1, ""},
		{"it slept before it", waiting(func(s *Stop) { s.Woke = t0.Add(-time.Hour) }), Tries{}, nil, Wait, t0.Add(30 * time.Minute), 1, ""},
		{"a call runs", waiting(func(s *Stop) { s.Busy = true }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"background work does not answer the model", waiting(func(s *Stop) { s.Tasks = 1 }), Tries{}, nil, Wait, t0.Add(30 * time.Minute), 1, ""},
		{"its user queued a message", waiting(func(s *Stop) { s.Queued = 1 }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a request of an earlier process", waiting(func(s *Stop) { s.Started = t0.Add(time.Minute) }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"no request", waiting(func(s *Stop) { s.RequestAt = time.Time{} }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a harness that records no requests", waiting(func(s *Stop) { s.Tool = "codex" }), Tries{}, nil, "", time.Time{}, 0, ""},
	})
	// The owner's prompt that sent the request holds nothing past his window: the wait is longer.
	if d := Plan(waiting(func(*Stop) {}), Tries{}, DefaultParams, nil); d.Rule != RuleStalled || d.Verdict != Wait {
		t.Fatalf("held: %+v", d)
	}
}

// transcript writes a synthetic Claude Code transcript, a line per call, each a second after the one before.
type transcript struct {
	t    *testing.T
	path string
	at   time.Time
}

func (x *transcript) add(line map[string]any) *transcript {
	x.t.Helper()
	x.at = x.at.Add(time.Second)
	if _, ok := line["timestamp"]; !ok {
		line["timestamp"] = x.at.Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(line)
	if err != nil {
		x.t.Fatal(err)
	}
	f, err := os.OpenFile(x.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		x.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		x.t.Fatal(err)
	}
	return x
}

func (x *transcript) user(text string) *transcript {
	return x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": text}})
}

func (x *transcript) say(text string) *transcript {
	return x.add(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-test-1", "content": []any{
		map[string]any{"type": "text", "text": text}}}})
}

func (x *transcript) request() *transcript {
	return x.add(map[string]any{"type": "api-request", "querySource": "repl_main_thread"})
}

func (x *transcript) notice(text string) *transcript {
	return x.add(map[string]any{"type": "system", "subtype": "informational", "level": "notice", "content": text, "isMeta": true})
}

// queueing is a sender for a pane whose turn runs: the terminal check sees the turn running, and an interrupt
// writes what Claude Code writes for one: the interruption, then the queued message as the next prompt.
type queueing struct {
	fakeSender
	x   *transcript
	now *time.Time
}

func (f *queueing) Interrupt(ctx context.Context, t act.Target, text string) (act.Path, error) {
	f.fakeSender.Interrupt(ctx, t, text)
	f.x.at = *f.now
	f.x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	f.x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued",
		"message": map[string]any{"role": "user", "content": text}})
	return act.Tmux, nil
}

// TestALoopStalledOnARequestIsInterruptedAndResumed rebuilds a stall: a loop's check ran and left a background
// waiter running; then a prompt typed by mistake, a /loop queued while it ran, an Escape that ended it and sent the
// /loop, whose request got no reply. Only plugin notices follow. The /loop's records open no turn of their own (its echo is a command's, its body
// the harness's), but its request does, and only the request says the turn waits on the model.
func TestALoopStalledOnARequestIsInterruptedAndResumed(t *testing.T) {
	dir := t.TempDir()
	x := &transcript{t: t, path: filepath.Join(dir, "s1.jsonl"), at: t0}
	x.user("/loop check the lab").
		add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1",
			"name": "Bash", "input": map[string]any{"command": "waitfor --until 'test -e done'", "run_in_background": true}}}}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
			"content": "Command running in background with ID: bw1. Output is being written to: /x/bw1.output"}}}}).
		say("Checked: all green. The waiter wakes me when the run stops.").
		user("'").request().request().
		add(map[string]any{"type": "queue-operation", "operation": "enqueue"}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}}).
		add(map[string]any{"type": "queue-operation", "operation": "dequeue"}).
		add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"},
			"message": map[string]any{"content": "<command-message>loop</command-message>\n<command-name>/loop</command-name>"}}).
		add(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": []any{map[string]any{"type": "text",
			"text": "# /loop\nRun the check now, then schedule the next one; a <task-notification> also wakes you."}}}}).
		request()
	sent := x.at
	x.notice("wait-guard: reloaded").notice("rulebook: reloaded")

	tr := claude.NewTracker(x.path)
	scan := func(now time.Time) *sessions.Session {
		s := &sessions.Session{Key: "sess-loop", Tool: "claude", Name: "lab", Where: "work:2.1", Pane: "%9", PID: 42,
			Transcript: x.path, Started: t0.Add(-time.Hour), Tasks: []sessions.Task{}}
		tr.Facts(s, now)
		s.TurnOpen = tr.TurnOpen()
		sessions.Decide(s, now)
		s.Since = sent // the phase stamp: working since the /loop's request
		return s
	}
	st := testStore(t)
	now := sent.Add(29 * time.Minute)
	f := &queueing{fakeSender: fakeSender{check: act.ErrBusy}, x: x, now: &now}
	sv := supervisor(st, &f.fakeSender, &now)
	sv.Sender, sv.Live = f, map[string]bool{"stalled": true}
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	run(t, sv, scan(now))
	if len(f.sent) != 0 {
		t.Fatalf("before its time: %q", f.sent)
	}
	now = sent.Add(30 * time.Minute)
	run(t, sv, scan(now))
	if len(f.sent) != 1 || !strings.HasPrefix(f.sent[0], "%9 interrupt "+Marker) {
		t.Fatalf("at its time: sent %q", f.sent)
	}
	if s := scan(now); len(s.Tasks) != 1 {
		t.Fatalf("the waiter runs all along: tasks %+v", s.Tasks)
	}
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["sent"] != true || metaMap(acts[0])["interrupt"] != true || metaMap(acts[0])["interrupted"] != true {
		t.Fatalf("its event: %+v", acts)
	}
	// The next scan: the message opened the next turn; the Escape that ended the stalled one is momentum's, so
	// nothing takes it for its owner at the pane.
	now = now.Add(time.Minute)
	s := scan(now)
	if s.RequestAt.Equal(sent) || !s.TurnOpen {
		t.Fatalf("after the interrupt: request %v open %v", s.RequestAt, s.TurnOpen)
	}
	run(t, sv, s)
	if r := results(t, st); !r[Delivered] {
		t.Fatalf("outcomes %v", r)
	}
	h := history(events(t, st))
	if p := h.presence(s, time.Time{}, time.Time{}); !p.Equal(s.Started) {
		t.Fatalf("presence %v: the interruption counted as its owner's", p)
	}
}

func TestAStalledTurnTheHarnessTookBackTakesTheMessageAsItIs(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{} // the screen shows the input line, empty, and no turn running
	now := t0.Add(31 * time.Minute)
	sv := supervisor(st, f, &now)
	sv.Live = map[string]bool{RuleStalled: true}
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	s := &sessions.Session{Key: "sess-drop", Tool: "claude", Pane: "%4", PID: 42, Phase: sessions.Working, TurnOpen: true, Since: t0,
		RequestAt: t0, Tasks: []sessions.Task{}, Log: &sessions.Log{}}
	run(t, sv, s)
	if len(f.sent) != 1 || f.sent[0] != "%4 "+StalledText {
		t.Fatalf("sent %q", f.sent)
	}
}

func TestAnInterruptThatDoesNotShowInTheTranscriptOpensACase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{check: act.ErrBusy}
	now := t0.Add(31 * time.Minute)
	sv := supervisor(st, f, &now)
	sv.Live = map[string]bool{RuleStalled: true}
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, bool, error) { return false, false, nil }
	defer func(w time.Duration) { promptedWait = w }(promptedWait)
	promptedWait = 10 * time.Millisecond
	s := &sessions.Session{Key: "sess-stuck", Tool: "claude", Pane: "%5", PID: 42, Phase: sessions.Working, TurnOpen: true, Since: t0,
		RequestAt: t0, Tasks: []sessions.Task{}, Log: &sessions.Log{}}
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if len(f.sent) != 1 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("sent %q, cases %+v", f.sent, events(t, st, KindCase))
	}
	if _, ok := note(t, st, ProblemNote+s.Key); !ok {
		t.Fatal("no problem note")
	}
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["unstable"] != true {
		t.Fatalf("acts %+v", acts)
	}
	// A refusal before anything was typed (the turn ended on its own meanwhile) passes by itself.
	st = testStore(t)
	f = &fakeSender{check: act.ErrBusy, interrupt: act.ErrIdle}
	sv = supervisor(st, f, &now)
	sv.Live = map[string]bool{RuleStalled: true}
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	run(t, sv, s)
	if len(events(t, st, KindCase)) != 0 || !errors.Is(act.ErrIdle, act.ErrIdle) {
		t.Fatalf("cases %+v", events(t, st, KindCase))
	}
}

func TestPromptedReadsTheTranscriptSinceTheScan(t *testing.T) {
	dir := t.TempDir()
	x := &transcript{t: t, path: filepath.Join(dir, "s.jsonl"), at: t0}
	x.user(StalledText).say("old turn") // the same text before the scan is not this message
	fi, err := os.Stat(x.path)
	if err != nil {
		t.Fatal(err)
	}
	from := fi.Size()
	if found, _, err := claude.Prompted(x.path, from, StalledText); found || err != nil {
		t.Fatalf("found %v %v", found, err)
	}
	x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}}).
		add(map[string]any{"type": "user", "promptSource": "queued", "message": map[string]any{"content": strings.ReplaceAll(StalledText, " ", "  ")}})
	found, interrupted, err := claude.Prompted(x.path, from, StalledText)
	if !found || !interrupted || err != nil {
		t.Fatalf("found %v interrupted %v %v", found, interrupted, err)
	}
	_ = store.Supervise
}
