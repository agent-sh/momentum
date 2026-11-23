package scorecard

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Verdict is a system's week as the rules judged it.
type Verdict struct {
	System   string `json:"system"`
	Week     string `json:"week"`
	Scored   bool   `json:"scored"`
	Delivers bool   `json:"delivers"`
	Why      string `json:"why"`
}

// Scored is what scoring a week came to.
type Scored struct {
	Deployed  time.Time  `json:"deployed"`
	Verdicts  []Verdict  `json:"verdicts"`
	Standings []Standing `json:"standings"`
	Shutdowns []Shutdown `json:"shutdowns,omitempty"`
	// Reenabled are the systems found on again after a shutdown: their scoring starts over from now.
	Reenabled []string `json:"reenabled,omitempty"`
}

// Score judges the observed week for both systems, keeps the verdicts, and shuts down a system that failed
// FailingWeeks scored weeks in a row. The first run that is not dry records the deployment, which scoring counts
// from: the first week scored is the first full week after it. A dry run writes nothing and shuts nothing; it says
// what a run now would do.
func Score(ctx context.Context, st *store.Store, sw Switch, o *Observation, dry bool, now time.Time) (Scored, error) {
	var sc Scored
	dep, at, ok, err := st.ScorecardValue(ctx, keyDeployed)
	if err != nil {
		return sc, err
	}
	sc.Deployed = at
	if !ok {
		sc.Deployed = now
		if !dry {
			if err := st.SetScorecardValue(ctx, keyDeployed, now.Format(time.RFC3339Nano), now); err != nil {
				return sc, err
			}
			for _, s := range []string{Tacit, Momentum} {
				if err := st.SetScorecardValue(ctx, keySince(s), now.Format(time.RFC3339Nano), now); err != nil {
					return sc, err
				}
			}
		}
	} else if t, err := time.Parse(time.RFC3339Nano, dep); err == nil {
		sc.Deployed = t
	}
	for _, system := range []string{Tacit, Momentum} {
		s, err := Stand(ctx, st, sw, system)
		if err != nil {
			return sc, err
		}
		if s.Since.IsZero() {
			s.Since = sc.Deployed // a dry run before the deployment: as if it were now
			s.FirstScored = firstScored(s.Since).Name
		}
		if s.Shut != "" && s.On {
			// turned back on after a shutdown: scoring starts over
			sc.Reenabled = append(sc.Reenabled, system)
			s.Shut, s.Since = "", now
			s.FirstScored = firstScored(now).Name
			if !dry {
				if err := st.SetScorecardValue(ctx, keySince(system), now.Format(time.RFC3339Nano), now); err != nil {
					return sc, err
				}
				if err := st.SetScorecardValue(ctx, keyShut(system), "", now); err != nil {
					return sc, err
				}
			}
		}
		v := Verdict{System: system, Week: o.Week.Name, Scored: s.On && scored(o.Week, s.Since)}
		var numbers any
		if system == Tacit {
			v.Delivers, v.Why = JudgeTacit(o.Tacit, o.TacitBefore)
			numbers = o.Tacit
		} else {
			v.Delivers, v.Why = JudgeMomentum(o.Momentum)
			numbers = o.Momentum
		}
		switch {
		case !s.On:
			v.Why = "not scored: " + system + " is off; " + v.Why
		case !v.Scored:
			v.Why = fmt.Sprintf("not scored: scoring starts with %s; %s", s.FirstScored, v.Why)
		}
		row := store.ScorecardWeek{System: system, Week: o.Week.Name, Scored: v.Scored, Delivers: v.Delivers, Why: v.Why,
			Numbers: numbersJSON(numbers), At: now}
		weeks := slices.DeleteFunc(slices.Clone(s.Weeks), func(w store.ScorecardWeek) bool { return w.Week == row.Week })
		weeks = append(weeks, row)
		slices.SortFunc(weeks, func(a, b store.ScorecardWeek) int {
			if a.Week < b.Week {
				return -1
			}
			if a.Week > b.Week {
				return 1
			}
			return 0
		})
		s.count(weeks)
		if !dry {
			if err := st.PutScorecardWeek(ctx, row); err != nil {
				return sc, err
			}
		}
		sc.Verdicts = append(sc.Verdicts, v)
		if s.On && s.Failing >= FailingWeeks {
			why := fmt.Sprintf("%d scored weeks in a row did not deliver; %s: %s", s.Failing, o.Week.Name, v.Why)
			if dry {
				sc.Shutdowns = append(sc.Shutdowns, Shutdown{System: system, OffFile: sw.OffFile(system), Failed: []string{"dry: nothing was shut"}})
			} else {
				sd, err := sw.Shut(ctx, system, o.Week.Name, why, now)
				if err != nil {
					return sc, err
				}
				if err := st.SetScorecardValue(ctx, keyShut(system), o.Week.Name, now); err != nil {
					return sc, err
				}
				sc.Shutdowns = append(sc.Shutdowns, sd)
				s.On, s.Shut, s.ToShutdown = false, o.Week.Name, 0
			}
		}
		sc.Standings = append(sc.Standings, s)
	}
	return sc, nil
}
