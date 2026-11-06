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

// guardRetention is how long a guard watches: what the store is sure to read again of its signal's source when the
// watch ends (SPEC, Measurement, Guards). The store keeps every event and drops a source's only on a rebuild, which
// reads again what the source itself still holds; the shortest such life the store knows is a transcript's, about 30
// days in its harness, the store's case retention.
const guardRetention = store.CaseRetention

// guardWeek is the span the watch's unit must be in use in: each week of it, the days past its fourth counted with
// the fourth.
const guardWeek = 7 * 24 * time.Hour

// guardPlan stands in for a measured plan refused as too new or too rare (instead), when the case's own store evidence
// shows its problem happened before (SPEC, Measurement, Guards). A guard is a recurrence watch, not a proof: it
// watches for the problem to come back after a preventive fix, and never closes a case as improved.
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

// setGuard records a counted history on a guard: its start, incidents and exposure.
func setGuard(g *Guard, h measure.GuardHistory) {
	g.Since, g.Incidents, g.Exposure = h.Since, h.Incidents, h.Exposure
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
	return measure.GuardCount(measure.Incidents(times, plan.Guard.Gap), hours, since, plan.Guard.Gap, anchored), nil
}

// recurrences are the events a plan's signal counts over a span in its units, the loop's own excepted (dir): of the
// owner alone for his hours, of a session for the sessions' hours.
func recurrences(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string) ([]store.Event, error) {
	return recurrencesWritten(ctx, st, plan, since, until, dir, 0, 0)
}

// recurrencesWritten is recurrences of the events whose last write came after the store's write sequence after and at
// or before through (store.Seq; either 0 for no bound). A zero until bounds no date: a watch's recurrence is any event
// of its signal from its start on, however it is dated.
func recurrencesWritten(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string, after, through int64) ([]store.Event, error) {
	if !until.IsZero() && !until.After(since) {
		return nil, nil
	}
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return nil, err
	}
	evs, err := countWritten(ctx, st, plan.Signal, since, until, dir, q.Actor, after, through)
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
	return fmt.Sprintf("%d incidents of its problem in %d %s of exposure since %s; it watches %s for a recurrence while its unit is in use every week; one recurrence brings the case back; no recurrence closes it as guarded, with P, had the problem kept happening: a problem that stopped by itself shows the same",
		g.Incidents, g.Exposure, plan.Units, g.Since.UTC().Format(time.DateTime), days(guardRetention))
}

// guardAtEffect counts a guard's history again through the moment its change first took effect: the plan was counted
// at the proposal, and the change may take effect much later. That count stands for the watch's report. why says what
// it waits on (a source behind); dry changes nothing.
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

// guardStart is where a guard's watch runs from: its change's first in-effect time, kept on the package, or the
// case's in-effect time for a package from before it was kept.
func guardStart(c store.Case, p *Package) time.Time {
	if !p.GuardFrom.IsZero() {
		return p.GuardFrom
	}
	return c.Deployed
}

// guardRegressed is the measure step for a measuring guard whose problem came back: the case comes back regressed,
// and the change goes as one with no measured effect does (noEffect).
func (r *Runner) guardRegressed(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, back []store.Event, from, now time.Time, dry bool) Measured {
	g := *plan.Guard
	watched, err := guardExposure(ctx, r.Store, plan, back, from, now, r.Dir)
	if err != nil {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	res := measure.Guarded(g.Incidents, g.Exposure, hitUnits(back, plan.Units), len(watched), plan.Units)
	m.Result, m.Guarded = &res, true
	return r.noEffect(ctx, c, p, m, fmt.Sprintf("regressed: its problem came back during its guard's watch, first at %s (%s)",
		earliest(back).UTC().Format(time.DateTime), res.Effect), dry)
}

// guardCase is the measure step for a measuring case whose plan is a guard, once its change is in effect. Any event
// its signal counts from the watch's first start is a recurrence (guardRegressed): that is counted before any wait.
// A command source's failed read inside the watch holds it until the source reads past the failure; the watch then
// goes on, its start where it was. The watch ends 30 days (guardRetention) after its first start, once its sources
// have read past that; it ends held as not in use when a week of it held no unit of its exposure, and otherwise, with
// no recurrence, closes the case as guarded, never improved, with the exposure watched and P, had the problem kept
// happening.
func (r *Runner) guardCase(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) Measured {
	g := *plan.Guard
	m.Guarded = true
	failed := func(err error) Measured {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	from := guardStart(c, p)
	end := from.Add(guardRetention)
	m.Due = end
	// the write sequence before any count: a row written after it is read by the next pass, after the close too
	seq, err := r.Store.Seq(ctx)
	if err != nil {
		return failed(err)
	}
	// no date bounds it: an event stored by now and dated past the pass's clock is one too, and the next pass reads
	// what is stored after the sequence
	if back, err := recurrences(ctx, r.Store, plan, from, time.Time{}, r.Dir); err != nil {
		return failed(err)
	} else if len(back) > 0 {
		return r.guardRegressed(ctx, c, p, plan, m, back, from, now, dry)
	}
	if gap, sources, err := r.gapIn(ctx, plan, from, earlier(now, end)); err != nil {
		return failed(err)
	} else if !gap.IsZero() {
		for _, source := range sources {
			read, err := r.Store.HarnessReadThrough(ctx, source, planHarness(plan))
			if err != nil {
				return failed(err)
			}
			if !read.After(gap) {
				m.Status, m.Why = "waiting", fmt.Sprintf("guarded: a run of %s did not read at %s, inside its watch: the watch goes on, from where it began, once it reads past it",
					source, gap.UTC().Format(time.DateTime))
				return m
			}
		}
	}
	if now.Before(end) {
		m.Status, m.Why = "waiting", fmt.Sprintf("guarded: watching for a recurrence until %s", end.UTC().Format(time.DateTime))
		return m
	}
	if why, err := r.readWindow(ctx, p, plan, from, end, dry); err != nil {
		return failed(err)
	} else if why != "" {
		m.Status, m.Why = "waiting", why
		return m
	}
	watched, err := guardExposure(ctx, r.Store, plan, nil, from, end, r.Dir)
	if err != nil {
		return failed(err)
	}
	for start := from; start.Before(end); start = start.Add(guardWeek) {
		stop := start.Add(guardWeek)
		if !stop.Add(guardWeek).Before(end) {
			stop = end // the days past its fourth week count with the fourth
		}
		used, err := guardExposure(ctx, r.Store, plan, nil, start, stop, r.Dir)
		if err != nil {
			return failed(err)
		}
		if len(used) == 0 {
			m.Status = "held"
			m.Why = fmt.Sprintf("guarded: not in use: its watch saw no unit of its exposure from %s to %s, so its quiet says nothing; it goes on watching for a recurrence",
				start.UTC().Format(time.DateOnly), stop.UTC().Format(time.DateOnly))
			if !dry && !p.GuardIdle {
				if err := r.noteStepSkip(ctx, c, "measure", "guard", m.Why); err != nil {
					r.logf("guard idle note: %v", err)
				}
				p.GuardIdle = true
				if err := r.Ship.save(p); err != nil {
					return failed(err)
				}
			}
			return m
		}
		if stop.Equal(end) {
			break
		}
	}
	res := measure.Guarded(g.Incidents, g.Exposure, 0, len(watched), plan.Units)
	m.Result, m.Status = &res, "guarded"
	if dry {
		return m
	}
	p.Result, p.GuardSeq = &res, seq
	if err := r.Ship.save(p); err != nil {
		return failed(err)
	}
	chance := "The history holds no incident after its first, so there is no rate to compare."
	if !res.NoRate {
		chance = fmt.Sprintf("Had it kept happening, it would have recurred within that with probability P=%.1f%%.", 100*res.Recur)
	}
	// the note before the close, under an id of its own for the change: written once. A decided note, not a shipped
	// one: a guard is no measured improvement.
	if err := r.noteOnce(ctx, store.Note{ID: noteID("guarded", *p), Kind: store.NoteDecided, Title: plainClip("momentum guarded: "+c.Title, store.TitleMax),
		Detail: fmt.Sprintf("Its change (%s) was guarded, not measured: its problem did not come back in the %s after the change first took effect, %d %s of exposure. %s A problem that stopped by itself shows the same. One recurrence brings the case back.",
			firstOf(p.PR, "a package"), days(guardRetention), len(watched), plan.Units, chance),
		Effect: res.Effect, Links: prLinks(p.PR)}); err != nil {
		return failed(fmt.Errorf("the note: %w", err))
	}
	if _, err := r.Store.CloseGuarded(ctx, c.ID, g.Incidents, g.Exposure, len(watched), res.Recur, res.Effect, "loop:measure"); err != nil {
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
// event its signal counts from the watch's first start brings the case back, regressed, with a decided note. It needs
// only the signal. Each pass reads the rows the store wrote or rewrote since the last (Package.GuardSeq, the store's
// write sequence): an event dated inside the watch and stored after the close counts, and so does a row rewritten
// into a form the signal counts. While the signal's source is stopped, a problem note says no recurrence can be seen.
func (r *Runner) guardBack(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, now time.Time, dry bool) (Measured, bool) {
	m.Guarded = true
	failed := func(err error) (Measured, bool) { m.Status, m.Why = "failed", err.Error(); return m, true }
	if live, _ := r.Ship.liveSources(ctx, []string{plan.Signal.Source}, guardWeek, now, planHarness(plan)); len(live) == 0 {
		if !dry {
			r.problem(ctx, p, "the signal's source stopped: "+plan.Signal.Source,
				"The case closed as guarded, and no recurrence of its problem can be seen while its signal's source is stopped.")
		}
	} else if !dry && strings.HasPrefix(p.Problem, "the signal's source stopped:") {
		r.resolve(ctx, p)
	}
	seq, err := r.Store.Seq(ctx)
	if err != nil {
		return failed(err)
	}
	if seq <= p.GuardSeq {
		return Measured{}, false
	}
	back, err := recurrencesWritten(ctx, r.Store, plan, guardStart(c, p), time.Time{}, r.Dir, p.GuardSeq, seq)
	if err != nil {
		return failed(err)
	}
	if len(back) == 0 {
		if !dry {
			p.GuardSeq = seq
			if err := r.Ship.save(p); err != nil {
				return failed(err)
			}
		}
		return Measured{}, false
	}
	m.Status = "back"
	m.Why = fmt.Sprintf("the problem it guarded against came back: %d events after its watch began, the first at %s", len(back), earliest(back).UTC().Format(time.DateTime))
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
	p.GuardSeq = seq
	if err := r.Ship.save(p); err != nil {
		return failed(err)
	}
	return m, true
}
