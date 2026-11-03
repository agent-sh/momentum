package loop

import (
	"os"

	"path/filepath"
	"strings"
	"testing"
)

// recoveryShip exercises the real ship and Git operations over local fixtures. Its process stand-in runs the
// harmless fixture test directly, so recovery boundaries can be tested where namespaces are unavailable.
// Sandbox confinement has its own bubblewrap integration tests.

func TestRunOutputAndGitLinkReadersRefuseSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "record")
	write(t, target, `{"type":"result","subtype":"success","result":"denied-marker-9c2b"}`+"\n")
	os.Symlink(target, filepath.Join(dir, "transcript.jsonl"))
	if _, err := readStream(filepath.Join(dir, "transcript.jsonl")); err == nil {
		t.Fatal("read a linked run transcript")
	}
	if f, err := exclusiveOutput(dir, "transcript.jsonl"); err == nil {
		f.Close()
		t.Fatal("a linked output was opened for truncation")
	}
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), "denied-marker-9c2b") {
		t.Fatal("a substituted output truncated the external fixture")
	}
}
