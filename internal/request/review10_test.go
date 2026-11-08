package request

// Tests from the tenth review of the requests change. Synthetic data only. Several began as the review's own
// probes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	stamp "github.com/agent-sh/momentum/sessions/state"
)

// writeAs is Artifacts on a writer's call and its result, with no error flag.
func writeAs(harness, tool, input, output string) string {
	call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Harness: harness, Tool: tool, Kind: "tool_call", Summary: input, Meta: `{"tool_id":"call_w"}`}
	res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(1).Add(time.Second), Harness: harness, Tool: tool, Kind: "tool_result", Summary: output, Meta: `{"tool_id":"call_w"}`}
	return kinds(Artifacts(call, res))
}

// A write its result says was refused wrote nothing, whether or not the harness flagged it.
func TestARefusedWriteWithNoErrorFlagWroteNothing(t *testing.T) {
	for _, c := range []struct{ name, harness, tool, input, output string }{
		{"pi's Error [CODE]", "pi", "edit", `{"path":"/work/app/layout.tsx","old_string":"a","new_string":"b"}`,
			"Error [NOT_READ_THIS_SESSION]: File has not been Read in this session: /work/app/layout.tsx\n\nCall Read on this path first"},
		{"pi's write refused", "pi", "write", `{"path":"/work/package.json","content":"{}"}`,
			"Error [NOT_READ_THIS_SESSION]: Write refuses to overwrite a file that has not been Read in this session: /work/package.json"},
		{"an Error: prefix", "claude", "Edit", `{"file_path":"/work/a.go","old_string":"a","new_string":"b"}`, "Error: String to replace not found in file."},
		{"a tool_use_error", "claude", "Write", `{"file_path":"/work/a.go","content":"x"}`,
			"<tool_use_error>File has not been read yet. Read it first before writing to it.</tool_use_error>"},
		{"Hermes's success false", "hermes", "patch", `{"mode": "replace", "path": "/work/d.md", "old_string": "a", "new_string": "b"}`,
			`{"success": false, "error": "Could not find a match for old_string in the file"}`},
		{"an error field", "hermes", "write_file", `{"path": "/work/e.md", "content": "x"}`, `{"error": "Permission denied: /work/e.md"}`},
		{"a cancellation", "claude", "Write", `{"file_path":"/work/g.go","content":"x"}`,
			"The user doesn't want to take this action right now. STOP what you are doing and wait for the user to tell you how to proceed."},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := writeAs(c.harness, c.tool, c.input, c.output); got != "" {
				t.Errorf("a refused write read as made: %s", got)
			}
		})
	}
	if got := writeAs("hermes", "write_file", `{"path": "/work/f.md", "content": "x"}`, `{"bytes_written": 1, "error": null}`); got != "file /work/f.md" {
		t.Errorf("a write that succeeded: %q", got)
	}
}

// A Codex session id an earlier command left running is reused once it exited: a later poll of it is read against
// the command that holds it now, whatever that names.
func TestAReusedSessionIDReadsTheNewCommand(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000b001"
	f.owner(s, at(0), "push the fix")
	f.pairWithID(s, "", "codex", "exec_command", "call_p1", at(1), `{"cmd":"git push origin main","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\nrunning the pre-push checks\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_p2", at(2), `{"session_id":42,"chars":""}`,
		"Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nTo github.com:o/r.git\n   1111111..2222222  main -> main\n")
	f.pairWithID(s, "", "codex", "exec_command", "call_p3", at(10), `{"cmd":"./scripts/preview-release.sh","yield_time_ms":1000}`,
		"Chunk ID: c\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\nbuilding\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_p4", at(11), `{"session_id":42,"chars":""}`,
		"Chunk ID: d\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nTo github.com:o/r.git\n * [new branch]      release-preview -> release-preview\n")
	if got := kinds(f.group()[0].Results); got != "push pushed main -> main (1111111..2222222) to github.com:o/r.git" {
		t.Errorf("got %q", got)
	}
}

// A command that names no git, given a session id an earlier git push held (whose exit no poll showed), holds it
// now: a poll of it is read against the new command.
func TestASessionIDANoGitCommandTakesIsItsNow(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000b004"
	f.owner(s, at(0), "push the fix")
	f.pairWithID(s, "", "codex", "exec_command", "call_n1", at(1), `{"cmd":"git push origin main","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, "", "codex", "exec_command", "call_n2", at(10), `{"cmd":"./scripts/preview-release.sh","yield_time_ms":1000}`,
		"Chunk ID: c\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\nbuilding\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_n3", at(11), `{"session_id":42,"chars":""}`,
		"Chunk ID: d\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nTo github.com:o/r.git\n * [new branch]      release-preview -> release-preview\n")
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("got %q", got)
	}
}

// A loop of polls that prints each result with the session it named: each is read against that session's command.
func TestALoopOfPollsReadsEachSession(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000b005"
	f.owner(s, at(0), "open the pull request")
	f.pairWithID(s, "", "codex", "exec", "call_l1", at(1), `text(await tools.exec_command({cmd:"gh pr create --fill",yield_time_ms:1000}))`,
		`Script completed Output: {"chunk_id":"a","session_id":42,"output":""}`)
	f.pairWithID(s, "", "codex", "exec", "call_l2", at(2), `for(const id of [41,42]) text({id,...await tools.write_stdin({session_id:id,chars:""})})`,
		`Script completed Output: {"id":41,"chunk_id":"b","exit_code":0,"output":"https://github.com/o/r/pull/3\n"} {"id":42,"chunk_id":"c","exit_code":0,"output":"https://github.com/o/r/pull/7\n"}`)
	if got := kinds(f.group()[0].Results); got != "pr pull request https://github.com/o/r/pull/7" {
		t.Errorf("got %q", got)
	}
}

// A poll whose process exited ends the session: a later poll of the same id, with no command left running it,
// reads as nothing.
func TestAPollAfterTheProcessExitedIsNothing(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000b003"
	f.owner(s, at(0), "push the fix")
	f.pairWithID(s, "", "codex", "exec_command", "call_x1", at(1), `{"cmd":"git push origin main","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_x2", at(2), `{"session_id":42,"chars":""}`,
		"Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nEverything up-to-date\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_x3", at(3), `{"session_id":42,"chars":""}`,
		"Chunk ID: c\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nTo github.com:o/r.git\n   1111111..2222222  main -> main\n")
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("got %q", got)
	}
}

// Each agent of a Codex thread numbers its own sessions: a subagent's session 42 is not the parent's.
func TestASubagentsSessionIsItsOwn(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000b002"
	sub := "019a0000-0000-7000-8000-00000000b0aa"
	f.owner(s, at(0), "open the pull request")
	f.pairWithID(s, "", "codex", "exec_command", "call_q1", at(1), `{"cmd":"gh pr create --fill","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, sub, "codex", "exec_command", "call_q2", at(2), `{"cmd":"./scripts/open-prs.sh","yield_time_ms":1000}`,
		"Chunk ID: b\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, sub, "codex", "write_stdin", "call_q3", at(3), `{"session_id":42,"chars":""}`,
		"Chunk ID: c\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nhttps://github.com/o/r/pull/3\nhttps://github.com/o/r/pull/4\n")
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("got %q", got)
	}
}

// A code-mode script's results are its calls' in the order it prints them; a script that runs calls side by side
// reads nothing.
func TestCodeModeResultsGoByWhatTheScriptPrints(t *testing.T) {
	cases := []struct{ name, script, output, want string }{
		{"printed out of call order",
			`const a = await tools.exec_command({cmd:"gh pr list --json url -q .[].url"}); const b = await tools.exec_command({cmd:"gh pr create --fill"}); text(b); text(a)`,
			`Script completed Output: {"chunk_id":"b","exit_code":0,"output":"https://github.com/o/r/pull/9\n"} {"chunk_id":"a","exit_code":0,"output":"https://github.com/o/r/pull/1\nhttps://github.com/o/r/pull/2\n"}`,
			"pr pull request https://github.com/o/r/pull/9"},
		{"Promise.all",
			`const [a, b] = await Promise.all([tools.exec_command({cmd:"gh pr list"}), tools.exec_command({cmd:"gh pr create --fill"})]); text(a); text(b)`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/1\n"} {"exit_code":0,"output":"https://github.com/o/r/pull/9\n"}`, ""},
		{"Promise.allSettled, each result printed with its place",
			`const rs = await Promise.allSettled([tools.exec_command({cmd:"gh pr list"}), tools.exec_command({cmd:"gh pr create --fill"})]); rs.forEach((r,i)=>text({i,...r}))`,
			`Script completed Output: {"i":1,"status":"fulfilled","value":{"exit_code":0,"output":"https://github.com/o/r/pull/9\n"}} {"i":0,"status":"fulfilled","value":{"exit_code":0,"output":"https://github.com/o/r/pull/1\n"}}`,
			"pr pull request https://github.com/o/r/pull/9"},
		{"a script that failed printed the results it reached",
			`text(await tools.exec_command({cmd:"git commit -am Fix"})); text(await tools.exec_command({cmd:"rm -rf /tmp/x"}))`,
			`Script failed Wall time 0.2 seconds Output: {"exit_code":0,"output":"[main 1a2b3c4] Fix\n 1 file changed\n"} Script error: exec_command failed: Rejected`,
			"commit commit 1a2b3c4 on main: Fix"},
		{"calls not awaited, printed by their variables",
			`const a = tools.exec_command({cmd:"gh pr list"}); const b = tools.exec_command({cmd:"gh pr create --fill"}); text(a); text(b)`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/9\n"} {"exit_code":0,"output":"https://github.com/o/r/pull/1\n"}`, ""},
		{"a call not awaited in turn",
			`const a = tools.exec_command({cmd:"gh pr list"}); const b = await tools.exec_command({cmd:"gh pr create --fill"}); text(await a); text(b)`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/1\n"} {"exit_code":0,"output":"https://github.com/o/r/pull/9\n"}`, ""},
		// the poll's exit code is its command's: a push whose poll exited non-zero pushed nothing
		{"a poll's exit code",
			`const r = await tools.exec_command({cmd:"git push origin main",yield_time_ms:1000}); text(r); text(await tools.write_stdin({session_id:r.session_id,chars:""}))`,
			`Script completed Output: {"chunk_id":"a","session_id":7,"output":""} {"chunk_id":"b","exit_code":1,"output":"To github.com:o/r.git\n   1111111..2222222  main -> main\n"}`, ""},
		// one result's output fields are one text
		{"stdout and stderr of one result",
			`text(await tools.exec_command({cmd:"git commit -am Fix && git push origin main"}))`,
			`Script completed Output: {"exit_code":0,"stdout":"[main 1a2b3c4] Fix\n 1 file changed\n","stderr":"To github.com:o/r.git\n   1111111..2222222  main -> main\n"}`,
			"commit commit 1a2b3c4 on main: Fix | push pushed main -> main (1111111..2222222) to github.com:o/r.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("exec", c.script, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A push the store's cut ends may hide a --dry-run: beside a real push, no ref line reads as pushed.
func TestACutPushBesideARealOneMakesNoPush(t *testing.T) {
	in := `{"command":"git push origin main && git push --dry-run origin topi…`
	out := "To github.com:o/r.git\n   1111111..2222222  main -> main\nTo github.com:o/r.git\n * [new branch]      topic -> topic\n"
	if got := artifactsAs("Bash", in, out, false); got != "" {
		t.Errorf("got %q", got)
	}
}

// gh issue, repo and release view print a body that may hold a pull request's address on a line of its own: beside
// gh pr create, the call makes no pull request.
func TestAViewBesideCreateBlocks(t *testing.T) {
	out := "https://github.com/o/r/pull/12\ntitle:\tx\n--\nSee the earlier attempt:\nhttps://github.com/o/r/pull/7\n"
	for _, cmd := range []string{"gh pr create --fill && gh issue view 64", "gh pr create --fill && gh repo view o/r", "gh pr create --fill && gh release view v1"} {
		if got := artifactsAs("Bash", `{"command":`+jsonString(cmd)+`}`, out, false); got != "" {
			t.Errorf("%s: got %q", cmd, got)
		}
	}
}

// A turn of the owner's after his check reopens it: he asked for more. A check of the newest result after the
// turn holds it again.
func TestATurnAfterTheCheckReopensIt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.tool("s1", "", "Bash", at(1), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	f.phase("s1", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if _, err := Check(ctx, f.st, rq, at(3)); err != nil {
		t.Fatal(err)
	}
	f.owner("s1", at(4), "and the tests too")
	f.phase("s1", at(5), "phase", "idle", nil)
	rq = f.group()[0]
	if rq.State == Checked || !strings.Contains(rq.Why, "reopened by a turn of the owner's after it") || !strings.Contains(rq.Open, "and the tests too") {
		t.Fatalf("state %s (%s), open %q", rq.State, rq.Why, rq.Open)
	}
	if c, err := Check(ctx, f.st, rq, at(6)); err != nil || !c.Added {
		t.Fatalf("check again: %+v %v", c, err)
	}
	if rq = f.group()[0]; rq.State != Checked {
		t.Errorf("after the check again: %s (%s)", rq.State, rq.Why)
	}
}

// A stamp counts only for the stretch its phase began: one from another stretch says nothing of the newest event.
func TestAStampOfAnotherStretchIsNotSeen(t *testing.T) {
	f := newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.phase("s1", at(0).Add(10*time.Second), "phase", "working", nil)
	f.owner("s1", at(3), "also add a test")
	f.put()
	st := stamp.Stamps{"s1": {Phase: sessions.Phase("working"), Since: at(0).Add(20 * time.Second), Seen: at(5)}}
	if err := f.st.SaveSessionStamps(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rq := f.groupWith(Options{Redact: fakeRedact})[0]; rq.State != Unknown {
		t.Errorf("state %s (%s)", rq.State, rq.Why)
	}
}

// The final report is the newest reply across the request's threads, whichever thread the read takes first.
func TestTheReportIsTheNewestReplyAcrossThreads(t *testing.T) {
	for _, newer := range []string{"s-a", "s-b"} {
		f := newFixture(t)
		f.owner("s-a", at(0), "split the parser")
		f.event("s-b", "", "harness_text", store.ActorHarness, at(1), "continued", map[string]any{"parent": "s-a"})
		older := map[string]string{"s-a": "s-b", "s-b": "s-a"}[newer]
		f.reply(older, at(2), "Half done.")
		f.reply(newer, at(3), "All done.")
		if rq := f.group()[0]; rq.Report == nil || rq.Report.Text != "All done." {
			t.Errorf("newer in %s: report %+v", newer, rq.Report)
		}
	}
}

// A thread whose segments of one request another request's segment splits is one thread of it, its span from the
// first segment to the last.
func TestASplitThreadIsOneThreadOfTheRequest(t *testing.T) {
	f := newFixture(t)
	f.owner("s-other", at(-50), "the other ask")
	other := f.group()[0]
	f.owner("s-split", at(0), "fix the parser")
	rq := f.group()[0]
	for _, x := range f.group() {
		if x.ID != other.ID {
			rq = x
		}
	}
	f.owner("s-split", at(5), "momentum request "+other.ID+" - a handoff\nmomentum request "+other.ID)
	f.owner("s-split", at(10), "continue\n"+Handoff(rq))
	f.reply("s-split", at(11), "Continuing.")
	for _, x := range f.group() {
		if x.ID != rq.ID {
			continue
		}
		n := 0
		for _, th := range x.Threads {
			if th.ID == "s-split" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the thread shows %d times: %+v", n, x.Threads)
		}
		return
	}
	t.Fatal("the request is gone")
}
