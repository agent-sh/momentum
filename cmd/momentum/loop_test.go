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
	"github.com/agent-sh/momentum/internal/loop"
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

// evidence git runs only the git commands that read, each with only the options it lists, by their exact names:
// an argument before `--` is a revision of the repository, checked; a path comes after `--`, inside the
// repository. The loop's agent reads history through it and runs no git of its own.
func TestEvidenceGitOnlyReads(t *testing.T) {
	fakeScrubber(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o600)
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nthree\n"), 0o600)
	gitIn(t, dir, "commit", "-q", "-am", "second")
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("hunter2-value\n"), 0o600)
	marker := filepath.Join(t.TempDir(), "ran")
	for _, c := range []struct {
		args []string
		has  string
	}{
		{[]string{"log", "--oneline"}, "first"}, {[]string{"log", "-n", "1", "--format=%s"}, "second"}, {[]string{"log", "-n1", "--stat"}, "a.txt"},
		{[]string{"show", "HEAD~1:a.txt"}, "two"}, {[]string{"diff", "HEAD~1", "HEAD", "--", "a.txt"}, "three"}, {[]string{"diff", "HEAD~1..HEAD", "--stat"}, "a.txt"},
		{[]string{"blame", "--", "a.txt"}, "three"}, {[]string{"grep", "-n", "-e", "three", "--", "a.txt"}, "three"},
		{[]string{"ls-files"}, "a.txt"}, {[]string{"branch", "--show-current"}, "main"}, {[]string{"rev-parse", "--short", "HEAD"}, ""},
	} {
		if code, out, errOut := momentum(append([]string{"evidence", "git", dir}, c.args...)...); code != 0 || !strings.Contains(out, c.has) {
			t.Errorf("git %v: exit %d %q %s", c.args, code, out, errOut)
		}
	}
	for _, args := range [][]string{
		// what reads a file outside the repository, runs a program or writes
		{"diff", "/dev/null", secret}, {"diff", "--", "/dev/null", secret}, {"diff", "--no-index", "/dev/null", secret},
		{"blame", "--cont=" + secret, "a.txt"}, {"blame", "--contents", secret, "--", "a.txt"}, {"blame", "-S", secret, "--", "a.txt"},
		{"blame", "--ignore-revs-file", secret, "--", "a.txt"}, {"grep", "--open-f=touch " + marker + "; true", "-e", "x"},
		{"grep", "-O", "-e", "x"}, {"grep", "-f", secret}, {"log", "--output=" + filepath.Join(dir, "out")}, {"log", "--outp=" + filepath.Join(dir, "out")},
		{"log", "--ext-diff"}, {"show", "--textconv", "HEAD"}, {"log", "--", "../x"}, {"log", "--", ":(top)x"},
		// what is not a reading command, or a listing's write
		{"push", "origin"}, {"commit", "-m", "x"}, {"branch", "new-branch"}, {"branch", "-D", "main"}, {"tag", "v1"},
		{"config", "user.email", "x@y"}, {"config", "--list"}, {"remote", "show", "origin"}, {"worktree", "add", "x"}, {"cat-file", "-p", "HEAD"},
		// a value of git's own options, and a revision that is no revision
		{"log", "-c", "core.pager=x"}, {"log", "not-a-revision"}, {"blame", "a.txt"},
	} {
		if code, _, _ := momentum(append([]string{"evidence", "git", dir}, args...)...); code != 2 {
			t.Errorf("git %v: exit %d, want 2", args, code)
		}
	}
	for _, f := range []string{filepath.Join(dir, "out"), marker} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("a refused command made %s", f)
		}
	}
	// git's own variables do not point it elsewhere, and a directory that is no work tree is refused
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nothing"))
	if code, out, errOut := momentum("evidence", "git", dir, "log", "--oneline"); code != 0 || !strings.Contains(out, "first") {
		t.Errorf("with GIT_DIR set: exit %d %q %s", code, out, errOut)
	}
	if code, _, _ := momentum("evidence", "git", t.TempDir(), "log"); code != 2 {
		t.Errorf("no work tree: exit %d", code)
	}
}

// fakeScrubber stands for the secret scrubber: it masks the word secret-value, as scrump masks a secret.
func fakeScrubber(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "scrub")
	os.WriteFile(p, []byte("#!/bin/sh\nsed 's/secret-value/************/g'\n"), 0o755)
	was := loop.Scrubber
	loop.Scrubber = []string{p}
	t.Cleanup(func() { loop.Scrubber = was })
}

// A run reads the machine's files and services only through momentum, which refuses where credentials are kept
// and scrubs what it prints; with no scrubber it prints nothing.
func TestEvidenceFileScrubsAndRefusesCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeScrubber(t)
	f := filepath.Join(home, "notes", "run.log")
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte("line one\nthe key is secret-value\nline three\n"), 0o600)
	if code, out, errOut := momentum("evidence", "file", f); code != 0 || strings.Contains(out, "secret-value") || !strings.Contains(out, "line three") {
		t.Fatalf("a file: exit %d %q %s", code, out, errOut)
	}
	if code, out, _ := momentum("evidence", "file", f, "--tail", "1"); code != 0 || strings.TrimSpace(out) != "line three" {
		t.Fatalf("its last line: exit %d %q", code, out)
	}
	for _, p := range []string{filepath.Join(home, ".ssh", "config"), filepath.Join(home, ".config", "gh", "hosts.yml"), filepath.Join(home, "x", ".env"),
		filepath.Join(home, "x", "deploy.pem"), filepath.Join(home, ".claude.json")} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("secret-value"), 0o600)
		if code, out, _ := momentum("evidence", "file", p); code != 2 || strings.Contains(out, "secret") {
			t.Errorf("%s: exit %d %q", p, code, out)
		}
	}
	// a link to a credential is the credential
	link := filepath.Join(home, "notes", "innocent.txt")
	os.Symlink(filepath.Join(home, ".ssh", "config"), link)
	if code, _, _ := momentum("evidence", "file", link); code != 2 {
		t.Errorf("a link to a credential: exit %d", code)
	}
	loop.Scrubber = []string{filepath.Join(t.TempDir(), "no-such-scrubber")}
	if code, out, _ := momentum("evidence", "file", f); code == 0 || out != "" {
		t.Errorf("with no scrubber: exit %d %q", code, out)
	}
}

// Inside one of the loop's runs, momentum lands nothing, rolls nothing back, runs no loop, reads any case but
// changes only the run's own, opens and detects none, takes a proposal's body only from the run's own directory,
// and writes a replay's points only there.
func TestALoopRunKeepsToItsOwnCase(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sig := range []string{"failures/v1:own", "failures/v1:other"} {
		c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: sig, Title: sig,
			Evidence: []store.Evidence{{Ref: "file:/t/" + sig, At: time.Now().Add(-time.Hour)}}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	st.Close()
	own, other := ids[0], ids[1]
	run := t.TempDir()
	t.Setenv("MOMENTUM_LOOP_RUN", "r1")
	t.Setenv("MOMENTUM_LOOP_CASE", own)
	t.Setenv("MOMENTUM_LOOP_DIR", run)
	for _, args := range [][]string{{"ship", "land", own}, {"ship", "rollback", own, "--why", "x"}, {"loop"}} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "loop") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := momentum("ship", "status", "--case", other); code != 2 || !strings.Contains(errOut, "works case "+own) {
		t.Errorf("another case's package: exit %d %s", code, errOut)
	}
	for _, args := range [][]string{{"case", "close", other, "--reason", "not-worth-it", "--why", "x"}, {"case", "set", other, "--status", "investigating"},
		{"case", "evidence", other, "--ref", "file:/t/x"}, {"case", "signature", other, "root:x"}, {"case", "open", "--family", "failures", "--signature", "failures/v1:x", "--title", "x", "--evidence", "file:/t/x"},
		{"case", "detect", "--dry"}} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "loop run") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if code, out, errOut := momentum("case", "show", other); code != 0 || !strings.Contains(out, other) {
		t.Errorf("reading another case: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "set", own, "--status", "investigating"); code != 0 {
		t.Errorf("its own case: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("supervise", "replay", "--since", "1d", "--points", filepath.Join(t.TempDir(), "p.jsonl")); code != 2 ||
		!strings.Contains(errOut, "writes only under") {
		t.Errorf("points outside the run: exit %d %s", code, errOut)
	}
	outside := filepath.Join(t.TempDir(), "body.md")
	os.WriteFile(outside, []byte("a description"), 0o600)
	if _, err := readBody(outside, "r1"); err == nil {
		t.Error("a body outside the run's directory was read")
	}
	inside := filepath.Join(run, "body.md")
	os.WriteFile(inside, []byte("a description"), 0o600)
	if b, err := readBody(inside, "r1"); err != nil || b != "a description" {
		t.Errorf("the run's own body: %q %v", b, err)
	}
	os.Symlink(outside, filepath.Join(run, "link.md"))
	if _, err := readBody(filepath.Join(run, "link.md"), "r1"); err == nil {
		t.Error("a link out of the run's directory was read")
	}
}

// A lesson goes to tacit as the loop's observation, scrubbed, in the scope of the repository its change went to,
// and only for a case closed as improved.
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
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:x", Title: "x keeps failing",
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
	if strings.Contains(string(got), "secret-value") || strings.Contains(string(got), "@someone") {
		t.Errorf("the lesson went unscrubbed:\n%s", got)
	}
}

// A repository is in use when a live session works in one of its trees, or above one and its last hour's calls
// name it, or its tracked files hold uncommitted changes; a session above it that works elsewhere is not its.
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
	call := func(session, text string) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: session + "@" + text, TS: time.Now().Add(-10 * time.Minute), Session: session,
			Harness: "claude", Actor: store.ActorAgent, Kind: "tool_call", Tool: "Bash", Summary: text}
	}
	if _, err := st.Put(ctx, []store.Event{call("s-above", `{"command":"cd shop && go test ./..."}`), call("s-other", `{"command":"cd garden && ls"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ss   map[int]capacity.Session
		want string
	}{
		{map[int]capacity.Session{}, ""},
		{map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: "s-in", Cwd: filepath.Join(repo, "sub")}}, "works in"},
		{map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: "s-above", Cwd: projects}}, "works in " + repo + " from"},
		{map[int]capacity.Session{1: {Key: "k", Tool: "claude", ID: "s-other", Cwd: projects}}, ""},
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
