package main

import (
	"context"

	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// What the evidence commands print passes the scrubber first, all of it: git's output and its diagnostics, an
// event's fields and its record. A credential file is in no history query, a blob selector of one is refused, and a
// scrubber that cannot run prints nothing.
func TestEveryEvidenceOutputIsScrubbed(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	record := filepath.Join(t.TempDir(), "record.log")
	os.WriteFile(record, []byte("secret-value\n"), 0600)
	e := store.Event{Source: store.Transcripts, SourceKey: "synthetic", TS: time.Now(), Kind: "result", Actor: store.ActorTool, Summary: "secret-value", Ref: record + "@0"}
	if _, err := st.Put(context.Background(), []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out+errOut, "secret-value") {
		t.Errorf("raw event %d %q %s", code, out, errOut)
	}
	loop.Scrubber = []string{filepath.Join(t.TempDir(), "absent")}
	for _, args := range [][]string{{"evidence", "show", e.ID()}} {
		if code, out, _ := momentum(args...); code == 0 || out != "" {
			t.Errorf("failed scrub emitted bytes: %d %q", code, out)
		}
	}
}

// A record is scrubbed whole before any of it is selected: an event's long record keeps its end, and a unit's output
// too long to scrub whole is withheld.
func TestOnlyWholeRecordsAreScrubbedAndShown(t *testing.T) {
	fakeScrubber(t)
	scrub := filepath.Join(t.TempDir(), "scrub")
	os.WriteFile(scrub, []byte("#!/bin/sh\nsed 's/marker-zq7vx9/[masked]/g'\n"), 0700)
	loop.Scrubber = []string{scrub}
	file := filepath.Join(t.TempDir(), "ordinary.log")
	value := "zq7vx9"
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	os.WriteFile(file, []byte(strings.Repeat("z", 70<<10)+" marker-"+value+" END\n"), 0600)
	e := store.Event{Source: store.Transcripts, SourceKey: "long", TS: time.Now(), Kind: "result", Actor: store.ActorTool, Ref: file + "@0"}
	st.Put(context.Background(), []store.Event{e})
	if code, out, errOut := momentum("evidence", "show", e.ID()); code != 0 || !strings.Contains(out, "END") || strings.Contains(out, value) {
		t.Errorf("partial event %d len=%d %s", code, len(out), errOut)
	}
	os.WriteFile(file, []byte(strings.Repeat("z", readMax+1)+"marker-"+value), 0600)
	// Unit output may be capped only by withholding the whole result.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncat '"+file+"'\n"), 0700)
	os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if code, out, _ := momentum("evidence", "unit", "fixture.service"); code == 0 || out != "" {
		t.Error("oversized unit output was emitted")
	}
}

// An event's record is read only from a regular file, opened without waiting on it: a reference that now names a
// pipe returns at once, with its record withheld.
func TestAnEventsRecordIsReadOnlyFromAFile(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	fifo := filepath.Join(t.TempDir(), "record")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no fifo here:", err)
	}
	e := store.Event{Source: store.Transcripts, SourceKey: "pipe", TS: time.Now().Add(-time.Minute), Session: "s1", Actor: store.ActorTool,
		Kind: "tool_result", Summary: "a result", Ref: fifo + "@0"}
	putEvents(t, e)
	type res struct {
		code     int
		out, err string
	}
	ch := make(chan res, 1)
	go func() {
		code, out, errOut := momentum("evidence", "show", e.ID())
		ch <- res{code, out, errOut}
	}()
	select {
	case got := <-ch:
		if got.code != 0 || !strings.Contains(got.out, "not a regular file") {
			t.Fatalf("exit %d %q %s", got.code, got.out, got.err)
		}
	case <-time.After(5 * time.Second):
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0) // let the blocked reader go before failing
		if err == nil {
			w.Close()
		}
		t.Fatal("evidence show waited on a pipe")
	}
}
