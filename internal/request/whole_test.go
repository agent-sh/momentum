package request

// Tests of reading a cut call whole from its transcript. Synthetic transcripts in each harness's own shape.

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

	_ "modernc.org/sqlite"

	"github.com/agent-sh/momentum/internal/store"
)

// fakeRedact masks the one secret the fixtures hold, as scrump would.
func fakeRedact(_ context.Context, texts []string) ([]string, error) {
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = strings.ReplaceAll(t, "SECRET-TOKEN-1234", "*****************")
	}
	return out, nil
}

// storedCut is a text as the store keeps it: on one line, its first 240 bytes and an ellipsis.
func storedCut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 240 {
		return s
	}
	return s[:240] + "…"
}

// writeLines writes a transcript and returns each line's offset.
func writeLines(t *testing.T, path string, lines ...string) []int64 {
	t.Helper()
	var b strings.Builder
	var offs []int64
	for _, l := range lines {
		offs = append(offs, int64(b.Len()))
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return offs
}

// cutPair adds a call and its result as the store holds them: cut, each with its record's place.
func (f *fixture) cutPair(session, harness, tool, id string, ts time.Time, input, output, callRef, resRef string) {
	f.event(session, "", "tool_call", store.ActorAgent, ts, storedCut(input), map[string]any{"tool_id": id})
	e := &f.evs[len(f.evs)-1]
	e.Tool, e.Harness, e.Ref = tool, harness, callRef
	f.event(session, "", "tool_result", store.ActorTool, ts.Add(time.Second), storedCut(output), map[string]any{"tool_id": id})
	e = &f.evs[len(f.evs)-1]
	e.Tool, e.Harness, e.Ref = tool, harness, resRef
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A long directory and hook output put the command's git and the commit line past the store's cut.
var (
	longDir  = "/work/" + strings.Repeat("deep/", 50) + "repo"
	hookText = strings.Repeat("hook: checking the formatting of every file in the tree\n", 6)
)

// claudeCall writes a Claude Code transcript holding one Bash call and its result, and stores them cut.
func claudeCall(t *testing.T, f *fixture, dir, session, command, output string) string {
	path := filepath.Join(dir, session+".jsonl")
	input, _ := json.Marshal(map[string]string{"command": command, "description": "Commit the fix"})
	offs := writeLines(t, path,
		`{"type":"user","sessionId":"`+session+`","timestamp":"2026-11-07T10:00:00.000Z","message":{"role":"user","content":"fix the parser"}}`,
		`{"type":"assistant","sessionId":"`+session+`","timestamp":"2026-11-07T10:01:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_q1","name":"Bash","input":`+string(input)+`}]}}`,
		`{"type":"user","sessionId":"`+session+`","timestamp":"2026-11-07T10:01:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_q1","content":`+jsonString(output)+`,"is_error":false}]}}`)
	f.owner(session, at(0), "fix the parser")
	f.cutPair(session, "claude", "Bash", "toolu_q1", at(1), string(input), output,
		fmt.Sprintf("%s@%d#0", path, offs[1]), fmt.Sprintf("%s@%d#0", path, offs[2]))
	return path
}

// A Claude Code call the store cut is read whole from its transcript: the git past the cut, the commit line past
// the cut, and the push's remote with its token masked before anything is read off.
func TestACutClaudeCallIsReadWhole(t *testing.T) {
	f := newFixture(t)
	command := "cd " + longDir + " && git commit -m 'Fix the parser' && git push https://bot:SECRET-TOKEN-1234@github.com/example/x.git main"
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed\nTo https://bot:SECRET-TOKEN-1234@github.com/example/x.git\n   1111111..2222222  main -> main"
	claudeCall(t, f, t.TempDir(), "s-claude-cut", command, output)
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	got := kinds(rq.Results)
	if !strings.Contains(got, "commit 1a2b3c4 on main: Fix the parser") || !strings.Contains(got, "1111111..2222222") {
		t.Fatalf("results: %s", got)
	}
	if h := Handoff(rq); strings.Contains(h, "SECRET-TOKEN") || strings.Contains(h, "details cut") {
		t.Fatalf("handoff:\n%s", h)
	}
}

// With the transcript gone, the call is read as the store holds it, and the handoff says so.
func TestACutCallWhoseTranscriptIsGoneFallsBack(t *testing.T) {
	f := newFixture(t)
	command := "git -C " + longDir + " commit -m 'Fix the parser'"
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
	path := claudeCall(t, f, t.TempDir(), "s-claude-gone", command, output)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if strings.Contains(kinds(rq.Results), "1a2b3c4") {
		t.Fatalf("a commit from a transcript that is gone: %s", kinds(rq.Results))
	}
	if h := Handoff(rq); !strings.Contains(h, "details cut, transcript gone: 1 of its calls") {
		t.Fatalf("handoff:\n%s", h)
	}
}

// When the text read whole cannot be redacted, nothing raw is read off it: the call is read as the store holds it.
func TestACutCallThatCannotBeRedactedIsNotReadOff(t *testing.T) {
	f := newFixture(t)
	command := "cd " + longDir + " && git commit -m 'Fix the parser'"
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
	claudeCall(t, f, t.TempDir(), "s-claude-raw", command, output)
	failing := func(context.Context, []string) ([]string, error) { return nil, fmt.Errorf("no scrubber") }
	rq := f.groupWith(Options{Redact: failing})[0]
	if strings.Contains(kinds(rq.Results), "1a2b3c4") || !strings.Contains(Handoff(rq), "details cut, not masked: 1 of its calls") {
		t.Fatalf("results %s\n%s", kinds(rq.Results), Handoff(rq))
	}
}

// A Codex call the store cut is read whole from its rollout.
func TestACutCodexCallIsReadWhole(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	session := "019a0000-0000-7000-8000-00000000c0de"
	path := filepath.Join(dir, "rollout-2026-11-07T10-00-00-"+session+".jsonl")
	args, _ := json.Marshal(map[string]string{"cmd": "cd " + longDir + " && git commit -am 'Fix the parser'", "workdir": "/work/x"})
	output := "Chunk ID: 1f\nWall time: 0.4 seconds\nProcess exited with code 0\nOutput:\n" + hookText + "[main 2b3c4d5] Fix the parser\n 1 file changed"
	offs := writeLines(t, path,
		`{"timestamp":"2026-11-07T10:00:00.000Z","type":"session_meta","payload":{"id":"`+session+`","cwd":"/work/x","originator":"codex-tui"}}`,
		`{"timestamp":"2026-11-07T10:01:00.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":`+jsonString(string(args))+`,"call_id":"call_c1"}}`,
		`{"timestamp":"2026-11-07T10:01:01.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_c1","output":`+jsonString(output)+`}}`)
	f.owner(session, at(0), "fix the parser")
	f.cutPair(session, "codex", "exec_command", "call_c1", at(1), string(args), output,
		fmt.Sprintf("%s@%d#0", path, offs[1]), fmt.Sprintf("%s@%d#0", path, offs[2]))
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); !strings.Contains(got, "commit 2b3c4d5 on main: Fix the parser") {
		t.Fatalf("results: %s", got)
	}
}

// hermesSchema is the columns of Hermes's state.db that tacit reads.
const hermesSchema = `create table sessions (id text primary key, source text not null, created_source text, model text, model_config text,
  parent_session_id text, started_at real not null, ended_at real, end_reason text, cwd text, git_repo_root text, title text);
create table messages (id integer primary key autoincrement, session_id text not null references sessions(id), role text not null,
  content text, tool_call_id text, tool_calls text, tool_name text, timestamp real not null, finish_reason text,
  _compressed_summary integer default 0, display_kind text);`

// A Hermes call the store cut is read whole from Hermes's store, by its message's id.
func TestACutHermesCallIsReadWhole(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session := "20261107_100000_hermes"
	args, _ := json.Marshal(map[string]string{"command": "cd " + longDir + " && git push origin main"})
	calls, _ := json.Marshal([]map[string]any{{"id": "call_h1", "type": "function", "function": map[string]string{"name": "terminal", "arguments": string(args)}}})
	output, _ := json.Marshal(map[string]any{"output": hookText + "To github.com:example/x.git\n   3333333..4444444  main -> main", "exit_code": 0})
	if _, err := db.Exec(hermesSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into sessions (id, source, started_at) values (?, 'cli', 1)`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into messages (id, session_id, role, content, tool_calls, timestamp) values (1, ?, 'assistant', '', ?, 2)`, session, string(calls)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into messages (id, session_id, role, content, tool_call_id, tool_name, timestamp) values (2, ?, 'tool', ?, 'call_h1', 'terminal', 3)`,
		session, string(output)); err != nil {
		t.Fatal(err)
	}
	f.owner(session, at(0), "push the fix")
	f.cutPair(session, "hermes", "terminal", "call_h1", at(1), string(args), string(output), path+"@1#0", path+"@2#0")
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); !strings.Contains(got, "pushed main -> main (3333333..4444444) to github.com:example/x.git") {
		t.Fatalf("results: %s", got)
	}
}

// A cut call is selected by the git flag the source recorded of its whole command: one that names no git opens no
// transcript, and once its transcript is gone it is no call whose details the request lost; one whose git lies past
// the cut is read whole.
func TestACutCallIsReadByItsGitFlag(t *testing.T) {
	for _, c := range []struct {
		name, command, flag string
		remove              bool
		opens, gone         int
		commit              bool
	}{
		{name: "no git", command: "cd " + longDir + " && make test", flag: "no"},
		{name: "no git, transcript gone", command: "cd " + longDir + " && make test", flag: "no", remove: true},
		{name: "git past the cut", command: "cd " + longDir + " && git commit -m 'Fix the parser'", flag: "yes", opens: 2, commit: true}, // its input, then its output
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			path := claudeCall(t, f, t.TempDir(), "s-flag", c.command, hookText+"[main 1a2b3c4] Fix the parser\n 1 file changed")
			for i := range f.evs {
				if e := &f.evs[i]; e.Kind == "tool_call" {
					e.Meta = metaJSON(map[string]any{"tool_id": metaOf(e.Meta).ToolID, store.GitKey: c.flag})
				}
			}
			if c.remove {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			f.put()
			r := newReader(f.st, Options{Redact: fakeRedact})
			defer r.close()
			r.enqueue("s-flag")
			reqs, err := r.settle(context.Background(), t0.Add(-time.Hour), func(*Request) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			rq := reqs[0]
			if n := r.opens[path]; n != c.opens {
				t.Errorf("the transcript was opened %d times, want %d", n, c.opens)
			}
			if rq.CutGone != c.gone {
				t.Errorf("%d calls counted cut with their transcript gone, want %d", rq.CutGone, c.gone)
			}
			if got := kinds(rq.Results); strings.Contains(got, "1a2b3c4") != c.commit {
				t.Errorf("results: %s", got)
			}
		})
	}
}

// The flag a source records is the whole read's decision: git or gh as a word of the decoded commands, and none for a
// Codex call that polls a session (the session ran a command of its own) or a tool that is no shell.
func TestGitFlagIsTheWholeReadsDecision(t *testing.T) {
	for _, c := range []struct{ harness, tool, input, want string }{
		{"claude", "Bash", `{"command":"cd /x &&\ngit push"}`, "yes"},
		{"claude", "Bash", `{"command":"cat .github/ci.yml | grep high"}`, "no"},
		{"codex", "exec_command", `{"cmd":"gh pr create --fill"}`, "yes"},
		{"codex", "exec_command", `{"cmd":"make test"}`, "no"},
		{"codex", "exec", `text(await tools.exec_command({cmd:"make test"}))`, "no"},
		{"codex", "exec", `for(const id of [41,42]) text({id,...await tools.write_stdin({session_id:id,chars:""})})`, ""},
		{"codex", "write_stdin", `{"chars":"","session_id":41}`, ""},
		{"claude", "Write", `{"file_path":"/x/notes.txt","content":"git push"}`, ""},
	} {
		if got := GitFlag(c.harness, c.tool, c.input); got != c.want {
			t.Errorf("%s %s %s: %q, want %q", c.harness, c.tool, c.input, got, c.want)
		}
	}
}
