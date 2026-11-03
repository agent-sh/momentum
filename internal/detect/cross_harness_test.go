package detect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestUnflaggedFailuresAcrossHarnesses(t *testing.T) {
	for _, tc := range []struct{ harness, tool, text string }{
		{"codex", "exec_command", "Chunk ID: marker-7f3a Wall time: 0.2 seconds Process exited with code 1 Final output: cat: neutral.txt: No such file or directory"},
		{"codex", "exec_command", "Command failed due to container SandboxDenied: operation not permitted"},
		{"codex", "apply_patch", "patch rejected: write outside of project; rejected by user approval settings"},
		{"codex", "exec_command", `{"stdout":"","stderr":"cat: neutral.txt: No such file or directory","exit_code":1}`},
		{"codex", "exec", `{"stdout":"","stderr":"cat: neutral.txt: No such file or directory","exit_code":1}`},
		{"codex", "exec_command", `{"output":"cat: neutral.txt: No such file or directory","metadata":{"exit_code":1}}`},
		{"hermes", "terminal", `{"output":"cat: neutral.txt: No such file or directory","exit_code":1}`},
		{"hermes", "terminal", `{"status":"error","error":"marker-7f3a: permission denied"}`},
		{"hermes", "terminal", `{"status":"error","error":{"message":"marker-7f3a: permission denied"}}`},
		{"hermes", "terminal", `{"status":"error","message":"marker-7f3a: permission denied"}`},
		{"opencode", "bash", "Exit code: 1 cat: neutral.txt: No such file or directory"},
		{"grok", "bash", "Exit code: 1 cat: neutral.txt: No such file or directory"},
		{"kimi", "Shell", "Exit code: 1 cat: neutral.txt: No such file or directory"},
	} {
		t.Run(tc.harness+"/"+tc.tool+"/"+tc.text[:8], func(t *testing.T) {
			st := testStore(t)
			for i := range 3 {
				e := ev(store.Transcripts, fmt.Sprintf("s%d", i%2), "tool_result", store.ActorTool, tc.tool, tc.text, "", t0.Add(time.Duration(i)*time.Minute))
				e.Harness = tc.harness
				put(t, st, e)
			}
			fs, err := (Failures{}).Detect(context.Background(), st, week())
			if err != nil || len(fs) != 1 || len(fs[0].Evidence) != 3 {
				t.Fatalf("unflagged failure missed: %d findings, %v", len(fs), err)
			}
		})
	}
}

func TestSuccessfulAndCancelledResultsExcluded(t *testing.T) {
	for _, text := range []string{
		"Process exited with code 0 Output: connection refused is a marker in this fixture",
		`{"output":"permission denied is a marker in this fixture","exit_code":0}`,
		"aborted by user after 2s",
		"Tool execution cancelled by user",
		"Error: User rejected the tool call",
		"cat: neutral.txt: No such file or directory", // no status or flag proves a failure
	} {
		st := testStore(t)
		for i := range 3 {
			e := ev(store.Transcripts, fmt.Sprintf("s%d", i%2), "tool_result", store.ActorTool, "exec_command", text, "", t0.Add(time.Duration(i)*time.Minute))
			e.Harness = "codex"
			put(t, st, e)
		}
		if fs, err := (Failures{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
			t.Fatalf("no failure %q counted: %d %v", text, len(fs), err)
		}
	}
	for _, text := range []string{"aborted by user after 2s", "Tool execution cancelled by user", "Error: User rejected the tool call"} {
		if _, ok := FailureOf("codex", "apply_patch", text, ""); ok {
			t.Fatalf("cancelled result keyed: %q", text)
		}
	}
}

func TestOwnerAndSessionDetectorsAcrossHarnesses(t *testing.T) {
	for _, harness := range store.Harnesses {
		t.Run(harness, func(t *testing.T) {
			st := testStore(t)
			for i := range 3 {
				session := fmt.Sprintf("%s-marker-%d", harness, i%2)
				at := t0.Add(time.Duration(i) * 3 * time.Hour)
				a := ev(store.Transcripts, session, "agent_text", store.ActorAgent, "", "Should I check marker-7f3a?", "", at)
				u := ev(store.Transcripts, session, "owner_text", store.ActorOwner, "", "Do it yourself.", `{"input_kind":"typed"}`, at.Add(time.Minute))
				a.Harness, u.Harness = harness, harness
				put(t, st, a, u)
			}
			fs, err := (OwnerBurden{}).Detect(context.Background(), st, week())
			if err != nil || len(fs) != 1 || len(fs[0].Evidence) != 3 {
				t.Fatalf("owner burden: %v %+v", err, fs)
			}
			st2 := testStore(t)
			for i := range 3 {
				stopped(t, st2, harness, fmt.Sprintf("%s-nudge-%d", harness, i%2), t0.Add(time.Duration(i)*3*time.Hour))
			}
			nudges, err := (Sessions{}).Detect(context.Background(), st2, week())
			if err != nil || len(nudges) != 1 || len(nudges[0].Evidence) != 3 {
				t.Fatalf("session nudge: %v %+v", err, nudges)
			}
		})
	}
}

func TestFailuresReadAllResultPages(t *testing.T) {
	st := testStore(t)
	var events []store.Event
	for i := range 3 {
		e := ev(store.Transcripts, fmt.Sprintf("s%d", i%2), "tool_result", store.ActorTool, "exec_command",
			"Exit code: 1 cat: neutral.txt: No such file or directory", "", t0)
		e.Harness = "codex"
		events = append(events, e)
	}
	for range resultPage {
		events = append(events, ev(store.Transcripts, "success", "tool_result", store.ActorTool, "Read", "marker-7f3a", "", t0))
	}
	put(t, st, events...)
	fs, err := (Failures{}).Detect(context.Background(), st, week())
	if err != nil || len(fs) != 1 || len(fs[0].Evidence) != 3 {
		t.Fatalf("older failures hidden by successful page: %d %v", len(fs), err)
	}
}

func TestJSONFileContentAndWorkFailuresExcluded(t *testing.T) {
	for _, tc := range []struct{ harness, tool, text string }{
		{"codex", "read_file", `{"error":"marker-7f3a","exit_code":1}`},
		{"kimi", "ReadFile", `{"status":"error","text":"marker-7f3a"}`},
		{"grok", "read_file", `{"error":"marker-7f3a"}`},
		{"hermes", "terminal", `{"error":"--- FAIL: TestMarker","exit_code":1}`},
		{"codex", "exec_command", `{"stdout":"--- FAIL: TestMarker","stderr":"cat: neutral.txt: No such file or directory","exit_code":1}`},
	} {
		st := testStore(t)
		for i := range 3 {
			e := ev(store.Transcripts, fmt.Sprintf("s%d", i%2), "tool_result", store.ActorTool, tc.tool, tc.text, "", t0.Add(time.Duration(i)*time.Minute))
			e.Harness = tc.harness
			put(t, st, e)
		}
		if fs, err := (Failures{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
			t.Fatalf("ordinary content or work failure counted: %s %d %v", tc.harness, len(fs), err)
		}
	}
}
