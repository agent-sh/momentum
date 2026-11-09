package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"os"
	"path/filepath"
	"regexp"
	"slices"

	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const (
	StepInvestigate = "investigate"
	StepBuild       = "build"
	StepReview      = "review"
	StepMeasure     = "measure"
)

// The pass's turns, as the store keeps them (LoopTurn): turnBuild names the family whose build a pass reserved last,
// turnRun the family in whose place a pass reserved its first run, and turnSingle, in place of a family, the turn
// (turnBuild or turnRun) that the last pass that took exactly one of the two took.
const (
	turnBuild  = StepBuild
	turnRun    = "run"
	turnSingle = "single"
)

// Budgets are the loop's limits, in US dollars and time. The defaults come from the Phase 0 baseline: what the
// native path spent to answer a family once (kept on the machine). A case may not cost more than a native family
// answer, a run a fraction of it, and a week what the baseline's whole set of answers cost.
type Budgets struct {
	Run, Case, Day, Week float64
	RunTime              time.Duration
	MaxTurns             int
}

// DefaultBudgets: a run $8 and 30 minutes, at most 120 turns; a case $20; a day $40; a week $200.
var DefaultBudgets = Budgets{Run: 8, Case: 20, Day: 40, Week: 200, RunTime: 30 * time.Minute, MaxTurns: 120}

// Runner owns case changes and the pass's host inputs.
type Runner struct {
	Store              *store.Store
	Ship               *Ship
	Toolchains         Toolchains
	Tacit              string
	Post               func(context.Context, string, string, string) (string, error)
	Agent              Agent
	Detectors          []detect.Detector
	Families           []string
	Budgets            Budgets
	Rules, Constraints []string
	Dir                string
	Inputs             InputOptions
	Now                func() time.Time
	Log                func(format string, args ...any)
	pass               *passInputs
	pickedSkips        []Done
	handUnread         map[string]bool // cases whose hand landing request could not be read in this pass
	budgetAt           time.Time       // the pass's one clock reading for its budgets (budgetNow)
}

// Options are what one run of the loop does.
type Options struct {
	Dry      bool     // pick, run no agent, write nothing
	Families []string // only these (default: the runner's)
	Case     string   // only this case
}

// Done is one step the run took, or why it took none.
type Done struct {
	Family, Case, Step string
	Run                store.LoopRun
	Skipped            string   // why no step ran
	Refused            []string // what the run's relay refused to forward (also in the run's summary)
	Did                string   // outcome of a host step with no agent run
}

// budgetNow is the moment the budgets count back from: the pass's one reading while a pass runs, so its room and every
// budget check in it see the same day and week, and the clock now otherwise.
func (r *Runner) budgetNow() time.Time {
	if !r.budgetAt.IsZero() {
		return r.budgetAt
	}
	return r.now()
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		if clean, err := Redact(context.Background(), fmt.Sprintf(format, args...)); err == nil {
			r.Log("%s", clean)
		}
	}
}

// Once runs the loop once. A dry run picks each family's case and step and says what it would run: it writes
// nothing, ends no run and files no finding.
func (r *Runner) Once(ctx context.Context, o Options) ([]Done, error) {
	r.handUnread = nil
	o.Families = uniqueFamilies(o.Families) // a family named twice is one family
	if !o.Dry {
		if err := sandbox.Available(true); err != nil {
			return nil, err
		}
		if err := sandbox.StopLeftover(ctx, UnitPrefix); err != nil {
			return nil, err
		}
		r.sweepPasses()
		r.pass = &passInputs{}
		defer func() { r.pass.close(); r.pass = nil }()
		if err := r.endStale(ctx); err != nil {
			return nil, err
		}
		r.sweepRuns(ctx)
		outs, swept, err := detect.Run(ctx, r.Store, r.Detectors, detect.Window{Since: r.now().Add(-detect.DefaultWindow), Until: r.now()},
			"loop:detect", false)
		if err != nil {
			return nil, err
		}
		for _, out := range outs {
			if out.Err != nil {
				r.logf("detector %s: %v", out.Family, out.Err)
			}
			opened := 0
			for _, f := range out.Filed {
				if f.Observed == store.Opened || f.Observed == store.Reopened {
					opened++
				}
			}
			r.logf("detector %s: %d findings, %d new or back, %d refused", out.Family, len(out.Filed), opened, out.Refused())
		}
		health, err := r.sweepHealth(ctx)
		if err != nil {
			// a case the sweep cannot read closes nothing this pass, and holds none of the pass's other steps
			r.logf("the component-health sweep closes nothing this pass: %v", err)
		}
		for _, c := range append(swept, health...) {
			r.logf("swept %s: %s", c.ID, c.Title)
		}
	}
	measured, err := r.Measure(ctx, o)
	if err != nil {
		return nil, err
	}
	families := uniqueFamilies(r.Families)
	if len(o.Families) > 0 {
		families = o.Families
	}
	// the turns: a turn the store cannot read leaves its order as configured
	turns := map[string]string{}
	for _, kind := range []string{turnBuild, turnRun} {
		turn, err := r.Store.LoopTurn(ctx, kind)
		if err != nil {
			r.logf("the loop's %s turn: %v; the pass takes the families in their configured order", kind, err)
			continue
		}
		turns[kind] = turn
	}
	// with room for one run, the build and the run turn take the passes in turn: a pass serves the run turn first when
	// the last pass that took exactly one turn took the build's, and when the store names no such pass or cannot be
	// read, so the side that can wait longest is served
	single, err := r.Store.LoopTurn(ctx, turnSingle)
	if err != nil {
		r.logf("the loop's last single turn: %v; a pass with room for one run serves the run turn first", err)
	}
	// every family's run is reserved before any starts, so the budgets count each in flight at its whole budget
	// whatever the others spend meanwhile
	type reserved struct {
		fam, step string
		c         store.Case
		run       store.LoopRun
	}
	var (
		done   []Done
		starts []reserved
		picked error    // the first family's pick that failed: the pass returns it after the others' runs
		clean  []string // the families picked without an error
	)
	for _, m := range measured {
		did := m.Status
		if m.Result != nil && m.Result.Effect != "" {
			did += ": " + m.Result.Effect
		}
		if m.Why != "" && !strings.Contains(did, m.Why) {
			did += ": " + m.Why
		}
		done = append(done, Done{Case: m.Case, Step: StepMeasure, Did: did})
	}
	cands := map[string][]candidate{}
	for _, fam := range families {
		if ctx.Err() != nil { // the pass was told to stop (a signal): no run starts
			done = append(done, Done{Family: fam, Skipped: "stopped: " + ctx.Err().Error()})
			continue
		}
		cs, why, err := r.candidates(ctx, fam, o.Case, o.Dry)
		done = append(done, r.pickedSkips...)
		switch {
		case err != nil:
			if picked == nil {
				picked = err
			}
			done = append(done, Done{Family: fam, Skipped: "pick: " + err.Error()})
			continue
		case len(cs) == 0:
			done = append(done, Done{Family: fam, Skipped: why})
		default:
			cands[fam] = cs
		}
		clean = append(clean, fam)
	}
	// prepareStep prepares one family's run without reserving it, and says what came of it: ready, or a run recorded
	// (the budgets refuse it, its preparation failed), or none (a hold, inputs that could not be made)
	prepared := map[string]store.LoopRun{} // by case: a build prepared and not yet reserved
	named := map[string]bool{}             // by case: a refused reservation whose row still names its directory
	prepareStep := func(fam string, c store.Case, step string) (store.LoopRun, outcome) {
		if ctx.Err() != nil {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "stopped: " + ctx.Err().Error()})
			return store.LoopRun{}, runNone
		}
		if why, err := r.affordable(ctx); why != "" || err != nil {
			run, refusal := r.reserve(ctx, fam, c.ID, step)
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
			return store.LoopRun{}, runRecorded
		}
		if err := r.prepareInputs(ctx); err != nil {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "inputs: " + err.Error()})
			return store.LoopRun{}, runNone
		}
		run, inputErr := r.prepareRun(ctx, c, fam, step)
		switch {
		case inputErr == nil:
			return run, runReady
		case step == StepBuild && held(inputErr):
			done = append(done, r.buildSkipped(ctx, c, "prepare", inputErr))
			return store.LoopRun{}, runNone
		case step != StepBuild:
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "inputs: " + inputErr.Error()})
			return store.LoopRun{}, runNone
		}
		dir := run.Dir
		run, refusal := r.reservePrepared(ctx, run)
		if refusal != "" {
			if run.Dir == "" { // no row names the directory: what the preparation left goes, its tree back to the case first
				r.removeRunDirs(context.WithoutCancel(ctx), c.ID, dir, true)
			}
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
			return store.LoopRun{}, runRecorded
		}
		key := "build inputs"
		var staged *buildStageError
		if errors.As(inputErr, &staged) {
			key = staged.key
		}
		res := Result{Started: true, CostKnown: true, Status: store.RunFailed, Failure: key, Text: inputErr.Error()}
		r.finish(context.WithoutCancel(ctx), c, &run, res, nil)
		done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run})
		return store.LoopRun{}, runRecorded
	}
	// reserveReady reserves a prepared run
	reserveReady := func(fam string, c store.Case, step string, run store.LoopRun) outcome {
		if ctx.Err() != nil {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "stopped: " + ctx.Err().Error()})
			return runNone
		}
		run, refusal := r.reservePrepared(ctx, run)
		if refusal != "" {
			if run.Dir != "" { // a row still names the run's directory: it stays
				named[c.ID] = true
			}
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
			return runRecorded
		}
		starts = append(starts, reserved{fam, step, c, run})
		return runReserved
	}
	// unready removes what a prepared build left on the host when it ends unreserved (refused, or the pass stopped).
	// The package's build tree is in the run directory by then (buildPackage), so it goes back to the case's work
	// directory first, and the case's next build finds the tree its package names; a stopping pass still moves it.
	// Then the host and run directories go. A tree that cannot go back (another ship step holds the case's lock)
	// keeps its run directory.
	unready := func(id string) {
		if run, ok := prepared[id]; ok {
			delete(prepared, id)
			r.removeRunDirs(context.WithoutCancel(ctx), id, run.Dir, true)
		}
	}
	prepare := func(fam string, c candidate) outcome {
		if o.Dry {
			return runReady
		}
		run, out := prepareStep(fam, c.c, c.step)
		if out == runReady {
			prepared[c.c.ID] = run
		}
		return out
	}
	reserve := func(fam string, c candidate) outcome {
		if o.Dry { // as a live pass would reserve it, with nothing prepared, reserved or written
			done = append(done, Done{Family: fam, Case: c.c.ID, Step: c.step, Skipped: "dry: " + c.why()})
			return runReserved
		}
		run, ok := prepared[c.c.ID]
		if !ok {
			var out outcome
			if run, out = prepareStep(fam, c.c, c.step); out != runReady {
				return out
			}
		}
		out := reserveReady(fam, c.c, c.step, run)
		switch {
		case out == runReserved || named[c.c.ID]: // reserved, or a refusal whose row still names the directory
			delete(prepared, c.c.ID)
		case ok:
			unready(c.c.ID)
		default: // prepared here and not reserved: what it prepared goes
			r.removeRunDirs(context.WithoutCancel(ctx), c.c.ID, run.Dir, c.step == StepBuild)
		}
		return out
	}
	// the room the budgets leave, no run, one or more: a dry pass ignores the budgets, and one that cannot read them
	// prepares as if they had room, so affordable refuses what they do not
	room, roomWhy := 2, ""
	if !o.Dry {
		r.budgetAt = r.now() // one reading for the pass's budgets: the room and every check after it
		defer func() { r.budgetAt = time.Time{} }()
		var err error
		if room, roomWhy, err = r.room(ctx, 2); err != nil {
			r.logf("the loop's budgets: %v", err)
			room, roomWhy = 2, ""
		}
	}
	steps, next, took := schedule(families, cands, turns, room, single != turnRun, prepare, reserve)
	for id := range prepared { // cannot happen: schedule reserves every build it readies
		unready(id)
	}
	for _, s := range steps {
		switch {
		case s.skipped == noRoom:
			done = append(done, Done{Family: s.fam, Case: s.cand.c.ID, Step: s.cand.step, Skipped: s.skipped + " (" + roomWhy + ")"})
		case s.skipped != "":
			done = append(done, Done{Family: s.fam, Case: s.cand.c.ID, Step: s.cand.step, Skipped: s.skipped})
		}
	}
	if !o.Dry {
		// a turn the store cannot write keeps its old family: the pass counts it among its steps
		failed, changed := 0, 0
		var writeErr error
		write := map[string]string{}
		for _, kind := range []string{turnBuild, turnRun} {
			if next[kind] != turns[kind] {
				write[kind] = next[kind]
			}
		}
		switch { // a pass that took exactly one turn is the one the next pass with room for one run answers
		case took[turnBuild] && !took[turnRun]:
			write[turnSingle] = turnBuild
		case took[turnRun] && !took[turnBuild]:
			write[turnSingle] = turnRun
		}
		if len(write) > 0 { // in one transaction: the turns and the single row move together, or none does
			changed = len(write)
			if err := r.Store.SetLoopTurns(ctx, write); err != nil {
				failed, writeErr = changed, err
				r.logf("the loop's turns %v: %v", write, err)
			}
		}
		if failed > 0 {
			done = append(done, Done{Step: "turn", Skipped: fmt.Sprintf("%d of %d turns not recorded: %v", failed, changed, writeErr)})
		}
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, s := range starts {
		wg.Add(1)
		go func(s reserved) {
			defer wg.Done()
			d := r.step(ctx, s.fam, s.c, s.step, s.run)
			mu.Lock()
			done = append(done, d)
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	// a family whose pick failed stops no other family's reviews, landings or lessons
	if !o.Dry && (len(clean) > 0 || len(families) == 0) {
		done = append(done, r.reviews(ctx, o.Case, clean...)...)
		if err := r.noteWaiting(ctx); err != nil {
			r.logf("waiting packages: %v", err)
		}
		if err := r.lessons(ctx, o.Case, clean); err != nil && picked == nil { // lessons logs each family's error
			picked = err
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Family+done[i].Step < done[j].Family+done[j].Step })
	return done, picked
}

// removeRunDirs removes a prepared run's host and run directories once nothing its case needs is in them: a
// build's tree goes back to the case first (returnWork), and a tree that cannot go back keeps its run directory.
func (r *Runner) removeRunDirs(ctx context.Context, caseID, dir string, build bool) {
	stays := false
	if build && r.Ship != nil {
		var err error
		if stays, err = r.Ship.returnWork(ctx, caseID, dir); err != nil {
			r.logf("build tree of %s: %v", caseID, err)
		}
	}
	removeInputTree(HostDir(dir))
	if !stays {
		removeInputTree(dir)
	}
}

// runIDForm is the form of a run's id (newRunID): a run directory has it, and its host directory is the id between
// "." and "-host" (HostDir).
var runIDForm = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{8}$`)

// sweepRuns removes the run directories that neither a run naming its directory nor a hold's receipt names
// (store.KnownLoopRunDirs) and whose agent never started, which a pass that ended between a build's preparation and
// its reservation (a crash) leaves: a build tree a package names in one goes back to its case first, and a directory
// whose tree cannot go back stays. A build tree is only ever a run directory's work, so one without it goes with no
// package read, and a package that cannot be read, or whose case the store does not know, is passed over, logged once
// a sweep. One whose agent started (agentRan: a store restored, or made anew, lost its row) stays, logged once. It
// looks only at entries in a run id's form, and removes nothing when the store cannot say which runs it knows. A pass
// sweeps when it starts, under the loop's lock, before it prepares any run of its own, so every run directory then is
// an earlier pass's.
func (r *Runner) sweepRuns(ctx context.Context) {
	runs := filepath.Join(r.Dir, "runs")
	entries, err := os.ReadDir(runs)
	if err != nil {
		return
	}
	ids := map[string]bool{}
	for _, e := range entries {
		id := e.Name()
		if host, ok := strings.CutPrefix(id, "."); ok {
			if id, ok = strings.CutSuffix(host, "-host"); !ok {
				continue
			}
		}
		if runIDForm.MatchString(id) {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return
	}
	known, err := r.Store.KnownLoopRunDirs(ctx)
	if err != nil {
		r.logf("run directories: the store cannot say which runs it knows: %v", err)
		return
	}
	passed := map[string]string{} // the packages passed over (returnTrees), logged once the sweep is done
	defer func() {
		for _, name := range slices.Sorted(maps.Keys(passed)) {
			r.logf("package %s: the sweep passed over it, and its case's next build makes its tree again: %s", name, passed[name])
		}
	}()
	for id := range ids {
		if known[id] {
			continue
		}
		dir := filepath.Join(runs, id)
		if agentRan(dir) {
			r.keepUnknownRun(id, dir)
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, "work")); err == nil && r.Ship != nil { // a build tree is only ever there
			over, err := r.Ship.returnTrees(ctx, dir)
			maps.Copy(passed, over)
			if err != nil {
				r.logf("run directory %s, which no run names: %v", id, err)
				continue
			}
		}
		removeInputTree(HostDir(dir))
		removeInputTree(dir)
		r.logf("removed run directory %s, which no run names", id)
	}
}

// agentRan says whether a run directory holds what only a started agent leaves: a transcript or an error log in its
// host directory, or its output. No agent starts before its run is reserved, so a run prepared and never reserved has
// none of these.
func agentRan(dir string) bool {
	for _, pattern := range []string{"transcript*.jsonl", "stderr*"} {
		if found, _ := filepath.Glob(filepath.Join(HostDir(dir), pattern)); len(found) > 0 {
			return true
		}
	}
	_, err := os.Lstat(filepath.Join(dir, "out"))
	return err == nil
}

// keptMark is the file a run directory no run names, whose agent started, is marked with once its keeping is logged.
const keptMark = ".kept"

// keepUnknownRun keeps a run directory no run names whose agent started, and logs it the first time.
func (r *Runner) keepUnknownRun(id, dir string) {
	mark := HostDir(dir)
	if _, err := os.Lstat(mark); err != nil {
		mark = dir
	}
	mark = filepath.Join(mark, keptMark)
	if _, err := os.Lstat(mark); err == nil {
		return
	}
	r.logf("kept run directory %s: no run names it, and its agent ran (its row may be in an older copy of the store)", id)
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		r.logf("run directory %s: %v", id, err)
	}
}

// inTurn is a turn's order of the families: as given, starting after the family the turn names (schedule), so the one
// build of a pass, and the room the budgets leave, go to each family in turn. A family not among them, or none, keeps
// the order as given.
func inTurn(families []string, last string) []string {
	i := slices.Index(families, last)
	if i < 0 {
		return families
	}
	return slices.Concat(families[i+1:], families[:i+1])
}

func (r *Runner) buildSkipped(ctx context.Context, c store.Case, stage string, cause error) Done {
	var outage *baseOutage
	var busy *busyMachine
	switch {
	case errors.As(cause, &busy):
		if ctx.Err() == nil {
			why := cause.Error() + gateWords(cause)
			r.logf("build of %s waits for room for its check: %s", c.ID, why)
			if err := r.noteCapacityHold(ctx, c, why); err != nil {
				r.logf("build waiting note: %v", err)
			}
		}
	case errors.As(cause, &outage):
		// a mirror's hold has Git's line in the pass's log already, from the pass's fetch; a proposal read's is new
		if outage.proposal && outage.line != "" {
			r.logf("build of %s waits to read its proposal branch: %s", c.ID, outage.line)
		}
		if ctx.Err() == nil { // a pass told to stop ended the fetch: no note names the stop as the remote's
			if err := r.noteBaseHold(ctx, c, cause.Error(), outage.proposal); err != nil {
				r.logf("build waiting note: %v", err)
			}
		}
	default:
		if err := r.noteBuildSkip(ctx, c, stage, cause.Error()); err != nil {
			r.logf("build waiting note: %v", err)
		}
	}
	return Done{Family: c.Family, Case: c.ID, Step: StepBuild, Skipped: cause.Error()}
}

// baseNotePrefix begins the ids of a case's notes of builds held because their base cannot be fetched (noteBaseHold).
func baseNotePrefix(caseID string) string { return "loop-build:" + store.IDPart(caseID) + ":base:" }

// proposalNotePrefix begins the ids of a case's notes of builds held because their proposal branch cannot be read now
// (noteBaseHold): a hold of its own, which a fetch of the base does not end, only a read of the branch that goes
// through (Ship.prepare) or the case leaving testing.
func proposalNotePrefix(caseID string) string {
	return "loop-build:" + store.IDPart(caseID) + ":proposal:"
}

// baseHoldRaise is how long a case's builds may wait for their base, or to read their proposal branch, before the
// wait's note is raised from low to normal importance: a passing outage ends within it, and one that lasts a day (gh
// logged out, a certificate the machine does not trust) needs a person.
const baseHoldRaise = 24 * time.Hour

// noteBaseHold notes a build held because its repository's mirror has no usable fetch, or, for proposal, because its
// proposal branch cannot be read now: one note per hold and class of cause (holdClass), open until the hold ends
// (endHoldNotes). Once the case's builds have waited baseHoldRaise since the oldest note of the hold, the note is
// raised to normal importance. A note a person took out of the report stays out while its hold lasts at the importance
// it was taken out at, and comes back once when that rises; one whose hold ended (it says so) opens again with the
// next hold of its class.
func (r *Runner) noteBaseHold(ctx context.Context, c store.Case, why string, proposal bool) error {
	prefix, title := baseNotePrefix(c.ID), "The loop's build for case "+c.ID+" waits for its base"
	if proposal {
		prefix, title = proposalNotePrefix(c.ID), "The loop's build for case "+c.ID+" waits to read its proposal branch"
	}
	detail, err := Redact(ctx, why)
	if err != nil {
		return err
	}
	detail = store.CaseNoteText(detail)
	sum := sha256.Sum256([]byte(detail))
	id := prefix + hex.EncodeToString(sum[:])
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	since := r.now()
	var current *store.Note
	for i, n := range notes {
		if !strings.HasPrefix(n.ID, prefix) {
			continue
		}
		if n.ID == id {
			current = &notes[i]
		}
		if !holdNoteEnded(n) && n.At.Before(since) { // a note of this hold, one a person took out of the report too
			since = n.At
		}
	}
	importance := store.ImportanceLow
	if r.now().Sub(since) >= baseHoldRaise {
		importance = store.ImportanceNormal
	}
	switch {
	case current != nil && holdNoteEnded(*current):
		current = nil // its hold ended (the note may still stand, if a pass stopped between its writes): this is a new one
	case current != nil && !current.Done.IsZero() && importance <= current.Importance:
		return nil // a person took it out of the report at this importance, while this hold lasts
	}
	// a dismissal covers the importance it was made at: the note comes back, once, when its importance rises
	at := r.now()
	if current != nil {
		if current.Importance >= importance && current.Done.IsZero() {
			return nil
		}
		at = current.At
	}
	_, err = r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: importance, At: at,
		Title: title, Detail: detail})
	return err
}

// The ends of a base, proposal or capacity note's hold, which endHoldNotes adds to the note it resolves.
const (
	baseFetched     = "It ended: a build of the case fetched its base."
	proposalRead    = "It ended: its proposal branch was read."
	caseLeftTesting = "It ended: the case left testing."
	roomFound       = "It ended: a later build of the case went on to its agent."
	caseStopped     = "It ended: the stop rule stopped the case."
	gateBroke       = "It ended: the capacity gate failed, which counts as the host's error at the check."
)

// holdNoteEnded says whether a base, proposal or capacity note says its hold ended (endHoldNotes).
func holdNoteEnded(n store.Note) bool {
	return strings.HasSuffix(n.Detail, baseFetched) || strings.HasSuffix(n.Detail, proposalRead) || strings.HasSuffix(n.Detail, caseLeftTesting) ||
		strings.HasSuffix(n.Detail, roomFound) || strings.HasSuffix(n.Detail, gateBroke) || strings.HasSuffix(n.Detail, caseStopped)
}

// capacityNoteID is the note of a case's builds held because the machine has no room for their check.
func capacityNoteID(caseID string) string { return "loop-build:" + store.IDPart(caseID) + ":capacity" }

// noteCapacityHold notes a build held because the capacity gate said the machine has no room for its check: one note a
// case, with what the gate said last, open until a build of the case finds room (endCapacityNote). Once the case's
// builds have waited baseHoldRaise since the hold began, the note is raised to normal importance, as a base hold's is:
// a gate that keeps saying no is a machine that never has room, or a gate that says no for another reason. A note a
// person took out of the report stays out while its hold lasts at the importance it was taken out at, and comes back
// once when that rises.
func (r *Runner) noteCapacityHold(ctx context.Context, c store.Case, why string) error {
	detail, err := Redact(ctx, why)
	if err != nil {
		return err
	}
	detail = store.CaseNoteText(detail)
	id := capacityNoteID(c.ID)
	n, ok, err := r.Store.Note(ctx, id)
	if err != nil {
		return err
	}
	going := ok && !holdNoteEnded(n) // the hold this note began goes on
	since := r.now()
	if going {
		since = n.At
	}
	importance := store.ImportanceLow
	if r.now().Sub(since) >= baseHoldRaise {
		importance = store.ImportanceNormal
	}
	switch {
	case going && !n.Done.IsZero() && importance <= n.Importance:
		return nil // a person took it out of the report at this importance, while this hold lasts
	case going && n.Done.IsZero() && importance <= n.Importance && n.Detail == detail:
		return nil
	}
	_, err = r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: importance, At: since,
		Title: "The loop's build for case " + c.ID + " waits for room for its check", Detail: detail})
	return err
}

// endCapacityNote ends a case's capacity note, as why says: a later build of the case went on to its agent, or the
// gate failed (a failure the stop rule counts, which no longer waits for room).
func (r *Runner) endCapacityNote(ctx context.Context, caseID, why string) error {
	n, ok, err := r.Store.Note(ctx, capacityNoteID(caseID))
	if err != nil || !ok {
		return err
	}
	return endHoldNotes(ctx, r.Store, r.now(), []store.Note{n}, n.ID, why)
}

// endBaseNotes ends a case's notes of builds held for their base, once their hold ended (why: a build fetched its
// base, or the case left testing).
func (r *Runner) endBaseNotes(ctx context.Context, caseID, why string) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	return endHoldNotes(ctx, r.Store, r.now(), notes, baseNotePrefix(caseID), why)
}

// endHoldNotes ends the notes among notes whose ids begin with prefix (a case's base or proposal notes), once their
// hold ended (why says how): each says so and is resolved, a note a person took out of the report too, so the next
// hold of its class opens it again.
func endHoldNotes(ctx context.Context, st *store.Store, now time.Time, notes []store.Note, prefix, why string) error {
	for _, n := range notes {
		if !strings.HasPrefix(n.ID, prefix) {
			continue
		}
		ended := holdNoteEnded(n)
		if ended && !n.Done.IsZero() {
			continue
		}
		if !ended { // a note that says its hold ended but stands (a pass stopped between the writes) is only resolved
			n.Detail = n.Detail + " " + why
			if _, err := st.PutNote(ctx, n); err != nil {
				return err
			}
		}
		if _, _, err := st.DoneNote(ctx, n.ID, now); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) noteBuildSkip(ctx context.Context, c store.Case, stage, why string) error {
	return r.noteStepSkip(ctx, c, "build", stage, why)
}

func (r *Runner) noteStepSkip(ctx context.Context, c store.Case, step, stage, why string) error {
	detail, err := Redact(ctx, why)
	if err != nil {
		return err
	}
	detail = store.CaseNoteText(detail)
	cause := sha256.Sum256([]byte(detail))
	id := "loop-" + step + ":" + store.IDPart(c.ID) + ":" + stage + ":" + hex.EncodeToString(cause[:])
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, note := range notes {
		if note.ID == id && note.Detail == detail {
			return nil
		}
	}
	_, err = r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow,
		At: r.now(), Title: "The loop's " + step + " for case " + c.ID + " waits: " + stage, Detail: detail})
	return err
}

// endStale ends the runs that started longer ago than any run may last and never ended: killed with the process
// that ran them, whose sandbox died with it. What such a run spent is unknown, so it counts its whole budget.
func (r *Runner) endStale(ctx context.Context) error {
	live := "" // an evaluation's runs are its own: their cases are in their trials' stores, not this one
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Experiment: &live})
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == store.RunRunning && r.now().Sub(run.Started) > r.Budgets.RunTime {
			run.Status, run.Failure, run.Ended = store.RunFailed, "killed", r.now()
			if run.USD == 0 {
				run.USD = r.Budgets.Run
			}
			if run.Step == StepBuild && run.Dir != "" {
				if _, err := os.Lstat(filepath.Join(HostDir(run.Dir), notStartedMarker)); err == nil {
					// killed before its agent started (its marker says so): it spent nothing
					run.USD, run.Started = 0, time.Time{}
					run.Summary = strings.TrimSpace(run.Summary + "\nkilled before its agent started")
				}
			}
			// a killed investigation leaves its findings file: the case keeps what it found, still investigating,
			// recorded before the run ends, as the runner records a run's
			if run.Step == StepInvestigate && run.Dir != "" {
				// Only the host's completed, authenticated result can conclude an investigation. Without it,
				// an unfinished run's findings file is still useful, but its stdout cannot move the case.
				res, streamErr := readHostResult(run.Dir)
				why := undecided(res, streamErr)
				if f, reported, ok := investigationFindings(res, run.Dir); ok || why != "" {
					if err := r.keepFindings(ctx, run, f, reported, why); err != nil {
						run.Failure, run.Summary = "scrub", "(findings withheld: recording failed)"
					}
				}
			}
			if err := r.Store.FinishLoopRun(ctx, run); err != nil {
				return err
			}
		}
	}
	return nil
}

// pick takes investigating cases before open cases, subject to reservations and budgets.
func (r *Runner) pick(ctx context.Context, family, only string, dry bool) (store.Case, string, string, error) {
	cands, why, err := r.candidates(ctx, family, only, dry)
	if err != nil || len(cands) == 0 {
		return store.Case{}, "", why, err
	}
	return cands[0].c, cands[0].step, cands[0].why(), nil
}

// candidate is a case a family's pick may run, with its step.
type candidate struct {
	c    store.Case
	step string
	rank int
}

func (c candidate) why() string {
	return fmt.Sprintf("%s %s (%s, %d evidence)", c.step, c.c.ID, c.c.Status, c.c.Evidence)
}

// candidates are the family's workable cases, best first (pick takes the first), or none and why.
func (r *Runner) candidates(ctx context.Context, family, only string, dry bool) ([]candidate, string, error) {
	r.pickedSkips = nil
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: family, All: true})
	if err != nil {
		return nil, "", err
	}
	var cands []candidate
	var passed []string
	if !dry {
		if err := r.releaseHolds(ctx, cs); err != nil {
			r.logf("hold notes: %v", err)
		}
		r.withdrawStale(ctx, cs, only)
	}
	for _, c := range cs {
		if only != "" && c.ID != only {
			continue
		}
		spent, running, err := r.caseRoom(ctx, c)
		if err != nil {
			return nil, "", err
		}
		if running {
			passed = append(passed, c.ID+" has a run in flight")
			continue
		}
		if r.Ship != nil && (c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring) {
			p, ok, err := r.Ship.Load(c.ID)
			if err != nil {
				if noteErr := r.packageProblem(ctx, c, err, dry); noteErr != nil {
					return nil, "", noteErr
				}
				passed = append(passed, c.ID+" package: "+err.Error())
				r.pickSkipped(c, "package", err)
				continue
			}
			if ok && p.Cleanup != "" && !dry {
				if _, err := r.Ship.retryCleanup(ctx, c.ID); err != nil {
					passed = append(passed, c.ID+" cleanup: "+err.Error())
					r.pickSkipped(c, "cleanup", err)
				}
			}
			if ok && (p.State == RolledBack || p.State == RollingBack) {
				if !dry {
					if _, err := r.Ship.RollBack(ctx, c.ID, "resume rollback"); err != nil {
						passed = append(passed, c.ID+" rollback: "+err.Error())
						r.pickSkipped(c, "rollback", err)
						continue
					}
				}
				passed = append(passed, c.ID+" has a rollback; its receipt precedes more work")
				continue
			}
		}
		if !dry && r.Ship != nil && (c.Status == store.CaseOpen || c.Status == store.CaseInvestigating) {
			// a hand landing withdrawal that stopped after its case reopened is finished here
			if p, ok, err := r.Ship.Load(c.ID); err == nil && ok && p.State == Withdrawn && p.PR != "" {
				if _, err := r.Ship.reconcileHand(ctx, c.ID); err != nil {
					r.logf("finish the withdrawal of %s: %v", c.ID, err)
				}
			}
		}
		step, rank := "", 0
		switch c.Status {
		case store.CaseTesting:
			step, rank = StepBuild, 3
			if r.Ship == nil {
				passed = append(passed, c.ID+" has no ship tool")
				continue
			}
			if p, ok, err := r.Ship.Load(c.ID); dry && err == nil && ok && p.State == Withdrawn && p.PR != "" {
				// as a live pass would: it finishes the withdrawal and builds nothing
				passed = append(passed, c.ID+"'s hand landing withdrawal finishes")
				continue
			} else if !dry && err == nil && ok && len(p.Hand) > 0 && p.PR != "" {
				// a proposal for a hand landing is a person's to land or close: read which before more work
				p, err := r.Ship.reconcileHand(ctx, c.ID)
				if err == nil {
					if noteErr := r.Ship.doneHandReadNotes(ctx, c.ID); noteErr != nil {
						r.logf("hand landing notes: %v", noteErr)
					}
				}
				switch {
				case err != nil:
					passed = append(passed, c.ID+" hand landing: "+err.Error())
					r.pickSkipped(c, "hand", err)
					if r.handUnread == nil {
						r.handUnread = map[string]bool{}
					}
					r.handUnread[c.ID] = true        // its review waits too: the request may be merged or closed already
					if !errors.Is(err, ErrRefused) { // a gh or git failure; a wait or an early merge is read again without a note
						if noteErr := r.noteStepSkip(ctx, c, "hand", "read", err.Error()); noteErr != nil {
							r.logf("hand landing note: %v", noteErr)
						}
					}
					continue
				case p.State == Landed:
					passed = append(passed, c.ID+" was landed by hand; its measurement starts")
					continue
				case p.State == Withdrawn && proposalsUsed(p):
					passed = append(passed, c.ID+" used its proposals")
					continue
				case p.State == Withdrawn:
					if cur, err := r.Store.CaseByID(ctx, c.ID); err != nil || cur.Status != store.CaseTesting {
						passed = append(passed, c.ID+"'s hand landing proposal was closed; the case reopens")
						continue
					} // still in testing, with what it holds now newer than the request: it builds a new package
				}
			}
			var settled *Package // a dry pass's settlement of a configuration package, as the live one would leave it
			if p, ok, err := r.Ship.Load(c.ID); err == nil && ok && p.Kind == KindConfig {
				// a configuration proposal waits for its review, and then for a person: the case's close ends it. Every
				// configuration package is settled here, with its note (configNote); a dry pass reports the settlement a
				// live one would make, and writes nothing (configSettled)
				settle := func() (Package, error) { return r.Ship.reconcileConfig(ctx, c.ID) }
				if dry {
					settle = func() (Package, error) { return r.Ship.configSettled(ctx, c.ID, p) }
				}
				if p, err = settle(); err != nil {
					passed = append(passed, c.ID+" configuration proposal: "+err.Error())
					r.pickSkipped(c, "config", err)
					continue
				}
				if dry {
					settled = &p
				}
				if why := configPassed(p); why != "" {
					r.placeHeld(ctx, c, p, dry, &passed)
					passed = append(passed, c.ID+why)
					continue
				} // withdrawn, it builds again on the file as it is; any other state goes on as a package does
			}
			load := func() (Package, bool, error) {
				if settled != nil {
					return *settled, true, nil
				}
				return r.Ship.Load(c.ID)
			}
			if p, ok, err := load(); err != nil {
				if noteErr := r.packageProblem(ctx, c, err, dry); noteErr != nil {
					return nil, "", noteErr
				}
				passed = append(passed, c.ID+" package: "+err.Error())
				r.pickSkipped(c, "package", err)
				continue
			} else if ok && p.State == Landed {
				r.liftHold(ctx, c, dry)
				if !dry {
					if p.Cleanup != "" {
						if _, err := r.Ship.retryCleanup(ctx, c.ID); err != nil {
							passed = append(passed, c.ID+" cleanup: "+err.Error())
							r.pickSkipped(c, "cleanup", err)
						}
					}
					if err := r.Ship.deploymentReceipt(ctx, p); err != nil {
						passed = append(passed, c.ID+" deployment: "+err.Error())
						r.pickSkipped(c, "deploy", err)
						continue
					}
					if _, err := r.Store.DeployCase(ctx, c.ID, r.Ship.pkgDir(c.ID), p.LandedAt, p.PR+": "+p.Title, "loop"); err != nil {
						passed = append(passed, c.ID+" deployment: "+err.Error())
						r.pickSkipped(c, "deploy", err)
						continue
					}
				}
				passed = append(passed, c.ID+" has landed; deployment precedes more work")
				continue
			} else if ok && proposalsUsed(p) {
				passed = append(passed, c.ID+" used its proposals")
				r.liftHold(ctx, c, dry)
				continue
			} else if ok && roundsUsed(p) {
				passed = append(passed, c.ID+" used its review rounds")
				r.liftHold(ctx, c, dry)
				continue
			} else if ok && (p.State == Proposed || p.State == Landing) {
				r.placeHeld(ctx, c, p, dry, &passed)
				continue // its review or its landing comes next (reviews)
			} else if ok && p.State != Withdrawn && !p.ProposalAt.IsZero() && p.Plan != nil {
				if wait := r.Ship.waiting(ctx, c, *p.Plan, r.now()); wait != nil {
					passed = append(passed, c.ID+"'s proposal waits on "+wait.what())
					r.liftHold(ctx, c, dry)
					continue
				}
			}
			if why, err := r.heldForPerson(ctx, c, dry); err != nil {
				return nil, "", err
			} else if why != "" {
				// a hold for a person takes no slot: the family's next workable case runs instead
				passed = append(passed, c.ID+" held "+why)
				continue
			}
			pkg, had, _ := load() // the package as selection has it: a dry pass's settlement, else the one on disk
			var unread recordError
			if repo, target, err := r.caseTarget(ctx, c); errors.As(err, &unread) {
				// one case's record: the case waits with a low note, and its family's next case runs
				if !dry {
					if noteErr := r.noteBuildSkip(ctx, c, "record", err.Error()); noteErr != nil { // it ends with testing (recordNotePrefix)
						r.logf("record note: %v", noteErr)
					}
				}
				passed = append(passed, c.ID+" record: "+err.Error())
				r.pickSkipped(c, "record", err)
				continue
			} else if err != nil {
				return nil, "", err
			} else if repo == "" && target == "" {
				if !dry {
					r.keepHold(ctx, c, "waits for a repository: the investigation named none", true)
				}
				passed = append(passed, c.ID+" waits for a repository")
				continue
			} else if why, known := r.heldTargetWith(ctx, c, repo, target, pkg, had); why != "" {
				// a hold of the configuration takes no slot: the family's next workable case runs instead
				if known && !dry {
					r.keepHold(ctx, c, why, false)
				}
				passed = append(passed, c.ID+" held: "+why)
				continue
			} else if !dry {
				r.keepHold(ctx, c, "", false)
			}
		case store.CaseClosed:
			continue
		case store.CaseInvestigating:
			step, rank = StepInvestigate, 2
		case store.CaseOpen:
			step, rank = StepInvestigate, 1
		default:
			continue
		}

		if spent+r.Budgets.Run > r.Budgets.Case {
			passed = append(passed, fmt.Sprintf("%s spent $%.2f of its $%.0f, no room for a run", c.ID, spent, r.Budgets.Case))
			continue
		}
		if stop, err := r.stopped(ctx, c, dry); err != nil {
			return nil, "", err
		} else if stop != "" {
			passed = append(passed, c.ID+" stopped: "+stop)
			continue
		}
		cands = append(cands, candidate{c, step, rank})
	}
	if len(cands) == 0 {
		why := "no case to work"
		if len(passed) > 0 {
			why += " (" + strings.Join(passed, "; ") + ")"
		}
		return nil, why, nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		if a.c.Evidence != b.c.Evidence {
			return a.c.Evidence > b.c.Evidence
		}
		return a.c.Seen.After(b.c.Seen)
	})
	return cands, "", nil
}

// placeHeld records, or lifts, the hold of a case whose package goes on to its review, its landing or a person (a
// proposal, or a landing) while its investigation names another place: the package's review and landing go on, and
// the hold's fixed-text reason and note tell a person the fix moved (placeHold). A dry pass records nothing.
func (r *Runner) placeHeld(ctx context.Context, c store.Case, p Package, dry bool, passed *[]string) {
	why := r.placeHold(ctx, c, p)
	if why == "" {
		r.liftHold(ctx, c, dry)
		return
	}
	if !dry {
		r.keepHold(ctx, c, why, false)
	}
	*passed = append(*passed, c.ID+" held: "+why)
}

// placeHold is the reason of placeHeld's hold (unfinishedHold), or "" when the package works on the place the case's
// investigation names, or that place cannot be read now. It reads only the case's record and the checkout's identity.
func (r *Runner) placeHold(ctx context.Context, c store.Case, p Package) string {
	repo, target, err := r.caseTarget(ctx, c)
	if err != nil || repo == "" && target == "" {
		return ""
	}
	kind, place := KindRepo, ""
	if target != "" {
		if place, err = configTargetName(target); err != nil {
			return ""
		}
		kind = KindConfig
	} else if place, err = r.repoIdentity(ctx, repo); err != nil {
		return ""
	}
	if samePlace(p, kind, place) {
		return ""
	}
	return unfinishedHold(c, p, kind, place)
}

// keepHold records a hold's change. A failure to record it holds no selection: the hold still takes no slot.
// liftHold ends a testing case's hold when its next step is no build: a proposal, a landing or a person.
func (r *Runner) liftHold(ctx context.Context, c store.Case, dry bool) {
	if !dry {
		r.keepHold(ctx, c, "", false)
	}
}

func (r *Runner) keepHold(ctx context.Context, c store.Case, why string, waiting bool) {
	if err := r.markHold(ctx, c, why, waiting); err != nil {
		r.logf("hold of %s: %v", c.ID, err)
	}
}

func (r *Runner) pickSkipped(c store.Case, step string, cause error) {
	r.pickedSkips = append(r.pickedSkips, Done{Family: c.Family, Case: c.ID, Step: step, Skipped: cause.Error()})
}

func (r *Runner) packageProblem(ctx context.Context, c store.Case, cause error, dry bool) error {
	if dry {
		return nil
	}
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	id := "loop-package:" + store.IDPart(c.ID)
	for _, note := range notes {
		if note.ID == id {
			return nil
		}
	}
	detail, err := Redact(ctx, cause.Error())
	if err != nil {
		return err
	}
	_, err = r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: r.now(),
		Title: "The loop cannot read the package for case " + c.ID, Detail: detail})
	return err
}

func (r *Runner) caseNoteOnce(ctx context.Context, id, action, detail string) error {
	clean, err := Redact(ctx, detail)
	if err != nil {
		return err
	}
	clean = store.CaseNoteText(clean)
	log, err := r.Store.CaseLog(ctx, id)
	if err != nil {
		return err
	}
	for _, entry := range log {
		if entry.Action == action && entry.Detail == clean {
			return nil
		}
	}
	return r.Store.CaseNote(cleanedNote(ctx), id, action, clean, "loop")
}

// stopRuns reads the stop rule for a case: its last two live runs that count (stopCounts; a run the host ended,
// stopped or killed, says nothing of the agent), since it last came back after a close or
// resumed after a fix of its stop, when both failed the same way in the same step. A build refused for its own output
// at a refusal stage fails the same way only for the same cause: each refusal's reason reaches the next build
// (refusal.md), so refusals for different causes are a build making progress. The newest such refusal stops the case
// on any earlier one there for its cause that its build answered for (buildSpan: of the current package, since the
// newest build that passed every stage), however many for other causes came between, so causes that take turns stop
// too. A run kept before causes were has none, and a missing cause matches nothing, so such a run
// never starts a stop. A host stage, and a host error at a refusal stage (hostCause), stops on its key alone. The
// runs are the newest and the earlier one it matched; ok is false when the rule does not hold.
func stopRuns(ctx context.Context, st *store.Store, c store.Case) (runs []store.LoopRun, ok bool, err error) {
	live := ""
	since := c.Back
	resumed, err := st.ResumedAt(ctx, c.ID)
	if err != nil {
		return nil, false, err
	}
	if resumed.After(since) {
		since = resumed
	}
	if !since.IsZero() {
		since = since.Add(time.Millisecond) // the kept clock is in milliseconds; only runs after the return count
	}
	all, err := st.LoopRuns(ctx, store.RunFilter{Case: c.ID, Experiment: &live, Judged: true, Since: since})
	if err != nil {
		return nil, false, err
	}
	for _, run := range all {
		if stopCounts(run) {
			runs = append(runs, run)
		}
	}
	if len(runs) < 2 {
		return nil, false, nil
	}
	a := runs[0]
	if a.Status == store.RunOK || a.Failure == "" {
		return nil, false, nil
	}
	if refusalStop(a) || a.Step == StepBuild && hostFailure(a) {
		if stopCause(a) == "" && slices.Contains(refusalKeys, a.Failure) {
			return nil, false, nil // a refusal-stage run kept before causes were: its cause is unknown
		}
		// the newest build stops on an earlier one its build answered for that failed at its stage for its cause: a
		// refusal's, whatever came between, or the host's (hostCause, or none at a host stage), unless a refusal came
		// between: a build refused for its output since was making progress, and the host errors around it are apart
		span, err := buildSpan(ctx, st, c, runs)
		if err != nil || len(span) == 0 {
			return nil, false, err
		}
		for _, e := range span[1:] {
			if !refusalStop(a) && refusalStop(e) {
				break
			}
			if e.Failure == a.Failure && stopCause(e) == stopCause(a) {
				return []store.LoopRun{a, e}, true, nil
			}
		}
		return nil, false, nil
	}
	// an agent's failure, or another step's, compares with the newest run before it that started: a build that failed
	// at a host stage before its agent (which the rule counts on its own stage) says nothing of the agent. An
	// investigation whose harness never started compares with the newest run before it, started or not.
	for _, b := range runs[1:] {
		if b.Started.IsZero() && !a.Started.IsZero() {
			continue
		}
		if b.Status == store.RunOK || a.Failure != b.Failure || a.Step != b.Step {
			return nil, false, nil
		}
		return []store.LoopRun{a, b}, true, nil
	}
	return nil, false, nil
}

// stopCounts says the stop rule counts a run: one that started and ended (a refusal of the budgets is no run, nor is
// one still running, nor a build that answered its fix is out of reach, whose case waits for a person), or one that
// failed at the host's hand before its agent started, which is kept at no cost and with no start, so nothing else
// bounds it: a build at a host stage (its inputs, a toolchain that is gone, a harness that does not start), or an
// investigation whose harness never started (run). A sandbox that exits before its harness starts, with nothing at
// the relay, is a harness that does not start (exitedBeforeStart): a build keeps it at build run, an investigation as
// run and a review as review run, so it stops on the stage alone, whatever its exit.
func stopCounts(run store.LoopRun) bool {
	switch {
	case run.Status == store.RunRefused || run.Status == store.RunRunning || run.Failure == "sandbox" || run.Status == store.RunOutOfReach:
		return false
	case run.Started.IsZero() && oldBrokerOutage(run):
		return false
	case run.Started.IsZero():
		// a build that never started counts at a host stage, or for a host error at a refusal stage (a capacity gate
		// that cannot run, before the agent); an investigation for the error that kept its harness from starting; a
		// review for a harness that does not start
		return run.Status == store.RunFailed && (run.Step == StepBuild && (slices.Contains(hostStages, run.Failure) || run.Cause == hostCause) ||
			run.Step == StepInvestigate && run.Failure == "run" || run.Step == StepReview && run.Failure == "review run")
	}
	return true
}

// oldBrokerText is how a run kept before the broker's outage had its own key (broker) told a broker env file that
// could not be read: a build's at build run, an investigation's as its error. The file's path, when it names one, may
// hold spaces.
var oldBrokerText = regexp.MustCompile(`no Bedrock route: the broker's env file (?:.* )?cannot be read`)

// oldBrokerOutage says a run that never started was kept, before the broker's outage had its own key, for a broker
// env file that could not be read: a build at build run, an investigation as run. This build keeps such a run as the
// broker's, which the stop rule does not count, so it does not count the old ones either.
func oldBrokerOutage(run store.LoopRun) bool {
	kept := run.Step == StepBuild && run.Failure == "build run" && run.Cause == "" || run.Step == StepInvestigate && run.Failure == "run"
	return kept && oldBrokerText.MatchString(run.Summary)
}

// runAt is when a run counts for the stop rule: its start, or its end for one that never started.
func runAt(run store.LoopRun) time.Time {
	if run.Started.IsZero() {
		return run.Ended
	}
	return run.Started
}

// stopSignature is the signature of the component-health case a stop files: by its step and its failure, and for a
// build refused for its own output by its cause too (causeKey), so the case names the refusal that repeated. A host
// stage's, and a host error's at a refusal stage, is its key alone, as every stop's was before causes were kept, so a
// case filed then still holds the stops it names.
func stopSignature(run store.LoopRun) string {
	sig := store.HealthFamily + "/" + detect.Versions[store.HealthFamily] + ":loop:" + run.Step + ":" + slugOf(run.Failure)
	if refusalStop(run) {
		sig += ":" + causeKey(run.Cause)
	}
	return sig
}

// stopText is how a stop's run failed, as the stop's why, its case's title and its evidence say it: a refusal's own
// cleaned text from its stage on, on one line, or its failure. The masked cause serves the signature and the match
// only.
func stopText(run store.LoopRun) string {
	if !refusalStop(run) {
		return run.Failure
	}
	if text := strings.Join(strings.Fields(refusalText(run)), " "); text != "" {
		return text
	}
	return run.Failure + ": " + run.Cause
}

// StoppedOn lists the cases the stop rule holds now on one of a component-health case's signatures: the working
// cases whose stop that case filed. Closing it as fixed resumes them (store.CloseFixed).
func StoppedOn(ctx context.Context, st *store.Store, health store.Case) ([]string, error) {
	stops, err := heldStops(ctx, st)
	if err != nil {
		return nil, err
	}
	return stoppedOn(stops, health), nil
}

// heldStop is a working case the stop rule holds now, and the signature of its stop (stopSignature).
type heldStop struct{ caseID, signature string }

// heldStops reads the stop rule once for every working case: the ones it holds now, most recently changed first.
func heldStops(ctx context.Context, st *store.Store) ([]heldStop, error) {
	cs, err := st.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseOpen, store.CaseInvestigating, store.CaseTesting}})
	if err != nil {
		return nil, err
	}
	var out []heldStop
	for _, c := range cs {
		runs, ok, err := stopRuns(ctx, st, c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, heldStop{c.ID, stopSignature(runs[0])})
		}
	}
	return out, nil
}

// stoppedOn lists, of the stops held now, the cases a component-health case holds on one of its signatures, other than itself.
func stoppedOn(stops []heldStop, health store.Case) []string {
	var out []string
	for _, s := range stops {
		if s.caseID != health.ID && slices.Contains(health.Signatures, s.signature) {
			out = append(out, s.caseID)
		}
	}
	return out
}

// sweepHealth closes the component-health cases unseen past their retention, which no detector's sweep reaches (the
// loop files them), but not one whose stop still holds a case (StoppedOn): closing it would never resume what it holds.
// It reads the stops only when a case is past its retention, and once for them all.
func (r *Runner) sweepHealth(ctx context.Context) ([]store.Case, error) {
	now := r.now()
	quiet, err := r.Store.QuietCases(ctx, now, store.CaseRetention, store.HealthFamily)
	if err != nil || len(quiet) == 0 {
		return nil, err
	}
	stops, err := heldStops(ctx, r.Store)
	if err != nil {
		return nil, err
	}
	var free []string
	for _, h := range quiet {
		if len(stoppedOn(stops, h)) == 0 {
			free = append(free, h.ID)
		}
	}
	return r.Store.SweepHealth(ctx, now, store.CaseRetention, "loop:detect", free)
}

// stopped applies the stop rule (stopRuns): a case whose last two runs failed the same way, in the same step, or whose
// build was refused twice for one cause, is not run again. It opens a case of the loop's own, in component health, the first time, with both runs'
// transcripts as its evidence, and says why. A stop whose older run began before that case closed as fixed resumes
// the case instead (store.ResumeLate). A dry check writes nothing.
func (r *Runner) stopped(ctx context.Context, c store.Case, dry bool) (string, error) {
	runs, ok, err := stopRuns(ctx, r.Store, c)
	if err != nil || !ok {
		return "", err
	}
	a, sig := runs[0], stopSignature(runs[0])
	why := fmt.Sprintf("its last two %s runs failed the same way: %s", a.Step, stopText(a))
	switch {
	case refusalStop(a):
		why = fmt.Sprintf("two of its %s runs were refused for one cause: %s", a.Step, stopText(a))
	case a.Step == StepBuild && hostFailure(a): // the host's, matched on its stage in the span, not only the last two
		why = fmt.Sprintf("two of its %s runs failed the same way: %s", a.Step, stopText(a))
	}
	// a stop whose older run began before its component-health case closed as fixed came too late for the close:
	// the case resumes as the close would have resumed it
	if dry {
		if _, _, late, err := r.Store.FixedStop(ctx, sig, runAt(runs[1])); err != nil || late {
			return "", err
		}
		return why, nil
	}
	if late, err := r.Store.ResumeLate(ctx, c.ID, sig, runAt(runs[1]), "loop"); err != nil || late {
		return "", err
	}
	// a stopped case waits for no room: its capacity note, if any, ends
	if err := r.endCapacityNote(ctx, c.ID, caseStopped); err != nil {
		r.logf("capacity note of %s: %v", c.ID, err)
	}
	f := store.Finding{Family: store.HealthFamily, Signature: sig,
		Title: plainClip("The loop fails the same way twice: "+stopText(a), store.TitleMax)}
	for _, run := range runs {
		ref := "file:" + filepath.Join(r.Dir, "runs", run.ID)
		if run.Dir != "" {
			ref = "file:" + filepath.Join(HostDir(run.Dir), "transcript.jsonl")
			if st, err := os.Stat(filepath.Join(HostDir(run.Dir), "transcript.fallback.jsonl")); err == nil && st.Mode().IsRegular() {
				ref = "file:" + filepath.Join(HostDir(run.Dir), "transcript.fallback.jsonl")
			}
		}
		f.Evidence = append(f.Evidence, store.Evidence{Ref: ref, At: runAt(run), Kind: "loop-run", Summary: run.Step + " " + run.Case + ": " + stopText(run)})
	}
	if _, _, err := r.Store.ObserveCase(ctx, f, "loop"); err != nil {
		return "", err
	}
	return why, nil
}

// reserve checks the budgets and records the run as started, so the next family's check counts it at its whole
// budget: the runner reserves each run in turn, before any of them starts, and two families' runs cannot both take
// the day's last room. A refusal is recorded as a refused run.
func (r *Runner) reserve(ctx context.Context, family, caseID, step string) (store.LoopRun, string) {
	run := store.LoopRun{ID: newRunID(r.now()), Case: caseID, Family: family, Step: step, Started: r.now(), Model: r.Agent.Model}
	return r.reservePrepared(ctx, run)
}

func (r *Runner) reservePrepared(ctx context.Context, run store.LoopRun) (store.LoopRun, string) {
	run.Started = r.now()
	why, err := r.affordable(ctx)
	if err != nil || why != "" {
		// a refusal names no directory, so what its preparation left is the sweep's to remove (sweepRuns)
		run.Status, run.Failure, run.Ended, run.Dir = store.RunRefused, firstOf(why, fmt.Sprint(err)), r.now(), ""
		r.Store.PutLoopRun(ctx, run)
		return run, run.Failure
	}
	// a refusal returns the run with the directory its row still names, if any: the caller keeps that one
	run.Dir, run.Status, run.USD = filepath.Join(r.Dir, "runs", run.ID), store.RunRunning, r.Budgets.Run
	if err := r.Store.PutLoopRun(ctx, run); err != nil {
		unwritten := run
		unwritten.Dir = ""
		return unwritten, err.Error()
	}
	stored, err := r.Store.LoopRunByID(ctx, run.ID)
	if err != nil {
		// the run never starts: its row ends unstarted and uncharged, naming no directory, before the caller removes
		// what it prepared
		ended := run
		ended.Started, ended.Ended, ended.Status, ended.Failure, ended.USD, ended.Dir = time.Time{}, r.now(), store.RunFailed,
			"reservation: "+err.Error(), 0, ""
		if endErr := r.Store.PutLoopRun(context.WithoutCancel(ctx), ended); endErr != nil {
			r.logf("run %s: its reservation could not be read (%v) or ended, so its directory stays: %v", run.ID, err, endErr)
			ended.Dir = run.Dir // the running row still names it
		}
		return ended, ended.Failure
	}
	run.CaseEpisode = stored.CaseEpisode
	return run, ""
}

func (r *Runner) step(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) Done {
	res, err := r.invoke(ctx, family, c, step, run)
	return r.completeStep(context.WithoutCancel(ctx), c, run, res, err)
}

func (r *Runner) completeStep(ctx context.Context, c store.Case, run store.LoopRun, res Result, err error) Done {
	if res.InputSummary == "" {
		res.InputSummary = run.Summary
	}
	recordErr := r.finish(ctx, c, &run, res, err)
	if run.Step == StepBuild && held(err) {
		stage := "ship"
		var staged *buildStageError
		if errors.As(err, &staged) {
			stage = strings.TrimPrefix(staged.key, "build ")
		}
		done := r.buildSkipped(ctx, c, stage, err)
		if heldBeforeAgent(res, err) && recordErr == nil {
			// a build held at the capacity gate before its agent leaves nothing once its reservation is released: its
			// tree goes back to the case, and its run's directories go, as an unreserved build's do. A release that
			// failed keeps them, the gate's wait marker among them, so endStale ends the reservation at no cost.
			r.removeRunDirs(ctx, c.ID, run.Dir, true)
		}
		return done
	}
	return Done{Family: c.Family, Case: c.ID, Step: run.Step, Run: run, Refused: res.Refused}
}

// finish records how a run ended, on the run and on its case, and returns the store's error when it could not.
func (r *Runner) finish(ctx context.Context, c store.Case, run *store.LoopRun, res Result, err error) error {
	reserved := run.USD
	if reserved == 0 {
		reserved = r.Budgets.Run
	}
	run.Ended = r.now()
	run.USD, run.Tokens, run.Turns = res.USD, res.Tokens, res.Turns
	run.Usage = store.Usage{Input: res.Usage.Input, Output: res.Usage.Output, CacheRead: res.Usage.CacheRead, CacheWrite: res.Usage.CacheWrite5 + res.Usage.CacheWrite1, CacheWrite1h: res.Usage.CacheWrite1}
	run.Model = firstOf(res.Model, run.Model)
	if err != nil {
		run.Status, run.Failure, run.Summary = store.RunFailed, "run", err.Error()
	} else {
		run.Status, run.Failure = res.Status, res.Failure
		run.Summary = summaryOf(res)
		if len(res.Refused) > 0 {
			// what the relay refused is shown with the run, beside its failure and never in it: the fallback and the stop
			// rule compare the failure
			run.Summary = strings.TrimSpace(run.Summary + " (the relay refused " + strings.Join(res.Refused, "; ") + ")")
		}
		if res.RelayAdmitted > 0 {
			run.Summary = strings.TrimSpace(run.Summary + fmt.Sprintf(" (relay: %d admitted, %d refused, %d charged, $%.6f)",
				res.RelayAdmitted, res.RelayRefused, res.RelayCharged, res.RelayUSD))
		}
	}
	switch {
	case res.CostKnown && res.RelayAdmitted+res.RelayRefused+res.RelayCharged > 0:
		run.USD = res.RelayUSD + res.ReservationUSD
	case !res.Started:
		run.Started = time.Time{}
		run.USD = 0 // it never started (its prompt could not be scrubbed, the harness did not start): it spent nothing
	case !res.CostKnown:
		run.USD = reserved // what it spent is unknown: its whole budget
	}
	cause := ""
	if err == nil && slices.Contains(refusalKeys, run.Failure) {
		cause = res.Cause
	}
	run.Summary = strings.TrimSpace(res.InputSummary + "\n" + run.Summary)
	for _, claims := range append(res.PriorHarness, res.Harness) {
		if claims.USD != nil || claims.Turns != nil {
			run.Summary += fmt.Sprintf(" (untrusted harness diagnostics: dollars %v, turns %v, usage %+v)",
				diagnosticNumber(claims.USD), diagnosticNumber(claims.Turns), claims.Usage)
		}
	}
	run.USD = max(run.USD, res.RelayUSD)
	// Summaries and printable diagnostics are generated text. Failure is a
	// normalized key, kept separate so cleaning cannot change the stop rule.
	// A refusal's cause is text too: it is cleaned, then masked into the key
	// the stop rule compares beside the failure.
	clean, cleanErr := Redact(ctx, run.Summary)
	if cleanErr == nil && cause != hostCause {
		cause, cleanErr = Redact(ctx, cause)
	}
	if cleanErr != nil {
		run.Summary = "(summary withheld: the cleaner failed)"
		run.Status, run.Failure, run.Cause = store.RunFailed, "scrub", ""
	} else {
		run.Summary, run.Cause = clean, refusalCause(cause)
	}
	if run.Step == StepInvestigate && res.Started && cleanErr == nil {
		why := undecided(res, err)
		if f, reported, ok := investigationFindings(res, run.Dir); ok || why != "" {
			if err := r.keepFindings(ctx, *run, f, reported, why); err != nil {
				run.Status, run.Failure, run.Summary = store.RunFailed, "findings", "(findings withheld: recording failed)"
				if errors.Is(err, ErrNoScrubber) {
					run.Failure = "scrub"
				}
			}
		}
	}

	var recordErr error
	switch {
	case run.Step == StepBuild && heldBeforeAgent(res, err):
		recordErr = r.Store.ReleaseLoopRun(ctx, run.ID)
	case run.Step == StepBuild && held(err):
		recordErr = r.Store.HoldLoopRun(ctx, *run)
	default:
		recordErr = r.Store.FinishLoopRun(ctx, *run)
	}
	if recordErr != nil {
		r.logf("record run %s: %v", run.ID, recordErr)
	}
	return recordErr
}

// keepFindings records an investigation's findings into its case, and logs why when it records nothing.
func (r *Runner) keepFindings(ctx context.Context, run store.LoopRun, f finding, reported bool, undecided string) error {
	if reported && f.Repo != "" && !repoName.MatchString(f.Repo) {
		return fmt.Errorf("invalid investigation repository name")
	}
	if r.Ship == nil || !r.Ship.ConfigTargets.Enabled {
		f.Config = "" // configuration targets are off: a report's config names nothing, and its repo stands
	}
	own, recorded, err := recordFindings(ctx, r.Store, run.Case, run.CaseEpisode, f, reported, undecided, "loop:"+run.ID)

	switch {
	case err != nil:
		r.logf("record the findings of %s: %v", run.Case, err)
	case !recorded && own.Into != "":
		r.logf("findings of %s not recorded: it went into %s", run.Case, own.Into)
	case !recorded && own.Episode != run.CaseEpisode:
		r.logf("findings of %s not recorded: its episode changed", run.Case)
	case !recorded:
		r.logf("findings of %s not recorded: it is %s now", run.Case, own.Status)
	}
	return err
}

// affordable says why no run may start now: the day's or the week's spend, with the runs in flight counted at
// their whole budget, would pass its limit. It is room for one run.
func (r *Runner) affordable(ctx context.Context) (string, error) {
	n, why, err := r.room(ctx, 1)
	if err != nil || n > 0 {
		return "", err
	}
	return why, nil
}

// room is how many runs, up to most, the day's and the week's budgets leave now (budgetNow), the smaller, and the
// budget that binds it, as affordable names it. Spend, the run's budget and each limit count in whole micro-dollars
// (runsThatFit), so the room is the number of reservations affordable lets through, one after another.
func (r *Runner) room(ctx context.Context, most int) (int, string, error) {
	now := r.budgetNow()
	n, why := -1, ""
	for _, lim := range []struct {
		name  string
		since time.Time
		usd   float64
	}{{"day", now.Add(-24 * time.Hour), r.Budgets.Day}, {"week", now.Add(-7 * 24 * time.Hour), r.Budgets.Week}} {
		spent, err := r.Store.LoopSpendMicros(ctx, lim.since, r.Budgets.Run)
		if err != nil {
			return 0, "", err
		}
		if left := runsThatFit(spent, store.Micros(r.Budgets.Run), store.Micros(lim.usd), most); n < 0 || left < n {
			n, why = left, fmt.Sprintf("the loop's %s budget: $%.2f spent of $%.0f", lim.name, float64(spent)/1e6, lim.usd)
		}
	}
	return n, why, nil
}

// runsThatFit is how many runs fit, up to most, when spent is spent of limit and each run counts run, all in whole
// micro-dollars (store.Micros): a run fits while the spend with it stays within the limit. The budget check and the
// room both count with it.
func runsThatFit(spent, run, limit int64, most int) int {
	n := 0
	for ; n < most && store.AddMicros(spent, run) <= limit; n++ {
		spent = store.AddMicros(spent, run)
	}
	return n
}

func (r *Runner) invoke(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) (Result, error) {
	if step == StepBuild {
		return r.build(ctx, c, run)
	}
	if step != StepInvestigate {
		return Result{}, fmt.Errorf("unknown step %s", step)
	}
	return r.investigate(ctx, c, run)
}

func summaryOf(res Result) string {
	if res.Failure != "" && res.Text != "" {
		return res.Text
	}
	var rep struct {
		Outcome   string `json:"outcome"`
		Summary   string `json:"summary"`
		BlockedBy string `json:"blocked_by"`
	}
	if json.Unmarshal(res.Report, &rep) == nil && rep.Summary != "" {
		if rep.Outcome == "" { // an investigation's report has no outcome: the runner moves its case
			return rep.Summary
		}
		s := rep.Outcome + ": " + rep.Summary
		if rep.BlockedBy != "" {
			s += " (blocked by " + rep.BlockedBy + ")"
		}
		return s
	}
	return res.Text
}

// newRunID is a run's id: its start and 4 random bytes.
func newRunID(now time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func slugOf(s string) string {
	w := slugWords(s)
	if len(w) > 8 {
		w = w[:8]
	}
	return strings.Join(w, "-")
}

// causeKey is a refusal's cause as its stop's signature names it: its words, lower case, joined by dashes and cut at
// a word to fit the case store's signature with the step and the failure, then a short hash of the whole kept cause,
// so two causes whose words are the same ("refused: a: b", "refused a b") or that differ past the cut stay two.
func causeKey(cause string) string {
	const room = 200
	slug := strings.Join(slugWords(cause), "-")
	if len(slug) > room {
		slug = slug[:room]
		if i := strings.LastIndexByte(slug, '-'); i > room/2 {
			slug = slug[:i]
		}
	}
	sum := sha256.Sum256([]byte(cause))
	return slug + "~" + hex.EncodeToString(sum[:4])
}

func slugWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
}

func plainClip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// caseRoom counts a case's running reservations as well as its charged cost, including runs started elsewhere
// and runs of a case merged into this one. Until recovery ends them, they also block work on the case.
func (r *Runner) caseRoom(ctx context.Context, c store.Case) (float64, bool, error) {
	live := ""
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Experiment: &live})
	if err != nil {
		return 0, false, err
	}
	spent, running := c.Cost.USD, false
	for _, run := range runs {
		if run.Status != store.RunRunning {
			continue
		}
		holder, err := r.Store.CaseLive(ctx, run.Case)
		if err != nil {
			return 0, false, err
		}
		if holder.ID != c.ID {
			continue
		}
		running = true
		cost := run.USD
		if cost == 0 {
			cost = r.Budgets.Run
		}
		spent += cost
	}
	return spent, running, nil
}
