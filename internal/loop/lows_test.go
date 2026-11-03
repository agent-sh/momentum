package loop

import (
	"context"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A report whose mechanism holds only an invisible character names no mechanism: the case store keeps a plain line,
// which drops it, so the run decided nothing. The case closes as insufficient evidence; the run does not fail as
// `findings` on a testing case with no mechanism.
func TestAnInvisibleOnlyFindingIsUndecided(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	report := `{"mechanism":"\u200b","alternatives":[],"fix":"take the lock first","summary":"the evidence cannot decide"}`
	result := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":1,"session_id":"s1","structured_output":` + report + `}`
	p := script(t, dir, "agent", `echo '{"type":"system","subtype":"init"}'
`+fixtureResult(result))
	r := runnerFor(t, st, []string{p})
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 {
		t.Fatalf("%+v %v", done, err)
	}
	if done[0].Run.Status != store.RunOK || done[0].Run.Failure != "" {
		t.Fatalf("the run: %s %q", done[0].Run.Status, done[0].Run.Failure)
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Status != store.CaseClosed || got.Reason != store.Insufficient || got.NextTest != "take the lock first" {
		t.Fatalf("the case: %s %s, mechanism %q, next test %q", got.Status, got.Reason, got.Mechanism, got.NextTest)
	}
}
