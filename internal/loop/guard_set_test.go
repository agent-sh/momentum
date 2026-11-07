package loop

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// serverSignal counts a server's failed results whatever its tool: the problem is the server's, and its failures
// surface in either of its two tools.
var serverSignal = Signal{Source: toolSignal.Source, Kinds: toolSignal.Kinds, Match: toolSignal.Match}

// The server's two tools, and a tool of another kind beside them.
const (
	serverA = "mcp__zz_server__fetch"
	serverB = "mcp__zz_server__list"
)

// serverUse is a successful result of a tool in a session of its own.
func serverUse(key string, at time.Time, tool string) store.Event {
	e := toolResult(key, at, key, false)
	e.Tool = tool
	return e
}

// serverStore is the store of one server whose failures split across its two tools: from 28 days before at until
// useUntil, the server's first tool answers in each even hour and its second in each odd hour, each in a session of
// its own (336 hours each, over four weeks), and Read in every hour beside them; the four bursts of fourBursts, three
// failures each, alternate between the two tools. The source began 60 days before at and was read at it. It returns a
// case in testing whose evidence is every failure.
func serverStore(t *testing.T, st *store.Store, at, useUntil time.Time) store.Case {
	t.Helper()
	return serverStoreIn(t, st, at, useUntil, "")
}

// serverStoreIn is serverStore with every failure in one directory, when cwd names one, and a failure of the second
// tool beside the first's in the first burst's first unit: one unit of its problem holds both tools.
func serverStoreIn(t *testing.T, st *store.Store, at, useUntil time.Time, cwd string) store.Case {
	t.Helper()
	evs := []store.Event{serverUse("began", at.Add(-60*aDay), serverA)}
	for i, u := 0, at.Add(-28*aDay); u.Before(useUntil); i, u = i+1, u.Add(time.Hour) {
		tool := serverA
		if i%2 == 1 {
			tool = serverB
		}
		evs = append(evs, serverUse(fmt.Sprint("use-", i), u, tool), serverUse(fmt.Sprint("read-", i), u.Add(10*time.Minute), "Read"))
	}
	var failures []store.Event
	for b, from := range fourBursts(at) {
		for j := 0; j < 3; j++ {
			e := toolResult(fmt.Sprintf("b%d-%d", b, j), from.Add(time.Duration(j)*time.Hour+30*time.Minute), fmt.Sprintf("b%d-%d", b, j), true)
			e.Tool = serverA
			if (b+j)%2 == 1 {
				e.Tool = serverB
			}
			failures = append(failures, e)
		}
	}
	if cwd != "" {
		both := toolResult("b0-0-both", failures[0].TS.Add(10*time.Minute), failures[0].Session, true)
		both.Tool = serverB
		failures = append(failures, both)
		for i := range failures {
			failures[i].Cwd = cwd
		}
	}
	put(t, st, append(evs, failures...))
	if err := st.MarkRead(t.Context(), toolSignal.Source, at); err != nil {
		t.Fatal(err)
	}
	return testingCaseOf(t, st, "failures/v2:zz-server", failures)
}

// A history whose failures split across two tools of one server, with no tool shared by all of them, gets a guard (#93):
// its exposure is the set of the two tools, and counts the hours in which either was called, 336 of each and the
// failures' own 12, not Read's 672 beside them. The proposal names both tools. Once its change is in effect, the watch
// counts either tool's use, 25 hours of each and none of Read's, and a recurrence on either tool brings the case back
// regressed.
func TestAGuardOverTheToolsOfOneServer(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	st := testStore(t)
	c := serverStore(t, st, now, now)
	plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
	g, _ := guardOf(t, plan)
	if err != nil || g == nil || strings.Join(g.Tools, " ") != serverA+" "+serverB || g.Tool != "" || g.Cwd != "" || g.Incidents != 4 || g.Exposure != 336+336+12 {
		t.Fatalf("a server's failures split across its two tools: %+v %v", g, err)
	}
	if text := (&Ship{}).packageText(c, Package{Plan: &plan, Check: &Check{}}); !strings.Contains(text, "any of the 2 tools its events were of was used ("+serverA+", "+serverB+")") {
		t.Fatalf("the proposal does not name the guard's tools: %s", text)
	}
	t.Run("in one directory, both tools in one unit", func(t *testing.T) {
		// every failure in the server's own directory, and one unit holding a failure of each tool: the tools are its
		// unit before the directory, and that unit counts once
		st := testStore(t)
		c := serverStoreIn(t, st, now, now, "/work/server")
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || strings.Join(g.Tools, " ") != serverA+" "+serverB || g.Cwd != "" || len(g.Cwds) != 0 || g.Exposure != 336+336+12 {
			t.Fatalf("a server's failures in one directory: %+v %v", g, err)
		}
	})
	t.Run("the watch counts both tools", func(t *testing.T) {
		m := guardedBy(t, serverSignal, time.Hour, toolSignal.Source, func(st *store.Store, planAt, useUntil time.Time) store.Case {
			return serverStore(t, st, planAt, useUntil)
		})
		m.pass(t, Options{})
		start := m.clock
		var evs []store.Event
		for i := 0; i < 25; i++ {
			at := start.Add(time.Duration(i+1) * 10 * time.Hour)
			evs = append(evs, serverUse(fmt.Sprint("wa-", i), at, serverA), serverUse(fmt.Sprint("wb-", i), at.Add(5*time.Hour), serverB),
				serverUse(fmt.Sprint("wr-", i), at.Add(2*time.Hour), "Read"))
		}
		put(t, m.st, evs)
		m.clock = start.Add(30*aDay + time.Hour)
		if got := m.pass(t, Options{}); got.Status != "guarded" || got.Result == nil || got.Result.Watched != 50 {
			t.Fatalf("a watch of both tools' use: %+v %+v", got, got.Result)
		}
	})
	for _, tool := range []string{serverA, serverB} {
		t.Run("a recurrence on "+tool, func(t *testing.T) {
			m := guardedBy(t, serverSignal, time.Hour, toolSignal.Source, func(st *store.Store, planAt, useUntil time.Time) store.Case {
				return serverStore(t, st, planAt, useUntil)
			})
			m.pass(t, Options{})
			effect := m.clock
			again := toolResult("again", effect.Add(2*time.Hour), "again", true)
			again.Tool = tool
			put(t, m.st, []store.Event{again})
			m.clock = effect.Add(3 * time.Hour)
			if got := m.pass(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "regressed: its problem came back during its guard's watch") {
				t.Fatalf("a recurrence on %s: %+v", tool, got)
			}
		})
	}
}

// anchoredServer is the store of a server whose source began with a burst of failures of the first tools given, one an
// hour, 27 days before at; the server's two tools answered every hour after it until at, and three bursts followed, a
// week and a half apart, of the tools later gives each failure (b from 1). It returns a case in testing whose evidence is
// every failure.
func anchoredServer(t *testing.T, st *store.Store, at time.Time, first []string, later func(b, j int) string) store.Case {
	t.Helper()
	bursts := fourBursts(at)
	var evs, failures []store.Event
	for j, tool := range first {
		e := toolResult(fmt.Sprintf("first-%d", j), bursts[0].Add(time.Duration(j)*time.Hour+30*time.Minute), fmt.Sprintf("first-%d", j), true)
		e.Tool = tool
		failures = append(failures, e)
	}
	for i, u := 0, bursts[0].Add(4*time.Hour); u.Before(at); i, u = i+1, u.Add(time.Hour) {
		evs = append(evs, serverUse(fmt.Sprint("use-", i), u, []string{serverA, serverB}[i%2]))
	}
	for b := 1; b < len(bursts); b++ {
		for j := 0; j < 3; j++ {
			e := toolResult(fmt.Sprintf("b%d-%d", b, j), bursts[b].Add(time.Duration(j)*time.Hour+30*time.Minute), fmt.Sprintf("b%d-%d", b, j), true)
			e.Tool = later(b, j)
			failures = append(failures, e)
		}
	}
	put(t, st, append(evs, failures...))
	if err := st.MarkRead(t.Context(), toolSignal.Source, at); err != nil {
		t.Fatal(err)
	}
	return testingCaseOf(t, st, "failures/v2:zz-anchored", failures)
}

// A guard's set is found in the incidents its history counts: the incident its source began with is dropped from them
// (it is no draw of the problem's rate), so its tools choose no member of the set and count toward no member's hours.
func TestAGuardsSetIsFoundAfterTheIncidentItsSourceBeganWith(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	t.Run("it chooses no member", func(t *testing.T) {
		st := testStore(t) // the first burst of a third tool; the server's two after it
		c := anchoredServer(t, st, now, []string{"Bash", "Bash", "Bash"}, func(b, j int) string { return []string{serverA, serverB}[(b+j)%2] })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || strings.Join(g.Tools, " ") != serverA+" "+serverB || !strings.Contains(g.Instead, "began inside") {
			t.Fatalf("the set after the first burst: %+v %v", g, err)
		}
	})
	t.Run("it holds no member's hour", func(t *testing.T) {
		st := testStore(t) // the second tool fails once after the first burst, and in that burst once more
		c := anchoredServer(t, st, now, []string{serverA, serverB, serverA}, func(b, j int) string {
			if b == 3 && j == 1 {
				return serverB
			}
			return serverA
		})
		_, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if err == nil || !strings.Contains(err.Error(), "(2 tools, 1 of them in one hour alone") {
			t.Fatalf("a member of one hour after the first burst: %v", err)
		}
	})
}

// A guard's set reads back from its plan's JSON as it was written: a set of more as tools or cwds, a set of one as tool
// or cwd, as the builds before sets wrote it.
func TestAGuardsSetReadsBackFromItsJSON(t *testing.T) {
	for _, g := range []Guard{{Tools: []string{serverA, serverB}}, {Cwds: []string{"/work/a", "/work/b"}}, {Tools: []string{serverA}}, {Cwds: []string{"/work/a"}}} {
		plan := Plan{Units: SessionHours, Guard: &g}
		raw, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := guardOf(t, plan)
		var back Plan
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		one := len(g.Tools) == 1 || len(g.Cwds) == 1
		if back.Guard == nil || !slices.Equal(back.Guard.Tools, g.Tools) || !slices.Equal(back.Guard.Cwds, g.Cwds) ||
			one != (wire.Tool != "" || wire.Cwd != "") || one == (len(wire.Tools) > 0 || len(wire.Cwds) > 0) {
			t.Fatalf("%+v read back as %+v from %s", g, back.Guard, raw)
		}
	}
}
