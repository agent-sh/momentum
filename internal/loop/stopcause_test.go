package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
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
	// the stop names the newest refusal as it was given, the signature its masked cause
	newest := `build propose: refused: invalid measurement window "a month": give whole weeks, such as 7d or 14d`
	if d := pass(); d.Run.ID != "" || !strings.Contains(d.Skipped, "stopped: two of its build runs were refused for one cause: "+newest) {
		t.Fatalf("two refusals for one cause did not stop the case: %+v", d)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(cs) != 1 || len(cs[0].Signatures) != 1 {
		t.Fatalf("the stop's component-health case: %+v %v", cs, err)
	}
	sig := regexp.MustCompile(`^component-health/v1:loop:build:build-propose:refused-invalid-measurement-window-q-give-whole-weeks-such-as-h-or-h~[0-9a-f]{8}$`)
	if !sig.MatchString(cs[0].Signatures[0]) || !strings.HasPrefix(cs[0].Title, `The loop fails the same way twice: build propose: refused: invalid measurement window "a month"`) || cs[0].Evidence != 2 {
		t.Fatalf("the stop's case does not name its cause: %q %v %d", cs[0].Title, cs[0].Signatures, cs[0].Evidence)
	}
	if held, err := StoppedOn(ctx, st, cs[0]); err != nil || len(held) != 1 || held[0] != c.ID {
		t.Fatalf("the cases the stop holds: %v %v", held, err)
	}
	// each piece of evidence names its run's refusal as it was given
	evs, err := st.CaseEvidence(ctx, cs[0].ID, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("the stop's evidence: %+v %v", evs, err)
	}
	for _, e := range evs {
		if !strings.Contains(e.Summary, "build propose: refused: invalid measurement window") || !strings.Contains(e.Summary, `"`) {
			t.Fatalf("evidence that does not name its refusal: %q", e.Summary)
		}
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
	if why := stopped(mixed); why != "two of its build runs were refused for one cause: build propose: refused: the same text" {
		t.Fatalf("two new refusals for one cause: %q", why)
	}
	put(st, outage, "outage-2", "build propose", hostCause, 2*time.Minute)
	if why := stopped(outage); why != "" {
		t.Fatalf("an old refusal and a new host error stopped their case: %s", why)
	}
	put(st, outage, "outage-3", "build propose", hostCause, time.Minute)
	if why := stopped(outage); why != "two of its build runs failed the same way: build propose" {
		t.Fatalf("two new host errors at a refusal stage: %q", why)
	}
	if why := stopped(host); why != "two of its build runs failed the same way: build prepare" {
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
		if why, err := r.stopped(ctx, step.c, false); err != nil || why != "two of its build runs failed the same way: build propose" {
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
	if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped: two of its build runs failed the same way: build toolchains)") {
		t.Fatalf("two builds that never started did not stop the case: %+v %v", done, err)
	}
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(cs) != 1 || !slices.Equal(cs[0].Signatures, []string{"component-health/v1:loop:build:build-toolchains"}) ||
		cs[0].Title != "The loop fails the same way twice: build toolchains" || cs[0].Evidence != 2 {
		t.Fatalf("the stop's component-health case: %+v %v", cs, err)
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if err != nil || len(runs) != 2 {
		t.Fatalf("the stopped case built again: %d %v", len(runs), err)
	}
	// each piece of evidence is dated by its build's end, as the stop rule dates a build that never started
	evs, err := r.Store.CaseEvidence(ctx, cs[0].ID, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("the stop's evidence: %+v %v", evs, err)
	}
	for _, e := range evs {
		if e.At.UnixMilli() != runs[0].Ended.UnixMilli() && e.At.UnixMilli() != runs[1].Ended.UnixMilli() {
			t.Fatalf("evidence dated %v, by neither build's end (%v, %v)", e.At, runs[0].Ended, runs[1].Ended)
		}
	}
}

// putBuild keeps one build run of c as a build keeps it, with its cause and its stage's text.
func putBuild(t *testing.T, st *store.Store, c store.Case, id, step, status, failure, cause, text string, at time.Time) {
	t.Helper()
	if err := st.PutLoopRun(context.Background(), store.LoopRun{ID: id, Case: c.ID, Family: c.Family, Step: step, Started: at, Ended: at.Add(time.Second),
		Status: status, Failure: failure, Cause: cause, Summary: text}); err != nil {
		t.Fatal(err)
	}
}

// A refusal stops the case only on an earlier one its build answered for: one of the current package, since the
// newest build that passed every stage. A refusal, a build that passed and its review, then the same refusal is a
// build that made progress, and so is the same refusal of a package prepared since. One more stops the case. The next
// build's refusal.md reads the same span.
func TestARefusalStopsOnlyOnOneItsBuildAnswersFor(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	text := func(exit int) string {
		return fmt.Sprintf("build check: refused: the test fails with the change (exit %d): the change does not pass its own test", exit)
	}
	cause := refusalCause("refused: the test fails with the change (exit 1): the change does not pass its own test")
	base := time.Now().Add(-time.Hour)
	stopped := func(c store.Case) string {
		t.Helper()
		why, err := r.stopped(ctx, c, true)
		if err != nil {
			t.Fatal(err)
		}
		return why
	}
	md := func(c store.Case) string {
		t.Helper()
		refusal, err := r.lastBuildRefusal(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return refusal
	}
	passed := openCase(t, st, "failures", "passed", 1)
	putBuild(t, st, passed, "p1", StepBuild, store.RunFailed, "build check", cause, text(1), base)
	putBuild(t, st, passed, "p2", StepBuild, store.RunOK, "", "", "proposed", base.Add(10*time.Minute))
	putBuild(t, st, passed, "p3", StepReview, store.RunOK, "", "", "two findings", base.Add(20*time.Minute))
	if refusal := md(passed); refusal != "" {
		t.Fatalf("a refusal before a build that passed reached the next build: %q", refusal)
	}
	putBuild(t, st, passed, "p4", StepBuild, store.RunFailed, "build check", cause, text(2), base.Add(30*time.Minute))
	if why := stopped(passed); why != "" {
		t.Fatalf("a refusal stopped on one before a build that passed: %s", why)
	}
	putBuild(t, st, passed, "p5", StepBuild, store.RunFailed, "build check", cause, text(3), base.Add(40*time.Minute))
	if why := stopped(passed); why == "" {
		t.Fatal("two refusals for one cause since the build that passed did not stop the case")
	}
	replaced := openCase(t, st, "failures", "replaced", 1)
	putBuild(t, st, replaced, "q1", StepBuild, store.RunFailed, "build check", cause, text(1), base)
	time.Sleep(5 * time.Millisecond)
	if err := st.CaseNote(ctx, replaced.ID, actionPrepared, "agent-sh/fake at a new base", "loop:test"); err != nil {
		t.Fatal(err)
	}
	if refusal := md(replaced); refusal != "" {
		t.Fatalf("a refusal of the package this one replaced reached its build: %q", refusal)
	}
	now := time.Now().Add(time.Second)
	putBuild(t, st, replaced, "q2", StepBuild, store.RunFailed, "build check", cause, text(2), now)
	if why := stopped(replaced); why != "" {
		t.Fatalf("a refusal stopped on one of the package it replaced: %s", why)
	}
	putBuild(t, st, replaced, "q3", StepBuild, store.RunFailed, "build check", cause, text(3), now.Add(time.Minute))
	if why := stopped(replaced); why == "" {
		t.Fatal("two refusals for one cause of the new package did not stop the case")
	}
	// an investigation and a review between two refusals of one package end nothing: the span is the builds'
	reviewed := openCase(t, st, "failures", "reviewed", 1)
	putBuild(t, st, reviewed, "r1", StepBuild, store.RunFailed, "build check", cause, text(1), base)
	putBuild(t, st, reviewed, "r2", StepInvestigate, store.RunTurns, "turns", "", "turns", base.Add(10*time.Minute))
	putBuild(t, st, reviewed, "r3", StepReview, store.RunOK, "", "", "two findings", base.Add(20*time.Minute))
	if refusal := md(reviewed); !strings.Contains(refusal, "Run r1 failed at build check") {
		t.Fatalf("an investigation or a review hid the refusal before it: %q", refusal)
	}
	putBuild(t, st, reviewed, "r4", StepBuild, store.RunFailed, "build check", cause, text(2), base.Add(30*time.Minute))
	if why := stopped(reviewed); why == "" {
		t.Fatal("an investigation and a review between two refusals for one cause ended the span")
	}
}

// A host error between two refusals hides neither: the next build reads past it to the refusal before it, as past a
// host stage, so a build that repeats that cause read it, and the repeat stops the case.
func TestAHostErrorDoesNotHideTheRefusalBeforeIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "hidden", 1)
	base := time.Now().Add(-time.Hour)
	refusal := `refused: invalid measurement window "fortnight": give whole weeks, such as 7d or 14d`
	putBuild(t, st, c, "b1", StepBuild, store.RunFailed, "build propose", refusalCause(refusal), "build propose: "+refusal, base)
	putBuild(t, st, c, "b2", StepBuild, store.RunFailed, "build propose", hostCause, "build propose: publication uncertain: the push failed", base.Add(10*time.Minute))
	md, err := r.lastBuildRefusal(ctx, c)
	if err != nil || !strings.Contains(md, "Run b1 failed at build propose") || !strings.Contains(md, `"fortnight"`) {
		t.Fatalf("the build after a host error lost the refusal before it: %q %v", md, err)
	}
	putBuild(t, st, c, "b3", StepBuild, store.RunFailed, "build propose", refusalCause(refusal), "build propose: "+refusal, base.Add(20*time.Minute))
	if why, err := r.stopped(ctx, c, true); err != nil || why == "" {
		t.Fatalf("a refusal its build read did not stop the case: %q %v", why, err)
	}
}

// An unreadable build report, whose error is no ErrRefused, is the build's own output: it keeps its text as its cause
// and reaches the next build.
func TestAnUnreadableReportIsARefusal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "report", 1)
	got := recordBuild(t, r, c, time.Now().Add(-time.Minute), "build report", errors.New("build report has a missing required field"))
	if got.Cause != "build report has a missing required field" {
		t.Fatalf("an unreadable report's cause: %q", got.Cause)
	}
	if md, err := r.lastBuildRefusal(ctx, c); err != nil || !strings.Contains(md, "build report has a missing required field") {
		t.Fatalf("an unreadable report did not reach the next build: %q %v", md, err)
	}
}

// A build that never started is dated by its end against a fix's close: two that ended after the component-health case
// of their stage closed as fixed are a stop after the fix, which brings that case back; a stop whose older one ended
// before the close resumes as of the close.
func TestBuildsThatNeverStartedAreDatedByTheirEnd(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	h, _, err := st.ObserveCase(ctx, store.Finding{Family: store.HealthFamily, Signature: "component-health/v1:loop:build:build-toolchains",
		Title: "The loop fails the same way twice: build toolchains", Evidence: []store.Evidence{{Ref: "file:/runs/old", At: time.Now().Add(-time.Hour)}}}, "loop")
	if err != nil {
		t.Fatal(err)
	}
	never := func(c store.Case, id string, ended time.Time) {
		t.Helper()
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: c.Family, Step: StepBuild, Ended: ended,
			Status: store.RunFailed, Failure: "build toolchains", Summary: "build toolchains: the toolchain is gone"}); err != nil {
			t.Fatal(err)
		}
	}
	late := openCase(t, st, "failures", "late", 1)
	never(late, "l1", time.Now().Add(-time.Minute))
	time.Sleep(5 * time.Millisecond)
	if _, _, _, err := st.CloseFixed(ctx, h.ID, "git:agent-sh/momentum@4814cc4", nil, "the toolchain is back", "cli"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	after := openCase(t, st, "failures", "after", 1)
	never(after, "a1", time.Now())
	never(after, "a2", time.Now().Add(time.Second))
	never(late, "l2", time.Now().Add(time.Second))
	// a dry check dates the late stop by its older build too, and holds nothing
	if why, err := r.stopped(ctx, late, true); err != nil || why != "" {
		t.Fatalf("a dry check held a stop whose older build ended before the fix: %q %v", why, err)
	}
	if why, err := r.stopped(ctx, late, false); err != nil || why != "" {
		t.Fatalf("a stop whose older build ended before the fix held its case: %q %v", why, err)
	}
	if at, err := st.ResumedAt(ctx, late.ID); err != nil || at.IsZero() {
		t.Fatalf("the late stop did not resume its case: %v %v", at, err)
	}
	if why, err := r.stopped(ctx, after, false); err != nil || why != "two of its build runs failed the same way: build toolchains" {
		t.Fatalf("two builds that never started, both after the fix: %q %v", why, err)
	}
	if back, err := st.CaseByID(ctx, h.ID); err != nil || back.Status != store.CaseOpen {
		t.Fatalf("the stop after the fix did not bring its case back: %+v %v", back, err)
	}
}

// A build with no Bedrock route (the broker's env file cannot be read) fails as the broker's, which the stop rule does
// not count, as the runs the broker never answered: the case is built again.
func TestABuildWithNoBedrockRouteIsTheBrokers(t *testing.T) {
	fakeManager(t) // host ordering only: the run ends before its harness or a unit starts
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	r.Agent = Agent{Runner: []string{script(t, t.TempDir(), "harness", "exit 9")}, Model: "global.anthropic.claude-opus-5-5",
		Bedrock: &Bedrock{File: filepath.Join(t.TempDir(), "absent.env")}}
	mechanism, fix := "one cause", "one fix"
	var err error
	if c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	for i := 0; i < 3; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || done[0].Skipped != "" || done[0].Run.Failure != "broker" || done[0].Run.USD != 0 {
			t.Fatalf("build %d with no Bedrock route: %+v %v", i, done, err)
		}
	}
	if cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: store.HealthFamily}); err != nil || len(cs) != 0 {
		t.Fatalf("builds with no Bedrock route filed a stop: %+v %v", cs, err)
	}
}

// A build that left no refusal of its output (an agent run that ran out of turns or time, exited, wrote no report,
// met the model's safeguards, or whose summary the cleaner could not clean) hides nothing: the next build reads past
// it to the refusal before it, so a build that repeats that cause read it, and the repeat stops the case.
func TestARunWithNoOutputDoesNotHideTheRefusalBeforeIt(t *testing.T) {
	ctx := context.Background()
	refusal := "refused: the test passes without the change too (exit 0 on the base with the test's files): it does not discriminate the change"
	for _, mid := range []struct{ status, failure string }{
		{store.RunTimeout, "timeout"}, {store.RunTurns, "turns"}, {store.RunFailed, "exit 1"}, {store.RunFailed, "report"},
		{store.RunFailed, "stream"}, {store.RunFailed, "the model's safeguards"}, {store.RunFailed, "scrub"},
	} {
		st := testStore(t)
		r := localRunner(t, st)
		c := openCase(t, st, "failures", "no-output", 1)
		base := time.Now().Add(-time.Hour)
		putBuild(t, st, c, "a1", StepBuild, store.RunFailed, "build check", refusalCause(refusal), "build check: "+refusal, base)
		putBuild(t, st, c, "a2", StepBuild, mid.status, mid.failure, "", "(summary withheld: the cleaner failed)", base.Add(10*time.Minute))
		if md, err := r.lastBuildRefusal(ctx, c); err != nil || !strings.Contains(md, "Run a1 failed at build check") {
			t.Fatalf("%s: the build after it lost the refusal before it: %q %v", mid.failure, md, err)
		}
		putBuild(t, st, c, "a3", StepBuild, store.RunFailed, "build check", refusalCause(refusal), "build check: "+refusal, base.Add(20*time.Minute))
		if why, err := r.stopped(ctx, c, true); err != nil || why == "" {
			t.Fatalf("%s: a refusal its build read did not stop the case: %q %v", mid.failure, why, err)
		}
	}
}

// A host error stops on any earlier one at its stage that its build answered for, not only on the one just before
// it: host errors that take turns between two stages stop, and so do builds that never started, taking turns.
func TestHostErrorsThatTakeTurnsStop(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	base := time.Now().Add(-time.Hour)
	turns := openCase(t, st, "failures", "turns", 1)
	for i, key := range []string{"build check", "build propose"} {
		putBuild(t, st, turns, fmt.Sprint("h", i), StepBuild, store.RunFailed, key, hostCause, key+": a host error", base.Add(time.Duration(i)*time.Minute))
	}
	if why, err := r.stopped(ctx, turns, true); err != nil || why != "" {
		t.Fatalf("two host errors at two stages: %q %v", why, err)
	}
	putBuild(t, st, turns, "h2", StepBuild, store.RunFailed, "build check", hostCause, "build check: another host error", base.Add(2*time.Minute))
	if why, err := r.stopped(ctx, turns, true); err != nil || why != "two of its build runs failed the same way: build check" {
		t.Fatalf("host errors that take turns: %q %v", why, err)
	}
	never := openCase(t, st, "failures", "never", 1)
	for i, key := range []string{"build inputs", "build toolchains", "build inputs"} {
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("n", i), Case: never.ID, Family: never.Family, Step: StepBuild,
			Ended: base.Add(time.Duration(i) * time.Minute), Status: store.RunFailed, Failure: key, Summary: key + ": gone"}); err != nil {
			t.Fatal(err)
		}
	}
	if why, err := r.stopped(ctx, never, true); err != nil || why != "two of its build runs failed the same way: build inputs" {
		t.Fatalf("builds that never started, taking turns: %q %v", why, err)
	}
}

// The gate holds at the check (Sandbox.Run gives a hold), but a build that meets it there has run its agent and been
// paid for: the build keeps the check as the host's error at build check (paidCheck), a counted run, so two paid builds
// busy at the check stop the case, and the case pays for no more than those two runs.
func TestABusyMachineAfterAPaidBuildIsTheHostsErrorAtTheCheck(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "busy", 1)
	// the gate ran and found no room: it exited unsuccessfully, as cmd/momentum's gate program does
	busy := Sandbox{Gate: func(ctx context.Context) error {
		if err := exec.CommandContext(ctx, "false").Run(); err != nil {
			return fmt.Errorf("the capacity gate: %w", err)
		}
		return nil
	}}
	for i := 0; i < 5; i++ {
		if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
			if i != 2 || why != "two of its build runs failed the same way: build check" {
				t.Fatalf("pass %d: %q %v", i, why, err)
			}
			break
		}
		_, err := busy.Run(ctx, t.TempDir(), []string{"go", "test", "./..."}, nil)
		if !held(err) {
			t.Fatalf("a busy machine at the sandbox is no hold: %v", err)
		}
		run, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
		if why != "" {
			t.Fatal(why)
		}
		res, serr := stageFailure(Result{Started: true, CostKnown: true, Status: store.RunOK, RelayAdmitted: 40, RelayCharged: 40, RelayUSD: 3.5},
			"build check", paidCheck(err))
		if done := r.completeStep(ctx, c, run, res, serr); done.Run.Failure != "build check" || done.Run.Cause != hostCause {
			t.Fatalf("a paid build busy at the check: %+v", done)
		}
	}
	runs, err := st.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	got, _ := st.CaseByID(ctx, c.ID)
	if err != nil || len(runs) != 2 || got.Cost.USD > 2*3.5+0.001 {
		t.Fatalf("paid builds busy at the check: %d runs, $%.2f, %v", len(runs), got.Cost.USD, err)
	}
}

// Only a broker that cannot be reached is the broker's: an env file that says the wrong things is the run's own
// failure, which counts, so two builds on it stop the case; and a fallback that found no route after its first attempt
// ran and was charged is no outage either.
func TestOnlyAnUnreachableBrokerIsTheBrokers(t *testing.T) {
	fakeManager(t) // host ordering only: the run ends before its harness or a unit starts
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	wrong := filepath.Join(t.TempDir(), "bedrock.env")
	write(t, wrong, "CLAUDE_CODE_USE_BEDROCK=\"0\"\n")
	r.Agent = Agent{Runner: []string{script(t, t.TempDir(), "harness", "exit 9")}, Model: "global.anthropic.claude-opus-5-5", Bedrock: &Bedrock{File: wrong}}
	mechanism, fix := "one cause", "one fix"
	var err error
	if c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || done[0].Run.Failure != "build run" {
			t.Fatalf("build %d on a wrong route: %+v %v", i, done, err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
	if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped: two of its build runs failed the same way: build run") {
		t.Fatalf("two builds on a wrong route did not stop the case: %+v %v", done, err)
	}
	unread := &unreachable{fmt.Errorf("%w: the broker's env file cannot be read", ErrNoBedrock)}
	if !brokerOutage(Result{}, unread) || brokerOutage(Result{Started: true, USD: 0.5}, unread) ||
		brokerOutage(Result{}, fmt.Errorf("%w: every headless run is on Bedrock", ErrNoBedrock)) {
		t.Fatal("the broker's outage is told wrong")
	}
}

// Two host errors at one stage with a refusal of the build's output between them are apart: the build was making
// progress, so they stop nothing. Host errors that take turns with each other still stop.
func TestHostErrorsApartDoNotStopAProgressingCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "progress", 1)
	base := time.Now().Add(-time.Hour)
	steps := []struct{ failure, cause, text string }{
		{"build propose", hostCause, "build propose: publication uncertain: the push failed (Could not resolve host: github.com)"},
		{"build check", refusalCause("refused: the test passes without the change too"), "build check: refused: the test passes without the change too"},
		{"build report", refusalCause("build report has a missing required field"), "build report: build report has a missing required field"},
		{"build check", refusalCause("refused: the test fails with the change (exit 1)"), "build check: refused: the test fails with the change (exit 1)"},
		{"build propose", hostCause, "build propose: publication uncertain: the push failed (Connection reset by peer)"},
	}
	for i, s := range steps {
		putBuild(t, st, c, fmt.Sprint("p", i), StepBuild, store.RunFailed, s.failure, s.cause, s.text, base.Add(time.Duration(i)*10*time.Minute))
		if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
			t.Fatalf("build %d of a progressing case stopped it: %q %v", i, why, err)
		}
	}
}

// A capacity gate that cannot run (its program is gone) is no machine without room: the check fails as the host's
// error at build check, which counts, so two such builds stop the case instead of charging it every pass.
func TestABrokenGateIsTheHostsErrorAtTheCheck(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "gate", 1)
	broken := Sandbox{Gate: func(ctx context.Context) error {
		if err := exec.CommandContext(ctx, "momentum-test-absent-gate", "--capacity", "20,8").Run(); err != nil {
			return fmt.Errorf("the machine has no room for the test now: %w", err)
		}
		return nil
	}}
	for i := 0; i < 2; i++ {
		_, err := broken.Run(ctx, t.TempDir(), []string{"go", "test", "./..."}, nil)
		if err == nil || held(err) {
			t.Fatalf("a gate that cannot run: %v", err)
		}
		run, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
		if why != "" {
			t.Fatal(why)
		}
		res, serr := stageFailure(Result{Started: true, CostKnown: true, Status: store.RunOK, USD: 4}, "build check", err)
		if done := r.completeStep(ctx, c, run, res, serr); done.Run.Failure != "build check" || done.Run.Cause != hostCause {
			t.Fatalf("a gate that cannot run is not the host's error at the check: %+v", done)
		}
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "two of its build runs failed the same way: build check" {
		t.Fatalf("two builds whose gate cannot run: %q %v", why, err)
	}
}

// A machine with no room for the check holds the build before its agent starts, so a busy pass spends nothing and
// keeps no run.
func TestABusyMachineHoldsTheBuildBeforeItsAgent(t *testing.T) {
	fakeManager(t) // host ordering only: no unit or model starts
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	busy := true
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{Gate: func(ctx context.Context) error {
		if !busy {
			return nil
		}
		if err := exec.CommandContext(ctx, "false").Run(); err != nil {
			return fmt.Errorf("the capacity gate: %w", err)
		}
		return nil
	}})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	var err error
	if c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.HasPrefix(done[0].Skipped, "build check: the machine has no room for the test now") {
			t.Fatalf("pass %d on a busy machine: %+v %v", i, done, err)
		}
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	got, _ := r.Store.CaseByID(ctx, c.ID)
	if err != nil || len(runs) != 0 || got.Cost.USD != 0 || got.Cost.Runs != 0 {
		t.Fatalf("a busy machine kept runs or spent: %+v %+v %v", runs, got.Cost, err)
	}
	if n, ok, err := r.Store.Note(ctx, capacityNoteID(c.ID)); err != nil || !ok || !n.Done.IsZero() {
		t.Fatalf("the hold's note: %+v %v %v", n, ok, err)
	}
	// the next pass finds room: the build goes on past the gate, and the hold's note ends
	busy = false
	if done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID}); err != nil || len(done) != 1 || done[0].Run.ID == "" {
		t.Fatalf("the pass with room: %+v %v", done, err)
	}
	if n, ok, err := r.Store.Note(ctx, capacityNoteID(c.ID)); err != nil || !ok || n.Done.IsZero() || !strings.HasSuffix(n.Detail, roomFound) {
		t.Fatalf("a build found room, but its hold's note: %+v %v %v", n, ok, err)
	}
}

// A case that leaves testing ends its capacity note, as its base and proposal notes.
func TestACapacityNoteEndsWhenItsCaseLeavesTesting(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "busy-leaves", 1)
	mechanism, fix := "one cause", "one fix"
	var err error
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	cause := &buildStageError{key: "build check", err: &shipHold{err: &busyMachine{err: errors.New("the capacity gate: exit status 1 (it said: TIMEOUT)")}}}
	r.buildSkipped(ctx, c, "check", cause)
	if n, ok, err := st.Note(ctx, capacityNoteID(c.ID)); err != nil || !ok || !n.Done.IsZero() {
		t.Fatalf("no open capacity note: %+v %v %v", n, ok, err)
	}
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.releaseHolds(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Note(ctx, capacityNoteID(c.ID)); n.Done.IsZero() || !strings.HasSuffix(n.Detail, caseLeftTesting) {
		t.Fatalf("the case left testing and its capacity note stands: %+v", n)
	}
}

// Old host errors at a refusal stage (no cause; a text that is no refusal, which hostFailure tells) count as the
// host's on both sides: two stop their case under the stage's signature, and a component-health case filed for that
// stage before causes were kept holds the stop.
func TestOldHostErrorsAtARefusalStageStillHold(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "old-host", 1)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2; i++ {
		putBuild(t, st, c, fmt.Sprint("oh", i), StepBuild, store.RunFailed, "build propose", "",
			"build propose: publication uncertain: the push failed (Could not resolve host: github.com)", base.Add(time.Duration(i)*time.Minute))
	}
	old, _, err := st.ObserveCase(ctx, store.Finding{Family: store.HealthFamily, Signature: "component-health/v1:loop:build:build-propose",
		Title: "The loop fails the same way twice: build propose", Evidence: []store.Evidence{{Ref: "file:/runs/oh1", At: base.Add(time.Minute)}}}, "loop")
	if err != nil {
		t.Fatal(err)
	}
	if held, err := StoppedOn(ctx, st, old); err != nil || len(held) != 1 || held[0] != c.ID {
		t.Fatalf("the stage's old case does not hold its old host errors: %v %v", held, err)
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "two of its build runs failed the same way: build propose" {
		t.Fatalf("two old host errors at build propose: %q %v", why, err)
	}
}

// A build kept before the broker's outage had its own key, for a broker env file that could not be read (build run,
// no start), is the broker's after the upgrade too: two such old runs stop nothing.
func TestOldBrokerOutagesDoNotCount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	base := time.Now().Add(-time.Hour)
	// each form the older binary wrote: with the file's path (it could not be opened), and without (it could not be read
	// through)
	for _, text := range []string{"no Bedrock route: the broker's env file /x/bedrock.env cannot be read (no such file or directory)",
		"no Bedrock route: the broker's env file cannot be read"} {
		c := openCase(t, st, "failures", text, 1)
		for i := 0; i < 2; i++ {
			if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint(c.ID, "-", i), Case: c.ID, Family: c.Family, Step: StepBuild,
				Ended: base.Add(time.Duration(i) * time.Minute), Status: store.RunFailed, Failure: "build run", Summary: "build run: " + text}); err != nil {
				t.Fatal(err)
			}
		}
		if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
			t.Fatalf("two old broker outages (%s) stopped the case: %q %v", text, why, err)
		}
	}
}

// Only an env file that cannot be read is the broker's outage: one the scanner cannot read through is, and a relay
// that cannot listen on the run's own socket (the host's storage) is not.
func TestOnlyAnUnreadableEnvFileIsTheBrokers(t *testing.T) {
	long := filepath.Join(t.TempDir(), "bedrock.env")
	write(t, long, "CLAUDE_CODE_USE_BEDROCK=1\nPAD="+strings.Repeat("x", 70<<10)+"\n")
	_, err := (&Bedrock{File: long}).read()
	if !brokerOutage(Result{}, err) {
		t.Fatalf("an env file the scanner cannot read through is not the broker's: %v", err)
	}
	env := filepath.Join(t.TempDir(), "bedrock.env")
	write(t, env, "CLAUDE_CODE_USE_BEDROCK=1\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=1\nANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:9/x\n")
	_, _, err = (&Bedrock{File: env}).open(filepath.Join(t.TempDir(), "gone", "relay.sock"), 1)
	if err == nil || !errors.Is(err, ErrNoBedrock) || brokerOutage(Result{}, err) {
		t.Fatalf("a relay that cannot listen on its own socket is the broker's: %v", err)
	}
}

// A build report a build kept before causes were, unreadable, is the build's refusal by its key, not the host's by its
// text: it reaches the next build, and two of them do not stop the case, since their causes are unknown.
func TestAnOldUnreadableReportIsTheBuildsRefusal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "old-report", 1)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2; i++ {
		putBuild(t, st, c, fmt.Sprint("r", i), StepBuild, store.RunFailed, "build report", "", "build report: build report has a missing required field",
			base.Add(time.Duration(i)*time.Minute))
	}
	if md, err := r.lastBuildRefusal(ctx, c); err != nil || !strings.Contains(md, "Run r1 failed at build report") {
		t.Fatalf("an old unreadable report did not reach the next build: %q %v", md, err)
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("two old unreadable reports stopped the case: %q %v", why, err)
	}
}

// A build stage that fails while the pass is told to stop is stopped, not the host's failure at that stage: it says
// nothing of the host, and the stop rule does not count it. The build's own refusal, and a hold, stay what they are.
func TestAStageThatFailsAsThePassStopsIsStopped(t *testing.T) {
	stopping, cancel := context.WithCancel(context.Background())
	cancel()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "stopping", 1)
	res, err := r.build(stopping, c, store.LoopRun{ID: "20261108-000000-0000aaaa", Case: c.ID, Step: StepBuild})
	if err != nil || res.Failure != "stopped" || res.Cause != "" {
		t.Fatalf("a build input that failed as the pass stopped: %+v %v", res, err)
	}
	for _, s := range []struct {
		key  string
		err  error
		want string
	}{
		{"build toolchains", errors.New("the toolchain is gone"), "stopped"},
		{"build package", errors.New("package is absent"), "stopped"},
		{"build authority", errors.New("case changed during the build"), "stopped"},
		{"build propose", errors.New("git push: signal: killed"), "stopped"},
		{"build propose", refused("a proposal needs its rollback"), "build propose"},
	} {
		if got, _ := stageFailureIn(stopping, Result{}, s.key, s.err); got.Failure != s.want {
			t.Errorf("%s %v as the pass stopped: %q, want %q", s.key, s.err, got.Failure, s.want)
		}
	}
	if _, err := stageFailureIn(stopping, Result{}, "build check", &shipHold{err: errors.New("no room")}); !held(err) {
		t.Fatalf("a hold as the pass stopped: %v", err)
	}
	// a gate ended as the pass stops is no machine without room: the stopped check holds nothing
	refusedRoom := exec.Command("false").Run()
	gate := Sandbox{Gate: func(context.Context) error { return refusedRoom }}
	if err := gate.room(context.Background()); !held(err) {
		t.Fatalf("a gate that refused is no hold: %v", err)
	}
	if err := gate.room(stopping); err == nil || held(err) {
		t.Fatalf("a gate ended as the pass stopped: %v", err)
	}
}

// A later stop of a review with the same why as the stopped note that was done puts the note again: the note's detail
// was rewritten as it was done.
func TestALaterStopWithTheSameWhyPutsTheNoteAgain(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.Reviews = nil
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets,
		pass: &passInputs{attempted: true, err: errors.New("inputs reached")}}
	id := store.ReviewNoteID(c.ID, "stopped")
	same := "its last two review runs failed the same way: marker-7f3a"
	if _, err := s.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: time.Now().Add(-time.Minute),
		Title: "The loop's review for case " + c.ID + " waits: stopped", Detail: same}); err != nil {
		t.Fatal(err)
	}
	if done := r.reviews(ctx, c.ID); len(done) != 1 || !strings.Contains(done[0].Skipped, "inputs reached") {
		t.Fatalf("the first review: %+v", done)
	}
	if n, _, _ := s.Store.Note(ctx, id); n.Done.IsZero() {
		t.Fatal("the stale note is still standing")
	}
	for i := 0; i < 2; i++ {
		at := time.Now()
		if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("review-", i), Case: c.ID, Step: StepReview, Family: c.Family,
			Started: at, Ended: at.Add(time.Millisecond), Status: store.RunFailed, Failure: "marker-7f3a"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if done := r.reviews(ctx, c.ID); len(done) != 1 || !strings.Contains(done[0].Skipped, same) {
		t.Fatalf("the later stop: %+v", done)
	}
	if n, _, _ := s.Store.Note(ctx, id); !n.Done.IsZero() {
		t.Fatalf("a later stop with the same why left the note done: %+v", n)
	}
}

// gateCase is inputRunner's case in testing with a repository, on a ship tool whose sandbox has gate, and an agent
// whose route is wrong: a build that got past the gate fails at build run before its harness.
func gateCase(t *testing.T, gate func(context.Context) error, holds ...int) (*Runner, store.Case, *[]string) {
	t.Helper()
	fakeManager(t) // host ordering only: no unit or model starts
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{Gate: gate, HoldExits: holds})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	wrong := filepath.Join(t.TempDir(), "bedrock.env")
	write(t, wrong, "CLAUDE_CODE_USE_BEDROCK=\"0\"\n")
	r.Agent = Agent{Runner: []string{script(t, t.TempDir(), "harness", "exit 9")}, Model: "global.anthropic.claude-opus-5-5", Bedrock: &Bedrock{File: wrong}}
	var logged []string
	r.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	mechanism, fix := "one cause", "one fix"
	var err error
	if c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	return r, c, &logged
}

// shellGate is a capacity gate that runs a shell line, as cmd/momentum's gate runs its program.
func shellGate(line string) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := exec.CommandContext(ctx, "sh", "-c", line).Run(); err != nil {
			return fmt.Errorf("the capacity gate: %w", err)
		}
		return nil
	}
}

// A capacity gate holds the build only for an exit status it names (none configured is 1, waitfor's deadline) or its
// own deadline. Any other failure (a usage error, a program that is gone) fails the build before its agent, at no cost
// and with no start, as the host's error at the check, logged: the stop rule counts it, so two stop the case under the
// check's signature instead of holding it forever.
func TestAGateThatFailsOtherwiseFailsTheBuildBeforeItsAgent(t *testing.T) {
	for _, gate := range []struct {
		name string
		gate func(context.Context) error
	}{
		{"a usage error", shellGate("echo 'waitfor: unknown argument: --capacty' >&2; exit 4")},
		{"a program that is gone", func(ctx context.Context) error {
			if err := exec.CommandContext(ctx, "momentum-test-absent-gate", "--capacity", "20,8").Run(); err != nil {
				return fmt.Errorf("the capacity gate: %w", err)
			}
			return nil
		}},
	} {
		ctx := context.Background()
		r, c, logged := gateCase(t, gate.gate)
		for i := 0; i < 2; i++ {
			done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
			if err != nil || len(done) != 1 || done[0].Run.Failure != "build check" || done[0].Run.Cause != hostCause || done[0].Run.USD != 0 ||
				!done[0].Run.Started.IsZero() {
				t.Fatalf("%s, build %d: %+v %v", gate.name, i, done, err)
			}
		}
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped: two of its build runs failed the same way: build check)") {
			t.Fatalf("%s: two builds whose gate failed did not stop the case: %+v %v", gate.name, done, err)
		}
		cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
		if err != nil || len(cs) != 1 || !slices.Equal(cs[0].Signatures, []string{"component-health/v1:loop:build:build-check"}) {
			t.Fatalf("%s: the stop's case: %+v %v", gate.name, cs, err)
		}
		said := 0
		for _, line := range *logged {
			if strings.Contains(line, "the capacity gate failed before its agent") {
				said++
			}
		}
		if said != 2 {
			t.Fatalf("%s: the failed gate was not logged at each build: %q", gate.name, *logged)
		}
	}
	// the exit statuses a gate is configured to hold on hold, and 1 no longer does
	r, c, _ := gateCase(t, shellGate("exit 4"), 4)
	if done, err := r.Once(context.Background(), Options{Families: []string{c.Family}, Case: c.ID}); err != nil || len(done) != 1 || done[0].Run.ID != "" ||
		!strings.HasPrefix(done[0].Skipped, "build check: the machine has no room for the test now") {
		t.Fatalf("a gate's configured hold exit: %+v %v", done, err)
	}
	if err := (Sandbox{Gate: shellGate("exit 1"), HoldExits: []int{4}}).room(context.Background()); err == nil || held(err) {
		t.Fatalf("an exit the gate is not configured to hold on: %v", err)
	}
	// a gate whose own deadline ended its wait holds
	if err := (Sandbox{Gate: func(context.Context) error { return fmt.Errorf("its wait passed: %w", context.DeadlineExceeded) }}).room(context.Background()); !held(err) {
		t.Fatalf("a gate its own deadline ended: %v", err)
	}
}

// A pass told to stop while the build waits at the capacity gate ends the build there, stopped, before its agent.
func TestAStoppingPassAtTheGateEndsTheBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, c, _ := gateCase(t, func(gctx context.Context) error {
		cancel() // the pass is told to stop while the gate waits
		<-gctx.Done()
		return fmt.Errorf("the capacity gate: %w", gctx.Err())
	})
	done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, d := range done {
		if d.Step == StepBuild && d.Case == c.ID {
			// it ended at the gate, before its agent: at build check, not at build run
			if d.Run.Failure != "stopped" || d.Run.USD != 0 || !strings.Contains(d.Run.Summary, "build check: the pass was told to stop") {
				t.Fatalf("a build whose pass stopped at the gate: %+v", d)
			}
			return
		}
	}
	t.Fatalf("no build of the case: %+v", done)
}

// A capacity hold keeps one note a case, with what the gate said last. It is raised to normal importance once the
// case's builds have waited a day, and ends when a build finds room; the next hold opens it again, low. A note taken
// out of the report stays out while its hold lasts at that importance, and comes back when the importance rises.
func TestACapacityHoldIsNotedAndRaisedAfterADay(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "busy", 1)
	now := time.Now().Add(-48 * time.Hour)
	r.Now = func() time.Time { return now }
	hold := func(said string) {
		t.Helper()
		cause := &buildStageError{key: "build check", err: &shipHold{err: &busyMachine{err: errors.New("the capacity gate: exit status 1 (it said: " + said + ")")}}}
		if done := r.buildSkipped(ctx, c, "check", cause); !strings.Contains(done.Skipped, said) {
			t.Fatalf("the hold: %+v", done)
		}
	}
	note := func() store.Note {
		t.Helper()
		n, ok, err := st.Note(ctx, capacityNoteID(c.ID))
		if err != nil || !ok {
			t.Fatalf("the capacity note: %v", err)
		}
		return n
	}
	hold("load 31")
	if n := note(); n.Importance != store.ImportanceLow || !strings.Contains(n.Detail, "load 31") {
		t.Fatalf("a new hold's note: %+v", n)
	}
	if _, _, err := st.DoneNote(ctx, capacityNoteID(c.ID), now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	hold("load 29")
	if n := note(); n.Done.IsZero() {
		t.Fatalf("a note taken out of the report came back at the same importance: %+v", n)
	}
	now = now.Add(24 * time.Hour)
	hold("load 30")
	first := note()
	if first.Importance != store.ImportanceNormal || !first.Done.IsZero() || !strings.Contains(first.Detail, "load 30") {
		t.Fatalf("a hold a day old: %+v", first)
	}
	if err := r.endCapacityNote(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if n := note(); n.Done.IsZero() || !strings.HasSuffix(n.Detail, roomFound) {
		t.Fatalf("a build found room but the note stands: %+v", n)
	}
	now = now.Add(time.Hour)
	hold("load 40")
	if n := note(); n.Importance != store.ImportanceLow || !n.Done.IsZero() || n.At.UnixMilli() != now.UnixMilli() {
		t.Fatalf("the next hold's note: %+v", n)
	}
}

// Builds kept before a machine with no room for the check was a hold (build check, no cause, the gate's text) stop
// nothing after the upgrade: this build holds such a pass.
func TestOldBusyChecksDoNotCount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "old-busy", 1)
	base := time.Now().Add(-time.Hour)
	for i, tail := range []string{"exit status 1", "signal: killed"} {
		putBuild(t, st, c, fmt.Sprint("ob", i), StepBuild, store.RunFailed, "build check", "",
			"store copy 30 days\nbuild check: the machine has no room for the test now: "+tail, base.Add(time.Duration(i)*time.Minute))
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("two old busy checks stopped the case: %q %v", why, err)
	}
}

// No git identity and a package with no host clone are the host's and the package's state, not the build's output:
// at build propose they are host errors, no refusal.
func TestAMissingIdentityOrCloneIsNoRefusal(t *testing.T) {
	s := &Ship{}
	if err := s.commitTree(context.Background(), Package{}, "", "", ""); err == nil || errors.Is(err, ErrRefused) || stageCause("build propose", err) != hostCause {
		t.Fatalf("no git identity: %v", err)
	}
	if _, _, err := s.applyPatch(context.Background(), Package{}, nil); err == nil || errors.Is(err, ErrRefused) || stageCause("build propose", err) != hostCause {
		t.Fatalf("no host clone: %v", err)
	}
}

// The host stages are the host's at the stop rule too: host errors at build authority, or at build package, with a host
// error at another stage between them stop on the stage, as any host stage's do.
func TestEveryHostStageStopsOnItsStage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	base := time.Now().Add(-time.Hour)
	for _, stage := range hostStages {
		c := openCase(t, st, "failures", stage, 1)
		putBuild(t, st, c, c.ID+"-1", StepBuild, store.RunFailed, stage, "", stage+": the host failed", base)
		putBuild(t, st, c, c.ID+"-2", StepBuild, store.RunFailed, "build check", hostCause, "build check: a host error", base.Add(time.Minute))
		putBuild(t, st, c, c.ID+"-3", StepBuild, store.RunFailed, stage, "", stage+": the host failed again", base.Add(2*time.Minute))
		if why, err := r.stopped(ctx, c, true); err != nil || why != "two of its build runs failed the same way: "+stage {
			t.Fatalf("%s: %q %v", stage, why, err)
		}
	}
}

// Only a host stage's failure, or a host error at a refusal stage, counts for a build that never started: other runs
// with no start (a reservation that could not be read) do not.
func TestOnlyHostFailuresCountWithNoStart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "reservation", 1)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2; i++ {
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("z", i), Case: c.ID, Family: c.Family, Step: StepBuild,
			Ended: base.Add(time.Duration(i) * time.Minute), Status: store.RunFailed, Failure: "reservation: database is locked"}); err != nil {
			t.Fatal(err)
		}
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("two reservations that could not be read stopped the case: %q %v", why, err)
	}
}

// A refusal whose summary holds no text of its stage (cut off behind a long input summary) leaves the next build no
// refusal.md, whatever refusal came before it: the newest refusal's reason is unknown. Two of them stop on their cause,
// and the stop names it.
func TestARefusalWithNoTextHidesTheOneBefore(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "no-text", 1)
	base := time.Now().Add(-time.Hour)
	cause := refusalCause("refused: the test fails with the change (exit 1)")
	putBuild(t, st, c, "t1", StepBuild, store.RunFailed, "build check", refusalCause("refused: another cause"), "build check: refused: another cause", base)
	putBuild(t, st, c, "t2", StepBuild, store.RunFailed, "build check", cause, "store copy 30 days, and more", base.Add(time.Minute))
	if md, err := r.lastBuildRefusal(ctx, c); err != nil || md != "" {
		t.Fatalf("a refusal with no text gave the next build an older refusal: %q %v", md, err)
	}
	putBuild(t, st, c, "t3", StepBuild, store.RunFailed, "build check", cause, "store copy 30 days, and more", base.Add(2*time.Minute))
	if why, err := r.stopped(ctx, c, true); err != nil || why != "two of its build runs were refused for one cause: build check: "+cause {
		t.Fatalf("two refusals with no text: %q %v", why, err)
	}
}

// A build that never started answers for its package too: one that ended before the package was prepared again does
// not pair with one after.
func TestBuildsThatNeverStartedRespectThePreparedLine(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "prepared", 1)
	never := func(id string, ended time.Time) {
		t.Helper()
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: c.Family, Step: StepBuild, Ended: ended,
			Status: store.RunFailed, Failure: "build toolchains", Summary: "build toolchains: the toolchain is gone"}); err != nil {
			t.Fatal(err)
		}
	}
	never("n1", time.Now().Add(-time.Minute))
	time.Sleep(5 * time.Millisecond)
	if err := st.CaseNote(ctx, c.ID, actionPrepared, "agent-sh/fake at a new base", "loop:test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	never("n2", time.Now())
	if why, err := r.stopped(ctx, c, true); err != nil || why != "" {
		t.Fatalf("a build that never started paired with one of the package before: %q %v", why, err)
	}
}

// A build whose agent ran and was paid for, and whose check then finds the machine busy, is the host's error at build
// check, a counted run, not a hold: holds happen only at the gate before the agent. Two such builds stop the case under
// the check's signature, and the case pays for those two runs and no more.
func TestPaidBuildsBusyAtTheCheckStopTheCase(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	c := testingCase(t, st)
	raw, err := json.Marshal(BuildReport{Summary: "The answer is corrected.", Test: []string{"go", "test", "./..."}, Title: "Correct the answer",
		Body:     "Fix one cause with a discriminating test. Prior art needs a network search, which this run cannot perform.",
		Rollback: "Revert the change.", Window: "7d", Lesson: "Test the answer at its boundary.", Signal: goodSignal})
	if err != nil {
		t.Fatal(err)
	}
	harness := script(t, t.TempDir(), "harness", `sed -i 's/return 41/return 42/' /work/fake.go
cat > /work/answer_test.go <<'TEST'
`+answerTest+`
TEST
`+fixtureResult(`{"type":"result","subtype":"success","structured_output":`+string(raw)+`}`))
	r := runnerFor(t, st, []string{harness})
	r.Ship = shipFor(t, st, f)
	r.Ship.Dir = r.Dir
	r.Ship.Live = false
	r.Toolchains = Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path}
	r.Inputs.ReadDirs = []string{f.main}
	// the gate has room before each build's agent, and none at its check
	calls := 0
	r.Ship.Sandbox.Gate = func(ctx context.Context) error {
		calls++
		if calls%2 == 1 {
			return nil
		}
		if err := exec.CommandContext(ctx, "false").Run(); err != nil {
			return fmt.Errorf("the capacity gate: %w", err)
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}})
		if err != nil || len(done) != 1 || done[0].Run.Failure != "build check" || done[0].Run.Cause != hostCause || done[0].Run.Started.IsZero() {
			t.Fatalf("paid build %d busy at the check: %+v %v", i, done, err)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{c.Family}})
	if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped: two of its build runs failed the same way: build check") {
		t.Fatalf("two paid builds busy at the check did not stop the case: %+v %v", done, err)
	}
	runs, err := st.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	got, _ := st.CaseByID(ctx, c.ID)
	if err != nil || len(runs) != 2 || got.Cost.Runs != 2 || got.Cost.USD > 2*r.Budgets.Run {
		t.Fatalf("paid builds busy at the check: %d runs, %+v, %v", len(runs), got.Cost, err)
	}
	cs, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(cs) != 1 || !slices.Equal(cs[0].Signatures, []string{"component-health/v1:loop:build:build-check"}) {
		t.Fatalf("the stop's case: %+v %v", cs, err)
	}
}
