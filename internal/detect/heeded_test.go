package detect

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A guard's refusal its thread heeded is no failure: the thread's next call of the tool ran, within heedSpan. One
// the thread repeated, one with no call after it or only a late one, and one only another thread or another tool
// followed still count.
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
	)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"failures/v2:guard:fence-guard:a private link in a commit message 3",
		"failures/v2:guard:fence-guard:a process match takes its own shell too 3",
		"failures/v2:guard:fence-guard:a retry loop with no bound 3",
	}
	if got := signatures(fs); !slices.Equal(got, want) {
		t.Fatalf("found\n%v\nwant\n%v", got, want)
	}
}

// A refusal younger than heedSpan at the window's end waits: its thread's next call may still come.
func TestARefusalAtTheWindowsEndWaits(t *testing.T) {
	st := testStore(t)
	deny := "<tool_use_error>fence-guard: a wildcard delete"
	put(t, st, toolError("s1", "Bash", deny, t0), toolError("s2", "Bash", deny, t0), toolError("s3", "Bash", deny, t0))
	w := Window{Since: t0.Add(-time.Hour), Until: t0.Add(heedSpan)}
	if fs, err := (Failures{}).Detect(context.Background(), st, w); err != nil || len(fs) != 0 {
		t.Fatalf("young refusals: %v %v", signatures(fs), err)
	}
	w.Until = t0.Add(heedSpan + time.Second)
	if fs, err := (Failures{}).Detect(context.Background(), st, w); err != nil || len(fs) != 1 {
		t.Fatalf("settled refusals: %v %v", signatures(fs), err)
	}
}
