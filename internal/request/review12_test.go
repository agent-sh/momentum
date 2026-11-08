package request

// Tests from the twelfth review of the requests change. Synthetic data only.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A commit line the store's cut ends shows its commit, not a shortened subject; a line the cut does not end keeps
// its subject.
func TestACutSubjectIsLeftOut(t *testing.T) {
	for _, c := range []struct{ name, out, want string }{
		{"the cut ends the line", `{"output": "[main 1a2b3c4] Fix the parser for long inp…`, "commit commit 1a2b3c4 on main"},
		{"a later line is cut", `{"output": "[main 1a2b3c4] Fix the parser\n 1 file changed, 2 insert…`, "commit commit 1a2b3c4 on main: Fix the parser"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			s := "20261108_100000_cutsub"
			f.owner(s, at(0), "fix the parser")
			f.evs[len(f.evs)-1].Harness = "hermes"
			f.pairWithID(s, "", "hermes", "terminal", "call_s1", at(1), `{"command": "git commit -am 'Fix the parser for long inputs'"}`, c.out)
			if got := kinds(f.group()[0].Results); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	// a code-mode result's JSON the cut ends
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000c012"
	f.owner(s, at(0), "fix the parser")
	f.pairWithID(s, "", "codex", "exec", "call_s2", at(1), `const r = await tools.exec_command({cmd:"git commit -am 'Fix the parser for long inputs'"}); text(r)`,
		`Script completed Wall time 0.4 seconds Output: {"chunk_id":"a","exit_code":0,"output":"[main 1a2b3c4] Fix the parser for long inp…`)
	if got := kinds(f.group()[0].Results); got != "commit commit 1a2b3c4 on main" {
		t.Errorf("code mode: got %q", got)
	}
}

// The same ask recorded again soon after, with no reply between, is one turn: it moves none of the request's turn
// times, so the check stands.
func TestATurnRecordedTwiceReopensNothing(t *testing.T) {
	ctx := context.Background()
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	f := newFixture(t)
	f.owner("s1", sec(0), "fix the parser")
	f.tool("s1", "", "Bash", sec(10), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.phase("s1", sec(20), "phase", "idle", nil)
	rq := f.group()[0]
	if _, err := Check(ctx, f.st, rq, sec(40)); err != nil {
		t.Fatal(err)
	}
	f.owner("s1", sec(60), "fix the parser")
	f.phase("s1", sec(70), "phase", "idle", nil)
	got := f.group()[0]
	if got.State != Checked || !got.LastTurn.Equal(sec(0)) || !got.LastOwnerTurn.Equal(sec(0)) || !got.Threads[0].turn.Equal(sec(0)) {
		t.Errorf("state %s (%s), last turn %v, his %v, the thread's %v", got.State, got.Why, got.LastTurn, got.LastOwnerTurn, got.Threads[0].turn)
	}
}

// An id the owner's prose names is no handoff: only the handoff's opening line, or its last line at the end of
// the text, starts a segment.
func TestAnIDInHisProseIsNoHandoff(t *testing.T) {
	f := newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.owner("s1", at(5), "also see the momentum request reviewers1234 thread")
	if reqs := f.group(); len(reqs) != 1 || reqs[0].Asks[len(reqs[0].Asks)-1].Handoff != "" {
		t.Errorf("%d requests: %+v", len(reqs), reqs)
	}
	for _, c := range []struct{ name, text string }{
		{"its opening line, the store's cut", "%s - a handoff of the owner's request Asked (the owner's words, oldest first): - fix the par…"},
		{"its last line", "carry on ... %s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-src", at(0), "fix the parser")
			src := f.group()[0]
			f.owner("s-next", at(10), strings.Replace(c.text, "%s", Line(src.ID), 1))
			if reqs := f.group(); len(reqs) != 1 || len(reqs[0].Threads) != 2 {
				t.Errorf("the handoff did not link: %d requests", len(reqs))
			}
		})
	}
}
