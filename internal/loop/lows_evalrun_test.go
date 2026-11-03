package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// The evaluation's budget releases a settled arm's reservation and counts what it spent: with one slot, a budget of
// one pair's room and a half runs a second pair after the first, and a budget of one room and less than a pair's
// spend skips it.
func TestTheEvaluationBudgetReleasesAndSpends(t *testing.T) {
	ctx := context.Background()
	e, task := sandboxEval(t, "true", fixtureResult(evalGrade))
	room := float64(len(e.arms())) * 1.5 * (e.Runner.Budgets.Run + judgeAttempts*judgeBudget)
	pair := 4 * fixtureCharge(1) // two investigations and two judges
	for _, c := range []struct {
		name   string
		budget float64
		ran    int
	}{{"released", 1.5 * room, 4}, {"spent", room + pair/2, 2}} {
		t.Run(c.name, func(t *testing.T) {
			e.Dir, e.Trials, e.Parallel, e.Budget = shortScratch(t), 2, 1, c.budget
			trials, err := e.Run(ctx, []GoldenTask{task})
			if err != nil || len(trials) != 4 {
				t.Fatalf("%v, %d trials", err, len(trials))
			}
			ran := 0
			for _, tr := range trials {
				if tr.Skipped == "" {
					ran++
				}
			}
			if ran != c.ran {
				t.Fatalf("%d arms ran, want %d", ran, c.ran)
			}
		})
	}
}

// Run orders each pair's arms by armOrder: over two tasks of one trial each arm goes first once.
func TestTheEvaluationRunAlternatesWhichArmGoesFirst(t *testing.T) {
	e, task := sandboxEval(t, "true", fixtureResult(evalGrade))
	other := task
	other.ID = "second-task"
	e.Trials, e.Parallel, e.Budget = 1, 1, 120
	trials, err := e.Run(context.Background(), []GoldenTask{task, other})
	if err != nil || len(trials) != 4 {
		t.Fatalf("%v, %d trials", err, len(trials))
	}
	first := map[string]int{}
	for _, tr := range trials {
		if tr.Position == 1 {
			first[tr.Arm]++
		}
	}
	if first["loop"] != 1 || first["native"] != 1 {
		t.Fatalf("first positions by arm: %v", first)
	}
}

// A judge that started and made no request the relay saw has an unknown cost: it holds its whole judge budget, as an
// investigation's unknown cost holds its reservation.
func TestAJudgeOfUnknownCostHoldsItsBudget(t *testing.T) {
	e, task := sandboxEval(t, "true", `echo '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0}'`)
	c := openCase(t, e.Runner.Store, task.Family, "judge-cost", 1)
	_, spent, _, err := e.grade(context.Background(), task, finding{Mechanism: "m", Fix: "f"}, filepath.Join(e.Dir, "judge", "cost"),
		store.LoopRun{Case: c.ID, Experiment: "eval:fixture"})
	if spent < judgeBudget {
		t.Fatalf("a judge of unknown cost counted $%.4f (%v)", spent, err)
	}
}
