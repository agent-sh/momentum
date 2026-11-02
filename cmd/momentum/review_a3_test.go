package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func TestEvidenceWithholdsEveryRunWritableRoot(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_LOOP_DIR", filepath.Join(t.TempDir(), "current"))
	for _, root := range []string{os.Getenv("MOMENTUM_LOOP_DIR"), filepath.Join(loopDir(), "runs", "earlier"),
		filepath.Join(loopDir(), "work", "case"), filepath.Join(loopDir(), "cache"), filepath.Join(loopDir(), "exports"), filepath.Join(loopDir(), "tmp")} {
		os.MkdirAll(root, 0700)
		record := filepath.Join(root, "ordinary.log")
		os.WriteFile(record, []byte("forged-marker-31b8\n"), 0600)
		for _, ref := range []string{record, filepath.Join(t.TempDir(), "alias")} {
			if ref != record {
				os.Symlink(record, ref)
			}
			e := store.Event{Source: store.Transcripts, SourceKey: ref, TS: time.Now(), Actor: store.ActorTool,
				Kind: "result", Ref: ref + "@0"}
			putEvents(t, e)
			if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out+diag, "forged-marker") ||
				!strings.Contains(out, "(record withheld: it lies where a run can write)") {
				t.Fatalf("writable record accepted: %d %q %s", code, out, diag)
			}
		}
	}
}

func TestPrintDoneCleansBothFormatsAndFailsClosed(t *testing.T) {
	fakeScrubber(t)
	d := loop.Done{Family: "failures", Case: "c1", Step: "review", Run: store.LoopRun{ID: "r1", Status: store.RunOK,
		Summary: "marker-7f3a"}, Refused: []string{"marker-7f3a"}}
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		if err := printDone(&out, json.NewEncoder(&out), d, asJSON); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "marker-7f3a") || !strings.Contains(out.String(), "[masked]") {
			t.Fatalf("unclean loop output: %s", out.String())
		}
		if asJSON && !json.Valid(out.Bytes()) {
			t.Fatalf("cleaning broke JSON: %s", out.String())
		}
	}
	loop.Scrubber = []string{filepath.Join(t.TempDir(), "missing")}
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		err := printDone(&out, json.NewEncoder(&out), d, asJSON)
		if err == nil || out.Len() != 0 {
			t.Fatalf("failed cleaner printed output: %s", out.String())
		}
	}
}

func TestRunCaseNotesAreCleanedBeforePersistence(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(context.Background(), store.Finding{Family: "failures", Signature: "fixture", Title: "fixture"}, "test")
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_CASE", c.ID)
	if code, _, diag := momentum("case", "close", c.ID, "--reason", "not-worth-it", "--why", "marker-7f3a"); code != 0 {
		t.Fatal(diag)
	}
	st, err = store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log, err := st.CaseLog(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range log {
		if strings.Contains(entry.Detail, "marker-7f3a") {
			t.Fatalf("unclean case note: %+v", entry)
		}
		found = found || strings.Contains(entry.Detail, "[masked]")
	}
	if !found {
		t.Fatal("case note did not pass through the cleaner")
	}
	loop.Scrubber = []string{filepath.Join(t.TempDir(), "missing")}
	before := len(log)
	if code, out, diag := momentum("case", "reopen", c.ID, "--why", "marker-7f3a", "--evidence", "file:/fixture/new"); code == 0 ||
		strings.Contains(out+diag, "marker-7f3a") {
		t.Fatalf("failed cleaner changed a case or displayed its input: %d %s %s", code, out, diag)
	}
	log, _ = st.CaseLog(context.Background(), c.ID)
	if len(log) != before {
		t.Fatal("failed cleaner persisted a case note")
	}
}

func TestLoopRunsCleansStoredTextInBothFormats(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := store.LoopRun{ID: "fixture", Case: "c1", Family: "failures", Step: "build", Started: time.Now(),
		Ended: time.Now(), Status: store.RunFailed, Failure: "marker-7f3a", Summary: "marker-7f3a"}
	if err := st.PutLoopRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"loop", "runs"}, {"loop", "runs", "--json"}} {
		if code, out, diag := momentum(args...); code != 0 || strings.Contains(out+diag, "marker-7f3a") || !strings.Contains(out, "[masked]") {
			t.Fatalf("unclean run listing: %d %s %s", code, out, diag)
		}
	}
	saved, _ := st.LoopRunByID(context.Background(), run.ID)
	if saved.Failure != run.Failure {
		t.Fatal("display cleaning changed a stored stop-rule key")
	}
}
