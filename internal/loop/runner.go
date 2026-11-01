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
	StepBuild       = "build"       // testing: build and propose the change
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
	MCP         string // for build runs
	Dir         string // the loop's directory
	Home        string
	Bins        []string // the directories of momentum's tools, first on a run's PATH
	Now         func() time.Time
	Log         func(format string, args ...any)
	// Post puts a review on its pull request, and returns where (the GitHub CLI by default).
	Post func(ctx context.Context, repo, pr, body string) (string, error)
}

// Options are what one run of the loop does.
type Options struct {
	Dry      bool     // detect and pick, run no agent
	Families []string // only these (default: the runner's)
	Case     string   // only this case
}

// Done is one step the run took, or why it took none.
type Done struct {
	Family, Case, Step string
	Run                store.LoopRun
	Skipped            string // why no step ran
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

// Once runs the loop once.
func (r *Runner) Once(ctx context.Context, o Options) ([]Done, error) {
	if err := r.endStale(ctx); err != nil {
		return nil, err
	}
	if !o.Dry {
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
			r.logf("detector %s: %d findings, %d new or back", out.Family, len(out.Filed), opened)
		}
		for _, c := range swept {
			r.logf("swept %s: %s", c.ID, c.Title)
		}
	}
	families := r.Families
	if len(o.Families) > 0 {
		families = o.Families
	}
	var mu sync.Mutex
	var done []Done
	var wg sync.WaitGroup
	for _, fam := range families {
		c, step, why, err := r.pick(ctx, fam, o.Case)
		if err != nil {
			return done, err
		}
		if c.ID == "" {
			done = append(done, Done{Family: fam, Skipped: why})
			continue
		}
		if o.Dry {
			done = append(done, Done{Family: fam, Case: c.ID, Step: step, Skipped: "dry: " + why})
			continue
		}
		wg.Add(1)
		go func(fam string, c store.Case, step string) {
			defer wg.Done()
			d := r.step(ctx, fam, c, step)
			mu.Lock()
			done = append(done, d)
			mu.Unlock()
		}(fam, c, step)
	}
	wg.Wait()
	if !o.Dry {
		done = append(done, r.reviews(ctx, o.Case)...)
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Family+done[i].Step < done[j].Family+done[j].Step })
	return done, nil
}

// endStale marks the runs that started longer ago than any run may last and never ended: killed with the
// process that ran them.
func (r *Runner) endStale(ctx context.Context) error {
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Since: r.now().Add(-7 * 24 * time.Hour)})
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == store.RunRunning && r.now().Sub(run.Started) > r.Budgets.RunTime+15*time.Minute {
			run.Status, run.Failure, run.Ended = store.RunFailed, "killed", r.now()
			if err := r.Store.PutLoopRun(ctx, run); err != nil {
				return err
			}
		}
	}
	return nil
}

// pick chooses a family's case and its step: a case in testing first (finish what is in flight), then one
// investigating, then an open one, the one with the most evidence first; and a case closed as improved whose
// lesson is not published. It passes over a case its runs stopped (the stop rule), one over its budget, and one
// whose change waits on its review or its measurement. why says what it chose or why none.
func (r *Runner) pick(ctx context.Context, family, only string) (store.Case, string, string, error) {
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
		step, rank := "", 0
		switch c.Status {
		case store.CaseTesting:
			step, rank = StepBuild, 3
			if p, ok, err := r.Ship.Load(c.ID); err != nil {
				return store.Case{}, "", "", err
			} else if ok && p.State == Proposed && (len(p.Reviews) == 0 || p.Clean() || p.Reviews[len(p.Reviews)-1].Head != p.Head) {
				continue // its review or its landing comes next (reviews)
			} else if ok && p.State == Proposed && len(p.Reviews) >= MaxRounds {
				passed = append(passed, c.ID+" used its review rounds")
				continue
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
		if c.Cost.USD >= r.Budgets.Case && step != StepPublish {
			passed = append(passed, fmt.Sprintf("%s spent $%.2f of its $%.0f", c.ID, c.Cost.USD, r.Budgets.Case))
			continue
		}
		if stop, err := r.stopped(ctx, c); err != nil {
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

// published reports whether a case's lesson went to tacit: a publish run of it that ended well.
func (r *Runner) published(ctx context.Context, id string) bool {
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: id})
	if err != nil {
		return true // unread: do not publish twice
	}
	for _, run := range runs {
		if run.Step == StepPublish && run.Status == store.RunOK {
			return true
		}
	}
	return false
}

// stopped applies the stop rule: a case whose last two runs failed the same way is not run a third time. It
// opens a case of the loop's own, in component health, the first time, and says why.
func (r *Runner) stopped(ctx context.Context, c store.Case) (string, error) {
	live := ""
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID, Experiment: &live, Limit: 2})
	if err != nil || len(runs) < 2 {
		return "", err
	}
	a, b := runs[0], runs[1]
	if a.Status == store.RunOK || b.Status == store.RunOK || a.Failure == "" || a.Failure != b.Failure || a.Step != b.Step {
		return "", nil
	}
	why := fmt.Sprintf("its last two %s runs failed the same way: %s", a.Step, a.Failure)
	f := store.Finding{Family: "component-health", Signature: "component-health/" + detect.Versions["component-health"] + ":loop:" + a.Step + ":" + slugOf(a.Failure),
		Title: plainClip("The loop fails the same way twice: "+a.Failure, store.TitleMax)}
	for _, run := range runs {
		if run.Dir != "" {
			f.Evidence = append(f.Evidence, store.Evidence{Ref: "file:" + filepath.Join(run.Dir, "transcript.jsonl"), At: run.Started,
				Kind: "loop-run", Summary: run.Step + " " + run.Case + ": " + run.Failure})
		}
	}
	if len(f.Evidence) > 0 {
		if _, _, err := r.Store.ObserveCase(ctx, f, "loop"); err != nil {
			return "", err
		}
	}
	return why, nil
}

// step runs one step of a case: the agent, its budget checked first and its cost recorded after.
func (r *Runner) step(ctx context.Context, family string, c store.Case, step string) Done {
	d := Done{Family: family, Case: c.ID, Step: step}
	run := store.LoopRun{ID: newRunID(r.now()), Case: c.ID, Family: family, Step: step, Started: r.now(), Model: r.Agent.Model}
	if why, err := r.affordable(ctx); err != nil || why != "" {
		run.Status, run.Failure, run.Ended = store.RunRefused, firstOf(why, fmt.Sprint(err)), r.now()
		r.Store.PutLoopRun(ctx, run)
		d.Run, d.Skipped = run, run.Failure
		return d
	}
	run.Dir = filepath.Join(r.Dir, "runs", run.ID)
	run.Status = store.RunRunning
	if err := r.Store.PutLoopRun(ctx, run); err != nil {
		d.Skipped = err.Error()
		return d
	}
	res, err := r.invoke(ctx, family, c, step, run)
	run.Ended = r.now()
	if err != nil {
		run.Status, run.Failure = store.RunFailed, err.Error()
	} else {
		run.Status, run.Failure, run.USD, run.Tokens, run.Turns = res.Status, res.Failure, res.USD, res.Tokens, res.Turns
		run.Summary = summaryOf(res)
	}
	if err := r.Store.PutLoopRun(ctx, run); err != nil {
		r.logf("record run %s: %v", run.ID, err)
	}
	if run.USD > 0 || run.Status != store.RunRefused {
		if _, err := r.Store.AddCaseCost(ctx, c.ID, store.Cost{USD: run.USD, Tokens: run.Tokens, Duration: run.Ended.Sub(run.Started), Runs: 1},
			"loop:"+run.ID); err != nil {
			r.logf("cost of run %s: %v", run.ID, err)
		}
	}
	r.Store.CaseNote(ctx, c.ID, "run", fmt.Sprintf("%s %s: %s %s", step, run.ID, run.Status, firstOf(run.Failure, run.Summary)), "loop:"+run.ID)
	d.Run = run
	return d
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
	inv.Settings = r.permissions(step, c, run)
	if step == StepBuild {
		inv.MCP = r.MCP
		inv.AddDirs = []string{r.Ship.workDir(c.ID)}
	}
	agent := r.Agent
	agent.Env = CleanEnv(map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID,
		"MOMENTUM_LOOP_DIR": run.Dir}, r.Bins...)
	return agent.Run(ctx, inv)
}

// runSchema is the report every run ends with.
const runSchema = `{"type":"object","additionalProperties":false,"required":["case","step","outcome","summary"],
"properties":{"case":{"type":"string"},"step":{"type":"string"},
"outcome":{"type":"string","enum":["investigated","testing","closed","merged","proposed","published","blocked","nothing"]},
"mechanism":{"type":"string"},"root":{"type":"string"},"summary":{"type":"string"},"blocked_by":{"type":"string"}}}`

// reviewSchema is a review run's report.
const reviewSchema = `{"type":"object","additionalProperties":false,"required":["high","medium","low","findings","summary"],
"properties":{"high":{"type":"integer"},"medium":{"type":"integer"},"low":{"type":"integer"},"summary":{"type":"string"},
"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["severity","where","problem","fix"],
"properties":{"severity":{"type":"string","enum":["high","medium","low"]},"where":{"type":"string"},"problem":{"type":"string"},"fix":{"type":"string"}}}}}}`

// permissions are what the harness lets a run call: reading anywhere but where credentials are kept, momentum's
// own tools, git's history through momentum, tacit's search, the user's services' status and logs; writing only
// in the run's own directory, and for a build in its case's work tree; the ship tool's steps a build takes;
// tacit's add for a publish. A call no rule allows is refused (dontAsk).
func (r *Runner) permissions(step string, c store.Case, run store.LoopRun) Settings {
	h := "/" + r.Home // an absolute path in the harness's rules opens with //
	s := Settings{
		Allow: []string{"Read(/" + h + "/**)", "Grep", "Glob", "Write(/" + run.Dir + "/**)", "Edit(/" + run.Dir + "/**)",
			"Bash(momentum case *)", "Bash(momentum evidence events *)", "Bash(momentum evidence status *)", "Bash(momentum evidence git *)",
			"Bash(momentum supervise replay *)", "Bash(momentum report *)", "Bash(momentum ship status *)",
			"Bash(tacit search *)", "Bash(tacit fact *)", "Bash(tacit tree *)",
			"Bash(systemctl --user status *)", "Bash(systemctl --user list-timers *)", "Bash(systemctl --user list-units *)",
			"Bash(systemctl --user cat *)", "Bash(journalctl --user *)", "Agent", "Task"},
		Deny: []string{"Read(/" + h + "/.ssh/**)", "Read(/" + h + "/.gnupg/**)", "Read(/" + h + "/.aws/**)", "Read(/" + h + "/.config/gh/**)",
			"Read(/" + h + "/.local/share/keyrings/**)", "Read(/" + h + "/.password-store/**)", "Read(/" + h + "/.claude/.credentials.json)",
			"Read(/" + h + "/.netrc)", "Read(/" + h + "/.git-credentials)", "Read(/" + h + "/.docker/config.json)", "Read(**/.env)",
			"Read(**/.env.*)", "Read(/" + h + "/.config/claude-code/**)", "Read(/" + h + "/.config/rclone/**)", "WebFetch", "WebSearch"},
	}
	switch step {
	case StepBuild:
		w := r.Ship.workDir(c.ID)
		s.Allow = append(s.Allow, "Write(/"+w+"/**)", "Edit(/"+w+"/**)", "Bash(momentum ship prepare *)", "Bash(momentum ship check *)",
			"Bash(momentum ship propose *)")
	case StepPublish:
		s.Allow = append(s.Allow, "Bash(tacit add *)")
	}
	return s
}

// reviews runs the clean-context review of each proposed change that has none on its head, posts it, and lands
// the change when it is clean and the loop is live.
func (r *Runner) reviews(ctx context.Context, only string) []Done {
	var out []Done
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseTesting}})
	if err != nil {
		r.logf("reviews: %v", err)
		return nil
	}
	for _, c := range cs {
		if only != "" && c.ID != only {
			continue
		}
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil || !ok || p.State != Proposed {
			continue
		}
		if p.Clean() {
			if _, err := r.Ship.Land(ctx, c.ID); err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: err.Error()})
				r.Store.CaseNote(ctx, c.ID, "not landed", err.Error(), "loop")
			} else {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land"})
			}
			continue
		}
		if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head || n >= MaxRounds {
			continue // its findings go back to the build
		}
		d := r.review(ctx, c, p)
		out = append(out, d)
	}
	return out
}

// review runs one review round of a proposed change.
func (r *Runner) review(ctx context.Context, c store.Case, p Package) Done {
	d := Done{Family: c.Family, Case: c.ID, Step: StepReview}
	run := store.LoopRun{ID: newRunID(r.now()), Case: c.ID, Family: c.Family, Step: StepReview, Started: r.now(), Model: r.Agent.Model}
	if why, err := r.affordable(ctx); err != nil || why != "" {
		run.Status, run.Failure, run.Ended = store.RunRefused, firstOf(why, fmt.Sprint(err)), r.now()
		r.Store.PutLoopRun(ctx, run)
		d.Run, d.Skipped = run, run.Failure
		return d
	}
	run.Dir, run.Status = filepath.Join(r.Dir, "runs", run.ID), store.RunRunning
	r.Store.PutLoopRun(ctx, run)
	res, err := r.invokeReview(ctx, c, p, run)
	run.Ended = r.now()
	if err != nil {
		run.Status, run.Failure = store.RunFailed, err.Error()
	} else {
		run.Status, run.Failure, run.USD, run.Tokens, run.Turns = res.Status, res.Failure, res.USD, res.Tokens, res.Turns
	}
	if run.Status == store.RunOK {
		var rep struct {
			High, Medium, Low int
			Summary           string
			Findings          []struct{ Severity, Where, Problem, Fix string }
		}
		if json.Unmarshal(res.Report, &rep) != nil {
			run.Status, run.Failure = store.RunFailed, "an unreadable review"
		} else {
			text := reviewText(len(p.Reviews)+1, rep.High, rep.Medium, rep.Low, rep.Summary, rep.Findings)
			rev := Review{High: rep.High, Medium: rep.Medium, Low: rep.Low, Text: text}
			if p.Kind == "repo" && r.Post != nil && p.PR != "" {
				if url, err := r.Post(ctx, p.Repo, p.PR, text); err == nil {
					rev.Comment = url
				} else {
					r.logf("post the review of %s: %v", c.ID, err)
				}
			}
			if _, err := r.Ship.Reviewed(ctx, c.ID, rev); err != nil {
				run.Status, run.Failure = store.RunFailed, err.Error()
			}
			run.Summary = fmt.Sprintf("%d high, %d medium, %d low: %s", rep.High, rep.Medium, rep.Low, rep.Summary)
		}
	}
	r.Store.PutLoopRun(ctx, run)
	r.Store.AddCaseCost(ctx, c.ID, store.Cost{USD: run.USD, Tokens: run.Tokens, Duration: run.Ended.Sub(run.Started), Runs: 1}, "loop:"+run.ID)
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
	diff := ""
	if p.Kind == "repo" {
		diff, _ = gitOutput(ctx, p.Work, "diff", "origin/"+p.Base+"..."+p.Head)
	} else if b, err := os.ReadFile(filepath.Join(r.Ship.pkgDir(c.ID), "diff.patch")); err == nil {
		diff = string(b)
	}
	if err := os.WriteFile(filepath.Join(run.Dir, "diff.patch"), []byte(diff), 0o600); err != nil {
		return Result{}, err
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(filepath.Join(run.Dir, "package.json"), raw, 0o600); err != nil {
		return Result{}, err
	}
	inv := Invocation{Dir: run.Dir, System: system, Schema: reviewSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: true, AddDirs: []string{p.Work}}
	inv.Prompt = fmt.Sprintf("Review the change package of case %s (%s): %s. The change is diff.patch and the package package.json in your "+
		"working directory %s; the work tree is %s. Read the case with `momentum case show %s --json`. End with the review's report.",
		c.ID, c.Family, p.Title, run.Dir, p.Work, c.ID)
	inv.Settings = r.permissions(StepReview, c, run)
	agent := r.Agent
	agent.Env = CleanEnv(map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID,
		"MOMENTUM_LOOP_DIR": run.Dir}, r.Bins...)
	return agent.Run(ctx, inv)
}

// reviewText is a review as it is posted: its round, its counts, and each finding.
func reviewText(round, high, medium, low int, summary string, findings []struct{ Severity, Where, Problem, Fix string }) string {
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

// contains reports whether xs holds x.
func contains(xs []string, x string) bool { return slices.Contains(xs, x) }
