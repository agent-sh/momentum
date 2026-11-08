package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/request"
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
		Words  string `json:"words"`
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

// his is the owner's turn checkable stores, and the proof that relays it whole.
const his = "Looks right to me. The rename is good"

// checked records his check on his own turn, as an agent relays it.
func checked(t *testing.T) (*checkEvents, store.Event) {
	t.Helper()
	c, turn := checkable(t)
	if code, out, errOut := momentum("request", "check", "s-cli", "--proof", his); code != 0 || !strings.Contains(out, "ev:"+turn.ID()) {
		t.Fatalf("check: exit %d %q %s", code, out, errOut)
	}
	return c, turn
}

// handoffText is a handoff as the store keeps a pasted one: its lines joined.
func handoffText(t *testing.T, id string) string {
	return strings.Join(strings.Fields(handoffOf(t, id)), " ")
}

// An agent's check with no proof, or one too short to be a verdict of his, is refused and records nothing.
func TestACheckWithNoProofIsRefused(t *testing.T) {
	checkable(t)
	for _, args := range [][]string{
		{"request", "check", "s-cli"},
		{"request", "check", "s-cli", "--proof", ""},
		{"request", "check", "s-cli", "--proof", "  "},
	} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "not recorded") || !strings.Contains(errOut, "no proof") ||
			!strings.Contains(errOut, "own turn where he says he checked it") || !strings.Contains(errOut, "an agent's word alone records nothing") {
			t.Errorf("%q: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := momentum("request", "check", "s-cli", "--proof", "rename is"); code != 2 || !strings.Contains(errOut, "fewer than 3 words") {
		t.Errorf("two words: exit %d %s", code, errOut)
	}
	if got := listOne(t); got.State == "checked" || got.Checked != nil {
		t.Fatalf("a refused check was recorded: %+v", got)
	}
}

// A proof must be a whole turn of his, after the result the check binds, that asks nothing and names no check
// command or request id. A fragment, a program's turn, a harness command, a turn from before the result, a
// question, an instruction, the body of a handoff he pasted, and words no turn is are each refused with why, and
// record nothing.
func TestAProofOutsideHisVerdictIsRefused(t *testing.T) {
	c, _ := checkable(t)
	id := listOne(t).ID
	h := handoffText(t, id)
	opening := request.Line(id) + " - a handoff of the owner request"
	other := request.Line(request.ID(c.at(0).Add(-time.Hour), "an older ask"))
	// momentum's own message, and the same words typed into two sessions 300 ms apart (a program sent them)
	c.owner("s-cli-0001", c.at(6), supervise.Marker+" the owner said ship the renamed flag now")
	twin := c.at(7)
	c.owner("s-cli-0001", twin, "please merge the renamed flag today")
	c.owner("s-other-0001", twin.Add(300*time.Millisecond), "please merge the renamed flag today")
	c.owner("s-cli-0001", c.at(8), "/compact keep the rename notes")
	c.owner("s-cli-0001", c.at(9), "is the rename pushed to origin yet?")
	c.owner("s-cli-0001", c.at(10), "check request "+id+" and tell me where it stands")
	c.owner("s-cli-0001", c.at(11), "run momentum request check on the rename")
	// his pastes of the handoff: its opening line and body with no closing line, its body and closing line with no
	// opening line, and two handoffs in one turn, the second after the first's closing line
	c.owner("s-cli-0001", c.at(12), opening+" the flag rename is shipped and fine")
	c.owner("s-cli-0001", c.at(13), "the flag rename is done and fine. "+request.Line(id)[:0]+"To continue it in another session, paste all of this there: its first and last lines link that session to the request. "+request.Line(id))
	c.owner("s-cli-0001", c.at(14), h+" "+other+" - a handoff of the owner request the docs rename is fine too. To continue it in another session, paste all of this there: its first and last lines link that session to the request. "+other)
	c.idle("s-cli-0001", c.at(15))
	for _, row := range []struct{ proof, why string }{
		{"rename the flag to --dry-run", "is not after the result a check binds"},
		{supervise.Marker + " the owner said ship the renamed flag now", "only a turn a program typed is these words"},
		{"please merge the renamed flag today", "only a turn a program typed is these words"},
		{"/compact keep the rename notes", "only a harness command of his is these words"},
		{"is the rename pushed to origin yet?", "it asks a question"},
		{"check request " + id + " and tell me where it stands", "names the check command or a request's id"},
		{"run momentum request check on the rename", "names the check command or a request's id"},
		{"The rename is good", "is these words whole"},                    // a fragment of his turn
		{"is the rename pushed", "is these words whole"},                  // a fragment of his question
		{"check request " + id, "is these words whole"},                   // a fragment of his instruction
		{"looks right to me. the rename is good", "is these words whole"}, // his words are case-sensitive
		{"the flag rename is shipped and fine", "is these words whole"},   // a handoff's body after its lone opening line
		{"the flag rename is done and fine.", "is these words whole"},     // a handoff's body before its lone closing line
		{"the docs rename is fine too.", "is these words whole"},          // the body of a second handoff in his turn
	} {
		code, _, errOut := momentum("request", "check", "s-cli", "--proof", row.proof)
		if code != 2 || !strings.Contains(errOut, "not recorded") || !strings.Contains(errOut, row.why) {
			t.Errorf("proof %q: exit %d %s", row.proof, code, errOut)
		}
	}
	// a proof no turn is says how far the store reaches, and never that he did not say it
	if _, _, errOut := momentum("request", "check", "s-cli", "--proof", "the rename is right"); !strings.Contains(errOut, "the store has read no transcripts yet") ||
		!strings.Contains(errOut, "retry after the next ingest") {
		t.Errorf("no turn: %s", errOut)
	}
	if got := listOne(t); got.State == "checked" || got.Checked != nil {
		t.Fatalf("a refused check was recorded: %+v", got)
	}
}

// A turn of his that he asked more after is refused: his later ask would reopen a check on it at once. His later
// verdict records it.
func TestACheckOnATurnHeAskedPastIsRefused(t *testing.T) {
	c, _ := checkable(t)
	c.owner("s-cli-0001", c.at(6), "fine, now update the changelog too")
	c.reply("s-cli-0001", c.at(7), "Updated the changelog.")
	later := c.owner("s-cli-0001", c.at(8), "The changelog reads right now as well")
	c.idle("s-cli-0001", c.at(9))
	if code, _, errOut := momentum("request", "check", "s-cli", "--proof", his); code != 2 || !strings.Contains(errOut, "but he asked more at") ||
		strings.Contains(errOut, "relay") {
		t.Fatalf("check on a turn he asked past: exit %d %s", code, errOut)
	}
	if code, out, errOut := momentum("request", "check", "s-cli", "--proof", "The changelog reads right now as well"); code != 0 ||
		!strings.Contains(out, "checked commit 4b5c6d7") || !strings.Contains(out, "ev:"+later.ID()) {
		t.Fatalf("check on his later verdict: exit %d %q %s", code, out, errOut)
	}
}

// His whole turn, relayed with its own whitespace as it comes, records the check of the request's newest commit
// (not the file written after it, not the older commit), with that turn's event as its proof and the words relayed;
// list and handoff show it checked, with the turn's time and the words.
func TestAProofOfHisWholeTurnRecordsTheCheck(t *testing.T) {
	_, turn := checkable(t)
	before := listOne(t)
	if before.State == "checked" || !strings.HasPrefix(before.Newest, "file:/work/cli/NOTES.md@") { // the newest result is the file
		t.Fatalf("before the check: %+v", before)
	}
	// the flag after the id or before it; the relayed words' own whitespace collapsed as his turn's is
	code, out, errOut := momentum("request", "check", "--proof", "Looks right to me.\n\n   The   rename is good ", "s-cli")
	if code != 0 || !strings.Contains(out, "checked commit 4b5c6d7 on main: Rename the flag") || !strings.Contains(out, "ev:"+turn.ID()) ||
		!strings.Contains(out, `relayed as "`+his+`"`) {
		t.Fatalf("check: exit %d %q %s", code, out, errOut)
	}
	got := listOne(t)
	if got.State != "checked" || got.Checked == nil || got.Checked.Result != "commit:4b5c6d7" || got.Checked.Proof != "ev:"+turn.ID() ||
		got.Checked.At != turn.TS.Local().Format(time.RFC3339) || got.Checked.Words != his {
		t.Fatalf("list: %+v %+v", got, got.Checked)
	}
	when := turn.TS.Local().Format("2006-01-02 15:04")
	if _, text, _ := momentum("request", "list", "--since", "1d"); !strings.Contains(text, " checked ") ||
		!strings.Contains(text, "checked in his turn of "+when+`: "`+his+`"`) {
		t.Fatalf("list: %q", text)
	}
	h := handoffOf(t, got.ID)
	for _, want := range []string{"State: checked - the owner checked commit 4b5c6d7 on main: Rename the flag in his turn of " + when + `, relayed as "` + his + `"`,
		"Open step: none: the owner checked it", "ev:" + turn.ID()} {
		if !strings.Contains(h, want) {
			t.Fatalf("the handoff lacks %q:\n%s", want, h)
		}
	}
	if code, out, _ := momentum("request", "check", got.ID, "--proof", his); code != 0 || !strings.Contains(out, "checked already") {
		t.Fatalf("check again: exit %d %q", code, out)
	}
}

// Of two turns of his with the same words, the newest is the proof: the older one he asked past.
func TestTheNewestOfHisSameTurnsIsTheProof(t *testing.T) {
	c, _ := checkable(t)
	c.reply("s-cli-0001", c.at(5), "Anything else?")
	again := c.owner("s-cli-0001", c.at(6), "Looks right to me. The rename is good")
	c.idle("s-cli-0001", c.at(7))
	if code, out, errOut := momentum("request", "check", "s-cli", "--proof", his); code != 0 || !strings.Contains(out, "ev:"+again.ID()) {
		t.Fatalf("check: exit %d %q %s", code, out, errOut)
	}
}

// Uncheck takes the same proof: none, a program's turn, or his turn the check rests on (or one before it) is
// refused; a later turn of his undoes it, on that turn. A check again on the turn that undid it is refused.
func TestUncheckTakesTheSameProof(t *testing.T) {
	c, _ := checked(t)
	if code, _, errOut := momentum("request", "uncheck", "s-cli"); code != 2 || !strings.Contains(errOut, "no proof") {
		t.Fatalf("uncheck with no proof: exit %d %s", code, errOut)
	}
	c.owner("s-cli-0001", c.at(6), supervise.Marker+" undo the check of the rename")
	for _, row := range []struct{ proof, why string }{
		{his, "is not after his turn of"},
		{"rename the flag to --dry-run", "is not after his turn of"},
		{supervise.Marker + " undo the check of the rename", "only a turn a program typed"},
	} {
		if code, _, errOut := momentum("request", "uncheck", "s-cli", "--proof", row.proof); code != 2 || !strings.Contains(errOut, row.why) {
			t.Errorf("uncheck on %q: exit %d %s", row.proof, code, errOut)
		}
	}
	later := c.owner("s-cli-0001", c.at(7), "undo my check")
	c.idle("s-cli-0001", c.at(8))
	id := listOne(t).ID
	if h := handoffOf(t, id); !strings.Contains(h, "is reopened by an ask of the owner's after it") {
		t.Fatalf("his later turn does not reopen it:\n%s", h)
	}
	code, out, errOut := momentum("request", "uncheck", "s-cli", "--proof", "undo my check")
	if code != 0 || !strings.Contains(out, "undid the check") || !strings.Contains(out, "ev:"+later.ID()) || !strings.Contains(out, `relayed as "undo my check"`) {
		t.Fatalf("uncheck: exit %d %q %s", code, out, errOut)
	}
	if h := handoffOf(t, id); strings.Contains(h, "State: checked") || strings.Contains(h, "is reopened") {
		t.Fatalf("after the undo:\n%s", h)
	}
	if code, _, errOut := momentum("request", "check", "s-cli", "--proof", "undo my check"); code != 2 || !strings.Contains(errOut, "that undid his last check") {
		t.Fatalf("a check on the turn that undid it: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("request", "uncheck", "s-cli", "--proof", "undo my check"); code != 2 || !strings.Contains(errOut, "no check to undo") {
		t.Fatalf("uncheck with none standing: exit %d %s", code, errOut)
	}
}

// A request with no artifact binds the agent's newest reply before his turn, and a newer reply reopens nothing; a
// newer artifact does.
func TestACheckOfAReplyStands(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	c := &checkEvents{t: t, base: time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)}
	c.owner("s-q-0001", c.at(0), "why is the build slow")
	c.reply("s-q-0001", c.at(1), "The cache is cold.")
	c.owner("s-q-0001", c.at(2), "That explains it, thanks")
	c.reply("s-q-0001", c.at(3), "Glad it helps.")
	c.idle("s-q-0001", c.at(4))
	if code, out, errOut := momentum("request", "check", "s-q", "--proof", "That explains it, thanks"); code != 0 {
		t.Fatalf("check: exit %d %q %s", code, out, errOut)
	}
	reply := "the agent's reply of " + c.at(1).Local().Format("2006-01-02 15:04")
	if got := listOne(t); got.State != "checked" || !strings.Contains(handoffOf(t, got.ID), "the owner checked "+reply) {
		t.Fatalf("the check binds the reply before his turn: %+v\n%s", got, handoffOf(t, got.ID))
	}
	c.reply("s-q-0001", c.at(5), "Also the linker is slow.")
	c.idle("s-q-0001", c.at(6))
	if got := listOne(t); got.State != "checked" {
		t.Fatalf("a newer reply reopened it: %+v", got)
	}
	c.write("s-q-0001", c.at(7), "/work/cli/LINKER.md")
	c.idle("s-q-0001", c.at(8))
	if got := listOne(t); got.State != "ready" || !strings.Contains(handoffOf(t, got.ID), "is reopened by a newer result, /work/cli/LINKER.md") {
		t.Fatalf("a newer artifact: %+v\n%s", got, handoffOf(t, got.ID))
	}
}

// Only his own later ask, or a newer artifact a check would bind over the checked one, reopens his check: a brief
// or a program's turn that pastes the handoff, his paste with no words of his (whole, cut at either end, or two in
// one turn), a harness command, a reply or a file written after a commit do not.
func TestOnlyHisLaterAskReopensTheCheck(t *testing.T) {
	other := func(c *checkEvents) string { return request.Line(request.ID(c.at(0).Add(-time.Hour), "an older ask")) }
	const last = "To continue it in another session, paste all of this there: its first and last lines link that session to the request. "
	for _, row := range []struct {
		name  string
		add   func(c *checkEvents, id, handoff string)
		state string
		why   string
		joins string // the session the paste joins to the request
	}{
		{"his later ask", func(c *checkEvents, _, _ string) { c.owner("s-cli-0001", c.at(8), "now rename the env var as well") },
			"open", "is reopened by an ask of the owner's after it", ""},
		{"a newer commit", func(c *checkEvents, _, _ string) { c.commit("s-cli-0001", c.at(8), "5c6d7e8", "Rename the env var") },
			"ready", "is reopened by a newer result, commit 5c6d7e8", ""},
		{"a brief that pastes the handoff", func(c *checkEvents, _, h string) {
			c.put(c.ev("s-brief-0001", "agent_text", store.ActorAgent, c.at(8), h, `{"why":"agent cwd"}`))
			c.idle("s-brief-0001", c.at(9))
		}, "checked", "", "s-brief-0001"},
		{"a program's turn that pastes the handoff", func(c *checkEvents, _, h string) {
			c.owner("s-prog-0001", c.at(8), supervise.Marker+" "+h)
			c.idle("s-prog-0001", c.at(9))
		}, "checked", "", "s-prog-0001"},
		{"his paste of the handoff with no words of his", func(c *checkEvents, _, h string) {
			c.owner("s-next-0001", c.at(8), h)
			c.idle("s-next-0001", c.at(9))
		}, "checked", "", "s-next-0001"},
		{"his paste of its opening line and body, no closing line", func(c *checkEvents, id, _ string) {
			c.owner("s-part-0001", c.at(8), request.Line(id)+" - a handoff of the owner request Asked: rename the flag and ship it")
			c.idle("s-part-0001", c.at(9))
		}, "checked", "", "s-part-0001"},
		{"his paste of its body and closing line, no opening line", func(c *checkEvents, id, _ string) {
			c.owner("s-tail-0001", c.at(8), "Asked: rename the flag and ship it. "+last+request.Line(id))
			c.idle("s-tail-0001", c.at(9))
		}, "checked", "", "s-tail-0001"},
		{"two handoffs in one turn", func(c *checkEvents, _, h string) {
			o := other(c)
			c.owner("s-two-0001", c.at(8), h+" "+o+" - a handoff of the owner request Asked: rename the docs too. "+last+o)
			c.idle("s-two-0001", c.at(9))
		}, "checked", "", "s-two-0001"},
		{"a harness command", func(c *checkEvents, _, _ string) { c.owner("s-cli-0001", c.at(8), "/compact") }, "checked", "", ""},
		{"a harness command with words", func(c *checkEvents, _, _ string) { c.owner("s-cli-0001", c.at(8), "/goal finish the rename") }, "checked", "", ""},
		{"a reply", func(c *checkEvents, _, _ string) { c.reply("s-cli-0001", c.at(8), "Anything else?") }, "checked", "", ""},
		{"a file written after it", func(c *checkEvents, _, _ string) { c.write("s-cli-0001", c.at(8), "/work/cli/CHANGES.md") }, "checked", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := checked(t)
			id := listOne(t).ID
			row.add(c, id, handoffText(t, id))
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

// A check reads what is new in the transcripts first, as the ingest timer does: his turn the store did not hold yet
// proves it. Words no turn is say how far the store reaches.
func TestACheckReadsHisNewestTurnInFirst(t *testing.T) {
	if _, err := exec.LookPath("scrump"); err != nil {
		t.Skip("needs scrump with its text profile")
	}
	home, cfg, user := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_HOME", home)
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", user) // the reader reads the owner's prompt history here
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	const session = "7a1b2c3d-0000-4000-8000-00000000c1a3"
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	line := func(after time.Duration, fields string) string {
		return `{` + fields + `,"timestamp":"` + base.Add(after).Format("2006-01-02T15:04:05.000Z") + `","entrypoint":"cli","cwd":"/home/u/shop","sessionId":"` + session + `"}` + "\n"
	}
	path := filepath.Join(user, ".claude", "projects", "-home-u-shop", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(line(0, `"type":"user","uuid":"u0","promptSource":"typed","origin":{"kind":"human"},"message":{"role":"user","content":"why is the build slow"}`)+
		line(5*time.Second, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"The cache is cold."}]}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(`{"transcripts": {"homes": ["`+user+`"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := momentum("evidence", "ingest"); code != 0 {
		t.Fatalf("ingest: exit %d %s %s", code, out, errOut)
	}
	// his verdict, after the last ingest
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line(time.Minute, `"type":"user","uuid":"u2","promptSource":"typed","origin":{"kind":"human"},"message":{"role":"user","content":"That explains it, thanks"}`)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if code, _, errOut := momentum("request", "check", "7a1b2c3d", "--proof", "the build is fast now"); code != 2 ||
		!strings.Contains(errOut, "the store holds the transcripts as read up to") || !strings.Contains(errOut, "retry after the next ingest") {
		t.Fatalf("words no turn is: exit %d %s", code, errOut)
	}
	if code, out, errOut := momentum("request", "check", "7a1b2c3d", "--proof", "That explains it, thanks"); code != 0 || !strings.Contains(out, "checked ") {
		t.Fatalf("check on his turn the store did not hold: exit %d %q %s", code, out, errOut)
	}
}
