package loop

import (
	"context"
	"testing"
)

func TestSubmoduleUpdateModesAreAllowedButCommandsRefused(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "submodule.lib.update", "none")
	if err := CheckGitConfig(context.Background(), dir); err != nil {
		t.Fatalf("a mode: %v", err)
	}
	run(t, dir, "git", "config", "submodule.lib.update", "!sh ./x")
	if err := CheckGitConfig(context.Background(), dir); err == nil {
		t.Fatal("a command was allowed")
	}
}
