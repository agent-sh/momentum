package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestARepositoryAsOfItsFixHoldsNothingAfter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	run(t, dir, "git", "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "a.go"), "package a\n")
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-q", "-m", "before")
	before := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	write(t, filepath.Join(src, "a.go"), "package a\n\n// the fix\n")
	run(t, src, "git", "commit", "-q", "-am", "the fix")
	fix := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	run(t, src, "git", "branch", "later")
	dst := filepath.Join(dir, "as-of")
	if err := cloneAsOf(context.Background(), src, before, dst); err != nil {
		t.Fatal(err)
	}
	if log := run(t, dst, "git", "log", "--all", "--format=%s"); strings.TrimSpace(log) != "before" {
		t.Fatalf("the clone's history: %q", log)
	}
	if err := exec.Command("git", "-C", dst, "cat-file", "-e", fix).Run(); err == nil {
		t.Fatal("the fix's commit is in the clone")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.go")); strings.Contains(string(b), "the fix") {
		t.Fatal("the work tree holds the fix")
	}
}

func TestACloneThatFailsNamesItsStep(t *testing.T) {
	dir := t.TempDir()
	err := cloneAsOf(context.Background(), filepath.Join(dir, "no-such-source"), "0000000000000000000000000000000000000000", filepath.Join(dir, "dst"))
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("the error does not name the clone step: %v", err)
	}
}

func TestGoldenTasksAreReadWhole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	write(t, p, `{"id":"a","family":"failures","case_title":"x keeps failing","cutoff":"2026-03-02T10:00:00Z","symptom":{"query":{"event_ids":["e1"]}},"root_cause":"a cause","key_facts":["f"],"repo_path":"~/projects/x"}`+"\n\n")
	ts, err := ReadGolden(p)
	if err != nil || len(ts) != 1 || ts[0].RepoPath != filepath.Join(home, "projects", "x") {
		t.Fatalf("%+v %v", ts, err)
	}
	write(t, p, `{"id":"a","family":"failures","case_title":"x","cutoff":"2026-03-02T10:00:00Z","symptom":{"query":{"event_ids":[]}},"root_cause":"a cause","key_facts":["f"]}`+"\n")
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("a task with no events was read")
	}
}

func TestAnEvaluationIsSummedByTask(t *testing.T) {
	at := time.Now()
	trial := func(task, verdict string, usd float64, minutes int) Trial {
		return Trial{Task: task, Family: "failures", Grade: Grade{Verdict: verdict},
			Run: store.LoopRun{USD: usd, Turns: 10, Started: at, Ended: at.Add(time.Duration(minutes) * time.Minute)}}
	}
	sums := Summarize([]Trial{trial("a", "reached", 1, 4), trial("a", "partial", 2, 6), trial("b", "reached", 1, 5), trial("b", "reached", 1, 5),
		{Task: "c", Family: "sessions", Err: "the repository as of the fix: no clone"}})
	if len(sums) != 3 {
		t.Fatalf("%+v", sums)
	}
	a, b, c := sums[0], sums[1], sums[2]
	if a.Reached != 1 || !a.AnyReached || a.AllReached || !a.Flipped || a.USD != 3 || a.Minutes != 5 {
		t.Errorf("a %+v", a)
	}
	if b.Reached != 2 || !b.AllReached || b.Flipped {
		t.Errorf("b %+v", b)
	}
	if len(c.Verdicts) != 0 || len(c.Errors) != 1 || c.AnyReached {
		t.Errorf("c %+v", c)
	}
}

func TestATrialEndsAtItsNewestSymptom(t *testing.T) {
	e, task := evalFixture(t)
	ctx := context.Background()
	end, err := e.snapshotEnd(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if !end.Equal(task.Cutoff.Add(-59 * time.Minute)) {
		t.Fatalf("end %s cutoff %s", end, task.Cutoff)
	}
	task.Cutoff = end.Add(-30 * time.Second)
	earlier, err := e.snapshotEnd(ctx, task)
	if err != nil || !earlier.Equal(task.Cutoff) {
		t.Fatalf("earlier %s %v", earlier, err)
	}
	task.Symptom.Query.EventIDs = []string{"absent"}
	if _, err = e.snapshotEnd(ctx, task); err == nil {
		t.Fatal("missing symptom accepted")
	}
}

func TestPairedSummariesKeepTheSameK(t *testing.T) {
	at := time.Now()
	trial := func(task, arm, v string, n int) Trial {
		return Trial{Task: task, Arm: arm, N: n, Grade: Grade{Verdict: v}, Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 2}}
	}
	trs := []Trial{trial("a", "loop", "reached", 1), trial("a", "loop", "reached", 2), {Task: "a", Arm: "loop", N: 3, Skipped: "budget"}, trial("a", "native", "missed", 1), trial("a", "native", "wrong", 2), trial("a", "native", "missed", 3)}
	e, _ := evalFixture(t)
	e.Trials = 3
	r := e.Results(trs)
	if *r.Arms[0].PassAllK != 0 || *r.Arms[0].PassAtK != 1 || r.Tasks[1].Flipped || r.Comparison.Tasks[0].MoreOften != "loop" {
		t.Fatalf("%+v", r)
	}
	if r.Comparison.ReachedPerDollar == nil || r.Comparison.ReachedPerDollar.Low != 0.5 || r.Comparison.ReachedPerDollar.High != 0.5 {
		t.Fatalf("interval %+v", r.Comparison)
	}
	if r.Comparison.ReachedPerMinute.Low != 1 || !reflect.DeepEqual(r.Comparison, e.Results(trs).Comparison) {
		t.Fatal("bootstrap not repeatable or wrong rate")
	}
	if r.Arms[0].USDPerReached == nil || *r.Arms[0].USDPerReached != 2 {
		t.Fatalf("rate %+v", r.Arms[0])
	}
}

func TestGoldenAndEvaluationIDsAreUnambiguous(t *testing.T) {
	_, task := evalFixture(t)
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	for _, id := range []string{"../outside", "two words", "Upper"} {
		task.ID = id
		raw, _ := json.Marshal(task)
		write(t, p, string(raw))
		if _, err := ReadGolden(p); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	task.ID = "valid-slug"
	raw, _ := json.Marshal(task)
	write(t, p, string(raw)+"\n"+string(raw))
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("duplicate accepted")
	}
	task.RootCause = ""
	raw, _ = json.Marshal(task)
	write(t, p, string(raw))
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("missing root accepted")
	}
	seen := map[string]bool{}
	for range 100 {
		id := NewEvalID()
		if seen[id] || len(id) < 26 {
			t.Fatalf("id %s", id)
		}
		seen[id] = true
	}
}

func TestHistoricalClonesDropAlternatesAndKeptPacks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	run(t, dir, "git", "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "file"), "before")
	run(t, src, "git", "add", "file")
	run(t, src, "git", "commit", "-qm", "before")
	before := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	write(t, filepath.Join(src, "file"), "after")
	run(t, src, "git", "commit", "-qam", "after")
	after := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD"))
	run(t, src, "git", "gc")
	packs, _ := filepath.Glob(filepath.Join(src, ".git", "objects", "pack", "*.pack"))
	for _, p := range packs {
		write(t, strings.TrimSuffix(p, ".pack")+".keep", "")
	}
	shared := filepath.Join(dir, "shared")
	run(t, dir, "git", "clone", "-q", "--shared", src, shared)
	hooks := filepath.Join(dir, "hooks")
	os.Mkdir(hooks, 0700)
	marker := filepath.Join(dir, "hook-ran")
	script(t, hooks, "post-checkout", fmt.Sprintf("touch '%s'", marker))
	config := filepath.Join(dir, "gitconfig")
	write(t, config, "[core]\n hooksPath = "+hooks+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	for i, source := range []string{src, shared} {
		dst := filepath.Join(dir, fmt.Sprint("clone", i))
		if err := cloneAsOf(context.Background(), source, before, dst); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("git", "-C", dst, "cat-file", "-e", after).Run(); err == nil {
			t.Fatal("later object retained")
		}
		if _, err := os.Stat(filepath.Join(dst, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
			t.Fatal("alternates retained")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("global hook ran")
	}
}

func TestResultProvenanceNamesEachFamily(t *testing.T) {
	e, task := evalFixture(t)
	task.Family = "sessions"
	tr := e.newTrial(task, 1, "loop")
	r := e.Results([]Trial{tr})
	if r.Procedures["sessions"].Systems["loop"] == "" || r.Provenance.Systems["loop"] != tr.Provenance.Systems["loop"] ||
		r.Provenance.Systems["loop"] != r.Provenance.Systems["native"] {
		t.Fatalf("%+v", r.Provenance)
	}
}

func TestATrialThatFailedKeepsItsTokens(t *testing.T) {
	run := store.LoopRun{ID: "r", Started: time.Now().Add(-time.Minute), Status: store.RunRunning, USD: 5}
	pricedUsage := Usage{Input: 2, Output: 3, CacheRead: 4, CacheWrite5: 5, CacheWrite1: 6}
	res := Result{Started: true, CostKnown: true, USD: 4.29, Tokens: pricedUsage.tokens(), Usage: pricedUsage, Turns: 4, Transcript: "t.jsonl"}
	got, started := settleRun(run, res, os.ErrDeadlineExceeded, 5)
	if !started || got.Usage != storedUsage(pricedUsage) || got.Tokens != pricedUsage.tokens() || math.Abs(got.USD-4.29) > 1e-9 || got.Status != store.RunFailed || got.Turns != 4 {
		t.Errorf("a failed run with its cost known: %+v", got)
	}
	got, started = settleRun(run, Result{Started: true, Transcript: "t.jsonl"}, os.ErrDeadlineExceeded, 5)
	if !started || got.USD != 5 {
		t.Errorf("a failed run with its cost unknown: %+v", got)
	}
	got, started = settleRun(run, Result{}, os.ErrNotExist, 5)
	if started || got.USD != 0 || got.Status != store.RunFailed {
		t.Errorf("a run that never started: %+v", got)
	}
}

func TestHistoricalCloneUsesTheLocalDefaultBranch(t *testing.T) {
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	for _, branch := range []string{"main", "master", "trunk"} {
		t.Run(branch, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source")
			run(t, dir, "git", "init", "-q", "-b", branch, src)
			local := commitAt(t, src, end.Add(-3*time.Hour), "job", "local", "local")
			run(t, src, "git", "checkout", "-qb", "upstream")
			remote := commitAt(t, src, end.Add(-2*time.Hour), "job", "upstream", "fetched but not pulled")
			run(t, src, "git", "checkout", "-q", branch)
			run(t, src, "git", "update-ref", "refs/remotes/origin/"+branch, remote)
			run(t, src, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
			run(t, src, "git", "branch", "-D", "upstream")
			repos, err := cloneRepositories(context.Background(), []string{src}, end, filepath.Join(dir, "repos"))
			if err != nil || len(repos) != 1 {
				t.Fatalf("%v %v", repos, err)
			}
			if got := strings.TrimSpace(run(t, repos[0], "git", "rev-parse", "HEAD")); got != local {
				t.Fatalf("head %s want local %s", got, local)
			}
			if exec.Command("git", "-C", repos[0], "cat-file", "-e", remote).Run() == nil {
				t.Fatal("remote-only object retained")
			}
		})
	}
}

func TestHistoricalCloneClearsGitEnvironment(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	run(t, dir, "git", "init", "-q", "-b", "main", src)
	head := commitAt(t, src, time.Now().Add(-time.Hour), "job", "before", "before")
	t.Setenv("GIT_DIR", filepath.Join(dir, "absent.git"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(dir, "wrong-tree"))
	dst := filepath.Join(dir, "clone")
	if err := cloneAsOf(context.Background(), src, head, dst); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dst, "job"))
	if err != nil || string(raw) != "before" {
		t.Fatalf("checkout %s %v", raw, err)
	}
}

func TestBootstrapLeavesZeroDenominatorsUndefined(t *testing.T) {
	pairs := map[string]map[string]Summary{
		"positive": {"loop": {Reached: 1, USD: 1, TotalMinutes: 1}, "native": {Reached: 1, USD: 1, TotalMinutes: 1}},
		"zero":     {"loop": {Reached: 1}, "native": {Reached: 1, USD: 1, TotalMinutes: 1}},
	}
	c := compareTasks(pairs)
	if c.ReachedPerDollar != nil || c.ReachedPerMinute != nil {
		t.Fatalf("undefined intervals %+v", c)
	}
}

func TestComparisonCostsUseOnlyMatchedTrials(t *testing.T) {
	e, _ := evalFixture(t)
	e.Trials = 2
	at := time.Now()
	trials := []Trial{
		{Task: "task", N: 1, Arm: "loop", Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 1}, Grade: Grade{Verdict: "reached"}},
		{Task: "task", N: 1, Arm: "native", Run: store.LoopRun{Started: at, Ended: at.Add(time.Minute), USD: 1}, Grade: Grade{Verdict: "reached"}},
		{Task: "task", N: 2, Arm: "loop", Run: store.LoopRun{Started: at, Ended: at.Add(10 * time.Minute), USD: 10}, Grade: Grade{Verdict: "reached"}},
		{Task: "task", N: 2, Arm: "native", Skipped: "stopped"},
	}
	r := e.Results(trials)
	for _, arm := range r.Arms {
		if arm.USDPerReached == nil || *arm.USDPerReached != 1 || arm.MinutesPerReached == nil || *arm.MinutesPerReached != 1 || arm.Reached != 1 || arm.Excluded != 1 || *arm.PassAllK != 0 {
			t.Fatalf("unmatched rates: %+v", arm)
		}
	}
	if r.Arms[0].USD != 11 || r.Arms[0].Ran != 2 {
		t.Fatal("accounting lost the unmatched run")
	}
	if r.Comparison.ReachedPerDollar == nil || r.Comparison.ReachedPerDollar.Low != 0 || r.Comparison.ReachedPerDollar.High != 0 {
		t.Fatalf("unmatched interval: %+v", r.Comparison)
	}
}

func TestTheJudgeReadsBothArmsInOneShape(t *testing.T) {
	written := "the job reads the lockfile before it takes the lock\n- seen in both failures\n- the lock is taken 40 ms later"
	raw, _ := json.Marshal(map[string]any{"mechanism": written, "alternatives": []string{"disk latency"}, "fix": "take the lock first", "summary": "found"})
	loop := loopFinding(store.Case{Mechanism: plain.Clip(plain.Line(written), 2000), Alternatives: []string{"disk latency"}, NextTest: "take the lock first"},
		nativeFinding(Result{Report: raw}))
	native := nativeFinding(Result{Report: raw})
	if judgeFinding(loop) != judgeFinding(native) {
		t.Fatalf("the shapes differ:\n%s\n%s", judgeFinding(loop), judgeFinding(native))
	}
}

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

// A task may name the harness its cause involves; a mistyped key is still refused.
func TestAGoldenTaskMayNameItsHarness(t *testing.T) {
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	line := func(extra string) string {
		return `{"id":"a","family":"failures","case_title":"x keeps failing","cutoff":"2026-03-02T10:00:00Z","symptom":{"query":{"event_ids":["e1"]}},"root_cause":"a cause","key_facts":["f"]` + extra + "}\n"
	}
	write(t, p, line(`,"harness":"codex"`))
	ts, err := ReadGolden(p)
	if err != nil || len(ts) != 1 || ts[0].Harness != "codex" {
		t.Fatalf("a task with a harness: %+v %v", ts, err)
	}
	write(t, p, line(`,"difficulty":"hard","fix":"x","verification":"y","evidence":["z"],"reach":"store","reach_detail":{}`))
	if ts, err = ReadGolden(p); err != nil || len(ts) != 1 {
		t.Fatalf("a task with the golden file's own notes: %v", err)
	}
	write(t, p, line(`,"harnes":"codex"`))
	if _, err := ReadGolden(p); err == nil {
		t.Fatal("a mistyped key was read")
	}
}

// A repository whose objects the scrubber would change is left out of a task's inputs and counted: the task goes on
// without it, as the loop's own pass does, instead of failing both arms of every trial.
func TestAHistoricalCloneTheScrubberWouldChangeIsLeftOut(t *testing.T) {
	goodInputScrubber(t)
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	dir := t.TempDir()
	clean, secret := filepath.Join(dir, "clean"), filepath.Join(dir, "secret")
	run(t, dir, "git", "init", "-q", "-b", "main", clean)
	commitAt(t, clean, end.Add(-3*time.Hour), "job", "neutral text", "neutral")
	run(t, dir, "git", "init", "-q", "-b", "main", secret)
	commitAt(t, secret, end.Add(-3*time.Hour), "job", "password=marker-7f3a", "holds a password")
	repos, refused, err := cloneRepositoriesCounted(context.Background(), []string{secret, clean}, end, filepath.Join(dir, "repos"))
	if err != nil || len(repos) != 1 || refused != 1 {
		t.Fatalf("clones %v refused %d: %v", repos, refused, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "repos")); len(entries) != 1 {
		t.Fatalf("the refused clone was left on disk: %d entries", len(entries))
	}
}
