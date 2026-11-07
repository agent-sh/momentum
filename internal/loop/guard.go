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

// guardRetention is how long a guard watches: what the store is sure to read again of its signal's source when the
// watch ends (SPEC, Measurement, Guards). The store keeps every event and drops a source's only on a rebuild, which
// reads again what the source itself still holds; the shortest such life the store knows is a transcript's, about 30
// days in its harness, the store's case retention.
const guardRetention = store.CaseRetention

// guardWeek is how long a source may stay behind past a watch's end before a problem note says so, and how long a
// signal's source may go unread after a guarded close before the watch is noted as blind: a week.
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
	evidence, err := caseEvents(ctx, s.Store, c.ID, s.Dir)
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
	g.Guard = &Guard{Instead: why, Gap: measure.GuardGap, Units: units, Tool: tool, Cwd: cwd, Begun: since, Anchored: anchored}
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
// was of; else the directory they all ran in, compared as the store keeps it now (store.CleanCwd), since a row stored
// before the store cleaned working directories keeps its form until its source is read again. ok is false when they
// share none: then every activity would count as the problem's unit in use.
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
	if d := same(func(e store.Event) string { return store.CleanCwd(e.Cwd) }); d != "" {
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

// guardRecurrences are a guard's recurrences from its watch's start: the events its signal counts, and the events of
// the case's own evidence dated from then (guardEvidence), the loop's own left out of both by one rule (store.InDir,
// the counts' ExcludeCwd). They need only the signal and the case, never a read.
func (r *Runner) guardRecurrences(ctx context.Context, c store.Case, plan Plan, from time.Time) ([]store.Event, error) {
	back, err := recurrences(ctx, r.Store, plan, from, time.Time{}, r.Dir)
	if err != nil {
		return nil, err
	}
	return r.guardEvidence(ctx, c, from, back)
}

// guardEvidence adds to back the events of the case's own evidence dated from a watch's start (the detector may file
// the problem by the case's signature in a form the signal does not count), but those back holds and the loop's own.
func (r *Runner) guardEvidence(ctx context.Context, c store.Case, from time.Time, back []store.Event) ([]store.Event, error) {
	evidence, err := r.Store.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range back {
		seen[e.ID()] = true
	}
	for _, ev := range evidence {
		id, ok := strings.CutPrefix(ev.Ref, "ev:")
		if !ok || ev.At.Before(from) || seen[id] {
			continue
		}
		e, found, err := r.Store.EventByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if !found || e.TS.Before(from) || store.InDir(e.Cwd, r.Dir) {
			continue
		}
		seen[id] = true
		back = append(back, e)
	}
	return back, nil
}

// guardWatched is a guard's exposure watched over a span: its exposure, less the units in the stretches its change
// was out of effect (Package.GuardAway), and in the one it is out of effect now, to the span's end.
func guardWatched(ctx context.Context, st *store.Store, p *Package, plan Plan, hits []store.Event, since, until time.Time, dir string) (map[store.Unit]bool, error) {
	units, err := guardExposure(ctx, st, plan, hits, since, until, dir)
	if err != nil {
		return nil, err
	}
	stretches := p.GuardAway
	if !p.OutOfEffect.IsZero() {
		stretches = append(slices.Clone(stretches), [2]time.Time{guardAwayFrom(p), until})
	}
	for u := range units {
		start := time.UnixMilli(u.Hour * 3_600_000)
		for _, away := range stretches {
			if start.Before(away[1]) && start.Add(time.Hour).After(away[0]) {
				delete(units, u)
				break
			}
		}
	}
	return units, nil
}

// guardAwayFrom is where a guard's change's stretch out of effect starts: the last pass that saw it in effect
// (Package.GuardSeen), since it may have gone out of effect any time after that pass, while no pass ran; else, for a
// package from before that was kept, the first pass that saw it out of effect.
func guardAwayFrom(p *Package) time.Time {
	if !p.GuardSeen.IsZero() && p.GuardSeen.Before(p.OutOfEffect) {
		return p.GuardSeen
	}
	return p.OutOfEffect
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
	return fmt.Sprintf("%d incidents of its problem in %d %s of exposure since %s; it watches %s for a recurrence, of its signal or of the case's own evidence; one recurrence brings the case back; no recurrence closes it as guarded, with the exposure watched and P, had the problem kept happening: a problem that stopped by itself shows the same",
		g.Incidents, g.Exposure, plan.Units, g.Since.UTC().Format(time.DateTime), days(guardRetention))
}

// guardEffectTime is when a guard's change first took effect, as the measure step sees it in effect at seen: the
// installed file's time when the plan names one, else the newest time of the change's own files in the checkout
// (CheckoutAt), when it has some and that falls between the landing and seen; else seen. A change with none of its files
// left (it only deletes) has its checkout time at the landing, which says nothing of when the checkout took it.
func guardEffectTime(c store.Case, p *Package, seen time.Time) time.Time {
	at := p.CheckoutAt
	if at.Equal(p.LandedAt) {
		at = time.Time{}
	}
	if p.Plan != nil && p.Plan.Installed != "" {
		at = p.InstalledAt
	}
	switch {
	case at.IsZero() || at.After(seen):
		return seen
	case at.Before(c.Deployed):
		return c.Deployed
	}
	return at
}

// guardBehind names the sources of a guard's plan whose newest read did not start after the end of its watch (for an
// ingest source a complete read, or one that read in part with every failed file older than its start), and the ones
// it leaves out.
//
// An ingest source that has read only in part since the end is left out a window (the plan's) after the end, as a
// stopped command source is (liveSources): a file it fails on for good would otherwise hold the close for good. So is
// one with no read at all since the end, but the signal's own source: a reader that no longer runs would hold it for
// good, and the signal's source is the one whose recurrences the watch reads, so it holds the close, with its note.
func (r *Runner) guardBehind(ctx context.Context, plan Plan, from, end, now time.Time) (behind, out []string, err error) {
	for _, source := range plan.Sources {
		read, err := r.Store.HarnessReadPast(ctx, source, planHarness(plan), from)
		if err != nil {
			return nil, nil, err
		}
		if read.After(end) {
			continue
		}
		if !slices.Contains(commandSources, source) && now.Sub(end) > plan.Window {
			partial, err := r.Store.PartialReadAfter(ctx, source, end)
			if err != nil {
				return nil, nil, err
			}
			// it reads, in part only, past the end; or it has not read since the end, and is not the signal's: left out
			if !partial.IsZero() || source != plan.Signal.Source {
				out = append(out, source)
				continue
			}
		}
		behind = append(behind, source)
	}
	return behind, out, nil
}

// guardCounted is a guard's plan with the sources its close leaves out (guardBehind) taken out of its sources, so they
// count in neither its history's recount nor its exposure watched, as a stopped source counts on neither side of a
// measured plan (countingPlan). The signal's own source stays: no plan counts without it, and its recurrences are what
// the watch reads.
func guardCounted(plan Plan, out []string) Plan {
	plan.Sources = slices.DeleteFunc(slices.Clone(plan.Sources), func(source string) bool {
		return source != plan.Signal.Source && slices.Contains(out, source)
	})
	return plan
}

// guardRecount counts a guard's history again through its watch's start: the plan was counted at the proposal, and
// the change may take effect much later. From the start on, an event is a recurrence, never history. It starts where
// the proposal's count did and drops the incident the source began with as that count did, so an event of that
// incident stored since does not count as another. The caller gives the plan with the sources its close left out
// taken out (guardCounted), and has seen each of the others read past the end, so past the start; the signal's own
// source may have read past it only in part.
func (r *Runner) guardRecount(ctx context.Context, plan Plan, from time.Time) (*Guard, error) {
	g := *plan.Guard
	begun, anchored := g.Since, false
	if !g.Begun.IsZero() {
		begun, anchored = g.Begun, g.Anchored
	}
	h, err := guardHistory(ctx, r.Store, plan, begun, from, r.Dir, anchored)
	if err != nil {
		return nil, err
	}
	if h.Exposure > 0 {
		setGuard(&g, h)
	}
	return &g, nil
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
	watched, err := guardWatched(ctx, r.Store, p, plan, back, from, now, r.Dir)
	if err != nil {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	res := measure.Guarded(g.Incidents, g.Exposure, hitUnits(back, plan.Units), len(watched), plan.Units)
	m.Result, m.Guarded = &res, true
	m = r.noEffect(ctx, c, p, m, fmt.Sprintf("regressed: its problem came back during its guard's watch, first at %s (%s)",
		earliest(back).UTC().Format(time.DateTime), res.Effect), dry)
	if m.Status == "held" && !dry {
		// its rollback held, the case keeps measuring: its status says it recurred
		if err := r.Store.GuardRecurred(ctx, c.ID, res.After.Hits); err != nil {
			r.logf("guard recurrence on %s: %v", c.ID, err)
		}
	}
	return m
}

// guardCase is the measure step for a measuring case whose plan is a guard, once its change is in effect. Any event
// its signal counts, or of the case's own evidence, from the watch's first start is a recurrence (guardRegressed): that
// is counted before any wait. The watch ends 30 days (guardRetention) after its first start, on the first pass after
// each live source of its plan has read past that, or is left out (guardBehind); then, with no recurrence, it closes
// the case as guarded, never improved, with its history counted again and the exposure watched (its stretches out of
// effect left out), both over the sources it kept (guardCounted), and P, had the problem kept happening, or that it
// was not watched at all, and why.
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
	if back, err := r.guardRecurrences(ctx, c, plan, from); err != nil {
		return failed(err)
	} else if len(back) > 0 {
		return r.guardRegressed(ctx, c, p, plan, m, back, from, now, dry)
	}
	if now.Before(end) {
		m.Status, m.Why = "waiting", fmt.Sprintf("guarded: watching for a recurrence until %s", end.UTC().Format(time.DateTime))
		return m
	}
	// each live source must have read past the end: its newest read, never a stretch read whole, which one failed run
	// would make a wait for good
	behind, out, err := r.guardBehind(ctx, plan, from, end, now)
	if err != nil {
		return failed(err)
	} else if len(behind) > 0 {
		m.Status = "waiting"
		m.Why = "guarded: its watch has ended; it closes once the store has read past its end: " + strings.Join(behind, ", ")
		if !dry && !now.Before(end.Add(guardWeek)) {
			r.problem(ctx, p, "the store is behind on "+strings.Join(behind, ", "), m.Why)
		}
		return m
	}
	// every source it keeps has read past the watch's end, and so past its start (the signal's own perhaps only in
	// part): its history is counted through the start, and its exposure watched, over those sources alone
	recorded, err := r.recordedSources(ctx, *p.Plan)
	if err != nil {
		return failed(err)
	}
	plan = guardCounted(plan, out)
	dropped := slices.DeleteFunc(slices.Clone(recorded), func(source string) bool { return slices.Contains(plan.Sources, source) })
	counted, err := r.guardRecount(ctx, plan, from)
	if err != nil {
		return failed(err)
	}
	g, plan.Guard = *counted, counted
	// the exposure watched, its change's stretches out of effect left out: nothing holds the close for it, and the
	// report says how thin the watch was
	watched, err := guardWatched(ctx, r.Store, p, plan, nil, from, end, r.Dir)
	if err != nil {
		return failed(err)
	}
	res := measure.Guarded(g.Incidents, g.Exposure, 0, len(watched), plan.Units)
	if res.Unwatched && len(p.GuardAway) > 0 {
		if used, err := guardExposure(ctx, r.Store, plan, nil, from, end, r.Dir); err != nil {
			return failed(err)
		} else if len(used) > 0 { // used, but only while the change was out of effect
			res = measure.GuardedAway(g.Incidents, g.Exposure, plan.Units)
		}
	}
	if res.Unwatched && measure.GuardNotWatched(res.Effect) == measure.GuardUnused && len(dropped) > 0 {
		// every recorded source, the ones switched off or stopped before the close (countingPlan) as well as the ones it
		// left out: was the unit used only in those?
		all := plan
		all.Sources = recorded
		if used, err := guardExposure(ctx, r.Store, all, nil, from, end, r.Dir); err != nil {
			return failed(err)
		} else if len(used) > 0 {
			res = measure.GuardedLeftOut(g.Incidents, g.Exposure, plan.Units)
		}
	}
	m.Result, m.Status = &res, "guarded"
	if dry {
		return m
	}
	p.Result, p.GuardSeq, p.Plan.Guard = &res, seq, counted
	if err := r.Ship.save(p); err != nil {
		return failed(err)
	}
	chance := "The history holds no incident after its first, so there is no rate to compare."
	switch {
	case res.Unwatched:
		chance = "It was " + measure.GuardNotWatched(res.Effect) + ", so there is no P."
	case !res.NoRate:
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
	// the sources the close left out of its counts, which no record names otherwise (a source switched off or stopped
	// is named by countingPlan's note): after the close, so a close that failed and is tried again names them once
	if left := slices.DeleteFunc(slices.Clone(out), func(source string) bool { return source == plan.Signal.Source }); len(left) > 0 {
		if err := r.Store.CaseNote(ctx, c.ID, "measurement sources", "sources left out of the guard's history and exposure watched at its watch's end, read in part or not at all since it: "+
			strings.Join(left, ", "), "loop:measure"); err != nil {
			r.logf("note on %s: %v", c.ID, err)
		}
	}
	kept, err := r.Ship.keepLocked(c.ID, res)
	if err != nil {
		return failed(err)
	}
	r.resolve(ctx, &kept)
	return m
}

// guardLostTitle starts the problem note of a case closed as guarded whose guard is gone (guardLost).
const guardLostTitle = "momentum cannot watch a guarded case's signal: "

// guardLost is the watch of a case closed as guarded whose package is gone, or holds no plan or guard (a build from
// before guards saved it, and wrote its plan back without one): with no plan its signal cannot be read, so the case's
// own evidence alone is, from its watch's start (the case's in-effect time, where the watch started), by the rule
// guardEvidence keeps, the loop's own left out. An event of it brings the case back regressed, with a decided note.
// While there is none, a problem note says its signal is not watched, and that case reopen brings it back by hand.
func (r *Runner) guardLost(ctx context.Context, c store.Case, p Package, has, dry bool) (Measured, bool) {
	m := Measured{Case: c.ID, Title: c.Title, PR: p.PR, Guarded: true}
	failed := func(err error) (Measured, bool) { m.Status, m.Why = "failed", err.Error(); return m, true }
	lost := "its package is gone"
	switch {
	case has && p.Plan == nil:
		lost = "its package holds no plan"
	case has:
		lost = "its package's plan holds no guard (a build from before guards saved it)"
	}
	back, err := r.guardEvidence(ctx, c, c.Deployed, nil)
	if err != nil {
		return failed(err)
	}
	if len(back) == 0 {
		if !dry {
			title := guardLostTitle + c.Title
			detail := fmt.Sprintf("The case closed as guarded, and %s, so its signal is not read: only an event of the case's own evidence from its watch's start, %s, brings it back. "+
				"To bring it back on other evidence: momentum case reopen %s --evidence REF --why WHY.", lost, c.Deployed.UTC().Format(time.DateTime), c.ID)
			if has {
				p.Case = c.ID
				r.problem(ctx, &p, title, detail)
			} else if err := r.guardLostNote(ctx, c.ID, title, detail); err != nil {
				r.logf("note measure:%s: %v", c.ID, err)
			}
		}
		return Measured{}, false
	}
	m.Status = "back"
	m.Why = fmt.Sprintf("the problem it guarded against came back: %d events of the case's own evidence after its watch began, the first at %s (%s, so its signal was not read)",
		len(back), earliest(back).UTC().Format(time.DateTime), lost)
	if dry {
		return m, true
	}
	id := fmt.Sprintf("back:%s:episode-%d", store.IDPart(c.ID), c.Episode)
	if has && len(p.Landed) > 0 {
		id = noteID("back", p)
	}
	if err := r.noteOnce(ctx, store.Note{ID: id, Kind: store.NoteDecided, Title: plainClip("momentum: a problem it guarded against is back: "+c.Title, store.TitleMax),
		Detail: "It came back after its guard's watch closed the case (" + m.Why + "): the case is open again. Its lesson in tacit is not withdrawn.", Links: prLinks(p.PR)}); err != nil {
		return failed(fmt.Errorf("the note: %w", err))
	}
	if _, err := r.Store.RegressCase(ctx, c.ID, m.Why, "loop:measure"); err != nil {
		return failed(err)
	}
	return m, true
}

// guardLostNote puts the problem note (measure:CASE) of a case whose package is gone, so has no record of what its
// note said (Package.Problem): once, unless the store holds a note under that id, done or not, whose title starts as
// this one's does (guardLostTitle). The measure step takes it out of the report once the case leaves its close.
func (r *Runner) guardLostNote(ctx context.Context, caseID, title, detail string) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.ID == "measure:"+caseID && strings.HasPrefix(n.Title, guardLostTitle) {
			return nil
		}
	}
	r.note(ctx, store.Note{ID: "measure:" + caseID, Kind: store.NoteProblem, Title: plainClip(title, store.TitleMax), Detail: detail})
	return nil
}

// guardBack is the watch of a case closed as guarded: the guard said its problem would not come back, so the first
// event its signal counts from the watch's first start, or of the case's own evidence dated from then, brings the case
// back, regressed, with a decided note. The case store leaves a guarded case's evidence to it. It reads the case's
// evidence on every pass, and the signal's rows the store wrote or rewrote since the last (Package.GuardSeq, the store's
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
	// the case's own evidence on every pass, whatever the sequence says: evidence filed between the closing pass's
	// read and its close is read now
	back, err := r.guardEvidence(ctx, c, guardStart(c, p), nil)
	if err != nil {
		return failed(err)
	}
	after := p.GuardSeq
	switch {
	case len(back) > 0:
	case seq == p.GuardSeq:
		return Measured{}, false
	case seq < p.GuardSeq:
		after = 0 // a store whose sequence went below the cursor (restored, or made anew): read it once from the start
	}
	if len(back) == 0 {
		if back, err = recurrencesWritten(ctx, r.Store, plan, guardStart(c, p), time.Time{}, r.Dir, after, seq); err != nil {
			return failed(err)
		}
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
