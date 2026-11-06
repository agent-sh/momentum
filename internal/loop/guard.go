package loop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// week is the step a guard's watch moves by past its end: windows are whole weeks.
const week = 7 * 24 * time.Hour

// guardPlan stands in for a measured plan refused as too new, too rare or with no recent event (instead), when the
// case's own store evidence shows its problem happened before (SPEC, Measurement, Guards). A preventive fix for a
// problem that has stopped has no rate to drop; a guard asks whether the problem comes back.
//
// It reads the plan's span, the four windows before the boundary, from the signal's source's first event when that
// came inside it, with the sources the plan would count for the units its history counts in, read whole. The
// signal's events there fall into incidents (no gap longer than the plan's window between two of them: a stretch
// shorter than a whole week can be the weekly cycle's lull, a night or a weekend, rather than the end of a cause). Its
// exposure is the plan's units of activity, of the signal's tool when it names one: absence counts only while the
// unit was in use. With λ the incidents per unit of exposure, the watch must reach E = ln(1/Alpha)/λ units with no
// recurrence (measure.GuardNeed); its window is the whole weeks the history took to hold that much exposure. A
// history whose exposure all lies inside its incidents is too thin to estimate λ, and a watch longer than
// measure.MaxWatch is longer than the loop counts after any change: both are refused, with the measured plan's
// refusal beside the guard's. sourcesFor gives the sources and the omitted ones for a count in units, or a
// *waitingPlan.
func (s *Ship) guardPlan(ctx context.Context, c store.Case, plan Plan, instead error, sourcesFor func(units string) ([]string, []string, error)) (Plan, error) {
	why := strings.TrimPrefix(instead.Error(), ErrRefused.Error()+": ")
	refuse := func(format string, a ...any) (Plan, error) {
		return Plan{}, refused("%s; nor can a guard stand in for it: %s", why, fmt.Sprintf(format, a...))
	}
	sig, at, window := plan.Signal, plan.At, plan.Window
	since := at.Add(-time.Duration(measure.BaselineWindows+1) * window)
	evs, err := Count(ctx, s.Store, sig, since, at, s.Dir, "")
	if err != nil {
		return Plan{}, err
	}
	if len(evs) == 0 {
		return refuse("the signal counts no event in the %s before", days(at.Sub(since)))
	}
	units := unitsOf(evs)
	first, err := s.Store.FirstHarnessEvent(ctx, sig.Source, harnessFor(sig, units))
	if err != nil {
		return Plan{}, err
	}
	if first.After(since) {
		since = first
	}
	sources, omitted, err := sourcesFor(units)
	g := Plan{Signal: sig, Window: window, Units: units, At: at, Installed: plan.Installed, Sources: sources, Omitted: omitted}
	if err != nil {
		var wait *waitingPlan
		if errors.As(err, &wait) {
			return g, err
		}
		return Plan{}, err
	}
	if unread, err := unreadWindow(ctx, s.Store, g, since, at, s.Dir); err != nil {
		return Plan{}, err
	} else if len(unread) > 0 {
		return g, &waitingPlan{sources: unread}
	}
	h, err := guardHistory(ctx, s.Store, g, since, at, s.Dir)
	if err != nil {
		return Plan{}, err
	}
	history := days(at.Sub(since))
	if len(h.hits) == 0 {
		return refuse("the signal counts no event of %s in the %s before", units, history)
	}
	evidence, err := s.Store.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		return Plan{}, err
	}
	counted := map[string]bool{}
	for _, e := range h.hits {
		counted["ev:"+e.ID()] = true
	}
	events, in, hit := 0, 0, 0
	for _, e := range evidence {
		if !strings.HasPrefix(e.Ref, "ev:") {
			continue
		}
		events++
		if e.At.Before(since) || !e.At.Before(at) {
			continue
		}
		in++
		if counted[e.Ref] {
			hit++
		}
	}
	switch {
	case events == 0:
		return refuse("the case holds no event of the store: nothing shows its problem happened")
	case in == 0:
		return refuse("the case has no evidence in the %s its history reads", history)
	case hit*2 < in:
		return refuse("the signal counts %d of the case's %d events in its history: it does not measure the case's problem", hit, in)
	case h.quiet == 0:
		return refuse("its history is too thin to tell how often the problem starts: all %d %s of its exposure lie inside its incidents, so the unit was never seen in use without the problem",
			h.exposure, units)
	}
	incidents := len(h.incidents)
	need := measure.GuardNeed(incidents, h.exposure)
	// the time the history took to hold that much exposure, in whole weeks
	expected := time.Duration(float64(at.Sub(since)) * float64(need) / float64(h.exposure))
	watch := max(week, (expected+week-1)/week*week)
	if watch > measure.MaxWatch {
		return refuse("%d incidents in %d %s of exposure over %s ask a watch of %d with no recurrence, about %s: longer than the %s a guard may watch, the longest span a measured plan counts after its change",
			incidents, h.exposure, units, history, need, days(watch), days(measure.MaxWatch))
	}
	g.Guard = &Guard{Instead: why, Since: since, Gap: window, Incidents: incidents, Exposure: h.exposure, Quiet: h.quiet, Need: need, Watch: watch}
	return g, nil
}

// guardCount is a guard's count over a span: the events its signal counts there, their incidents, the units of
// exposure, and those outside every incident.
type guardCount struct {
	hits      []store.Event
	incidents []measure.Incident
	exposure  int
	quiet     int
}

// guardHistory counts a guard's history over [since, until) by its plan's signal, units, sources and window (the gap
// that separates two incidents).
func guardHistory(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string) (guardCount, error) {
	hits, err := recurrences(ctx, st, plan, since, until, dir)
	if err != nil {
		return guardCount{}, err
	}
	base, err := guardExposure(ctx, st, plan, hits, since, until, dir)
	if err != nil {
		return guardCount{}, err
	}
	times := make([]time.Time, len(hits))
	for i, e := range hits {
		times[i] = e.TS
	}
	h := guardCount{hits: hits, incidents: measure.Incidents(times, plan.Window), exposure: len(base)}
	for u := range base {
		inside := slices.ContainsFunc(h.incidents, func(in measure.Incident) bool {
			return u.Hour >= in.First.UnixMilli()/3_600_000 && u.Hour <= in.Last.UnixMilli()/3_600_000
		})
		if !inside {
			h.quiet++
		}
	}
	return h, nil
}

// recurrences are the events a plan's signal counts over a span in its units: of the owner alone for his hours, of a
// session for the sessions' hours.
func recurrences(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string) ([]store.Event, error) {
	if !until.After(since) {
		return nil, nil
	}
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return nil, err
	}
	evs, err := Count(ctx, st, plan.Signal, since, until, dir, q.Actor)
	if err != nil {
		return nil, err
	}
	var out []store.Event
	for _, e := range evs {
		if q.BySession && e.Session == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// guardExposure is a guard's exposure over a span: the plan's units of activity among its sources, only those in
// which the signal's tool was called or answered when it names one (the owner's hours hold no tool), with the units
// of its hits.
func guardExposure(ctx context.Context, st *store.Store, plan Plan, hits []store.Event, since, until time.Time, dir string) (map[store.Unit]bool, error) {
	if !until.After(since) {
		return map[store.Unit]bool{}, nil
	}
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return nil, err
	}
	q.Sources = plan.Sources
	if plan.Units != OwnerHours {
		q.Tool = plan.Signal.Tool
	}
	base, err := st.Activity(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, e := range hits {
		base[store.UnitOf(e, q.BySession)] = true
	}
	return base, nil
}

// hitUnits is how many units events fall in.
func hitUnits(evs []store.Event, units string) int {
	seen := map[store.Unit]bool{}
	for _, e := range evs {
		seen[store.UnitOf(e, units != MachineHours)] = true
	}
	return len(seen)
}

// earliest is the time of the earliest of events.
func earliest(evs []store.Event) time.Time {
	var at time.Time
	for _, e := range evs {
		if at.IsZero() || e.TS.Before(at) {
			at = e.TS
		}
	}
	return at
}

// guardDetail is what a guarded case's log says when its watch starts.
func guardDetail(plan Plan) string {
	g := plan.Guard
	return fmt.Sprintf("%d incidents of its problem in %d %s of exposure since %s; one recurrence fails it, %d %s of exposure with none close it as improved",
		g.Incidents, g.Exposure, plan.Units, g.Since.UTC().Format(time.DateTime), g.Need, plan.Units)
}

// guardCase is the measure step for a measuring case whose plan is a guard. Any event its signal counts since the
// change was in effect is a recurrence: the watch fails at once, and the change goes as one with no measured effect
// does (noEffect). A failed read inside the watch starts it again after the failure (afterGap). From the watch's end,
// once read, exposure that reached the guard's need with no recurrence closes the case as improved, guarded; short of
// it, the watch goes on a week at a time up to measure.MaxWatch after the change, then the change goes, since
// nothing could show it helped.
func (r *Runner) guardCase(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) Measured {
	g := *plan.Guard
	m.Guarded = true
	failed := func(err error) Measured {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	back, err := recurrences(ctx, r.Store, plan, c.Deployed, now, r.Dir)
	if err != nil {
		return failed(err)
	}
	if len(back) > 0 {
		watched, err := guardExposure(ctx, r.Store, plan, back, c.Deployed, now, r.Dir)
		if err != nil {
			return failed(err)
		}
		res := measure.Guarded(g.Incidents, g.Exposure, hitUnits(back, plan.Units), len(watched))
		m.Result = &res
		return r.noEffect(ctx, c, p, m, fmt.Sprintf("its guard failed: its problem came back after its change was in effect, first at %s (%s)",
			earliest(back).UTC().Format(time.DateTime), res.Effect), dry)
	}
	last := c.Deployed.Add(max(g.Watch, measure.MaxWatch))
	m.Due = c.Deployed.Add(g.Watch)
	watchPlan := plan
	watchPlan.Window = g.Watch
	if restarted, done, err := r.afterGap(ctx, c, p, watchPlan, m, last, dry); err != nil {
		return failed(err)
	} else if done {
		return restarted
	}
	if now.Before(m.Due) {
		m.Status, m.Why = "waiting", fmt.Sprintf("guarded: it watches for a recurrence until %s, when its exposure should reach the %d %s it needs",
			m.Due.UTC().Format(time.DateTime), g.Need, plan.Units)
		return m
	}
	end := m.Due
	for next := end.Add(week); !next.After(now) && !next.After(last); next = end.Add(week) {
		end = next
	}
	if why, err := r.readWindow(ctx, p, plan, c.Deployed, end, dry); err != nil {
		return failed(err)
	} else if why != "" {
		m.Status, m.Why = "waiting", why
		return m
	}
	watched, err := guardExposure(ctx, r.Store, plan, nil, c.Deployed, end, r.Dir)
	if err != nil {
		return failed(err)
	}
	res := measure.Guarded(g.Incidents, g.Exposure, 0, len(watched))
	m.Result = &res
	if res.Improved {
		m.Status = "improved"
		if dry {
			return m
		}
		p.Result = &res
		if err := r.Ship.save(p); err != nil {
			return failed(err)
		}
		// the note before the close, as for a measured change: written once, under an id of its own for the change
		if err := r.noteOnce(ctx, store.Note{ID: noteID("shipped", *p), Kind: store.NoteShipped, Title: plainClip("momentum guarded: "+c.Title, store.TitleMax),
			Detail: fmt.Sprintf("Its change (%s) was guarded, not measured: its problem did not come back in the %s after the change was in effect. That is no measured drop; a change that did nothing would have met a recurrence there with probability %.1f%%.",
				firstOf(p.PR, "a package"), days(end.Sub(c.Deployed)), 100*(1-res.P)),
			Effect: res.Effect, Links: prLinks(p.PR)}); err != nil {
			return failed(fmt.Errorf("the note: %w", err))
		}
		if _, err := r.Store.CloseMeasured(ctx, c.ID, 0, res.Effect, "loop:measure"); err != nil {
			return failed(err)
		}
		kept, err := r.Ship.keepLocked(c.ID, res)
		if err != nil {
			return failed(err)
		}
		r.resolve(ctx, &kept)
		return m
	}
	if end.Before(last) {
		m.Status, m.Due = "waiting", end.Add(week)
		m.Why = fmt.Sprintf("guarded: no recurrence, and %d of the %d %s of exposure its watch needs; it watches on a week at a time, until %s at the latest",
			len(watched), g.Need, plan.Units, last.UTC().Format(time.DateTime))
		return m
	}
	return r.noEffect(ctx, c, p, m, fmt.Sprintf("nothing could show it helped: its guard watched the %s it may and reached %d of the %d %s of exposure it needs, with no recurrence",
		days(last.Sub(c.Deployed)), len(watched), g.Need, plan.Units), dry)
}

// guardBack is the watch of a case a guard closed as improved: the guard said its problem would not come back, so its
// first event the signal counts after the close brings the case back, regressed, with a decided note.
func (r *Runner) guardBack(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) (Measured, bool) {
	m.Guarded = true
	back, err := recurrences(ctx, r.Store, plan, c.Closed, now, r.Dir)
	if err != nil {
		m.Status, m.Why = "failed", err.Error()
		return m, true
	}
	if len(back) == 0 {
		return Measured{}, false
	}
	m.Status = "back"
	m.Why = fmt.Sprintf("the problem its guard closed came back: %d events since its close, the first at %s", len(back), earliest(back).UTC().Format(time.DateTime))
	if dry {
		return m, true
	}
	if err := r.noteOnce(ctx, store.Note{ID: noteID("back", *p), Kind: store.NoteDecided,
		Title:  plainClip("momentum: a problem it guarded against is back: "+c.Title, store.TitleMax),
		Detail: "It came back after its guard closed the case (" + m.Why + "): the case is open again. Its lesson in tacit is not withdrawn.", Links: prLinks(p.PR)}); err != nil {
		m.Status, m.Why = "failed", "the note: "+err.Error()
		return m, true
	}
	if _, err := r.Store.RegressCase(ctx, c.ID, m.Why, "loop:measure"); err != nil {
		m.Status, m.Why = "failed", err.Error()
		return m, true
	}
	return m, true
}
