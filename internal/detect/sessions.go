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
		kind := m.FromAskKind
		if kind == "" {
			kind = "unknown"
		}
		occ = append(occ, occurrence{sig: signature("sessions", "waited:"+slug(kind, 20)), ev: e,
			title: fmt.Sprintf("Sessions wait on the owner past %s (%s)", LongWait, kind)})
	}
	return occ, nil
}
