package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// A judge that fails is tried once more on the same model; a trial still without a grade leaves the comparison with
// its pair, counted apart, so the arm whose judge failed is not read as having missed.
func TestAFailedJudgeIsRetriedAndAnUngradedPairLeftOut(t *testing.T) {
	for _, failures := range []int{1, 2} {
		e, task := evalFixture(t)
		e.Trials = 2
		harness := filepath.Dir(e.Runner.Agent.Runner[0])
		counter := filepath.Join(harness, fmt.Sprint("calls-", failures))
		// the loop's first trial's judge is the first to run; it fails this many times before it grades
		e.Judge.Runner = []string{script(t, harness, "flaky-judge", `n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`
if [ "$n" -le `+strconv.Itoa(failures)+` ]; then `+modelResult(`{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0.1}`)+`; exit 1; fi
`+modelResult(`{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached","facts":[],"reason":"r"}}`))}
		trs, err := e.Run(context.Background(), []GoldenTask{task})
		if err != nil {
			t.Fatal(err)
		}
		r := e.Results(trs)
		loop, native := r.Arms[0], r.Arms[1]
		if loop.Arm != "loop" || native.Arm != "native" || loop.PassAllK != native.PassAllK || r.Comparison.Tasks[0].MoreOften != "tie" {
			t.Fatalf("%d failures: %+v %+v %+v", failures, loop, native, r.Comparison.Tasks)
		}
		if failures == 1 && (loop.Graded != 2 || loop.Ungraded != 0) {
			t.Fatalf("a judge failing once is retried: %+v", loop)
		}
		if failures == 2 && (loop.Graded != 1 || loop.Ungraded != 1 || native.Ungraded != 1 || loop.ComparedReached != 1 || native.ComparedReached != 1) {
			t.Fatalf("an ungraded pair is left out of the comparison: %+v %+v", loop, native)
		}
		if x := r.Comparison.ReachedPerDollar; x == nil || x.Low != 0 || x.High != 0 {
			t.Fatalf("%d failures: the interval of identical arms: %+v", failures, x)
		}
	}
}

func TestAMalformedJudgeIsRetriedAndThenLeavesThePairUngraded(t *testing.T) {
	for _, failures := range []int{1, 2} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			e, task := evalFixture(t)
			counter := filepath.Join(filepath.Dir(e.Judge.Runner[0]), "malformed-calls")
			e.Judge.Runner = []string{script(t, filepath.Dir(counter), "malformed-judge", `n=$(cat "`+counter+`" 2>/dev/null || echo 0)
n=$((n+1)); echo "$n" > "`+counter+`"
if [ "$n" -le `+strconv.Itoa(failures)+` ]; then
  `+modelResult(`{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached"}}`)+`
else
  `+modelResult(`{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached","facts":[],"reason":"marker-7f3a"}}`)+`
fi`)}
			trials, err := e.Run(context.Background(), []GoldenTask{task})
			if err != nil || len(trials) != 2 {
				t.Fatalf("trials: %d %v", len(trials), err)
			}
			results := e.Results(trials)
			if !near(trials[0].JudgeUSD, 0.2) {
				t.Fatalf("retry was not charged: %+v", trials[0])
			}
			if failures == 1 && (results.Arms[0].Graded != 1 || results.Arms[0].Ungraded != 0) {
				t.Fatalf("retry did not recover: %+v", results.Arms)
			}
			if failures == 2 && (results.Arms[0].Ungraded != 1 || results.Arms[1].Ungraded != 1 || len(results.Comparison.Tasks) != 0) {
				t.Fatalf("malformed grade entered comparison: %+v", results)
			}
		})
	}
}

// A judge tried again keeps the stream of the first attempt beside the second's, and both attempts' runs are
// recorded and priced.
func TestAJudgeTriedAgainKeepsItsFirstStream(t *testing.T) {
	e, task := evalFixture(t)
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	counter := filepath.Join(harness, "calls")
	e.Judge.Runner = []string{script(t, harness, "flaky-judge", `n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`
if [ "$n" -le 1 ]; then `+modelResult(`{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0.1}`)+`; exit 1; fi
`+modelResult(`{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached","facts":[],"reason":"r"}}`))}
	e.Arms = []string{"native"}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 1 || trs[0].Err != "" || trs[0].Grade.Verdict != "reached" {
		t.Fatalf("%+v %v", trs, err)
	}
	failed, err := os.ReadFile(filepath.Join(HostDir(trs[0].JudgeDir), "transcript.jsonl"))
	if err != nil || !strings.Contains(string(failed), "error_during_execution") {
		t.Fatalf("the first stream was not kept: %q %v", failed, err)
	}
	graded, _ := os.ReadFile(filepath.Join(HostDir(trs[0].JudgeDir), "transcript.fallback.jsonl"))
	if !strings.Contains(string(graded), `"verdict":"reached"`) {
		t.Fatalf("the second stream is not the judge's: %q", graded)
	}
	if !near(trs[0].JudgeUSD, 0.2) {
		t.Errorf("both attempts are priced: $%v", trs[0].JudgeUSD)
	}
}

// The judge reads both arms' fields in one shape, folded to one line, as the case store keeps the loop's.
func TestTheJudgeReadsBothArmsInOneShape(t *testing.T) {
	written := "the job reads the lockfile before it takes the lock\n- seen in both failures\n- the lock is taken 40 ms later"
	raw, _ := json.Marshal(map[string]any{"mechanism": written, "alternatives": []string{"disk latency"}, "fix": "take the lock first", "summary": "found"})
	loop := loopFinding(store.Case{Mechanism: plain.Clip(plain.Line(written), 2000), Alternatives: []string{"disk latency"}, NextTest: "take the lock first"},
		reportedFinding(t, Result{Report: raw}))
	native := reportedFinding(t, Result{Report: raw})
	if judgeFinding(loop) != judgeFinding(native) {
		t.Fatalf("the shapes differ:\n%s\n%s", judgeFinding(loop), judgeFinding(native))
	}
}

// A cut-short native run's alternatives keep their list shape: the findings file's section is split into its items
// (a leading dash, star or number off), as the loop's case keeps each alternative apart, so the judge reads the same
// shape from either arm.
func TestACutShortNativeRunsAlternativesKeepTheirListShape(t *testing.T) {
	file := "## Mechanism\ncause\n## Alternatives ruled out\n- disk latency\n- a second writer, which\n  held the lock too\n3. a stale cache\n\nthe last one is prose\n## Fix\nfix\n"
	native := findingsFile(file)
	want := []string{"disk latency", "a second writer, which held the lock too", "a stale cache", "the last one is prose"}
	if !equalStrings(native.Alternatives, want) {
		t.Fatalf("the items: %q, want %q", native.Alternatives, want)
	}
	loop := loopFinding(store.Case{Mechanism: "cause", Alternatives: want, NextTest: "fix"}, finding{})
	if judgeFinding(native) != judgeFinding(loop) {
		t.Fatalf("the judge reads the arms differently:\n%s\n%s", judgeFinding(native), judgeFinding(loop))
	}
	if one := findingsFile("## Alternatives ruled out\nonly a paragraph of prose\nthat goes on\n"); !equalStrings(one.Alternatives, []string{"only a paragraph of prose that goes on"}) {
		t.Errorf("a paragraph is one item: %q", one.Alternatives)
	}
	if none := findingsFile("## Mechanism\ncause\n"); len(none.Alternatives) != 0 {
		t.Errorf("no section, no alternative: %q", none.Alternatives)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The bootstrap draws a task with both its arms, and reads the 5th and 95th percentiles of 10,000 resamples.
func TestTheBootstrapPairsArmsByTaskAndReadsItsPercentiles(t *testing.T) {
	same := map[string]Summary{"loop": {Reached: 2, USD: 2, TotalMinutes: 2}, "native": {Reached: 2, USD: 2, TotalMinutes: 2}}
	pairs := map[string]map[string]Summary{"a": same, "b": {"loop": {Reached: 0, USD: 2, TotalMinutes: 2}, "native": {Reached: 0, USD: 2, TotalMinutes: 2}}, "c": same}
	// paired by task, identical arms give a point at zero in every resample; arms drawn apart would not
	if c := compareTasks(pairs); c.ReachedPerDollar == nil || c.ReachedPerDollar.Low != 0 || c.ReachedPerDollar.High != 0 {
		t.Fatalf("paired: %+v", c.ReachedPerDollar)
	}
	// with b's loop one cause ahead, a resample holding b k times gives k/6 per dollar: k is 0 in 8 of 27 draws, 1 in
	// 12, 2 in 6 and 3 in 1, so the 5th percentile is 0 and the 95th 2/6, where the largest would be 3/6
	pairs["b"] = map[string]Summary{"loop": {Reached: 1, USD: 2, TotalMinutes: 2}, "native": {Reached: 0, USD: 2, TotalMinutes: 2}}
	c := compareTasks(pairs)
	if c.ReachedPerDollar == nil || c.ReachedPerDollar.Low != 0 || c.ReachedPerDollar.High < 0.333 || c.ReachedPerDollar.High > 0.334 {
		t.Fatalf("percentiles: %+v", c.ReachedPerDollar)
	}
}

// The rates per root cause divide the compared trials' cost by the causes reached among them, not by every cause
// reached: a cause reached in a trial left out of the comparison (its pair ungraded) does not lower the rate.
func TestTheRatesPerCauseDivideByTheComparedCauses(t *testing.T) {
	e, _ := evalFixture(t)
	e.Trials = 2
	at := time.Now()
	ran := func(arm string, n int, usd float64, verdict string) Trial {
		return Trial{Task: "task", N: n, Arm: arm, Grade: Grade{Verdict: verdict}, Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: usd}}
	}
	// trial 2's native judge failed: its pair is ungraded, though the loop reached the cause in it
	trs := []Trial{ran("loop", 1, 2, "reached"), ran("native", 1, 2, "missed"), ran("loop", 2, 2, "reached"), ran("native", 2, 2, "")}
	r := e.Results(trs)
	loop := r.Arms[0]
	if loop.Reached != 2 || loop.ComparedReached != 1 || loop.Ungraded != 1 {
		t.Fatalf("the counts: %+v", loop)
	}
	if loop.USDPerReached == nil || *loop.USDPerReached != 2 || *loop.MinutesPerReached != 1 {
		t.Fatalf("the rate per cause reached divides by %d causes: $%v", loop.ComparedReached, loop.USDPerReached)
	}
}
