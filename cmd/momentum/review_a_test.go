package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func needEvidenceBwrap(t *testing.T) {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bubblewrap on this machine")
	}
	if out, err := exec.Command(bwrap, "--unshare-all", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot make a sandbox here: %v: %s", err, out)
	}
}

func TestEvidenceGitCannotFollowObjectAndRefLinks(t *testing.T) {
	needEvidenceBwrap(t)
	fakeScrubber(t)
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	if err := os.MkdirAll(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	denied := evidenceRepo(t)
	if err := os.WriteFile(filepath.Join(denied, "private.txt"), []byte("denied-marker-9c2b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, denied, "add", ".")
	gitIn(t, denied, "commit", "-qm", "fixture")
	probe := filepath.Join(allowed, "probe")
	meta := filepath.Join(probe, ".git")
	if err := os.MkdirAll(meta, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"objects", "refs"} {
		if err := os.Symlink(filepath.Join(denied, ".git", name), filepath.Join(meta, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture reads the denied blob without filesystem confinement.
	if got := gitIn(t, probe, "show", "HEAD:private.txt"); !strings.Contains(got, "denied-marker-9c2b") {
		t.Fatal("fixture does not reach the denied repository")
	}
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_READ", allowed)
	if code, out, diag := momentum("evidence", "git", probe, "show", "HEAD:private.txt"); code == 0 || strings.Contains(out+diag, "denied-marker-9c2b") {
		t.Fatalf("linked storage escaped: %d %q %s", code, out, diag)
	}
	normal := filepath.Join(allowed, "normal")
	if err := os.MkdirAll(normal, 0700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, normal, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(normal, "ordinary.txt"), []byte("allowed-marker-31b8\n"), 0600)
	gitIn(t, normal, "add", ".")
	gitIn(t, normal, "commit", "-qm", "fixture")
	if code, out, diag := momentum("evidence", "git", normal, "show", "HEAD:ordinary.txt"); code != 0 || !strings.Contains(out, "allowed-marker-31b8") {
		t.Fatalf("normal repository: %d %q %s", code, out, diag)
	}
}

func TestEvidenceGitFailsClosedWithoutItsSandbox(t *testing.T) {
	fakeScrubber(t)
	dir := evidenceRepo(t)
	bin := t.TempDir()
	args := filepath.Join(bin, "sandbox-args")
	os.WriteFile(filepath.Join(bin, "bwrap"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+args+"'\necho 'fixture namespace refusal' >&2\nexit 1\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if code, out, diag := momentum("evidence", "git", dir, "show", "HEAD:ordinary.txt"); code == 0 || strings.Contains(out, "a value") ||
		!strings.Contains(diag, "sandbox") {
		t.Fatalf("sandbox failure fell back to host Git: %d %q %s", code, out, diag)
	}
	raw, err := os.ReadFile(args)
	if err != nil || !strings.Contains(string(raw), "--unshare-all\n") || strings.Contains(string(raw), "\n--bind\n") ||
		!strings.Contains(string(raw), "--ro-bind-fd\n") || !strings.Contains(string(raw), "\n"+dir+"\n") {
		t.Fatalf("Git was not confined read-only without a door or route: %q %v", raw, err)
	}
}

func TestGitSandboxDirectoryIsPinnedBeforeReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "probe")
	os.Mkdir(dir, 0700)
	os.WriteFile(filepath.Join(dir, "ordinary.txt"), []byte("original"), 0600)
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_READ", root)
	f, err := openGitDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	os.Rename(dir, filepath.Join(root, "old"))
	os.Symlink(t.TempDir(), dir)
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("the sandbox's mount changed with its pathname")
	}
	if _, err := openGitDirectory(dir); err == nil {
		t.Fatal("the replacement outside the read root was opened")
	}
}
func TestProposalBodyUsesItsOpenedDescriptor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOMENTUM_LOOP_DIR", dir)
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
	if _, err := readBody(body, "fixture"); err == nil {
		t.Fatal("a symlink at open time was accepted")
	}
	// An in-root symlink is refused too.
	os.Symlink("old.md", filepath.Join(dir, "local.md"))
	if _, err := readBody(filepath.Join(dir, "local.md"), "fixture"); err == nil {
		t.Fatal("an in-root symlink was accepted")
	}
	os.WriteFile(filepath.Join(dir, "large.md"), []byte(strings.Repeat("x", bodyMax+1)), 0600)
	if _, err := readBody(filepath.Join(dir, "large.md"), "fixture"); err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err == nil {
		if _, err := readBody(filepath.Join(dir, "pipe"), "fixture"); err == nil {
			t.Fatal("a pipe was accepted as a body")
		}
	}
}

func TestEvidenceRecordInARunsTreeCannotFollowLinks(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_DIR", dir)
	denied := filepath.Join(t.TempDir(), "record.log")
	os.WriteFile(denied, []byte("denied-marker-9c2b\n"), 0600)
	link := filepath.Join(dir, "records")
	os.Symlink(filepath.Dir(denied), link)
	e := store.Event{Source: store.Transcripts, SourceKey: "fixture", TS: time.Now(), Actor: store.ActorTool, Kind: "result",
		Ref: filepath.Join(link, "record.log") + "@0"}
	putEvents(t, e)
	if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out+diag, "denied-marker-9c2b") {
		t.Fatalf("record link escaped: %d %q %s", code, out, diag)
	}
	ordinary := filepath.Join(dir, "ordinary.log")
	os.WriteFile(ordinary, []byte("original-marker-31b8\n"), 0600)
	f, err := openRecord(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Rename(ordinary, ordinary+".old")
	os.Symlink(denied, ordinary)
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "original-marker-31b8\n" {
		t.Fatalf("record descriptor was reopened: %q %v", b, err)
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
