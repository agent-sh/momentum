package supervise

import (
	"context"
	"encoding/json"
	"fmt"
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
		{"tries used up: the last message's own request waits too", waiting(func(*Stop) {}), Tries{RuleStalled: 2}, nil, Wait, t0.Add(30 * time.Minute), 2, ""},
		{"the machine slept after it", waiting(func(s *Stop) { s.Woke = t0.Add(2 * time.Hour) }), Tries{}, nil, Wait, t0.Add(2*time.Hour + 30*time.Minute), 1, ""},
		{"it slept before it", waiting(func(s *Stop) { s.Woke = t0.Add(-time.Hour) }), Tries{}, nil, Wait, t0.Add(30 * time.Minute), 1, ""},
		{"it slept after its time came", waiting(func(s *Stop) { s.Slept, s.Woke = t0.Add(40*time.Minute), t0.Add(2*time.Hour) }), Tries{}, nil, Wait, t0.Add(30 * time.Minute), 1, ""},
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
	// With the tries used up, its time opens a case, not a third message.
	used := Tries{RuleStalled: 2}
	if d := Decide(waiting(func(*Stop) {}), used, DefaultParams, nil, t0.Add(29*time.Minute)); d.Verdict != Wait {
		t.Fatalf("before its time: %+v", d)
	}
	if d := Decide(waiting(func(*Stop) {}), used, DefaultParams, nil, t0.Add(30*time.Minute)); d.Verdict != Case || d.Why != "2 messages did not move it" {
		t.Fatalf("at its time: %+v", d)
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
	if _, ok := line["version"]; !ok {
		line["version"] = "2.1.316" // a Claude Code that records scheduled runs
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
	sv.Sleep = func(context.Context) (time.Time, time.Time) { return time.Time{}, time.Time{} }
	return sv
}

// unread makes a supervisor read no transcript: nothing answered the request since the scan.
func unread(sv *Supervisor) *Supervisor {
	sv.Answered = func(*sessions.Session, time.Time) (bool, error) { return false, nil }
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

func TestAStalledTurnsCaseWaitsForTheRequestItsLastMessageSent(t *testing.T) {
	x, sent, scan := stalledLoop(t)
	st := testStore(t)
	now := sent.Add(30 * time.Minute)
	f := &queueing{x: x, now: &now}
	sv := liveStalled(st, f, &now)
	for try := 1; try <= 2; try++ {
		run(t, sv, scan(now))
		if len(f.sent) != try {
			t.Fatalf("try %d: sent %q", try, f.sent)
		}
		x.request() // the message's turn sends its request, and it gets no reply either
		now = x.at.Add(time.Minute)
		run(t, sv, scan(now)) // the message delivered, and its request only a minute old
		if len(events(t, st, KindCase)) != 0 {
			t.Fatalf("try %d: a case a minute after the message: %+v", try, events(t, st, KindCase))
		}
		now = x.at.Add(30 * time.Minute)
	}
	run(t, sv, scan(now))
	if cs := events(t, st, KindCase); len(cs) != 1 || len(f.sent) != 2 || !strings.Contains(cs[0].Summary, "2 messages did not move it") {
		t.Fatalf("at the last request's time: cases %+v, sent %q", cs, f.sent)
	}
}

// waitingSession is a Claude Code session whose turn has waited on a request since t0.
func waitingSession(key string) *sessions.Session {
	return &sessions.Session{Key: key, Tool: "claude", Pane: "%4", PID: 42, Phase: sessions.Working, TurnOpen: true, Since: t0,
		RequestAt: t0, Tasks: []sessions.Task{}, Log: &sessions.Log{}, Transcript: "/sessions/" + key + ".jsonl", Size: 100}
}

func TestAStalledTurnWhoseScreenShowsNoTurnTakesTheMessageAsItsNextPrompt(t *testing.T) {
	st := testStore(t)
	// Its screen shows no turn running (an Escape takes one back without a record), and its plain check meets a
	// running row the interrupt check could not be sure of.
	f := &fakeSender{checkInterrupt: act.ErrIdle, check: act.ErrBusy}
	now := t0.Add(31 * time.Minute)
	sv := liveStalled(st, f, &now)
	answered := false
	sv.Answered = func(*sessions.Session, time.Time) (bool, error) { return answered, nil }
	s := waitingSession("sess-idle")
	run(t, sv, s)
	if len(f.sent) != 0 || f.checks != 2 { // the interrupt check, then the plain one
		t.Fatalf("a busy screen: sent %q checks %d", f.sent, f.checks)
	}
	// The screen clears, but the reply came since the scan: the turn went on by itself.
	f.check, answered = nil, true
	now = now.Add(time.Minute)
	run(t, sv, s)
	if len(f.sent) != 0 {
		t.Fatalf("typed after the reply: %q", f.sent)
	}
	// No reply: the message goes in the plain way, as the next prompt.
	answered = false
	now = now.Add(time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 || f.sent[0] != "%4 "+StalledText {
		t.Fatalf("sent %q", f.sent)
	}
	var sentAct map[string]any
	for _, e := range events(t, st, KindAct) {
		if m := metaMap(e); m["sent"] == true {
			sentAct = m
		}
	}
	if sentAct == nil || sentAct["interrupt"] != nil || sentAct["request_at"] != float64(t0.UnixMilli()) || len(events(t, st, KindCase)) != 0 {
		t.Fatalf("its event %+v, cases %+v", sentAct, events(t, st, KindCase))
	}
	if n, ok := note(t, st, AskNote+s.Key); ok {
		t.Fatalf("a needs note: %+v", n)
	}
	if n, ok := note(t, st, fmt.Sprintf("%s%s@%d", ResumedNote, s.Key, t0.UnixMilli())); !ok || n.Kind != store.NoteDecided ||
		!strings.Contains(n.Title, "as its next prompt") {
		t.Fatalf("decided note %+v %v", n, ok)
	}
}

func TestADryStalledTurnRecordsWhatItWouldHaveSent(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(31 * time.Minute)
	sv := supervisor(st, f, &now) // stalled is not live
	sv.Sleep = func(context.Context) (time.Time, time.Time) { return time.Time{}, time.Time{} }
	s := waitingSession("sess-dry")
	run(t, sv, s)
	acts := events(t, st, KindAct)
	if len(f.sent) != 0 || f.checks != 0 || len(acts) != 1 || acts[0].Experiment != Dry {
		t.Fatalf("sent %q checks %d acts %+v", f.sent, f.checks, acts)
	}
	if ns, _ := sv.Store.Notes(context.Background()); len(ns) != 0 {
		t.Fatalf("notes %+v", ns)
	}
}

func TestAStalledTurnNoPathReachesGetsANeedsNoteForItsRequest(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrNoPath} // no terminal or queue reaches it
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-nowhere")
	run(t, sv, s)
	n, ok := note(t, st, AskNote+s.Key)
	if !ok || !n.Done.IsZero() || !strings.HasPrefix(n.Title, "momentum could not resume ") || strings.Contains(n.Title, "?") || !n.At.Equal(t0) {
		t.Fatalf("needs note %+v %v", n, ok)
	}
	if _, ok := note(t, st, ProblemNote+s.Key); ok || len(events(t, st, KindCase)) != 0 || len(f.sent) != 0 {
		t.Fatalf("a case for it: %+v, sent %q", events(t, st, KindCase), f.sent)
	}
	// Its owner comes by (a local command): the rule holds, and the note stands, kept by the mark for the request.
	s.ControlAt = now
	now = now.Add(time.Minute)
	run(t, sv, s)
	// Past its stale hour, nothing tries it, and the note stands.
	now = t0.Add(30*time.Minute + DefaultParams.Stale + 2*time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); !n.Done.IsZero() || len(events(t, st, KindSkip)) != 2 {
		t.Fatalf("the note %+v, skips %+v", n, events(t, st, KindSkip))
	}
	// Claude Code retries the request by itself: the wait starts again, and the note about the first goes.
	s.RequestAt = now.Add(-time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); n.Done.IsZero() {
		t.Fatalf("the note outlived its request: %+v", n)
	}
	// Before the retry has waited as long, no note about it: what the check found was about the first request.
	retry := s.RequestAt
	now = retry.Add(20 * time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); n.Done.IsZero() {
		t.Fatalf("a note before the retry waited: %+v", n)
	}
	// The retry waits as long, and no path reaches it either: its own note.
	now = retry.Add(30 * time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); !ok || !n.Done.IsZero() || !n.At.Equal(retry) {
		t.Fatalf("the retry's note %+v %v", n, ok)
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
	sv := unread(liveStalled(st, f, &now))
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

// ended is a sender for a turn that ends between the check and the paste: Interrupt types nothing.
type ended struct{ fakeSender }

func (f *ended) Interrupt(context.Context, act.Target, string, func() error) (act.Path, error) {
	return act.Tmux, act.ErrIdle
}

func TestAnInterruptThatDoesNotShowInTheTranscriptOpensACase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
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
	// A refusal before anything was typed (the turn ended on its own between the check and the paste) passes by
	// itself.
	st = testStore(t)
	e := &ended{}
	sv = unread(liveStalled(st, e, &now))
	run(t, sv, s)
	if len(events(t, st, KindCase)) != 0 || len(e.sent) != 0 || len(events(t, st, KindAct)) != 1 {
		t.Fatalf("cases %+v sent %q acts %+v", events(t, st, KindCase), e.sent, events(t, st, KindAct))
	}
	// A session whose place in its transcript the scan does not know: nothing could tell that the message went in.
	st = testStore(t)
	f = &fakeSender{}
	sv = unread(liveStalled(st, f, &now))
	blind := waitingSession("sess-blind")
	blind.Size = -1
	run(t, sv, blind)
	if len(f.sent) != 0 || f.checks != 0 || len(events(t, st, KindCase)) != 0 || len(events(t, st, KindAct)) != 1 {
		t.Fatalf("no place: sent %q checks %d cases %+v acts %+v", f.sent, f.checks, events(t, st, KindCase), events(t, st, KindAct))
	}
}

func TestAStalledTurnPastItsStaleHourIsLeftAlone(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(30*time.Minute + DefaultParams.Stale + time.Minute) // its time came over an hour ago: nothing watched then
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-old")
	run(t, sv, s)
	if len(f.sent) != 0 || f.checks != 0 {
		t.Fatalf("sent %q checks %d", f.sent, f.checks)
	}
	if n, ok := note(t, st, AskNote+s.Key); ok {
		t.Fatalf("a needs note: %+v", n)
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
	y.request() // Claude Code's own retry: the wait starts again from it
	if a, err := claude.Answered(y.path, fi.Size(), sent); !a || err != nil {
		t.Fatalf("a retry: %v %v", a, err)
	}
	z := &transcript{t: t, path: filepath.Join(dir, "z.jsonl"), at: t0}
	z.user("go").request()
	sent = z.at
	fi, _ = os.Stat(z.path)
	z.say("Done.")
	if a, err := claude.Answered(z.path, fi.Size(), sent); !a || err != nil {
		t.Fatalf("a reply: %v %v", a, err)
	}
}
