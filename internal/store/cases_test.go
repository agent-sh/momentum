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
		out = append(out, Evidence{Ref: fmt.Sprintf("file:%s-%d", prefix, i), At: at.Add(time.Duration(i) * time.Minute), Kind: "test"})
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

// A dismissed case keeps what comes after it closed and comes back only once that reaches what it held at the close:
// the problem has happened as often again. Evidence dated before the close is no change.
func TestADismissedCaseComesBackOnlyOnAsMuchNewEvidence(t *testing.T) {
	for _, reason := range []CloseReason{NotWorthIt, Insufficient} {
		t.Run(string(reason), func(t *testing.T) {
			st := caseStore(t)
			ctx := context.Background()
			now := time.Now()
			c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 3, now.Add(-time.Hour))})
			if _, err := st.CloseCase(ctx, c.ID, reason, "", "dismissed", "agent"); err != nil {
				t.Fatal(err)
			}
			if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt("old", 5, now.Add(-30*time.Minute))}); obs != KeptShut || got.Status != CaseClosed {
				t.Fatalf("evidence from before the close %s: %+v", obs, got)
			}
			later := now.Add(time.Hour)
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
// last window holds as much as the window before its change did.
func TestASolvedCaseComesBackWhenItRecurs(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	now := time.Now()
	c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "s", Title: "t", Evidence: refsAt("a", 3, now.Add(-time.Hour))})
	if _, err := st.CloseCase(ctx, c.ID, Solved, "", "fixed upstream", "agent"); err != nil {
		t.Fatal(err)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "s", Evidence: refsAt("b", 1, now.Add(time.Hour))}); obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("a recurrence after already solved %s: %+v", obs, got)
	}

	imp, _ := observeOK(t, st, Finding{Family: "failures", Signature: "i", Title: "t", Evidence: refsAt("i", 4, now.Add(-time.Hour))})
	// what the measure tool leaves: closed as improved, 4 in the week before its change, 0 after
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET status = 'closed', reason = 'improved', closed = ?, window_ms = ?,
	  before_n = 4, after_n = 0 WHERE id = ?`, now.UnixMilli(), (7 * 24 * time.Hour).Milliseconds(), imp.ID); err != nil {
		t.Fatal(err)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "i", Evidence: refsAt("j", 3, now.Add(time.Minute))}); obs != KeptShut {
		t.Fatalf("3 in its window of 4 before %s: %+v", obs, got)
	}
	if got, obs := observeOK(t, st, Finding{Family: "failures", Signature: "i", Evidence: refsAt("k", 1, now.Add(time.Hour))}); obs != Reopened || got.Status != CaseOpen {
		t.Fatalf("back at its old rate %s: %+v", obs, got)
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
	if _, merged, err := st.AddCaseSignature(ctx, a.ID, "root:cache", "agent"); err != nil || merged {
		t.Fatalf("a's root: merged %v, %v", merged, err)
	}
	got, merged, err := st.AddCaseSignature(ctx, b.ID, "root:cache", "agent")
	if err != nil || !merged || got.ID != a.ID {
		t.Fatalf("b's root: %+v merged %v, %v", got, merged, err)
	}
	if got.Evidence != 5 || got.Cost.USD != 1.5 || got.Cost.Runs != 1 || !slices.Equal(got.Signatures, []string{"sym:a", "sym:b", "root:cache"}) {
		t.Fatalf("merged %+v", got)
	}
	gone, err := st.CaseByID(ctx, b.ID)
	if err != nil || gone.Status != CaseClosed || gone.Reason != Duplicate || gone.Into != a.ID || gone.Evidence != 0 {
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

	// a regression the investigation finds: the new case's root is a closed case's
	if _, err := st.CloseCase(ctx, a.ID, Solved, "", "fixed", "agent"); err != nil {
		t.Fatal(err)
	}
	n, _ := observeOK(t, st, Finding{Family: "failures", Signature: "sym:n", Title: "n", Evidence: refsAt("n", 1, now.Add(time.Hour))})
	got, merged, err = st.AddCaseSignature(ctx, n.ID, "root:cache", "agent")
	if err != nil || !merged || got.ID != a.ID || got.Status != CaseOpen {
		t.Fatalf("a regression merged into %+v (merged %v, %v)", got, merged, err)
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
	if _, err := st.ReopenCase(ctx, c.ID, refsAt("a", 1, time.Now()), "again", "agent"); !errors.Is(err, ErrBadCase) {
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

// A case unseen past its evidence's retention closes by itself, as insufficient evidence; a deployed one is its
// measurement's and stays.
func TestTheSweepClosesACaseUnseenPastItsRetention(t *testing.T) {
	st := caseStore(t)
	ctx := context.Background()
	old := time.Now().Add(-40 * 24 * time.Hour)
	stale, _ := observeOK(t, st, Finding{Family: "failures", Signature: "old", Title: "old", Evidence: refsAt("o", 2, old)})
	fresh, _ := observeOK(t, st, Finding{Family: "failures", Signature: "new", Title: "new", Evidence: refsAt("n", 2, time.Now())})
	deployed, _ := observeOK(t, st, Finding{Family: "failures", Signature: "dep", Title: "dep", Evidence: refsAt("d", 2, old)})
	if _, err := st.db.ExecContext(ctx, `UPDATE cases SET status = 'deployed' WHERE id = ?`, deployed.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := st.SweepCases(ctx, time.Now(), CaseRetention)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].ID != stale.ID || closed[0].Reason != Insufficient || closed[0].ClosedEvidence != 2 {
		t.Fatalf("swept %+v", closed)
	}
	for _, id := range []string{fresh.ID, deployed.ID} {
		if c, _ := st.CaseByID(ctx, id); c.Status == CaseClosed {
			t.Errorf("%s swept", c.Title)
		}
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
