package loop

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// Run reserves each pair together. A slot runs both arms and their judges.
func (e *Eval) Run(ctx context.Context, tasks []GoldenTask) ([]Trial, error) {
	if e.Runner == nil || e.Runner.Store == nil {
		return nil, fmt.Errorf("evaluation needs a runner and its recording store")
	}
	if e.Budget < 0 || math.IsNaN(e.Budget) || math.IsInf(e.Budget, 0) {
		return nil, fmt.Errorf("invalid evaluation budget")
	}
	seen := map[string]bool{}
	for _, arm := range e.Arms {
		if (arm != "loop" && arm != "native") || seen[arm] {
			return nil, fmt.Errorf("invalid or repeated arm %q", arm)
		}
		seen[arm] = true
	}
	if err := e.Runner.Agent.Bedrock.Check(); err != nil {
		return nil, fmt.Errorf("the trials: %w", err)
	}
	if err := e.Judge.Bedrock.Check(); err != nil {
		return nil, fmt.Errorf("the judge: %w", err)
	}
	if err := sandbox.Available(true); err != nil {
		return nil, err
	}
	if err := sandbox.StopLeftover(ctx, "momentum-run-eval-"); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.Dir, 0700); err != nil {
		return nil, err
	}
	if err := e.pinRecordsBinary(); err != nil {
		return nil, fmt.Errorf("pin the records binary: %w", err)
	}
	repositories := append([]string(nil), e.Repositories...)
	for _, t := range tasks {
		if t.RepoPath != "" {
			repositories = append(repositories, t.RepoPath)
		}
	}
	sort.Strings(repositories)
	repositories = uniqueStrings(repositories)
	k := max(e.Trials, 1)
	inputs := make([]*taskInputs, len(tasks))
	for i := range inputs {
		inputs[i] = &taskInputs{remaining: k}
		defer inputs[i].close()
	}
	var mu sync.Mutex
	var out []Trial
	spent, reserved := 0.0, 0.0
	// Every relay admits up to 1.5 times its named budget, including a retry.
	perArm := 1.5 * (e.Runner.Budgets.Run + judgeAttempts*judgeBudget)
	room := float64(len(e.arms())) * perArm
	sem := make(chan struct{}, max(e.Parallel, 1))
	var wg sync.WaitGroup
	for n := 1; n <= k; n++ {
		for i, t := range tasks {
			sem <- struct{}{}
			mu.Lock()
			if spent+reserved+room > e.Budget || ctx.Err() != nil {
				for _, arm := range e.arms() {
					tr := e.newTrial(t, n, arm)
					tr.Skipped = fmt.Sprintf("the evaluation's budget ($%.2f) has no room for all arms, or it was stopped", e.Budget)
					out = append(out, tr)
				}
				mu.Unlock()
				inputs[i].release()
				<-sem
				continue
			}
			reserved += room
			mu.Unlock()
			wg.Add(1)
			go func(t GoldenTask, n, i int) {
				defer wg.Done()
				defer func() { <-sem }()
				defer inputs[i].release()
				in := inputs[i]
				in.once.Do(func() { e.makeTask(ctx, in, t, repositories) })
				for position, arm := range armOrder(e.arms(), n, i) {
					tr := e.trial(ctx, in, t, n, arm, position+1)
					mu.Lock()
					reserved -= perArm
					spent += tr.Run.USD + tr.JudgeUSD
					out = append(out, tr)
					mu.Unlock()
				}
			}(t, n, i)
		}
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N < out[j].N
		}
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].Arm < out[j].Arm
	})
	return out, nil
}

func uniqueStrings(xs []string) []string {
	var out []string
	for _, x := range xs {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}

func (e *Eval) newTrial(t GoldenTask, n int, arm string) Trial {
	return Trial{Task: t.ID, Family: t.Family, N: n, Arm: arm, Key: digest(fmt.Sprintf("%s\x00%s\x00%d", e.ID, t.ID, n))[:16], Provenance: e.provenance()}
}

func (e *Eval) armDir(tr Trial) string {
	return filepath.Join(e.Dir, tr.Key, digest(tr.Key + "\x00" + tr.Arm)[:16])
}

func (e *Eval) trial(ctx context.Context, in *taskInputs, t GoldenTask, n int, arm string, position int) (tr Trial) {
	tr = e.newTrial(t, n, arm)
	tr.Position, tr.EndOfStore = position, in.end
	tr.Inputs, tr.InputHashes = in.manifest, in.hashes
	tr.TacitOffered = in.manifest.Tacit.Commit != ""
	tr.TacitCommit, tr.TacitErr = in.manifest.Tacit.Commit, in.manifest.Tacit.Error
	if in.manifest.Records.Stopped != "" {
		tr.ComparisonExcluded = "records capture reached its bound: " + in.manifest.Records.Stopped
	}
	dir := e.armDir(tr)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		tr.Err = "trial directory already exists or cannot be inspected"
		return tr
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		tr.Err = err.Error()
		return tr
	}
	defer func() {
		saved := tr
		saved.Task = ""
		raw, err := json.MarshalIndent(saved, "", "  ")
		if err == nil {
			err = writeOwn(filepath.Join(HostDir(dir), "trial.json"), raw)
		}
		if err != nil {
			tr.Err = err.Error()
		}
	}()
	if err := hostDir(HostDir(dir)); err != nil {
		tr.Err = err.Error()
		return tr
	}
	if in.err != nil {
		tr.Err = in.err.Error()
		return tr
	}
	private, err := os.MkdirTemp(HostDir(dir), ".case-")
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	defer removeInputTree(private)
	db := filepath.Join(private, "momentum.db")
	if err := copyStore(in.store, db); err != nil {
		tr.Err = "the case store: " + err.Error()
		return tr
	}
	snap, err := store.Open(db)
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	defer snap.Close()
	f := store.Finding{Family: t.Family, Signature: "eval/" + tr.Key, Title: t.CaseTitle}
	for _, id := range t.Symptom.Query.EventIDs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + strings.TrimPrefix(id, "ev:")})
	}
	c, _, err := snap.ObserveCase(ctx, f, "eval")
	if err != nil {
		tr.Err = "the case: " + err.Error()
		return tr
	}
	inv, err := e.trialInvocation(ctx, in, t, c, snap, dir, arm)
	if err != nil {
		tr.Err = "the investigation did not start: " + err.Error()
		return tr
	}
	tr.PromptHash = digest(inv.Prompt)
	tr.Provenance.Systems[arm] = digest(inv.System)
	run := store.LoopRun{ID: inv.ID, Case: c.ID, CaseEpisode: c.Episode, Family: t.Family,
		Step: StepInvestigate, Started: time.Now(), Model: e.Runner.Agent.Model, Dir: dir,
		Experiment: "eval:" + tr.Key + ":" + arm, Status: store.RunRunning, USD: e.Runner.Budgets.Run}
	if err := e.Runner.Store.PutLoopRun(ctx, run); err != nil {
		tr.Err = "record the investigation reservation: " + err.Error()
		return tr
	}
	agent := e.Runner.Agent
	agent.Env, agent.Binds = nil, nil
	res, runErr := agent.RunOrFallback(ctx, inv)
	run, started := settleRun(run, res, runErr, e.Runner.Budgets.Run)
	tr.Run = run
	if err := e.Runner.Store.PutLoopRun(context.WithoutCancel(ctx), run); err != nil {
		tr.Err = "record the investigation completion: " + err.Error()
		return tr
	}
	if !started {
		why := res.Failure
		if runErr != nil {
			why = runErr.Error()
		}
		tr.Err = "the investigation did not start: " + why
		return tr
	}
	tr.CutShort = res.Status == store.RunBudget || res.Status == store.RunTurns || res.Status == store.RunTimeout || len(res.Report) == 0
	found, reported, named := investigationFindings(res, dir)
	found, err = cleanFinding(ctx, found)
	if err != nil {
		tr.Err = "the investigation's findings: " + err.Error()
		return tr
	}
	if arm == "loop" {
		if why := undecided(res, runErr); named || why != "" {
			if _, _, err := recordFindings(ctx, snap, c.ID, run.CaseEpisode, found, reported, why, "eval:"+run.ID); err != nil {
				tr.Err = "the case's findings: " + err.Error()
				return tr
			}
		}
	}
	after, err := snap.CaseLive(ctx, c.ID)
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	tr.Status = string(after.Status)
	if after.Reason != "" {
		tr.Status += " " + string(after.Reason)
	}
	if arm == "loop" {
		found = loopFinding(after, found)
	} else {
		tr.Status = "native"
	}
	raw, err := json.MarshalIndent(caseRecord(after), "", "  ")
	if err == nil {
		err = writeOwn(filepath.Join(HostDir(dir), "case.json"), raw)
	}
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	tr.Mechanism = found.Mechanism
	tr.JudgeDir = filepath.Join(e.Dir, "judge", rand.Text()[:12])
	tr.JudgePromptHash = digest(judgePrompt(t, found))
	tr.Grade, tr.JudgeUSD, tr.JudgeModel, err = e.grade(ctx, t, found, tr.JudgeDir, run)
	if err != nil {
		tr.Err = "the judge: " + err.Error()
	}
	return tr
}

func (e *Eval) system() (string, error) {
	return investigationSystem(nil, e.Runner.Constraints)
}

func (e *Eval) trialInvocation(ctx context.Context, in *taskInputs, t GoldenTask, c store.Case, st *store.Store, dir, arm string) (Invocation, error) {
	system, err := e.system()
	if err != nil {
		return Invocation{}, err
	}
	evidence := ""
	if arm == "loop" {
		evidence, err = caseEvidence(ctx, st, c)
		if err != nil {
			return Invocation{}, err
		}
	} else {
		evidence = fmt.Sprintf("Symptom events: %s.\n", strings.Join(t.Symptom.Query.EventIDs, " "))
	}
	prompt := fmt.Sprintf("%s\nProblem: %s\n%s%s%s", liveQuestion, c.Title, evidence, findingsInstruction, inputReference())
	clean, err := RedactAll(ctx, []string{system, prompt})
	if err != nil {
		return Invocation{}, err
	}
	input := filepath.Join(HostDir(dir), "input")
	if err := hostDir(input); err != nil {
		return Invocation{}, err
	}
	evs, err := st.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		return Invocation{}, err
	}
	// Both arms receive the live symptom-only case file. It holds no answer.
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		return Invocation{}, err
	}
	raw, err := json.Marshal(struct {
		Case     store.Case        `json:"case"`
		Evidence []store.Evidence  `json:"evidence"`
		Log      []store.CaseEntry `json:"log"`
	}{c, evs, log})
	if err != nil {
		return Invocation{}, err
	}
	scrubbed, err := Redact(ctx, string(raw))
	if err != nil || !json.Valid([]byte(scrubbed)) {
		return Invocation{}, fmt.Errorf("scrubbed case is not JSON: %v", err)
	}
	if err := writeExclusive(input, "case.json", []byte(scrubbed)); err != nil {
		return Invocation{}, err
	}
	if err := writeExclusive(HostDir(dir), "prompt.txt", []byte(clean[1])); err != nil {
		return Invocation{}, err
	}
	return Invocation{ID: newRunID(time.Now()), Dir: dir, System: clean[0], Prompt: clean[1], Schema: investigationSchema,
		MaxTurns: e.Runner.Budgets.MaxTurns, BudgetUSD: e.Runner.Budgets.Run, Timeout: e.Runner.Budgets.RunTime,
		Binds: append([]sandbox.Bind(nil), in.binds...), unitPrefix: "momentum-run-eval-"}, nil
}

func (e *Eval) grade(ctx context.Context, t GoldenTask, f finding, jdir string, run store.LoopRun) (Grade, float64, string, error) {
	inv := Invocation{ID: newRunID(time.Now()), Dir: jdir, System: judgeSystem, Schema: gradeSchema, MaxTurns: 3,
		BudgetUSD: judgeBudget, Timeout: 5 * time.Minute, Prompt: judgePrompt(t, f), Tools: "none",
		unitPrefix: "momentum-run-eval-", inputOnly: true}
	if err := hostDir(HostDir(jdir)); err != nil {
		return Grade{}, 0, "", err
	}
	if err := writeExclusive(HostDir(jdir), "prompt.txt", []byte(inv.Prompt)); err != nil {
		return Grade{}, 0, "", err
	}
	judge := e.Judge
	judge.Fallback, judge.Env, judge.Binds = "", nil, nil
	spent, model := 0.0, ""
	var last error
	for attempt := 0; attempt < judgeAttempts; attempt++ {
		inv.fallback = attempt > 0
		start := time.Now()
		jr, err := judge.Run(ctx, inv)
		model = firstOf(jr.Model, model)
		jrun := store.LoopRun{ID: newRunID(time.Now()), Case: run.Case, Family: t.Family, Step: "judge",
			Started: start, Model: model, Dir: jdir, Experiment: run.Experiment}
		jrun, _ = settleRun(jrun, jr, err, judgeBudget)
		spent += jrun.USD
		if recordErr := e.Runner.Store.PutLoopRun(context.WithoutCancel(ctx), jrun); recordErr != nil {
			return Grade{}, spent, model, recordErr
		}
		if err != nil {
			last = err
			continue
		}
		g, gradeErr := decodeGrade(jr.Report)
		if gradeErr != nil {
			last = fmt.Errorf("no grade: %w", gradeErr)
			continue
		}
		if jrun.Status != store.RunOK {
			last = fmt.Errorf("no grade: %s %s", jrun.Status, jrun.Failure)
			continue
		}
		return g, spent, model, nil
	}
	return Grade{}, spent, model, last
}

func storedUsage(u Usage) store.Usage {
	return store.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite5 + u.CacheWrite1, CacheWrite1h: u.CacheWrite1}
}
