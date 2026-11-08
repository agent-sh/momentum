package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
)

// Sessions finds what keeps sessions from moving on without the owner:
//   - gave-up: supervision used up a rule's tries, or a message of its own did not land (its case events);
//   - outcome: a message it sent did not land, or the turn it opened stopped the same way again;
//   - nudged: the owner had to nudge a stopped session that no rule would have continued, keyed by why none
//     would (the replay of the rules over the store's history: its phase and what kept each rule off);
//   - waited: a session waited on him past the owner window, keyed by what it asked (its phase events).
type Sessions struct {
	Recur Recur // default SessionsRecur
	// Platforms names the platform each Hermes conversation runs on, which the store does not keep (nil: none
	// known, and the replay leaves those conversations alone, as supervision does).
	Platforms func(context.Context) (map[string]string, error)
	Programs  Programs // a nudge a program typed is not his
}

// SessionsRecur: three in two sessions in a week. A rule that gives up is a finding at once.
var (
	SessionsRecur = Recur{Events: 3, Sessions: 2}
	GaveUpRecur   = Recur{Events: 1, Sessions: 1}
)

// LongWait is how long a session must have waited on its owner for the wait to count: past the owner window
// supervision holds for, he was not there.
var LongWait = supervise.DefaultParams.OwnerWindow

func (Sessions) Family() string { return "sessions" }

func (d Sessions) Detect(ctx context.Context, st *store.Store, w Window) ([]store.Finding, error) {
	r := d.Recur
	if r == (Recur{}) {
		r = SessionsRecur
	}
	// each part's findings stand when another part fails: the errors come back beside them
	sup, serr := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Source: store.Supervise,
		Kinds: []string{supervise.KindCase, supervise.KindOutcome}, Limit: maxRows})
	var gave, other []occurrence
	for _, e := range sup {
		var m struct {
			Rule, Why, Result string
		}
		json.Unmarshal([]byte(e.Meta), &m)
		switch e.Kind {
		case supervise.KindCase:
			reason := slug(strings.SplitN(m.Why, ":", 2)[0], 40)
			gave = append(gave, occurrence{sig: signature("sessions", "gave-up:"+m.Rule+":"+reason), ev: e,
				title: fmt.Sprintf("Supervision gives up on stopped sessions (%s: %s)", m.Rule, strings.SplitN(m.Why, ":", 2)[0])})
		case supervise.KindOutcome:
			if m.Result != supervise.NotDelivered && m.Result != supervise.FailedAgain {
				continue
			}
			other = append(other, occurrence{sig: signature("sessions", "outcome:"+m.Rule+":"+slug(m.Result, 20)), ev: e,
				title: fmt.Sprintf("Supervision's %s messages: %s", m.Rule, m.Result)})
		}
	}
	nudged, nerr := d.nudged(ctx, st, w)
	other = append(other, nudged...)
	waited, werr := waits(ctx, st, w)
	other = append(other, waited...)
	return append(gather("sessions", gave, GaveUpRecur), gather("sessions", other, r)...), errors.Join(serr, nerr, werr)
}

// nudged replays the rules over the window and keeps the stops the owner ended with a nudge that no rule would
// have acted on first, each keyed by why none would.
func (d Sessions) nudged(ctx context.Context, st *store.Store, w Window) ([]occurrence, error) {
	points, err := supervise.History(ctx, st, w.Since, w.Until)
	if err != nil {
		return nil, err
	}
	if d.Platforms != nil {
		platforms, err := d.Platforms(ctx)
		if err != nil {
			return nil, err
		}
		for i := range points {
			if points[i].Stop.Tool == "hermes" {
				points[i].Stop.Where = platforms[points[i].Session]
			}
		}
	}
	probe, err := supervise.StoreProbe(ctx, st, w.Since, w.Until)
	if err != nil {
		return nil, err
	}
	turns, program, err := ownerTurns(ctx, st, d.Programs, w.Since, w.Until, w.Since)
	if err != nil {
		return nil, err
	}
	programs := map[string]bool{}
	for i, t := range turns {
		if program[i] {
			programs[t.ID()] = true
		}
	}
	var occ []occurrence
	for _, res := range supervise.Replay(points, nil, supervise.DefaultParams, probe, w.Until) {
		next := res.Point.Next
		if next.Kind != "owner" || !supervise.Nudge(next.Text) || res.Fired || res.Point.Stop.Exited || res.Point.Stop.Reopened {
			continue // an exited stop is one no rule can continue: the owner's input reopens it, after a command or not
		}
		why := supervise.Why(res)
		turn, err := st.Before(ctx, res.Point.Session, []string{"owner_text"}, next.At.Add(-time.Second), next.At.Add(time.Second), 1)
		if err != nil {
			return occ, err
		}
		if len(turn) == 0 || programs[turn[0].ID()] {
			continue // no turn of his in the store, or a program typed it
		}
		key := slug(Norm(why, 60), 60)
		occ = append(occ, occurrence{sig: signature("sessions", "nudged:"+key), ev: turn[0],
			title: "The owner nudges stopped sessions no rule continues: " + Norm(why, 60)})
	}
	return occ, nil
}

// waits are the stretches a session waited on its owner past LongWait, by what it asked, from the sessions events
// that saw them end.
func waits(ctx context.Context, st *store.Store, w Window) ([]occurrence, error) {
	evs, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Source: store.Sessions, Kinds: []string{"phase", "vanished"}, Limit: maxRows})
	if err != nil {
		return nil, err
	}
	sup, err := supervisedStops(ctx, st, w)
	if err != nil {
		return nil, err
	}
	var occ []occurrence
	for _, e := range evs {
		var m struct {
			From, To    string
			ForMS       int64  `json:"for_ms"`
			FromAskKind string `json:"from_ask_kind"`
			Auto        bool   `json:"auto"`
		}
		if json.Unmarshal([]byte(e.Meta), &m) != nil || m.From != "needs_you" || m.Auto || time.Duration(m.ForMS)*time.Millisecond < LongWait {
			continue
		}
		if sup.covers(e.Session, e.TS.Add(-time.Duration(m.ForMS)*time.Millisecond), e.TS) {
			continue // the access rule held this stop for a stated reset, or a supervise rule resolved it on its first try: nothing waited on him
		}
		kind := m.FromAskKind
		if kind == "" {
			kind = "unknown"
		}
		occ = append(occ, occurrence{sig: signature("sessions", "waited:"+slug(kind, 20)), ev: e,
			title: fmt.Sprintf("Sessions wait on the owner past %s (%s)", LongWait, kind)})
	}
	return occ, nil
}

// resolution is one stop a supervise rule resolved on its first try: its message (try 1) opened a turn that worked.
// The stop lasted from stopAt to sentAt, when the message was typed.
type resolution struct{ stopAt, sentAt time.Time }

// hold is one stop the access rule held for the reset its usage limit states: the stop began at stopAt. Every
// supervise run that finds the stop still held rewrites the stop's wait event (one per stop and try), so seen, the
// event's time, is the last run that held it: just before the stop ended, by the rule's message at the reset or
// another way (a restart, a new login, another account), or when supervision stopped looking at it. No run rewrites
// it once the rule's time (the reset plus its grace) comes: the rule acts then, so seen is before that time.
type hold struct{ stopAt, seen time.Time }

// supervised is supervision's own record of the stops it took on, by session.
type supervised struct {
	resolved map[string][]resolution
	held     map[string][]hold
}

// resolveSlack is how far before the stop a phase wait may begin and still belong to it (the phase scan and the
// transcript's stop time differ by a scan or two), and how long after the message, or the last run that held the
// stop, the wait may still end.
const resolveSlack = 5 * time.Minute

// supervisedStops reads supervision's own record of the stops it took on. A resolution is an act of try 1 whose
// outcome says the turn it opened worked; later tries are not resolutions. A hold is an access wait for a usage
// limit that states its reset: the rule's time was known from the stop's first moment, so the stop waited on the
// reset, not on the owner. An access wait on a login, an organization block or a limit that states no reset holds
// nothing, whatever its time: that time comes from a sign that access works again, such as a new login, after the
// stop had waited on him, and the event keeps only its last time. A session that meets the limit again on every
// wakeup is a new stop each time, with its own wait event and stop_at. A stop one of whose messages did not land or
// failed again is neither: its outcome is a finding of its own, and its wait counts.
func supervisedStops(ctx context.Context, st *store.Store, w Window) (supervised, error) {
	evs, err := st.Events(ctx, store.Query{Since: w.Since.Add(-historyLead), Until: w.Until.Add(historyLead), Source: store.Supervise,
		Kinds: []string{supervise.KindAct, supervise.KindOutcome, supervise.KindWait}, Limit: maxRows})
	if err != nil {
		return supervised{}, err
	}
	type stop struct {
		session string
		at      int64 // its stop_at
	}
	type act struct {
		stop
		try    int
		sentAt time.Time
	}
	acts := map[string]act{}
	for _, e := range evs {
		if e.Kind != supervise.KindAct {
			continue
		}
		var m struct {
			Try    int
			StopAt int64 `json:"stop_at"`
		}
		if json.Unmarshal([]byte(e.Meta), &m) != nil || m.StopAt == 0 {
			continue
		}
		acts[e.SourceKey] = act{stop{e.Session, m.StopAt}, m.Try, e.TS}
	}
	out := supervised{resolved: map[string][]resolution{}, held: map[string][]hold{}}
	failed := map[stop]bool{}
	for _, e := range evs {
		if e.Kind != supervise.KindOutcome {
			continue
		}
		var m struct{ Of, Result string }
		if json.Unmarshal([]byte(e.Meta), &m) != nil {
			continue
		}
		a, ok := acts[m.Of]
		if !ok || a.session != e.Session {
			continue
		}
		switch m.Result {
		case supervise.Worked:
			if a.try == 1 {
				out.resolved[a.session] = append(out.resolved[a.session], resolution{time.UnixMilli(a.at), a.sentAt})
			}
		case supervise.NotDelivered, supervise.FailedAgain:
			failed[a.stop] = true
		}
	}
	for _, e := range evs {
		if e.Kind != supervise.KindWait {
			continue
		}
		var m struct {
			Rule, Error string
			StopAt      int64 `json:"stop_at"`
		}
		if json.Unmarshal([]byte(e.Meta), &m) != nil || m.Rule != supervise.RuleAccess || m.StopAt == 0 || failed[stop{e.Session, m.StopAt}] {
			continue
		}
		stopAt := time.UnixMilli(m.StopAt)
		if _, stated := supervise.ResetAt(m.Error, stopAt); !stated || supervise.ErrorClass(m.Error) != supervise.Limit {
			continue
		}
		out.held[e.Session] = append(out.held[e.Session], hold{stopAt, e.TS})
	}
	return out, nil
}

// historyLead reaches back far enough to see the act or the hold of a stop that began before the window, and on far
// enough to see the outcome of a message sent inside it (the turn it opened stops later).
const historyLead = 24 * time.Hour

// covers reports whether the wait from start to end lies within a stop supervision took on: one it resolved on its
// first try, or one an access wait held, from the stop to the last run that held it.
func (s supervised) covers(session string, start, end time.Time) bool {
	for _, x := range s.resolved[session] {
		if !start.Before(x.stopAt.Add(-resolveSlack)) && start.Before(x.sentAt) && !end.After(x.sentAt.Add(resolveSlack)) {
			return true
		}
	}
	for _, x := range s.held[session] {
		if !start.Before(x.stopAt.Add(-resolveSlack)) && !end.After(x.seen.Add(resolveSlack)) {
			return true
		}
	}
	return false
}
