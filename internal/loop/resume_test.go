package loop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A stop holds until its cause is fixed: closing the component-health case of the stop as already solved, naming
// what fixed it, resumes every case the stop rule held on its signature. Each keeps its status and episode, the
// stop rule counts only its runs after the resume, the next pass works it again, and a case stopped another way
// stays stopped. A fix that does not hold brings the component-health case back.
func TestAFixOfTheStopResumesTheCasesItHeld(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, nil)
	n := 0
	fail := func(c store.Case, step, failure string, ago time.Duration) {
		t.Helper()
		n++
		at := time.Now().Add(-ago)
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: "run-" + string(rune('a'+n)), Case: c.ID, Family: c.Family, Step: step,
			Started: at, Ended: at.Add(time.Second), Status: store.RunFailed, Failure: failure}); err != nil {
			t.Fatal(err)
		}
	}
	a := openCase(t, st, "failures", "a", 3)
	b := openCase(t, st, "failures", "b", 2)
	other := openCase(t, st, "failures", "other", 2)
	mech, fix := "the export reads the lockfile first", "take the lock first"
	b, err := st.UpdateCase(ctx, b.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &fix}, "test")
	if err != nil {
		t.Fatal(err)
	}
	fail(a, StepInvestigate, "turns", 4*time.Minute)
	fail(a, StepInvestigate, "turns", 3*time.Minute)
	fail(b, StepInvestigate, "turns", 4*time.Minute)
	fail(b, StepInvestigate, "turns", 3*time.Minute)
	fail(other, StepInvestigate, "timeout", 4*time.Minute)
	fail(other, StepInvestigate, "timeout", 3*time.Minute)
	for _, c := range []store.Case{a, b, other} {
		if why, err := r.stopped(ctx, c, false); err != nil || why == "" {
			t.Fatalf("%s is not stopped: %q %v", c.ID, why, err)
		}
	}
	health, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(health) != 2 {
		t.Fatalf("the loop's own cases: %+v %v", health, err)
	}
	var turns store.Case
	for _, h := range health {
		if strings.Contains(h.Title, "turns") {
			turns = h
		}
	}
	if turns.ID == "" || turns.Evidence != 4 {
		t.Fatalf("the stop on turns: %+v", turns)
	}
	if got, _, why, err := r.pick(ctx, "failures", "", true); err != nil || got.ID != "" {
		t.Fatalf("a stopped family worked %s: %s %v", got.ID, why, err)
	}

	// closed without naming its fix, the stop would hold on: refused
	if _, err := st.CloseCase(ctx, turns.ID, store.Solved, "", "fixed", "cli"); !errors.Is(err, store.ErrBadCase) || !strings.Contains(err.Error(), "--fix") {
		t.Fatalf("a component-health case closed as solved without its fix: %v", err)
	}
	stopped, err := StoppedOn(ctx, st, turns)
	if err != nil || len(stopped) != 2 || !slices.Contains(stopped, a.ID) || !slices.Contains(stopped, b.ID) {
		t.Fatalf("the cases the stop on turns holds: %v %v", stopped, err)
	}
	if _, _, _, err := st.CloseFixed(ctx, a.ID, "git:agent-sh/momentum@4814cc4", stopped, "the cap was raised", "cli"); !errors.Is(err, store.ErrBadCase) {
		t.Fatalf("a failures case named a fix: %v", err)
	}
	if _, _, _, err := st.CloseFixed(ctx, turns.ID, "commit 4814cc4", stopped, "the cap was raised", "cli"); !errors.Is(err, store.ErrBadCase) {
		t.Fatalf("a fix that is no reference: %v", err)
	}
	if _, err := st.PutNote(ctx, store.Note{ID: store.ReviewNoteID(a.ID, "stopped"), Kind: store.NoteProblem, Importance: store.ImportanceLow,
		At: time.Now(), Title: "The loop's review for case " + a.ID + " waits: stopped", Detail: "its last two review runs failed the same way: report"}); err != nil {
		t.Fatal(err)
	}
	// late has failed once, and its second run is still running at the close: the close does not see it held
	late := openCase(t, st, "failures", "late", 2)
	fail(late, StepInvestigate, "turns", 3*time.Minute)
	// mixed has failed once before the close, and fails again after it
	mixed := openCase(t, st, "failures", "mixed", 2)
	fail(mixed, StepInvestigate, "turns", 3*time.Minute)
	running := time.Now().Add(-time.Minute)
	time.Sleep(5 * time.Millisecond)
	closed, resumed, _, err := st.CloseFixed(ctx, turns.ID, "git:agent-sh/momentum@4814cc4", stopped, "the cap was raised", "cli")
	if err != nil || closed.Status != store.CaseClosed || closed.Reason != store.Solved || len(resumed) != 2 {
		t.Fatalf("the close: %+v %v %v", closed, resumed, err)
	}
	log, _ := st.CaseLog(ctx, turns.ID)
	if last := log[len(log)-1]; !strings.Contains(last.Detail, "fixed by git:agent-sh/momentum@4814cc4; resumes ") {
		t.Fatalf("the close's line: %+v", last)
	}
	for _, c := range []store.Case{a, b} {
		got, _ := st.CaseByID(ctx, c.ID)
		if got.Status != c.Status || got.Episode != c.Episode {
			t.Errorf("%s resumed as %s, episode %d (was %s, %d)", c.ID, got.Status, got.Episode, c.Status, c.Episode)
		}
		log, _ := st.CaseLog(ctx, c.ID)
		if last := log[len(log)-1]; last.Action != store.ActionResumed || !strings.Contains(last.Detail, turns.ID) || !strings.Contains(last.Detail, "4814cc4") {
			t.Errorf("%s's resume line: %+v", c.ID, last)
		}
		if why, err := r.stopped(ctx, got, true); err != nil || why != "" {
			t.Errorf("%s is still stopped: %q %v", c.ID, why, err)
		}
	}
	if why, err := r.stopped(ctx, other, true); err != nil || why == "" {
		t.Fatalf("the case stopped another way resumed: %v", err)
	}
	if n, ok, _ := st.Note(ctx, store.ReviewNoteID(a.ID, "stopped")); !ok || n.Done.IsZero() {
		t.Fatalf("the resumed case's review note stays: %+v", n)
	}
	// its run started before the close and ends after it, failing the same way: the case resumes as of the close
	time.Sleep(5 * time.Millisecond)
	n++
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "run-late", Case: late.ID, Family: late.Family, Step: StepInvestigate,
		Started: running, Ended: time.Now(), Status: store.RunFailed, Failure: "turns"}); err != nil {
		t.Fatal(err)
	}
	if why, err := r.stopped(ctx, late, true); err != nil || why != "" {
		t.Fatalf("a dry check holds the late stop: %q %v", why, err)
	}
	if why, err := r.stopped(ctx, late, false); err != nil || why != "" {
		t.Fatalf("the late stop held its case: %q %v", why, err)
	}
	log, _ = st.CaseLog(ctx, late.ID)
	if last := log[len(log)-1]; last.Action != store.ActionResumed || !strings.Contains(last.Detail, "fixed by git:agent-sh/momentum@4814cc4, after this stop's first run started") ||
		last.At.UnixMilli() != closed.Closed.UnixMilli() {
		t.Fatalf("the late case's resume line, as of the close at %v: %+v", closed.Closed, last)
	}
	if h, _ := st.CaseByID(ctx, turns.ID); h.Status != store.CaseClosed || h.Evidence != turns.Evidence {
		t.Fatalf("the late stop changed the closed case: %s, %d evidence", h.Status, h.Evidence)
	}
	if why, err := r.stopped(ctx, late, false); err != nil || why != "" {
		t.Fatalf("the late case is held after its resume: %q %v", why, err)
	}
	if got, step, why, err := r.pick(ctx, "failures", "", true); err != nil || got.ID != a.ID || step != StepInvestigate {
		t.Fatalf("the next pass: %s %s: %s %v", got.ID, step, why, err)
	}
	if _, _, _, err := st.CloseFixed(ctx, turns.ID, "git:agent-sh/momentum@4814cc4", nil, "again", "cli"); !errors.Is(err, store.ErrBadCase) {
		t.Fatalf("a closed case closed again: %v", err)
	}

	// a resumed case's next failure does not stop it, nor a late-resumed one's: its second run started before the fix
	time.Sleep(5 * time.Millisecond)
	fail(a, StepInvestigate, "turns", 0)
	if why, err := r.stopped(ctx, a, true); err != nil || why != "" {
		t.Fatalf("one failure after the resume stopped it: %q %v", why, err)
	}
	fail(late, StepInvestigate, "turns", 0)
	if _, held, err := stopRuns(ctx, st, late); err != nil || held {
		t.Fatalf("one failure after the late resume stopped it: %v", err)
	}
	// the fix did not hold: mixed fails after the close, resumes as of it, and its next failure is the second after
	// the fix, which stops it and brings the component-health case back
	time.Sleep(5 * time.Millisecond)
	fail(mixed, StepInvestigate, "turns", 0)
	if why, err := r.stopped(ctx, mixed, false); err != nil || why != "" {
		t.Fatalf("one failure after the fix stopped mixed: %q %v", why, err)
	}
	time.Sleep(5 * time.Millisecond)
	fail(mixed, StepInvestigate, "turns", 0)
	if why, err := r.stopped(ctx, mixed, false); err != nil || why == "" {
		t.Fatalf("two failures after the fix did not stop mixed: %v", err)
	}
	back, _ := st.CaseByID(ctx, turns.ID)
	log, _ = st.CaseLog(ctx, turns.ID)
	if back.Status != store.CaseOpen || log[len(log)-1].Action != "regressed" {
		t.Fatalf("the fix that did not hold: %s, %+v", back.Status, log[len(log)-1])
	}
	// a resume brings back only the stopped note: a budget note its reader resolved before the resume stays resolved
	budget := "its review is past the round budget: report"
	if _, err := st.PutNote(ctx, store.Note{ID: store.ReviewNoteID(a.ID, "budget"), Kind: store.NoteProblem, Importance: store.ImportanceLow,
		At: time.Now().Add(-time.Hour), Title: "budget", Detail: budget}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.DoneNote(ctx, store.ReviewNoteID(a.ID, "budget"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.noteReviewSkip(ctx, a, "budget", budget); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := st.Note(ctx, store.ReviewNoteID(a.ID, "budget")); !ok || n.Done.IsZero() {
		t.Fatalf("a resume put back a resolved budget note: %+v", n)
	}
}

// A stop after a resume puts the review's stopped note again, through the review pass: the resume makes the first
// stop's note done, the resumed case's review goes on, and its next two failures stop it and put the note again.
func TestAStopAfterAResumePutsTheReviewNoteAgain(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.Reviews = nil
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets,
		pass: &passInputs{attempted: true, err: errors.New("marker-7f3a inputs reached")}}
	n := 0
	fail := func() {
		t.Helper()
		n++
		at := time.Now()
		if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("review-", n), Case: c.ID, Step: StepReview, Family: c.Family,
			Started: at, Ended: at.Add(time.Millisecond), Status: store.RunFailed, Failure: "marker-7f3a"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	review := func(want string) {
		t.Helper()
		if done := r.reviews(ctx, c.ID); len(done) != 1 || !strings.Contains(done[0].Skipped, want) {
			t.Fatalf("the review, wanting %q: %+v", want, done)
		}
	}
	note := func() store.Note {
		t.Helper()
		n, ok, err := s.Store.Note(ctx, store.ReviewNoteID(c.ID, "stopped"))
		if err != nil || !ok {
			t.Fatalf("the review's stopped note: %v", err)
		}
		return n
	}
	fail()
	fail()
	review("last two review runs failed")
	if !note().Done.IsZero() {
		t.Fatal("the stop put no standing note")
	}
	health, err := s.Store.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(health) != 1 {
		t.Fatalf("the stop's component-health case: %+v %v", health, err)
	}
	stopped, err := StoppedOn(ctx, s.Store, health[0])
	if err != nil || len(stopped) != 1 || stopped[0] != c.ID {
		t.Fatalf("the cases the stop holds: %v %v", stopped, err)
	}
	if _, resumed, _, err := s.Store.CloseFixed(ctx, health[0].ID, "git:agent-sh/momentum@4814cc4", stopped, "the report was fixed", "cli"); err != nil || len(resumed) != 1 {
		t.Fatalf("the fix: %v %v", resumed, err)
	}
	if note().Done.IsZero() {
		t.Fatal("the resume left the stopped note standing")
	}
	time.Sleep(5 * time.Millisecond)
	review("inputs: marker-7f3a inputs reached")
	fail()
	fail()
	review("last two review runs failed")
	if !note().Done.IsZero() {
		t.Fatal("a stop after the resume left the review's stopped note done")
	}
}

// A component-health case closed as already solved before a close named its fix gets its fix named after its close:
// the cases its stop held resume in the same transaction, and it stays closed, in its episode, with its evidence and
// no regressed line. The naming counts as its close: a stop whose older run began before it resumes as of it, and
// two failures after it bring the case back.
func TestNamingTheFixOfAStopClosedBeforeItResumesWithoutAReopen(t *testing.T) {
	ctx := context.Background()
	st, path := testStoreAt(t)
	r := runnerFor(t, st, nil)
	n := 0
	fail := func(c store.Case, started, ended time.Time) {
		t.Helper()
		n++
		if err := st.PutLoopRun(ctx, store.LoopRun{ID: fmt.Sprint("run-", n), Case: c.ID, Family: c.Family, Step: StepInvestigate,
			Started: started, Ended: ended, Status: store.RunFailed, Failure: "turns"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	held := openCase(t, st, "failures", "held", 2)
	fail(held, now.Add(-5*time.Minute), now.Add(-5*time.Minute+time.Second))
	fail(held, now.Add(-4*time.Minute), now.Add(-4*time.Minute+time.Second))
	if why, err := r.stopped(ctx, held, false); err != nil || why == "" {
		t.Fatalf("held is not stopped: %q %v", why, err)
	}
	health, err := st.Cases(ctx, store.CaseFilter{Family: store.HealthFamily})
	if err != nil || len(health) != 1 {
		t.Fatalf("the stop's component-health case: %+v %v", health, err)
	}
	h := health[0]
	// closed as already solved by a version before a close named its fix: the stop holds on
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	closedAt := now.Add(-3 * time.Minute).UnixMilli()
	if _, err := db.Exec(`UPDATE cases SET status = 'closed', reason = 'already-solved', closed = ?, updated = ?, closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = cases.id) WHERE id = ?`,
		closedAt, closedAt, h.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if h, err = st.CaseByID(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if why, err := r.stopped(ctx, held, false); err != nil || why == "" {
		t.Fatalf("held resumed before its fix was named: %v", err)
	}
	// late failed once after the close, and its second run is still running when the fix is named
	late := openCase(t, st, "failures", "late", 2)
	fail(late, now.Add(-2*time.Minute), now.Add(-2*time.Minute+time.Second))
	stopped, err := StoppedOn(ctx, st, h)
	if err != nil || len(stopped) != 1 || stopped[0] != held.ID {
		t.Fatalf("the cases the stop holds: %v %v", stopped, err)
	}
	named, resumed, onlyNamed, err := st.CloseFixed(ctx, h.ID, "git:agent-sh/momentum@4814cc4", stopped, "the cap was raised", "cli")
	if err != nil || len(resumed) != 1 || resumed[0] != held.ID || !onlyNamed {
		t.Fatalf("naming the fix: %v %v %v", resumed, onlyNamed, err)
	}
	if named.Status != store.CaseClosed || named.Reason != store.Solved || named.Episode != h.Episode || !named.Closed.Equal(h.Closed) || named.Evidence != h.Evidence {
		t.Fatalf("naming the fix changed the close: %+v, was %+v", named, h)
	}
	log, _ := st.CaseLog(ctx, h.ID)
	var at time.Time
	for _, e := range log {
		if e.Action == "regressed" || e.Action == "reopened" {
			t.Fatalf("naming the fix reopened the case: %+v", e)
		}
		if e.Action == store.ActionFixed {
			at = e.At
		}
	}
	if last := log[len(log)-1]; at.IsZero() || !strings.Contains(last.Detail, "fixed by git:agent-sh/momentum@4814cc4; resumes "+held.ID+": the cap was raised") {
		t.Fatalf("the naming's lines: %+v", log)
	}
	hl, _ := st.CaseLog(ctx, held.ID)
	if last := hl[len(hl)-1]; last.Action != store.ActionResumed || !strings.Contains(last.Detail, h.ID) || !strings.Contains(last.Detail, "4814cc4") {
		t.Fatalf("held's resume line: %+v", last)
	}
	if why, err := r.stopped(ctx, held, true); err != nil || why != "" {
		t.Fatalf("held is still stopped: %q %v", why, err)
	}
	if _, _, _, err := st.CloseFixed(ctx, h.ID, "git:agent-sh/momentum@5925dd5", nil, "again", "cli"); !errors.Is(err, store.ErrBadCase) || !strings.Contains(err.Error(), "its fix is named already") {
		t.Fatalf("a fix named twice: %v", err)
	}
	// late's second run began before the naming and fails after it: late resumes as of the naming
	time.Sleep(5 * time.Millisecond)
	fail(late, now.Add(-time.Minute), time.Now())
	if why, err := r.stopped(ctx, late, false); err != nil || why != "" {
		t.Fatalf("the late stop held its case: %q %v", why, err)
	}
	ll, _ := st.CaseLog(ctx, late.ID)
	if last := ll[len(ll)-1]; last.Action != store.ActionResumed || last.At.UnixMilli() != at.UnixMilli() {
		t.Fatalf("late's resume line, as of the naming at %v: %+v", at, last)
	}
	if got, _ := st.CaseByID(ctx, h.ID); got.Status != store.CaseClosed || got.Evidence != h.Evidence {
		t.Fatalf("the late stop changed the closed case: %s, %d evidence", got.Status, got.Evidence)
	}
	// the fix did not hold: two failures after the naming stop held and bring the case back
	time.Sleep(5 * time.Millisecond)
	fail(held, time.Now(), time.Now())
	time.Sleep(5 * time.Millisecond)
	fail(held, time.Now(), time.Now())
	if why, err := r.stopped(ctx, held, false); err != nil || why == "" {
		t.Fatalf("two failures after the naming did not stop held: %v", err)
	}
	back, _ := st.CaseByID(ctx, h.ID)
	log, _ = st.CaseLog(ctx, h.ID)
	if back.Status != store.CaseOpen || back.Episode != h.Episode+1 || log[len(log)-1].Action != "regressed" {
		t.Fatalf("the fix that did not hold: %+v, %+v", back, log[len(log)-1])
	}
}
