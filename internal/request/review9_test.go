package request

// Tests from the ninth review of the requests change. Synthetic data only. The first two began as the review's own
// probes.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A code-mode script that first polls a command an earlier cell started: the poll's result is that command's, not
// the next command's, so gh pr create, still running, does not take the address gh pr view printed.
func TestAForeignPollTakesNoCommandsResult(t *testing.T) {
	script := `text(await tools.write_stdin({session_id:41,chars:"",yield_time_ms:1000})); ` +
		`text(await tools.exec_command({cmd:"gh pr view 5 --json url -q .url"})); ` +
		`text(await tools.exec_command({cmd:"gh pr create --fill",yield_time_ms:1000})); ` +
		`text(await tools.exec_command({cmd:"npm test",yield_time_ms:1000}))`
	output := `Script completed Output: {"chunk_id":"a","wall_time_seconds":0.5,"exit_code":0,"original_token_count":3,"output":"build ok\n"} ` +
		`{"chunk_id":"b","wall_time_seconds":0.4,"exit_code":0,"original_token_count":5,"output":"https://github.com/o/r/pull/5\n"} ` +
		`{"chunk_id":"c","wall_time_seconds":1.0,"session_id":42,"original_token_count":0,"output":""} ` +
		`{"chunk_id":"d","wall_time_seconds":1.0,"session_id":43,"original_token_count":0,"output":""}`
	if got := artifactsAs("exec", script, output, false); got != "" {
		t.Errorf("gh pr create, still running, took gh pr view's address: %q", got)
	}
}

// The same pairing keeps the pull request gh pr create printed on gh pr create.
func TestAForeignPollShiftsNoResult(t *testing.T) {
	script := `text(await tools.write_stdin({session_id:41,chars:"",yield_time_ms:1000})); ` +
		`text(await tools.exec_command({cmd:"gh pr create --fill"})); ` +
		`text(await tools.exec_command({cmd:"npm ci > ci.log 2>&1",yield_time_ms:1000})); ` +
		`text(await tools.exec_command({cmd:"npm install > in.log 2>&1",yield_time_ms:1000}))`
	output := `Script completed Output: {"chunk_id":"a","wall_time_seconds":0.5,"exit_code":0,"original_token_count":3,"output":"build ok\n"} ` +
		`{"chunk_id":"b","wall_time_seconds":0.4,"exit_code":0,"original_token_count":5,"output":"https://github.com/o/r/pull/33\n"} ` +
		`{"chunk_id":"c","wall_time_seconds":1.0,"session_id":42,"original_token_count":0,"output":""} ` +
		`{"chunk_id":"d","wall_time_seconds":1.0,"session_id":43,"original_token_count":0,"output":""}`
	if got := artifactsAs("exec", script, output, false); got != "pr pull request https://github.com/o/r/pull/33" {
		t.Errorf("got %q", got)
	}
}

// A poll in the script of a session one of its own commands yielded, by its literal id: that command's result.
func TestAPollOfASessionTheScriptYielded(t *testing.T) {
	script := `text(await tools.exec_command({cmd:"git push origin main",yield_time_ms:1000})); ` +
		`text(await tools.write_stdin({session_id:42,chars:"",yield_time_ms:30000}))`
	output := `Script completed Output: {"chunk_id":"a","session_id":42,"output":"running the pre-push checks\n"} ` +
		`{"chunk_id":"b","exit_code":0,"output":"To github.com:o/r.git\n   1111111..2222222  main -> main\n"}`
	if got := artifactsAs("exec", script, output, false); got != "push pushed main -> main (1111111..2222222) to github.com:o/r.git" {
		t.Errorf("got %q", got)
	}
}

// A later cell's poll of a session an earlier cell left running is read against the earlier command, with its
// guards: gh pr create's address counts, gh pr view's does not.
func TestALaterCellsPollIsTheEarlierCommands(t *testing.T) {
	for _, c := range []struct{ cmd, want string }{
		{"gh pr create --fill", "pr pull request https://github.com/o/r/pull/7"},
		{"gh pr view 7 --json url -q .url", ""},
	} {
		f := newFixture(t)
		s := "019a0000-0000-7000-8000-00000000a0a0"
		f.owner(s, at(0), "open the pull request")
		f.pairWithID(s, "", "codex", "exec", "call_c1", at(1), `text(await tools.exec_command({cmd:`+jsonString(c.cmd)+`,yield_time_ms:1000}))`,
			`Script completed Output: {"chunk_id":"a","session_id":42,"output":""}`)
		f.pairWithID(s, "", "codex", "exec", "call_c2", at(2), `text(await tools.write_stdin({session_id:42,chars:"",yield_time_ms:30000}))`,
			`Script completed Output: {"chunk_id":"b","exit_code":0,"output":"https://github.com/o/r/pull/7\n"}`)
		if got := kinds(f.group()[0].Results); got != c.want {
			t.Errorf("%s: got %q, want %q", c.cmd, got, c.want)
		}
	}
}

// Codex's write_stdin tool polls a session its exec_command tool left running: the poll's result is that
// command's.
func TestCodexsWriteStdinToolPollsItsSession(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000a0a1"
	f.owner(s, at(0), "push the fix")
	f.pairWithID(s, "", "codex", "exec_command", "call_e1", at(1), `{"cmd":"git push origin main","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 77\nOutput:\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_w1", at(2), `{"session_id":77,"chars":""}`,
		"Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nTo github.com:o/r.git\n   1111111..2222222  main -> main\n")
	f.pairWithID(s, "", "codex", "exec_command", "call_e2", at(3), `{"cmd":"git push origin topic","yield_time_ms":1000}`,
		"Chunk ID: c\nWall time: 1.0 seconds\nProcess running with session ID 78\nOutput:\n")
	// a poll whose command exited non-zero: its ref lines are no push
	f.pairWithID(s, "", "codex", "write_stdin", "call_w2", at(4), `{"session_id":78,"chars":""}`,
		"Chunk ID: d\nWall time: 3.0 seconds\nProcess exited with code 1\nOutput:\nTo github.com:o/r.git\n   3333333..4444444  topic -> topic\n")
	if got := kinds(f.group()[0].Results); got != "push pushed main -> main (1111111..2222222) to github.com:o/r.git" {
		t.Errorf("got %q", got)
	}
}

// A JavaScript escape past the last code point is no character: it is read as written.
func TestAnEscapePastTheLastCodePointIsKept(t *testing.T) {
	if s, _, _ := literal(`"a\u{110000}b"`); s != `a\u{110000}b` {
		t.Errorf("got %q", s)
	}
}

// Each reason tacit gives user-role text that may carry the owner's handoff links it; another agent's report does
// not.
func TestEachLinkingReasonLinks(t *testing.T) {
	for _, c := range []struct {
		why   string
		links bool
	}{
		{"cut from the owner's turn", true},
		{"agent text in the owner's turn", true},
		{"codex exec", true},
		{"pi headless", true},
		{"agent cwd", true},
		{"entrypoint sdk-cli", true},
		{"originator codex_exec", true},
		{"codex thread_source subagent", true},
		{"hermes session from cron", true},
		{"message from another agent", false},
	} {
		t.Run(c.why, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-src", at(0), "index the archive")
			src := f.group()[0]
			f.event("s-next", "", "agent_text", store.ActorAgent, at(10), "Continue this. "+Line(src.ID)+" ... "+Line(src.ID), map[string]any{"why": c.why})
			f.tool("s-next", "", "Bash", at(11), `{"command":"git commit -m index"}`, "[main 7c7c7c7] Index the archive", false)
			linked := false
			for _, rq := range f.group() {
				linked = linked || rq.ID == src.ID && strings.Contains(kinds(rq.Results), "7c7c7c7")
			}
			if linked != c.links {
				t.Errorf("linked %v", linked)
			}
		})
	}
}

// The same words again soon are one turn only when they paste the same handoff: the turn that pastes the request's
// handoff after the same words is an ask of its own.
func TestTheSameWordsWithAHandoffAreAnotherTurn(t *testing.T) {
	f := newFixture(t)
	f.owner("s-h", at(0), "fix the parser")
	rq := f.group()[0]
	f.owner("s-h", at(1), "continue")
	f.owner("s-h", at(1).Add(30*time.Second), "continue\n"+Handoff(rq))
	rq = f.group()[0]
	if n := len(rq.Asks); n != 3 || rq.Asks[2].Handoff != rq.ID {
		t.Errorf("asks %q, the last's handoff %q", words(rq.Asks), rq.Asks[len(rq.Asks)-1].Handoff)
	}
}

// A Hermes delegation's rows are its own session's: a cut call of it is read whole by the delegation's id.
func TestADelegationsCutCallIsReadByItsSession(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	parent, child := "20261107_100000_parent", "20261107_100500_child"
	args, _ := json.Marshal(map[string]string{"command": "cd " + longDir + " && git push origin main"})
	calls, _ := json.Marshal([]map[string]any{{"id": "call_d1", "type": "function", "function": map[string]string{"name": "terminal", "arguments": string(args)}}})
	output, _ := json.Marshal(map[string]any{"output": hookText + "To github.com:example/x.git\n   3333333..4444444  main -> main", "exit_code": 0})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{hermesSchema, nil},
		{`insert into sessions (id, source, started_at) values (?, 'cli', 1), (?, 'delegate', 2)`, []any{parent, child}},
		{`insert into messages (id, session_id, role, content, tool_calls, timestamp) values (1, ?, 'assistant', '', ?, 2)`, []any{child, string(calls)}},
		{`insert into messages (id, session_id, role, content, tool_call_id, tool_name, timestamp) values (2, ?, 'tool', ?, 'call_d1', 'terminal', 3)`,
			[]any{child, string(output)}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	f.owner(parent, at(0), "push the fix")
	f.cutPair(parent, "hermes", "terminal", "call_d1", at(1), string(args), string(output), path+"@1#0", path+"@2#0")
	f.evs[len(f.evs)-1].Agent, f.evs[len(f.evs)-2].Agent = child, child
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); !strings.Contains(got, "3333333..4444444") {
		t.Errorf("results %q", got)
	}
}
