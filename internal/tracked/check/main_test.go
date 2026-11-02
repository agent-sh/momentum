package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// The workflow reads the command's exit code: 0 when the range holds nothing to refuse, 1 when it does, 2 when the
// range could not be read or none was given.
func TestTheExitCodeSaysWhatTheRangeHolds(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("text\n"), 0o644)
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "a")
	var out, diag bytes.Buffer
	if code := run(dir, []string{"HEAD"}, &out, &diag); code != 0 {
		t.Fatalf("a clean history: exit %d, %s %s", code, out.String(), diag.String())
	}
	os.WriteFile(filepath.Join(dir, "tool"), append([]byte("\x7fELF"), make([]byte, 64)...), 0o755)
	git(t, dir, "add", "tool")
	git(t, dir, "commit", "-q", "-m", "tool")
	out.Reset()
	if code := run(dir, []string{"HEAD~1..HEAD"}, &out, &diag); code != 1 || !strings.Contains(out.String(), "tool") {
		t.Fatalf("a binary: exit %d, %s", code, out.String())
	}
	if code := run(dir, []string{"no-such-revision"}, &out, &diag); code != 2 {
		t.Fatalf("an unreadable range: exit %d", code)
	}
	if code := run(dir, nil, &out, &diag); code != 2 {
		t.Fatalf("no range: exit %d", code)
	}
}
