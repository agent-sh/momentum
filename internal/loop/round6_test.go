package loop

import (
	"context"

	"path/filepath"

	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A scrubber that reads a text and prints nothing did not scrub it.

// A scrubber that reads a text and prints nothing did not scrub it.
func TestAScrubberThatPrintsNothingFails(t *testing.T) {
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	Scrubber = []string{script(t, t.TempDir(), "scrub", "cat > /dev/null")}
	if _, err := Redact(context.Background(), "a text with a secret-value in it"); err == nil {
		t.Fatal("an empty scrub passed")
	}
	if out, err := Redact(context.Background(), ""); err != nil || out != "" {
		t.Fatalf("an empty text: %q %v", out, err)
	}
}

// A pass whose inputs cannot be scrubbed starts no run: nothing is reserved or recorded, and the case and the loop's
// budgets are charged nothing.
func TestARunThatNeverStartedSpendsNothing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	Scrubber = []string{filepath.Join(t.TempDir(), "no-scrubber")}
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.ID != "" || done[0].Skipped == "" {
		t.Fatalf("%+v %v", done, err)
	}
	if runs, _ := st.LoopRuns(ctx, store.RunFilter{}); len(runs) != 0 {
		t.Fatalf("a run was recorded: %+v", runs)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Cost.USD != 0 || got.Cost.Runs != 0 {
		t.Fatalf("the case was charged %+v", got.Cost)
	}
}
