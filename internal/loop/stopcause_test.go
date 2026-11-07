package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// Two builds refused at one refusal stage for different causes are a build making progress, not a loop that fails
// the same way: each refusal's reason reaches the next build, and the case builds again. Two refused for the same
// cause, whatever the values in it, stop the case, and the component-health case of the stop names that cause.
func TestBuildRefusalsStopOnlyOnTheSameCause(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	c := testingCase(t, st)
	report := func(window string) string {
		raw, err := json.Marshal(BuildReport{Summary: "The answer is corrected.", Test: []string{"go", "test", "./..."}, Title: "Correct the answer",
			Body:     "Fix one cause with a discriminating test. Prior art needs a network search, which this run cannot perform.",
			Rollback: "Revert the change.", Window: window, Lesson: "Test the answer at its boundary.", Signal: goodSignal})
		if err != nil {
			t.Fatal(err)
		}
		return fixtureResult(`{"type":"result","subtype":"success","structured_output":` + string(raw) + `}`)
	}
	// each build makes the same checked change and reports a window the host refuses at build propose: first one it
	// cannot read, then two it reads that are not whole weeks
	harness := script(t, t.TempDir(), "harness", `n=$(cat /opt/fixture/builds 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > /opt/fixture/builds
sed -i 's/return 41/return 42/' /work/fake.go
cat > /work/answer_test.go <<'TEST'
`+answerTest+`
TEST
case "$n" in
1) `+report("fortnight")+` ;;
2) `+report("10d")+` ;;
*) `+report("11d")+` ;;
esac`)
	r := runnerFor(t, st, []string{harness})
	r.Ship = shipFor(t, st, f)
	r.Ship.Dir = r.Dir
	r.Ship.Live = false
	r.Toolchains = Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path}
	r.Inputs.ReadDirs = []string{f.main}
	pass := func() Done {
		t.Helper()
		done, err := r.Once(ctx, Options{Families: []string{c.Family}})
		if err != nil || len(done) != 1 {
			t.Fatalf("the pass: %+v %v", done, err)
		}
		return done[0]
	}
	build := func(want string) store.LoopRun {
		t.Helper()
		d := pass()
		if d.Skipped != "" || d.Run.Step != StepBuild {
			t.Fatalf("the build did not run: %+v", d)
		}
		got, err := st.LoopRunByID(ctx, d.Run.ID)
		if err != nil || got.Failure != "build propose" || got.Cause != want {
			t.Fatalf("the refused build's cause, want %q: %+v %v", want, got, err)
		}
		return got
	}
	unreadable := build("refused: invalid measurement window Q: give whole weeks, such as H or H")
	weeks := "refused: a measurement window is a whole number of weeks (H, H and so on)"
	build(weeks)
	if !strings.Contains(unreadable.Summary, `"fortnight"`) {
		t.Fatalf("the run's summary lost the refusal's own text: %q", unreadable.Summary)
	}
	if cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily}); err != nil || len(cs) != 0 {
		t.Fatalf("two refusals for different causes filed a stop: %+v %v", cs, err)
	}
	// the third build runs, and is refused for the second's cause, another value in it
	build(weeks)
	if d := pass(); d.Run.ID != "" || !strings.Contains(d.Skipped, "stopped: its last two build runs failed the same way: build propose: "+weeks) {
		t.Fatalf("two refusals for one cause did not stop the case: %+v", d)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(cs) != 1 {
		t.Fatalf("the stop's component-health case: %+v %v", cs, err)
	}
	want := "component-health/v1:loop:build:build-propose:refused-a-measurement-window-is-a-whole-number-of-weeks-h-h-and-so-on"
	if !slices.Contains(cs[0].Signatures, want) || !strings.HasPrefix(cs[0].Title, "The loop fails the same way twice: build propose: refused: a measurement window") || cs[0].Evidence != 2 {
		t.Fatalf("the stop's case does not name its cause: %q %v %d", cs[0].Title, cs[0].Signatures, cs[0].Evidence)
	}
	if held, err := StoppedOn(ctx, st, cs[0]); err != nil || len(held) != 1 || held[0] != c.ID {
		t.Fatalf("the cases the stop holds: %v %v", held, err)
	}
}

// A store from before causes were kept migrates in place, and its old runs have none: two old refusals at one stage
// no longer stop their case, nor does an old one with a new one of the same text, since a missing cause matches
// nothing. Two new ones for the same cause stop it, and two host-stage failures stop on their stage as before.
func TestAStoreFromBeforeCausesStopsOnNoOldRefusal(t *testing.T) {
	ctx := context.Background()
	st, path := testStoreAt(t)
	old := openCase(t, st, "failures", "old", 1)
	mixed := openCase(t, st, "failures", "mixed", 1)
	host := openCase(t, st, "failures", "host", 1)
	now := time.Now()
	put := func(st *store.Store, c store.Case, id, failure, cause string, ago time.Duration) {
		t.Helper()
		at := now.Add(-ago)
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: c.Family, Step: StepBuild, Started: at, Ended: at.Add(time.Second),
			Status: store.RunFailed, Failure: failure, Cause: cause, Summary: failure + ": refused: the same text"}); err != nil {
			t.Fatal(err)
		}
	}
	put(st, old, "old-1", "build propose", "", 4*time.Minute)
	put(st, old, "old-2", "build propose", "", 3*time.Minute)
	put(st, mixed, "mixed-1", "build propose", "", 4*time.Minute)
	put(st, host, "host-1", "build prepare", "", 4*time.Minute)
	put(st, host, "host-2", "build prepare", "", 3*time.Minute)
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE loop_runs DROP COLUMN cause`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if st, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := localRunner(t, st)
	stopped := func(c store.Case) string {
		t.Helper()
		why, err := r.stopped(ctx, c, true)
		if err != nil {
			t.Fatal(err)
		}
		return why
	}
	if why := stopped(old); why != "" {
		t.Fatalf("two old refusals with no cause stopped their case: %s", why)
	}
	cause := refusalCause("refused: the same text")
	put(st, mixed, "mixed-2", "build propose", cause, 2*time.Minute)
	if why := stopped(mixed); why != "" {
		t.Fatalf("an old refusal and a new one stopped their case: %s", why)
	}
	put(st, mixed, "mixed-3", "build propose", cause, time.Minute)
	if why := stopped(mixed); why != "its last two build runs failed the same way: build propose: "+cause {
		t.Fatalf("two new refusals for one cause: %q", why)
	}
	if why := stopped(host); why != "its last two build runs failed the same way: build prepare" {
		t.Fatalf("two old host-stage failures: %q", why)
	}
}

// A component-health case filed before causes were kept names its stop by step and failure alone. One of a host
// stage still holds its case on a migrated store, and its close as fixed resumes it; one of a refusal stage holds
// nothing now, and the case it held builds again: its old runs have no cause, so they make no stop.
func TestAnOldStageOnlyStopStillResumesItsCase(t *testing.T) {
	ctx := context.Background()
	st, path := testStoreAt(t)
	host := openCase(t, st, "failures", "host", 1)
	proposed := openCase(t, st, "failures", "proposed", 1)
	now := time.Now()
	for i, c := range []store.Case{host, host, proposed, proposed} {
		failure := map[string]string{host.ID: "build prepare", proposed.ID: "build propose"}[c.ID]
		at := now.Add(-time.Duration(5-i%2) * time.Minute)
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("run-", i), Case: c.ID, Family: c.Family, Step: StepBuild, Started: at,
			Ended: at.Add(time.Second), Status: store.RunFailed, Failure: failure, Summary: failure + ": refused: a reason"}); err != nil {
			t.Fatal(err)
		}
	}
	file := func(sig, title string) store.Case {
		t.Helper()
		h, _, err := st.ObserveCase(ctx, store.Finding{Family: store.HealthFamily, Signature: sig, Title: title,
			Evidence: []store.Evidence{{Ref: "file:/runs/" + sig, At: now.Add(-time.Minute)}}}, "loop")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	// the signatures as the stop rule wrote them before causes were kept
	prepare := file("component-health/v1:loop:build:build-prepare", "The loop fails the same way twice: build prepare")
	propose := file("component-health/v1:loop:build:build-propose", "The loop fails the same way twice: build propose")
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE loop_runs DROP COLUMN cause`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if st, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := localRunner(t, st)
	if held, err := StoppedOn(ctx, st, propose); err != nil || len(held) != 0 {
		t.Fatalf("an old refusal-stage stop still holds: %v %v", held, err)
	}
	if why, err := r.stopped(ctx, proposed, true); err != nil || why != "" {
		t.Fatalf("the case an old refusal-stage stop held is still stopped: %q %v", why, err)
	}
	held, err := StoppedOn(ctx, st, prepare)
	if err != nil || len(held) != 1 || held[0] != host.ID {
		t.Fatalf("the cases the old host-stage stop holds: %v %v", held, err)
	}
	if why, err := r.stopped(ctx, host, true); err != nil || why == "" {
		t.Fatalf("the old host-stage stop does not hold its case: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, resumed, _, err := st.CloseFixed(ctx, prepare.ID, "git:agent-sh/momentum@4814cc4", held, "the clone storage was fixed", "cli"); err != nil ||
		len(resumed) != 1 || resumed[0] != host.ID {
		t.Fatalf("closing the old stop as fixed: %v %v", resumed, err)
	}
	if at, err := st.ResumedAt(ctx, host.ID); err != nil || at.IsZero() {
		t.Fatalf("the held case has no resumed line: %v %v", at, err)
	}
	if why, err := r.stopped(ctx, host, true); err != nil || why != "" {
		t.Fatalf("the resumed case is still stopped: %q %v", why, err)
	}
}
