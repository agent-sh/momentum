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
// writes what Claude Code writes for one: the interruption and the queued message as the next prompt, a few
// milliseconds apart, in either order (promptFirst).
type queueing struct {
	fakeSender
	x           *transcript
	now         *time.Time
	promptFirst bool
}

func (f *queueing) Interrupt(ctx context.Context, t act.Target, text string, still func() error) (act.Path, error) {
	if _, err := f.fakeSender.Interrupt(ctx, t, text, still); err != nil {
		return act.Tmux, err
	}
	stamp := func(d time.Duration) string { return f.now.Add(time.Second + d).Format(time.RFC3339Nano) }
	escape := map[string]any{"type": "user", "timestamp": stamp(0),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}}
	prompt := map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "queued", "timestamp": stamp(30 * time.Millisecond),
		"message": map[string]any{"role": "user", "content": text}}
	if f.promptFirst { // then the request of the turn the prompt opened, as Claude Code writes it in this order
		escape["timestamp"] = stamp(37 * time.Millisecond)
		f.x.add(prompt).add(escape).add(map[string]any{"type": "api-request", "querySource": "repl_main_thread",
			"timestamp": stamp(61 * time.Millisecond)})
	} else {
		f.x.add(escape).add(prompt)
	}
	f.x.at = f.now.Add(2 * time.Second)
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
	// Claude Code writes the interruption and the queued prompt in either order: the Escape is momentum's both ways.
	for _, promptFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("prompt first %v", promptFirst), func(t *testing.T) { loopStalledInterrupted(t, promptFirst) })
	}
}

func loopStalledInterrupted(t *testing.T, promptFirst bool) {
	x, sent, scan := stalledLoop(t)
	st := testStore(t)
	now := sent.Add(29 * time.Minute)
	f := &queueing{x: x, now: &now, promptFirst: promptFirst}
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

func TestAnInterruptWrittenPromptFirstKeepsItsTurnThroughARetry(t *testing.T) {
	// Claude Code writes the queued prompt, then the Escape's interruption: the message's turn runs on, through a
	// retry's backoff too, when the retry's error has answered the request and its own request is not out yet.
	x, sent, scan := stalledLoop(t)
	st := testStore(t)
	now := sent.Add(30 * time.Minute)
	f := &queueing{x: x, now: &now, promptFirst: true}
	sv := liveStalled(st, f, &now)
	run(t, sv, scan(now))
	now = now.Add(time.Minute)
	run(t, sv, scan(now))
	x.at = now
	x.add(map[string]any{"type": "system", "subtype": "api_error", "level": "error"})
	now = now.Add(30 * time.Second)
	s := scan(now)
	run(t, sv, s)
	if r := results(t, st); !s.TurnOpen || s.Phase != sessions.Working || !r[Delivered] || r[Otherwise] {
		t.Fatalf("in the retry's backoff: open %v phase %s outcomes %v", s.TurnOpen, s.Phase, r)
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
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s := waitingSession("sess-idle")
	s.Since = t0.Add(-5 * time.Minute) // the turn began before its request: the notes go by the request
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
	// No reply, and the first look at the idle screen: its owner may have just taken the turn back, so the rule
	// holds for the owner window from this look.
	answered = false
	now = now.Add(time.Minute)
	idleAt := now
	run(t, sv, s)
	now = idleAt.Add(DefaultParams.OwnerWindow - time.Minute)
	run(t, sv, s)
	if len(f.sent) != 0 || len(events(t, st, KindWait)) != 2 {
		t.Fatalf("within the window of its first idle look: sent %q waits %+v", f.sent, events(t, st, KindWait))
	}
	// Past it: the message goes in the plain way, as the next prompt.
	now = idleAt.Add(DefaultParams.OwnerWindow + time.Second)
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
		!strings.Contains(n.Title, "as its next prompt") || !strings.Contains(n.Detail, StalledText) {
		t.Fatalf("decided note %+v %v", n, ok)
	}
	// A plain send whose paste may sit on the input line opens a case, and says nothing was resumed.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrIdle, send: act.ErrUnstable}
	sv = liveStalled(st, f, &now)
	sv.Answered = func(*sessions.Session, time.Time) (bool, error) { return false, nil }
	idleAt = now
	run(t, sv, s)
	now = idleAt.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	if ns, _ := st.Notes(context.Background()); len(events(t, st, KindCase)) != 1 || len(ns) != 1 || ns[0].Kind != store.NoteProblem {
		t.Fatalf("cases %+v notes %+v", events(t, st, KindCase), ns)
	}
}

func TestAnIdleScreensWaitIsTheRulesOwnTime(t *testing.T) {
	// The first idle look comes late in the hour (runs before it were skipped): the message still goes in, the
	// owner window after it, since the wait moves the rule's own time and so its stale hour.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrIdle}
	now := t0.Add(80 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s := waitingSession("sess-late-look")
	run(t, sv, s)
	now = t0.Add(80*time.Minute + DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	if len(f.sent) != 1 {
		t.Fatalf("a late first look: sent %q, skips %+v", f.sent, events(t, st, KindSkip))
	}
	// The message's own request stalls on an idle screen too: its first look waits again, once per request.
	lands(s, StalledText, now.Add(time.Second), false)
	s.RequestAt = now.Add(2 * time.Second)
	now = s.RequestAt.Add(30 * time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 {
		t.Fatalf("the second request's first idle look: sent %q", f.sent)
	}
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	if len(f.sent) != 2 {
		t.Fatalf("past its window: sent %q", f.sent)
	}
	// A draft its owner cleared on the idle screen: that look is its first idle look too, and its wait ends with
	// the hold of his clearing, not a window after it.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrDraft}
	now = t0.Add(31 * time.Minute)
	sv = unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s = waitingSession("sess-cleared")
	run(t, sv, s)
	f.checkInterrupt = act.ErrIdle // he cleared his draft: no turn running, an empty line
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s) // the check after the hold finds it cleared: him there just now
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	if len(f.sent) != 1 || len(events(t, st, KindWait)) != 1 {
		t.Fatalf("after the hold of his clearing: sent %q, waits %+v", f.sent, events(t, st, KindWait))
	}
	// He clears his draft while the turn still runs, then takes the turn back with Escape: the first look at the
	// idle screen still waits the owner window, since the clearing came before the Escape.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrDraft}
	now = t0.Add(31 * time.Minute)
	sv = unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s = waitingSession("sess-cleared-then-escape")
	run(t, sv, s)
	f.checkInterrupt = nil // cleared, the turn still running
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	f.checkInterrupt = act.ErrIdle // his Escape: no turn running, an empty line
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	lines, err := sv.Run(context.Background(), []*sessions.Session{s}, map[int]uint64{42: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 || len(lines) != 1 || lines[0].Verdict != Wait || lines[0].Err != "" {
		t.Fatalf("the first idle look after his Escape: sent %q, lines %+v", f.sent, lines)
	}
	now = now.Add(DefaultParams.OwnerWindow - time.Minute)
	run(t, sv, s)
	if len(f.sent) != 0 {
		t.Fatalf("within its window: sent %q", f.sent)
	}
	now = now.Add(2 * time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 {
		t.Fatalf("past its window: sent %q", f.sent)
	}
}

func TestAClearedLineTakesNoSecondIdleWait(t *testing.T) {
	// The first idle look at 31, then his drafts on the idle screen hold the rule; he clears the line late and
	// leaves. The look that finds it cleared is no first idle look: the wait it began stays at 31, the stale hour
	// with it, and he held the stall into that hour, so neither a message nor a note.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrIdle}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s := waitingSession("sess-cleared-twice")
	run(t, sv, s)
	f.checkInterrupt = act.ErrDraft
	for now.Before(t0.Add(100 * time.Minute)) {
		now = now.Add(time.Minute)
		run(t, sv, s)
	}
	f.checkInterrupt = act.ErrIdle // cleared, on the idle screen
	for now.Before(t0.Add(3 * time.Hour)) {
		now = now.Add(time.Minute)
		run(t, sv, s)
	}
	marks := 0
	for _, e := range events(t, st, KindWait) {
		if strings.Contains(e.SourceKey, "#idle@") {
			marks++
		}
	}
	if n, ok := note(t, st, AskNote+s.Key); len(f.sent) != 0 || ok || marks != 1 {
		t.Fatalf("sent %q, needs note %+v %v, idle marks %d", f.sent, n, ok, marks)
	}
}

func TestThePlainWayTakesNoInterruptionForItsOwn(t *testing.T) {
	// The plain way presses no Escape: an interruption the transcript shows by its prompt (its owner's Escape on
	// the turn it opened) is his, and stays his presence.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrIdle}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	his := t0.Add(46*time.Minute + 500*time.Millisecond)
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) { return true, his, nil }
	s := waitingSession("sess-his-escape")
	run(t, sv, s)
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	var sent map[string]any
	for _, e := range events(t, st, KindAct) {
		if m := metaMap(e); m["sent"] == true {
			sent = m
		}
	}
	if len(f.sent) != 1 || sent == nil || sent["interrupted"] != false || sent["interrupted_at"] != nil {
		t.Fatalf("sent %q, its event %+v", f.sent, sent)
	}
}

func TestAPlainSendTheTranscriptDoesNotShowOpensACase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrIdle} // no turn running on the screen, though one may run unseen
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return false, time.Time{}, nil
	}
	defer func(w time.Duration) { promptedWait = w }(promptedWait)
	promptedWait = 10 * time.Millisecond
	s := waitingSession("sess-unseen")
	run(t, sv, s)
	now = now.Add(DefaultParams.OwnerWindow + time.Second)
	run(t, sv, s)
	ns, _ := st.Notes(context.Background())
	if len(f.sent) != 1 || len(events(t, st, KindCase)) != 1 || len(ns) != 1 || ns[0].Kind != store.NoteProblem {
		t.Fatalf("sent %q, cases %+v, notes %+v", f.sent, events(t, st, KindCase), ns)
	}
}

func TestAStallsNeedsNoteNeverStandsBesideItsCase(t *testing.T) {
	// A dialog at the first look, then a turn running whose interrupt may have left the message on its input line:
	// a case. At the stale hour the refusal on record must not add a needs note to the case's problem note.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrDialog}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-cased")
	run(t, sv, s)
	f.checkInterrupt, f.interrupt = nil, act.ErrUnstable
	now = now.Add(time.Minute)
	run(t, sv, s)
	now = t0.Add(30*time.Minute + DefaultParams.Stale + DefaultParams.OwnerWindow + time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); ok || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("needs note %+v beside cases %+v", n, events(t, st, KindCase))
	}
	// No path at first, then a path whose interrupt is unstable: the run that opens the case takes the note back.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrNoPath}
	now = t0.Add(31 * time.Minute)
	sv = unread(liveStalled(st, f, &now))
	s = waitingSession("sess-found-cased")
	run(t, sv, s)
	f.checkInterrupt, f.interrupt = nil, act.ErrUnstable
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); !ok || n.Done.IsZero() || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("needs note %+v %v, cases %+v", n, ok, events(t, st, KindCase))
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
	if acts := events(t, st, KindAct); len(acts) != 1 || metaMap(acts[0])["no_path"] != true || !strings.HasSuffix(n.Title, act.ErrNoPath.Error()) {
		t.Fatalf("its refusal %+v, title %q", acts, n.Title)
	}
	// A note an older build wrote for the same request, standing with another title, takes this one.
	if _, err := st.PutNote(context.Background(), store.Note{ID: AskNote + s.Key, Kind: store.NoteNeeds, Title: "Resume it?", At: t0}); err != nil {
		t.Fatal(err)
	}
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); !strings.HasPrefix(n.Title, "momentum could not resume ") {
		t.Fatalf("the older note stands as it was: %+v", n)
	}
	// A look that finds its pane after all, with a dialog on it: the note stands, titled by the last look.
	f.checkInterrupt = act.ErrDialog
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); !n.Done.IsZero() || !strings.HasSuffix(n.Title, act.ErrDialog.Error()) {
		t.Fatalf("after a look at a dialog: %+v", n)
	}
	f.checkInterrupt = act.ErrNoPath
	now = now.Add(time.Minute)
	run(t, sv, s)
	// Its owner comes by (a local command): the rule holds, and the note stands, kept by the mark for the request.
	s.ControlAt = now
	now = now.Add(time.Minute)
	run(t, sv, s)
	// Past its stale hour, nothing tries it, and the note stands.
	now = t0.Add(30*time.Minute + DefaultParams.Stale + 2*time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, AskNote+s.Key); !n.Done.IsZero() || len(events(t, st, KindSkip)) != 1 {
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
	// A path that comes after one look found none (a pane the listing missed): the run that sends takes the note
	// back with it.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrNoPath}
	now = t0.Add(31 * time.Minute)
	sv = unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s = waitingSession("sess-found")
	run(t, sv, s)
	f.checkInterrupt = nil
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); len(f.sent) != 1 || !ok || n.Done.IsZero() {
		t.Fatalf("sent %q, note %+v %v", f.sent, n, ok)
	}
}

func TestAStalledTurnItsLooksKeepRefusingGetsANeedsNoteAtItsStaleHour(t *testing.T) {
	st := testStore(t)
	// A dialog on its screen first, then a screen neither check reads plainly: no turn running by the interrupt
	// check, a running row by the plain one, as on a pane too narrow for the row's time.
	f := &fakeSender{checkInterrupt: act.ErrDialog}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-unsure")
	run(t, sv, s)
	f.checkInterrupt, f.check = act.ErrIdle, act.ErrBusy
	for now.Before(t0.Add(85 * time.Minute)) {
		now = now.Add(time.Minute)
		run(t, sv, s)
	}
	s.ControlAt = now // its owner comes by just before its hour runs out
	for now.Before(t0.Add(30*time.Minute + DefaultParams.Stale + 2*time.Minute)) {
		now = now.Add(time.Minute)
		run(t, sv, s)
	}
	if n, ok := note(t, st, AskNote+s.Key); ok || len(f.sent) != 0 || len(events(t, st, KindCase)) != 0 {
		t.Fatalf("before the owner window since he came by: note %+v, sent %q, cases %+v", n, f.sent, events(t, st, KindCase))
	}
	// Its hour ran out and he has been gone the owner window: momentum stopped looking, and says what it could not
	// do and why, by the last look.
	now = s.ControlAt.Add(DefaultParams.OwnerWindow + time.Minute)
	run(t, sv, s)
	n, ok := note(t, st, AskNote+s.Key)
	if !ok || !n.Done.IsZero() || n.Title != "momentum could not resume sess-unsure: "+errUnsure.Error() || !n.At.Equal(t0) {
		t.Fatalf("needs note %+v %v", n, ok)
	}
	// A look its owner let through before then ends the refusals: no note.
	st = testStore(t)
	f = &fakeSender{checkInterrupt: act.ErrDialog}
	now = t0.Add(31 * time.Minute)
	sv = unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s = waitingSession("sess-let-through")
	run(t, sv, s)
	f.checkInterrupt = nil
	now = now.Add(time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 {
		t.Fatalf("after the dialog: sent %q", f.sent)
	}
	s.RequestAt = now // the message's own request
	now = t0.Add(30*time.Minute + DefaultParams.Stale + 2*time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); ok {
		t.Fatalf("a note after a look went through: %+v", n)
	}
}

func TestALookThatWentThroughEndsTheRefusalsBeforeIt(t *testing.T) {
	// A dialog at the first look; the next finds the screen idle and clean, and its wait begins (the rule's time
	// moves to the owner window after it, and its stale hour with it); then its owner keeps coming by until that
	// hour. Nothing stood in momentum's way at the last look: no needs note.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrDialog}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	s := waitingSession("sess-went-through")
	run(t, sv, s)
	f.checkInterrupt = act.ErrIdle
	now = now.Add(time.Minute)
	run(t, sv, s)
	stale := now.Add(DefaultParams.OwnerWindow + DefaultParams.Stale)
	for now.Before(stale.Add(DefaultParams.OwnerWindow + 2*time.Minute)) {
		if now.Before(stale) {
			s.ControlAt = now // a local command now and then: he is there
		}
		now = now.Add(5 * time.Minute)
		run(t, sv, s)
	}
	if n, ok := note(t, st, AskNote+s.Key); ok || len(f.sent) != 0 {
		t.Fatalf("needs note %+v, sent %q", n, f.sent)
	}
}

func TestARefusalAfterALookThatWentThroughStandsAgain(t *testing.T) {
	// A dialog; the first idle look goes through and its wait begins; then the same dialog at every look until
	// the stale hour: the note says so, though the reason is the one on record before the look that went through.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrDialog}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-dialog-again")
	run(t, sv, s)
	f.checkInterrupt = act.ErrIdle
	now = now.Add(time.Minute)
	run(t, sv, s)
	f.checkInterrupt = act.ErrDialog
	stale := now.Add(DefaultParams.OwnerWindow + DefaultParams.Stale)
	for !now.After(stale.Add(time.Minute)) {
		now = now.Add(time.Minute)
		run(t, sv, s)
	}
	if n, ok := note(t, st, AskNote+s.Key); !ok || !n.Done.IsZero() || !strings.HasSuffix(n.Title, act.ErrDialog.Error()) || len(f.sent) != 0 {
		t.Fatalf("needs note %+v %v, sent %q", n, ok, f.sent)
	}
}

func TestADraftClearedWhileTheTurnRunsEndsItsRefusal(t *testing.T) {
	// Drafts at every look, then he clears the line while the turn still runs and leaves; the hold of his
	// clearing runs into the stale hour. He was there and left it as it is: no message, and no note naming a draft.
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrDraft}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-cleared-late")
	for now.Before(t0.Add(70 * time.Minute)) { // looks at 31, 46 and 61 meet the draft
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	f.checkInterrupt = nil // the line empty, the turn still running on its screen: the look at 76 finds it cleared
	for now.Before(t0.Add(30*time.Minute + DefaultParams.Stale + 2*DefaultParams.OwnerWindow)) {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if n, ok := note(t, st, AskNote+s.Key); ok || len(f.sent) != 0 {
		t.Fatalf("needs note %+v, sent %q", n, f.sent)
	}
}

func TestAStalledNeedsNoteItsOwnerResolvedStaysResolved(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrNoPath}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-resolved")
	run(t, sv, s)
	if _, _, err := st.DoneNote(context.Background(), AskNote+s.Key, now); err != nil {
		t.Fatal(err)
	}
	f.checkInterrupt = act.ErrDialog // a new last look, and so a new title
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); !ok || n.Done.IsZero() {
		t.Fatalf("a note he resolved stands again: %+v %v", n, ok)
	}
}

func TestAStalledCasesProblemNoteStandsThroughARetry(t *testing.T) {
	// The interrupt's paste may sit on the input line: a case and its problem note. Claude Code then retries the
	// request by itself, and the retry gets no reply either: the turn does not work again, and the note stands.
	st := testStore(t)
	f := &fakeSender{interrupt: act.ErrUnstable}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-retry-after-case")
	run(t, sv, s)
	if n, ok := note(t, st, ProblemNote+s.Key); !ok || !n.Done.IsZero() {
		t.Fatalf("problem note %+v %v", n, ok)
	}
	// A scan in the retry's backoff: its error answered the request, and the retry's own request is not out yet.
	s.RequestAt = time.Time{}
	now = now.Add(9 * time.Minute)
	run(t, sv, s)
	s.RequestAt = t0.Add(40 * time.Minute)
	for now.Before(t0.Add(3 * time.Hour)) {
		now = now.Add(10 * time.Minute)
		run(t, sv, s)
	}
	if n, _ := note(t, st, ProblemNote+s.Key); !n.Done.IsZero() {
		t.Fatalf("the problem note went with the retry: %+v", n)
	}
	// The reply comes and the turn goes on with a call: it works again.
	s.RequestAt, s.Now = time.Time{}, "Bash: make test"
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, ProblemNote+s.Key); n.Done.IsZero() {
		t.Fatalf("still stands with a call in flight: %+v", n)
	}
}

func TestADraftThatStaysOnAStalledTurnEndsInANeedsNote(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{checkInterrupt: act.ErrDraft}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	s := waitingSession("sess-draft-stays")
	for now.Before(t0.Add(30*time.Minute + DefaultParams.Stale)) { // its owner is at the pane: the rule holds
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if n, ok := note(t, st, AskNote+s.Key); ok || len(f.sent) != 0 {
		t.Fatalf("while it holds: note %+v, sent %q", n, f.sent)
	}
	for i := 0; i < 20; i++ { // past its stale hour, once the owner window passed since the last sighting
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if n, ok := note(t, st, AskNote+s.Key); !ok || !strings.HasSuffix(n.Title, act.ErrDraft.Error()) || len(f.sent) != 0 {
		t.Fatalf("needs note %+v %v, sent %q", n, ok, f.sent)
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
	if ns, _ := st.Notes(context.Background()); len(ns) != 1 || ns[0].Kind != store.NoteProblem {
		t.Fatalf("notes %+v: nothing went in", ns)
	}
	// A refusal before anything was typed (the turn ended on its own between the check and the paste) passes by
	// itself.
	st = testStore(t)
	e := &ended{}
	sv = unread(liveStalled(st, e, &now))
	run(t, sv, s)
	if ns, _ := st.Notes(context.Background()); len(events(t, st, KindCase)) != 0 || len(e.sent) != 0 || len(events(t, st, KindAct)) != 1 || len(ns) != 0 {
		t.Fatalf("cases %+v sent %q acts %+v notes %+v", events(t, st, KindCase), e.sent, events(t, st, KindAct), ns)
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

func TestAnInterruptReadsOnForItsEscapesRecord(t *testing.T) {
	// The message shows as the prompt before its interruption is on record: the run reads on for it a moment.
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(31 * time.Minute)
	sv := unread(liveStalled(st, f, &now))
	looks := 0
	escaped := t0.Add(31*time.Minute + time.Second)
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		if looks++; looks < 3 {
			return true, time.Time{}, nil
		}
		return true, escaped, nil
	}
	defer func(w time.Duration) { interruptWait = w }(interruptWait)
	interruptWait = 5 * time.Second
	run(t, sv, waitingSession("sess-escape-late"))
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["interrupted_at"] != float64(escaped.UnixMilli()) {
		t.Fatalf("acts %+v after %d looks", acts, looks)
	}
	// It shows late in the wait, its interruption not on record yet: the read-on runs past the wait, and it went in.
	st = testStore(t)
	sv = unread(liveStalled(st, &fakeSender{}, &now))
	start := time.Now()
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return time.Since(start) > 150*time.Millisecond, time.Time{}, nil
	}
	defer func(w time.Duration) { promptedWait = w }(promptedWait)
	promptedWait = 200 * time.Millisecond
	interruptWait = 300 * time.Millisecond
	run(t, sv, waitingSession("sess-escape-shown-late"))
	if acts := events(t, st, KindAct); len(acts) != 1 || metaMap(acts[0])["sent"] != true || len(events(t, st, KindCase)) != 0 {
		t.Fatalf("shown late: acts %+v cases %+v", acts, events(t, st, KindCase))
	}
	// It never comes: the message still went in, with no interruption on record, and no case.
	st = testStore(t)
	sv = unread(liveStalled(st, &fakeSender{}, &now))
	sv.Prompted = func(context.Context, *sessions.Session, string) (bool, time.Time, error) {
		return true, time.Time{}, nil
	}
	interruptWait = 300 * time.Millisecond
	run(t, sv, waitingSession("sess-escape-never"))
	if acts := events(t, st, KindAct); len(acts) != 1 || metaMap(acts[0])["sent"] != true || metaMap(acts[0])["interrupted"] != false ||
		len(events(t, st, KindCase)) != 0 {
		t.Fatalf("acts %+v cases %+v", acts, events(t, st, KindCase))
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
	// The interruption written a moment after the prompt is its Escape's too; one long after it, or after another
	// user record, is not.
	for _, c := range []struct {
		name  string
		after time.Duration
		other bool
		want  bool
	}{{"just after", 7 * time.Millisecond, false, true}, {"long after", 30 * time.Second, false, false},
		{"after another record", 7 * time.Millisecond, true, false}} {
		w := &transcript{t: t, path: filepath.Join(dir, "w-"+strings.ReplaceAll(c.name, " ", "-")+".jsonl"), at: t0}
		w.user("old turn")
		fi, _ := os.Stat(w.path)
		prompted := t0.Add(time.Minute)
		w.add(map[string]any{"type": "user", "promptSource": "queued", "timestamp": prompted.Format(time.RFC3339Nano), "message": map[string]any{"content": StalledText}})
		if c.other {
			w.add(map[string]any{"type": "user", "timestamp": prompted.Format(time.RFC3339Nano), "message": map[string]any{"content": "a typed line"}})
		}
		w.add(map[string]any{"type": "user", "timestamp": prompted.Add(c.after).Format(time.RFC3339Nano),
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
		found, interrupted, err := claude.Prompted(w.path, fi.Size(), StalledText)
		if !found || interrupted.IsZero() == c.want || err != nil {
			t.Fatalf("%s: found %v interrupted %v %v", c.name, found, interrupted, err)
		}
	}
	// An interruption on record before the prompt is its Escape's; one right after it is someone else's then.
	v := &transcript{t: t, path: filepath.Join(dir, "v.jsonl"), at: t0}
	v.user("old turn")
	fi, _ = os.Stat(v.path)
	before := t0.Add(time.Minute)
	v.add(map[string]any{"type": "user", "timestamp": before.Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	v.add(map[string]any{"type": "user", "promptSource": "queued", "timestamp": before.Add(30 * time.Millisecond).Format(time.RFC3339Nano),
		"message": map[string]any{"content": StalledText}})
	v.add(map[string]any{"type": "user", "timestamp": before.Add(500 * time.Millisecond).Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	if found, interrupted, err := claude.Prompted(v.path, fi.Size(), StalledText); !found || !interrupted.Equal(before) || err != nil {
		t.Fatalf("an interruption before and one after: found %v interrupted %v %v", found, interrupted, err)
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
