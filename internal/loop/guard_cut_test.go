package loop

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A guard's count and its watch weigh the case's evidence in turn, and each piece is weighed by exactly one of the
// two (#110): the count weighs what is dated before it, here a hand landing's recount as of the landing, and the watch
// the rest, from the landing on, through the stretch before the change took effect, and after its close. One incident
// of Bash failures 20 days before a proposal admits a signal naming Bash with Bash as its unit; the change lands by
// hand 20 days after the proposal, more than its 14-day window, so the landing counts the plan again, on a pass a day
// and a half later; the change is first seen in effect two days after the landing. One more failure, of Bash or of
// Edit, is filed as the case's evidence at each position, its event kept or lost from the store:
//
//   - before the landing (before the history, in it before the proposal, between the proposal and the landing), the
//     recount weighs it: Bash kept leaves the guard, which closes as guarded after its watch; Edit, or an event lost,
//     refuses it, and the change is measured by hand;
//   - between the landing and the effect, and after the effect, the watch weighs it: kept, it is a recurrence and the
//     case's rollback is held; lost, the watch passes over it (it cannot tell it from the loop's own), and the guard
//     closes as guarded;
//   - after the close, the watch after the close weighs it: kept, the case comes back; lost, it stays closed.
//
// The piece between the landing and the effect was weighed by neither before (the recount cut at the landing, the
// watch read from the effect).
func TestEachPieceOfACasesEvidenceIsWeighedOnce(t *testing.T) {
	positions := []struct {
		name string
		at   func(proposed, landed, effect time.Time) time.Time
		side string // the count's, the watch's, or the watch's after the close
	}{
		{"before the history", func(p, _, _ time.Time) time.Time { return p.Add(-60 * aDay) }, "count"},
		{"in the history before the proposal", func(p, _, _ time.Time) time.Time { return p.Add(-10 * aDay) }, "count"},
		{"between the proposal and the landing", func(p, _, _ time.Time) time.Time { return p.Add(10 * aDay) }, "count"},
		{"between the landing and the effect", func(_, l, _ time.Time) time.Time { return l.Add(aDay) }, "landing"},
		{"after the effect", func(_, _, e time.Time) time.Time { return e.Add(10 * aDay) }, "watch"},
		{"after the close", func(_, _, e time.Time) time.Time { return e.Add(35 * aDay) }, "closed"},
	}
	for _, pos := range positions {
		for _, tool := range []string{"Bash", "Edit"} {
			for _, lost := range []bool{false, true} {
				var want string
				switch {
				case pos.side == "count" && (tool == "Edit" || lost):
					want = "measured by hand"
				case pos.side == "count", lost:
					want = "guarded"
				case pos.side == "closed":
					want = "back"
				default:
					want = "held"
				}
				t.Run(fmt.Sprintf("%s, %s, lost %t", pos.name, tool, lost), func(t *testing.T) {
					if got := weighedOnce(t, pos.at, pos.side, tool, lost); got != want {
						t.Fatalf("its verdict: %s, want %s", got, want)
					}
				})
			}
		}
	}
}

// weighedOnce runs TestEachPieceOfACasesEvidenceIsWeighedOnce's case with one more failure of tool dated by at, filed
// on the side's pass (before the proposal's count is read again for count and landing), its event lost from the store
// when lost, and says what came of it: "measured by hand" (the recount refused the guard), "held" (a recurrence in the
// watch), "back" (a recurrence after its close) or "guarded" (its close, and nothing after).
func weighedOnce(t *testing.T, at func(proposed, landed, effect time.Time) time.Time, side, tool string, lost bool) string {
	t.Helper()
	ctx := context.Background()
	st, db := testStoreAt(t)
	dir := t.TempDir()
	m := &measured{st: st, db: db, r: &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets,
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	proposed := time.Now().Add(-365 * aDay).Truncate(time.Hour)
	landed, effect := proposed.Add(20*aDay), proposed.Add(22*aDay)
	bash := serverSignal
	bash.Tool = "Bash"
	c := oneIncidentOf(t, st, proposed.Add(-5*aDay), proposed, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" })
	m.c = c
	if err := st.MarkRead(ctx, toolSignal.Source, proposed); err != nil {
		t.Fatal(err)
	}
	plan, err := m.r.Ship.validPlan(ctx, c, bash, 14*aDay, "", proposed)
	if g, _ := guardOf(t, plan); err != nil || g == nil || g.Tool != "Bash" {
		t.Fatalf("the proposal: %+v %v", g, err)
	}
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "before the change")
	write(t, filepath.Join(m.repo, "a.txt"), "b\n")
	run(t, m.repo, "git", "commit", "-q", "-am", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: m.repo, PR: "https://github.com/agent-sh/fake/pull/7", State: Proposed,
		Head: head, Hand: []string{"a.txt"}, Plan: &plan}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	// file is the detector filing the failure as the case's evidence, then, when lost, the store losing its event
	ev := toolResult("cell", at(proposed, landed, effect), "cell", true)
	ev.Tool, ev.Cwd = tool, "/work/cell"
	file := func() {
		t.Helper()
		put(t, st, []store.Event{ev})
		f := store.Finding{Family: "failures", Signature: "failures/v2:zz-one-incident", Title: "the tool fails", Evidence: []store.Evidence{{Ref: "ev:" + ev.ID()}}}
		if _, _, err := st.ObserveCase(ctx, f, "detector"); err != nil {
			t.Fatal(err)
		}
		if !lost {
			return
		}
		conn, err := sql.Open("sqlite", db+"?_pragma=busy_timeout(10000)")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if res, err := conn.Exec(`DELETE FROM events WHERE source = ? AND source_key = 'cell'`, toolSignal.Source); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("deleted %d events", n)
		}
	}
	if side == "count" || side == "landing" {
		file()
	}
	// the landing's pass, a day and a half after it: the plan is counted again as of the landing
	m.clock = landed.Add(36 * time.Hour)
	m.scanned(t)
	if c, err = st.CaseByID(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.r.Ship.landHand(ctx, c, &p, head, landed); err != nil {
		t.Fatal(err)
	}
	if p, _, err = m.r.Ship.Load(c.ID); err != nil {
		t.Fatal(err)
	} else if p.Plan.Guard == nil {
		if p.Plan.Units != "" {
			t.Fatalf("a recount with no guard measures: %+v", p.Plan)
		}
		return "measured by hand"
	}
	// the first pass that sees the change in effect starts the watch
	m.clock = effect
	if got := m.pass(t, Options{}); got.Status != "measuring" {
		return got.Status
	}
	if side == "watch" {
		file()
		m.clock = ev.TS.Add(time.Hour)
		if got := m.pass(t, Options{}); got.Status != "waiting" {
			return got.Status
		}
	}
	m.clock = effect.Add(guardRetention + time.Hour)
	if got := m.pass(t, Options{}); got.Status != "guarded" {
		return got.Status + ": " + got.Why
	}
	if side == "closed" {
		file()
		m.clock = ev.TS.Add(time.Hour)
		ms, err := m.passes(t, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range ms {
			if got.Case == c.ID {
				return got.Status
			}
		}
	}
	if got := m.theCase(t); got.Status != store.CaseClosed || got.Reason != guardedReason {
		return fmt.Sprintf("%s %s", got.Status, got.Reason)
	}
	return "guarded"
}

// A guard's count and its watch meet where its plan was counted, for a change the loop landed too, with no recount at
// its landing: an Edit failure dated a day after the proposal's count, the day before the landing, is the watch's, a
// recurrence (#110). The count never saw it, and a watch from the landing or from the effect would not either.
func TestALoopLandedGuardWeighsEvidenceAfterItsCount(t *testing.T) {
	m := guarded(t, 2*aDay, toolSignal.Source, nil)
	edit := toolResult("edit-after-count", m.planAt.Add(aDay), "edit-after-count", true)
	edit.Tool, edit.Cwd = "Edit", "/work/e"
	fileEvidence(t, m, edit)
	if got := m.pass(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "regressed: its problem came back") {
		t.Fatalf("evidence between the count and the landing: %+v", got)
	}
}
