package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/store"
)

// Steps a run takes a case through.
const (
	StepInvestigate = "investigate" // open or investigating: the native run; the runner records its report
	StepBuild       = "build"       // testing: prepare, then build and propose in the next run, or revise for its review
	StepReview      = "review"      // a proposed change: a clean-context review
	StepPublish     = "publish"     // closed as improved: the lesson to tacit
)

// Budgets are the loop's limits, in US dollars and time. The defaults come from the Phase 0 baseline: what the
// native path spent to answer a family once (kept on the machine). A case may not cost more than a native family
// answer, a run a fraction of it, and a week what the baseline's whole set of answers cost.
type Budgets struct {
	Run, Case, Day, Week float64
	RunTime              time.Duration
	MaxTurns             int
}

// DefaultBudgets: a run $4 and 30 minutes, at most 120 turns; a case $20; a day $40; a week $200.
var DefaultBudgets = Budgets{Run: 4, Case: 20, Day: 40, Week: 200, RunTime: 30 * time.Minute, MaxTurns: 120}

// Runner runs the loop: the detectors, then one step for each family's case in parallel, then the review and the
// landing of what was proposed.
type Runner struct {
	Store       *store.Store
	Agent       Agent
	Ship        *Ship
	Detectors   []detect.Detector
	Families    []string
	Budgets     Budgets
	Rules       []string // the machine's rules files
	Constraints []string
	ReadDirs    []string // the only repositories a run's evidence git reads (loop.read_dirs)
	Dir         string   // the loop's directory
	Home        string
	Bins        []string // the directories of momentum's tools, first on the path of what a run's door runs
	// Momentum and Tacit are the programs a run's door runs for its momentum and tacit commands: momentum's own
	// binary, and tacit as the path finds it.
	Momentum, Tacit string
	// Toolchains are what a build's sandbox reads to run its change's tests: the toolchains and their caches
	// (read-only), the loop's build caches (writable), their programs' directories and variables.
	Toolchains Toolchains
	Now        func() time.Time
	Log        func(format string, args ...any)
	// Post puts a review on its pull request, and returns where (the GitHub CLI by default).
	Post func(ctx context.Context, repo, pr, body string) (string, error)
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
	if !o.Dry {
		if err := r.endStale(ctx); err != nil {
			return nil, err
		}
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
		for _, c := range swept {
			r.logf("swept %s: %s", c.ID, c.Title)
		}
		r.dropLeft(ctx)
	}
	families := r.Families
	if len(o.Families) > 0 {
		families = o.Families
	}
	// every family's run is reserved before any starts, so the budgets count each in flight at its whole budget
	// whatever the others spend meanwhile; a pick that fails stops the reserving, and the runs reserved still run
	type reserved struct {
		fam, step string
		c         store.Case
		run       store.LoopRun
	}
	var (
		done   []Done
		starts []reserved
		picked error
	)
	building := false
	for _, fam := range families {
		c, step, why, err := r.pick(ctx, fam, o.Case, o.Dry)
		if err != nil {
			picked = err
			break
		}
		switch {
		case c.ID == "":
			done = append(done, Done{Family: fam, Skipped: why})
			continue
		case o.Dry:
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "dry: " + why})
			continue
		case step == StepBuild && building:
			// one build at a time, including preparation runs
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "another case builds in this run; the next run takes this one"})
			continue
		}
		run, refusal := r.reserve(ctx, fam, c.ID, step)
		if refusal != "" {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
			continue
		}
		building = building || step == StepBuild
		starts = append(starts, reserved{fam, step, c, run})
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
	if picked != nil {
		return done, picked
	}
	if !o.Dry {
		done = append(done, r.reviews(ctx, o.Case, families...)...)
		if err := r.noteWaiting(ctx); err != nil {
			r.logf("notes: %v", err)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Family+done[i].Step < done[j].Family+done[j].Step })
	return done, nil
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
			// a killed investigation leaves its findings file: the case keeps what it found, still investigating,
			// recorded before the run ends, as the runner records a run's
			if run.Step == StepInvestigate && run.Dir != "" {
				// Only the host's completed, authenticated result can conclude an investigation. Without it,
				// an unfinished run's findings file is still useful, but its stdout cannot move the case.
				res, streamErr := readHostResult(run.Dir)
				why := undecided(res, streamErr)
				if f, reported, ok := investigationFindings(res, run.Dir); ok || why != "" {
					r.keepFindings(ctx, run, f, reported, why)
				}
			}
			if err := r.Store.FinishLoopRun(ctx, run); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropLeft drops each change before its proposal whose case left testing (closed, sent back, merged into another,
// swept): nothing else in the loop returns to its host and run clones.
func (r *Runner) dropLeft(ctx context.Context) {
	if r.Ship == nil {
		return
	}
	entries, err := os.ReadDir(filepath.Join(r.Ship.Dir, "packages"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.logf("changes left by their cases: %v", err)
		}
		return
	}
	for _, e := range entries {
		p, ok, err := r.Ship.Load(e.Name())
		if !e.IsDir() || err != nil || !ok || !unproposed(p) {
			continue
		}
		c, err := r.Store.CaseByID(ctx, p.Case)
		if err != nil {
			r.logf("the change of %s: %v", p.Case, err)
			continue
		}
		if c.Status == store.CaseTesting && c.Into == "" {
			continue
		}
		why := "its case is " + string(c.Status)
		if c.Reason != "" {
			why += " (" + string(c.Reason) + ")"
		}
		if _, err := r.Ship.Drop(ctx, p.Case, why); err != nil {
			r.logf("drop the change of %s: %v", p.Case, err)
			continue
		}
		r.logf("dropped the change of %s: %s", p.Case, why)
	}
}

// pick chooses a family's case and its step: a case in testing first (finish what is in flight: its build, or its
// revision after a review), then one investigating, then an open one, the one with the most evidence first; and a
// case closed as improved whose lesson is not published. It passes over a case its runs stopped (the stop rule), one
// whose budget has no room for another run, and one whose change waits on its review or its measurement. why says
// what it chose or why none. A dry pick files nothing.
func (r *Runner) pick(ctx context.Context, family, only string, dry bool) (store.Case, string, string, error) {
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: family, All: true})
	if err != nil {
		return store.Case{}, "", "", err
	}
	type cand struct {
		c    store.Case
		step string
		rank int
	}
	var cands []cand
	var passed []string
	for _, c := range cs {
		if only != "" && c.ID != only {
			continue
		}
		spent, running, err := r.caseRoom(ctx, c)
		if err != nil {
			return store.Case{}, "", "", err
		}
		if running {
			passed = append(passed, c.ID+" has a run in flight")
			continue
		}
		if c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring {
			p, ok, err := r.Ship.Load(c.ID)
			if err != nil {
				return store.Case{}, "", "", err
			}
			if ok && (p.State == RolledBack || p.State == RollingBack) {
				if !dry {
					if _, err := r.Ship.RollBack(ctx, c.ID, "resume rollback"); err != nil {
						return store.Case{}, "", "", err
					}
				}
				passed = append(passed, c.ID+" has a rollback; its receipt precedes more work")
				continue
			}
		}
		step, rank := "", 0
		switch c.Status {
		case store.CaseTesting:
			step, rank = StepBuild, 3
			if p, ok, err := r.Ship.Load(c.ID); err != nil {
				return store.Case{}, "", "", err
			} else if ok && p.State == Landed {
				if !dry {
					if _, err := r.Store.DeployCase(ctx, c.ID, r.Ship.pkgDir(c.ID), p.LandedAt, p.PR+": "+p.Title, "loop"); err != nil {
						return store.Case{}, "", "", err
					}
				}
				passed = append(passed, c.ID+" has landed; deployment precedes more work")
				continue
			} else if ok && (p.State == Proposed || p.State == Landing) {
				if roundsUsed(p) {
					passed = append(passed, c.ID+" used its review rounds")
				}
				continue // its review or its landing comes next (reviews)
			}
		case store.CaseInvestigating:
			step, rank = StepInvestigate, 2
		case store.CaseOpen:
			step, rank = StepInvestigate, 1
		case store.CaseClosed:
			if c.Reason != store.Improved {
				continue
			}
			published, cooling, err := r.lessonState(ctx, c.ID)
			if err != nil {
				return store.Case{}, "", "", err
			}
			if published || cooling {
				continue
			}
			step, rank = StepPublish, 0
		default:
			continue // deployed or measuring: the measure tool's
		}
		if step != StepPublish && spent+r.Budgets.Run > r.Budgets.Case {
			passed = append(passed, fmt.Sprintf("%s spent $%.2f of its $%.0f, no room for a run", c.ID, spent, r.Budgets.Case))
			continue
		}
		if stop, err := r.stopped(ctx, c, dry); err != nil {
			return store.Case{}, "", "", err
		} else if stop != "" {
			passed = append(passed, c.ID+" stopped: "+stop)
			continue
		}
		cands = append(cands, cand{c, step, rank})
	}
	if len(cands) == 0 {
		why := "no case to work"
		if len(passed) > 0 {
			why += " (" + strings.Join(passed, "; ") + ")"
		}
		return store.Case{}, "", why, nil
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
	return cands[0].c, cands[0].step, fmt.Sprintf("%s %s (%s, %d evidence)", cands[0].step, cands[0].c.ID, cands[0].c.Status, cands[0].c.Evidence), nil
}

// LessonRetry is how long a lesson whose write left no receipt waits before a publish run tries it again.
const LessonRetry = 24 * time.Hour

// lessonState separates the durable receipt from the cooldown after reserving a lesson whose write failed.
func (r *Runner) lessonState(ctx context.Context, id string) (published, cooling bool, err error) {
	entries, err := r.Store.CaseLog(ctx, id)
	if err != nil {
		return false, false, err
	}
	for _, entry := range entries {
		published = published || entry.Action == store.LessonPublished
		cooling = cooling || entry.Action == store.LessonReserved && r.now().Sub(entry.At) < LessonRetry
	}
	return published, cooling, nil
}

// published requires the lesson command's durable receipt. A reservation alone did not publish.
func (r *Runner) published(ctx context.Context, id string) bool {
	published, _, err := r.lessonState(ctx, id)
	if err != nil {
		r.logf("read lesson receipt of %s: %v", id, err)
	}
	return published
}

// stopped applies the stop rule: a case whose last two live runs that ran (a refusal of the budgets is no run)
// failed the same way, in the same step, is not run a third time. It opens a case of the loop's own, in component
// health, the first time, with both runs' transcripts as its evidence, and says why. A dry check opens nothing.
func (r *Runner) stopped(ctx context.Context, c store.Case, dry bool) (string, error) {
	live := ""
	since := c.Back
	if !since.IsZero() {
		since = since.Add(time.Millisecond) // the kept clock is in milliseconds; only runs after the return count
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID, Experiment: &live, Ran: true, Since: since, Limit: 2})
	if err != nil {
		return "", err
	}
	if len(runs) < 2 {
		return "", nil
	}
	a, b := runs[0], runs[1]
	if a.Status == store.RunOK || b.Status == store.RunOK || a.Failure == "" || a.Failure != b.Failure || a.Step != b.Step {
		return "", nil
	}
	why := fmt.Sprintf("its last two %s runs failed the same way: %s", a.Step, a.Failure)
	if dry {
		return why, nil
	}
	f := store.Finding{Family: "component-health", Signature: "component-health/" + detect.Versions["component-health"] + ":loop:" + a.Step + ":" + slugOf(a.Failure),
		Title: plainClip("The loop fails the same way twice: "+a.Failure, store.TitleMax)}
	for _, run := range runs {
		ref := "file:" + filepath.Join(r.Dir, "runs", run.ID)
		if run.Dir != "" {
			ref = "file:" + filepath.Join(HostDir(run.Dir), "transcript.jsonl")
			if st, err := os.Stat(filepath.Join(HostDir(run.Dir), "transcript.fallback.jsonl")); err == nil && st.Mode().IsRegular() {
				ref = "file:" + filepath.Join(HostDir(run.Dir), "transcript.fallback.jsonl")
			}
		}
		f.Evidence = append(f.Evidence, store.Evidence{Ref: ref, At: run.Started, Kind: "loop-run", Summary: run.Step + " " + run.Case + ": " + run.Failure})
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
	why, err := r.affordable(ctx)
	if err != nil || why != "" {
		run.Status, run.Failure, run.Ended = store.RunRefused, firstOf(why, fmt.Sprint(err)), r.now()
		r.Store.PutLoopRun(ctx, run)
		return run, run.Failure
	}
	run.Dir, run.Status, run.USD = filepath.Join(r.Dir, "runs", run.ID), store.RunRunning, r.Budgets.Run
	if err := r.Store.PutLoopRun(ctx, run); err != nil {
		return run, err.Error()
	}
	stored, err := r.Store.LoopRunByID(ctx, run.ID)
	if err != nil {
		return run, err.Error()
	}
	run.CaseEpisode = stored.CaseEpisode
	return run, ""
}

// step runs one step of a case on its reserved run: the agent, then its cost recorded on the run and the case. A
// run that ended with no cost of its own known (killed, timed out, crashed before its result) counts its whole
// budget.
func (r *Runner) step(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) Done {
	d := Done{Family: family, Case: c.ID, Step: step}
	began := "" // a build that resumes a change before its proposal
	if step == StepBuild && r.Ship != nil {
		if p, ok, _ := r.Ship.Load(c.ID); ok && unproposed(p) {
			began = p.State
		}
	}
	res, err := r.invoke(ctx, family, c, step, run)
	// one that ended well with its change where it began, and its case still in testing, moved nothing: it is a stop,
	// so two in a row trip the stop rule before the case's budget is gone
	if began != "" && err == nil && res.Status == store.RunOK && r.idle(ctx, c.ID, began) {
		res.Status, res.Failure = store.RunFailed, "no progress"
	}
	r.finish(ctx, c, &run, res, err)
	d.Run, d.Refused = run, res.Refused
	return d
}

// idle says whether a build left its case in testing and its change before its proposal, in the state it began in.
func (r *Runner) idle(ctx context.Context, caseID, began string) bool {
	c, err := r.Store.CaseByID(ctx, caseID)
	if err != nil || c.Status != store.CaseTesting || c.Into != "" {
		return false
	}
	p, ok, err := r.Ship.Load(caseID)
	return err == nil && ok && unproposed(p) && p.State == began
}

// finish records how a run ended, on the run and on its case.
func (r *Runner) finish(ctx context.Context, c store.Case, run *store.LoopRun, res Result, err error) {
	reserved := run.USD
	if reserved == 0 {
		reserved = r.Budgets.Run
	}
	run.Ended = r.now()
	run.USD, run.Tokens, run.Usage, run.Turns = res.USD, res.Tokens, res.Usage, res.Turns
	run.Model = firstOf(res.Model, run.Model)
	if err != nil {
		run.Status, run.Failure, run.Summary = store.RunFailed, "run", err.Error()
	} else {
		run.Status, run.Failure = res.Status, res.Failure
		if run.Summary == "" {
			run.Summary = summaryOf(res)
		}
		if len(res.Refused) > 0 {
			// what the relay refused is shown with the run, beside its failure and never in it: the fallback and the stop
			// rule compare the failure
			run.Summary = strings.TrimSpace(run.Summary + " (the relay refused " + strings.Join(res.Refused, "; ") + ")")
		}
		if res.RelayAdmitted > 0 {
			run.Summary = strings.TrimSpace(run.Summary + fmt.Sprintf(" (relay: %d admitted, %d refused, %d charged, $%.6f)",
				res.RelayAdmitted, res.RelayRefused, res.RelayCharged, res.RelayUSD))
		}
		if run.Status == store.RunOK && run.Step == StepPublish && !r.published(ctx, c.ID) {
			// a publish is done by its receipt, never by the run ending well: one that left none failed, and the stop rule
			// stops a case whose publishes keep leaving none
			run.Status, run.Failure = store.RunFailed, "no receipt"
		}
	}
	switch {
	case !res.Started:
		run.USD = 0 // it never started (its prompt could not be scrubbed, the harness did not start): it spent nothing
		run.Started = time.Time{}
	case !res.CostKnown:
		run.USD = reserved // what it spent is unknown: its whole budget
	}
	run.USD = max(run.USD, res.RelayUSD)
	// Summaries and printable diagnostics are generated text. Failure is a
	// normalized key, kept separate so cleaning cannot change the stop rule.
	clean, cleanErr := Redact(ctx, run.Summary)
	if cleanErr != nil {
		run.Summary = "(summary withheld: the cleaner failed)"
		run.Status, run.Failure = store.RunFailed, "scrub"
	} else {
		run.Summary = clean
	}
	root := ""
	if err == nil && run.Status == store.RunOK && run.Step != StepReview && run.Step != StepInvestigate {
		var report struct {
			Root string `json:"root"`
		}
		if json.Unmarshal(res.Report, &report) == nil && strings.HasPrefix(report.Root, "root:") {
			root, cleanErr = Redact(ctx, report.Root)
			if cleanErr != nil || !strings.HasPrefix(root, "root:") {
				root, run.Summary = "", "(reported root withheld: the cleaner failed)"
				run.Status, run.Failure = store.RunFailed, "scrub"
			}
		}
	}
	// The runner keeps an investigation's case: what the run found goes into it, deterministically, from its report
	// or, when it was cut short, from the findings it kept as it went, whatever error ended the run. It is recorded
	// before the run ends, so a loop that dies between the two leaves a running run, which stale recovery ends and
	// records again under the same guard.
	if run.Step == StepInvestigate && res.Started && cleanErr == nil {
		// one that ended well with a report short of a mechanism or a fix did not decide: its case closes as
		// insufficient evidence, and comes back once as much evidence again comes in (Cases)
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
	if err := r.Store.FinishLoopRun(ctx, *run); err != nil {
		r.logf("record run %s: %v", run.ID, err)
		return
	}
	// A report cannot choose another case to change. Investigations record only their findings, and an open
	// proposal keeps its case while its review decides it.
	if root != "" {
		if state, open := r.Ship.ChangeOpen(run.Case); open {
			r.logf("root of %s ignored: its change is %s on its pull request", run.Case, state)
		} else if own, recorded, err := r.Store.RecordRoot(ctx, run.Case, root, "loop"); err != nil {
			r.logf("record root of %s: %v", run.Case, err)
		} else if !recorded {
			r.logf("root of %s ignored: its case went into %s", run.Case, own.Into)
		}
	}
}

// keepFindings records an investigation's findings into its case, and logs why when it records nothing.
func (r *Runner) keepFindings(ctx context.Context, run store.LoopRun, f finding, reported bool, undecided string) error {
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
// their whole budget, would pass its limit.
func (r *Runner) affordable(ctx context.Context) (string, error) {
	now := r.now()
	for _, lim := range []struct {
		name  string
		since time.Time
		usd   float64
	}{{"day", now.Add(-24 * time.Hour), r.Budgets.Day}, {"week", now.Add(-7 * 24 * time.Hour), r.Budgets.Week}} {
		spent, err := r.Store.LoopSpend(ctx, lim.since, r.Budgets.Run)
		if err != nil {
			return "", err
		}
		if spent+r.Budgets.Run > lim.usd {
			return fmt.Sprintf("the loop's %s budget: $%.2f spent of $%.0f", lim.name, spent, lim.usd), nil
		}
	}
	return "", nil
}

// invoke runs the agent for a step, in its sandbox: its own directory, a prepared build's clone and the toolchains its
// tests need, and its step's commands through its door.
func (r *Runner) invoke(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) (Result, error) {
	if step == StepInvestigate {
		return r.investigate(ctx, c, run)
	}
	system, err := systemPrompt(step, family, r.Rules, r.Constraints)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return Result{}, err
	}
	inv := Invocation{Dir: run.Dir, System: system, Schema: runSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: true}
	inv.Private = []string{filepath.Join(r.Ship.Dir, "host"), filepath.Join(r.Ship.Dir, "packages")}
	inv.Prompt = fmt.Sprintf("Case %s (%s, %s): %s\nStep: %s. Read the case with `momentum case show %s --json`, then take this step as the "+
		"loop's procedure and the %s skill say. Your working directory is %s.", c.ID, c.Family, c.Status, c.Title, step, c.ID, family, run.Dir)
	roots := []string{run.Dir}
	if step == StepBuild {
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil {
			return Result{}, err
		}
		if ok && p.State != RolledBack && p.State != Dropped && (p.Host == "" || p.BaseCommit == "") {
			return Result{}, refused("legacy package has no independent host clone; prepare a fresh package before building")
		}
		if ok && p.RefreshBase {
			p, err = r.Ship.refreshBase(ctx, p)
			if err != nil {
				return Result{}, err
			}
			previous, err := os.ReadFile(filepath.Join(r.Ship.pkgDir(c.ID), "change.patch"))
			if err != nil {
				return Result{}, err
			}
			inv.Files = map[string][]byte{"previous.patch": previous}
			inv.Prompt += "\nThe default branch moved. Start from /run/momentum/input/previous.patch, which is read-only. Adapt it to the new base."
		}
		if !ok || p.State == RolledBack || p.State == Dropped {
			inv.Prompt += "\nNo clone is mounted yet. Choose a repository with the evidence commands and call momentum ship prepare --repo DIR. End this run after preparation with the report; the next build mounts the complete clone. Preparation counts toward all budgets and the stop rule."
		} else {
			roots = append(roots, p.Work)
			if err := r.Toolchains.makeCaches(); err != nil {
				return Result{}, err
			}
			inv.Mounts = append([]jail.Mount{{Path: p.Work, Write: true, OwnRepository: true}}, r.Toolchains.mounts()...)
			inv.Env = r.Toolchains.env()
			inv.Prompt += fmt.Sprintf("\nYour independent clone is %s. BASE is %s. Host repositories and the host clone are absent. Use git inside your sandbox for history. Finish with git add -A && git diff --cached --binary %s > $MOMENTUM_LOOP_DIR/change.patch, then ship check and ship propose. Every revision is cumulative against BASE.", p.Work, p.BaseCommit, p.BaseCommit)
		}
		_, inProgress := r.Ship.ChangeOpen(c.ID)
		switch {
		case ok && (p.State == Revising || p.State == Checked) && len(p.Reviews) > 0:
			// A checked revision still answers its last review before it is proposed again.
			last := p.Reviews[len(p.Reviews)-1]
			text, err := Redact(ctx, last.Text)
			if err != nil {
				return Result{}, err
			}
			if inv.Files == nil {
				inv.Files = map[string][]byte{}
			}
			inv.Files["review.md"] = []byte(text)
			inv.Prompt += fmt.Sprintf("\nIts change %s was reviewed: round %d found %d high, %d medium and %d low, in review.md. Revise the change "+
				"in its independent clone %s for what the review found (read /run/momentum/input/review.md), check it again (`momentum ship check`) and propose it again (`momentum ship "+
				"propose`), which updates its pull request.", p.PR, last.Round, last.High, last.Medium, last.Low, p.Work)
		case ok && (inProgress || unproposed(p)):
			inv.Prompt += fmt.Sprintf("\nIts change is %s, in its independent clone %s: resume it from there (`momentum ship status` says where it "+
				"stands); its findings were checked when the change began.", p.State, p.Work)
			if !inProgress {
				inv.Prompt += " If it cannot go on (its test does not tell the change apart, its plan cannot count the problem), close the " +
					"case or send it back: the loop drops a change whose case left testing."
			}
		}
	}
	inv.Door = r.door(step, c, run, roots)
	// the prompt holds the case's title, which a detector made of what it read: it reaches the run scrubbed
	if inv.Prompt, err = Redact(ctx, inv.Prompt); err != nil {
		return Result{}, err
	}
	agent := r.Agent
	agent.Env = map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID, "MOMENTUM_LOOP_DIR": run.Dir}
	return agent.RunOrFallback(ctx, inv)
}

// runSchema is the report every run ends with.
const runSchema = `{"type":"object","additionalProperties":false,"required":["case","step","outcome","summary"],
"properties":{"case":{"type":"string"},"step":{"type":"string"},
"outcome":{"type":"string","enum":["investigated","testing","closed","proposed","published","blocked","nothing"]},
"mechanism":{"type":"string"},"root":{"type":"string"},"summary":{"type":"string"},"blocked_by":{"type":"string"}}}`

// reviewSchema is a review run's report: its findings, which its counts are made from.
const reviewSchema = `{"type":"object","additionalProperties":false,"required":["findings","summary"],
"properties":{"summary":{"type":"string"},
"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["severity","where","problem","fix"],
"properties":{"severity":{"type":"string","enum":["high","medium","low"]},"where":{"type":"string"},"problem":{"type":"string"},"fix":{"type":"string"}}}}}}`

// reviews runs the clean-context review of each proposed change that has none on its head, posts it, and lands
// the change when it is clean and the loop is live. A review run is a run: the stop rule, the case's budget and
// the loop's budgets hold it as they hold any other.
func (r *Runner) reviews(ctx context.Context, only string, families ...string) []Done {
	var out []Done
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseTesting}})
	if err != nil {
		r.logf("reviews: %v", err)
		return nil
	}
	for _, c := range cs {
		if len(families) > 0 && !slices.Contains(families, c.Family) {
			continue
		}
		if only != "" && c.ID != only {
			continue
		}
		spent, running, err := r.caseRoom(ctx, c)
		if err != nil || running {
			why := "a run is in flight"
			if err != nil {
				why = "cannot read the case's reservations: " + err.Error()
			}
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: why})
			continue
		}
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil || !ok || (p.State != Proposed && p.State != Landing) {
			continue
		}
		if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head && p.Reviews[n-1].Comment == "" {
			var err error
			p, err = r.postReview(ctx, p)
			if err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: err.Error()})
				continue
			}
		}
		if p.Clean() {
			if landed, err := r.Ship.Land(ctx, c.ID); err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: err.Error()})
				action := "not landed"
				if landed.State == Landed && landed.Cleanup != "" {
					action = "cleanup refused"
				}
				r.Store.CaseNote(store.WithTextCleaner(ctx, Redact), c.ID, action, err.Error(), "loop")
			} else {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land"})
			}
			continue
		}
		if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head || n >= MaxRounds {
			continue // reviewed on its head: its findings go back to the build, or its rounds are used
		}
		if spent+r.Budgets.Run > r.Budgets.Case {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: fmt.Sprintf("spent $%.2f of its $%.0f, no room for a review", spent, r.Budgets.Case)})
			continue
		}
		if stop, err := r.stopped(ctx, c, false); err != nil || stop != "" {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: firstOf(stop, fmt.Sprint(err))})
			continue
		}
		run, refusal := r.reserve(ctx, c.Family, c.ID, StepReview)
		if refusal != "" {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Run: run, Skipped: refusal})
			continue
		}
		out = append(out, r.review(ctx, c, p, run))
	}
	return out
}

// review runs one review round of a proposed change on its reserved run, and posts it (scrubbed) on the pull
// request: a review that is not posted lands nothing.
func (r *Runner) review(ctx context.Context, c store.Case, p Package, run store.LoopRun) Done {
	d := Done{Family: c.Family, Case: c.ID, Step: StepReview}
	res, err := r.invokeReview(ctx, c, p, run)
	if err == nil && res.Status == store.RunOK {
		var rep struct {
			Summary  *string    `json:"summary"`
			Findings *[]Finding `json:"findings"`
		}
		if json.Unmarshal(res.Report, &rep) != nil || rep.Summary == nil || rep.Findings == nil {
			res.Status, res.Failure = store.RunFailed, "an unreadable review"
		} else if h, m, l, cerr := countFindings(*rep.Findings); cerr != nil {
			res.Status, res.Failure = store.RunFailed, "report"
		} else {
			text := reviewText(len(p.Reviews)+1, h, m, l, *rep.Summary, *rep.Findings)
			rev := Review{Findings: *rep.Findings, Text: text}
			saved, saveErr := r.Ship.Reviewed(ctx, c.ID, rev)
			if saveErr != nil {
				res.Status, res.Failure = store.RunFailed, "review"
			} else if _, postErr := r.postReview(ctx, saved); postErr != nil {
				r.logf("post the review of %s: %v", c.ID, postErr)
			}

			run.Summary = fmt.Sprintf("%d high, %d medium, %d low: %s", h, m, l, *rep.Summary)
		}
	}
	r.finish(ctx, c, &run, res, err)
	d.Run, d.Refused = run, res.Refused
	return d
}

func (r *Runner) invokeReview(ctx context.Context, c store.Case, p Package, run store.LoopRun) (Result, error) {
	system, err := systemPrompt(StepReview, c.Family, r.Rules, r.Constraints)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return Result{}, err
	}
	diff, err := r.reviewDiff(ctx, p)
	if err != nil {
		return Result{}, err
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	pkg, err := Redact(ctx, string(raw))
	if err != nil {
		return Result{}, err
	}
	if !json.Valid([]byte(pkg)) {
		pkg = `{"withheld": "the package's record did not stay valid once scrubbed"}`
	}
	inv := Invocation{Dir: run.Dir, System: system, Schema: reviewSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: true, Files: map[string][]byte{"diff.patch": []byte(diff), "package.json": []byte(pkg)}}
	inv.Private = []string{filepath.Join(r.Ship.Dir, "host"), filepath.Join(r.Ship.Dir, "packages")}
	export, err := r.Ship.export(ctx, p, "review", p.Head)
	if err != nil {
		return Result{}, err
	}
	defer r.Ship.unexport(p, export)
	pin, err := os.Open(export)
	if err != nil {
		return Result{}, err
	}
	defer pin.Close()
	roots := []string{run.Dir}
	inv.Mounts = []jail.Mount{{Path: export, File: pin}}
	inv.Prompt = fmt.Sprintf("Review case %s (%s): %s. Read /run/momentum/input/diff.patch and /run/momentum/input/package.json. Your working directory is %s. The proposed tree is exported read-only at %s with no .git. Use momentum evidence git only for repositories named by loop.read_dirs. Read momentum case show %s --json. End with the review report.", c.ID, c.Family, p.Title, run.Dir, export, c.ID)
	inv.Door = r.door(StepReview, c, run, roots)
	if inv.Prompt, err = Redact(ctx, inv.Prompt); err != nil {
		return Result{}, err
	}
	agent := r.Agent
	agent.Env = map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID, "MOMENTUM_LOOP_DIR": run.Dir}
	return agent.RunOrFallback(ctx, inv)
}

// reviewText is a review as it is posted: its round, its counts (the findings'), and each finding.
func reviewText(round, high, medium, low int, summary string, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Clean-context review, round %d\n\n", round)
	verdict := "Ready to merge"
	if high > 0 || medium > 0 {
		verdict = "Not ready to merge"
	}
	fmt.Fprintf(&b, "%s: %d High, %d Medium, %d Low.\n\n%s\n", verdict, high, medium, low, strings.TrimSpace(summary))
	n := map[string]int{}
	for _, f := range findings {
		tag := strings.ToUpper(f.Severity[:1])
		n[tag]++
		fmt.Fprintf(&b, "\n**%s%d.** `%s`: %s Fix: %s\n", tag, n[tag], f.Where, strings.TrimSpace(f.Problem), strings.TrimSpace(f.Fix))
	}
	b.WriteString("\nPosted by momentum's improvement loop.\n")
	return b.String()
}

func summaryOf(res Result) string {
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
	w := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	if len(w) > 8 {
		w = w[:8]
	}
	return strings.Join(w, "-")
}

func plainClip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// postReview retries the saved review, without another model invocation or review round.
func (r *Runner) postReview(ctx context.Context, p Package) (Package, error) {
	n := len(p.Reviews) - 1
	if n < 0 || p.Reviews[n].Comment != "" {
		return p, nil
	}
	if r.Post == nil || p.PR == "" {
		return p, fmt.Errorf("review posting is unavailable")
	}
	pub, err := r.Ship.public(ctx, p.Reviews[n].Text)
	if err != nil {
		return p, err
	}
	t, err := r.Ship.Gate.Repository(ctx, p.Dir)
	if err != nil {
		return p, err
	}
	if !strings.EqualFold(t.Repo, p.Repo) {
		return p, refused("the review destination is not the package's repository")
	}
	url, err := r.Post(ctx, p.Repo, p.PR, pub)
	if err != nil {
		return p, err
	}
	if url == "" {
		return p, fmt.Errorf("review post returned no receipt")
	}
	p.Reviews[n].Text, p.Reviews[n].Comment = pub, url
	if (p.Reviews[n].High > 0 || p.Reviews[n].Medium > 0) && len(p.Reviews) < MaxRounds {
		p.State = Revising
	}
	return p, r.Ship.save(&p)
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

// reviewDiff is the change as its review reads it, scrubbed: what the change's branch holds that its base does not.
func (r *Runner) reviewDiff(ctx context.Context, p Package) (string, error) {
	diff, err := r.Ship.git(ctx, p.Host, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", p.BaseCommit, p.Head)
	if err != nil {
		return "", err
	}
	return Redact(ctx, diff)
}

// roundsUsed says whether a proposed change used its review rounds: the last of them found something on the commit it
// proposes now, and no round is left. Nothing builds or reviews it more.
func roundsUsed(p Package) bool {
	n := len(p.Reviews)
	return (p.State == Proposed || p.State == Landing) && n >= MaxRounds && !p.Clean() && p.Reviews[n-1].Head == p.Head
}

// waitingPrefix starts the id of the report's note about a change that waits for a person.
const waitingPrefix = "loop-rounds:"

// noteWaiting keeps the report's passive note about each change that used its review rounds, since the loop builds
// and reviews it no more and nothing else says it waits. One note for each case, keyed by it, so a loop run that finds
// the change still waiting replaces the note and adds none: a low problem, put when the change reaches that state and
// done when it leaves it (the case closes, the change is revised or lands). A note its reader resolved while the
// change still waits stays resolved. Nothing pages anyone: the report is the owner's passive surface.
func (r *Runner) noteWaiting(ctx context.Context) error {
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseTesting}})
	if err != nil {
		return err
	}
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	have := map[string]store.Note{}
	for _, n := range notes {
		have[n.ID] = n
	}
	waiting := map[string]bool{}
	var unread []error
	for _, c := range cs {
		id := waitingPrefix + store.IDPart(c.ID)
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil {
			waiting[id] = true // unread is not proof that the case left its waiting state
			unread = append(unread, fmt.Errorf("case %s package: %w", c.ID, err))
			continue
		}
		if !ok || !roundsUsed(p) {
			continue
		}
		waiting[id] = true
		at := p.Reviews[len(p.Reviews)-1].At
		if n, ok := have[id]; ok && (n.Done.IsZero() || !n.At.Before(at.Truncate(time.Millisecond))) {
			continue // it stands, or it was resolved while this change waited
		}
		note := store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: at,
			Title: fmt.Sprintf("The loop's change for case %s used its %d review rounds and waits for a person", c.ID, MaxRounds),
			Detail: fmt.Sprintf("The last review still found a high or medium problem in %s. The loop builds and reviews this change no more. "+
				"Take it over in its pull request, or close the case.", firstOf(p.PR, "its change"))}
		if u, err := url.Parse(p.PR); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			note.Links = []store.Link{{Label: "pull request", URL: p.PR}}
		}
		if _, err := r.Store.PutNote(ctx, note); err != nil {
			return err
		}
	}
	for id, n := range have {
		if strings.HasPrefix(id, waitingPrefix) && n.Done.IsZero() && !waiting[id] {
			if _, _, err := r.Store.DoneNote(ctx, id, r.now()); err != nil {
				return err
			}
		}
	}
	return errors.Join(unread...)
}
