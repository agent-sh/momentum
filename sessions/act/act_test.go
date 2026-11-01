package act

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
)

// emptyInput is what a fake pane shows: an empty Claude Code input line.
var emptyInput = "output\n" + ruleLine + "\n❯" + nbsp + "\n" + ruleLine + "\n" + status

// fakeTmux answers the tmux commands a send runs, records them, and makes the input line show what was pasted.
type fakeTmux struct {
	screen string // the styled screen before the paste
	mode   string // pane_in_mode
	sync   string // pane_synchronized
	shell  string // pane_pid; "" is the fake shell's
	tty    string // pane_tty; "" is the terminal the fake agent runs in
	pasted string
	after  string   // what the screen shows after the paste instead of the paste, when set
	later  string   // the mode and sync once the paste is in ("1 0", "0 1"), when set
	slow   int      // how many reads after the paste still show the screen before it
	reads  []string // the screens the next reads after the paste show, after slow, before the paste itself
	cmds   []string
}

func (f *fakeTmux) run(_ context.Context, stdin, name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	if name != "tmux" {
		return nil, fmt.Errorf("unexpected %s", name)
	}
	switch args[0] {
	case "display-message":
		mode, sync := f.mode, f.sync
		if f.later != "" && f.pasted != "" {
			mode, sync, _ = strings.Cut(f.later, " ")
		}
		shell, tty := f.shell, f.tty
		if shell == "" {
			shell = "10"
		}
		if tty == "" {
			tty = paneTTY
		}
		return []byte(fmt.Sprintf("0 %s %s %s %s\n", mode, sync, shell, tty)), nil
	case "capture-pane":
		switch {
		case f.after != "" && f.pasted != "":
			return []byte(f.after), nil
		case f.pasted != "" && f.slow > 0:
			f.slow--
			return []byte(f.screen), nil
		case f.pasted != "" && len(f.reads) > 0:
			next := f.reads[0]
			f.reads = f.reads[1:]
			return []byte(next), nil
		case f.pasted != "":
			return []byte(strings.Replace(f.screen, "❯"+nbsp, "❯"+nbsp+f.pasted, 1)), nil
		}
		return []byte(f.screen), nil
	case "load-buffer":
		f.pasted = stdin
	}
	return nil, nil
}

func (f *fakeTmux) typed() bool {
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "tmux load-buffer") || strings.HasPrefix(c, "tmux send-keys") {
			return true
		}
	}
	return false
}

// pane is a fake machine: a shell (pid 10) in a pane, running the agent (pid 11) in its terminal's foreground.
func pane(t *testing.T, f *fakeTmux, agent fake.Proc) (*Sender, Target) {
	root := fake.ProcRoot(t, fake.Proc{PID: 10, PPID: 1, Argv: []string{"zsh"}, Start: 5, PGRP: 10, TPGID: 11, TTY: paneTTYNr}, agent)
	if f.sync == "" {
		f.sync = "0"
	}
	return &Sender{Run: f.run, ProcRoot: root, Pause: time.Millisecond}, Target{Tool: "claude", Pane: "%3", PID: 11, Start: 6}
}

// The fake pane's terminal is a character device every machine has; paneTTYNr is its number as a stat file
// writes it (tty_nr).
const paneTTY = "/dev/null"

var paneTTYNr = func() int {
	var st syscall.Stat_t
	if syscall.Stat(paneTTY, &st) != nil {
		return 0
	}
	major := (st.Rdev>>8)&0xfff | (st.Rdev>>32)&^0xfff
	minor := st.Rdev&0xff | (st.Rdev>>12)&^0xff
	return int(major<<8 | minor&0xff | (minor&^0xff)<<12)
}()

var agent = fake.Proc{PID: 11, PPID: 10, Argv: []string{"claude"}, Start: 6, PGRP: 11, TPGID: 11, TTY: paneTTYNr}

func TestSendPastesThenPressesEnter(t *testing.T) {
	f := &fakeTmux{screen: emptyInput, mode: "0"}
	s, tgt := pane(t, f, agent)
	path, err := s.Send(context.Background(), tgt, "[momentum] continue\nnow")
	if err != nil || path != Tmux {
		t.Fatalf("send: %v %v", path, err)
	}
	if f.pasted != "[momentum] continue now" {
		t.Fatalf("pasted %q", f.pasted)
	}
	if last := f.cmds[len(f.cmds)-1]; last != "tmux send-keys -t %3 Enter" {
		t.Fatalf("last command %q, all %q", last, f.cmds)
	}
	var paste string
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "tmux paste-buffer") {
			paste = c
		}
	}
	if !strings.Contains(paste, " -p ") || !strings.Contains(paste, "-t %3") {
		t.Fatalf("not a bracketed paste into the pane: %q", paste)
	}
	// On Claude Code's input box, only its own rows say a turn runs: the key in the conversation does not.
	f = &fakeTmux{screen: "● Press esc to interrupt it when it hangs.\n\n" + strings.TrimPrefix(emptyInput, "output\n"), mode: "0"}
	s, tgt = pane(t, f, agent)
	if _, err := s.Send(context.Background(), tgt, "[momentum] continue"); err != nil || f.cmds[len(f.cmds)-1] != "tmux send-keys -t %3 Enter" {
		t.Fatalf("the key in the conversation: %v %q", err, f.cmds)
	}
}

func TestSendRefusesWhenTheSessionIsNotWaitingForInput(t *testing.T) {
	stopped, behind, recycled, elsewhere, ownTTY := agent, agent, agent, agent, agent
	stopped.State = 'T' // suspended: the shell that stopped it owns the terminal
	behind.TPGID = 10   // the shell's group is the terminal's foreground
	recycled.Start = 99 // another process now has the pid
	elsewhere.PPID = 1  // not under the pane's shell
	ownTTY.TTY = 34816  // a terminal of its own, which another program in the pane opened for it
	claudeWorking := "● Running the tests\n\n✻ Working… (12s · esc to interrupt)\n\n" + strings.TrimPrefix(emptyInput, "output\n")
	codexWorking := "• Working (4s • esc to interrupt)\n\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n"
	cases := []struct {
		name   string
		screen string
		mode   string
		sync   string
		agent  fake.Proc
		text   string
		want   error
	}{
		{"a permission prompt", "  Do you want to proceed?\n  ❯ 1. Yes\n    2. No\n", "0", "0", agent, "continue", ErrDialog},
		{"a draft", "output\n" + ruleLine + "\n❯" + nbsp + "half typed\n" + ruleLine, "0", "0", agent, "continue", ErrDraft},
		{"copy mode", emptyInput, "1", "0", agent, "continue", ErrMode},
		{"synchronized panes", emptyInput, "0", "1", agent, "continue", ErrSync},
		{"no input line", "a shell prompt $ ", "0", "0", agent, "continue", ErrNoInput},
		{"a stopped process", emptyInput, "0", "0", stopped, "continue", ErrNotFront},
		{"a process behind the shell", emptyInput, "0", "0", behind, "continue", ErrNotFront},
		{"a recycled pid", emptyInput, "0", "0", recycled, "continue", ErrGone},
		{"a process outside the pane", emptyInput, "0", "0", elsewhere, "continue", ErrGone},
		{"a process in a terminal of its own", emptyInput, "0", "0", ownTTY, "continue", ErrNotFront},
		{"a claude turn running", claudeWorking, "0", "0", agent, "continue", ErrBusy},
		{"a codex turn running", codexWorking, "0", "0", agent, "continue", ErrBusy},
		{"a message that reads as a menu row", emptyInput, "0", "0", agent, "1. run the tests", ErrMessage},
		{"a message that reads as a prompt", emptyInput, "0", "0", agent, "ship it [y/N]", ErrMessage},
		{"a message that reads as a running turn", emptyInput, "0", "0", agent, "if it hangs use esc to interrupt", ErrMessage},
		{"a slash command", emptyInput, "0", "0", agent, "/compact", ErrCommand},
		{"a shell escape", emptyInput, "0", "0", agent, "!rm -rf build", ErrCommand},
	}
	for _, c := range cases {
		f := &fakeTmux{screen: c.screen, mode: c.mode, sync: c.sync}
		s, tgt := pane(t, f, c.agent)
		if _, err := s.Send(context.Background(), tgt, c.text); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if f.typed() {
			t.Errorf("%s: typed anyway: %q", c.name, f.cmds)
		}
	}
	// A target with no identity, or a pane whose shell cannot be read, is refused before anything runs.
	f := &fakeTmux{screen: emptyInput, mode: "0"}
	s, tgt := pane(t, f, agent)
	tgt.Start = 0
	if _, err := s.Send(context.Background(), tgt, "continue"); !errors.Is(err, ErrGone) || f.typed() {
		t.Fatalf("no start: %v, typed %v", err, f.typed())
	}
	f = &fakeTmux{screen: emptyInput, mode: "0", shell: "x"}
	s, tgt = pane(t, f, agent)
	if _, err := s.Send(context.Background(), tgt, "continue"); !errors.Is(err, ErrGone) || f.typed() {
		t.Fatalf("an unreadable pane_pid: %v, typed %v", err, f.typed())
	}
}

func TestSendDoesNotPressEnterUnlessTheInputLineHoldsTheMessageAlone(t *testing.T) {
	msg := "[momentum] continue"
	cases := map[string]struct{ after, later string }{
		"someone else's words":             {emptyInputWith("someone else's words"), ""},
		"the message and more":             {emptyInputWith(msg + " and more"), ""},
		"a dialog opened meanwhile":        {emptyInputWith(msg) + "\n  Do you want to proceed?\n  ❯ 1. Yes\n    2. No\n", ""},
		"a turn started meanwhile":         {"✻ Working… (2s · esc to interrupt)\n" + strings.TrimPrefix(emptyInputWith(msg), "output\n"), ""},
		"a turn whose key the paste hides": {strings.Replace(emptyInputWith(msg), "output\n", "output\n✻ Working… (2s)\n\n", 1), ""},
		"the input line went away":         {"output only\n", ""},
		"copy mode meanwhile":              {"", "1 0"},
		"panes synchronized meanwhile":     {"", "0 1"},
	}
	for name, c := range cases {
		f := &fakeTmux{screen: emptyInput, mode: "0", after: c.after, later: c.later}
		s, tgt := pane(t, f, agent)
		if _, err := s.Send(context.Background(), tgt, msg); !errors.Is(err, ErrUnstable) {
			t.Errorf("%s: got %v", name, err)
		}
		pasted := false
		for _, cmd := range f.cmds {
			pasted = pasted || strings.HasPrefix(cmd, "tmux paste-buffer")
			if strings.HasPrefix(cmd, "tmux send-keys") {
				t.Errorf("%s: pressed Enter", name)
			}
		}
		if !pasted {
			t.Errorf("%s: it never pasted, so no check after the paste refused it", name)
		}
	}
	// A message that wraps across lines still matches.
	f := &fakeTmux{screen: emptyInput, mode: "0", after: "output\n" + ruleLine + "\n❯" + nbsp + "[momentum] a long message that\n  wraps here\n" + ruleLine}
	s, tgt := pane(t, f, agent)
	if _, err := s.Send(context.Background(), tgt, "[momentum] a long message that wraps here"); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	// A terminal that draws the paste a moment late gets the time to: Enter once the input line holds it.
	f = &fakeTmux{screen: emptyInput, mode: "0", slow: settleReads - 1}
	s, tgt = pane(t, f, agent)
	if _, err := s.Send(context.Background(), tgt, msg); err != nil || f.cmds[len(f.cmds)-1] != "tmux send-keys -t %3 Enter" {
		t.Fatalf("drawn late: %v %q", err, f.cmds)
	}
	f = &fakeTmux{screen: emptyInput, mode: "0", slow: settleReads}
	s, tgt = pane(t, f, agent)
	if _, err := s.Send(context.Background(), tgt, msg); !errors.Is(err, ErrUnstable) || strings.HasPrefix(f.cmds[len(f.cmds)-1], "tmux send-keys") {
		t.Fatalf("never drawn: %v %q", err, f.cmds)
	}
	// A screen caught mid-redraw, its bottom rule not drawn yet, is read again.
	f = &fakeTmux{screen: emptyInput, mode: "0", reads: []string{"output\n" + ruleLine + "\n❯" + nbsp + msg + "\n"}}
	s, tgt = pane(t, f, agent)
	if _, err := s.Send(context.Background(), tgt, msg); err != nil || f.cmds[len(f.cmds)-1] != "tmux send-keys -t %3 Enter" {
		t.Fatalf("half drawn: %v %q", err, f.cmds)
	}
	// Enter that fails leaves the caller an error it can tell apart: the message may sit on the input line.
	f = &fakeTmux{screen: emptyInput, mode: "0"}
	s, tgt = pane(t, f, agent)
	run := s.Run
	s.Run = func(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
		if args[0] == "send-keys" {
			return nil, errors.New("signal: killed")
		}
		return run(ctx, stdin, name, args...)
	}
	if _, err := s.Send(context.Background(), tgt, msg); !errors.Is(err, ErrUnstable) {
		t.Fatalf("a failed Enter: %v", err)
	}
}

func emptyInputWith(text string) string {
	return strings.Replace(emptyInput, "❯"+nbsp, "❯"+nbsp+text, 1)
}

func TestChoosePicksThePaneThenKittyThenCodexQueue(t *testing.T) {
	root := fake.ProcRoot(t, fake.Proc{PID: 20, Argv: []string{"claude"}, Env: []string{"KITTY_LISTEN_ON=unix:@k", "KITTY_WINDOW_ID=4"}},
		fake.Proc{PID: 21, Argv: []string{"codex"}})
	s := &Sender{ProcRoot: root}
	cases := []struct {
		t    Target
		want Path
		err  error
	}{
		{Target{Tool: "claude", Pane: "%1", PID: 20}, Tmux, nil},
		{Target{Tool: "claude", PID: 20}, Kitty, nil},
		{Target{Tool: "codex", PID: 21, Thread: "th-1"}, CodexQueue, nil},
		{Target{Tool: "claude", PID: 21}, "", ErrNoPath},
		{Target{Tool: "claude", Pane: "%1", PID: 20, Background: true}, "", ErrNoPath},
	}
	for _, c := range cases {
		got, err := s.Choose(c.t)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%+v: got %q %v, want %q %v", c.t, got, err, c.want, c.err)
		}
	}
}

func TestTargetOfKeysCodexByThreadAndKnowsBackgroundSessions(t *testing.T) {
	if got := TargetOf(&sessions.Session{Tool: "codex", Key: "019a-thread"}, 0); got.Thread != "019a-thread" {
		t.Fatalf("thread %q", got.Thread)
	}
	if got := TargetOf(&sessions.Session{Tool: "codex", Key: "codex:proc:9:3"}, 0); got.Thread != "" {
		t.Fatalf("a process key is not a thread: %q", got.Thread)
	}
	if got := TargetOf(&sessions.Session{Tool: "claude", Where: "bg", Pane: "%2"}, 7); !got.Background || got.Start != 7 {
		t.Fatalf("background %+v", got)
	}
}

// fakeKitty answers kitty's remote control for one window, whose foreground runs fg.
type fakeKitty struct {
	fg      int
	fgAfter int    // the window's foreground once the text is in, when set
	ls      string // what kitty @ ls answers, when set
	screen  string
	text    string
	cmds    []string
}

func (f *fakeKitty) run(_ context.Context, stdin, name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	if name != "kitty" || len(args) < 4 {
		return nil, fmt.Errorf("unexpected %s %v", name, args)
	}
	switch args[3] {
	case "ls":
		if f.ls != "" {
			return []byte(f.ls), nil
		}
		fg := f.fg
		if f.fgAfter != 0 && f.text != "" {
			fg = f.fgAfter
		}
		return []byte(fmt.Sprintf(`[{"tabs":[{"windows":[{"id":4,"foreground_processes":[{"pid":%d}]}]}]}]`, fg)), nil
	case "get-text":
		return []byte(strings.Replace(f.screen, "❯"+nbsp, "❯"+nbsp+f.text, 1)), nil
	case "send-text":
		f.text = stdin
	}
	return nil, nil
}

func TestKittySendsOnlyToAWindowWhoseForegroundIsTheSession(t *testing.T) {
	proc := fake.Proc{PID: 20, Argv: []string{"claude"}, Start: 3, PGRP: 20, TPGID: 20, Env: []string{"KITTY_LISTEN_ON=unix:@k", "KITTY_WINDOW_ID=4"}}
	root := fake.ProcRoot(t, proc)
	tgt := Target{Tool: "claude", PID: 20, Start: 3}

	f := &fakeKitty{fg: 20, screen: emptyInput}
	s := &Sender{Run: f.run, ProcRoot: root, Pause: time.Millisecond}
	if path, err := s.Send(context.Background(), tgt, "[momentum] continue"); err != nil || path != Kitty {
		t.Fatalf("send: %v %v", path, err)
	}
	if f.text != "[momentum] continue" || !strings.HasSuffix(f.cmds[len(f.cmds)-1], "send-key --match id:4 enter") {
		t.Fatalf("text %q, commands %q", f.text, f.cmds)
	}

	// The window runs something else in its foreground (a tmux server started from it, say): nothing is sent.
	// Nor when the match would reach more than the one window, or the window id is not one.
	for name, f := range map[string]*fakeKitty{
		"another foreground": {fg: 99, screen: emptyInput},
		"two windows":        {ls: `[{"tabs":[{"windows":[{"id":4,"foreground_processes":[{"pid":20}]},{"id":5,"foreground_processes":[{"pid":20}]}]}]}]`, screen: emptyInput},
		"another window":     {ls: `[{"tabs":[{"windows":[{"id":5,"foreground_processes":[{"pid":20}]}]}]}]`, screen: emptyInput},
	} {
		s = &Sender{Run: f.run, ProcRoot: root, Pause: time.Millisecond}
		if _, err := s.Send(context.Background(), tgt, "[momentum] continue"); err == nil {
			t.Fatalf("%s: sent", name)
		}
		for _, c := range f.cmds {
			if strings.Contains(c, "send-") {
				t.Fatalf("%s: sent anyway: %q", name, f.cmds)
			}
		}
	}
	odd := fake.ProcRoot(t, fake.Proc{PID: 20, Argv: []string{"claude"}, Start: 3, PGRP: 20, TPGID: 20,
		Env: []string{"KITTY_LISTEN_ON=unix:@k", "KITTY_WINDOW_ID=4 or state:focused"}})
	f = &fakeKitty{fg: 20, screen: emptyInput}
	s = &Sender{Run: f.run, ProcRoot: odd, Pause: time.Millisecond}
	if _, err := s.Send(context.Background(), tgt, "[momentum] continue"); !errors.Is(err, ErrGone) || len(f.cmds) != 0 {
		t.Fatalf("a window id that is an expression: %v %q", err, f.cmds)
	}
	// The foreground changed once the text was in: no Enter.
	f = &fakeKitty{fg: 20, fgAfter: 99, screen: emptyInput}
	s = &Sender{Run: f.run, ProcRoot: root, Pause: time.Millisecond}
	if _, err := s.Send(context.Background(), tgt, "[momentum] continue"); !errors.Is(err, ErrUnstable) {
		t.Fatalf("foreground changed: %v", err)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "send-key") {
			t.Fatalf("pressed enter: %q", f.cmds)
		}
	}
}

func TestACodexQueueThatFailedMayHaveTakenTheMessage(t *testing.T) {
	root := fake.ProcRoot(t, fake.Proc{PID: 21, Argv: []string{"codex"}, Start: 4})
	tgt := Target{Tool: "codex", PID: 21, Start: 4, Thread: "019a-thread"}
	for _, c := range []struct {
		err      error
		unstable bool
	}{{context.DeadlineExceeded, true}, {errors.New("exit status 1"), true}, {&exec.Error{Name: "codex", Err: exec.ErrNotFound}, false}} {
		s := &Sender{ProcRoot: root, Run: func(context.Context, string, string, ...string) ([]byte, error) { return nil, c.err }}
		if _, err := s.Send(context.Background(), tgt, "[momentum] continue"); errors.Is(err, ErrUnstable) != c.unstable {
			t.Errorf("%v: got %v, unstable %v", c.err, err, c.unstable)
		}
	}
}

func TestCodexQueueKeepsTheMessageOneArgument(t *testing.T) {
	var got []string
	s := &Sender{Run: func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return nil, nil
	}, ProcRoot: fake.ProcRoot(t)}
	if _, err := s.Send(context.Background(), Target{Tool: "codex", Thread: "th-1"}, "- a message that starts with a dash"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "codex|queue|--thread=th-1|--message=- a message that starts with a dash" {
		t.Fatalf("command %q", got)
	}
}

// tmuxServer starts a throwaway tmux server on a socket in a temporary directory, removed with it.
func tmuxServer(t *testing.T) (sock string, run func(args ...string) string) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	socketDir, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socketDir.Close() })
	sock = fmt.Sprintf("/proc/%d/fd/%d/tmux.sock", os.Getpid(), socketDir.Fd())
	// The server and its shells get a home of their own: an interactive shell saves its history there, not in
	// the home of whoever runs the test.
	env := append(os.Environ(), "HOME="+dir, "HISTFILE="+filepath.Join(dir, "history"))
	run = func(args ...string) string {
		cmd := exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() { exec.Command("tmux", "-S", sock, "kill-server").Run() })
	return sock, run
}

// startTicks is a live process's start, from its stat file.
func startTicks(t *testing.T, pid int) uint64 {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	n, _ := strconv.ParseUint(f[19], 10, 64)
	return n
}

// TestSendTypesIntoARealPane runs a throwaway tmux server whose shell runs an agent that draws an input line and
// reads one line, sends into it, and refuses the same agent once the shell stopped it.
func TestSendTypesIntoARealPane(t *testing.T) {
	sock, tmux := tmuxServer(t)
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	agentScript := filepath.Join(dir, "agent.sh")
	// Like Claude Code, it frames its input line with a rule above and below, and reads the line typed into it.
	fake.File(t, agentScript, "printf '%s\\n❯ \\n%s' '────────────────────────────────' '────────────────────────────────'\n"+
		"printf '\\033[1A\\033[3G'\nIFS= read -r line\nprintf '%s' \"$line\" > \"$1\"\nsleep 300\n")
	shellRC := filepath.Join(dir, "shell.sh")
	// The shell's own prompt looks like an empty input line: only the process checks tell the two apart.
	fake.File(t, shellRC, "export HISTFILE="+filepath.Join(dir, "history")+"\nexport PS1='────────────────────────────────\\n❯ '\nexec bash --norc --noprofile -i\n")
	tmux("new-session", "-d", "-x", "120", "-y", "20", "-s", "t", "bash "+shellRC)
	pane := tmux("display-message", "-p", "-t", "t", "#{pane_id}")
	shell, _ := strconv.Atoi(tmux("display-message", "-p", "-t", "t", "#{pane_pid}"))
	// Cleanups run last first: this one, before the directories go, ends the server and waits for the shell,
	// which writes its history as it exits.
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
		for i := 0; i < 200; i++ {
			if _, err := os.Stat("/proc/" + strconv.Itoa(shell)); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	waitFor(t, func() bool { return strings.Contains(tmux("capture-pane", "-p", "-t", pane), "❯") })
	tmux("send-keys", "-t", pane, "bash "+agentScript+" "+got, "Enter")
	agentPID := child(t, shell)
	waitFor(t, func() bool { return strings.Count(tmux("capture-pane", "-p", "-t", pane), "❯") >= 2 })

	s := &Sender{SocketPath: sock, Pause: 300 * time.Millisecond}
	tgt := Target{Tool: "claude", Pane: pane, PID: agentPID, Start: startTicks(t, agentPID)}
	if _, err := s.Send(context.Background(), tgt, "[momentum] continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(got); return string(b) == "[momentum] continue" })

	// The agent stopped by its shell: the shell's prompt shows, and nothing is typed into it.
	tmux("send-keys", "-t", pane, "C-z")
	waitFor(t, func() bool { return strings.Contains(tmux("capture-pane", "-p", "-t", pane), "Stopped") })
	if _, err := s.Send(context.Background(), tgt, "[momentum] again"); !errors.Is(err, ErrNotFront) {
		t.Fatalf("stopped: %v", err)
	}
	tmux("send-keys", "-t", pane, "fg", "Enter")
	waitFor(t, func() bool { st, _ := stat("/proc", agentPID); return st.state != 'T' })

	// The same pane in copy mode: nothing is typed.
	tmux("copy-mode", "-t", pane)
	if _, err := s.Send(context.Background(), tgt, "[momentum] again"); !errors.Is(err, ErrMode) {
		t.Fatalf("copy mode: %v", err)
	}
}

// child is the first child process of pid, once it has one.
func child(t *testing.T, pid int) int {
	var kid int
	waitFor(t, func() bool {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
		if err != nil {
			return false
		}
		f := strings.Fields(string(raw))
		if len(f) == 0 {
			return false
		}
		kid, _ = strconv.Atoi(f[0])
		return kid > 0
	})
	return kid
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
