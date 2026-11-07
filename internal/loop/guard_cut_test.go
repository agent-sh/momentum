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

// One instant, the change's landing, sorts a guard's case's evidence by its own date (#110). Dated before the landing,
// it is history: the count's rule judges what was filed by the time the count runs, once, and history filed after the
// count changes nothing (a known limit, SPEC); it never brings a recurrence or a rollback. Dated from the landing on,
// it is a recurrence, the watch's, during the watch and after its close.
//
// One incident of Bash failures 20 days before a proposal admits a signal naming Bash on its own-tool exemption, the
// signal's source read two days short of the proposal (its boundary). The change lands three days after the proposal
// as the loop lands one, or twenty days after it by hand, which counts the plan again as of the landing on a pass a day
// and a half later; it is first seen in effect two days after the landing. One more failure, of Bash or of Edit, its
// event kept or lost from the store, is dated before the boundary, between it and the proposal, between the proposal
// and the landing, between the landing and the effect, after the effect, or after the close, and filed before the
// count, after it before the watch, during the watch, or after the close, wherever its date allows:
//
//   - dated before the landing and filed before the count: Bash kept leaves the guard, which closes as guarded; Edit,
//     or an event lost, breaks the exemption, so the proposal's count refuses it, or a hand landing's recount does and
//     the change is measured by hand;
//   - dated before the landing and filed after the count: nothing changes, the guard closes as guarded, and nothing
//     rolls back;
//   - dated from the landing on: kept, it is a recurrence, its rollback held, or after the close the case comes back;
//     lost, the watch passes over it (it cannot tell it from the loop's own) and the guard closes as guarded.
func TestEachPieceOfACasesEvidenceIsJudgedByOneRule(t *testing.T) {
	positions := []struct {
		name    string
		at      func(c cutTimes) time.Time
		history bool
	}{
		{"before the boundary", func(c cutTimes) time.Time { return c.proposed.Add(-10 * aDay) }, true},
		{"between the boundary and the proposal", func(c cutTimes) time.Time { return c.proposed.Add(-aDay) }, true},
		{"between the proposal and the landing", func(c cutTimes) time.Time { return c.proposed.Add(c.landed.Sub(c.proposed) / 3) }, true},
		{"between the landing and the effect", func(c cutTimes) time.Time { return c.landed.Add(aDay) }, false},
		{"after the effect", func(c cutTimes) time.Time { return c.effect.Add(10 * aDay) }, false},
		{"after the close", func(c cutTimes) time.Time { return c.effect.Add(35 * aDay) }, false},
	}
	for _, hand := range []bool{false, true} {
		for _, pos := range positions {
			for _, filing := range []string{cutBeforeCount, cutBeforeWatch, cutDuringWatch, cutAfterClose} {
				times := cutTimesOf(hand)
				dated := pos.at(times)
				if !times.files(filing, dated) {
					continue
				}
				for _, tool := range []string{"Bash", "Edit"} {
					for _, lost := range []bool{false, true} {
						var want string
						switch {
						case pos.history && tool == "Bash" && !lost, pos.history && filing != cutBeforeCount, !pos.history && lost:
							want = "guarded"
						case pos.history && !hand:
							want = "refused"
						case pos.history:
							want = "measured by hand"
						case filing == cutAfterClose:
							want = "back"
						default:
							want = "held"
						}
						landing := map[bool]string{false: "landed by the loop", true: "landed by hand"}[hand]
						t.Run(fmt.Sprintf("%s, dated %s, filed %s, %s, lost %t", landing, pos.name, filing, tool, lost), func(t *testing.T) {
							if got := judgedOnce(t, times, dated, filing, tool, lost); got != want {
								t.Fatalf("its verdict: %s, want %s", got, want)
							}
						})
					}
				}
			}
		}
	}
}

// The filings of TestEachPieceOfACasesEvidenceIsJudgedByOneRule.
const (
	cutBeforeCount = "before the count"
	cutBeforeWatch = "after the count, before the watch"
	cutDuringWatch = "during the watch"
	cutAfterClose  = "after the close"
)

// cutTimes are the instants of TestEachPieceOfACasesEvidenceIsJudgedByOneRule's change: its proposal, its landing, the
// pass that counts a hand landing's plan again (count), and its first pass in effect, which starts its watch.
type cutTimes struct {
	hand                                 bool
	proposed, landed, count, effect, end time.Time
}

func cutTimesOf(hand bool) cutTimes {
	c := cutTimes{hand: hand, proposed: time.Now().Add(-365 * aDay).Truncate(time.Hour)}
	c.landed, c.count = c.proposed.Add(3*aDay), c.proposed
	if hand {
		c.landed = c.proposed.Add(20 * aDay)
		c.count = c.landed.Add(36 * time.Hour)
	}
	c.effect = c.landed.Add(2 * aDay)
	c.end = c.effect.Add(guardRetention + time.Hour)
	return c
}

// files says whether a piece of evidence so dated can be filed so: after its date, in that stretch.
func (c cutTimes) files(filing string, dated time.Time) bool {
	switch filing {
	case cutBeforeCount:
		return dated.Before(c.count)
	case cutBeforeWatch:
		return dated.Before(c.effect)
	case cutDuringWatch:
		return dated.Before(c.end.Add(-time.Hour))
	}
	return true
}

// judgedOnce runs a case of TestEachPieceOfACasesEvidenceIsJudgedByOneRule and says what came of it: "refused" (the
// proposal's count refused the guard), "measured by hand" (a hand landing's recount refused it), "held" (a recurrence
// in the watch, its rollback held), "back" (a recurrence after the close) or "guarded" (its close, and nothing after).
func judgedOnce(t *testing.T, at cutTimes, dated time.Time, filing, tool string, lost bool) string {
	t.Helper()
	ctx := context.Background()
	st, db := testStoreAt(t)
	dir := t.TempDir()
	m := &measured{st: st, db: db, r: &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets,
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.planAt, m.deployed = at.proposed, at.landed
	bash := serverSignal
	bash.Tool = "Bash"
	c := oneIncidentOf(t, st, at.proposed.Add(-5*aDay), at.proposed, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" })
	m.c = c
	// file is the detector filing the failure as the case's evidence, then, when lost, the store losing its event
	ev := toolResult("cell", dated, "cell", true)
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
	// the proposal, its signal's source read through two days before it (its boundary)
	if err := st.MarkRead(ctx, toolSignal.Source, at.proposed.Add(-2*aDay)); err != nil {
		t.Fatal(err)
	}
	if !at.hand && filing == cutBeforeCount {
		file()
	}
	plan, err := m.r.Ship.validPlan(ctx, c, bash, 14*aDay, "", at.proposed)
	if g, _ := guardOf(t, plan); err != nil || g == nil {
		if !at.hand && filing == cutBeforeCount {
			return "refused"
		}
		t.Fatalf("the proposal: %+v %v", g, err)
	} else if g.Tool != "Bash" {
		t.Fatalf("the proposal's guard: %+v", g)
	}
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "before the change")
	write(t, filepath.Join(m.repo, "a.txt"), "b\n")
	run(t, m.repo, "git", "commit", "-q", "-am", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: m.repo, PR: "https://github.com/agent-sh/fake/pull/7", Plan: &plan}
	byHand := func() bool {
		t.Helper()
		got, _, err := m.r.Ship.Load(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Plan.Guard == nil && got.Plan.Units != "" {
			t.Fatalf("a plan with no guard measures: %+v", got.Plan)
		}
		return got.Plan.Guard == nil
	}
	if at.hand {
		// landed by hand: the landing's pass counts the plan again as of the landing
		p.State, p.Head, p.Hand = Proposed, head, []string{"a.txt"}
		if err := m.r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		if filing == cutBeforeCount {
			file()
		}
		m.clock = at.count
		m.scanned(t)
		if c, err = st.CaseByID(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := m.r.Ship.landHand(ctx, c, &p, head, at.landed); err != nil {
			t.Fatal(err)
		}
		if byHand() {
			return "measured by hand"
		}
	} else {
		// landed by the loop, with the plan its proposal counted
		p.State, p.Landed, p.LandedAt = Landed, []string{head}, at.landed
		if err := m.r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		if m.c, err = st.DeployCase(ctx, c.ID, m.r.Ship.pkgDir(c.ID), at.landed, "landed", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if filing == cutBeforeWatch {
		file()
	}
	// pass is a pass of the measure step at a time, and what came of it when it decided something
	pass := func(clock time.Time, quiet string) string {
		t.Helper()
		m.clock = clock
		got := m.pass(t, Options{})
		switch {
		case byHand():
			return "measured by hand"
		case got.Status != quiet:
			return got.Status
		}
		return ""
	}
	// the first pass that sees the change in effect starts the watch
	if got := pass(at.effect, "measuring"); got != "" {
		return got
	}
	if filing == cutDuringWatch {
		file()
		if got := pass(latest(dated, at.effect).Add(time.Hour), "waiting"); got != "" {
			return got
		}
	}
	if got := pass(at.end, "guarded"); got != "" {
		return got
	}
	if filing == cutAfterClose {
		file()
		m.clock = latest(dated, at.end).Add(time.Hour)
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

// landedGuard is a change the loop landed with a guard on the own-tool exemption (one incident of Bash failures 20 days
// before its proposal, the signal's source read through two days before it), on cutTimesOf's clock. Its first pass
// in effect has run, unless beforeFirst is given: then it runs beforeFirst instead, with the clock at that pass.
func landedGuard(t *testing.T, beforeFirst func(m *measured, at cutTimes)) (*measured, cutTimes) {
	t.Helper()
	ctx := context.Background()
	at := cutTimesOf(false)
	st, db := testStoreAt(t)
	dir := t.TempDir()
	m := &measured{st: st, db: db, r: &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets,
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.planAt, m.deployed = at.proposed, at.landed
	bash := serverSignal
	bash.Tool = "Bash"
	c := oneIncidentOf(t, st, at.proposed.Add(-5*aDay), at.proposed, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" })
	m.c = c
	if err := st.MarkRead(ctx, toolSignal.Source, at.proposed.Add(-2*aDay)); err != nil {
		t.Fatal(err)
	}
	plan, err := m.r.Ship.validPlan(ctx, c, bash, 14*aDay, "", at.proposed)
	if g, _ := guardOf(t, plan); err != nil || g == nil || g.Tool != "Bash" || g.Incidents != 1 {
		t.Fatalf("the proposal: %+v %v", g, err)
	}
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "init", "-q", "-b", "main", m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: m.repo, PR: "https://github.com/agent-sh/fake/pull/7", State: Landed,
		Landed: []string{head}, LandedAt: at.landed, Plan: &plan}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if m.c, err = st.DeployCase(ctx, c.ID, m.r.Ship.pkgDir(c.ID), at.landed, "landed", "test"); err != nil {
		t.Fatal(err)
	}
	m.clock = at.effect
	if beforeFirst != nil {
		beforeFirst(m, at)
		return m, at
	}
	if got := m.pass(t, Options{}); got.Status != "measuring" {
		t.Fatalf("the first pass: %+v", got)
	}
	return m, at
}

// failureOf is a failure of a tool in a session and a directory of its own.
func failureOf(key, tool string, at time.Time) store.Event {
	e := toolResult(key, at, key, true)
	e.Tool, e.Cwd = tool, "/work/"+key
	return e
}

// History never stands in a recurrence's way: with no judgment of history filed after the count, a recurrence always
// acts (#110). A recurrence and an Edit failure dated between the proposal and the landing filed before the same pass:
// the recurrence brings the case back, its rollback held. A held recurrence stays held when such history is filed a
// pass later. A problem that went on through the landing, an Edit failure before it and a Bash failure after it, both
// filed before the first pass in effect, is a recurrence on that pass.
func TestHistoryFiledLateNeverHidesARecurrence(t *testing.T) {
	regressed := func(t *testing.T, m *measured, got Measured) {
		t.Helper()
		if c := m.theCase(t); got.Status != "held" || !strings.Contains(got.Why, "regressed") || c.Status != store.CaseMeasuring || c.Measurement.After == 0 {
			t.Fatalf("a recurrence not acted on: %+v, case %s %+v", got, c.Status, c.Measurement)
		}
	}
	t.Run("filed in one pass", func(t *testing.T) {
		m, at := landedGuard(t, nil)
		fileEvidence(t, m, failureOf("recur", "Bash", at.effect.Add(aDay)), failureOf("late-edit", "Edit", at.proposed.Add(aDay)))
		m.clock = at.effect.Add(2 * aDay)
		regressed(t, m, m.pass(t, Options{}))
	})
	t.Run("filed a pass after the recurrence", func(t *testing.T) {
		m, at := landedGuard(t, nil)
		fileEvidence(t, m, failureOf("recur", "Bash", at.effect.Add(aDay)))
		m.clock = at.effect.Add(2 * aDay)
		regressed(t, m, m.pass(t, Options{}))
		fileEvidence(t, m, failureOf("late-edit", "Edit", at.proposed.Add(aDay)))
		m.clock = at.effect.Add(3 * aDay)
		got := m.pass(t, Options{})
		if c := m.theCase(t); c.Status != store.CaseMeasuring || c.Measurement.After == 0 {
			t.Fatalf("the held recurrence is gone: %+v, case %s %+v", got, c.Status, c.Measurement)
		}
	})
	t.Run("a problem that went on through the landing", func(t *testing.T) {
		m, _ := landedGuard(t, func(m *measured, at cutTimes) {
			fileEvidence(t, m, failureOf("recur-out", "Bash", at.landed.Add(aDay)), failureOf("edit-between", "Edit", at.proposed.Add(aDay)))
		})
		regressed(t, m, m.pass(t, Options{}))
	})
}

// The evidence's own date sorts it, never its event's time now (#110): a failure filed as the case's evidence dated
// after the landing, whose event a later read rewrites to a time before it, is still a recurrence.
func TestARewrittenEventCannotCrossTheLanding(t *testing.T) {
	m, at := landedGuard(t, nil)
	e := failureOf("rewritten", "Bash", at.effect.Add(aDay))
	fileEvidence(t, m, e)
	conn, err := sql.Open("sqlite", m.db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if res, err := conn.Exec(`UPDATE events SET ts = ? WHERE source = ? AND source_key = 'rewritten'`, at.landed.Add(-aDay).UnixMilli(), toolSignal.Source); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("rewrote %d events", n)
	}
	m.clock = at.effect.Add(2 * aDay)
	if got := m.pass(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "regressed") {
		t.Fatalf("a recurrence whose event was rewritten to before the landing: %+v", got)
	}
}

// A guarded close names the case's evidence dated from the landing whose event the store no longer holds, which the
// watch passed over (#110): its own log line is the one record of it.
func TestAGuardedCloseNamesTheEvidenceItCouldNotRead(t *testing.T) {
	ctx := context.Background()
	m, at := landedGuard(t, nil)
	e := failureOf("gone", "Bash", at.effect.Add(aDay))
	fileEvidence(t, m, e)
	conn, err := sql.Open("sqlite", m.db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if res, err := conn.Exec(`DELETE FROM events WHERE source = ? AND source_key = 'gone'`, toolSignal.Source); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("deleted %d events", n)
	}
	m.clock = at.end
	if got := m.pass(t, Options{}); got.Status != "guarded" {
		t.Fatalf("the close: %+v", got)
	}
	log, err := m.st.CaseLog(ctx, m.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range log {
		if strings.HasPrefix(l.Detail, "guarded") || l.Action == "closed" {
			if strings.Contains(l.Detail, "ev:"+e.ID()) && strings.Contains(l.Detail, "whose events the store no longer holds") {
				return
			}
		}
	}
	t.Fatalf("the close's log line does not name the evidence it could not read: %+v", log)
}

// The exposure watched of a guard whose case's evidence dated between its landing and its watch's start brings it back
// leaves that recurrence's hour out: the change was not in effect then (guardWatched). Its use in the watch, Bash an
// hour for a day, is the exposure watched, and nothing else.
func TestAWatchLeavesOutTheHourOfARecurrenceBeforeItsStart(t *testing.T) {
	ctx := context.Background()
	m, at := landedGuard(t, nil)
	var uses []store.Event
	for i := 0; i < 24; i++ {
		uses = append(uses, serverUse(fmt.Sprint("w-", i), at.effect.Add(aDay+time.Duration(i)*time.Hour), "Bash"))
	}
	put(t, m.st, uses)
	fileEvidence(t, m, failureOf("before-start", "Bash", at.landed.Add(aDay)))
	m.clock = at.effect.Add(5 * aDay)
	p, _, err := m.r.Ship.Load(m.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := guardExposure(ctx, m.st, *p.Plan, nil, p.GuardFrom, m.clock, m.r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	got := m.pass(t, Options{})
	if got.Status != "held" || got.Result == nil || got.Result.After.Base != len(want) || len(want) != 24 {
		t.Fatalf("a recurrence before the watch's start: %+v, watched %+v, want %d", got, got.Result, len(want))
	}
}
