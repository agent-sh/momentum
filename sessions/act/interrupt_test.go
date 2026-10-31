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

// turnScreen is a Claude Code screen whose turn runs, with what its input line holds.
func turnScreen(input string) string {
	return "● Reading the logs\n✻ Working… (31m 4s · esc to interrupt)\n\n" + ruleLine + "\n❯" + nbsp + input + "\n" + ruleLine + "\n" + status
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
	path, err := s.Interrupt(context.Background(), tgt, "[momentum] Your last request got\nno reply.")
	if err != nil || path != Tmux {
		t.Fatalf("interrupt: %v %v", path, err)
	}
	if f.pasted != "[momentum] Your last request got no reply." || strings.Join(f.keys, " ") != "Enter Escape" {
		t.Fatalf("pasted %q, keys %q", f.pasted, f.keys)
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
		{"a message already queued", turnScreen(queuedHint), "", "claude", ErrQueued},
		{"copy mode", turnScreen(""), "1", "claude", ErrMode},
		{"no input line", "✻ Working… (esc to interrupt)\nplain output\n", "", "claude", ErrNoInput},
		{"a harness whose queue it does not know", turnScreen(""), "", "codex", ErrNoPath},
	}
	for _, c := range cases {
		f := &fakeTurn{screen: c.screen, mode: c.mode}
		s, tgt := turnPane(t, f)
		tgt.Tool = c.tool
		if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue"); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if f.pasted != "" || len(f.keys) != 0 {
			t.Errorf("%s: typed: pasted %q keys %q", c.name, f.pasted, f.keys)
		}
	}
	f := &fakeTurn{}
	s, tgt := turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "/compact"); !errors.Is(err, ErrCommand) || f.pasted != "" {
		t.Fatalf("a command: %v", err)
	}
}

func TestInterruptPressesEscapeOnlyOnceTheMessageWaitsInTheQueue(t *testing.T) {
	// The paste never shows on the input line: no Enter.
	f := &fakeTurn{late: settleReads}
	s, tgt := turnPane(t, f)
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue"); !errors.Is(err, ErrUnstable) || len(f.keys) != 0 {
		t.Fatalf("never drawn: %v keys %q", err, f.keys)
	}
	// Enter submitted it instead (the turn had just ended): no Escape, which would end the turn it opened.
	for name, f := range map[string]*fakeTurn{"the turn ended": {noQueue: true}, "a new turn runs it": {newTurn: true}} {
		s, tgt = turnPane(t, f)
		if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue"); !errors.Is(err, ErrUnstable) || strings.Join(f.keys, " ") != "Enter" {
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
	if _, err := s.Interrupt(context.Background(), tgt, "[momentum] continue"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(got + ".queued"); return string(b) == "[momentum] continue" })
	waitFor(t, func() bool { b, _ := os.ReadFile(got + ".key"); return string(b) == "escape" })
}
