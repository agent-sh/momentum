package supervise

import (
	"testing"
	"time"
)

// A Codex turn that an input opened and that Codex closed with no agent message keeps no words of its own: the
// question its stop carries is the turn before's, which the input answered. The replay reads it as a turn with no
// reply, which a rule continues, not as a session that asks its owner.
func TestACodexTurnWithNoReplyIsNotAnAsk(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "codex-noreply").owner(0, "fix the sound output", "").
		say(time.Minute, "I switched the default sink. Can you hear it through the earbuds now?").
		owner(5*time.Minute, "it still fails, remove the sandbox and let me try", "task_complete")
	instruction := b.at
	b.owner(32*time.Second, ".", "task_complete")
	firstNudge := b.at
	b.owner(20*time.Minute, ".", "task_complete")
	secondNudge := b.at
	b.say(5*time.Second, "Removing the sandbox restrictions now.")
	b.codex().put(st)

	asked := replayed(t, st, "codex-noreply", instruction)
	if asked.Decision.Rule == "noreply" || Why(asked) != "its last words ask the owner" {
		t.Fatalf("the stop on the agent's own question: rule %q, %q", asked.Decision.Rule, Why(asked))
	}
	held := replayed(t, st, "codex-noreply", firstNudge)
	if held.Decision.Rule != "noreply" || Why(held) == "its last words ask the owner" {
		t.Fatalf("a turn with no reply 32s before the nudge: rule %q, %q", held.Decision.Rule, Why(held))
	}
	if want := instruction.Add(DefaultParams.OwnerWindow); held.Decision.Verdict != Hold || !held.Decision.At.Equal(want) {
		t.Fatalf("held by the owner window: %s at %v, want hold at %v", held.Decision.Verdict, held.Decision.At, want)
	}
	acted := replayed(t, st, "codex-noreply", secondNudge)
	if acted.Decision.Rule != "noreply" || !acted.Fired || acted.Decision.Text != StalledText {
		t.Fatalf("a turn with no reply 20m before the nudge: rule %q fired %v text %q, %q", acted.Decision.Rule, acted.Fired, acted.Decision.Text, Why(acted))
	}
}

// The rule reads only a Codex stop whose turn began where it ended, outside the guards.
func TestTheNoReplyRuleReadsOnlyAnEmptyCodexTurn(t *testing.T) {
	empty := Stop{Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0, Words: "Should I go on?"}
	if d := Plan(empty, Tries{}, DefaultParams, nil); d.Rule != "noreply" || d.Verdict != Wait || !d.At.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("an empty Codex turn: %+v", d)
	}
	if d := Plan(empty, Tries{"noreply": DefaultParams.StalledTries}, DefaultParams, nil); d.Rule != "noreply" || d.Verdict != Case {
		t.Fatalf("its tries used up: %+v", d)
	}
	for name, s := range map[string]Stop{
		"claude":          {Key: "s", Tool: "claude", At: t0, TurnBegan: t0, IdleAt: t0},
		"a reply came":    {Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0.Add(time.Minute), Words: "Should I go on?"},
		"no turn start":   {Key: "s", Tool: "codex", At: t0, IdleAt: t0, Words: "Should I go on?"},
		"turn open":       {Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0, Working: true},
		"background work": {Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0, Tasks: 1},
		"an error":        {Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0, Error: "stream disconnected"},
		"a past process":  {Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0, Started: t0.Add(time.Hour)},
		"automated":       {Key: "s", Tool: "codex", At: t0, TurnBegan: t0, IdleAt: t0, Auto: true},
	} {
		if d := Plan(s, Tries{}, DefaultParams, nil); d.Rule == "noreply" {
			t.Fatalf("%s: %+v", name, d)
		}
	}
}
