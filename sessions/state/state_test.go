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

func TestApplyReportsEveryChangeOfStretch(t *testing.T) {
	st := Stamps{}
	asks := &sessions.Session{Key: "b", Tool: "claude", Name: "cleanup", Phase: sessions.NeedsYou, Seq: 4, LastActive: t0.Add(-5 * time.Minute),
		Ask: "permission prompt: Bash: rm -rf build", AskKind: sessions.AskPermission}
	got := st.Apply([]*sessions.Session{sess("a", sessions.Idle, 3, t0.Add(-time.Hour)), asks}, t0)
	if len(got) != 2 || got[0].Key != "a" || got[0].From != "" || got[0].To != sessions.Idle || !got[0].At.Equal(t0.Add(-time.Hour)) ||
		got[1].To != sessions.NeedsYou || got[1].ToAskKind != sessions.AskPermission || got[1].ToAsk != asks.Ask || got[1].Tool != "claude" {
		t.Fatalf("first sightings: %+v", got)
	}
	// both start working; the wait on the permission prompt lasted from its start until now
	got = st.Apply([]*sessions.Session{sess("a", sessions.Working, 5, t0), sess("b", sessions.Working, 6, t0)}, t0.Add(3*time.Minute))
	if len(got) != 2 || got[0].From != sessions.Idle || got[0].For != time.Hour+3*time.Minute ||
		got[1].From != sessions.NeedsYou || got[1].To != sessions.Working || got[1].For != 8*time.Minute ||
		got[1].FromAskKind != sessions.AskPermission || got[1].FromAsk != asks.Ask || got[1].Name != "cleanup" || !got[1].Seen.Equal(t0) {
		t.Fatalf("into working: %+v", got)
	}
	// a's turn ended at +5m; b keeps working, which is no change
	got = st.Apply([]*sessions.Session{sess("a", sessions.Idle, 9, t0.Add(5*time.Minute)), sess("b", sessions.Working, 8, t0)}, t0.Add(6*time.Minute))
	if len(got) != 1 || got[0].Key != "a" || got[0].From != sessions.Working || got[0].To != sessions.Idle || got[0].For != 2*time.Minute {
		t.Fatalf("a turn ends: %+v", got)
	}
	// a ran a whole turn between two runs: a new idle stretch
	got = st.Apply([]*sessions.Session{sess("a", sessions.Idle, 12, t0.Add(8*time.Minute))}, t0.Add(9*time.Minute))
	if len(got) != 1 || got[0].From != sessions.Idle || got[0].To != sessions.Idle || got[0].For != 3*time.Minute {
		t.Fatalf("idle again: %+v", got)
	}
	// b is gone for longer than Keep: it left working, last seen at +6m
	got = st.Apply([]*sessions.Session{sess("a", sessions.Idle, 12, t0.Add(8*time.Minute))}, t0.Add(9*time.Minute+Keep+time.Minute))
	if len(got) != 1 || got[0].Key != "b" || got[0].From != sessions.Working || got[0].To != "" || !got[0].At.Equal(t0.Add(6*time.Minute)) ||
		got[0].For != 3*time.Minute {
		t.Fatalf("gone: %+v", got)
	}
}

func TestAFailedScanKeepsItsSessionsStamps(t *testing.T) {
	st := Stamps{}
	hermes := func() *sessions.Session {
		return &sessions.Session{Key: "h", Tool: "hermes", Phase: sessions.Idle, Seq: 2, LastActive: t0.Add(-time.Hour)}
	}
	st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0), hermes()}, t0)
	// Hermes's scan fails for longer than Keep: its conversation is unknown, not gone
	if got := st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0)}, t0.Add(Keep+time.Minute), "hermes"); len(got) != 0 {
		t.Fatalf("changes while the scan failed: %+v", got)
	}
	if got := st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0), hermes()}, t0.Add(Keep+2*time.Minute)); len(got) != 0 {
		t.Fatalf("the same stretch goes on once the scan works: %+v", got)
	}
	got := st.Apply([]*sessions.Session{sess("a", sessions.Idle, 1, t0)}, t0.Add(2*Keep+3*time.Minute))
	if len(got) != 1 || got[0].Key != "h" || got[0].To != "" || !got[0].At.Equal(t0.Add(Keep+2*time.Minute)) {
		t.Fatalf("gone once a working scan misses it: %+v", got)
	}
}

func TestAPartialReadChangesNothing(t *testing.T) {
	st := Stamps{}
	st.Apply([]*sessions.Session{sess("a", sessions.Idle, 5, t0.Add(-time.Hour))}, t0)
	short := sess("a", sessions.Idle, 2, t0.Add(-time.Hour)) // a failed page: the log came back short
	short.Partial = true
	if got := st.Apply([]*sessions.Session{short}, t0.Add(10*time.Minute)); len(got) != 0 || !short.Since.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("changes %+v since %v", got, short.Since)
	}
	if r := st["a"]; r.Seq != 5 || !r.Seen.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("the stamp keeps its stretch and records the sighting: %+v", r)
	}
	if got := st.Apply([]*sessions.Session{sess("a", sessions.Idle, 5, t0.Add(-time.Hour))}, t0.Add(20*time.Minute)); len(got) != 0 {
		t.Fatalf("a whole read of the same stretch: %+v", got)
	}
	unknown := sess("codex:proc:11:1100", sessions.Idle, 0, time.Time{}) // a process whose thread lookup failed
	unknown.Partial = true
	if got := st.Apply([]*sessions.Session{unknown}, t0.Add(21*time.Minute)); len(got) != 0 || len(st) != 1 {
		t.Fatalf("a first sighting read in part gets no stamp: %+v %v", got, st)
	}
}
