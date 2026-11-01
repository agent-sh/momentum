package detect

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// ev is a synthetic event; its key is new each time.
func ev(source, session, kind string, actor store.Actor, tool, summary, meta string, at time.Time) store.Event {
	seq++
	return store.Event{Source: source, SourceKey: fmt.Sprintf("%s@%06d", session, seq), TS: at, Session: session, Harness: "claude",
		Actor: actor, Kind: kind, Tool: tool, Summary: summary, Meta: meta}
}

func toolError(session, tool, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "tool_result", store.ActorTool, tool, text, `{"is_error":true,"tool_id":"x"}`, at)
}

func week() Window { return Window{Since: t0.Add(-24 * time.Hour), Until: t0.Add(6 * 24 * time.Hour)} }

func signatures(fs []store.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s %d", f.Signature, len(f.Evidence)))
	}
	return out
}

// Two sightings of one failure share a key: what varies between them (a path, a quoted value, a number, an id) is
// taken out, and so is the punctuation that ends it.
func TestNormKeysTwoSightingsAlike(t *testing.T) {
	a := Norm("pgrep -f 'dbtest.py' also matches the shell (pid 4312) in /home/u/x/run.sh at 0x7f3a2c1b.", 80)
	b := Norm("pgrep -f 'bench.sh 12' also matches the shell (pid 77) in ~/y/z.py at 0x11aa22bb", 80)
	if a != b || a != "pgrep -f Q also matches the shell (pid N) in P at H" {
		t.Fatalf("%q != %q", a, b)
	}
	if got := Norm(strings.Repeat("word ", 40), 23); got != "word word word word" {
		t.Fatalf("cut %q", got)
	}
}

// A failure is keyed by where it came from and its words: a guard's refusal by the guard, the harness's refusal,
// an MCP server's error, a Hermes or Codex tool's error, a command's failure only by a mark of the setup failing.
// The owner's no, a call cancelled beside another that failed, and a command whose output shows the work failing
// (a test, a lint) are no setup's failure.
func TestAFailureIsKeyedByItsOriginAndWords(t *testing.T) {
	for _, c := range []struct{ harness, tool, text, want string }{
		{"claude", "Bash", "<tool_use_error>wait-guard: polling loop with sleep and no deadline: it spins forever if the condition never comes",
			"guard:wait-guard:polling loop with sleep and no deadline: it spins forever if the condition"},
		{"claude", "Bash", "<tool_use_error>capacity-guard: held `go vet ./...`: the machine is busy, load 20.7 (limit 20).",
			"guard:capacity-guard:held Q: the machine is busy, load N (limit N)"},
		{"claude", "Write", "Held for the owner's review as rule r40 by the rulebook mod: nothing ran and nothing was written",
			"guard:rulebook:held for the owner's review as rule rN by the rulebook"},
		{"claude", "Bash", "PreToolUse:Bash hook error: broad-scan guard: `rg x` over ~ owner's rule: never scan",
			"guard:broad-scan-guard:Q over P owner's rule: never scan"},
		{"claude", "Bash", "This agent is isolated in the worktree /w/a, but this command names git in a form too complex",
			"harness:claude:this agent is isolated in the worktree P, but this command names git in a form"},
		{"claude", "mcp__harness-web__webfetch", "tool failed: spawn failed for /usr/bin/x: spawn /usr/bin/x ENOENT",
			"mcp:harness-web:tool failed: spawn failed for P: spawn P enoent"},
		{"hermes", "write_file", `{"error": "BLOCKED: write to protected agent-instruction file(s) (AGENTS.md) approval prompt timed out."}`,
			"tool:write-file:blocked: write to protected agent-instruction file(s) (agents.md) approval"},
		{"hermes", "terminal", `{"output": "== x:\nusage: scheduled-repo-guard [-h] --repo REPO", "exit_code": 2, "error": null}`,
			"exec:usage:usage: scheduled-repo-guard [-h] --repo repo"},
		{"codex", "exec", `Script failed Wall time 0.1 seconds Output: Script error: exec_command failed: CreateProcess { message: "Rejected(rm -f)" }`,
			"tool:exec:exec_command failed: createprocess { message: Q }"},
		{"claude", "Bash", "Exit code 1\ntail: option used in invalid context -- 2", "exec:tail:tail: option used in invalid context -- N"},
		{"claude", "Read", "File does not exist. Note: your current working directory is /home/u.", "tool:read:file does not exist. note: your current working directory is P"},
		{"claude", "Bash", "Exit code 1\nFAIL: TestParse (0.01s)\n    parse_test.go:12: got 3, want 4", ""},
		{"claude", "Bash", "<tool_use_error>Cancelled: parallel tool call Bash(go test) errored", ""},
		{"claude", "Bash", "The user doesn't want to take this action right now. STOP what you are doing", ""},
	} {
		k, ok := FailureOf(c.harness, c.tool, c.text)
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %s %q:\n got %q\nwant %q", c.harness, c.tool, c.text, got, c.want)
		}
	}
}

// A failure is a finding when it recurs: three sightings in two sessions in the window. The mistakes log's row
// of a refusal the transcript holds too counts once; a row of its own (a model's refusal) counts. An incident counts
// its repeats, and one closed or resolved counts for nothing.
func TestFailuresAreFoundWhenTheyRecur(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>wait-guard: pgrep -f 'x' also matches the shell running this command"
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s1", "Bash", deny, t0.Add(time.Hour)), toolError("s2", "Bash", deny, t0.Add(2*time.Hour)),
		// one session only: no finding
		toolError("s3", "Bash", "<tool_use_error>wait-guard: an rm whose argument is a glob", t0),
		toolError("s3", "Bash", "<tool_use_error>wait-guard: an rm whose argument is a glob", t0.Add(time.Minute)),
		toolError("s3", "Bash", "<tool_use_error>wait-guard: an rm whose argument is a glob", t0.Add(2*time.Minute)),
		// the work failing, not the setup
		toolError("s4", "Bash", "Exit code 1\nFAIL TestX", t0), toolError("s5", "Bash", "Exit code 1\nFAIL TestX", t0), toolError("s6", "Bash", "Exit code 1\nFAIL TestX", t0),
	)
	// the mistakes log holds the first deny again (the same sighting) and a refusal of the model three times
	held := "capacity-guard: held `make`: the machine is busy, load 30 (limit 20)."
	put(t, st, ev("mistakes", "s1", "deny", store.ActorHarness, "Bash", "Bash: "+deny[len("<tool_use_error>"):], "", t0.Add(2*time.Second)),
		ev("mistakes", "s7", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative cyber content", "", t0),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative cyber content", "", t0.Add(time.Hour)),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative cyber content", "", t0.Add(2*time.Hour)),
		ev("mistakes", "s9", "deny", store.ActorHarness, "Bash", "Bash: "+held, "", t0),
		ev("incidents", "", "incident", store.ActorDetector, "", "httpx.ConnectError: [Errno -3] Temporary failure in name resolution", `{"status":"reported","repeat":12}`, t0),
		ev("incidents", "", "incident", store.ActorDetector, "", "telegram.error.NetworkError: Bad Gateway", `{"status":"resolved","repeat":40}`, t0),
		ev("incidents", "", "incident", store.ActorDetector, "", "openai.APIConnectionError: Connection error.", `{"status":"reported","repeat":2}`, t0),
	)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"failures/v1:guard:wait-guard:pgrep -f Q also matches the shell running this command 3",
		"failures/v1:model:refusal:this request triggered restrictions on violative cyber content 3",
		"failures/v1:incident:httpx.connecterror: [errno -N] temporary failure in name resolution 1",
	}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fs[0].Title != "wait-guard keeps refusing: pgrep -f Q also matches the shell running this command" || fs[0].Family != "failures" {
		t.Fatalf("finding %+v", fs[0])
	}
	if !strings.HasPrefix(fs[0].Evidence[0].Ref, "ev:") || !fs[0].Evidence[0].At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("evidence, newest first: %+v", fs[0].Evidence)
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
}

func ownerTurn(session, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "owner_text", store.ActorOwner, "", text, `{"input_kind":"typed"}`, at)
}

func agentSays(session, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "agent_text", store.ActorAgent, "", text, "", at)
}

// The owner's turns of one kind in two sessions are a finding. A program's text is never his: momentum's own
// message, a wake loop's, a template sent three times, the same text in two sessions within 2 s; and one turn the
// transcript holds twice counts once.
func TestOwnerBurdenIsFoundInHisOwnWords(t *testing.T) {
	st := testStore(t)
	ask := "The build is ready. Should I deploy it now?"
	put(t, st,
		agentSays("a", ask, t0), ownerTurn("a", "why do you ask, you can decide", t0.Add(time.Minute)),
		ownerTurn("a", "why do you ask, you can decide", t0.Add(90*time.Second)), // the same turn recorded twice
		agentSays("a", ask, t0.Add(time.Hour)), ownerTurn("a", "no need to ask me that, do it", t0.Add(time.Hour+time.Minute)),
		agentSays("b", ask, t0), ownerTurn("b", "up to you, why ask", t0.Add(2*time.Minute)),
		// the same handback, typed by programs
		agentSays("c", ask, t0), ownerTurn("c", "[momentum] why stop, you can decide", t0.Add(time.Minute)),
		agentSays("d", ask, t0), ownerTurn("d", "Wake nonce=1 why stop, you can decide", t0.Add(time.Minute)),
		agentSays("e", ask, t0), ownerTurn("e", "nightly: why ask, you decide", t0.Add(time.Minute)),
		agentSays("f", ask, t0), ownerTurn("f", "nightly: why ask, you decide", t0.Add(2*time.Hour)),
		agentSays("g", ask, t0), ownerTurn("g", "nightly: why ask, you decide", t0.Add(3*time.Hour)),
		agentSays("h", ask, t0), ownerTurn("h", "why, you decide this", t0.Add(4*time.Hour)),
		agentSays("i", ask, t0), ownerTurn("i", "why, you decide this", t0.Add(4*time.Hour+time.Second)),
	)
	fs, err := OwnerBurden{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	if got := signatures(fs); !slices.Equal(got, []string{"owner-burden/v1:handback 3"}) {
		t.Fatalf("found %v", got)
	}
}

// Supervision's own record is read for what it gave up on (a finding at once) and the messages that did not land
// or failed again; the sessions' phase events for the waits on the owner past the owner window, but an automated
// run's.
func TestSessionsAreFoundInSupervisionsOwnRecord(t *testing.T) {
	st := testStore(t)
	sup := func(session, kind, meta string, at time.Time) store.Event {
		return ev(store.Supervise, session, kind, store.ActorDetector, "", "x", meta, at)
	}
	wait := func(session, kind string, mins int, auto bool, at time.Time) store.Event {
		return ev(store.Sessions, session, "phase", store.ActorHarness, "", "x",
			fmt.Sprintf(`{"from":"needs_you","to":"working","for_ms":%d,"from_ask_kind":%q,"auto":%v}`, mins*60_000, kind, auto), at)
	}
	put(t, st,
		sup("a", "case", `{"rule":"transient","why":"four tries without a turn that worked: the API stays down"}`, t0),
		sup("a", "outcome", `{"rule":"promise","result":"failed again"}`, t0), sup("b", "outcome", `{"rule":"promise","result":"failed again"}`, t0),
		sup("b", "outcome", `{"rule":"promise","result":"failed again"}`, t0.Add(time.Hour)),
		sup("c", "outcome", `{"rule":"promise","result":"worked"}`, t0), sup("d", "outcome", `{"rule":"promise","result":"worked"}`, t0),
		sup("e", "outcome", `{"rule":"promise","result":"worked"}`, t0),
		wait("a", "permission", 40, false, t0), wait("b", "permission", 20, false, t0), wait("b", "permission", 90, false, t0.Add(time.Hour)),
		wait("c", "question", 40, true, t0), wait("d", "question", 40, true, t0), wait("e", "question", 40, true, t0),
		wait("f", "plan", 5, false, t0), wait("g", "plan", 5, false, t0), wait("h", "plan", 5, false, t0),
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

// The owner nudging stopped sessions that no rule would continue is a finding, keyed by why none would: the replay
// of the rules over the store's history.
func TestTheNudgesNoRuleCoversAreFound(t *testing.T) {
	st := testStore(t)
	for _, s := range []string{"n1", "n2"} {
		at := t0
		for i := 0; i < 2; i++ {
			put(t, st, ownerTurn(s, "build the release", at),
				agentSays(s, "The release is built. Do you want me to publish it now?", at.Add(time.Minute)),
				ownerTurn(s, "continue", at.Add(time.Hour)))
			at = at.Add(3 * time.Hour)
		}
	}
	fs, err := Sessions{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	if got := signatures(fs); !slices.Equal(got, []string{"sessions/v1:nudged:its-last-words-ask-the-owner 4"}) {
		t.Fatalf("found %v", got)
	}
}

// Run files what the detectors find: a case opens for a new signature, a second run finds it again, a dry run files
// nothing, and a detector that fails is named while the others still file theirs.
func TestRunFilesFindingsIntoCases(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	deny := "<tool_use_error>wait-guard: pgrep -f 'x' also matches the shell running this command"
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
	if c.Family != "failures" || c.Evidence != 3 || c.Title != "wait-guard keeps refusing: pgrep -f Q also matches the shell running this command" {
		t.Fatalf("case %+v", c)
	}
}

type failing struct{}

func (failing) Family() string { return "outside" }
func (failing) Detect(context.Context, *store.Store, Window) ([]store.Finding, error) {
	return nil, fmt.Errorf("no source")
}
