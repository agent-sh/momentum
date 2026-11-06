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

// Measured is what the measure step did with one case.
type Measured struct {
	Case   string          `json:"case"`
	Title  string          `json:"title"`
	PR     string          `json:"pr,omitempty"`
	Status string          `json:"status"` // measuring, waiting, improved, guarded, rolled back, held, back, failed
	Result *measure.Result `json:"result,omitempty"`
	Due    time.Time       `json:"due,omitzero"` // when its window after the change ends
	Why    string          `json:"why,omitempty"`
	// Guarded says its plan is a guard: it watches for a recurrence of a problem that had stopped, and measures no drop.
	Guarded bool `json:"guarded,omitempty"`
}

// The units a plan's measurement counts in (internal/measure): the hours of the sessions (of the signal's harness,
// when it names one) in which they did anything; of those, the hours in which the owner spoke, for a signal of his
// own words; and the machine's hours, for a signal of events no session holds (an incident, a capacity run).
const (
	SessionHours = "session hours"
	OwnerHours   = "owner hours"
	MachineHours = "machine hours"
)

// unitsOf is what a signal is counted in, by what it counts before the change: the hours of the sessions when every
// event it counts is a session's, the hours in which the owner spoke when every one is his too, and the machine's
// hours otherwise.
func unitsOf(counted []store.Event) string {
	switch {
	case len(counted) == 0 || slices.ContainsFunc(counted, func(e store.Event) bool { return e.Session == "" }):
		return MachineHours
	case slices.ContainsFunc(counted, func(e store.Event) bool { return e.Actor != store.ActorOwner }):
		return SessionHours
	}
	return OwnerHours
}

// Tally counts a signal over a span in its units: the units in which it counts an event, and the units of activity
// they are among.
func Tally(ctx context.Context, st *store.Store, sig Signal, units string, since, until time.Time, excludeCwd string) (measure.Counts, error) {
	q, err := activityQuery(sig, units, since, until, excludeCwd)
	if err != nil {
		return measure.Counts{}, err
	}
	evs, err := Count(ctx, st, sig, since, until, excludeCwd, q.Actor)
	if err != nil {
		return measure.Counts{}, err
	}
	return tally(ctx, st, sig, units, evs, since, until, excludeCwd)
}

// tally is Tally over the events the signal counted already. The activity holds every unit the signal counts.
func tally(ctx context.Context, st *store.Store, sig Signal, units string, evs []store.Event, since, until time.Time, excludeCwd string) (measure.Counts, error) {
	return tallySources(ctx, st, sig, units, evs, since, until, excludeCwd, nil)
}

func tallySources(ctx context.Context, st *store.Store, sig Signal, units string, evs []store.Event, since, until time.Time, excludeCwd string, sources []string) (measure.Counts, error) {
	q, err := activityQuery(sig, units, since, until, excludeCwd)
	if err != nil {
		return measure.Counts{}, err
	}
	q.Sources = sources
	base, err := st.Activity(ctx, q)
	if err != nil {
		return measure.Counts{}, err
	}
	hits := map[store.Unit]bool{}
	for _, e := range evs {
		if q.Actor != "" && e.Actor != q.Actor {
			continue
		}
		if q.BySession && e.Session == "" {
			continue // a sessions' measurement counts what sessions hold; the plan's units say its signal counts only that
		}
		u := store.UnitOf(e, q.BySession)
		hits[u], base[u] = true, true
	}
	return measure.Counts{Hits: len(hits), Base: len(base)}, nil
}

// activityQuery picks the same activity for a tally and its source-read check.
func activityQuery(sig Signal, units string, since, until time.Time, excludeCwd string) (store.ActivityQuery, error) {
	q := store.ActivityQuery{Since: since, Until: until, ExcludeCwd: excludeCwd}
	switch units {
	case SessionHours:
		q.BySession, q.Harness = true, sig.Harness
	case OwnerHours:
		q.BySession, q.Harness, q.Actor = true, sig.Harness, store.ActorOwner
	case MachineHours:
	default:
		return store.ActivityQuery{}, fmt.Errorf("a measurement in %q: %s, %s or %s", units, SessionHours, OwnerHours, MachineHours)
	}
	return q, nil
}

func unreadWindow(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string) ([]string, error) {
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return nil, err
	}
	q.Sources = plan.Sources
	return st.UnreadSources(ctx, plan.Signal.Source, q)
}

// readWindow holds a decision until every source counted has read past its end.
func (r *Runner) readWindow(ctx context.Context, p *Package, plan Plan, since, until time.Time, dry bool) (string, error) {
	unread, err := unreadWindow(ctx, r.Store, plan, since, until, r.Dir)
	if err != nil {
		return "", err
	}
	if len(unread) == 0 {
		if !dry && !p.ReadWait.IsZero() && !until.Before(p.ReadUntil) {
			p.ReadWait, p.ReadUntil = time.Time{}, time.Time{}
			r.resolve(ctx, p)
			if err := r.Ship.save(p); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	why := "the store has not read past the window's end: " + strings.Join(unread, ", ")
	if !dry {
		if p.ReadWait.IsZero() {
			p.ReadWait = r.now()
		}
		p.ReadUntil = until
		if err := r.Ship.save(p); err != nil {
			return "", err
		}
		if !r.now().Before(p.ReadWait.Add(p.Plan.Window)) {
			r.problem(ctx, p, "the store is behind on "+strings.Join(unread, ", "), why)
		}
	}
	return why, nil
}

// takes reports whether a run narrowed to a case or to families takes this case.
func (o Options) takes(c store.Case) bool {
	return (o.Case == "" || c.ID == o.Case) && (len(o.Families) == 0 || slices.Contains(o.Families, c.Family))
}

// Measure is the measure step, deterministic. A deployed case starts measuring once its change is in effect (the
// installed build holds it, or the repository's checkout does when no installed file is named), with its plan's
// window and count before; until then it waits, and a problem note says so after a window. A measuring case whose
// window after its change has ended and been read is measured against its independent baseline: one whose share fell
// beyond chance (measure.Compare) closes as improved, its change kept, with a shipped note of what it did; one
// whose share did not has its change rolled back, with a decided note. A window after too quiet for any count to
// show an effect (measure.Measurable) waits one more window, then counts both; still too quiet, the change is
// rolled back, since nothing could show it helped. While the loop is not live, it rolls nothing back and says so.
// A case closed as improved comes back only through its completed watch windows (measure.Back). The
// options narrow it as they narrow the run. A dry measure says what it would do and changes nothing.
func (r *Runner) Measure(ctx context.Context, o Options) ([]Measured, error) {
	if r.Ship == nil {
		return nil, nil
	}
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseDeployed, store.CaseMeasuring}})
	if err != nil {
		return nil, err
	}
	var out []Measured
	bad := map[string]bool{}
	if !o.Dry {
		notes, err := r.Store.Notes(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range notes {
			id, ok := strings.CutPrefix(n.ID, "measure:")
			if !ok || !n.Done.IsZero() {
				continue
			}
			c, err := r.Store.CaseByID(ctx, id)
			if err != nil {
				if o.Case == "" || o.Case == id {
					out = append(out, Measured{Case: id, Status: "failed", Why: err.Error()})
					r.logf("note %s: %v", n.ID, err)
					bad[id] = true
				}
				continue
			}
			if !o.takes(c) || c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring || (c.Status == store.CaseClosed && (c.Reason == store.Improved || c.Reason == store.Guarded)) {
				continue
			}
			cleanup := func() error {
				unlock, err := r.Ship.lock(ctx, id)
				if err != nil {
					return err
				}
				defer unlock()
				p, ok, err := r.Ship.Load(id)
				if err != nil {
					return err
				}
				if c.Status == store.CaseTesting && !p.ProposalAt.IsZero() {
					return nil
				}
				if _, _, err := r.Store.DoneNote(ctx, n.ID, r.now()); err != nil {
					return err
				}
				if ok {
					p.Problem = ""
					return r.Ship.save(&p)
				}
				return nil
			}
			if err := cleanup(); err != nil {
				out = append(out, r.measureFailure(ctx, c, err, false))
				bad[id] = true
				r.logf("note %s: %v", n.ID, err)
			}

		}
	}
	for _, c := range cs {
		if o.takes(c) && !bad[c.ID] {
			out = append(out, r.measureCase(ctx, c, o.Dry))
		}
	}
	closed, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseClosed}})
	if err != nil {
		return out, err
	}
	for _, c := range closed {
		if (c.Reason == store.Improved || c.Reason == store.Guarded) && o.takes(c) && !bad[c.ID] {
			if m, ok := r.watch(ctx, c, o.Dry); ok {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// measureFailure treats a busy case as a passive hold. Measurement reserves no run.
func (r *Runner) measureFailure(ctx context.Context, c store.Case, err error, dry bool) Measured {
	m := Measured{Case: c.ID, Title: c.Title, Status: "failed", Why: err.Error()}
	if held(err) {
		m.Status = "held"
		if !dry {
			if err := r.noteStepSkip(ctx, c, "measure", "ship", m.Why); err != nil {
				r.logf("measure waiting note: %v", err)
			}
		}
	}
	return m
}

func (r *Runner) legacyPlan(ctx context.Context, c store.Case, dry bool) Measured {
	m := Measured{Case: c.ID, Title: c.Title, Status: "held",
		Why: "its plan has no time it was counted at or no units: it must be measured by hand"}
	if !dry {
		if err := r.noteStepSkip(ctx, c, "measure", "plan", m.Why); err != nil {
			r.logf("legacy measurement note: %v", err)
		}
	}
	return m
}

// measureCase is the measure step for one deployed or measuring case.
func (r *Runner) measureCase(ctx context.Context, c store.Case, dry bool) Measured {
	// Read the pass clock before loading the package. A ship operation that completed before the lock is taken
	// must be visible in that load. Keep the lock through every package write and case decision.
	now := r.now()
	if !dry {
		unlock, err := r.Ship.lock(ctx, c.ID)
		if err != nil {
			return r.measureFailure(ctx, c, err, dry)
		}
		defer unlock()
	}
	p, ok, err := r.Ship.Load(c.ID)
	m := Measured{Case: c.ID, Title: c.Title, PR: p.PR}
	failed := func(why string) Measured {
		m.Status, m.Why = "failed", why
		return m
	}
	switch {
	case err != nil:
		return failed(err.Error())
	case !ok || p.Plan == nil:
		return failed("its package or plan is gone")
	case p.State == RolledBack || p.State == RollingBack:
		// its revert went in, or is journaled to go in, and the case did not follow: finish that, whoever began it, before
		// anything is measured - a change whose revert is on its way is not a change to keep
		m.Status, m.Why = "rolled back", "its change was rolled back already; the case goes back to testing"
		if p.State == RollingBack {
			m.Why = "its rollback was begun already; it is finished before anything is measured"
		}
		if !dry {
			if _, err := r.Ship.rollBackLocked(ctx, c.ID, "its change was rolled back"); err != nil {
				if held(err) {
					return r.measureFailure(ctx, c, err, dry)
				}
				return failed(err.Error())
			}
			if p, _, err = r.Ship.Load(c.ID); err == nil {
				r.resolve(ctx, &p)
			}
		}
		return m
	case p.Plan.At.IsZero() || p.Plan.Units == "":
		return r.legacyPlan(ctx, c, dry)
	}
	plan, why, err := r.countingPlan(ctx, &p, dry)
	if err != nil {
		return failed(err.Error())
	}
	if why != "" {
		m.Status, m.Why = "waiting", why
		return m
	}
	if plan.Guard != nil && c.Status == store.CaseMeasuring {
		// a guard's recurrence counts from its watch's first start, before any wait or restart
		from := guardStart(c, &p)
		if back, err := recurrences(ctx, r.Store, plan, from, now.Add(time.Minute), r.Dir); err != nil {
			return failed(err.Error())
		} else if len(back) > 0 {
			return r.guardRegressed(ctx, c, &p, plan, m, back, from, now, dry)
		}
	}
	since, why, err := r.Ship.InEffect(ctx, &p)
	if err != nil {
		return failed(err.Error())
	}
	if !dry {
		if err := r.Ship.save(&p); err != nil {
			return failed(err.Error())
		}
	}
	if why != "" {
		m.Status, m.Why = "waiting", "its change is not in effect: "+why
		from := p.LandedAt
		if c.Status == store.CaseMeasuring {
			if p.OutOfEffect.IsZero() {
				p.OutOfEffect = now
				if !dry {
					if err := r.Ship.save(&p); err != nil {
						return failed(err.Error())
					}
				}
			}
			from = p.OutOfEffect
		}
		if !dry && !now.Before(from.Add(plan.Window)) {
			r.problem(ctx, &p, "momentum's change is not in effect: "+c.Title,
				fmt.Sprintf("It has waited %s and %s. Its window after starts again when it is in effect.", days(now.Sub(from)), why))
		}
		return m
	}
	if c.Status == store.CaseDeployed && plan.Guard != nil {
		// the watch's report rests on its history counted through the change's first effect: the plan was counted at
		// the proposal
		g, why, err := r.guardAtEffect(ctx, &p, plan, since, dry)
		switch {
		case err != nil:
			return failed(err.Error())
		case why != "":
			m.Status, m.Why, m.Guarded = "waiting", why, true
			return m
		}
		plan.Guard, p.Plan.Guard = g, g
		if p.GuardFrom.IsZero() {
			p.GuardFrom = since
		}
	}
	if plan.Guard != nil && c.Status == store.CaseMeasuring && !p.OutOfEffect.IsZero() {
		// in effect again: the watch goes on from its first start
		if !dry {
			p.OutOfEffect = time.Time{}
			if err := r.Ship.save(&p); err != nil {
				return failed(err.Error())
			}
			r.resolve(ctx, &p)
		}
	}
	if c.Status == store.CaseDeployed || plan.Guard == nil && !p.OutOfEffect.IsZero() {
		m.Status, m.Due = "measuring", since.Add(plan.Window)
		if plan.Guard != nil {
			m.Due, m.Guarded = since.Add(guardRetention), true
			m.Why = fmt.Sprintf("guarded, not measured: it watches %s for a recurrence while its unit is in use every week", days(guardRetention))
		}
		if !dry {
			switch {
			case c.Status == store.CaseDeployed && plan.Guard != nil:
				_, err = r.Store.StartGuarding(ctx, c.ID, guardRetention, plan.Guard.Incidents, plan.Guard.Exposure, since, guardDetail(plan), "loop:measure")
			case c.Status == store.CaseDeployed:
				_, err = r.Store.StartMeasuring(ctx, c.ID, plan.Window, plan.Before, since, "loop:measure")
			default:
				_, err = r.Store.RestartMeasuring(ctx, c.ID, since, "in effect again from", "loop:measure")
			}
			if err != nil {
				return failed(err.Error())
			}
			p.OutOfEffect = time.Time{}
			if err := r.Ship.save(&p); err != nil {
				return failed(err.Error())
			}
			r.resolve(ctx, &p)
		}
		return m
	}
	if plan.Guard != nil {
		return r.guardCase(ctx, c, &p, plan, m, now, dry)
	}
	m.Due = c.Deployed.Add(plan.Window)
	if restarted, done, err := r.afterGap(ctx, c, &p, plan, m, m.Due, dry); err != nil {
		return failed(err.Error())
	} else if done {
		return restarted
	}
	if now.Before(m.Due) {
		m.Status = "waiting"
		return m
	}
	if why, err := r.readWindow(ctx, &p, plan, c.Deployed, m.Due, dry); err != nil {
		return failed(err.Error())
	} else if why != "" {
		m.Status, m.Why = "waiting", why
		return m
	}
	before := measure.Counts{Hits: plan.Before, Base: plan.BeforeBase}
	after, err := tallyPlan(ctx, r.Store, plan, c.Deployed, m.Due, r.Dir)
	if err != nil {
		return failed(err.Error())
	}
	if !measure.Measurable(before, after.Base) {
		m.Due = c.Deployed.Add(2 * plan.Window)
		if restarted, done, err := r.afterGap(ctx, c, &p, plan, m, m.Due, dry); err != nil {
			return failed(err.Error())
		} else if done {
			return restarted
		}
		if now.Before(m.Due) {
			m.Status, m.Why = "waiting", fmt.Sprintf("its window after was too quiet to show anything (%d active hours, after %d of %d before): it waits one more window",
				after.Base, before.Hits, before.Base)
			return m
		}
		if why, err := r.readWindow(ctx, &p, plan, c.Deployed, m.Due, dry); err != nil {
			return failed(err.Error())
		} else if why != "" {
			m.Status, m.Why = "waiting", why
			return m
		}
		if after, err = tallyPlan(ctx, r.Store, plan, c.Deployed, m.Due, r.Dir); err != nil {
			return failed(err.Error())
		}
	}
	res, compared := measure.Compare(before, after)
	if compared {
		m.Result = &res
	}
	if compared && res.Improved {
		m.Status = "improved"
		if dry {
			return m
		}
		p.Result = &res
		if err := r.Ship.save(&p); err != nil {
			return failed(err.Error())
		}
		// The note before the close, under an id of its own for this change: a run that stops or fails after the decision
		// is recorded would leave a close no note goes with, and nothing writes it again; a run that stops before it
		// decides again and finds the note written.
		if err := r.noteOnce(ctx, store.Note{ID: noteID("shipped", p), Kind: store.NoteShipped, Title: plainClip("momentum improved: "+c.Title, store.TitleMax),
			Detail: fmt.Sprintf("Its change (%s) measurably helped over the %s after it was in effect.", firstOf(p.PR, "a package"), days(m.Due.Sub(c.Deployed))),
			Effect: res.Effect, Links: prLinks(p.PR)}); err != nil {
			return failed("the note: " + err.Error())
		}
		if _, err := r.Store.CloseMeasured(ctx, c.ID, after.Hits, res.Effect, "loop:measure"); err != nil {
			return failed(err.Error())
		}
		if p, err = r.Ship.keepLocked(c.ID, res); err != nil {
			return failed(err.Error())
		}
		r.resolve(ctx, &p)
		return m
	}
	why = "no measured effect: " + res.Effect
	switch {
	case before.Base == 0:
		why = "nothing to measure it by: no activity in the window before its change"
	case after.Base == 0:
		why = "nothing could show it helped: no activity in the two windows after its change, too quiet to tell"
	case !measure.Measurable(before, after.Base):
		why = "nothing could show it helped: two windows after it were too quiet to tell (" + res.Effect + ")"
	}
	return r.noEffect(ctx, c, &p, m, why, dry)
}

// noEffect is what the measure step does with a change that showed no effect, why: the ship tool rolls it back, only
// while the loop is live and never for a change a person landed by hand; otherwise the rollback is held with a decided
// note, once, and the case keeps measuring.
func (r *Runner) noEffect(ctx context.Context, c store.Case, p *Package, m Measured, why string, dry bool) Measured {
	failed := func(why string) Measured {
		m.Status, m.Why = "failed", why
		return m
	}
	m.Status, m.Why = "rolled back", why
	if dry && len(p.Hand) > 0 {
		m.Status, m.Why = "held", why+"; it was landed by hand, so the loop rolls nothing back"
	}
	if dry {
		return m
	}
	if !r.Ship.Live || len(p.Hand) > 0 {
		m.Status, m.Why = "held", why+"; the loop is not live, so it rolls nothing back"
		detail := why + ". The loop is not live, so it rolls nothing back. To roll it back: momentum ship rollback " + c.ID + " --why ..."
		if len(p.Hand) > 0 {
			// a change landed by hand is undone by hand: the loop never pushes to paths it may not land
			m.Why = why + "; it was landed by hand, so the loop rolls nothing back"
			detail = why + ". It was landed by hand, so the loop rolls nothing back. To roll it back, revert it through a pull request; the case keeps measuring with its rollback held."
		}
		if !p.Held {
			// the note before the flag that says it was written, under an id of its own, so it is written once
			if err := r.noteOnce(ctx, store.Note{ID: noteID("held", *p), Kind: store.NoteDecided, Title: plainClip("momentum holds the rollback of its change for: "+c.Title, store.TitleMax),
				Detail: detail,
				Links:  prLinks(p.PR)}); err != nil {
				return failed("the note: " + err.Error())
			}
			p.Held = true
			if err := r.Ship.save(p); err != nil {
				return failed(err.Error())
			}
		}
		return m
	}
	if _, err := r.Ship.rollBackLocked(ctx, c.ID, why); err != nil {
		if held(err) {
			return r.measureFailure(ctx, c, err, dry)
		}
		m.Status, m.Why = "failed", "the rollback: "+err.Error()
		r.rollbackFailed(ctx, c, err)
		return m
	}
	if loaded, _, err := r.Ship.Load(c.ID); err == nil {
		*p = loaded
		r.resolve(ctx, p)
	}
	return m // the rollback's receipt put its note
}

// rollbackFailed counts a rollback that did not go through; the second in a row puts a problem note in the report,
// once: a revert that conflicts with a later commit fails the same way on every run.
func (r *Runner) rollbackFailed(ctx context.Context, c store.Case, cause error) {
	p, ok, err := r.Ship.Load(c.ID)
	if err != nil || !ok {
		return
	}
	p.RollbackFailures++
	clean, err := Redact(ctx, cause.Error())
	if err != nil {
		r.logf("rollback cause: %v", err)
		return
	}
	clean = store.CaseNoteText(clean)
	if p.RollbackProblem != clean {
		if err := r.Store.CaseNote(ctx, c.ID, "not rolled back", clean, "loop:measure"); err != nil {
			r.logf("rollback note: %v", err)
			return
		}
		p.RollbackProblem = clean
	}
	if err := r.Ship.save(&p); err != nil {
		r.logf("rollback failure record: %v", err)
		return
	}
	if p.RollbackFailures >= 2 {
		r.problem(ctx, &p, "momentum cannot roll back its change for: "+c.Title,
			fmt.Sprintf("Its measurement showed no effect, and the rollback failed %d times in a row: %v. The next run tries again.", p.RollbackFailures, cause))
	}
}

// watch finishes an interrupted keep, then reads each ended window since the close once. The windows do not
// overlap. Back needs three consecutive windows at the old share, so repeated runs cannot add windows.
func (r *Runner) watch(ctx context.Context, c store.Case, dry bool) (Measured, bool) {
	now := r.now()
	if !dry {
		unlock, err := r.Ship.lock(ctx, c.ID)
		if err != nil {
			return r.measureFailure(ctx, c, err, dry), true
		}
		defer unlock()
	}
	p, ok, err := r.Ship.Load(c.ID)
	m := Measured{Case: c.ID, Title: c.Title, PR: p.PR}
	failed := func(err error) (Measured, bool) { m.Status, m.Why = "failed", err.Error(); return m, true }
	if err != nil {
		return failed(err)
	}
	if !ok || (p.State != Kept && p.State != Landed) || p.Plan == nil {
		return Measured{}, false
	}
	if p.Plan.At.IsZero() || p.Plan.Units == "" {
		return r.legacyPlan(ctx, c, dry), true
	}
	if p.Plan.Window <= 0 {
		return failed(fmt.Errorf("its plan has no window"))
	}
	if p.Plan.Guard != nil {
		// a case closed as guarded is watched by its signal alone: no recount of its history, no other source
		if p.State == Landed && p.Result != nil && !dry {
			if p, err = r.Ship.keepLocked(c.ID, *p.Result); err != nil {
				return failed(err)
			}
		}
		return r.guardBack(ctx, c, &p, *p.Plan, m, now, dry)
	}
	plan, why, err := r.countingPlan(ctx, &p, dry)
	if err != nil {
		return failed(err)
	}
	if why != "" {
		m.Status, m.Why = "waiting", why
		return m, true
	}
	if !slices.Equal(p.WatchSources, plan.Sources) {
		p.WatchSources = slices.Clone(plan.Sources)
		p.WatchRecent = nil
		// Recount past windows under the same sources before advancing the watch.
		p.WatchWindow = 0
	}
	if p.State == Landed {
		if p.Result == nil {
			// A close from before results were saved ahead of it: recover the window's activity.
			if why, err := r.readWindow(ctx, &p, plan, c.Deployed, c.Deployed.Add(p.Plan.Window), dry); err != nil {
				return failed(err)
			} else if why != "" {
				m.Status, m.Why = "waiting", why
				return m, true
			}
			before := measure.Counts{Hits: plan.Before, Base: plan.BeforeBase}
			after, err := tallyPlan(ctx, r.Store, plan, c.Deployed, c.Deployed.Add(p.Plan.Window), r.Dir)
			if err != nil {
				return failed(err)
			}
			if !measure.Measurable(before, after.Base) {
				if why, err := r.readWindow(ctx, &p, plan, c.Deployed, c.Deployed.Add(2*p.Plan.Window), dry); err != nil {
					return failed(err)
				} else if why != "" {
					m.Status, m.Why = "waiting", why
					return m, true
				}
				after, err = tallyPlan(ctx, r.Store, plan, c.Deployed, c.Deployed.Add(2*p.Plan.Window), r.Dir)
				if err != nil {
					return failed(err)
				}
			}
			after.Hits = c.Measurement.After
			res, _ := measure.Compare(before, after)
			p.Result = &res
		}
		if !dry {
			p, err = r.Ship.keepLocked(c.ID, *p.Result)
			if err != nil {
				return failed(err)
			}
		}
	}
	if p.Result == nil {
		return failed(fmt.Errorf("its kept package has no measurement"))
	}
	ended := int(now.Sub(c.Closed) / p.Plan.Window)
	for j := p.WatchWindow + 1; j <= ended; j++ {
		start := c.Closed.Add(time.Duration(j-1) * p.Plan.Window)
		// a window that holds an incomplete run of a counted source is never read: its share says nothing, so it breaks
		// the run of windows at the old share, and the watch goes on past it
		if gap, _, err := r.gapIn(ctx, plan, start, start.Add(p.Plan.Window)); err != nil {
			return failed(err)
		} else if !gap.IsZero() {
			p.WatchRecent, p.WatchWindow = nil, j
			if !dry {
				if err := r.Ship.save(&p); err != nil {
					return failed(err)
				}
			}
			continue
		}
		if why, err := r.readWindow(ctx, &p, plan, start, start.Add(p.Plan.Window), dry); err != nil {
			return failed(err)
		} else if why != "" {
			m.Status, m.Why = "waiting", why
			return m, true
		}
		recent, err := tallyPlan(ctx, r.Store, plan, start, start.Add(p.Plan.Window), r.Dir)
		if err != nil {
			return failed(err)
		}
		p.WatchRecent = append(p.WatchRecent, recent)
		if len(p.WatchRecent) > 3 {
			p.WatchRecent = p.WatchRecent[len(p.WatchRecent)-3:]
		}
		if measure.Back(measure.Counts{Hits: plan.Before, Base: plan.BeforeBase}, p.WatchRecent) {
			m.Status = "back"
			m.Why = fmt.Sprintf("%d of %d active hours in window %d since its close, the third consecutive window (of %s) at its old share, %d of %d before its change",
				recent.Hits, recent.Base, j, days(p.Plan.Window), plan.Before, plan.BeforeBase)
			if dry {
				return m, true
			}
			// the note first, under an id of its own for this window: a run that stops after the case is open again would leave
			// it no note, and a run that stops before reads the same windows again and finds the note written
			if err := r.noteOnce(ctx, store.Note{ID: noteID(fmt.Sprintf("back-%d", j), p), Kind: store.NoteDecided,
				Title:  plainClip("momentum: a problem it fixed is back: "+c.Title, store.TitleMax),
				Detail: "Back at its old share for three consecutive windows (" + m.Why + "): the case is open again. Its lesson in tacit is not withdrawn.", Links: prLinks(p.PR)}); err != nil {
				return failed(fmt.Errorf("the note: %w", err))
			}
			if _, err := r.Store.RegressCase(ctx, c.ID, m.Why, "loop:measure"); err != nil {
				return failed(err)
			}
			p.WatchWindow = j
			if err := r.Ship.save(&p); err != nil {
				return failed(err)
			}
			return m, true
		}
		p.WatchWindow = j
		if !dry {
			if err := r.Ship.save(&p); err != nil {
				return failed(err)
			}
		}
	}
	return Measured{}, false
}

// problem puts the case's problem note (measure:CASE) in the report, passive, once for what it says: a note the
// owner resolved stands again only when what it says changes.
func (r *Runner) problem(ctx context.Context, p *Package, title, detail string) {
	if p.Problem == title {
		return
	}
	n := store.Note{ID: "measure:" + p.Case, Kind: store.NoteProblem, Title: plainClip(title, store.TitleMax), Detail: detail, Links: prLinks(p.PR)}
	r.note(ctx, n)
	p.Problem = title
	r.Ship.save(p)
}

// resolve takes the case's problem note out of the report once what it said is over.
func (r *Runner) resolve(ctx context.Context, p *Package) {
	if p.Problem == "" {
		return
	}
	if _, _, err := r.Store.DoneNote(ctx, "measure:"+p.Case, r.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
		r.logf("note measure:%s: %v", p.Case, err)
		return
	}
	p.Problem = ""
	r.Ship.save(p)
}

// noteID names the note of a decision about a change by what it is and the change it is about (the package's last landed
// commit), as a decision of the same change always gets the same id: the note is written once however many times a run
// stops and comes back to the decision. A case that comes back and is changed again has a change, and notes, of its own.
func noteID(what string, p Package) string {
	commit := lastOf(p.Landed)
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return what + ":" + p.Case + ":" + commit
}

// noteOnce puts a decision's note in the report, passive, unless it is there already (done or not): a run that comes
// back to a decision it recorded finds its note and writes no second. An error is the caller's to act on: a decision
// whose note failed is not made, or is made again.
func (r *Runner) noteOnce(ctx context.Context, n store.Note) error {
	n.Source, n.Importance, n.At = "momentum", store.ImportanceNormal, r.now()
	_, err := r.Store.PutNoteOnce(ctx, n)
	return err
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

// commandSources are the sources a command writes, each run reading all it reads or recording no read.
var commandSources = []string{store.Sessions, store.Supervise, "capacity"}

// planHarness is the harness a plan's activity is of (harnessFor): the one its command sources must have read.
func planHarness(plan Plan) string { return harnessFor(plan.Signal, plan.Units) }

// gapIn is the newest incomplete run in [since, until] of a command source the plan counts, for the harness it counts,
// and the sources that have one. A run that failed, or read less than it should have, loses what it should have seen,
// and no later run puts it in the store: a window that holds one is never read.
func (r *Runner) gapIn(ctx context.Context, plan Plan, since, until time.Time) (last time.Time, sources []string, err error) {
	for _, source := range plan.Sources {
		if !slices.Contains(commandSources, source) {
			continue
		}
		at, err := r.Store.Incomplete(ctx, source, planHarness(plan), since, until)
		if err != nil {
			return last, nil, err
		}
		if at.IsZero() {
			continue
		}
		sources = append(sources, source)
		if at.After(last) {
			last = at
		}
	}
	return last, sources, nil
}

// afterGap restarts the window after a measuring change when a counted command source failed inside it (from its start
// to its end, or to now while it runs), as it restarts after a gap out of effect: from the end of the failure, once the
// source reads again. done is false when no source failed in the window. Until the source has read past the failure
// the case waits, so the window moves once, not at each run of a failure that goes on.
func (r *Runner) afterGap(ctx context.Context, c store.Case, p *Package, plan Plan, m Measured, until time.Time, dry bool) (Measured, bool, error) {
	if now := r.now(); now.Before(until) {
		until = now
	}
	gap, sources, err := r.gapIn(ctx, plan, c.Deployed, until)
	if err != nil || gap.IsZero() {
		return m, false, err
	}
	for _, source := range sources {
		read, err := r.Store.HarnessReadThrough(ctx, source, planHarness(plan))
		if err != nil {
			return m, false, err
		}
		if !read.After(gap) {
			m.Status, m.Why = "waiting", fmt.Sprintf("a run of %s did not read at %s, inside the window after the change: the window starts again once it reads past it",
				source, gap.Format(time.DateTime))
			return m, true, nil
		}
	}
	from := gap.Add(time.Millisecond)
	m.Status, m.Due = "measuring", from.Add(plan.Window)
	m.Why = fmt.Sprintf("a run of %s did not read at %s, inside the window after the change: the window starts again after it", strings.Join(sources, ", "),
		gap.Format(time.DateTime))
	if dry {
		return m, true, nil
	}
	if _, err := r.Store.RestartMeasuring(ctx, c.ID, from, "a source did not read what its window holds; the window starts again from", "loop:measure"); err != nil {
		return m, false, err
	}
	p.ReadWait, p.ReadUntil = time.Time{}, time.Time{}
	if err := r.Ship.save(p); err != nil {
		return m, false, err
	}
	r.resolve(ctx, p)
	return m, true, nil
}

// liveSources leaves out the readers that stopped: one switched off or no longer configured, and a command's
// source (a sessions scan, supervision, a capacity run) that has not finished a complete read for a whole window, or
// that never finished one in more than a window since its first event
// (a scan that keeps failing, a session supervision keeps reading in part, a timer that no longer
// runs), which would otherwise hold every decision that waits on it for good. A scan and supervision read each harness
// on their own, so the harness a signal counts is the one they must have read: another's failing scan stops
// nothing of it. An ingest source reads per file, and its partial runs say which windows they cover.
func (s *Ship) liveSources(ctx context.Context, sources []string, window time.Duration, now time.Time, harness string) (live, omitted []string) {
	for _, source := range sources {
		stopped := s.SourceLive != nil && !s.SourceLive(source)
		if !stopped && s.Store != nil && window > 0 && slices.Contains(commandSources, source) {
			// its newest complete read, or, when it never finished one, its first event: how long it has run unread
			since, err := s.Store.HarnessReadThrough(ctx, source, harness)
			if err == nil && since.IsZero() {
				since, err = s.Store.FirstHarnessEvent(ctx, source, harness)
			}
			stopped = err == nil && !since.IsZero() && now.Sub(since) > window
		}
		if stopped {
			omitted = append(omitted, source)
		} else {
			live = append(live, source)
		}
	}
	return
}

func tallyPlan(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string) (measure.Counts, error) {
	q, err := activityQuery(plan.Signal, plan.Units, since, until, dir)
	if err != nil {
		return measure.Counts{}, err
	}
	evs, err := Count(ctx, st, plan.Signal, since, until, dir, q.Actor)
	if err != nil {
		return measure.Counts{}, err
	}
	return tallySources(ctx, st, plan.Signal, plan.Units, evs, since, until, dir, plan.Sources)
}

// countingPlan recounts the original baseline when a recorded reader stopped. Its sources stay recorded. A guard's
// history is recounted through its change's first in-effect time when it has one (else through the plan), and the
// package keeps that count.
func (r *Runner) countingPlan(ctx context.Context, p *Package, dry bool) (Plan, string, error) {
	plan := *p.Plan
	if p.Result != nil {
		plan.Before, plan.BeforeBase = p.Result.Before.Hits, p.Result.Before.Base
	}
	sources := plan.Sources
	if len(sources) == 0 { // packages counted before sources were recorded
		var err error
		sources, err = r.Store.Sources(ctx, plan.Signal.Source, store.ActivityQuery{Since: plan.At.Add(-4 * plan.Window), Until: plan.At, ExcludeCwd: r.Dir})
		if err != nil {
			return plan, "", err
		}
	}
	live, omitted := r.Ship.liveSources(ctx, sources, plan.Window, r.now(), planHarness(plan))
	plan.Sources = live
	if !slices.Equal(p.Omitted, omitted) && !dry {
		detail := "sources left out of both counts: " + strings.Join(omitted, ", ")
		if len(omitted) == 0 {
			detail = "all recorded sources are live again"
		}
		if err := r.Store.CaseNote(ctx, p.Case, "measurement sources", detail, "loop:measure"); err != nil {
			return plan, "", err
		}
		p.Omitted = omitted
		if err := r.Ship.save(p); err != nil {
			return plan, "", err
		}
	}
	if !slices.Contains(live, plan.Signal.Source) {
		why := "the signal's source stopped: " + plan.Signal.Source
		if !dry {
			r.problem(ctx, p, why, "There is no decision while the signal's source is stopped.")
		}
		return plan, why, nil
	}
	if !dry && strings.HasPrefix(p.Problem, "the signal's source stopped:") {
		r.resolve(ctx, p)
	}
	if len(omitted) > 0 && plan.Guard != nil {
		// a guard's history is recounted over the live sources as a baseline is, through the change's effect: its
		// exposure, and the exposure its watch runs to
		g := *plan.Guard
		through := plan.At
		if !p.GuardFrom.IsZero() {
			through = p.GuardFrom
		}
		if why, err := r.readWindow(ctx, p, plan, g.Since, through, dry); err != nil || why != "" {
			return plan, why, err
		}
		h, err := guardHistory(ctx, r.Store, plan, g.Since, through, r.Dir, false)
		if err != nil {
			return plan, "", err
		}
		if h.Exposure > 0 {
			setGuard(&g, h)
		}
		plan.Guard = &g
		if !dry && (g.Incidents != p.Plan.Guard.Incidents || g.Exposure != p.Plan.Guard.Exposure) {
			// the recount stands for its report: the guard keeps what it counted
			kept := g
			p.Plan.Guard = &kept
			if err := r.Ship.save(p); err != nil {
				return plan, "", err
			}
		}
	} else if len(omitted) > 0 {
		if why, err := r.readWindow(ctx, p, plan, plan.At.Add(-4*plan.Window), plan.At.Add(-plan.Window), dry); err != nil || why != "" {
			return plan, why, err
		}
		before, err := tallyPlan(ctx, r.Store, plan, plan.At.Add(-4*plan.Window), plan.At.Add(-plan.Window), r.Dir)
		if err != nil {
			return plan, "", err
		}
		plan.Before, plan.BeforeBase = before.Hits, before.Base
	}
	return plan, "", nil
}
