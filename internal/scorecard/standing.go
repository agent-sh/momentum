package scorecard

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The scorecard's state in momentum's store: when it was deployed (its first run that was not dry), and for each
// system the time its scoring counts from (the deployment, or when it was last turned back on) and the week it was
// shut down in, while it is.

const keyDeployed = "deployed"

func keySince(system string) string { return system + ".since" }
func keyShut(system string) string  { return system + ".shut" }

// Standing is where a system stands.
type Standing struct {
	System      string                `json:"system"`
	On          bool                  `json:"on"`
	Shut        string                `json:"shut_in,omitempty"` // the week the scorecard shut it down in
	Since       time.Time             `json:"scored_since"`      // zero before the scorecard was deployed
	FirstScored string                `json:"first_scored_week,omitempty"`
	Weeks       []store.ScorecardWeek `json:"-"`
	Failing     int                   `json:"failing_in_a_row"` // scored failing weeks in a row, up to the newest
	ToShutdown  int                   `json:"weeks_to_shutdown"`
}

// Stand reads a system's standing.
func Stand(ctx context.Context, st *store.Store, sw Switch, system string) (Standing, error) {
	s := Standing{System: system, On: !sw.Off(system)}
	since, err := sinceOf(ctx, st, system)
	if err != nil {
		return s, err
	}
	s.Since = since
	if !since.IsZero() {
		s.FirstScored = firstScored(since).Name
	}
	shut, _, ok, err := st.ScorecardValue(ctx, keyShut(system))
	if err != nil {
		return s, err
	}
	if ok {
		s.Shut = shut
	}
	weeks, err := st.ScorecardWeeks(ctx, system)
	if err != nil {
		return s, err
	}
	s.count(weeks)
	return s, nil
}

// count takes the kept weeks, the oldest first: the scored ones, and the failing ones in a row.
func (s *Standing) count(weeks []store.ScorecardWeek) {
	s.Weeks = nil
	for _, w := range weeks {
		if wk, err := ParseWeek(w.Week, time.Local); w.Scored && err == nil && scored(wk, s.Since) {
			s.Weeks = append(s.Weeks, w) // a week before a re-enable no longer counts
		}
	}
	s.Failing = failingInARow(s.Weeks, time.Local)
	s.ToShutdown = max(0, FailingWeeks-s.Failing)
	if !s.On {
		s.ToShutdown = 0
	}
}

// firstScored is the first week that begins at or after since.
func firstScored(since time.Time) Week {
	w := WeekOf(since)
	if w.Start.Before(since) {
		w = w.Next()
	}
	return w
}

func sinceOf(ctx context.Context, st *store.Store, system string) (time.Time, error) {
	v, _, ok, err := st.ScorecardValue(ctx, keySince(system))
	if err != nil || !ok {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, nil
	}
	return t.Local(), nil
}

// failingInARow counts the scored weeks that failed, from the newest back, while each is the week before the one
// after it: a week missing from the record breaks the run.
func failingInARow(weeks []store.ScorecardWeek, loc *time.Location) int {
	n := 0
	next := ""
	for i := len(weeks) - 1; i >= 0; i-- {
		w := weeks[i]
		if w.Delivers {
			break
		}
		if next != "" {
			nw, err := ParseWeek(next, loc)
			if err != nil || nw.Prev(1).Name != w.Week {
				break
			}
		}
		n++
		next = w.Week
	}
	return n
}

// scored says whether a week counts for a system whose scoring counts from since: it began at or after since.
func scored(w Week, since time.Time) bool { return !since.IsZero() && !w.Start.Before(since) }

// numbersJSON is v as the JSON a scorecard week keeps.
func numbersJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
