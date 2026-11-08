package loop

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
// which the problem's unit was in use: of the units that hold every event of the history (guardCandidates), the one
// with the least exposure there. It is refused only where no watch could be kept: the case's evidence is not the
// signal's, no unit holds its events, or its history holds no exposure. A refusal names the measured plan's beside
// the guard's. countedAt is the count's own time, which the plan's boundary (plan.At) may be before, and evidence the
// case's evidence (caseEvents) as the count read it. sourcesFor gives the sources and the omitted ones for a count in
// units, or a *waitingPlan.
func (s *Ship) guardPlan(ctx context.Context, c store.Case, plan Plan, instead error, countedAt time.Time, evidence []caseEvent, sourcesFor func(units string) ([]string, []string, error)) (Plan, error) {
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
	counted := map[string]bool{}
	for _, e := range hits {
		counted["ev:"+e.ID()] = true
	}
	// own is the signal's tool when every event of the case's evidence dated before the count (countedAt) is of it, the
	// loop's own left out (caseEvents): its detector filed them, and no plan words them, so
	// naming a tool in the signal takes no tool past the two incidents (guardCandidates). It is read over no span back
	// from the count: the history is four of the build's windows, so evidence of another tool outside it would drop out
	// under a shorter one, and a span would lose, at a later recount (a hand landing's, which runs on a later pass, as
	// of the landing), the evidence the proposal saw. Evidence dated from the count on is not read here; the watch reads
	// the case's evidence from its own start, so a piece dated between the two is weighed by neither (SPEC, Exposure, a
	// known limit; #140). A case with no event of the store gets no guard (events, below). An event is of its tool by
	// caseEvent.tool: the store's event's, or for one the store lost (a rebuild, a stale row dropped) the tool its
	// evidence recorded at its last filing (or a merge), and none when it recorded none.
	events, in, hit, own := 0, 0, 0, sig.Tool
	for _, e := range evidence {
		events++
		if own != "" && e.At.Before(countedAt) && e.tool() != own {
			own = ""
		}
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
	// the unit is found in the incidents the history counts: an incident the source began with is dropped from them
	// (measure.GuardCount), so it chooses no tool; with none after it, the units are found in it alone
	times := make([]time.Time, len(hits))
	for i, e := range hits {
		times[i] = e.TS
	}
	cut := measure.GuardCount(measure.Incidents(times, measure.GuardGap), nil, since, measure.GuardGap, anchored).Since
	later := slices.DeleteFunc(slices.Clone(hits), func(e store.Event) bool { return e.TS.Before(cut) })
	cands, none := guardCandidates(units, own, later)
	if len(later) == 0 {
		cands, none = guardCandidates(units, own, hits)
	}
	if len(cands) == 0 {
		return refuse("%s", none)
	}
	// of the units that hold every event, the one with the least exposure in the history: the narrowest that holds
	// the problem (a broader unit counts hours in which the problem could not have come back, in the history and in
	// the watch alike); on a tie, the narrower by construction (guardCandidate.holds), whose watch counts no hour the
	// other's would not, then the first. The history's events are the same for each: read once, above.
	var best *Guard
	var bestC guardCandidate
	var bestH measure.GuardHistory
	for _, cand := range cands {
		g.Guard = &Guard{Instead: why, Gap: measure.GuardGap, Units: units, Unit: cand.unit, Tools: cand.tools, Server: cand.server, Cwds: cand.cwds, Begun: since, Anchored: anchored}
		h, err := guardHistoryOf(ctx, s.Store, g, hits, since, at, s.Dir, anchored)
		if err != nil {
			return Plan{}, err
		}
		if h.Exposure > 0 && (best == nil || h.Exposure < bestH.Exposure || h.Exposure == bestH.Exposure && bestC.holds(cand)) {
			best, bestC, bestH = g.Guard, cand, h
		}
	}
	if best == nil {
		return refuse("its history holds no exposure to count the problem's rate by")
	}
	g.Guard = best
	setGuard(g.Guard, bestH)
	return g, nil
}

// setGuard records a counted history on a guard: its start, incidents and exposure.
func setGuard(g *Guard, h measure.GuardHistory) {
	g.Since, g.Incidents, g.Exposure = h.Since, h.Incidents, h.Exposure
}

// guardCandidate is a unit a guard's exposure may be narrowed to: what it is, and its tools or MCP server, and its
// directory.
type guardCandidate struct {
	unit        string
	tools, cwds []string
	server      string
}

// holds says whether every hour a candidate's exposure could count, another's counts too, by how the two are defined
// and whatever the store holds: a set holds a tool or a set of its members, a server any tool unit of its own tools
// and any server within it (codex_apps its connectors), a directory any unit within it, and a unit with no tool or no
// directory holds any as far as that part goes.
func (c guardCandidate) holds(o guardCandidate) bool {
	switch {
	case c.server != "":
		if !store.HoldsServer(c.server, o.server) && (len(o.tools) == 0 || slices.ContainsFunc(o.tools, func(t string) bool { return !store.OfServer(t, c.server) })) {
			return false
		}
	case len(c.tools) > 0:
		if len(o.tools) == 0 || slices.ContainsFunc(o.tools, func(t string) bool { return !slices.Contains(c.tools, t) }) {
			return false
		}
	}
	return len(c.cwds) == 0 || slices.Equal(c.cwds, o.cwds)
}

// guardCandidates are the units that hold every one of a history's events, which guardPlan picks the one of least
// exposure from: the one tool every event was of; the set of the tools they were of, when every one was of a tool;
// the MCP server every event's tool was of (store.MCPServer), any of its tools, or codex_apps as a whole when every
// event's tool was of it and no one connector holds them all (a problem across connectors, an expired sign-in say);
// the one directory every event ran in; and each of those tool units within that directory. The owner's hours take
// no tool: his turns are of none. A directory is compared as the store keeps it now (store.CleanCwd), since a row
// stored before the store cleaned working directories keeps its form until its source is read again; it is a unit
// only when it is every event's own, and there is no set of directories, since one directory can be another's parent
// and cover nearly all work.
//
// A tool found by its events, alone or in a set, must be in two or more of the history's incidents, but the signal's
// own (own), which every event is of by the signal's definition, and which guardPlan passes only when every event of
// the case's evidence dated before the count is of it too (caseEvent.tool): the plan's word alone would take any tool
// past. The incidents are the history's independent draws: events of one incident share a cause (measure.Incidents),
// so a tool one incident alone holds may be where that cause happened to surface, and its hours would count activity
// in general as the problem's unit in use. A server takes no such bound: it is defined by the server, not by which of
// its tools failed, and holds no built-in tool. none says why there is no candidate, when
// there is none: then every activity would count as the problem's unit in use.
func guardCandidates(units, own string, hits []store.Event) (cands []guardCandidate, none string) {
	// of is the members events are of, and the incidents each is in, or nil when one event is of none
	times := make([]time.Time, len(hits))
	for i, e := range hits {
		times[i] = e.TS
	}
	incidents := measure.Incidents(times, measure.GuardGap)
	incidentOf := func(at time.Time) int {
		for i, inc := range incidents {
			if !at.Before(inc.First) && !at.After(inc.Last) {
				return i
			}
		}
		return -1
	}
	of := func(member func(store.Event) string) ([]string, map[string]map[int]bool) {
		in := map[string]map[int]bool{}
		for _, e := range hits {
			v := member(e)
			if v == "" {
				return nil, nil // an event outside every member: no unit of them holds them all
			}
			if in[v] == nil {
				in[v] = map[int]bool{}
			}
			in[v][incidentOf(e.TS)] = true
		}
		return slices.Sorted(maps.Keys(in)), in
	}
	var tools []string
	var toolIncidents map[string]map[int]bool
	if units != OwnerHours {
		tools, toolIncidents = of(func(e store.Event) string { return e.Tool })
	}
	cwds, _ := of(func(e store.Event) string { return store.CleanCwd(e.Cwd) })
	var toolUnits []guardCandidate
	thin := 0
	for _, in := range toolIncidents {
		if len(in) < 2 {
			thin++
		}
	}
	switch {
	case len(tools) == 1 && (tools[0] == own || thin == 0):
		toolUnits = append(toolUnits, guardCandidate{unit: "tool", tools: tools})
	case len(tools) > 1 && thin == 0:
		toolUnits = append(toolUnits, guardCandidate{unit: "tools", tools: tools})
	}
	servers := map[string]bool{}    // the MCP servers the tools are of (store.MCPServer), "" for a tool of none
	fields := map[string]bool{}     // the servers they are named of, codex_apps for any of its tools
	connectors := map[string]bool{} // codex_apps's connectors its tools are of, "" for one of none
	for _, tool := range tools {
		server := store.MCPServer(tool)
		servers[server] = true
		if !store.OfServer(tool, store.CodexApps) {
			fields[server] = true
			continue
		}
		fields[store.CodexApps], connectors[server] = true, true
	}
	switch {
	case len(servers) == 1 && !servers[""]:
		for server := range servers {
			toolUnits = append(toolUnits, guardCandidate{unit: "server", server: server})
		}
	case len(fields) == 1 && fields[store.CodexApps]:
		toolUnits = append(toolUnits, guardCandidate{unit: "server", server: store.CodexApps})
	}
	cands = append(cands, toolUnits...)
	if len(cwds) == 1 {
		cands = append(cands, guardCandidate{unit: "directory", cwds: cwds})
		for _, t := range toolUnits {
			cands = append(cands, guardCandidate{unit: t.unit + " in its directory", tools: t.tools, server: t.server, cwds: cwds})
		}
	}
	if len(cands) > 0 {
		return cands, ""
	}
	whys := []string{fmt.Sprintf("%d tools, %d of them in fewer than two of its %d incidents", len(tools), thin, len(incidents))}
	if len(tools) == 1 {
		whys[0] = "1 tool in one incident alone"
	}
	switch {
	case units == OwnerHours:
		whys[0] = "the owner's turns are of no tool"
	case tools == nil:
		whys[0] = "some of no tool"
	}
	whys = append(whys, fmt.Sprintf("in %d directories", len(cwds)))
	if cwds == nil {
		whys[1] = "some in no directory"
	}
	switch {
	case tools == nil:
	case len(fields) == 1 && fields[""]:
		whys = append(whys, "of no MCP server")
	case fields[""]:
		whys = append(whys, "some of no MCP server")
	case fields[store.CodexApps]:
		named := len(connectors)
		if connectors[""] {
			named--
		}
		w := fmt.Sprintf("of %d MCP servers, %s's tools of %s", len(fields), store.CodexApps, connectorsText(named))
		if connectors[""] && named > 0 {
			w += " and some of none"
		}
		whys = append(whys, w)
	default:
		whys = append(whys, fmt.Sprintf("of %d MCP servers", len(fields)))
	}
	return nil, fmt.Sprintf("no unit holds all its events (%s: a tool, alone or in a set, is in two incidents or more, the signal's own too unless every event of the case's evidence dated before the count is of it, as the store holds it or, where the store lost it, as its evidence recorded it; a directory is every event's own; an MCP server is the one every event's tool is of, a connector of codex_apps where one holds them all), so absence cannot be counted only while the problem's unit was in use",
		strings.Join(whys, "; "))
}

// connectorsText counts codex_apps's connectors for a refusal: "no connector" for none.
func connectorsText(n int) string {
	if n == 0 {
		return "no connector"
	}
	return fmt.Sprintf("%d connector%s", n, plural(n))
}

// plural is "s" but for one.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// guardUnitText says what a guard's exposure is narrowed to, for its proposal: the tools and the server by name, the
// directory as its directory (a path is the machine's own), and that it is, of the units that hold every event, the one
// with the least exposure.
func guardUnitText(g Guard) string {
	var on string
	switch {
	case len(g.Tools) == 1:
		on = "in which " + g.Tools[0] + " was used"
	case len(g.Tools) > 1:
		on = fmt.Sprintf("in which any of the %d tools its events were of was used (%s)", len(g.Tools), strings.Join(g.Tools, ", "))
	case g.Server != "":
		on = "in which any tool of the MCP server " + g.Server + " was used"
	}
	switch {
	case len(g.Cwds) > 0 && on != "":
		on += " in its directory"
	case len(g.Cwds) > 0:
		on = "of work in its directory"
	case on == "":
		return ""
	}
	unit := ""
	if g.Unit != "" {
		unit = " (its unit: the " + g.Unit + ", of the units that hold every event the one with the least exposure)"
	}
	return ", counting the hours " + on + unit
}

// guardHistory counts a guard's history over [since, until) by its plan's signal, units, sources, narrowing and gap.
func guardHistory(ctx context.Context, st *store.Store, plan Plan, since, until time.Time, dir string, anchored bool) (measure.GuardHistory, error) {
	hits, err := recurrences(ctx, st, plan, since, until, dir)
	if err != nil {
		return measure.GuardHistory{}, err
	}
	return guardHistoryOf(ctx, st, plan, hits, since, until, dir, anchored)
}

// guardHistoryOf is guardHistory with the events its signal counts over the span (recurrences) read already: they do
// not depend on the guard's narrowing, so a proposal reads them once for every candidate unit.
func guardHistoryOf(ctx context.Context, st *store.Store, plan Plan, hits []store.Event, since, until time.Time, dir string, anchored bool) (measure.GuardHistory, error) {
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
// problem's unit was in use (a call to one of its guard's tools or to any tool of its server, or work in one of its
// directories), with the units of its hits.
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
		q.Tools, q.Server, q.Cwds = plan.Guard.Tools, plan.Guard.Server, plan.Guard.Cwds
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

// guardOutText names, for the close's own log line, the sources its close left out (guardBehind), which no other
// record names (a source switched off or stopped is named by countingPlan's note): the ones left out of its counts,
// and the signal's own, kept in them, when it read only in part since the watch's end.
func guardOutText(plan Plan, out []string) string {
	var says []string
	if left := slices.DeleteFunc(slices.Clone(out), func(source string) bool { return source == plan.Signal.Source }); len(left) > 0 {
		says = append(says, "sources left out of the guard's history and exposure watched at its watch's end, read in part or not at all since it: "+strings.Join(left, ", "))
	}
	if slices.Contains(out, plan.Signal.Source) {
		says = append(says, plan.Signal.Source+", the signal's own source, kept in the counts, read only in part since the watch's end")
	}
	return strings.Join(says, "; ")
}

// guardRecount counts a guard's history again through its watch's start: the plan was counted at the proposal, and
// the change may take effect much later. From the start on, an event is a recurrence, never history. It starts where
// the proposal's count did and drops the incident the source began with as that count did, so an event of that
// incident stored since does not count as another. The caller gives the plan with the sources its close left out
// taken out (guardCounted), and has seen each of the others read past the end, so past the start; the signal's own
// source may have read past it only in part. A recount with no exposure keeps the proposal's count (a rebuild may have
// dropped what the store read of the history), but where sources were dropped and a recount over every recorded
// source (recorded, given only then) holds exposure: then the sources dropped held all of it, the proposal's count
// holds theirs, so the recount's stands, and the close says its history held no exposure in the sources it kept.
func (r *Runner) guardRecount(ctx context.Context, plan Plan, from time.Time, recorded []string) (*Guard, error) {
	g := *plan.Guard
	begun, anchored := g.Since, false
	if !g.Begun.IsZero() {
		begun, anchored = g.Begun, g.Anchored
	}
	h, err := guardHistory(ctx, r.Store, plan, begun, from, r.Dir, anchored)
	if err != nil {
		return nil, err
	}
	if h.Exposure == 0 && recorded != nil {
		all := plan
		all.Sources = recorded
		if whole, err := guardHistory(ctx, r.Store, all, begun, from, r.Dir, anchored); err != nil {
			return nil, err
		} else if whole.Exposure == 0 {
			return &g, nil // none in any recorded source either: what the store read of the history is gone
		}
	}
	if h.Exposure > 0 || recorded != nil {
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
	var all []string // every recorded source, where the close dropped some
	if len(dropped) > 0 {
		all = recorded
	}
	counted, err := r.guardRecount(ctx, plan, from, all)
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
	if res.Unwatched {
		// why it watched nothing, read over every recorded source: the ones switched off or stopped before the close
		// (countingPlan) and the ones it left out, as well as the ones it kept. Its unit used while the change was in
		// effect, in a source left out; else used only while the change was out of effect; else not used.
		all := plan
		all.Sources = recorded
		why := measure.GuardUnused
		if len(dropped) > 0 {
			if seen, err := guardWatched(ctx, r.Store, p, all, nil, from, end, r.Dir); err != nil {
				return failed(err)
			} else if len(seen) > 0 {
				why = measure.GuardLeftOut
			}
		}
		if why == measure.GuardUnused && len(p.GuardAway) > 0 {
			if used, err := guardExposure(ctx, r.Store, all, nil, from, end, r.Dir); err != nil {
				return failed(err)
			} else if len(used) > 0 { // used, but only while the change was out of effect
				why = measure.GuardAway
			}
		}
		switch why {
		case measure.GuardLeftOut:
			res = measure.GuardedLeftOut(g.Incidents, g.Exposure, plan.Units)
		case measure.GuardAway:
			res = measure.GuardedAway(g.Incidents, g.Exposure, plan.Units)
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
	case g.Exposure == 0:
		chance = "The history holds no exposure in the sources kept at the watch's end, so there is no rate to compare."
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
	if _, err := r.Store.CloseGuarded(ctx, c.ID, g.Incidents, g.Exposure, len(watched), res.Recur, res.Effect, guardOutText(plan, out), "loop:measure"); err != nil {
		return failed(err)
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
