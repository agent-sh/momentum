package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// putRequestEvents stores a synthetic session: the owner's turn, a commit and the agent's reply, and a sessions run
// that last saw it idle.
func putRequestEvents(t *testing.T, base time.Time) {
	t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := func(n int, kind string, actor store.Actor, at time.Time, summary, meta string) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("s-cli.jsonl@%012d#000", n), TS: at, Session: "s-cli-0001",
			Harness: "claude", Cwd: "/work/cli", Actor: actor, Kind: kind, Summary: summary, Meta: meta}
	}
	shell := func(e store.Event) store.Event { e.Tool = "Bash"; return e }
	evs := []store.Event{
		ev(1, "owner_text", store.ActorOwner, base, "rename the flag", `{"input_kind":"typed"}`),
		shell(ev(2, "tool_call", store.ActorAgent, base.Add(time.Minute), `{"command":"git commit -am rename"}`, `{"tool_id":"c1"}`)),
		shell(ev(3, "tool_result", store.ActorTool, base.Add(time.Minute+time.Second), "[main 4b5c6d7] Rename the flag\n 1 file changed", `{"tool_id":"c1"}`)),
		ev(4, "agent_text", store.ActorAgent, base.Add(2*time.Minute), "Renamed, and the tests pass.", ""),
		{Source: store.Sessions, SourceKey: "s-cli-0001@1", TS: base.Add(3 * time.Minute), Session: "s-cli-0001", Harness: "claude",
			Actor: store.ActorHarness, Kind: "phase", Meta: `{"from":"working","to":"idle"}`},
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
}

// From a session's id to a handoff and the list, end to end.
func TestRequestHandoffAndList(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	putRequestEvents(t, time.Now().Add(-time.Hour).Truncate(time.Millisecond))

	code, out, errOut := momentum("request", "handoff", "s-cli")
	if code != 0 {
		t.Fatalf("handoff: exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	first := strings.Fields(lines[0])
	if len(first) < 3 || first[0] != "momentum" || first[1] != "request" || lines[len(lines)-1] != "momentum request "+first[2] {
		t.Fatalf("the id lines: %q ... %q", lines[0], lines[len(lines)-1])
	}
	id := first[2]
	for _, want := range []string{"rename the flag", "commit 4b5c6d7 on main: Rename the flag", "Claimed (the agent's last reply",
		"Renamed, and the tests pass.", "State: ready", "s-cli-0001 in /work/cli"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the handoff lacks %q:\n%s", want, out)
		}
	}
	// the request's own id finds the same request
	if code, again, _ := momentum("request", "handoff", id); code != 0 || again != out {
		t.Fatalf("by its id: exit %d\n%s", code, again)
	}

	code, out, errOut = momentum("request", "list", "--since", "1d", "--json")
	var list struct {
		Requests []requestJSON `json:"requests"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list.Requests) != 1 || list.Requests[0].ID != id ||
		list.Requests[0].State != "ready" || list.Requests[0].Newest != "commit:4b5c6d7" {
		t.Fatalf("list: exit %d %q %s", code, out, errOut)
	}
	if code, out, _ := momentum("request", "list", "--since", "1d"); code != 0 || len(strings.Fields(out)) < 2 ||
		strings.Fields(out)[0] != id || strings.Fields(out)[1] != "ready" {
		t.Fatalf("list: exit %d %q", code, out)
	}
	for _, gone := range []string{"check", "uncheck"} {
		if code, _, errOut := momentum("request", gone, id); code != 2 || !strings.Contains(errOut, "unknown request command") {
			t.Fatalf("request %s: exit %d %s", gone, code, errOut)
		}
	}
	if code, _, errOut := momentum("request", "handoff", "no-such-session"); code != 2 || !strings.Contains(errOut, "neither a request's id nor a session's") {
		t.Fatalf("an unknown id: exit %d %s", code, errOut)
	}
}

// A request whose last turn and commit are before the window, and that only a read touched since, is ready by the
// commit the list reads back for its state: the commit counts in no window it is not in.
func TestResultsInWindowCountsTheWindowsOnly(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	ev := func(n int, kind string, actor store.Actor, at time.Time, tool, summary, meta string) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("s-win.jsonl@%012d#000", n), TS: at, Session: "s-win-0001",
			Harness: "claude", Cwd: "/work/win", Actor: actor, Kind: kind, Tool: tool, Summary: summary, Meta: meta}
	}
	evs := []store.Event{
		ev(1, "owner_text", store.ActorOwner, base, "", "rename the flag", `{"input_kind":"typed"}`),
		ev(2, "tool_call", store.ActorAgent, base.Add(time.Minute), "Bash", `{"command":"git commit -am rename"}`, `{"tool_id":"c1"}`),
		ev(3, "tool_result", store.ActorTool, base.Add(time.Minute+time.Second), "Bash", "[main 4b5c6d7] Rename the flag\n 1 file changed", `{"tool_id":"c1"}`),
		ev(4, "tool_call", store.ActorAgent, base.Add(40*time.Minute), "Read", `{"file_path":"/work/win/flag.go"}`, `{"tool_id":"c2"}`),
		{Source: store.Sessions, SourceKey: "s-win-0001@1", TS: base.Add(41 * time.Minute), Session: "s-win-0001", Harness: "claude",
			Actor: store.ActorHarness, Kind: "phase", Meta: `{"from":"working","to":"idle"}`},
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, c := range []struct {
		since string
		n     int
	}{{base.Add(30 * time.Minute).Format(time.RFC3339), 0}, {"1d", 1}} {
		code, out, errOut := momentum("request", "list", "--since", c.since, "--json")
		var list struct {
			Requests []requestJSON `json:"requests"`
		}
		if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list.Requests) != 1 || list.Requests[0].State != "ready" ||
			list.Requests[0].ResultsInWindow != c.n {
			t.Errorf("since %s: exit %d %q %s", c.since, code, out, errOut)
		}
	}
}

// Every command line a handoff prints runs as printed: a shell (bash) parses each into its words, and its id lines
// are its handoff, its evidence lines evidence show commands the CLI takes.
func TestEveryHandoffCommandLineRuns(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	putRequestEvents(t, time.Now().Add(-time.Hour).Truncate(time.Millisecond))
	code, out, errOut := momentum("request", "handoff", "s-cli")
	if code != 0 {
		t.Fatalf("handoff: exit %d: %s", code, errOut)
	}
	ran := 0
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "momentum ") {
			continue
		}
		words, err := exec.Command("bash", "-c", "set -- "+l+"; printf '%s\\0' \"$@\"").Output()
		if err != nil {
			t.Errorf("a shell cannot run %q: %v", l, err)
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(words), "\x00"), "\x00")[1:]
		code, got, errOut := momentum(args...)
		if code == 2 {
			t.Errorf("%q: a usage error: %s", l, errOut)
		}
		if args[0] == "request" && (code != 0 || got != out) {
			t.Errorf("%q: exit %d, not the handoff:\n%s", l, code, got)
		}
		ran++
	}
	if ran < 3 {
		t.Errorf("%d command lines in:\n%s", ran, out)
	}
}
