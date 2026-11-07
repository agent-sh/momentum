package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A case closed as guarded whose guard is gone, its package deleted or saved by a build from before guards (which
// writes its plan back without one), is still watched by its own evidence: while there is none, a problem note says
// its signal is not read and names case reopen; an event of its evidence from the watch's start, in a form the signal
// does not count, brings it back regressed, and the note goes once the case leaves its close.
func TestAGuardedCaseWithoutItsGuardComesBackOnItsEvidence(t *testing.T) {
	lose := map[string]func(t *testing.T, m *measured){
		"its package gone": func(t *testing.T, m *measured) {
			if err := os.RemoveAll(m.r.Ship.pkgDir(m.c.ID)); err != nil {
				t.Fatal(err)
			}
		},
		"its guard dropped by an older build": func(t *testing.T, m *measured) {
			path := filepath.Join(m.r.Ship.pkgDir(m.c.ID), "package.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var pkg map[string]any
			if err := json.Unmarshal(raw, &pkg); err != nil {
				t.Fatal(err)
			}
			delete(pkg["plan"].(map[string]any), "guard")
			if raw, err = json.Marshal(pkg); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, lost := range lose {
		t.Run(name, func(t *testing.T) {
			m := guarded(t, time.Hour, toolSignal.Source, nil)
			start := m.clock
			closeGuarded(t, m)
			lost(t, m)
			noted := func() []store.Note {
				var out []store.Note
				for _, n := range m.notes(t) {
					if n.ID == "measure:"+m.c.ID && n.Kind == store.NoteProblem {
						out = append(out, n)
					}
				}
				return out
			}
			m.clock = m.theCase(t).Closed.Add(time.Hour)
			if _, err := m.passes(t, Options{}); err != nil {
				t.Fatal(err)
			}
			if ns := noted(); len(ns) != 1 || !strings.Contains(ns[0].Title, "cannot watch") || !strings.Contains(ns[0].Detail, "case reopen "+m.c.ID) ||
				m.theCase(t).Status != store.CaseClosed {
				t.Fatalf("a guarded close with no guard to watch by: %+v, case %s", ns, m.theCase(t).Status)
			}
			back := toolResult("new-form", start.Add(10*aDay), "new-form", true) // dated inside the watch, filed after its close
			back.Summary = "Exit code 1 zz-tool-crashed: the tool failed"
			fileEvidence(t, m, back)
			m.clock = m.clock.Add(time.Hour)
			if _, err := m.passes(t, Options{}); err != nil {
				t.Fatal(err)
			}
			c := m.theCase(t)
			if l := lastLog(t, m.st, c.ID); c.Status != store.CaseOpen || l.Action != "regressed" || !strings.Contains(l.Detail, "own evidence") {
				t.Fatalf("its evidence after the close: case %s, %+v", c.Status, l)
			}
			m.clock = m.clock.Add(time.Hour)
			if _, err := m.passes(t, Options{}); err != nil {
				t.Fatal(err)
			}
			if ns := noted(); len(ns) != 0 {
				t.Fatalf("its problem note stayed after it came back: %+v", ns)
			}
		})
	}
}

// The loop's own runs are left out of a plan's admission by the rule its counts keep (store.InDir): their failures,
// filed as the case's evidence, are no evidence the signal fails to count. A measured plan whose gate holds seven of
// them beside the case's six is admitted, and so is a guard whose history holds fourteen beside its twelve.
func TestTheLoopsOwnEvidenceIsLeftOutOfAPlansAdmission(t *testing.T) {
	ctx := context.Background()
	file := func(t *testing.T, st *store.Store, c store.Case, evs []store.Event) {
		t.Helper()
		put(t, st, evs)
		f := store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title}
		for _, e := range evs {
			f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
		}
		if _, _, err := st.ObserveCase(ctx, f, "detector"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a measured plan", func(t *testing.T) {
		st := testStore(t)
		dir := t.TempDir()
		c := testingCase(t, st)
		var own []store.Event
		for i := 0; i < 7; i++ {
			own = append(own, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprint("loop-", i), TS: time.Now().Add(-time.Duration(i+1)*time.Hour - 30*time.Minute),
				Session: fmt.Sprint("loop-", i), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash",
				Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`, Cwd: filepath.Join(dir, "runs", fmt.Sprint(i))})
		}
		file(t, st, c, own)
		plan, err := (&Ship{Store: st, Dir: dir}).validPlan(ctx, c, goodSignal, 7*aDay, "", time.Now())
		if err != nil || plan.Guard != nil || plan.Gate != 6 {
			t.Fatalf("a measured plan beside the loop's own evidence: %+v %v", plan, err)
		}
	})
	t.Run("a guard", func(t *testing.T) {
		st := testStore(t)
		dir := t.TempDir()
		now := time.Now().Truncate(time.Hour)
		c, _ := recurring(t, st, toolSignal.Source, now, now, now, fourBursts(now)...)
		var own []store.Event // between the bursts, outside the gate
		for i := 0; i < 14; i++ {
			own = append(own, loopRun(fmt.Sprint("loop-", i), now.Add(-20*aDay+time.Duration(i)*time.Hour), dir, false))
		}
		file(t, st, c, own)
		plan, err := (&Ship{Store: st, Dir: dir}).validPlan(ctx, c, toolSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Incidents != 4 || g.Exposure != 684 {
			t.Fatalf("a guard beside the loop's own evidence: %+v %v", g, err)
		}
	})
}

// An ingest source of a guard's plan that has not read at all since its watch's end, other than the signal's own, is
// left out a window (the plan's) after the end, as a stopped command source is: a reader that no longer runs does not
// hold the close for good. Within the window the watch waits for it. The signal's own source with no read since the
// end holds the close past the window, and a week past the end its problem note says so.
func TestAGuardLeavesOutAnIngestSourceWithNoReadAWindowAfterItsEnd(t *testing.T) {
	watch := func(t *testing.T) (*measured, time.Time) {
		t.Helper()
		m := guarded(t, time.Hour, "zz-use", nil)
		markAll(t, m, toolSignal.Source, "zz-use")
		m.measure(t, Options{})
		start := m.clock
		weekly(t, m.st, "zz-use", "w", start)
		m.clock = start.Add(10 * aDay)
		markAll(t, m, toolSignal.Source, "zz-use")
		m.measure(t, Options{})
		return m, start.Add(30 * aDay)
	}
	t.Run("another ingest source", func(t *testing.T) {
		m, end := watch(t)
		m.clock = end.Add(2 * time.Hour)
		markAll(t, m, toolSignal.Source) // zz-use reads no more
		if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "zz-use") {
			t.Fatalf("within a window of the end: %+v", got)
		}
		m.clock = end.Add(7*aDay + time.Hour)
		markAll(t, m, toolSignal.Source)
		if got := m.measure(t, Options{}); got.Status != "guarded" {
			t.Fatalf("a window past the end, a source with no read since it: %+v", got)
		}
	})
	t.Run("the signal's own source", func(t *testing.T) {
		m, end := watch(t)
		m.clock = end.Add(8 * aDay)
		markAll(t, m, "zz-use") // the signal's source reads no more
		if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, toolSignal.Source) {
			t.Fatalf("a window past the end, the signal's source with no read since it: %+v", got)
		}
		behind := false
		for _, n := range m.notes(t) {
			behind = behind || n.Kind == store.NoteProblem && strings.Contains(n.Title, "behind on "+toolSignal.Source)
		}
		if !behind {
			t.Fatalf("no note that the store is behind on the signal's source: %+v", m.notes(t))
		}
	})
}

// A guard's stretch out of effect starts at the last pass that saw its change in effect: the change may have gone
// out of effect any time after it, while no pass ran. In effect at its first pass, then out of effect until the pass
// ten days on (the pass two days on found it so), it closes with the 75 hours of its last three weeks' use watched,
// not the 100 a stretch from the first pass that found it out of effect would give.
func TestAGuardsStretchOutOfEffectStartsAtTheLastPassThatSawItInEffect(t *testing.T) {
	m := guarded(t, time.Hour, toolSignal.Source, nil)
	m.pass(t, Options{})
	start := m.clock
	weekly(t, m.st, toolSignal.Source, "w", start)
	run(t, m.repo, "git", "checkout", "-q", "HEAD~1") // out of effect from just after that pass, while none ran
	m.clock = start.Add(2 * aDay)
	if got := m.pass(t, Options{}); got.Status != "waiting" {
		t.Fatalf("out of effect: %+v", got)
	}
	run(t, m.repo, "git", "checkout", "-q", "main")
	m.clock = start.Add(10 * aDay)
	m.pass(t, Options{})
	m.clock = start.Add(30*aDay + time.Hour)
	if got := m.pass(t, Options{}); got.Status != "guarded" || got.Result == nil || !strings.Contains(got.Result.Effect, "no recurrence in 75 session hours watched") {
		t.Fatalf("a watch out of effect from its first pass to day ten: %+v %+v", got, got.Result)
	}
}
