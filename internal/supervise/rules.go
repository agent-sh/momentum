// Package supervise keeps the owner's agent sessions moving without him: a session that stopped on a transient
// API error, a login or a usage limit, a reopening after a restart, a Hermes output cutoff, a model request that
// never got its reply, a wakeup or a loop run of its own that never came, or a turn that ended on a promise nothing
// will wake it for gets the message that continues it. Deterministic rules decide; no
// model takes part. Each rule is a small function of a Stop, and the replay runs the same functions over the
// store's history to measure them.
package supervise

import (
	"fmt"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
)

// The rules, in the order they are tried.
const (
	RuleTransient = "transient" // the turn ended on a transient API error: try again with a backoff
	RuleAccess    = "access"    // the turn ended on a login, a usage limit or an organization block: resume once access is back
	RuleCutoff    = "cutoff"    // Hermes stopped at the model's output limit and asks for `continue`
	RuleReopened  = "reopened"  // the session was reopened after it ended mid-task and sits idle
	RuleStalled   = "stalled"   // a Claude Code turn waits on a model request that never got its reply
	RuleOverdue   = "overdue"   // a wakeup or a loop run the session set came due long ago and did not fire
	RulePromise   = "promise"   // the turn ended on a promise or a step it announced, and nothing will wake it
)

// Rules lists them in order.
var Rules = []string{RuleTransient, RuleAccess, RuleCutoff, RuleReopened, RuleStalled, RuleOverdue, RulePromise}

// Verdicts.
const (
	Act  = "act"  // send Text now
	Wait = "wait" // a rule applies and acts at At; with no At, it waits for a sign (access back)
	Hold = "hold" // a rule applies, but its owner typed into the session recently: it looks again at At
	Skip = "skip" // never: the session asks its owner, is automated, or has no terminal to type into
	Case = "case" // the rule used up its tries: a case opens, and nothing more is sent
)

// Marker opens every message momentum types, so the session, the owner and the evidence can tell it from his.
const Marker = "[momentum]"

// Stop is a session at a point where it stopped, with the facts the rules read.
type Stop struct {
	Key, Tool, Name string
	Where           string // a tmux target, tty, bg, or a Hermes platform
	Auto            bool   // an automated run
	Phase           sessions.Phase
	At              time.Time        // when the session stopped: its last turn ended
	Error           string           // the API error the turn ended on
	Words           string           // the agent's last words, as Words keeps them
	Ask             sessions.AskKind // a blocking ask: a question, a plan or a permission
	Working         bool             // a turn is open
	Busy            bool             // a tool call is running, or the model owes its reply mid-call
	Tasks           int              // background work in flight
	WakeAt          time.Time
	Loop            string
	Runs            []sessions.Run // its own runs its harness should start, earliest first
	WakeDue         time.Time      // when the run the overdue rule reads (Choose) should have started at the latest
	WakeFor         string         // what that run is for
	WakeKey         string         // which run that is
	WakeJob         bool           // a scheduled job's run, not a self-paced loop's wakeup
	WakeOnce        bool           // a one-shot job's only run
	WakeSign        string         // why a self-paced wakeup may have been lost: a cause its harness drops one for
	TurnBegan       time.Time      // when its last turn began
	RanAt           time.Time      // when a scheduled run of its own last started
	IdleAt          time.Time      // when its last turn ended
	ResumedAt       time.Time      // the harness reopened the session after it ended mid-task, and nothing happened since
	RequestAt       time.Time      // the model request the open turn still waits on: nothing answered it since
	Queued          int            // messages its user typed while the turn ran, which the harness holds
	Slept, Woke     time.Time      // the machine's last suspend: a wait counts from its end when it began before the wait ended
	OwnerAt         time.Time      // when the owner was last at the session: his last turn, or his opening it
	Started         time.Time      // when the session's process started; a stop before it is a past process's
}

// Tries counts a session's messages by rule in the episode that is still going: since the owner's last turn and
// the rule's last message that worked.
type Tries map[string]int

// Probe tells when access came back after an access error: a login, a usage limit or an organization block.
type Probe interface {
	// AccessBack is the first time after since that access of the error's class worked again.
	AccessBack(class string, since time.Time) (time.Time, bool)
}

// Params are the thresholds.
type Params struct {
	OwnerWindow  time.Duration   // the owner typed into the session within it: he is there, so momentum holds
	Backoff      []time.Duration // transient: the wait after the error before each try; their count bounds the tries
	AccessTries  int             // access: tries, one per sign that access is back
	ResetGrace   time.Duration   // access: after the reset time a limit states
	CutoffWait   time.Duration
	CutoffTries  int
	ReopenWait   time.Duration
	StalledWait  time.Duration // stalled: past the longest a reply legitimately takes to begin
	StalledTries int
	OverdueSlack time.Duration // overdue: past the latest a wakeup or a loop run that fired came
	OverdueTries int
	PromiseWait  time.Duration
	PromiseTries int           // promise: messages without an owner turn in between
	Stale        time.Duration // a rule whose time came longer ago than this does not act: the stop is past
}

// DefaultParams are the thresholds momentum ships with, chosen with `momentum supervise replay` over a store's
// history; SPEC.md, Supervision, says how each one is chosen.
var DefaultParams = Params{
	OwnerWindow:  15 * time.Minute,
	Backoff:      []time.Duration{10 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute},
	AccessTries:  3,
	ResetGrace:   time.Minute,
	CutoffWait:   2 * time.Minute,
	CutoffTries:  3,
	ReopenWait:   2 * time.Minute,
	StalledWait:  30 * time.Minute,
	StalledTries: 2,
	OverdueSlack: 10 * time.Minute,
	OverdueTries: 2,
	PromiseWait:  17 * time.Minute,
	PromiseTries: 2,
	Stale:        time.Hour,
}

// Live are the rules that act by default; the others run dry, recording what they would have sent. A rule is live
// when its replay acts unasked no more often than the owner's own nudges go to a session that would have gone on
// by itself (SPEC.md, Supervision).
var Live = map[string]bool{RuleTransient: true, RuleAccess: true}

// Decision is what supervision does about one session now.
type Decision struct {
	Rule     string
	Verdict  string
	At       time.Time // when the rule acts; for a hold, when it looks again
	RuleAt   time.Time // when the rule's own wait ends, whatever holds it: the stale hour counts from it
	Text     string
	Why      string
	Try      int  // the try a message is, from 1
	Stale    bool // the rule's time came longer ago than Params.Stale: it does not act
	thenCase bool // its time opens a case, not a message: the tries are used up, and only the wait tells
}

// Decide applies the guards and the rules to a stop at now. A zero Decision means no rule applies. A rule whose
// own time came more than Params.Stale ago does not act, held or not: momentum was not running then, or the stop
// is old, and the owner left it as it is.
func Decide(s Stop, tries Tries, p Params, probe Probe, now time.Time) Decision {
	d := Plan(s, tries, p, probe)
	if d.Verdict != Wait && d.Verdict != Hold {
		return d
	}
	if !d.RuleAt.IsZero() && p.Stale > 0 && now.Sub(d.RuleAt) > p.Stale {
		d.Verdict, d.Stale, d.Why = Skip, true, d.Why+"; stale: its time came at "+d.RuleAt.Local().Format("Jan 2 15:04")
		return d
	}
	if !d.At.IsZero() && !now.Before(d.At) {
		d.Verdict = Act
		if d.thenCase {
			d.Verdict, d.Why = Case, used(d.Rule, d.Try).Why
		}
	}
	return d
}

// Plan is what the guards and the rules make of a stop, regardless of the time: the first rule that applies,
// with the time it acts. The replay compares that time with what happened next.
func Plan(s Stop, tries Tries, p Params, probe Probe) Decision {
	if d, never := guard(s); never {
		return d
	}
	return planRules(s, tries, p, probe)
}

// ruleFuncs are the rules, in Rules order.
var ruleFuncs = map[string]func(Stop, Tries, Params, Probe) (Decision, bool){RuleTransient: transient, RuleAccess: access,
	RuleCutoff: cutoff, RuleReopened: reopened, RuleStalled: stalled, RuleOverdue: overdue, RulePromise: promise}

// planRules is the first rule that applies to a stop, past the guards, with the time it acts, held while the
// owner typed into the session recently.
func planRules(s Stop, tries Tries, p Params, probe Probe) Decision {
	return planWith(Rules, s, tries, p, probe)
}

// PlanOnly is Plan with one rule: the replay's stops whose turn went on to its reply are the stalled rule's alone.
func PlanOnly(rule string, s Stop, tries Tries, p Params, probe Probe) Decision {
	if d, never := guard(s); never {
		return d
	}
	return planWith([]string{rule}, s, tries, p, probe)
}

func planWith(rules []string, s Stop, tries Tries, p Params, probe Probe) Decision {
	for _, name := range rules {
		d, ok := ruleFuncs[name](s, tries, p, probe)
		if !ok {
			continue
		}
		if d.Verdict == Wait && !d.At.IsZero() && !s.OwnerAt.IsZero() {
			if until := s.OwnerAt.Add(p.OwnerWindow); until.After(d.At) {
				d.Verdict, d.At = Hold, until
				d.Why += "; its owner typed into it at " + s.OwnerAt.Local().Format("15:04")
			}
		}
		return d
	}
	return Decision{}
}

// guard is what no rule may do: act on an automated run, answer a session's question, plan or permission, or
// type into a Hermes conversation on a messaging platform, which has no terminal and would reach the owner's
// phone.
func guard(s Stop) (Decision, bool) {
	switch {
	case s.Auto:
		return Decision{Verdict: Skip, Why: "an automated run"}, true
	case s.Ask == sessions.AskQuestion || s.Ask == sessions.AskPlan || s.Ask == sessions.AskPermission:
		return Decision{Verdict: Skip, Why: "it asks its owner (" + string(s.Ask) + ")"}, true
	case s.Tool == "hermes" && s.Where != "cli" && s.Where != "tui":
		return Decision{Verdict: Skip, Why: "a Hermes conversation on " + firstNonEmpty(s.Where, "an unknown platform") + ": no terminal, and its reply would reach the owner"}, true
	}
	return Decision{}, false
}

func wait(rule string, at time.Time, try int, text, why string) Decision {
	return Decision{Rule: rule, Verdict: Wait, At: at, RuleAt: at, Try: try, Text: text, Why: why}
}

func used(rule string, n int) Decision {
	return Decision{Rule: rule, Verdict: Case, Try: n, Why: fmt.Sprintf("%d messages did not move it", n)}
}

// usedAt is a case that waits for the rule's own time: the stop it reads comes back with the last message (a
// stalled turn's message sends a request of its own), and only the wait tells that the message did not move it.
func usedAt(rule string, n int, at time.Time, why string) Decision {
	d := wait(rule, at, n, "", why+"; then a case: "+used(rule, n).Why)
	d.thenCase = true
	return d
}

// priorProcess reports a stop from before the session's process started: a process before this one stopped
// there, and the session has been opened again since. Only the reopened rule reads such a stop.
func priorProcess(s Stop) bool { return !s.Started.IsZero() && s.At.Before(s.Started) }

// transient: the turn ended on a transient API error. It tries again after each wait of the backoff, counted from
// the error that ended the last try, and opens a case when the waits run out.
func transient(s Stop, tries Tries, p Params, _ Probe) (Decision, bool) {
	if s.Working || priorProcess(s) || ErrorClass(s.Error) != Transient {
		return Decision{}, false
	}
	n := tries[RuleTransient]
	if n >= len(p.Backoff) {
		return used(RuleTransient, n), true
	}
	text := Marker + " Your last turn stopped on a transient API error. Continue where you left off."
	return wait(RuleTransient, s.At.Add(p.Backoff[n]), n+1, text, "the turn ended on a transient API error"), true
}

// access: the turn ended on a login that expired, a usage limit or an organization block. Trying again blindly
// meets the same wall, so it resumes once a probe shows access works again, or at the reset time a limit states.
func access(s Stop, tries Tries, p Params, probe Probe) (Decision, bool) {
	class := ErrorClass(s.Error)
	if s.Working || priorProcess(s) || class != Login && class != Limit && class != Org {
		return Decision{}, false
	}
	n := tries[RuleAccess]
	if n >= p.AccessTries {
		return used(RuleAccess, n), true
	}
	if class == Limit {
		if reset, ok := ResetAt(s.Error, s.At); ok {
			text := fmt.Sprintf("%s The usage limit reset at %s. Continue where you left off.", Marker, reset.Local().Format("15:04"))
			return wait(RuleAccess, reset.Add(p.ResetGrace), n+1, text, "a usage limit that resets at a stated time"), true
		}
	}
	why := map[string]string{Login: "the login expired", Limit: "a usage limit", Org: "the organization turned access off"}[class]
	if probe != nil {
		if back, ok := probe.AccessBack(class, s.At); ok {
			text := fmt.Sprintf("%s Access works again (%s before). Continue where you left off.", Marker, why)
			return wait(RuleAccess, back, n+1, text, why+"; access works again since "+back.Local().Format("15:04")), true
		}
	}
	return Decision{Rule: RuleAccess, Verdict: Wait, Try: n + 1, Why: why + "; waiting for access to work again"}, true
}

// cutoff: Hermes stopped because the model's reply hit its output limit, and says to send `continue`.
func cutoff(s Stop, tries Tries, p Params, _ Probe) (Decision, bool) {
	if s.Tool != "hermes" || s.Working || priorProcess(s) || !Cutoff(s.Words) {
		return Decision{}, false
	}
	n := tries[RuleCutoff]
	if n >= p.CutoffTries {
		return used(RuleCutoff, n), true
	}
	return wait(RuleCutoff, s.At.Add(p.CutoffWait), n+1, Marker+" continue", "the reply hit the model's output limit"), true
}

// reopened: the harness reopened the session after it ended mid-task (Claude Code's resume records), or Hermes
// closed a turn a restart cut short, and nothing has happened since. Once per reopening.
func reopened(s Stop, tries Tries, p Params, _ Probe) (Decision, bool) {
	at := s.ResumedAt
	switch {
	case !at.IsZero():
		// The turn may be open on the reopening's own notice, which no model answers until someone types: nothing
		// has happened since the reopening, or ResumedAt would be clear. act refuses a turn that runs on screen.
		if s.Busy || AsksOwner(s.Words) { // its words are the model's, from before the end
			return Decision{}, false
		}
	case s.Tool == "hermes" && !s.Working && CutShort(s.Words): // the words are Hermes's own notice
		at = s.At
	default:
		return Decision{}, false
	}
	if n := tries[RuleReopened]; n >= 1 {
		return used(RuleReopened, n), true
	}
	text := Marker + " This session was reopened after it ended mid-task. Check what the interrupted work left behind, then continue where you left off."
	return wait(RuleReopened, at.Add(p.ReopenWait), 1, text, "reopened after it ended mid-task"), true
}

// StalledText is the stalled rule's message. Nothing of the stall rides in it, so it is the same every time.
const StalledText = Marker + " Your last request to the model got no reply. Continue where you left off."

// stalled: a Claude Code turn waits on a model request that never got its reply: the last record of its
// conversation is the request, and no tool call is in flight. Background work does not answer the model: its
// notification waits in the queue behind the turn. Past the longest a reply legitimately takes to begin, counted
// from the request or from the machine waking after it, the message goes in as the next prompt, and Escape ends
// the turn that waits (act's Interrupt). A message its user queued waits for him. With its tries used up, a case
// opens once the last message's own request has waited as long.
func stalled(s Stop, tries Tries, p Params, _ Probe) (Decision, bool) {
	if s.Tool != "claude" || s.RequestAt.IsZero() || !s.Working || s.Busy || s.Queued > 0 ||
		!s.Started.IsZero() && s.RequestAt.Before(s.Started) {
		return Decision{}, false
	}
	n := tries[RuleStalled]
	from, why := afterSleep(s, s.RequestAt, p.StalledWait, "a model request with no reply since "+s.RequestAt.Local().Format("15:04"))
	if n >= p.StalledTries { // the last message's own request: a case once it too has waited
		return usedAt(RuleStalled, n, from.Add(p.StalledWait), why), true
	}
	return wait(RuleStalled, from.Add(p.StalledWait), n+1, StalledText, why), true
}

// afterSleep counts a rule's wait, which began at from, from the machine's wake instead when the machine slept
// before the wait ended and woke after it began: nothing could come while it slept. A suspend after the rule's time
// came does not start the wait again, so a stale stop stays stale.
func afterSleep(s Stop, from time.Time, wait time.Duration, why string) (time.Time, string) {
	if s.Woke.After(from) && s.Slept.Before(from.Add(wait)) {
		return s.Woke, why + "; the machine was suspended, and woke at " + s.Woke.Local().Format("15:04")
	}
	return from, why
}

// overdue: the turn is over, nothing is in flight, and a wakeup or a run of a job the session set itself came due
// long ago and never fired: past the latest time its harness fires it (WakeDue), counted from the end of the last
// turn when it came due while that turn ran (a run waits for the turn), or from the machine waking from a suspend.
// One due before the last turn began had its time then: the turn since does not start its wait again.
// Claude Code fires a run between turns, within a second when nothing holds it. An Esc at an idle prompt stops a
// self-paced loop and leaves no record, so a wakeup that did not fire counts only with a sign that it was lost: a
// task's notification or a compaction while it waited (WakeSign), or a suspend through its time. One resync per
// wakeup or run, quoting what it was for; a second lost one without an owner turn, then a case.
func overdue(s Stop, tries Tries, p Params, _ Probe) (Decision, bool) {
	if s.Tool != "claude" || s.WakeDue.IsZero() || s.Working || s.Error != "" || s.Tasks > 0 || priorProcess(s) || AsksOwner(s.Words) {
		return Decision{}, false
	}
	sign := s.WakeSign
	if sign == "" && s.sleptThrough(s.WakeDue) {
		sign = "the machine slept through its time"
	}
	if !s.WakeJob && sign == "" {
		return Decision{}, false
	}
	what := "its own wakeup"
	if s.WakeJob {
		what = "its scheduled job's run"
	}
	why := what + ", due by " + s.WakeDue.Local().Format("Jan 2 15:04") + ", did not fire"
	if sign != "" {
		why += "; " + sign
	}
	from, why := afterSleep(s, overdueFrom(s, s.WakeDue), p.OverdueSlack, why)
	n := tries[RuleOverdue]
	if n >= p.OverdueTries { // the turn the last message opened has just ended: a case once it too has waited
		return usedAt(RuleOverdue, n, from.Add(p.OverdueSlack), why), true
	}
	return wait(RuleOverdue, from.Add(p.OverdueSlack), n+1, OverdueText(s, n+1), why), true
}

// turnGrace is how far apart a turn's start and a run's time may be and still be one moment: the records' clocks.
const turnGrace = 5 * time.Second

// overdueFrom is when a run due by due should have started: then, or when the last turn ended if it came due
// while that turn ran (a run waits for the turn). One due before the last turn began had its time then.
func overdueFrom(s Stop, due time.Time) time.Time {
	if s.IdleAt.After(due) && !s.TurnBegan.After(due.Add(turnGrace)) {
		return s.IdleAt
	}
	return due
}

// sleptThrough reports whether the machine's last suspend spans t.
func (s Stop) sleptThrough(t time.Time) bool {
	return !s.Slept.IsZero() && s.Slept.Before(t) && s.Woke.After(t)
}

// Choose sets the run the overdue rule reads: the earliest of the stop's runs that no resync went in for (handled),
// that the rule would act on (a wakeup with a sign that it was lost), and whose time, counted as the rule counts
// it, is not past the stale hour at now. A later run of a job that keeps missing them is its own.
func (s *Stop) Choose(handled func(key string) bool, now time.Time, p Params) {
	for _, r := range s.Runs {
		if handled(r.Key) || !r.Job && r.Sign == "" && !s.sleptThrough(r.Due) {
			continue
		}
		if from, _ := afterSleep(*s, overdueFrom(*s, r.Due), p.OverdueSlack, ""); from.Add(p.OverdueSlack + p.Stale).Before(now) {
			continue
		}
		s.WakeDue, s.WakeFor, s.WakeKey, s.WakeJob, s.WakeOnce, s.WakeSign = r.Due, r.For, r.Key, r.Job, r.Once, r.Sign
		return
	}
}

// OverdueText is the overdue rule's message for the stop's run and its try: what did not run, when it was due,
// and what it was for, quoted. Claude Code cancels a self-paced loop's wakeup on its user's Esc and leaves no
// record, so the loop goes on only if it is still wanted, and when the conversation does not say, the session asks
// its user (Claude Code's own words for a wakeup a restart lost). A job keeps its schedule; one whose runs keep
// not starting is made again.
func OverdueText(s Stop, try int) string {
	when := s.WakeDue.Local().Format("Jan 2 15:04")
	what := clip(s.WakeFor, 140)
	quoted := ""
	if what != "" {
		quoted = fmt.Sprintf(", so nothing ran %q", what)
	}
	switch {
	case s.WakeOnce:
		return fmt.Sprintf("%s Your one-shot task, due by %s, did not start%s. Do it now.", Marker, when, quoted)
	case s.WakeJob && try > 1:
		return fmt.Sprintf("%s Your scheduled job's run, due by %s, did not start either%s. Do it now, and make the job again if it is still wanted: its runs are not starting.", Marker, when, quoted)
	case s.WakeJob:
		return fmt.Sprintf("%s Your scheduled job's run, due by %s, did not start%s. Do it now; the job keeps its schedule.", Marker, when, quoted)
	}
	if what != "" {
		quoted = fmt.Sprintf(", so nothing woke you for %q", what)
	}
	return fmt.Sprintf("%s Your own wakeup, due by %s, did not fire%s. If the loop is still wanted, do it now and set the next one; when the conversation does not make that clear, ask your user first.", Marker, when, quoted)
}

// promise: the turn ended, nothing is in flight or scheduled to wake it, and its last words promise to come back
// (report, check, wait for something) or announce a step it did not take.
func promise(s Stop, tries Tries, p Params, _ Probe) (Decision, bool) {
	// Hermes runs background processes the session layer does not track, so an idle Hermes may not be idle.
	if s.Tool == "hermes" || priorProcess(s) || s.Phase != sessions.Idle || s.Working || s.Error != "" || s.Tasks > 0 || !s.WakeAt.IsZero() || s.Loop != "" {
		return Decision{}, false
	}
	quote := Promise(s.Words)
	if quote == "" {
		return Decision{}, false
	}
	n := tries[RulePromise]
	if n >= p.PromiseTries {
		return used(RulePromise, n), true
	}
	text := fmt.Sprintf("%s Your last turn ended with %q, but nothing is set to wake you: no background task, wakeup or loop. Do it now, or set one up.",
		Marker, clip(quote, 140))
	return wait(RulePromise, s.At.Add(p.PromiseWait), n+1, text, "the turn ended on a promise and nothing will wake it"), true
}

// clip folds whitespace and cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
