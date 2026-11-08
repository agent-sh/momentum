package supervise

import (
	"testing"
	"time"
)

// The rule and its wait as the tests name them, so the tests build without the rule and fail on what it does.
const (
	ruleNoReply = "noreply"
	noReplyWait = 2 * time.Minute
)

// The live path: the owner answers the agent's question with an instruction, and Codex closes the turn it opened
// with no agent message. The session asks nothing and got no reply: with the rules momentum ships, supervision
// sends the no-reply message once the owner window since his instruction has passed, and not before.
func TestALiveCodexTurnWithNoReplyIsContinued(t *testing.T) {
	r, instruction, began, ended := answeredThenSilent(t, t.TempDir())
	_, stop, res := live(t, r.path, ended)
	if d := res.Decision; d.Rule != ruleNoReply || d.Verdict != Hold || !d.At.Equal(instruction.Add(DefaultParams.OwnerWindow)) {
		t.Fatalf("the empty turn that began at %v: %+v (stop %+v)", began, d, stop)
	}
	st := testStore(t)
	f := &fakeSender{}
	now := instruction.Add(DefaultParams.OwnerWindow - time.Minute)
	sv := supervisor(st, f, &now)
	sv.Live = Live
	run(t, sv, scanned(r.path, ended, now))
	if len(f.sent) != 0 {
		t.Fatalf("sent while the owner window holds: %q", f.sent)
	}
	now = instruction.Add(DefaultParams.OwnerWindow + time.Minute)
	run(t, sv, scanned(r.path, ended, now))
	if len(f.sent) != 1 || f.sent[0] != "%7 "+StalledText {
		t.Fatalf("after the owner window: sent %q", f.sent)
	}
}

// The replay path: an agent question, the owner's instruction that Codex closes with no agent message, then his
// "." nudges. The stop on the question is still his to answer; the stops after the instruction are the noreply
// rule's, held by the owner window, then fired before the nudge 20 minutes on.
func TestTheReplayOfACodexTurnWithNoReplyIsTheNoReplyRules(t *testing.T) {
	st := testStore(t)
	b := newTimeline(t, "codex-noreply").owner(0, "fix the sound output", "").
		say(time.Minute, codexAsked).
		owner(5*time.Minute, "it still fails, remove the sandbox and let me try", "task_complete")
	instruction := b.at
	b.owner(32*time.Second, ".", "task_complete")
	firstNudge := b.at
	b.owner(20*time.Minute, ".", "task_complete")
	secondNudge := b.at
	b.say(5*time.Second, "Removing the sandbox restrictions now.")
	b.codex().put(st)

	asked := replayed(t, st, "codex-noreply", instruction)
	if asked.Decision.Rule == ruleNoReply || Why(asked) != asksTheOwner {
		t.Fatalf("the stop on the agent's own question: rule %q, %q", asked.Decision.Rule, Why(asked))
	}
	held := replayed(t, st, "codex-noreply", firstNudge)
	if held.Decision.Rule != ruleNoReply || held.Decision.Verdict != Hold || !held.Decision.At.Equal(instruction.Add(DefaultParams.OwnerWindow)) {
		t.Fatalf("a turn with no reply 32s before the nudge: %+v, %q", held.Decision, Why(held))
	}
	acted := replayed(t, st, "codex-noreply", secondNudge)
	if acted.Decision.Rule != ruleNoReply || !acted.Fired || acted.Decision.Text != StalledText {
		t.Fatalf("a turn with no reply 20m before the nudge: %+v fired %v, %q", acted.Decision, acted.Fired, Why(acted))
	}
}

// The rule reads only an idle Codex stop with no words of its own and nothing in flight, and its tries are the
// stalled rule's.
func TestTheNoReplyRuleReadsOnlyAnIdleCodexTurnWithoutWords(t *testing.T) {
	end := t0.Add(time.Minute)
	empty := Stop{Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end}
	if d := Plan(empty, Tries{}, DefaultParams, nil); d.Rule != ruleNoReply || d.Verdict != Wait || !d.At.Equal(end.Add(noReplyWait)) || d.Text != StalledText {
		t.Fatalf("an empty Codex turn: %+v", d)
	}
	if d := Plan(empty, Tries{ruleNoReply: DefaultParams.StalledTries}, DefaultParams, nil); d.Rule != ruleNoReply || d.Verdict != Case {
		t.Fatalf("its tries used up: %+v", d)
	}
	if !Live[ruleNoReply] {
		t.Fatal("the noreply rule does not ship live")
	}
	for name, s := range map[string]Stop{
		"claude":          {Key: "s", Tool: "claude", At: end, TurnBegan: t0, IdleAt: end},
		"its own words":   {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Words: "Should I go on?"},
		"no turn start":   {Key: "s", Tool: "codex", At: end, IdleAt: end},
		"turn open":       {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Working: true},
		"a call running":  {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Busy: true},
		"background work": {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Tasks: 1},
		"an error":        {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Error: "stream disconnected"},
		"a past process":  {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Started: end.Add(time.Hour)},
		"automated":       {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Auto: true},
		"exited":          {Key: "s", Tool: "codex", At: end, TurnBegan: t0, IdleAt: end, Exited: true},
	} {
		if d := Plan(s, Tries{}, DefaultParams, nil); d.Rule == ruleNoReply {
			t.Fatalf("%s: %+v", name, d)
		}
	}
}
