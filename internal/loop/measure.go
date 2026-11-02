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
	Status string          `json:"status"` // measuring, waiting, improved, rolled back, held, back, failed
	Result *measure.Result `json:"result,omitempty"`
	Due    time.Time       `json:"due,omitzero"` // when its window after the change ends
	Why    string          `json:"why,omitempty"`
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
	evs, err := Count(ctx, st, sig, since, until, excludeCwd)
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
			if !o.takes(c) || c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring || (c.Status == store.CaseClosed && c.Reason == store.Improved) {
				continue
			}
			cleanup := func() error {
				p, ok, err := r.Ship.Load(id)
				if err != nil {
					return err
				}
				if c.Status == store.CaseTesting && !p.ProposalAt.IsZero() && !p.ReadWait.IsZero() {
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
				out = append(out, Measured{Case: id, Title: c.Title, Status: "failed", Why: err.Error()})
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
		if c.Reason == store.Improved && o.takes(c) && !bad[c.ID] {
			if m, ok := r.watch(ctx, c, o.Dry); ok {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// measureCase is the measure step for one deployed or measuring case.
func (r *Runner) measureCase(ctx context.Context, c store.Case, dry bool) Measured {
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
	case p.Plan.At.IsZero() || p.Plan.Units == "":
		return failed("its plan has no time it was counted at or no units: nothing bounds its window before")
	case p.State == RolledBack:
		// its revert went in and the case did not follow: finish that
		m.Status, m.Why = "rolled back", "its change was rolled back already; the case goes back to testing"
		if !dry {
			if _, err := r.Store.RollBackCase(ctx, c.ID, "its change was rolled back", "loop:measure"); err != nil {
				return failed(err.Error())
			}
			r.resolve(ctx, &p)
		}
		return m
	}
	plan, why, err := r.countingPlan(ctx, &p, dry)
	if err != nil {
		return failed(err.Error())
	}
	if why != "" {
		m.Status, m.Why = "waiting", why
		return m
	}
	now := r.now()
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
	if c.Status == store.CaseDeployed || !p.OutOfEffect.IsZero() {
		m.Status, m.Due = "measuring", since.Add(plan.Window)
		if !dry {
			if c.Status == store.CaseDeployed {
				_, err = r.Store.StartMeasuring(ctx, c.ID, plan.Window, plan.Before, since, "loop:measure")
			} else {
				_, err = r.Store.RestartMeasuring(ctx, c.ID, since, "loop:measure")
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
	m.Due = c.Deployed.Add(plan.Window)
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
		if _, err := r.Store.CloseMeasured(ctx, c.ID, after.Hits, res.Effect, "loop:measure"); err != nil {
			return failed(err.Error())
		}
		if p, err = r.Ship.Keep(c.ID, res); err != nil {
			return failed(err.Error())
		}
		r.resolve(ctx, &p)
		r.note(ctx, store.Note{Kind: store.NoteShipped, Title: plainClip("momentum improved: "+c.Title, store.TitleMax),
			Detail: fmt.Sprintf("Its change (%s) measurably helped over the %s after it was in effect.", firstOf(p.PR, "a package"), days(m.Due.Sub(c.Deployed))),
			Effect: res.Effect, Links: prLinks(p.PR)})
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
	m.Status, m.Why = "rolled back", why
	if dry {
		return m
	}
	if !r.Ship.Live {
		m.Status, m.Why = "held", why+"; the loop is not live, so it rolls nothing back"
		if !p.Held {
			p.Held = true
			r.Ship.save(&p)
			r.note(ctx, store.Note{Kind: store.NoteDecided, Title: plainClip("momentum holds the rollback of its change for: "+c.Title, store.TitleMax),
				Detail: why + ". The loop is not live, so it rolls nothing back. To roll it back: momentum ship rollback " + c.ID + " --why ...",
				Links:  prLinks(p.PR)})
		}
		return m
	}
	if _, err := r.Ship.RollBack(ctx, c.ID, why); err != nil {
		m.Status, m.Why = "failed", "the rollback: "+err.Error()
		r.Store.CaseNote(ctx, c.ID, "not rolled back", err.Error(), "loop:measure")
		r.rollbackFailed(ctx, c, err)
		return m
	}
	if p, _, err = r.Ship.Load(c.ID); err == nil {
		r.resolve(ctx, &p)
	}
	r.note(ctx, store.Note{Kind: store.NoteDecided, Title: plainClip("momentum rolled back its change for: "+c.Title, store.TitleMax),
		Detail: why + ". To keep the change after all: revert the revert on the default branch, then momentum case close " + c.ID +
			" --reason already-solved --why ..., or the loop builds another change for the problem.", Links: prLinks(p.PR)})
	return m
}

// rollbackFailed counts a rollback that did not go through; the second in a row puts a problem note in the report,
// once: a revert that conflicts with a later commit fails the same way on every run.
func (r *Runner) rollbackFailed(ctx context.Context, c store.Case, cause error) {
	p, ok, err := r.Ship.Load(c.ID)
	if err != nil || !ok {
		return
	}
	p.RollbackFailures++
	r.Ship.save(&p)
	if p.RollbackFailures >= 2 {
		r.problem(ctx, &p, "momentum cannot roll back its change for: "+c.Title,
			fmt.Sprintf("Its measurement showed no effect, and the rollback failed %d times in a row: %v. The next run tries again.", p.RollbackFailures, cause))
	}
}

// watch finishes an interrupted keep, then reads each ended window since the close once. The windows do not
// overlap. Back needs three consecutive windows at the old share, so repeated runs cannot add windows.
func (r *Runner) watch(ctx context.Context, c store.Case, dry bool) (Measured, bool) {
	p, ok, err := r.Ship.Load(c.ID)
	m := Measured{Case: c.ID, Title: c.Title, PR: p.PR}
	failed := func(err error) (Measured, bool) { m.Status, m.Why = "failed", err.Error(); return m, true }
	if err != nil {
		return failed(err)
	}
	if !ok || (p.State != Kept && p.State != Landed) || p.Plan == nil {
		return Measured{}, false
	}
	if p.Plan.Window <= 0 {
		return failed(fmt.Errorf("its plan has no window"))
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
			p, err = r.Ship.Keep(c.ID, *p.Result)
			if err != nil {
				return failed(err)
			}
		}
	}
	if p.Result == nil {
		return failed(fmt.Errorf("its kept package has no measurement"))
	}
	now := r.now()
	ended := int(now.Sub(c.Closed) / p.Plan.Window)
	for j := p.WatchWindow + 1; j <= ended; j++ {
		start := c.Closed.Add(time.Duration(j-1) * p.Plan.Window)
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
			m.Why = fmt.Sprintf("%d of %d active hours in window %d, the third consecutive window at its old share since its close (%s), %d of %d before its change",
				recent.Hits, recent.Base, j, days(p.Plan.Window), plan.Before, plan.BeforeBase)
			if dry {
				return m, true
			}
			if _, err := r.Store.RegressCase(ctx, c.ID, m.Why, "loop:measure"); err != nil {
				return failed(err)
			}
			p.WatchWindow = j
			if err := r.Ship.save(&p); err != nil {
				return failed(err)
			}
			r.note(ctx, store.Note{Kind: store.NoteDecided, Title: plainClip("momentum: a problem it fixed is back: "+c.Title, store.TitleMax),
				Detail: "Back at its old share for three consecutive windows (" + m.Why + "): the case is open again. Its lesson in tacit is not withdrawn.", Links: prLinks(p.PR)})
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

// liveSources leaves out only readers known to have stopped.
func (s *Ship) liveSources(sources []string) (live, omitted []string) {
	for _, source := range sources {
		if s.SourceLive != nil && !s.SourceLive(source) {
			omitted = append(omitted, source)
		} else {
			live = append(live, source)
		}
	}
	return
}

func tallyPlan(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string) (measure.Counts, error) {
	evs, err := Count(ctx, st, plan.Signal, since, until, dir)
	if err != nil {
		return measure.Counts{}, err
	}
	return tallySources(ctx, st, plan.Signal, plan.Units, evs, since, until, dir, plan.Sources)
}

// countingPlan recounts the original baseline when a recorded reader stopped. Its sources stay recorded.
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
	live, omitted := r.Ship.liveSources(sources)
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
	if len(omitted) > 0 {
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
