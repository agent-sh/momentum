package main

import (
	"context"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScrubber stands for the secret scrubber: it masks the word secret-value, as scrump masks a secret.
func fakeScrubber(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "scrub")
	os.WriteFile(p, []byte("#!/bin/sh\nsed -e 's/secret-value/************/g' -e 's/marker-7f3a/[masked]/g'\n"), 0o755)
	was := loop.Scrubber
	loop.Scrubber = []string{p}
	t.Cleanup(func() { loop.Scrubber = was })
}

func TestEvidenceShowScrubsWholeRecordBeforeBytesAndAcceptsTenIDs(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	record := filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(record, []byte("secret-value tail-marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e := store.Event{Source: store.Transcripts, SourceKey: "record", TS: time.Now().Add(-time.Minute), Actor: store.ActorTool, Kind: "tool_result", Summary: "marker-7f3a", Ref: record + "@0"}
	if _, err := st.Put(context.Background(), []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--bytes", "6", e.ID()}, {e.ID(), "--bytes=6"}} {
		code, out, diag := momentum(append([]string{"evidence", "show"}, args...)...)
		if code != 0 || strings.Contains(out, "secret") || !strings.Contains(out, "******") || !strings.Contains(out, "first 6 of") {
			t.Fatalf("bytes cut before scrub: %d %q %s", code, out, diag)
		}
	}
	args := []string{"evidence", "show"}
	for i := 0; i < 10; i++ {
		args = append(args, e.ID())
	}
	code, out, diag := momentum(args...)
	if code != 0 || strings.Count(out, "tail-marker") != 10 {
		t.Fatalf("multi-id show: %d %q %s", code, out, diag)
	}
	if code, _, _ := momentum(append(args, e.ID())...); code != 2 {
		t.Fatal("accepted eleven IDs")
	}
}
