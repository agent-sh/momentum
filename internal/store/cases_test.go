package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func caseStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// refsAt are n file references dated at, at+1m, ...: evidence that is not an event.
func refsAt(prefix string, n int, at time.Time) []Evidence {
	var out []Evidence
	for i := 0; i < n; i++ {
		out = append(out, Evidence{Ref: fmt.Sprintf("file:/t/%s-%d", prefix, i), At: at.Add(time.Duration(i) * time.Minute), Kind: "test"})
	}
	return out
}

func observeOK(t *testing.T, st *Store, f Finding) (Case, Observed) {
	t.Helper()
	c, obs, err := st.ObserveCase(context.Background(), f, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c, obs
}

// A finding opens a case by its signature, and the same signature lands in the same case again: new evidence is
// added once, the case's own title stays, and evidence it holds already changes nothing.
func TestAFindingOpensACaseAndItsSignatureFindsItAgain(t *testing.T) {
	st := caseStore(t)
	at := time.Now().Add(-time.Hour)
	c, obs := observeOK(t, st, Finding{Family: "failures", Signature: "failures:tool:x", Title: "x fails", Evidence: refsAt("a", 3, at)})
	if obs != Opened || c.Status != CaseOpen || c.Evidence != 3 || c.Title != "x fails" || !slices.Equal(c.Signatures, []string{"failures:tool:x"}) {
		t.Fatalf("opened %s: %+v", obs, c)
	}
	if !c.Seen.Equal(at.Add(2 * time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("seen %v, want its newest evidence %v", c.Seen, at.Add(2*time.Minute))
	}
	again, obs := observeOK(t, st, Finding{Family: "failures", Signature: "failures:tool:x", Title: "x fails differently",
		Evidence: refsAt("a", 4, at)})
	if obs != Updated || again.ID != c.ID || again.Evidence != 4 || again.Title != "x fails" {
		t.Fatalf("again %s: %+v", obs, again)
	}
	same, obs := observeOK(t, st, Finding{Family: "failures", Signature: "failures:tool:x", Evidence: refsAt("a", 4, at)})
	if obs != Unchanged || same.Evidence != 4 {
		t.Fatalf("the same evidence again %s: %+v", obs, same)
	}
	other, obs := observeOK(t, st, Finding{Family: "failures", Signature: "failures:tool:y", Title: "y fails", Evidence: refsAt("a", 1, at)})
	if obs != Opened || other.ID == c.ID {
		t.Fatalf("another signature %s: %+v", obs, other)
	}
}

// An event's reference takes the event's time, kind and summary; a reference to an event the store does not hold is
// refused, and so are a family, a signature or a reference that is not what a case holds.
func TestEvidenceIsCheckedAndAnEventFillsItsOwn(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	e := Event{Source: "mistakes", SourceKey: "k1", TS: time.UnixMilli(1_700_000_000_000), Actor: ActorTool, Kind: "tool-error",
		Tool: "Bash", Summary: "Bash: exit 2"}
	if _, err := st.Put(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: []Evidence{{Ref: "ev:" + e.ID()}}})
	evs, err := st.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "tool-error" || evs[0].Summary != "Bash: exit 2" || !evs[0].At.Equal(e.TS) {
		t.Fatalf("evidence %+v", evs)
	}
	for _, f := range []Finding{
		{Family: "failures", Signature: "s2", Title: "t", Evidence: []Evidence{{Ref: "ev:nope"}}},
		{Family: "nonsense", Signature: "s3", Title: "t"},
		{Family: "failures", Signature: "", Title: "t"},
		{Family: "failures", Signature: "a\nb", Title: "t"},
		{Family: "failures", Signature: "s4", Title: ""},
		{Family: "failures", Signature: "s5", Title: "t", Evidence: []Evidence{{Ref: "-x"}}},
		{Family: "failures", Signature: "root:cache", Title: "a stated cause opens no case"},
	} {
		if _, _, err := st.ObserveCase(ctx, f, "test"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%+v: %v, want ErrBadCase", f, err)
		}
	}
	if cs, err := st.Cases(ctx, CaseFilter{All: true}); err != nil || len(cs) != 1 {
		t.Fatalf("a refused finding left a case: %d %v", len(cs), err)
	}
}

func ptr(s string) *string { return &s }

// An investigation moves a case among open, investigating and testing, and to testing only with a mechanism and a
// next discriminating test on record. Deploying is the ship tool's, measuring and improved the measure tool's.
func TestAnInvestigationMovesACaseOnlyWhereItMay(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	c, _ := observeOK(t, st, Finding{Family: "sessions", Signature: "s", Title: "t", Evidence: refsAt("a", 1, time.Now())})
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseInvestigating}, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: ptr("the cache is stale")}, "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("testing without a next test: %v", err)
	}
	got, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: ptr("the cache is stale"),
		NextTest: ptr("clear it and rerun"), Alternatives: []string{"a race", " "}, Benefit: ptr("no reruns")}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CaseTesting || got.Mechanism != "the cache is stale" || !slices.Equal(got.Alternatives, []string{"a race"}) ||
		got.NextTest != "clear it and rerun" || got.Benefit != "no reruns" {
		t.Fatalf("set %+v", got)
	}
	for _, to := range []CaseStatus{CaseDeployed, CaseMeasuring, CaseClosed, "nonsense"} {
		if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: to}, "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("to %s: %v, want ErrBadCase", to, err)
		}
	}
	if _, err := st.CloseCase(ctx, c.ID, Improved, "", "it worked", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("closed as improved by hand: %v", err)
	}
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseInvestigating}, "agent"); err != nil {
		t.Fatalf("back to investigating: %v", err)
	}
	closed, err := st.CloseCase(ctx, c.ID, NotWorthIt, "", "costs more than it saves", "agent")
	if err != nil || closed.Status != CaseClosed || closed.Reason != NotWorthIt || closed.ClosedEvidence != 1 {
		t.Fatalf("closed %+v %v", closed, err)
	}
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Mechanism: ptr("something else")}, "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("a closed case changed: %v", err)
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range log {
		actions = append(actions, e.Action)
	}
	if !slices.Equal(actions, []string{"opened", "set", "set", "set", "closed"}) {
		t.Fatalf("log %v", actions)
	}
}

// closedAt moves a case's close back to at, as if it had closed then: the store dates a close now, and evidence is
// never dated after now.
func closedAt(t *testing.T, st *Store, id string, at time.Time) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), `UPDATE cases SET closed = ? WHERE id = ?`, at.UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
}

// evidenceAt are file references, one at each time.
func evidenceAt(prefix string, ats ...time.Time) []Evidence {
	var out []Evidence
	for i, at := range ats {
		out = append(out, Evidence{Ref: fmt.Sprintf("file:/t/%s-%d", prefix, i), At: at, Kind: "test"})
	}
	return out
}

// A dismissed case keeps what comes after it closed and comes back only once that reaches what it held at the close:
// the problem has happened as often again. Evidence dated before the close is no change.
func TestADismissedCaseComesBackOnlyOnAsMuchNewEvidence(t *testing.T) {
	for _, reason := range []CloseReason{NotWorthIt, Insufficient} {
		t.Run(string(reason), func(t *testing.T) {
			st := caseStore(t)
			ctx := context.Background()
			t0 := time.Now().Add(-10 * 24 * time.Hour)
			c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 3, t0)})
			if _, err := st.CloseCase(ctx, c.ID, reason, "", "dismissed", "agent"); err != nil {
				t.Fatal(err)
			}
			closedAt(t, st, c.ID, t0.Add(time.Hour))
			if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt("old", 5, t0.Add(30*time.Minute))}); obs != KeptShut || got.Status != CaseClosed {
				t.Fatalf("evidence from before the close %s: %+v", obs, got)
			}
			later := t0.Add(2 * time.Hour)
			if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt("b", 2, later)}); obs != KeptShut || got.Status != CaseClosed {
				t.Fatalf("2 new of 3 %s: %+v", obs, got)
			}
			got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt("c", 1, later)})
			if obs != Reopened || got.Status != CaseOpen || got.Reason != "" || !got.Closed.IsZero() {
				t.Fatalf("3 new of 3 %s: %+v", obs, got)
			}
		})
	}
}

// A case closed as already solved comes back on any evidence after the close, and one closed as improved once its
// last window holds as much as the window before its change did. Either comes back regressed.
func TestASolvedCaseComesBackWhenItRecurs(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	day := 24 * time.Hour
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 3, now.Add(-3*day))})
	if _, err := st.CloseCase(ctx, c.ID, Solved, "", "fixed upstream", "agent"); err != nil {
		t.Fatal(err)
	}
	closedAt(t, st, c.ID, now.Add(-2*day))
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt("b", 1, now.Add(-day))}); obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("a recurrence after already solved %s: %+v", obs, got)
	}

	imp, _ := observeOK(t, st, Finding{Family: "failures", Signature: "i", Title: "t", Evidence: refsAt("i", 4, now.Add(-9*day))})
	// what the measure tool leaves: closed as improved, 4 in the week before its change, 0 after
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET status = 'closed', reason = 'improved', closed = ?, window_ms = ?,
	  before_n = 4, after_n = 0 WHERE id = ?`, now.Add(-8*day).UnixMilli(), (7 * day).Milliseconds(), imp.ID); err != nil {
		t.Fatal(err)
	}
	// one after the close but outside its last window, which slides, and two in it
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "i", Evidence: evidenceAt("j", now.Add(-7*day-time.Hour),
		now.Add(-6*day), now.Add(-6*day+time.Minute))}); obs != KeptShut {
		t.Fatalf("2 in its last window of 4 before %s: %+v", obs, got)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "i", Evidence: evidenceAt("w", now.Add(-time.Hour))}); obs != KeptShut {
		t.Fatalf("3 in its last window, 4 before %s: %+v", obs, got)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "i", Evidence: evidenceAt("k", now.Add(-30*time.Minute))}); obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("back at its old rate %s: %+v", obs, got)
	}
	for _, id := range []string{c.ID, imp.ID} {
		if log, _ := st.CaseLog(ctx, id); log[len(log)-1].Action != "regressed" {
			t.Fatalf("%s's comeback logs %q", id, log[len(log)-1].Action)
		}
	}
	// without a measurement's window, any evidence after the close brings it back
	nw, _ := observeOK(t, st, Finding{Family: "failures", Signature: "nw", Title: "t", Evidence: refsAt("nw", 4, now.Add(-2*day))})
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET status = 'closed', reason = 'improved', closed = ? WHERE id = ?`, now.Add(-day).UnixMilli(), nw.ID); err != nil {
		t.Fatal(err)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "nw", Evidence: refsAt("nw2", 1, now.Add(-time.Hour))}); obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("improved without a window %s: %+v", obs, got)
	}
}

// Cases dedupe by root cause: the investigation names it as a signature, and a case that names one another case
// holds goes into that case, its evidence, signatures and cost with it. What its own symptom brings later lands in
// the case it went into, and a merge that brings a closed case's problem back reopens it.
func TestARootCauseMergesTheCasesThatShareIt(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	a, _ := observeOK(t, st, Finding{Family: "failures", Signature: "sym:a", Title: "a", Evidence: refsAt("a", 2, now.Add(-time.Hour))})
	b, _ := observeOK(t, st, Finding{Family: "failures", Signature: "sym:b", Title: "b", Evidence: refsAt("b", 3, now.Add(-time.Hour))})
	if _, err := st.AddCaseCost(ctx, b.ID, Cost{USD: 1.5, Tokens: 1000, Duration: time.Minute, Runs: 1}, "loop"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(ctx, b.ID, CaseUpdate{Mechanism: ptr("the cache is stale"), NextTest: ptr("clear it"),
		Alternatives: []string{"a race"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, merged, err := st.AddCaseSignature(ctx, a.ID, "root:cache", "agent"); err != nil || merged {
		t.Fatalf("a's root: merged %v, %v", merged, err)
	}
	got, merged, err := st.AddCaseSignature(ctx, b.ID, "root:cache", "agent")
	if err != nil || !merged || got.ID != a.ID {
		t.Fatalf("b's root: %+v merged %v, %v", got, merged, err)
	}
	if got.Evidence != 5 || got.Cost.USD != 1.5 || got.Cost.Runs != 1 || !slices.Equal(got.Signatures, []string{"sym:a", "sym:b", "root:cache"}) ||
		got.Mechanism != "the cache is stale" || got.NextTest != "clear it" || !slices.Equal(got.Alternatives, []string{"a race"}) {
		t.Fatalf("merged %+v", got)
	}
	gone, err := st.CaseByID(ctx, b.ID)
	if err != nil || gone.Status != CaseClosed || gone.Reason != Duplicate || gone.Into != a.ID || gone.Evidence != 0 || gone.Cost != (Cost{}) {
		t.Fatalf("b after the merge %+v %v", gone, err)
	}
	if c, obs := observeOK(t, st, Finding{Family: "failures", Signature: "sym:b", Evidence: refsAt("b2", 1, now)}); c.ID != a.ID || obs != Updated {
		t.Fatalf("b's symptom later went to %s (%s)", c.ID, obs)
	}
	if c, err := st.CaseLive(ctx, b.ID); err != nil || c.ID != a.ID {
		t.Fatalf("b lives in %s %v", c.ID, err)
	}
	if _, err := st.UpdateCase(ctx, b.ID, CaseUpdate{Mechanism: ptr("stale cache")}, "agent"); err != nil {
		t.Fatalf("a change to b goes where it went: %v", err)
	}
	if c, _ := st.CaseByID(ctx, a.ID); c.Mechanism != "stale cache" {
		t.Fatalf("a's mechanism %q", c.Mechanism)
	}
	if c, err := st.AddCaseCost(ctx, b.ID, Cost{USD: 0.5, Runs: 1}, "loop"); err != nil || c.ID != a.ID || c.Cost.USD != 2 {
		t.Fatalf("b's cost goes where it went: %+v %v", c, err)
	}

	// a regression the investigation finds: a new case is a duplicate of a case closed as solved
	if _, err := st.CloseCase(ctx, a.ID, Solved, "", "fixed", "agent"); err != nil {
		t.Fatal(err)
	}
	closedAt(t, st, a.ID, now.Add(-30*time.Minute))
	n, _ := observeOK(t, st, Finding{Family: "failures", Signature: "sym:n", Title: "n", Evidence: refsAt("n", 1, now.Add(-10*time.Minute))})
	got, err = st.CloseCase(ctx, n.ID, Duplicate, a.ID, "the same cache", "agent")
	if err != nil || got.ID != a.ID || got.Status != CaseOpen {
		t.Fatalf("a regression merged into %+v (%v)", got, err)
	}
	if _, err := st.CloseCase(ctx, got.ID, Duplicate, got.ID, "itself", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("into itself: %v", err)
	}
}

// A closed case comes back by hand only on evidence it does not hold.
func TestAReopeningNeedsNewEvidence(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	c, _ := observeOK(t, st, Finding{Family: "owner-burden", Signature: "s", Title: "t", Evidence: refsAt("a", 2, time.Now())})
	if _, err := st.ReopenCase(ctx, c.ID, refsAt("new", 1, time.Now()), "again", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("an open case reopened: %v", err)
	}
	if _, err := st.CloseCase(ctx, c.ID, Insufficient, "", "two turns", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReopenCase(ctx, c.ID, refsAt("a", 2, time.Now()), "again", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("reopened on what it held: %v", err)
	}
	got, err := st.ReopenCase(ctx, c.ID, refsAt("b", 1, time.Now()), "the owner said it again", "agent")
	if err != nil || got.Status != CaseOpen || got.Evidence != 3 {
		t.Fatalf("reopened %+v %v", got, err)
	}
}

// A case unseen past its evidence's retention, and open as long, closes by itself, as insufficient evidence, and
// any evidence after brings it back: the problem stopped, which judges nothing about what it rests on. One opened
// within the retention on older evidence stays its while, and a deployed one is its measurement's.
func TestTheSweepClosesACaseUnseenPastItsRetention(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	old := time.Now().Add(-40 * 24 * time.Hour)
	stale, _ := observeOK(t, st, Finding{Family: "failures", Signature: "old", Title: "old", Evidence: refsAt("o", 12, old)})
	recent, _ := observeOK(t, st, Finding{Family: "failures", Signature: "late", Title: "opened late", Evidence: refsAt("l", 2, old)})
	fresh, _ := observeOK(t, st, Finding{Family: "failures", Signature: "new", Title: "new", Evidence: refsAt("n", 2, time.Now())})
	deployed, _ := observeOK(t, st, Finding{Family: "failures", Signature: "dep", Title: "dep", Evidence: refsAt("d", 2, old)})
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET opened = ? WHERE id IN (?, ?)`, old.UnixMilli(), stale.ID, deployed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET status = 'deployed' WHERE id = ?`, deployed.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := st.SweepCases(ctx, time.Now().Add(-time.Minute), CaseRetention, "loop:test")
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].ID != stale.ID || closed[0].Reason != Insufficient || closed[0].ClosedEvidence != 0 {
		t.Fatalf("swept %+v", closed)
	}
	for _, id := range []string{recent.ID, fresh.ID, deployed.ID} {
		if c, _ := st.CaseByID(ctx, id); c.Status == CaseClosed {
			t.Errorf("%s swept", c.Title)
		}
	}
	if log, _ := st.CaseLog(ctx, stale.ID); log[len(log)-1].Actor != "loop:test" {
		t.Fatalf("the sweep logged as %q", log[len(log)-1].Actor)
	}
	// evidence dated before the sweep's close changes nothing; one after it brings the case back
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "old", Evidence: refsAt("o1", 1, old.Add(time.Hour))}); obs != KeptShut || got.Status != CaseClosed {
		t.Fatalf("old evidence after the sweep %s: %+v", obs, got)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "old", Evidence: refsAt("o2", 1, time.Now().Add(-time.Second))}); obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("one recurrence after the sweep %s: %+v", obs, got)
	}
}

// A case brought back has its retention from then, not from when it first opened: brought back on old evidence,
// the next sweep leaves it.
func TestACaseBroughtBackHasItsRetentionFromThen(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	long := time.Now().Add(-50 * 24 * time.Hour)
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 2, long)})
	if _, err := st.CloseCase(ctx, c.ID, NotWorthIt, "", "rare", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET opened = ?, closed = ? WHERE id = ?`, long.UnixMilli(), long.Add(time.Hour).UnixMilli(), c.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReopenCase(ctx, c.ID, refsAt("b", 1, time.Now().Add(-35*24*time.Hour)), "seen again", "agent")
	if err != nil || got.Back.IsZero() || !got.Seen.Equal(time.Now().Add(-35*24*time.Hour).Truncate(time.Millisecond)) && got.Seen.Before(time.Now().Add(-36*24*time.Hour)) {
		t.Fatalf("reopened %+v %v", got, err)
	}
	swept, err := st.SweepCases(ctx, time.Now(), CaseRetention, "loop:test")
	if err != nil || len(swept) != 0 {
		t.Fatalf("a case brought back today was swept: %+v %v", swept, err)
	}
}

// A case in testing holds a mechanism and the next test, after any change as before it.
func TestATestingCaseKeepsItsMechanismAndNextTest(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 1, time.Now())})
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: ptr("m"), NextTest: ptr("n")}, "agent"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []CaseUpdate{{Mechanism: ptr("")}, {NextTest: ptr("  ")}, {Mechanism: ptr(""), NextTest: ptr("")}} {
		if _, err := st.UpdateCase(ctx, c.ID, u, "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%+v on a testing case: %v", u, err)
		}
	}
	if got, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseInvestigating, Mechanism: ptr("")}, "agent"); err != nil || got.Mechanism != "" {
		t.Fatalf("back to investigating without a mechanism: %+v %v", got, err)
	}
}

// What only the ship and measure tools move stays where it is: a deployed or measuring case is not moved, closed,
// reopened or merged by hand.
func TestADeployedCaseIsTheToolsToMove(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	for _, status := range []CaseStatus{CaseDeployed, CaseMeasuring} {
		c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s" + string(status), Title: "t", Evidence: refsAt(string(status), 1, time.Now())})
		if _, err := st.db.ExecContext(ctx, `UPDATE cases SET status = ? WHERE id = ?`, status, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseInvestigating}, "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%s moved by hand: %v", status, err)
		}
		if _, err := st.CloseCase(ctx, c.ID, NotWorthIt, "", "x", "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%s closed by hand: %v", status, err)
		}
		if _, err := st.ReopenCase(ctx, c.ID, refsAt("new"+string(status), 1, time.Now()), "x", "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%s reopened: %v", status, err)
		}
		other, _ := observeOK(t, st, Finding{Family: "failures", Signature: "o" + string(status), Title: "o", Evidence: refsAt("o"+string(status), 1, time.Now())})
		if _, _, err := st.AddCaseSignature(ctx, other.ID, "root:r"+string(status), "agent"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.AddCaseSignature(ctx, c.ID, "root:r"+string(status), "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%s merged by its root: %v", status, err)
		}
		if got, _ := st.CaseByID(ctx, c.ID); got.Status != status {
			t.Errorf("%s is %s", status, got.Status)
		}
	}
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "x", Title: "x", Evidence: refsAt("x", 1, time.Now())})
	if _, err := st.CloseCase(ctx, c.ID, NotWorthIt, c.ID, "x", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("--into without duplicate: %v", err)
	}
}

// A duplicate's id closes and reopens nothing: the case it went into holds its evidence, and only --into follows
// merges, along a chain of them.
func TestADuplicatesIDDoesNotReachWhereItWent(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	a, _ := observeOK(t, st, Finding{Family: "failures", Signature: "a", Title: "a", Evidence: refsAt("a", 1, now)})
	b, _ := observeOK(t, st, Finding{Family: "failures", Signature: "b", Title: "b", Evidence: refsAt("b", 1, now)})
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "c", Title: "c", Evidence: refsAt("c", 1, now)})
	if _, err := st.CloseCase(ctx, a.ID, Duplicate, b.ID, "same", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseCase(ctx, b.ID, Duplicate, c.ID, "same", "agent"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.CaseLive(ctx, a.ID); err != nil || got.ID != c.ID || got.Evidence != 3 {
		t.Fatalf("a lives in %+v %v", got, err)
	}
	if _, err := st.CloseCase(ctx, a.ID, NotWorthIt, "", "bury it", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("closing a duplicate's id: %v", err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != CaseOpen {
		t.Fatalf("the case it went into is %s", got.Status)
	}
	if _, err := st.CloseCase(ctx, c.ID, NotWorthIt, "", "rare", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReopenCase(ctx, a.ID, refsAt("z", 1, now), "again", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("reopening a duplicate's id: %v", err)
	}
	d, _ := observeOK(t, st, Finding{Family: "failures", Signature: "d", Title: "d", Evidence: refsAt("d", 1, now)})
	if got, err := st.CloseCase(ctx, d.ID, Duplicate, a.ID, "same", "agent"); err != nil || got.ID != c.ID {
		t.Fatalf("--into a duplicate goes where it went: %+v %v", got, err)
	}
}

// An event's reference takes the event's own time, kind and summary whatever the caller gives, so an event from
// before a close cannot bring the case back by a later date; a note's reference takes the note's.
func TestAnEventsReferenceTakesTheEventsOwn(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	before := time.Now().Add(-2 * time.Hour)
	e := Event{Source: "mistakes", SourceKey: "k", TS: before, Actor: ActorTool, Kind: "tool-error", Summary: "exit 2"}
	if _, err := st.Put(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutNote(ctx, Note{ID: "n-1", Kind: NoteProblem, Title: "a disk fills", At: before}); err != nil {
		t.Fatal(err)
	}
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 1, before.Add(-time.Hour))})
	if _, err := st.CloseCase(ctx, c.ID, Solved, "", "fixed", "agent"); err != nil {
		t.Fatal(err)
	}
	closedAt(t, st, c.ID, time.Now().Add(-time.Hour))
	got, obs, err := st.AddCaseEvidence(ctx, c.ID, []Evidence{{Ref: "ev:" + e.ID(), At: time.Now().Add(time.Hour), Kind: "made up", Summary: "made up"},
		{Ref: "note:n-1", At: time.Now().Add(time.Hour)}}, "agent")
	if err != nil || obs != KeptShut || got.Status != CaseClosed {
		t.Fatalf("an event and a note from before the close: %s %+v %v", obs, got, err)
	}
	evs, _ := st.CaseEvidence(ctx, c.ID, 0)
	for _, ev := range evs {
		switch ev.Ref {
		case "ev:" + e.ID():
			if !ev.At.Equal(before.Truncate(time.Millisecond)) || ev.Kind != "tool-error" || ev.Summary != "exit 2" {
				t.Errorf("the event as stored: %+v", ev)
			}
		case "note:n-1":
			if ev.Kind != "note:problem" || ev.Summary != "a disk fills" {
				t.Errorf("the note as stored: %+v", ev)
			}
		}
	}
	got, obs, err = st.AddCaseEvidence(ctx, c.ID, []Evidence{{Ref: "https://example.com/issue/4", At: time.Now()}}, "agent")
	if err != nil || obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("a recurrence after already solved: %s %+v %v", obs, got, err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	if last := log[len(log)-1]; last.Action != "regressed" || !strings.Contains(last.Detail, "1 new") {
		t.Fatalf("the log's last line %+v", last)
	}
	for _, ref := range []string{"typo", "note:nope", "ev:0123", "file:relative/x", "git:repo@abc", "ftp://x"} {
		if _, _, err := st.AddCaseEvidence(ctx, c.ID, []Evidence{{Ref: ref}}, "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("reference %q: %v", ref, err)
		}
	}
	if _, _, err := st.AddCaseSignature(ctx, c.ID, "root:", "agent"); !errors.Is(err, ErrBadCase) {
		t.Errorf("a root without a key: %v", err)
	}
	for _, ref := range []string{"file:/etc/systemd/system/getty@tty1.service", "file:~/.config/systemd/user/momentum@.service@120",
		"file:~/p/node_modules/@types/node/index.d.ts"} {
		if _, _, err := st.AddCaseEvidence(ctx, c.ID, []Evidence{{Ref: ref}}, "agent"); err != nil {
			t.Errorf("reference %q: %v", ref, err)
		}
	}
	for _, ref := range []string{"file:/", "file:~/"} {
		if _, _, err := st.AddCaseEvidence(ctx, c.ID, []Evidence{{Ref: ref}}, "agent"); !errors.Is(err, ErrBadCase) {
			t.Errorf("reference %q names no file: %v", ref, err)
		}
	}
	if _, _, err := st.AddCaseEvidence(ctx, c.ID, []Evidence{{Ref: "file:/t/future", At: time.Now().Add(time.Hour)}}, "agent"); !errors.Is(err, ErrBadCase) {
		t.Errorf("evidence from the future: %v", err)
	}
}

// A working case does not go into a closed case by a signature that one holds: the closed one comes back first, on
// evidence it does not hold, and only then takes it in.
func TestAWorkingCaseDoesNotGoIntoAClosedOne(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	dismissed, _ := observeOK(t, st, Finding{Family: "failures", Signature: "a", Title: "a", Evidence: refsAt("a", 3, now.Add(-time.Hour))})
	if _, _, err := st.AddCaseSignature(ctx, dismissed.ID, "root:r", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseCase(ctx, dismissed.ID, NotWorthIt, "", "rare", "agent"); err != nil {
		t.Fatal(err)
	}
	w, _ := observeOK(t, st, Finding{Family: "failures", Signature: "b", Title: "b", Evidence: refsAt("b", 2, now.Add(-time.Minute))})
	if _, err := st.UpdateCase(ctx, w.ID, CaseUpdate{Status: CaseTesting, Mechanism: ptr("m"), NextTest: ptr("n")}, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCaseSignature(ctx, w.ID, "root:r", "agent"); !errors.Is(err, ErrBadCase) || !strings.Contains(err.Error(), "case reopen") {
		t.Fatalf("into a closed case: %v", err)
	}
	if got, _ := st.CaseByID(ctx, w.ID); got.Status != CaseTesting {
		t.Fatalf("the working case is %s", got.Status)
	}
	if _, err := st.ReopenCase(ctx, dismissed.ID, refsAt("b", 2, now.Add(-time.Minute)), "b shares its root", "agent"); err != nil {
		t.Fatal(err)
	}
	got, merged, err := st.AddCaseSignature(ctx, w.ID, "root:r", "agent")
	if err != nil || !merged || got.ID != dismissed.ID || got.Status != CaseOpen {
		t.Fatalf("after the reopening: %+v %v %v", got, merged, err)
	}
	if _, merged, err := st.AddCaseSignature(ctx, dismissed.ID, "root:r", "agent"); err != nil || merged {
		t.Fatalf("a signature the case holds: %v %v", merged, err)
	}
}

// A merge keeps the target's own investigation, and moves the rest of the duplicate's with all of its cost; every
// earlier duplicate is repointed, so a long chain stays one hop deep.
func TestAMergeKeepsTheTargetsOwnAndMovesTheRest(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	a, _ := observeOK(t, st, Finding{Family: "failures", Signature: "a", Title: "a", Evidence: refsAt("a", 1, now)})
	b, _ := observeOK(t, st, Finding{Family: "failures", Signature: "b", Title: "b", Evidence: refsAt("b", 1, now)})
	if _, err := st.UpdateCase(ctx, a.ID, CaseUpdate{Mechanism: ptr("a's"), NextTest: ptr("a's test")}, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(ctx, b.ID, CaseUpdate{Mechanism: ptr("b's"), NextTest: ptr("b's test"), Benefit: ptr("b's benefit"),
		Alternatives: []string{"b's alternative"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddCaseCost(ctx, b.ID, Cost{USD: 2, Tokens: 500, Duration: time.Minute, Runs: 2}, "loop"); err != nil {
		t.Fatal(err)
	}
	got, err := st.CloseCase(ctx, b.ID, Duplicate, a.ID, "same", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mechanism != "a's" || got.NextTest != "a's test" || got.Benefit != "b's benefit" || !slices.Equal(got.Alternatives, []string{"b's alternative"}) ||
		got.Cost != (Cost{USD: 2, Tokens: 500, Duration: time.Minute, Runs: 2}) {
		t.Fatalf("merged %+v", got)
	}
	log, _ := st.CaseLog(ctx, a.ID)
	if last := log[len(log)-1]; last.Action != "merged" || !strings.Contains(last.Detail, "its benefit, alternatives") {
		t.Fatalf("the merge's line %+v", last)
	}
	prev := a.ID
	first := a.ID
	for i := 0; i < mergeHops+4; i++ {
		n, _ := observeOK(t, st, Finding{Family: "failures", Signature: fmt.Sprint("n", i), Title: "n", Evidence: refsAt(fmt.Sprint("n", i), 1, now)})
		if _, err := st.CloseCase(ctx, prev, Duplicate, n.ID, "same", "agent"); err != nil {
			t.Fatalf("merge %d: %v", i, err)
		}
		prev = n.ID
	}
	if c, err := st.CaseLive(ctx, first); err != nil || c.ID != prev {
		t.Fatalf("the first case after %d merges lives in %s: %v", mergeHops+4, c.ID, err)
	}
	if c, _ := st.CaseByID(ctx, first); c.Into != prev {
		t.Fatalf("the first case points to %s, not its live case %s", c.Into, prev)
	}
}

// What a case's store refuses: an empty title, a negative cost.
func TestACaseRefusesAnEmptyTitleAndANegativeCost(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 1, time.Now())})
	if _, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Title: ptr("  ")}, "agent"); !errors.Is(err, ErrBadCase) {
		t.Errorf("an empty title: %v", err)
	}
	if _, err := st.AddCaseCost(ctx, c.ID, Cost{USD: -1}, "loop"); !errors.Is(err, ErrBadCase) {
		t.Errorf("a negative cost: %v", err)
	}
}

// A closed case logs one line for each observation that brings it nothing back, and its seen moves with the newest
// evidence, also when a merge brings it.
func TestAClosedCaseLogsOneLineAnObservationAndSeenMoves(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 10, now.Add(-3*time.Hour))})
	if _, err := st.CloseCase(ctx, c.ID, NotWorthIt, "", "rare", "agent"); err != nil {
		t.Fatal(err)
	}
	closedAt(t, st, c.ID, now.Add(-time.Hour))
	for i := 0; i < 5; i++ {
		observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt(fmt.Sprint("o", i), 1, now.Add(-50*time.Minute+time.Duration(i)*time.Minute))})
	}
	log, _ := st.CaseLog(ctx, c.ID)
	if len(log) != 2+5 || log[6].Action != "kept closed" || !strings.Contains(log[6].Detail, "1 new by s") {
		t.Fatalf("log %+v", log)
	}
	if got, _ := st.CaseByID(ctx, c.ID); !got.Seen.Equal(now.Add(-46 * time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("seen %v", got.Seen)
	}
	other, _ := observeOK(t, st, Finding{Family: "failures", Signature: "o", Title: "o", Evidence: refsAt("x", 1, now.Add(-time.Minute))})
	open, _ := observeOK(t, st, Finding{Family: "failures", Signature: "p", Title: "p", Evidence: refsAt("y", 1, now.Add(-time.Hour))})
	if got, err := st.CloseCase(ctx, other.ID, Duplicate, open.ID, "same", "agent"); err != nil || !got.Seen.Equal(now.Add(-time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("seen after a merge %v %v", got.Seen, err)
	}
}

// Cases are a kept table: a store from before them gains them at its next open and keeps its events, the report's
// read-only open refuses such a store until then, and nothing drops the cases after.
func TestTheCasesAreKept(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, []Event{{Source: "mistakes", SourceKey: "k", TS: time.Now(), Actor: ActorTool, Kind: "tool-error"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range caseTableNames {
		if _, err := db.Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("a store without the case tables read as current: %v", err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if evs, err := st.Events(ctx, Query{Source: "mistakes"}); err != nil || len(evs) != 1 {
		t.Fatalf("the events of a store from before the cases: %d %v", len(evs), err)
	}
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "kept", Evidence: refsAt("a", 1, time.Now())})
	if _, err := st.Rebuild(ctx, "mistakes", false); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.CaseByID(ctx, c.ID)
	if err != nil || got.Title != "kept" || got.Evidence != 1 {
		t.Fatalf("after a rebuild and a reopen: %+v %v", got, err)
	}
	if !strings.HasPrefix(got.ID, "c") || len(got.ID) != 9 {
		t.Fatalf("id %q", got.ID)
	}
}

// A reopening by hand of a case closed as already solved comes back regressed; a close and a reopening say why;
// a case open for long with fresh evidence is not swept; a comeback on its own evidence stamps when it came back.
func TestTheLastRulesOfACasesComeBack(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 1, now.Add(-2*time.Hour))})
	if _, err := st.CloseCase(ctx, c.ID, Solved, "", " ", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("a close without why: %v", err)
	}
	if _, err := st.CloseCase(ctx, c.ID, Solved, "", "fixed", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReopenCase(ctx, c.ID, refsAt("b", 1, now.Add(-time.Minute)), "", "agent"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("a reopening without why: %v", err)
	}
	if _, err := st.ReopenCase(ctx, c.ID, refsAt("b", 1, now.Add(-time.Minute)), "it recurred", "agent"); err != nil {
		t.Fatal(err)
	}
	if log, _ := st.CaseLog(ctx, c.ID); log[len(log)-1].Action != "regressed" {
		t.Fatalf("a solved case reopened by hand logs %q", log[len(log)-1].Action)
	}
	old, _ := observeOK(t, st, Finding{Family: "failures", Signature: "o", Title: "o", Evidence: refsAt("o", 1, now.Add(-5*24*time.Hour))})
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET opened = ? WHERE id = ?`, now.Add(-40*24*time.Hour).UnixMilli(), old.ID); err != nil {
		t.Fatal(err)
	}
	if swept, err := st.SweepCases(ctx, now, CaseRetention, "loop:test"); err != nil || len(swept) != 0 {
		t.Fatalf("a long-open case with fresh evidence was swept: %+v %v", swept, err)
	}
	d, _ := observeOK(t, st, Finding{Family: "failures", Signature: "d", Title: "d", Evidence: refsAt("d", 1, now.Add(-3*time.Hour))})
	if _, err := st.CloseCase(ctx, d.ID, Solved, "", "fixed", "agent"); err != nil {
		t.Fatal(err)
	}
	closedAt(t, st, d.ID, now.Add(-2*time.Hour))
	got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "d", Evidence: refsAt("d2", 1, now.Add(-time.Hour))})
	if obs != Reopened || got.Back.IsZero() {
		t.Fatalf("a comeback on its own evidence: %s back %v", obs, got.Back)
	}
}

// Recording a root merges only the original case. Once it went into another case, a later report is a no-op
// with an explicit result; it neither adds a signature nor merges the holder it never owned.
func TestRecordRootKeepsTheAssignedCase(t *testing.T) {
	ctx := context.Background()
	st := caseStore(t)
	makeCase := func(sig string) Case {
		c, _ := observeOK(t, st, Finding{Family: "failures", Signature: sig, Title: sig, Evidence: refsAt(sig, 1, time.Now())})
		return c
	}
	a, b, c := makeCase("a"), makeCase("b"), makeCase("c")
	for _, v := range []struct{ id, sig string }{{b.ID, "root:b"}, {c.ID, "root:c"}} {
		if _, recorded, err := st.RecordRoot(ctx, v.id, v.sig, "fixture"); err != nil || !recorded {
			t.Fatalf("record root: %v %v", recorded, err)
		}
	}
	if got, recorded, err := st.RecordRoot(ctx, a.ID, "root:b", "fixture"); err != nil || !recorded || got.ID != b.ID {
		t.Fatalf("ordinary merge: %+v %v %v", got, recorded, err)
	}
	for _, sig := range []string{"root:c", "root:new"} {
		if got, recorded, err := st.RecordRoot(ctx, a.ID, sig, "fixture"); err != nil || recorded || got.ID != a.ID || got.Into != b.ID {
			t.Fatalf("stale root: %+v %v %v", got, recorded, err)
		}
	}
	got, err := st.CaseLive(ctx, b.ID)
	if err != nil || got.ID != b.ID || slices.Contains(got.Signatures, "root:c") || slices.Contains(got.Signatures, "root:new") {
		t.Fatalf("alias target changed: %+v %v", got, err)
	}
}
