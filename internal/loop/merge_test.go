package loop

import (
	"context"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Preflight failures have no start time. The stop rule counts only runs that ran,
// and filters before it takes the latest two.
func TestMergedStopRuleIgnoresRunsThatNeverStarted(t *testing.T) {
	ctx := context.Background()
	r := recordingRunner(t)
	c := openCase(t, r.Store, "failures", "preflight", 1)
	for _, id := range []string{"first", "second"} {
		if err := r.Store.PutLoopRun(ctx, store.LoopRun{
			ID: id, Case: c.ID, Family: c.Family, Step: StepInvestigate,
			Ended: time.Now(), Status: store.RunFailed, Failure: "synthetic preflight error",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("runs that never started stopped the case: %q %v", why, err)
	}
}
