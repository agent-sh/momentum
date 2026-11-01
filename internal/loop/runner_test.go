package loop

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/agent-sh/momentum/internal/store"
)

// fakeAgent is a runner that ends with the result line given, as Claude Code's stream does, and keeps the
// arguments it was given in a file beside it.
func fakeAgent(t *testing.T, result string) []string {
	t.Helper()
	dir := t.TempDir()
	p := script(t, dir, "agent", `printf '%s\n' "$@" > "`+filepath.Join(dir, "args")+`"
env > "`+filepath.Join(dir, "env")+`"
echo '{"type":"system","subtype":"init"}'
echo '`+result+`'`)
	return []string{p}
}

func agentArgs(t *testing.T, runner []string) (string, string) {
	t.Helper()
	dir := filepath.Dir(runner[0])
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	return string(args), string(env)
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":0.75,"session_id":"s1",` +
	`"structured_output":{"case":"x","step":"investigate","outcome":"investigated","summary":"looked at it"},` +
	`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}`

func runnerFor(t *testing.T, st *store.Store, agent []string) *Runner {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.md")
	write(t, rules, "Delete with literal paths.")
	return &Runner{Store: st, Agent: Agent{Runner: agent, Model: "model-x"}, Ship: &Ship{Store: st, Dir: filepath.Join(dir, "loop")},
		Families: []string{"failures", "owner-burden"}, Budgets: Budgets{Run: 4, Case: 20, Day: 40, Week: 200, RunTime: time.Minute, MaxTurns: 30},
		Rules: []string{rules}, Constraints: []string{"never wake him"}, Dir: filepath.Join(dir, "loop"), Home: dir}
}

func openCase(t *testing.T, st *store.Store, family, sig string, n int) store.Case {
	t.Helper()
	f := store.Finding{Family: family, Signature: sig, Title: "case " + sig}
	for i := 0; i < n; i++ {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: fmt.Sprintf("file:/t/%s-%d", sig, i), At: time.Now().Add(-time.Duration(i+1) * time.Minute)})
	}
	c, _, err := st.ObserveCase(context.Background(), f, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A run takes each family's case one step: the one with the most evidence, in testing before investigating before
// open. The agent runs headless and restricted with the family's skill and the machine's rules in its system
// prompt, its file tools in its own directory, refused what no rule allows, with nothing of the caller's
// environment, and its cost goes on the case and the run.
func TestTheLoopTakesEachFamilysCaseOneStep(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, okResult)
	r := runnerFor(t, st, agent)
	r.ReadDirs = []string{"/srv/projects"}
	small := openCase(t, st, "failures", "a", 1)
	big := openCase(t, st, "failures", "b", 3)
	openCase(t, st, "sessions", "c", 5) // a family the loop does not run here
	t.Setenv("CLAUDE_CODE_PLUGIN_DIRS", "/should/not/pass")
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0].Case != big.ID || done[0].Step != StepInvestigate || done[0].Run.Status != store.RunOK || done[0].Run.USD != 0.75 {
		t.Fatalf("done %+v", done)
	}
	args, env := agentArgs(t, agent)
	for _, want := range []string{"--bare", "--restricted", "--tools\n" + DefaultTools, "--permission-mode\ndontAsk", "--model\nmodel-x",
		"--max-budget-usd\n4.00", "--max-turns\n30", "--strict-mcp-config", "--append-system-prompt-file", "--add-dir\n/srv/projects"} {
		if !strings.Contains(args, want) {
			t.Errorf("the agent's arguments lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(env, "CLAUDE_CODE_PLUGIN_DIRS") || !strings.Contains(env, "MOMENTUM_LOOP_CASE="+big.ID) {
		t.Errorf("the agent's environment:\n%s", env)
	}
	system, _ := os.ReadFile(filepath.Join(done[0].Run.Dir, "system.md"))
	for _, want := range []string{"# One run on one case", "# Failures", "never wake him", "Delete with literal paths."} {
		if !strings.Contains(string(system), want) {
			t.Errorf("the system prompt lacks %q", want)
		}
	}
	if strings.Contains(string(system), "# Owner burden") {
		t.Error("the system prompt holds another family's skill")
	}
	settings, _ := os.ReadFile(filepath.Join(done[0].Run.Dir, "settings.json"))
	for _, want := range []string{`"Write(/` + done[0].Run.Dir + `/**)"`, `"Bash(momentum case *)"`, `"Bash(momentum evidence file *)"`, `"Read(**/.env)"`} {
		if !strings.Contains(string(settings), want) {
			t.Errorf("an investigation's permissions lack %s: %s", want, settings)
		}
	}
	for _, not := range []string{"momentum ship propose", "journalctl", "systemctl", "tacit add", "///"} {
		if strings.Contains(string(settings), not) {
			t.Errorf("an investigation's permissions hold %s: %s", not, settings)
		}
	}
	if got, _ := st.CaseByID(ctx, big.ID); got.Cost.USD != 0.75 || got.Cost.Runs != 1 || got.Cost.Tokens != 100 {
		t.Fatalf("the case's cost %+v", got.Cost)
	}
	runs, _ := st.LoopRuns(ctx, store.RunFilter{})
	if len(runs) != 1 || runs[0].Case != big.ID || runs[0].Summary != "investigated: looked at it" || runs[0].Turns != 4 {
		t.Fatalf("runs %+v", runs)
	}
	if _, err := st.UpdateCase(ctx, small.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "test"); err != nil {
		t.Fatal(err)
	}
	c, step, _, _ := r.pick(ctx, "failures", "", false)
	if c.ID != small.ID || step != StepInvestigate {
		t.Fatalf("an investigating case comes before an open one: %s %s", c.ID, step)
	}
}

// A build's permissions add its work tree, the ship tool's building steps and its MCP servers' tools; a
// publish's, the lesson command; no other step has either.
func TestEachStepHasItsOwnPermissions(t *testing.T) {
	st := testStore(t)
	r := runnerFor(t, st, nil)
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	write(t, mcp, `{"mcpServers":{"web-tools":{"command":"x"}}}`)
	r.MCP = mcp
	c := store.Case{ID: "c1"}
	run := store.LoopRun{Dir: "/l/runs/r1"}
	build := strings.Join(r.permissions(StepBuild, c, run).Allow, " ")
	for _, want := range []string{"Write(/" + r.Ship.workDir("c1") + "/**)", "Bash(momentum ship propose *)", "mcp__web-tools"} {
		if !strings.Contains(build, want) {
			t.Errorf("a build lacks %s: %s", want, build)
		}
	}
	if pub := strings.Join(r.permissions(StepPublish, c, run).Allow, " "); !strings.Contains(pub, "Bash(momentum loop lesson *)") || strings.Contains(pub, "ship propose") {
		t.Errorf("a publish: %s", pub)
	}
	for _, step := range []string{StepInvestigate, StepReview} {
		if s := strings.Join(r.permissions(step, c, run).Allow, " "); strings.Contains(s, "ship propose") || strings.Contains(s, "lesson") || strings.Contains(s, "mcp__") {
			t.Errorf("%s: %s", step, s)
		}
	}
}

// A case whose last two runs failed the same way is not run a third time: the loop opens a case of its own, in
// component health, and goes on with the family's next case. A refusal of the budgets is no run of the case.
func TestALoopThatFailsTheSameWayTwiceStops(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1.5}`)
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 3)
	other := openCase(t, st, "failures", "b", 1)
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{"failures"}})
		if err != nil || done[0].Case != c.ID || done[0].Run.Status != store.RunTurns {
			t.Fatalf("run %d: %+v %v", i, done, err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || done[0].Case != other.ID {
		t.Fatalf("the third run worked %+v %v", done, err)
	}
	cs, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health"})
	if len(cs) != 1 || !strings.Contains(cs[0].Title, "turns") || cs[0].Evidence != 2 {
		t.Fatalf("the loop's own case %+v", cs)
	}
	// two refusals of the budgets stop nothing
	st2 := testStore(t)
	r2 := runnerFor(t, st2, agent)
	c2 := openCase(t, st2, "failures", "a", 3)
	for i := 0; i < 2; i++ {
		st2.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("r", i), Case: c2.ID, Family: "failures", Step: StepInvestigate,
			Started: time.Now().Add(-time.Duration(i+1) * time.Minute), Ended: time.Now(), Status: store.RunRefused, Failure: "the loop's day budget: $37.00 spent of $40"})
	}
	if got, _, why, _ := r2.pick(ctx, "failures", "", false); got.ID != c2.ID {
		t.Fatalf("two refusals stopped the case: %s", why)
	}
}

// No run starts when the day's spend, with the runs in flight counted at their whole budget, would pass the day's
// budget, and two families cannot both take its last room. A case whose budget has no room for a run's is passed
// over. A run whose cost is unknown counts its whole budget.
func TestTheBudgetsHoldTheRuns(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, okResult)
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 2)
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "r0", Case: "c0", Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-time.Hour),
		Ended: time.Now(), Status: store.RunOK, USD: 37}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || done[0].Run.Status != store.RunRefused || !strings.Contains(done[0].Skipped, "day budget") {
		t.Fatalf("over the day's budget: %+v %v", done, err)
	}
	if _, err := st.AddCaseCost(ctx, c.ID, store.Cost{USD: 17}, "test"); err != nil {
		t.Fatal(err)
	}
	if got, _, why, _ := r.pick(ctx, "failures", "", false); got.ID != "" || !strings.Contains(why, "spent $17.00") {
		t.Fatalf("a case with no room for a run picked: %s %s", got.ID, why)
	}
	// the day has room for one run: of three families, one starts
	st2 := testStore(t)
	r2 := runnerFor(t, st2, fakeAgent(t, okResult))
	r2.Families = []string{"failures", "owner-burden", "sessions"}
	r2.Budgets.Day = 5
	for _, fam := range r2.Families {
		openCase(t, st2, fam, fam+"/x", 1)
	}
	done, err = r2.Once(ctx, Options{})
	started := 0
	for _, d := range done {
		if d.Run.Status == store.RunOK {
			started++
		}
	}
	if err != nil || started != 1 {
		t.Fatalf("%d runs started on room for one: %+v %v", started, done, err)
	}
	// a run that wrote no result spent its whole budget, as far as the budgets know
	st3 := testStore(t)
	r3 := runnerFor(t, st3, fakeAgent(t, `{"type":"system","subtype":"init"}`))
	c3 := openCase(t, st3, "failures", "a", 1)
	done, err = r3.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || done[0].Run.USD != 4 {
		t.Fatalf("a run with no cost known: %+v %v", done, err)
	}
	if got, _ := st3.CaseByID(ctx, c3.ID); got.Cost.USD != 4 {
		t.Fatalf("its case's cost %+v", got.Cost)
	}
}

// How a run ended comes from its stream's result: ok with its report, its limit, or failed by its kind.
func TestARunsResultSaysHowItEnded(t *testing.T) {
	for _, c := range []struct {
		subtype           string
		isError, reported bool
		status, failure   string
	}{
		{"success", false, true, store.RunOK, ""},
		{"success", false, false, store.RunFailed, "no report"},
		{"error_max_turns", true, false, store.RunTurns, "turns"},
		{"error_max_budget_usd", true, false, store.RunBudget, "budget"},
		{"success", true, false, store.RunFailed, "error"},
		{"error_during_execution", true, false, store.RunFailed, "error_during_execution"},
	} {
		st, f := resultStatus(c.subtype, c.isError, c.reported)
		if st != c.status || f != c.failure {
			t.Errorf("%s %v %v: %s %q, want %s %q", c.subtype, c.isError, c.reported, st, f, c.status, c.failure)
		}
	}
}

// A run the model's safeguards refuse runs once more on the fallback model, and its result says which ran and
// what both cost.
func TestASafeguardsRefusalFallsBack(t *testing.T) {
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	p := script(t, dir, "agent", `case "$*" in *"--model model-a"*) echo '`+refused+`' ;; *) echo '`+okResult+`' ;; esac`)
	a := Agent{Runner: []string{p}, Model: "model-a", Fallback: "model-b"}
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK || res.Model != "model-b" || res.USD != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

// reviewable is a work tree with a change to review: a commit on origin/main's.
func reviewable(t *testing.T) (string, string) {
	t.Helper()
	work := t.TempDir()
	run(t, work, "git", "init", "-q", "-b", "main")
	write(t, filepath.Join(work, "x.go"), "package x\n")
	run(t, work, "git", "add", "-A")
	run(t, work, "git", "commit", "-q", "-m", "base")
	run(t, work, "git", "update-ref", "refs/remotes/origin/main", "HEAD")
	write(t, filepath.Join(work, "x.go"), "package x\n\n// X is x.\nconst X = 1\n")
	run(t, work, "git", "commit", "-q", "-am", "change")
	return work, strings.TrimSpace(run(t, work, "git", "rev-parse", "HEAD"))
}

// A run stopped at its time takes every process it started with it, those that ignore its TERM too.
func TestATimedOutRunLeavesNothingRunning(t *testing.T) {
	was := stopGrace
	stopGrace = time.Second
	defer func() { stopGrace = was }()
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")
	p := script(t, dir, "agent", `trap '' TERM
(trap '' TERM; sleep 3; touch "`+marker+`") &
sleep 30`)
	a := Agent{Runner: []string{p}, Model: "m"}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Second})
	if err != nil || res.Status != store.RunTimeout {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a process of the run outlived it")
	}
	// a leader that ends on its TERM leaves none of its group behind, those that ignore TERM neither
	marker2 := filepath.Join(dir, "survived2")
	p2 := script(t, dir, "agent2", `(trap '' TERM; sleep 3; touch "`+marker2+`") &
sleep 30`)
	res, err = Agent{Runner: []string{p2}, Model: "m"}.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run2"), Schema: runSchema, MaxTurns: 3,
		BudgetUSD: 1, Timeout: time.Second})
	if err != nil || res.Status != store.RunTimeout {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker2); err == nil {
		t.Fatal("a process of the run's group outlived its leader")
	}
}

// In its scope a run's stop reaches the processes that left its process group too.
func TestARunsScopeTakesWhatLeftItsGroup(t *testing.T) {
	if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "true").Run(); err != nil {
		t.Skip("no user systemd here")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")
	p := script(t, dir, "agent", `setsid sh -c 'sleep 3; touch "`+marker+`"' > /dev/null 2>&1 < /dev/null &
echo '`+okResult+`'`)
	a := Agent{Runner: []string{p}, Model: "m", Scope: true}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run-"+newRunID(time.Now())), Schema: runSchema, MaxTurns: 3,
		BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a process that left the run's group outlived it")
	}
	if ScopeUnit(filepath.Join(dir, "a", "1")) == ScopeUnit(filepath.Join(dir, "b", "1")) {
		t.Fatal("two runs' directories of one name share a scope")
	}
}

// A review run is a run: the stop rule and the case's budget hold it, and its counts are its findings'. A review
// that finds something sends the change back to its build, with the review beside the build's run; one that finds
// nothing but is not posted lands nothing.
func TestAReviewIsHeldAsARunIsAndGoesBackToTheBuild(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	review := `{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":1,` +
		`"structured_output":{"summary":"one problem","findings":[{"severity":"medium","where":"x.go:1","problem":"p","fix":"f"}]}}`
	r := runnerFor(t, st, fakeAgent(t, review))
	c := testingCase(t, st)
	work, head := reviewable(t)
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", Work: work, State: Proposed, Head: head, PR: "https://x/pull/1"}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	posted := ""
	r.Post = func(_ context.Context, repo, pr, body string) (string, error) { posted = body; return pr + "#c", nil }
	r.Ship.Public = func(_ context.Context, s string) (string, error) { return Neutral(s), nil }
	done := r.reviews(ctx, "")
	if len(done) != 1 || done[0].Step != StepReview {
		t.Fatalf("reviews %+v", done)
	}
	got, _, _ := r.Ship.Load(c.ID)
	if got.State != Revising || got.Reviews[0].Medium != 1 || got.Reviews[0].Comment == "" || !strings.Contains(posted, "Not ready to merge: 0 High, 1 Medium") {
		t.Fatalf("after the review %+v, posted %q", got, posted)
	}
	pc, step, _, _ := r.pick(ctx, "failures", "", false)
	if pc.ID != c.ID || step != StepBuild {
		t.Fatalf("a revising change goes back to its build: %s %s", pc.ID, step)
	}
	r.Agent.Runner = fakeAgent(t, okResult)
	built, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(built) == 0 || built[0].Step != StepBuild {
		t.Fatalf("the build after its review: %+v %v", built, err)
	}
	if b, _ := os.ReadFile(filepath.Join(built[0].Run.Dir, "review.md")); !strings.Contains(string(b), "Not ready to merge") {
		t.Fatalf("the build's run has no review: %q", b)
	}
	if args, _ := agentArgs(t, r.Agent.Runner); !strings.Contains(args, "review.md") {
		t.Fatalf("the build's prompt names no review:\n%s", args)
	}
	// two review runs failing the same way stop the third, and a case without room for one gets none
	st2 := testStore(t)
	r2 := runnerFor(t, st2, fakeAgent(t, `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":3.5,"result":"the safeguards blocked it"}`))
	c2 := testingCase(t, st2)
	p2 := Package{Case: c2.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", Work: work, State: Proposed, Head: head, PR: "https://x/pull/1"}
	r2.Ship.save(&p2)
	for i := 0; i < 2; i++ {
		if d := r2.reviews(ctx, ""); len(d) != 1 || d[0].Run.Status != store.RunFailed {
			t.Fatalf("review run %d: %+v", i, d)
		}
	}
	if d := r2.reviews(ctx, ""); len(d) != 1 || !strings.Contains(d[0].Skipped, "failed the same way") {
		t.Fatalf("a third review: %+v", d)
	}
	st3 := testStore(t)
	r3 := runnerFor(t, st3, fakeAgent(t, review))
	c3 := testingCase(t, st3)
	st3.AddCaseCost(ctx, c3.ID, store.Cost{USD: 17}, "test")
	p3 := Package{Case: c3.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", Work: work, State: Proposed, Head: head, PR: "https://x/pull/1"}
	r3.Ship.save(&p3)
	if d := r3.reviews(ctx, ""); len(d) != 1 || !strings.Contains(d[0].Skipped, "no room") {
		t.Fatalf("a review over the case's budget: %+v", d)
	}
}

// A dry run picks and says what it would run, and writes nothing: no run ended, no case of its own opened.
func TestADryRunWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "a", 3)
	for i := 0; i < 2; i++ {
		st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("f", i), Case: c.ID, Family: "failures", Step: StepInvestigate,
			Started: time.Now().Add(-time.Duration(i+1) * time.Minute), Ended: time.Now(), Status: store.RunTurns, Failure: "turns"})
	}
	st.PutLoopRun(ctx, store.LoopRun{ID: "old", Case: "c0", Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-5 * time.Hour), Status: store.RunRunning})
	done, err := r.Once(ctx, Options{Dry: true, Families: []string{"failures"}})
	if err != nil || len(done) != 1 || !strings.Contains(done[0].Skipped, "stopped") {
		t.Fatalf("dry %+v %v", done, err)
	}
	if cs, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health"}); len(cs) != 0 {
		t.Fatalf("a dry run opened %d cases", len(cs))
	}
	if run, _ := st.LoopRunByID(ctx, "old"); run.Status != store.RunRunning {
		t.Fatalf("a dry run ended a run: %+v", run)
	}
	db, _ := sql.Open("sqlite", "file:"+path)
	defer db.Close()
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM loop_runs`).Scan(&n)
	if n != 3 {
		t.Fatalf("a dry run recorded runs: %d", n)
	}
}

// Of two families' cases ready to build, one builds in a run: a build's work tree is uncommitted work, which the
// scope check of the other reads as someone's.
func TestOneCaseBuildsAtATime(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, okResult))
	for _, fam := range []string{"failures", "owner-burden"} {
		c := openCase(t, st, fam, fam+"/x", 1)
		mech, next := "m", "n"
		if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	done, err := r.Once(ctx, Options{})
	builds, waits := 0, 0
	for _, d := range done {
		switch {
		case d.Step == StepBuild && d.Run.Status == store.RunOK:
			builds++
		case d.Step == StepBuild && strings.Contains(d.Skipped, "another case builds"):
			waits++
		}
	}
	if err != nil || builds != 1 || waits != 1 {
		t.Fatalf("%d builds, %d waiting: %+v %v", builds, waits, done, err)
	}
}

// A run killed with the process that ran it is ended on the next loop run, its whole budget counted; and a run the
// model's safeguards refuse runs on the fallback model, which the run records.
func TestAStaleRunCountsItsBudgetAndARefusalFallsBack(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	p := script(t, dir, "agent", `case "$*" in *"--model model-x"*) echo '`+refused+`' ;; *) echo '`+okResult+`' ;; esac`)
	r := runnerFor(t, st, []string{p})
	r.Agent.Fallback = "model-y"
	c := openCase(t, st, "failures", "a", 2)
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "stale", Case: "c0", Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-3 * time.Hour),
		Status: store.RunRunning}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Case != c.ID || done[0].Run.Status != store.RunOK || done[0].Run.Model != "model-y" || done[0].Run.USD != 1 {
		t.Fatalf("done %+v %v", done, err)
	}
	if run, _ := st.LoopRunByID(ctx, "stale"); run.Status != store.RunFailed || run.Failure != "killed" || run.USD != r.Budgets.Run {
		t.Fatalf("the stale run %+v", run)
	}
}
