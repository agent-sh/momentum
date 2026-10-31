package supervise

import (
	"context"
	"encoding/json"
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

func (f *queueing) Interrupt(ctx context.Context, t act.Target, text string, still func() error) (act.Path, error) {
	if _, err := f.fakeSender.Interrupt(ctx, t, text, still); err != nil {
		return act.Tmux, err
	}
	f.x.at = *f.now
	f.x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	f.x.add(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued",
		"message": map[string]any{"role": "user", "content": text}})
	return act.Tmux, nil
}

// stalledLoop writes a synthetic stall: a loop's check ran and left a background waiter running; then a prompt
// typed by mistake, a /loop queued while it ran, an Escape that ended it and sent the /loop, whose request got no
// reply. Only plugin notices follow. The /loop's records open no turn of their own (its echo is a command's, its
// body the harness's), but its request does, and only the request says the turn waits on the model. It returns
// when the request went out, and a scan of the session at a time.
func stalledLoop(t *testing.T) (*transcript, time.Time, func(time.Time) *sessions.Session) {
	x := &transcript{t: t, path: filepath.Join(t.TempDir(), "s1.jsonl"), at: t0}
	x.user("/loop check the lab").
		add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1",
			"name": "Bash", "input": map[string]any{"command": "./watch-the-run", "run_in_background": true}}}}}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
			"content": "Command running in background with ID: bw1. Output is being written to: /x/bw1.output"}}}}).
		say("Checked: all green. The watcher wakes me when the run stops.").
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
	x.notice("a plugin reloaded").notice("another plugin reloaded")
	tr := claude.NewTracker(x.path)
	return x, sent, func(now time.Time) *sessions.Session {
		s := &sessions.Session{Key: "sess-loop", Tool: "claude", Name: "lab", Where: "work:2.1", Pane: "%9", PID: 42,
			Transcript: x.path, Started: t0.Add(-time.Hour), Tasks: []sessions.Task{}}
		tr.Facts(s, now)
		s.TurnOpen = tr.TurnOpen()
		sessions.Decide(s, now)
		s.Since = sent // the phase stamp: working since the /loop's request
		return s
	}
}

func liveStalled(st *store.Store, f Sender, now *time.Time) *Supervisor {
	sv := supervisor(st, &fakeSender{}, now)
	sv.Sender, sv.Live = f, map[string]bool{"stalled": true}
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	return sv
}

func TestALoopStalledOnARequestIsInterruptedAndResumed(t *testing.T) {
	x, sent, scan := stalledLoop(t)
	st := testStore(t)
	now := sent.Add(29 * time.Minute)
	f := &queueing{x: x, now: &now}
	sv := liveStalled(st, f, &now)
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
	if s.RequestAt.Equal(sent) || !s.TurnOpen || s.ControlAt.IsZero() {
		t.Fatalf("after the interrupt: request %v open %v control %v", s.RequestAt, s.TurnOpen, s.ControlAt)
	}
	run(t, sv, s)
	if r := results(t, st); !r[Delivered] {
		t.Fatalf("outcomes %v", r)
	}
	h := history(events(t, st))
	if p := h.presence(s, time.Time{}, time.Time{}); !p.Equal(s.Started) {
		t.Fatalf("presence %v: the interruption counted as its owner's", p)
	}
	// Its owner's own interruption a moment later still counts.
	s.ControlAt = s.ControlAt.Add(time.Second)
	if p := h.presence(s, time.Time{}, time.Time{}); !p.Equal(s.ControlAt) {
		t.Fatalf("presence %v: its owner's Escape did not count", p)
	}
}

// waitingSession is a Claude Code session whose turn has waited on a request since t0.
func waitingSession(key string) *sessions.Session {
	return &sessions.Session{Key: key, Tool: "claude", Pane: "%4", PID: 42, Phase: sessions.Working, TurnOpen: true, Since: t0,
		RequestAt: t0, Tasks: []sessions.Task{}, Log: &sessions.Log{}}
}

func TestAStalledTurnTakenBackIsLeftToItsOwner(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrIdle} // its screen shows no turn running: its owner's Escape, most likely
	now := t0.Add(31 * time.Minute)
	sv := liveStalled(st, f, &now)
	s := waitingSession("sess-back")
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	acts := events(t, st, KindAct)
	if len(f.sent) != 0 || len(acts) != 1 || !strings.Contains(metaMap(acts[0])["refused"].(string), "taken back") && !strings.Contains(metaMap(acts[0])["refused"].(string), "turn over") ||
		len(events(t, st, KindCase)) != 0 {
		t.Fatalf("sent %q, acts %+v, cases %+v", f.sent, acts, events(t, st, KindCase))
	}
	if n, ok := note(t, st, AskNote+s.Key); !ok || !strings.HasPrefix(n.Title, "Resume it?") || !n.Done.IsZero() {
		t.Fatalf("needs note %+v %v", n, ok)
	}
}

func TestADryStalledTurnGetsANeedsNote(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(31 * time.Minute)
	sv := supervisor(st, f, &now) // stalled is not live
	sv.Woke = func(context.Context) time.Time { return time.Time{} }
	s := waitingSession("sess-dry")
	run(t, sv, s)
	if len(f.sent) != 0 || f.checks != 0 {
		t.Fatalf("sent %q checks %d", f.sent, f.checks)
	}
	if n, ok := note(t, st, AskNote+s.Key); !ok || !strings.HasPrefix(n.Title, "Resume it?") {
		t.Fatalf("needs note %+v %v", n, ok)
	}
	// Its reply comes: the note is done.
	s.TurnOpen, s.RequestAt, s.Phase, s.Since, s.LastText = false, time.Time{}, sessions.Idle, now, "Done."
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); n.Done.IsZero() {
		t.Fatalf("still stands: %+v", n)
	}
}

func TestADraftOnAStalledTurnIsItsOwnerAtThePane(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrDraft}
	now := t0.Add(31 * time.Minute)
	sv := liveStalled(st, f, &now)
	s := waitingSession("sess-draft")
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if len(f.sent) != 0 || len(events(t, st, KindPresent)) != 1 || len(events(t, st, KindAct)) != 1 {
		t.Fatalf("sent %q, present %+v, acts %+v", f.sent, events(t, st, KindPresent), events(t, st, KindAct))
	}
	// He clears it: the line empty after a draft is him there just now, and nothing is typed.
	f.checkInterrupt = nil
	now = t0.Add(33*time.Minute + DefaultParams.OwnerWindow + time.Second) // past the window of his last draft
	run(t, sv, s)
	if len(f.sent) != 0 || metaMap(events(t, st, KindPresent)[0])["state"] != "cleared" {
		t.Fatalf("typed as he cleared it: %q", f.sent)
	}
}

func TestARequestAnsweredAfterTheScanIsLeftAlone(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(31 * time.Minute)
	sv := liveStalled(st, f, &now)
	sv.Answered = func(*sessions.Session, time.Time) (bool, error) { return true, nil } // its reply came just now
	run(t, sv, waitingSession("sess-late"))
	if len(f.sent) != 0 || len(events(t, st, KindCase)) != 0 {
		t.Fatalf("sent %q, cases %+v", f.sent, events(t, st, KindCase))
	}
}

func TestAnInterruptThatDoesNotShowInTheTranscriptOpensACase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(31 * time.Minute)
	sv := liveStalled(st, f, &now)
	sv.Answered = func(*sessions.Session, time.Time) (bool, error) { return false, nil }
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return false, time.Time{}, nil
	}
	defer func(w time.Duration) { promptedWait = w }(promptedWait)
	promptedWait = 10 * time.Millisecond
	s := waitingSession("sess-stuck")
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if len(f.sent) != 1 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("sent %q, cases %+v", f.sent, events(t, st, KindCase))
	}
	// Its problem note stands: the turn still waits on the request the case is about.
	if n, ok := note(t, st, ProblemNote+s.Key); !ok || !n.Done.IsZero() {
		t.Fatalf("problem note %+v %v", n, ok)
	}
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["unstable"] != true {
		t.Fatalf("acts %+v", acts)
	}
	// A refusal before anything was typed (the turn ended on its own meanwhile) passes by itself.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrIdle}
	sv = liveStalled(st, f, &now)
	run(t, sv, s)
	if len(events(t, st, KindCase)) != 0 || len(f.sent) != 0 {
		t.Fatalf("cases %+v sent %q", events(t, st, KindCase), f.sent)
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
	x.add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	escaped := x.at
	x.add(map[string]any{"type": "user", "promptSource": "queued", "message": map[string]any{"content": strings.ReplaceAll(StalledText, " ", "  ")}})
	found, interrupted, err := claude.Prompted(x.path, from, StalledText)
	if !found || !interrupted.Equal(escaped) || err != nil {
		t.Fatalf("found %v interrupted %v %v", found, interrupted, err)
	}
	// Answered: a notice since the scan answers nothing; a reply does.
	y := &transcript{t: t, path: filepath.Join(dir, "a.jsonl"), at: t0}
	y.user("go").request()
	sent := y.at
	fi, _ = os.Stat(y.path)
	y.notice("a plugin reloaded")
	if a, err := claude.Answered(y.path, fi.Size(), sent); a || err != nil {
		t.Fatalf("a notice: %v %v", a, err)
	}
	y.say("Done.")
	if a, err := claude.Answered(y.path, fi.Size(), sent); !a || err != nil {
		t.Fatalf("a reply: %v %v", a, err)
	}
}
