package detect

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A guard's refusal its thread heeded is no failure: the thread's next call of the tool ran, within heedSpan. One
// the thread repeated, one with no call after it or only a late one, and one only another thread, another tool, a
// call made before the refusal came or the owner's no followed still count.
func TestAHeededRefusalIsNoFailure(t *testing.T) {
	st := testStore(t)
	deny := func(session, why string, at time.Time) store.Event {
		return toolError(session, "Bash", "<tool_use_error>fence-guard: "+why, at)
	}
	ran := func(session, agent, tool string, at time.Time) store.Event {
		e := ev(store.Transcripts, session, "tool_result", store.ActorTool, tool, "done", `{"tool_id":"y"}`, at)
		e.Agent = agent
		return e
	}
	beside := func(session, id string, at time.Time) store.Event {
		return call(session, "Bash", id, `{"command":"ls"}`, at)
	}
	done := func(session, id string, at time.Time) store.Event {
		return ev(store.Transcripts, session, "tool_result", store.ActorTool, "Bash", "done", `{"tool_id":"`+id+`"}`, at)
	}
	declinedCall := func(session string, at time.Time) store.Event {
		return toolError(session, "Bash", `{"error":{"message":"user rejected the command"}}`, at)
	}
	sec, hour := time.Second, time.Hour
	put(t, st,
		// heeded: each refusal's next Bash call ran
		deny("a1", "a wildcard delete", t0), ran("a1", "", "Bash", t0.Add(9*sec)),
		deny("a1", "a wildcard delete", t0.Add(hour)), ran("a1", "", "Bash", t0.Add(hour+9*sec)),
		deny("a2", "a wildcard delete", t0), ran("a2", "", "Bash", t0.Add(9*sec)),
		// repeated: only the last refusal of each run was heeded
		deny("b1", "a retry loop with no bound", t0), deny("b1", "a retry loop with no bound", t0.Add(9*sec)),
		deny("b1", "a retry loop with no bound", t0.Add(18*sec)), ran("b1", "", "Bash", t0.Add(27*sec)),
		deny("b2", "a retry loop with no bound", t0), deny("b2", "a retry loop with no bound", t0.Add(9*sec)),
		ran("b2", "", "Bash", t0.Add(18*sec)),
		// nothing after, or a call long after
		deny("c1", "a private link in a commit message", t0), ran("c1", "", "Bash", t0.Add(hour)),
		deny("c2", "a private link in a commit message", t0), deny("c3", "a private link in a commit message", t0),
		// another thread's call, and another tool's
		deny("d1", "a process match takes its own shell too", t0), ran("d1", "sub1", "Bash", t0.Add(9*sec)),
		deny("d1", "a process match takes its own shell too", t0.Add(hour)), ran("d1", "", "Read", t0.Add(hour+9*sec)),
		deny("d2", "a process match takes its own shell too", t0), ran("d2", "sub1", "Bash", t0.Add(9*sec)),
		// a call beside the refused one, made before the refusal came and done after it
		deny("e1", "a pipe into a shell", t0), beside("e1", "k1", t0.Add(-sec)), done("e1", "k1", t0.Add(5*sec)),
		deny("e1", "a pipe into a shell", t0.Add(hour)), beside("e1", "k2", t0.Add(hour-sec)), done("e1", "k2", t0.Add(hour+5*sec)),
		deny("e2", "a pipe into a shell", t0), beside("e2", "k3", t0.Add(-sec)), done("e2", "k3", t0.Add(5*sec)),
		// the owner's no in a wrapper is no call
		deny("f1", "a force push", t0), declinedCall("f1", t0.Add(5*sec)),
		deny("f1", "a force push", t0.Add(hour)), declinedCall("f1", t0.Add(hour+5*sec)),
		deny("f2", "a force push", t0), declinedCall("f2", t0.Add(5*sec)),
		caughtUp(t0.Add(3*hour)),
	)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"failures/v2:guard:fence-guard:a force push 3",
		"failures/v2:guard:fence-guard:a pipe into a shell 3",
		"failures/v2:guard:fence-guard:a private link in a commit message 3",
		"failures/v2:guard:fence-guard:a process match takes its own shell too 3",
		"failures/v2:guard:fence-guard:a retry loop with no bound 3",
	}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%v\nwant\n%v", got, want)
	}
}

// caughtUp is another session's result at a time: the store has read the transcripts up to it, so a refusal more
// than heedSpan before it no longer waits.
func caughtUp(at time.Time) store.Event {
	return ev(store.Transcripts, "later", "tool_result", store.ActorTool, "Read", "done", `{"tool_id":"y"}`, at)
}

// A refusal waits while the store holds no result more than heedSpan after it: the transcripts are ingested on
// their own clock, so the window's end says nothing of what the store has read, and the thread's next call may
// still come.
func TestARefusalWaitsForTheStoreToCatchUp(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>fence-guard: a wildcard delete"
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s2", "Bash", deny, t0), toolError("s3", "Bash", deny, t0))
	w := Window{Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour)}
	if fs, err := (Failures{}).Detect(context.Background(), st, w); err != nil || len(fs) != 0 {
		t.Fatalf("refusals at the store's newest result: %v %v", signatures(fs), err)
	}
	put(t, st, caughtUp(t0.Add(heedSpan+time.Second)))
	if fs, err := (Failures{}).Detect(context.Background(), st, w); err != nil || len(fs) != 1 {
		t.Fatalf("refusals the store has read past: %v %v", signatures(fs), err)
	}
}
