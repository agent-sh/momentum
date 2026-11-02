package loop

import (
	"context"
	"encoding/json"
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
		counter := filepath.Join(t.TempDir(), "calls")
		// the loop's first trial's judge is the first to run; it fails this many times before it grades
		e.Judge.Runner = []string{script(t, t.TempDir(), "flaky-judge", `n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`
if [ "$n" -le `+itoa(failures)+` ]; then echo '{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0.1}'; exit 1; fi
echo '{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached","facts":[],"reason":"r"}}'`)}
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

func itoa(n int) string { return string(rune('0' + n)) }

// A default branch reset or rebased after the end leaves its commit at the end only in the reflog, which a clone does
// not carry: the commit dates set the clone then, and the trials run.
func TestACloneFallsBackWhenTheReflogsCommitIsGone(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	other := filepath.Join(dir, "other")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", other)
	first := commitAt(t, other, end.Add(-48*time.Hour), "job", "a", "A")
	commitAt(t, other, end.Add(-24*time.Hour), "job", "b", "B, a local commit main held at the end")
	historyGit(t, other, end.Add(24*time.Hour), "reset", "-q", "--hard", "HEAD~1")
	commitAt(t, other, end.Add(25*time.Hour), "job", "c", "C")
	e.Repositories = []string{other}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Err != "" || tr.Run.Started.IsZero() {
			t.Fatalf("%s: %q", tr.Arm, tr.Err)
		}
		if got := historyGit(t, filepath.Join(tr.Run.Dir, "repo-1"), time.Time{}, "rev-parse", "HEAD"); got != first {
			t.Fatalf("%s: the clone stands at %s, want %s", tr.Arm, got, first)
		}
	}
}

// Both arms of a trial read one store as of its end, made before either runs, whatever the store holds by the time
// the second runs; and the arms take turns going first.
func TestBothArmsOfATrialReadOneStoreAndTakeTurns(t *testing.T) {
	e, task := evalFixture(t)
	e.Trials = 2
	order := filepath.Join(t.TempDir(), "order")
	before := task.Cutoff.Add(-2 * time.Hour).UnixMilli() // inside every trial's window
	// each arm writes down how many events its store holds, then adds one of the window to the source store, as an
	// ingest between the arms would: the arm that runs second must not see it
	agent := script(t, t.TempDir(), "count-and-add", `python3 -I -c 'import sqlite3,sys; print(sys.argv[2], sqlite3.connect(sys.argv[1]).execute("select count(*) from events").fetchone()[0])' momentum.db "$(basename "$PWD")" >> `+order+`
python3 -I -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("insert into events (id,source,source_key,ts,actor,kind,summary,ingested) values (?,?,?,?,?,?,?,?)", (sys.argv[2],"transcripts",sys.argv[2],int(sys.argv[3]),"agent","agent_text","a rewrite between the arms",0)); c.commit()' `+e.Source+` "late-$$" `+strconv.FormatInt(before, 10)+`
printf '%s\n' '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}'`)
	e.Runner.Agent.Runner = []string{agent}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	arms := map[string]Trial{}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		arms[filepath.Base(tr.Run.Dir)] = tr
	}
	raw, _ := os.ReadFile(order)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("runs: %q", raw)
	}
	counts := map[int][]string{}
	var first []string
	for _, line := range lines {
		dir, n, _ := strings.Cut(line, " ")
		tr := arms[dir]
		if len(counts[tr.N]) == 0 {
			first = append(first, tr.Arm)
		}
		counts[tr.N] = append(counts[tr.N], n)
	}
	for n, c := range counts {
		if len(c) != 2 || c[0] != c[1] {
			t.Errorf("trial %d: its arms read different stores: %v", n, c)
		}
	}
	if len(first) != 2 || first[0] == first[1] {
		t.Errorf("the same arm went first in both trials: %v", first)
	}
}

// The judge reads both arms' fields in one shape, folded to one line, as the case store keeps the loop's.
func TestTheJudgeReadsBothArmsInOneShape(t *testing.T) {
	written := "the job reads the lockfile before it takes the lock\n- seen in both failures\n- the lock is taken 40 ms later"
	loop := caseFinding(store.Case{Mechanism: plain.Clip(plain.Line(written), 2000), Alternatives: []string{"disk latency"}, NextTest: "take the lock first"},
		Result{Report: json.RawMessage(`{"summary":"found"}`)})
	raw, _ := json.Marshal(map[string]any{"mechanism": written, "alternatives": []string{"disk latency"}, "fix": "take the lock first", "summary": "found"})
	native := nativeFinding(Result{Report: raw})
	if judgeFinding(loop) != judgeFinding(native) {
		t.Fatalf("the shapes differ:\n%s\n%s", judgeFinding(loop), judgeFinding(native))
	}
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
