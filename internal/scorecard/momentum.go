package scorecard

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// MomentumWeek is a week of momentum's own numbers, from its store: what its cases came to, what it landed, what
// its loop spent and what its supervision did.
type MomentumWeek struct {
	Week string `json:"week"`
	// Improved are the cases closed as improved: a measured drop of their signal after a landed change.
	Improved      int            `json:"improved"`
	ImprovedCases []string       `json:"improved_cases,omitempty"`
	Landed        int            `json:"landed"` // changes deployed
	Opened        int            `json:"opened"`
	Closed        map[string]int `json:"closed"` // by reason
	SpendUSD      float64        `json:"spend_usd"`
	Runs          map[string]int `json:"runs"`      // by step and status
	Supervise     map[string]int `json:"supervise"` // outcomes by rule and result
	SuperviseActs int            `json:"supervise_acts"`
}

// momentumWeek reads a week of momentum's numbers.
func momentumWeek(ctx context.Context, st *store.Store, w Week) (MomentumWeek, error) {
	m := MomentumWeek{Week: w.Name, Closed: map[string]int{}, Runs: map[string]int{}, Supervise: map[string]int{}}
	closed, err := st.CaseLogBetween(ctx, "closed", w.Start, w.End)
	if err != nil {
		return m, err
	}
	for _, e := range closed {
		reason, _, _ := strings.Cut(e.Detail, ":")
		if strings.HasPrefix(reason, "duplicate of") {
			reason = string(store.Duplicate)
		}
		m.Closed[reason]++
		if reason == string(store.Improved) {
			m.Improved++
			m.ImprovedCases = append(m.ImprovedCases, e.Case)
		}
	}
	for action, n := range map[string]*int{"deployed": &m.Landed, "opened": &m.Opened} {
		es, err := st.CaseLogBetween(ctx, action, w.Start, w.End)
		if err != nil {
			return m, err
		}
		*n = len(es)
	}
	from, err := st.LoopSpend(ctx, w.Start, 0)
	if err != nil {
		return m, err
	}
	after, err := st.LoopSpend(ctx, w.End, 0)
	if err != nil {
		return m, err
	}
	m.SpendUSD = float64(int64((from-after)*100+0.5)) / 100
	live := ""
	runs, err := st.LoopRuns(ctx, store.RunFilter{Since: w.Start, Experiment: &live})
	if err != nil {
		return m, err
	}
	for _, r := range runs {
		if r.Started.Before(w.End) {
			m.Runs[r.Step+" "+r.Status]++
		}
	}
	evs, err := st.Events(ctx, store.Query{Source: store.Supervise, Kinds: []string{"outcome", "act"}, Since: w.Start, Until: w.End, Limit: allEvents})
	if err != nil {
		return m, err
	}
	for _, e := range evs {
		if e.Kind == "act" {
			m.SuperviseActs++
			continue
		}
		var meta struct{ Rule, Result string }
		if json.Unmarshal([]byte(e.Meta), &meta) == nil && meta.Rule != "" {
			m.Supervise[meta.Rule+": "+meta.Result]++
		}
	}
	return m, nil
}

// allEvents is the limit of a query that reads a whole week: the store's default is a page.
const allEvents = 1 << 30
