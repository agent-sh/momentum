package supervise

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/source"
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

// answeredThenSilent is a Codex session whose turn ends on a question to its owner, then his instruction opens a
// turn that Codex closes with no agent message. It returns the rollout, the instruction and the empty turn's start
// and end.
func answeredThenSilent(t *testing.T, home string) (r *codexRollout, instruction, began, ended time.Time) {
	r = newCodexRollout(t, home)
	r.line(0, "session_meta", map[string]any{"id": codexTurnID, "timestamp": t0.Format(time.RFC3339Nano), "cwd": "/w/audio",
		"originator": "codex-tui", "cli_version": "0.162.1", "source": "cli", "thread_source": "user", "model_provider": "p"})
	r.started(time.Second)
	r.user(0, "fix the sound output")
	r.agent(time.Minute, codexAsked)
	r.completed(0, codexAsked)
	began = r.started(5 * time.Minute)
	instruction = r.user(0, "it still fails, remove the sandbox and let me try")
	ended = r.completed(2*time.Second, "")
	return r, instruction, began, ended
}

// The live path: the tracker reads the rollout, and supervision reads its session. The turn the instruction opened
// has no words of its own, so its stop is not the question the turn before it asked, and no needs note waits on
// that question; the stop says when the turn began and ended, Codex's task_started and task_complete.
func TestALiveCodexTurnWithNoReplyDoesNotAskTheOwner(t *testing.T) {
	r, _, began, ended := answeredThenSilent(t, t.TempDir())
	now := ended.Add(20 * time.Minute) // the owner window since his instruction has passed
	s := &sessions.Session{Key: codexTurnID, Tool: "codex", Name: "audio", Where: "work:1.1", Pane: "%7", PID: 42, Since: ended,
		Tasks: []sessions.Task{}}
	tr := codex.NewTracker(r.path)
	tr.Facts(s, now)
	s.TurnOpen = tr.Open()
	sessions.Decide(s, now)
	if s.Phase != sessions.Idle {
		t.Fatalf("the turn is over: %s", s.Phase)
	}
	stop := StopOf(s, s.UserAt)
	res := Result{Point: Point{Session: s.Key, Stop: stop}, Decision: Plan(stop, Tries{}, DefaultParams, probeAt(time.Time{}))}
	if stop.Words != "" || Why(res) == asksTheOwner {
		t.Fatalf("the empty turn's stop reads %q: %q", stop.Words, Why(res))
	}
	if !stop.TurnBegan.Equal(began) || !stop.IdleAt.Equal(ended) {
		t.Fatalf("the turn began %v and ended %v, want %v and %v", stop.TurnBegan, stop.IdleAt, began, ended)
	}
	st := testStore(t)
	run(t, supervisor(st, &fakeSender{}, &now), s)
	if n, ok := note(t, st, AskNote+s.Key); ok {
		t.Fatalf("a needs note on the question the instruction answered: %+v", n)
	}
}

// The replay path: the same rollout, read into a store by the transcripts source, then nudged. The stop the nudge
// ends is the turn with no agent message, and it is not keyed as a question to the owner.
func TestTheReplayOfACodexTurnWithNoReplyDoesNotAskTheOwner(t *testing.T) {
	if err := evidence.RedactionReady(); err != nil {
		t.Skip("needs scrump with its text profile:", err)
	}
	home := t.TempDir()
	for k, v := range map[string]string{"HOME": home, "PI_CODING_AGENT_DIR": "", "PI_CODING_AGENT_SESSION_DIR": "",
		"TACIT_HERMES_DB": "off", "XDG_DATA_HOME": "", "GROK_HOME": "", "KIMI_CODE_HOME": ""} {
		t.Setenv(k, v)
	}
	r, instruction, _, _ := answeredThenSilent(t, home)
	r.started(30 * time.Second)
	nudge := r.user(0, ".")
	st := testStore(t)
	if _, err := (source.Transcripts{Homes: []string{home}}).Ingest(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	asked := replayed(t, st, codexTurnID, instruction)
	if asked.Point.Stop.Words == "" || Why(asked) != asksTheOwner {
		t.Fatalf("the stop on the agent's own question reads %q: %q", asked.Point.Stop.Words, Why(asked))
	}
	silent := replayed(t, st, codexTurnID, nudge)
	if s := silent.Point.Stop; s.Words != "" || Why(silent) == asksTheOwner || s.Working || !s.TurnBegan.Equal(instruction) {
		t.Fatalf("the empty turn's stop reads %q (working %v, began %v): %q", s.Words, s.Working, s.TurnBegan, Why(silent))
	}
}
