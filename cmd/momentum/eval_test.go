package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func TestEvaluationRecordChildWithholdsLaterAndHermesRows(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	end := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	for _, sample := range []struct {
		name, harness, source, text string
		withheld                    bool
	}{
		{"before", "claude", store.Transcripts, `{"timestamp":"2026-03-02T09:59:59Z","text":"earlier record"}`, false},
		{"tie", "claude", store.Transcripts, `{"timestamp":"2026-03-02T10:00:00Z","text":"later answer"}`, true},
		{"after", "claude", store.Transcripts, `{"timestamp":"2026-03-02T11:00:00Z","text":"later answer"}`, true},
		{"undated-place", "claude", store.Sessions, `{"text":"later answer"}`, true},
		{"hermes", "hermes", store.Transcripts, `{"timestamp":"2026-03-02T09:59:59Z","text":"later answer"}`, true},
	} {
		path := filepath.Join(dir, sample.name)
		if err := os.WriteFile(path, []byte(sample.text+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		ev := store.Event{Source: sample.source, SourceKey: sample.name, Kind: "tool_result", Actor: store.ActorTool, TS: end.Add(-time.Minute),
			Harness: sample.harness, Ref: path + "@0", Summary: "fixture summary"}
		if _, err := st.Put(context.Background(), []store.Event{ev}); err != nil {
			t.Fatal(err)
		}
		code, out, diag := momentum("evidence", "show", ev.ID())
		if code != 0 || strings.Contains(out, "later answer") || strings.Contains(out, "withheld") != sample.withheld {
			t.Fatalf("as-of child %s: %d %s %s", sample.name, code, out, diag)
		}
	}
	t.Setenv("MOMENTUM_NOW", "invalid")
	if code, _, diag := momentum("evidence", "show", "fixture"); code != 2 || !strings.Contains(diag, "MOMENTUM_NOW") {
		t.Fatal("invalid evaluation clock silently became live")
	}
}

func TestEvaluationUnitChildUsesOnlyTheHistoricalJournal(t *testing.T) {
	fakeScrubber(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	for _, program := range []string{"systemctl", "journalctl"} {
		script := "#!/bin/sh\nprintf '%s\\n' '" + program + "' \"$@\" >> '" + log + "'\nprintf 'fixture output\\n'\n"
		if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MOMENTUM_NOW", "2026-03-02T10:00:00Z")
	code, _, diag := momentum("evidence", "unit", "fixture.service")
	raw, err := os.ReadFile(log)
	if code != 0 || err != nil || strings.Contains(string(raw), "systemctl") ||
		!strings.Contains(string(raw), "--until\n@1772445599.999999") {
		t.Fatalf("historical journal: %d %s %s %v", code, raw, diag, err)
	}
}

func TestEvaluationCLIRejectsUnknownGoldenKeysAndInvalidSplits(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("MOMENTUM_HOME", dir)
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	path := filepath.Join(dir, "golden.jsonl")
	task := `{"id":"fixture","family":"failures","case_title":"worker failed","symptom":{"query":{"event_ids":["e1"]}},"cutoff":"2026-03-02T10:00:00Z","root_cause":"cause","key_facts":["fact"],"wrong_path":[]}`
	if err := os.WriteFile(path, []byte(task), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := momentum("loop", "eval", "--golden", path); code != 2 {
		t.Fatal("unknown golden key reached setup")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(task, "wrong_path", "wrong_paths", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--split", "other"}, {"--split", "held-out"}, {"--held-out", "absent"}, {"--task", "absent"},
		{"--arms", "loop,loop"}, {"--parallel", "0"}, {"--budget", "NaN"}} {
		args := append([]string{"loop", "eval", "--golden", path}, flags...)
		if code, _, _ := momentum(args...); code != 2 {
			t.Fatal("invalid evaluation flags reached setup")
		}
	}
	if _, err := os.Stat(paths.DB()); !os.IsNotExist(err) {
		t.Fatal("refused evaluation opened the store")
	}
}

func TestEvaluationPrintsUndefinedRatesAndFailedStartReason(t *testing.T) {
	var out bytes.Buffer
	printEvaluation(&out, loop.EvaluationResults{Arms: []loop.ArmSummary{{Arm: "loop", K: 1, Errors: 1}},
		Trials: []loop.Trial{{Arm: "loop", Err: "fixture sandbox did not start"}}})
	if !strings.Contains(out.String(), "pass@k undefined, pass^k undefined") ||
		!strings.Contains(out.String(), "error: fixture sandbox did not start") {
		t.Fatal("undefined rates or startup error hidden")
	}
}
