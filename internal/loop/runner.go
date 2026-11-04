package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"path/filepath"

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
	}
	measured, err := r.Measure(ctx, o)
	if err != nil {
		return nil, err
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
	building := false
	for _, fam := range families {
		if ctx.Err() != nil { // the pass was told to stop (a signal): no run starts
			done = append(done, Done{Family: fam, Skipped: "stopped: " + ctx.Err().Error()})
			continue
		}
		c, step, why, err := r.pick(ctx, fam, o.Case, o.Dry)
		done = append(done, r.pickedSkips...)
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
		}
		if step == StepBuild && building {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "another case builds in this pass"})
			continue
		}
		if why, err := r.affordable(ctx); why != "" || err != nil {
			run, refusal := r.reserve(ctx, fam, c.ID, step)
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
			continue
		}
		if err := r.prepareInputs(ctx); err != nil {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "inputs: " + err.Error()})
			continue
		}
		run, inputErr := r.prepareRun(ctx, c, fam, step)
		if inputErr != nil {
			if step == StepBuild && held(inputErr) {
				done = append(done, r.buildSkipped(ctx, c, "prepare", inputErr))
				continue
			}
			if step == StepBuild {
				run, refusal := r.reservePrepared(ctx, run)
				if refusal != "" {
					done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
					continue
				}
				key := "build inputs"
				var staged *buildStageError
				if errors.As(inputErr, &staged) {
					key = staged.key
				}
				res := Result{Started: true, CostKnown: true, Status: store.RunFailed, Failure: key, Text: inputErr.Error()}
				r.finish(context.WithoutCancel(ctx), c, &run, res, nil)
				done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run})
			} else {
				done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "inputs: " + inputErr.Error()})
			}
			continue
		}
		run, refusal := r.reservePrepared(ctx, run)
		if refusal != "" {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Run: run, Skipped: refusal})
			continue
		}
		if step == StepBuild {
			building = true
		}
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
			r.logf("waiting packages: %v", err)
		}
		if err := r.lessons(ctx, o.Case, families); err != nil {
			return done, err
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Family+done[i].Step < done[j].Family+done[j].Step })
	return done, nil
}

func (r *Runner) buildSkipped(ctx context.Context, c store.Case, stage string, cause error) Done {
	if err := r.noteBuildSkip(ctx, c, stage, cause.Error()); err != nil {
		r.logf("build waiting note: %v", err)
	}
	return Done{Family: c.Family, Case: c.ID, Step: StepBuild, Skipped: cause.Error()}
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
	r.pickedSkips = nil
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
	if !dry {
		if err := r.releaseHolds(ctx, cs); err != nil {
			r.logf("hold notes: %v", err)
		}
	}
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
		if r.Ship != nil && (c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring) {
			p, ok, err := r.Ship.Load(c.ID)
			if err != nil {
				if noteErr := r.packageProblem(ctx, c, err, dry); noteErr != nil {
					return store.Case{}, "", "", noteErr
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
		step, rank := "", 0
		switch c.Status {
		case store.CaseTesting:
			step, rank = StepBuild, 3
			if r.Ship == nil {
				passed = append(passed, c.ID+" has no ship tool")
				continue
			}
			if p, ok, err := r.Ship.Load(c.ID); err != nil {
				if noteErr := r.packageProblem(ctx, c, err, dry); noteErr != nil {
					return store.Case{}, "", "", noteErr
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
				r.liftHold(ctx, c, dry)
				continue // its review or its landing comes next (reviews)
			} else if ok && !p.ProposalAt.IsZero() && p.Plan != nil {
				if wait := r.Ship.waiting(ctx, c, *p.Plan, r.now()); wait != nil {
					passed = append(passed, c.ID+"'s proposal waits on "+wait.what())
					r.liftHold(ctx, c, dry)
					continue
				}
			}
			if repo, err := r.caseRepo(ctx, c); err != nil {
				return store.Case{}, "", "", err
			} else if repo == "" {
				if !dry {
					r.keepHold(ctx, c, "waits for a repository: the investigation named none", true)
				}
				passed = append(passed, c.ID+" waits for a repository")
				continue
			} else if why, known := r.heldBuild(ctx, repo); why != "" {
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

// stopped applies the stop rule: a case whose last two live runs that ran (a refusal of the budgets is no run, and a run the
// host ended, stopped or killed, says nothing of the agent)
// failed the same way, in the same step, is not run a third time. It opens a case of the loop's own, in component
// health, the first time, with both runs' transcripts as its evidence, and says why. A dry check opens nothing.
func (r *Runner) stopped(ctx context.Context, c store.Case, dry bool) (string, error) {
	live := ""
	since := c.Back
	if !since.IsZero() {
		since = since.Add(time.Millisecond) // the kept clock is in milliseconds; only runs after the return count
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID, Experiment: &live, Ran: true, Judged: true, Since: since, Limit: 2})
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
	return r.reservePrepared(ctx, run)
}

func (r *Runner) reservePrepared(ctx context.Context, run store.LoopRun) (store.LoopRun, string) {
	run.Started = r.now()
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

func (r *Runner) step(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) Done {
	res, err := r.invoke(ctx, family, c, step, run)
	return r.completeStep(context.WithoutCancel(ctx), c, run, res, err)
}

func (r *Runner) completeStep(ctx context.Context, c store.Case, run store.LoopRun, res Result, err error) Done {
	if res.InputSummary == "" {
		res.InputSummary = run.Summary
	}
	r.finish(ctx, c, &run, res, err)
	if run.Step == StepBuild && held(err) {
		stage := "ship"
		var staged *buildStageError
		if errors.As(err, &staged) {
			stage = strings.TrimPrefix(staged.key, "build ")
		}
		return r.buildSkipped(ctx, c, stage, err)
	}
	return Done{Family: c.Family, Case: c.ID, Step: run.Step, Run: run, Refused: res.Refused}
}

// finish records how a run ended, on the run and on its case.
func (r *Runner) finish(ctx context.Context, c store.Case, run *store.LoopRun, res Result, err error) {
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
	clean, cleanErr := Redact(ctx, run.Summary)
	if cleanErr != nil {
		run.Summary = "(summary withheld: the cleaner failed)"
		run.Status, run.Failure = store.RunFailed, "scrub"
	} else {
		run.Summary = clean
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
	if run.Step == StepBuild && held(err) {
		recordErr = r.Store.HoldLoopRun(ctx, *run)
	} else {
		recordErr = r.Store.FinishLoopRun(ctx, *run)
	}
	if recordErr != nil {
		r.logf("record run %s: %v", run.ID, recordErr)
		return
	}
}

// keepFindings records an investigation's findings into its case, and logs why when it records nothing.
func (r *Runner) keepFindings(ctx context.Context, run store.LoopRun, f finding, reported bool, undecided string) error {
	if reported && f.Repo != "" && !repoName.MatchString(f.Repo) {
		return fmt.Errorf("invalid investigation repository name")
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
