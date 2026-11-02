package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A live investigation is the native path's: it is shown the directories the loop reads, its door runs the evidence
// commands and no case command, and the runner, not the agent, writes its report into the case.
func TestALiveInvestigationHasNoCaseCommandAndNamesWhatItReads(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	outside := t.TempDir()
	log := filepath.Join(outside, "ran")
	momentum := script(t, outside, "momentum", `echo "momentum $*" >> `+log+`
echo "momentum answered $1"`)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	harness := script(t, dir, "harness", `printf '%s\n' "$@" > `+filepath.Join(dir, "args")+`
momentum evidence status >> `+out+` 2>&1; echo "status=$?" >> `+out+`
for c in 'case show '"$MOMENTUM_LOOP_CASE" 'case list' 'case set '"$MOMENTUM_LOOP_CASE"' --status testing' 'case close '"$MOMENTUM_LOOP_CASE"; do
  momentum $c >> `+out+` 2>&1; echo "$c => $?" >> `+out+`
done
`+modelResult(investigatedResult))
	r := runnerFor(t, st, []string{harness})
	r.Momentum = momentum
	r.ReadDirs = []string{"/srv/projects"}
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
		t.Fatalf("%+v %v", done, err)
	}
	got, _ := os.ReadFile(out)
	ran, _ := os.ReadFile(log)
	for _, want := range []string{"momentum answered evidence", "status=0", "case show " + c.ID + " => 126", "case list => 126",
		"case set " + c.ID + " --status testing => 126", "case close " + c.ID + " => 126"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the harness read %q, want %q", got, want)
		}
	}
	if strings.Contains(string(ran), "case") {
		t.Errorf("the door ran a case command for an investigation:\n%s", ran)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "/srv/projects") || !strings.Contains(string(args), "momentum evidence git DIR CMD") {
		t.Errorf("the prompt does not name the directories evidence git reads:\n%s", args)
	}
	if after, _ := st.CaseByID(ctx, c.ID); after.Status != store.CaseTesting {
		t.Errorf("the runner did not move the case on its report: %+v", after)
	}
}

// An investigation is shown what its case holds from earlier runs, its record to check, which its report replaces.
func TestAnInvestigationIsShownWhatItsCaseHolds(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := fakeAgent(t, investigatedResult)
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 2)
	note, fix := "not settled: the next investigation must zz-settle-the-lock-order-against-the-disk", "take the lock first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &note, NextTest: &fix, Alternatives: []string{"a slow disk"}}, "loop:build"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	args, _ := agentArgs(t, agent)
	if !strings.Contains(args, "zz-settle-the-lock-order-against-the-disk") || !strings.Contains(args, "alternative: a slow disk") || !strings.Contains(args, "its record") {
		t.Fatalf("the prompt lacks the case's record:\n%s", args)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting || got.Mechanism == note {
		t.Fatalf("the report did not replace the record: %+v", got)
	}
}

// A live investigation cut short keeps what it found: the runner records its findings file into the case, which stays
// investigating (only a report moves it to testing), and the second stop in a row trips the stop rule.
func TestTheRunnerRecordsAnInvestigationsFindings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := []string{script(t, t.TempDir(), "unfinished", `cat > findings.md <<'NOTES'
## Mechanism
the export reads the lockfile before it takes the lock
## Alternatives ruled out
a slow disk
## Fix
take the lock first
NOTES
`+modelResult(`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1.5}`)+`
`)}
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunTurns {
		t.Fatalf("%+v %v", done, err)
	}
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Status != store.CaseInvestigating || got.Mechanism != "the export reads the lockfile before it takes the lock" ||
		got.NextTest != "take the lock first" || len(got.Alternatives) != 1 || got.Alternatives[0] != "a slow disk" {
		t.Fatalf("the case after a run cut short: %+v %v", got, err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	if len(log) == 0 || log[len(log)-1].Actor != "loop:"+done[0].Run.ID {
		t.Fatalf("the change is the run's: %+v", log)
	}
	// the second investigation, shown what the first left, stops the same way: the stop rule holds the case
	if done, err = r.Once(ctx, Options{Families: []string{"failures"}}); err != nil || len(done) != 1 || done[0].Run.Status != store.RunTurns {
		t.Fatalf("second: %+v %v", done, err)
	}
	if done, err = r.Once(ctx, Options{Families: []string{"failures"}}); err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped") {
		t.Fatalf("a third run after two stops: %+v %v", done, err)
	}
}

// The runner records findings only into the case the run investigated, while it is open or investigating: never into
// the case it went into, nor one moved to testing or closed while it ran; a report with a mechanism and no fix leaves
// it investigating.
func TestRecordFindingsKeepsToItsOwnOpenCase(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a, b, moved, closed, short := openCase(t, st, "failures", "a", 1), openCase(t, st, "failures", "b", 1), openCase(t, st, "failures", "m", 1),
		openCase(t, st, "failures", "c", 1), openCase(t, st, "failures", "s", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:shared", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCaseSignature(ctx, a.ID, "root:shared", "test"); err != nil { // a goes into b
		t.Fatal(err)
	}
	mech, next := "the lock order", "take the lock first"
	if _, err := st.UpdateCase(ctx, moved.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseCase(ctx, closed.ID, store.NotWorthIt, "", "fixture", "test"); err != nil {
		t.Fatal(err)
	}
	f := finding{Mechanism: "zz-run-mechanism", Fix: "zz-run-fix"}
	for _, id := range []string{a.ID, moved.ID, closed.ID} {
		if _, recorded, err := recordFindings(ctx, st, id, 1, f, true, "", "loop:r"); err != nil || recorded {
			t.Errorf("%s: recorded %v %v", id, recorded, err)
		}
	}
	for _, id := range []string{b.ID, moved.ID, closed.ID} {
		if got, _ := st.CaseByID(ctx, id); strings.Contains(got.Mechanism, "zz-run") {
			t.Errorf("%s took another run's findings: %+v", id, got)
		}
	}
	if got, recorded, err := recordFindings(ctx, st, short.ID, short.Episode, finding{Mechanism: "zz-only-a-mechanism"}, true, "", "loop:r"); err != nil || !recorded ||
		got.Status != store.CaseInvestigating || got.NextTest != "" {
		t.Fatalf("a report with no fix: %+v %v %v", got, recorded, err)
	}
}

// A killed investigation's findings file reaches its case when stale recovery ends the run.
func TestAKilledInvestigationKeepsItsFindings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "a", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	os.MkdirAll(run.Dir, 0o700)
	write(t, filepath.Join(run.Dir, "findings.md"), "## Mechanism\nthe export reads the lockfile first\n## Fix\ntake the lock first\n")
	later := time.Now().Add(r.Budgets.RunTime + time.Hour)
	r.Now = func() time.Time { return later }
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseInvestigating || got.Mechanism != "the export reads the lockfile first" {
		t.Fatalf("the killed run's findings: %+v", got)
	}
}

// A loop that dies after its agent ended leaves the host's authenticated result: stale recovery records the report, not
// the findings file, so one that named a mechanism and a fix moves the case to testing.
func TestStaleRecoveryRecordsAKilledRunsReport(t *testing.T) {
	for _, stream := range []string{"transcript.jsonl", "transcript.fallback.jsonl"} {
		t.Run(stream, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			r := localRunner(t, st)
			c := openCase(t, st, "failures", "a", 1)
			run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
			if why != "" {
				t.Fatal(why)
			}
			write(t, filepath.Join(run.Dir, "findings.md"), "## Mechanism\nstill settling it\n")
			forged := strings.ReplaceAll(investigatedResult, "take the lock first", "the run forged this fix")
			write(t, filepath.Join(run.Dir, stream), forged+"\n")
			if stream == "transcript.fallback.jsonl" {
				write(t, filepath.Join(HostDir(run.Dir), "transcript.jsonl"), forged+"\n")
			}
			write(t, filepath.Join(HostDir(run.Dir), stream), investigatedResult+"\n")
			authenticatedFixture(t, run.Dir, stream, investigatedResult)
			later := run.Started.Add(r.Budgets.RunTime + time.Hour)
			r.Now = func() time.Time { return later }
			if err := r.endStale(ctx); err != nil {
				t.Fatal(err)
			}
			got, _ := st.CaseByID(ctx, c.ID)
			if got.Status != store.CaseTesting || got.Mechanism != "the export reads the lockfile before it takes the lock" || got.NextTest != "take the lock first" {
				t.Fatalf("after stale recovery: %s, mechanism %q, fix %q", got.Status, got.Mechanism, got.NextTest)
			}
			saved, err := st.LoopRunByID(ctx, run.ID)
			if err != nil || saved.Failure != "killed" || saved.USD != r.Budgets.Run || got.Cost.USD != r.Budgets.Run {
				t.Fatalf("stale recovery lost its charge: %+v %+v %v", saved, got.Cost, err)
			}
		})
	}
}

// The findings are recorded before the run ends: a run whose end cannot be recorded still leaves them.
func TestFindingsComeBeforeTheRunsEnd(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "a", 1)
	run := store.LoopRun{ID: "unreserved", Case: c.ID, Family: c.Family, Step: StepInvestigate, Started: time.Now(), Dir: t.TempDir(), CaseEpisode: c.Episode}
	res := Result{Started: true, Status: store.RunOK, CostKnown: true, USD: 1,
		Report: []byte(`{"mechanism":"the export reads the lockfile first","alternatives":[],"fix":"take the lock first","summary":"found"}`)}
	r.finish(ctx, c, &run, res, nil)
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("a run whose end failed lost its findings: %+v", got)
	}
}

// An investigation whose run ends in an error after it kept findings (here its fallback model cannot start once the
// safeguards refused the first) still leaves them in its case.
func TestAnInvestigationThatErredKeepsItsFindings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	agent := script(t, t.TempDir(), "refused-once", `cat > findings.md <<'NOTES'
## Mechanism
the export reads the lockfile first
## Fix
take the lock first
NOTES
rmdir tmp || exit 1
ln -s /fixture/absent tmp || exit 1
`+modelResult(refused))
	r := runnerFor(t, st, []string{agent})
	r.Agent.Fallback = "model-y"
	c := openCase(t, st, "failures", "a", 1)
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if len(runs) != 1 || runs[0].Status != store.RunFailed || runs[0].Failure != "run" || !strings.Contains(runs[0].Summary, "run directory tmp is not a directory") || runs[0].USD != 0.25 {
		t.Fatalf("the run did not end in an error: %+v", runs)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Mechanism != "the export reads the lockfile first" || got.NextTest != "take the lock first" || got.Status != store.CaseInvestigating {
		t.Fatalf("the case holds %q, %q, %s", got.Mechanism, got.NextTest, got.Status)
	}
}

// A run cut short sets only what its findings file holds: a file with only a mechanism keeps the fix and the
// alternatives the case held from an earlier run.
func TestARunCutShortKeepsWhatItsFileDoesNotHold(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := []string{script(t, t.TempDir(), "unfinished", `cat > findings.md <<'NOTES'
## Mechanism
still settling the disk against the lock order
NOTES
`+modelResult(`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1.5}`)+`
`)}
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 1)
	mech, fix := "the export reads the lockfile before it takes the lock; the disk is left open", "take the lock first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &mech, NextTest: &fix, Alternatives: []string{"a slow disk"}}, "loop:build"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Mechanism != "still settling the disk against the lock order" || got.NextTest != fix || len(got.Alternatives) != 1 {
		t.Fatalf("the case holds %q, next test %q, alternatives %q", got.Mechanism, got.NextTest, got.Alternatives)
	}
}

// An investigation that ended well with a report short of a mechanism or a fix did not decide: its findings go into
// the case, which closes as insufficient evidence (no stop, no case of the loop's own), and comes back once as much
// evidence again comes in, to be investigated again. A report naming neither takes its findings file's.
func TestAnUndecidedInvestigationClosesItsCase(t *testing.T) {
	for name, report := range map[string]string{
		"a mechanism and no fix": `{"mechanism":"the export reads the lockfile before it takes the lock","alternatives":[],"fix":"","summary":"no fix within reach: the evidence cannot decide"}`,
		"neither, a full file":   `{"mechanism":"","alternatives":[],"fix":"","summary":"the evidence cannot decide"}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			dir := t.TempDir()
			result := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":1,"session_id":"s1","structured_output":` + report + `}`
			p := script(t, dir, "agent", `printf '## Mechanism\nthe export reads the lockfile first\n## Fix\ntake the lock first\n' > findings.md
echo '{"type":"system","subtype":"init"}'
`+modelResult(result))
			r := runnerFor(t, st, []string{p})
			c := openCase(t, st, "failures", "a", 2)
			done, err := r.Once(ctx, Options{Families: []string{"failures"}})
			if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
				t.Fatalf("%+v %v", done, err)
			}
			got, _ := st.CaseByID(ctx, c.ID)
			health, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health", All: true})
			if got.Status != store.CaseClosed || got.Reason != store.Insufficient || !strings.Contains(got.Mechanism, "lockfile") || len(health) != 0 {
				t.Fatalf("the case: %s %s, mechanism %q; loop cases %d", got.Status, got.Reason, got.Mechanism, len(health))
			}
			if name == "neither, a full file" && got.NextTest != "take the lock first" {
				t.Errorf("the findings file's fix: %q", got.NextTest)
			}
			time.Sleep(5 * time.Millisecond)
			more := store.Finding{Family: "failures", Signature: "a", Title: "case a"}
			for i := 0; i < 2; i++ {
				more.Evidence = append(more.Evidence, store.Evidence{Ref: "file:/t/a-later-" + string(rune('0'+i)), At: time.Now()})
			}
			if _, _, err := st.ObserveCase(ctx, more, "test"); err != nil {
				t.Fatal(err)
			}
			if back, _ := st.CaseByID(ctx, c.ID); back.Status != store.CaseOpen {
				t.Fatalf("with as much evidence again the case is %s", back.Status)
			}
			done, err = r.Once(ctx, Options{Families: []string{"failures"}})
			if err != nil || len(done) != 1 || done[0].Step != StepInvestigate || done[0].Run.ID == "" {
				t.Fatalf("the case that came back: %+v %v", done, err)
			}
		})
	}
}

// The stop rule counts only the runs since a case last came back: a case its two failed runs stopped, closed by hand
// and back on new evidence, is investigated again.
func TestTheStopRuleCountsOnlyRunsSinceTheCaseCameBack(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	turns := `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1}`
	r := runnerFor(t, st, fakeAgent(t, turns))
	c := openCase(t, st, "failures", "a", 2)
	for i := 0; i < 2; i++ {
		if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
			t.Fatal(err)
		}
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why == "" {
		t.Fatalf("two runs that failed the same way: %q %v", why, err)
	}
	if _, err := st.CloseCase(ctx, c.ID, store.Insufficient, "", "held by the stop rule; let it come back", "cli"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	back := store.Finding{Family: "failures", Signature: "a", Title: "case a"}
	for i := 0; i < 3; i++ {
		back.Evidence = append(back.Evidence, store.Evidence{Ref: "file:/t/a-back-" + string(rune('0'+i)), At: time.Now()})
	}
	if _, _, err := st.ObserveCase(ctx, back, "test"); err != nil {
		t.Fatal(err)
	}
	reopened, _ := st.CaseByID(ctx, c.ID)
	if reopened.Status != store.CaseOpen {
		t.Fatalf("the case did not come back: %s", reopened.Status)
	}
	if why, err := r.stopped(ctx, reopened, true); err != nil || why != "" {
		t.Fatalf("the case that came back is still held: %q %v", why, err)
	}
}

// An empty alternative reaches neither arm's judge.
func TestAnEmptyAlternativeReachesNoJudge(t *testing.T) {
	if got := judgeFinding(finding{Mechanism: "m", Alternatives: []string{"disk latency", " "}, Fix: "f"}); strings.Contains(got, "disk latency | ") {
		t.Fatalf("%s", got)
	}
}
