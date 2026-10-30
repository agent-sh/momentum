package supervise

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
)

// history writes one session's transcript events into a store, a second apart unless an event says when.
type timelineBuilder struct {
	t       *testing.T
	session string
	at      time.Time
	evs     []store.Event
}

func newTimeline(t *testing.T, session string) *timelineBuilder {
	return &timelineBuilder{t: t, session: session, at: t0}
}

func (b *timelineBuilder) add(after time.Duration, kind string, actor store.Actor, tool, summary, meta string) *timelineBuilder {
	b.at = b.at.Add(after)
	b.evs = append(b.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%03d", b.session, len(b.evs)), TS: b.at,
		Session: b.session, Harness: "claude", Actor: actor, Kind: kind, Tool: tool, Summary: summary, Meta: meta})
	return b
}

func (b *timelineBuilder) owner(after time.Duration, text, prior string) *timelineBuilder {
	return b.add(after, "owner_text", store.ActorOwner, "", text, fmt.Sprintf(`{"input_kind":"typed","prior":%q}`, prior))
}
func (b *timelineBuilder) say(after time.Duration, text string) *timelineBuilder {
	return b.add(after, "agent_text", store.ActorAgent, "", text, "")
}
func (b *timelineBuilder) apiError(after time.Duration, text string) *timelineBuilder {
	return b.add(after, "harness_text", store.ActorHarness, "", text, `{"is_error":true,"why":"api error"}`)
}
func (b *timelineBuilder) notify(after time.Duration, text string) *timelineBuilder {
	return b.add(after, "harness_text", store.ActorHarness, "", text, `{"why":"origin task-notification"}`)
}
func (b *timelineBuilder) call(after time.Duration, tool, id, summary string) *timelineBuilder {
	return b.add(after, "tool_call", store.ActorAgent, tool, summary, fmt.Sprintf(`{"tool_id":%q}`, id))
}
func (b *timelineBuilder) result(after time.Duration, tool, id, summary string) *timelineBuilder {
	return b.add(after, "tool_result", store.ActorTool, tool, summary, fmt.Sprintf(`{"tool_id":%q}`, id))
}

func (b *timelineBuilder) put(st *store.Store) {
	if _, err := st.Put(context.Background(), b.evs); err != nil {
		b.t.Fatal(err)
	}
}

func outcomeOf(t *testing.T, rs []Result, session string, at time.Time) Result {
	t.Helper()
	for _, r := range rs {
		if r.Point.Session == session && r.Point.Stop.At.Equal(at) {
			return r
		}
	}
	t.Fatalf("no stop of %s at %v", session, at)
	return Result{}
}

func TestReplayMeasuresEachRuleAgainstWhatCameNext(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	// A turn ends on a 503; the owner nudges 30 minutes later: covered.
	newTimeline(t, "nudged").owner(0, "deploy it", "turn_duration").say(10*time.Second, "Deploying.").
		apiError(time.Minute, err503).owner(30*time.Minute, ".", "").put(st)
	// A turn ends on a promise, and a notification wakes it 5 minutes later: the rule waits longer, so it does not act.
	newTimeline(t, "woke").owner(0, "run the suite", "turn_duration").say(10*time.Second, "The suite is running. I'll report when it finishes.").
		notify(5*time.Minute, "<task-notification> <task-id>b1</task-id> <status>completed</status> </task-notification>").put(st)
	// A turn ends on a promise and nothing came after it: the rule would have acted unasked.
	newTimeline(t, "left").owner(0, "watch it", "turn_duration").say(10*time.Second, "Watching it. I'll check again at noon.").put(st)
	// A background command is in flight: no promise rule.
	newTimeline(t, "busy").owner(0, "build it", "turn_duration").call(time.Second, "Bash", "tu1", `{"command":"make"}`).
		result(time.Second, "Bash", "tu1", "Command running in background with ID: bx1. Output is being written to: /x").
		say(time.Second, "Building. I'll report when it finishes.").owner(time.Hour, "status?", "turn_duration").put(st)

	until := t0.Add(5 * time.Hour)
	points, err := History(ctx, st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Label{{Session: "nudged", At: t0.Add(10*time.Second + time.Minute + 30*time.Minute), Kind: "nudge"}}
	rs := Replay(points, labels, DefaultParams, probeAt(time.Time{}), until)

	r := outcomeOf(t, rs, "nudged", t0.Add(10*time.Second+time.Minute))
	// It would have tried at 10 minutes, but the owner typed the turn 70 seconds before it ended: held until 15
	// minutes after his turn.
	if r.Outcome != Covered || r.Decision.Rule != RuleTransient || r.Decision.Verdict != Hold || r.Earlier != 16*time.Minute+10*time.Second {
		t.Fatalf("nudged: %+v", r)
	}
	if r := outcomeOf(t, rs, "woke", t0.Add(10*time.Second)); r.Decision.Rule != RulePromise || r.Fired || r.Outcome != "" {
		t.Fatalf("woke: %+v", r)
	}
	if r := outcomeOf(t, rs, "left", t0.Add(10*time.Second)); r.Outcome != Left {
		t.Fatalf("left: %+v", r)
	}
	if r := outcomeOf(t, rs, "busy", t0.Add(3*time.Second)); r.Point.Stop.Phase != sessions.Background || r.Decision.Rule != "" {
		t.Fatalf("busy: %+v %+v", r.Point.Stop, r.Decision)
	}
	sums := Summarize(rs)
	for _, s := range sums {
		if s.Rule == RuleTransient && (s.Covered != 1 || s.FalseActions() != 0) || s.Rule == RulePromise && s.Left != 1 {
			t.Fatalf("summary %+v", sums)
		}
	}
}

func TestAnInterruptionHoldsTheReplayToo(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	// He interrupts a turn, and the next one stops on a 503 a minute later: he was there.
	newTimeline(t, "esc").owner(0, "deploy it", "turn_duration").call(time.Second, "Bash", "tu1", `{"command":"deploy"}`).
		add(5*time.Minute, "harness_text", store.ActorHarness, "", "[Request interrupted by user for tool use]", "").
		say(time.Second, "Stopped.").apiError(time.Minute, err503).put(st)
	points, err := History(ctx, st, t0.Add(-time.Hour), t0.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		if p.Stop.Error == "" {
			continue
		}
		d := Plan(p.Stop, Tries{}, DefaultParams, nil)
		interrupted := t0.Add(time.Second + 5*time.Minute)
		if d.Rule != RuleTransient || d.Verdict != Hold || !d.At.Equal(interrupted.Add(DefaultParams.OwnerWindow)) {
			t.Fatalf("%+v", d)
		}
		return
	}
	t.Fatal("no stop on the error")
}

func TestHistoryFindsAReopeningInATranscriptTheStoreCut(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	// The store keeps the start of a notification; what says it is a reopening's comes later in the line.
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"user","message":{"content":"<task-notification><task-id>b7</task-id><output-file>` + strings.Repeat("x", 300) +
		`</output-file><status>stopped</status><summary>Background shell command didn't finish before the previous session ended</summary></task-notification>"}}` + "\n"
	pad := `{"type":"assistant"}` + "\n"
	if err := os.WriteFile(path, []byte(pad+line), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newTimeline(t, "reopened").owner(0, "run the soak test", "turn_duration").call(time.Second, "Bash", "tu1", `{"command":"soak"}`).
		result(time.Second, "Bash", "tu1", "Command running in background with ID: b7. Output is being written to: /x").
		say(time.Second, "The soak test is running.")
	b.notify(2*time.Hour, "<task-notification> <task-id>b7</task-id> <output-file>/tmp/"+strings.Repeat("x", 200))
	b.evs[len(b.evs)-1].Ref = fmt.Sprintf("%s@%d#0", path, len(pad))
	b.owner(10*time.Second, "resume", "")
	b.put(st)
	points, err := History(ctx, st, t0.Add(-time.Hour), t0.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		if p.Next.Text == "resume" {
			if p.Stop.ResumedAt.IsZero() || p.Stop.Tasks != 0 {
				t.Fatalf("the reopening was missed: %+v", p.Stop)
			}
			return
		}
	}
	t.Fatal("no point before the resume")
}

func TestReadLabelsReadsBothForms(t *testing.T) {
	ls, err := ReadLabels(strings.NewReader(`{"session":"a","at":"2026-03-04T10:00:00Z","kind":"nudge"}
{"session":"b","at":1772618400000,"kind":"restart"}
{"sid":"c","ep":1772618400.5,"kind":"turn","fam":["F1"],"text":"resume"}
`))
	if err != nil || len(ls) != 3 || ls[0].Kind != "nudge" || ls[1].Kind != "restart" || ls[2].Kind != "nudge" || ls[2].Session != "c" ||
		!ls[1].At.Equal(time.UnixMilli(1772618400000)) {
		t.Fatalf("labels %+v %v", ls, err)
	}
	if _, err := ReadLabels(strings.NewReader(`{"session":"a"}`)); err == nil {
		t.Fatal("a label with no time")
	}
}

func TestNudgeKnowsTheOwnersShortWords(t *testing.T) {
	for _, s := range []string{".", ",", "continue", "Resume", "try again", "Try again", "keep going", "dont stop", "don't stop"} {
		if !Nudge(s) {
			t.Errorf("%q is a nudge", s)
		}
	}
	for _, s := range []string{"status?", "rename the config flag", "", "what changed in the parser"} {
		if Nudge(s) {
			t.Errorf("%q is not a nudge", s)
		}
	}
}

func TestAccessBackNeedsASessionThatWasBlocked(t *testing.T) {
	// "old" met the error long before and has worked since: it was not blocked when this stop came.
	calls := []store.Mark{{Session: "old", At: t0.Add(-39 * time.Hour)}, {Session: "other", At: t0.Add(time.Minute)},
		{Session: "old", At: t0.Add(2 * time.Minute)}, {Session: "blocked", At: t0.Add(time.Hour)}}
	errs := []store.Mark{{Session: "old", At: t0.Add(-40 * time.Hour), Summary: errLogin}, {Session: "blocked", At: t0, Summary: errLogin}}
	p := AccessProbe{Calls: calls, Errors: errs}
	if back, ok := p.AccessBack(Login, t0); !ok || !back.Equal(t0.Add(time.Hour)) {
		t.Fatalf("back %v %v: a session that never met the error, or met it and worked since, proves nothing", back, ok)
	}
	if _, ok := p.AccessBack(Org, t0); ok {
		t.Fatal("no session met an organization block")
	}
}
