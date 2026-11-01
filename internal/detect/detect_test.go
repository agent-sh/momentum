package detect

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	for _, pair := range [][2]string{
		{"pgrep -f 'dbtest.py' also matches the shell (pid 4312) in /home/u/x/run.sh at 0x7f3a2c1b.",
			"pgrep -f 'bench.sh 12' also matches the shell (pid 77) in ~/y/z.py at 0x11aa22bb"},
		{"Error: can't open 'config.yaml'", "Error: can't open 'data.json'"},
		{"request req_011CUjQ9xYzAbC failed", "request req_9ZZqT7mmK2pL failed"},
		{"open internal/detect/testdata/a.json: permission denied", "open cmd/x/b.json: permission denied"},
		{"fatal: bad object 3f1c9e2", "fatal: bad object 9ab04de"},
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
// Codex tool's error by the tool; a command's failure only by the clause that holds a mark of the setup failing,
// once the harness's header is off it. The owner's no, a call cancelled beside another that failed, and a command
// whose output shows the work failing (a test, a timeout of tests, a compile error) are no setup's failure.
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
		{"claude", "Bash", "PreToolUse:Bash hook error: build-check: Gate C can't be recorded: run the check first", "guard:build-check:gate c can't be recorded: run the check first"},
		{"claude", "Write", "PreToolUse:Write hook error: build-check: Gate C can't be recorded: run the check first", "guard:build-check:gate c can't be recorded: run the check first"},
		{"claude", "Edit", "PreToolUse:Edit hook error: Blocked by policy", "guard:hook:blocked by policy"},
		{"claude", "Bash", "This agent is isolated in the worktree /w/a, but this command names git in a form too complex",
			"harness:claude:this agent is isolated in the worktree P, but this command names git in a form"},
		{"claude", "mcp__harness-web__webfetch", "tool failed: spawn failed for /usr/bin/x: spawn /usr/bin/x ENOENT",
			"mcp:harness-web:tool failed: spawn failed for P: spawn P enoent"},
		{"hermes", "write_file", `{"error": "BLOCKED: write to protected agent-instruction file(s) (AGENTS.md) approval prompt timed out."}`,
			"tool:write-file:blocked: write to protected agent-instruction file(s) (agents.md) approval"},
		{"hermes", "terminal", `{"output": "== x: usage: fake-repo-check [-h] --repo REPO", "exit_code": 2, "error": null}`,
			"exec:usage:usage: fake-repo-check [-h] --repo repo"},
		{"codex", "exec", `Script failed Wall time 0.1 seconds Output: Script error: exec_command failed: CreateProcess { message: "Rejected(rm -f)" }`,
			"tool:exec:exec_command failed: createprocess { message: Q }"},
		{"codex", "exec_command", "Chunk ID: 7f3a2c Wall time: 0.0521 seconds Process exited with code 127 Original token count: 12 Output: bash: line 1: rg: command not found",
			"exec:rg:rg: command not found"},
		{"codex", "exec_command", "Chunk ID: 9b1e44 Wall time: 1.2034 seconds Process exited with code 1 Original token count: 30 Output: fatal: unable to access 'https://x/y.git/': Could not resolve host: x",
			"exec:fatal:fatal: unable to access Q: could not resolve host: x"},
		{"claude", "Bash", "Exit code 1 tail: option used in invalid context -- 2", "exec:tail:tail: option used in invalid context -- N"},
		{"claude", "Bash", "Exit code 1 === RUN TestX --- PASS (0.00s) dial tcp: connection refused", "exec:tcp:tcp: connection refused"},
		{"claude", "Bash", "Exit code 1 cat: /x/y.txt: No such file or directory", "exec:cat:cat: P: no such file or directory"},
		{"claude", "Bash", "Exit code 128 fatal: Unable to create '/r/.git/index.lock': File exists.", "exec:fatal:fatal: unable to create Q: file exists"},
		{"claude", "Read", "File does not exist. Note: your current working directory is /home/u.", "tool:read:file does not exist. note: your current working directory is P"},
		{"claude", "Bash", "Exit code 1 --- FAIL: TestTokenExpired (0.00s) tok_test.go:12: got true, want false", ""},
		{"claude", "Bash", "Exit code 1 panic: test timed out after 10m0s running tests: TestBig (10m0s)", ""},
		{"claude", "Bash", "Exit code 1 ./x.go:12:3: undefined: thing no such file or directory", ""},
		{"claude", "Bash", "Exit code 1 FAIL: TestParse (0.01s) parse_test.go:12: got 3, want 4", ""},
		{"claude", "Bash", "<tool_use_error>Cancelled: parallel tool call Bash(go test) errored", ""},
		{"claude", "Edit", "<tool_use_error>Cancelled: parallel tool call Bash(go test) errored", ""},
		{"claude", "Bash", "The user doesn't want to take this action right now. STOP what you are doing", ""},
		{"claude", "Edit", "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file).", ""},
		{"claude", "mcp__x__y", "The user doesn't want to proceed with this tool use. The tool use was rejected.", ""},
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

// A failure is a finding when it recurs: three sightings in two sessions in the window. The mistakes log's row of a
// failure the transcript holds in the same session, of the same tool, within 5 seconds, counts once whatever text
// each kept; a row of its own (a model's refusal) counts. An incident counts its sightings' repeats, and one closed
// or resolved counts for nothing.
func TestFailuresAreFoundWhenTheyRecur(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>wait-guard: pgrep -f 'x' also matches the shell running this command"
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s1", "Bash", deny, t0.Add(time.Hour)), toolError("s2", "Bash", deny, t0.Add(2*time.Hour)),
		// one session only: no finding
		toolError("s3", "Bash", "<tool_use_error>wait-guard: an rm whose argument is a glob", t0),
		toolError("s3", "Bash", "<tool_use_error>wait-guard: an rm whose argument is a glob", t0.Add(time.Minute)),
		toolError("s3", "Bash", "<tool_use_error>wait-guard: an rm whose argument is a glob", t0.Add(2*time.Minute)),
		// two sightings in two sessions: no finding
		toolError("s4", "Bash", "<tool_use_error>wait-guard: a session link would go into a commit", t0),
		toolError("s5", "Bash", "<tool_use_error>wait-guard: a session link would go into a commit", t0),
		// the work failing, not the setup
		toolError("s4", "Bash", "Exit code 1 --- FAIL: TestX", t0), toolError("s5", "Bash", "Exit code 1 --- FAIL: TestX", t0), toolError("s6", "Bash", "Exit code 1 --- FAIL: TestX", t0),
		// a result that is no error
		ev(store.Transcripts, "s6", "tool_result", store.ActorTool, "Bash", deny, `{"tool_id":"y"}`, t0.Add(3*time.Hour)),
	)
	held := "capacity-guard: held `make`: the machine is busy"
	put(t, st,
		// the transcript's first deny again, its text cut otherwise: the same sighting
		ev("mistakes", "s1", "deny", store.ActorHarness, "Bash", "Bash: capacity-guard: held `x`", "", t0.Add(2*time.Second)),
		// a refusal of the model, three times in two sessions
		ev("mistakes", "s7", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative cyber content", "", t0),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative cyber content", "", t0.Add(time.Hour)),
		ev("mistakes", "s8", "refusal", store.ActorAgent, "", "This request triggered restrictions on violative cyber content", "", t0.Add(2*time.Hour)),
		// a hold of the mistakes log's own: 6 s after a failure of the transcript's, of another tool beside one, and alone;
		// and one a second after the transcript's failure of its session and tool, which is that one
		ev("mistakes", "s1", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0.Add(6*time.Second)),
		ev("mistakes", "s2", "held", store.ActorHarness, "Write", "Write: "+held, "", t0.Add(2*time.Hour+time.Second)),
		ev("mistakes", "s9", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0),
		ev("mistakes", "s2", "held", store.ActorHarness, "Bash", "Bash: "+held, "", t0.Add(2*time.Hour+time.Second)),
	)
	sight := func(key, text, status string, fresh int, at time.Time) store.Event {
		e := ev("incidents", "", "incident-seen", store.ActorDetector, "", text, fmt.Sprintf(`{"status":%q,"new":%d}`, status, fresh), at)
		e.SourceKey = key
		return e
	}
	put(t, st, sight("i1@1", "httpx.ConnectError: [Errno -3] Temporary failure in name resolution", "reported", 6, t0),
		sight("i1@2", "httpx.ConnectError: [Errno -3] Temporary failure in name resolution", "reported", 6, t0.Add(time.Hour)),
		sight("i2@1", "telegram.error.NetworkError: Bad Gateway", "resolved", 40, t0),
		sight("i3@1", "openai.APIConnectionError: Connection error.", "reported", 9, t0),
		// an incident's state, which the detector does not read: its sightings say what it did
		ev("incidents", "", "incident", store.ActorDetector, "", "openai.APIConnectionError: Connection error.", `{"status":"reported","repeat":400}`, t0))
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"failures/v1:guard:capacity-guard:held Q: the machine is busy 3",
		"failures/v1:guard:wait-guard:pgrep -f Q also matches the shell running this command 3",
		"failures/v1:model:refusal:this request triggered restrictions on violative cyber content 3",
		"failures/v1:incident:httpx.connecterror: [errno -N] temporary failure in name resolution 2",
	}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fs[1].Title != "wait-guard keeps refusing calls: pgrep -f Q also matches the shell running this command" || fs[1].Family != "failures" {
		t.Fatalf("finding %+v", fs[1])
	}
	if !strings.HasPrefix(fs[1].Evidence[0].Ref, "ev:") || !fs[1].Evidence[0].At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("evidence, newest first: %+v", fs[1].Evidence)
	}
}

// A finding files at most MaxEvidence of its newest sightings on a run.
func TestAFindingFilesItsNewestEvidence(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>wait-guard: pgrep -f 'x' also matches the shell running this command"
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
}

func ownerTurn(session, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "owner_text", store.ActorOwner, "", text, `{"input_kind":"typed"}`, at)
}

func agentSays(session, text string, at time.Time) store.Event {
	return ev(store.Transcripts, session, "agent_text", store.ActorAgent, "", text, "", at)
}

// The owner's turns of one kind in two sessions are a finding. A program's text is never his: momentum's own
// message, a configured wake loop's, a long template sent in three sessions, the same text in two sessions within
// 2 s; one turn the transcript holds twice counts once; a subagent's thread and another actor's text are not his
// turns. A short nudge he types in three sessions is his.
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
		// the same handback, typed by programs
		agentSays("c", ask, t0), ownerTurn("c", "[momentum] why stop, you can decide", t0.Add(time.Minute)),
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
	if got := signatures(fs); !slices.Equal(got, []string{"owner-burden/v1:handback 3"}) {
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
		agentSays("a", ask, t0), ownerTurn("a", "why ask me", t0.Add(time.Minute)), ownerTurn("a", "just do it, you decide", t0.Add(5*time.Minute)),
		ownerTurn("a", "why do you still wait", t0.Add(20*time.Minute)),
		agentSays("b", ask, t0), ownerTurn("b", "up to you, why ask", t0.Add(7*time.Hour)), // past the look back
	)
	if fs, err := (OwnerBurden{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("one step-in counted per turn after it: %v %v", signatures(fs), err)
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
	for si, s := range []string{"n1", "n2"} {
		at := t0.Add(time.Duration(si) * 10 * time.Minute)
		for i := 0; i < 2; i++ {
			put(t, st, ownerTurn(s, "build the release", at),
				agentSays(s, "The release is built. Do you want me to publish it now?", at.Add(time.Minute)),
				ownerTurn(s, "continue", at.Add(time.Hour+time.Duration(i)*time.Minute)))
			at = at.Add(3 * time.Hour)
		}
	}
	// a nudge a program typed into two sessions at once; an answer that is no nudge; a stop the transient rule
	// would have resumed before his nudge
	for _, s := range []string{"p1", "p2"} {
		put(t, st, ownerTurn(s, "build the release", t0), agentSays(s, "The release is built. Do you want me to publish it now?", t0.Add(time.Minute)),
			ownerTurn(s, "continue", t0.Add(2*time.Hour)))
	}
	put(t, st, ownerTurn("q", "build the release", t0), agentSays("q", "The release is built. Do you want me to publish it now?", t0.Add(time.Minute)),
		ownerTurn("q", "yes, publish it to the beta channel", t0.Add(time.Hour)))
	put(t, st, ownerTurn("r", "deploy it", t0), agentSays("r", "Deploying.", t0.Add(10*time.Second)),
		ev(store.Transcripts, "r", "harness_text", store.ActorHarness, "", "API Error: 503 the provider is unable to process your request, a server-side issue",
			`{"is_error":true,"why":"api error"}`, t0.Add(time.Minute)),
		ownerTurn("r", "continue", t0.Add(40*time.Minute)))
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
	if c.Family != "failures" || c.Evidence != 3 || c.Title != "wait-guard keeps refusing calls: pgrep -f Q also matches the shell running this command" {
		t.Fatalf("case %+v", c)
	}
}

type failing struct{}

func (failing) Family() string { return "outside" }
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
// from being filed: each is named.
func TestRunFilesWhatItCanAndNamesTheRest(t *testing.T) {
	st := testStore(t)
	out, _, err := Run(context.Background(), st, []Detector{partial{}}, week(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	o := out[0]
	if len(o.Filed) != 2 || o.Filed[0].Err == nil || o.Filed[1].Observed != store.Opened || o.Err == nil ||
		!strings.Contains(o.Err.Error(), "the replay failed") || !strings.Contains(o.Err.Error(), "bad case") {
		t.Fatalf("run %+v", o)
	}
}
