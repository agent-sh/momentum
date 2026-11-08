package loop

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// guardPath is how a guard's change lands in these tests.
type guardPath string

const (
	byLoop    guardPath = "landed by the loop"
	byHand    guardPath = "landed by hand"                     // into its base, more than a window after its plan: counted again
	intoOther guardPath = "landed by hand into another branch" // merged elsewhere, it reaches its base a week later
	byContent guardPath = "landed by hand by content"          // a commit dated a week before the pass that sees it on its base
)

// cutTimes are the instants of a guard's change in these tests: its proposal, its landing (Package.LandedAt), the
// instant it reached its base, the pass that lands it, its first pass in effect (the watch's start), and the pass after
// the watch's end.
type cutTimes struct {
	path                                                    guardPath
	proposed, landed, reached, pass, effect, mid, end, back time.Time
}

func cutTimesOf(path guardPath) cutTimes {
	c := cutTimes{path: path, proposed: time.Now().Add(-365 * aDay).Truncate(time.Hour)}
	switch path {
	case byLoop:
		c.landed = c.proposed.Add(3 * aDay)
		c.reached, c.pass = c.landed, c.landed
	case byHand:
		c.landed = c.proposed.Add(20 * aDay)
		c.reached, c.pass = c.landed, c.landed.Add(36*time.Hour)
	default:
		c.landed = c.proposed.Add(3 * aDay)
		c.reached = c.proposed.Add(10 * aDay)
		c.pass = c.reached
	}
	c.effect = c.reached.Add(2 * aDay)
	c.mid = c.effect.Add(15 * aDay)
	c.end = c.effect.Add(guardRetention + time.Hour)
	c.back = c.effect.Add(36 * aDay)
	return c
}

// reachedAt is the same change with its instant, and so its landing's pass and its watch, moved to r.
func (c cutTimes) reachedAt(r time.Time) cutTimes {
	c.reached, c.pass = r, r
	c.effect = c.reached.Add(2 * aDay)
	c.mid = c.effect.Add(15 * aDay)
	c.end = c.effect.Add(guardRetention + time.Hour)
	c.back = c.effect.Add(36 * aDay)
	return c
}

// The phases of a guard's run in which evidence is filed.
const (
	cutBeforeCount = "before the count"
	cutBeforePass  = "after the count, before the landing's pass"
	cutBeforeWatch = "after the landing's pass, before the watch"
	cutDuringWatch = "during the watch"
	cutAfterClose  = "after the close"
)

// before is when a phase's filing happens at the latest, for a piece so dated to be filed then.
func (c cutTimes) before(phase string) time.Time {
	switch phase {
	case cutBeforeCount:
		return c.proposed
	case cutBeforePass:
		return c.pass
	case cutBeforeWatch:
		return c.effect
	case cutDuringWatch:
		return c.mid
	}
	return c.back
}

// guardRun is a run of a guard on the own-tool exemption, one incident of Bash failures 20 days before its proposal
// (the signal's source read two days short of it, its boundary), landed by its path and watched to its close, on a
// clock of the test's own. hooks run in their phases. It says what came of it: "refused" (the proposal's count refused
// the guard), "measured by hand" (the recount or the judgment as the change landed refused it), "held" (a recurrence
// in the watch, its rollback held), "back" (a recurrence after the close) or "guarded" (its close, and nothing after);
// res is the deciding pass's.
func guardRun(t *testing.T, at cutTimes, hooks map[string]func(m *measured)) (verdict string, res Measured, m *measured) {
	t.Helper()
	ctx := context.Background()
	st, db := testStoreAt(t)
	dir := t.TempDir()
	m = &measured{st: st, db: db, r: &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets,
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}}
	m.r.Now = func() time.Time { return m.clock }
	m.r.Ship.Now = m.r.Now
	m.planAt, m.deployed = at.proposed, at.landed
	hook := func(phase string) {
		if h := hooks[phase]; h != nil {
			h(m)
		}
	}
	bash := serverSignal
	bash.Tool = "Bash"
	c := oneIncidentOf(t, st, at.proposed.Add(-5*aDay), at.proposed, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" })
	m.c = c
	if err := st.MarkRead(ctx, toolSignal.Source, at.proposed.Add(-2*aDay)); err != nil {
		t.Fatal(err)
	}
	hook(cutBeforeCount)
	m.clock = at.proposed
	plan, err := m.r.Ship.validPlan(ctx, c, bash, 14*aDay, "", at.proposed)
	if g, _ := guardOf(t, plan); err != nil || g == nil {
		return "refused", Measured{Why: fmt.Sprint(err)}, m
	} else if g.Tool != "Bash" {
		t.Fatalf("the proposal's guard: %+v", g)
	}
	// the change's checkout, with an origin for the loop's own landing to clean its branch up in
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, t.TempDir(), "git", "init", "-q", "--bare", "-b", "main", origin)
	m.repo = filepath.Join(t.TempDir(), "repo")
	run(t, t.TempDir(), "git", "clone", "-q", origin, m.repo)
	write(t, filepath.Join(m.repo, "a.txt"), "a\n")
	run(t, m.repo, "git", "add", "-A")
	run(t, m.repo, "git", "commit", "-q", "-m", "before the change")
	write(t, filepath.Join(m.repo, "a.txt"), "b\n")
	run(t, m.repo, "git", "commit", "-q", "-am", "the change")
	head := strings.TrimSpace(run(t, m.repo, "git", "rev-parse", "HEAD"))
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Dir: m.repo, Host: m.repo, PR: "https://github.com/agent-sh/fake/pull/7", Plan: &plan}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	hook(cutBeforePass)
	m.clock = at.pass
	m.scanned(t)
	if c, err = st.CaseByID(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if at.path == byLoop {
		p.LandingHead, p.Landed, p.LandedAt = head, []string{head}, at.landed
		if _, err := m.r.Ship.finishLanding(ctx, p); err != nil {
			t.Fatal(err)
		}
	} else {
		p.State, p.Head, p.Hand = Proposed, head, []string{"a.txt"}
		if err := landByHand(ctx, m.r.Ship, c, &p, head, at.landed, at.reached); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, err := m.r.Ship.Load(c.ID); err != nil {
		t.Fatal(err)
	} else if got.Plan.Guard == nil {
		if got.Plan.Units != "" {
			t.Fatalf("a plan with no guard measures: %+v", got.Plan)
		}
		return "measured by hand", Measured{}, m
	}
	hook(cutBeforeWatch)
	pass := func(clock time.Time, quiet string) Measured {
		t.Helper()
		m.clock = clock
		return m.pass(t, Options{})
	}
	if got := pass(at.effect, "measuring"); got.Status != "measuring" {
		return got.Status, got, m
	}
	hook(cutDuringWatch)
	if got := pass(at.mid, "waiting"); got.Status != "waiting" {
		return got.Status, got, m
	}
	if got := pass(at.end, "guarded"); got.Status != "guarded" {
		return got.Status, got, m
	}
	hook(cutAfterClose)
	m.clock = at.back
	ms, err := m.passes(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range ms {
		if got.Case == c.ID {
			return got.Status, got, m
		}
	}
	if got := m.theCase(t); got.Status != store.CaseClosed || got.Reason != guardedReason {
		return fmt.Sprintf("%s %s", got.Status, got.Reason), Measured{}, m
	}
	return "guarded", Measured{}, m
}

// failureOf is a failure of a tool in a session and a directory of its own.
func failureOf(key, tool string, at time.Time) store.Event {
	e := toolResult(key, at, key, true)
	e.Tool, e.Cwd = tool, "/work/"+key
	return e
}

// lose drops an event from the store, as a rebuild whose source no longer holds it does.
func lose(t *testing.T, m *measured, e store.Event) {
	t.Helper()
	conn, err := sql.Open("sqlite", m.db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if res, err := conn.Exec(`DELETE FROM events WHERE source = ? AND source_key = ?`, e.Source, e.SourceKey); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("deleted %d events", n)
	}
}

// mergeIn files an event as the evidence of a duplicate case and closes that case into the guarded one, as a person or
// a build's root signature does: the event comes in as the guarded case's evidence.
func mergeIn(t *testing.T, m *measured, e store.Event) {
	t.Helper()
	ctx := context.Background()
	put(t, m.st, []store.Event{e})
	dup, _, err := m.st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v2:zz-dup-" + e.SourceKey, Title: "the tool fails again",
		Evidence: []store.Evidence{{Ref: "ev:" + e.ID()}}}, "detector")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.CloseCase(ctx, dup.ID, store.Duplicate, m.c.ID, "the same failure", "owner"); err != nil {
		t.Fatal(err)
	}
}

// One instant, when the change reached its base, sorts a guard's case's evidence by its own date (#110), for every way
// the change lands: by the loop, by hand into its base (counted again more than a window after its plan), by hand into
// another branch that reaches the base a week later, and by content, a commit dated a week before the pass that sees
// it. One more failure, of Bash or of Edit, its event kept or lost from the store, is dated before the boundary,
// between it and the proposal, between the proposal and the instant, between the instant and the effect, after the
// effect, or after the close; it is filed before the count, after the count before the landing's pass, after that pass
// before the watch, during the watch, or after the close, or merged in from a duplicate case before the landing's pass
// or during the watch, wherever its date allows:
//
//   - dated before the instant: Bash kept closes as guarded; Edit, or an event lost, filed before the count is
//     refused, and filed or merged in before the landing's pass is measured by hand (the recount, or the judgment as
//     the change lands); filed or merged in after that pass, it changes nothing and never rolls back (the known limit);
//   - dated from the instant on: kept, it is a recurrence, its rollback held, or after the close the case comes back;
//     lost, the watch passes over it (it cannot tell it from the loop's own) and the guard closes as guarded.
func TestEachPieceOfACasesEvidenceIsJudgedByOneRule(t *testing.T) {
	positions := []struct {
		name string
		at   func(c cutTimes) time.Time
	}{
		{"before the boundary", func(c cutTimes) time.Time { return c.proposed.Add(-10 * aDay) }},
		{"between the boundary and the proposal", func(c cutTimes) time.Time { return c.proposed.Add(-aDay) }},
		{"between the proposal and the instant", func(c cutTimes) time.Time { return c.proposed.Add(c.reached.Sub(c.proposed) / 2) }},
		{"between the instant and the effect", func(c cutTimes) time.Time { return c.reached.Add(aDay) }},
		{"after the effect", func(c cutTimes) time.Time { return c.effect.Add(10 * aDay) }},
		{"after the close", func(c cutTimes) time.Time { return c.effect.Add(35 * aDay) }},
	}
	filings := []struct {
		name, phase string
		merged      bool
	}{
		{cutBeforeCount, cutBeforeCount, false},
		{cutBeforePass, cutBeforePass, false},
		{"merged in " + cutBeforePass, cutBeforePass, true},
		{cutBeforeWatch, cutBeforeWatch, false},
		{cutDuringWatch, cutDuringWatch, false},
		{"merged in " + cutDuringWatch, cutDuringWatch, true},
		{cutAfterClose, cutAfterClose, false},
	}
	for _, path := range []guardPath{byLoop, byHand, intoOther, byContent} {
		times := cutTimesOf(path)
		for _, pos := range positions {
			dated := pos.at(times)
			history := dated.Before(times.reached)
			for _, f := range filings {
				if !dated.Before(times.before(f.phase)) {
					continue
				}
				for _, tool := range []string{"Bash", "Edit"} {
					for _, lost := range []bool{false, true} {
						var want string
						switch {
						case history && tool == "Bash" && !lost, history && f.phase != cutBeforeCount && f.phase != cutBeforePass, !history && lost:
							want = "guarded"
						case history && f.phase == cutBeforeCount:
							want = "refused"
						case history:
							want = "measured by hand"
						case f.phase == cutAfterClose:
							want = "back"
						default:
							want = "held"
						}
						f := f
						t.Run(fmt.Sprintf("%s, dated %s, filed %s, %s, lost %t", path, pos.name, f.name, tool, lost), func(t *testing.T) {
							e := failureOf("cell", tool, dated)
							got, res, _ := guardRun(t, times, map[string]func(m *measured){f.phase: func(m *measured) {
								if f.merged {
									mergeIn(t, m, e)
								} else {
									fileEvidence(t, m, e)
								}
								if lost {
									lose(t, m, e)
								}
							}})
							if got != want {
								t.Fatalf("its verdict: %s (%s), want %s", got, res.Why, want)
							}
						})
					}
				}
			}
		}
	}
}

// guardReader is a row of SPEC's time model: a reader of a guard's case, the instant it cuts at and what it reads.
type guardReader struct{ reader, cut, reads string }

// guardTimeModel is SPEC's time model (Measurement, Guards, The time model), row by row.
var guardTimeModel = []guardReader{
	{"The proposal's count", "its own time", "the case's evidence dated before it"},
	{"A hand landing's recount", "the instant the change reached its base", "the history before it"},
	{"The judgment as the change lands", "the instant the change reached its base", "the case's evidence dated before it and filed by then"},
	{"The watch, the case's evidence", "the instant the change reached its base", "evidence dated from it on"},
	{"The watch, the signal's events", "the instant the change reached its base", "events from it on"},
	{"Out of effect", "the instant the change reached its base, to the watch's start", "recurrences, which count, in hours no exposure watched holds"},
	{"The exposure watched", "the watch's start", "hours from it on, stretches out of effect left out"},
	{"The close's recount", "the instant the change reached its base", "the history before it"},
	{"The watch after the close", "the instant the change reached its base", "the signal's events and the case's evidence from it on"},
	{"A closed guard whose package is gone", "the case's in-effect time", "the case's evidence from it on"},
}

// Every reader of a guard's case cuts at the instant SPEC's time model gives it, and reads the side it gives (#110):
// SPEC's table is guardTimeModel, row for row, and each row has a check that puts a piece just before its cut and one
// at it, on a change merged into another branch (its landing a week before it reached its base, its watch two days
// after), and sees each read on its side. A closed guard whose package holds no plan reads from the instant when its
// package keeps it, and from its watch's start when not.
func TestEachGuardReaderCutsAtTheTimeModelsInstant(t *testing.T) {
	spec, err := os.ReadFile(filepath.Join("..", "..", "SPEC.md"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	in := false
	for _, line := range strings.Split(string(spec), "\n") {
		switch {
		case strings.HasPrefix(line, "  - **The time model.**"):
			in = true
		case in && strings.HasPrefix(line, "    | ") && !strings.HasPrefix(line, "    | Reader") && !strings.HasPrefix(line, "    |---"):
			rows = append(rows, line)
		case in && strings.HasPrefix(line, "  - **"):
			in = false
		}
	}
	var want []string
	for _, r := range guardTimeModel {
		want = append(want, fmt.Sprintf("    | %s | %s | %s |", r.reader, r.cut, r.reads))
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("SPEC's time model:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	at := cutTimesOf(intoOther)
	edit := func(when time.Time) store.Event { return failureOf("cut", "Edit", when) }
	bash := func(when time.Time) store.Event { return failureOf("cut", "Bash", when) }
	file := func(e store.Event) func(m *measured) { return func(m *measured) { fileEvidence(t, m, e) } }
	stored := func(e store.Event) func(m *measured) { return func(m *measured) { put(t, m.st, []store.Event{e}) } }
	verdict := func(t *testing.T, at cutTimes, hooks map[string]func(m *measured), want string) {
		t.Helper()
		if got, res, _ := guardRun(t, at, hooks); got != want {
			t.Fatalf("%s (%s), want %s", got, res.Why, want)
		}
	}
	checks := map[string]func(t *testing.T){
		"The proposal's count": func(t *testing.T) {
			verdict(t, at, map[string]func(*measured){cutBeforeCount: file(edit(at.proposed.Add(-time.Millisecond)))}, "refused")
			// at its own time it is not the count's: the judgment as the change lands takes it
			verdict(t, at, map[string]func(*measured){cutBeforeCount: file(edit(at.proposed))}, "measured by hand")
		},
		"A hand landing's recount": func(t *testing.T) {
			at := cutTimesOf(byHand)
			verdict(t, at, map[string]func(*measured){cutBeforePass: file(edit(at.reached.Add(-time.Millisecond)))}, "measured by hand")
			verdict(t, at, map[string]func(*measured){cutBeforePass: file(edit(at.reached))}, "held")
			// merged elsewhere and on its base 20 days after the proposal, it is counted again as of that instant, not
			// as of its merge
			far := cutTimesOf(intoOther).reachedAt(cutTimesOf(intoOther).proposed.Add(20 * aDay))
			var counted time.Time
			if got, res, _ := guardRun(t, far, map[string]func(*measured){cutBeforeWatch: func(m *measured) {
				p, _, err := m.r.Ship.Load(m.c.ID)
				if err != nil {
					t.Fatal(err)
				}
				counted = p.Plan.At
			}}); got != "guarded" || counted.Before(far.reached.Add(-time.Hour)) {
				t.Fatalf("a recount for a change that reached its base %s, merged %s: counted at %s, %s (%s)", far.reached, far.landed, counted, got, res.Why)
			}
		},
		"The judgment as the change lands": func(t *testing.T) {
			verdict(t, at, map[string]func(*measured){cutBeforePass: file(edit(at.reached.Add(-time.Millisecond)))}, "measured by hand")
			verdict(t, at, map[string]func(*measured){cutBeforeWatch: file(edit(at.reached.Add(-time.Millisecond)))}, "guarded") // filed after it
			verdict(t, at, map[string]func(*measured){cutBeforeWatch: file(edit(at.reached))}, "held")
		},
		"The watch, the case's evidence": func(t *testing.T) {
			verdict(t, at, map[string]func(*measured){cutDuringWatch: file(edit(at.reached.Add(-time.Millisecond)))}, "guarded")
			verdict(t, at, map[string]func(*measured){cutDuringWatch: file(edit(at.reached))}, "held")
		},
		"The watch, the signal's events": func(t *testing.T) {
			verdict(t, at, map[string]func(*measured){cutDuringWatch: stored(bash(at.reached.Add(-time.Millisecond)))}, "guarded")
			verdict(t, at, map[string]func(*measured){cutDuringWatch: stored(bash(at.reached))}, "held")
		},
		"Out of effect": func(t *testing.T) {
			for _, when := range []time.Time{at.reached, at.effect.Add(time.Hour)} {
				got, res, _ := guardRun(t, at, map[string]func(*measured){cutDuringWatch: file(edit(when))})
				if early := strings.Contains(res.Why, "before it took effect"); got != "held" || early != when.Before(at.effect) || strings.Contains(res.Why, "1 of 0") {
					t.Fatalf("a recurrence at %s: %s (%s)", when, got, res.Why)
				}
			}
		},
		"The exposure watched": func(t *testing.T) {
			got, res, m := guardRun(t, at, map[string]func(*measured){cutBeforeWatch: func(m *measured) {
				put(t, m.st, []store.Event{serverUse("use-before", at.effect.Add(-30*time.Minute), "Bash"), serverUse("use-in", at.effect.Add(time.Hour), "Bash")})
			}})
			if c := m.theCase(t); got != "guarded" || c.Measurement.Watched != 1 {
				t.Fatalf("the exposure watched: %s (%s), %+v", got, res.Why, c.Measurement)
			}
			// a watch starts no sooner than its change reached its base: a checkout whose change's file is dated after
			// the merge and before that instant starts the watch at the instant
			got, res, m = guardRun(t, at, map[string]func(*measured){cutBeforeWatch: func(m *measured) {
				p, _, err := m.r.Ship.Load(m.c.ID)
				if err != nil {
					t.Fatal(err)
				}
				p.Changed = []string{"a.txt"}
				if err := m.r.Ship.save(&p); err != nil {
					t.Fatal(err)
				}
				took := at.landed.Add(aDay)
				if err := os.Chtimes(filepath.Join(m.repo, "a.txt"), took, took); err != nil {
					t.Fatal(err)
				}
			}})
			if p, _, err := m.r.Ship.Load(m.c.ID); err != nil || got != "guarded" || !p.GuardFrom.Equal(at.reached) {
				t.Fatalf("a watch whose change's file is dated before its instant: from %s, want %s; %s (%s) %v", p.GuardFrom, at.reached, got, res.Why, err)
			}
		},
		"The close's recount": func(t *testing.T) {
			got, res, m := guardRun(t, at, map[string]func(*measured){cutDuringWatch: stored(bash(at.reached.Add(-time.Millisecond)))})
			if c := m.theCase(t); got != "guarded" || c.Measurement.Before != 2 {
				t.Fatalf("the close's recount of a second incident before the instant: %s (%s), %+v", got, res.Why, c.Measurement)
			}
			verdict(t, at, map[string]func(*measured){cutDuringWatch: stored(bash(at.reached))}, "held")
			// its exposure: an hour of the unit's use before the instant is history's; one after it, before the watch's
			// start, is no one's
			exposure := func(use func(m *measured)) int {
				t.Helper()
				got, res, m := guardRun(t, at, map[string]func(*measured){cutBeforeWatch: use})
				if got != "guarded" {
					t.Fatalf("the close: %s (%s)", got, res.Why)
				}
				return m.theCase(t).Measurement.Exposure
			}
			none := exposure(func(*measured) {})
			before := exposure(stored(serverUse("use-before", at.reached.Add(-time.Hour), "Bash")))
			after := exposure(stored(serverUse("use-after", at.reached.Add(time.Hour), "Bash")))
			if before != none+1 || after != none {
				t.Fatalf("the close's history exposure: %d with no use, %d with one before the instant, %d with one after it", none, before, after)
			}
		},
		"The watch after the close": func(t *testing.T) {
			verdict(t, at, map[string]func(*measured){cutAfterClose: file(edit(at.reached.Add(-time.Millisecond)))}, "guarded")
			verdict(t, at, map[string]func(*measured){cutAfterClose: file(edit(at.reached))}, "back")
			// the signal's events too, stored after the close
			verdict(t, at, map[string]func(*measured){cutAfterClose: stored(bash(at.reached.Add(-time.Millisecond)))}, "guarded")
			verdict(t, at, map[string]func(*measured){cutAfterClose: stored(bash(at.reached.Add(time.Hour)))}, "back")
		},
		"A closed guard whose package is gone": func(t *testing.T) {
			gone := func(m *measured) {
				if err := os.RemoveAll(m.r.Ship.pkgDir(m.c.ID)); err != nil {
					t.Fatal(err)
				}
			}
			verdict(t, at, map[string]func(*measured){cutAfterClose: func(m *measured) { gone(m); fileEvidence(t, m, edit(at.effect.Add(-time.Millisecond))) }}, "guarded")
			verdict(t, at, map[string]func(*measured){cutAfterClose: func(m *measured) { gone(m); fileEvidence(t, m, edit(at.effect)) }}, "back")
			// its package there with no plan: from the instant it keeps, from its watch's start when it keeps none
			planless := func(keep bool) func(m *measured) {
				return func(m *measured) {
					p, _, err := m.r.Ship.Load(m.c.ID)
					if err != nil {
						t.Fatal(err)
					}
					p.Plan = nil
					if !keep {
						clearReached(&p)
					}
					if err := m.r.Ship.save(&p); err != nil {
						t.Fatal(err)
					}
					fileEvidence(t, m, edit(at.reached.Add(time.Hour)))
				}
			}
			verdict(t, at, map[string]func(*measured){cutAfterClose: planless(true)}, "back")
			verdict(t, at, map[string]func(*measured){cutAfterClose: planless(false)}, "guarded")
		},
	}
	for _, r := range guardTimeModel {
		check := checks[r.reader]
		if check == nil {
			t.Fatalf("no check for %q", r.reader)
		}
		t.Run(r.reader, check)
	}
}

// A hand landing's instant is when the loop first saw its change on its base: a request merged into its base reached
// it at its merge; one merged into another branch, and a change landed by content, on the pass that sees it there,
// which comes after the merge or the commit's date (#110).
func TestAHandLandingsInstantIsWhenTheLoopSawItOnItsBase(t *testing.T) {
	ctx := context.Background()
	clock := time.Now().Truncate(time.Second).Add(time.Hour)
	merged := clock.Add(-2 * time.Hour)
	at := merged.UTC().Format(time.RFC3339)
	t.Run("merged into its base", func(t *testing.T) {
		s, c, _, p := proposedHand(t)
		s.Now = func() time.Time { return clock }
		run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+at+`","baseRefName":"main","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
		if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Landed || !reachedOf(got).Equal(merged) || !got.LandedAt.Equal(merged) {
			t.Fatalf("merged into its base: %v %v %v", reachedOf(got), got.LandedAt, err)
		}
	})
	t.Run("merged into another branch", func(t *testing.T) {
		s, c, _, p := proposedHand(t)
		s.Now = func() time.Time { return clock }
		run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+at+`","baseRefName":"release","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
		if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Landed || !reachedOf(got).Equal(clock) || !got.LandedAt.Equal(merged) {
			t.Fatalf("merged into another branch: %v %v %v", reachedOf(got), got.LandedAt, err)
		}
	})
	t.Run("landed by content", func(t *testing.T) {
		s, c, f, _ := proposedHand(t)
		s.Now = func() time.Time { return clock }
		pushOther(t, f, "main", map[string]string{
			"internal/capacity/names.go":      "package capacity\n\n// Named is the count.\nfunc Named() int { return 42 }\n",
			"internal/capacity/names_test.go": "package capacity\n\nimport \"testing\"\n\nfunc TestNamed(t *testing.T) {\n\tif Named() != 42 {\n\t\tt.Fatal(Named())\n\t}\n}\n",
		})
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED"}'`)
		if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Landed || !reachedOf(got).Equal(clock) || !got.LandedAt.Before(clock) {
			t.Fatalf("landed by content: %v %v %v", reachedOf(got), got.LandedAt, err)
		}
	})
}

// A hand landing's change merged into another branch reaches its base a week later: a Bash failure dated between its
// merge and that instant, filed at once, is history, never a recurrence, and the guard closes as guarded (#110, the
// review's probe C). A Bash failure between the instant and the watch's start is a recurrence filed as the case's
// evidence or not, one instant sorting the signal's events and the evidence alike (probe D).
func TestOneInstantSortsTheSignalAndTheEvidence(t *testing.T) {
	at := cutTimesOf(intoOther)
	if got, res, _ := guardRun(t, at, map[string]func(*measured){cutBeforePass: func(m *measured) {
		fileEvidence(t, m, failureOf("before-base", "Bash", at.landed.Add(3*aDay)))
	}}); got != "guarded" {
		t.Fatalf("a failure between the merge and its reaching the base: %s (%s)", got, res.Why)
	}
	for _, filed := range []bool{true, false} {
		e := failureOf("between", "Bash", at.reached.Add(aDay))
		got, res, _ := guardRun(t, at, map[string]func(*measured){cutBeforeWatch: func(m *measured) {
			if filed {
				fileEvidence(t, m, e)
			} else {
				put(t, m.st, []store.Event{e})
			}
		}})
		if got != "held" {
			t.Fatalf("a failure between the instant and the effect, filed %t: %s (%s)", filed, got, res.Why)
		}
	}
}

// History never stands in a recurrence's way: no judgment of history runs in the watch, so a recurrence always acts
// (#110). A recurrence and an Edit failure dated between the proposal and the instant filed before the same pass: the
// recurrence brings the case back, its rollback held. A held recurrence stays held when such history is filed a pass
// later. A problem that went on through the landing, an Edit failure dated before the instant and a Bash failure after
// it, both filed after the landing's pass and before the first pass in effect, is a recurrence on that pass.
func TestHistoryFiledLateNeverHidesARecurrence(t *testing.T) {
	at := cutTimesOf(byLoop)
	held := func(t *testing.T, got, why string, m *measured) {
		t.Helper()
		if c := m.theCase(t); got != "held" || !strings.Contains(why, "regressed") || c.Status != store.CaseMeasuring || c.Measurement.After == 0 {
			t.Fatalf("a recurrence not acted on: %s %s, case %s %+v", got, why, c.Status, c.Measurement)
		}
	}
	t.Run("filed in one pass", func(t *testing.T) {
		got, res, m := guardRun(t, at, map[string]func(*measured){cutDuringWatch: func(m *measured) {
			fileEvidence(t, m, failureOf("recur", "Bash", at.effect.Add(aDay)), failureOf("late-edit", "Edit", at.proposed.Add(aDay)))
		}})
		held(t, got, res.Why, m)
	})
	t.Run("filed a pass after the recurrence", func(t *testing.T) {
		got, res, m := guardRun(t, at, map[string]func(*measured){cutDuringWatch: func(m *measured) {
			fileEvidence(t, m, failureOf("recur", "Bash", at.effect.Add(aDay)))
		}})
		held(t, got, res.Why, m)
		fileEvidence(t, m, failureOf("late-edit", "Edit", at.proposed.Add(aDay)))
		m.clock = at.mid.Add(aDay)
		m.pass(t, Options{})
		if c := m.theCase(t); c.Status != store.CaseMeasuring || c.Measurement.After == 0 {
			t.Fatalf("the held recurrence is gone: case %s %+v", c.Status, c.Measurement)
		}
	})
	t.Run("a problem that went on through the landing", func(t *testing.T) {
		got, res, m := guardRun(t, at, map[string]func(*measured){cutBeforeWatch: func(m *measured) {
			fileEvidence(t, m, failureOf("edit-between", "Edit", at.proposed.Add(aDay)), failureOf("recur-out", "Bash", at.reached.Add(aDay)))
		}})
		held(t, got, res.Why, m)
	})
}

// The evidence's own date places a recurrence, never its event's time now (#110): a failure filed as the case's
// evidence dated after the instant, whose event a later read rewrites to a time before it, is still a recurrence, and
// it is reported first at the evidence's date.
func TestAGuardSortsARewrittenEventByItsEvidencesDate(t *testing.T) {
	at := cutTimesOf(byLoop)
	e := failureOf("rewritten", "Bash", at.effect.Add(aDay))
	got, res, _ := guardRun(t, at, map[string]func(*measured){cutDuringWatch: func(m *measured) {
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
	}})
	if first := "first at " + e.TS.UTC().Format(time.DateTime); got != "held" || !strings.Contains(res.Why, first) {
		t.Fatalf("a recurrence whose event was rewritten to before the instant: %s (%s), want %q", got, res.Why, first)
	}
}

// A guarded close names, in its log line, the case's evidence dated from the instant whose event the store no longer
// holds, which the watch passed over, and nothing else (#110): not such a piece dated before the instant, filed after
// the landing's pass (history), nor a piece of the loop's own still in the store. With more of them than the line
// holds, it names what fits and how many more, and the line stays whole.
func TestAGuardedCloseNamesTheEvidenceItCouldNotRead(t *testing.T) {
	ctx := context.Background()
	at := cutTimesOf(byLoop)
	closeLine := func(t *testing.T, m *measured) string {
		t.Helper()
		log, err := m.st.CaseLog(ctx, m.c.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range log {
			if l.Action == "closed" && strings.HasPrefix(l.Detail, "guarded") {
				return l.Detail
			}
		}
		t.Fatalf("no close in the log: %+v", log)
		return ""
	}
	t.Run("by date", func(t *testing.T) {
		before, between, in := failureOf("before", "Bash", at.reached.Add(-time.Hour)), failureOf("between", "Bash", at.reached.Add(time.Hour)), failureOf("in", "Bash", at.effect.Add(aDay))
		own := failureOf("own", "Bash", at.effect.Add(2*aDay))
		got, res, m := guardRun(t, at, map[string]func(*measured){cutBeforeWatch: func(m *measured) {
			for _, e := range []store.Event{before, between} {
				fileEvidence(t, m, e)
				lose(t, m, e)
			}
		}, cutDuringWatch: func(m *measured) {
			own.Cwd = m.r.Dir // the loop's own run
			fileEvidence(t, m, in, own)
			lose(t, m, in)
		}})
		if got != "guarded" {
			t.Fatalf("the close: %s (%s)", got, res.Why)
		}
		line := closeLine(t, m)
		for e, named := range map[store.Event]bool{before: false, between: true, in: true, own: false} {
			if strings.Contains(line, "ev:"+e.ID()) != named || named && !strings.Contains(line, "whose events the store no longer holds") {
				t.Errorf("the close's line and %s (named %t): %s", e.SourceKey, named, line)
			}
		}
	})
	t.Run("more than the line holds", func(t *testing.T) {
		got, res, m := guardRun(t, at, map[string]func(*measured){cutDuringWatch: func(m *measured) {
			for i := 0; i < 120; i++ {
				e := failureOf(fmt.Sprint("gone-", i), "Bash", at.effect.Add(aDay+time.Duration(i)*time.Minute))
				fileEvidence(t, m, e)
				lose(t, m, e)
			}
		}})
		if got != "guarded" {
			t.Fatalf("the close: %s (%s)", got, res.Why)
		}
		line := closeLine(t, m)
		if n := len([]rune(line)); n > noteMax() || strings.HasSuffix(line, "…") || !strings.HasSuffix(line, "more") || strings.Count(line, "ev:") >= 120 {
			t.Fatalf("a close's line of %d characters: %s", n, line)
		}
	})
}

// The exposure watched of a guard whose case's evidence dated between the instant and its watch's start brings it back
// leaves that recurrence's hour out: the change was not in effect then (guardWatched). Its use in the watch, Bash an
// hour for a day, is the exposure watched, and nothing else; the report says the recurrence came before the change took
// effect, never more recurrences than hours watched.
func TestAWatchLeavesOutTheHourOfARecurrenceBeforeItsStart(t *testing.T) {
	ctx := context.Background()
	at := cutTimesOf(byLoop)
	var want map[store.Unit]bool
	got, res, _ := guardRun(t, at, map[string]func(*measured){cutDuringWatch: func(m *measured) {
		var uses []store.Event
		for i := 0; i < 24; i++ {
			uses = append(uses, serverUse(fmt.Sprint("w-", i), at.effect.Add(aDay+time.Duration(i)*time.Hour), "Bash"))
		}
		put(t, m.st, uses)
		fileEvidence(t, m, failureOf("before-start", "Bash", at.reached.Add(aDay)))
		p, _, err := m.r.Ship.Load(m.c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want, err = guardExposure(ctx, m.st, *p.Plan, nil, p.GuardFrom, at.mid, m.r.Dir); err != nil {
			t.Fatal(err)
		}
	}})
	if got != "held" || len(want) != 24 || res.Result == nil || res.Result.After.Base != len(want) || !strings.Contains(res.Why, "before it took effect") || strings.Contains(res.Why, "1 of 24") {
		t.Fatalf("a recurrence before the watch's start: %s (%s), %+v, want %d watched", got, res.Why, res.Result, len(want))
	}
}
