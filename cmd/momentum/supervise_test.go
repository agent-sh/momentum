package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/act"
)

// typist records what supervision would type.
type typist struct{ sent []string }

func (t *typist) Check(context.Context, act.Target) (act.Path, error) { return act.Tmux, nil }
func (t *typist) Interrupt(_ context.Context, _ act.Target, text string) (act.Path, error) {
	t.sent = append(t.sent, text)
	return act.Tmux, nil
}
func (t *typist) Send(_ context.Context, _ act.Target, text string) (act.Path, error) {
	t.sent = append(t.sent, text)
	return act.Tmux, nil
}

func TestSuperviseDecidesNothingOnAShortScan(t *testing.T) {
	ctx, db := context.Background(), filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Minute)
	stuck := sessions.Session{Key: "s1", Tool: "test", Name: "release", Pane: "%4", PID: 7, AskKind: sessions.AskError, LastActive: t0,
		Ask: "stopped on an error: API Error: 503 service unavailable", Log: logOf(2)}
	src := fixed{stuck}
	now := t0
	ty := &typist{}
	sv := &supervise.Supervisor{Store: st, Sender: ty, Params: supervise.DefaultParams, Live: supervise.Live,
		Probe: noAccess{}, Now: func() time.Time { return now }}
	pass := func(at time.Time, snapErr error) error {
		t.Helper()
		now = at
		var stderr bytes.Buffer
		_, err := superviseOnce(ctx, st, db, func(time.Time) (*sessions.World, error) { return &sessions.World{Now: at}, snapErr },
			[]sessions.Source{src}, sv, &stderr)
		if snapErr != nil && !strings.Contains(stderr.String(), "supervise skipped") {
			t.Fatalf("stderr %q", stderr.String())
		}
		return err
	}
	if err := pass(t0, nil); err != nil { // it stops on the error
		t.Fatal(err)
	}
	// Past the first try's time, a look at the machine comes back short: nothing is decided, and that is recorded.
	if err := pass(t0.Add(11*time.Minute), errors.New("tmux panes: timed out")); err != nil || len(ty.sent) != 0 {
		t.Fatalf("err %v, sent %q", err, ty.sent)
	}
	kinds := map[string]int{}
	evs, _ := st.Events(ctx, store.Query{Since: time.Unix(0, 0), Source: store.Supervise})
	for _, e := range evs {
		kinds[e.Kind]++
	}
	if len(evs) != 2 || kinds[supervise.KindWait] != 1 || kinds[supervise.KindSkip] != 1 {
		t.Fatalf("supervise events %+v", evs)
	}
	// The next whole scan acts.
	if err := pass(t0.Add(12*time.Minute), nil); err != nil || len(ty.sent) != 1 || !strings.HasPrefix(ty.sent[0], supervise.Marker) {
		t.Fatalf("err %v, sent %q", err, ty.sent)
	}
}

type noAccess struct{}

func (noAccess) AccessBack(string, time.Time) (time.Time, bool) { return time.Time{}, false }
