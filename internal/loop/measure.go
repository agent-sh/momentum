package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// Measured is what the measure step did with one case.
type Measured struct {
	Case, Title, PR string
	Status          string // measuring, improved, rolled back, waiting, failed
	Result          measure.Result
	Due             time.Time // when its window after the change ends
	Why             string
}

// Count is the events a signal counts over a span; Base, the activity they are a part of: the events of its
// source, kinds, tool and harness, whatever their text.
func Base(ctx context.Context, st *store.Store, sig Signal, since, until time.Time) (int, error) {
	all := sig
	all.Match = nil
	evs, err := Count(ctx, st, all, since, until)
	return len(evs), err
}

// windowCounts are a signal's counts over a span: its hits and its base.
func windowCounts(ctx context.Context, st *store.Store, sig Signal, since, until time.Time) (measure.Counts, error) {
	hits, err := Count(ctx, st, sig, since, until)
	if err != nil {
		return measure.Counts{}, err
	}
	base, err := Base(ctx, st, sig, since, until)
	return measure.Counts{Hits: len(hits), Base: base}, err
}

// Measure is the measure step, deterministic: a deployed case starts measuring, with its plan's window and count
// before; a measuring case whose window after its change has ended is measured against the window before it. One
// whose signal's rate fell (measure.Compare) closes as improved, with a shipped note that says what its change did;
// one whose rate did not fall has its change rolled back, with a decided note. A window after with no activity at
// all is waited out once more; a second one rolls the change back, since nothing could show it helped. A dry
// measure says what it would do and changes nothing.
func (r *Runner) Measure(ctx context.Context, dry bool) ([]Measured, error) {
	var out []Measured
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseDeployed, store.CaseMeasuring}})
	if err != nil {
		return nil, err
	}
	now := r.now()
	for _, c := range cs {
		p, ok, err := r.Ship.Load(c.ID)
		m := Measured{Case: c.ID, Title: c.Title, PR: p.PR}
		switch {
		case err != nil:
			m.Status, m.Why = "failed", err.Error()
		case !ok || p.Plan == nil:
			m.Status, m.Why = "failed", "its package or plan is gone"
		}
		if m.Status != "" {
			out = append(out, m)
			continue
		}
		plan := *p.Plan
		m.Due = c.Deployed.Add(plan.Window)
		if c.Status == store.CaseDeployed {
			m.Status = "measuring"
			if !dry {
				if _, err := r.Store.StartMeasuring(ctx, c.ID, plan.Window, plan.Before, "loop:measure"); err != nil {
					m.Status, m.Why = "failed", err.Error()
				}
			}
			out = append(out, m)
			continue
		}
		if now.Before(m.Due) {
			m.Status = "waiting"
			out = append(out, m)
			continue
		}
		before, err := windowCounts(ctx, r.Store, plan.Signal, plan.At.Add(-plan.Window), plan.At)
		if err != nil {
			return out, err
		}
		end := m.Due
		after, err := windowCounts(ctx, r.Store, plan.Signal, c.Deployed, end)
		if err != nil {
			return out, err
		}
		if after.Base == 0 && now.Before(c.Deployed.Add(2*plan.Window)) {
			m.Due = c.Deployed.Add(2 * plan.Window)
			m.Status, m.Why = "waiting", "no activity in the window after its change: it waits one more window"
			out = append(out, m)
			continue
		}
		if after.Base == 0 {
			end = c.Deployed.Add(2 * plan.Window)
			if after, err = windowCounts(ctx, r.Store, plan.Signal, c.Deployed, end); err != nil {
				return out, err
			}
		}
		res, ok := measure.Compare(before, after)
		m.Result = res
		if dry {
			m.Status = map[bool]string{true: "improved", false: "rolled back"}[ok && res.Improved]
			out = append(out, m)
			continue
		}
		if ok && res.Improved {
			if _, err := r.Store.CloseMeasured(ctx, c.ID, after.Hits, res.Effect, "loop:measure"); err != nil {
				m.Status, m.Why = "failed", err.Error()
				out = append(out, m)
				continue
			}
			m.Status = "improved"
			r.note(ctx, store.Note{Kind: store.NoteShipped, Title: plainClip("momentum improved: "+c.Title, store.TitleMax),
				Detail: fmt.Sprintf("Its change (%s) measurably helped over %s after it went in.", firstOf(p.PR, "a package"), days(plan.Window)),
				Effect: res.Effect, Links: prLinks(p.PR)})
			out = append(out, m)
			continue
		}
		why := "no measured effect: " + res.Effect
		if !ok {
			why = "nothing to measure it by: no activity in the windows before and after its change"
		}
		if _, err := r.Ship.RollBack(ctx, c.ID, why); err != nil {
			m.Status, m.Why = "failed", "the rollback: "+err.Error()
			r.Store.CaseNote(ctx, c.ID, "not rolled back", err.Error(), "loop:measure")
			out = append(out, m)
			continue
		}
		m.Status, m.Why = "rolled back", why
		r.note(ctx, store.Note{Kind: store.NoteDecided, Title: plainClip("momentum rolled back its change for: "+c.Title, store.TitleMax),
			Detail: why + ". To keep the change, revert the revert on the default branch.", Links: prLinks(p.PR)})
		out = append(out, m)
	}
	return out, nil
}

// note puts a note of the loop's in the report, passive: it pages nobody.
func (r *Runner) note(ctx context.Context, n store.Note) {
	n.Source, n.Importance, n.At = "momentum", store.ImportanceNormal, r.now()
	if _, err := r.Store.PutNote(ctx, n); err != nil {
		r.logf("note %q: %v", n.Title, err)
	}
}

func prLinks(pr string) []store.Link {
	if pr == "" {
		return nil
	}
	return []store.Link{{Label: "pull request", URL: pr}}
}

// days is a duration in whole days, or hours under two days.
func days(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
