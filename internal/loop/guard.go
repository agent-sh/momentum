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

// guardPlan stands in for a measured plan refused as too new or too rare (instead), when the case's own store evidence
// shows its problem happened before (SPEC, Measurement, Guards). A guard is a watch, not a proof: it watches for the
// problem to come back after a preventive fix, and never closes a case as improved.
//
// It reads the plan's span, the four windows before the boundary, from the signal's source's first event when that
// came inside it, with the sources the plan would count for the units its history counts in, read whole. The
// signal's events there fall into incidents (no gap longer than measure.GuardGap between two of them), and an
// incident the source began with is dropped (measure.GuardCount). Its exposure is the plan's units of activity in
// which the problem's unit was in use: the signal's tool, else the tool or directory all its events share. It is
// refused only where no watch could be kept: the case's evidence is not the signal's, its events share no unit, or
// its history holds no exposure. A refusal names the measured plan's beside the guard's. sourcesFor gives the
// sources and the omitted ones for a count in units, or a *waitingPlan.
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
	g.Guard = &Guard{Instead: why, Gap: measure.GuardGap, Units: units, Tool: tool, Cwd: cwd}
	h, err := guardHistory(ctx, s.Store, g, since, at, s.Dir, anchored)
	if err != nil {
		return Plan{}, err
	}
	if h.Exposure == 0 {
		return refuse("its history holds no exposure to count the problem's rate by")
	}
	setGuard(g.Guard, h)
	return g, nil
}

// setGuard records a counted history on a guard: its incidents, exposure and the exposure its watch runs to, and the
// whole weeks the history took to hold that much, at most the retention.
func setGuard(g *Guard, h measure.GuardHistory) {
	g.Since, g.Incidents, g.Exposure, g.Before = h.Since, h.Incidents, h.Exposure, h.Before
	g.Need, g.Watch = measure.GuardNeed(h.Incidents, h.Exposure), measure.GuardWatch(h, guardRetention)
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
	return recurrencesStored(ctx, st, plan, since, until, dir, time.Time{})
}

// recurrencesStored is recurrences of the events the store took in at or after stored.
func recurrencesStored(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string, stored time.Time) ([]store.Event, error) {
	if !until.After(since) {
		return nil, nil
	}
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return nil, err
	}
	evs, err := countStored(ctx, st, plan.Signal, since, until, dir, q.Actor, stored)
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
	return fmt.Sprintf("%d incidents of its problem in %d %s of exposure since %s; it watches until %d %s of exposure or %s, whichever comes first; one recurrence brings the case back; no recurrence closes it as guarded, with the probability that the problem, had it kept happening, would have recurred: it assumes the problem was still happening when the change took effect",
		g.Incidents, g.Exposure, plan.Units, g.Since.UTC().Format(time.DateTime), g.Need, plan.Units, days(guardRetention))
}

// guardAtEffect counts a guard's history again through the moment its change took effect: the plan was counted at the
// proposal, and the change may take effect much later. The watch is sized from that count. why says what it waits on
// (a source behind); dry changes nothing.
func (r *Runner) guardAtEffect(ctx context.Context, p *Package, plan Plan, effect time.Time, dry bool) (*Guard, string, error) {
	g := *plan.Guard
	if why, err := r.readWindow(ctx, p, plan, g.Since, effect, dry); err != nil || why != "" {
		return nil, why, err
	}
	h, err := guardHistory(ctx, r.Store, plan, g.Since, effect, r.Dir, false)
	if err != nil {
		return nil, "", err
	}
	if h.Exposure > 0 {
		setGuard(&g, h)
	}
	return &g, "", nil
}

// guardThrough is how far a guard's watch has been read since from: the earliest moment every source it counts has
// read the window from since through, at most until.
func (r *Runner) guardThrough(ctx context.Context, plan Plan, since, until time.Time) (time.Time, error) {
	through := until
	for _, source := range plan.Sources {
		read, err := r.Store.HarnessReadWindowThrough(ctx, source, planHarness(plan), since)
		if err != nil {
			return time.Time{}, err
		}
		through = earlier(through, read)
	}
	return through, nil
}

// guardCase is the measure step for a measuring case whose plan is a guard. Any event its signal counts since the
// change was in effect is a recurrence: the case comes back regressed, and the change goes as one with no measured
// effect does (noEffect). A failed read inside the watch starts it again after the failure (afterGap). The watch ends
// on read exposure: when it reached the guard's need, or 30 days after the change (guardRetention), whichever comes
// first. With no recurrence it closes the case as guarded, never improved, with the exposure watched and the
// predictive probability that the problem, had it kept happening, would have recurred within it.
func (r *Runner) guardCase(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) Measured {
	g := *plan.Guard
	m.Guarded = true
	failed := func(err error) Measured {
		m.Status, m.Why = "failed", err.Error()
		return m
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
	through, err := r.guardThrough(ctx, plan, c.Deployed, earlier(now, last))
	if err != nil {
		return failed(err)
	}
	// read after the sources' reads: an event stored late is counted with the rest
	back, err := recurrences(ctx, r.Store, plan, c.Deployed, now, r.Dir)
	if err != nil {
		return failed(err)
	}
	if len(back) > 0 {
		watched, err := guardExposure(ctx, r.Store, plan, back, c.Deployed, now, r.Dir)
		if err != nil {
			return failed(err)
		}
		res := measure.Guarded(g.Incidents, g.Exposure, hitUnits(back, plan.Units), len(watched), plan.Units)
		m.Result = &res
		return r.noEffect(ctx, c, p, m, fmt.Sprintf("regressed: its problem came back during its guard's watch, first at %s (%s)",
			earliest(back).UTC().Format(time.DateTime), res.Effect), dry)
	}
	watched, err := guardExposure(ctx, r.Store, plan, nil, c.Deployed, through, r.Dir)
	if err != nil {
		return failed(err)
	}
	if len(watched) < g.Need && through.Before(last) {
		if !now.Before(last) {
			if why, err := r.readWindow(ctx, p, plan, c.Deployed, last, dry); err != nil {
				return failed(err)
			} else if why != "" {
				m.Status, m.Why = "waiting", why
				return m
			}
		}
		m.Status, m.Due = "waiting", earlier(m.Due, last)
		m.Why = fmt.Sprintf("guarded: watching for a recurrence; %d of the %d %s of exposure it runs to, until %s at the latest",
			len(watched), g.Need, plan.Units, last.UTC().Format(time.DateTime))
		return m
	}
	res := measure.Guarded(g.Incidents, g.Exposure, 0, len(watched), plan.Units)
	m.Result, m.Status = &res, "guarded"
	if dry {
		return m
	}
	p.Result = &res
	if err := r.Ship.save(p); err != nil {
		return failed(err)
	}
	// the note before the close, under an id of its own for the change: written once. A decided note, not a shipped
	// one: a guard is no measured improvement.
	if err := r.noteOnce(ctx, store.Note{ID: noteID("guarded", *p), Kind: store.NoteDecided, Title: plainClip("momentum guarded: "+c.Title, store.TitleMax),
		Detail: fmt.Sprintf("Its change (%s) was guarded, not measured: its problem did not come back in %d %s of exposure over the %s after the change took effect. Had it kept happening, it would have recurred within that with probability P=%.1f%%. That rests on the problem still happening when the change took effect: a problem that stopped by itself shows the same. One recurrence brings the case back.",
			firstOf(p.PR, "a package"), len(watched), plan.Units, days(through.Sub(c.Deployed)), 100*res.Recur),
		Effect: res.Effect, Links: prLinks(p.PR)}); err != nil {
		return failed(fmt.Errorf("the note: %w", err))
	}
	if _, err := r.Store.CloseGuarded(ctx, c.ID, len(watched), res.Recur, res.Effect, "loop:measure"); err != nil {
		return failed(err)
	}
	kept, err := r.Ship.keepLocked(c.ID, res)
	if err != nil {
		return failed(err)
	}
	r.resolve(ctx, &kept)
	return m
}

// guardBack is the watch of a case closed as guarded: the guard said its problem would not come back, so the first
// event its signal counts after the change brings the case back, regressed, with a decided note. It needs only the
// signal. Each pass reads the events dated from the change's in-effect time that the store took in since the last
// pass began (Package.GuardRead, by the wall clock the store dates what it stores by): an event dated inside the watch
// and stored only after the close still counts, and no pass reads the whole span again.
func (r *Runner) guardBack(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) (Measured, bool) {
	m.Guarded = true
	failed := func(err error) (Measured, bool) { m.Status, m.Why = "failed", err.Error(); return m, true }
	scan := time.Now()
	until := now
	if scan.After(until) {
		until = scan
	}
	back, err := recurrencesStored(ctx, r.Store, plan, c.Deployed, until.Add(time.Minute), r.Dir, p.GuardRead)
	if err != nil {
		return failed(err)
	}
	if len(back) == 0 {
		if !dry {
			p.GuardRead = scan
			if err := r.Ship.save(p); err != nil {
				return failed(err)
			}
		}
		return Measured{}, false
	}
	m.Status = "back"
	m.Why = fmt.Sprintf("the problem it guarded against came back: %d events after its change, the first at %s", len(back), earliest(back).UTC().Format(time.DateTime))
	if dry {
		return m, true
	}
	if err := r.noteOnce(ctx, store.Note{ID: noteID("back", *p), Kind: store.NoteDecided,
		Title:  plainClip("momentum: a problem it guarded against is back: "+c.Title, store.TitleMax),
		Detail: "It came back after its guard's watch closed the case (" + m.Why + "): the case is open again. Its lesson in tacit is not withdrawn.", Links: prLinks(p.PR)}); err != nil {
		return failed(fmt.Errorf("the note: %w", err))
	}
	if _, err := r.Store.RegressCase(ctx, c.ID, m.Why, "loop:measure"); err != nil {
		return failed(err)
	}
	p.GuardRead = scan
	if err := r.Ship.save(p); err != nil {
		return failed(err)
	}
	return m, true
}
