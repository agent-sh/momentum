package main

import (
	"context"
	"database/sql"
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

func TestProposalBodyUsesItsOpenedDescriptor(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "body.md")
	os.WriteFile(body, []byte("original marker-31b8"), 0600)
	f, err := loop.OpenFile(dir, "body.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Stat(); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(t.TempDir(), "denied.md")
	os.WriteFile(denied, []byte("denied marker-9c2b"), 0600)
	if err := os.Rename(body, filepath.Join(dir, "old.md")); err != nil {
		t.Fatal(err)
	}
	os.Symlink(denied, body)
	if got, err := readBodyFile(f); err != nil || got != "original marker-31b8" {
		t.Fatalf("path replacement changed the descriptor read: %q %v", got, err)
	}
	if _, err := readBody(body); err == nil {
		t.Fatal("a symlink at open time was accepted")
	}
	// An in-root symlink is refused too.
	os.Symlink("old.md", filepath.Join(dir, "local.md"))
	if _, err := readBody(filepath.Join(dir, "local.md")); err == nil {
		t.Fatal("an in-root symlink was accepted")
	}
	os.WriteFile(filepath.Join(dir, "large.md"), []byte(strings.Repeat("x", bodyMax+1)), 0600)
	if _, err := readBody(filepath.Join(dir, "large.md")); err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err == nil {
		if _, err := readBody(filepath.Join(dir, "pipe")); err == nil {
			t.Fatal("a pipe was accepted as a body")
		}
	}
}

func TestAFailingLessonBodyCleanerCallsNoPublisher(t *testing.T) {
	TestALessonIsTheLoopsObservation(t)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _, err := st.ObserveCase(context.Background(), store.Finding{Family: "failures", Signature: "body-refusal", Title: "marker-7f3a",
		Evidence: []store.Evidence{{Ref: "file:/fixture/record", At: time.Now()}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET status = ?, reason = ? WHERE id = ?`, store.CaseClosed, store.Improved, c.ID); err != nil {
		t.Fatal(err)
	}
	bin := strings.Split(os.Getenv("PATH"), ":")[0]
	args := filepath.Join(bin, "args")
	os.Remove(args)
	scrub := filepath.Join(bin, "body-cleaner")
	os.WriteFile(scrub, []byte("#!/bin/sh\ndata=$(cat)\ncase \"$data\" in Learned*) exit 1;; esac\nprintf '%s' \"$data\"\n"), 0700)
	loop.Scrubber = []string{scrub}
	if code, _, _ := momentum("loop", "lesson", "--case", c.ID, "--text", "a clean lesson"); code == 0 {
		t.Fatal("the fixture did not refuse the body cleaner")
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatalf("publisher was invoked after body-cleaner failure: %v", err)
	}
}
