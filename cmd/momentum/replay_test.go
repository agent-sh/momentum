package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

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
