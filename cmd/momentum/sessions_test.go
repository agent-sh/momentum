package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
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
		ss, scanErr, err := board(ctx, &sessions.World{Now: now}, st, []sessions.Source{src})
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
