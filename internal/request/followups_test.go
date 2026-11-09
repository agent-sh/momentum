package request

// Tests for the follow-ups of the requests change (#194): a copy across the window's edge, and rules no other test
// failed without. Synthetic data only.

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// recordedAgain is a thread of a harness that records a call again: a commit, the agent's reply, a later turn,
// and the commit's copy with its call at copyAt, its result a second later.
func recordedAgain(t *testing.T, harness string, copyAt time.Time) *fixture {
	f := newFixture(t)
	s := "20261107_100000_again"
	commit, out := `{"command": "git add -A && git commit -m docs"}`, `{"output": "[main 1a2b3c4] docs\n 1 file changed", "exit_code": 0, "error": null}`
	if harness == "codex" {
		s = "019a0000-0000-7000-8000-00000000f301"
		commit, out = `{"cmd":"git add -A && git commit -m docs"}`, "Chunk ID: a\nWall time: 0.2 seconds\nProcess exited with code 0\nOutput:\n[main 1a2b3c4] docs\n 1 file changed\n"
	}
	tool := map[string]string{"hermes": "terminal", "codex": "exec_command"}[harness]
	f.owner(s, at(0), "commit the docs")
	f.evs[len(f.evs)-1].Harness = harness
	f.pairWithID(s, "", harness, tool, "call_a1", at(1), commit, out)
	f.reply(s, at(2), "Committed.")
	f.owner(s, at(5), "anything else left?")
	f.evs[len(f.evs)-1].Harness = harness
	f.pairWithID(s, "", harness, tool, "call_a1", copyAt, commit, out)
	f.phase(s, at(12), "phase", "idle", nil)
	return f
}

// A Codex subagent forked with its parent's context records its calls again: the copy in the window made nothing
// more, as Hermes's does.
func TestACodexCallRecordedAgainInTheWindow(t *testing.T) {
	sameState(t, recordedAgain(t, "codex", at(10)), at(3))
}

// A copy whose call is just before the window and whose result is in it made nothing more either.
func TestACopyWhoseCallIsBeforeTheWindow(t *testing.T) {
	for _, harness := range []string{"hermes", "codex"} {
		t.Run(harness, func(t *testing.T) {
			sameState(t, recordedAgain(t, harness, at(10)), at(10).Add(500*time.Millisecond))
		})
	}
}

// The owner's words recorded twice in one session a second apart are his turn, recorded twice: a turn's twin is
// in another session, so his own session's record is no program's.
func TestHisTurnRecordedTwiceInItsSessionIsNoTwin(t *testing.T) {
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	f := newFixture(t)
	f.owner("s-twice", sec(0), "fix the parser")
	f.owner("s-twice", sec(1), "fix the parser")
	f.tool("s-twice", "", "Bash", sec(30), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	reqs := f.group()
	if len(reqs) != 1 || !reqs[0].Opened.Equal(sec(0)) || words(reqs[0].Asks) != "fix the parser" {
		t.Fatalf("%d requests: %+v", len(reqs), reqs)
	}
}

// A session whose events name it as their agent (a subagent's whose parent the store does not know) is its own
// main thread: its turn opens a request, its reply is the request's report, its directory the thread's.
func TestASessionThatIsItsOwnAgentIsItsMainThread(t *testing.T) {
	f := newFixture(t)
	s := "s-own"
	f.event(s, s, "owner_text", store.ActorOwner, at(0), "write the release notes", map[string]any{"input_kind": "typed"})
	f.tool(s, s, "Bash", at(1), `{"command":"git commit -am Notes"}`, "[main 2b3c4d5] Notes\n 1 file changed", false)
	f.event(s, s, "agent_text", store.ActorAgent, at(2), "The notes are in.", nil)
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d", len(reqs))
	}
	rq := reqs[0]
	if words(rq.Asks) != "write the release notes" || rq.Report == nil || rq.Report.Text != "The notes are in." ||
		!strings.Contains(kinds(rq.Results), "commit 2b3c4d5") || len(rq.Threads) != 1 || rq.Threads[0].Cwd != "/work/"+s {
		t.Fatalf("asks %q, report %+v, results %q, threads %+v", words(rq.Asks), rq.Report, kinds(rq.Results), rq.Threads)
	}
}

// A result and a reply at the last turn's own time came with it, not after it: nothing came after the turn.
func TestAResultAtTheLastTurnsTimeIsNotAfterIt(t *testing.T) {
	f := newFixture(t)
	f.owner("s-same", at(0), "fix the parser")
	f.tool("s-same", "", "Bash", at(1), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.reply("s-same", at(1).Add(time.Second), "Fixed.")
	f.owner("s-same", at(1).Add(time.Second), "now the docs")
	f.phase("s-same", at(5), "phase", "idle", nil)
	if rq := f.group()[0]; rq.State != Open {
		t.Fatalf("state %s (%s)", rq.State, rq.Why)
	}
}
