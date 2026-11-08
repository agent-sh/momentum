package request

// Tests from the eleventh review of the requests change. Synthetic data only. Several began as the review's own
// probes.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	stamp "github.com/agent-sh/momentum/sessions/state"
)

// Hermes's write_file wrote and verified the file, and its lint found a syntax error: the write is a write.
func TestAWriteWhoseLintFailedIsAWrite(t *testing.T) {
	res := `{"bytes_written": 120, "dirs_created": false, "verified": true, "lint": {"status": "error", "output": "SyntaxError: invalid syntax (line 3, column 1)"}, "resolved_path": "/work/a.py"}`
	f := newFixture(t)
	s := "20261107_100000_lintw"
	f.owner(s, at(0), "write the script")
	f.evs[len(f.evs)-1].Harness = "hermes"
	f.pairWithID(s, "", "hermes", "write_file", "call_w1", at(1), `{"path": "/work/a.py", "content": "print(1"}`, res)
	if got := kinds(f.group()[0].Results); !strings.Contains(got, "file /work/a.py") {
		t.Errorf("the verified write is not a result: %q", got)
	}
}

// The open step is his last ask, not a harness command after it; a path is an ask, not a command.
func TestTheOpenStepIsNoHarnessCommand(t *testing.T) {
	f := newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.owner("s1", at(1), "/compact")
	if rq := f.group()[0]; !strings.HasSuffix(rq.Open, "fix the parser") {
		t.Errorf("open %q", rq.Open)
	}
	f = newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.owner("s1", at(1), "/home/someone/notes.md read this")
	if rq := f.group()[0]; !strings.HasSuffix(rq.Open, "/home/someone/notes.md read this") {
		t.Errorf("a path read as a command: open %q", rq.Open)
	}
}

// A script that printed fewer results than its text() calls, and did not stop: its results cannot be paired.
func TestFewerResultsThanPrintsReadNothing(t *testing.T) {
	script := `const a = await tools.exec_command({cmd:"cat notes.txt"}); if (a.exit_code !== 0) { text(await tools.exec_command({cmd:"git commit -am Fix"})); } text(a)`
	out := `Script completed Wall time 0.2 seconds Output: {"chunk_id":"a","exit_code":0,"output":"[main 1a2b3c4] an old note quoted\n"}`
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000c003"
	f.owner(s, at(0), "check the notes")
	f.pairWithID(s, "", "codex", "exec", "call_c1", at(1), script, out)
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("a cat's output read as the commit's: %q", got)
	}
}

// Each agent of a thread numbers its own sessions: the main thread's poll of its session 42 is its own command's,
// though a subagent's command took a session 42 since.
func TestTheMainThreadsSessionIsItsOwn(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000b004"
	sub := "019a0000-0000-7000-8000-00000000b0bb"
	f.owner(s, at(0), "open the pull request")
	f.pairWithID(s, "", "codex", "exec_command", "call_m1", at(1), `{"cmd":"gh pr create --fill","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, sub, "codex", "exec_command", "call_m2", at(2), `{"cmd":"./scripts/notes.sh","yield_time_ms":1000}`,
		"Chunk ID: b\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_m3", at(3), `{"session_id":42,"chars":""}`,
		"Chunk ID: c\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nhttps://github.com/o/r/pull/7\n")
	if got := kinds(f.group()[0].Results); got != "pr pull request https://github.com/o/r/pull/7" {
		t.Errorf("got %q", got)
	}
}

// A code-mode script that prints its one command's output text (text(r.output)), not its result: the text's lines
// are that command's, and with no exit code shown only its commit lines count. The store keeps the text flattened,
// so it is read whole.
func TestAPrintedOutputTextIsItsCommands(t *testing.T) {
	cases := []struct{ name, script, out, want string }{
		{"a commit",
			"const r = await tools.exec_command({\n  cmd:\"git commit -m \\\"docs: define the contract\\\"\",\n  yield_time_ms:30000\n});\ntext(r.output);\ntext(`exit=${r.exit_code}`);\n",
			"Script completed\nWall time 0.4 seconds\nOutput:\n\n[docs/contract 0eed442] docs: define the contract\n 1 file changed, 46 insertions(+)\n\nexit=0",
			"commit commit 0eed442 on docs/contract: docs: define the contract"},
		{"a push: no exit code shows",
			"const r = await tools.exec_command({cmd:\"git push origin main\"});\ntext(r.output);\n",
			"Script completed\nWall time 0.4 seconds\nOutput:\n\nTo github.com:o/r.git\n   1111111..2222222  main -> main\n", ""},
		{"two commands' texts",
			"const a = await tools.exec_command({cmd:\"git commit -m Fix\"});\nconst b = await tools.exec_command({cmd:\"cat old.log\"});\ntext(a.output);\ntext(b.output);\n",
			"Script completed\nWall time 0.4 seconds\nOutput:\n\n[main 9f8e7d6] an old commit\n", ""},
		{"a variable a commit set, then a cat",
			"let r = await tools.exec_command({cmd:\"git commit -m Fix\"});\nr = await tools.exec_command({cmd:\"cat old.log\"});\ntext(r.output);\n",
			"Script completed\nWall time 0.4 seconds\nOutput:\n\n[main 9f8e7d6] an old commit\n", ""},
		{"a variable a cat set, then a commit",
			"let r = await tools.exec_command({cmd:\"cat old.log\"});\nr = await tools.exec_command({cmd:\"git commit -m Fix\"});\ntext(r.output);\n",
			"Script completed\nWall time 0.4 seconds\nOutput:\n\n[main 9f8e7d6] an old commit\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			s := "019a0000-0000-7000-8000-00000000c005"
			f.owner(s, at(0), "commit the docs")
			f.pairWithID(s, "", "codex", "exec", "call_c5", at(1), c.script, c.out)
			if got := kinds(f.group()[0].Results); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	// the store keeps the printed text flattened: with the rollout gone, no line of it is read
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000c006"
	f.owner(s, at(0), "commit the docs")
	f.pairWithID(s, "", "codex", "exec", "call_c6", at(1), cases[0].script, cases[0].out)
	removeFile(t, filepath.Join(f.dir, "rollout-2026-11-07T10-00-00-"+s+".jsonl"))
	if got := kinds(f.groupWith(Options{Redact: fakeRedact})[0].Results); got != "" {
		t.Errorf("read off the flattened text: %q", got)
	}
}

// A thread no request holds is not read for its calls: its transcript is never opened.
func TestAThreadNoRequestHoldsReadsNoCalls(t *testing.T) {
	f := newFixture(t)
	f.owner("s-ask", at(0), "fix the parser")
	// an automated run with no turn of the owner's: it opens no request and links to none
	f.tool("s-auto", "", "Bash", at(1), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.put()
	r := newReader(f.st, Options{Redact: fakeRedact})
	defer r.close()
	r.enqueue("s-ask")
	r.enqueue("s-auto")
	if _, err := r.settle(context.Background(), t0.Add(-time.Hour), func(rq *Request) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if n := r.opens[filepath.Join(f.dir, "s-auto.jsonl")]; n != 0 || r.threads["s-auto"] == nil || r.threads["s-auto"].tools {
		t.Errorf("the automated run's calls were read: %d opens", n)
	}
}

// A live session whose phase began before the request did, and that a sessions run still sees in it after the
// turn, is in that phase: its stamp stands for its newest event, which the bounded read of phases leaves out.
func TestALiveSessionsPhaseFromBeforeTheRequest(t *testing.T) {
	f := newFixture(t)
	f.phase("s1", at(0), "phase", "working", nil)
	f.owner("s1", at(5), "and fix the parser too")
	f.put()
	st := stamp.Stamps{"s1": {Phase: sessions.Phase("working"), Since: at(0), Seen: at(6)}}
	if err := f.st.SaveSessionStamps(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rq := f.groupWith(Options{Redact: fakeRedact})[0]; rq.State != Working {
		t.Errorf("state %s (%s)", rq.State, rq.Why)
	}
}
