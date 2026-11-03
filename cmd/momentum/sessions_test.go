package main

import (
	"bytes"
	"context"
	"database/sql"
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

func TestPhaseEventsKeepTheStampedDirectory(t *testing.T) {
	for _, cwd := range []string{"", "/w/new"} {
		change := state.Change{Key: "synthetic", Cwd: "/w/loop/run", Tool: "claude", To: sessions.Idle}
		live := &sessions.Session{Key: change.Key, Cwd: cwd, Model: "fixture-model"}
		evs := phaseEvents([]state.Change{change}, []*sessions.Session{live}, time.Now())
		want := change.Cwd
		if cwd != "" {
			want = cwd
		}
		if len(evs) != 1 || evs[0].Cwd != want || evs[0].Model != live.Model {
			t.Fatalf("live cwd %q lost the stamp or model: %+v", cwd, evs)
		}
	}
}

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
		"0s sess-a claude >working 0 / /",
		"1m0s sess-a claude working>needs_you 1 / permission/permission prompt: Bash: rm -rf build",
		"9m0s thread-b codex idle>needs_you 69 / question/Which branch?",
		"10m0s sess-a claude needs_you>working 9 permission/permission prompt: Bash: rm -rf build /",
		"19m0s sess-a claude working>idle 9 / /",
		"20m0s sess-a claude idle> 1 / /",
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
	if err := json.Unmarshal(out.Bytes(), &e); err != nil || e.Kind != "vanished" || !strings.Contains(e.Meta, `"from":"idle"`) || e.Cwd != "/w/parser" {
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

// Only a complete sessions scan advances the source's read time, including a scan with no live sessions.
func TestOnlyACompleteSessionsScanAdvancesItsReadTime(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, scanErr, err := board(ctx, &sessions.World{Now: start}, nil, st, "test", []sessions.Source{fixed{}}); err != nil || scanErr != nil {
		t.Fatal(scanErr, err)
	}
	// a session gone from a scan is written vanished up to state.Keep later: the scan has read up to that much before it
	read, err := st.ReadThrough(ctx, store.Sessions)
	if err != nil || !read.Equal(start.Add(-state.Keep)) {
		t.Fatalf("complete scan: %v %v", read, err)
	}
	if _, _, err := board(ctx, &sessions.World{Now: start.Add(time.Hour)}, errors.New("incomplete snapshot"), st, "test", []sessions.Source{fixed{}}); err != nil {
		t.Fatal(err)
	}
	read, err = st.ReadThrough(ctx, store.Sessions)
	if err != nil || !read.Equal(start.Add(-state.Keep)) {
		t.Fatalf("incomplete scan advanced: %v %v", read, err)
	}
}

// scanDay runs a sessions scan every five minutes from 08:00 to 21:00 of a day over a Claude Code source and a Codex
// source, and says at each scan what each shows: scan(now) names the claude sessions as they stand, and whether the
// harness's scan fails.
func scanDay(t *testing.T, st *store.Store, day time.Time, claudeAt func(at time.Time) ([]sessions.Session, bool), codexFails func(at time.Time) bool) {
	t.Helper()
	claude := &flaky{tool: "claude"}
	codex := &flaky{tool: "codex", ss: []sessions.Session{{Key: "codex-a", Tool: "codex", Name: "a", Phase: sessions.Working, Log: logOf(2), LastActive: day}}}
	for at := day.Add(8 * time.Hour); !at.After(day.Add(21 * time.Hour)); at = at.Add(5 * time.Minute) {
		claude.ss, claude.fail = claudeAt(at)
		codex.fail = codexFails != nil && codexFails(at)
		if _, _, err := board(context.Background(), &sessions.World{Now: at}, nil, st, "test", []sessions.Source{claude, codex}); err != nil {
			t.Fatal(err)
		}
	}
}

// A harness whose scan fails for a stretch shorter than a window keeps its sessions as they were, so a wait that
// begins and ends inside the stretch is never seen; the first complete scan after it must not call the window it
// lost read. Another harness's windows are read all the same.
func TestAScanThatFailedLeavesAHoleTheLaterScansDoNotFill(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	// two sessions each wait on a permission prompt for half an hour; the claude scan fails from 10:00 to 20:00
	waiting := func(key string, from time.Time, now time.Time) sessions.Session {
		s := sessions.Session{Key: key, Tool: "claude", Name: key, Phase: sessions.Working, Log: logOf(3), LastActive: now}
		if !now.Before(from) && now.Before(from.Add(30*time.Minute)) {
			s.Phase, s.AskKind, s.Ask, s.LastActive = sessions.NeedsYou, sessions.AskPermission, "run the build?", from
		}
		return s
	}
	scanDay(t, st, day, func(now time.Time) ([]sessions.Session, bool) {
		return []sessions.Session{waiting("w1", at(9, 0), now), waiting("w2", at(13, 0), now)}, !now.Before(at(10, 0)) && now.Before(at(20, 0))
	}, nil)
	evs, err := st.Events(ctx, store.Query{Since: day, Source: store.Sessions})
	if err != nil {
		t.Fatal(err)
	}
	var waits []string
	for _, e := range evs {
		if strings.Contains(e.Summary, "waited on") {
			waits = append(waits, e.Session)
		}
	}
	if len(waits) != 1 || waits[0] != "w1" {
		t.Fatalf("the waits written: %v (the second wait began and ended inside the failure)", waits)
	}
	unread := func(harness string, since, until time.Time) string {
		t.Helper()
		got, err := st.UnreadSources(ctx, store.Transcripts, store.ActivityQuery{Since: since, Until: until, BySession: true, Harness: harness, Sources: []string{store.Sessions}})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	for _, c := range []struct {
		harness      string
		since, until time.Time
		want         string
	}{
		{"claude", at(8, 0), at(19, 0), "sessions"}, // holds the failed scans, though the scan at 20:00 is complete
		{"", at(8, 0), at(19, 0), "sessions"},
		{"codex", at(8, 0), at(19, 0), ""},
		{"claude", at(8, 0), at(9, 55), ""},            // read before the failure began
		{"claude", at(20, 0), at(20, 30), ""},          // and after it ended
		{"", at(20, 0), at(20, 30), ""},                //
		{"claude", at(19, 55), at(20, 30), "sessions"}, // the last failed scan was at 19:55: a window that starts then holds it
	} {
		if got := unread(c.harness, c.since, c.until); got != c.want {
			t.Errorf("harness %q, %s to %s: unread %q, want %q", c.harness, c.since.Format("15:04"), c.until.Format("15:04"), got, c.want)
		}
	}
}

// A harness that keeps failing holds up only itself: the sessions scan is read for another harness's windows.
func TestAHarnessThatKeepsFailingHoldsUpOnlyItsOwnWindows(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	scanDay(t, st, day, func(time.Time) ([]sessions.Session, bool) { return nil, false },
		func(now time.Time) bool { return !now.Before(day.Add(10 * time.Hour)) }) // codex's scan fails from 10:00 on
	q := store.ActivityQuery{Since: day.Add(11 * time.Hour), Until: day.Add(12 * time.Hour), BySession: true, Sources: []string{store.Sessions}}
	for harness, want := range map[string]string{"claude": "", "codex": "sessions", "": "sessions"} {
		q.Harness = harness
		got, err := st.UnreadSources(ctx, store.Transcripts, q)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("harness %q: unread %v %v, want %q", harness, got, err, want)
		}
	}
	// the whole source's read stopped at the last scan both harnesses completed (09:55, read through 09:40)
	if read, err := st.ReadThrough(ctx, store.Sessions); err != nil || !read.Equal(day.Add(9*time.Hour+40*time.Minute)) {
		t.Fatalf("the source as a whole: read through %v %v", read, err)
	}
}

// A scan that fails as a whole, with no harness to name, is an incomplete run of the source as a whole.
func TestAScanThatFailedAsAWholeHoldsEveryHarness(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	if _, _, err := board(ctx, &sessions.World{Now: t0}, nil, st, "test", []sessions.Source{fixed{}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := board(ctx, &sessions.World{Now: t0.Add(time.Hour)}, errors.New("incomplete snapshot"), st, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := board(ctx, &sessions.World{Now: t0.Add(2 * time.Hour)}, nil, st, "test", []sessions.Source{fixed{}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.UnreadSources(ctx, store.Transcripts, store.ActivityQuery{Since: t0, Until: t0.Add(90 * time.Minute), BySession: true, Harness: "test", Sources: []string{store.Sessions}})
	if err != nil || strings.Join(got, ",") != "sessions" {
		t.Fatalf("a window that holds the failed scan: unread %v %v", got, err)
	}
}

// What a scan read lands with the stamps and events it covers, or none of it does: a run cut short between them would
// leave sessions saved that no record of the scan's failure or read goes with.
func TestAScansReadRecordsLandWithItsStampsAndEvents(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// the store refuses the records of reads, as a disk that fails at the end of the run would
	if _, err := db.Exec(`CREATE TRIGGER refuse_reads BEFORE INSERT ON cursors BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Minute)
	src := fixed{{Key: "a", Tool: "test", Name: "in a pane", Phase: sessions.Idle, Log: logOf(3), LastActive: t0}}
	if _, _, err := board(ctx, &sessions.World{Now: t0}, nil, st, "test", []sessions.Source{src}); err == nil {
		t.Fatal("a scan whose read records were refused succeeded")
	}
	if evs, err := st.Events(ctx, store.Query{Since: t0.Add(-time.Hour), Source: store.Sessions}); err != nil || len(evs) != 0 {
		t.Fatalf("events of a scan that did not land: %+v %v", evs, err)
	}
	if stamps, err := st.SessionStamps(ctx); err != nil || len(stamps) != 0 {
		t.Fatalf("stamps of a scan that did not land: %+v %v", stamps, err)
	}
}

// openStore is a store of the test's own.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// unreadSessions says which of the sessions source's reads fall short of a window, for a harness (none: every one).
func unreadSessions(t *testing.T, st *store.Store, harness string, since, until time.Time) string {
	t.Helper()
	got, err := st.UnreadSources(context.Background(), store.Transcripts, store.ActivityQuery{Since: since, Until: until, BySession: true, Harness: harness,
		Sources: []string{store.Sessions}})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, ",")
}

// A session a source read only in part (a query of the harness's own failed) keeps its stamp as it was, so a wait that
// began and ended while it was read short leaves no event; the harness's run is incomplete, and a window that holds it
// is never read, as one that holds a failed scan is not. Another harness's windows, and the harness's after the scans
// that read it short, are read.
func TestASessionReadInPartLeavesItsHarnessesRunIncomplete(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	// a Hermes session works, asks for a permission from 09:00 to 09:30 and works again; a scan every five minutes
	scans := func(st *store.Store, partial bool) {
		t.Helper()
		hermes := &flaky{tool: "hermes"}
		claude := &flaky{tool: "claude", ss: []sessions.Session{{Key: "claude-a", Tool: "claude", Name: "a", Phase: sessions.Working, Log: logOf(2), LastActive: day}}}
		for now := at(8, 0); !now.After(at(12, 0)); now = now.Add(5 * time.Minute) {
			s := sessions.Session{Key: "hermes:c", Tool: "hermes", Name: "c", Phase: sessions.Working, Log: logOf(3), LastActive: now}
			if !now.Before(at(9, 0)) && now.Before(at(9, 30)) {
				if partial { // a query of the harness's own failed on these scans
					s.Partial = true
				} else {
					s.Phase, s.AskKind, s.Ask, s.LastActive = sessions.NeedsYou, sessions.AskPermission, "run the build?", at(9, 0)
				}
			}
			hermes.ss = []sessions.Session{s}
			if _, scanErr, err := board(ctx, &sessions.World{Now: now}, nil, st, "test", []sessions.Source{hermes, claude}); err != nil || scanErr != nil {
				t.Fatal(scanErr, err)
			}
		}
	}
	waits := func(st *store.Store) int {
		t.Helper()
		evs, err := st.Events(ctx, store.Query{Since: day, Source: store.Sessions})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range evs {
			if strings.Contains(e.Summary, "waited on") {
				n++
			}
		}
		return n
	}
	whole, short := openStore(t), openStore(t)
	scans(whole, false)
	scans(short, true)
	if got, lost := waits(whole), waits(short); got != 1 || lost != 0 {
		t.Fatalf("the wait is written by whole reads (%d) and lost by reads in part (%d)", got, lost)
	}
	for _, c := range []struct {
		name         string
		st           *store.Store
		harness      string
		since, until time.Time
		want         string
	}{
		{"whole reads: the window over the wait", whole, "hermes", at(8, 0), at(11, 0), ""},
		{"read in part: the window over the wait", short, "hermes", at(8, 0), at(11, 0), "sessions"},
		{"read in part: a window that starts inside the stretch", short, "hermes", at(9, 10), at(11, 0), "sessions"},
		{"read in part: every harness's window", short, "", at(8, 0), at(11, 0), "sessions"},
		{"read in part: another harness's window", short, "claude", at(8, 0), at(11, 0), ""},
		{"read in part: a window after the scans that read it short", short, "hermes", at(9, 40), at(11, 0), ""},
	} {
		if got := unreadSessions(t, c.st, c.harness, c.since, c.until); got != c.want {
			t.Errorf("%s: unread %q, want %q", c.name, got, c.want)
		}
	}
	// the source as a whole was not read by the scans that read a session short
	if read, err := short.ReadThrough(ctx, store.Sessions); err != nil || !read.Equal(at(12, 0).Add(-state.Keep)) {
		t.Fatalf("the source as a whole is read through the last whole scan: %v %v", read, err)
	}
	// a session read in part whose harness is no source of the scan leaves the source as a whole incomplete
	st := openStore(t)
	stray := fixed{{Key: "x", Tool: "hermes", Name: "x", Phase: sessions.Working, Log: logOf(1), LastActive: at(9, 0), Partial: true}}
	for _, now := range []time.Time{at(9, 0), at(9, 5)} {
		if _, _, err := board(ctx, &sessions.World{Now: now}, nil, st, "test", []sessions.Source{stray}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := st.Incomplete(ctx, store.Sessions, "", at(8, 0), at(10, 0)); err != nil || !got.Equal(at(9, 5)) {
		t.Fatalf("a session read in part of no source's harness: incomplete at %v %v", got, err)
	}
	if read, err := st.ReadThrough(ctx, store.Sessions); err != nil || !read.IsZero() {
		t.Fatalf("and the source is not read: %v %v", read, err)
	}
}

// A scan that starts further from the last complete one than state.Keep has no scan between: the timer stopped, a run
// gave up on the lock of a stuck one, or the machine was off. What the sessions did in between is not in the store, and
// the scans after it do not put it there, so the first one records the stretch as an incomplete run: a window that holds
// it is never read, though the stopped rule would let a decision on it through once the scans resumed. A harness whose
// scans went on is read all the same.
func TestScansThatResumeAfterAStopLeaveItsStretchUnread(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	claude := &flaky{tool: "claude", ss: []sessions.Session{{Key: "claude-a", Tool: "claude", Name: "a", Phase: sessions.Working, Log: logOf(2), LastActive: day}}}
	codex := &flaky{tool: "codex", ss: []sessions.Session{{Key: "codex-a", Tool: "codex", Name: "a", Phase: sessions.Working, Log: logOf(2), LastActive: day}}}
	scan := func(now time.Time, srcs ...sessions.Source) {
		t.Helper()
		if _, scanErr, err := board(ctx, &sessions.World{Now: now}, nil, st, "test", srcs); err != nil || scanErr != nil {
			t.Fatal(scanErr, err)
		}
	}
	for now := at(8, 0); !now.After(at(9, 0)); now = now.Add(5 * time.Minute) {
		scan(now, claude, codex)
	}
	// the claude scan did not run from 09:00 to 14:00 (the codex scan did), then it ran again
	for now := at(9, 5); now.Before(at(14, 0)); now = now.Add(5 * time.Minute) {
		scan(now, codex)
	}
	for now := at(14, 0); !now.After(at(15, 0)); now = now.Add(5 * time.Minute) {
		scan(now, claude, codex)
	}
	for _, c := range []struct {
		name         string
		harness      string
		since, until time.Time
		want         string
	}{
		{"the stretch and what follows", "claude", at(8, 0), at(15, 0), "sessions"},
		{"a window inside the stretch", "claude", at(10, 0), at(12, 0), "sessions"},
		{"a window that ends where the scans stopped", "claude", at(8, 0), at(9, 0), ""},
		{"a window that starts where the scans resumed", "claude", at(14, 0), at(14, 30), ""},
		{"the harness whose scans went on", "codex", at(8, 0), at(14, 30), ""},
		{"a signal of every harness", "", at(10, 0), at(12, 0), "sessions"},
	} {
		if got := unreadSessions(t, st, c.harness, c.since, c.until); got != c.want {
			t.Errorf("%s: unread %q, want %q", c.name, got, c.want)
		}
	}
	// scans no further apart than state.Keep leave nothing behind them
	steady := openStore(t)
	for now := at(8, 0); !now.After(at(15, 0)); now = now.Add(state.Keep) {
		if _, _, err := board(ctx, &sessions.World{Now: now}, nil, steady, "test", []sessions.Source{claude}); err != nil {
			t.Fatal(err)
		}
	}
	if got := unreadSessions(t, steady, "claude", at(8, 0), at(14, 0)); got != "" {
		t.Errorf("scans %v apart: unread %q", state.Keep, got)
	}
}
