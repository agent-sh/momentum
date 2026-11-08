package request

// Tests from the eighth review of the requests change. Synthetic data only. The first five began as the review's
// own probes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A push that printed no To line (nothing new to push) beside a fetch the parser cannot see (over ssh, inside
// submodule foreach, in a script): the fetch's ref lines read as pushed.
func TestAToLessPushTakesNoFetchsRefLines(t *testing.T) {
	pull := "From github.com:o/r\n   1111111..2222222  release/2026-11 -> origin/release/2026-11\nUpdating 1111111..2222222\nFast-forward\n a.go | 1 +\n"
	for _, c := range []struct{ cmd, out string }{
		{"git push origin main && ssh deploy 'cd /srv/app && git pull'", "Everything up-to-date\n" + pull},
		{"git push && git submodule foreach git pull", "Everything up-to-date\nEntering 'sub'\n" + pull},
		{"git push && ./scripts/sync.sh", "Everything up-to-date\n" + pull},
	} {
		got := artifactsAs("Bash", `{"command":`+jsonString(c.cmd)+`}`, c.out, false)
		t.Logf("%s -> %q", c.cmd, got)
		if got != "" {
			t.Errorf("FALSE PUSH from %q: %q", c.cmd, got)
		}
	}
}

// A check of an artifact whose transcript went: a reply after the check (a follow-up's answer) reopens it,
// though with the transcript there the same reply does not (a newer report reopens only a request with no
// artifact), and SPEC says the read losing a result reopens nothing.
func TestALostCheckIsNotReopenedByAReply(t *testing.T) {
	f := newFixture(t)
	s := "s-claude-lost"
	f.tool(s, "", "Write", at(0).Add(20*time.Second), `{"file_path":"/work/x/a.go","content":"package x"}`, "File created successfully", false)
	command := "cd " + longDir + " && git commit -m 'Fix the parser'"
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
	path := claudeCall(t, f, t.TempDir(), s, command, output)
	f.reply(s, at(3), "Committed the parser fix.")
	f.phase(s, at(4), "phase", "idle", nil)
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if n := rq.Newest(); n == nil || !strings.Contains(n.Key, "1a2b3c4") {
		t.Fatalf("newest before: %+v", rq.Newest())
	}
	if _, err := Check(context.Background(), f.st, rq, at(10)); err != nil {
		t.Fatal(err)
	}
	f.owner(s, at(11), "thanks, is the old parser still used anywhere?")
	f.reply(s, at(12), "No, nothing imports it.")
	f.phase(s, at(13), "phase", "idle", nil)
	f.put()
	reqs, err := Group(context.Background(), f.st, t0.Add(-time.Hour), Options{Redact: fakeRedact})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("with the transcript: %s (%s), newest %s", reqs[0].State, reqs[0].Why, reqs[0].Newest().Key)
	if reqs[0].State != Checked {
		t.Fatalf("with the transcript: %s", reqs[0].State)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reqs, err = Group(context.Background(), f.st, t0.Add(-time.Hour), Options{Redact: fakeRedact})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("transcript gone: %s (%s), newest %s", reqs[0].State, reqs[0].Why, reqs[0].Newest().Key)
	if reqs[0].State != Checked {
		t.Errorf("REOPENED by the lost read: %s (%s)", reqs[0].State, reqs[0].Why)
	}
}

// A Codex code-mode script: gh pr create in one exec_command, gh pr view in another. Each result is read against
// its own command (SPEC), and a short output gives the pull request; a long one the store cut is read whole only
// when the script's commands together make something, and gh pr view in the other exec_command says they make no
// pull request, so the cut output is never read and the pull request is lost.
func TestACodeModePullRequestIsReadWholeWhenCut(t *testing.T) {
	script := `text(await tools.exec_command({cmd:"gh pr create --fill"})); text(await tools.exec_command({cmd:"gh pr view 12 --json title"}))`
	for _, long := range []bool{false, true} {
		f := newFixture(t)
		dir := t.TempDir()
		session := "019a0000-0000-7000-8000-00000000c0d1"
		path := filepath.Join(dir, "rollout-2026-11-07T10-00-00-"+session+".jsonl")
		warn := ""
		if long {
			warn = strings.Repeat("Warning: 1 uncommitted change\n", 10)
		}
		c1, _ := json.Marshal(map[string]any{"chunk_id": "a1", "exit_code": 0, "output": warn + "https://github.com/o/r/pull/12\n"})
		c2, _ := json.Marshal(map[string]any{"chunk_id": "a2", "exit_code": 0, "output": "{\"title\":\"x\"}\n"})
		output := "Script completed Wall time 3.1 seconds Output: " + string(c1) + " " + string(c2)
		offs := writeLines(t, path,
			`{"timestamp":"2026-11-07T10:00:00.000Z","type":"session_meta","payload":{"id":"`+session+`","cwd":"/work/x","originator":"codex-tui"}}`,
			`{"timestamp":"2026-11-07T10:01:00.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":`+jsonString(script)+`,"call_id":"call_e1"}}`,
			`{"timestamp":"2026-11-07T10:01:01.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_e1","output":`+jsonString(output)+`}}`)
		f.owner(session, at(0), "open the pull request")
		f.cutPair(session, "codex", "exec", "call_e1", at(1), script, output,
			fmt.Sprintf("%s@%d#0", path, offs[1]), fmt.Sprintf("%s@%d#0", path, offs[2]))
		rq := f.groupWith(Options{Redact: fakeRedact})[0]
		got := kinds(rq.Results)
		t.Logf("long=%v stored output cut=%v: %q", long, strings.HasSuffix(storedCut(output), "…"), got)
		if !strings.Contains(got, "pull/12") {
			t.Errorf("LOST: long=%v: %q", long, got)
		}
	}
}

// A patch the store cut after its first file header: the store's text names a file, so the call is not read
// whole, and the files past the cut are never results, with Hermes's store right there.
func TestACutPatchIsReadWholeForItsLaterFiles(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session := "20261107_100000_hpatch"
	body := strings.Repeat("+// a line of the new code\n", 8)
	patch := "*** Begin Patch\n*** Update File: src/a.go\n@@\n" + body + "*** Update File: src/b.go\n@@\n" + body + "*** Add File: src/c.go\n" + body + "*** End Patch"
	args, _ := json.Marshal(map[string]string{"mode": "patch", "patch": patch})
	calls, _ := json.Marshal([]map[string]any{{"id": "call_p1", "type": "function", "function": map[string]string{"name": "patch", "arguments": string(args)}}})
	output, _ := json.Marshal(map[string]any{"success": true})
	if _, err := db.Exec(hermesSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into sessions (id, source, started_at) values (?, 'cli', 1)`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into messages (id, session_id, role, content, tool_calls, timestamp) values (1, ?, 'assistant', '', ?, 2)`, session, string(calls)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into messages (id, session_id, role, content, tool_call_id, tool_name, timestamp) values (2, ?, 'tool', ?, 'call_p1', 'patch', 3)`,
		session, string(output)); err != nil {
		t.Fatal(err)
	}
	f.owner(session, at(0), "split the parser")
	f.cutPair(session, "hermes", "patch", "call_p1", at(1), string(args), string(output), path+"@1#0", path+"@2#0")
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	got := kinds(rq.Results)
	t.Logf("stored input: %q", storedCut(string(args)))
	t.Logf("results: %q", got)
	for _, p := range []string{"src/a.go", "src/b.go", "src/c.go"} {
		if !strings.Contains(got, p) {
			t.Errorf("MISSED %s", p)
		}
	}
}

// A code-mode exec_command still running when it yielded (a session id, no exit code), its rest read by
// write_stdin: one command, two results, so nothing is read, though the second result is plainly its own.
func TestACodeModeCommandFinishedByWriteStdin(t *testing.T) {
	script := `const r = await tools.exec_command({cmd:"git commit -am Fix && git push origin main", yield_time_ms:1000}); text(r); text(await tools.write_stdin({session_id:r.session_id, chars:"", yield_time_ms:30000}))`
	output := `Script completed Output: {"chunk_id":"a1","wall_time_seconds":1.0,"session_id":7,"original_token_count":20,"output":"[main 1a2b3c4] Fix\n 1 file changed\n"} ` +
		`{"chunk_id":"a2","wall_time_seconds":4.2,"exit_code":0,"original_token_count":30,"output":"To github.com:o/r.git\n   1111111..2222222  main -> main\n"}`
	got := artifactsAs("exec", script, output, false)
	t.Logf("got %q", got)
	if !strings.Contains(got, "1a2b3c4") || !strings.Contains(got, "1111111..2222222") {
		t.Errorf("MISSED: %q", got)
	}
}

// A row for each rule the other tests leave untaken: each fails without its rule.
func TestEachRuleOfTheEighthReviewHasARow(t *testing.T) {
	commit := "[main 1a2b3c4] Fix\n 1 file changed"
	cases := []struct{ name, tool, input, output, want string }{
		// lines are matched at their start
		{"a commit line mid-line", "Bash", `{"command":"git commit -am Fix && cat notes"}`, "note: [main 1a2b3c4] Fix\n", ""},
		{"a To mid-line", "Bash", `{"command":"git push origin main 2>&1 | tail -n 2"}`, "Copied To /srv/x\n   1111111..2222222  main -> main",
			"push pushed main -> main (1111111..2222222) to origin"},
		{"a From mid-line in a push's block", "Bash", `{"command":"git push origin main"}`,
			"To github.com:o/r.git\nremote: merged From the queue\n   1111111..2222222  main -> main", "push pushed main -> main (1111111..2222222) to github.com:o/r.git"},
		// a gh command that prints addresses blocks a pull request, though the store cut it
		{"a cut gh pr view", "Bash", `{"command":"gh pr create --fill && gh pr view 5 --js…`, "https://github.com/o/r/pull/8\n", ""},
		// the cut flag is the last code-mode command's
		{"a code-mode command the cut ends", "exec", `text(await tools.exec_command({cmd:"gh pr create --fill"})); text(await tools.exec_command({cmd:"git push origin fe…`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/8\n"} {"exit_code":0,"output":"To github.com:o/r.git\n   1111111..2222222  feat -> feat\n"}`,
			"pr pull request https://github.com/o/r/pull/8"},
		{"a commit", "Bash", `{"command":"git commit -am Fix"}`, commit, "commit commit 1a2b3c4 on main: Fix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs(c.tool, c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A phase counts only when seen after the last turn: a session seen working or waiting on the owner before his
// last turn is not known to work or wait now.
func TestAPhaseFromBeforeTheTurnIsNotTrusted(t *testing.T) {
	for _, phase := range []string{"working", "needs_you"} {
		f := newFixture(t)
		f.owner("s-x", at(0), "draft the report")
		f.phase("s-x", at(1), "phase", phase, map[string]any{"ask": "Ship it?", "ask_kind": "question"})
		f.owner("s-x", at(5), "and the summary")
		rq := f.group()[0]
		if rq.State != Unknown {
			t.Errorf("%s from before the turn: state %s (%s)", phase, rq.State, rq.Why)
		}
	}
}

// A check whose result can no longer be read is reopened by an artifact that came after it.
func TestALostCheckIsReopenedByANewerArtifact(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.tool("s1", "", "Bash", at(1), `{"command":"git commit -am one"}`, "[main 1111111] one\n 1 file changed", false)
	f.phase("s1", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if _, err := f.st.CheckOnce(ctx, store.RequestCheck{Request: rq.ID, Result: "push:origin gone@2222222", At: at(3)}, nil); err != nil {
		t.Fatal(err)
	}
	f.tool("s1", "", "Bash", at(4), `{"command":"git commit -am two"}`, "[main 2222222] two\n 1 file changed", false)
	f.phase("s1", at(5), "phase", "idle", nil)
	f.put()
	reqs, err := Group(ctx, f.st, t0.Add(-time.Hour), Options{Redact: fakeRedact})
	if err != nil {
		t.Fatal(err)
	}
	if rq = reqs[0]; rq.State == Checked {
		t.Errorf("state %s (%s)", rq.State, rq.Why)
	}
}

// A whole read of a one-line output (an address alone, with no line end) has its line.
func TestAOneLineWholeReadHasItsLine(t *testing.T) {
	f := newFixture(t)
	path := claudeCall(t, f, t.TempDir(), "s-one-line", "gh pr create --fill", "https://github.com/o/r/pull/9")
	_ = path
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); got != "pr pull request https://github.com/o/r/pull/9" {
		t.Errorf("results %q", got)
	}
}
