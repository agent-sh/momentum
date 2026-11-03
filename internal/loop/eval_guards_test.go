package loop

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestEvaluationGoldenRejectsMisspelledAndRepeatedKeys(t *testing.T) {
	e, task := evalFixture(t)
	raw, _ := json.Marshal(task)
	for _, text := range []string{
		strings.Replace(string(raw), `"wrong_paths"`, `"wrong_path"`, 1),
		strings.Replace(string(raw), `"event_ids"`, `"events"`, 1),
		strings.Replace(string(raw), `"case_title"`, `"Case_Title"`, 1),
		strings.TrimSuffix(string(raw), "}") + `,"id":"other"}`,
		strings.Replace(string(raw), `"repo":""`, `"repo":"example"`, 1),
	} {
		if _, err := ParseGolden([]byte(text), "fixture"); err == nil {
			t.Fatal("accepted malformed golden task")
		}
	}
	path := filepath.Join(t.TempDir(), "golden.jsonl")
	write(t, path, string(raw))
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Replace(string(raw), task.ID, "replacement", 1))
	tasks, err := ParseGolden(bytes, "fixture")
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatal("parsed tasks came from a second read")
	}
	e.GoldenHash = digest(string(bytes))
	if e.Results(nil).Provenance.GoldenHash != digest(string(raw)) {
		t.Fatal("golden digest and parsed bytes differ")
	}
}

func TestGradeRejectsCasingAndDuplicateMembers(t *testing.T) {
	for _, raw := range []string{
		`{"verdict":"missed","VERDICT":"reached","facts":[],"reason":"r"}`,
		`{"Verdict":"reached","Facts":[],"Reason":"r"}`,
		`{"verdict":"missed","verdict":"reached","facts":[],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"fact":"a","fact":"b","reached":true}],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"Fact":"a","reached":true}],"reason":"r"}`,
	} {
		if _, err := decodeGrade(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted a duplicate or differently cased member")
		}
	}
}

func TestAuthenticationDoesNotHideDuplicateJudgeMembers(t *testing.T) {
	raw := json.RawMessage(`{"verdict":"missed","verdict":"reached","facts":[],"reason":"r"}`)
	canonical, err := canonicalReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := Result{Report: raw}
	authenticateReport(&res, []json.RawMessage{canonical}, false)
	if res.Status != store.RunOK {
		t.Fatal("fixture did not authenticate")
	}
	if _, err := decodeGrade(res.Report); err == nil {
		t.Fatal("authentication hid the repeated key from the grade check")
	}
}

func TestBootstrapPinsSeedAndNinetyPercentWidth(t *testing.T) {
	pairs := map[string]map[string]Summary{}
	for _, id := range []string{"a", "b", "c", "d"} {
		pairs[id] = map[string]Summary{"loop": {USD: 2, TotalMinutes: 4}, "native": {USD: 2, TotalMinutes: 4}}
	}
	pairs["a"]["loop"] = Summary{USD: 2, TotalMinutes: 4, Reached: 1}
	c := compareTasks(pairs)
	if c.Seed != 1 || c.Resamples != 10000 {
		t.Fatalf("bootstrap controls: %+v", c)
	}
	if c.ReachedPerDollar == nil || c.ReachedPerDollar.Low != 0 || c.ReachedPerDollar.High != 3.0/8 ||
		c.ReachedPerMinute == nil || c.ReachedPerMinute.High != 3.0/16 {
		t.Fatalf("5th and 95th percentiles: %+v %+v", c.ReachedPerDollar, c.ReachedPerMinute)
	}
}

func TestStopRuleIgnoresFailedEvaluationRunsOfTheSameCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := openCase(t, st, "failures", "fixture", 1)
	r := &Runner{Store: st}
	for _, id := range []string{"a", "b"} {
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, CaseEpisode: c.Episode, Step: StepInvestigate,
			Status: store.RunFailed, Failure: "fixture", Started: time.Now().Add(-time.Minute), Ended: time.Now(),
			USD: 4, Experiment: "eval:fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("experiment stopped a live case: %s %v", why, err)
	}
}

func TestEvaluationPromptsUseTheLiveQuestionAndSharedPaths(t *testing.T) {
	e, task, in := preparedEval(t)
	st := testStore(t)
	ev := store.Event{Source: "fixture", SourceKey: "symptom", Kind: "tool_result", Actor: store.ActorTool, TS: in.end.Add(-time.Minute), Summary: "marker-7f3a"}
	if _, err := st.Put(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	task.Symptom.Query.EventIDs = []string{ev.ID()}
	c, _, err := st.ObserveCase(context.Background(), store.Finding{Family: task.Family, Signature: "fixture",
		Title: task.CaseTitle, Evidence: []store.Evidence{{Ref: "ev:" + ev.ID()}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var invs []Invocation
	for _, arm := range e.arms() {
		dir := filepath.Join(t.TempDir(), "run")
		inv, err := e.trialInvocation(context.Background(), in, task, c, st, dir, arm)
		if err != nil {
			t.Fatal(err)
		}
		if inv.unitPrefix != "momentum-run-eval-" || !strings.HasPrefix(inv.Prompt, liveQuestion) || !strings.HasSuffix(inv.Prompt, inputReference()) ||
			strings.Contains(inv.Prompt, task.RootCause) || strings.Contains(inv.Prompt, task.ID) {
			t.Fatal("trial question, reference or answer isolation differs")
		}
		invs = append(invs, inv)
	}
	if invs[0].System != invs[1].System || !reflect.DeepEqual(invs[0].Binds, invs[1].Binds) {
		t.Fatal("arms received different systems or read-only inputs")
	}
}

func TestLauncherWitnessCrossesChunksAndDoesNotAcceptDiagnostics(t *testing.T) {
	var output bytes.Buffer
	w := &launchWitness{writer: &output, marker: []byte("fixture-witness\n")}
	for _, part := range []string{"bwrap: failed\n", "fixture-wit", "ness\n"} {
		before := w.seen
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		if part == "bwrap: failed\n" && (before || w.seen) {
			t.Fatal("launcher failure was treated as sandbox entry")
		}
	}
	if !w.seen || output.String() != "bwrap: failed\nfixture-witness\n" {
		t.Fatal("startup witness or diagnostic archive lost")
	}
}

func TestEvaluationReservationWriteErrorStartsNoAgent(t *testing.T) {
	e, task, in := preparedEval(t)
	e.Runner.Store.Close()
	tr := e.trial(context.Background(), in, task, 1, "loop", 1)
	if !strings.Contains(tr.Err, "record the investigation reservation") || !tr.Run.Started.IsZero() || tr.Run.USD != 0 {
		t.Fatal("failed reservation was ignored")
	}
}

func TestEvaluationCompletionWriteErrorIsReported(t *testing.T) {
	e, task, in := preparedEval(t)
	writer, err := sql.Open("sqlite", e.Runner.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TRIGGER fixture_refuse_completion BEFORE UPDATE ON loop_runs
		WHEN NEW.status <> 'running' BEGIN SELECT RAISE(ABORT, 'fixture completion refused'); END`); err != nil {
		t.Fatal(err)
	}
	tr := e.trial(context.Background(), in, task, 1, "loop", 1)
	if !strings.Contains(tr.Err, "record the investigation completion") || tr.Run.USD != 0 {
		t.Fatal("failed completion write was ignored")
	}
}

func TestNeverStartedFallbackWithArchiveKeepsThePrimary(t *testing.T) {
	first := Result{Started: true, USD: .25, RelayUSD: .25, CostKnown: true, RelayAdmitted: 1, Model: "primary"}
	got := finishFallback(first, Result{Transcript: "fixture.jsonl", Model: "fallback"}, 3.75)
	if !got.Started || got.USD != .25 || got.ReservationUSD != 0 || got.Model != "primary" {
		t.Fatal("failed fallback startup lost or enlarged the primary charge")
	}
}

func TestEvaluationTrialTagIsStoredEvenWhenTheHarnessCannotStart(t *testing.T) {
	e, task, in := preparedEval(t)
	tr := e.trial(context.Background(), in, task, 1, "loop", 1)
	if tr.Run.ID == "" {
		t.Fatal("fixture did not reach reservation")
	}
	run, err := e.Runner.Store.LoopRunByID(context.Background(), tr.Run.ID)
	if err != nil || run.Experiment != "eval:"+tr.Key+":loop" {
		t.Fatal("investigation lost its experiment tag")
	}
	spend, err := e.Runner.Store.LoopSpend(context.Background(), time.Time{}, 4)
	if err != nil || spend != 0 {
		t.Fatal("failed evaluation entered live budgets")
	}
}

func TestEvaluationArmOrderDiffersByAtMostOneForOddK(t *testing.T) {
	for k := 1; k <= 6; k++ {
		for task := 0; task < 4; task++ {
			counts := map[string]int{}
			for n := 1; n <= k; n++ {
				counts[armOrder([]string{"loop", "native"}, n, task)[0]]++
			}
			if math.Abs(float64(counts["loop"]-counts["native"])) > 1 {
				t.Fatal("unbalanced arm order")
			}
		}
	}
}

func TestNoComparedTasksHaveUndefinedPassRatesAndKeepErrors(t *testing.T) {
	e, _ := evalFixture(t)
	r := e.Results([]Trial{{Task: "fixture", N: 1, Arm: "loop", Err: "fixture did not start"},
		{Task: "fixture", N: 1, Arm: "native"}})
	for _, a := range r.Arms {
		if a.PassAtK != nil || a.PassAllK != nil {
			t.Fatal("undefined pass rates became measurements")
		}
	}
	if r.Arms[0].Errors != 1 {
		t.Fatal("never-started failure was hidden by pairing")
	}
	raw, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(raw), `"PassAtK":null`) {
		t.Fatal("undefined pass rate did not serialize as null")
	}
}
