package sandbox

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A source directory is refused for a socket anywhere under it, however deep, and passes without one. A link to a
// directory elsewhere is not followed: what it points at is no part of the source.
func TestASourceWithASocketAnywhereUnderItIsRefused(t *testing.T) {
	root, err := os.MkdirTemp("", "src") // a short path: a socket's name has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	mk := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	listen := func(path string) {
		t.Helper()
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
	}
	clean, deep, elsewhere := mk("clean", "a", "b"), mk("deep", "a", "b"), mk("elsewhere")
	for _, f := range []string{filepath.Join(clean, "file"), filepath.Join(root, "clean", "top")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listen(filepath.Join(deep, "s"))
	listen(filepath.Join(elsewhere, "s"))
	if err := os.Symlink(elsewhere, filepath.Join(clean, "link")); err != nil {
		t.Fatal(err)
	}
	if err := source(filepath.Join(root, "clean")); err != nil {
		t.Errorf("a source with files, directories and a link out of it: %v", err)
	}
	if err := source(filepath.Join(clean, "file")); err != nil {
		t.Errorf("a regular file: %v", err)
	}
	for _, dir := range []string{filepath.Join(root, "deep"), elsewhere} {
		if err := source(dir); err == nil || !strings.Contains(err.Error(), "holds a socket") {
			t.Errorf("%s: %v, want the refusal of a socket", strings.TrimPrefix(dir, root), err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "clean", "a"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "clean", "a"), 0o700) })
	if err := source(filepath.Join(root, "clean")); err == nil || !strings.Contains(err.Error(), "walk source") {
		t.Errorf("a directory that cannot be read: %v, want a refusal", err)
	}
}

// The check of a source names nothing under it in Go's test log. Package os records every file a test stats or
// opens there, and the go command reads that log whole after a run it may cache: a walk through package os of the
// trees the loop binds, at every launch, made that log and the go command's memory grow without bound. The test
// binary runs again as go test starts it for such a run, and walks a small tree.
func TestASourceWalkStaysOutOfGoTestLog(t *testing.T) {
	if root := os.Getenv("MOMENTUM_TEST_SOURCE_WALK"); root != "" { // the run that walks
		if err := source(root); err != nil {
			t.Fatal(err)
		}
		return
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deep", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"top", "deep/file", "deep/inner/file"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "testlog.txt")
	cmd := exec.Command(os.Args[0], "-test.run=^TestASourceWalkStaysOutOfGoTestLog$", "-test.testlogfile="+log)
	cmd.Env = append(os.Environ(), "MOMENTUM_TEST_SOURCE_WALK="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the walk: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the run wrote no test log to read: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, root+string(filepath.Separator)) {
			t.Fatalf("the walk put an entry of the source in go's test log: %q", line)
		}
	}
}
