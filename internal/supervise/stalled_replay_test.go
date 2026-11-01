package supervise

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// recorded writes a session's transcript and the store's events of it together: each line the store keeps (an
// owner's turn, a call, a result, an agent's text) is an event whose Ref is its line's place, and an api-request
// line is in the transcript only, as Claude Code writes it and tacit's reader passes it over.
type recorded struct {
	t       *testing.T
	session string
	path    string
	at      time.Time
	off     int64
	evs     []store.Event
}

func newRecorded(t *testing.T, session string) *recorded {
	return &recorded{t: t, session: session, path: filepath.Join(t.TempDir(), session+".jsonl"), at: t0}
}

func (r *recorded) line(after time.Duration, v map[string]any) (int64, time.Time) {
	r.t.Helper()
	r.at = r.at.Add(after)
	if _, ok := v["timestamp"]; !ok {
		v["timestamp"] = r.at.Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(v)
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
	off := r.off
	r.off += int64(len(b) + 1)
	return off, r.at
}

func (r *recorded) event(after time.Duration, v map[string]any, kind string, actor store.Actor, tool, summary, meta string) *recorded {
	off, at := r.line(after, v)
	r.evs = append(r.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%d", r.session, off), TS: at, Session: r.session,
		Harness: "claude", Actor: actor, Kind: kind, Tool: tool, Summary: summary, Meta: meta, Ref: fmt.Sprintf("%s@%d#0", r.path, off)})
	return r
}

func (r *recorded) owner(after time.Duration, text string) *recorded {
	return r.event(after, map[string]any{"type": "user", "message": map[string]any{"content": text}}, "owner_text", store.ActorOwner, "", text,
		`{"input_kind":"typed"}`)
}

func (r *recorded) request(after time.Duration) *recorded {
	r.line(after, map[string]any{"type": "api-request", "querySource": "repl_main_thread"})
	return r
}

func (r *recorded) notice(after time.Duration) *recorded {
	r.line(after, map[string]any{"type": "system", "subtype": "informational", "content": "a plugin reloaded"})
	return r
}

func (r *recorded) call(after time.Duration, id string) *recorded {
	return r.event(after, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id}}}},
		"tool_call", store.ActorAgent, "Bash", `{"command":"make"}`, fmt.Sprintf(`{"tool_id":%q}`, id))
}

func (r *recorded) result(after time.Duration, id string) *recorded {
	return r.event(after, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id}}}},
		"tool_result", store.ActorTool, "Bash", "ok", fmt.Sprintf(`{"tool_id":%q}`, id))
}

func (r *recorded) say(after time.Duration, text string) *recorded {
	return r.event(after, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}},
		"agent_text", store.ActorAgent, "", text, "")
}

func (r *recorded) put(st *store.Store) {
	if _, err := st.Put(context.Background(), r.evs); err != nil {
		r.t.Fatal(err)
	}
}

func TestReplayFindsTheRequestATurnWaitedOn(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	// A request that never answers: the owner nudges it 40 minutes later.
	stuck := newRecorded(t, "stuck").owner(0, "check the lab").request(time.Second).notice(10 * time.Minute)
	sent := t0.Add(time.Second)
	stuck.owner(30*time.Minute, "continue")
	stuck.put(st)
	// A request whose reply took 20 minutes to begin: the session went on by itself.
	slow := newRecorded(t, "slow").owner(0, "prove it").call(time.Second, "tu1").result(time.Second, "tu1").request(time.Second).
		say(20*time.Minute, "Proved.")
	slow.put(st)
	// A notification whose record Claude Code wrote after the request that carries it, dated before it.
	late := newRecorded(t, "late").owner(0, "watch it").say(time.Second, "Watching.")
	late.request(time.Minute)
	carried := late.at
	off, _ := late.line(0, map[string]any{"type": "user", "timestamp": carried.Add(-50 * time.Millisecond).Format(time.RFC3339Nano),
		"message": map[string]any{"content": "<task-notification> done"}})
	late.evs = append(late.evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("late@%d", off), TS: carried.Add(-50 * time.Millisecond),
		Session: "late", Harness: "claude", Actor: store.ActorHarness, Kind: "harness_text",
		Summary: "<task-notification> <task-id>b2</task-id> <status>completed</status> </task-notification>", Meta: `{"why":"origin task-notification"}`,
		Ref: fmt.Sprintf("%s@%d#0", late.path, off)})
	late.owner(40*time.Minute, "continue")
	late.put(st)
	// A notification at a turn's end that sent no request: nothing waited on the model.
	quiet := newRecorded(t, "quiet").owner(0, "watch it").say(time.Second, "Watching.")
	quiet.event(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification> done"}},
		"harness_text", store.ActorHarness, "", "<task-notification> <task-id>b1</task-id> <status>completed</status> </task-notification>",
		`{"why":"origin task-notification"}`)
	quiet.owner(time.Hour, "status?")
	quiet.put(st)

	until := t0.Add(5 * time.Hour)
	points, err := History(ctx, st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Label{{Session: "stuck", At: stuck.at, Kind: "nudge"}, {Session: "late", At: late.at, Kind: "nudge"}}
	rs := Replay(points, labels, DefaultParams, probeAt(time.Time{}), until)
	var stalls, replies []Result
	for _, r := range rs {
		switch {
		case r.Point.Waiting:
			replies = append(replies, r)
		case r.Decision.Rule == RuleStalled:
			stalls = append(stalls, r)
		}
	}
	if len(stalls) != 2 || stalls[0].Point.Session != "stuck" || !stalls[0].Point.Stop.RequestAt.Equal(sent) || stalls[0].Outcome != Covered ||
		stalls[0].Earlier != stuck.at.Sub(sent.Add(30*time.Minute)) {
		t.Fatalf("stalls %+v", stalls)
	}
	if stalls[1].Point.Session != "late" || !stalls[1].Point.Stop.RequestAt.Equal(carried) || stalls[1].Outcome != Covered {
		t.Fatalf("a request before the record it carries: %+v", stalls[1])
	}
	if len(replies) != 1 || replies[0].Point.Session != "slow" || replies[0].Fired || replies[0].Decision.Rule != RuleStalled {
		t.Fatalf("replies %+v", replies)
	}
	// Only the stalled rule reads a turn that waited: a reopening's notice before the reply is no reopened stop.
	back := newRecorded(t, "back").owner(0, "soak it")
	back.event(time.Second, map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification> stopped"}}, "harness_text",
		store.ActorHarness, "", "<task-notification> <task-id>b9</task-id> <status>stopped</status> <summary>Background shell command didn't finish before the previous session ended</summary> </task-notification>",
		`{"why":"origin task-notification"}`)
	back.request(time.Second).say(5*time.Minute, "Restarting the soak.")
	back.put(st)
	points, err = History(ctx, st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Replay(points, labels, DefaultParams, probeAt(time.Time{}), until) {
		if r.Point.Waiting && r.Decision.Rule != "" && r.Decision.Rule != RuleStalled {
			t.Fatalf("a waiting point read by %s: %+v", r.Decision.Rule, r)
		}
	}
	// With a wait under the reply's, the rule would have interrupted a turn that went on by itself.
	p := DefaultParams
	p.StalledWait = 10 * time.Minute
	for _, r := range Replay(points, labels, p, probeAt(time.Time{}), until) {
		if r.Point.Session == "slow" && r.Point.Waiting && r.Outcome != SelfWoke {
			t.Fatalf("a 10-minute wait: %+v", r)
		}
		if r.Point.Session == "quiet" && r.Decision.Rule == RuleStalled {
			t.Fatalf("a turn that sent no request: %+v", r)
		}
	}
}

func TestTheReplayReadsNoRequestRecordPastItsWindow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	// The window ends while the request waits; its reply came after the window.
	r := newRecorded(t, "open").owner(0, "prove it").request(time.Second)
	sent := r.at
	r.say(2*time.Hour, "Proved.")
	r.put(st)
	until := t0.Add(time.Hour)
	points, err := History(ctx, st, t0.Add(-time.Hour), until)
	if err != nil {
		t.Fatal(err)
	}
	last := points[len(points)-1]
	if last.Next.Kind != "none" || !last.Stop.RequestAt.Equal(sent) {
		t.Fatalf("the last stop: %+v", last)
	}
}

// A request at the snapshot's exclusive end cannot change the replay's pending request.
func TestReplayRequestReadsStopAtTheSnapshotEnd(t *testing.T) {
	r := newRecorded(t, "bounded").owner(0, "inspect the worker").request(time.Second)
	sent := r.at
	r.request(time.Hour)
	end := r.at
	if got := requestBetween(r.evs[0].Ref, "", end); !got.Equal(sent) {
		t.Fatalf("request %s want %s", got, sent)
	}
}
