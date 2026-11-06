package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/supervise"
)

// A labelled step on a local command is the one measured at its stop, and a later labelled step there counts with
// it: the redundant-nudge bar counts every nudge, the label counts find the later step, the points file names both,
// and the anchors measure to the step, at a last stop too.
func TestReplayOutputsCountEveryLabelledStepAtAStop(t *testing.T) {
	at := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	labels := []supervise.Label{{Session: "s", At: at.Add(3 * time.Minute), Kind: "restart"}, {Session: "s", At: at.Add(4 * time.Minute), Kind: "nudge"},
		{Session: "t", At: at.Add(8 * time.Minute), Kind: "restart"}}
	decision := supervise.Decision{Rule: supervise.RuleTransient, Verdict: supervise.Hold, At: at.Add(10 * time.Minute)}
	rs := []supervise.Result{
		{Point: supervise.Point{Session: "s", Stop: supervise.Stop{Tasks: 1}, Next: supervise.Next{At: labels[1].At, Kind: "owner"}},
			Decision: decision, Label: &labels[0], StepAt: labels[0].At, Also: []*supervise.Label{&labels[1]}, Outcome: supervise.OwnerFirst},
		{Point: supervise.Point{Session: "t", Next: supervise.Next{Kind: "none"}},
			Decision: decision, Label: &labels[2], StepAt: labels[2].At, Outcome: supervise.OwnerFirst},
	}
	var out bytes.Buffer
	printRedundant(&out, rs)
	if !strings.Contains(out.String(), ": 1 of 1 (100%)") {
		t.Fatalf("the nudge after a labelled restart is not counted: %q", out.String())
	}
	// an exited stop: the process was gone, so the owner's input that reopens it is no redundant nudge
	exited := []supervise.Result{{Point: supervise.Point{Session: "x", Stop: supervise.Stop{Exited: true, Loop: "*/10 * * * *"},
		Next: supervise.Next{At: labels[1].At, Kind: "owner"}}, Label: &labels[1], StepAt: labels[1].At}}
	out.Reset()
	printRedundant(&out, append(exited, rs...))
	if !strings.Contains(out.String(), ": 1 of 1 (100%)") {
		t.Fatalf("a nudge on an exited stop counted: %q", out.String())
	}
	out.Reset()
	printReplay(&out, rs, labels, at.Add(-time.Hour), at.Add(time.Hour))
	if !strings.Contains(out.String(), "nudge steps: 1 (0 not in the store)") || !strings.Contains(out.String(), "restart steps: 2 (0 not in the store)") {
		t.Fatalf("the label counts: %s", out.String())
	}
	out.Reset()
	printAnchors(&out, rs)
	if !strings.Contains(out.String(), "owner restart      2    -7.0") {
		t.Fatalf("the anchors do not measure to the step: %s", out.String())
	}
	path := filepath.Join(t.TempDir(), "points.jsonl")
	if err := writePoints(path, rs); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"also":["nudge"]`) || !strings.Contains(string(raw), `"step_at":"2026-03-04T10:03:00Z"`) {
		t.Fatalf("the points: %s %v", raw, err)
	}
}
