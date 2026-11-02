package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func recordingRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{Store: testStore(t), Dir: t.TempDir(), Budgets: DefaultBudgets}
}

func TestStaleRecoveryKeepsAnUndecidedFinalReport(t *testing.T) {
	for _, mechanism := range []string{"marker-7f3a", ""} {
		t.Run(mechanism, func(t *testing.T) {
			ctx := context.Background()
			r := recordingRunner(t)
			c := openCase(t, r.Store, "failures", "symptom", 1)
			run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
			if why != "" {
				t.Fatal(why)
			}
			if err := os.MkdirAll(run.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			report, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success",
				"structured_output": map[string]any{"mechanism": mechanism, "fix": "", "alternatives": []string{}, "summary": "evidence cannot decide marker-7f3a"}})
			write(t, filepath.Join(HostDir(run.Dir), "transcript.jsonl"), string(report)+"\n")
			authenticatedFixture(t, run.Dir, "transcript.jsonl", string(report))
			later := run.Started.Add(r.Budgets.RunTime + time.Hour)
			r.Now = func() time.Time { return later }
			if err := r.endStale(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := r.Store.CaseByID(ctx, c.ID)
			if err != nil || got.Status != store.CaseClosed || got.Reason != store.Insufficient || !strings.Contains(got.Mechanism, mechanism) {
				t.Fatalf("completed undecided report lost: %+v %v", got, err)
			}
		})
	}
}

func TestAnEarlierInvestigationCannotChangeAReopenedEpisode(t *testing.T) {
	for _, completion := range []string{"findings", "undecided"} {
		t.Run(completion, func(t *testing.T) {
			ctx := context.Background()
			r := recordingRunner(t)
			c := openCase(t, r.Store, "failures", "symptom", 1)
			run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
			if why != "" {
				t.Fatal(why)
			}
			if _, err := r.Store.CloseCase(ctx, c.ID, store.Solved, "", "closed", "test"); err != nil {
				t.Fatal(err)
			}
			reopened, err := r.Store.ReopenCase(ctx, c.ID, []store.Evidence{{Ref: "file:/fixture/new", At: time.Now()}}, "new evidence", "test")
			if err != nil {
				t.Fatal(err)
			}
			fix := "old fix"
			if completion == "undecided" {
				fix = ""
			}
			report, _ := json.Marshal(map[string]any{"mechanism": "old cause", "fix": fix, "summary": "old episode"})
			res := Result{Started: true, Status: store.RunOK, Report: report, CostKnown: true, USD: 0.1}
			r.finish(ctx, c, &run, res, nil)
			got, err := r.Store.CaseByID(ctx, c.ID)
			if err != nil || got.Status != reopened.Status || got.Mechanism != reopened.Mechanism || got.NextTest != reopened.NextTest {
				t.Fatalf("old report changed reopened case: %+v %v", got, err)
			}
			if !near(got.Cost.USD, 0.1) {
				t.Fatalf("old run's cost was lost: %+v", got.Cost)
			}
		})
	}
}

func TestASandboxSetupFailureCostsNothing(t *testing.T) {
	run := store.LoopRun{Started: time.Now(), USD: 4}
	res := Result{Status: store.RunFailed, Failure: "sandbox", Transcript: "transcript.jsonl"}
	got, started := settleRun(run, res, nil, 4)
	if started || got.USD != 0 || !got.Started.IsZero() || got.Failure != "sandbox" {
		t.Fatalf("sandbox setup charged as a model run: %+v started=%v", got, started)
	}
}

func TestStaleRecoveryKeepsAnUnfinishedInvestigationOpen(t *testing.T) {
	ctx := context.Background()
	r := recordingRunner(t)
	c := openCase(t, r.Store, "failures", "symptom", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	os.MkdirAll(run.Dir, 0o700)
	write(t, filepath.Join(run.Dir, "findings.md"), "## Mechanism\nmarker-7f3a\n")
	later := run.Started.Add(r.Budgets.RunTime + time.Hour)
	r.Now = func() time.Time { return later }
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.CaseByID(ctx, c.ID)
	if got.Status != store.CaseInvestigating || got.Mechanism != "marker-7f3a" {
		t.Fatalf("unfinished investigation closed: %+v", got)
	}
}
