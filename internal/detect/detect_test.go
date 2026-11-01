package detect

import (
	"context"
	"database/sql"
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
		{"pgrep -f 'dbtest.py' also matches the shell (pid 4312) in /home/u/x/run.sh at 0x7f3a2c1b.",
			"pgrep -f 'bench.sh 12' also matches the shell (pid 77) in ~/y/z.py at 0x11aa22bb"},
		{"Error: can't open 'config.yaml'", "Error: can't open 'data.json'"},
		{"request req_011CUjQ9xYzAbC failed", "request req_9ZZqT7mmK2pL failed"},
		{"open internal/detect/testdata/a.json: permission denied", "open cmd/x/b.json: permission denied"},
		{"fatal: bad object 3f1c9e2", "fatal: bad object 9ab04de"},
		{"cat: notes.txt: No such file or directory", "cat: run-2.log: No such file or directory"},
		{"deploy@build-a.example: Permission denied (publickey)", "git@forge.example.org: Permission denied (publickey)"},
	} {
		if a, b := Norm(pair[0], 80), Norm(pair[1], 80); a != b {
			t.Errorf("%q and %q key apart: %q, %q", pair[0], pair[1], a, b)
		}
	}
	if got := Norm("pgrep -f 'dbtest.py' also matches the shell (pid 4312) in /home/u/x/run.sh at 0x7f3a2c1b.", 80); got != "pgrep -f Q also matches the shell (pid N) in P at H" {
		t.Errorf("norm %q", got)
	}
	if got := Norm("Error: can't open 'config.yaml'", 80); got != "error: can't open Q" {
		t.Errorf("an apostrophe in a word: %q", got)
	}
	if got := Norm("it's 2'x' and 'y'", 80); got != "it's N'x' and Q" {
		t.Errorf("a quote after a digit: %q", got)
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
	if s := signature("failures", "guard:x:short"); s != "failures/v1:guard:x:short" {
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
		{"claude", "Bash", "<tool_use_error>fence-guard: a loop with sleep and no deadline: it spins if the condition never comes",
			"guard:fence-guard:a loop with sleep and no deadline: it spins if the condition never comes"},
		{"claude", "Bash", "<tool_use_error>load-gate: held `go vet ./...`: the machine is busy, load 20.7 (limit 20).",
			"guard:load-gate:held Q: the machine is busy, load N (limit N)"},
		// a hook's deny names its guard in lower case; Claude Code's own messages there open with a capital
		{"claude", "Write", "<tool_use_error>ledger: a reviewer refused this note", "guard:ledger:a reviewer refused this note"},
		{"claude", "Write", "ledger: a reviewer refused this note", "tool:write:ledger: a reviewer refused this note"},
		{"claude", "Grep", "<tool_use_error>Error: No such tool available: Grep", "tool:grep:error: no such tool available: grep"},
		{"claude", "Write", "Held for the owner's review as rule r7 by the ledger mod: the call waits", "guard:ledger:held for the owner's review as rule rN by the ledger"},
		// a hook by the name its message opens with, the command in Claude Code's brackets, or none
		{"claude", "Bash", "PreToolUse:Bash hook error: deep-scan guard: `rg x` over ~ is a scan of the whole home",
			"guard:deep-scan-guard:Q over P is a scan of the whole home"},
		{"claude", "Bash", "PreToolUse:Bash hook error: keeper-check: Gate C can't be recorded: run the check first", "guard:keeper-check:gate c can't be recorded: run the check first"},
		{"claude", "Write", "PreToolUse:Write hook error: keeper-check: Gate C can't be recorded: run the check first", "guard:keeper-check:gate c can't be recorded: run the check first"},
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
		{"claude", "mcp__web-tools__fetch", "tool failed: spawn failed for /usr/bin/x: spawn /usr/bin/x ENOENT",
			"mcp:web-tools:tool failed: spawn failed for P: spawn P enoent"},
		// Hermes: a tool's error, a guard inside one, a command that exits otherwise than 0, code that failed
		{"hermes", "write_file", `{"error": "BLOCKED: write to protected agent-instruction file(s) (AGENTS.md) approval prompt timed out."}`,
			"tool:write-file:blocked: write to protected agent-instruction file(s) (P) approval prompt timed"},
		{"hermes", "terminal", `{"output": "", "exit_code": -1, "error": "fence-guard: the command waits on nothing"}`, "guard:fence-guard:the command waits on nothing"},
		{"hermes", "terminal", `{"output": "== x: usage: fake-repo-check [-h] --repo REPO", "exit_code": 2, "error": null}`, "exec:usage: fake-repo-check"},
		{"claude", "Bash", "Exit code 2 usage: /opt/tools/bin/x-tool [-h] --repo REPO", "exec:usage: x-tool"},
		{"hermes", "terminal", `{"output": "ls: cannot access 'x': No such file or directory", "exit_code": 0, "error": null}`, ""},
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
		{`{"command":"git push"}`, "exec:U: permission denied", []string{
			"Exit code 128 git@github.com: Permission denied (publickey). fatal: Could not read from remote repository."}},
		{`{"command":"rgg x"}`, "exec:rgg: command not found", []string{"Exit code 127 /bin/bash: line 1: rgg: command not found"}},
		{`{"command":"rgg x"}`, "exec:zsh: command not found: rgg", []string{"Exit code 127 zsh: command not found: rgg"}},
		{`{"command":"waitfor --timeout 1m --file x && wc -l x"}`, "exec:wc: P: no such file or directory", []string{
			"Exit code 1 waitfor: done after 3s wc: x/y: No such file or directory", "Exit code 1 wc: x/z: No such file or directory"}},
		// a program's name past the reach or a sentence's end opens another message; with no name, three words
		{`{"command":"ls x"}`, "exec:x: no such file or directory", []string{
			"Exit code 2 ls: " + strings.Repeat("word ", 25) + "x: No such file or directory", "Exit code 2 ls: done. x: No such file or directory"}},
		{`{"command":"run"}`, "exec:c d e connection refused", []string{"Exit code 1 a b c d e connection refused"}},
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
		"● reads › fails ENOENT: no such file or directory, open 'x' Tests: 1 failed, 1 total",
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
	deny := "<tool_use_error>fence-guard: pgrep -f 'x' also matches the shell running this command"
	curl := func(ms int) string {
		return fmt.Sprintf("Exit code 7 curl: (7) Failed to connect to localhost port 8080 after %d ms: Connection refused", ms)
	}
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s1", "Bash", deny, t0.Add(time.Hour)), toolError("s2", "Bash", deny, t0.Add(2*time.Hour)),
		// one session only: no finding
		toolError("s3", "Bash", "<tool_use_error>fence-guard: an rm whose argument is a glob", t0),
		toolError("s3", "Bash", "<tool_use_error>fence-guard: an rm whose argument is a glob", t0.Add(time.Minute)),
		toolError("s3", "Bash", "<tool_use_error>fence-guard: an rm whose argument is a glob", t0.Add(2*time.Minute)),
		// two sightings in two sessions: no finding
		toolError("s4", "Bash", "<tool_use_error>fence-guard: a session link would go into a commit", t0),
		toolError("s5", "Bash", "<tool_use_error>fence-guard: a session link would go into a commit", t0),
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
	)
	held := "load-gate: held `make`: the machine is busy"
	put(t, st,
		// the transcript's first deny again, 3 s on, its text the same: the same sighting
		ev("mistakes", "s1", "deny", store.ActorHarness, "Bash", "Bash: "+strings.TrimPrefix(deny, "<tool_use_error>"), "", t0.Add(3*time.Second)),
		// a refusal of the model, three times in two sessions
		ev("mistakes", "s7", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative content", "", t0),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative content", "", t0.Add(time.Hour)),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative content", "", t0.Add(2*time.Hour)),
		// a hold of the mistakes log's own: 6 s after a failure of the transcript's, of another tool beside one, and alone;
		// and one a second after the transcript's failure of its session and tool, which is that one
		ev("mistakes", "s1", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0.Add(6*time.Second)),
		ev("mistakes", "s2", "held", store.ActorHarness, "Write", "Write: "+held, "", t0.Add(2*time.Hour+time.Second)),
		ev("mistakes", "s9", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0),
		ev("mistakes", "s2", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0.Add(2*time.Hour+time.Second)),
		// a deny of a guard of one word, which only the log's kind says is a guard's; and the turns' errors
		ev("mistakes", "s10", "deny", store.ActorHarness, "Write", "Write: ledger: a reviewer refused this note", "", t0),
		ev("mistakes", "s11", "deny", store.ActorHarness, "Write", "Write: ledger: a reviewer refused this note", "", t0),
		ev("mistakes", "s11", "deny", store.ActorHarness, "Write", "Write: ledger: a reviewer refused this note", "", t0.Add(time.Minute)),
		ev("mistakes", "s12", "turn-error", store.ActorHarness, "", "API Error: 529 overloaded", "", t0),
		ev("mistakes", "s13", "turn-error", store.ActorHarness, "", "API Error: 529 overloaded", "", t0),
		ev("mistakes", "s13", "turn-error", store.ActorHarness, "", "API Error: 529 overloaded", "", t0.Add(time.Minute)),
	)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"failures/v1:exec:curl: (N) failed to connect to localhost port N after N ms: connection refused 3",
		"failures/v1:guard:fence-guard:pgrep -f Q also matches the shell running this command 3",
		"failures/v1:guard:ledger:a reviewer refused this note 3",
		"failures/v1:guard:load-gate:held Q: the machine is busy 3",
		"failures/v1:model:refusal:this request triggered restrictions on violative content 3",
		"failures/v1:turn:claude:api error: N overloaded 3",
	}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fs[1].Title != "fence-guard keeps refusing calls: pgrep -f Q also matches the shell running this command" || fs[1].Family != "failures" {
		t.Fatalf("finding %+v", fs[1])
	}
	if fs[5].Title != "Claude Code's turns keep failing: api error: N overloaded" {
		t.Fatalf("a turn's error: %q", fs[5].Title)
	}
	if !strings.HasPrefix(fs[1].Evidence[0].Ref, "ev:") || !fs[1].Evidence[0].At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("evidence, newest first: %+v", fs[1].Evidence)
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
	if got := signatures(fs); !slices.Equal(got, []string{"failures/v1:incident:httpx.connecterror: [errno -N] temporary failure in name resolution 2"}) {
		t.Fatalf("found %v", got)
	}
}

// A finding files at most MaxEvidence of its newest sightings on a run.
func TestAFindingFilesItsNewestEvidence(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>fence-guard: pgrep -f 'x' also matches the shell running this command"
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
	if got := OwnerClass("send the apk to my new phone again please", "", []string{"please send the apk to my new phone"}); got != "repeat" {
		t.Errorf("a turn he repeats: %q", got)
	}
	// five words he shares with what he said are a repeat; four are not
	if got := OwnerClass("ship the beta build today", "", []string{"ship the beta build today"}); got != "repeat" {
		t.Errorf("five words again: %q", got)
	}
	if got := OwnerClass("ship the beta build", "", []string{"ship the beta build"}); got != "" {
		t.Errorf("four words again: %q", got)
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
	template := "nightly sweep: why do you ask me, you decide which repo goes first"
	put(t, st,
		agentSays("a", ask, t0), ownerTurn("a", "why do you ask, you can decide", t0.Add(time.Minute)),
		ownerTurn("a", "why do you ask, you can decide", t0.Add(90*time.Second)), // the same turn recorded twice
		agentSays("a", ask, t0.Add(time.Hour)), ownerTurn("a", "no need to ask me that, do it", t0.Add(time.Hour+time.Minute)),
		agentSays("b", ask, t0), ownerTurn("b", "up to you, why ask", t0.Add(2*time.Minute)),
		// his words after momentum's marker
		agentSays("c", ask, t0), ownerTurn("c", "[momentum] why stop, you can decide", t0.Add(time.Minute)),
		// the same handback, typed by programs
		agentSays("d", ask, t0), ownerTurn("d", "Wake nonce=1 why stop, you can decide", t0.Add(time.Minute)),
		agentSays("e", ask, t0), ownerTurn("e", template, t0.Add(time.Minute)),
		agentSays("f", ask, t0), ownerTurn("f", template, t0.Add(2*time.Hour)),
		agentSays("g", ask, t0), ownerTurn("g", template, t0.Add(3*time.Hour)),
		agentSays("h", ask, t0), ownerTurn("h", "why, you decide this", t0.Add(4*time.Hour)),
		agentSays("i", ask, t0), ownerTurn("i", "why, you decide this", t0.Add(4*time.Hour+time.Second)),
		agentSays("j", ask, t0), sub, agentSays("k", ask, t0), other,
	)
	fs, err := OwnerBurden{Programs: Programs{Prefixes: []string{"Wake nonce="}}}.Detect(context.Background(), st, week())
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
	p := Programs{Prefixes: []string{"Wake nonce="}}
	long := "nightly sweep: pick the next repo and go on with its open work"
	turns := []store.Event{
		ownerTurn("a", "go on", t0), ownerTurn("a", "go on", t0.Add(90*time.Second)), ownerTurn("a", "go on", t0.Add(210*time.Second)),
		ownerTurn("b", "Wake nonce=7 look at the queue", t0),
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
// the nudges found, and the error comes back beside them.
func TestEachSessionsPartStandsWhenAnotherFails(t *testing.T) {
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
	deny := "<tool_use_error>fence-guard: pgrep -f 'x' also matches the shell running this command"
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
	if c.Family != "failures" || c.Evidence != 3 || c.Title != "fence-guard keeps refusing calls: pgrep -f Q also matches the shell running this command" {
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
}
