package term_test

import (
	"testing"

	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/term"
)

func TestParsePanesReadsTheTabSeparatedFormat(t *testing.T) {
	out := "%3\twork\t1\t0\tmy window\t4242\t/home/u/repo\t✳ title with\ttab\n" + // a title with a tab is malformed: skipped
		"%4\twork\t1\t1\teditor\t4243\t/home/u/repo\tclaude\n" +
		"garbage\n"
	ps := term.ParsePanes(out)
	if len(ps) != 1 {
		t.Fatalf("panes %+v", ps)
	}
	p := ps[0]
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
