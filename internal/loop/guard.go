package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// week is the step a guard's watch moves by past its end: windows are whole weeks.
const week = 7 * 24 * time.Hour

// guardRetention is the longest a guard watches: what the store is sure to read again of its signal's source when the
// watch ends (SPEC, Measurement, Guards). The store keeps every event and drops a source's only on a rebuild, which
// reads again what the source itself still holds; the shortest such life the store knows is a transcript's, about 30
// days in its harness, which is why the store's evidence retention is 30 days.
const guardRetention = store.CaseRetention

// guardPlan stands in for a measured plan refused as too new, too rare or with no recent event (instead), when the
// case's own store evidence shows its problem happened before (SPEC, Measurement, Guards). A preventive fix for a
// problem that has stopped has no rate to drop; a guard asks whether the problem comes back.
//
// It reads the plan's span, the four windows before the boundary, from the signal's source's first event when that
// came inside it, with the sources the plan would count for the units its history counts in, read whole. The
// signal's events there fall into incidents (no gap longer than the plan's window between two of them: a stretch
// shorter than a whole week can be the weekly cycle's lull, a night or a weekend, rather than the end of a cause). An
// incident the source began with is dropped (measure.GuardCount). Its exposure is the plan's units of activity in
// which the problem's unit was in use: the signal's tool, else the tool or directory all its events share; with none
// shared, it is refused. measure.GuardAdmit sizes the watch from the posterior predictive and refuses a thin history,
// a problem whose quiet stretch says it had already stopped, and a watch past guardRetention. A refusal names the
// measured plan's beside the guard's. sourcesFor gives the sources and the omitted ones for a count in units, or a
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
	anchored := first.After(since)
	if anchored {
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
	hits, err := recurrences(ctx, s.Store, g, since, at, s.Dir)
	if err != nil {
		return Plan{}, err
	}
	history := days(at.Sub(since))
	if len(hits) == 0 {
		return refuse("the signal counts no event of %s in the %s before", units, history)
	}
	evidence, err := s.Store.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		return Plan{}, err
	}
	counted := map[string]bool{}
	for _, e := range hits {
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
	}
	tool, cwd, ok := sharedUnit(sig, units, hits)
	if !ok {
		return refuse("its events share no tool or directory, so absence cannot be counted only while the problem's unit was in use")
	}
	g.Guard = &Guard{Instead: why, Gap: window, Units: units, Tool: tool, Cwd: cwd}
	h, err := guardHistory(ctx, s.Store, g, since, at, s.Dir, anchored)
	if err != nil {
		return Plan{}, err
	}
	need, expected, reason := measure.GuardAdmit(h, guardRetention)
	if reason != "" {
		return refuse("%s", reason)
	}
	setGuard(g.Guard, h, need, expected)
	return g, nil
}

// setGuard records a counted history on a guard: its incidents, exposure and need, and its watch in whole weeks, at
// least one and at most the retention.
func setGuard(g *Guard, h measure.GuardHistory, need int, expected time.Duration) {
	g.Since, g.Incidents, g.Exposure, g.Before, g.Quiet, g.Need = h.Since, h.Incidents, h.Exposure, h.Before, h.Quiet, need
	g.Watch = measure.GuardWatch(expected, guardRetention)
}

// sharedUnit is what a guard's exposure is narrowed to: the signal's tool; else the tool every event of its history
// was of; else the directory they all ran in. ok is false when they share none: then every activity would count as
// the problem's unit in use.
func sharedUnit(sig Signal, units string, hits []store.Event) (tool, cwd string, ok bool) {
	if sig.Tool != "" && units != OwnerHours {
		return sig.Tool, "", true
	}
	same := func(of func(store.Event) string) string {
		v := of(hits[0])
		for _, e := range hits[1:] {
			if of(e) != v {
				return ""
			}
		}
		return v
	}
	if t := same(func(e store.Event) string { return e.Tool }); t != "" && units != OwnerHours {
		return t, "", true
	}
	if d := same(func(e store.Event) string { return e.Cwd }); d != "" {
		return "", d, true
	}
	return "", "", false
}

// guardUnitText says what a guard's exposure is narrowed to, for its proposal.
func guardUnitText(g Guard) string {
	switch {
	case g.Tool != "":
		return ", counting the hours in which " + g.Tool + " was used"
	case g.Cwd != "":
		return ", counting the hours of work in its directory"
	}
	return ""
}

// guardHistory counts a guard's history over [since, until) by its plan's signal, units, sources, narrowing and gap.
func guardHistory(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string, anchored bool) (measure.GuardHistory, error) {
	hits, err := recurrences(ctx, st, plan, since, until, dir)
	if err != nil {
		return measure.GuardHistory{}, err
	}
	base, err := guardExposure(ctx, st, plan, hits, since, until, dir)
	if err != nil {
		return measure.GuardHistory{}, err
	}
	times := make([]time.Time, len(hits))
	for i, e := range hits {
		times[i] = e.TS
	}
	hours := make([]int64, 0, len(base))
	for u := range base {
		hours = append(hours, u.Hour)
	}
	return measure.GuardCount(measure.Incidents(times, plan.Guard.Gap), hours, since, until, plan.Guard.Gap, anchored), nil
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

// guardExposure is a guard's exposure over a span: the plan's units of activity among its sources in which the
// problem's unit was in use (its guard's tool or directory), with the units of its hits.
func guardExposure(ctx context.Context, st *store.Store, plan Plan, hits []store.Event, since, until time.Time, dir string) (map[store.Unit]bool, error) {
	if !until.After(since) {
		return map[store.Unit]bool{}, nil
	}
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return nil, err
	}
	q.Sources = plan.Sources
	if plan.Guard != nil {
		q.Tool, q.Cwd = plan.Guard.Tool, plan.Guard.Cwd
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

// earlier is the earlier of two times.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
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

// guardDetail is what a guarded case's log says when its watch starts, with the assumption it rests on.
func guardDetail(plan Plan) string {
	g := plan.Guard
	return fmt.Sprintf("%d incidents of its problem in %d %s of exposure since %s; one recurrence fails it, %d %s of exposure with none close it as improved; it assumes the problem was still happening when the change took effect",
		g.Incidents, g.Exposure, plan.Units, g.Since.UTC().Format(time.DateTime), g.Need, plan.Units)
}

// guardAtEffect counts a guard's history again through the moment its change took effect: the plan was counted at the
// proposal, and the change may take effect much later. A watch tells the change from the problem's own end only when
// the problem was still happening then, so the stretch since the last incident is checked again, as are the need and
// the retention (measure.GuardAdmit). It returns the guard as counted then, or why its watch cannot start; dry
// changes nothing.
func (r *Runner) guardAtEffect(ctx context.Context, p *Package, plan Plan, effect time.Time, dry bool) (*Guard, string, bool, error) {
	g := *plan.Guard
	if why, err := r.readWindow(ctx, p, plan, g.Since, effect, dry); err != nil || why != "" {
		return nil, why, false, err
	}
	h, err := guardHistory(ctx, r.Store, plan, g.Since, effect, r.Dir, false)
	if err != nil {
		return nil, "", false, err
	}
	need, expected, reason := measure.GuardAdmit(h, guardRetention)
	if reason != "" {
		return nil, "its guard cannot stand at its change: " + reason + "; it must be judged by hand", true, nil
	}
	setGuard(&g, h, need, expected)
	return &g, "", false, nil
}

// guardCase is the measure step for a measuring case whose plan is a guard. Any event its signal counts since the
// change was in effect is a recurrence: the watch fails at once, and the change goes as one with no measured effect
// does (noEffect). A failed read inside the watch starts it again after the failure (afterGap). From the watch's end,
// once read, and once the recurrences are counted again through it (an event read late is still one), exposure that
// reached the guard's need with no recurrence closes the case as improved, guarded; short of it, the watch goes on a
// week at a time up to guardRetention after the change, then the change goes, since nothing could show it helped.
func (r *Runner) guardCase(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) Measured {
	g := *plan.Guard
	m.Guarded = true
	failed := func(err error) Measured {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	recurred := func(back []store.Event) Measured {
		watched, err := guardExposure(ctx, r.Store, plan, back, c.Deployed, now, r.Dir)
		if err != nil {
			return failed(err)
		}
		res := measure.Guarded(g.Incidents, g.Exposure, hitUnits(back, plan.Units), len(watched), plan.Units)
		m.Result = &res
		return r.noEffect(ctx, c, p, m, fmt.Sprintf("its guard failed: its problem came back after its change was in effect, first at %s (%s)",
			earliest(back).UTC().Format(time.DateTime), res.Effect), dry)
	}
	back, err := recurrences(ctx, r.Store, plan, c.Deployed, now, r.Dir)
	if err != nil {
		return failed(err)
	}
	if len(back) > 0 {
		return recurred(back)
	}
	last := c.Deployed.Add(guardRetention)
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
	for end.Before(last) {
		next := earlier(end.Add(week), last)
		if next.After(now) {
			break
		}
		end = next
	}
	if why, err := r.readWindow(ctx, p, plan, c.Deployed, end, dry); err != nil {
		return failed(err)
	} else if why != "" {
		m.Status, m.Why = "waiting", why
		return m
	}
	// every source has read past the watch's end now: an event of it read late is counted with the rest
	if back, err = recurrences(ctx, r.Store, plan, c.Deployed, end, r.Dir); err != nil {
		return failed(err)
	} else if len(back) > 0 {
		return recurred(back)
	}
	watched, err := guardExposure(ctx, r.Store, plan, nil, c.Deployed, end, r.Dir)
	if err != nil {
		return failed(err)
	}
	res := measure.Guarded(g.Incidents, g.Exposure, 0, len(watched), plan.Units)
	m.Result = &res
	if res.Improved {
		m.Status = "improved"
		if dry {
			return m
		}
		p.Result, p.GuardRead = &res, end
		if err := r.Ship.save(p); err != nil {
			return failed(err)
		}
		// the note before the close, as for a measured change: written once, under an id of its own for the change
		if err := r.noteOnce(ctx, store.Note{ID: noteID("shipped", *p), Kind: store.NoteShipped, Title: plainClip("momentum guarded: "+c.Title, store.TitleMax),
			Detail: fmt.Sprintf("Its change (%s) was guarded, not measured: its problem did not come back in the %s after the change was in effect, %d %s of exposure. That is no measured drop; a change that did nothing would have met a recurrence there with predictive probability %.1f%%, had the problem still been happening when the change took effect.",
				firstOf(p.PR, "a package"), days(end.Sub(c.Deployed)), len(watched), plan.Units, 100*(1-res.P)),
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
		m.Status, m.Due = "waiting", earlier(end.Add(week), last)
		m.Why = fmt.Sprintf("guarded: no recurrence, and %d of the %d %s of exposure its watch needs; it watches on a week at a time, until %s at the latest",
			len(watched), g.Need, plan.Units, last.UTC().Format(time.DateTime))
		return m
	}
	return r.noEffect(ctx, c, p, m, fmt.Sprintf("nothing could show it helped: its guard watched the %s it may and reached %d of the %d %s of exposure it needs, with no recurrence",
		days(last.Sub(c.Deployed)), len(watched), g.Need, plan.Units), dry)
}

// guardBack is the watch of a case a guard closed as improved: the guard said its problem would not come back, so the
// first event its signal counts after the change brings the case back, regressed, with a decided note. Each pass reads
// only the span its signal's source has read since the last (Package.GuardRead, from the watch's end), so an event
// read late is still read, and no pass counts the whole span since the change again.
func (r *Runner) guardBack(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) (Measured, bool) {
	m.Guarded = true
	failed := func(err error) (Measured, bool) { m.Status, m.Why = "failed", err.Error(); return m, true }
	from := p.GuardRead
	if from.IsZero() {
		from = c.Deployed
	}
	through, err := r.Store.HarnessReadThrough(ctx, plan.Signal.Source, planHarness(plan))
	if err != nil {
		return failed(err)
	}
	through = earlier(through, now)
	if !through.After(from) {
		return Measured{}, false
	}
	back, err := recurrences(ctx, r.Store, plan, from, through, r.Dir)
	if err != nil {
		return failed(err)
	}
	if len(back) == 0 {
		if !dry {
			p.GuardRead = through
			if err := r.Ship.save(p); err != nil {
				return failed(err)
			}
		}
		return Measured{}, false
	}
	m.Status = "back"
	m.Why = fmt.Sprintf("the problem its guard closed came back: %d events after its change, the first at %s", len(back), earliest(back).UTC().Format(time.DateTime))
	if dry {
		return m, true
	}
	if err := r.noteOnce(ctx, store.Note{ID: noteID("back", *p), Kind: store.NoteDecided,
		Title:  plainClip("momentum: a problem it guarded against is back: "+c.Title, store.TitleMax),
		Detail: "It came back after its guard closed the case (" + m.Why + "): the case is open again. Its lesson in tacit is not withdrawn.", Links: prLinks(p.PR)}); err != nil {
		return failed(fmt.Errorf("the note: %w", err))
	}
	if _, err := r.Store.RegressCase(ctx, c.ID, m.Why, "loop:measure"); err != nil {
		return failed(err)
	}
	p.GuardRead = through
	if err := r.Ship.save(p); err != nil {
		return failed(err)
	}
	return m, true
}
