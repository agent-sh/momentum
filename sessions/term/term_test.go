package term_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/term"
)

func TestParsePanesReadsTheTabSeparatedFormat(t *testing.T) {
	out := "%3\twork\t1\t0\tmy window\t4242\t/home/u/repo\t✳ title with\ttab\n" + // a program put a tab in its title
		"%4\twork\t1\t1\teditor\t4243\t/home/u/repo\tclaude\n" +
		"garbage\n"
	ps := term.ParsePanes(out)
	if len(ps) != 2 || ps[0].Title != "✳ title with\ttab" || ps[0].PID != 4242 {
		t.Fatalf("panes %+v", ps)
	}
	p := ps[1]
	if p.ID != "%4" || p.PID != 4243 || p.WindowName != "editor" || p.Path != "/home/u/repo" || p.Target() != "work:1.1" {
		t.Fatalf("pane %+v", p)
	}
}

func TestPermissionPromptNeedsTheDialogsOptions(t *testing.T) {
	dialog := "  Bash command\n\n  Do you want to proceed with rm -rf build?\n  ❯ 1. Yes\n    2. No\n"
	if got := term.PermissionPrompt(dialog); got != "Do you want to proceed with rm -rf build?" {
		t.Fatalf("got %q", got)
	}
	if got := term.PermissionPrompt("Do you want to proceed with this plan? Tell me.\n"); got != "" {
		t.Fatalf("a question in prose is not a dialog: %q", got)
	}
}

func TestKittyEnvReadsTheInheritedWindow(t *testing.T) {
	root := fake.ProcRoot(t, fake.Proc{PID: 7, Argv: []string{"claude"}, Env: []string{"HOME=/h", "KITTY_LISTEN_ON=unix:@k", "KITTY_WINDOW_ID=12", "KITTY_PID=99"}})
	listen, win, pid := term.KittyEnv(root, 7)
	if listen != "unix:@k" || win != "12" || pid != 99 {
		t.Fatalf("got %q %q %d", listen, win, pid)
	}
	if l, w, p := term.KittyEnv(root, 8); l != "" || w != "" || p != 0 {
		t.Fatal("a missing process has no kitty window")
	}
}

// fakeTmux puts a tmux on PATH that runs script.
func fakeTmux(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// No tmux server is no panes; a tmux that does not answer by the caller's deadline is an error, not an empty list.
func TestATmuxThatDoesNotAnswerIsAnError(t *testing.T) {
	fakeTmux(t, `echo "no server running on /x" >&2; exit 1`)
	if ps, err := term.ListPanes(context.Background()); err != nil || ps != nil {
		t.Fatalf("no server: %v %v", ps, err)
	}
	fakeTmux(t, `exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := term.ListPanes(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("a tmux that hangs: %v after %v", err, time.Since(start))
	}
}
