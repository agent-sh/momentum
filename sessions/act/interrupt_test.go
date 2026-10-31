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
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions/internal/fake"
)

const queuedHint = "\x1b[7m\x1b[39mP\x1b[0;2mress up to edit queued messages\x1b[0m"

// turnScreen is a Claude Code screen whose turn runs, with what its input line holds. As Claude Code does, its
// status line offers the interrupt key only while nothing is typed on the input line.
func turnScreen(input string) string {
	hint := ""
	if input == "" || input == queuedHint {
		hint = " · esc to interrupt"
	}
	return "● Reading the logs\n✻ Working… (31m 4s)\n\n" + ruleLine + "\n❯" + nbsp + input + "\n" + ruleLine + "\n  ⏵⏵ auto mode on" + hint
}

// fakeTurn answers the tmux commands Interrupt runs for a pane whose turn runs: the paste shows on the input line,
// Enter moves it to the queue and the input line says a message waits there, Escape is recorded.
type fakeTurn struct {
	screen   string // the screen before anything is typed; "" is a running turn with an empty input line
	mode     string
	line     string // what the input line holds now
	pasted   string
	entered  bool
	noQueue  bool // Enter submits instead of queueing: the turn ends and the hint never shows
	ends     bool // the turn ends as the paste lands: the input line holds it, and no turn runs
	newTurn  bool // Enter submits it as the prompt of a new turn, which runs: the input line is empty, no hint
	late     int  // reads after the paste that still show the line empty
	keys     []string
	commands []string
}

func (f *fakeTurn) run(_ context.Context, stdin, name string, args ...string) ([]byte, error) {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	if name != "tmux" {
		return nil, fmt.Errorf("unexpected %s", name)
	}
	switch args[0] {
	case "display-message":
		mode := f.mode
		if mode == "" {
			mode = "0"
		}
		return []byte(fmt.Sprintf("0 %s 0 10 %s\n", mode, paneTTY)), nil
	case "capture-pane":
		switch {
		case f.screen != "" && f.pasted == "":
			return []byte(f.screen), nil
		case f.entered && f.noQueue:
			return []byte(emptyInput), nil
		case f.entered && f.newTurn:
			return []byte(turnScreen("")), nil
		case f.entered:
			return []byte(turnScreen(queuedHint)), nil
		case f.ends && f.pasted != "":
			return []byte(emptyInputWith(f.line)), nil
		case f.pasted != "" && f.late > 0:
			f.late--
			return []byte(turnScreen("")), nil
		}
		return []byte(turnScreen(f.line)), nil
	case "load-buffer":
		f.pasted = stdin
	case "paste-buffer":
		f.line = f.pasted
	case "send-keys":
		key := args[len(args)-1]
		f.keys = append(f.keys, key)
		if key == "Enter" {
			f.entered, f.line = true, ""
		}
	}
	return nil, nil
}

func turnPane(t *testing.T, f *fakeTurn) (*Sender, Target) {
	root := fake.ProcRoot(t, fake.Proc{PID: 10, PPID: 1, Argv: []string{"zsh"}, Start: 5, PGRP: 10, TPGID: 11, TTY: paneTTYNr}, agent)
	return &Sender{Run: f.run, ProcRoot: root, Pause: time.Millisecond}, Target{Tool: "claude", Pane: "%3", PID: 11, Start: 6}
}

func TestInterruptQueuesTheMessageThenPressesEscape(t *testing.T) {
	f := &fakeTurn{late: settleReads - 1}
	s, tgt := turnPane(t, f)
	path, err := s.Interrupt(context.Background(), tgt, "[momentum] Your last request got\nno reply.", nil)
	if err != nil || path != Tmux {
		t.Fatalf("interrupt: %v %v", path, err)
	}
	if f.pasted != "[momentum] Your last request got no reply." || strings.Join(f.keys, " ") != "Enter Escape" {
		t.Fatalf("pasted %q, keys %q", f.pasted, f.keys)
	}
}

func TestTheTurnRunsByClaudeCodesOwnRows(t *testing.T) {
	box := ruleLine + "\n❯" + nbsp + "\n" + ruleLine + "\n"
	for name, screen := range map[string]string{
		"the interrupt key on the status line": "output\n\n" + box + "  ⏵⏵ auto mode on · esc to interrupt",
		"its running row":                      "output\n✻ Fermenting… (35m 11s)\n\n" + box + status,
		"a running row of several words":       "✻ Running the test suite… (2m · ↓ 3k tokens)\n  ⎿  Tip: a tip\n\n" + box + status,
		"the waiting notice in its place":      "Waiting for API response · will retry in 31s · check your network\n" + box + status,
		"the retry notice in its place":        "✻ Request timed out. · Retrying in 4s · attempt 3/10\n" + box + status,
	} {
		if !TurnRuns(screen, "claude") {
			t.Errorf("%s: not running", name)
		}
	}
	for name, screen := range map[string]string{
		"the row a finished turn leaves":      "✻ Worked for 3m 2s\n\n" + box + status,
		"retry words in a tool's output":      "ran it: retrying in 5s (attempt 2/3)\n" + box + status,
		"the agent's words about waiting":     "I am Waiting for API response from the vendor.\n" + box + status,
		"a bullet that trails off":            "* Next…\n" + box + status,
		"the key in the conversation above":   "press esc to interrupt it\n" + box + status,
		"a running row far up in the history": "✻ Fermenting… (35m 11s)\none\ntwo\nthree\n" + box + status,
		"no input box":                        "✻ Fermenting… (35m 11s)\nesc to interrupt\n",
	} {
		if TurnRuns(screen, "claude") {
			t.Errorf("%s: running", name)
		}
	}
	for _, hint := range []string{queuedHint, dim + "Press up to select a queued message to edit, or Enter to send them now" + reset} {
		if !Queued(turnScreen(hint), "claude") {
			t.Errorf("%q: no message queued", hint)
		}
	}
	if Queued("the queued messages went out\n"+emptyInput, "claude") || Queued(turnScreen("why are queued messages stuck"), "claude") {
		t.Error("words in the transcript, or typed on the line, are not the harness's hint")
	}
}

func TestInterruptRefusesBeforeTypingAnything(t *testing.T) {
	cases := []struct {
		name, screen, mode string
		tool               string
		want               error
	}{
		{"no turn running", emptyInput, "", "claude", ErrIdle},
		{"a dialog", turnScreen("") + "\n  Do you want to proceed?\n  ❯ 1. Yes\n    2. No\n", "", "claude", ErrDialog},
		{"a draft", turnScreen("half typed"), "", "claude", ErrDraft},
		{"a draft under a running row of several words", strings.Replace(turnScreen("half typed"), "✻ Working…", "✻ Running the test suite…", 1), "", "claude", ErrDraft},
		{"a draft that names the queue", turnScreen("why are queued messages stuck"), "", "claude", ErrDraft},
		{"a message already queued", turnScreen(queuedHint), "", "claude", ErrQueued},
		{"copy mode", turnScreen(""), "1", "claude", ErrMode},
		{"no input line", "✻ Working… (esc to interrupt)\nplain output\n", "", "claude", ErrNoInput},
		{"a harness whose queue it does not know", turnScreen(""), "", "codex", ErrNoPath},
	}
	for _, c := range cases {
		f := &fakeTurn{screen: c.screen, mode: c.mode}
		s, tgt := turnPane(t, f)
		tgt.Tool = c.tool
		if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", nil); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if f.pasted != "" || len(f.keys) != 0 {
			t.Errorf("%s: typed: pasted %q keys %q", c.name, f.pasted, f.keys)
		}
	}
	f := &fakeTurn{}
	s, tgt := turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "/compact", nil); !errors.Is(err, ErrCommand) || f.pasted != "" {
		t.Fatalf("a command: %v", err)
	}
	// still fails before the paste: nothing is typed, and its error comes back as it is.
	answered := errors.New("answered")
	f = &fakeTurn{}
	s, tgt = turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", func() error { return answered }); !errors.Is(err, answered) || errors.Is(err, ErrUnstable) ||
		f.pasted != "" || len(f.keys) != 0 {
		t.Fatalf("still: %v, pasted %q keys %q", err, f.pasted, f.keys)
	}
	// The same checks are CheckInterrupt's.
	f = &fakeTurn{screen: emptyInput}
	s, tgt = turnPane(t, f)
	if _, err := s.CheckInterrupt(context.Background(), tgt); !errors.Is(err, ErrIdle) || len(f.keys) != 0 {
		t.Fatalf("check: %v", err)
	}
}

func TestInterruptPressesEscapeOnlyOnceTheMessageWaitsInTheQueue(t *testing.T) {
	// The paste never shows on the input line: no Enter.
	f := &fakeTurn{late: settleReads}
	s, tgt := turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", nil); !errors.Is(err, ErrUnstable) || len(f.keys) != 0 {
		t.Fatalf("never drawn: %v keys %q", err, f.keys)
	}
	// The turn ends as the paste lands: no Enter, which would make the message its prompt.
	f = &fakeTurn{ends: true}
	s, tgt = turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", nil); !errors.Is(err, ErrUnstable) || len(f.keys) != 0 {
		t.Fatalf("ended at the paste: %v keys %q", err, f.keys)
	}
	// still fails before Enter: no Enter.
	calls := 0
	f = &fakeTurn{}
	s, tgt = turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", func() error {
		if calls++; calls > 1 {
			return errors.New("answered")
		}
		return nil
	}); !errors.Is(err, ErrUnstable) || len(f.keys) != 0 {
		t.Fatalf("still before Enter: %v keys %q", err, f.keys)
	}
	// still fails before the Escape (its reply came meanwhile): the message waits in the queue, and no Escape.
	calls = 0
	f = &fakeTurn{}
	s, tgt = turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", func() error {
		if calls++; calls > 2 {
			return errors.New("answered")
		}
		return nil
	}); !errors.Is(err, ErrUnstable) || strings.Join(f.keys, " ") != "Enter" {
		t.Fatalf("still before the Escape: %v keys %q", err, f.keys)
	}
	// Enter submitted it instead (the turn had just ended): no Escape, which would end the turn it opened.
	for name, f := range map[string]*fakeTurn{"the turn ended": {noQueue: true}, "a new turn runs it": {newTurn: true}} {
		s, tgt = turnPane(t, f)
		if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", nil); !errors.Is(err, ErrUnstable) || strings.Join(f.keys, " ") != "Enter" {
			t.Fatalf("%s: %v keys %q", name, err, f.keys)
		}
	}
}

// TestInterruptTypesIntoARealPane runs a throwaway tmux server whose shell runs an agent that draws a running turn
// and an input line, reads the line typed into it, then draws the queue's hint and waits for a key.
func TestInterruptTypesIntoARealPane(t *testing.T) {
	sock, tmux := tmuxServer(t)
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	agentScript := filepath.Join(dir, "agent.sh")
	rule := "────────────────────────────────"
	fake.File(t, agentScript, "draw() { printf '\\033[2J\\033[H✻ Working… (31m · esc to interrupt)\\n%s\\n❯ %s\\n%s' '"+rule+"' \"$1\" '"+rule+"'; printf '\\033[1A\\033[3G'; }\n"+
		"draw ''\nIFS= read -r line\nprintf '%s' \"$line\" > \"$1.queued\"\n"+
		"draw \"$(printf '\\033[2mPress up to edit queued messages\\033[0m')\"\nIFS= read -rsn1 key\n"+
		"[ \"$key\" = \"$(printf '\\033')\" ] && printf escape > \"$1.key\"\nsleep 300\n")
	tmux("new-session", "-d", "-x", "120", "-y", "20", "-s", "t", "bash --norc --noprofile -i")
	pane := tmux("display-message", "-p", "-t", "t", "#{pane_id}")
	shell, _ := strconv.Atoi(tmux("display-message", "-p", "-t", "t", "#{pane_pid}"))
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
		for i := 0; i < 200; i++ {
			if _, err := os.Stat("/proc/" + strconv.Itoa(shell)); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	tmux("send-keys", "-t", pane, "bash "+agentScript+" "+got, "Enter")
	agentPID := child(t, shell)
	waitFor(t, func() bool { return strings.Contains(tmux("capture-pane", "-p", "-t", pane), "esc to interrupt") })

	s := &Sender{SocketPath: sock, Pause: 300 * time.Millisecond}
	tgt := Target{Tool: "claude", Pane: pane, PID: agentPID, Start: startTicks(t, agentPID)}
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue", nil); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(got + ".queued"); return string(b) == "[momentum] continue" })
	waitFor(t, func() bool { b, _ := os.ReadFile(got + ".key"); return string(b) == "escape" })
}
