package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/act"
	"github.com/agent-sh/momentum/sessions/claude"
)

// Dry is the experiment tag of what a rule that runs dry would have sent: recorded, never typed.
const Dry = "supervise-dry"

// Kinds of the events a supervise run writes, source store.Supervise.
const (
	KindAct     = "act"     // a message being sent, sent, refused by the terminal, or (dry) one that would have been sent
	KindWait    = "wait"    // a rule applies and acts later, or waits for access to come back
	KindHold    = "hold"    // a rule applies, and the owner was at the session recently
	KindSkip    = "skip"    // a rule would apply, but the session is not momentum's to act on, or the stop is stale
	KindOutcome = "outcome" // what came of a message
	KindCase    = "case"    // a rule used up its tries, or a message could not land: a case for the loop
	KindPresent = "present" // its owner seen at the session's pane: a draft on its input line, a tmux mode, a line he cleared
)

// Outcomes of a message.
const (
	Delivered    = "delivered"         // a scan showed the message as a user turn of the session
	NotDelivered = "not delivered"     // it did not show within DeliveryWait
	Worked       = "worked"            // the turn it opened did work, tool calls, and stopped without an error
	FailedAgain  = "failed again"      // the turn it opened stopped the same way
	Otherwise    = "stopped otherwise" // the turn it opened stopped another way, without work
)

// DeliveryWait is how long a message has to show up as the session's user turn, two scans of the sessions timer
// and some, before it counts as not delivered.
const DeliveryWait = 3 * time.Minute

// historyDays is how far back a session's supervise events are read when its owner never wrote in it.
const historyDays = 30

// Sender types a message into a session: act.Sender, or a test's.
type Sender interface {
	Check(ctx context.Context, t act.Target) (act.Path, error)
	Send(ctx context.Context, t act.Target, text string) (act.Path, error)
	// CheckInterrupt is the checks Interrupt makes before it types anything: act.ErrIdle when no turn runs on the
	// screen.
	CheckInterrupt(ctx context.Context, t act.Target) (act.Path, error)
	// Interrupt types the message into a turn that runs, which holds it in its queue, and ends the turn, which
	// sends it: a turn stalled on a request with no reply. still is asked before the paste and before the Escape.
	Interrupt(ctx context.Context, t act.Target, text string, still func() error) (act.Path, error)
}

// errTakenBack is the stalled rule's refusal when the screen shows no turn running while the transcript still
// waits on the request: the turn was taken back (its owner's Escape takes one back without a record), and the
// message would resume work its owner stopped. It passes when the session moves on.
var errTakenBack = errors.New("its screen shows the turn over while its transcript still waits on the request")

// errAnswered is a stalled request that something answered since the scan: the turn went on by itself.
var errAnswered = errors.New("its transcript shows the request answered since the scan")

// Supervisor runs the rules over the sessions one scan found, sends what the live rules decide, and records
// every decision, every message and what came of it as an event.
type Supervisor struct {
	Store  *store.Store
	Sender Sender
	Params Params
	Live   map[string]bool // the rules that send; the others record what they would have sent (Dry)
	// Probe tells when access came back after an access error; nil reads the store and Claude Code's
	// credentials (ClaudeDir) when a session is on an access stop.
	Probe     Probe
	ClaudeDir string
	// Notice, when set, is a command run once for each case opened, with a line about the session as its last
	// argument: a passive notice for its owner. It must not be a path that wakes him.
	Notice []string
	Now    func() time.Time
	// Woke tells when the machine last woke from a suspend: a stalled request's wait counts from it when it came
	// later. nil asks systemd (lastWake).
	Woke func(ctx context.Context) time.Time
	// Prompted tells whether the session's transcript shows the message as the prompt of a turn since the scan,
	// after an interrupt put it there; nil reads the transcript (claude.Prompted), a few seconds at most.
	Prompted func(ctx context.Context, s *sessions.Session, text string) (found bool, interrupted time.Time, err error)
	// Answered tells whether the session's transcript shows, since the scan, a record that answers a request sent
	// at sent; nil reads the transcript (claude.Answered).
	Answered func(s *sessions.Session, sent time.Time) (bool, error)
}

// Line is one decision of a run, for the command's output.
type Line struct {
	Session, Name, Rule, Verdict, Text, Why string
	Sent                                    bool
	Err                                     string
	takenBack                               bool // the stalled turn's screen shows it taken back
}

func (sv *Supervisor) now() time.Time {
	if sv.Now != nil {
		return sv.Now()
	}
	return time.Now()
}

// Run decides for each session and acts. starts maps a session's pid to its process's start ticks, from the
// scan's snapshot, and ss must be the whole of a scan that worked: a session missing from it has ended. A
// session the scan read only in part is left alone. Then it keeps the report's notes about the sessions in step
// (notes). It returns the run's decisions.
func (sv *Supervisor) Run(ctx context.Context, ss []*sessions.Session, starts map[int]uint64) ([]Line, error) {
	var lines []Line
	probe := sv.Probe
	seen := map[string]*seenSession{}
	var woke *time.Time // asked once per run, and only when a session waits on a request
	for _, s := range ss {
		now := sv.now()
		if s.Partial {
			seen[s.Key] = nil // its facts may be short: decide on the next scan, and keep its notes as they are
			if err := sv.put(ctx, []store.Event{sv.note(s, KindSkip, "partial", now, "the scan read it only in part: decided on the next scan")}); err != nil {
				return lines, err
			}
			continue
		}
		marks, err := sv.Store.OwnerTurns(ctx, StoreSession(s), 20)
		if err != nil {
			return lines, err
		}
		// From the owner's last turn on is what the tries and the cases count; read at least the last 30 days.
		since := now.AddDate(0, 0, -historyDays)
		switch {
		case len(marks) == 0:
			since = time.Time{}
		case marks[0].At.Before(since):
			since = marks[0].At.Add(-time.Minute)
		}
		evs, err := sv.Store.SuperviseEvents(ctx, s.Key, since)
		if err != nil {
			return lines, err
		}
		h := history(evs)
		ownerAt := h.ownerTurn(marks)
		answered, err := sv.Store.AskAnswered(ctx, s.Key)
		if err != nil {
			return lines, err
		}
		presence := h.presence(s, ownerAt, answered)
		stop := StopOf(s, presence)
		if !stop.RequestAt.IsZero() {
			if woke == nil {
				w := sv.woke(ctx)
				woke = &w
			}
			stop.Woke = *woke
		}
		var put []store.Event
		resolved := sv.outcome(ctx, s, stop, &h, now, &put)
		if !resolved {
			put = append(put, sv.note(s, KindWait, "in-flight@"+h.last().key, now, "its last message has not shown up yet"))
		}
		if err := sv.put(ctx, put); err != nil {
			return lines, err
		}
		if !resolved {
			seen[s.Key] = nil
			continue
		}
		if probe == nil && isAccess(stop.Error) {
			p, err := StoreProbe(ctx, sv.Store, now.Add(-48*time.Hour), now.Add(time.Minute))
			if err != nil {
				return lines, err
			}
			probe = credentials{inner: p, path: filepath.Join(sv.ClaudeDir, ".credentials.json"), hold: sv.Params.OwnerWindow}
		}
		d := Decide(stop, h.tries(ownerAt), sv.Params, probe, now)
		if d.Rule != "" && d.Verdict != Case && d.Verdict != Skip && h.closed(d.Rule, ownerAt) {
			d.Verdict, d.Why = Case, "a case is open since the owner's last turn"
		}
		if d.Verdict == Skip && d.Rule == "" {
			d.Rule = planRules(stop, Tries{}, sv.Params, probe).Rule // the rule a guard keeps from acting, if any
		}
		entry := &seenSession{s: s, stop: stop, presence: presence}
		entry.ask, entry.asks = waitsOnOwner(s, stop, d, presence, sv.Params, false)
		seen[s.Key] = entry
		if d.Rule == "" {
			continue // no rule applies, or a guard and no rule would apply anyway: nothing was decided
		}
		if d.Verdict == Act {
			d.Text = act.SafeText(d.Text) // as it will be typed, and so as the session will record it
		}
		line := Line{Session: s.Key, Name: s.Name, Rule: d.Rule, Verdict: d.Verdict, Text: d.Text, Why: d.Why}
		switch d.Verdict {
		case Act:
			if !sv.Live[d.Rule] {
				e := sv.event(s, stop, d, KindAct, now, "")
				e.Experiment = Dry
				e.Summary = oneLine(fmt.Sprintf("%s: would send %q (try %d)", d.Rule, d.Text, d.Try))
				e.Meta = meta(map[string]any{"rule": d.Rule, "try": d.Try, "text": d.Text, "why": d.Why, "stop_at": stop.At.UnixMilli(),
					"error": stop.Error, "dry": true})
				if err := sv.put(ctx, []store.Event{e}); err != nil {
					return lines, err
				}
				break
			}
			if err := sv.send(ctx, s, stop, d, h, starts, &line); err != nil {
				return lines, err
			}
		case Case:
			if !h.closed(d.Rule, ownerAt) { // one case per rule until the owner comes back
				if err := sv.put(ctx, []store.Event{sv.openCase(ctx, s, stop, d.Rule, d.Why, d.Try, now)}); err != nil {
					return lines, err
				}
			}
		default:
			kind := map[string]string{Wait: KindWait, Hold: KindHold, Skip: KindSkip}[d.Verdict]
			e := sv.event(s, stop, d, kind, now, "")
			m := map[string]any{"rule": d.Rule, "try": d.Try, "why": d.Why, "stop_at": stop.At.UnixMilli(), "error": stop.Error}
			when := ""
			if !d.At.IsZero() {
				m["at"], when = d.At.UnixMilli(), " until "+d.At.Local().Format("15:04")
			}
			e.Meta = meta(m)
			e.Summary = oneLine(fmt.Sprintf("%s: %s%s: %s", d.Rule, d.Verdict, when, d.Why))
			if err := sv.put(ctx, []store.Event{e}); err != nil {
				return lines, err
			}
		}
		// A stalled turn the rule leaves to its owner (dry, taken back on its screen, or past its stale hour) waits on
		// him: only he can tell whether to resume it.
		if d.Rule == RuleStalled && (d.Verdict == Act && !sv.Live[d.Rule] || line.takenBack || d.Stale) {
			entry.ask, entry.asks = waitsOnOwner(s, stop, d, presence, sv.Params, true)
		}
		lines = append(lines, line)
	}
	return lines, sv.notes(ctx, seen)
}

// note is a supervise event about a session that is no rule's decision: a session left for the next scan, a
// message still in flight. what keys it, so the same note on the next run is the same event.
func (sv *Supervisor) note(s *sessions.Session, kind, what string, now time.Time, why string) store.Event {
	return store.Event{Source: store.Supervise, TS: now, Session: s.Key, Harness: s.Tool, Model: s.Model, Cwd: s.Cwd, Actor: store.ActorDetector,
		Kind: kind, SourceKey: fmt.Sprintf("%s@%d#%s", s.Key, s.Since.UnixMilli(), what), Summary: oneLine(why),
		Meta: meta(map[string]any{"why": why, "stop_at": s.Since.UnixMilli()})}
}

func (sv *Supervisor) put(ctx context.Context, evs []store.Event) error {
	if len(evs) == 0 {
		return nil
	}
	_, err := sv.Store.Put(ctx, evs)
	return err
}

// send types a live rule's message. It checks the message and the terminal first: a refusal there is recorded
// once per stop and try, the first time, and the next run checks again. Then it records the message as being sent
// before it types it, so a run that dies after the Enter leaves a record that stops the next run from typing it
// again, and records the result.
func (sv *Supervisor) send(ctx context.Context, s *sessions.Session, stop Stop, d Decision, h past, starts map[int]uint64, line *Line) error {
	t := act.TargetOf(s, starts[s.PID])
	m := map[string]any{"rule": d.Rule, "try": d.Try, "text": d.Text, "why": d.Why, "stop_at": stop.At.UnixMilli(), "error": stop.Error, "seq": s.Seq}
	// A stalled turn takes the message through its queue while it runs on its screen, and Escape ends it. One its
	// screen shows taken back is left alone: the message would resume work its owner stopped.
	interrupt := d.Rule == RuleStalled
	path, err := act.Path(""), act.CheckText(d.Text)
	switch {
	case err != nil:
	case interrupt:
		path, err = sv.Sender.CheckInterrupt(ctx, t)
		if errors.Is(err, act.ErrIdle) {
			err, line.takenBack = errTakenBack, true
		}
	default:
		path, err = sv.Sender.Check(ctx, t)
	}
	var evs []store.Event
	switch {
	case errors.Is(err, act.ErrDraft):
		evs = append(evs, sv.present(s, stop, "draft")) // every sighting: its owner is at the pane
	case errors.Is(err, act.ErrMode):
		evs = append(evs, sv.present(s, stop, "mode"))
	case err == nil && h.atPane[stop.At.UnixMilli()]:
		// the draft or the mode the last check met is gone: its owner cleared it just now, and is there
		line.Err = "its owner cleared the input line since the last check"
		return sv.put(ctx, []store.Event{sv.present(s, stop, "cleared")})
	}
	if err != nil {
		e := sv.event(s, stop, d, KindAct, sv.now(), "#refused")
		line.Err = err.Error()
		if !h.keys[e.SourceKey] { // on record once, the first time; the next run checks again
			m["path"], m["refused"] = string(path), err.Error()
			e.Meta = meta(m)
			e.Summary = oneLine(fmt.Sprintf("%s: not sent (%v): %q", d.Rule, err, d.Text))
			evs = append(evs, e)
		}
		return sv.put(ctx, append(evs, sv.cannot(ctx, s, stop, d, err)...))
	}
	at := sv.now()
	e := sv.event(s, stop, d, KindAct, at, fmt.Sprintf("@%d", at.UnixMilli()))
	m["sending"] = true
	if interrupt {
		m["interrupt"] = true // Escape, after the message went in its queue
	}
	e.Meta = meta(m)
	e.Summary = oneLine(fmt.Sprintf("%s: sending %q (try %d)", d.Rule, d.Text, d.Try))
	if err := sv.put(ctx, []store.Event{e}); err != nil {
		return err // nothing typed
	}
	if interrupt {
		path, err = sv.Sender.Interrupt(ctx, t, d.Text, func() error { return sv.unanswered(s, stop.RequestAt) })
		if err == nil {
			err = sv.prompted(ctx, s, d.Text, m)
		}
	} else {
		path, err = sv.Sender.Send(ctx, t, d.Text)
	}
	delete(m, "sending")
	m["path"] = string(path)
	if err != nil {
		m["refused"], line.Err = err.Error(), err.Error()
		e.Summary = oneLine(fmt.Sprintf("%s: not sent (%v): %q", d.Rule, err, d.Text))
		if errors.Is(err, act.ErrUnstable) {
			// the paste may sit on the input line, or have gone in: type nothing more at this stop
			m["unstable"] = true
		}
		evs = sv.cannot(ctx, s, stop, d, err)
	} else {
		m["sent"], line.Sent = true, true
		e.Summary = oneLine(fmt.Sprintf("%s: sent %q (try %d) by %s", d.Rule, d.Text, d.Try, path))
	}
	e.Meta = meta(m)
	return sv.put(ctx, append([]store.Event{e}, evs...))
}

// cannot is the case a refusal opens, if it is one that will not pass by itself: a paste that may sit on the input
// line, a session nothing can type into, a harness whose input line act does not know, or a message act will not
// type. A dialog, a draft or a busy turn passes, and the next run checks again.
func (sv *Supervisor) cannot(ctx context.Context, s *sessions.Session, stop Stop, d Decision, err error) []store.Event {
	switch {
	case errors.Is(err, act.ErrUnstable):
		return []store.Event{sv.openCase(ctx, s, stop, d.Rule, "pasted, not known to be submitted: "+err.Error(), d.Try, sv.now())}
	case errors.Is(err, act.ErrNoPath), errors.Is(err, act.ErrNoInput) && s.Tool != "claude" && s.Tool != "codex":
		return []store.Event{sv.openCase(ctx, s, stop, d.Rule, "no way to type into it: "+err.Error(), d.Try, sv.now())}
	case errors.Is(err, act.ErrMessage), errors.Is(err, act.ErrCommand), errors.Is(err, act.ErrNoMessage):
		return []store.Event{sv.openCase(ctx, s, stop, d.Rule, "the message cannot be typed: "+err.Error(), d.Try, sv.now())}
	}
	return nil
}

// present records its owner seen at the session's pane at a stop: state is draft or mode while the check met one,
// and cleared once a check found the line empty after it. Each sighting moves its time.
func (sv *Supervisor) present(s *sessions.Session, stop Stop, state string) store.Event {
	why := map[string]string{"draft": "a draft on its input line: its owner is at the pane",
		"mode": "a tmux mode owns its keyboard: its owner is at the pane", "cleared": "its owner cleared the input line"}[state]
	return store.Event{Source: store.Supervise, TS: sv.now(), Session: s.Key, Harness: s.Tool, Model: s.Model, Cwd: s.Cwd,
		Actor: store.ActorDetector, Kind: KindPresent, SourceKey: fmt.Sprintf("%s@%d#present", s.Key, stop.At.UnixMilli()), Summary: why,
		Meta: meta(map[string]any{"state": state, "why": why, "stop_at": stop.At.UnixMilli()})}
}

// event is a supervise event about one session and one stop, keyed so that the same decision about the same
// stop, made again on the next run, is the same event; a message's key adds when it was sent.
func (sv *Supervisor) event(s *sessions.Session, stop Stop, d Decision, kind string, now time.Time, suffix string) store.Event {
	actor := store.ActorDetector
	if kind == KindAct {
		actor = store.ActorAgent
	}
	e := store.Event{Source: store.Supervise, TS: now, Session: s.Key, Harness: s.Tool, Model: s.Model, Cwd: s.Cwd, Actor: actor, Kind: kind,
		SourceKey: fmt.Sprintf("%s@%d#%s#%s#%d%s", s.Key, stop.At.UnixMilli(), d.Rule, kind, d.Try, suffix)}
	if s.Transcript != "" && s.Size >= 0 {
		e.Ref = fmt.Sprintf("%s@%d", s.Transcript, s.Size)
	}
	return e
}

// openCase is a case about one stop of one rule, for one reason; its notice goes out once.
func (sv *Supervisor) openCase(ctx context.Context, s *sessions.Session, stop Stop, rule, why string, tries int, now time.Time) store.Event {
	reason := strings.SplitN(why, ":", 2)[0]
	e := store.Event{Source: store.Supervise, TS: now, Session: s.Key, Harness: s.Tool, Model: s.Model, Cwd: s.Cwd, Actor: store.ActorDetector,
		Kind: KindCase, SourceKey: fmt.Sprintf("case:%s:%s:%d:%s", s.Key, rule, stop.At.UnixMilli(), reason)}
	e.Summary = oneLine(fmt.Sprintf("%s: case: %s: %s", rule, firstNonEmpty(s.Name, s.Key), why))
	m := map[string]any{"family": "sessions", "rule": rule, "why": why, "tries": tries, "stop_at": stop.At.UnixMilli(), "error": stop.Error}
	if !sv.seen(ctx, e) { // a new case: one problem note, and the notice, once
		title := fmt.Sprintf("momentum could not continue %s (%s)", sessionName(s), rule)
		detail := why + "."
		if last := firstNonEmpty(stop.Error, Asked(stop.Words), stop.Words); last != "" {
			detail += " It stopped on: " + clip(last, 160)
		}
		if _, err := sv.Store.PutNote(ctx, store.SessionProblem(s.Key, title, detail, now)); err != nil {
			m["note"] = err.Error()
		}
		if len(sv.Notice) > 0 {
			args := append(append([]string{}, sv.Notice[1:]...), title)
			nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := exec.CommandContext(nctx, sv.Notice[0], args...).Run(); err != nil {
				m["notice"] = err.Error()
			}
			cancel()
		}
	}
	e.Meta = meta(m)
	return e
}

// seen reports whether the store already holds the event: a case's notice goes out once.
func (sv *Supervisor) seen(ctx context.Context, e store.Event) bool {
	evs, err := sv.Store.SuperviseEvents(ctx, e.Session, time.Time{})
	if err != nil {
		return true
	}
	for _, x := range evs {
		if x.SourceKey == e.SourceKey {
			return true
		}
	}
	return false
}

// sent is one message momentum typed, or began to type, into a session, from its event.
type sent struct {
	at     time.Time
	rule   string
	try    int
	seq    int
	stopAt time.Time
	key    string
	text   string
	result map[string]bool // its outcomes so far
}

type past struct {
	sends     []sent         // sent or being sent, oldest first
	cases     []caseRec      // cases opened, oldest first
	texts     []string       // every text momentum typed, or may have typed, into the session, folded
	presentAt time.Time      // when its owner was last seen at the pane: a draft, a tmux mode, a line he cleared
	atPane    map[int64]bool // the stops (their ms) whose last check met a draft or a mode
	keys      map[string]bool
	escapes   []time.Time // the interruptions momentum's own Escapes left in the transcript
}

type caseRec struct {
	at     time.Time
	rule   string
	stopAt time.Time
}

func history(evs []store.Event) past {
	h := past{keys: map[string]bool{}, atPane: map[int64]bool{}}
	outcomes := map[string]map[string]bool{}
	for _, e := range evs {
		h.keys[e.SourceKey] = true
		var m map[string]any
		json.Unmarshal([]byte(e.Meta), &m)
		switch e.Kind {
		case KindAct:
			if e.Experiment != "" {
				continue // what a dry rule would have sent
			}
			sending, _ := m["sending"].(bool) // a run that died after it began to type
			ok, _ := m["sent"].(bool)
			if unstable, _ := m["unstable"].(bool); ok || sending || unstable {
				h.texts = append(h.texts, fold(str(m["text"])))
				if at := num(m["interrupted_at"]); at > 0 {
					h.escapes = append(h.escapes, time.UnixMilli(int64(at)))
				}
			}
			if !ok && !sending {
				continue
			}
			h.sends = append(h.sends, sent{at: e.TS, rule: str(m["rule"]), try: num(m["try"]), seq: num(m["seq"]), key: e.SourceKey,
				stopAt: time.UnixMilli(int64(num(m["stop_at"]))), text: fold(str(m["text"]))})
		case KindPresent:
			if e.TS.After(h.presentAt) {
				h.presentAt = e.TS
			}
			state := str(m["state"])
			h.atPane[int64(num(m["stop_at"]))] = state == "draft" || state == "mode"
		case KindOutcome:
			of := str(m["of"])
			if outcomes[of] == nil {
				outcomes[of] = map[string]bool{}
			}
			outcomes[of][str(m["result"])] = true
		case KindCase:
			h.cases = append(h.cases, caseRec{at: e.TS, rule: str(m["rule"]), stopAt: time.UnixMilli(int64(num(m["stop_at"])))})
		}
	}
	for i := range h.sends {
		h.sends[i].result = outcomes[h.sends[i].key]
		if h.sends[i].result == nil {
			h.sends[i].result = map[string]bool{}
		}
	}
	return h
}

func (h past) last() *sent {
	if len(h.sends) == 0 {
		return nil
	}
	return &h.sends[len(h.sends)-1]
}

// tries counts each rule's messages since the owner's last turn and since the last of the rule's messages that
// worked: the episode still going, however long ago it began.
func (h past) tries(ownerAt time.Time) Tries {
	t := Tries{}
	from := map[string]time.Time{}
	for _, s := range h.sends {
		if s.result[Worked] && s.at.After(from[s.rule]) {
			from[s.rule] = s.at
		}
	}
	for _, s := range h.sends {
		if s.at.After(ownerAt) && s.at.After(from[s.rule]) {
			t[s.rule]++
		}
	}
	return t
}

// closed reports a rule momentum must not send for again: a case is open for it since the owner's last turn
// (tries used up, a message that did not land or stayed on the input line). It stays closed until he comes back.
func (h past) closed(rule string, ownerAt time.Time) bool {
	for _, c := range h.cases {
		if c.rule == rule && c.at.After(ownerAt) {
			return true
		}
	}
	return false
}

// presence is when the owner was last at the session: his last turn by the store; the harness's last user turn,
// unless it is one of momentum's own messages word for word; a local command or an interruption; an ask of the
// session's he answered; a draft or a tmux mode that kept a message out; or the session's process starting (he
// opened it). The rules hold while it is recent.
func (h past) presence(s *sessions.Session, ownerAt, answered time.Time) time.Time {
	at := ownerAt
	control := s.ControlAt
	if h.ourEscape(control) {
		control = time.Time{} // the interruption is momentum's own Escape, not its owner's
	}
	for _, t := range []time.Time{control, answered, h.presentAt, s.Started} {
		if t.After(at) {
			at = t
		}
	}
	if s.UserAt.After(at) && !h.ours(lastUser(s)) {
		at = s.UserAt
	}
	return at
}

// ourEscape reports whether an interruption at t is momentum's own: the transcript's record of the Escape it
// pressed after one of its messages, as the run that sent it read it.
func (h past) ourEscape(t time.Time) bool {
	for _, e := range h.escapes {
		if !t.IsZero() && t.UnixMilli() == e.UnixMilli() {
			return true
		}
	}
	return false
}

// ours reports whether text is one of the messages momentum typed, or may have typed, into the session.
func (h past) ours(text string) bool {
	text = fold(text)
	for _, t := range h.texts {
		if t != "" && t == text {
			return true
		}
	}
	return false
}

// ownerTurn is the newest of the owner's turns (newest first) that is his: not one of momentum's messages, and
// not a turn that opens with the marker and came within DeliveryWait of a message, which the store may have
// credited to him.
func (h past) ownerTurn(marks []store.Mark) time.Time {
	for _, m := range marks {
		if h.ours(m.Summary) {
			continue
		}
		near := false
		for _, s := range h.sends {
			near = near || !m.At.Before(s.at.Add(-2*time.Second)) && m.At.Before(s.at.Add(DeliveryWait))
		}
		if near && strings.HasPrefix(strings.TrimSpace(m.Summary), Marker) {
			continue
		}
		return m.At
	}
	return time.Time{}
}

// lastUser is the text of the session's last user-role entry, from its activity log.
func lastUser(s *sessions.Session) string {
	if s.Log == nil {
		return ""
	}
	items, _ := s.Log.Since(max(0, s.Log.Seq()-400))
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Role == sessions.RoleUser {
			return items[i].Text
		}
	}
	return ""
}

// outcome records what came of the session's last message, once each, and reports whether it is resolved: it
// showed up as a user turn of the session, or did not within DeliveryWait; then the turn it opened worked,
// stopped the same way again, or stopped otherwise.
func (sv *Supervisor) outcome(ctx context.Context, s *sessions.Session, stop Stop, h *past, now time.Time, put *[]store.Event) bool {
	l := h.last()
	if l == nil {
		return true
	}
	mk := func(result string) store.Event {
		l.result[result] = true
		return store.Event{Source: store.Supervise, TS: now, Session: s.Key, Harness: s.Tool, Actor: store.ActorDetector, Kind: KindOutcome,
			SourceKey: l.key + "#outcome#" + result, Summary: oneLine(fmt.Sprintf("%s: %s, try %d", l.rule, result, l.try)),
			Meta: meta(map[string]any{"of": l.key, "rule": l.rule, "result": result, "sent_at": l.at.UnixMilli()})}
	}
	if l.result[NotDelivered] || l.result[Worked] || l.result[FailedAgain] || l.result[Otherwise] {
		return true
	}
	if !l.result[Delivered] {
		landed := !s.UserAt.Before(l.at.Add(-2*time.Second)) && landedSince(s, l.seq, l.text)
		switch {
		case landed:
			*put = append(*put, mk(Delivered))
		case now.Sub(l.at) >= DeliveryWait:
			c := Stop{At: l.stopAt, Error: stop.Error}
			*put = append(*put, mk(NotDelivered), sv.openCase(ctx, s, c, l.rule, "not delivered: the message did not show up as its turn", l.try, now))
			h.cases = append(h.cases, caseRec{at: now, rule: l.rule, stopAt: l.stopAt}) // closed from this run on
			return true
		default:
			return false
		}
	}
	if stop.Working || !stop.At.After(l.at) {
		return true // the turn it opened still runs, or has not stopped yet
	}
	again := false
	switch l.rule {
	case RuleTransient:
		again = ErrorClass(stop.Error) == Transient
	case RuleAccess:
		again = isAccess(stop.Error)
	case RuleCutoff:
		again = Cutoff(stop.Words)
	case RulePromise:
		again = Promise(stop.Words) != "" && stop.Tasks == 0 && stop.WakeAt.IsZero() && stop.Loop == ""
	}
	switch {
	case again:
		*put = append(*put, mk(FailedAgain))
	case stop.Error == "" && worked(s, l.seq):
		*put = append(*put, mk(Worked))
	default: // no work, or it stopped on another error
		*put = append(*put, mk(Otherwise))
	}
	return true
}

// landedSince reports a user-role entry in the session's log since sequence number seq that is the message, text
// folded: it showed up as a turn, whatever came after it.
func landedSince(s *sessions.Session, seq int, text string) bool {
	if s.Log == nil || text == "" {
		return false
	}
	items, _ := s.Log.Since(seq)
	for _, it := range items {
		if it.Role == sessions.RoleUser && fold(it.Text) == text {
			return true
		}
	}
	return false
}

// worked reports tool calls in the session's log since sequence number seq: the turn did work.
func worked(s *sessions.Session, seq int) bool {
	if s.Log == nil {
		return false
	}
	items, _ := s.Log.Since(seq)
	for _, it := range items {
		if it.Role == sessions.RoleTool {
			return true
		}
	}
	return false
}

func isAccess(text string) bool {
	c := ErrorClass(text)
	return c == Login || c == Limit || c == Org
}

// StopOf is a live session's stop: the facts the rules read, from the session layer. presence is when its owner
// was last at it.
func StopOf(s *sessions.Session, presence time.Time) Stop {
	st := Stop{Key: s.Key, Tool: s.Tool, Name: s.Name, Where: s.Where, Auto: s.Auto, Phase: s.Phase, At: s.Since,
		Words: Words(s.LastText), Working: s.TurnOpen, Busy: s.Now != "", Tasks: len(s.Tasks), WakeAt: s.WakeAt, Loop: s.Loop,
		ResumedAt: s.ResumedAt, RequestAt: s.RequestAt, Queued: s.Queued, OwnerAt: presence, Started: s.Started}
	switch s.AskKind {
	case sessions.AskError:
		st.Error = strings.TrimPrefix(s.Ask, "stopped on an error: ")
	case sessions.AskQuestion, sessions.AskPlan, sessions.AskPermission:
		if s.Blocking() {
			st.Ask = s.AskKind
		}
	}
	return st
}

// StoreSession is the session's id in the store's events: a Claude Code transcript's name, a Codex thread, a
// Hermes conversation.
func StoreSession(s *sessions.Session) string {
	switch {
	case s.Tool == "claude" && s.Transcript != "":
		return strings.TrimSuffix(filepath.Base(s.Transcript), ".jsonl")
	case s.Tool == "hermes":
		return strings.TrimPrefix(s.Key, "hermes:")
	}
	return s.Key
}

// credentials adds to a probe that the owner logged in again: Claude Code rewrote its credentials after a login
// error.
type credentials struct {
	inner Probe
	path  string
	hold  time.Duration // a login is the owner at the machine: the sign counts once the owner window after it passed
}

func (c credentials) AccessBack(class string, since time.Time) (time.Time, bool) {
	back, ok := c.inner.AccessBack(class, since)
	if class != Login {
		return back, ok
	}
	if fi, err := os.Stat(c.path); err == nil && fi.ModTime().After(since) {
		if at := fi.ModTime().Add(c.hold); !ok || at.Before(back) {
			return at, true
		}
	}
	return back, ok
}

func (sv *Supervisor) woke(ctx context.Context) time.Time {
	if sv.Woke != nil {
		return sv.Woke(ctx)
	}
	return lastWake(ctx)
}

// lastWake is when the machine last woke from a suspend, by systemd's sleep target, which goes inactive as the
// machine resumes. Zero when it has not slept since it booted, or systemd cannot tell.
func lastWake(ctx context.Context) time.Time {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "sleep.target", "--property=InactiveEnterTimestamp", "--timestamp=unix", "--value").Output()
	if err != nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(string(out)), "@"), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// promptedWait is how long a message sent with an Escape has to show up in the transcript as the prompt of the
// turn after the one it ended.
var promptedWait = 10 * time.Second

// prompted waits for the session's transcript to show the message as the prompt of a turn, after the Escape that
// ended the turn the message waited behind, and notes in m whether the transcript recorded the interruption.
// When it does not show, the message is in a state nobody can tell (on the input line, in the queue): ErrUnstable.
func (sv *Supervisor) prompted(ctx context.Context, s *sessions.Session, text string, m map[string]any) error {
	look := sv.Prompted
	if look == nil {
		if s.Transcript == "" || s.Size < 0 { // where the scan stopped is not known: an earlier try would answer
			return fmt.Errorf("%w: no transcript place to read from", act.ErrUnstable)
		}
		look = func(_ context.Context, s *sessions.Session, text string) (bool, time.Time, error) {
			return claude.Prompted(s.Transcript, s.Size, text)
		}
	}
	end := time.Now().Add(promptedWait)
	for {
		found, interrupted, err := look(ctx, s, text)
		if err == nil && found {
			m["interrupted"] = !interrupted.IsZero()
			if !interrupted.IsZero() {
				m["interrupted_at"] = interrupted.UnixMilli()
			}
			return nil
		}
		if !time.Now().Before(end) {
			if err != nil {
				return fmt.Errorf("%w: the transcript: %v", act.ErrUnstable, err)
			}
			return fmt.Errorf("%w: the transcript shows no turn that it opened", act.ErrUnstable)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", act.ErrUnstable, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// unanswered checks, just before it types and just before its Escape, that the request a stalled turn waits on is
// still unanswered: something written since the scan (a reply, a call, an input) means the turn went on.
func (sv *Supervisor) unanswered(s *sessions.Session, sent time.Time) error {
	look := sv.Answered
	if look == nil {
		if s.Transcript == "" || s.Size < 0 {
			return errors.New("no transcript place to read from")
		}
		look = func(s *sessions.Session, sent time.Time) (bool, error) {
			return claude.Answered(s.Transcript, s.Size, sent)
		}
	}
	answered, err := look(s, sent)
	switch {
	case err != nil:
		return err
	case answered:
		return errAnswered
	}
	return nil
}

func meta(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func oneLine(s string) string { return clip(s, 240) }

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
