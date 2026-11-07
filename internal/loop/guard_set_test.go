package loop

import (
	"database/sql"
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

// The server's two tools that fail, a third of its own that never does, and a tool of another server.
const (
	serverA     = "mcp__zz_server__fetch"
	serverB     = "mcp__zz_server__list"
	serverC     = "mcp__zz_server__ping"
	otherServer = "mcp__zz_other__list"
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

// serverStoreIn is serverStore with the server's work in one directory, when cwd names one: its two tools' use and
// every failure there (Read's use in none), and a failure of the second tool beside the first's in the first burst's
// first unit, so one unit of its problem holds both tools.
func serverStoreIn(t *testing.T, st *store.Store, at, useUntil time.Time, cwd string) store.Case {
	t.Helper()
	evs := []store.Event{serverUse("began", at.Add(-60*aDay), serverA)}
	for i, u := 0, at.Add(-28*aDay); u.Before(useUntil); i, u = i+1, u.Add(time.Hour) {
		tool := serverA
		if i%2 == 1 {
			tool = serverB
		}
		use := serverUse(fmt.Sprint("use-", i), u, tool)
		use.Cwd = cwd
		evs = append(evs, use, serverUse(fmt.Sprint("read-", i), u.Add(10*time.Minute), "Read"))
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
	t.Run("in one directory, the tools within it", func(t *testing.T) {
		// the server's work all in its own directory, one unit holding a failure of each tool: the tools, the server,
		// the directory and the tools or server within it hold the same 684 hours, that unit counted once, and the
		// tools within the directory, which each of the others holds by construction, are the guard's (#98)
		st := testStore(t)
		c := serverStoreIn(t, st, now, now, "/work/server")
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Unit != "tools in its directory" || g.Cwd != "/work/server" || strings.Join(g.Tools, " ") != serverA+" "+serverB ||
			g.Server != "" || g.Exposure != 336+336+12 {
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
// (it is no draw of the problem's rate), so its tools choose no member of the set and count toward no member's incidents.
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
	t.Run("it holds no member's incident", func(t *testing.T) {
		st := testStore(t) // another server's tool fails once after the first burst, and in that burst once more
		c := anchoredServer(t, st, now, []string{serverA, otherServer, serverA}, func(b, j int) string {
			if b == 3 && j == 1 {
				return otherServer
			}
			return serverA
		})
		_, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if err == nil || !strings.Contains(err.Error(), "(2 tools, 1 of them in fewer than two of its 3 incidents") {
			t.Fatalf("a member of one incident after the first burst: %v", err)
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

// oneIncident is the store of a server whose failures fall in one incident: from 28 days before at until useUntil its
// three tools answer in turn, one an hour, each call in a session of its own (224 hours of each over four weeks), and
// Read beside them every hour; 15 days before at, six failures an hour apart, of the tool tool gives each, each in a
// session and a directory of its own. The source began 60 days before at and was read at it. It returns a case in
// testing whose evidence is every failure.
func oneIncident(t *testing.T, st *store.Store, at, useUntil time.Time, tool func(j int) string) store.Case {
	t.Helper()
	return oneIncidentOf(t, st, at, useUntil, [3]string{serverA, serverB, serverC}, tool)
}

// oneIncidentOf is oneIncident with the three tools that answer in turn named by uses, and extra events stored beside
// the failures and filed with them as the case's evidence.
func oneIncidentOf(t *testing.T, st *store.Store, at, useUntil time.Time, uses [3]string, tool func(j int) string, extra ...store.Event) store.Case {
	t.Helper()
	evs := []store.Event{serverUse("began", at.Add(-60*aDay), uses[0])}
	for i, u := 0, at.Add(-28*aDay); u.Before(useUntil); i, u = i+1, u.Add(time.Hour) {
		evs = append(evs, serverUse(fmt.Sprint("use-", i), u, uses[i%3]), serverUse(fmt.Sprint("read-", i), u.Add(10*time.Minute), "Read"))
	}
	var failures []store.Event
	for j := 0; j < 6; j++ {
		key := fmt.Sprint("f-", j)
		e := toolResult(key, at.Add(-15*aDay+time.Duration(j)*time.Hour+30*time.Minute), key, true)
		e.Tool, e.Cwd = tool(j), fmt.Sprint("/work/d", j)
		failures = append(failures, e)
	}
	failures = append(failures, extra...)
	put(t, st, append(evs, failures...))
	if err := st.MarkRead(t.Context(), toolSignal.Source, at); err != nil {
		t.Fatal(err)
	}
	return testingCaseOf(t, st, "failures/v2:zz-one-incident", failures)
}

// alternate gives the failures of oneIncident to the server's two tools in turn.
func alternate(j int) string { return []string{serverA, serverB}[j%2] }

// A history of one incident spread across two tools of one MCP server, from six directories, holds no tool, set or
// directory every event shares, but its server holds them all (#98): the guard's unit is the server, and its exposure
// counts the hours with a call to any of its tools, its third that never failed too (224 hours of each, and the
// failures' own 6), never Read's. Once its change is in effect, its watch counts any of the server's tools, and a
// recurrence on either tool that failed brings the case back regressed. A history that mixes the server's tools with
// Bash gets no server: Bash is of none.
func TestAGuardOverAnMCPServer(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	t.Run("its unit is the server", func(t *testing.T) {
		st := testStore(t)
		c := oneIncident(t, st, now, now, alternate)
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		g, _ := guardOf(t, plan)
		if err != nil || g == nil || g.Unit != "server" || g.Server != "zz_server" || g.Tool != "" || len(g.Tools) != 0 || g.Cwd != "" || g.Incidents != 1 || g.Exposure != 3*224+6 {
			t.Fatalf("one incident across a server's two tools: %+v %v", g, err)
		}
		if text := (&Ship{}).packageText(c, Package{Plan: &plan, Check: &Check{}}); !strings.Contains(text, "in which any tool of the MCP server zz_server was used (its unit: the server") {
			t.Fatalf("the proposal does not name the guard's server: %s", text)
		}
	})
	t.Run("a history with Bash beside the server's tools", func(t *testing.T) {
		st := testStore(t)
		c := oneIncident(t, st, now, now, func(j int) string {
			if j == 3 {
				return "Bash"
			}
			return alternate(j)
		})
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err == nil || !strings.Contains(err.Error(), "(3 tools, 3 of them in fewer than two of its 1 incidents; in 6 directories; some of no MCP server") {
			t.Fatalf("a server's tools and Bash: %+v %v", g, err)
		}
	})
	guardedServer := func(t *testing.T) *measured {
		return guardedBy(t, serverSignal, time.Hour, toolSignal.Source, func(st *store.Store, planAt, useUntil time.Time) store.Case {
			return oneIncident(t, st, planAt, useUntil, alternate)
		})
	}
	t.Run("the watch counts any of the server's tools", func(t *testing.T) {
		m := guardedServer(t)
		m.pass(t, Options{})
		start := m.clock
		var evs []store.Event
		for i := 0; i < 25; i++ {
			at := start.Add(time.Duration(i+1) * 20 * time.Hour)
			evs = append(evs, serverUse(fmt.Sprint("wa-", i), at, serverA), serverUse(fmt.Sprint("wb-", i), at.Add(5*time.Hour), serverB),
				serverUse(fmt.Sprint("wc-", i), at.Add(10*time.Hour), serverC), serverUse(fmt.Sprint("wr-", i), at.Add(15*time.Hour), "Read"))
		}
		put(t, m.st, evs)
		m.clock = start.Add(30*aDay + time.Hour)
		if got := m.pass(t, Options{}); got.Status != "guarded" || got.Result == nil || got.Result.Watched != 75 {
			t.Fatalf("a watch of the server's use: %+v %+v", got, got.Result)
		}
	})
	for _, tool := range []string{serverA, serverB} {
		t.Run("a recurrence on "+tool, func(t *testing.T) {
			m := guardedServer(t)
			m.pass(t, Options{})
			effect := m.clock
			again := toolResult("again", effect.Add(2*time.Hour), "again", true)
			again.Tool, again.Cwd = tool, "/work/elsewhere"
			put(t, m.st, []store.Event{again})
			m.clock = effect.Add(3 * time.Hour)
			if got := m.pass(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "regressed: its problem came back during its guard's watch") {
				t.Fatalf("a recurrence on %s: %+v", tool, got)
			}
		})
	}
}

// Two tools of Codex's gmail connector that fail, a third of it that never does, one Codex named whole (its name did
// not begin with the connector's), and another connector's tool.
const (
	gmailSearch  = "mcp__codex_apps__gmail___search"
	gmailSend    = "mcp__codex_apps__gmail___send"
	gmailLabel   = "mcp__codex_apps__gmail___label"
	gmailCapture = "mcp__codex_apps__gmail__capture_file_upload"
	calendarList = "mcp__codex_apps__calendar___list"
)

// Codex serves every app connector from one server, codex_apps, so the whole server would count every connector's hours
// as the problem's unit in use (#101): each connector is a server of its own. One incident across two tools of the
// gmail connector, from six directories, takes the connector, and its exposure counts the hours of its tools (224 of
// the one that failed and of the one that never did, and the failures' own 6), never the calendar connector's or
// Read's. One incident across a tool Codex cut the connector's name from and one it named whole takes the connector,
// whose exposure counts both forms (224 of each, and the failures' 6). A problem across connectors (an expired
// sign-in) takes codex_apps as a whole, whose exposure counts every connector's tools (224 of each of three, and the
// failures' 6). A history of codex_apps's tools beside another server's has no server, and its refusal counts
// codex_apps once, with its connectors.
func TestACodexAppsConnectorIsAGuardsServer(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	t.Run("its unit is the connector", func(t *testing.T) {
		st := testStore(t)
		c := oneIncidentOf(t, st, now, now, [3]string{gmailSearch, gmailLabel, calendarList}, func(j int) string { return []string{gmailSearch, gmailSend}[j%2] })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		g, _ := guardOf(t, plan)
		if err != nil || g == nil || g.Unit != "server" || g.Server != "codex_apps__gmail" || g.Incidents != 1 || g.Exposure != 2*224+6 {
			t.Fatalf("one incident across a connector's two tools: %+v %v", g, err)
		}
		if text := (&Ship{}).packageText(c, Package{Plan: &plan, Check: &Check{}}); !strings.Contains(text, "in which any tool of the MCP server codex_apps__gmail was used") {
			t.Fatalf("the proposal does not name the guard's connector: %s", text)
		}
	})
	t.Run("both of Codex's name forms take the connector", func(t *testing.T) {
		st := testStore(t)
		c := oneIncidentOf(t, st, now, now, [3]string{gmailSearch, gmailCapture, calendarList}, func(j int) string { return []string{gmailSearch, gmailCapture}[j%2] })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Unit != "server" || g.Server != "codex_apps__gmail" || g.Exposure != 2*224+6 {
			t.Fatalf("a connector's tools in both forms: %+v %v", g, err)
		}
	})
	t.Run("two connectors take codex_apps", func(t *testing.T) {
		st := testStore(t)
		c := oneIncidentOf(t, st, now, now, [3]string{gmailSearch, gmailLabel, calendarList}, func(j int) string { return []string{gmailSearch, calendarList}[j%2] })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Unit != "server" || g.Server != "codex_apps" || g.Exposure != 3*224+6 {
			t.Fatalf("two connectors: %+v %v", g, err)
		}
	})
	t.Run("codex_apps beside another server", func(t *testing.T) {
		st := testStore(t)
		c := oneIncidentOf(t, st, now, now, [3]string{gmailSearch, gmailLabel, calendarList}, func(j int) string { return []string{gmailSearch, calendarList, serverA}[j%3] })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err == nil || !strings.Contains(err.Error(), "(3 tools, 3 of them in fewer than two of its 1 incidents; in 6 directories; of 2 MCP servers, codex_apps's tools of 2 connectors") {
			t.Fatalf("codex_apps's tools and another server's: %+v %v", g, err)
		}
	})
}

// codex_apps holds each of its connectors and any tools of it, so on a tie a connector or a set of its tools is the
// narrower; a connector holds neither codex_apps nor another connector.
func TestCodexAppsHoldsItsConnectors(t *testing.T) {
	apps, gmail := guardCandidate{unit: "server", server: "codex_apps"}, guardCandidate{unit: "server", server: "codex_apps__gmail"}
	for _, tc := range []struct {
		c, o guardCandidate
		want bool
	}{
		{apps, gmail, true},
		{apps, guardCandidate{unit: "tools", tools: []string{gmailSearch, calendarList}}, true},
		{apps, guardCandidate{unit: "tools in its directory", tools: []string{gmailSearch, calendarList}, cwds: []string{"/work/a"}}, true},
		{apps, guardCandidate{unit: "tools", tools: []string{gmailSearch, "Bash"}}, false},
		{gmail, apps, false},
		{gmail, guardCandidate{unit: "server", server: "codex_apps__calendar"}, false},
		{gmail, guardCandidate{unit: "tool", tools: []string{gmailSearch}}, true},
		{gmail, guardCandidate{unit: "tool", tools: []string{calendarList}}, false},
	} {
		if got := tc.c.holds(tc.o); got != tc.want {
			t.Errorf("%+v holds %+v: %v", tc.c, tc.o, got)
		}
	}
}

// A tool found by its events, not named by the signal, is a unit only in two incidents or more, as a set's member is
// (#98): one incident's cause may surface in any tool. A built-in tool in one incident from six directories leaves no
// unit; an MCP server's one tool there leaves its server. The signal's own tool keeps its exemption: every event is of
// it by the signal's definition (a guard of a problem that stopped, TestAGuardStandsInWhereAMeasuredPlanIsRefused).
func TestAToolFoundByItsEventsNeedsTwoIncidents(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	t.Run("a built-in tool", func(t *testing.T) {
		st := testStore(t)
		c := oneIncident(t, st, now, now, func(int) string { return "Edit" })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err == nil || !strings.Contains(err.Error(), "(1 tool in one incident alone; in 6 directories; of no MCP server") {
			t.Fatalf("a built-in tool in one incident: %+v %v", g, err)
		}
	})
	t.Run("an MCP server's tool", func(t *testing.T) {
		st := testStore(t)
		c := oneIncident(t, st, now, now, func(int) string { return serverA })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, serverSignal, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Unit != "server" || g.Server != "zz_server" || g.Tool != "" || g.Exposure != 3*224+6 {
			t.Fatalf("an MCP tool in one incident: %+v %v", g, err)
		}
	})
}

// The signal's own tool is exempt from the two incidents only on the case's evidence, which its detector filed, never
// on the plan's word, which a build writes (#101). A signal that names Bash, over one incident of Bash failures from six
// directories, is refused where the case's evidence holds Edit failures beside them, and admitted with Bash as its unit
// (224 hours of its use, and the failures' own 6) where every event of the evidence is of Bash. Only the evidence in
// the history counts: an Edit failure before it leaves Bash admitted. An event of the evidence the store no longer holds
// is of no tool, so Bash is refused.
func TestTheSignalsToolIsExemptOnlyOnTheCasesEvidence(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	bash := serverSignal
	bash.Tool = "Bash"
	t.Run("evidence of another tool beside it", func(t *testing.T) {
		st := testStore(t)
		var edits []store.Event
		for j := 0; j < 2; j++ {
			key := fmt.Sprint("edit-", j)
			e := toolResult(key, now.Add(-15*aDay+time.Duration(j)*time.Hour+40*time.Minute), key, true)
			e.Tool, e.Cwd = "Edit", fmt.Sprint("/work/e", j)
			edits = append(edits, e)
		}
		c := oneIncidentOf(t, st, now, now, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" }, edits...)
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, bash, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err == nil || !strings.Contains(err.Error(), "(1 tool in one incident alone; in 6 directories; of no MCP server") {
			t.Fatalf("a named tool over evidence of two tools: %+v %v", g, err)
		}
	})
	t.Run("evidence all of it", func(t *testing.T) {
		st := testStore(t)
		c := oneIncidentOf(t, st, now, now, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" })
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, bash, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Unit != "tool" || g.Tool != "Bash" || g.Incidents != 1 || g.Exposure != 224+6 {
			t.Fatalf("a named tool over evidence all of it: %+v %v", g, err)
		}
	})
	t.Run("evidence of another tool before the history", func(t *testing.T) {
		st := testStore(t)
		edit := toolResult("edit-before", now.Add(-35*aDay), "edit-before", true)
		edit.Tool, edit.Cwd = "Edit", "/work/e"
		c := oneIncidentOf(t, st, now, now, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" }, edit)
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, bash, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err != nil || g == nil || g.Unit != "tool" || g.Tool != "Bash" || g.Exposure != 224+6 {
			t.Fatalf("a named tool over evidence of another tool before its history: %+v %v", g, err)
		}
	})
	t.Run("evidence the store no longer holds", func(t *testing.T) {
		st, path := testStoreAt(t)
		c := oneIncidentOf(t, st, now, now, [3]string{"Bash", serverB, serverC}, func(int) string { return "Bash" })
		db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if res, err := db.Exec(`DELETE FROM events WHERE source = ? AND source_key = 'f-0'`, toolSignal.Source); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("deleted %d events", n)
		}
		plan, err := (&Ship{Store: st}).validPlan(t.Context(), c, bash, 7*aDay, "", now)
		if g, _ := guardOf(t, plan); err == nil || !strings.Contains(err.Error(), "(1 tool in one incident alone; in 5 directories; of no MCP server") {
			t.Fatalf("a named tool over evidence the store lost: %+v %v", g, err)
		}
	})
}
