package request

// Tests from the fifteenth review of the requests change. Synthetic data only.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/evidenceview"
)

// The handoff's evidence is lines of evidence show commands, each of at most what it takes: every line runs.
func TestEachEvidenceLineRuns(t *testing.T) {
	f := newFixture(t)
	f.owner("s-ev", at(0), "fix the parser")
	for i := range 12 {
		f.tool("s-ev", "", "Bash", at(1+i), fmt.Sprintf(`{"command":"git commit -am c%d"}`, i), fmt.Sprintf("[main %07x] c%d\n 1 file changed", 0x1a2b3c0+i, i), false)
	}
	h := Handoff(f.group()[0])
	ids := 0
	for _, l := range strings.Split(h, "\n") {
		cmd, ok := strings.CutPrefix(l, "momentum evidence show ")
		if !ok {
			continue
		}
		args := strings.Fields(cmd)
		if _, _, err := evidenceview.ParseShow(args, io.Discard); err != nil {
			t.Errorf("%q: %v", l, err)
		}
		ids += len(args)
	}
	if ids != evidenceIDs {
		t.Errorf("%d ids printed, want %d:\n%s", ids, evidenceIDs, h)
	}
}

// A list reads only what it shows: the results whose calls came in its window, while a handoff reads the whole
// history; both find the same state. A request whose last turn is before the window, that nothing in it came after,
// is read back to that turn, so its state is its handoff's.
func TestAListReadsItsWindow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-long", at(0), "port the scheduler")
	f.tool("s-long", "", "Bash", at(1), `{"command":"git commit -am one"}`, "[main 1111111] one\n 1 file changed", false)
	f.owner("s-long", at(60), "and the timers")
	f.tool("s-long", "", "Bash", at(61), `{"command":"git commit -am two"}`, "[main 2222222] two\n 1 file changed", false)
	f.phase("s-long", at(62), "phase", "idle", nil)
	f.owner("s-old", at(0), "tune the cache")
	f.tool("s-old", "", "Bash", at(10), `{"command":"git commit -am cache"}`, "[main 3333333] cache\n 1 file changed", false)
	// in the window, a push whose transcript is gone: it shows nothing, and its details count once
	gone := filepath.Join(t.TempDir(), "s-old-gone.jsonl")
	f.cutPair("s-old", "claude", "Bash", "toolu_g1", at(70), `{"command":"git push origin main"}`,
		"To github.com:o/r.git\n   1111111..4444444  main -> main\n", gone+"@0#0", gone+"@100#0")
	f.phase("s-old", at(71), "phase", "idle", nil)
	f.put()
	list, err := List(ctx, f.st, at(50), Options{Redact: fakeRedact})
	if err != nil {
		t.Fatal(err)
	}
	for _, rq := range list {
		full, err := Find(ctx, f.st, rq.ID, Options{Redact: fakeRedact})
		if err != nil {
			t.Fatal(err)
		}
		if rq.State != full.State {
			t.Errorf("%s: list says %s, handoff %s", rq.ID, rq.State, full.State)
		}
		switch got := kinds(rq.Results); rq.ID {
		case ID(at(0), "port the scheduler"):
			if got != "commit commit 2222222 on main: two" || len(full.Results) != 2 {
				t.Errorf("the list read %q, the handoff %q", got, kinds(full.Results))
			}
		case ID(at(0), "tune the cache"):
			if rq.State != Ready || got != "commit commit 3333333 on main: cache" || rq.CutGone != 1 {
				t.Errorf("state %s (%s), results %q, cut and gone %d", rq.State, rq.Why, got, rq.CutGone)
			}
		}
	}
	if len(list) != 2 {
		t.Errorf("%d requests", len(list))
	}
}

// An id at the end of his own prose is no handoff's last line: only one after the handoff's last sentence is.
func TestAnIDAtTheEndOfHisProseIsNoHandoff(t *testing.T) {
	f := newFixture(t)
	f.owner("s-src", at(0), "tune the cache")
	src := f.group()[0]
	f.owner("s-next", at(10), "is this the same bug as "+Line(src.ID))
	if reqs := f.group(); len(reqs) != 2 {
		t.Errorf("%d requests: the prose linked", len(reqs))
	}
}

// A Codex result's exit is read off its header: a command still running whose own output prints the exited words
// shows no ref line yet.
func TestARunningHeaderIsRead(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000f001"
	f.owner(s, at(0), "push the branch")
	f.pairWithID(s, "", "codex", "exec_command", "call_h1", at(1), `{"cmd":"./push-and-log.sh && git push origin feat","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\nProcess exited with code 0\nTo github.com:o/r.git\n   1111111..2222222  feat -> feat\n")
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("got %q", got)
	}
}

// The same short ask twice with the agent's work between (a commit) is two turns: only one with nothing of the
// agent's between is one turn recorded twice.
func TestTheSameAskAfterWorkIsAnotherTurn(t *testing.T) {
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	f := newFixture(t)
	f.owner("s-go", sec(0), "go on")
	f.tool("s-go", "", "Bash", sec(10), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.owner("s-go", sec(60), "go on")
	if rq := f.group()[0]; len(rq.Asks) != 2 || !rq.LastTurn.Equal(sec(60)) {
		t.Errorf("asks %q, last turn %v", words(rq.Asks), rq.LastTurn)
	}
}

// A Promise.all array's places count from its first call: a result printed with its place is that call's, past the
// calls before the array.
func TestAPlaceCountsFromTheArray(t *testing.T) {
	script := `const x = await tools.exec_command({cmd:"git status"}); const rs = await Promise.all([tools.exec_command({cmd:"gh pr list"}), tools.exec_command({cmd:"gh pr create --fill"})]); rs.forEach((r,i)=>text({i,...r}))`
	out := `Script completed Output: {"i":1,"exit_code":0,"output":"https://github.com/o/r/pull/9\n"} {"i":0,"exit_code":0,"output":"https://github.com/o/r/pull/1\n"}`
	if got := artifactsAs("exec", script, out, false); got != "pr pull request https://github.com/o/r/pull/9" {
		t.Errorf("got %q", got)
	}
}

// Two openings in one millisecond: an id names its own opening by its hash, and the time alone names neither.
func TestTwoOpeningsInOneMillisecond(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-a", at(0), "first ask")
	f.owner("s-b", at(0), "second ask")
	f.put()
	for _, c := range []struct{ id, thread string }{{ID(at(0), "first ask"), "s-a"}, {ID(at(0), "second ask"), "s-b"}} {
		rq, err := Find(ctx, f.st, c.id, Options{Redact: fakeRedact})
		if err != nil || rq.ID != c.id || len(rq.Threads) != 1 || rq.Threads[0].ID != c.thread {
			t.Errorf("%s: %+v %v", c.id, rq, err)
		}
	}
	other := timePart(at(0)) + "ffff"
	if rq, err := Find(ctx, f.st, other, Options{Redact: fakeRedact}); !errors.Is(err, ErrNoRequest) {
		t.Errorf("the time named %+v (%v)", rq, err)
	}
}

// The handoff writes the home directory as ~ where it starts a path; another directory that starts with its name
// is kept.
func TestTheHandoffFoldsTheHome(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	f := newFixture(t)
	f.owner("s-home", at(0), "write /home/someone/proj/notes.md and compare /home/someone2/x and /data/home/someone/y")
	f.evs[len(f.evs)-1].Cwd = "/home/someone/proj"
	f.tool("s-home", "", "Write", at(1), `{"file_path":"/home/someone/proj/a.go","content":"package a"}`, "File created successfully", false)
	f.evs[len(f.evs)-1].Cwd = "/home/someone/proj"
	h := Handoff(f.group()[0])
	for _, want := range []string{"wrote ~/proj/a.go", "write ~/proj/notes.md", "/home/someone2/x", "/data/home/someone/y"} {
		if !strings.Contains(h, want) {
			t.Errorf("lacks %q:\n%s", want, h)
		}
	}
	if strings.Contains(strings.ReplaceAll(h, "/data/home/someone/", ""), "/home/someone/") {
		t.Errorf("the home is named:\n%s", h)
	}
}
