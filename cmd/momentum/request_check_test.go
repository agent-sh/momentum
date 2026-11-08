package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
)

// Synthetic sessions only: every turn, commit and reply here is made up.

// checkEvents builds a store's transcript events for the check tests, a minute apart from base.
type checkEvents struct {
	t    *testing.T
	base time.Time
	n    int
}

func (c *checkEvents) at(min int) time.Time { return c.base.Add(time.Duration(min) * time.Minute) }

func (c *checkEvents) put(evs ...store.Event) {
	c.t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		c.t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Put(context.Background(), evs); err != nil {
		c.t.Fatal(err)
	}
}

func (c *checkEvents) ev(session, kind string, actor store.Actor, at time.Time, summary, meta string) store.Event {
	c.n++
	return store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s.jsonl@%012d#000", session, c.n), TS: at, Session: session,
		Harness: "claude", Cwd: "/work/cli", Actor: actor, Kind: kind, Summary: summary, Meta: meta}
}

// owner stores a turn of the owner's and returns it.
func (c *checkEvents) owner(session string, at time.Time, words string) store.Event {
	e := c.ev(session, "owner_text", store.ActorOwner, at, words, `{"input_kind":"typed"}`)
	c.put(e)
	return e
}

func (c *checkEvents) commit(session string, at time.Time, hash, subject string) {
	id := fmt.Sprintf("c%d", c.n)
	call := c.ev(session, "tool_call", store.ActorAgent, at, `{"command":"git commit -am x"}`, `{"tool_id":"`+id+`"}`)
	res := c.ev(session, "tool_result", store.ActorTool, at.Add(time.Second), "["+"main "+hash+"] "+subject+"\n 1 file changed", `{"tool_id":"`+id+`"}`)
	call.Tool, res.Tool = "Bash", "Bash"
	c.put(call, res)
}

func (c *checkEvents) write(session string, at time.Time, path string) {
	id := fmt.Sprintf("w%d", c.n)
	call := c.ev(session, "tool_call", store.ActorAgent, at, `{"file_path":"`+path+`","content":"x"}`, `{"tool_id":"`+id+`"}`)
	res := c.ev(session, "tool_result", store.ActorTool, at.Add(time.Second), "File created.", `{"tool_id":"`+id+`"}`)
	call.Tool, res.Tool = "Write", "Write"
	c.put(call, res)
}

func (c *checkEvents) reply(session string, at time.Time, text string) {
	c.put(c.ev(session, "agent_text", store.ActorAgent, at, text, ""))
}

func (c *checkEvents) idle(session string, at time.Time) {
	c.n++
	c.put(store.Event{Source: store.Sessions, SourceKey: fmt.Sprintf("%s@%d", session, c.n), TS: at, Session: session, Harness: "claude",
		Actor: store.ActorHarness, Kind: "phase", Meta: `{"from":"working","to":"idle"}`})
}

// listedRequest is one request of `request list --json`, read with no type of this build, so the tests build on any.
type listedRequest struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Newest  string `json:"newest"`
	Checked *struct {
		Result string `json:"result"`
		At     string `json:"at"`
		Proof  string `json:"proof"`
	} `json:"checked"`
}

func listOne(t *testing.T) listedRequest {
	t.Helper()
	code, out, errOut := momentum("request", "list", "--since", "1d", "--json")
	var list struct {
		Requests []listedRequest `json:"requests"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list.Requests) != 1 {
		t.Fatalf("list: exit %d %q %s", code, out, errOut)
	}
	return list.Requests[0]
}

func handoffOf(t *testing.T, id string) string {
	t.Helper()
	code, out, errOut := momentum("request", "handoff", id)
	if code != 0 {
		t.Fatalf("handoff: exit %d %s", code, errOut)
	}
	return out
}

// checkable stores a request the owner asked for, whose agent committed twice and then wrote a file, replied, and then
// the owner's turn that says he checked it, with a sessions run that last saw it idle. It returns that turn.
func checkable(t *testing.T) (*checkEvents, store.Event) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	c := &checkEvents{t: t, base: time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)}
	c.owner("s-cli-0001", c.at(0), "rename the flag to --dry-run")
	c.commit("s-cli-0001", c.at(0).Add(30*time.Second), "1a2b3c4", "Start the rename") // an older commit reopens nothing
	c.commit("s-cli-0001", c.at(1), "4b5c6d7", "Rename the flag")
	c.write("s-cli-0001", c.at(2), "/work/cli/NOTES.md")
	c.reply("s-cli-0001", c.at(3), "Renamed, and the tests pass.")
	his := c.owner("s-cli-0001", c.at(4), "Looks right to me.\n  The rename   is good")
	c.idle("s-cli-0001", c.at(5))
	return c, his
}

// checked records his check on his own words, as an agent relays them.
func checked(t *testing.T) (*checkEvents, store.Event) {
	t.Helper()
	c, his := checkable(t)
	if code, out, errOut := momentum("request", "check", "s-cli", "--proof", "The rename is good"); code != 0 || !strings.Contains(out, "ev:"+his.ID()) {
		t.Fatalf("check: exit %d %q %s", code, out, errOut)
	}
	return c, his
}

// An agent's check with no proof, or one too short to be his words, is refused and records nothing.
func TestACheckWithNoProofIsRefused(t *testing.T) {
	checkable(t)
	for _, args := range [][]string{
		{"request", "check", "s-cli"},
		{"request", "check", "s-cli", "--proof", ""},
		{"request", "check", "s-cli", "--proof", "  "},
	} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "not recorded") || !strings.Contains(errOut, "no proof") ||
			!strings.Contains(errOut, "an agent's word alone records nothing") {
			t.Errorf("%q: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := momentum("request", "check", "s-cli", "--proof", "rename is"); code != 2 || !strings.Contains(errOut, "at least 3") {
		t.Errorf("two words: exit %d %s", code, errOut)
	}
	if got := listOne(t); got.State == "checked" || got.Checked != nil {
		t.Fatalf("a refused check was recorded: %+v", got)
	}
}

// A proof that only a program's turn holds, or only a turn of his from before the result the check binds, or no
// turn at all, is refused with why, and records nothing.
func TestAProofOutsideHisLaterTurnsIsRefused(t *testing.T) {
	c, _ := checkable(t)
	// momentum's own message, and the same words typed into two sessions 300 ms apart (a program sent them)
	c.owner("s-cli-0001", c.at(6), supervise.Marker+" the owner said ship the renamed flag now")
	twin := c.at(7)
	c.owner("s-cli-0001", twin, "please merge the renamed flag today")
	c.owner("s-other-0001", twin.Add(300*time.Millisecond), "please merge the renamed flag today")
	c.owner("s-cli-0001", c.at(8), "/compact keep the rename notes")
	for _, row := range []struct{ proof, why string }{
		{"rename the flag", "came before the result a check binds"},
		{"ship the renamed flag", "only a turn a program typed holds these words"},
		{"merge the renamed flag", "only a turn a program typed holds these words"},
		{"keep the rename notes", "only a harness command of his holds these words"},
		{"the rename is good", "no turn of the owner's"}, // his words are case-sensitive
		{"rename is good, ship", "no turn of the owner's"},
		{"ename is good", "no turn of the owner's"}, // whole words only
	} {
		code, _, errOut := momentum("request", "check", "s-cli", "--proof", row.proof)
		if code != 2 || !strings.Contains(errOut, "not recorded") || !strings.Contains(errOut, row.why) {
			t.Errorf("proof %q: exit %d %s", row.proof, code, errOut)
		}
	}
	if got := listOne(t); got.State == "checked" || got.Checked != nil {
		t.Fatalf("a refused check was recorded: %+v", got)
	}
}

// Words of a turn of his that he asked more after are refused: his later ask would reopen a check on them at once.
// Words of the later turn record it.
func TestACheckOnWordsHeAskedPastIsRefused(t *testing.T) {
	c, _ := checkable(t)
	later := c.owner("s-cli-0001", c.at(6), "fine, now update the changelog too")
	c.idle("s-cli-0001", c.at(7))
	if code, _, errOut := momentum("request", "check", "s-cli", "--proof", "The rename is good"); code != 2 || !strings.Contains(errOut, "but he asked more at") {
		t.Fatalf("check on words he asked past: exit %d %s", code, errOut)
	}
	if got := listOne(t); got.State == "checked" {
		t.Fatalf("recorded: %+v", got)
	}
	if code, out, errOut := momentum("request", "check", "s-cli", "--proof", "now update the changelog"); code != 0 || !strings.Contains(out, "ev:"+later.ID()) {
		t.Fatalf("check on his later words: exit %d %q %s", code, out, errOut)
	}
}

// A proof his turn holds records the check of the request's newest commit (not the file written after it), with
// that turn's event as its proof; list and handoff show it checked, with the turn's time.
func TestAProofInHisTurnRecordsTheCheck(t *testing.T) {
	_, his := checkable(t)
	before := listOne(t)
	if before.State == "checked" || !strings.HasPrefix(before.Newest, "file:/work/cli/NOTES.md@") { // the newest result is the file
		t.Fatalf("before the check: %+v", before)
	}
	// the flag after the id or before it; his words with their whitespace collapsed
	code, out, errOut := momentum("request", "check", "--proof", "Looks right to me. The rename is good", "s-cli")
	if code != 0 || !strings.Contains(out, "checked commit 4b5c6d7 on main: Rename the flag") || !strings.Contains(out, "ev:"+his.ID()) {
		t.Fatalf("check: exit %d %q %s", code, out, errOut)
	}
	got := listOne(t)
	if got.State != "checked" || got.Checked == nil || got.Checked.Result != "commit:4b5c6d7" || got.Checked.Proof != "ev:"+his.ID() ||
		got.Checked.At != his.TS.Local().Format(time.RFC3339) {
		t.Fatalf("list: %+v %+v", got, got.Checked)
	}
	when := his.TS.Local().Format("2006-01-02 15:04")
	if _, text, _ := momentum("request", "list", "--since", "1d"); !strings.Contains(text, " checked ") || !strings.Contains(text, "checked in his turn of "+when) {
		t.Fatalf("list: %q", text)
	}
	h := handoffOf(t, got.ID)
	for _, want := range []string{"State: checked - the owner checked commit 4b5c6d7 on main: Rename the flag in his turn of " + when,
		"Open step: none: the owner checked it", "ev:" + his.ID()} {
		if !strings.Contains(h, want) {
			t.Fatalf("the handoff lacks %q:\n%s", want, h)
		}
	}
	if code, out, _ := momentum("request", "check", got.ID, "--proof", "The rename is good"); code != 0 || !strings.Contains(out, "checked already") {
		t.Fatalf("check again: exit %d %q", code, out)
	}
}

// Uncheck takes the same proof: none, a program's turn, or his turn the check rests on (or one before it) is
// refused; words of his from a later turn undo it, on that turn.
func TestUncheckTakesTheSameProof(t *testing.T) {
	c, _ := checked(t)
	if code, _, errOut := momentum("request", "uncheck", "s-cli"); code != 2 || !strings.Contains(errOut, "no proof") {
		t.Fatalf("uncheck with no proof: exit %d %s", code, errOut)
	}
	c.owner("s-cli-0001", c.at(6), supervise.Marker+" undo the check of the rename")
	for _, row := range []struct{ proof, why string }{
		{"The rename is good", "is not after his turn of"},
		{"rename the flag to", "is not after his turn of"},
		{"undo the check of", "only a turn a program typed"},
	} {
		if code, _, errOut := momentum("request", "uncheck", "s-cli", "--proof", row.proof); code != 2 || !strings.Contains(errOut, row.why) {
			t.Errorf("uncheck on %q: exit %d %s", row.proof, code, errOut)
		}
	}
	later := c.owner("s-cli-0001", c.at(7), "wait, the docs still say --dry; undo my check")
	c.idle("s-cli-0001", c.at(8))
	id := listOne(t).ID
	if h := handoffOf(t, id); !strings.Contains(h, "is reopened by an ask of the owner's after it") {
		t.Fatalf("his later turn does not reopen it:\n%s", h)
	}
	code, out, errOut := momentum("request", "uncheck", "s-cli", "--proof", "the docs still say --dry;")
	if code != 0 || !strings.Contains(out, "undid the check") || !strings.Contains(out, "ev:"+later.ID()) {
		t.Fatalf("uncheck: exit %d %q %s", code, out, errOut)
	}
	if h := handoffOf(t, id); strings.Contains(h, "State: checked") || strings.Contains(h, "is reopened") {
		t.Fatalf("after the undo:\n%s", h)
	}
	if code, _, errOut := momentum("request", "uncheck", "s-cli", "--proof", "the docs still say --dry;"); code != 2 || !strings.Contains(errOut, "no check to undo") {
		t.Fatalf("uncheck with none standing: exit %d %s", code, errOut)
	}
}

// Only his own later ask, or a newer commit, push or pull request, reopens his check: a brief or a program's turn
// that pastes the handoff, his paste with no words of his, a harness command, a reply or a file written after it do
// not.
func TestOnlyHisLaterAskReopensTheCheck(t *testing.T) {
	for _, row := range []struct {
		name  string
		add   func(c *checkEvents, handoff string)
		state string
		why   string
		joins string // the session the paste joins to the request
	}{
		{"his later ask", func(c *checkEvents, _ string) { c.owner("s-cli-0001", c.at(8), "now rename the env var as well") },
			"open", "is reopened by an ask of the owner's after it", ""},
		{"a newer commit", func(c *checkEvents, _ string) { c.commit("s-cli-0001", c.at(8), "5c6d7e8", "Rename the env var") },
			"ready", "is reopened by a newer result, commit 5c6d7e8", ""},
		{"a brief that pastes the handoff", func(c *checkEvents, h string) {
			c.put(c.ev("s-brief-0001", "agent_text", store.ActorAgent, c.at(8), h, `{"why":"agent cwd"}`))
			c.idle("s-brief-0001", c.at(9))
		}, "checked", "", "s-brief-0001"},
		{"a program's turn that pastes the handoff", func(c *checkEvents, h string) {
			c.owner("s-prog-0001", c.at(8), supervise.Marker+" "+h)
			c.idle("s-prog-0001", c.at(9))
		}, "checked", "", "s-prog-0001"},
		{"his paste of the handoff with no words of his", func(c *checkEvents, h string) {
			c.owner("s-next-0001", c.at(8), h)
			c.idle("s-next-0001", c.at(9))
		}, "checked", "", "s-next-0001"},
		{"a harness command", func(c *checkEvents, _ string) { c.owner("s-cli-0001", c.at(8), "/compact") }, "checked", "", ""},
		{"a reply", func(c *checkEvents, _ string) { c.reply("s-cli-0001", c.at(8), "Anything else?") }, "checked", "", ""},
		{"a file written after it", func(c *checkEvents, _ string) { c.write("s-cli-0001", c.at(8), "/work/cli/CHANGES.md") }, "checked", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := checked(t)
			id := listOne(t).ID
			row.add(c, handoffOf(t, id))
			c.idle("s-cli-0001", c.at(10))
			got := listOne(t)
			h := handoffOf(t, id)
			if got.ID != id || got.State != row.state || !strings.Contains(h, "State: "+row.state+" - ") || row.why != "" && !strings.Contains(h, row.why) ||
				row.joins != "" && !strings.Contains(h, "claude "+row.joins+" in ") {
				t.Fatalf("list %+v; handoff:\n%s", got, h)
			}
		})
	}
}
