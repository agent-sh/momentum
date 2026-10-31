package act

import (
	"strings"
	"testing"
)

const (
	ruleLine = "\x1b[38;5;244m────────────────────────────────────────\x1b[39m"
	dim      = "\x1b[2m"
	reset    = "\x1b[0m"
	nbsp     = " "
	status   = "  \x1b[38;5;246m~/repo ctx:12%  Model\x1b[39m"
)

func TestDraftReadsWhatIsTypedOnTheInputLine(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		screen string
		typed  string
		ok     bool
	}{
		{"claude, empty", "claude", "text\n" + ruleLine + "\n\x1b[39m❯" + nbsp + "\n" + ruleLine + "\n" + status, "", true},
		{"claude, a dim suggestion", "claude", ruleLine + "\n\x1b[39m❯" + nbsp + dim + "add a test for the parser" + reset + "\n" + ruleLine, "", true},
		{"claude, typed", "claude", ruleLine + "\n❯" + nbsp + "half a thought\n" + ruleLine, "half a thought", true},
		{"claude, typed then a dim hint", "claude", ruleLine + "\n❯" + nbsp + "fix it" + dim + "  (shift+tab)" + reset + "\n" + ruleLine, "fix it", true},
		{"claude, typed across two lines", "claude", ruleLine + "\n❯" + nbsp + "a long line that\n  wraps on\n" + ruleLine, "a long line that wraps on", true},
		{"claude glyph in the transcript, not under a rule", "claude", "❯ an old prompt\nsome output\n", "", false},
		{"claude, the session's name in the rule", "claude", "──────────────────────────── my-session ─\n❯ \n────────────────────────────────────────", "", true},
		{"a line of text with a few dashes is not a rule", "claude", "a ── b c d e f g h i j k l m n o p q r s t\n❯ typed\n", "", false},
		{"claude, a dim › status line below its draft", "claude", ruleLine + "\n❯" + nbsp + "my draft\n" + ruleLine + "\n" + dim + "› status" + reset, "my draft", true},
		{"codex, placeholder", "codex", "\x1b[1m›\x1b[0m\x1b[48;2;30;30;30m \x1b[2mAsk Codex to do anything\x1b[0m\n\n  model · 86% left", "", true},
		{"codex, typed", "codex", "\x1b[1m›\x1b[0m run the tests\n", "run the tests", true},
		{"a codex input line is not claude's", "claude", "\x1b[1m›\x1b[0m run the tests\n", "", false},
		{"boxed prompt, empty", "claude", "╭──────╮\n│ > \x1b[2mTry \"edit\"\x1b[0m │\n╰──────╯", "", true},
		{"boxed prompt, typed", "claude", "╭──────╮\n│ > fix the bug │\n╰──────╯", "fix the bug", true},
		{"no input line", "claude", "just output\nmore output\n", "", false},
		{"a tool with no input line known", "pi", ruleLine + "\n❯ \n" + ruleLine, "", false},
		{"dim cleared by 22", "claude", ruleLine + "\n❯" + nbsp + dim + "hint\x1b[22mreal" + reset + "\n" + ruleLine, "real", true},
		{"a 256 color is not dim", "claude", ruleLine + "\n❯" + nbsp + "\x1b[38;5;2mgreen" + reset + "\n" + ruleLine, "green", true},
		{"a double underline (4:2) is not dim", "claude", ruleLine + "\n❯" + nbsp + "\x1b[4:2mtyped" + reset + "\n" + ruleLine, "typed", true},
		{"a colon color (38:2:...) is not dim", "claude", ruleLine + "\n❯" + nbsp + "\x1b[38:2::2:2:2mtyped" + reset + "\n" + ruleLine, "typed", true},
		{"a colon color whose fields hold a 2 is not dim", "claude", ruleLine + "\n❯" + nbsp + "\x1b[38:2:0:2:2:2mtyped" + reset + "\n" + ruleLine, "typed", true},
		{"claude, no rule below it is not its input line", "claude", ruleLine + "\n❯" + nbsp + "my draft\nmore text", "", false},
		{"claude, a dim ❯ status line under the bottom rule", "claude", ruleLine + "\n❯" + nbsp + "my draft\n" + ruleLine + "\n" + dim + "❯ status" + reset, "my draft", true},
		// The cursor is drawn reversed: on a dim placeholder it sits on its first letter.
		{"claude, the cursor on a dim placeholder", "claude", ruleLine + "\n\x1b[39m❯" + nbsp + "\x1b[7m\x1b[39mP\x1b[0;2mress up to edit queued messages\x1b[0m\n" + ruleLine, "", true},
		{"claude, the cursor inside typed text", "claude", ruleLine + "\n❯" + nbsp + "fi\x1b[7mx\x1b[0m it\n" + ruleLine, "fix it", true},
		{"claude, the cursor on the last typed letter", "claude", ruleLine + "\n❯" + nbsp + "fix i\x1b[7mt\x1b[0m\n" + ruleLine, "fix it", true},
		{"claude, the cursor after typed text", "claude", ruleLine + "\n❯" + nbsp + "fix it\x1b[7m \x1b[0m\n" + ruleLine, "fix it", true},
	}
	for _, c := range cases {
		typed, ok := Draft(c.screen, c.tool)
		if ok != c.ok || typed != c.typed {
			t.Errorf("%s: got %q %v, want %q %v", c.name, typed, ok, c.typed, c.ok)
		}
	}
}

func TestDialogFindsMenusAndPrompts(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		dialog bool
	}{
		{"permission prompt", "  Bash command\n  Do you want to proceed?\n  ❯ 1. Yes\n    2. No\n", true},
		{"numbered menu under a cursor", "  Resume from summary (recommended)\n ❯ 1. Resume from summary\n   2. Resume full session\n", true},
		{"menu footer", "  pick one\n  Enter to select · Esc to cancel\n", true},
		{"arrow footer", "  pick one\n  ↑/↓ to navigate\n", true},
		{"yes or no", "Overwrite the file? [y/N]\n", true},
		{"codex approval", "  Would you like to run the following command?\n  $ rm -rf build\n", true},
		{"a numbered list in prose", "Next steps:\n1. Run the tests\n2. Merge\n", false},
		{"an input line", ruleLine + "\n❯" + nbsp + "\n" + ruleLine, false},
		{"a question in prose", "Do you want me to merge it? Tell me.\n", false},
		{"an old dialog scrolled up", "  Do you want to proceed?\n  ❯ 1. Yes\n" + strings.Repeat("output\n", 30) + ruleLine + "\n❯ \n" + ruleLine, false},
	}
	for _, c := range cases {
		if got := Dialog(Plain(c.screen)); (got != "") != c.dialog {
			t.Errorf("%s: dialog %q, want %v", c.name, got, c.dialog)
		}
	}
}

func TestSafeTextIsOneLineWithoutControls(t *testing.T) {
	cases := map[string]string{
		"continue":                       "continue",
		"two\nlines":                     "two lines",
		"esc\x1b[31m red\x03":            "esc[31m red",
		"  spaced \t out  ":              "spaced out",
		"c1\u0085 control, café, ❯ kept": "c1 control, café, ❯ kept",
	}
	for in, want := range cases {
		if got := SafeText(in); got != want {
			t.Errorf("SafeText(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []string{"/compact", "!rm -rf x", "#remember", "?"} {
		if !command(c) {
			t.Errorf("%q runs as a command", c)
		}
	}
	if command("[momentum] continue") {
		t.Error("a message is not a command")
	}
}
