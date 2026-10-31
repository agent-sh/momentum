package supervise

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/act"
)

// fakeSender records what it is asked to send. check is what the terminal check returns, send what sending does,
// interrupt what an interrupt does.
type fakeSender struct {
	sent                                   []string
	check, send, checkInterrupt, interrupt error
	checks                                 int
}

func (f *fakeSender) CheckInterrupt(_ context.Context, t act.Target) (act.Path, error) {
	f.checks++
	return act.Tmux, f.checkInterrupt
}

func (f *fakeSender) Interrupt(_ context.Context, t act.Target, text string, still func() error) (act.Path, error) {
	if f.checkInterrupt != nil {
		return act.Tmux, f.checkInterrupt
	}
	if err := still(); err != nil {
		return act.Tmux, err
	}
	f.sent = append(f.sent, t.Pane+" interrupt "+text)
	return act.Tmux, f.interrupt
}

func (f *fakeSender) Check(_ context.Context, t act.Target) (act.Path, error) {
	f.checks++
	return act.Tmux, f.check
}

func (f *fakeSender) Send(_ context.Context, t act.Target, text string) (act.Path, error) {
	if f.check != nil {
		return act.Tmux, f.check
	}
	f.sent = append(f.sent, t.Pane+" "+text)
	return act.Tmux, f.send
}

func testStore(t *testing.T) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "momentum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func supervisor(st *store.Store, f *fakeSender, now *time.Time) *Supervisor {
	return &Supervisor{Store: st, Sender: f, Params: DefaultParams, Live: map[string]bool{RuleTransient: true},
		Probe: probeAt(time.Time{}), Now: func() time.Time { return *now }}
}

func run(t *testing.T, sv *Supervisor, ss ...*sessions.Session) {
	t.Helper()
	if _, err := sv.Run(context.Background(), ss, map[int]uint64{42: 9}); err != nil {
		t.Fatal(err)
	}
}

func events(t *testing.T, st *store.Store, kinds ...string) []store.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.Query{Source: store.Supervise, Since: time.Time{}, Kinds: kinds, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func metaMap(e store.Event) map[string]any {
	m := map[string]any{}
	json.Unmarshal([]byte(e.Meta), &m)
	return m
}

func results(t *testing.T, st *store.Store) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range events(t, st, KindOutcome) {
		out[metaMap(e)["result"].(string)] = true
	}
	return out
}

func note(t *testing.T, st *store.Store, id string) (store.Note, bool) {
	t.Helper()
	ns, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.ID == id {
			return n, true
		}
	}
	return store.Note{}, false
}

// erroring is a Claude Code session in a pane whose turn ended on a transient API error at t0.
func erroring() *sessions.Session {
	s := &sessions.Session{Key: "sess-1", Tool: "claude", Name: "build", Where: "work:1.1", Pane: "%7", PID: 42,
		Phase: sessions.NeedsYou, Since: t0, AskKind: sessions.AskError, Ask: "stopped on an error: " + err503, LastText: err503,
		Tasks: []sessions.Task{}, Log: &sessions.Log{}}
	s.Log.Add(sessions.Entry{Role: sessions.RoleUser, Text: "build the release notes"})
	s.Log.Add(sessions.Entry{Role: sessions.RoleAssistant, Text: err503})
	s.Seq = s.Log.Seq()
	return s
}

// lands is a message showing up as the session's turn at, and the turn it opened doing tool calls when work is set.
func lands(s *sessions.Session, text string, at time.Time, work bool) {
	s.Log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text})
	if work {
		s.Log.Add(sessions.Entry{Role: sessions.RoleTool, Name: "Bash", Text: "make notes"})
	}
	s.UserAt, s.Seq = at, s.Log.Seq()
}

// typed is the owner typing into the session at.
func typed(s *sessions.Session, text string, at time.Time) {
	s.Log.Add(sessions.Entry{Role: sessions.RoleUser, Text: text})
	s.UserAt, s.Seq = at, s.Log.Seq()
}

func TestATransientErrorWaitsThenSendsThenFollowsTheOutcome(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(5 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()

	run(t, sv, s)
	if len(f.sent) != 0 || len(events(t, st, KindWait)) != 1 {
		t.Fatalf("before its time: sent %q, events %+v", f.sent, events(t, st))
	}
	now = t0.Add(10 * time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 || !strings.HasPrefix(f.sent[0], "%7 "+Marker) {
		t.Fatalf("at its time: sent %q", f.sent)
	}
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["sent"] != true || metaMap(acts[0])["sending"] != nil || acts[0].Actor != store.ActorAgent ||
		acts[0].Experiment != "" {
		t.Fatalf("its event: %+v", acts)
	}
	text := metaMap(acts[0])["text"].(string)
	// The next scan: it has not shown up yet, so nothing is sent again.
	now = now.Add(time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 {
		t.Fatalf("sent again before it landed: %q", f.sent)
	}
	// It landed, and the turn it opened ended on the same error again.
	lands(s, text, t0.Add(10*time.Minute+2*time.Second), false)
	s.Since = t0.Add(16 * time.Minute)
	now = t0.Add(17 * time.Minute)
	run(t, sv, s)
	if r := results(t, st); !r[Delivered] || !r[FailedAgain] || r[Worked] {
		t.Fatalf("outcomes %v", r)
	}
	// The second try waits 4 minutes after the error that ended the first; momentum's own message is not the
	// owner's turn, so nothing holds it.
	now = t0.Add(20 * time.Minute)
	run(t, sv, s)
	if len(f.sent) != 2 || !strings.Contains(f.sent[1], "API error") {
		t.Fatalf("second try: %q", f.sent)
	}
	tries := map[float64]bool{}
	for _, e := range events(t, st, KindAct) {
		if m := metaMap(e); m["sent"] == true {
			tries[m["try"].(float64)] = true
		}
	}
	if len(tries) != 2 || !tries[1] || !tries[2] {
		t.Fatalf("each try its own event: %+v", events(t, st, KindAct))
	}
	if ns, _ := st.Notes(context.Background()); len(ns) != 0 { // a decided note is the stalled rule's alone
		t.Fatalf("notes %+v", ns)
	}
}

func TestAMessageThatWorkedResetsTheTries(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	run(t, sv, s)
	lands(s, metaMap(events(t, st, KindAct)[0])["text"].(string), now.Add(time.Second), true)
	s.Phase, s.AskKind, s.Ask, s.LastText, s.Since = sessions.Idle, "", "", "The notes are written.", t0.Add(20*time.Minute)
	now = t0.Add(21 * time.Minute)
	run(t, sv, s)
	if r := results(t, st); !r[Worked] {
		t.Fatalf("outcomes %v", r)
	}
	// A later error starts over at the first try.
	s.Phase, s.AskKind, s.Ask, s.LastText, s.Since = sessions.NeedsYou, sessions.AskError, "stopped on an error: "+err503, err503, t0.Add(40*time.Minute)
	now = t0.Add(50 * time.Minute)
	run(t, sv, s)
	if len(f.sent) != 2 {
		t.Fatalf("sent %q", f.sent)
	}
	for _, e := range events(t, st, KindAct) {
		if m := metaMap(e); m["sent"] == true && m["try"].(float64) != 1 {
			t.Fatalf("a try after one that worked: %+v", m)
		}
	}
}

func TestADryRuleRecordsWhatItWouldHaveSent(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(20 * time.Minute)
	sv := supervisor(st, f, &now)
	s := &sessions.Session{Key: "sess-2", Tool: "claude", Pane: "%8", PID: 43, Phase: sessions.Idle, Since: t0,
		LastText: "The import is running. I'll report when it finishes.", Tasks: []sessions.Task{}}
	for i := 0; i < 2; i++ { // the same decision on the next run is the same event
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	acts := events(t, st, KindAct)
	if len(f.sent) != 0 || f.checks != 0 || len(acts) != 1 || acts[0].Experiment != Dry || metaMap(acts[0])["dry"] != true ||
		!strings.Contains(acts[0].Summary, "would send") {
		t.Fatalf("sent %q, events %+v", f.sent, acts)
	}
}

func TestNeverForAQuestionAndHoldWhileTheOwnerIsThere(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(30 * time.Minute)
	sv := supervisor(st, f, &now)
	asks := erroring()
	asks.Key, asks.AskKind, asks.Ask = "sess-3", sessions.AskPermission, "Do you want to proceed?"
	auto := erroring()
	auto.Key, auto.Auto = "sess-5", true
	present := erroring()
	present.Key = "sess-4"
	typed(present, "and the changelog too", t0.Add(25*time.Minute))
	opened := erroring() // he opened it again, and it stopped on the error right after
	opened.Key, opened.Started, opened.Since = "sess-6", t0.Add(24*time.Minute), t0.Add(24*time.Minute+time.Second)
	run(t, sv, asks, auto, present, opened)
	if len(f.sent) != 0 {
		t.Fatalf("sent %q", f.sent)
	}
	skips, holds := events(t, st, KindSkip), events(t, st, KindHold)
	got := map[string]string{}
	for _, e := range append(skips, holds...) {
		got[e.Session] = e.Kind
	}
	if len(got) != 3 || got["sess-5"] != KindSkip || got["sess-4"] != KindHold || got["sess-6"] != KindHold {
		t.Fatalf("skips %+v holds %+v", skips, holds)
	}
}

func TestAStopFromAnEarlierProcessOrLongAgoIsLeftAlone(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(26 * time.Hour)
	sv := supervisor(st, f, &now)
	// He reopened a session that ended on an error yesterday: the error is the earlier process's.
	reopened := erroring()
	reopened.Started = now.Add(-30 * time.Second)
	// The error came while nothing was watching, a day ago: its time came and went.
	old := erroring()
	old.Key = "sess-8"
	run(t, sv, reopened, old)
	if len(f.sent) != 0 || f.checks != 0 {
		t.Fatalf("sent %q", f.sent)
	}
	skips := events(t, st, KindSkip)
	if len(skips) != 1 || skips[0].Session != "sess-8" || !strings.Contains(skips[0].Summary, "stale") {
		t.Fatalf("skips %+v", skips)
	}
	// Reopened 14 minutes after it stopped, and left alone past the owner window: still the earlier process's.
	now = t0.Add(30 * time.Minute)
	recent := erroring()
	recent.Key, recent.Started = "sess-12", t0.Add(14*time.Minute)
	run(t, sv, recent)
	if len(f.sent) != 0 || f.checks != 0 {
		t.Fatalf("sent %q", f.sent)
	}
}

func TestAProblemNoteIsDoneOnceTheSessionWorksAgain(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{check: act.ErrNoPath}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	run(t, sv, s)
	if n, ok := note(t, st, ProblemNote+s.Key); !ok || !n.Done.IsZero() {
		t.Fatalf("problem note %+v %v", n, ok)
	}
	// Between two runs it took a turn, made calls and stopped without an error.
	s.Phase, s.AskKind, s.Ask, s.LastText, s.Since = sessions.Idle, "", "", "The notes are written.", now.Add(30*time.Second)
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, ProblemNote+s.Key); n.Done.IsZero() {
		t.Fatalf("still stands: %+v", n)
	}
}

func TestAnotherHarnessesOpenTurnWorksAgain(t *testing.T) {
	// A Codex session's case: its problem note ends once a turn is open again, a call in flight or not; only
	// Claude Code's open turn may wait on a model request with no reply.
	st := testStore(t)
	f := &fakeSender{check: act.ErrNoPath}
	now := t0.Add(11 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	s.Tool = "codex"
	run(t, sv, s)
	if n, ok := note(t, st, ProblemNote+s.Key); !ok || !n.Done.IsZero() {
		t.Fatalf("problem note %+v %v", n, ok)
	}
	s.TurnOpen, s.Now, s.AskKind, s.Ask = true, "", "", ""
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, ProblemNote+s.Key); n.Done.IsZero() {
		t.Fatalf("a Codex turn open again left its note standing: %+v", n)
	}
}

func TestARefusalIsRecordedOnceAndCheckedAgain(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{check: act.ErrBusy}
	now := t0.Add(11 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	acts := events(t, st, KindAct)
	if len(acts) != 1 || metaMap(acts[0])["sent"] == true || metaMap(acts[0])["sending"] != nil || metaMap(acts[0])["present"] != nil ||
		f.checks != 3 || len(events(t, st, KindCase)) != 0 {
		t.Fatalf("refused: checks %d, %+v", f.checks, acts)
	}
	f.check = nil
	run(t, sv, s)
	if len(f.sent) != 1 {
		t.Fatalf("checked again: %q", f.sent)
	}
}

func TestADraftOnTheInputLineIsTheOwnerAtThePane(t *testing.T) {
	for _, refusal := range []error{act.ErrDraft, act.ErrMode} {
		st := testStore(t)
		f := &fakeSender{check: refusal}
		now := t0.Add(11 * time.Minute)
		sv := supervisor(st, f, &now)
		s := erroring()
		for i := 0; i < 3; i++ {
			run(t, sv, s)
			now = now.Add(time.Minute)
		}
		if f.checks != 1 || len(events(t, st, KindHold)) != 1 || len(events(t, st, KindPresent)) != 1 {
			t.Fatalf("%v: checks %d, holds %+v, present %+v", refusal, f.checks, events(t, st, KindHold), events(t, st, KindPresent))
		}
		// He clears it: the check after the hold finds the line empty, which is him there just now.
		f.check = nil
		now = t0.Add(11*time.Minute + DefaultParams.OwnerWindow + time.Second)
		run(t, sv, s)
		if len(f.sent) != 0 || metaMap(events(t, st, KindPresent)[0])["state"] != "cleared" {
			t.Fatalf("%v: typed as he cleared it: %q", refusal, f.sent)
		}
		now = now.Add(DefaultParams.OwnerWindow + time.Second)
		run(t, sv, s)
		if len(f.sent) != 1 {
			t.Fatalf("%v: after the window: %q", refusal, f.sent)
		}
	}
}

func TestADraftThatStaysEndsInANeedsNote(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{check: act.ErrDraft}
	now := t0.Add(11 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	for i := 0; i < 120; i++ { // two hours, a run a minute
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	stale := false
	for _, e := range events(t, st, KindSkip) {
		stale = stale || strings.Contains(e.Summary, "stale")
	}
	n, ok := note(t, st, AskNote+s.Key)
	if len(f.sent) != 0 || len(events(t, st, KindAct)) != 1 || !stale || !ok || !strings.HasPrefix(n.Title, "It stopped on an error") {
		t.Fatalf("sent %d, acts %d, stale %v, note %+v %v", len(f.sent), len(events(t, st, KindAct)), stale, n, ok)
	}
	// Each check that met the draft moved its time: the hold ran from the last one, not the first.
	if p := events(t, st, KindPresent); len(p) != 1 || !p[0].TS.After(t0.Add(11*time.Minute+DefaultParams.OwnerWindow)) || f.checks < 3 {
		t.Fatalf("present %+v, checks %d", p, f.checks)
	}
}

// changing is a sender whose checks return one refusal after another.
type changing struct {
	fakeSender
	refusals []error
}

func (f *changing) Check(ctx context.Context, t act.Target) (act.Path, error) {
	f.checks++
	if len(f.refusals) == 0 {
		return act.Tmux, nil
	}
	err := f.refusals[0]
	f.refusals = f.refusals[1:]
	return act.Tmux, err
}

func TestASecondRefusalStillCounts(t *testing.T) {
	// A running turn at the first check, then his draft: the draft holds.
	st := testStore(t)
	f := &changing{refusals: []error{act.ErrBusy, act.ErrDraft}}
	now := t0.Add(11 * time.Minute)
	sv := supervisor(st, &f.fakeSender, &now)
	sv.Sender = f
	s := erroring()
	for i := 0; i < 5; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if f.checks != 2 || len(f.sent) != 0 || len(events(t, st, KindPresent)) != 1 {
		t.Fatalf("checks %d, sent %q, present %+v", f.checks, f.sent, events(t, st, KindPresent))
	}
	// A running turn at the first check, then no way in: the second opens its case.
	st = testStore(t)
	f = &changing{refusals: []error{act.ErrBusy, act.ErrNoPath, act.ErrNoPath}}
	now = t0.Add(11 * time.Minute)
	sv = supervisor(st, &f.fakeSender, &now)
	sv.Sender = f
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if f.checks != 2 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("checks %d, cases %+v", f.checks, events(t, st, KindCase))
	}
}

func TestALimitThatStatesNoResetGetsANoteOnceItOutlastsTheStaleHour(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(30 * time.Minute)
	sv := supervisor(st, f, &now)
	s := &sessions.Session{Key: "limit", Tool: "claude", Pane: "%6", PID: 54, Phase: sessions.NeedsYou, Since: t0, Tasks: []sessions.Task{},
		AskKind: sessions.AskError, Ask: "stopped on an error: 5-hour limit reached", LastText: "5-hour limit reached"}
	run(t, sv, s)
	if _, ok := note(t, st, AskNote+s.Key); ok {
		t.Fatal("a note while the limit may still lift by itself")
	}
	now = t0.Add(DefaultParams.Stale + time.Minute)
	run(t, sv, s)
	if n, ok := note(t, st, AskNote+s.Key); !ok || !strings.HasPrefix(n.Title, "A usage limit") {
		t.Fatalf("note %+v %v", n, ok)
	}
}

// storing is a sender that checks, as it types, that the message is on record as being sent.
type storing struct {
	fakeSender
	t  *testing.T
	st *store.Store
}

func (f *storing) Send(ctx context.Context, t act.Target, text string) (act.Path, error) {
	recorded := false
	for _, e := range events(f.t, f.st, KindAct) {
		recorded = recorded || metaMap(e)["sending"] == true && metaMap(e)["text"] == text
	}
	if !recorded {
		f.t.Fatalf("typed %q before it was on record", text)
	}
	return f.fakeSender.Send(ctx, t, text)
}

func TestAMessageIsOnRecordBeforeItIsTyped(t *testing.T) {
	st := testStore(t)
	f := &storing{t: t, st: st}
	now := t0.Add(10 * time.Minute)
	sv := &Supervisor{Store: st, Sender: f, Params: DefaultParams, Live: map[string]bool{RuleTransient: true},
		Probe: probeAt(time.Time{}), Now: func() time.Time { return now }}
	run(t, sv, erroring())
	if len(f.sent) != 1 {
		t.Fatalf("sent %q", f.sent)
	}
}

func TestOnlyTheMessageItselfIsDelivery(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	run(t, sv, s)
	// The turns after it are not the message: a tool call, and his own words that open with the marker.
	s.Log.Add(sessions.Entry{Role: sessions.RoleTool, Name: "Bash", Text: "ls"})
	lands(s, Marker+" what did you send?", now.Add(time.Second), false)
	now = now.Add(time.Minute)
	run(t, sv, s)
	if r := results(t, st); r[Delivered] {
		t.Fatalf("delivered by another turn: %v", r)
	}
	now = t0.Add(10*time.Minute + DeliveryWait)
	run(t, sv, s)
	if r := results(t, st); !r[NotDelivered] || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("outcomes %v, cases %+v", r, events(t, st, KindCase))
	}
}

func TestHisOwnTurnThatOpensWithTheMarkerHolds(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(30 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	typed(s, Marker+" Your last turn stopped on a transient API error. Continue where you left off. Then tag the release.", t0.Add(25*time.Minute))
	run(t, sv, s)
	if len(f.sent) != 0 || len(events(t, st, KindHold)) != 1 {
		t.Fatalf("sent %q, holds %+v", f.sent, events(t, st, KindHold))
	}
}

func TestALocalCommandOrAnAnsweredAskIsTheOwnerThere(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(30 * time.Minute)
	sv := supervisor(st, f, &now)
	login := erroring()
	login.ControlAt = t0.Add(29 * time.Minute) // he ran /login in it
	ss := []*sessions.Session{login}
	// he answered its question, plan or permission a minute ago, and it went on to stop on the error
	for _, kind := range []sessions.AskKind{sessions.AskQuestion, sessions.AskPlan, sessions.AskPermission} {
		answered := erroring()
		answered.Key = "answered-" + string(kind)
		if _, err := st.Put(context.Background(), []store.Event{{Source: store.Sessions, SourceKey: answered.Key + "@1", TS: t0.Add(29 * time.Minute),
			Session: answered.Key, Actor: store.ActorHarness, Kind: "phase", Meta: `{"from":"needs_you","to":"working","from_ask_kind":"` + string(kind) + `"}`}}); err != nil {
			t.Fatal(err)
		}
		ss = append(ss, answered)
	}
	run(t, sv, ss...)
	if len(f.sent) != 0 || len(events(t, st, KindHold)) != 4 {
		t.Fatalf("sent %q, holds %+v", f.sent, events(t, st, KindHold))
	}
}

func TestALoginIsHeldThroughTheRunsOwnProbe(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(25 * time.Minute)
	sv := supervisor(st, f, &now)
	sv.Probe, sv.ClaudeDir = nil, t.TempDir() // the run builds its probe: the store and the credentials
	cred := filepath.Join(sv.ClaudeDir, ".credentials.json")
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	loggedIn := t0.Add(20 * time.Minute)
	if err := os.Chtimes(cred, loggedIn, loggedIn); err != nil {
		t.Fatal(err)
	}
	s := erroring()
	s.AskKind, s.Ask, s.LastText = sessions.AskError, "stopped on an error: "+errLogin, errLogin
	sv.Live = map[string]bool{RuleAccess: true}
	run(t, sv, s)
	if len(f.sent) != 0 {
		t.Fatalf("sent within the window after his login: %q", f.sent)
	}
	now = loggedIn.Add(DefaultParams.OwnerWindow + time.Minute)
	run(t, sv, s)
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "Access works again") {
		t.Fatalf("after it: %q", f.sent)
	}
}

func TestTheOwnersLoginIsHeldForTheOwnerWindow(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	loggedIn := t0.Add(20 * time.Minute)
	if err := os.Chtimes(cred, loggedIn, loggedIn); err != nil {
		t.Fatal(err)
	}
	c := credentials{inner: probeAt(time.Time{}), path: cred, hold: DefaultParams.OwnerWindow}
	if back, ok := c.AccessBack(Login, t0); !ok || !back.Equal(loggedIn.Add(DefaultParams.OwnerWindow)) {
		t.Fatalf("back %v %v", back, ok)
	}
}

func TestAnOwnerTurnThatIsMomentumsDoesNotResetTheTries(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0
	sv := supervisor(st, f, &now)
	s := erroring()
	key := StoreSession(s)
	at := t0
	for try := 0; try < 4; try++ {
		now = at.Add(DefaultParams.Backoff[try])
		run(t, sv, s)
		text := strings.TrimPrefix(f.sent[len(f.sent)-1], "%7 ")
		lands(s, text, now.Add(time.Second), false)
		// the store credited the message to the owner, its text changed on the way
		if _, err := st.Put(context.Background(), []store.Event{{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@t%d", key, try),
			TS: now.Add(time.Second), Session: key, Actor: store.ActorOwner, Kind: "owner_text", Summary: text + " *"}}); err != nil {
			t.Fatal(err)
		}
		at = now.Add(5 * time.Minute)
		s.Since = at
		now = at.Add(time.Second)
		run(t, sv, s)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(20 * time.Minute)
		run(t, sv, s)
	}
	if len(f.sent) != 4 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("sent %d, cases %+v", len(f.sent), events(t, st, KindCase))
	}
}

func TestAMessageActWillNotTypeOpensOneCase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(20 * time.Minute)
	sv := supervisor(st, f, &now)
	sv.Live = map[string]bool{RulePromise: true}
	// its promise quotes what reads like a running turn's line
	s := &sessions.Session{Key: "sess-10", Tool: "claude", Pane: "%5", PID: 42, Phase: sessions.Idle, Since: t0, Tasks: []sessions.Task{},
		LastText: "The build runs; I'll report when it finishes, or press esc to interrupt it."}
	for i := 0; i < 5; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if f.checks != 0 || len(events(t, st, KindAct)) != 1 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("checks %d, acts %+v, cases %+v", f.checks, events(t, st, KindAct), events(t, st, KindCase))
	}
}

func TestPartialAndInFlightSessionsAreRecorded(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	run(t, sv, s) // sends
	now = now.Add(time.Minute)
	part := erroring()
	part.Key, part.Partial = "sess-11", true
	run(t, sv, s, part)
	got := map[string]string{}
	for _, e := range events(t, st, KindWait, KindSkip) {
		got[e.Session+" "+e.Kind] = e.Summary
	}
	if !strings.Contains(got["sess-1 wait"], "not shown up") || !strings.Contains(got["sess-11 skip"], "only in part") {
		t.Fatalf("events %v", got)
	}
}

func TestARunThatDiedWhileSendingDoesNotSendAgain(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	// What a run that died after the Enter left: the message recorded as being sent, and no result.
	d := Decision{Rule: RuleTransient, Verdict: Act, Try: 1, Text: Marker + " Continue."}
	e := sv.event(s, StopOf(s, time.Time{}), d, KindAct, now, "@1")
	e.Meta = meta(map[string]any{"rule": RuleTransient, "try": 1, "text": d.Text, "stop_at": t0.UnixMilli(), "seq": s.Seq, "sending": true})
	if err := sv.put(context.Background(), []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	run(t, sv, s)
	if len(f.sent) != 0 {
		t.Fatalf("sent again: %q", f.sent)
	}
	texts, err := st.SentTexts(context.Background())
	if err != nil || !texts[d.Text] {
		t.Fatalf("the text is momentum's: %v %v", texts, err)
	}
}

func TestAMessageThatOpensNoTurnOpensACaseAndAProblemNote(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	run(t, sv, s)
	for i := 0; i < 20; i++ { // well past the wait for it, and past every try's time
		now = now.Add(time.Minute)
		run(t, sv, s)
	}
	cases := events(t, st, KindCase)
	if len(f.sent) != 1 || len(cases) != 1 || !strings.HasPrefix(metaMap(cases[0])["why"].(string), "not delivered") || !results(t, st)[NotDelivered] {
		t.Fatalf("sent %d, cases %+v", len(f.sent), cases)
	}
	n, ok := note(t, st, ProblemNote+s.Key)
	if !ok || n.Kind != store.NoteProblem || !n.Done.IsZero() || !strings.Contains(n.Title, "build") {
		t.Fatalf("problem note %+v %v", n, ok)
	}
	// Its owner comes back to it: the problem is done.
	typed(s, "try the build again", now.Add(time.Minute))
	now = now.Add(2 * time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, ProblemNote+s.Key); n.Done.IsZero() {
		t.Fatalf("still stands after he came back: %+v", n)
	}
}

func TestUsedUpTriesOpenOneCase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0
	sv := supervisor(st, f, &now)
	s := erroring()
	at := t0
	for try := 0; try < 4; try++ { // each try lands and fails again
		now = at.Add(DefaultParams.Backoff[try])
		run(t, sv, s)
		if len(f.sent) != try+1 {
			t.Fatalf("try %d: sent %d", try+1, len(f.sent))
		}
		lands(s, strings.TrimPrefix(f.sent[try], "%7 "), now.Add(time.Second), false)
		at = now.Add(5 * time.Minute)
		s.Since = at
		now = at.Add(time.Second)
		run(t, sv, s)
	}
	for i := 0; i < 3; i++ { // later runs, hours later too, send nothing and open nothing more
		now = now.Add(time.Hour)
		run(t, sv, s)
	}
	if c := events(t, st, KindCase); len(c) != 1 || len(f.sent) != 4 {
		t.Fatalf("cases %+v, sent %d", c, len(f.sent))
	}
	if _, ok := note(t, st, ProblemNote+s.Key); !ok {
		t.Fatal("no problem note")
	}
}

func TestAPasteThatMayStayOnTheInputLineOpensACase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{send: act.ErrUnstable}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if len(f.sent) != 1 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("sent %d, cases %+v", len(f.sent), events(t, st, KindCase))
	}
	texts, err := st.SentTexts(context.Background())
	if err != nil || len(texts) != 1 {
		t.Fatalf("its text is momentum's, if it goes in: %v %v", texts, err)
	}
}

func TestASessionWithNoWayInOpensOneCase(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{check: act.ErrNoPath}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := erroring()
	for i := 0; i < 3; i++ {
		run(t, sv, s)
		now = now.Add(time.Minute)
	}
	if f.checks != 1 || len(events(t, st, KindCase)) != 1 {
		t.Fatalf("checks %d, cases %+v", f.checks, events(t, st, KindCase))
	}
}

func TestASessionWaitingOnItsOwnerGetsANeedsNote(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(10 * time.Minute)
	sv := supervisor(st, f, &now)
	s := &sessions.Session{Key: "sess-7", Tool: "claude", Name: "release", Pane: "%9", PID: 44, Phase: sessions.NeedsYou, Since: t0,
		AskKind: sessions.AskPermission, Ask: "permission: Bash: make tag", Tasks: []sessions.Task{}}
	id := AskNote + s.Key
	run(t, sv, s)
	if _, ok := note(t, st, id); ok {
		t.Fatal("a note before the owner window passed")
	}
	now = t0.Add(16 * time.Minute)
	run(t, sv, s)
	n, ok := note(t, st, id)
	if !ok || n.Kind != store.NoteNeeds || n.Title != "permission: Bash: make tag" || !n.At.Equal(t0) || !n.Done.IsZero() {
		t.Fatalf("needs note %+v %v", n, ok)
	}
	// He resolves it while the session still waits on the same ask: it stays resolved.
	if _, _, err := st.DoneNote(context.Background(), id, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, id); n.Done.IsZero() {
		t.Fatalf("added again: %+v", n)
	}
	// A new ask is a new note; the session moving on makes it done.
	s.AskKind, s.Ask, s.Since = sessions.AskQuestion, "Which branch should I tag?", t0.Add(30*time.Minute)
	now = t0.Add(46 * time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, id); n.Title != "Which branch should I tag?" || !n.Done.IsZero() {
		t.Fatalf("the new ask: %+v", n)
	}
	s.AskKind, s.Ask, s.Phase, s.TurnOpen, s.Since = "", "", sessions.Working, true, now
	now = now.Add(time.Minute)
	run(t, sv, s)
	if n, _ := note(t, st, id); n.Done.IsZero() {
		t.Fatalf("still stands after it moved on: %+v", n)
	}
}

func TestNeedsNotesForWhatOnlyTheOwnerCanAnswer(t *testing.T) {
	st := testStore(t)
	f := &fakeSender{}
	now := t0.Add(20 * time.Minute)
	sv := supervisor(st, f, &now)
	idle := &sessions.Session{Key: "idle", Tool: "claude", Pane: "%1", PID: 51, Phase: sessions.Idle, Since: t0, Tasks: []sessions.Task{},
		LastText: "Two rows have no owner. Should I drop them, or keep them and fill the owner in?"}
	refused := &sessions.Session{Key: "refused", Tool: "claude", Pane: "%2", PID: 52, Phase: sessions.NeedsYou, Since: t0, Tasks: []sessions.Task{},
		AskKind: sessions.AskError, Ask: "stopped on an error: Prompt is too long", LastText: "Prompt is too long"}
	auto := &sessions.Session{Key: "auto", Tool: "claude", Auto: true, Phase: sessions.NeedsYou, Since: t0, Tasks: []sessions.Task{},
		AskKind: sessions.AskQuestion, Ask: "Which of the two?"}
	phone := &sessions.Session{Key: "hermes:p1", Tool: "hermes", Where: "telegram", Phase: sessions.Idle, Since: t0, Tasks: []sessions.Task{},
		LastText: "Should I book it?"}
	login := &sessions.Session{Key: "login", Tool: "claude", Pane: "%3", PID: 53, Phase: sessions.NeedsYou, Since: t0, Tasks: []sessions.Task{},
		AskKind: sessions.AskError, Ask: "stopped on an error: " + errLogin, LastText: errLogin}
	limit := &sessions.Session{Key: "limit", Tool: "claude", Pane: "%6", PID: 54, Phase: sessions.NeedsYou, Since: t0, Tasks: []sessions.Task{},
		AskKind: sessions.AskError, Ask: "stopped on an error: 5-hour limit reached", LastText: "5-hour limit reached"} // it lifts by itself
	old := erroring() // its rule's time came and went: it is the owner's now
	old.Key, old.Since = "old", t0.Add(-3*time.Hour)
	retried := erroring() // a rule takes care of it
	partial := &sessions.Session{Key: "partial", Tool: "claude", Partial: true, Phase: sessions.NeedsYou, Since: t0, Tasks: []sessions.Task{},
		AskKind: sessions.AskQuestion, Ask: "Ship it?"}
	run(t, sv, idle, refused, auto, phone, login, limit, old, retried, partial)
	want := map[string]string{"idle": "Should I drop them, or keep them and fill the owner in?", "refused": "It stopped on an error: Prompt is too long",
		"login": "Log in again: " + errLogin, "old": "It stopped on an error: " + err503}
	ns, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range ns {
		if strings.HasPrefix(n.ID, AskNote) {
			got[strings.TrimPrefix(n.ID, AskNote)] = n.Title
		}
	}
	for key, title := range want { // the store clips a title to its 120 characters
		if g := strings.TrimSuffix(got[key], "…"); g == "" || !strings.HasPrefix(title, g) {
			t.Fatalf("needs notes %v", got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("needs notes %v", got)
	}
	// A session left alone keeps its note; one that ended has its note done.
	run(t, sv, partial)
	for key := range want {
		if n, _ := note(t, st, AskNote+key); n.Done.IsZero() {
			t.Fatalf("%s ended, and its note stands: %+v", key, n)
		}
	}
}
