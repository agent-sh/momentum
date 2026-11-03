package loop

import (
	"context"
	"database/sql"
	"encoding/json"

	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The sandbox's isolation test runs whole under a scratch path too long for a socket's address.

// A run the model's safeguards refused runs once more on the fallback model within what is left of its run's
// budget, turns and time, and not at all when what the first attempt spent is unknown or leaves nothing. What the
// first attempt spent is what its relay charged, never what its stream says: the fake model charges 50,000 output
// tokens of a request at about one dollar, so three requests leave about one of the run's four.
func TestAFallbackSharesItsRunsAllowance(t *testing.T) {
	for name, c := range map[string]struct {
		primaryFields string
		runs          bool
		left          float64
	}{
		"cheap":   {``, true, 4 - fixtureCharge(3)},
		"3":       {`,"fixture_output_tokens":50000`, true, 4 - 3*(1+.00004)},
		"4":       {`,"fixture_output_tokens":66667`, false, 0},
		"unknown": {`,"fixture_no_usage":true`, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "fallback-args")
			primary := `{"type":"result","subtype":"success","is_error":true,"num_turns":3,"result":"safeguards"` + c.primaryFields + `}`
			fake := script(t, dir, "fixture", `model=""; budget=""; turns=""
while [ "$#" -gt 0 ]; do
 case "$1" in --model) model="$2"; shift;; --max-budget-usd) budget="$2"; shift;; --max-turns) turns="$2"; shift;; esac
 shift
done
if [ "$model" = "primary" ]; then `+fixtureResult(primary)+`; else
 printf '%s %s\n' "$budget" "$turns" > '`+argsFile+`'
 `+fixtureResult(`{"type":"result","subtype":"success","num_turns":1,"structured_output":{"summary":"ok"}}`)+`
fi`)
			r := runnerFor(t, st, []string{fake})
			r.Agent.Model, r.Agent.Fallback = "primary", "fallback"
			r.Budgets.Run, r.Budgets.Case, r.Budgets.Day = 4, 4, 100
			r.Budgets.MaxTurns = 10
			c1 := openCase(t, st, "failures", "budget", 1)
			done, err := r.Once(context.Background(), Options{Families: []string{"failures"}})
			if err != nil || len(done) != 1 {
				t.Fatalf("%+v %v", done, err)
			}
			got, _ := st.CaseByID(context.Background(), c1.ID)
			if got.Cost.USD > 6 {
				t.Errorf("the relay's allowance was exceeded: %.2f", got.Cost.USD)
			}
			b, err := os.ReadFile(argsFile)
			if !c.runs {
				if !os.IsNotExist(err) {
					t.Error("fallback ran with no known remaining allowance")
				}
				return
			}
			var budget float64
			var turns int
			if _, scanErr := fmt.Sscanf(string(b), "%g %d", &budget, &turns); err != nil || scanErr != nil || math.Abs(budget-c.left) > 1e-4 || turns != 10 {
				t.Errorf("fallback got fresh allowances: %q %v, want %.5f of the budget and the 10 turns of the main thread", b, err, c.left)
			}
		})
	}
	// The run's time is shared by its attempts: the fallback has what the first left, not a fresh allowance. The race
	// detector adds about a second to each sandboxed attempt (the sandbox's init is the test binary, race-built, and the
	// race runtime sleeps a second as it exits), so the times leave room for it: the first attempt (3 s) ends inside the
	// run's 5 s with and without it; the fallback (3 s) cannot end in the 2 s or less left to it, and would end inside
	// a fresh 5 s even with the second added.
	t.Run("deadline", func(t *testing.T) {
		dir := t.TempDir()
		fake := script(t, dir, "fixture", `case "$*" in *"--model primary"*) sleep 3; `+fixtureResult(`{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":1,"result":"safeguards"}`)+` ;; *) sleep 3; `+fixtureResult(okResult)+` ;; esac`)
		res, err := testSandboxAgent(t, Agent{Runner: []string{fake}, Model: "primary", Fallback: "fallback"}).RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), BudgetUSD: 4, MaxTurns: 10, Timeout: 5 * time.Second})
		if err != nil || res.Status != store.RunTimeout || res.USD != 4 || res.Model != "fallback" {
			t.Fatalf("fallback restarted time or lost unknown cost: %+v %v", res, err)
		}
	})
}

// A run killed midway is charged its reservation once, on the run and on its case in one transaction, whatever
// the budget says now; a recovery that fails charges nothing, and the case's budget then holds the next pick.
func TestARecoveredRunChargesItsCaseOnce(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "stale", 1)
	if _, err := st.AddCaseCost(ctx, c.ID, store.Cost{USD: 16}, "fixture"); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return time.Now().Add(-3 * time.Hour) }
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	r.Now = nil
	r.Budgets.Run = 1 // Recovery must use the original reservation.
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER stop_finish BEFORE UPDATE ON loop_runs BEGIN SELECT RAISE(ABORT, 'synthetic interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.endStale(ctx); err == nil {
		t.Error("failed finalization was ignored")
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Cost.USD != 16 {
		t.Fatal("partial charge survived rollback")
	}
	if _, err := db.Exec(`DROP TRIGGER stop_finish`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.endStale(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = st.CaseByID(ctx, c.ID)
	ended, _ := st.LoopRunByID(ctx, run.ID)
	if got.Cost.USD != 20 || got.Cost.Runs != 1 || ended.USD != 4 || ended.Status != store.RunFailed {
		t.Fatalf("case/run accounting: %+v %+v", got.Cost, ended)
	}
	picked, _, _, err := r.pick(ctx, c.Family, "", false)
	if err != nil || picked.ID != "" {
		t.Fatalf("exhausted case selected: %s %v", picked.ID, err)
	}
}

// A GitHub address the loop posts writes into no other thread: its host is made inert, in a link's target too.
func TestAPostedURLReferencesNothing(t *testing.T) {
	for _, url := range []string{"https://github.com/example-org/example-repo/issues/123", "https://github.com/example-org/example-repo/pull/4", "HTTPS://GITHUB.COM/example-org/example-repo/issues/123", "https://github.com/example-org/example-repo/%69ssues/123"} {
		for _, text := range []string{url, "[see](" + url + ")", "<" + url + ">", "[ref]: " + url} {
			got := Neutral(text)
			if got == text || strings.Contains(strings.ToLower(got), "github.com/") || !strings.Contains(got, "github[.]com/") {
				t.Errorf("active reference %q", got)
			}
		}
	}
}

// A run's end is recorded once: finishing it again charges its case nothing more.
func TestARunChargesItsCaseOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "finish", 1)
	run, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	res := Result{Status: store.RunOK, Started: true, CostKnown: true, TurnsKnown: true, USD: fixtureCharge(4), RelayUSD: fixtureCharge(4),
		RelayAdmitted: 4, RelayCharged: 4, Turns: 4, Report: json.RawMessage(`{"mechanism":"a mechanism","fix":"a fix","summary":"a summary"}`)}
	for i := 0; i < 2; i++ {
		r.finish(ctx, c, &run, res, nil)
	}
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.USD != fixtureCharge(4) || got.Cost.Runs != 1 {
		t.Fatalf("duplicate completion charge: %+v %v", got.Cost, err)
	}
}
