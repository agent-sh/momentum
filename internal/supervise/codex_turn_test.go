package supervise

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/codex"
)

const (
	codexTurnID  = "01a1c0de-0000-7000-8000-0000000000ce"
	codexAsked   = "I switched the default sink. Can you hear it through the earbuds now?"
	asksTheOwner = "its last words ask the owner"
)

// codexRollout writes a synthetic Codex rollout, line by line, as Codex writes it.
type codexRollout struct {
	t    *testing.T
	path string
	at   time.Time
}

func newCodexRollout(t *testing.T, home string) *codexRollout {
	t.Helper()
	path := filepath.Join(home, ".codex", "sessions", "2026", "03", "04", "rollout-2026-03-04T10-00-00-"+codexTurnID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return &codexRollout{t: t, path: path, at: t0}
}

func (r *codexRollout) line(after time.Duration, typ string, payload map[string]any) time.Time {
	r.t.Helper()
	r.at = r.at.Add(after)
	b, err := json.Marshal(map[string]any{"timestamp": r.at.Format(time.RFC3339Nano), "type": typ, "payload": payload})
	if err != nil {
		r.t.Fatal(err)
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		r.t.Fatal(err)
	}
	return r.at
}

func (r *codexRollout) started(after time.Duration) time.Time {
	return r.line(after, "event_msg", map[string]any{"type": "task_started"})
}

func (r *codexRollout) user(after time.Duration, text string) time.Time {
	return r.line(after, "response_item", map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}}})
}

func (r *codexRollout) agent(after time.Duration, text string) time.Time {
	return r.line(after, "response_item", map[string]any{"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text}}})
}

// completed is Codex's end of a turn; an empty last is a turn with no agent message.
func (r *codexRollout) completed(after time.Duration, last string) time.Time {
	var msg any
	if last != "" {
		msg = last
	}
	return r.line(after, "event_msg", map[string]any{"type": "task_complete", "last_agent_message": msg})
}

// meta is the rollout's first line: an interactive session its owner types into.
func (r *codexRollout) meta() {
	r.line(0, "session_meta", map[string]any{"id": codexTurnID, "timestamp": t0.Format(time.RFC3339Nano), "cwd": "/w/audio",
		"originator": "codex-tui", "cli_version": "0.162.1", "source": "cli", "thread_source": "user", "model_provider": "p"})
}

// call is a tool call and its output, a second apart.
func (r *codexRollout) call(after time.Duration, id string) time.Time {
	r.line(after, "response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": id, "arguments": `{"cmd":"ls"}`})
	return r.line(time.Second, "response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": "ok"})
}

// fromSubagent is a turn a subagent's message opens by itself, with no input of the owner's.
func (r *codexRollout) fromSubagent(after time.Duration, text string) time.Time {
	r.started(after)
	r.line(0, "inter_agent_communication_metadata", map[string]any{"trigger_turn": true})
	return r.line(0, "response_item", map[string]any{"type": "agent_message", "id": "m1", "author": "worker", "recipient": "main",
		"content": []any{map[string]any{"type": "input_text", "text": text}}})
}

// asked is the turn the owner opens, which ends on the agent's question to him.
func (r *codexRollout) asked() {
	r.meta()
	r.started(time.Second)
	r.user(0, "fix the sound output")
	r.agent(time.Minute, codexAsked)
	r.completed(0, codexAsked)
}

// answeredThenSilent is a Codex session whose turn ends on a question to its owner, then his instruction opens a
// turn that Codex closes with no agent message. It returns the rollout, the instruction and the empty turn's start
// and end.
func answeredThenSilent(t *testing.T, home string) (r *codexRollout, instruction, began, ended time.Time) {
	r = newCodexRollout(t, home)
	r.asked()
	began = r.started(5 * time.Minute)
	instruction = r.user(0, "it still fails, remove the sandbox and let me try")
	ended = r.completed(2*time.Second, "")
	return r, instruction, began, ended
}

// live is the session the live tracker reads from the rollout as it stands, 20 minutes after its turn ended (the
// owner window since his last input has passed), its stop and why no rule acts on it.
func live(t *testing.T, path string, ended time.Time) (*sessions.Session, Stop, Result) {
	t.Helper()
	now := ended.Add(20 * time.Minute)
	s := &sessions.Session{Key: codexTurnID, Tool: "codex", Name: "audio", Where: "work:1.1", Pane: "%7", PID: 42, Since: ended,
		Tasks: []sessions.Task{}}
	tr := codex.NewTracker(path)
	tr.Facts(s, now)
	s.TurnOpen = tr.Open()
	sessions.Decide(s, now)
	if s.Phase != sessions.Idle {
		t.Fatalf("the turn is over: %s", s.Phase)
	}
	stop := StopOf(s, s.UserAt)
	return s, stop, Result{Point: Point{Session: s.Key, Stop: stop}, Decision: Plan(stop, Tries{}, DefaultParams, probeAt(time.Time{}))}
}

// needsNote is the needs note a supervision run 20 minutes after the turn ended writes for the session, if any.
func needsNote(t *testing.T, s *sessions.Session, ended time.Time) (store.Note, bool) {
	t.Helper()
	now := ended.Add(20 * time.Minute)
	st := testStore(t)
	run(t, supervisor(st, &fakeSender{}, &now), s)
	return note(t, st, AskNote+s.Key)
}

// codexHome is a synthetic home the transcripts source reads, and nothing of the machine's.
func codexHome(t *testing.T) string {
	t.Helper()
	if err := evidence.RedactionReady(); err != nil {
		t.Skip("needs scrump with its text profile:", err)
	}
	home := t.TempDir()
	for k, v := range map[string]string{"HOME": home, "PI_CODING_AGENT_DIR": "", "PI_CODING_AGENT_SESSION_DIR": "",
		"TACIT_HERMES_DB": "off", "XDG_DATA_HOME": "", "GROK_HOME": "", "KIMI_CODE_HOME": ""} {
		t.Setenv(k, v)
	}
	return home
}

// ingested is a store the transcripts source read the home into.
func ingested(t *testing.T, home string) *store.Store {
	t.Helper()
	st := testStore(t)
	if _, err := (source.Transcripts{Homes: []string{home}}).Ingest(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	return st
}

// The live path: the tracker reads the rollout, and supervision reads its session. The turn the instruction opened
// has no words of its own, so its stop is not the question the turn before it asked, and no needs note waits on
// that question; the stop says when the turn began and ended, Codex's task_started and task_complete.
func TestALiveCodexTurnWithNoReplyDoesNotAskTheOwner(t *testing.T) {
	r, _, began, ended := answeredThenSilent(t, t.TempDir())
	s, stop, res := live(t, r.path, ended)
	if stop.Words != "" || Why(res) == asksTheOwner {
		t.Fatalf("the empty turn's stop reads %q: %q", stop.Words, Why(res))
	}
	if !stop.TurnBegan.Equal(began) || !stop.IdleAt.Equal(ended) {
		t.Fatalf("the turn began %v and ended %v, want %v and %v", stop.TurnBegan, stop.IdleAt, began, ended)
	}
	if n, ok := needsNote(t, s, ended); ok {
		t.Fatalf("a needs note on the question the instruction answered: %+v", n)
	}
}

// The replay path: the same rollout, read into a store by the transcripts source, then nudged. The stop the nudge
// ends is the turn with no agent message, and it is not keyed as a question to the owner.
func TestTheReplayOfACodexTurnWithNoReplyDoesNotAskTheOwner(t *testing.T) {
	home := codexHome(t)
	r, instruction, _, _ := answeredThenSilent(t, home)
	r.started(30 * time.Second)
	nudge := r.user(0, ".")
	st := ingested(t, home)
	asked := replayed(t, st, codexTurnID, instruction)
	if asked.Point.Stop.Words == "" || Why(asked) != asksTheOwner {
		t.Fatalf("the stop on the agent's own question reads %q: %q", asked.Point.Stop.Words, Why(asked))
	}
	silent := replayed(t, st, codexTurnID, nudge)
	if s := silent.Point.Stop; s.Words != "" || Why(silent) == asksTheOwner || s.Working || !s.TurnBegan.Equal(instruction) {
		t.Fatalf("the empty turn's stop reads %q (working %v, began %v): %q", s.Words, s.Working, s.TurnBegan, Why(silent))
	}
}

// A subagent's message opens a turn by itself and Codex closes it with no agent message: no input of the owner's
// answered his question, so the session still asks it, and its needs note stands.
func TestALiveCodexTurnASubagentOpenedKeepsTheOwnersQuestion(t *testing.T) {
	r := newCodexRollout(t, t.TempDir())
	r.asked()
	r.fromSubagent(5*time.Minute, "The worker finished the build.")
	r.call(time.Second, "c1")
	ended := r.completed(time.Second, "")
	s, stop, res := live(t, r.path, ended)
	if stop.Words != Words(codexAsked) || Why(res) != asksTheOwner {
		t.Fatalf("the stop reads %q: %q", stop.Words, Why(res))
	}
	if n, ok := needsNote(t, s, ended); !ok || n.Title != Question(codexAsked) {
		t.Fatalf("the needs note on his unanswered question: %+v %v", n, ok)
	}
}

// A subagent's message is not the session's own words, live or in the replay: a turn it opened that Codex closed
// with no agent message reads the same both ways, the question the session asked its owner before it.
func TestACodexTurnASubagentOpenedReadsTheSameLiveAndInTheReplay(t *testing.T) {
	home := codexHome(t)
	r := newCodexRollout(t, home)
	r.asked()
	r.fromSubagent(5*time.Minute, "The worker finished. Should the parent rerun the soak now?")
	r.call(time.Second, "c1")
	ended := r.completed(time.Second, "")
	_, stop, _ := live(t, r.path, ended)
	r.started(30 * time.Minute)
	nudge := r.user(0, ".")
	got := replayed(t, ingested(t, home), codexTurnID, nudge)
	if stop.Words != Words(codexAsked) || got.Point.Stop.Words != stop.Words {
		t.Fatalf("live %q, replay %q (%s)", stop.Words, got.Point.Stop.Words, Why(got))
	}
}

// The owner steers a message into a running turn the agent already spoke in, and Codex closes it with no agent
// message after it: no turn opened, so the turn keeps its own words, live and in the replay.
func TestASteeredCodexInputKeepsTheTurnsOwnWords(t *testing.T) {
	home := codexHome(t)
	r := newCodexRollout(t, home)
	r.meta()
	r.started(time.Second)
	r.user(0, "fix the sound output")
	r.agent(time.Minute, codexAsked)
	r.call(time.Second, "c1")
	r.user(10*time.Second, "also check the bluetooth profile")
	ended := r.completed(5*time.Second, "")
	_, stop, _ := live(t, r.path, ended)
	r.started(30 * time.Minute)
	nudge := r.user(0, ".")
	got := replayed(t, ingested(t, home), codexTurnID, nudge)
	if stop.Words != Words(codexAsked) || got.Point.Stop.Words != stop.Words {
		t.Fatalf("live %q, replay %q (%s)", stop.Words, got.Point.Stop.Words, Why(got))
	}
}

// The store marks the owner's answer to the agent's question as an answer, also one steered into the running turn,
// where it opens no turn: the turn keeps its own words. An answer the store says came after the turn was over opens
// one. (Codex writes an answer in its own reply markup, so this fixture is the store's events.)
func TestASteeredCodexAnswerKeepsTheTurnsOwnWords(t *testing.T) {
	st := testStore(t)
	steered := newTimeline(t, "codex-steered-answer").owner(0, "fix the sound output", "").say(time.Minute, codexAsked).
		add(10*time.Second, "owner_text", store.ActorOwner, "", "the earbuds", `{"input_kind":"answer"}`)
	steered.owner(5*time.Minute, ".", "task_complete")
	nudge := steered.at
	steered.codex().put(st)
	over := newTimeline(t, "codex-answer-after").owner(0, "fix the sound output", "").say(time.Minute, codexAsked).
		add(5*time.Minute, "owner_text", store.ActorOwner, "", "the earbuds", `{"input_kind":"answer","prior":"task_complete"}`)
	over.owner(5*time.Minute, ".", "task_complete")
	overNudge := over.at
	over.codex().put(st)
	if got := replayed(t, st, "codex-steered-answer", nudge); got.Point.Stop.Words != codexAsked {
		t.Fatalf("a steered answer cleared the turn's words: %q (%s)", got.Point.Stop.Words, Why(got))
	}
	if got := replayed(t, st, "codex-answer-after", overNudge); got.Point.Stop.Words != "" {
		t.Fatalf("an answer that opened a turn kept the turn before's words: %q (%s)", got.Point.Stop.Words, Why(got))
	}
}

// scanned is the live session the tracker reads from the rollout at now, as a scan hands it to supervision.
func scanned(path string, since, now time.Time) *sessions.Session {
	s := &sessions.Session{Key: codexTurnID, Tool: "codex", Name: "audio", Where: "work:1.1", Pane: "%7", PID: 42, Since: since,
		Tasks: []sessions.Task{}}
	tr := codex.NewTracker(path)
	tr.Facts(s, now)
	s.TurnOpen = tr.Open()
	sessions.Decide(s, now)
	if s.Log != nil {
		s.Seq = s.Log.Seq()
	}
	return s
}

const codexPromise = "The soak is running. I'll report when it finishes."

// momentum's own resync opens no turn of its user's: a turn Codex closes after it with no agent message keeps the
// words the resync quoted, so the promise rule tries again, and after its second try a case opens.
func TestALiveCodexResyncThatGetsNoReplyIsTriedAgainThenACase(t *testing.T) {
	if codex.Marker != Marker {
		t.Fatalf("the tracker knows momentum's messages by %q, momentum types %q", codex.Marker, Marker)
	}
	for _, withCall := range []bool{false, true} {
		r := newCodexRollout(t, t.TempDir())
		r.meta()
		r.started(time.Second)
		r.user(0, "run the soak")
		r.agent(time.Minute, codexPromise)
		ended := r.completed(0, codexPromise)
		st := testStore(t)
		f := &fakeSender{}
		now := ended.Add(20 * time.Minute)
		sv := supervisor(st, f, &now)
		sv.Live = map[string]bool{RulePromise: true}
		run(t, sv, scanned(r.path, ended, now))
		for try := 1; try <= 2; try++ {
			if len(f.sent) != try {
				t.Fatalf("with a call %v, try %d: sent %q", withCall, try, f.sent)
			}
			r.at = now.Add(time.Second)
			r.started(0)
			r.user(0, strings.TrimPrefix(f.sent[try-1], "%7 ")) // Codex records the resync as its user's message
			if withCall {
				r.call(5*time.Second, "c1")
			}
			ended = r.completed(2*time.Second, "")
			now = ended.Add(20 * time.Minute)
			run(t, sv, scanned(r.path, ended, now))
		}
		cases := events(t, st, KindCase)
		if len(f.sent) != 2 || len(cases) != 1 || metaMap(cases[0])["rule"] != RulePromise {
			t.Fatalf("with a call %v: sent %d, cases %d", withCall, len(f.sent), len(cases))
		}
	}
}

// The replay reads momentum's message as live: it opens no turn of the owner's, so the silent turn after it keeps
// the promise its resync quoted, and so does a turn of his that opens with the marker (a paste of it that stayed).
func TestTheReplayOfAResyncThatGetsNoReplyKeepsItsWords(t *testing.T) {
	st := testStore(t)
	resync := newTimeline(t, "codex-resync").owner(0, "run the soak", "").say(time.Minute, codexPromise).
		add(20*time.Minute, "harness_text", store.ActorHarness, "", Marker+" Your last turn ended with a promise.",
			`{"input_kind":"typed","prior":"task_complete","why":"momentum supervise"}`)
	resync.owner(40*time.Minute, "status?", "task_complete")
	next := resync.at
	resync.codex().put(st)
	pasted := newTimeline(t, "codex-pasted-marker").owner(0, "run the soak", "").say(time.Minute, codexPromise).
		owner(20*time.Minute, Marker+" Your last turn ended with a promise. also run the linter", "task_complete")
	pasted.owner(40*time.Minute, "status?", "task_complete")
	pastedNext := pasted.at
	pasted.codex().put(st)
	for session, at := range map[string]time.Time{"codex-resync": next, "codex-pasted-marker": pastedNext} {
		if got := replayed(t, st, session, at); got.Point.Stop.Words != codexPromise || got.Decision.Rule != RulePromise {
			t.Fatalf("%s: the stop after the silent turn reads %q, rule %q", session, got.Point.Stop.Words, got.Decision.Rule)
		}
	}
}

// Only the owner's input opens a Codex turn in the replay, and only one the live tracker reads as typed: not the
// session's own input (a notification), and not a message that opens with "<", which the live tracker reads as
// Codex's own markup.
func TestACodexInputTheLiveTrackerDoesNotReadAsTypedOpensNoTurn(t *testing.T) {
	st := testStore(t)
	const asked = "I switched the sink. Can you hear it now?"
	self := newTimeline(t, "codex-self").owner(0, "fix the sound output", "").say(time.Minute, asked).
		add(20*time.Minute, "harness_text", store.ActorHarness, "", "scheduled check", `{"input_kind":"typed","prior":"scheduled_task_fire"}`)
	self.owner(40*time.Minute, "status?", "task_complete")
	selfNext := self.at
	self.codex().put(st)
	angle := newTimeline(t, "codex-angle").owner(0, "fix the sound output", "").say(time.Minute, asked).
		owner(20*time.Minute, "<= 5 retries is enough, keep going", "task_complete")
	angle.owner(40*time.Minute, "status?", "task_complete")
	angleNext := angle.at
	angle.codex().put(st)
	for session, at := range map[string]time.Time{"codex-self": selfNext, "codex-angle": angleNext} {
		if got := replayed(t, st, session, at); got.Point.Stop.Words != asked {
			t.Fatalf("%s: the stop after the silent turn reads %q (%s)", session, got.Point.Stop.Words, Why(got))
		}
	}
}

// A message Codex wrote before the task_started of the turn it opens opens that turn, live and in the replay.
func TestACodexMessageTypedBeforeItsTurnStartsOpensIt(t *testing.T) {
	home := codexHome(t)
	r := newCodexRollout(t, home)
	r.asked()
	r.user(5*time.Minute, "it still fails, remove the sandbox and let me try")
	r.started(0)
	ended := r.completed(2*time.Second, "")
	_, stop, _ := live(t, r.path, ended)
	r.started(30 * time.Minute)
	nudge := r.user(0, ".")
	got := replayed(t, ingested(t, home), codexTurnID, nudge)
	if stop.Words != "" || got.Point.Stop.Words != "" {
		t.Fatalf("live %q, replay %q (%s)", stop.Words, got.Point.Stop.Words, Why(got))
	}
}

// In a turn a subagent's message opened, the agent's calls make a message the owner types after them a steer: the
// turn keeps its words, live and in the replay.
func TestASteerAfterCallsInATurnASubagentOpenedKeepsTheWords(t *testing.T) {
	home := codexHome(t)
	r := newCodexRollout(t, home)
	r.asked()
	r.fromSubagent(5*time.Minute, "The worker finished the build.")
	r.call(time.Second, "c1")
	r.user(10*time.Second, "also check the bluetooth profile")
	ended := r.completed(5*time.Second, "")
	_, stop, _ := live(t, r.path, ended)
	r.started(30 * time.Minute)
	nudge := r.user(0, ".")
	got := replayed(t, ingested(t, home), codexTurnID, nudge)
	if stop.Words != Words(codexAsked) || got.Point.Stop.Words != stop.Words {
		t.Fatalf("live %q, replay %q (%s)", stop.Words, got.Point.Stop.Words, Why(got))
	}
}

// A subagent's message that comes while a call runs is passed over in the replay, as the live tracker passes over
// it: the stop is the call still running, live and replayed.
func TestASubagentsMessageDuringACallIsPassedOver(t *testing.T) {
	home := codexHome(t)
	r := newCodexRollout(t, home)
	r.meta()
	r.started(time.Second)
	r.user(0, "wait for the worker")
	called := r.line(5*time.Second, "response_item", map[string]any{"type": "function_call", "name": "wait_agent", "call_id": "w1", "arguments": `{"agent":"worker"}`})
	r.line(time.Minute, "response_item", map[string]any{"type": "agent_message", "id": "m1", "author": "worker", "recipient": "main",
		"content": []any{map[string]any{"type": "input_text", "text": "The worker finished the build."}}})
	now := r.at.Add(time.Minute)
	s := scanned(r.path, called, now)
	liveRes := Result{Point: Point{Stop: StopOf(s, s.UserAt)}}
	liveRes.Decision = Plan(liveRes.Point.Stop, Tries{}, DefaultParams, probeAt(time.Time{}))
	nudge := r.user(time.Minute, ".")
	got := replayed(t, ingested(t, home), codexTurnID, nudge)
	if Why(liveRes) != "working: a tool call was running" || Why(got) != Why(liveRes) || !got.Point.Stop.At.Equal(called) {
		t.Fatalf("live %q, replay %q at %v (the call at %v)", Why(liveRes), Why(got), got.Point.Stop.At, called)
	}
}
