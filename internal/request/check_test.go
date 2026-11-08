package request

// Tests of the owner's checks on his own words. Synthetic data only.

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A list whose window starts after the result a standing check binds reads back to that result: a newer commit
// between them reopens the check in the list as in the handoff, and with none the list holds the check as the
// handoff does, with the result it names.
func TestListAndHandoffAgreeOnACheckFromBefore(t *testing.T) {
	for _, newer := range []bool{false, true} {
		name := "nothing newer"
		if newer {
			name = "a newer commit before the window"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			f.owner("s-one", at(0), "port the scheduler")
			f.tool("s-one", "", "Bash", at(1), `{"command":"git commit"}`, "[main 1111aaa] Port the scheduler", false)
			f.owner("s-one", at(3), "the port looks right to me")
			f.phase("s-one", at(4), "phase", "idle", nil)
			rq := f.group()[0]
			if c, err := Check(ctx, f.st, rq, "the port looks right", at(5)); err != nil || !c.Added || c.Result.Key != "commit:1111aaa" {
				t.Fatalf("check: %+v %v", c, err)
			}
			if newer {
				f.tool("s-one", "", "Bash", at(50), `{"command":"git commit"}`, "[main 2222bbb] Port the timers", false)
			}
			f.reply("s-one", at(300), "Anything else?")
			f.phase("s-one", at(301), "phase", "idle", nil)
			f.put()
			full, err := Find(ctx, f.st, rq.ID, Options{Redact: fakeRedact})
			if err != nil {
				t.Fatal(err)
			}
			list, err := List(ctx, f.st, at(200), Options{Redact: fakeRedact})
			if err != nil || len(list) != 1 {
				t.Fatalf("list: %v %d", err, len(list))
			}
			want := Checked
			if newer {
				want = Ready
			}
			if full.State != want || list[0].State != full.State || list[0].Why != full.Why {
				t.Fatalf("handoff %s (%s), list %s (%s)", full.State, full.Why, list[0].State, list[0].Why)
			}
		})
	}
}

// A check stored under an id that resolves to the request (an opening joined into it) counts for it.
func TestACheckUnderAnAliasCounts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-main", at(0), "ship the exporter")
	f.event("s-next", "", "tool_call", store.ActorAgent, at(19), "{}", map[string]any{"parent": "s-main"})
	f.owner("s-next", at(20), "now write the release notes")
	f.tool("s-next", "", "Bash", at(21), `{"command":"git commit"}`, "[main 3c4d5e6] Release notes", false)
	his := f.owner("s-next", at(22), "the notes read well, done")
	f.phase("s-next", at(23), "phase", "idle", nil)
	rq := f.group()[0]
	if len(rq.Aliases) != 1 {
		t.Fatalf("aliases: %v", rq.Aliases)
	}
	if ok, err := f.st.CheckOnce(ctx, store.RequestCheck{Request: rq.Aliases[0], Result: "commit:3c4d5e6", ResultAt: at(21).Add(1e9),
		Proof: his.ID(), ProofAt: at(22), At: at(24)}, nil); err != nil || !ok {
		t.Fatalf("store the check: %v %v", ok, err)
	}
	got := f.group()[0]
	if got.State != Checked {
		t.Fatalf("a check under its alias: %s (%s)", got.State, got.Why)
	}
	if _, _, err := Uncheck(ctx, f.st, got, "the notes read well", at(25)); !errors.Is(err, ErrNoProof) {
		t.Fatalf("an undo on the check's own turn: %v", err)
	}
}

// The id hashes the stored words of the opening. A read again that stores other words for it (tacit's cut of a
// paste reads the prompt history at read time) keeps the owner's check, by the time alone while one opening has it.
func TestAReadAgainKeepsTheCheck(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	opening := f.owner("s-re", at(0), "fix the importer")
	f.tool("s-re", "", "Bash", at(1), `{"command":"git commit"}`, "[main 3333ccc] Fix the importer", false)
	f.owner("s-re", at(2), "the importer fix looks right")
	f.phase("s-re", at(3), "phase", "idle", nil)
	rq := f.group()[0]
	if c, err := Check(ctx, f.st, rq, "importer fix looks right", at(4)); err != nil || !c.Added {
		t.Fatalf("check: %+v %v", c, err)
	}
	opening.Summary = "fix the importer [the paste it cut before]"
	f.evs = append(f.evs, opening)
	again := f.group()[0]
	if again.ID == rq.ID || again.State != Checked {
		t.Fatalf("after a read again: id %s (was %s), %s (%s)", again.ID, rq.ID, again.State, again.Why)
	}
}
