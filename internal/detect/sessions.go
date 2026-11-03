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
		if next.Kind != "owner" || !supervise.Nudge(next.Text) || res.Fired {
			continue
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
	resolved, err := resolvedStops(ctx, st, w)
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
		if resolved.covers(e.Session, e.TS.Add(-time.Duration(m.ForMS)*time.Millisecond), e.TS) {
			continue // a live supervise rule held this stop and resolved it on its first try: nothing waited on him
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

type resolutions map[string][]resolution

// resolveSlack is how far before the stop a phase wait may begin and still belong to it (the phase scan and the
// transcript's stop time differ by a scan or two), and how long after the message the wait may still end.
const resolveSlack = 5 * time.Minute

// resolvedStops reads supervision's own record for the stops one of its rules resolved on the first try: an act of
// try 1 whose outcome says the turn it opened worked. The wait events of the stop (an access wait is renewed every
// half hour) share its stop_at. Later tries and failed messages are not resolutions.
func resolvedStops(ctx context.Context, st *store.Store, w Window) (resolutions, error) {
	evs, err := st.Events(ctx, store.Query{Since: w.Since.Add(-historyLead), Until: w.Until.Add(historyLead), Source: store.Supervise,
		Kinds: []string{supervise.KindAct, supervise.KindOutcome}, Limit: maxRows})
	if err != nil {
		return nil, err
	}
	type act struct {
		session        string
		stopAt, sentAt time.Time
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
		if json.Unmarshal([]byte(e.Meta), &m) != nil || m.Try != 1 || m.StopAt == 0 {
			continue
		}
		acts[e.SourceKey] = act{e.Session, time.UnixMilli(m.StopAt), e.TS}
	}
	out := resolutions{}
	for _, e := range evs {
		if e.Kind != supervise.KindOutcome {
			continue
		}
		var m struct{ Of, Result string }
		if json.Unmarshal([]byte(e.Meta), &m) != nil || m.Result != supervise.Worked {
			continue
		}
		if a, ok := acts[m.Of]; ok && a.session == e.Session {
			out[a.session] = append(out[a.session], resolution{a.stopAt, a.sentAt})
		}
	}
	return out, nil
}

// historyLead reaches back far enough to see the act of a stop that began before the window, and on far enough to
// see the outcome of a message sent inside it (the turn it opened stops later).
const historyLead = 24 * time.Hour

// covers reports whether the wait from start to end is part of a stop that supervision resolved on its first try.
func (r resolutions) covers(session string, start, end time.Time) bool {
	for _, x := range r[session] {
		if !start.Before(x.stopAt.Add(-resolveSlack)) && start.Before(x.sentAt) && !end.After(x.sentAt.Add(resolveSlack)) {
			return true
		}
	}
	return false
}
