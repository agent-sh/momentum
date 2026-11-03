package main

import (
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

func TestEvidenceShowOneScrubberStartForAllHeadersAndWholeRecords(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "starts")
	scrub := filepath.Join(dir, "scrub")
	if err := os.WriteFile(scrub, []byte("#!/bin/sh\nprintf 'start\\n' >> '"+count+"'\nsed 's/marker-7f3a/[masked]/g'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	was := loop.Scrubber
	loop.Scrubber = []string{scrub}
	t.Cleanup(func() { loop.Scrubber = was })
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	record := filepath.Join(dir, "record")
	if err := os.WriteFile(record, []byte("marker-7f3a full record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"evidence", "show", "--bytes", "4"}
	for _, key := range []string{"one", "two", "three"} {
		ev := store.Event{Source: "fixture", SourceKey: key, TS: time.Now(), Actor: store.ActorTool, Kind: "tool_result", Summary: "marker-7f3a", Ref: record + "@0"}
		if _, err := st.Put(context.Background(), []store.Event{ev}); err != nil {
			t.Fatal(err)
		}
		args = append(args, ev.ID())
	}
	args = append(args, "missing-marker-7f3a")
	code, out, diag := momentum(args...)
	if code != 0 || strings.Contains(out, "marker-7f3a") || strings.Count(out, "[mas\n") != 3 || strings.Count(out, "first 4 of") != 3 {
		t.Fatalf("batch scrub and clip: %d %q %s", code, out, diag)
	}
	starts, err := os.ReadFile(count)
	if err != nil || string(starts) != "start\n" {
		t.Fatalf("wanted one scrubber start, got %q: %v", starts, err)
	}
}
