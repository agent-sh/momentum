package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/store"
)

// Steps a run takes a case through.
const (
	StepInvestigate = "investigate" // open or investigating: find the mechanism, decide
	StepBuild       = "build"       // testing: build and propose the change, or revise it for its review
	StepReview      = "review"      // a proposed change: a clean-context review
	StepPublish     = "publish"     // closed as improved: the lesson to tacit
	StepMeasure     = "measure"     // deployed, measuring or improved: the measure step, no agent (Measure)
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
	MCP         string   // for build runs: the servers its prior-art search uses
	ReadDirs    []string // the directories an investigation's file tools reach besides its own (loop.read_dirs)
	Dir         string   // the loop's directory
	Home        string
	Bins        []string // the directories of momentum's tools, first on a run's PATH
	Now         func() time.Time
	Log         func(format string, args ...any)
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
	Skipped            string // why no step ran
	Did                string // what a step that is no agent's run did: the measure step's outcome
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
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
		effect := ""
		if m.Result != nil {
			effect = m.Result.Effect
		}
		for _, s := range []string{effect, m.Why} {
			if s != "" && !strings.Contains(did, s) {
				did += ": " + s
			}
		}
		done = append(done, Done{Case: m.Case, Step: StepMeasure, Did: did})
	}
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
			// one build at a time: a build leaves its work tree changed, which the scope check of another reads as work
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
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Family+done[i].Step < done[j].Family+done[j].Step })
	return done, nil
}

// endStale ends the runs that started longer ago than any run may last and never ended: killed with the process
// that ran them. What such a run spent is unknown, so it counts its whole budget, and its scope is stopped with
// whatever it left running.
func (r *Runner) endStale(ctx context.Context) error {
	live := "" // an evaluation's runs are its own: their cases are in their trials' stores, not this one
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Experiment: &live})
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == store.RunRunning && r.now().Sub(run.Started) > r.Budgets.RunTime+15*time.Minute {
			run.Status, run.Failure, run.Ended = store.RunFailed, "killed", r.now()
			if run.USD == 0 {
				run.USD = r.Budgets.Run
			}
			if r.Agent.Scope && run.Dir != "" {
				StopScope(ScopeUnit(run.Dir))
			}
			if err := r.Store.FinishLoopRun(ctx, run); err != nil {
				return err
			}
		}
	}
	return nil
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
				if n := len(p.Reviews); n >= MaxRounds && !p.Clean() && p.Reviews[n-1].Head == p.Head {
					passed = append(passed, c.ID+" used its review rounds")
				}
				continue // its review or its landing comes next (reviews)
			}
		case store.CaseInvestigating:
			step, rank = StepInvestigate, 2
		case store.CaseOpen:
			step, rank = StepInvestigate, 1
		case store.CaseClosed:
			if c.Reason != store.Improved || r.published(ctx, c.ID) {
				continue
			}
			step, rank = StepPublish, 4
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

// published says whether a case's publish step is done for now: the lesson command left its receipt on the case
// (never the harness's outcome: a run that ended well may have published nothing), or it reserved the lesson
// within the last LessonRetry and tacit's write did not go through, which waits that long before it is tried again.
func (r *Runner) published(ctx context.Context, id string) bool {
	entries, err := r.Store.CaseLog(ctx, id)
	if err != nil {
		return true // unread: do not publish twice
	}
	for _, entry := range entries {
		if entry.Action == store.LessonPublished || entry.Action == store.LessonReserved && r.now().Sub(entry.At) < LessonRetry {
			return true
		}
	}
	return false
}

// stopped applies the stop rule: a case whose last two live runs that ran (a refusal of the budgets is no run)
// failed the same way, in the same step, is not run a third time. It opens a case of the loop's own, in component
// health, the first time, with both runs' transcripts as its evidence, and says why. A dry check opens nothing.
func (r *Runner) stopped(ctx context.Context, c store.Case, dry bool) (string, error) {
	live := ""
	all, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID, Experiment: &live, Limit: 20})
	if err != nil {
		return "", err
	}
	var runs []store.LoopRun
	for _, run := range all {
		if run.Status != store.RunRefused && run.Status != store.RunRunning && len(runs) < 2 {
			runs = append(runs, run)
		}
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
			ref = "file:" + filepath.Join(run.Dir, "transcript.jsonl")
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
	return run, ""
}

// step runs one step of a case on its reserved run: the agent, then its cost recorded on the run and the case. A
// run that ended with no cost of its own known (killed, timed out, crashed before its result) counts its whole
// budget.
func (r *Runner) step(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) Done {
	d := Done{Family: family, Case: c.ID, Step: step}
	res, err := r.invoke(ctx, family, c, step, run)
	r.finish(ctx, c, &run, res, err)
	d.Run = run
	return d
}

// finish records how a run ended, on the run and on its case.
func (r *Runner) finish(ctx context.Context, c store.Case, run *store.LoopRun, res Result, err error) {
	reserved := run.USD
	if reserved == 0 {
		reserved = r.Budgets.Run
	}
	run.Ended = r.now()
	if err != nil {
		run.Status, run.Failure = store.RunFailed, err.Error()
	} else {
		run.Status, run.Failure, run.USD, run.Tokens, run.Turns = res.Status, res.Failure, res.USD, res.Tokens, res.Turns
		run.Model = firstOf(res.Model, run.Model)
		if run.Summary == "" {
			run.Summary = summaryOf(res)
		}
	}
	switch {
	case !res.CostKnown && err != nil && res.Transcript == "":
		run.USD = 0 // it never started (its prompt could not be scrubbed, the harness did not start): it spent nothing
	case !res.CostKnown:
		run.USD = reserved // what it spent is unknown: its whole budget
	}
	if err := r.Store.FinishLoopRun(ctx, *run); err != nil {
		r.logf("record run %s: %v", run.ID, err)
		return
	}
	// A report cannot choose another case to change. Its root is applied to this run's case, after it ends.
	if err == nil && res.Status == store.RunOK && run.Step != StepReview {
		var report struct {
			Root string `json:"root"`
		}
		if json.Unmarshal(res.Report, &report) == nil && strings.HasPrefix(report.Root, "root:") {
			if own, recorded, err := r.Store.RecordRoot(ctx, run.Case, report.Root, "loop"); err != nil {
				r.logf("record root of %s: %v", run.Case, err)
			} else if !recorded {
				r.logf("root of %s ignored: its case went into %s", run.Case, own.Into)
			}
		}
	}
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

// invoke runs the agent for a step.
func (r *Runner) invoke(ctx context.Context, family string, c store.Case, step string, run store.LoopRun) (Result, error) {
	system, err := systemPrompt(step, family, r.Rules, r.Constraints)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return Result{}, err
	}
	inv := Invocation{Dir: run.Dir, System: system, Schema: runSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: true}
	inv.Prompt = fmt.Sprintf("Case %s (%s, %s): %s\nStep: %s. Read the case with `momentum case show %s --json`, then take this step as the "+
		"loop's procedure and the %s skill say. Your working directory is %s.", c.ID, c.Family, c.Status, c.Title, step, c.ID, family, run.Dir)
	switch step {
	case StepBuild:
		inv.MCP = r.MCP
		inv.AddDirs = []string{r.Ship.workDir(c.ID)}
		if p, ok, _ := r.Ship.Load(c.ID); ok && p.State == Revising && len(p.Reviews) > 0 {
			last := p.Reviews[len(p.Reviews)-1]
			text, err := Redact(ctx, last.Text)
			if err != nil {
				return Result{}, err
			}
			if err := os.WriteFile(filepath.Join(run.Dir, "review.md"), []byte(text), 0o600); err != nil {
				return Result{}, err
			}
			inv.Prompt += fmt.Sprintf("\nIts change %s was reviewed: round %d found %d high, %d medium and %d low, in review.md. Revise the change "+
				"in its work tree %s for what the review found, check it again (`momentum ship check`) and propose it again (`momentum ship "+
				"propose`), which updates its pull request.", p.PR, last.Round, last.High, last.Medium, last.Low, p.Work)
		}
	case StepInvestigate:
		inv.AddDirs = append(inv.AddDirs, r.ReadDirs...)
	}
	inv.Settings = r.permissions(step, c, run)
	// the prompt holds the case's title, which a detector made of what it read: it reaches the run scrubbed
	if inv.Prompt, err = Redact(ctx, inv.Prompt); err != nil {
		return Result{}, err
	}
	agent := r.Agent
	agent.Env = CleanEnv(map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID,
		"MOMENTUM_LOOP_DIR": run.Dir}, r.Bins...)
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

// rule is a harness permission rule for a path: an absolute path opens with // in the harness's rules.
func rule(tool, path string) string { return tool + "(/" + filepath.Clean(path) + ")" }

// permissions are what the harness lets a run call. Its file tools reach only its directories (restricted mode:
// its own, a build's or a review's work tree, an investigation's read directories), and within them no credential
// file; it writes only in its own directory and a build's work tree. Its shell runs only momentum's own commands
// that read the store, the records its events name, git history, the user's services and files (each scrubbed of
// secrets), its own case's commands, tacit's search; a build's the ship tool's steps, a publish's the lesson's.
// A call no rule allows is refused (dontAsk).
func (r *Runner) permissions(step string, c store.Case, run store.LoopRun) Settings {
	s := Settings{
		Allow: []string{"Read", "Grep", "Glob", rule("Write", run.Dir+"/**"), rule("Edit", run.Dir+"/**"),
			"Bash(momentum case *)", "Bash(momentum evidence events *)", "Bash(momentum evidence status *)", "Bash(momentum evidence show *)",
			"Bash(momentum evidence git *)", "Bash(momentum evidence file *)", "Bash(momentum evidence unit *)",
			"Bash(momentum supervise replay *)", "Bash(momentum report *)", "Bash(momentum ship status *)",
			"Bash(tacit search *)", "Bash(tacit fact *)", "Bash(tacit tree *)", "Agent", "Task"},
		Deny: append(CredentialDenials(), "WebFetch", "WebSearch"),
	}
	switch step {
	case StepBuild:
		w := r.Ship.workDir(c.ID)
		s.Allow = append(s.Allow, rule("Write", w+"/**"), rule("Edit", w+"/**"), "Bash(momentum ship prepare *)", "Bash(momentum ship check *)",
			"Bash(momentum ship propose *)")
		for _, name := range mcpServers(r.MCP) {
			s.Allow = append(s.Allow, "mcp__"+name)
		}
	case StepPublish:
		s.Allow = append(s.Allow, "Bash(momentum loop lesson *)")
	}
	return s
}

// mcpServers are the names of an MCP configuration's servers.
func mcpServers(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	var out []string
	for n := range cfg.Servers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

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
			if lp, err := r.Ship.Land(ctx, c.ID); err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: err.Error()})
				r.Store.CaseNote(ctx, c.ID, "not landed", err.Error(), "loop")
			} else {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land"})
				r.note(ctx, store.Note{Kind: store.NoteDecided, Title: plainClip("momentum landed a change for: "+c.Title, store.TitleMax),
					Detail: fmt.Sprintf("Reviewed clean and landed; measured over the %s after it. To undo it: momentum ship rollback %s --why ...",
						days(lp.Plan.Window), c.ID), Links: prLinks(lp.PR)})
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
			res.Status, res.Failure = store.RunFailed, cerr.Error()
		} else {
			text := reviewText(len(p.Reviews)+1, h, m, l, *rep.Summary, *rep.Findings)
			rev := Review{Findings: *rep.Findings, Text: text}
			saved, saveErr := r.Ship.Reviewed(ctx, c.ID, rev)
			if saveErr != nil {
				res.Status, res.Failure = store.RunFailed, saveErr.Error()
			} else if _, postErr := r.postReview(ctx, saved); postErr != nil {
				r.logf("post the review of %s: %v", c.ID, postErr)
			}

			run.Summary = fmt.Sprintf("%d high, %d medium, %d low: %s", h, m, l, *rep.Summary)
		}
	}
	r.finish(ctx, c, &run, res, err)
	d.Run = run
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
	if err := os.WriteFile(filepath.Join(run.Dir, "diff.patch"), []byte(diff), 0o600); err != nil {
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
	if err := os.WriteFile(filepath.Join(run.Dir, "package.json"), []byte(pkg), 0o600); err != nil {
		return Result{}, err
	}
	inv := Invocation{Dir: run.Dir, System: system, Schema: reviewSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: true, AddDirs: []string{p.Work}}
	inv.Prompt = fmt.Sprintf("Review the change package of case %s (%s): %s. The change is diff.patch and the package package.json in your "+
		"working directory %s; the work tree is %s. Read the case with `momentum case show %s --json`. End with the review's report.",
		c.ID, c.Family, p.Title, run.Dir, p.Work, c.ID)
	inv.Settings = r.permissions(StepReview, c, run)
	if inv.Prompt, err = Redact(ctx, inv.Prompt); err != nil {
		return Result{}, err
	}
	agent := r.Agent
	agent.Env = CleanEnv(map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID,
		"MOMENTUM_LOOP_DIR": run.Dir}, r.Bins...)
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

// reviewDiff is the change as its review reads it: scrubbed, and with no credential file in it (each one the change
// touches is named, and its content withheld).
func (r *Runner) reviewDiff(ctx context.Context, p Package) (string, error) {
	span := "origin/" + p.Base + "..." + p.Head
	all, err := r.Ship.git(ctx, p.Work, "diff", "--name-only", "-z", "--no-renames", span)
	if err != nil {
		return "", err
	}
	shown, err := r.Ship.git(ctx, p.Work, append([]string{"diff", "--name-only", "-z", "--no-renames", span, "--"}, CredentialPathspecs()...)...)
	if err != nil {
		return "", err
	}
	kept := map[string]bool{}
	for _, f := range strings.Split(shown, "\x00") {
		kept[f] = true
	}
	var withheld []string
	for _, f := range strings.Split(all, "\x00") {
		if f != "" && !kept[f] {
			withheld = append(withheld, f)
		}
	}
	diff, err := r.Ship.git(ctx, p.Work, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", span, "--"}, CredentialPathspecs()...)...)
	if err != nil {
		return "", err
	}
	if len(withheld) > 0 {
		diff = "# The change also touches credential files, whose content is withheld here: " + strings.Join(withheld, ", ") + "\n" + diff
	}
	return Redact(ctx, diff)
}
