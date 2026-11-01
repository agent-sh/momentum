package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
)

func TestTheBarDoesNotCountNudgesToATurnWaitingOnARequest(t *testing.T) {
	at := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	nudge := &supervise.Label{Kind: "nudge"}
	rs := []supervise.Result{
		{Point: supervise.Point{Stop: supervise.Stop{Tasks: 1}}, Label: nudge},                // a task would have woken it
		{Point: supervise.Point{Stop: supervise.Stop{Tasks: 1, RequestAt: at}}, Label: nudge}, // a stalled turn no task moves
		{Point: supervise.Point{Stop: supervise.Stop{}}, Label: nudge},
	}
	var out bytes.Buffer
	printRedundant(&out, rs)
	if !strings.Contains(out.String(), ": 1 of 3 (33%)") {
		t.Fatalf("got %q", out.String())
	}
}

// A run reads replay output, never an export containing the store's raw text.
func TestAReplayInARunRefusesPoints(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_DIR", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(-3 * time.Hour)
	evs := []store.Event{
		{Source: store.Transcripts, SourceKey: "o1", Session: "fixture", Harness: "claude", Kind: "owner_text", Actor: store.ActorOwner, TS: at, Summary: "do the task"},
		{Source: store.Transcripts, SourceKey: "a1", Session: "fixture", Harness: "claude", Kind: "agent_text", Actor: store.ActorAgent, TS: at.Add(time.Minute), Summary: "I will continue. password=secret-value"},
		{Source: store.Transcripts, SourceKey: "o2", Session: "fixture", Harness: "claude", Kind: "owner_text", Actor: store.ActorOwner, TS: at.Add(time.Hour), Summary: "continue password=secret-value"},
	}
	if _, err = st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("MOMENTUM_LOOP_DIR"), "points.jsonl")
	code, out, diag := momentum("supervise", "replay", "--since", "1d", "--points", path)
	if code == 0 || !strings.Contains(diag, "--points is refused") {
		t.Fatalf("points accepted: %d %s %s", code, out, diag)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("points file created: %v", err)
	}
	for _, args := range [][]string{{"supervise", "replay", "--since", "1d"}, {"supervise", "replay", "--since", "1d", "--anchor"}} {
		code, out, diag := momentum(args...)
		if code != 0 || out == "" || strings.Contains(out+diag, "secret-value") {
			t.Fatalf("replay output: %d %s %s", code, out, diag)
		}
	}
}

// A points symlink is refused without touching its target, even when it is inside the run directory.
func TestAReplayInARunRefusesAPointsSymlink(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	dir := t.TempDir()
	t.Setenv("MOMENTUM_LOOP_DIR", dir)
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "points.jsonl")
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	code, _, diag := momentum("supervise", "replay", "--points", p)
	raw, err := os.ReadFile(outside)
	if code == 0 || !strings.Contains(diag, "--points is refused") || err != nil || string(raw) != "keep" {
		t.Fatalf("points symlink: %d %q %s %v", code, raw, diag, err)
	}
}

// A run refuses labels before opening a FIFO. It does not spend its timeout waiting for a writer.
func TestAReplayInARunRefusesLabelsBeforeOpening(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	fifo := filepath.Join(t.TempDir(), "labels")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	type result struct {
		code int
		diag string
	}
	done := make(chan result, 1)
	go func() {
		var out, diag bytes.Buffer
		code := run(ctx, []string{"supervise", "replay", "--labels", fifo}, &out, &diag)
		done <- result{code, diag.String()}
	}()
	select {
	case got := <-done:
		if got.code == 0 || !strings.Contains(got.diag, "--labels is refused") {
			t.Fatalf("labels: %+v", got)
		}
	case <-time.After(time.Second):
		// Release the synthetic reader if a regression opened the FIFO, so the test leaves no blocked goroutine.
		w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if err == nil {
			w.Write([]byte("\n"))
			w.Close()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("fixture reader did not finish")
		}
		t.Fatal("labels open blocked beyond cancellation")
	}
	for _, arg := range []string{"--labels=", "--points="} {
		code, _, diag := momentum("supervise", "replay", arg)
		if code == 0 || !strings.Contains(diag, "is refused") {
			t.Fatalf("empty file flag accepted: %s %d %s", arg, code, diag)
		}
	}
}
