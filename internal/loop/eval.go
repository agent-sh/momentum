package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// The golden-set evaluation runs the loop's investigation on real past problems whose root cause and fix are
// verified, each as of the moment before its fix: a store that holds only what came before, the code of the fixed
// repository at the commit before the fix, and no tacit (a lesson published since would give the answer). A
// clean-context judge grades whether the investigation reached the verified root cause. Each task runs several
// times, each trial from nothing of another's, and the results say which tasks flipped.

// GoldenTask is one problem of the golden set, as the machine keeps it (never in a repository).
type GoldenTask struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	Title     string `json:"title"`
	CaseTitle string `json:"case_title"` // the symptom alone, as a detector would title it
	Symptom   struct {
		Query struct {
			Since    string   `json:"since"`
			EventIDs []string `json:"event_ids"`
		} `json:"query"`
	} `json:"symptom"`
	Cutoff     time.Time `json:"cutoff"`
	Repo       string    `json:"repo"`
	RepoPath   string    `json:"repo_path"`
	PreFix     string    `json:"pre_fix_commit"`
	RootCause  string    `json:"root_cause"`
	KeyFacts   []string  `json:"key_facts"`
	WrongPaths []string  `json:"wrong_paths"`
}

// ReadGolden reads a golden set, one task per line.
func ReadGolden(path string) ([]GoldenTask, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []GoldenTask
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var t GoldenTask
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if t.ID == "" || t.Family == "" || t.CaseTitle == "" || t.Cutoff.IsZero() || len(t.Symptom.Query.EventIDs) == 0 || len(t.KeyFacts) == 0 {
			return nil, fmt.Errorf("%s:%d: a task needs an id, a family, a case title, a cutoff, symptom events and key facts", path, n)
		}
		t.RepoPath = paths.Expand(t.RepoPath)
		out = append(out, t)
	}
	return out, sc.Err()
}

// Eval runs the golden set.
type Eval struct {
	Runner   *Runner // its agent, budgets, rules and record
	Source   string  // the store the snapshots are made from
	Dir      string  // where the trials go
	Trials   int
	Budget   float64 // the whole evaluation's, in US dollars
	Parallel int     // trials at once
	ID       string  // this evaluation's name, in its runs' experiment tags
	Judge    Agent
	// Native runs the native path in place of the loop: the harness's own agent, asked the question with the same
	// evidence and the same limits, without the loop's procedure, the family's skill or a case to record on.
	Native bool
}

// Grade is the judge's reading of one investigation.
type Grade struct {
	Verdict string `json:"verdict"` // reached, partial, missed, wrong
	Facts   []struct {
		Fact    string `json:"fact"`
		Reached bool   `json:"reached"`
	} `json:"facts"`
	Reason string `json:"reason"`
}

// Trial is one run of one task.
type Trial struct {
	Task, Family string
	N            int
	Run          store.LoopRun
	Grade        Grade
	JudgeUSD     float64
	Mechanism    string
	Root         string
	Status       string // the case's status after the run, with its reason when closed
	Err          string
}

// evalWindow is how far before a task's cutoff its store reaches: the evidence's retention, as much of the past as
// a live investigation's searches read (an approval two weeks before, the run that set a value).
func evalWindow(t GoldenTask) time.Time { return t.Cutoff.Add(-store.CaseRetention) }

// Run runs every trial of every task, Parallel at a time, until the budget is spent, and returns them in order.
func (e *Eval) Run(ctx context.Context, tasks []GoldenTask) ([]Trial, error) {
	type job struct {
		t GoldenTask
		n int
	}
	var jobs []job
	for n := 1; n <= max(e.Trials, 1); n++ {
		for _, t := range tasks {
			jobs = append(jobs, job{t, n})
		}
	}
	var mu sync.Mutex
	var out []Trial
	spent := 0.0
	sem := make(chan struct{}, max(e.Parallel, 1))
	var wg sync.WaitGroup
	for _, j := range jobs {
		mu.Lock()
		over := spent+e.Runner.Budgets.Run > e.Budget
		mu.Unlock()
		if over {
			out = append(out, Trial{Task: j.t.ID, Family: j.t.Family, N: j.n, Err: fmt.Sprintf("the evaluation's budget ($%.0f) is spent", e.Budget)})
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			tr := e.trial(ctx, j.t, j.n)
			mu.Lock()
			spent += tr.Run.USD + tr.JudgeUSD
			out = append(out, tr)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].N < out[j].N
	})
	return out, nil
}

// trial runs one task once, from nothing.
func (e *Eval) trial(ctx context.Context, t GoldenTask, n int) Trial {
	tr := Trial{Task: t.ID, Family: t.Family, N: n}
	dir := filepath.Join(e.Dir, t.ID, fmt.Sprint(n))
	if err := os.RemoveAll(dir); err != nil {
		tr.Err = err.Error()
		return tr
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tr.Err = err.Error()
		return tr
	}
	db := filepath.Join(dir, "momentum.db")
	defer func() { // the snapshot is the store's own text again: it goes, the trial's record stays
		for _, f := range []string{db, db + "-wal", db + "-shm"} {
			os.Remove(f)
		}
	}()
	if err := store.AsOf(ctx, e.Source, db, evalWindow(t), t.Cutoff); err != nil {
		tr.Err = "the store as of the cutoff: " + err.Error()
		return tr
	}
	snap, err := store.Open(db)
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	defer snap.Close()
	f := store.Finding{Family: t.Family, Signature: "eval/" + t.ID, Title: t.CaseTitle}
	for _, id := range t.Symptom.Query.EventIDs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + id})
	}
	c, _, err := snap.ObserveCase(ctx, f, "eval")
	if err != nil {
		tr.Err = "the case: " + err.Error()
		return tr
	}
	repo := ""
	if t.RepoPath != "" && t.PreFix != "" {
		repo = filepath.Join(dir, "repo")
		if err := cloneAsOf(ctx, t.RepoPath, t.PreFix, repo); err != nil {
			tr.Err = "the repository as of the fix: " + err.Error()
			return tr
		}
	}
	run := store.LoopRun{ID: newRunID(time.Now()), Case: c.ID, Family: t.Family, Step: StepInvestigate, Started: time.Now(),
		Model: e.Runner.Agent.Model, Dir: dir, Experiment: fmt.Sprintf("eval:%s:%s:%d", e.ID, t.ID, n), Status: store.RunRunning}
	e.Runner.Store.PutLoopRun(ctx, run)
	step := e.investigate
	if e.Native {
		step = e.native
	}
	res, err := step(ctx, t, c, dir, repo, run)
	run.Ended = time.Now()
	if err != nil {
		run.Status, run.Failure = store.RunFailed, err.Error()
	} else {
		run.Status, run.Failure, run.USD, run.Tokens, run.Turns, run.Summary = res.Status, res.Failure, res.USD, res.Tokens, res.Turns, summaryOf(res)
		run.Model = firstOf(res.Model, run.Model)
	}
	e.Runner.Store.PutLoopRun(ctx, run)
	tr.Run = run
	after, err := snap.CaseLive(ctx, c.ID)
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	tr.Mechanism, tr.Status = after.Mechanism, string(after.Status)
	if after.Reason != "" {
		tr.Status += " " + string(after.Reason)
	}
	for _, s := range after.Signatures {
		if strings.HasPrefix(s, store.RootPrefix) {
			tr.Root = s
		}
	}
	raw, _ := json.MarshalIndent(caseRecord(after), "", "  ")
	os.WriteFile(filepath.Join(dir, "case.json"), raw, 0o600)
	found := caseFinding(after, res)
	if e.Native {
		found = nativeFinding(res)
		tr.Mechanism, tr.Root, tr.Status = found.Mechanism, found.Root, "native"
	}
	tr.Grade, tr.JudgeUSD, err = e.grade(ctx, t, found, dir, run)
	if err != nil {
		tr.Err = "the judge: " + err.Error()
	}
	raw, _ = json.MarshalIndent(tr, "", "  ")
	os.WriteFile(filepath.Join(dir, "trial.json"), raw, 0o600)
	return tr
}

// caseRecord is what a trial keeps of its case.
func caseRecord(c store.Case) map[string]any {
	return map[string]any{"id": c.ID, "status": c.Status, "reason": c.Reason, "mechanism": c.Mechanism, "alternatives": c.Alternatives,
		"next_test": c.NextTest, "benefit": c.Benefit, "signatures": c.Signatures, "evidence": c.Evidence}
}

// cloneAsOf clones a repository at a commit with nothing after it: every other ref deleted and what only they
// reached pruned, so no command of the agent's finds the fix.
func cloneAsOf(ctx context.Context, src, commit, dst string) error {
	steps := [][]string{
		{"clone", "-q", "--no-hardlinks", "--no-checkout", src, dst},
		{"-C", dst, "checkout", "-q", "--detach", commit},
	}
	for _, s := range steps {
		if out, err := exec.CommandContext(ctx, "git", s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", s[0], err, out)
		}
	}
	refs, err := exec.CommandContext(ctx, "git", "-C", dst, "for-each-ref", "--format=%(refname)").Output()
	if err != nil {
		return err
	}
	for _, r := range strings.Fields(string(refs)) {
		if out, err := exec.CommandContext(ctx, "git", "-C", dst, "update-ref", "-d", r).CombinedOutput(); err != nil {
			return fmt.Errorf("delete %s: %v: %s", r, err, out)
		}
	}
	for _, s := range [][]string{{"-C", dst, "checkout", "-q", "-b", "main"}, {"-C", dst, "remote", "remove", "origin"},
		{"-C", dst, "reflog", "expire", "--expire=now", "--all"}, {"-C", dst, "gc", "-q", "--prune=now"}} {
		if out, err := exec.CommandContext(ctx, "git", s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(s[2:], " "), err, out)
		}
	}
	return nil
}

// investigate runs the investigation step of a trial: the loop's own procedure and skill, with an evaluation's
// permissions: the trial's directory alone to read and write, the store as of the cutoff through momentum, the
// records its events name (evidence show), the repository as of the fix, and no tacit, no services, no report.
func (e *Eval) investigate(ctx context.Context, t GoldenTask, c store.Case, dir, repo string, run store.LoopRun) (Result, error) {
	r := e.Runner
	// no rules file: the machine's rules as they are now hold the lessons of fixes made since the cutoff
	system, err := systemPrompt(StepInvestigate, t.Family, nil, r.Constraints)
	if err != nil {
		return Result{}, err
	}
	where := "No repository was changed by this problem's fix, so there is none to read."
	if repo != "" {
		where = fmt.Sprintf("The code of %s as of then is a checkout at %s: read it with Read and Grep, and its history with "+
			"`momentum evidence git %s log ...`.", t.Repo, repo, repo)
	}
	// the file tools read only the trial's directory (restricted, as every run is), and no command reads the machine
	// as it is now (a transcript's later lines, a configuration fixed since): the moment's store, its records and the
	// repository as of the fix are all it reads
	inv := Invocation{Dir: dir, System: system, Schema: runSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: false}
	inv.Prompt = fmt.Sprintf("Case %s (%s, open): %s\nStep: investigate. Read the case with `momentum case show %s --json`, then take this "+
		"step as the loop's procedure and the %s skill say. The store holds what the machine recorded up to %s; read an event's record "+
		"with `momentum evidence show ID`. %s Your working directory is %s. This run evaluates the investigation: record what you find "+
		"on the case, and build no change.", c.ID, c.Family, c.Title, c.ID, t.Family, t.Cutoff.Format(time.RFC3339), where, dir)
	inv.Settings = Settings{
		Allow: []string{"Read", "Grep", "Glob", rule("Write", dir+"/**"), rule("Edit", dir+"/**"), "Bash(momentum case *)",
			"Bash(momentum evidence events *)", "Bash(momentum evidence status *)", "Bash(momentum evidence show *)",
			"Bash(momentum evidence search *)", "Bash(momentum evidence git *)", "Bash(momentum supervise replay *)", "Agent", "Task"},
		Deny: []string{"WebFetch", "WebSearch"},
	}
	agent := r.Agent
	agent.Env = CleanEnv(map[string]string{"MOMENTUM_HOME": dir, "MOMENTUM_EVAL": e.ID, "MOMENTUM_ACTOR": "eval:" + run.ID,
		"MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID, "MOMENTUM_LOOP_DIR": dir}, r.Bins...)
	return agent.RunOrFallback(ctx, inv)
}

// nativeSchema is the native path's report: what it found.
const nativeSchema = `{"type":"object","additionalProperties":false,"required":["mechanism","root_cause","summary"],
"properties":{"mechanism":{"type":"string"},"alternatives":{"type":"array","items":{"type":"string"}},"root_cause":{"type":"string"},
"summary":{"type":"string"}}}`

// nativeBrief is the native path's whole instruction of its own: the question, and where its evidence is.
const nativeBrief = `You find the root cause of a problem in an agent system on one machine, from what the machine recorded, and report
it. Your file tools reach only your working directory and a repository checkout when you are given one; you read the machine's records
through momentum's evidence commands.`

// native runs the native path of a trial: the harness's own agent with the same evidence, permissions and limits as
// the loop's investigation, its system prompt only the brief and the owner's standing constraints (no rules file,
// as for the loop's arm), its question the symptom's title and events, its answer its report.
func (e *Eval) native(ctx context.Context, t GoldenTask, c store.Case, dir, repo string, run store.LoopRun) (Result, error) {
	r := e.Runner
	var b strings.Builder
	b.WriteString(nativeBrief + "\n")
	if len(r.Constraints) > 0 {
		b.WriteString("\n# Standing constraints from the owner\n\n")
		for _, x := range r.Constraints {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	where := "No repository was changed by this problem's fix, so there is none to read."
	if repo != "" {
		where = fmt.Sprintf("The code of %s as of then is a checkout at %s: read it with Read and Grep, and its history with "+
			"`momentum evidence git %s log ...`.", t.Repo, repo, repo)
	}
	inv := Invocation{Dir: dir, System: b.String(), Schema: nativeSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: false}
	inv.Prompt = fmt.Sprintf("Find the root cause of this problem: %s\n\nIts evidence is these events of momentum's event store: %s. "+
		"The store holds what the machine recorded up to %s: the agents' sessions (their calls and results, the owner's words), incidents "+
		"and supervision. `momentum evidence show ID` prints an event and the record it names, `momentum evidence search TEXT` finds the "+
		"events whose text holds a phrase, `momentum evidence events --session ID --since T --until T --json` reads a session around a "+
		"moment, and `momentum evidence status` counts the store's events. %s Your working directory is %s.\n\nEnd with your report: "+
		"the mechanism (what in the setup makes this happen), the alternatives you ruled out, and the root cause.", c.Title,
		strings.Join(t.Symptom.Query.EventIDs, " "), t.Cutoff.Format(time.RFC3339), where, dir)
	inv.Settings = Settings{
		Allow: []string{"Read", "Grep", "Glob", rule("Write", dir+"/**"), rule("Edit", dir+"/**"),
			"Bash(momentum evidence events *)", "Bash(momentum evidence status *)", "Bash(momentum evidence show *)",
			"Bash(momentum evidence search *)", "Bash(momentum evidence git *)", "Bash(momentum supervise replay *)", "Agent", "Task"},
		Deny: []string{"WebFetch", "WebSearch"},
	}
	agent := r.Agent
	agent.Env = CleanEnv(map[string]string{"MOMENTUM_HOME": dir, "MOMENTUM_EVAL": e.ID, "MOMENTUM_ACTOR": "eval:" + run.ID,
		"MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID, "MOMENTUM_LOOP_DIR": dir}, r.Bins...)
	return agent.RunOrFallback(ctx, inv)
}

// finding is what a trial's investigation found, as the judge reads it.
type finding struct {
	Mechanism, NextTest, Root, Status, Report string
	Alternatives                              []string
}

// caseFinding is what the loop's investigation recorded on its case.
func caseFinding(c store.Case, res Result) finding {
	return finding{Mechanism: c.Mechanism, Alternatives: c.Alternatives, NextTest: c.NextTest, Root: strings.Join(c.Signatures, " "),
		Status: strings.TrimSpace(string(c.Status) + " " + string(c.Reason)), Report: firstOf(summaryOf(res), res.Text)}
}

// nativeFinding is what the native path reported.
func nativeFinding(res Result) finding {
	var rep struct {
		Mechanism    string   `json:"mechanism"`
		Alternatives []string `json:"alternatives"`
		Root         string   `json:"root_cause"`
		Summary      string   `json:"summary"`
	}
	if json.Unmarshal(res.Report, &rep) != nil {
		return finding{Report: res.Text}
	}
	return finding{Mechanism: rep.Mechanism, Alternatives: rep.Alternatives, Root: rep.Root, Report: firstOf(rep.Summary, res.Text)}
}

// gradeSchema is the judge's report.
const gradeSchema = `{"type":"object","additionalProperties":false,"required":["verdict","facts","reason"],
"properties":{"verdict":{"type":"string","enum":["reached","partial","missed","wrong"]},"reason":{"type":"string"},
"facts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["fact","reached"],
"properties":{"fact":{"type":"string"},"reached":{"type":"boolean"}}}}}}`

// judgeSystem is the judge's whole instruction.
const judgeSystem = `You grade an investigation of a problem against its verified root cause. You read only what you are given; you
have no tools. For each key fact, say whether the investigation states it or something that means the same, in its own words: a fact
it only hints at, or names as one alternative among others it did not rule out, is not reached. Then give a verdict: reached (it
states the root cause: every key fact, or all but one minor one), partial (some of the key facts, the cause not whole), missed (it
did not find the cause), wrong (it settled on one of the wrong paths, or on another cause it presents as found). Be strict and
literal; give the reason in one or two sentences.`

// grade asks the judge whether a trial's investigation reached the task's root cause.
func (e *Eval) grade(ctx context.Context, t GoldenTask, f finding, dir string, run store.LoopRun) (Grade, float64, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## The verified root cause\n\n%s\n\n## Key facts\n\n", t.RootCause)
	for i, f := range t.KeyFacts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f)
	}
	b.WriteString("\n## Wrong paths\n\n")
	for _, w := range t.WrongPaths {
		fmt.Fprintf(&b, "- %s\n", w)
	}
	fmt.Fprintf(&b, "\n## The investigation\n\nMechanism: %s\n\nAlternatives: %s\n\nNext test: %s\n\nRoot cause: %s\n\n"+
		"Status: %s\n\nIts report: %s\n", firstOf(f.Mechanism, "(none)"), strings.Join(f.Alternatives, " | "), firstOf(f.NextTest, "(none)"),
		firstOf(f.Root, "(none)"), firstOf(f.Status, "(none)"), firstOf(f.Report, "(none)"))
	jdir := filepath.Join(dir, "judge")
	inv := Invocation{Dir: jdir, System: judgeSystem, Schema: gradeSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: 5 * time.Minute,
		Prompt: b.String(), Tools: "none"}
	judge := e.Judge
	judge.Env = CleanEnv(nil, e.Runner.Bins...)
	jr, err := judge.Run(ctx, inv)
	jrun := store.LoopRun{ID: newRunID(time.Now()), Case: run.Case, Family: t.Family, Step: "judge", Started: time.Now().Add(-jr.Duration),
		Ended: time.Now(), Status: jr.Status, Failure: jr.Failure, USD: jr.USD, Tokens: jr.Tokens, Turns: jr.Turns, Model: judge.Model, Dir: jdir,
		Experiment: run.Experiment}
	e.Runner.Store.PutLoopRun(ctx, jrun)
	if err != nil {
		return Grade{}, 0, err
	}
	var g Grade
	if jr.Status != store.RunOK || json.Unmarshal(jr.Report, &g) != nil {
		return Grade{}, jr.USD, fmt.Errorf("no grade: %s %s", jr.Status, jr.Failure)
	}
	return g, jr.USD, nil
}

// Summary is an evaluation's results by task: its trials' verdicts, whether any and every trial reached the root
// cause (pass@k, pass^k), whether the trials disagreed (a flip), and what they cost.
type Summary struct {
	Task, Family string
	Verdicts     []string
	Reached      int
	AnyReached   bool
	AllReached   bool
	Flipped      bool
	USD          float64 // investigations only
	JudgeUSD     float64
	Minutes      float64 // the mean investigation's
	Turns        float64
	Errors       []string
}

// Summarize groups an evaluation's trials by task.
func Summarize(trials []Trial) []Summary {
	by := map[string]*Summary{}
	var order []string
	for _, tr := range trials {
		s := by[tr.Task]
		if s == nil {
			s = &Summary{Task: tr.Task, Family: tr.Family}
			by[tr.Task] = s
			order = append(order, tr.Task)
		}
		if tr.Err != "" && tr.Grade.Verdict == "" {
			s.Errors = append(s.Errors, tr.Err)
			continue
		}
		s.Verdicts = append(s.Verdicts, tr.Grade.Verdict)
		if tr.Grade.Verdict == "reached" {
			s.Reached++
		}
		s.USD += tr.Run.USD
		s.JudgeUSD += tr.JudgeUSD
		s.Minutes += tr.Run.Ended.Sub(tr.Run.Started).Minutes()
		s.Turns += float64(tr.Run.Turns)
	}
	var out []Summary
	for _, id := range order {
		s := by[id]
		if n := len(s.Verdicts); n > 0 {
			s.Minutes /= float64(n)
			s.Turns /= float64(n)
			s.AnyReached, s.AllReached = s.Reached > 0, s.Reached == n
			for _, v := range s.Verdicts {
				s.Flipped = s.Flipped || v != s.Verdicts[0]
			}
		}
		out = append(out, *s)
	}
	return out
}
