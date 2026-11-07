package loop

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// archive keeps the host receipt before a new package replaces it.
func (s *Ship) archive(p Package) error {
	dir := s.pkgDir(p.Case)
	past := filepath.Join(dir, "past", p.Created.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(past, 0700); err != nil {
		return err
	}
	// package.json is copied, not moved: the new package replaces it, so a preparation whose save fails keeps the old
	// one and what it counts
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := os.WriteFile(filepath.Join(past, "package.json"), raw, 0o600); err != nil {
			return err
		}
	}
	for _, name := range []string{"check.json", "test-command.json", "change.patch"} {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(past, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// installedReadable confines a build report's host path to the admitted read directories.
func (s *Ship) installedReadable(installed string) error {
	if installed == "" {
		return nil
	}
	file := expandHome(installed)
	if !filepath.IsAbs(file) {
		return refused("installed names an absolute file in loop.read_dirs")
	}
	if under(file, s.Dir) || s.Sandbox.Cache != "" && under(file, s.Sandbox.Cache) {
		return refused("installed never names run storage or a writable build cache")
	}
	for _, dir := range s.InstalledRoots {
		root, err := filepath.EvalSymlinks(expandHome(dir))
		if err == nil && under(file, root) {
			real, err := filepath.EvalSymlinks(file)
			if err != nil || real != file {
				return refused("installed names a regular file without links in loop.read_dirs")
			}
			info, err := os.Lstat(real)
			if err == nil && info.Mode().IsRegular() {
				return nil
			}
		}
	}
	return refused("installed names a regular file in loop.read_dirs")
}

// deploymentReceipt leaves a passive note before the case leaves testing.
func (s *Ship) deploymentReceipt(ctx context.Context, p Package) error {
	c, err := s.Store.CaseByID(ctx, p.Case)
	if err != nil {
		return err
	}
	window := ""
	if p.Plan != nil {
		window = " Measured over the " + days(p.Plan.Window) + " after it is in effect."
		if g := p.Plan.Guard; g != nil && p.Plan.Units != "" {
			window = fmt.Sprintf(" Guarded, not measured: it watches %s for a recurrence from when it first takes effect; %d incidents in %d %s before it. One recurrence brings the case back; no recurrence closes it as guarded, with P, had the problem kept happening. A problem that stopped by itself shows the same.",
				days(guardRetention), g.Incidents, g.Exposure, p.Plan.Units)
		}
	}
	if len(p.Hand) > 0 {
		return s.runner().noteOnce(ctx, store.Note{ID: noteID("landed", p), Kind: store.NoteDecided,
			Title:  plainClip("A session landed momentum's proposal for: "+c.Title, store.TitleMax),
			Detail: "Landed by hand from its proposal." + window + " The loop rolls nothing back that it did not land: to undo it, revert it through a pull request.",
			Links:  prLinks(p.PR)})
	}
	return s.runner().noteOnce(ctx, store.Note{ID: noteID("landed", p), Kind: store.NoteDecided,
		Title:  plainClip("momentum landed a change for: "+c.Title, store.TitleMax),
		Detail: "Reviewed clean and landed." + window + " To undo it: momentum ship rollback " + c.ID + " --why ...",
		Links:  prLinks(p.PR)})
}

// waitingPlan is a proposal that cannot be counted yet, whatever the change: the store is behind on a source the plan
// counts, or has an incomplete run of one inside the span, or the signal's own source stopped. The proposal is
// recorded as waiting, and a build for it is passed over until the wait is over.
type waitingPlan struct {
	sources []string // what it waits on
	stopped bool     // the signal's own source stopped: there is no read to wait for, but a reader to run again
}

func (e *waitingPlan) Error() string {
	if e.stopped {
		return "the signal's source stopped: " + strings.Join(e.sources, ", ")
	}
	return "the store has not read past the window before: " + strings.Join(e.sources, ", ")
}
func (e *waitingPlan) Unwrap() error { return ErrRefused }

// what is what the proposal waits on, for the pick's account of the cases it passes over.
func (e *waitingPlan) what() string {
	if e.stopped {
		return "its signal's source, which stopped: " + strings.Join(e.sources, ", ")
	}
	return "a read of " + strings.Join(e.sources, ", ")
}

// title is the problem note a proposal raises once it has waited a window.
func (e *waitingPlan) title() string {
	if e.stopped {
		return e.Error()
	}
	return "the store is behind on " + strings.Join(e.sources, ", ")
}

// runner is a runner of this ship tool's own, for what the measure step does that a proposal does too (the report's
// notes).
func (s *Ship) runner() *Runner { return &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Now: s.Now} }

// waitProposal records a proposal that waits: its plan and the boundary it was counted at, and the first time it
// waited. A window of waiting puts a passive problem note in the report, once.
func (s *Ship) waitProposal(ctx context.Context, p *Package, plan Plan, wait *waitingPlan) error {
	r := s.runner()
	p.Plan, p.ProposalAt, p.ReadUntil = &plan, plan.At, plan.At
	if p.ReadWait.IsZero() {
		p.ReadWait = r.now()
	}
	if err := s.save(p); err != nil {
		return err
	}
	if !r.now().Before(p.ReadWait.Add(plan.Window)) {
		r.problem(ctx, p, wait.title(), wait.Error())
	}
	return nil
}

// waiting says whether a proposal of a plan would wait now: the plan counted again at at, with its signal, window and
// installed file. A proposal refused for anything else is not waiting: a build would change it.
func (s *Ship) waiting(ctx context.Context, c store.Case, plan Plan, at time.Time) *waitingPlan {
	_, err := s.validPlan(ctx, c, plan.Signal, plan.Window, plan.Installed, at)
	var wait *waitingPlan
	if errors.As(err, &wait) {
		return wait
	}
	return nil
}

// harnessFor is the harness a plan's activity is of: the signal's, in the hours of sessions; none, in the machine's.
func harnessFor(sig Signal, units string) string {
	if units == MachineHours {
		return ""
	}
	return sig.Harness
}

// validPlan checks a measurement plan and returns it, counted at at: a signal that names a source, kinds and at
// least one text, a window of whole weeks, at least MinSignal units of activity in which it counts an event
// over the window before, and a signal that counts the case's own problem: the case holds events, and at least half
// of those in that window are among what it counts. A file the change is installed in must be one there is.
// A plan that cannot be counted yet comes back with a *waitingPlan: the store has not read the span whole, or the
// signal's source stopped. A plan refused as too new (its signal's source began inside the span) or too rare (fewer
// than MinSignal units in the gate, or a baseline no drop could pass) comes back as a guard when one can stand in for
// it (guardPlan), and refused with both reasons when none can. A signal that counts less than half of the case's
// evidence in the gate is refused before any guard is tried, and so is a gate that holds the problem and none of the
// case's evidence.
func (s *Ship) validPlan(ctx context.Context, c store.Case, sig Signal, window time.Duration, installed string, at time.Time) (Plan, error) {
	switch {
	case sig.Source == "" || len(sig.Kinds) == 0 || len(sig.Match) == 0:
		return Plan{}, refused("a measurement signal names a source, kinds and at least one text its events hold")
	case !measure.WholeWeeks(window):
		return Plan{}, refused("a measurement window is a whole number of weeks (7d, 14d and so on)")
	}
	for _, m := range sig.Match {
		if len(strings.TrimSpace(m)) < 4 {
			return Plan{}, refused("the signal's text %q is too short to name a problem", m)
		}
	}
	if installed != "" {
		path := expandHome(installed)
		if !filepath.IsAbs(path) {
			return Plan{}, refused("the file the change is installed in is an absolute path or one under ~/, not %q", installed)
		}
		if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() { // a link is none: the step opens the file without following one
			return Plan{}, refused("the file the change is installed in, %s, is no file here (a link is none: name the file it points to)", installed)
		}
	}
	now := at // the proposal's own time: the readers that stopped are those that have not run for a window up to it, and the span is the four windows before it
	first, err := Count(ctx, s.Store, sig, at.Add(-window), at, s.Dir, "")
	if err != nil {
		return Plan{}, err
	}
	units := unitsOf(first)
	var sources, omitted []string
	// The sources recorded are those whose events the plan's units can count in the span: a sessions' measurement counts
	// no event without a session (a capacity run, an incident), so it waits on no reader of those. A reader that stopped
	// is left out of both sides, and so is one that began inside the span: it adds activity the earlier windows held in
	// part or not at all. The signal's own source is neither left out nor waited on when it stopped (the proposal
	// waits), and a plan whose signal began inside its span is refused, as too new to measure. The span is the four
	// windows before the proposal: a failure inside it moves the boundary back, and the span with it, but a source old
	// enough at the proposal is not too new for the plan the failure moved (it would be refused at each run, which is
	// no wait).
	span := 4 * window
	live := func(units string, until time.Time) ([]string, error) {
		q, err := activityQuery(sig, units, until.Add(-span), until, s.Dir)
		if err != nil {
			return nil, err
		}
		found, err := s.Store.Sources(ctx, sig.Source, q)
		if err != nil {
			return nil, err
		}
		kept, stopped := s.liveSources(ctx, found, window, now, harnessFor(sig, units))
		omitted = append(omitted, stopped...)
		if !slices.Contains(kept, sig.Source) {
			return kept, &waitingPlan{sources: []string{sig.Source}, stopped: true}
		}
		return kept, nil
	}
	// tooNew is the measured plan's refusal when the signal's source began inside the span: no measured plan can stand,
	// but a guard, which reads the history from the source's first event, may (guardPlan). The source is kept for it.
	var tooNew error
	begun := func(units string, list []string) ([]string, error) {
		var kept []string
		for _, source := range list {
			began, err := s.Store.FirstHarnessEvent(ctx, source, harnessFor(sig, units))
			if err != nil {
				return nil, err
			}
			switch {
			case !began.After(now.Add(-span)):
				kept = append(kept, source)
			case source == sig.Source:
				tooNew = refused("the signal's source began inside the %s before: the problem is too new to measure with that window", days(span))
				kept = append(kept, source)
			default:
				omitted = append(omitted, source)
			}
		}
		return kept, nil
	}
	waits := func(err error) (Plan, error) {
		return Plan{Signal: sig, Window: window, Units: units, At: at, Installed: installed, Sources: sources, Omitted: omitted}, err
	}
	if sources, err = live(units, at); err != nil {
		return waits(err)
	}
	if sources, err = begun(units, sources); err != nil {
		return Plan{}, err
	}
	boundary := at
	// Moving the boundary can expose another source at the start of the baseline. Include it before counting.
	for {
		previous, behind := boundary, map[string]time.Time{}
		for _, source := range sources {
			read, err := s.Store.HarnessReadWindowThrough(ctx, source, harnessFor(sig, units), boundary.Add(-span))
			if err != nil {
				return Plan{}, err
			}
			if !read.After(boundary) {
				boundary = read
				if !read.IsZero() {
					boundary = read.Add(-time.Millisecond)
				}
				behind[source] = boundary
			}
		}
		if boundary.IsZero() || at.Sub(boundary) > window {
			// the readers that put it further back than a window are the ones it waits on
			var late []string
			for _, source := range sources {
				if b, ok := behind[source]; ok && (b.IsZero() || at.Sub(b) > window) {
					late = append(late, source)
				}
			}
			return waits(&waitingPlan{sources: late})
		}
		found, err := live(units, boundary)
		if err != nil {
			return waits(err)
		}
		if found, err = begun(units, found); err != nil {
			return Plan{}, err
		}
		added := false
		for _, source := range found {
			if !slices.Contains(sources, source) {
				sources = append(sources, source)
				added = true
			}
		}
		if !added && boundary.Equal(previous) {
			break
		}
	}
	at = boundary
	got, err := Count(ctx, s.Store, sig, at.Add(-window), at, s.Dir, "")
	if err != nil {
		return Plan{}, err
	}
	if unitsOf(got) != units { // the boundary moved what the signal counts in: its units' sources then
		units = unitsOf(got)
		if sources, err = live(units, at); err != nil {
			return waits(err)
		}
		if sources, err = begun(units, sources); err != nil {
			return Plan{}, err
		}
	}
	slices.Sort(sources)
	slices.Sort(omitted)
	omitted = slices.Compact(omitted)
	plan := Plan{Signal: sig, Window: window, Units: units, At: at, Installed: installed, Sources: sources, Omitted: omitted}
	// guard stands in for a measured plan refused as too new, too rare or with no recent event, at the same boundary:
	// the sources it counts are those this plan's would be, for the units its history counts in.
	guard := func(why error) (Plan, error) {
		return s.guardPlan(ctx, c, plan, why, func(units string) ([]string, []string, error) {
			found, err := live(units, at)
			if err != nil {
				return found, nil, err
			}
			if found, err = begun(units, found); err != nil {
				return nil, nil, err
			}
			slices.Sort(found)
			left := slices.Clone(omitted)
			slices.Sort(left)
			return found, slices.Compact(left), nil
		})
	}
	// The case's evidence in the gate, and how much of it the signal counts. Where the case has evidence there, a signal
	// that counts less than half of it does not measure the case's problem as it is now (its form may have changed):
	// that refuses a measured plan and a guard alike, so it is checked before any guard stands in.
	evs, err := caseEvents(ctx, s.Store, c.ID, s.Dir)
	if err != nil {
		return Plan{}, err
	}
	counted := map[string]bool{}
	for _, e := range got {
		counted["ev:"+e.ID()] = true
	}
	events, in, hit := 0, 0, 0
	for _, e := range evs {
		if !strings.HasPrefix(e.Ref, "ev:") {
			continue
		}
		events++
		if e.At.Before(at.Add(-window)) || !e.At.Before(at) {
			continue
		}
		in++
		if counted[e.Ref] {
			hit++
		}
	}
	notIts := func(also error) error {
		why := fmt.Sprintf("the signal counts %d of the case's %d events in the window: it does not measure the case's problem", hit, in)
		if also == nil {
			return refused("%s", why)
		}
		return refused("%s; and %s", strings.TrimPrefix(also.Error(), ErrRefused.Error()+": "), why)
	}
	mismatch := in > 0 && hit*2 < in
	before, err := tallySources(ctx, s.Store, sig, plan.Units, got, at.Add(-window), at, s.Dir, sources)
	if err != nil {
		return Plan{}, err
	}
	if unread, err := unreadWindow(ctx, s.Store, plan, at.Add(-(measure.BaselineWindows+1)*window), at, s.Dir); err != nil {
		return Plan{}, err
	} else if len(unread) > 0 {
		return plan, &waitingPlan{sources: unread}
	}
	if tooNew != nil {
		// the gate is tallied on the too-new path too: an active problem with none of the case's evidence there gets no
		// guard, and a signal that does not count the case's evidence there gets none either
		switch {
		case mismatch:
			return Plan{}, notIts(tooNew)
		case before.Hits >= MinSignal && in == 0:
			return Plan{}, refused("%s; and the case has no evidence in the window, which holds its problem in %d %s: measure over a window that holds it",
				strings.TrimPrefix(tooNew.Error(), ErrRefused.Error()+": "), before.Hits, plan.Units)
		}
		return guard(tooNew)
	}
	plan.Gate, plan.GateBase = before.Hits, before.Base
	if before.Hits < MinSignal {
		few := refused("the signal counts events in %d %s of the %s before, fewer than %d: no window could show an effect", before.Hits,
			plan.Units, days(window), MinSignal)
		if mismatch {
			return Plan{}, notIts(few)
		}
		return guard(few)
	}
	switch {
	case events == 0:
		return Plan{}, refused("the case holds no event of the store: its measurement has nothing of its own problem to count")
	case in == 0:
		// the gate holds the problem in five units or more, and none of the case's evidence: the problem is active, so no
		// guard stands in; a window that holds the evidence measures it
		return Plan{}, refused("the case has no evidence in the window: measure over a window that holds it")
	case mismatch:
		return Plan{}, notIts(nil)
	}
	baseline, err := tallyPlan(ctx, s.Store, plan, at.Add(-(measure.BaselineWindows+1)*window), at.Add(-window), s.Dir)
	if err != nil {
		return Plan{}, err
	}
	if !measure.Measurable(baseline, 2*plan.GateBase) {
		return guard(refused("the problem is too new or too rare to measure with that window: use a week, or two for a rarer problem, once enough history is read"))
	}
	plan.Before, plan.BeforeBase = baseline.Hits, baseline.Base
	return plan, nil
}

// caseEvents is a case's evidence of events (ev:ID), the loop's own left out by the one rule every count of a plan
// keeps (store.InDir): the counts never count an event of the loop's own runs, so the evidence their admission weighs
// them against holds none either. An event the store no longer holds stays, by its evidence's time.
func caseEvents(ctx context.Context, st *store.Store, caseID, dir string) ([]store.Evidence, error) {
	evidence, err := st.CaseEvidence(ctx, caseID, 0)
	if err != nil {
		return nil, err
	}
	var out []store.Evidence
	for _, ev := range evidence {
		id, ok := strings.CutPrefix(ev.Ref, "ev:")
		if !ok {
			continue
		}
		e, found, err := st.EventByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if found && store.InDir(e.Cwd, dir) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// expandHome is a path with a leading ~/ made the user's home.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return filepath.Clean(p)
}

// Count is the events a signal counts over a span: those of its source, kinds and harness whose text holds every one of its
// texts, and, of the tool results, the failed ones (a successful result that quotes the error is no hit). A span that holds
// more than countLimit events of its source, kinds and harness is an error, not a count of what fits.
var countLimit = 1_000_000

func Count(ctx context.Context, st *store.Store, sig Signal, since, until time.Time, excludeCwd string, actor store.Actor) ([]store.Event, error) {
	return countWritten(ctx, st, sig, since, until, excludeCwd, actor, 0, 0)
}

// countWritten is Count of the events whose last write came after the store's write sequence after and at or before
// through (store.Seq; either 0 for no bound).
func countWritten(ctx context.Context, st *store.Store, sig Signal, since, until time.Time, excludeCwd string, actor store.Actor, after, through int64) ([]store.Event, error) {
	evs, err := st.Events(ctx, store.Query{Since: since, Until: until, ExcludeCwd: excludeCwd, Source: sig.Source, Kinds: sig.Kinds, Harness: sig.Harness,
		Actor: actor, FailedResults: true, Limit: countLimit + 1, SeqAfter: after, SeqThrough: through})
	if err != nil {
		return nil, err
	}
	if len(evs) > countLimit {
		return nil, fmt.Errorf("the signal's events over %s to %s are more than %d: a count of them would lose the oldest", since.Format(time.DateTime),
			until.Format(time.DateTime), countLimit)
	}
	var out []store.Event
	for _, e := range evs {
		if sig.Tool != "" && e.Tool != sig.Tool {
			continue
		}
		low := strings.ToLower(e.Summary)
		all := true
		for _, m := range sig.Match {
			all = all && strings.Contains(low, strings.ToLower(m))
		}
		if all {
			out = append(out, e)
		}
	}
	return out, nil
}

// InEffect says whether a landed change is in effect, and since when the measure step may count: the repository's
// checkout as it stands holds the change, when its plan names no installed file; else the installed file, which is
// decided once for each state it is in (its hash and time) and keeps the decision while it stands. A Go binary
// carries the revision it was built from. Another file is in effect once it changed after the checkout first held the
// change, from a checkout that held it then. The caller saves the build and first checkout records except on a dry run.
func (s *Ship) InEffect(ctx context.Context, p *Package) (time.Time, string, error) {
	switch {
	case len(p.Landed) == 0 || p.LandedAt.IsZero():
		return time.Time{}, "", fmt.Errorf("case %s's package holds no landed commit", p.Case)
	case p.Plan == nil:
		return time.Time{}, "", fmt.Errorf("case %s's package has no plan", p.Case)
	}
	var installedAt time.Time
	var installedHash string
	changed := false
	if p.Plan.Installed != "" {
		path := expandHome(p.Plan.Installed)
		f, info, why, err := openInstalled(path)
		if err != nil {
			return time.Time{}, "", err
		}
		if f == nil {
			return time.Time{}, filepath.Base(p.Plan.Installed) + ", where it is installed, " + why, nil
		}
		defer f.Close()
		hash := sha256.New()
		if _, err := io.Copy(hash, f); err != nil {
			return time.Time{}, "", err
		}
		installedAt, installedHash = info.ModTime(), fmt.Sprintf("%x", hash.Sum(nil))
		changed = !installedAt.Equal(p.InstalledAt) || installedHash != p.InstalledHash
		if !changed && p.InstalledHash != "" && (p.InstalledGo || !p.CheckoutAt.IsZero()) {
			if p.InstalledOK {
				return s.now(), "", nil
			}
			return time.Time{}, firstOf(p.InstalledWhy, "the installed file has not changed since a build from a checkout without the change"), nil
		}
		if changed {
			if ok, why, decided, isGo, err := s.goBuildHolds(ctx, p, f, installedAt); err != nil {
				return time.Time{}, "", err
			} else if isGo {
				if decided {
					p.InstalledAt, p.InstalledHash, p.InstalledOK, p.InstalledGo, p.InstalledWhy = installedAt, installedHash, ok, true, why
				}
				if ok {
					return s.now(), "", nil
				}
				return time.Time{}, why, nil
			}
		}
	}
	if p.Plan.Installed == "" || p.CheckoutAt.IsZero() || changed {
		last := p.Landed[len(p.Landed)-1]
		// a changed installed file is checked against the checkout as it stood when the file was written (the commits
		// its HEAD's reflog names then), not as it stands now: a build from an older tree does not count once the
		// checkout is back on the change. The reflog's commits are all that is read: a build from uncommitted edits, or
		// from another work tree or clone, is not checked.
		heads, reflog := []string{"HEAD"}, false
		if changed {
			if at, ok := s.headsAt(ctx, p.Dir, installedAt); ok {
				heads, reflog = at, true
			}
		}
		held, lacking := 0, 0
		for _, head := range heads {
			_, err := s.git(ctx, p.Dir, "merge-base", "--is-ancestor", last, head)
			var exit *exec.ExitError
			switch {
			case err == nil:
				held++
			case errors.As(err, &exit) && exit.ExitCode() == 1:
				lacking++
			default:
				return time.Time{}, fmt.Sprintf("cannot check the repository's checkout: %v", err), nil
			}
		}
		why := ""
		switch {
		case held == 0 && reflog:
			why = fmt.Sprintf("the checkout the installed file was built from did not hold %.12s", last)
		case held == 0:
			why = fmt.Sprintf("the repository's checkout does not hold %.12s yet", last)
		case lacking > 0:
			// the checkout moved between a commit with the change and one without it when the file was written, and the
			// reflog cannot say which came first
			why = movedInItsSecond
		}
		if why != "" {
			// A definite rejection stands, with its reason, until the next build. A transient checkout error records no
			// decision, and an unchanged file keeps the one it has.
			if changed {
				p.InstalledAt, p.InstalledHash, p.InstalledOK, p.InstalledGo, p.InstalledWhy = installedAt, installedHash, false, false, why
			}
			return time.Time{}, why, nil
		}
		checkoutAt := p.LandedAt
		newest, err := newestChange(p)
		if err != nil {
			return time.Time{}, "", err
		}
		if !newest.IsZero() {
			checkoutAt = newest
		}
		if p.Plan.Installed == "" || p.CheckoutAt.IsZero() {
			p.CheckoutAt = checkoutAt
		}
	}
	if p.Plan.Installed != "" {
		if !changed {
			return time.Time{}, firstOf(p.InstalledWhy, "the installed file has not changed since a build from a checkout without the change"), nil
		}
		p.InstalledAt, p.InstalledHash, p.InstalledOK, p.InstalledGo, p.InstalledWhy = installedAt, installedHash, false, false, ""
		if !installedAt.After(p.CheckoutAt) {
			p.InstalledWhy = fmt.Sprintf("%s, where it is installed, has not changed since the checkout held the change", filepath.Base(p.Plan.Installed))
			return time.Time{}, p.InstalledWhy, nil
		}
		p.InstalledOK = true
	}

	return s.now(), "", nil
}

// commitID is a commit's name in a Go binary's build information: a SHA-1 or SHA-256 hash.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// readBuildInfo reads the build information of a Go binary from the descriptor the step hashed (a test puts its own).
var readBuildInfo = buildinfo.Read

// openInstalled opens the file a change is installed in, once, and checks the descriptor, not the path: the one the step
// hashes and reads the build information from. The path was accepted at the proposal, and may have been swapped since. A
// last link is refused (O_NOFOLLOW), so a link to a file the plan never named is not hashed, and a pipe or a device does
// not hold the open (O_NONBLOCK) before the descriptor says what it is. why says what a file that is no regular file is,
// or that it is not there; the file is nil then. A link is no installed file: the file it points to is the one to name.
func openInstalled(path string) (f *os.File, info os.FileInfo, why string, err error) {
	f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, syscall.ELOOP):
		return nil, nil, "is a link: name the file it points to", nil
	case err != nil:
		return nil, nil, "is not there", nil
	}
	if info, err = f.Stat(); err != nil {
		f.Close()
		return nil, nil, "", err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, "is no regular file", nil
	}
	return f, info, "", nil
}

// goBuildHolds decides, for the state an installed file is in now, whether a Go binary built from a repository holds
// the landed change. isGo is false for a file that is no Go binary or carries no revision: it is another installed
// file. decided is false when the answer waits on something that may change (git could not tell), so the file is
// asked again at the next run; a decided answer is kept, with its reason, while the file stands.
//
// A binary built from a tree with uncommitted edits (Go calls an untracked file one too) holds the change when its
// revision does, the change's own files as the working tree has them match the revision, and none of them was modified
// after the binary was written. That is decided here, once: the working tree moves on without the binary (a
// commit, an edit undone), and a tree whose files were touched since cannot say what the binary was built from.
func (s *Ship) goBuildHolds(ctx context.Context, p *Package, bin io.ReaderAt, builtAt time.Time) (ok bool, why string, decided, isGo bool, err error) {
	info, rerr := readBuildInfo(bin)
	if rerr != nil {
		return false, "", false, false, nil
	}
	revision, modified := "", ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" || modified == "" {
		return false, "", false, false, nil
	}
	if !commitID.MatchString(revision) {
		return false, "the installed Go binary's revision is no commit", true, true, nil
	}
	if _, gerr := s.git(ctx, p.Dir, "merge-base", "--is-ancestor", p.Landed[len(p.Landed)-1], revision); gerr != nil {
		var exit *exec.ExitError
		if errors.As(gerr, &exit) && exit.ExitCode() == 1 {
			return false, "the installed Go binary's revision does not hold the change", true, true, nil
		}
		return false, "cannot check the installed Go binary's revision: " + gerr.Error(), false, true, nil
	}
	if modified == "false" {
		return true, "", true, true, nil
	}
	if len(p.Changed) == 0 {
		return false, "the installed Go binary was built from a tree whose change's own files are modified", true, true, nil
	}
	if newest, serr := newestChange(p); serr != nil {
		return false, "", false, true, serr
	} else if newest.After(builtAt) {
		return false, "a file of the change was modified after the installed Go binary was built from a modified tree", true, true, nil
	}
	if match, gerr := s.changeFilesMatch(ctx, p, revision); gerr != nil {
		return false, "cannot check the installed Go binary's tree: " + gerr.Error(), false, true, nil
	} else if !match {
		return false, "the installed Go binary was built from a tree whose change's own files are modified", true, true, nil
	}
	return true, "", true, true, nil
}

// newestChange is the newest modification time among the change's own files in its checkout, and zero when none of
// them is there. A link is dated by its own time, which a checkout sets when it writes the link, not by its target's.
// A file under a path that is no directory now (the change turned a directory into a file) is not there.
func newestChange(p *Package) (time.Time, error) {
	var newest time.Time
	for _, name := range p.Changed {
		st, err := os.Lstat(filepath.Join(p.Dir, name))
		if absent(err) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	return newest, nil
}

// absent says whether a stat's error says the file is not there.
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// changeFilesMatch says whether the change's own files, as the working tree has them, match a revision's. A change
// that names no file matches nothing.
func (s *Ship) changeFilesMatch(ctx context.Context, p *Package, revision string) (bool, error) {
	if len(p.Changed) == 0 {
		return false, nil
	}
	// the names are the change's own, chosen by its author: literal, never a pathspec (":(exclude)x" is a file name)
	args := append([]string{"--literal-pathspecs", "diff", "--quiet", revision, "--"}, p.Changed...)
	if _, err := s.git(ctx, p.Dir, args...); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// movedInItsSecond is the reason an installed file is out of effect when HEAD's reflog moved between a commit with the
// change and one without it in the file's second or the next (headsAt): the reflog cannot order the move and the file.
// A false in effect cannot come of it, and a false out of effect costs one install, in a later second.
const movedInItsSecond = "the checkout moved in the second the file was installed: install again to measure"

// headsAt is the commits a checkout's HEAD may have named at a moment, by its reflog, and false when the reflog does
// not reach back to it. The reflog dates a move to the whole second, and git dates it after it writes the tree, while
// the moment is finer: a move dated in the moment's second or the next may have come before it or after it. The
// commits are each HEAD moved to then, and the one it named before them, which is the reflog's next entry by its place,
// not by its time: a clock stepped back can date it later than they are.
func (s *Ship) headsAt(ctx context.Context, dir string, at time.Time) ([]string, bool) {
	out, err := s.git(ctx, dir, "reflog", "show", "--format=%H %gd", "--date=unix", "HEAD")
	if err != nil {
		return nil, false
	}
	second := at.Unix()
	var heads []string
	moved := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") { // newest first
		commit, place, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(place, "HEAD@{"), "}")
		secs, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			continue
		}
		within := secs == second || secs == second+1
		if !moved && secs > second+1 {
			continue
		}
		if !slices.Contains(heads, commit) {
			heads = append(heads, commit)
		}
		if !within {
			break
		}
		moved = true
	}
	return heads, len(heads) > 0
}

// Keep records that a change's measurement kept it: its case closed as improved with the result.
func (s *Ship) Keep(caseID string, res measure.Result) (Package, error) {
	unlock, err := s.lock(context.Background(), caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	return s.keepLocked(caseID, res)
}

// keepLocked reloads the package under the caller's case lock.
func (s *Ship) keepLocked(caseID string, res measure.Result) (Package, error) {
	p, ok, err := s.Load(caseID)
	switch {
	case err != nil:
		return p, err
	case ok && p.State == Kept:
		return p, nil
	case !ok || p.State != Landed:
		return p, refused("case %s has no landed package to keep", caseID)
	}
	p.State, p.Result = Kept, &res
	return p, s.save(&p)
}
