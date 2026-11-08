package request

// Tests from the fourth review of the requests change. Synthetic data only.

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

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
)

// A push's ref lines are read only inside its own block: from its To line to a fetch's From line, the next push's
// To line, or the text's end. A To line is one at a line's start (or, in a text the store joined, a remote's word)
// outside the commit lines.
func TestAPushIsReadInsideItsOwnBlock(t *testing.T) {
	cases := []struct{ name, input, output, want string }{
		{"a fetch after the push", `{"command":"git push origin main && git fetch origin"}`,
			"To github.com:example/x.git\n   9a47efc..5f120dc  main -> main\nFrom github.com:example/x\n * [new branch]      dependabot-x -> origin/dependabot-x",
			"push pushed main -> main (9a47efc..5f120dc) to github.com:example/x.git"},
		{"a fetch after the push, the lines joined", `{"command":"git push origin main && git fetch origin"}`,
			"To github.com:example/x.git 9a47efc..5f120dc main -> main From github.com:example/x * [new branch] dependabot-x -> origin/dependabot-x",
			"push pushed main -> main (9a47efc..5f120dc) to github.com:example/x.git"},
		{"two pushes to two remotes", `{"command":"git push a main && git push b main"}`,
			"To github.com:example/a.git\n   1111111..2222222  main -> main\nTo github.com:example/b.git\n   3333333..4444444  main -> main",
			"push pushed main -> main (1111111..2222222) to github.com:example/a.git | push pushed main -> main (3333333..4444444) to github.com:example/b.git"},
		{"a commit subject that starts with To", `{"command":"git commit -am 'To avoid the race, lock first' && git push origin main"}`,
			"[main 1a2b3c4] To avoid the race, lock first\n 1 file changed\nTo github.com:example/x.git\n   1111111..2222222  main -> main",
			"commit commit 1a2b3c4 on main: To avoid the race, lock first | push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// git prints a commit line only when the commit was made: a call that then failed (a rejected push) still made it.
// Its push made nothing.
func TestAFailedCallsShownCommitWasMade(t *testing.T) {
	out := "[main 1a2b3c4] Fix the parser\n 1 file changed\nTo github.com:example/x.git\n ! [rejected]        main -> main (fetch first)\nerror: failed to push some refs to 'github.com:example/x.git'"
	if got := artifactsAs("Bash", `{"command":"git commit -am 'Fix the parser' && git push origin main"}`, out, true); got != "commit commit 1a2b3c4 on main: Fix the parser" {
		t.Errorf("got %q", got)
	}
}

// An output is decoded as JSON only where the harness keeps it so (Codex's code mode, Hermes): a plain output that
// prints JSON is read as printed.
func TestAPlainOutputIsNotDecodedAsJSON(t *testing.T) {
	out := `[main 1a2b3c4] Fix the parser 1 file changed {"output": "ok", "status": 0}`
	if got := artifactsAs("Bash", `{"command":"git commit -am 'Fix the parser' && cat result.json"}`, out, false); !strings.Contains(got, "1a2b3c4") {
		t.Errorf("commit lost: %q", got)
	}
}

// pairWithID adds a call and its result with a given tool id, as a harness that records the same call again later
// (Hermes re-recording a call, a Codex subagent forked with its parent's context) stores them.
func (f *fixture) pairWithID(session, agent, harness, tool, id string, ts time.Time, input, output string) {
	in, out, callRef, resRef := f.recorded(session, harness, tool, id, input, output, false)
	f.event(session, agent, "tool_call", store.ActorAgent, ts, in, map[string]any{"tool_id": id})
	e := &f.evs[len(f.evs)-1]
	e.Tool, e.Harness, e.Ref = tool, harness, callRef
	f.event(session, agent, "tool_result", store.ActorTool, ts.Add(time.Second), out, map[string]any{"tool_id": id})
	e = &f.evs[len(f.evs)-1]
	e.Tool, e.Harness, e.Ref = tool, harness, resRef
}

// A call is one by its session and tool id: the same call recorded again later, with a new time, made nothing more,
// and nothing after the last turn.
func TestACallRecordedAgainIsOneCall(t *testing.T) {
	f := newFixture(t)
	s := "20261107_100000_rerec"
	commit, out := `{"command": "git add -A && git commit -m docs"}`, `{"output": "[main 1a2b3c4] docs\n 1 file changed", "exit_code": 0, "error": null}`
	patch := `{"mode": "replace", "path": "/work/d.md", "old_string": "a", "new_string": "b"}`
	f.owner(s, at(0), "commit the docs")
	f.evs[len(f.evs)-1].Harness = "hermes"
	f.pairWithID(s, "", "hermes", "terminal", "call_r1", at(1), commit, out)
	f.pairWithID(s, "", "hermes", "patch", "call_r2", at(1).Add(5*time.Second), patch, `{"success": true}`)
	f.reply(s, at(2), "Committed.")
	f.owner(s, at(5), "anything else left?")
	f.evs[len(f.evs)-1].Harness = "hermes"
	f.pairWithID(s, "", "hermes", "terminal", "call_r1", at(6), commit, out)
	f.pairWithID(s, "", "hermes", "patch", "call_r2", at(6).Add(5*time.Second), patch, `{"success": true}`)
	f.phase("hermes:"+s, at(7), "phase", "idle", nil)
	f.evs[len(f.evs)-1].Harness = "hermes"
	rq := f.group()[0]
	if n := strings.Count(kinds(rq.Results), "1a2b3c4"); n != 1 {
		t.Errorf("one commit made, %d counted: %s", n, kinds(rq.Results))
	}
	for _, x := range rq.Results {
		if x.Kind == KindFile && x.Writes != 1 {
			t.Errorf("one write made, %d counted", x.Writes)
		}
	}
	if rq.State == Ready {
		t.Errorf("ready on a call recorded again, not made again: %s", rq.Why)
	}
}

// A Codex subagent forked with its parent's context carries the parent's calls over with new times: a commit made
// before a pasted handoff is not the pasted request's.
func TestAForkCarriesNoCommitIntoAPastedRequest(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000f0f0"
	other := ID(at(-100), "the other ask")
	out := "Process exited with code 0\nOutput:\n[main 1a2b3c4] Fix the parser"
	f.owner("s-other", at(-100), "the other ask")
	f.owner(s, at(0), "fix the parser")
	f.pairWithID(s, "", "codex", "exec_command", "call_p1", at(1), `{"cmd":"git commit -am 'Fix the parser'"}`, out)
	f.owner(s, at(5), "momentum request "+other+" - a handoff of the owner request\nmomentum request "+other)
	f.pairWithID(s, "019a0000-0000-7000-8000-00000000c1d0", "codex", "exec_command", "call_p1", at(6), `{"cmd":"git commit -am 'Fix the parser'"}`, out)
	for _, rq := range f.group() {
		if rq.ID == other && strings.Contains(kinds(rq.Results), "1a2b3c4") {
			t.Errorf("the pasted request holds a commit its own segment never made: %s", kinds(rq.Results))
		}
	}
}

// What a whole read took off a transcript is kept only parsed and masked: no raw record stays in the reader after
// its job, and no unmasked token is in what a request keeps, its keys included.
func TestARawRecordIsLetGoAfterItsJob(t *testing.T) {
	f := newFixture(t)
	command := "cd " + longDir + " && git commit -m 'Fix the parser' && git push https://bot:SECRET-TOKEN-1234@github.com/example/x.git main"
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed\nTo https://bot:SECRET-TOKEN-1234@github.com/example/x.git\n   1111111..2222222  main -> main"
	claudeCall(t, f, t.TempDir(), "s-claude-raw2", command, output)
	f.put()
	r := newReader(f.st, Options{Redact: fakeRedact})
	defer r.close()
	r.enqueue("s-claude-raw2")
	reqs, err := r.settle(context.Background(), t0.Add(-time.Hour), func(rq *Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(r.records) != 0 {
		t.Errorf("%d raw records kept after the run", len(r.records))
	}
	pushed := false
	for _, rq := range reqs {
		for _, x := range rq.Results {
			pushed = pushed || x.Kind == KindPush
			if strings.Contains(x.Text, "SECRET-TOKEN-1234") || strings.Contains(x.Key, "SECRET-TOKEN-1234") {
				t.Errorf("an unmasked token kept: %q %q", x.Key, x.Text)
			}
		}
	}
	if !pushed {
		t.Fatalf("the push was not read: %s", kinds(reqs[0].Results))
	}
}

// A compressed transcript is decompressed once: a cut call's input and its cut output are read in one pass.
func TestACompressedTranscriptIsReadOnce(t *testing.T) {
	f := newFixture(t)
	command := "cd " + longDir + " && git commit -m 'Fix the parser'"
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
	path := claudeCall(t, f, t.TempDir(), "s-claude-zst", command, output)
	plain, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".zst", enc.EncodeAll(plain, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.put()
	r := newReader(f.st, Options{Redact: fakeRedact})
	defer r.close()
	r.enqueue("s-claude-zst")
	reqs, err := r.settle(context.Background(), t0.Add(-time.Hour), func(rq *Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(reqs[0].Results); !strings.Contains(got, "1a2b3c4") {
		t.Fatalf("the compressed transcript was not read: %s", got)
	}
	if n := r.opens[path]; n != 1 {
		t.Errorf("the compressed transcript was opened %d times", n)
	}
}

// A request on a harness no sessions run reads (Kimi, Grok) never has a phase, and says that is why its state is
// not known.
func TestAHarnessNoSessionsRunReadsSaysSo(t *testing.T) {
	f := newFixture(t)
	f.owner("s-kimi", at(0), "fix the parser")
	f.evs[len(f.evs)-1].Harness = "kimi"
	f.tool("s-kimi", "", "Shell", at(1), `{"command":"git commit -m fix"}`, "[main 1a2b3c4] fix", false)
	f.reply("s-kimi", at(2), "Done.")
	rq := f.group()[0]
	if rq.State != Unknown || !strings.Contains(rq.Why, "no sessions run reads kimi sessions") {
		t.Errorf("state %s: %s", rq.State, rq.Why)
	}
}

// The question a session waits on the owner with is masked before the open step quotes it, and left out when it
// cannot be.
func TestTheOpenStepQuotesAMaskedAsk(t *testing.T) {
	for _, c := range []struct {
		name   string
		redact func(context.Context, []string) ([]string, error)
		want   string
	}{
		{"masked", fakeRedact, "waits on the owner (question): Push with " + strings.Repeat("*", len("SECRET-TOKEN-1234")) + "?"},
		{"cannot be masked", func(context.Context, []string) ([]string, error) { return nil, fmt.Errorf("no scrubber") }, "waits on the owner (question)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-x", at(0), "push the fix")
			f.reply("s-x", at(1), "Ready to push.")
			f.phase("s-x", at(9), "phase", "needs_you", map[string]any{"ask": "Push with SECRET-TOKEN-1234?", "ask_kind": "question"})
			rq := f.groupWith(Options{Redact: c.redact})[0]
			if !strings.HasSuffix(rq.Open, c.want) || strings.Contains(rq.Open, "SECRET") {
				t.Errorf("open %q", rq.Open)
			}
		})
	}
}

// A record at a call's place that is no longer the call's (its tool id is another's) is gone: nothing is read off
// it, for each harness's record.
func TestARecordThatIsNoLongerTheCallsIsGone(t *testing.T) {
	command := "git -C " + longDir + " commit -m 'Fix the parser'" // the stored part names git: it is not skipped
	output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
	check := func(t *testing.T, rq *Request) {
		t.Helper()
		if strings.Contains(kinds(rq.Results), "1a2b3c4") || rq.CutGone != 1 {
			t.Errorf("results %s, cut and gone %d", kinds(rq.Results), rq.CutGone)
		}
	}
	claude := func(t *testing.T, useID, resultID string) *Request {
		f := newFixture(t)
		session := "s-claude-other"
		path := filepath.Join(t.TempDir(), session+".jsonl")
		input, _ := json.Marshal(map[string]string{"command": command})
		offs := writeLines(t, path,
			`{"type":"user","sessionId":"`+session+`","message":{"role":"user","content":"fix the parser"}}`,
			`{"type":"assistant","sessionId":"`+session+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"`+useID+`","name":"Bash","input":`+string(input)+`}]}}`,
			`{"type":"user","sessionId":"`+session+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+resultID+`","content":`+jsonString(output)+`}]}}`)
		f.owner(session, at(0), "fix the parser")
		f.cutPair(session, "claude", "Bash", "toolu_q1", at(1), string(input), output,
			fmt.Sprintf("%s@%d#0", path, offs[1]), fmt.Sprintf("%s@%d#0", path, offs[2]))
		return f.groupWith(Options{Redact: fakeRedact})[0]
	}
	t.Run("a Claude Code call", func(t *testing.T) { check(t, claude(t, "toolu_other", "toolu_q1")) })
	t.Run("a Claude Code result", func(t *testing.T) { check(t, claude(t, "toolu_q1", "toolu_other")) })
	t.Run("a Codex call", func(t *testing.T) {
		f := newFixture(t)
		session := "019a0000-0000-7000-8000-00000000c0d1"
		path := filepath.Join(t.TempDir(), "rollout-2026-11-07T10-00-00-"+session+".jsonl")
		args, _ := json.Marshal(map[string]string{"cmd": command})
		out := "Process exited with code 0\nOutput:\n" + output
		offs := writeLines(t, path,
			`{"type":"session_meta","payload":{"id":"`+session+`"}}`,
			`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":`+jsonString(string(args))+`,"call_id":"call_other"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_c1","output":`+jsonString(out)+`}}`)
		f.owner(session, at(0), "fix the parser")
		f.cutPair(session, "codex", "exec_command", "call_c1", at(1), string(args), out,
			fmt.Sprintf("%s@%d#0", path, offs[1]), fmt.Sprintf("%s@%d#0", path, offs[2]))
		check(t, f.groupWith(Options{Redact: fakeRedact})[0])
	})
	hermes := func(t *testing.T, callRow, resultRow int, resultID string) *Request {
		f := newFixture(t)
		path := filepath.Join(t.TempDir(), "state.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		session := "20261107_100000_hother"
		args, _ := json.Marshal(map[string]string{"command": command})
		calls, _ := json.Marshal([]map[string]any{{"id": "call_h1", "type": "function", "function": map[string]string{"name": "terminal", "arguments": string(args)}}})
		out, _ := json.Marshal(map[string]any{"output": output, "exit_code": 0})
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{hermesSchema, nil},
			{`insert into sessions (id, source, started_at) values (?, 'cli', 1)`, []any{session}},
			{`insert into messages (id, session_id, role, content, tool_calls, timestamp) values (?, ?, 'assistant', '', ?, 2)`, []any{callRow, session, string(calls)}},
			{`insert into messages (id, session_id, role, content, tool_call_id, tool_name, timestamp) values (?, ?, 'tool', ?, ?, 'terminal', 3)`,
				[]any{resultRow, session, string(out), resultID}},
		} {
			if _, err := db.Exec(q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		f.owner(session, at(0), "fix the parser")
		f.cutPair(session, "hermes", "terminal", "call_h1", at(1), string(args), string(out), path+"@1#0", path+"@2#0")
		return f.groupWith(Options{Redact: fakeRedact})[0]
	}
	// the rows at the call's places are gone; later rows hold the same call
	t.Run("a Hermes row that is another's", func(t *testing.T) { check(t, hermes(t, 3, 4, "call_h1")) })
	t.Run("a Hermes result of another call", func(t *testing.T) { check(t, hermes(t, 1, 2, "call_other")) })
}

// A Codex subagent forked with its parent's context, in a session of its own, carries the parent's call over with
// its tool id at a new time: the call made one push.
func TestAForksCopyInItsOwnSessionIsOneCall(t *testing.T) {
	f := newFixture(t)
	push, out := `{"cmd":"git push -u origin cache"}`, "Process exited with code 0\nOutput:\nTo github.com:example/x.git\n * [new branch]      cache -> cache"
	f.owner("s-main", at(0), "add the cache")
	f.pairWithID("s-main", "", "codex", "exec_command", "call_w1", at(1), push, out)
	f.event("s-main", "t-child", "agent_text", store.ActorAgent, at(3), "child working", nil)
	f.pairWithID("t-child", "", "codex", "exec_command", "call_w1", at(4), push, out)
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("requests %d", len(reqs))
	}
	if n := strings.Count(kinds(reqs[0].Results), "pushed new branch"); n != 1 {
		t.Errorf("one push made, %d counted: %s", n, kinds(reqs[0].Results))
	}
}
