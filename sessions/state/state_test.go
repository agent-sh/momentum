package state

import (
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
)

var t0 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func sess(key string, phase sessions.Phase, seq int, last time.Time) *sessions.Session {
	return &sessions.Session{Key: key, Phase: phase, Seq: seq, LastActive: last}
}

func TestAFirstSightingDatesThePhaseFromTheLastMove(t *testing.T) {
	st := Stamps{}
	idle, work := sess("a", sessions.Idle, 3, t0.Add(-time.Hour)), sess("b", sessions.Working, 3, t0.Add(-time.Hour))
	st.Apply([]*sessions.Session{idle, work}, t0)
	if !idle.Since.Equal(t0.Add(-time.Hour)) || !work.Since.Equal(t0) {
		t.Fatalf("idle since %v, working since %v", idle.Since, work.Since)
	}
}

func TestAStampSurvivesRunsWhileNothingChanges(t *testing.T) {
	st := Stamps{}
	st.Apply([]*sessions.Session{sess("a", sessions.Working, 3, t0)}, t0)
	for i := 1; i <= 3; i++ { // later runs, in new processes that loaded the stamps
		s := sess("a", sessions.Working, 3+i, t0.Add(time.Duration(i)*time.Minute)) // a working turn writes as it goes
		st.Apply([]*sessions.Session{s}, t0.Add(time.Duration(i)*time.Minute))
		if !s.Since.Equal(t0) {
			t.Fatalf("run %d: since %v", i, s.Since)
		}
	}
}

func TestAPhaseChangeSeenLateDatesFromTheTurnsEnd(t *testing.T) {
	st := Stamps{}
	st.Apply([]*sessions.Session{sess("a", sessions.Working, 3, t0)}, t0)
	end := t0.Add(2 * time.Minute) // the turn ended here; the next run comes ten minutes later
	s := sess("a", sessions.Idle, 9, end)
	st.Apply([]*sessions.Session{s}, t0.Add(10*time.Minute))
	if !s.Since.Equal(end) {
		t.Fatalf("since %v, want the turn's end %v", s.Since, end)
	}
	asks := sess("a", sessions.NeedsYou, 9, end) // a permission dialog opened; the transcript did not move
	st.Apply([]*sessions.Session{asks}, t0.Add(12*time.Minute))
	if !asks.Since.Equal(t0.Add(12 * time.Minute)) {
		t.Fatalf("a change without a move dates from the run: %v", asks.Since)
	}
}

func TestATurnBetweenTwoRunsStartsANewIdleStretch(t *testing.T) {
	st := Stamps{}
	st.Apply([]*sessions.Session{sess("a", sessions.Idle, 5, t0.Add(-time.Hour))}, t0)
	end := t0.Add(20 * time.Minute)
	s := sess("a", sessions.Idle, 8, end) // idle again, after a turn the runs did not see
	st.Apply([]*sessions.Session{s}, t0.Add(30*time.Minute))
	if !s.Since.Equal(end) {
		t.Fatalf("since %v, want %v", s.Since, end)
	}
}

func TestStampsOfDepartedSessionsExpire(t *testing.T) {
	st := Stamps{}
	st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0), sess("b", sessions.Idle, 1, t0)}, t0)
	st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0)}, t0.Add(Keep))
	if _, ok := st["b"]; !ok {
		t.Fatal("a session gone for Keep is still remembered")
	}
	st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0)}, t0.Add(Keep+time.Minute))
	if _, ok := st["b"]; ok {
		t.Fatal("a session gone for longer than Keep is forgotten")
	}
	if _, ok := st["a"]; !ok {
		t.Fatal("a session still seen stays")
	}
}

func TestAMoveInTheFutureDatesFromNow(t *testing.T) {
	st := Stamps{}
	s := sess("a", sessions.Idle, 1, t0.Add(time.Hour)) // a clock skew
	st.Apply([]*sessions.Session{s}, t0)
	if !s.Since.Equal(t0) {
		t.Fatalf("since %v", s.Since)
	}
}

func TestDismissalsHoldUntilTheSessionMoves(t *testing.T) {
	d := Dismissals{"a": 7}
	if !d.Covers("a", 7) || d.Covers("a", 8) || d.Covers("b", 7) {
		t.Fatal("covers")
	}
}
