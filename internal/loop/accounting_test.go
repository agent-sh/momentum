package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The week's spend holds the runs as the day's does: a run starts only when the week's spend by the live runs, those
// in flight at their whole budget, leaves room for its own, whatever room the day has.
func TestTheWeeksSpendHoldsTheRuns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	r.Budgets.Day, r.Budgets.Week = 100, 10
	openCase(t, st, "failures", "a", 2)
	// 7 dollars three days ago: the day is empty, and the week has room for 3 more
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "r0", Case: "c0", Family: "failures", Step: StepInvestigate, Started: time.Now().Add(-72 * time.Hour),
		Ended: time.Now().Add(-71 * time.Hour), Status: store.RunOK, USD: 7}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunRefused || !strings.Contains(done[0].Skipped, "week budget") {
		t.Fatalf("over the week's budget: %+v %v", done, err)
	}
	// a week and a day later those 7 dollars are out of the week
	r.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if why, err := r.affordable(ctx); err != nil || why != "" {
		t.Fatalf("a week later: %q %v", why, err)
	}
}

// localRunner exercises selection and accounting without starting an agent or requiring a sandbox.
func localRunner(t *testing.T, st *store.Store) *Runner {
	t.Helper()
	dir := t.TempDir()
	return &Runner{Store: st, Ship: &Ship{Store: st, Dir: dir}, Dir: dir,
		Families: []string{"failures"}, Budgets: DefaultBudgets}
}

// A running reservation is stale only after its allowed duration, with no extra grace period.
func TestARunIsStaleOnlyPastTheTimeARunMayLast(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	now := time.Now().Truncate(time.Millisecond)
	r.Now = func() time.Time { return now }
	r.Budgets.RunTime = 30 * time.Minute
	for id, age := range map[string]time.Duration{
		"young": 29 * time.Minute, "boundary": 30 * time.Minute,
		"just stale": 30*time.Minute + time.Millisecond, "old": 40 * time.Minute,
	} {
		c := openCase(t, st, "failures", id, 1)
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: id, Case: c.ID, Family: "failures", Step: StepInvestigate, Started: now.Add(-age),
			Status: store.RunRunning, USD: 4}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"young", "boundary"} {
		if got, err := st.LoopRunByID(ctx, id); err != nil || got.Status != store.RunRunning {
			t.Errorf("%s was ended within its duration: %+v %v", id, got, err)
		}
	}
	for _, id := range []string{"just stale", "old"} {
		if got, err := st.LoopRunByID(ctx, id); err != nil || got.Status != store.RunFailed || got.Failure != "killed" || got.USD != 4 {
			t.Errorf("%s was not ended and charged past its duration: %+v %v", id, got, err)
		}
	}
}

// The stop rule reads the last two runs of one step that ran: a build and a review that failed alike are two steps,
// not the same way twice.
func TestTheStopRuleReadsOneStepAtATime(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	for _, c := range []struct {
		name  string
		steps [2]string
		stop  bool
		gap   int
	}{{"mixed", [2]string{StepBuild, StepReview}, false, 0}, {"one step", [2]string{StepReview, StepReview}, true, 0},
		{"history gap", [2]string{StepInvestigate, StepInvestigate}, true, 20}} {
		cs := openCase(t, st, "failures", c.name, 1)
		now := time.Now()
		for i, step := range c.steps {
			if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint(c.name, i), Case: cs.ID, Family: "failures", Step: step,
				Started: now.Add(-time.Duration(3-i) * time.Minute), Ended: now, Status: store.RunTurns, Failure: "turns"}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < c.gap; i++ {
			if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint(c.name, "-refused-", i), Case: cs.ID, Family: "failures",
				Step: StepInvestigate, Started: now.Add(-3*time.Minute + time.Duration(i+1)*time.Second),
				Ended: now, Status: store.RunRefused, Failure: "day budget"}); err != nil {
				t.Fatal(err)
			}
		}
		why, err := r.stopped(ctx, cs, true)
		if err != nil || (why != "") != c.stop {
			t.Errorf("%s: stopped %q %v, want %v", c.name, why, err, c.stop)
		}
		if c.gap > 0 {
			got, _, why, err := r.pick(ctx, cs.Family, cs.ID, false)
			if err != nil || got.ID != "" || !strings.Contains(why, "stopped") {
				t.Errorf("history gap admitted a third run: %+v %q %v", got, why, err)
			}
		}
	}
}

// What a run of a case that went into another case costs is that case's: it counts in the room of the case that
// holds it, and holds it while it runs.
func TestAMergedCasesRunsCountInTheRoomOfTheCaseThatHoldsIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	a, b := openCase(t, st, "failures", "a", 1), openCase(t, st, "failures", "b", 1)
	if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCaseSignature(ctx, a.ID, "root:fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "merged", Case: a.ID, Family: "failures", Step: StepInvestigate, Started: time.Now(), Status: store.RunRunning, USD: 3}); err != nil {
		t.Fatal(err)
	}
	if spent, running, err := r.caseRoom(ctx, b); err != nil || !running || spent != 3 {
		t.Fatalf("the holder's room: %v %v %v", spent, running, err)
	}
}

// improvedCase is a case closed as improved, whose lesson is not published: the measure tool closes so, and the
// store has no other way in a test.
func improvedCase(t *testing.T) (*store.Store, store.Case) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := openCase(t, st, "failures", "improved", 2)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET status='closed', reason='improved' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	return st, c
}

// A publish run that ends well but leaves no receipt did not publish: it is recorded as a failed run, with the
// failure "no receipt", and two of them in a row stop the case, so the loop does not pick it again at every run until
// a cap stops it. Only a run that left the receipt is done.
func TestAPublishThatLeavesNoReceiptIsAFailureThatStopsTheCase(t *testing.T) {
	ctx := context.Background()
	st, c := improvedCase(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	for i := 0; i < 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{"failures"}})
		if err != nil || len(done) != 1 || done[0].Step != StepPublish || done[0].Run.Status != store.RunFailed || done[0].Run.Failure != "no receipt" {
			t.Fatalf("publish %d: %+v %v", i, done, err)
		}
		if got, _ := st.CaseByID(ctx, c.ID); got.Cost.USD != 0.75*float64(i+1) {
			t.Fatalf("a run that spent money is charged as it is: %+v", got.Cost)
		}
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "stopped: its last two publish runs failed the same way: no receipt") {
		t.Fatalf("the third pick: %+v %v", done, err)
	}
	if cs, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health"}); len(cs) != 1 || !strings.Contains(cs[0].Title, "no receipt") {
		t.Fatalf("the loop's own case: %+v", cs)
	}
}

// A reservation is a retry cooldown, never the durable receipt required for a successful publish run.
func TestAPublishFinishRequiresADurableReceipt(t *testing.T) {
	ctx := context.Background()
	for name, leave := range map[string]func(st *store.Store, c store.Case) error{
		"receipt": func(st *store.Store, c store.Case) error {
			return st.CaseNote(ctx, c.ID, store.LessonPublished, "projects/fake: a lesson", "test")
		},
		"reserved": func(st *store.Store, c store.Case) error {
			_, _, err := st.BeginLesson(ctx, c.ID, "a lesson")
			return err
		},
		"neither": func(st *store.Store, c store.Case) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			st, c := improvedCase(t)
			r := localRunner(t, st)
			run, why := r.reserve(ctx, c.Family, c.ID, StepPublish)
			if why != "" {
				t.Fatal(why)
			}
			if err := leave(st, c); err != nil {
				t.Fatal(err)
			}
			r.finish(ctx, c, &run, Result{Status: store.RunOK, CostKnown: true, USD: 1}, nil)
			wantStatus, wantFailure := store.RunFailed, "no receipt"
			if name == "receipt" {
				wantStatus, wantFailure = store.RunOK, ""
			}
			if got, err := st.LoopRunByID(ctx, run.ID); err != nil || got.Status != wantStatus || got.Failure != wantFailure || got.USD != 1 {
				t.Errorf("%s: %+v %v", name, got, err)
			}
		})
	}
}

// A reserved lesson waits a day before selection retries it. A receipt prevents every later retry.
func TestALessonReservationHasASeparateRetryCooldown(t *testing.T) {
	ctx := context.Background()
	st, c := improvedCase(t)
	r := localRunner(t, st)
	if got, step, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || got.ID != c.ID || step != StepPublish {
		t.Fatalf("unreserved lesson: %+v %s %v", got, step, err)
	}
	if _, _, err := st.BeginLesson(ctx, c.ID, "marker-7f3a"); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || got.ID != "" {
		t.Fatalf("reservation was retried during cooldown: %+v %v", got, err)
	}
	r.Now = func() time.Time { return time.Now().Add(LessonRetry + time.Minute) }
	if got, step, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || got.ID != c.ID || step != StepPublish {
		t.Fatalf("reservation was not retried after cooldown: %+v %s %v", got, step, err)
	}
	if err := st.CaseNote(ctx, c.ID, store.LessonPublished, "marker-7f3a", "test"); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || got.ID != "" {
		t.Fatalf("durable receipt was retried: %+v %v", got, err)
	}
}

// Selection finishes a change in flight before publishing another case's lesson.
func TestATestingCaseComesBeforeAPublishInTheSameFamily(t *testing.T) {
	ctx := context.Background()
	st, improved := improvedCase(t)
	r := localRunner(t, st)
	testing := testingCase(t, st)
	if got, step, _, err := r.pick(ctx, improved.Family, "", true); err != nil || got.ID != testing.ID || step != StepBuild {
		t.Fatalf("testing case lost to publish: %+v %s %v", got, step, err)
	}
}

// waitingFixture is a case in testing whose change used its review rounds: three reviews, each posted, the last on
// the commit the change proposes now.
func waitingFixture(t *testing.T) (*Runner, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	r := localRunner(t, st)
	c := testingCase(t, st)
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", State: Proposed, Head: "abc", PR: "https://example.invalid/pull/9"}
	reviewRounds(&p, "abc", time.Now().Add(-time.Hour))
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	return r, c, p
}

// reviewRounds gives a package its three rounds on head, each finding something medium, the last at the time given.
func reviewRounds(p *Package, head string, last time.Time) {
	p.Reviews = nil
	for i := 1; i <= MaxRounds; i++ {
		p.Reviews = append(p.Reviews, Review{Round: i, Medium: 1, Head: head, Comment: fmt.Sprint("https://example.invalid/comment/", i),
			At: last.Add(-time.Duration(MaxRounds-i) * time.Minute)})
	}
}

func standingNotes(t *testing.T, st *store.Store) (standing []store.Note, all int) {
	t.Helper()
	ns, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.Done.IsZero() {
			standing = append(standing, n)
		}
	}
	return standing, len(ns)
}

// A change that used its three review rounds waits for a person, and the report says so: one passive note for the case
// (a low problem, keyed by it, so every loop run replaces it and adds none), which is done when the case leaves that
// state, and stays resolved when its reader resolved it while the change still waited.
func TestAChangeThatUsedItsReviewRoundsLeavesOnePassiveNote(t *testing.T) {
	ctx := context.Background()
	r, c, p := waitingFixture(t)
	if _, err := r.Once(ctx, Options{Dry: true, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if _, all := standingNotes(t, r.Store); all != 0 {
		t.Fatalf("a dry run wrote %d notes", all)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
			t.Fatal(err)
		}
	}
	standing, all := standingNotes(t, r.Store)
	if len(standing) != 1 || all != 1 {
		t.Fatalf("%d notes stand of %d after three loop runs: %+v", len(standing), all, standing)
	}
	n := standing[0]
	if n.ID != "loop-rounds:"+c.ID || n.Kind != store.NoteProblem || n.Importance != store.ImportanceLow || !strings.Contains(n.Title, c.ID) ||
		len(n.Links) != 1 || n.Links[0].URL != p.PR || !strings.Contains(n.Detail, "close the case") {
		t.Fatalf("the note: %+v", n)
	}
	if d, _, _, _ := r.pick(ctx, c.Family, "", false); d.ID != "" {
		t.Fatalf("a change that used its rounds was picked: %+v", d)
	}
	// its reader resolves it: it stays resolved while the change waits
	if _, _, err := r.Store.DoneNote(ctx, n.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 0 || all != 1 {
		t.Fatalf("a note its reader resolved stands again: %+v", standing)
	}
	// the change is revised and uses its rounds again: a new wait, a note that stands again
	p.Head = "def"
	reviewRounds(&p, "def", time.Now())
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Fatalf("the second wait: %d stand of %d", len(standing), all)
	}
	// the case leaves that state: the note is done
	if _, err := r.Store.CloseCase(ctx, c.ID, store.NotWorthIt, "", "the change is not worth more rounds", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 0 || all != 1 {
		t.Fatalf("the case left the state and its note stands: %+v", standing)
	}
}

// A change with a round left, one whose last review is clean and one revised since its last review are not waiting.
func TestOnlyAChangeWithNoRoundLeftIsWaiting(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(p *Package){
		"a round left": func(p *Package) { p.Reviews = p.Reviews[:MaxRounds-1] },
		"clean":        func(p *Package) { p.Reviews[MaxRounds-1].Medium = 0 },
		"revised":      func(p *Package) { p.Head = "def" },
	} {
		t.Run(name, func(t *testing.T) {
			r, _, p := waitingFixture(t)
			edit(&p)
			if err := r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			if err := r.noteWaiting(ctx); err != nil {
				t.Fatal(err)
			}
			if _, all := standingNotes(t, r.Store); all != 0 {
				t.Fatalf("a change that waits for nothing has %d notes", all)
			}
		})
	}
}

// An unreadable package leaves its waiting note standing, reports the error and recovers when readable again.
func TestAPackageReadErrorPreservesItsWaitingNote(t *testing.T) {
	ctx := context.Background()
	r, c, p := waitingFixture(t)
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Ship.pkgDir(c.ID), "package.json")
	if err := os.WriteFile(path, []byte("{marker-7f3a"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	r.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	// Narrowing case work must not lose another family's unreadable waiting package.
	if _, err := r.Once(ctx, Options{Families: []string{"sessions"}}); err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(logs, "\n"); !strings.Contains(text, c.ID) || !strings.Contains(text, "package") {
		t.Errorf("package read error was not reported: %q", text)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Errorf("package read error resolved its note: %+v", standing)
	}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Fatalf("unchanged package did not recover its note: %+v", standing)
	}
	// Only a known transition out of the waiting state resolves it.
	p.Head = "def"
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 0 || all != 1 {
		t.Fatalf("revised package kept its old waiting note: %+v", standing)
	}
}

// A report's root is recorded for an investigation, a build or a publish, and never for a review, whose report is
// its findings: it cannot merge the case it reviews into another.
func TestOnlyAStepsReportRecordsItsRoot(t *testing.T) {
	ctx := context.Background()
	for step, merged := range map[string]bool{StepReview: false, StepInvestigate: true} {
		st := testStore(t)
		r := runnerFor(t, st, nil)
		a, b := openCase(t, st, "failures", "a", 1), openCase(t, st, "failures", "b", 1)
		if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:fixture", "fixture"); err != nil {
			t.Fatal(err)
		}
		run, why := r.reserve(ctx, a.Family, a.ID, step)
		if why != "" {
			t.Fatal(why)
		}
		r.finish(ctx, a, &run, Result{Status: store.RunOK, CostKnown: true, Report: json.RawMessage(`{"findings":[],"summary":"x","root":"root:fixture"}`)}, nil)
		got, err := st.CaseLive(ctx, a.ID)
		if err != nil || (got.ID == b.ID) != merged {
			t.Errorf("%s: the case is %s, merged into %s: %v, want %v", step, got.ID, b.ID, got.ID == b.ID, merged)
		}
	}
}
