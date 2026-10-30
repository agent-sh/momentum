package supervise

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
)

var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

const (
	err503   = "API Error: 503 Bedrock is unable to process your request. This is a server-side issue, usually temporary."
	errLogin = "Login expired · Please run /login"
)

// probeAt says access came back at t.
type probeAt time.Time

func (p probeAt) AccessBack(_ string, since time.Time) (time.Time, bool) {
	if t := time.Time(p); t.After(since) {
		return t, true
	}
	return time.Time{}, false
}

func stopped(f func(*Stop)) Stop {
	s := Stop{Key: "s1", Tool: "claude", Where: "work:1.1", Phase: sessions.Idle, At: t0}
	f(&s)
	return s
}

// check runs one rule's cases: the verdict, the time it acts, the try, and words the text holds.
type ruleCase struct {
	name    string
	stop    Stop
	tries   Tries
	probe   Probe
	verdict string // "" for a rule that does not apply
	at      time.Time
	try     int
	text    string
}

func check(t *testing.T, rule func(Stop, Tries, Params, Probe) (Decision, bool), cases []ruleCase) {
	t.Helper()
	for _, c := range cases {
		d, ok := rule(c.stop, c.tries, DefaultParams, c.probe)
		if c.verdict == "" {
			if ok {
				t.Errorf("%s: applies: %+v", c.name, d)
			}
			continue
		}
		if !ok || d.Verdict != c.verdict || !d.At.Equal(c.at) || d.Try != c.try || !strings.Contains(d.Text, c.text) {
			t.Errorf("%s: got %v %+v, want %s at %v try %d text with %q", c.name, ok, d, c.verdict, c.at, c.try, c.text)
		}
	}
}

func TestTransientTriesAgainWithABackoff(t *testing.T) {
	errStop := stopped(func(s *Stop) { s.Phase, s.Error = sessions.NeedsYou, err503 })
	check(t, transient, []ruleCase{
		{"first try", errStop, Tries{}, nil, Wait, t0.Add(10 * time.Minute), 1, "Continue where you left off"},
		{"second try, counted from the error that ended the first", errStop, Tries{RuleTransient: 1}, nil, Wait, t0.Add(4 * time.Minute), 2, Marker},
		{"fourth try", errStop, Tries{RuleTransient: 3}, nil, Wait, t0.Add(16 * time.Minute), 4, ""},
		{"tries used up", errStop, Tries{RuleTransient: 4}, nil, Case, time.Time{}, 4, ""},
		{"background work in flight does not stop it", stopped(func(s *Stop) { s.Phase, s.Error, s.Tasks = sessions.Background, err503, 2 }), Tries{}, nil, Wait, t0.Add(10 * time.Minute), 1, ""},
		{"a refusal is never tried again", stopped(func(s *Stop) { s.Error = "API Error: the safeguards flagged this message" }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a login error is not transient", stopped(func(s *Stop) { s.Error = errLogin }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a turn running", stopped(func(s *Stop) { s.Error, s.Working = err503, true }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"no error", stopped(func(s *Stop) {}), Tries{}, nil, "", time.Time{}, 0, ""},
	})
}

func TestAccessResumesOnceAccessIsBack(t *testing.T) {
	login := stopped(func(s *Stop) { s.Phase, s.Error = sessions.NeedsYou, errLogin })
	lis, _ := time.LoadLocation("Europe/Lisbon")
	limit := stopped(func(s *Stop) {
		s.Phase, s.Error, s.At = sessions.NeedsYou, "You've hit your weekly limit · resets Mar 7, 9am (Europe/Lisbon)", time.Date(2026, 3, 4, 9, 0, 0, 0, lis)
	})
	check(t, access, []ruleCase{
		{"login, access back", login, Tries{}, probeAt(t0.Add(3 * time.Hour)), Wait, t0.Add(3 * time.Hour), 1, "Access works again"},
		{"login, access not back", login, Tries{}, probeAt(time.Time{}), Wait, time.Time{}, 1, ""},
		{"a sign from before the error is no sign", login, Tries{}, probeAt(t0.Add(-time.Minute)), Wait, time.Time{}, 1, ""},
		{"a limit resets at the time it states", limit, Tries{}, nil, Wait, time.Date(2026, 3, 7, 9, 1, 0, 0, lis), 1,
			"reset at " + time.Date(2026, 3, 7, 9, 0, 0, 0, lis).Local().Format("15:04")},
		{"organization block", stopped(func(s *Stop) { s.Error = "Your organization has disabled Claude subscription access for Claude Code" }), Tries{}, probeAt(t0.Add(time.Hour)), Wait, t0.Add(time.Hour), 1, ""},
		{"tries used up", login, Tries{RuleAccess: 3}, probeAt(t0.Add(time.Hour)), Case, time.Time{}, 3, ""},
		{"a transient error is not an access error", stopped(func(s *Stop) { s.Error = err503 }), Tries{}, probeAt(t0.Add(time.Hour)), "", time.Time{}, 0, ""},
	})
}

func TestCutoffContinuesAHermesReplyCutAtItsLimit(t *testing.T) {
	cut := "The model's reply was cut off before it finished (it hit its output length limit), so Hermes didn't run the incomplete action. Nothing was changed. Send `continue`, ask for the work in smaller steps, or raise max_tokens for this model."
	hermes := stopped(func(s *Stop) { s.Tool, s.Where, s.Words = "hermes", "cli", Words(cut) })
	check(t, cutoff, []ruleCase{
		{"hermes", hermes, Tries{}, nil, Wait, t0.Add(2 * time.Minute), 1, Marker + " continue"},
		{"tries used up", hermes, Tries{RuleCutoff: 3}, nil, Case, time.Time{}, 3, ""},
		{"not hermes", stopped(func(s *Stop) { s.Words = Words(cut) }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a hermes turn running", stopped(func(s *Stop) { s.Tool, s.Words, s.Working = "hermes", Words(cut), true }), Tries{}, nil, "", time.Time{}, 0, ""},
	})
}

func TestReopenedResumesASessionThatEndedMidTask(t *testing.T) {
	back := t0.Add(time.Hour)
	reopened1 := stopped(func(s *Stop) { s.ResumedAt, s.Words = back, "Deploying the build to staging." })
	check(t, reopened, []ruleCase{
		{"reopened", reopened1, Tries{}, nil, Wait, back.Add(2 * time.Minute), 1, "reopened after it ended mid-task"},
		{"its turn is open on the reopening's notice", stopped(func(s *Stop) { s.ResumedAt, s.Working, s.Phase = back, true, sessions.Working }), Tries{}, nil, Wait, back.Add(2 * time.Minute), 1, ""},
		{"once per reopening", reopened1, Tries{RuleReopened: 1}, nil, Case, time.Time{}, 1, ""},
		{"a call running", stopped(func(s *Stop) { s.ResumedAt, s.Busy = back, true }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"its last words asked the owner", stopped(func(s *Stop) { s.ResumedAt, s.Words = back, "Merge it now?" }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"hermes closed a turn a restart cut short", stopped(func(s *Stop) {
			s.Tool, s.Where, s.Words = "hermes", "cli", "Your request was not processed. Send it again if you still want me to carry it out."
		}), Tries{}, nil, Wait, t0.Add(2 * time.Minute), 1, ""},
		{"not reopened", stopped(func(s *Stop) {}), Tries{}, nil, "", time.Time{}, 0, ""},
	})
}

func TestPromiseResyncsATurnThatLeftWorkWithNothingToWakeIt(t *testing.T) {
	said := "The import is at row 9000. I'll report when the three tables are loaded."
	idle := stopped(func(s *Stop) { s.Words = Words(said) })
	check(t, promise, []ruleCase{
		{"a promise and nothing to wake it", idle, Tries{}, nil, Wait, t0.Add(17 * time.Minute), 1, `"I'll report when the three tables are loaded."`},
		{"twice without its owner", idle, Tries{RulePromise: 2}, nil, Case, time.Time{}, 2, ""},
		{"a task in flight wakes it", stopped(func(s *Stop) { s.Words, s.Tasks, s.Phase = Words(said), 1, sessions.Background }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a wakeup", stopped(func(s *Stop) { s.Words, s.WakeAt, s.Phase = Words(said), t0.Add(time.Hour), sessions.Scheduled }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"a loop", stopped(func(s *Stop) { s.Words, s.Loop, s.Phase = Words(said), "*/5 * * * *", sessions.Scheduled }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"it asks its owner", stopped(func(s *Stop) { s.Words = Words("Ready. Should I merge it?") }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"it is done", stopped(func(s *Stop) { s.Words = Words("Merged and released. Nothing else is left.") }), Tries{}, nil, "", time.Time{}, 0, ""},
		{"hermes runs processes nobody tracks", stopped(func(s *Stop) { s.Tool, s.Words = "hermes", Words(said) }), Tries{}, nil, "", time.Time{}, 0, ""},
	})
}

func TestGuardsKeepRulesOffWhatIsNotMomentums(t *testing.T) {
	errStop := func(f func(*Stop)) Stop {
		return stopped(func(s *Stop) { s.Phase, s.Error = sessions.NeedsYou, err503; f(s) })
	}
	cases := []struct {
		name string
		stop Stop
		skip bool
	}{
		{"an automated run", errStop(func(s *Stop) { s.Auto = true }), true},
		{"a permission prompt", errStop(func(s *Stop) { s.Ask = sessions.AskPermission }), true},
		{"a question", errStop(func(s *Stop) { s.Ask = sessions.AskQuestion }), true},
		{"a plan", errStop(func(s *Stop) { s.Ask = sessions.AskPlan }), true},
		{"a hermes conversation on the phone", errStop(func(s *Stop) { s.Tool, s.Where = "hermes", "mobile" }), true},
		{"a hermes conversation in a terminal", errStop(func(s *Stop) { s.Tool, s.Where = "hermes", "cli" }), false},
		{"a claude session in tmux", errStop(func(s *Stop) {}), false},
	}
	for _, c := range cases {
		d := Plan(c.stop, Tries{}, DefaultParams, nil)
		if (d.Verdict == Skip) != c.skip {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

func TestTheOwnersRecentTurnHoldsARule(t *testing.T) {
	s := stopped(func(s *Stop) { s.Phase, s.Error, s.OwnerAt = sessions.NeedsYou, err503, t0.Add(-10*time.Minute) })
	d := Plan(s, Tries{}, DefaultParams, nil)
	if d.Verdict != Wait || !d.At.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("his window ends before the rule acts: %+v", d)
	}
	s.OwnerAt = t0.Add(-time.Minute) // the turn he typed ended on the error
	if d := Plan(s, Tries{}, DefaultParams, nil); d.Verdict != Hold || !d.At.Equal(t0.Add(14*time.Minute)) {
		t.Fatalf("his window ends after: %+v", d)
	}
	s.OwnerAt = t0.Add(5 * time.Minute) // he typed after it stopped, and is there
	d = Plan(s, Tries{}, DefaultParams, nil)
	if d.Verdict != Hold || !d.At.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("typed recently: %+v", d)
	}
	if d := Decide(s, Tries{}, DefaultParams, nil, t0.Add(15*time.Minute)); d.Verdict != Hold {
		t.Fatalf("still held: %+v", d)
	}
	if d := Decide(s, Tries{}, DefaultParams, nil, t0.Add(20*time.Minute)); d.Verdict != Act {
		t.Fatalf("the hold ended: %+v", d)
	}
}

func TestDecideActsOnceTheWaitIsOver(t *testing.T) {
	s := stopped(func(s *Stop) { s.Phase, s.Error = sessions.NeedsYou, err503 })
	if d := Decide(s, Tries{}, DefaultParams, nil, t0.Add(9*time.Minute)); d.Verdict != Wait {
		t.Fatalf("before: %+v", d)
	}
	if d := Decide(s, Tries{}, DefaultParams, nil, t0.Add(10*time.Minute)); d.Verdict != Act || d.Rule != RuleTransient {
		t.Fatalf("after: %+v", d)
	}
	if d := Decide(stopped(func(s *Stop) {}), Tries{}, DefaultParams, nil, t0.Add(time.Hour)); d.Rule != "" || d.Verdict != "" {
		t.Fatalf("no rule: %+v", d)
	}
}
