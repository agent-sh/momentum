package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// Builds refused at one refusal stage for different causes are a build making progress, not a loop that fails the
// same way: each refusal's reason reaches the next build, and the case builds again. A refusal for a cause an earlier
// one of the stage had, with another value in its text and another cause between them, stops the case, and the
// component-health case of the stop names that cause.
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
	// each build makes the same checked change and reports a window the host refuses at build propose: one it cannot
	// read, one it reads that is not whole weeks, then another it cannot read, which the refusal quotes
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
*) `+report("a month")+` ;;
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
	build := func(want, value string) store.LoopRun {
		t.Helper()
		d := pass()
		if d.Skipped != "" || d.Run.Step != StepBuild {
			t.Fatalf("the build did not run: %+v", d)
		}
		got, err := st.LoopRunByID(ctx, d.Run.ID)
		if err != nil || got.Failure != "build propose" || got.Cause != want || !strings.Contains(got.Summary, value) {
			t.Fatalf("the refused build, want cause %q and %s in its text: %+v %v", want, value, got, err)
		}
		return got
	}
	unreadable := "refused: invalid measurement window Q: give whole weeks, such as H or H"
	build(unreadable, `"fortnight"`)
	build("refused: a measurement window is a whole number of weeks (H, H and so on)", "whole number of weeks")
	if cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily}); err != nil || len(cs) != 0 {
		t.Fatalf("two refusals for different causes filed a stop: %+v %v", cs, err)
	}
	// the third build runs, and is refused for the first's cause, with another window in its text
	build(unreadable, `"a month"`)
	if d := pass(); d.Run.ID != "" || !strings.Contains(d.Skipped, "stopped: two of its build runs were refused for one cause: build propose: "+unreadable) {
		t.Fatalf("two refusals for one cause did not stop the case: %+v", d)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(cs) != 1 || len(cs[0].Signatures) != 1 {
		t.Fatalf("the stop's component-health case: %+v %v", cs, err)
	}
	sig := regexp.MustCompile(`^component-health/v1:loop:build:build-propose:refused-invalid-measurement-window-q-give-whole-weeks-such-as-h-or-h~[0-9a-f]{8}$`)
	if !sig.MatchString(cs[0].Signatures[0]) || !strings.HasPrefix(cs[0].Title, "The loop fails the same way twice: build propose: refused: invalid measurement window Q") || cs[0].Evidence != 2 {
		t.Fatalf("the stop's case does not name its cause: %q %v %d", cs[0].Title, cs[0].Signatures, cs[0].Evidence)
	}
	if held, err := StoppedOn(ctx, st, cs[0]); err != nil || len(held) != 1 || held[0] != c.ID {
		t.Fatalf("the cases the stop holds: %v %v", held, err)
	}
}

// A store from before causes were kept migrates in place, and its old runs have none: two old refusals at one stage
// no longer stop their case, nor does an old one with a new one of the same text, nor an old one with a new host
// error there, since a missing cause matches nothing. Two new ones for the same cause stop it, two new host errors
// at the stage stop it on the stage, and two host-stage failures stop on their stage as before.
func TestAStoreFromBeforeCausesStopsOnNoOldRefusal(t *testing.T) {
	ctx := context.Background()
	st, path := testStoreAt(t)
	old := openCase(t, st, "failures", "old", 1)
	mixed := openCase(t, st, "failures", "mixed", 1)
	host := openCase(t, st, "failures", "host", 1)
	outage := openCase(t, st, "failures", "outage", 1)
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
	put(st, outage, "outage-1", "build propose", "", 4*time.Minute)
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
	if why := stopped(mixed); why != "two of its build runs were refused for one cause: build propose: "+cause {
		t.Fatalf("two new refusals for one cause: %q", why)
	}
	put(st, outage, "outage-2", "build propose", hostCause, 2*time.Minute)
	if why := stopped(outage); why != "" {
		t.Fatalf("an old refusal and a new host error stopped their case: %s", why)
	}
	put(st, outage, "outage-3", "build propose", hostCause, time.Minute)
	if why := stopped(outage); why != "its last two build runs failed the same way: build propose" {
		t.Fatalf("two new host errors at a refusal stage: %q", why)
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

// recordBuild records one build run of c that failed at key with err, as a build records it: stageFailure, then
// finish (the scrubber, the mask, FinishLoopRun).
func recordBuild(t *testing.T, r *Runner, c store.Case, at time.Time, key string, err error) store.LoopRun {
	t.Helper()
	ctx := context.Background()
	r.Now = func() time.Time { return at }
	run, refusal := r.reserve(ctx, c.Family, c.ID, StepBuild)
	if refusal != "" {
		t.Fatalf("reserve: %s", refusal)
	}
	res, held := stageFailure(Result{Status: store.RunOK, Started: true}, key, err)
	if held != nil {
		t.Fatalf("the stage held: %v", held)
	}
	r.Now = func() time.Time { return at.Add(time.Second) }
	r.finish(ctx, c, &run, res, nil)
	got, gerr := r.Store.LoopRunByID(ctx, run.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	return got
}

// A host error at a refusal stage (a network, git or gh failure, a full disk) is no refusal of the build's output: it
// reaches the next build as no refusal.md, and its text, which may hold the build's own title and body, is not
// compared. Twice at one stage stops the case on the stage alone, as a host stage does, under the stage's signature,
// so a component-health case filed for the stage before causes were kept holds the stop.
func TestHostErrorsAtARefusalStageStopOnTheStage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	base := time.Now().Add(-10 * time.Minute)
	old, _, err := st.ObserveCase(ctx, store.Finding{Family: store.HealthFamily, Signature: "component-health/v1:loop:build:build-propose",
		Title: "The loop fails the same way twice: build propose", Evidence: []store.Evidence{{Ref: "file:/runs/old", At: base.Add(-time.Hour)}}}, "loop")
	if err != nil {
		t.Fatal(err)
	}
	network := openCase(t, st, "failures", "network", 1)
	push := func(reason string) error {
		pushErr := fmt.Errorf("git %s: %w: %s", "push --force-with-lease=refs/heads/momentum/c1a2b3c4d origin 0123abcd0123abcd0123abcd0123abcd0123abcd:refs/heads/momentum/c1a2b3c4d",
			errors.New("exit status 128"), "fatal: unable to access 'https://github.com/agent-sh/x.git/': "+reason)
		lsErr := fmt.Errorf("git %s: %w: %s", "ls-remote --refs origin refs/heads/momentum/c1a2b3c4d", errors.New("exit status 128"),
			"fatal: unable to access 'https://github.com/agent-sh/x.git/': "+reason)
		return fmt.Errorf("%w: the push failed (%v) and the remote could not be read (%v)", errPublicationUncertain, pushErr, lsErr)
	}
	disk := openCase(t, st, "failures", "disk", 1)
	commit := func(title, body string) error {
		msg := fmt.Sprintf("%s\n\n%s\n\nCase %s.", title, body, disk.ID)
		return fmt.Errorf("git %s: %w: %s", strings.Join([]string{"commit-tree", "0123abcd0123abcd0123abcd0123abcd0123abcd", "-m", msg}, " "),
			errors.New("exit status 128"), "fatal: unable to write new object: No space left on device")
	}
	for _, step := range []struct {
		c      store.Case
		errors [2]error
	}{
		{network, [2]error{push("Could not resolve host: github.com"), push("Failed to connect to github.com port 443 after 21004 ms: Couldn't connect to server")}},
		{disk, [2]error{commit("Correct the answer", "Fix one cause with a discriminating test."), commit("Retry the call once", "Wrap the call in a single retry.")}},
	} {
		for i, failure := range step.errors {
			if got := recordBuild(t, r, step.c, base.Add(time.Duration(i)*time.Minute), "build propose", failure); got.Cause != hostCause {
				t.Fatalf("%s: a host error kept a cause of its text: %q", step.c.ID, got.Cause)
			}
		}
		if refusal, err := r.lastBuildRefusal(ctx, step.c); err != nil || refusal != "" {
			t.Fatalf("%s: a host error at build propose reached the next build: %q %v", step.c.ID, refusal, err)
		}
		if why, err := r.stopped(ctx, step.c, false); err != nil || why != "its last two build runs failed the same way: build propose" {
			t.Fatalf("%s: two host errors at build propose: %q %v", step.c.ID, why, err)
		}
		base = base.Add(3 * time.Minute)
	}
	held, err := StoppedOn(ctx, st, old)
	if err != nil || len(held) != 2 || !slices.Contains(held, network.ID) || !slices.Contains(held, disk.ID) {
		t.Fatalf("the stage's old case does not hold the host stops: %v %v", held, err)
	}
	if cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily}); err != nil || len(cs) != 1 || cs[0].Evidence != 5 {
		t.Fatalf("the host stops went elsewhere than the stage's case: %+v %v", cs, err)
	}
}

// A refusal's cause is cleaned by the scrubber before it is masked: what the scrubber masks never reaches the cause,
// the signature or the title.
func TestARefusalsCauseIsScrubbed(t *testing.T) {
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "scrub", 1)
	got := recordBuild(t, r, c, time.Now().Add(-time.Minute), "build check", refused("the test command holds password=marker-7f3a"))
	if got.Cause != "refused: the test command holds password=[masked]" || strings.Contains(got.Summary, "marker-7f3a") {
		t.Fatalf("the cause kept what the scrubber masks: %q, summary %q", got.Cause, got.Summary)
	}
}

// One cause masks one way: a run id whatever its digits, a value the scrubber masked whatever its length, a quoted
// value whatever it says. Causes compare on their whole masked text, so two that differ past what a run keeps whole
// stay two, and the signature tells apart causes whose words are the same.
func TestOneCauseMasksOneWay(t *testing.T) {
	for _, p := range [][2]string{
		{"refused: unit momentum-run-loop-patch-20261107-223927-a1b2c3d4 is in use", "refused: unit momentum-run-loop-patch-20261107-224501-12345678 is in use"},
		{"refused: gh: token=" + strings.Repeat("*", 40) + " rejected", "refused: gh: token=" + strings.Repeat("*", 36) + " rejected"},
		{`refused: invalid measurement window "fortnight": give whole weeks, such as 7d or 14d`, `refused: invalid measurement window "a month": give whole weeks, such as 7d or 14d`},
	} {
		if a, b := refusalCause(p[0]), refusalCause(p[1]); a != b {
			t.Errorf("one cause masks two ways: %q vs %q", a, b)
		}
	}
	long := "refused: " + strings.Repeat("the patch changes a file the loop never lands by hand ", 10)
	first, second := refusalCause(long+"first"), refusalCause(long+"second")
	if first == second || utf8.RuneCountInString(first) > causeMax || utf8.RuneCountInString(second) > causeMax {
		t.Fatalf("two long causes: %q %q", first, second)
	}
	sig := func(cause string) string {
		return stopSignature(store.LoopRun{Step: StepBuild, Failure: "build propose", Cause: refusalCause(cause)})
	}
	if a, b := sig("refused: a: b"), sig("refused a b"); a == b {
		t.Fatalf("two causes share a signature: %s", a)
	}
	if a, b := sig(long+"first"), sig(long+"second"); a == b || len(a) > 300 || len(b) > 300 {
		t.Fatalf("two long causes' signatures: %s %s", a, b)
	}
}

// A review's stopped note from a stop the rule no longer counts (two refusals a build kept before causes were) is
// done at the next review pass, and the review goes on; a later stop puts the note again.
func TestAStopTheRuleNoLongerCountsEndsTheReviewsStoppedNote(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.Reviews = nil
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets,
		pass: &passInputs{attempted: true, err: errors.New("marker-7f3a inputs reached")}}
	now := time.Now()
	for i := 0; i < 2; i++ {
		at := now.Add(-time.Duration(3-i) * time.Minute)
		if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("old-", i), Case: c.ID, Step: StepBuild, Family: c.Family,
			Started: at, Ended: at.Add(time.Second), Status: store.RunFailed, Failure: "build propose", Summary: "build propose: refused: a reason"}); err != nil {
			t.Fatal(err)
		}
	}
	id := store.ReviewNoteID(c.ID, "stopped")
	if _, err := s.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: now.Add(-time.Minute),
		Title: "The loop's review for case " + c.ID + " waits: stopped", Detail: "its last two build runs failed the same way: build propose"}); err != nil {
		t.Fatal(err)
	}
	review := func(want string) {
		t.Helper()
		if done := r.reviews(ctx, c.ID); len(done) != 1 || !strings.Contains(done[0].Skipped, want) {
			t.Fatalf("the review, wanting %q: %+v", want, done)
		}
	}
	note := func() store.Note {
		t.Helper()
		n, ok, err := s.Store.Note(ctx, id)
		if err != nil || !ok {
			t.Fatalf("the review's stopped note: %v", err)
		}
		return n
	}
	review("inputs: marker-7f3a inputs reached")
	if note().Done.IsZero() {
		t.Fatal("a stop the rule no longer counts left the review's stopped note standing")
	}
	for i := 0; i < 2; i++ {
		at := time.Now()
		if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("review-", i), Case: c.ID, Step: StepReview, Family: c.Family,
			Started: at, Ended: at.Add(time.Millisecond), Status: store.RunFailed, Failure: "marker-7f3a"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	review("last two review runs failed")
	if !note().Done.IsZero() {
		t.Fatal("a later stop did not put the review's stopped note again")
	}
}

// A build that fails at a host stage before its agent starts (here a toolchain that is gone) is kept at no cost and
// with no start. The stop rule counts it all the same: twice at one stage stops the case, under the stage's signature,
// and the case is not built a third time.
func TestABuildThatNeverStartsStopsOnItsStage(t *testing.T) {
	fakeManager(t) // host ordering only: the build fails before any unit or model starts
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	r.Toolchains = Toolchains{GoRoot: filepath.Join(t.TempDir(), "absent")}
	mechanism, fix := "one cause", "one fix"
	var err error
	if c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || done[0].Run.Failure != "build toolchains" || done[0].Run.USD != 0 || !done[0].Run.Started.IsZero() {
			t.Fatalf("build %d did not fail at its toolchains before it started: %+v %v", i, done, err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
	if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped: its last two build runs failed the same way: build toolchains)") {
		t.Fatalf("two builds that never started did not stop the case: %+v %v", done, err)
	}
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(cs) != 1 || !slices.Equal(cs[0].Signatures, []string{"component-health/v1:loop:build:build-toolchains"}) ||
		cs[0].Title != "The loop fails the same way twice: build toolchains" || cs[0].Evidence != 2 {
		t.Fatalf("the stop's component-health case: %+v %v", cs, err)
	}
	if runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID}); err != nil || len(runs) != 2 {
		t.Fatalf("the stopped case built again: %d %v", len(runs), err)
	}
}
