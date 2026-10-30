package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

// fixed is a source that reports the same sessions on every scan, as a harness would between two runs.
type fixed []sessions.Session

func (f fixed) Tool() string { return "test" }
func (f fixed) Close() error { return nil }
func (f fixed) Scan(context.Context, *sessions.World) ([]*sessions.Session, error) {
	var out []*sessions.Session
	for _, s := range f {
		s := s
		out = append(out, &s)
	}
	return out, nil
}

func TestPhaseTimesSurviveBetweenRuns(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	src := fixed{{Key: "w", Tool: "claude", TurnOpen: true}, {Key: "a", Tool: "hermes", Auto: true, TurnOpen: true}}
	run := func(now time.Time) []*sessions.Session { // one CLI run: its own process, its own store handle
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		ss, scanErr, err := board(ctx, &sessions.World{Now: now}, nil, st, "test", []sessions.Source{src})
		if err != nil || scanErr != nil {
			t.Fatal(err, scanErr)
		}
		return ss
	}
	run(t0)
	ss := run(t0.Add(5 * time.Minute))
	if ss[0].Phase != sessions.Working || !ss[0].Since.Equal(t0) {
		t.Fatalf("the second run lost the phase time: %s since %v", ss[0].Phase, ss[0].Since)
	}
	var out bytes.Buffer
	if err := printBoard(&out, ss, t0.Add(5*time.Minute), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "working       5m  claude") || !strings.Contains(out.String(), "1 sessions, 1 automated runs not listed") {
		t.Fatalf("board:\n%s", out.String())
	}
}

func TestSessionsTakesNoArguments(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"sessions", "extra"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
}

// timeline is a source whose sessions a test sets before each run, as a harness would show them.
type timeline struct{ ss []sessions.Session }

func (l *timeline) Tool() string { return "test" }
func (l *timeline) Close() error { return nil }
func (l *timeline) Scan(context.Context, *sessions.World) ([]*sessions.Session, error) {
	var out []*sessions.Session
	for _, s := range l.ss {
		s := s
		out = append(out, &s)
	}
	return out, nil
}

// logOf is an activity log n entries long.
func logOf(n int) *sessions.Log {
	var l sessions.Log
	for i := 0; i < n; i++ {
		l.Add(sessions.Entry{Role: sessions.RoleOut, Text: "x"})
	}
	return &l
}

func TestPhaseChangesAreKeptAsEvents(t *testing.T) {
	ctx := context.Background()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Minute)
	plus := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	src := &timeline{}
	a := func(phase sessions.Phase, seq int, last time.Time) sessions.Session {
		return sessions.Session{Key: "sess-a", Tool: "claude", Name: "fix the parser", Cwd: "/w/parser", Phase: phase, TurnOpen: phase == sessions.Working,
			Log: logOf(seq), LastActive: last}
	}
	b := func(phase sessions.Phase, seq int, last time.Time) sessions.Session {
		return sessions.Session{Key: "thread-b", Tool: "codex", Name: "port the cli", Phase: phase, TurnOpen: phase == sessions.Working, Log: logOf(seq), LastActive: last}
	}
	runAt := func(at time.Time, ss ...sessions.Session) {
		t.Helper()
		src.ss = ss
		st, err := store.Open(paths.DB())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, scanErr, err := board(ctx, &sessions.World{Now: at}, nil, st, "test", []sessions.Source{src}); err != nil || scanErr != nil {
			t.Fatal(err, scanErr)
		}
	}
	asks := func(s sessions.Session, kind sessions.AskKind, ask string) sessions.Session {
		s.Phase, s.TurnOpen, s.AskKind, s.Ask = sessions.NeedsYou, kind == sessions.AskPermission, kind, ask
		return s
	}
	runAt(t0, a(sessions.Working, 3, t0), b(sessions.Idle, 5, plus(-60)))
	runAt(plus(2), asks(a("", 4, plus(1)), sessions.AskPermission, "permission prompt: Bash: rm -rf build"), b(sessions.Idle, 5, plus(-60)))
	runAt(plus(10), a(sessions.Working, 6, plus(9)), asks(b("", 7, plus(9)), sessions.AskQuestion, "Which branch?"))
	runAt(plus(20), a(sessions.Idle, 9, plus(19)), b(sessions.Working, 8, plus(19)))
	runAt(plus(40), b(sessions.Idle, 10, plus(39))) // a is gone for longer than state.Keep

	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(ctx, store.Query{Since: t0.Add(-2 * time.Hour), Source: store.Sessions, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := len(evs) - 1; i >= 0; i-- { // oldest first
		e := evs[i]
		var m phaseMeta
		if err := json.Unmarshal([]byte(e.Meta), &m); err != nil {
			t.Fatal(err)
		}
		if e.Actor != store.ActorHarness {
			t.Fatalf("actor %s", e.Actor)
		}
		got = append(got, fmt.Sprintf("%s %s %s %s>%s %d %s/%s %s/%s", e.TS.Sub(t0), e.Session, e.Harness, m.From, m.To, m.ForMS/60000,
			m.FromAskKind, m.FromAsk, m.AskKind, m.Ask))
	}
	want := []string{
		"-1h0m0s thread-b codex >idle 0 / /",
		"0s sess-a claude-code >working 0 / /",
		"1m0s sess-a claude-code working>needs_you 1 / permission/permission prompt: Bash: rm -rf build",
		"9m0s thread-b codex idle>needs_you 69 / question/Which branch?",
		"10m0s sess-a claude-code needs_you>working 9 permission/permission prompt: Bash: rm -rf build /",
		"19m0s sess-a claude-code working>idle 9 / /",
		"20m0s sess-a claude-code idle> 1 / /",
		"20m0s thread-b codex needs_you>working 11 question/Which branch? /",
		"39m0s thread-b codex working>idle 19 / /",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The history answers how long sessions waited on a permission prompt, and which ones.
	waited := map[string]time.Duration{}
	for _, e := range evs {
		var m phaseMeta
		json.Unmarshal([]byte(e.Meta), &m)
		if m.From == sessions.NeedsYou && m.FromAskKind == sessions.AskPermission {
			waited[m.Name] += time.Duration(m.ForMS) * time.Millisecond
		}
	}
	if len(waited) != 1 || waited["fix the parser"] != 9*time.Minute {
		t.Fatalf("permission waits %v", waited)
	}

	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"evidence", "events", "--source", "sessions", "--since", "1d"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, line := range []string{
		"harness  phase            fix the parser: needs_you -> working after 9m; waited on (permission) permission prompt: Bash: rm -rf build",
		"harness  phase            port the cli: idle -> needs_you after 1h09m; asks (question) Which branch?",
		"harness  vanished         fix the parser: vanished after 1m idle",
		"harness  appeared         port the cli: appeared, idle",
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("the list lacks %q:\n%s", line, out.String())
		}
	}
	out.Reset()
	if code := run(ctx, []string{"evidence", "events", "--source", "sessions", "--kind", "vanished", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var e store.Event
	if err := json.Unmarshal(out.Bytes(), &e); err != nil || e.Kind != "vanished" || !strings.Contains(e.Meta, `"from":"idle"`) || e.Cwd != "" {
		t.Fatalf("json %s: %v", out.String(), err)
	}
}

func TestObservedEventsCannotBeRebuilt(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"evidence", "rebuild", "sessions"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "observations") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
}

// flaky is a harness's source whose scan fails while fail is set, as a locked database or a failing lister does.
type flaky struct {
	tool string
	ss   []sessions.Session
	fail bool
}

func (f *flaky) Tool() string { return f.tool }
func (f *flaky) Close() error { return nil }
func (f *flaky) Scan(context.Context, *sessions.World) ([]*sessions.Session, error) {
	if f.fail {
		return nil, errors.New("database is locked")
	}
	var out []*sessions.Session
	for _, s := range f.ss {
		s := s
		out = append(out, &s)
	}
	return out, nil
}

func TestASourceThatFailsDoesNotMakeItsSessionsVanish(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	t0 := time.Now().Add(-time.Hour).Truncate(time.Minute)
	src := &flaky{tool: "codex", ss: []sessions.Session{{Key: "thread-b", Tool: "codex", Name: "port the cli", Phase: sessions.Idle, Log: logOf(4), LastActive: t0}}}
	runAt := func(at time.Time, fail bool) {
		t.Helper()
		src.fail = fail
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, _, err := board(ctx, &sessions.World{Now: at}, nil, st, "test", []sessions.Source{src}); err != nil {
			t.Fatal(err)
		}
	}
	runAt(t0, false)
	runAt(t0.Add(state.Keep+time.Minute), true) // the scan fails for longer than state.Keep
	runAt(t0.Add(state.Keep+2*time.Minute), false)
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(ctx, store.Query{Since: t0.Add(-time.Hour), Source: store.Sessions})
	if err != nil || len(evs) != 1 || evs[0].Kind != "appeared" {
		t.Fatalf("one appearance and no departure: %+v %v", evs, err)
	}
}

func TestAPartialReadWritesNoEvent(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	t0 := time.Now().Add(-time.Hour).Truncate(time.Minute)
	src := &timeline{}
	runAt := func(at time.Time, s sessions.Session) {
		t.Helper()
		src.ss = []sessions.Session{s}
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, _, err := board(ctx, &sessions.World{Now: at}, nil, st, "test", []sessions.Source{src}); err != nil {
			t.Fatal(err)
		}
	}
	whole := sessions.Session{Key: "hermes:c", Tool: "hermes", Name: "notes", Phase: sessions.Idle, Log: logOf(6), LastActive: t0.Add(-time.Hour)}
	short := whole
	short.Log, short.Partial = logOf(2), true // a page of its log failed
	runAt(t0, whole)
	runAt(t0.Add(time.Minute), short)
	runAt(t0.Add(2*time.Minute), whole)
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if evs, err := st.Events(ctx, store.Query{Since: t0.Add(-2 * time.Hour), Source: store.Sessions}); err != nil || len(evs) != 1 || evs[0].Kind != "appeared" {
		t.Fatalf("only the first sighting: %+v %v", evs, err)
	}
}

// counted is a source that counts its scans.
type counted struct {
	timeline
	scans int
}

func (c *counted) Scan(ctx context.Context, w *sessions.World) ([]*sessions.Session, error) {
	c.scans++
	return c.timeline.Scan(ctx, w)
}

func TestASessionsRunTakesItsTurn(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	open := func() *store.Store {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	holder, st := open(), open() // another run holds the store
	unlock, err := holder.LockSessions(store.BusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	src := &counted{timeline: timeline{ss: []sessions.Session{{Key: "a", Tool: "test", Phase: sessions.Idle, Log: logOf(1)}}}}
	snapshot := func(now time.Time) (*sessions.World, error) { return &sessions.World{Now: now}, nil }
	if _, _, _, err := observe(ctx, st, 200*time.Millisecond, snapshot, "test", []sessions.Source{src}); err == nil || src.scans != 0 {
		t.Fatalf("a run scanned while another held the store: %v, %d scans", err, src.scans)
	}
	unlock()
	ss, _, scanErr, err := observe(ctx, st, 200*time.Millisecond, snapshot, "test", []sessions.Source{src})
	if err != nil || scanErr != nil || len(ss) != 1 || src.scans != 1 {
		t.Fatalf("got %v %v %d sessions, %d scans", err, scanErr, len(ss), src.scans)
	}
	again, err := holder.LockSessions(0) // free once the run returned, before it prints
	if err != nil {
		t.Fatalf("the run kept the lock: %v", err)
	}
	again()
}

func TestAnIncompleteSnapshotMakesNoSessionVanish(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	t0 := time.Now().Add(-time.Hour).Truncate(time.Minute)
	src := &timeline{}
	runAt := func(at time.Time, snapErr error, ss ...sessions.Session) {
		t.Helper()
		src.ss = ss
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, _, err := board(ctx, &sessions.World{Now: at}, snapErr, st, "test", []sessions.Source{src}); err != nil {
			t.Fatal(err)
		}
	}
	a := sessions.Session{Key: "a", Tool: "test", Name: "in a pane", Phase: sessions.Idle, Log: logOf(3), LastActive: t0}
	runAt(t0, nil, a)
	runAt(t0.Add(state.Keep+time.Minute), errors.New("tmux panes: timed out")) // the pane's session is not found
	runAt(t0.Add(state.Keep+2*time.Minute), nil, a)
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if evs, err := st.Events(ctx, store.Query{Since: t0.Add(-time.Hour), Source: store.Sessions}); err != nil || len(evs) != 1 || evs[0].Kind != "appeared" {
		t.Fatalf("one appearance and no departure: %+v %v", evs, err)
	}
}
