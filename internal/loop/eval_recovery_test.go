package loop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestStaleRecoveryKeepsAnUndecidedFinalReport(t *testing.T) {
	for _, sample := range []struct{ name, mechanism string }{{"with a mechanism", "marker-7f3a"}, {"with neither", ""}} {
		t.Run(sample.name, func(t *testing.T) {
			ctx := context.Background()
			r := localRunner(t, testStore(t))
			c := openCase(t, r.Store, "failures", "fixture", 1)
			run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
			if why != "" {
				t.Fatal(why)
			}
			report, _ := json.Marshal(map[string]any{"mechanism": sample.mechanism, "fix": "",
				"alternatives": []string{}, "summary": "marker-7f3a evidence cannot decide"})
			raw, _ := json.Marshal(Result{Started: true, Status: store.RunOK, Report: report})
			write(t, filepath.Join(HostDir(run.Dir), "result.json"), string(raw))
			later := run.Started.Add(r.Budgets.RunTime + time.Hour)
			r.Now = func() time.Time { return later }
			if err := r.endStale(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := r.Store.CaseByID(ctx, c.ID)
			if err != nil || got.Status != store.CaseClosed || got.Reason != store.Insufficient ||
				!strings.Contains(got.Mechanism, sample.mechanism) {
				t.Fatal("stale recovery lost an undecided authenticated report")
			}
		})
	}
}
