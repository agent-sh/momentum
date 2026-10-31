package supervise

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions/act"
)

// idling is a sender for a pane whose screen shows no turn running while the transcript still waits on the
// request: Claude Code ended the turn without a record, as an Escape with nothing queued does. A message typed
// there is what Claude Code writes for a prompt typed at its input line.
type idling struct {
	fakeSender
	x   *transcript
	now *time.Time
}

func (f *idling) CheckInterrupt(context.Context, act.Target) (act.Path, error) {
	f.checks++
	return act.Tmux, act.ErrIdle
}

func (f *idling) Interrupt(context.Context, act.Target, string, func() error) (act.Path, error) {
	return act.Tmux, act.ErrIdle // no turn runs to hold the message in its queue: nothing typed
}

func (f *idling) Send(ctx context.Context, t act.Target, text string) (act.Path, error) {
	if _, err := f.fakeSender.Send(ctx, t, text); err != nil {
		return act.Tmux, err
	}
	f.x.at = *f.now
	f.x.user(text)
	return act.Tmux, nil
}

// The loop's stall as its owner met it: the turn waits on a request that never got a reply, and nobody is at the
// session. As the rules ship, the stalled rule resumes it at its time on either screen he may find there: through
// the queue and Escape while the turn runs on it, as the next prompt typed the plain way when it shows no turn
// running, once the owner window has passed since the first look that found it so. Each send is a decided note
// that says what it did and where; no needs note asks him anything.
func TestTheLoopsStallIsResumedOnEitherScreen(t *testing.T) {
	for _, c := range []struct {
		name, screen string
		live         map[string]bool
	}{
		{"running", "running", Live}, // as it ships
		{"idle", "idle", Live},
	} {
		t.Run(c.name, func(t *testing.T) { stallResumed(t, c.screen, c.live) })
	}
}

// stallResumed runs the loop's stall to its time and the scan after, on one screen with one set of live rules.
func stallResumed(t *testing.T, screen string, live map[string]bool) {
	x, sent, scan := stalledLoop(t)
	st := testStore(t)
	now := sent.Add(29 * time.Minute)
	var sv *Supervisor
	var typed func() []string
	if screen == "running" {
		f := &queueing{x: x, now: &now}
		sv, typed = supervisor(st, &f.fakeSender, &now), func() []string { return f.sent }
		sv.Sender = f
	} else {
		f := &idling{x: x, now: &now}
		sv, typed = supervisor(st, &f.fakeSender, &now), func() []string { return f.sent }
		sv.Sender = f
	}
	sv.Live = live
	sv.Sleep = func(context.Context) (time.Time, time.Time) { return time.Time{}, time.Time{} }
	run(t, sv, scan(now))
	if len(typed()) != 0 {
		t.Fatalf("before its time: %q", typed())
	}
	now = sent.Add(30 * time.Minute)
	run(t, sv, scan(now))
	if screen == "idle" { // the first look at the idle screen holds the owner window: an Escape leaves no record
		if len(typed()) != 0 {
			t.Fatalf("at the first look at the idle screen: %q", typed())
		}
		now = now.Add(DefaultParams.OwnerWindow + time.Second)
		run(t, sv, scan(now))
	}
	want := "%9 " + StalledText
	if screen == "running" {
		want = "%9 interrupt " + StalledText
	}
	if got := typed(); len(got) != 1 || got[0] != want {
		t.Fatalf("at its time: sent %q", got)
	}
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["sent"] != true || (metaMap(acts[0])["interrupt"] == true) != (screen == "running") {
		t.Fatalf("its event: %+v", acts)
	}
	ns, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var decided []store.Note
	for _, n := range ns {
		switch {
		case n.Kind == store.NoteDecided:
			decided = append(decided, n)
		case n.Kind == store.NoteNeeds:
			t.Fatalf("a needs note: %+v", n)
		}
	}
	how := map[string]string{"running": "pressed Escape", "idle": "as its next prompt"}[screen]
	if len(decided) != 1 || !strings.HasPrefix(decided[0].Title, "Resumed lab: ") || !strings.Contains(decided[0].Title, how) ||
		!strings.Contains(decided[0].Detail, "work:2.1") || !strings.Contains(decided[0].Detail, "supervise.live") {
		t.Fatalf("decided notes %+v", decided)
	}
	// The next scan: the message opened the next turn.
	now = now.Add(time.Minute)
	s := scan(now)
	if s.RequestAt.Equal(sent) || !s.TurnOpen {
		t.Fatalf("after the message: request %v open %v", s.RequestAt, s.TurnOpen)
	}
	run(t, sv, s)
	if r := results(t, st); !r[Delivered] || len(typed()) != 1 {
		t.Fatalf("outcomes %v, sent %q", r, typed())
	}
}
