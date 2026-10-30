package sessions

import (
	"testing"
	"time"
)

// The phase rule, one row per rule and per precedence between them.
func TestPhaseRule(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	task := []Task{{ID: "b1", Kind: "bash"}}
	for _, tc := range []struct {
		name string
		s    Session
		want Phase
	}{
		{"nothing at all", Session{}, Idle},
		{"1: a question while its tool call waits", Session{TurnOpen: true, AskKind: AskQuestion}, NeedsYou},
		{"1: a plan waits for approval", Session{TurnOpen: true, AskKind: AskPlan}, NeedsYou},
		{"1: a permission dialog", Session{TurnOpen: true, AskKind: AskPermission}, NeedsYou},
		{"1: an ask beats tasks in flight", Session{AskKind: AskQuestion, Tasks: task}, NeedsYou},
		{"1: an async ask while its turn runs does not block", Session{TurnOpen: true, AskKind: AskQuestion, AskAsync: true}, Working},
		{"1: an async ask blocks once the turn is over", Session{AskKind: AskQuestion, AskAsync: true}, NeedsYou},
		{"1: a blocking ask that holds the turn open blocks", Session{TurnOpen: true, AskKind: AskQuestion, AskAsync: false}, NeedsYou},
		{"2: an open turn", Session{TurnOpen: true}, Working},
		{"2: an open turn beats an error", Session{TurnOpen: true, AskKind: AskError}, Working},
		{"3: tasks in flight", Session{Tasks: task}, Background},
		{"3: tasks beat an error: finished work starts the next turn", Session{Tasks: task, AskKind: AskError}, Background},
		{"3: tasks beat a failed job", Session{Tasks: task, Failed: "it broke"}, Background},
		{"4: an error", Session{AskKind: AskError}, NeedsYou},
		{"4: an error beats a wakeup", Session{AskKind: AskError, WakeAt: now.Add(time.Hour)}, NeedsYou},
		{"5: a failed background job", Session{Failed: "it broke"}, NeedsYou},
		{"5: a failure beats a loop", Session{Failed: "it broke", Loop: "*/5 * * * *"}, NeedsYou},
		{"6: a wakeup ahead", Session{WakeAt: now.Add(10 * time.Minute)}, Scheduled},
		{"6: a wakeup just due still counts", Session{WakeAt: now.Add(-WakeGrace + time.Second)}, Scheduled},
		{"6: a wakeup long past does not", Session{WakeAt: now.Add(-WakeGrace - time.Second)}, Idle},
		{"6: a loop", Session{Loop: "*/5 * * * *"}, Scheduled},
		{"7: a review's own ask is not a scanner fact", Session{AskKind: AskModel}, Idle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseOf(&tc.s, now); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDecideGivesAFailedJobItsFailureAsTheAsk(t *testing.T) {
	s := &Session{Failed: "the job failed: exit 1"}
	Decide(s, time.Now())
	if s.Phase != NeedsYou || s.Ask != "the job failed: exit 1" || s.AskKind != AskError {
		t.Fatalf("got %+v", s)
	}
	q := &Session{Failed: "x", AskKind: AskQuestion, Ask: "Which one?"}
	Decide(q, time.Now())
	if q.Ask != "Which one?" || q.AskKind != AskQuestion {
		t.Fatalf("an existing ask stays: %+v", q)
	}
}
