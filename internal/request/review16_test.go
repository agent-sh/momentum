package request

// Tests from the sixteenth review of the requests change. Synthetic data only. The first three began as the
// review's own probes.

import (
	"context"
	"testing"
	"time"
)

// sameState lists the requests with events since a time and finds each by its id: list and handoff hold the same
// state.
func sameState(t *testing.T, f *fixture, since time.Time) {
	t.Helper()
	ctx := context.Background()
	f.put()
	list, err := List(ctx, f.st, since, Options{Redact: fakeRedact})
	if err != nil || len(list) == 0 {
		t.Fatalf("list: %d requests, %v", len(list), err)
	}
	for _, rq := range list {
		full, err := Find(ctx, f.st, rq.ID, Options{Redact: fakeRedact})
		if err != nil {
			t.Fatal(err)
		}
		if rq.State != full.State {
			t.Errorf("%s: list says %s (%s), handoff %s (%s)", rq.ID, rq.State, rq.Why, full.State, full.Why)
		}
	}
}

// A call made before the window whose result came in it is read with its result.
func TestACallAcrossTheWindowsEdge(t *testing.T) {
	f := newFixture(t)
	f.owner("s-st", at(0), "push the fix")
	f.tool("s-st", "", "Bash", at(10), `{"command":"git push origin main"}`, "To github.com:o/r.git\n   1111111..2222222  main -> main\n", false)
	f.phase("s-st", at(12), "phase", "idle", nil)
	sameState(t, f, at(10).Add(500*time.Millisecond))
}

// A Codex command left running before the window and polled in it is read against its command, the last turn
// before the window or in it.
func TestAPollInTheWindowOfACommandBeforeIt(t *testing.T) {
	for _, inside := range []bool{false, true} {
		f := newFixture(t)
		s := "019a0000-0000-7000-8000-00000000f101"
		f.owner(s, at(0), "push the fix")
		f.pairWithID(s, "", "codex", "exec_command", "call_q1", at(10), `{"cmd":"git push origin main","yield_time_ms":1000}`,
			"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\nrunning the pre-push checks\n")
		if inside {
			f.owner(s, at(25), "is it pushed yet?")
		}
		f.pairWithID(s, "", "codex", "write_stdin", "call_q2", at(30), `{"session_id":42,"chars":""}`,
			"Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nTo github.com:o/r.git\n   1111111..2222222  main -> main\n")
		f.phase(s, at(40), "phase", "idle", nil)
		sameState(t, f, at(20))
	}
}

// A Hermes call recorded again in the window, its first record before the last turn, made nothing more.
func TestACallRecordedAgainInTheWindow(t *testing.T) {
	f := newFixture(t)
	s := "20261107_100000_rewin"
	commit, out := `{"command": "git add -A && git commit -m docs"}`, `{"output": "[main 1a2b3c4] docs\n 1 file changed", "exit_code": 0, "error": null}`
	f.owner(s, at(0), "commit the docs")
	f.evs[len(f.evs)-1].Harness = "hermes"
	f.pairWithID(s, "", "hermes", "terminal", "call_w1", at(1), commit, out)
	f.reply(s, at(2), "Committed.")
	f.owner(s, at(5), "anything else left?")
	f.evs[len(f.evs)-1].Harness = "hermes"
	f.pairWithID(s, "", "hermes", "terminal", "call_w1", at(10), commit, out)
	f.phase(s, at(12), "phase", "idle", nil)
	sameState(t, f, at(3))
}

// A repeat is decided against the turn just before it: the third "go on", after the agent's commit, is a turn,
// though the second was a repeat of the first.
func TestAThirdRepeatAfterWorkIsATurn(t *testing.T) {
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	f := newFixture(t)
	f.owner("s-rep", sec(0), "go on")
	f.owner("s-rep", sec(30), "go on")
	f.tool("s-rep", "", "Bash", sec(40), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.owner("s-rep", sec(90), "go on")
	if rq := f.group()[0]; len(rq.Asks) != 2 || !rq.LastTurn.Equal(sec(90)) {
		t.Errorf("asks %q, last turn %v", words(rq.Asks), rq.LastTurn.Sub(t0))
	}
}

// A call whose every push is quiet prints no ref line of its own: a To block beside it is another program's.
func TestAQuietPushTakesNoOtherProgramsBlock(t *testing.T) {
	if got := artifactsAs("Bash", `{"command":"git push -q origin main && ./deploy.sh"}`,
		"deploying\nTo prod:/srv/app\n   1111111..2222222  main -> main\n", false); got != "" {
		t.Errorf("got %q", got)
	}
}

// What a script printed with a result in the object nearest it wins over an outer object's.
func TestTheNearestPlaceWins(t *testing.T) {
	script := `const rs = await Promise.all([tools.exec_command({cmd:"gh pr list"}), tools.exec_command({cmd:"gh pr create --fill"})]); rs.forEach((r,i)=>text({i:0, value:{i, ...r}}))`
	out := `Script completed Output: {"i":0,"value":{"i":1,"exit_code":0,"output":"https://github.com/o/r/pull/9\n"}} {"i":0,"value":{"i":0,"exit_code":0,"output":"https://github.com/o/r/pull/1\n"}}`
	if got := artifactsAs("exec", script, out, false); got != "pr pull request https://github.com/o/r/pull/9" {
		t.Errorf("got %q", got)
	}
}
