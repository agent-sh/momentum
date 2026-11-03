package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/ro"
	"github.com/agent-sh/momentum/internal/store"
)

func TestReadOnlyDispatchUsesExecutableNameNeverArgvZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "momentum-ro")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
	host := filepath.Join(dir, "momentum")
	in, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(host, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	// Only fixture values enter these children. A poisoned config would fail
	// any host command that loaded it.
	cfg := filepath.Join(state, "config")
	if err := os.Mkdir(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte("invalid fixture config"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"evidence", "ingest"}, {"evidence", "rebuild", "fixture"}, {"rebuild"},
		{"case", "open"}, {"loop"}, {"ship"}, {"unknown"}, {"version"}} {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Args[0] = "momentum"
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + state, "MOMENTUM_HOME=" + filepath.Join(state, "store"), "MOMENTUM_CONFIG=" + cfg}
		got, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || strings.TrimSpace(string(got)) != ro.Refusal {
			t.Fatalf("name dispatch %v: %v %q", args, err, got)
		}
	}
	// The built reader also exercises every read form, with no scrubber in its
	// PATH and a config that must remain unread.
	copies := t.TempDir()
	source := filepath.Join(copies, "source.db")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	event := store.Event{Source: "fixture", SourceKey: "read", TS: time.Now().Add(-time.Minute), Actor: store.ActorTool,
		Kind: "tool_result", Summary: "marker-7f3a summary", Ref: "/fixture/not-opened@0"}
	if _, err := st.Put(ctx, []store.Event{event}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	scrub := filepath.Join(copies, "scrub")
	if err := os.WriteFile(scrub, []byte("#!/bin/sh\nsed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	was := loop.Scrubber
	loop.Scrubber = []string{scrub}
	t.Cleanup(func() { loop.Scrubber = was })
	snapshot := filepath.Join(copies, "store.sqlite")
	if _, err := loop.StoreCopy(ctx, loop.StoreCopyOptions{Source: source, Destination: snapshot, ScratchDir: copies, Until: time.Now()}); err != nil {
		t.Fatal(err)
	}
	record := "marker-7f3a captured\n"
	for _, name := range []string{"ev-" + event.ID() + ".txt", "unit-fixture.service.txt"} {
		if err := os.WriteFile(filepath.Join(copies, name), []byte(record), 0444); err != nil {
			t.Fatal(err)
		}
	}
	caseFile := filepath.Join(copies, "case.json")
	raw := `{"case":{"ID":"cfixture","Title":"marker-7f3a case","Family":"failures","Status":"open"},"evidence":[],"log":[]}`
	if err := os.WriteFile(caseFile, []byte(raw), 0444); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"evidence", "events"}, "tool_result"},
		{[]string{"evidence", "show", "ev:" + event.ID()}, record},
		{[]string{"evidence", "status"}, "tool_result 1"},
		{[]string{"evidence", "unit", "fixture.service"}, record},
		{[]string{"case", "show"}, "cfixture  marker-7f3a case"},
		{[]string{"case", "show", "--json"}, raw},
	} {
		args := append(tc.args, "--store", snapshot, "--records", copies, "--case", caseFile)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Args[0] = "momentum"
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + state, "MOMENTUM_HOME=" + filepath.Join(state, "store"), "MOMENTUM_CONFIG=" + cfg}
		got, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(got), tc.want) {
			t.Fatalf("built reader %v: %v %q", tc.args, err, got)
		}
	}
	cmd := exec.CommandContext(ctx, host, "version")
	cmd.Args[0] = "momentum-ro"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + state}
	got, err := cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(got), "momentum ") {
		t.Fatalf("a renamed copy stayed in read-only mode: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(state, "store")); !os.IsNotExist(err) {
		t.Fatal("refused executable reached a host store")
	}
}

func TestEvidenceUnitOneScrubberStartAndWholeBeforeTail(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "starts")
	scrub := filepath.Join(dir, "scrub")
	if err := os.WriteFile(scrub, []byte("#!/bin/sh\nprintf 'start\\n' >> '"+count+"'\nsed 's/marker-7f3a/[masked]/g'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	was := loop.Scrubber
	loop.Scrubber = []string{scrub}
	t.Cleanup(func() { loop.Scrubber = was })
	for _, program := range []string{"systemctl", "journalctl"} {
		if err := os.WriteFile(filepath.Join(dir, program), []byte("#!/bin/sh\nprintf 'marker-7f3a whole\\nend marker-7f3a\\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("MOMENTUM_NOW", "")
	code, out, diag := momentum("evidence", "unit", "fixture.service", "--bytes", "12")
	if code != 0 || strings.Contains(out, "marker-7f3a") || !strings.Contains(out, "last") || !strings.Contains(out, "[masked]") {
		t.Fatalf("unit scrub and tail: %d %q %s", code, out, diag)
	}
	starts, err := os.ReadFile(count)
	if err != nil || string(starts) != "start\n" {
		t.Fatalf("unit starts: %q %v", starts, err)
	}
}
