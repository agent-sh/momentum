package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/capacity"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestALessonIsTheLoopsObservation(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	bin := t.TempDir()
	args := filepath.Join(bin, "args")
	os.WriteFile(filepath.Join(bin, "tacit"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+args+"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:x", Title: "marker-7f3a keeps failing",
		Evidence: []store.Evidence{{Ref: "file:/t/x", At: time.Now().Add(-time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if code, _, errOut := momentum("loop", "lesson", "--case", c.ID, "--text", "a lesson"); code != 2 || !strings.Contains(errOut, "improved") {
		t.Fatalf("an open case's lesson: exit %d %s", code, errOut)
	}
	db, err := sql.Open("sqlite", "file:"+paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cases SET status = ?, reason = ? WHERE id = ?`, store.CaseClosed, store.Improved, c.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.MkdirAll(filepath.Join(paths.Data(), "loop", "packages", c.ID), 0o700)
	os.WriteFile(filepath.Join(paths.Data(), "loop", "packages", c.ID, "package.json"), []byte(`{"case":"`+c.ID+`","repo":"agent-sh/fake"}`), 0o600)
	if code, out, errOut := momentum("loop", "lesson", "--case", c.ID, "--text", "the key secret-value ends with @someone"); code != 0 || strings.Contains(out, "secret-value") {
		t.Fatalf("exit %d %q %s", code, out, errOut)
	}
	got, _ := os.ReadFile(args)
	if !strings.HasSuffix(string(got), "\nmomentum-lesson-"+strings.ToLower(c.ID)+"\n") {
		t.Errorf("tacit add lacks the lesson's name, its last argument:\n%s", got)
	}
	for _, want := range []string{"--harness\nclaude", "--kind\nobserved", "--scope\nprojects/fake"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("tacit add lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "secret-value") || strings.Contains(string(got), "marker-7f3a") || strings.Contains(string(got), "@someone") {
		t.Errorf("the lesson went unscrubbed:\n%s", got)
	}
	if !strings.Contains(string(got), "--body\nUnverified lesson from momentum's improvement loop") || !strings.Contains(string(got), "[masked] keeps failing") {
		t.Errorf("the case title was not masked in the body:\n%s", got)
	}
}

func TestARepositoryIsBusyWhenSomeoneWorksInIt(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	projects := t.TempDir()
	repo := filepath.Join(projects, "shop")
	os.MkdirAll(repo, 0o700)
	gitIn(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "first")
	old := time.Now().Add(-2 * time.Hour)
	for _, f := range []string{"index", "HEAD", "logs/HEAD"} {
		os.Chtimes(filepath.Join(repo, ".git", f), old, old)
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	call := func(session, tool, text string) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: session + "@" + text, TS: time.Now().Add(-10 * time.Minute), Session: session,
			Harness: "claude", Cwd: projects, Actor: store.ActorAgent, Kind: "tool_call", Tool: tool, Summary: text}
	}
	if _, err := st.Put(ctx, []store.Event{call("s-above", "Bash", `{"command":"cd shop && go test ./..."}`),
		call("s-other", "Bash", `{"command":"cd garden && ls"}`),
		// the repository's name, a read of its files and a brief that names its path are no work in it
		call("s-gh", "Bash", `{"command":"gh pr view 5 --repo owner/shop --json state"}`),
		call("s-reads", "Bash", `{"command":"grep -n first `+repo+`/a.txt; git -C `+repo+` log --oneline -3; journalctl --user -u shop"}`),
		call("s-reads", "Read", `{"file_path":"`+repo+`/a.txt"}`),
		call("s-reads", "Agent", `{"prompt":"review the change in `+repo+`"}`),
		call("s-commit", "Bash", `{"command":"git -C `+repo+` commit -qm fix"}`),
		call("s-edit", "Edit", `{"file_path":"`+repo+`/a.txt","old_string":"a","new_string":"b"}`),
		// a call it cannot read counts when it names the tree's path, not when it names the repository
		call("s-unread", "mcp__tool__run", `{"target":"`+repo+`/a.txt"}`),
		call("s-unread-name", "mcp__tool__run", `{"repo":"owner/shop"}`),
	}); err != nil {
		t.Fatal(err)
	}
	above := func(id string) map[int]capacity.Session {
		return map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: id, Cwd: projects}}
	}
	for _, c := range []struct {
		ss   map[int]capacity.Session
		want string
	}{
		{map[int]capacity.Session{}, ""},
		{map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: "s-in", Cwd: filepath.Join(repo, "sub")}}, "works in"},
		{map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: "s-above", Cwd: projects}}, "works in " + repo + " from"},
		{map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: "s-other", Cwd: projects}}, ""},
		{above("s-gh"), ""},
		{above("s-reads"), ""},
		{above("s-commit"), "works in " + repo + " from"},
		{above("s-edit"), "works in " + repo + " from"},
		{above("s-unread"), "works in " + repo + " from"},
		{above("s-unread-name"), ""},
	} {
		if got, err := busyIn(ctx, st, repo, c.ss); err != nil || (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q %v", c.ss, got, err)
		}
	}
	// a process at work in the tree, whatever started it
	sub := filepath.Join(repo, "sub")
	os.MkdirAll(sub, 0o700)
	sleep := exec.Command("sleep", "30")
	sleep.Dir = sub
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	got, err := busyIn(ctx, st, repo, nil)
	sleep.Process.Kill()
	sleep.Wait()
	if err != nil || !strings.Contains(got, fmt.Sprintf("sleep (pid %d) in %s", sleep.Process.Pid, repo)) {
		t.Errorf("a process in the tree: %q %v", got, err)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("b\n"), 0o600)
	if got, _ := busyIn(ctx, st, repo, nil); !strings.Contains(got, "uncommitted") {
		t.Errorf("uncommitted changes: %q", got)
	}
}
