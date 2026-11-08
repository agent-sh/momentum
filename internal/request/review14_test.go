package request

// Tests from the fourteenth review of the requests change. Synthetic data only. The first began as the review's own
// probe.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A handoff whose own body names its id (an ask, or the agent's reply that quotes it): pasted as typed or as the
// store keeps it (lines joined), the paste runs to its closing line, after its last sentence.
func TestAHandoffBodyNamingItsID(t *testing.T) {
	for _, where := range []string{"ask", "reply"} {
		for _, joined := range []bool{false, true} {
			f := newFixture(t)
			f.owner("s-src", at(0), "tune the cache")
			src := f.group()[0]
			if where == "ask" {
				f.owner("s-src", at(2), "keep going on "+Line(src.ID)+" and the docs")
			} else {
				f.owner("s-src", at(2), "keep going")
				f.reply("s-src", at(3), "Handed off as "+Line(src.ID)+" for the next session.")
			}
			src = f.group()[0]
			text := Handoff(src) + "now do the env overrides"
			if joined {
				text = strings.Join(strings.Fields(text), " ")
			}
			f.owner("s-next", at(10), text)
			var last Ask
			for _, rq := range f.group() {
				if rq.ID == src.ID {
					last = rq.Asks[len(rq.Asks)-1]
				}
			}
			if last.Handoff != src.ID || last.Words != "now do the env overrides" {
				t.Errorf("%s joined=%v: his words %.120q", where, joined, last.Words)
			}
		}
	}
}

// The closing id ends at a space or the end of the text: one glued to what follows is no closing line.
func TestAClosingIDEndsAtASpace(t *testing.T) {
	f := newFixture(t)
	f.owner("s-src", at(0), "tune the cache")
	src := f.group()[0]
	text := strings.Join(strings.Fields(Handoff(src)), " ") + "x now do the env overrides"
	f.owner("s-next", at(10), text)
	a := f.group()[0].Asks
	if w := a[len(a)-1].Words; w == "x now do the env overrides" || !strings.HasPrefix(w, "Asked") {
		t.Errorf("his words %.120q", w)
	}
}

// A gh command inside a substitution that is another command's argument prints into that argument, not the
// output: it blocks no pull request. echo and printf print theirs.
func TestASubstitutionInAnArgumentPrintsNothing(t *testing.T) {
	for _, c := range []struct{ name, command, want string }{
		{"a title", `gh pr create --base main --head x --title "$(gh pr view 9 --json title -q .title)" --body y`, "pr pull request https://github.com/o/r/pull/9"},
		{"a review's number", `gh pr review $(gh pr list --json number -q '.[0].number') --approve && gh pr create --fill`, "pr pull request https://github.com/o/r/pull/9"},
		{"echo prints it", `echo "$(gh pr view 9 --json url -q .url)" && gh pr create --fill`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-arg", at(0), "open the pull request")
			f.tool("s-arg", "", "Bash", at(1), `{"command":`+jsonString(c.command)+`}`, "https://github.com/o/r/pull/9\n", false)
			if got := kinds(f.groupWith(Options{Redact: fakeRedact})[0].Results); got != c.want {
				t.Errorf("got %q", got)
			}
		})
	}
}

// A request ready only because the agent replied after the last turn says it rests on that reply alone.
func TestReadyOnAReplyAloneSaysSo(t *testing.T) {
	f := newFixture(t)
	f.owner("s-r", at(0), "why is the build slow")
	f.reply("s-r", at(1), "The cache is cold.")
	f.phase("s-r", at(2), "phase", "idle", nil)
	if rq := f.group()[0]; rq.State != Ready || !strings.Contains(rq.Why, "rests on that reply alone") {
		t.Errorf("state %s (%s)", rq.State, rq.Why)
	}
	f = newFixture(t)
	f.owner("s-r", at(0), "fix the parser")
	f.tool("s-r", "", "Bash", at(1), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.reply("s-r", at(2), "Fixed.")
	f.phase("s-r", at(3), "phase", "idle", nil)
	if rq := f.group()[0]; rq.State != Ready || rq.Why != "a result came after the last turn" {
		t.Errorf("state %s (%s)", rq.State, rq.Why)
	}
}

// A Hermes call that names no git or gh (the store's read takes it for the gh inside "high") is not read from
// Hermes's store, though the store cut its output.
func TestAHermesCallThatMakesNothingIsNotRead(t *testing.T) {
	f := newFixture(t)
	s := "20261108_100000_nohermes"
	f.owner(s, at(0), "build the docs")
	db := filepath.Join(t.TempDir(), "state.db")
	f.cutPair(s, "hermes", "terminal", "call_h1", at(1), `{"command": "cd /work/high-level && make docs"}`,
		`{"output": "`+strings.Repeat("building page ", 30)+`", "exit_code": 0}`, db+"@3#0", db+"@4#0")
	f.put()
	r := newReader(f.st, Options{Redact: fakeRedact})
	defer r.close()
	r.enqueue(s)
	if _, err := r.settle(context.Background(), t0.Add(-time.Hour), func(rq *Request) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if len(r.hermes) != 0 {
		t.Errorf("Hermes's store was read for it: %v", r.hermes)
	}
}

// A code-mode script whose poll's output the store cut is read whole: the ref lines past the cut are the polled
// command's.
func TestACutPollInAScriptIsReadWhole(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000e001"
	f.owner(s, at(0), "push the branch")
	f.pairWithID(s, "", "codex", "exec_command", "call_q1", at(1), `{"cmd":"git push origin feat","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, "", "codex", "exec", "call_q2", at(2), `text(await tools.write_stdin({session_id:42,chars:""}))`,
		`Script completed Wall time 3.0 seconds Output: {"chunk_id":"b","exit_code":0,"output":"`+strings.Repeat("remote: resolving deltas\n", 12)+
			`To github.com:o/r.git\n   1111111..2222222  feat -> feat\n"}`)
	if got := kinds(f.group()[0].Results); got != "push pushed feat -> feat (1111111..2222222) to github.com:o/r.git" {
		t.Errorf("got %q", got)
	}
}

// A code-mode result's JSON keeps its lines in its escapes: it is not joined, so with its transcript gone it is
// read as stored and no detail counts as cut.
func TestCodeModeJSONIsNotJoined(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000e002"
	f.owner(s, at(0), "fix the parser")
	gone := filepath.Join(t.TempDir(), "rollout-gone.jsonl")
	f.cutPair(s, "codex", "exec", "call_j1", at(1), `text(await tools.exec_command({cmd:"git commit -am Fix"}))`,
		`Script completed Wall time 0.2 seconds Output: {"chunk_id":"a","exit_code":0,"output":"[main 1a2b3c4] Fix\n 1 file changed\n"}`, gone+"@0#0", gone+"@100#0")
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); got != "commit commit 1a2b3c4 on main: Fix" || rq.CutGone != 0 {
		t.Errorf("got %q, cut and gone %d", got, rq.CutGone)
	}
}

// A code-mode call is found from its "tools." and the assignment before it, as a JavaScript reader would.
func TestCodeCallsAreFoundFromTheirTools(t *testing.T) {
	for _, c := range []struct {
		script string
		want   string // each call's tool, the variable its result is set to, and where its statement starts
	}{
		{`const r = await tools.exec_command({cmd:"x"})`, "exec_command=r@0"},
		{`let  $a=await  tools.write_stdin({session_id:1})`, "write_stdin=$a@0"},
		{`r = tools.exec_command({cmd:"x"}); s = await tools.exec_command({cmd:"y"})`, "exec_command=r@0 exec_command=s@35"},
		{`if (x == tools.exec_command({cmd:"x"})) {}`, "exec_command=@9"},
		{`const f = () => tools.exec_command({cmd:"x"})`, "exec_command=@16"},
		{`mytools.exec_command({cmd:"x"}); a.tools.exec_command({cmd:"y"})`, "exec_command=@35"},
		{`const awaited = tools.exec_command({cmd:"x"})`, "exec_command=awaited@0"},
		{`x = tools.exec_command({cmd:"a"})tools.exec_command({cmd:"b"})`, "exec_command=x@0 exec_command=@33"},
		{`tools.exec_commandx({}); tools.view_image({})`, ""},
	} {
		var got []string
		for _, l := range codeCallLocs(c.script) {
			name := ""
			if l[2] >= 0 {
				name = c.script[l[2]:l[3]]
			}
			got = append(got, fmt.Sprintf("%s=%s@%d", c.script[l[4]:l[5]], name, l[0]))
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s: got %q, want %q", c.script, g, c.want)
		}
	}
}
