package detect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/agent-sh/momentum/internal/store"
)

var t0 = time.Date(2026, 3, 2, 10, 0, 0, 0, time.Local)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func put(t *testing.T, st *store.Store, evs ...store.Event) {
	t.Helper()
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
}

var seq int

// ev is a synthetic event of Claude Code's; its key is new each time.
func ev(source, session, kind string, actor store.Actor, tool, summary, meta string, at time.Time) store.Event {
	seq++
	return store.Event{Source: source, SourceKey: fmt.Sprintf("%s@%06d", session, seq), TS: at, Session: session, Harness: "claude",
		Actor: actor, Kind: kind, Tool: tool, Summary: summary, Meta: meta}
}

func toolError(session, tool, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "tool_result", store.ActorTool, tool, text, `{"is_error":true,"tool_id":"x"}`, at)
}

// call and result are a tool call and its failed result, by the call's id.
func call(session, tool, id, input string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "tool_call", store.ActorAgent, tool, input, fmt.Sprintf(`{"tool_id":%q}`, id), at)
}

func result(session, tool, id, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "tool_result", store.ActorTool, tool, text, fmt.Sprintf(`{"is_error":true,"tool_id":%q}`, id), at)
}

func week() Window { return Window{Since: t0.Add(-24 * time.Hour), Until: t0.Add(6 * 24 * time.Hour)} }

func signatures(fs []store.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s %d", f.Signature, len(f.Evidence)))
	}
	return out
}

// Two sightings of one failure share a key: what varies between them (a path, a file's name, a quoted value, an
// address, a number, an id) is taken out, and so is the punctuation that ends it.
func TestNormKeysTwoSightingsAlike(t *testing.T) {
	for _, pair := range [][2]string{
		{"a match on 'dbtest.py' takes the shell too (pid 4312) in /home/u/x/run.sh at 0x7f3a2c1b.",
			"a match on 'bench.sh 12' takes the shell too (pid 77) in ~/y/z.py at 0x11aa22bb"},
		{"Error: can't open 'config.yaml'", "Error: can't open 'data.json'"},
		{"request req_011CUjQ9xYzAbC failed", "request req_9ZZqT7mmK2pL failed"},
		{"open internal/detect/testdata/a.json: permission denied", "open cmd/x/b.json: permission denied"},
		{"fatal: bad object 3f1c9e2", "fatal: bad object 9ab04de"},
		{"cat: notes.txt: No such file or directory", "cat: run-2.log: No such file or directory"},
		{"deploy@build-a.example: Permission denied (publickey)", "git@forge.example.org: Permission denied (publickey)"},
		{"cat: .venv/bin/activate: No such file or directory", "cat: venv/bin/activate: No such file or directory"},
		{"cat: ./venv/bin/activate: No such file or directory", "cat: .config/app/settings.toml: No such file or directory"},
		{"ls: cannot access ‘build’: No such file or directory", "ls: cannot access 'build': No such file or directory"},
		{"ls: cannot access “build”: No such file or directory", "ls: cannot access `build': No such file or directory"},
	} {
		if a, b := Norm(pair[0], 80), Norm(pair[1], 80); a != b {
			t.Errorf("%q and %q key apart: %q, %q", pair[0], pair[1], a, b)
		}
	}
	if got := Norm("a match on 'dbtest.py' takes the shell too (pid 4312) in /home/u/x/run.sh at 0x7f3a2c1b.", 80); got != "a match on Q takes the shell too (pid N) in P at H" {
		t.Errorf("norm %q", got)
	}
	if got := Norm("Error: can't open 'config.yaml'", 80); got != "error: can't open Q" {
		t.Errorf("an apostrophe in a word: %q", got)
	}
	if got := Norm("it's 2'x' and 'y'", 80); got != "it's N'x' and Q" {
		t.Errorf("a quote after a digit: %q", got)
	}
	if got := Norm("req xy12zw34 and xy12zw3 failed", 80); got != "req H and xyNzwN failed" {
		t.Errorf("an id of 8 and a word of 7: %q", got)
	}
	if got := Norm(strings.Repeat("word ", 40), 23); got != "word word word word" {
		t.Fatalf("cut %q", got)
	}
}

// A signature fits what the case store keeps: a key too long in bytes keeps its start and a short hash of the
// whole, so two long keys stay two.
func TestASignatureFitsTheStore(t *testing.T) {
	long := strings.Repeat("🟥", 90)
	a, b := signature("failures", "mcp:x:"+long), signature("failures", "mcp:x:"+long+"!")
	if len(a) > signatureMax || len(b) > signatureMax || a == b || !utf8.ValidString(a) {
		t.Fatalf("%d %d bytes, equal %v", len(a), len(b), a == b)
	}
	if s := signature("failures", "guard:x:short"); s != "failures/v2:guard:x:short" {
		t.Fatalf("a short key: %q", s)
	}
}

// A failure is keyed by where it came from and its words, as the store keeps a result: on one line. A hook's or a
// guard's refusal by the hook or guard; the harness's own refusal by the harness; an MCP server's, a Hermes or a
// Codex tool's error by the tool; a command's failure only by the message that holds a mark of the setup failing,
// once the harness's header is off it. The owner's no, a call cancelled beside another that failed, and a command
// whose output shows the work failing are no setup's failure. The guards, hooks and servers here are made up.
func TestAFailureIsKeyedByItsOriginAndWords(t *testing.T) {
	for _, c := range []struct{ harness, tool, text, want string }{
		{"claude", "Bash", "<tool_use_error>fence-guard: a retry loop with no bound: give it a limit and a way out",
			"guard:fence-guard:a retry loop with no bound: give it a limit and a way out"},
		{"claude", "Bash", "<tool_use_error>load-gate: held `make check`: the box is full, 31 jobs (limit 30).",
			"guard:load-gate:held Q: the box is full, N jobs (limit N)"},
		// a hook's deny names its guard in lower case; Claude Code's own messages there open with a capital
		{"claude", "Write", "<tool_use_error>ledger: this note names no source", "guard:ledger:this note names no source"},
		{"claude", "Write", "ledger: this note names no source", "tool:write:ledger: this note names no source"},
		{"claude", "Grep", "<tool_use_error>Error: No such tool available: Grep", "tool:grep:error: no such tool available: grep"},
		{"claude", "Write", "Held for the owner's review as item 7 by the ledger: it waits", "guard:ledger:held for the owner's review as item N by the ledger"},
		// a hook by the name its message opens with, the command in Claude Code's brackets, or none
		{"claude", "Bash", "PreToolUse:Bash hook error: deep-scan guard: `find /` reads every disk",
			"guard:deep-scan-guard:Q reads every disk"},
		{"claude", "Bash", "PreToolUse:Bash hook error: keeper-check: step B is not recorded: record it first", "guard:keeper-check:step b is not recorded: record it first"},
		{"claude", "Write", "PreToolUse:Write hook error: keeper-check: step B is not recorded: record it first", "guard:keeper-check:step b is not recorded: record it first"},
		{"claude", "Edit", "PreToolUse:Edit hook error: [~/.claude/hooks/fence.sh]: the edit touches a lock", "guard:fence:the edit touches a lock"},
		{"claude", "Edit", "PreToolUse:Edit hook error: Blocked by policy", "guard:hook:blocked by policy"},
		{"claude", "Bash", "PreToolUse:Bash hook error: Error: the command touches main", "guard:hook:error: the command touches main"},
		{"claude", "Bash", "PreToolUse:Bash hook error: Command blocked: the command touches main", "guard:hook:command blocked: the command touches main"},
		{"claude", "Bash", "PreToolUse:Bash hook error: the command was blocked: it touches main", "guard:hook:the command was blocked: it touches main"},
		// the harness's own refusals, by their kind
		{"claude", "Bash", "This agent is isolated in the worktree /w/a, but this command names git in a form too complex",
			"harness:claude:this agent is isolated in the worktree P, but this command names git in a form"},
		{"claude", "Bash", "Permission to use Bash with command rm -rf build has been denied.", "harness:claude:permission to use bash with command rm has been denied"},
		{"claude", "Edit", "Permission to use Edit has been denied because Claude Code is running in don't ask mode. Ask first.",
			"harness:claude:permission to use edit has been denied because claude code is running in don't"},
		{"claude", "Bash", "Blocked: sleep 30 followed by: cat /x/y.log", "harness:claude:blocked: sleep N followed by"},
		{"claude", "Bash", "Permission for this action was denied by the Claude Code auto mode classifier. Reason: it deletes a tree",
			"harness:claude:permission for this action was denied by the claude code auto mode classifier"},
		{"claude", "Bash", "This action was denied by the Claude Code auto mode classifier. Reason: it deletes a tree",
			"harness:claude:this action was denied by the claude code auto mode classifier"},
		{"claude", "Bash", "Permission to use Bash with command cd /srv/app && git push origin main has been denied.", "harness:claude:permission to use bash with command git has been denied"},
		{"claude", "Bash", "Permission to use Bash with command timeout 60 /usr/bin/curl -s x has been denied.", "harness:claude:permission to use bash with command curl has been denied"},
		{"claude", "mcp__web-tools__fetch", "tool failed: spawn failed for /usr/bin/x: spawn /usr/bin/x ENOENT",
			"mcp:web-tools:tool failed: spawn failed for P: spawn P enoent"},
		// Hermes: a tool's error, a guard inside one, a command that exits otherwise than 0, code that failed
		{"hermes", "write_file", `{"error": "BLOCKED: write to protected agent-instruction file(s) (AGENTS.md) approval prompt timed out."}`,
			"tool:write-file:blocked: write to protected agent-instruction file(s) (P) approval prompt timed"},
		{"hermes", "terminal", `{"output": "", "exit_code": -1, "error": "fence-guard: the command waits on nothing"}`, "guard:fence-guard:the command waits on nothing"},
		{"hermes", "terminal", `{"output": "== x: usage: fake-repo-check [-h] --repo REPO", "exit_code": 2, "error": null}`, "exec:usage: fake-repo-check"},
		{"claude", "Bash", "Exit code 2 usage: /opt/tools/bin/x-tool [-h] --repo REPO", "exec:usage: x-tool"},
		{"hermes", "terminal", `{"output": "ls: cannot access 'x': No such file or directory", "exit_code": 0, "error": null}`, ""},
		{"hermes", "terminal", `{"output": "cat: a.txt: No such file or directory and more output past the store's cut`, "exec:cat: P: no such file or directory"},
		{"hermes", "execute_code", "ModuleNotFoundError: No module named 'yaml'", "exec:modulenotfounderror: no module named"},
		{"hermes", "execute_code", `{"status": "error", "output": "Traceback (most recent call last):\n  File \"x.py\", line 3, in <module>\nModuleNotFoundError: No module named 'yaml'"}`,
			"exec:modulenotfounderror: no module named"},
		// Codex: a script's error, its commands under either header, the stdin it writes
		{"codex", "exec", `Script failed Wall time 0.1 seconds Output: Script error: exec_command failed: CreateProcess { message: "Rejected(rm -f)" }`,
			"tool:exec:exec_command failed: createprocess { message: Q }"},
		{"codex", "exec_command", "Chunk ID: 7f3a2c Wall time: 0.0521 seconds Process exited with code 127 Original token count: 12 Output: bash: line 1: rg: command not found",
			"exec:rg: command not found"},
		{"codex", "exec_command", "Chunk ID: 9b1e44 Wall time: 1.2034 seconds Process exited with code 1 Original token count: 30 Output: fatal: unable to access 'https://x/y.git/': Could not resolve host: x",
			"exec:fatal: unable to access Q: could not resolve host: x"},
		{"codex", "shell", "Exit code: 1 Wall time: 0.2 seconds Output: connection refused", "exec:connection refused"},
		{"codex", "exec_command", "Chunk ID: 1a Wall time: 0.1 seconds Process exited with code 1 Original token count: 3 Output: connection refused", "exec:connection refused"},
		{"pi", "bash_output", "ls: cannot access 'a.txt': No such file or directory", "exec:ls: cannot access Q: no such file or directory"},
		{"codex", "write_stdin", "Chunk ID: 1a Wall time: 1.0 seconds Process exited with code 1 Original token count: 3 Output: cat: a.txt: No such file or directory",
			"exec:cat: P: no such file or directory"},
		{"codex", "exec_command", "bash: rgg: command not found", "exec:rgg: command not found"},
		{"codex", "write_stdin", "cat: a.txt: No such file or directory", "exec:cat: P: no such file or directory"},
		{"claude", "Bash", "bash: rgg: command not found", "exec:rgg: command not found"},
		{"codex", "local_shell", "bash: rgg: command not found", "exec:rgg: command not found"},
		{"hermes", "terminal", "bash: rgg: command not found", "exec:rgg: command not found"},
		// pi's shell, whose results have no header
		{"pi", "bash", "--- FAIL: TestX (0.00s) x_test.go:9: got 1, want 2 FAIL", ""},
		{"pi", "bash", "ls: cannot access 'a.txt': No such file or directory", "exec:ls: cannot access Q: no such file or directory"},
		// Claude Code's commands, its header off
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", "exec:ls: cannot access Q: no such file or directory"},
		{"claude", "Bash", "Exit code 7 connection refused", "exec:connection refused"},
		{"claude", "Bash", "Exit code 1 tail: option used in invalid context -- 2", "exec:tail: option used in invalid context"},
		{"claude", "Bash", "Exit code 1 === RUN TestX --- PASS (0.00s) dial tcp: connection refused", "exec:tcp: connection refused"},
		{"claude", "Bash", "Exit code 1 cat: /x/y.txt: No such file or directory", "exec:cat: P: no such file or directory"},
		{"claude", "Bash", "Exit code 128 fatal: Unable to create '/r/.git/index.lock': File exists.", "exec:fatal: unable to create Q: file exists"},
		// Node's error code and its message, a mark in a quoted value, a usage line in colour and none
		{"claude", "Bash", "Exit code 1 Error: ENOENT: no such file or directory, open '/srv/app/a.json'", "exec:error: enoent: no such file or directory"},
		{"claude", "Bash", "Exit code 2 grep: 'connection refused': No such file or directory", "exec:grep: Q: no such file or directory"},
		{"claude", "Bash", "Exit code 1 Traceback (most recent call last): File \"x.py\", line 9, in <module> if \"no module named\" in err: raise", ""},
		{"claude", "Bash", "Exit code 2 usage: \x1b[1mx-tool\x1b[0m [-h] --repo REPO", "exec:usage: x-tool"},
		{"claude", "Bash", "Exit code 1 CPU usage: 12.5% memory usage: 40% check failed", ""},
		{"claude", "Bash", "Exit code 1 httpx.ConnectError: [Errno -3] Temporary failure in name resolution", "exec:httpx.connecterror: [errno -N] temporary failure in name resolution"},
		{"claude", "Bash", "Exit code 1 9lives: connection refused", "exec:connection refused"},
		{"claude", "Bash", "Exit code 1 x:y connection refused", "exec:connection refused"},
		{"claude", "Bash", "Exit code 1 x: failed(connection refused)", "exec:x: failed(connection refused"},
		{"claude", "Bash", "Exit code 127 zsh: command not found: rgg.", "exec:zsh: command not found: rgg"},
		{"claude", "Bash", "Exit code 127 zsh: command not found: " + strings.Repeat("x", 45), "exec:zsh: command not found: " + strings.Repeat("x", 40)},
		{"claude", "Read", "File does not exist. Note: your current working directory is /home/u.", "tool:read:file does not exist. note: your current working directory is P"},
		{"claude", "Bash", "<tool_use_error>Cancelled: parallel tool call Bash(go test) errored", ""},
		{"claude", "Edit", "<tool_use_error>Cancelled: parallel tool call Bash(go test) errored", ""},
		{"claude", "Bash", "The user doesn't want to take this action right now. STOP what you are doing", ""},
		{"claude", "Edit", "The user doesn't want to take this action right now. STOP what you are doing", ""},
		{"claude", "Edit", "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file).", ""},
		{"claude", "mcp__x__y", "The user doesn't want to proceed with this tool use. The tool use was rejected.", ""},
		{"claude", "Bash", "Permission for this tool use was denied. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). The user said: not now", ""},
	} {
		k, ok := FailureOf(c.harness, c.tool, c.text, "")
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %s %q:\n got %q\nwant %q", c.harness, c.tool, c.text, got, c.want)
		}
	}
}

// A failed command is keyed by the message that holds its mark, whatever surrounds it: a long path or a short one,
// what the output printed before the message and after the mark. The message starts at the name of the program
// the call ran (curl's and psql's messages hold more words that end in a colon), else at the name closest before
// the mark, and ends at the mark with the word after its colon; two programs' messages key apart.
func TestACommandsFailureIsKeyedByItsMessage(t *testing.T) {
	ls := "Exit code 2 ls: cannot access 'a.txt': No such file or directory"
	for _, set := range []struct {
		call, want string
		outs       []string
	}{
		{`{"command":"ls -la a.txt"}`, "exec:ls: cannot access Q: no such file or directory", []string{ls,
			ls + " ls: cannot access 'b.txt': No such file or directory", ls + " /home/u/x",
			"Exit code 2 ls: cannot access '/home/u/projects/some-project/a-long-directory-name/sub/file.txt': No such file or directory",
			"Exit code 2 building x step 3: done ls: cannot access 'a.txt': No such file or directory"}},
		{`{"command":"python3 run.py"}`, "exec:filenotfounderror: [errno N] no such file or directory: Q", []string{
			"Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: 'data/in.csv'",
			"Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: 'config'",
			"Exit code 1 Traceback (most recent call last): File \"run.py\", line 3, in <module> open('config') FileNotFoundError: [Errno 2] No such file or directory: 'config'"}},
		{`{"command":"curl -s http://localhost:8080/health"}`, "exec:curl: (N) failed to connect to localhost port N after N ms: connection refused", []string{
			"Exit code 7 curl: (7) Failed to connect to localhost port 8080 after 0 ms: Connection refused",
			"Exit code 7 curl: (7) Failed to connect to localhost port 9090 after 2 ms: Connection refused"}},
		{`{"command":"cd /x && psql -h localhost -c 'select 1'"}`, "exec:psql: error: connection to server at Q (N), port N failed: connection refused", []string{
			`Exit code 2 psql: error: connection to server at "localhost" (127.0.0.1), port 5432 failed: Connection refused`}},
		{`{"command":"go run ./cmd/server"}`, "exec:connect: connection refused", []string{
			"Exit code 1 dial tcp 127.0.0.1:5432: connect: connection refused",
			"Exit code 1 2026/03/02 10:00:00 dial tcp [::1]:5432: connect: connection refused"}},
		{`{"command":"git push"}`, "exec:permission denied", []string{
			"Exit code 128 git@github.com: Permission denied (publickey). fatal: Could not read from remote repository."}},
		{`{"command":"rgg x"}`, "exec:rgg: command not found", []string{"Exit code 127 /bin/bash: line 1: rgg: command not found"}},
		{`{"command":"rgg x"}`, "exec:zsh: command not found: rgg", []string{"Exit code 127 zsh: command not found: rgg"}},
		{`{"command":"poll-ready --file x && wc -l x"}`, "exec:wc: P: no such file or directory", []string{
			"Exit code 1 poll-ready: done after 3s wc: x/y: No such file or directory", "Exit code 1 wc: x/z: No such file or directory"}},
		// a program's name past the reach or a sentence's end opens another message; with no name, three words
		{`{"command":"ls x"}`, "exec:x: no such file or directory", []string{
			"Exit code 2 ls: " + strings.Repeat("word ", 25) + "x: No such file or directory", "Exit code 2 ls: done. x: No such file or directory"}},
		{`{"command":"run"}`, "exec:connection refused", []string{"Exit code 1 a b c d e connection refused", "Exit code 1 ok connection refused",
			"Exit code 1 x: " + strings.Repeat("word ", 25) + "connection refused"}},
		{`{"command":"ls x"}`, "exec:x: no such file or directory", []string{"Exit code 2 ls: done; x: No such file or directory",
			"Exit code 2 ls: done! x: No such file or directory", "Exit code 2 ls: done? x: No such file or directory"}},
		{`{"command":"poll-ready --file x; python3 run.py"}`, "exec:modulenotfounderror: no module named", []string{
			"Exit code 1 poll-ready: gave up after 60s ModuleNotFoundError: No module named 'yaml'", "Exit code 1 ModuleNotFoundError: No module named 'yaml'"}},
		{`{"command":"run"}`, "exec:sh: N: rgg: not found", []string{"Exit code 127 sh: 1: rgg: not found", "Exit code 127 sh: 7: rgg: not found"}},
	} {
		for _, out := range set.outs {
			k, ok := FailureOf("claude", "Bash", out, set.call)
			if !ok || k.String() != set.want {
				t.Errorf("%q after %s:\n got %q %v\nwant %q", out, set.call, k.String(), ok, set.want)
			}
		}
	}
	// without the call, a name in the output does not open the message: curl's is keyed by its last context
	if k, _ := FailureOf("claude", "Bash", "Exit code 7 curl: (7) Failed to connect to localhost port 8080 after 0 ms: Connection refused", ""); k.String() != "exec:ms: connection refused" {
		t.Errorf("curl's without its call: %q", k.String())
	}
	for _, pair := range [][2]string{
		{"Exit code 1 cat: a.txt: No such file or directory", "Exit code 1 wc: a.txt: No such file or directory"},
		{"Exit code 1 gh: Not Found (HTTP 404)", "Exit code 1 gh: Not Found (HTTP 403)"},
	} {
		a, _ := FailureOf("claude", "Bash", pair[0], "")
		b, _ := FailureOf("claude", "Bash", pair[1], "")
		if a == b {
			t.Errorf("two failures share a key: %q", a.String())
		}
	}
	if k, _ := FailureOf("claude", "Bash", "Exit code 1 x: no such file or directory", ""); k.Title() != "A command keeps failing: x: no such file or directory" {
		t.Errorf("title %q", k.Title())
	}
}

// Each mark of the setup failing keys a command's output; each mark of the work failing keeps one that also holds a
// setup's mark from being keyed.
func TestEveryMarkSaysWhoseFailureItIs(t *testing.T) {
	for _, out := range []string{
		"bash: x: command not found", "x: not found in PATH", "error: no such command: x", "cat: y: No such file or directory", "sh: 1: rgg: not found",
		"error: unknown option '--x'", "error: unknown flag: --x", "flag provided but not defined: -x", "error: unrecognized arguments: --x",
		"error: invalid choice: 'x'", "usage: x [-h]", "Error: accepts 1 arg(s), received 2", "Error: requires at least 1 arg(s), only received 0",
		"fatal: not a git repository", "open x: permission denied", "rm: x: Operation not permitted", "touch: x: Read-only file system",
		"write: No space left on device", "connect: connection reset by peer", "dial: connection timed out", "curl: (7) Couldn't connect to server",
		"curl: (6) Could not resolve host: x", "Temporary failure in name resolution", "ssh: x: Name or service not known",
		"bind: address already in use", "Error: connect ETIMEDOUT 10.0.0.1:443", "Error: getaddrinfo ENOTFOUND registry.example.org",
		"Error: connect ECONNREFUSED 127.0.0.1:80", "Error: read ECONNRESET", "getaddrinfo EAI_AGAIN x", "connect EHOSTUNREACH 10.0.0.9:22",
		"connect ENETUNREACH 10.0.0.9:22", "listen EADDRINUSE: :::3000", "fork: Cannot allocate memory", "fatal error: out of memory",
		"ModuleNotFoundError: No module named 'x'", "error obtaining VCS status: exit status 128", "API rate limit exceeded",
		"429 Too Many Requests", "gh: Not Found (HTTP 404)", "x509: certificate has expired", "certificate verify failed",
		"401 Unauthorized", "403 Forbidden", "authentication failed for x", "not logged in", "token expired", "tail: option used in invalid context -- 2",
		"Error: cannot open display: :0", "spawn x ENOENT", "error: spawn failed", "mkdir: x: File exists", "Error: database is locked",
		"E: Could not get lock /var/lib/dpkg/lock", "error: could not lock config file x", "could not acquire the lease",
		"Resource temporarily unavailable", "lock on x held by pid 12",
	} {
		if _, ok := FailureOf("claude", "Bash", "Exit code 1 "+out, ""); !ok {
			t.Errorf("no key: %q", out)
		}
	}
	for _, out := range []string{
		"--- FAIL: TestDial (0.00s) dial: connection refused",
		"FAIL src/a.test.ts ● reads › open x: no such file or directory",
		"ok a 0.1s FAIL example.com/x 0.02s connection refused",
		"panic: test timed out after 10m0s running tests: TestBig (10m0s) connection refused",
		"thread 'tests::reads' panicked at 'called `Result::unwrap()` on an `Err` value: Os { code: 2, kind: NotFound, message: \"No such file or directory\" }', src/lib.rs:10:5",
		"./x.go:12:3: undefined: thing no such file or directory",
		"error[E0425]: cannot find value `x` in this scope permission denied",
		"AssertionError: expected the file, got No such file or directory",
		"2 tests failed: connection refused",
		"==== 1 failed in 0.02s ==== connection refused",
		"FAILED tests/test_net.py::test_fetch - ConnectionRefusedError: [Errno 111] Connection refused",
		"Ran 1 test in 0.001s FAILED (errors=1) FileNotFoundError: [Errno 2] No such file or directory: 'x'",
		"test x ... FAILED No such file or directory test result: FAILED. 1 passed; 1 failed; 0 ignored",
		"● reads › fails ENOENT: no such file or directory, open 'x'",
		"Tests: 1 failed, 1 total ENOENT: no such file or directory, open 'x'",
		"collected 3 items ===== FAILURES ===== ____ test_reads ____ FileNotFoundError: [Errno 2] No such file or directory: 'x'",
		"FAIL: test_reads (tests.test_io.TestIO) FileNotFoundError: [Errno 2] No such file or directory: 'x'",
		"0 passing (5ms) 1 failing 1) reads: Error: ENOENT: no such file or directory",
		"✖ reads a file (1.2ms) Error: ENOENT: no such file or directory",
		"lint error: unused variable x, no such file or directory",
	} {
		if k, ok := FailureOf("claude", "Bash", "Exit code 1 "+out, ""); ok {
			t.Errorf("the work failing keyed as the setup's: %q: %q", out, k.String())
		}
	}
	// a crash is neither's
	if k, ok := FailureOf("claude", "Bash", "Exit code 139 segmentation fault (core dumped)", ""); ok {
		t.Errorf("a crash keyed: %q", k.String())
	}
}

// The programs a call runs are the first words of its commands, past assignments and wrappers, by their base names;
// a here-document's text is no command, and a call cut short names what it holds.
func TestACallNamesItsPrograms(t *testing.T) {
	for in, want := range map[string]string{
		`{"command":"cd /x && FOO=1 nice -n 10 timeout 5m ./bin/run --flag | tee out; sudo -n rm -f y"}`:    "cd rm run tee",
		`{"cmd":"python3 - <<'EOF'\nimport os\nEOF"}`:                                                       "python3",
		`{"command":"systemd-run --user --scope -p CPUWeight=20 -- rg x $(git rev-parse --show-toplevel)"}`: "git rg",
		`{"command":"if [ -f x ]; then cat x; fi && env A=b /usr/bin/ls -l`:                                 "[ cat fi ls",
		`{"session_id":3,"chars":""}`:                                                                       "",
	} {
		var got []string
		for p := range callPrograms(in) {
			got = append(got, p)
		}
		slices.Sort(got)
		if strings.Join(got, " ") != want {
			t.Errorf("%s: %q, want %q", in, strings.Join(got, " "), want)
		}
	}
}

// A failure is a finding when it recurs: three sightings in two sessions in the window, each command keyed with the
// call it answers. The mistakes log's row of a failure the transcript holds in the same session, of the same tool,
// within 5 seconds, counts once whatever text each kept; a row of its own (a deny, a model's refusal, a turn's
// error) counts.
func TestFailuresAreFoundWhenTheyRecur(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>fence-guard: a process match on 'x' takes its own shell too"
	curl := func(ms int) string {
		return fmt.Sprintf("Exit code 7 curl: (7) Failed to connect to localhost port 8080 after %d ms: Connection refused", ms)
	}
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s1", "Bash", deny, t0.Add(time.Hour)), toolError("s2", "Bash", deny, t0.Add(2*time.Hour)),
		// one session only: no finding
		toolError("s3", "Bash", "<tool_use_error>fence-guard: a wildcard delete", t0),
		toolError("s3", "Bash", "<tool_use_error>fence-guard: a wildcard delete", t0.Add(time.Minute)),
		toolError("s3", "Bash", "<tool_use_error>fence-guard: a wildcard delete", t0.Add(2*time.Minute)),
		// two sightings in two sessions: no finding
		toolError("s4", "Bash", "<tool_use_error>fence-guard: a private link in a commit message", t0),
		toolError("s5", "Bash", "<tool_use_error>fence-guard: a private link in a commit message", t0),
		// the work failing, not the setup
		toolError("s4", "Bash", "Exit code 1 --- FAIL: TestX", t0), toolError("s5", "Bash", "Exit code 1 --- FAIL: TestX", t0), toolError("s6", "Bash", "Exit code 1 --- FAIL: TestX", t0),
		// a result that is no error
		ev(store.Transcripts, "s6", "tool_result", store.ActorTool, "Bash", deny, `{"tool_id":"y"}`, t0.Add(3*time.Hour)),
		// curl's failures, keyed with the calls they answer; a call of the same id past a day before its result is
		// another's
		call("c1", "Bash", "t1", `{"command":"curl -s localhost:8080"}`, t0), result("c1", "Bash", "t1", curl(0), t0.Add(time.Second)),
		call("c1", "Bash", "t2", `{"command":"curl -s localhost:8080"}`, t0.Add(time.Hour)), result("c1", "Bash", "t2", curl(1), t0.Add(time.Hour+time.Second)),
		call("c2", "Bash", "t3", `{"command":"curl -s localhost:8080"}`, t0.Add(2*time.Hour)), result("c2", "Bash", "t3", curl(0), t0.Add(2*time.Hour+time.Second)),
		call("c3", "Bash", "t4", `{"command":"curl -s localhost:8080"}`, t0.Add(5*time.Hour)), result("c3", "Bash", "t4", curl(0), t0.Add(30*time.Hour)),
		result("c4", "Bash", "t5", curl(0), t0), call("c4", "Bash", "t5", `{"command":"curl -s localhost:8080"}`, t0.Add(time.Minute)),
		// the window's first result, its call ten minutes before it
		call("c5", "Bash", "t6", `{"command":"curl -s localhost:8080"}`, t0.Add(-30*time.Minute)), result("c5", "Bash", "t6", curl(0), t0.Add(-20*time.Minute)),
	)
	held := "load-gate: held `make`: the machine is busy"
	put(t, st,
		// the transcript's first deny again, 3 s on, its text the same: the same sighting
		ev("mistakes", "s1", "deny", store.ActorHarness, "Bash", "Bash: "+strings.TrimPrefix(deny, "<tool_use_error>"), "", t0.Add(3*time.Second)),
		// a refusal of the model, three times in two sessions
		ev("mistakes", "s7", "refusal", store.ActorAgent, "", "The model declined this request as written", "", t0),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "The model declined this request as written", "", t0.Add(time.Hour)),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "The model declined this request as written", "", t0.Add(2*time.Hour)),
		// a hold of the mistakes log's own: 6 s after a failure of the transcript's, of another tool beside one, and alone;
		// and one a second after the transcript's failure of its session and tool, which is that one
		ev("mistakes", "s1", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0.Add(6*time.Second)),
		ev("mistakes", "s2", "held", store.ActorHarness, "Write", "Write: "+held, "", t0.Add(2*time.Hour+time.Second)),
		ev("mistakes", "s9", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0),
		ev("mistakes", "s2", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0.Add(2*time.Hour+time.Second)),
		// a deny of a guard of one word, which only the log's kind says is a guard's; and the turns' errors
		ev("mistakes", "s10", "deny", store.ActorHarness, "Write", "Write: ledger: this note names no source", "", t0),
		ev("mistakes", "s11", "deny", store.ActorHarness, "Write", "Write: ledger: this note names no source", "", t0),
		ev("mistakes", "s11", "deny", store.ActorHarness, "Write", "Write: ledger: this note names no source", "", t0.Add(time.Minute)),
		ev("mistakes", "s12", "turn-error", store.ActorHarness, "", "API Error: 529 overloaded", "", t0),
		ev("mistakes", "s13", "turn-error", store.ActorHarness, "", "API Error: 529 overloaded", "", t0),
		ev("mistakes", "s13", "turn-error", store.ActorHarness, "", "API Error: 529 overloaded", "", t0.Add(time.Minute)),
	)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"failures/v2:exec:curl: (N) failed to connect to localhost port N after N ms: connection refused 4",
		"failures/v2:guard:fence-guard:a process match on Q takes its own shell too 3",
		"failures/v2:guard:ledger:this note names no source 3",
		"failures/v2:guard:load-gate:held Q: the machine is busy 3",
		"failures/v2:model:refusal:the model declined this request as written 3",
		"failures/v2:turn:claude:api error: N overloaded 3",
	}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fs[1].Title != "fence-guard keeps refusing calls: a process match on Q takes its own shell too" || fs[1].Family != "failures" {
		t.Fatalf("finding %+v", fs[1])
	}
	if fs[5].Title != "Claude Code's turns keep failing: api error: N overloaded" {
		t.Fatalf("a turn's error: %q", fs[5].Title)
	}
	if !strings.HasPrefix(fs[1].Evidence[0].Ref, "ev:") || !fs[1].Evidence[0].At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("evidence, newest first: %+v", fs[1].Evidence)
	}
}

// fileTools makes a file tool's calls and their results as the transcripts keep them, each call with an id of its
// own.
type fileTools struct {
	harness, read, write string
	n                    int
}

func (f *fileTools) pair(session, tool, input, text string, failed bool, at time.Time) []store.Event {
	f.n++
	id := fmt.Sprintf("%s-%d", tool, f.n)
	c, r := call(session, tool, id, input, at), result(session, tool, id, text, at.Add(time.Second))
	if !failed {
		r.Meta = fmt.Sprintf(`{"tool_id":%q}`, id)
	}
	c.Harness, r.Harness = f.harness, f.harness
	return []store.Event{c, r}
}

// claudeFiles are Claude Code's Read and Write.
var claudeFiles = fileTools{harness: "claude", read: "Read", write: "Write"}

const claudeMissing = "<tool_use_error>File does not exist. Note: your current working directory is /w.</tool_use_error>"

// readMissing is Claude Code's Read of a file that is not there; wrote, its Write of one.
func (f *fileTools) readMissing(session, path string, at time.Time) []store.Event {
	return f.pair(session, f.read, fmt.Sprintf(`{"file_path":%q}`, path), claudeMissing, true, at)
}

func (f *fileTools) wrote(session, path string, at time.Time) []store.Event {
	return f.pair(session, f.write, fmt.Sprintf(`{"file_path":%q,"content":"x"}`, path), "File created successfully at: "+path, false, at)
}

// A batch's sessions each read the file they are about to create: a read that found no file, of a path its own
// session then writes with success, is no failure. Each case holds such reads beside what must still count, so
// without the rule it finds more.
func TestAReadOfAFileItsSessionThenCreatesIsNoFailure(t *testing.T) {
	missing := "failures/v2:tool:read:file does not exist. note: your current working directory is P"
	for _, tc := range []struct {
		name   string
		events func(f *fileTools) []store.Event
		want   []string
	}{
		{"its own session writes it", func(f *fileTools) []store.Event {
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), f.wrote("s1", "/w/out/a.md", t0.Add(5*time.Minute)),
				f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)), f.readMissing("s2", "/w/out/c.md", t0.Add(2*time.Hour)))
		}, nil},
		{"two sessions each write their own", func(f *fileTools) []store.Event {
			return slices.Concat(f.readMissing("s1", "/w/out/s1.md", t0), f.readMissing("s1", "/w/out/s1.md", t0.Add(time.Minute)),
				f.wrote("s1", "/w/out/s1.md", t0.Add(5*time.Minute)),
				f.readMissing("s2", "/w/out/s2.md", t0), f.wrote("s2", "/w/out/s2.md", t0.Add(4*time.Minute)))
		}, nil},
		{"the mistakes log keeps the reads too", func(f *fileTools) []store.Event {
			logged := func(session string, at time.Time) store.Event {
				return ev("mistakes", session, "tool-error", store.ActorHarness, "Read", "Read: "+claudeMissing, "", at.Add(3*time.Second))
			}
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), []store.Event{logged("s1", t0)}, f.wrote("s1", "/w/out/a.md", t0.Add(time.Minute)),
				f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)), []store.Event{logged("s1", t0.Add(time.Hour))},
				f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				f.readMissing("s2", "/w/out/c.md", t0), []store.Event{logged("s2", t0)}, f.wrote("s2", "/w/out/c.md", t0.Add(time.Minute)))
		}, nil},
		{"a sibling subagent writes it", func(f *fileTools) []store.Event {
			as := func(agent string, evs []store.Event) []store.Event {
				for i := range evs {
					evs[i].Agent = agent
				}
				return evs
			}
			return slices.Concat(as("a1", f.readMissing("s1", "/w/out/a.md", t0)), as("a2", f.wrote("s1", "/w/out/a.md", t0.Add(time.Minute))),
				as("a1", f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour))), as("a2", f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute))),
				as("a1", f.readMissing("s2", "/w/out/c.md", t0)), as("a2", f.wrote("s2", "/w/out/c.md", t0.Add(time.Minute))),
				as("a3", f.readMissing("s3", "/w/out/d.md", t0)), as("a3", f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute))))
		}, []string{missing + " 3"}},
		{"the parent writes its subagent's read", func(f *fileTools) []store.Event {
			as := func(agent string, evs []store.Event) []store.Event {
				for i := range evs {
					evs[i].Agent = agent
				}
				return evs
			}
			return slices.Concat(as("a1", f.readMissing("s1", "/w/out/a.md", t0)), f.wrote("s1", "/w/out/a.md", t0.Add(time.Minute)),
				as("a1", f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour))), f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				as("a1", f.readMissing("s2", "/w/out/c.md", t0)), f.wrote("s2", "/w/out/c.md", t0.Add(time.Minute)),
				as("a3", f.readMissing("s3", "/w/out/d.md", t0)), as("a3", f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute))))
		}, []string{missing + " 3"}},
		{"another session writes it", func(f *fileTools) []store.Event {
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)),
				f.readMissing("s2", "/w/out/c.md", t0.Add(2*time.Hour)),
				f.wrote("s3", "/w/out/a.md", t0.Add(3*time.Hour)), f.wrote("s3", "/w/out/b.md", t0.Add(3*time.Hour)),
				f.wrote("s3", "/w/out/c.md", t0.Add(3*time.Hour)),
				f.readMissing("s3", "/w/out/d.md", t0.Add(4*time.Hour)), f.wrote("s3", "/w/out/d.md", t0.Add(5*time.Hour)))
		}, []string{missing + " 3"}},
		{"access refused", func(f *fileTools) []store.Event {
			denied := func(session, path string, at time.Time) []store.Event {
				return f.pair(session, "Read", fmt.Sprintf(`{"file_path":%q}`, path), "<tool_use_error>EACCES: permission denied, open '"+path+"'</tool_use_error>", true, at)
			}
			return slices.Concat(denied("s1", "/w/out/a.md", t0), f.wrote("s1", "/w/out/a.md", t0.Add(time.Minute)),
				denied("s1", "/w/out/b.md", t0.Add(time.Hour)), f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				denied("s2", "/w/out/c.md", t0), f.wrote("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)),
				f.readMissing("s4", "/w/out/e.md", t0), f.wrote("s4", "/w/out/e.md", t0.Add(time.Minute)),
				f.readMissing("s4", "/w/out/f.md", t0.Add(time.Hour)), f.wrote("s4", "/w/out/f.md", t0.Add(time.Hour+time.Minute)))
		}, []string{"failures/v2:tool:read:eacces: permission denied, open Q 3"}},
		{"the owner declines the write", func(f *fileTools) []store.Event {
			declined := func(session, path string, at time.Time) []store.Event {
				return f.pair(session, "Write", fmt.Sprintf(`{"file_path":%q,"content":"x"}`, path),
					"The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file).", true, at)
			}
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), declined("s1", "/w/out/a.md", t0.Add(time.Minute)),
				f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)), declined("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				f.readMissing("s2", "/w/out/c.md", t0), declined("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)))
		}, []string{missing + " 3"}},
		{"the write fails", func(f *fileTools) []store.Event {
			refused := func(session, path string, at time.Time) []store.Event {
				return f.pair(session, "Write", fmt.Sprintf(`{"file_path":%q,"content":"x"}`, path), "<tool_use_error>EACCES: permission denied, open '"+path+"'</tool_use_error>", true, at)
			}
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), refused("s1", "/w/out/a.md", t0.Add(time.Minute)),
				f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)), refused("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				f.readMissing("s2", "/w/out/c.md", t0), refused("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)))
		}, []string{missing + " 3", "failures/v2:tool:write:eacces: permission denied, open Q 3"}},
		{"the write came first", func(f *fileTools) []store.Event {
			return slices.Concat(f.wrote("s1", "/w/out/a.md", t0), f.readMissing("s1", "/w/out/a.md", t0.Add(time.Minute)),
				f.wrote("s1", "/w/out/b.md", t0), f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)),
				f.wrote("s2", "/w/out/c.md", t0), f.readMissing("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)))
		}, []string{missing + " 3"}},
		{"a guard refuses the read", func(f *fileTools) []store.Event {
			refused := func(session, path string, at time.Time) []store.Event {
				return f.pair(session, "Read", fmt.Sprintf(`{"file_path":%q}`, path), "<tool_use_error>read-guard: path not found in the allowlist</tool_use_error>", true, at)
			}
			return slices.Concat(refused("s1", "/w/out/a.md", t0), f.wrote("s1", "/w/out/a.md", t0.Add(time.Minute)),
				refused("s1", "/w/out/b.md", t0.Add(time.Hour)), f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				refused("s2", "/w/out/c.md", t0), f.wrote("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)),
				f.readMissing("s4", "/w/out/e.md", t0), f.wrote("s4", "/w/out/e.md", t0.Add(time.Minute)),
				f.readMissing("s4", "/w/out/f.md", t0.Add(time.Hour)), f.wrote("s4", "/w/out/f.md", t0.Add(time.Hour+time.Minute)))
		}, []string{"failures/v2:guard:read-guard:path not found in the allowlist 3"}},
		{"another tool's not-found error", func(f *fileTools) []store.Event {
			searched := func(session, path string, at time.Time) []store.Event {
				return f.pair(session, "Grep", fmt.Sprintf(`{"path":%q,"pattern":"x"}`, path), "<tool_use_error>Path does not exist: "+path+"</tool_use_error>", true, at)
			}
			return slices.Concat(searched("s1", "/w/out/a.md", t0), f.wrote("s1", "/w/out/a.md", t0.Add(time.Minute)),
				searched("s1", "/w/out/b.md", t0.Add(time.Hour)), f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				searched("s2", "/w/out/c.md", t0), f.wrote("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)),
				f.readMissing("s4", "/w/out/e.md", t0), f.wrote("s4", "/w/out/e.md", t0.Add(time.Minute)),
				f.readMissing("s4", "/w/out/f.md", t0.Add(time.Hour)), f.wrote("s4", "/w/out/f.md", t0.Add(time.Hour+time.Minute)))
		}, []string{"failures/v2:tool:grep:path does not exist: P 3"}},
		{"a tool that only reads names the path", func(f *fileTools) []store.Event {
			searched := func(session, path string, at time.Time) []store.Event {
				return f.pair(session, "Grep", fmt.Sprintf(`{"path":%q,"pattern":"x"}`, path), "No matches found", false, at)
			}
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), searched("s1", "/w/out/a.md", t0.Add(time.Minute)),
				f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)), searched("s1", "/w/out/b.md", t0.Add(time.Hour+time.Minute)),
				f.readMissing("s2", "/w/out/c.md", t0), searched("s2", "/w/out/c.md", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)))
		}, []string{missing + " 3"}},
		{"another path", func(f *fileTools) []store.Event {
			return slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), f.wrote("s1", "/w/out/./a.md", t0.Add(time.Minute)),
				f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)), f.wrote("s1", "/w/out/b.md.tmp", t0.Add(time.Hour+time.Minute)),
				f.readMissing("s2", "/w/out/c.md", t0), f.wrote("s2", "/w/out", t0.Add(time.Minute)),
				f.readMissing("s3", "/w/out/d.md", t0), f.wrote("s3", "/w/out/d.md", t0.Add(time.Minute)))
		}, []string{missing + " 3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := claudeFiles
			put(t, st, tc.events(&f)...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if err != nil {
				t.Fatal(err)
			}
			if got := signatures(fs); !slices.Equal(got, tc.want) {
				t.Fatalf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// Each harness's file tools: its read's own not-found error, of a path its session then writes, is no failure.
func TestAReadBeforeACreateAcrossHarnesses(t *testing.T) {
	for _, tc := range []struct {
		harness, read, write, readIn, missing, writeIn, wrote string
	}{
		{"claude", "Read", "Edit", `{"file_path":"/w/@"}`, claudeMissing, `{"replace_all":false,"file_path":"/w/@","old_string":"","new_string":"x"}`, "The file /w/@ has been updated successfully."},
		{"kimi", "Read", "Write", `{"path":"/w/@"}`, `"/w/@" does not exist.`, `{"content":"x","path":"/w/@"}`, "Wrote 1 bytes to /w/@"},
		{"hermes", "read_file", "write_file", `{"limit":200,"path":"/w/@"}`, `{"error": "File not found: /w/@"}`, `{"path":"/w/@","content":"x"}`, `{"bytes_written": 1, "dirs_created": true}`},
		{"hermes", "read_file", "patch", `{"path":"/w/@"}`, `{"error": "File not found: /w/@"}`, `{"mode":"replace","path":"/w/@"}`, `{"success": true, "diff": "x"}`},
		{"opencode", "read", "write", `{"filePath":"/w/@"}`, "File not found: /w/@", `{"filePath":"/w/@","content":"x"}`, "Created file successfully: /w/@"},
		{"pi", "read", "write", `{"path":"/w/@"}`, "ENOENT: no such file or directory, access '/w/@'", `{"path":"/w/@","content":"x"}`, "Successfully wrote 1 bytes to /w/@"},
		{"pi", "read", "edit", `{"path":"/w/@"}`, "ENOENT: '/w/@'", `{"path":"/w/@","oldText":"","newText":"x"}`, "Successfully replaced text in /w/@"},
		{"hermes", "read_file", "write_file", `{"path":"/w/@"}`, `{"error": "[Errno 2] No such file or directory: '/w/@'"}`, `{"path":"/w/@","content":"x"}`, `{"bytes_written": 1}`},
		{"claude", "Read", "NotebookEdit", `{"file_path":"/w/@"}`, claudeMissing, `{"notebook_path":"/w/@","new_source":"x","edit_mode":"insert"}`, "Inserted cell"},
		{"claude", "Read", "MultiEdit", `{"file_path":"/w/@"}`, claudeMissing, `{"file_path":"/w/@","edits":[{"old_string":"","new_string":"x"}]}`, "Applied 1 edit to /w/@"},
	} {
		t.Run(tc.harness+"/"+tc.write, func(t *testing.T) {
			st := testStore(t)
			f := fileTools{harness: tc.harness}
			var evs []store.Event
			for i, session := range []string{"s1", "s1", "s2"} {
				p := fmt.Sprintf("out/%d.md", i)
				at := t0.Add(time.Duration(i) * time.Hour)
				evs = append(evs, f.pair(session, tc.read, strings.ReplaceAll(tc.readIn, "@", p), strings.ReplaceAll(tc.missing, "@", p), true, at)...)
				evs = append(evs, f.pair(session, tc.write, strings.ReplaceAll(tc.writeIn, "@", p), strings.ReplaceAll(tc.wrote, "@", p), false, at.Add(time.Minute))...)
			}
			put(t, st, evs...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if err != nil || len(fs) != 0 {
				t.Fatalf("found %v, %v", signatures(fs), err)
			}
			// the same reads, the write saying it failed or the store's cut leaving out its path: a failure each
			st = testStore(t)
			evs = evs[:0]
			for i, session := range []string{"s1", "s1", "s2"} {
				p := fmt.Sprintf("out/%d.md", i)
				at := t0.Add(time.Duration(i) * time.Hour)
				in, out := strings.ReplaceAll(tc.writeIn, "@", p), strings.ReplaceAll(tc.wrote, "@", p)
				if i == 0 {
					in = `{"content":"` + strings.Repeat("x", 240) + `…`
				} else if strings.HasPrefix(out, "{") {
					out = `{"success": false, "error": "Could not find a match"}`
				} else {
					in = `{"command":"x"}`
				}
				evs = append(evs, f.pair(session, tc.read, strings.ReplaceAll(tc.readIn, "@", p), strings.ReplaceAll(tc.missing, "@", p), true, at)...)
				evs = append(evs, f.pair(session, tc.write, in, out, false, at.Add(time.Minute))...)
			}
			put(t, st, evs...)
			if fs, err := (Failures{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 1 || len(fs[0].Evidence) != 3 {
				t.Fatalf("found %v, %v", signatures(fs), err)
			}
		})
	}
}

// shellCall is a Bash call of Kimi's and its result as the store keeps them, in cwd: the input JSON with > escaped,
// cut to 240 bytes and marked so. An input that is already JSON (it opens with {") is kept as it is.
func shellCall(f *fileTools, session, cwd, command string, failed bool, at time.Time) []store.Event {
	in := command
	if !strings.HasPrefix(command, `{"`) {
		b, _ := json.Marshal(map[string]string{"command": command})
		if len(b) > 240 {
			b = append(b[:240], "…"...)
		}
		in = string(b)
	}
	evs := f.pair(session, "Bash", in, "", failed, at)
	for i := range evs {
		evs[i].Cwd = cwd
	}
	return evs
}

// kimiReadMissing is Kimi's Read of a file that is not there.
func kimiReadMissing(f *fileTools, session, path string, at time.Time) []store.Event {
	return f.pair(session, "Read", fmt.Sprintf(`{"path":%q}`, path), fmt.Sprintf("%q does not exist.", path), true, at)
}

// A session can create the file it looked for with its shell, but only a form the whitelist holds counts: cat > FILE,
// cat >> FILE or tee [-a] FILE, one here-document at most, a single leading cd at most. Each line is spelled so one
// clause decides: a write of the read's path, or none. Three reads in two sessions, each followed by the line in its
// own session, make a finding only when the line writes no file there.
func TestAShellLineWritesOnlyInTheWhitelistedForms(t *testing.T) {
	body := "\n" + strings.Repeat(`{"id":1,"verdict":"keep"}`+"\n", 20) + "EOF"
	const x = "/w/out/x.jsonl"
	for _, tc := range []struct {
		name, cwd, path, line, result string
		failed, writes                bool
	}{
		{"cat > and a here-document", "", x, "cat > /w/out/x.jsonl <<'EOF'" + body, "", false, true},
		{"cat >> and a here-document", "", x, "cat >> /w/out/x.jsonl <<'EOF'" + body, "", false, true},
		{"the here-document first, the file quoted", "", x, `cat <<'EOF' > "/w/out/x.jsonl"` + body, "", false, true},
		{"cat > alone", "", x, "cat > /w/out/x.jsonl", "", false, true},
		{"no space before the file", "", x, "cat >/w/out/x.jsonl", "", false, true},
		{"tee and a here-document", "", x, "tee /w/out/x.jsonl <<'EOF'" + body, "", false, true},
		{"tee -a", "", x, "tee -a /w/out/x.jsonl <<'EOF'\n{\"id\":1}\nEOF", "", false, true},
		{"in the directory a leading cd enters", "/w", x, "cd /w/out && cat > x.jsonl <<'EOF'\n{\"id\":1}\nEOF", "", false, true},
		{"in the call's directory", "/w/out", x, "cat >> x.jsonl <<'EOF'\n{\"id\":1}\nEOF", "", false, true},
		{"single quotes", "", x, "cat > '/w/out/x.jsonl'", "", false, true},
		{"an escaped letter", "", x, `cat > /w/out/x\.jsonl`, "", false, true},
		{"an escaped $ in double quotes", "", "/w/out/$x.jsonl", `cat > "/w/out/\$x.jsonl"`, "", false, true},
		{"cleaned", "", x, "cat > /w/out//x.jsonl", "", false, true},
		{"a here-document whose end is indented", "", x, "cat > /w/out/x.jsonl <<-EOF\n\t{\"id\":1}\n\tEOF", "", false, true},
		{"a here-document and nothing after it", "", x, "cat > /w/out/x.jsonl <<'EOF'\n{\"id\":1}\nEOF\n", "", false, true},
		{"a blank line after the here-document", "", x, "cat > /w/out/x.jsonl <<'EOF'\n{\"id\":1}\nEOF\n   \n", "", false, true},
		{"cut in a later field", "", x, `{"command":"cat > /w/out/x.jsonl","description":"` + strings.Repeat("d", 200) + `…`, "", false, true},
		{"cut inside an escape in the here-document", "", x, `{"command":"cat > /w/out/x.jsonl <<'EOF'\n{\"a\":\"b \u00…`, "", false, true},
		{"another file's error in the result", "", x, "cat > /w/out/x.jsonl", "cat: other.txt: No such file or directory", false, true},
		{"a tab-led end does not end a << here-document", "", x, "cat > /w/out/x.jsonl <<EOF\n{\"id\":1}\n\tEOF\necho done", "", false, true},
		{"an escaped quote in double quotes", "", `/w/out/a"b.jsonl`, `cat > "/w/out/a\"b.jsonl"`, "", false, true},
		{"an escaped backslash in double quotes", "", `/w/out/a\b.jsonl`, `cat > "/w/out/a\\b.jsonl"`, "", false, true},
		{"an escaped backtick in double quotes", "", "/w/out/a`b.jsonl", "cat > \"/w/out/a\\`b.jsonl\"", "", false, true},
		{"a plain backslash in double quotes", "", `/w/out/a\b.jsonl`, `cat > "/w/out/a\b.jsonl"`, "", false, true},

		{"the command failed", "", x, "cat > /w/out/x.jsonl <<'EOF'" + body, "", true, false},
		{"an error opening the file", "", x, "cat > /w/out/x.jsonl", "bash: /w/out/x.jsonl: No such file or directory", false, false},
		{"a read-only file system", "", x, "cat > /w/out/x.jsonl", "bash: /w/out/x.jsonl: Read-only file system", false, false},
		{"not a directory", "", x, "cat > /w/out/x.jsonl", "bash: /w/out/x.jsonl: Not a directory", false, false},
		{"a directory", "", x, "cat > /w/out/x.jsonl", "bash: /w/out/x.jsonl: Is a directory", false, false},
		{"permission denied", "", x, "cat > /w/out/x.jsonl", "bash: /w/out/x.jsonl: Permission denied", false, false},
		{"cannot create", "", x, "cat > /w/out/x.jsonl", "sh: 1: cannot create /w/out/x.jsonl: Directory nonexistent", false, false},
		{"no space left", "", x, "tee /w/out/x.jsonl <<'EOF'\nx\nEOF", "tee: /w/out/x.jsonl: No space left on device", false, false},
		{"cancelled beside a failed call", "", x, "cat > /w/out/x.jsonl", "Cancelled: parallel tool call Bash(ls) errored", true, false},
		{"cancelled with no flag", "", x, "cat > /w/out/x.jsonl", "Tool execution cancelled by user", false, false},
		{"a comment word", "/w", "/w/#x", "tee #x", "", false, false},
		{"a comment after a cd", "/w", "/w/out/#c", "cd /w/out && tee #c", "", false, false},
		{"a command on the next line", "", x, "cat > /w/out/x.jsonl\necho ok", "", false, false},
		{"a command after the here-document", "", x, "cat > /w/out/x.jsonl <<'EOF'\n{\"id\":1}\nEOF\necho done", "", false, false},
		{"an indented end, then a command", "", x, "cat > /w/out/x.jsonl <<-EOF\n\t{\"id\":1}\n\tEOF\necho done", "", false, false},
		{"the store cut right after the line", "", x, `{"command":"cat > /w/out/x.jsonl\n\u00…`, "", false, false},
		{"the store cut the line", "", x, `{"command":"cat > /w/out/x.jsonl \u00…`, "", false, false},
		{"a here-document's end, then the cut", "", x, `{"command":"cat > /w/out/x.jsonl <<'EOF'\nx\nEOF\nech…`, "", false, false},
		{"another program", "", x, "python3 gen.py > /w/out/x.jsonl", "", false, false},
		{"cat reads a file", "", x, "cat in.txt > /w/out/x.jsonl", "", false, false},
		{"a command before it", "", x, "mkdir -p /w/out && cat > /w/out/x.jsonl", "", false, false},
		{"a command after it", "", x, "cat > /w/out/x.jsonl; true", "", false, false},
		{"or", "", x, "cat > /w/out/x.jsonl || true", "", false, false},
		{"in the background", "", x, "cat > /w/out/x.jsonl &", "", false, false},
		{"a pipe into tee", "", x, "python3 gen.py | tee /w/out/x.jsonl", "", false, false},
		{"negated", "", x, "! cat > /w/out/x.jsonl", "", false, false},
		{"timed", "", x, "time -p cat > /w/out/x.jsonl", "", false, false},
		{"eval", "/w", x, "eval cd /w/out && cat > x.jsonl", "", false, false},
		{"source", "", x, "source env.sh && cat > /w/out/x.jsonl", "", false, false},
		{"a dot", "", x, ". env.sh && cat > /w/out/x.jsonl", "", false, false},
		{"exec", "", x, "exec cat > /w/out/x.jsonl", "", false, false},
		{"a subshell", "", x, "(cat > /w/out/x.jsonl)", "", false, false},
		{"a group", "", x, "{ cat > /w/out/x.jsonl; }", "", false, false},
		{"another descriptor", "", x, "cat 2> /w/out/x.jsonl", "", false, false},
		{"both outputs", "", x, "cat &> /w/out/x.jsonl", "", false, false},
		{"noclobber's >|", "", x, "cat >| /w/out/x.jsonl", "", false, false},
		{"a here-string", "", x, "cat <<< x > /w/out/x.jsonl", "", false, false},
		{"two files", "", "/w/out/y.jsonl", "cat > /w/out/x.jsonl > /w/out/y.jsonl", "", false, false},
		{"two here-documents", "", x, "cat > /w/out/x.jsonl <<A <<B\na\nA\nb\nB", "", false, false},
		{"tee and two files", "", "/w/out/y.jsonl", "tee /w/out/x.jsonl /w/out/y.jsonl", "", false, false},
		{"tee's other option", "", x, "tee -i /w/out/x.jsonl", "", false, false},
		{"tee -a twice", "", x, "tee -a -a /w/out/x.jsonl", "", false, false},
		{"tee with a redirection", "", x, "tee > /w/out/x.jsonl", "", false, false},
		{"cat with no file", "", x, "cat <<'EOF'\n> /w/out/x.jsonl\nEOF", "", false, false},
		{"a comment", "", x, "cat > /w/out/x.jsonl # note", "", false, false},
		{"a continued line", "", x, "cat \\\n> /w/out/x.jsonl", "", false, false},
		{"an open quote", "", x, "cat > '/w/out/x.jsonl", "", false, false},
		{"a variable", "", "/w/out/$x.jsonl", "cat > /w/out/$x.jsonl", "", false, false},
		{"a quoted variable", "", "/w/out/$x.jsonl", `cat > "/w/out/$x.jsonl"`, "", false, false},
		{"a quoted command's output", "", "/w/out/`date`.jsonl", "cat > \"/w/out/`date`.jsonl\"", "", false, false},
		{"a command's output", "", "/w/out/`date`.jsonl", "cat > /w/out/`date`.jsonl", "", false, false},
		{"a * glob", "", "/w/out/x*.jsonl", "cat > /w/out/x*.jsonl", "", false, false},
		{"a ? glob", "", "/w/out/x?.jsonl", "cat > /w/out/x?.jsonl", "", false, false},
		{"a [ glob", "", "/w/out/x[1].jsonl", "cat > /w/out/x[1].jsonl", "", false, false},
		{"a brace expansion", "", "/w/out/{a,b}.jsonl", "cat > /w/out/{a,b}.jsonl", "", false, false},
		{"home's ~", "/w", "/w/~/out/x.jsonl", "cat > ~/out/x.jsonl", "", false, false},
		{"an empty file name", "/w/out", "/w/out", "cat > ''", "", false, false},
		{"a .. in the file", "", x, "cat > /w/a/../out/x.jsonl", "", false, false},
		{"a .. in the directory", "/w", x, "cd /w/a/.. && cat > out/x.jsonl", "", false, false},
		{"a relative cd", "/w", "out/x.jsonl", "cd out && cat > x.jsonl", "", false, false},
		{"a cd with more words", "/w", x, "cd /w/out x cat > x.jsonl", "", false, false},
		{"a directory that is no whole path", "w/out", "w/out/x.jsonl", "cat > x.jsonl", "", false, false},
		{"echo", "", x, "echo > /w/out/x.jsonl", "", false, false},
		{"a here-document with no word", "", x, "cat > /w/out/x.jsonl <<", "", false, false},
		{"tee's -a after the file", "", x, "tee /w/out/x.jsonl -a", "", false, false},
		{"tee's other option alone", "/w/out", "/w/out/-i", "tee -i", "", false, false},
		{"a backslash at the line's end", "", x, "cat > /w/out/x.jsonl\\", "", false, false},
		{"a tab", "", x, "cat >\t/w/out/x.jsonl", "", false, true},
		{"a cd to a variable", "/w", "/w/$d/x.jsonl", "cd /w/$d && cat > x.jsonl", "", false, false},
		{"two cds", "/w", x, "cd /w && cd /w/out && cat > x.jsonl", "", false, false},
		{"a cd and ;", "/w", x, "cd /w/out; cat > x.jsonl", "", false, false},
		{"a pushd", "/w", x, "pushd /w/out && cat > x.jsonl", "", false, false},
		{"no directory", "", "x.jsonl", "cat > x.jsonl", "", false, false},
		{"a cd with nothing after it", "/w", "/w/out", "cd /w/out &&", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := fileTools{harness: "kimi"}
			var evs []store.Event
			for i, session := range []string{"s1", "s1", "s2"} {
				at := t0.Add(time.Duration(i) * time.Hour)
				evs = append(evs, kimiReadMissing(&f, session, tc.path, at)...)
				call := shellCall(&f, session, tc.cwd, tc.line, tc.failed, at.Add(time.Minute))
				call[1].Summary = tc.result
				evs = append(evs, call...)
			}
			put(t, st, evs...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if err != nil {
				t.Fatal(err)
			}
			if tc.writes && len(fs) != 0 || !tc.writes && (len(fs) != 1 || len(fs[0].Evidence) != 3) {
				t.Fatalf("writes %v, found %v", tc.writes, signatures(fs))
			}
		})
	}
}

// A relative file is read in the directory the shell call ran in: the workdir the call names, else the directory
// its session was in when it made the call, never the one its result reports. Hermes's terminal keeps a directory
// of its own, so only a workdir or a cd on the line says where it is. A background launch writes nothing.
func TestAShellRunsInItsCallsDirectory(t *testing.T) {
	const x = "/w/out/x.jsonl"
	long := "cat >> x.jsonl <<'EOF'\n" + strings.Repeat(`{"id":1,"verdict":"keep"}`+"\n", 20)
	cut, _ := json.Marshal(map[string]string{"command": long})
	for _, tc := range []struct {
		name, harness, tool, callCwd, resultCwd, input, result, path string
		writes                                                       bool
	}{
		{"the call's directory, not the result's", "kimi", "Bash", "/w/out", "/w/elsewhere", `{"command":"cat >> x.jsonl"}`, "", x, true},
		{"the result's directory is not the call's", "kimi", "Bash", "/w/elsewhere", "/w/out", `{"command":"cat >> x.jsonl"}`, "", x, false},
		{"a whole workdir", "opencode", "bash", "/w", "/w", `{"command":"cat >> x.jsonl","workdir":"/w/out"}`, "", x, true},
		{"a relative workdir", "opencode", "bash", "/w", "/w", `{"command":"cat >> x.jsonl","workdir":"out"}`, "", "out/x.jsonl", false},
		{"a relative workdir, not the session's directory", "opencode", "bash", "/w", "/w", `{"command":"cat >> x.jsonl","workdir":"out"}`, "", "/w/x.jsonl", false},
		{"a workdir with a ..", "opencode", "bash", "/w", "/w", `{"command":"cat >> x.jsonl","workdir":"/w/a/../out"}`, "", x, false},
		{"a Claude call cut in its here-document", "claude", "Bash", "/w/out", "/w/out", string(cut[:240]) + "…", "", x, true},
		{"Claude's background launch", "claude", "Bash", "/w/out", "/w/out", `{"command":"cat >> x.jsonl","run_in_background":true}`, "", x, false},
		{"a background launch the result tells", "claude", "Bash", "/w/out", "/w/out", string(cut[:240]) + "…", "Command running in background with ID: b1", x, false},
		{"Kimi's cwd", "kimi", "Bash", "/w", "/w", `{"command":"cat >> x.jsonl","cwd":"/w/out"}`, "", x, true},
		{"a Kimi call cut before its cwd", "kimi", "Bash", "/w/out", "/w/out", string(cut[:240]) + "…", "", x, false},
		{"a pi call cut before its workdir", "pi", "bash", "/w/out", "/w/out", string(cut[:240]) + "…", "", x, false},
		{"Codex's workdir", "codex", "exec_command", "/w", "/w", `{"cmd":"cat >> x.jsonl","workdir":"/w/out"}`, "Process exited with code 0\nOutput:\n", x, true},
		{"a Codex command still running", "codex", "exec_command", "/w", "/w", `{"cmd":"cat >> x.jsonl","workdir":"/w/out"}`,
			"Chunk ID: 1\nWall time: 1.0 seconds\nProcess running with session ID 7\nOutput:\n", x, false},
		{"a cut call that may name a workdir", "opencode", "bash", "/w/out", "/w/out", string(cut[:240]) + "…", "", x, false},
		{"Hermes's terminal keeps its own directory", "hermes", "terminal", "/w/out", "/w/out", `{"command":"cat >> x.jsonl"}`, "", x, false},
		{"Hermes after a cd", "hermes", "terminal", "/w/elsewhere", "/w/elsewhere", `{"command":"cd /w/out && cat >> x.jsonl"}`, "", x, true},
		{"Hermes's workdir", "hermes", "terminal", "/w/elsewhere", "/w/elsewhere", `{"command":"cat >> x.jsonl","workdir":"/w/out"}`, "", x, true},
		{"Hermes's background launch", "hermes", "terminal", "/w", "/w", `{"command":"cat >> x.jsonl","workdir":"/w/out","background":true}`, "", x, false},
		{"Grok's terminal keeps its own directory", "grok", "run_terminal_command", "/w/out", "/w/out", `{"command":"cat >> x.jsonl"}`, "", x, false},
		{"Grok after a cd", "grok", "run_terminal_command", "/w/elsewhere", "/w/elsewhere", `{"command":"cd /w/out && cat >> x.jsonl"}`, "", x, true},
		{"Grok's working_directory", "grok", "run_terminal_command", "/w/elsewhere", "/w/elsewhere", `{"command":"cat >> x.jsonl","working_directory":"/w/out"}`, "", x, true},
		{"Codex's older exit code", "codex", "exec_command", "/w", "/w", `{"cmd":"cat >> x.jsonl","workdir":"/w/out"}`, "Exit code: 0\nWall time: 0.1 seconds\nOutput:\n", x, true},
		{"PowerShell is not bash", "claude", "PowerShell", "/w/out", "/w/out", `{"command":"cat > x.jsonl"}`, "", x, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := fileTools{harness: "kimi"}
			w := fileTools{harness: tc.harness}
			var evs []store.Event
			for i, session := range []string{"s1", "s1", "s2"} {
				at := t0.Add(time.Duration(i) * time.Hour)
				evs = append(evs, kimiReadMissing(&f, session, tc.path, at)...)
				pair := w.pair(session, tc.tool, tc.input, tc.result, false, at.Add(time.Minute))
				pair[0].Cwd, pair[1].Cwd = tc.callCwd, tc.resultCwd
				evs = append(evs, pair...)
			}
			put(t, st, evs...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if err != nil {
				t.Fatal(err)
			}
			if tc.writes && len(fs) != 0 || !tc.writes && (len(fs) != 1 || len(fs[0].Evidence) != 3) {
				t.Fatalf("writes %v, found %v", tc.writes, signatures(fs))
			}
		})
	}
}

// The batch's own shape: each session appends to its file by its name alone, in the directory its call ran in.
func TestARelativeWriteIsReadInItsCallsDirectory(t *testing.T) {
	st := testStore(t)
	f := fileTools{harness: "kimi"}
	var evs []store.Event
	for i, session := range []string{"s1", "s2", "s3"} {
		dir := fmt.Sprintf("/w/b%d", i)
		at := t0.Add(time.Duration(i) * time.Hour)
		read := kimiReadMissing(&f, session, dir+"/out.jsonl", at)
		for k := range read {
			read[k].Cwd = dir
		}
		evs = append(evs, read...)
		evs = append(evs, shellCall(&f, session, dir, "cat >> out.jsonl <<'EOF'\n{\"id\":1}\nEOF", false, at.Add(2*time.Minute))...)
	}
	put(t, st, evs...)
	if fs, err := (Failures{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("found %v, %v", signatures(fs), err)
	}
}

// A read that found no file waits an hour before it counts: a run whose window ends between the read and its
// session's write files nothing, so the case store holds no sighting the write would clear.
func TestAReadWaitsForItsWriteBeforeItCounts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := claudeFiles
	put(t, st, slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)),
		f.readMissing("s2", "/w/out/c.md", t0.Add(2*time.Hour)))...)
	outs, _, err := Run(ctx, st, []Detector{Failures{}}, Window{Since: t0.Add(-24 * time.Hour), Until: t0.Add(2*time.Hour + 30*time.Minute)}, "test", false)
	if err != nil || len(outs) != 1 || len(outs[0].Filed) != 0 {
		t.Fatalf("a run before the last read settled: %+v, %v", outs, err)
	}
	put(t, st, slices.Concat(f.wrote("s1", "/w/out/a.md", t0.Add(10*time.Minute)), f.wrote("s1", "/w/out/b.md", t0.Add(time.Hour+10*time.Minute)),
		f.wrote("s2", "/w/out/c.md", t0.Add(2*time.Hour+10*time.Minute)))...)
	outs, _, err = Run(ctx, st, []Detector{Failures{}}, Window{Since: t0.Add(-24 * time.Hour), Until: t0.Add(4 * time.Hour)}, "test", false)
	if err != nil || len(outs) != 1 || len(outs[0].Filed) != 0 {
		t.Fatalf("a run that holds the writes: %+v, %v", outs, err)
	}
}

// A read is cleared only by its thread's write within the hour after it, so the verdict of a settled read is final:
// a write later than that clears nothing, and one before the read neither.
func TestAReadClearsOnlyOnAWriteWithinTheHour(t *testing.T) {
	st := testStore(t)
	f := claudeFiles
	put(t, st, slices.Concat(
		f.readMissing("s1", "/w/out/a.md", t0), f.wrote("s1", "/w/out/a.md", t0.Add(time.Hour)),
		f.readMissing("s1", "/w/out/b.md", t0.Add(2*time.Hour)), f.wrote("s1", "/w/out/b.md", t0.Add(3*time.Hour+time.Minute)),
		f.readMissing("s2", "/w/out/c.md", t0), f.wrote("s2", "/w/out/c.md", t0.Add(61*time.Minute)),
		f.readMissing("s2", "/w/out/d.md", t0.Add(4*time.Hour)), f.wrote("s2", "/w/out/d.md", t0.Add(6*time.Hour)),
		// a write before the read and one within the hour; one within the hour and one long after
		f.wrote("s3", "/w/out/e.md", t0.Add(-10*time.Minute)), f.readMissing("s3", "/w/out/e.md", t0), f.wrote("s3", "/w/out/e.md", t0.Add(30*time.Minute)),
		f.readMissing("s3", "/w/out/g.md", t0), f.wrote("s3", "/w/out/g.md", t0.Add(30*time.Minute)), f.wrote("s3", "/w/out/g.md", t0.Add(3*time.Hour)))...)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if got, want := signatures(fs), []string{"failures/v2:tool:read:file does not exist. note: your current working directory is P 3"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("found %v, %v, want %v", got, err, want)
	}
}

// A read exactly an hour before the window's end still waits: its write at the hour's end would be the window's
// end, which the window does not hold.
func TestAReadAnHourBeforeTheWindowsEndStillWaits(t *testing.T) {
	st := testStore(t)
	f := claudeFiles
	put(t, st, slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), f.readMissing("s1", "/w/out/b.md", t0),
		f.readMissing("s2", "/w/out/c.md", t0))...)
	edge := Window{Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour + time.Second)}
	if fs, err := (Failures{}).Detect(context.Background(), st, edge); err != nil || len(fs) != 0 {
		t.Fatalf("at the hour's end: %v, %v", signatures(fs), err)
	}
	if fs, err := (Failures{}).Detect(context.Background(), st, Window{Since: edge.Since, Until: edge.Until.Add(time.Millisecond)}); err != nil || len(fs) != 1 {
		t.Fatalf("a moment later: %v, %v", signatures(fs), err)
	}
}

// A not-found read whose call the store does not hold has no path to clear it by, but waits its hour too.
func TestAReadWithNoPathWaitsToo(t *testing.T) {
	st := testStore(t)
	put(t, st, result("s1", "Read", "r1", claudeMissing, t0), result("s1", "Read", "r2", claudeMissing, t0.Add(time.Minute)),
		result("s2", "Read", "r3", claudeMissing, t0.Add(2*time.Minute)))
	if fs, err := (Failures{}).Detect(context.Background(), st, Window{Since: t0.Add(-time.Hour), Until: t0.Add(30 * time.Minute)}); err != nil || len(fs) != 0 {
		t.Fatalf("inside the hour: %v, %v", signatures(fs), err)
	}
	if fs, err := (Failures{}).Detect(context.Background(), st, Window{Since: t0.Add(-time.Hour), Until: t0.Add(2 * time.Hour)}); err != nil || len(fs) != 1 {
		t.Fatalf("past it: %v, %v", signatures(fs), err)
	}
}

// The mistakes log's own not-found read, which no transcript result holds, waits its hour as well: the log keeps no
// path its write could clear, but a run inside the hour does not count it yet.
func TestALoggedReadWaitsToo(t *testing.T) {
	st := testStore(t)
	logged := func(session string, at time.Time) store.Event {
		return ev("mistakes", session, "tool-error", store.ActorHarness, "Read", "Read: "+claudeMissing, "", at)
	}
	denied := func(session string, at time.Time) store.Event {
		return ev("mistakes", session, "deny", store.ActorHarness, "Write", "Write: ledger: this note names no source", "", at)
	}
	put(t, st, logged("s1", t0), logged("s1", t0.Add(time.Minute)), logged("s2", t0.Add(2*time.Minute)),
		denied("s1", t0), denied("s1", t0.Add(time.Minute)), denied("s2", t0.Add(2*time.Minute)))
	// only the reads wait: another of the log's rows counts at once
	fs, err := Failures{}.Detect(context.Background(), st, Window{Since: t0.Add(-time.Hour), Until: t0.Add(30 * time.Minute)})
	if got, want := signatures(fs), []string{"failures/v2:guard:ledger:this note names no source 3"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("inside the hour: %v, %v, want %v", got, err, want)
	}
	if fs, err := (Failures{}).Detect(context.Background(), st, Window{Since: t0.Add(-time.Hour), Until: t0.Add(2 * time.Hour)}); err != nil || len(fs) != 2 {
		t.Fatalf("past it: %v, %v", signatures(fs), err)
	}
}

// A write within the hour that the store holds late leaves a settled read a failure on the runs before it is
// stored, and the first run that holds it no longer counts the read.
func TestALateStoredWriteClearsASettledRead(t *testing.T) {
	st := testStore(t)
	f := claudeFiles
	put(t, st, slices.Concat(f.readMissing("s1", "/w/out/a.md", t0), f.readMissing("s1", "/w/out/b.md", t0.Add(time.Hour)),
		f.readMissing("s2", "/w/out/c.md", t0.Add(2*time.Hour)))...)
	early := Window{Since: t0.Add(-time.Hour), Until: t0.Add(3*time.Hour + 2*time.Second)}
	if fs, err := (Failures{}).Detect(context.Background(), st, early); err != nil || len(fs) != 1 || len(fs[0].Evidence) != 3 {
		t.Fatalf("before the writes are stored: %v, %v", signatures(fs), err)
	}
	put(t, st, slices.Concat(f.wrote("s1", "/w/out/a.md", t0.Add(30*time.Minute)), f.wrote("s1", "/w/out/b.md", t0.Add(90*time.Minute)),
		f.wrote("s2", "/w/out/c.md", t0.Add(150*time.Minute)))...)
	if fs, err := (Failures{}).Detect(context.Background(), st, early); err != nil || len(fs) != 0 {
		t.Fatalf("once they are: %v, %v", signatures(fs), err)
	}
}

// sighting is an incident-seen event as the incidents source writes it.
func sighting(incident, text string, fresh int, first bool, firstSeen time.Time, at time.Time) store.Event {
	seq++
	m := fmt.Sprintf(`{"incident":%q,"new":%d,"status":"reported"`, incident, fresh)
	if first {
		m += `,"first":true`
		if !firstSeen.IsZero() {
			m += fmt.Sprintf(`,"first_seen":%q`, firstSeen.UTC().Format(time.RFC3339Nano))
		}
	}
	return store.Event{Source: store.Incidents, SourceKey: fmt.Sprintf("%s@%d", incident, seq), TS: at, Actor: store.ActorDetector,
		Kind: "incident-seen", Summary: text, Meta: m + "}"}
}

func incident(id, text, status string, at time.Time) store.Event {
	return store.Event{Source: store.Incidents, SourceKey: id, TS: at, Actor: store.ActorDetector, Kind: "incident", Summary: text,
		Meta: fmt.Sprintf(`{"status":%q}`, status)}
}

// An incident counts its sightings' repeats, ten in the window. One closed or resolved now, as its own event says,
// counts for nothing, whatever its sightings said; a first sighting holds the incident's whole count, which counts
// only when the incident was first seen in the window.
func TestIncidentsCountTheirSightings(t *testing.T) {
	st := testStore(t)
	dns := "httpx.ConnectError: [Errno -3] Temporary failure in name resolution"
	put(t, st,
		incident("i1", dns, "reported", t0.Add(time.Hour)),
		sighting("i1", dns, 6, true, t0.Add(-time.Hour), t0), sighting("i1", dns, 6, false, time.Time{}, t0.Add(time.Hour)),
		// open when seen, closed since
		incident("i2", "a network drops", "closed", t0.Add(time.Hour)),
		sighting("i2", "a network drops", 5, true, t0, t0), sighting("i2", "a network drops", 25, false, time.Time{}, t0.Add(time.Hour)),
		// first seen forty days ago, four hundred repeats since: seen once this week
		incident("i3", "the bot keeps reconnecting", "reported", t0),
		sighting("i3", "the bot keeps reconnecting", 400, true, t0.Add(-40*24*time.Hour), t0),
		// a first sighting that says nothing of when it was first seen
		incident("i4", "the disk fills", "reported", t0),
		sighting("i4", "the disk fills", 50, true, time.Time{}, t0),
		incident("i5", "a queue backs up", "resolved", t0.Add(time.Hour)),
		sighting("i5", "a queue backs up", 12, true, t0, t0),
		// a sighting with no incident: an older build's
		store.Event{Source: store.Incidents, SourceKey: "old@1", TS: t0, Actor: store.ActorDetector, Kind: "incident-seen", Summary: "old", Meta: `{"new":90}`},
	)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	if got := signatures(fs); !slices.Equal(got, []string{"failures/v2:incident:httpx.connecterror: [errno -N] temporary failure in name resolution 2"}) {
		t.Fatalf("found %v", got)
	}
}

// A finding files at most MaxEvidence of its newest sightings on a run.
func TestAFindingFilesItsNewestEvidence(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>fence-guard: a process match on 'x' takes its own shell too"
	var evs []store.Event
	for i := 0; i < MaxEvidence+1; i++ {
		evs = append(evs, toolError(fmt.Sprint("s", i%2), "Bash", deny, t0.Add(time.Duration(i)*time.Minute)))
	}
	put(t, st, evs...)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil || len(fs) != 1 || len(fs[0].Evidence) != MaxEvidence {
		t.Fatalf("%d findings, %v", len(fs), err)
	}
	if !fs[0].Evidence[0].At.Equal(t0.Add(MaxEvidence*time.Minute)) || !fs[0].Evidence[MaxEvidence-1].At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("not the newest: %v .. %v", fs[0].Evidence[0].At, fs[0].Evidence[MaxEvidence-1].At)
	}
}

// The owner's turn is read with the agent's last words before it: what he pushes back on, hands back, says does
// not hold, nudges or says again.
func TestAnOwnerTurnIsReadWithTheAgentsLastWords(t *testing.T) {
	for _, c := range []struct{ owner, agent, want string }{
		{"i never said that, why do you think so?", "Per your rule, the reviewer must be another model.", "attribution"},
		{"you can do as you think", "I rewrote one repo. Should I rewrite the other two, or none?", "handback"},
		{"why you need me to run it?", "It is ready… To finish, please run `make deploy` in your terminal.", "handback"},
		{"it still doesnt work, same error", "The fix is in… Both tests pass and the page is live.", "done-claim"},
		{"are you off?", "The sweep runs now… I'll report when it finishes.", "stopped"},
		{"thanks, looks good", "Both tests pass and the page is live.", ""},
		{"why is the cache this big?", "The cache holds 40 GB of wheels.", ""},
		{"continue", "Here is the plan.", ""},
	} {
		if got := OwnerClass(c.owner, c.agent, nil); got != c.want {
			t.Errorf("%q after %q: %q, want %q", c.owner, c.agent, got, c.want)
		}
	}
	if got := OwnerClass("send the build to the test phone again please", "", []string{"please send the build to the test phone"}); got != "repeat" {
		t.Errorf("a turn he repeats: %q", got)
	}
	// five words he shares with what he said are a repeat; four are not
	if got := OwnerClass("ship the beta build today", "", []string{"ship the beta build today"}); got != "repeat" {
		t.Errorf("five words again: %q", got)
	}
	if got := OwnerClass("ship the beta build", "", []string{"ship the beta build"}); got != "" {
		t.Errorf("four words again: %q", got)
	}
	// half the words in common is no repeat
	if got := OwnerClass("alpha beta gamma delta epsilon zeta", "", []string{"alpha beta gamma delta eta theta"}); got != "" {
		t.Errorf("half the words again: %q", got)
	}
}

func ownerTurn(session, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "owner_text", store.ActorOwner, "", text, `{"input_kind":"typed"}`, at)
}

func agentSays(session, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "agent_text", store.ActorAgent, "", text, "", at)
}

// The owner's turns of one kind in two sessions are a finding. A program's text is never his: a configured wake
// loop's, a long template sent in three sessions, the same text in two sessions within 2 s; one turn the transcript
// holds twice counts once; a subagent's thread and another actor's text are not his turns. A turn of his that opens
// with momentum's marker holds his own words after it, and a short nudge he types in three sessions is his.
func TestOwnerBurdenIsFoundInHisOwnWords(t *testing.T) {
	st := testStore(t)
	ask := "The build is ready. Should I deploy it now?"
	sub := ownerTurn("j", "why do you ask, you can decide", t0.Add(time.Minute))
	sub.Agent = "sub-1"
	other := ownerTurn("k", "why do you ask, you can decide", t0.Add(time.Minute))
	other.Actor = store.ActorHarness
	template := "scheduled pass: why do you ask me, you decide which item goes first"
	put(t, st,
		agentSays("a", ask, t0), ownerTurn("a", "why do you ask, you can decide", t0.Add(time.Minute)),
		ownerTurn("a", "why do you ask, you can decide", t0.Add(90*time.Second)), // the same turn recorded twice
		agentSays("a", ask, t0.Add(time.Hour)), ownerTurn("a", "no need to ask me that, do it", t0.Add(time.Hour+time.Minute)),
		agentSays("b", ask, t0), ownerTurn("b", "up to you, why ask", t0.Add(2*time.Minute)),
		// his words after momentum's marker
		agentSays("c", ask, t0), ownerTurn("c", "[momentum] why stop, you can decide", t0.Add(time.Minute)),
		// the same handback, typed by programs
		agentSays("d", ask, t0), ownerTurn("d", "Ping id=1 why stop, you can decide", t0.Add(time.Minute)),
		agentSays("e", ask, t0), ownerTurn("e", template, t0.Add(time.Minute)),
		agentSays("f", ask, t0), ownerTurn("f", template, t0.Add(2*time.Hour)),
		agentSays("g", ask, t0), ownerTurn("g", template, t0.Add(3*time.Hour)),
		agentSays("h", ask, t0), ownerTurn("h", "why, you decide this", t0.Add(4*time.Hour)),
		agentSays("i", ask, t0), ownerTurn("i", "why, you decide this", t0.Add(4*time.Hour+time.Second)),
		agentSays("j", ask, t0), sub, agentSays("k", ask, t0), other,
	)
	fs, err := OwnerBurden{Programs: Programs{Prefixes: []string{"Ping id="}}}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	if got := signatures(fs); !slices.Equal(got, []string{"owner-burden/v1:handback 4"}) {
		t.Fatalf("found %v", got)
	}

	// a short nudge he types in three sessions is his, not a template
	st2 := testStore(t)
	wait := "The sweep runs now… I'll report when it finishes."
	for i, s := range []string{"x", "y", "z"} {
		put(t, st2, agentSays(s, wait, t0), ownerTurn(s, "are you off?", t0.Add(time.Duration(i+1)*time.Hour)))
	}
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st2, week()); err != nil || !slices.Equal(signatures(fs), []string{"owner-burden/v1:stopped 3"}) {
		t.Fatalf("his own nudges: %v %v", signatures(fs), err)
	}
}

// A turn of his with none of his words (a compressed image or an attached resource he sent alone) answers the
// agent's last words as any turn does: the next turn's look back starts at it, so what the agent asked before it is
// not read again. It is never classed itself.
func TestAnEmptyOwnerTurnBoundsTheLookBack(t *testing.T) {
	ask := "The build is ready. Should I deploy it now?"
	for _, empty := range []bool{false, true} {
		st := testStore(t)
		for i, s := range []string{"a", "b", "c"} {
			at := t0.Add(time.Duration(i) * time.Hour)
			put(t, st, agentSays(s, ask, at))
			if empty {
				put(t, st, ownerTurn(s, "", at.Add(time.Minute)))
			}
			put(t, st, ownerTurn(s, "up to you, why ask", at.Add(2*time.Minute)))
		}
		var want []string
		if !empty {
			want = []string{"owner-burden/v1:handback 3"}
		}
		if fs, err := (OwnerBurden{}).Detect(context.Background(), st, week()); err != nil || !slices.Equal(signatures(fs), want) {
			t.Fatalf("with an empty turn before his answer %v: %v %v, want %v", empty, signatures(fs), err, want)
		}
	}
}

// A message another session sent into a session is stored as agent text, a harness line or by its origin, but it is
// not the agent's words. His turn after one is read with the agent's reply before it, however many came between,
// and a message alone since his turn before leaves nothing to read it with: its own words handing him a step are
// not the agent's. A reply of the model's that opens as a peer message does is the agent's words.
func TestAPeerMessageIsNotTheAgentsLastWords(t *testing.T) {
	ask := "The build is ready. Should I deploy it now?"
	peer := func(session, text, why string, at time.Time) store.Event {
		return ev(store.Transcripts, session, "agent_text", store.ActorAgent, "", text, fmt.Sprintf(`{"why":%q}`, why), at)
	}
	tag := func(name string) string { return "<" + name + " from=\"neutral\">" } // built from parts: the file holds no tag
	msgs := []struct{ text, why string }{
		{tag("cross-session-message") + "the review is posted", "harness line"},
		{tag("agent-message") + "the review is posted", "harness line"},
		{"Another Claude session sent a message: the review is posted", "harness line"},
		{"the review is posted", "origin peer"},
	}
	st := testStore(t)
	for i, s := range []string{"a", "b", "c", "d"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		put(t, st, agentSays(s, ask, at), peer(s, msgs[i].text, msgs[i].why, at.Add(time.Minute)), ownerTurn(s, "up to you, why ask", at.Add(2*time.Minute)))
	}
	at := t0.Add(5 * time.Hour)
	put(t, st, agentSays("e", ask, at), ownerTurn("e", "up to you, why ask", at.Add(time.Minute)))
	for j := range 10 { // more than one read of the newest
		put(t, st, peer("e", msgs[0].text, msgs[0].why, at.Add(time.Duration(j+1)*time.Second)))
	}
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st, week()); err != nil || !slices.Equal(signatures(fs), []string{"owner-burden/v1:handback 5"}) {
		t.Errorf("his answers to the agent's asks: %v %v", signatures(fs), err)
	}
	st2 := testStore(t)
	for i, s := range []string{"x", "y", "z"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		put(t, st2, ownerTurn(s, "tag the release", at), agentSays(s, "Tagging it.", at.Add(time.Minute)),
			peer(s, tag("cross-session-message")+"Should I take the release notes too?", "harness line", at.Add(2*time.Minute)),
			ownerTurn(s, "why ask me, you decide", at.Add(3*time.Minute)))
	}
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st2, week()); err != nil || len(fs) != 0 {
		t.Fatalf("a message's own words read as the agent's: %v %v", signatures(fs), err)
	}
	// With no reply of the agent's since his turn before, the message is all there is, and it is still not the agent's.
	st3 := testStore(t)
	for i, s := range []string{"x", "y", "z"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		put(t, st3, ownerTurn(s, "tag the release", at),
			peer(s, tag("cross-session-message")+"Should I take the release notes too?", "harness line", at.Add(time.Minute)),
			ownerTurn(s, "why ask me, you decide", at.Add(2*time.Minute)))
	}
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st3, week()); err != nil || len(fs) != 0 {
		t.Fatalf("a message alone since his turn before read as the agent's: %v %v", signatures(fs), err)
	}
	// Only a harness line in a peer message's form is one. The model's own reply that opens the same way is its words,
	// with no meta as the store keeps a reply or with meta of its own, as it keeps a failed turn's.
	st4 := testStore(t)
	relayed := "Another Claude session sent a message, and the build is ready. Should I deploy it now?"
	for i, meta := range []string{"", `{"is_error":true}`, `{"is_error":true}`} {
		s, at := string(rune('a'+i)), t0.Add(time.Duration(i)*time.Hour)
		put(t, st4, ev(store.Transcripts, s, "agent_text", store.ActorAgent, "", relayed, meta, at), ownerTurn(s, "up to you, why ask", at.Add(time.Minute)))
	}
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st4, week()); err != nil || !slices.Equal(signatures(fs), []string{"owner-burden/v1:handback 3"}) {
		t.Fatalf("the model's reply in a peer message's form: %v %v", signatures(fs), err)
	}
}

// A turn is read with the agent's last words since his turn before it, within 6 hours: words he answered once are
// not read again, and words from longer ago are not what he answers. Three turns of one kind in one session are no
// finding; a turn he repeats a day later is read against what he said that day.
func TestAnOwnerTurnIsReadOnceAgainstWhatHeAnswers(t *testing.T) {
	st := testStore(t)
	ask := "The build is ready. Should I deploy it now?"
	put(t, st,
		// three turns after one ask: only the first answers it
		agentSays("a", ask, t0), ownerTurn("a", "why ask me", t0.Add(time.Minute)), ownerTurn("a", "just do it, you decide", t0.Add(5*time.Minute)),
		ownerTurn("a", "why do you still wait", t0.Add(20*time.Minute)),
		agentSays("b", ask, t0), ownerTurn("b", "up to you, why ask", t0.Add(2*time.Minute)),
		agentSays("c", ask, t0), ownerTurn("c", "up to you, why ask", t0.Add(7*time.Hour)), // past the look back
	)
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("one step-in counted per turn after it: %v %v", signatures(fs), err)
	}
	// three of one kind in one session, each after its own ask, are no finding; in two sessions they are
	st1 := testStore(t)
	for i := 0; i < 3; i++ {
		at := t0.Add(time.Duration(i) * time.Hour)
		put(t, st1, agentSays("a", ask, at), ownerTurn("a", "up to you, why ask", at.Add(time.Minute)))
	}
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st1, week()); err != nil || len(fs) != 0 {
		t.Fatalf("one session's: %v %v", signatures(fs), err)
	}
	put(t, st1, agentSays("b", ask, t0.Add(30*time.Minute)), ownerTurn("b", "up to you, why ask", t0.Add(31*time.Minute)))
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st1, week()); err != nil || !slices.Equal(signatures(fs), []string{"owner-burden/v1:handback 4"}) {
		t.Fatalf("two sessions': %v %v", signatures(fs), err)
	}
	st2 := testStore(t)
	say := func(s, what string) string { return "send the " + what + " notes to the list before you tag" }
	for _, s := range []string{"x", "y"} {
		put(t, st2, ownerTurn(s, say(s, s+"-release"), t0), ownerTurn(s, say(s, s+"-release")+" it", t0.Add(20*time.Hour)))
	}
	put(t, st2, ownerTurn("z", say("z", "z-release"), t0.Add(-2*24*time.Hour)), ownerTurn("z", say("z", "z-release")+" it", t0.Add(time.Hour)))
	fs, err := OwnerBurden{}.Detect(context.Background(), st2, week())
	if err != nil || len(fs) != 0 {
		t.Fatalf("two repeats in two sessions are under three: %v %v", signatures(fs), err)
	}
	put(t, st2, ownerTurn("w", say("w", "w-release"), t0.Add(2*time.Hour)), ownerTurn("w", say("w", "w-release")+" it", t0.Add(3*time.Hour)))
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st2, week()); err != nil || !slices.Equal(signatures(fs), []string{"owner-burden/v1:repeat 3"}) {
		t.Fatalf("three repeats in three sessions, one past a day not counted: %v %v", signatures(fs), err)
	}
}

// Only the window's turns are read for what he steps in on (those before it only for what he repeats), and only a
// session's own agent's words: a subagent's words are not what he answers.
func TestOwnerBurdenReadsTheWindowsTurnsAndTheSessionsOwnWords(t *testing.T) {
	st := testStore(t)
	ask := "The build is ready. Should I deploy it now?"
	w := week()
	before := w.Since.Add(-2 * time.Hour)
	subAsk := agentSays("c", ask, t0)
	subAsk.Agent = "sub-1"
	put(t, st,
		agentSays("a", ask, before), ownerTurn("a", "up to you, why ask", before.Add(time.Minute)),
		agentSays("b", ask, t0), ownerTurn("b", "up to you, why ask", t0.Add(time.Minute)),
		subAsk, ownerTurn("c", "up to you, why ask", t0.Add(10*time.Minute)),
		agentSays("d", ask, t0), ownerTurn("d", "up to you, why ask", t0.Add(20*time.Minute)),
	)
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st, w); err != nil || len(fs) != 0 {
		t.Fatalf("a turn before the window or after a subagent's words counted: %v %v", signatures(fs), err)
	}
	put(t, st, agentSays("e", ask, t0), ownerTurn("e", "up to you, why ask", t0.Add(30*time.Minute)))
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st, w); err != nil || !slices.Equal(signatures(fs), []string{"owner-burden/v1:handback 3"}) {
		t.Fatalf("three in the window: %v %v", signatures(fs), err)
	}
}

// A program's text: one turn recorded twice within 2 minutes of the one before it (the first of a run of them
// counts), a configured prefix, the same text in two sessions within 2 seconds, a long text in three sessions of the
// window (sessions before it do not count).
func TestProgramsMarkWhatAProgramTyped(t *testing.T) {
	p := Programs{Prefixes: []string{"Ping id="}}
	long := "scheduled pass: take the next item and go on with its open work"
	turns := []store.Event{
		ownerTurn("a", "go on", t0), ownerTurn("a", "go on", t0.Add(90*time.Second)), ownerTurn("a", "go on", t0.Add(210*time.Second)),
		ownerTurn("b", "Ping id=7 look at the queue", t0),
		ownerTurn("c", "is it done", t0), ownerTurn("d", "is it done", t0.Add(time.Second)), ownerTurn("e", "is it done", t0.Add(4*time.Second)),
		ownerTurn("f", long, t0.Add(-2*24*time.Hour)), ownerTurn("g", long, t0), ownerTurn("h", long, t0.Add(time.Hour)),
		ownerTurn("i", long+" now", t0), ownerTurn("j", long+" now", t0.Add(time.Hour)), ownerTurn("k", long+" now", t0.Add(2*time.Hour)),
	}
	slices.SortStableFunc(turns, func(a, b store.Event) int { return a.TS.Compare(b.TS) })
	marked := p.Of(turns, week().Since)
	var got []string
	for i, t := range turns {
		if marked[i] {
			got = append(got, fmt.Sprintf("%s@%s", t.Session, t.TS.Sub(t0)))
		}
	}
	slices.Sort(got)
	want := []string{"a@1m30s", "b@0s", "c@0s", "d@1s", "i@0s", "j@1h0m0s", "k@2h0m0s"}
	if !slices.Equal(got, want) {
		t.Fatalf("marked %v\nwant %v", got, want)
	}
}

// Supervision's own record is read for what it gave up on (a finding at once) and the messages that did not land
// or failed again; the sessions' phase events for the waits on the owner past the owner window, but an automated
// run's, and only waits on him.
func TestSessionsAreFoundInSupervisionsOwnRecord(t *testing.T) {
	st := testStore(t)
	sup := func(session, kind, meta string, at time.Time) store.Event {
		return ev(store.Supervise, session, kind, store.ActorDetector, "", "x", meta, at)
	}
	wait := func(session, from, kind string, mins int, auto bool, at time.Time) store.Event {
		return ev(store.Sessions, session, "phase", store.ActorHarness, "", "x",
			fmt.Sprintf(`{"from":%q,"to":"working","for_ms":%d,"from_ask_kind":%q,"auto":%v}`, from, mins*60_000, kind, auto), at)
	}
	put(t, st,
		sup("a", "case", `{"rule":"transient","why":"four tries without a turn that worked: the API stays down"}`, t0),
		sup("a", "outcome", `{"rule":"promise","result":"failed again"}`, t0), sup("b", "outcome", `{"rule":"promise","result":"failed again"}`, t0),
		sup("b", "outcome", `{"rule":"promise","result":"failed again"}`, t0.Add(time.Hour)),
		sup("c", "outcome", `{"rule":"promise","result":"worked"}`, t0), sup("d", "outcome", `{"rule":"promise","result":"worked"}`, t0),
		sup("e", "outcome", `{"rule":"promise","result":"worked"}`, t0),
		wait("a", "needs_you", "permission", 40, false, t0), wait("b", "needs_you", "permission", 20, false, t0),
		wait("b", "needs_you", "permission", 90, false, t0.Add(time.Hour)),
		wait("c", "needs_you", "question", 40, true, t0), wait("d", "needs_you", "question", 40, true, t0), wait("e", "needs_you", "question", 40, true, t0),
		wait("f", "needs_you", "plan", 5, false, t0), wait("g", "needs_you", "plan", 5, false, t0), wait("h", "needs_you", "plan", 5, false, t0),
		wait("i", "idle", "plan", 60, false, t0), wait("j", "idle", "plan", 60, false, t0), wait("k", "idle", "plan", 60, false, t0),
	)
	fs, err := Sessions{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sessions/v1:gave-up:transient:four-tries-without-a-turn-that-worked 1", "sessions/v1:outcome:promise:failed-again 3",
		"sessions/v1:waited:permission 3"}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An access wait (a usage limit with a stated reset) that supervision held and resumed on its first try is not a
// case, though the phase scan kept the session in needs_you for hours. A stop its rule resumed on a later try, or
// whose message did not work, still is one.
func TestSessionsWaitResolvedOnFirstTryIsNotACase(t *testing.T) {
	st := testStore(t)
	stopAt := t0
	wait := func(session string, start time.Time, mins int) store.Event {
		return ev(store.Sessions, session, "phase", store.ActorHarness, "", "x",
			fmt.Sprintf(`{"from":"needs_you","to":"needs_you","for_ms":%d,"from_ask_kind":"error","auto":false}`, mins*60_000), start.Add(time.Duration(mins)*time.Minute))
	}
	act := func(session string, try int, at time.Time) store.Event {
		e := ev(store.Supervise, session, "act", store.ActorAgent, "", "x",
			fmt.Sprintf(`{"rule":"access","try":%d,"stop_at":%d}`, try, stopAt.UnixMilli()), at)
		return e
	}
	outcome := func(of store.Event, result string, at time.Time) store.Event {
		return ev(store.Supervise, of.Session, "outcome", store.ActorDetector, "", "x",
			fmt.Sprintf(`{"of":%q,"rule":"access","result":%q}`, of.SourceKey, result), at)
	}
	var evs []store.Event
	for _, s := range []string{"a", "b", "c"} {
		for i := 0; i < 6; i++ {
			evs = append(evs, wait(s, stopAt.Add(time.Duration(i)*30*time.Minute), 30))
		}
	}
	sent := stopAt.Add(3 * time.Hour)
	aa, ab, ac := act("a", 1, sent), act("b", 1, sent), act("c", 2, sent)
	evs = append(evs, aa, ab, ac, outcome(aa, "worked", sent.Add(time.Minute)), outcome(ab, "failed again", sent.Add(time.Minute)),
		outcome(ac, "worked", sent.Add(time.Minute)))
	put(t, st, evs...)
	fs, err := Sessions{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	// only b (its first try failed) and c (resolved on the second try) count: 6 waits each, in two sessions
	for _, f := range fs {
		if f.Signature != "sessions/v1:waited:error" || len(f.Evidence) != 12 {
			t.Fatalf("unexpected finding %s with %d events", f.Signature, len(f.Evidence))
		}
	}
	if len(fs) != 1 {
		t.Fatalf("found %d findings, want the one for b and c", len(fs))
	}
}

// accessWait is the access rule's wait event for a stop as the last supervise run that held the stop, at seen, left
// it: every run rewrites the one event of a stop and try. at is when the rule resumes the stop.
func accessWait(session, errText string, stopAt, at, seen time.Time) store.Event {
	return accessWaitTry(session, errText, 1, stopAt, at, seen)
}

// accessWaitTry is accessWait for the stop of a later try.
func accessWaitTry(session, errText string, try int, stopAt, at, seen time.Time) store.Event {
	m, _ := json.Marshal(map[string]any{"rule": "access", "try": try, "why": "x", "stop_at": stopAt.UnixMilli(), "at": at.UnixMilli(),
		"error": errText})
	return store.Event{Source: store.Supervise, SourceKey: fmt.Sprintf("%s@%d#%s#%s#%d", session, stopAt.UnixMilli(), "access", "wait", try),
		TS: seen, Session: session, Harness: "claude", Actor: store.ActorDetector, Kind: "wait", Summary: "x", Meta: string(m)}
}

// needsYou is the phase event that saw a session's wait on its owner end: in needs_you, asking kind, from start to
// end, and then in phase to.
func needsYou(session, kind, to string, start, end time.Time) store.Event {
	return ev(store.Sessions, session, "phase", store.ActorHarness, "", "x",
		fmt.Sprintf(`{"from":"needs_you","to":%q,"for_ms":%d,"from_ask_kind":%q}`, to, end.Sub(start).Milliseconds(), kind), end)
}

// limitHit is the error of a usage limit that states when it resets.
func limitHit(reset time.Time) string {
	return "You've hit your weekly limit · resets " + reset.UTC().Format("Jan 2, 3:04pm") + " (UTC)"
}

// heldStops are n stops of a session on a usage limit that each wakeup of its loop meets again, every half hour
// from start: each a piece in needs_you of its own, and the access wait the last run before the next stop left.
func heldStops(session, limit string, start time.Time, n int, at time.Time) []store.Event {
	var evs []store.Event
	for i := range n {
		stop := start.Add(time.Duration(i) * 30 * time.Minute)
		next := stop.Add(30 * time.Minute)
		evs = append(evs, needsYou(session, "error", "needs_you", stop, next), accessWait(session, limit, stop, at, next.Add(-time.Minute)))
	}
	return evs
}

// A usage limit that states a reset four days away stops two sessions, every wakeup of their loops meets it again,
// and the access rule holds each stop for the reset with no message: nothing waits on the owner, before a restart
// ends the last stop or after it.
func TestSessionsStopHeldForAStatedResetIsNotACase(t *testing.T) {
	st := testStore(t)
	reset := t0.Add(4 * 24 * time.Hour)
	limit, at := limitHit(reset), reset.Add(time.Minute)
	const n = 8
	last := t0.Add(n * 30 * time.Minute) // the stop the restart ends
	for _, s := range []string{"a", "b"} {
		put(t, st, heldStops(s, limit, t0, n, at)...)
		put(t, st, accessWait(s, limit, last, at, last.Add(10*time.Minute)))
	}
	none := func(when string) {
		t.Helper()
		fs, err := Sessions{}.Detect(context.Background(), st, week())
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) > 0 {
			t.Fatalf("%s: found %v, want nothing", when, signatures(fs))
		}
	}
	none("before the restart")
	restart := last.Add(20 * time.Minute)
	for _, s := range []string{"a", "b"} {
		put(t, st, accessWait(s, limit, last, at, restart.Add(-time.Minute)), needsYou(s, "error", "working", last, restart))
	}
	none("after the restart")
}

// What a hold does not cover still counts, and only that: a stop still held when the reset came and still stopped
// past it with no message (a, b), one whose message at the reset failed again (c) or did not land (d), a stop on
// another error after a restart ended the held one, before the reset (e), and a login's wait whose time a new login
// set (f). A hold begins at its stop: a question that waited on the owner before a wakeup met a limit days out (g),
// and the stop whose message failed again before a later try's held stop (h), still count. An access wait on a
// limit that states no reset, with a time a probe gave (i), or on an organization block whose text names a reset
// time (j), holds nothing. The held pieces before them are not evidence.
func TestSessionsWaitPastAHoldStillCounts(t *testing.T) {
	st := testStore(t)
	reset := t0.Add(3 * time.Hour)
	limit, at := limitHit(reset), reset.Add(time.Minute)
	last := t0.Add(5 * 30 * time.Minute) // the stop the reset comes in
	var want []string
	names := map[string]string{} // a counted piece's session, for the message
	counted := func(e store.Event) store.Event {
		var m struct {
			Kind string `json:"from_ask_kind"`
		}
		_ = json.Unmarshal([]byte(e.Meta), &m)
		want = append(want, m.Kind+" ev:"+e.ID())
		names[m.Kind+" ev:"+e.ID()] = e.Session
		return e
	}
	for _, s := range []string{"a", "b", "c", "d"} {
		put(t, st, heldStops(s, limit, t0, 5, at)...)
		put(t, st, accessWait(s, limit, last, at, at.Add(-time.Minute)))
	}
	for _, s := range []string{"a", "b"} {
		put(t, st, counted(needsYou(s, "error", "working", last, at.Add(2*time.Hour))))
	}
	sent := at.Add(time.Minute)
	for s, result := range map[string]string{"c": "failed again", "d": "not delivered"} {
		a := ev(store.Supervise, s, "act", store.ActorAgent, "", "x", fmt.Sprintf(`{"rule":"access","try":1,"stop_at":%d}`, last.UnixMilli()), sent)
		put(t, st, a, ev(store.Supervise, s, "outcome", store.ActorDetector, "", "x", fmt.Sprintf(`{"of":%q,"rule":"access","result":%q}`, a.SourceKey, result),
			sent.Add(3*time.Minute)), counted(needsYou(s, "error", "working", last, sent.Add(time.Minute))))
	}
	restart := t0.Add(70 * time.Minute)
	put(t, st, heldStops("e", limit, t0, 2, at)...)
	put(t, st, accessWait("e", limit, t0.Add(time.Hour), at, restart.Add(-time.Minute)), needsYou("e", "error", "working", t0.Add(time.Hour), restart),
		counted(needsYou("e", "error", "working", restart.Add(10*time.Minute), restart.Add(70*time.Minute))))
	login := t0.Add(2 * time.Hour)
	put(t, st, accessWait("f", "OAuth token has expired · Please run /login", t0, login.Add(15*time.Minute), login.Add(5*time.Minute)),
		counted(needsYou("f", "error", "working", t0, login.Add(6*time.Minute))))
	far := t0.Add(4 * 24 * time.Hour)
	farLimit, farAt := limitHit(far), far.Add(time.Minute)
	wake := t0.Add(2 * time.Hour)
	put(t, st, counted(needsYou("g", "question", "needs_you", t0, wake)), accessWait("g", farLimit, wake, farAt, wake.Add(30*time.Minute)),
		needsYou("g", "error", "working", wake, wake.Add(31*time.Minute)))
	a := ev(store.Supervise, "h", "act", store.ActorAgent, "", "x", fmt.Sprintf(`{"rule":"access","try":1,"stop_at":%d}`, t0.UnixMilli()), sent)
	again := sent.Add(2 * time.Minute) // the turn the message opened meets a limit again
	put(t, st, accessWait("h", limit, t0, at, at.Add(-time.Minute)), a,
		ev(store.Supervise, "h", "outcome", store.ActorDetector, "", "x", fmt.Sprintf(`{"of":%q,"rule":"access","result":"failed again"}`, a.SourceKey),
			sent.Add(3*time.Minute)), counted(needsYou("h", "error", "working", t0, sent.Add(time.Minute))),
		accessWaitTry("h", farLimit, 2, again, farAt, again.Add(29*time.Minute)), needsYou("h", "error", "working", again, again.Add(30*time.Minute)))
	back := t0.Add(2 * time.Hour) // when a probe saw access work again
	org := "Your organization has disabled Claude subscription access for Claude Code · resets " + reset.UTC().Format("Jan 2, 3:04pm") + " (UTC)"
	for s, errText := range map[string]string{"i": "You've hit your usage limit", "j": org} {
		put(t, st, accessWait(s, errText, t0, back, back.Add(-time.Minute)), counted(needsYou(s, "error", "working", t0, back.Add(time.Minute))))
	}
	// every occurrence is a finding here, one alone too: the test is which pieces count, not how often
	fs, err := Sessions{Recur: Recur{Events: 1, Sessions: 1}}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	var got, others []string
	for _, f := range fs {
		kind, ok := strings.CutPrefix(f.Signature, "sessions/v1:waited:")
		if !ok {
			others = append(others, f.Signature)
			continue
		}
		for _, e := range f.Evidence {
			got = append(got, kind+" "+e.Ref)
		}
	}
	if want := []string{"sessions/v1:outcome:access:failed-again", "sessions/v1:outcome:access:not-delivered"}; !slices.Equal(others, want) {
		t.Fatalf("found %v beside the waits, want the outcomes %v", others, want)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		var missing, extra []string
		for _, w := range want {
			if !slices.Contains(got, w) {
				missing = append(missing, names[w]+": "+w)
			}
		}
		for _, g := range got {
			if !slices.Contains(want, g) {
				extra = append(extra, g)
			}
		}
		t.Fatalf("evidence %d events, want %d: missing %v, extra %v", len(got), len(want), missing, extra)
	}
}

// stopped puts a session that stopped on asking the owner, and his nudge an hour on.
func stopped(t *testing.T, st *store.Store, harness, session string, at time.Time) {
	t.Helper()
	evs := []store.Event{ownerTurn(session, "build the release", at),
		agentSays(session, "The release is built. Do you want me to publish it now?", at.Add(time.Minute)),
		ownerTurn(session, "continue", at.Add(time.Hour))}
	for i := range evs {
		evs[i].Harness = harness
	}
	put(t, st, evs...)
}

// apiStop puts a session whose turn ended on an API error the transient rule resumes, and his nudge 40 minutes on:
// the rule would have acted first.
func apiStop(t *testing.T, st *store.Store, session string, at time.Time) {
	t.Helper()
	put(t, st, ownerTurn(session, "deploy it", at), agentSays(session, "Deploying.", at.Add(10*time.Second)),
		ev(store.Transcripts, session, "harness_text", store.ActorHarness, "", "API Error: 503 the provider is unable to process your request, a server-side issue",
			`{"is_error":true,"why":"api error"}`, at.Add(time.Minute)),
		ownerTurn(session, "continue", at.Add(40*time.Minute)))
}

// The owner nudging stopped sessions that no rule would continue is a finding, keyed by why none would: the replay
// of the rules over the store's history. A stop a rule would have acted on first is no such nudge; neither is one a
// program typed into two sessions at once, nor an answer. A Hermes conversation's platform comes from Hermes.
func TestTheNudgesNoRuleCoversAreFound(t *testing.T) {
	st := testStore(t)
	for si, s := range []string{"n1", "n2"} {
		stopped(t, st, "claude", s, t0.Add(time.Duration(si)*10*time.Minute))
		stopped(t, st, "claude", s, t0.Add(time.Duration(si)*10*time.Minute+3*time.Hour))
	}
	// a nudge a program typed into two sessions at once; an answer that is no nudge; stops the transient rule would
	// have resumed before his nudge, three in two sessions
	for _, s := range []string{"p1", "p2"} {
		put(t, st, ownerTurn(s, "build the release", t0), agentSays(s, "The release is built. Do you want me to publish it now?", t0.Add(time.Minute)),
			ownerTurn(s, "continue", t0.Add(2*time.Hour)))
	}
	put(t, st, ownerTurn("q", "build the release", t0), agentSays("q", "The release is built. Do you want me to publish it now?", t0.Add(time.Minute)),
		ownerTurn("q", "yes, publish it to the beta channel", t0.Add(time.Hour)))
	apiStop(t, st, "r1", t0)
	apiStop(t, st, "r1", t0.Add(3*time.Hour))
	apiStop(t, st, "r2", t0.Add(10*time.Minute))
	fs, err := Sessions{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	if got := signatures(fs); !slices.Equal(got, []string{"sessions/v1:nudged:its-last-words-ask-the-owner 4"}) {
		t.Fatalf("found %v", got)
	}
	// Hermes conversations in its terminal are supervised as any session; ones whose platform is unknown are not
	st2 := testStore(t)
	stopped(t, st2, "hermes", "h1", t0)
	stopped(t, st2, "hermes", "h1", t0.Add(3*time.Hour))
	stopped(t, st2, "hermes", "h2", t0.Add(10*time.Minute))
	platforms := func(context.Context) (map[string]string, error) {
		return map[string]string{"h1": "cli", "h2": "cli"}, nil
	}
	if fs, err := (Sessions{Platforms: platforms}).Detect(context.Background(), st2, week()); err != nil ||
		!slices.Equal(signatures(fs), []string{"sessions/v1:nudged:its-last-words-ask-the-owner 3"}) {
		t.Fatalf("Hermes in its terminal: %v %v", signatures(fs), err)
	}
}

// Each part of the sessions detector stands when another fails: supervision's record that cannot be read leaves
// the nudges found, and the error comes back beside them; a replay that cannot run says so too.
func TestEachSessionsPartStandsWhenAnotherFails(t *testing.T) {
	broken := func(context.Context) (map[string]string, error) { return nil, fmt.Errorf("no hermes") }
	if _, err := (Sessions{Platforms: broken}).Detect(context.Background(), testStore(t), week()); err == nil {
		t.Fatal("a replay that could not run came back as read")
	}
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for si, s := range []string{"n1", "n2"} {
		stopped(t, st, "claude", s, t0.Add(time.Duration(si)*10*time.Minute))
		stopped(t, st, "claude", s, t0.Add(time.Duration(si)*10*time.Minute+3*time.Hour))
	}
	put(t, st, ev(store.Supervise, "a", "case", store.ActorDetector, "", "x", `{"rule":"transient","why":"four tries"}`, t0))
	// a row of supervision's that does not read
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET heuristic = 'x' WHERE source = ?`, store.Supervise); err != nil {
		t.Fatal(err)
	}
	db.Close()
	fs, err := Sessions{}.Detect(context.Background(), st, week())
	if err == nil || !slices.Equal(signatures(fs), []string{"sessions/v1:nudged:its-last-words-ask-the-owner 4"}) {
		t.Fatalf("found %v, %v", signatures(fs), err)
	}
}

// Run files what the detectors find: a case opens for a new signature, a second run finds it again, a dry run files
// nothing, and a detector that fails is named while the others still file theirs.
func TestRunFilesFindingsIntoCases(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	deny := "<tool_use_error>fence-guard: a process match on 'x' takes its own shell too"
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s1", "Bash", deny, t0.Add(time.Hour)), toolError("s2", "Bash", deny, t0.Add(2*time.Hour)))
	w := week()
	dry, _, err := Run(ctx, st, []Detector{Failures{}}, w, "test", true)
	if err != nil || len(dry) != 1 || len(dry[0].Filed) != 1 || dry[0].Filed[0].Observed != "" {
		t.Fatalf("dry %+v %v", dry, err)
	}
	if cs, _ := st.Cases(ctx, store.CaseFilter{All: true}); len(cs) != 0 {
		t.Fatalf("a dry run filed %d cases", len(cs))
	}
	out, _, err := Run(ctx, st, []Detector{failing{}, Failures{}}, w, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Err == nil || out[1].Err != nil || len(out[1].Filed) != 1 || out[1].Filed[0].Observed != store.Opened {
		t.Fatalf("run %+v", out)
	}
	again, _, err := Run(ctx, st, []Detector{Failures{}}, w, "test", false)
	if err != nil || again[0].Filed[0].Observed != store.Unchanged || again[0].Filed[0].Case.ID != out[1].Filed[0].Case.ID {
		t.Fatalf("again %+v %v", again, err)
	}
	c := out[1].Filed[0].Case
	if c.Family != "failures" || c.Evidence != 3 || c.Title != "fence-guard keeps refusing calls: a process match on Q takes its own shell too" {
		t.Fatalf("case %+v", c)
	}
}

type failing struct{ family string }

func (f failing) Family() string { return firstOf(f.family, "outside") }
func (failing) Detect(context.Context, *store.Store, Window) ([]store.Finding, error) {
	return nil, fmt.Errorf("no source")
}

// partial finds two findings, one the case store refuses, and fails after them.
type partial struct{}

func (partial) Family() string { return "failures" }
func (partial) Detect(context.Context, *store.Store, Window) ([]store.Finding, error) {
	return []store.Finding{{Family: "failures", Signature: "bad\nsignature", Title: "x"},
		{Family: "failures", Signature: "failures/v1:good", Title: "good", Evidence: []store.Evidence{{Ref: "file:/t/g", At: t0}}}}, fmt.Errorf("the replay failed")
}

// What a detector found before it failed is filed, and a finding the store refuses does not keep the next one
// from being filed: each is named, the refusal on its finding and the detector's failure on its run.
func TestRunFilesWhatItCanAndNamesTheRest(t *testing.T) {
	st := testStore(t)
	out, _, err := Run(context.Background(), st, []Detector{partial{}}, week(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	o := out[0]
	if len(o.Filed) != 2 || o.Filed[0].Err == nil || !strings.Contains(o.Filed[0].Err.Error(), "bad case") || o.Filed[1].Observed != store.Opened ||
		o.Err == nil || o.Err.Error() != "the replay failed" || o.Refused() != 1 {
		t.Fatalf("run %+v", o)
	}
}

// The sweep closes the cases unseen past their retention only of the families whose detector read the window: a
// family whose detector failed, or did not run, was not looked at.
func TestTheSweepTakesOnlyTheFamiliesItLookedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	var ids []string
	for _, family := range []string{"failures", "owner-burden", "sessions"} {
		c, _, err := st.ObserveCase(ctx, store.Finding{Family: family, Signature: family + "/v1:old", Title: "an old one",
			Evidence: []store.Evidence{{Ref: "file:/t/" + family, At: time.Now().Add(-40 * 24 * time.Hour)}}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	if _, err := db.Exec(`UPDATE cases SET opened = ?, seen = ?`, old, old); err != nil {
		t.Fatal(err)
	}
	db.Close()
	now := Window{Since: time.Now().Add(-DefaultWindow), Until: time.Now()}
	_, swept, err := Run(ctx, st, []Detector{failing{family: "failures"}, Sessions{}}, now, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 || swept[0].ID != ids[2] {
		t.Fatalf("swept %+v", swept)
	}
	if c, _ := st.CaseByID(ctx, ids[0]); c.Status == store.CaseClosed {
		t.Fatalf("a family whose detector failed was swept")
	}
	if _, swept, err := Run(ctx, st, []Detector{failing{family: "failures"}}, now, "test", false); err != nil || len(swept) != 0 {
		t.Fatalf("nothing looked at, swept %v %v", swept, err)
	}
	hour := Window{Since: time.Now().Add(-time.Hour), Until: time.Now()}
	if _, swept, err := Run(ctx, st, []Detector{OwnerBurden{}}, hour, "test", false); err != nil || len(swept) != 0 {
		t.Fatalf("an hour's look swept %v %v", swept, err)
	}
	if _, swept, err := Run(ctx, st, []Detector{OwnerBurden{}}, now, "test", false); err != nil || len(swept) != 1 || swept[0].ID != ids[1] {
		t.Fatalf("a week's look: %v %v", swept, err)
	}
}

// An /exit ends its session's stop: the owner's input that reopens it is no nudge a rule could have spared him.
func TestANudgeThatReopensAnExitedSessionIsNoFinding(t *testing.T) {
	st := testStore(t)
	for si, s := range []string{"e1", "e2", "e3"} {
		at := t0.Add(time.Duration(si) * 10 * time.Minute)
		put(t, st, ownerTurn(s, "build the release", at), agentSays(s, "The release is built. Do you want me to publish it now?", at.Add(time.Minute)),
			ev(store.Transcripts, s, "harness_text", store.ActorHarness, "", "<command-name>/exit</command-name> <command-message>exit</command-message> <command-args></command-args>",
				`{"input_kind":"slash","why":"local command"}`, at.Add(2*time.Minute)),
			ev(store.Transcripts, s, "harness_text", store.ActorHarness, "", "<local-command-stdout>Bye!</local-command-stdout>", `{"input_kind":"typed","why":"harness text"}`, at.Add(2*time.Minute)),
			ownerTurn(s, "continue", at.Add(2*time.Hour)))
	}
	// opened again with a local command first (claude -c, then /model), then the owner's "continue"
	for si, s := range []string{"m1", "m2", "m3"} {
		at := t0.Add(time.Duration(si)*10*time.Minute + 5*time.Minute)
		put(t, st, ownerTurn(s, "build the release", at), agentSays(s, "The release is built. Do you want me to publish it now?", at.Add(time.Minute)),
			ev(store.Transcripts, s, "harness_text", store.ActorHarness, "", "<command-name>/exit</command-name> <command-message>exit</command-message> <command-args></command-args>",
				`{"input_kind":"slash","why":"local command"}`, at.Add(2*time.Minute)),
			ev(store.Transcripts, s, "harness_text", store.ActorHarness, "", "<command-name>/model</command-name> <command-message>model</command-message> <command-args></command-args>",
				`{"input_kind":"slash","why":"local command"}`, at.Add(2*time.Hour)),
			ownerTurn(s, "continue", at.Add(2*time.Hour+time.Minute)))
	}
	fs, err := Sessions{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range signatures(fs) {
		if strings.Contains(s, "nudged") {
			t.Fatalf("a reopening after /exit read as a nudge: %v", signatures(fs))
		}
	}
}
