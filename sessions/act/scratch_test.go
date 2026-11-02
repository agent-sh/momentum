package act

import (
	"os"
	"strings"
	"testing"
)

// The tmux fixtures work under a scratch path too long for a socket's address.
func TestATmuxFixtureHoldsUnderALongScratchPath(t *testing.T) {
	dir, err := os.MkdirTemp("", strings.Repeat("long", 30))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	_, tmux := tmuxServer(t)
	tmux("new-session", "-d", "-s", "fixture", "cat")
	if got := tmux("display-message", "-p", "-t", "fixture", "#{session_name}"); got != "fixture" {
		t.Fatalf("fixture session %q", got)
	}
}
