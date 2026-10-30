package sessions

import "time"

// WakeGrace is how long past its time a wakeup still counts as scheduled: a due wakeup takes a moment to fire.
const WakeGrace = 2 * time.Minute

// Blocking reports whether the session's ask stops it until its owner answers. A question, a plan or a
// permission blocks; an async ask (one that returned at once while its turn went on) blocks only once the turn
// is over.
func (s *Session) Blocking() bool {
	switch s.AskKind {
	case AskQuestion, AskPlan, AskPermission:
		return !s.TurnOpen || !s.AskAsync
	}
	return false
}

// PhaseOf is the phase rule. In order:
//  1. a blocking ask gives needs_you;
//  2. an open turn gives working;
//  3. tasks in flight give background, even after an error: finished work starts the next turn by itself;
//  4. an error gives needs_you;
//  5. a failed background job gives needs_you;
//  6. a wakeup or a loop gives scheduled;
//  7. otherwise idle.
//
// Only a review moves idle on to needs_you or done (package verdict), and only with evidence.
func PhaseOf(s *Session, now time.Time) Phase {
	switch {
	case s.Blocking():
		return NeedsYou
	case s.TurnOpen:
		return Working
	case len(s.Tasks) > 0:
		return Background
	case s.AskKind == AskError:
		return NeedsYou
	case s.Failed != "":
		return NeedsYou
	case !s.WakeAt.IsZero() && s.WakeAt.After(now.Add(-WakeGrace)) || s.Loop != "":
		return Scheduled
	}
	return Idle
}

// Decide sets the session's phase by PhaseOf. A failed job with no ask of its own gets its failure as the ask.
func Decide(s *Session, now time.Time) {
	s.Phase = PhaseOf(s, now)
	if s.Phase == NeedsYou && s.Ask == "" && s.Failed != "" {
		s.Ask, s.AskKind = s.Failed, AskError
	}
}
