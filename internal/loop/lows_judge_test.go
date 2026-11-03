package loop

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The judge's unit carries the evaluation's prefix, not the loop's: a live pass's sweep of its own leftovers never
// stops a judge, and the evaluation's sweep finds it. The agent renames the loop's unit to the invocation's prefix.
func TestTheJudgeRunsUnderTheEvaluationPrefix(t *testing.T) {
	e, task := sandboxEval(t, "true", "sleep 2\n"+fixtureResult(evalGrade))
	c := openCase(t, e.Runner.Store, task.Family, "judge-prefix", 1)
	stop, seen := make(chan struct{}), make(chan []string, 1)
	go func() {
		var names []string
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			out, _ := exec.Command("systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "momentum-run-*").Output()
			for _, field := range strings.Fields(string(out)) {
				if strings.HasPrefix(field, UnitPrefix) || strings.HasPrefix(field, EvalUnitPrefix) {
					names = append(names, field)
				}
			}
			select {
			case <-stop:
				seen <- names
				return
			case <-tick.C:
			}
		}
	}()
	g, _, _, err := e.grade(context.Background(), task, finding{Mechanism: "m", Fix: "f"}, filepath.Join(e.Dir, "judge", "fixture"),
		store.LoopRun{Case: c.ID, Experiment: "eval:fixture"})
	close(stop)
	names := <-seen
	if err != nil || g.Verdict != "reached" {
		t.Fatalf("the judge: %+v %v", g, err)
	}
	judged := false
	for _, name := range names {
		if strings.HasPrefix(name, UnitPrefix) {
			t.Fatalf("the judge ran as the loop's unit %s", name)
		}
		judged = judged || strings.HasPrefix(name, EvalUnitPrefix)
	}
	if !judged {
		t.Fatal("no unit of the evaluation's prefix was seen while the judge ran")
	}
}
