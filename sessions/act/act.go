// Package act types a message into a live agent session and submits it, the way its owner would at the
// keyboard: a bracketed paste into the session's tmux pane or kitty window followed by Enter, or Codex's own
// queue for a thread with no terminal of its own. It is the action half of the session layer; package sessions
// only reads.
//
// It types only into a session it can show is waiting for input. Before it pastes, it reads the terminal and the
// screen and refuses when the terminal does not run the session's process in its foreground, a tmux mode owns
// the pane's keyboard or the pane's input goes to other panes too, a dialog or a menu is showing (typed text
// would answer it and Enter would pick its highlighted option), or the input line holds text someone typed and
// has not sent. After the paste it makes the same checks again and presses Enter only when the input line holds
// the message and nothing else. A refusal names its reason (the Err values).
package act

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/proc"
	"github.com/agent-sh/momentum/sessions/term"
)

// Path is how a message reaches a session.
type Path string

const (
	Tmux       Path = "tmux"        // pasted into the session's tmux pane, then Enter
	Kitty      Path = "kitty"       // sent to the session's kitty window by remote control, then the enter key
	CodexQueue Path = "codex-queue" // queued for the Codex thread by `codex queue`
)

// Refusals before the paste: nothing was typed.
var (
	ErrNoPath    = errors.New("no terminal or queue reaches it")
	ErrGone      = errors.New("its terminal does not run its process")
	ErrNotFront  = errors.New("its process is stopped or not its terminal's foreground")
	ErrMode      = errors.New("a tmux mode owns the pane's keyboard")
	ErrSync      = errors.New("the pane's input goes to other panes too")
	ErrDialog    = errors.New("a dialog or menu is on its screen")
	ErrBusy      = errors.New("its screen shows a turn running")
	ErrDraft     = errors.New("its input line holds text nobody sent")
	ErrNoInput   = errors.New("no input line on its screen")
	ErrCommand   = errors.New("the message would run as a command")
	ErrMessage   = errors.New("the message reads like the interface's own dialog or status")
	ErrNoMessage = errors.New("nothing to send")
	// Refusals of Interrupt before it types: the screen shows no turn to interrupt, or a message already waits in
	// the harness's queue, which Escape would send.
	ErrIdle   = errors.New("its screen shows no turn running")
	ErrQueued = errors.New("its screen shows a message waiting in its queue")
)

// ErrUnstable means the paste began and the message was not known to be submitted: after the paste a check
// failed or the input line did not hold the message alone, so Enter was not pressed, or a command from the paste
// on failed. The message may sit on the input line unsent; when Enter itself failed, it may have gone through.
var ErrUnstable = errors.New("pasted, but not known to be submitted")

// Target is a session as a sender needs it: where it runs and which process is its.
type Target struct {
	Tool       string // claude, codex, pi, opencode, hermes
	Name       string
	Pane       string // a tmux pane id
	PID        int    // the session's agent process
	Start      uint64 // its start in clock ticks, telling it from a later process with the pid
	Thread     string // a Codex thread id, for Codex's queue
	Background bool   // a Claude Code background session, which has no terminal of its own
}

// TargetOf is the target of a session found by package sessions. start is its process's start ticks, from the
// snapshot that found it.
func TargetOf(s *sessions.Session, start uint64) Target {
	t := Target{Tool: s.Tool, Name: s.Name, Pane: s.Pane, PID: s.PID, Start: start, Background: s.Where == "bg"}
	if s.Tool == "codex" && !strings.Contains(s.Key, ":proc:") {
		t.Thread = s.Key // a Codex session is keyed by its thread
	}
	return t
}

// Runner runs a command with stdin and returns its standard output.
type Runner func(ctx context.Context, stdin, name string, args ...string) ([]byte, error)

// Sender types messages into sessions. The zero value runs the real tmux, kitty and codex commands on the
// default tmux server and reads /proc.
type Sender struct {
	Run        Runner        // nil runs the command
	ProcRoot   string        // the proc filesystem; "" is /proc
	Socket     string        // a tmux server's socket name (tmux -L); "" is the default server
	SocketPath string        // a tmux server's socket path (tmux -S), which wins over Socket
	Pause      time.Duration // between the paste and Enter, so the interface takes the paste first; 0 is 250 ms
	Codex      string        // the codex command for its queue; "" is codex on PATH
}

// Timeout bounds every command a send runs.
const Timeout = 10 * time.Second

func (s *Sender) run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	if s.Run != nil {
		return s.Run(ctx, stdin, name, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func (s *Sender) tmux(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	switch {
	case s.SocketPath != "":
		args = append([]string{"-S", s.SocketPath}, args...)
	case s.Socket != "":
		args = append([]string{"-L", s.Socket}, args...)
	}
	return s.run(ctx, stdin, "tmux", args...)
}

func (s *Sender) procRoot() string {
	if s.ProcRoot == "" {
		return "/proc"
	}
	return s.ProcRoot
}

// Choose is the path a message to t would take: its tmux pane, else the kitty window its process runs in, else
// Codex's queue for its thread. A background session has none.
func (s *Sender) Choose(t Target) (Path, error) {
	switch {
	case t.Background:
		return "", ErrNoPath
	case t.Pane != "":
		return Tmux, nil
	case t.PID > 0 && kittyWindow(s.procRoot(), t.PID):
		return Kitty, nil
	case t.Tool == "codex" && t.Thread != "":
		return CodexQueue, nil
	}
	return "", ErrNoPath
}

func kittyWindow(root string, pid int) bool {
	listen, win, _ := term.KittyEnv(root, pid)
	return listen != "" && win != ""
}

// Check reports whether a message could be typed into t now, and by which path: the checks Send makes before it
// types anything.
func (s *Sender) Check(ctx context.Context, t Target) (Path, error) {
	path, err := s.Choose(t)
	if err != nil {
		return "", err
	}
	switch path {
	case Tmux:
		_, err = s.tmuxReady(ctx, t, true)
	case Kitty:
		_, err = s.kittyReady(ctx, t, true)
	}
	return path, err
}

// CheckText reports whether text can be sent as a message at all, as SafeText makes it: not empty, not a command
// the interface would run, and not reading like the interface's own text. Its refusals never pass by themselves.
func CheckText(text string) error {
	text = SafeText(text)
	switch {
	case text == "":
		return ErrNoMessage
	case command(text):
		return ErrCommand
	case Dialog(claudeGlyph+" "+text) != "" || Dialog(text) != "" || Running(text):
		// the check after the paste would read it as a dialog and leave it on the input line
		return ErrMessage
	}
	return nil
}

// Send types text into t and submits it, after the checks Check makes. text is typed as SafeText makes it. The
// path is returned also when the send fails.
func (s *Sender) Send(ctx context.Context, t Target, text string) (Path, error) {
	if err := CheckText(text); err != nil {
		return "", err
	}
	text = SafeText(text)
	path, err := s.Choose(t)
	if err != nil {
		return "", err
	}
	switch path {
	case Tmux:
		return path, s.sendTmux(ctx, t, text)
	case Kitty:
		return path, s.sendKitty(ctx, t, text)
	default:
		codex := s.Codex
		if codex == "" {
			codex = "codex"
		}
		// --message=TEXT: a message that starts with a dash stays a message.
		if _, err := s.run(ctx, "", codex, "queue", "--thread="+t.Thread, "--message="+text); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return path, err // it never ran
			}
			// a timeout or a failure after it started does not say whether the thread took the message
			return path, fmt.Errorf("%w: codex queue: %v", ErrUnstable, err)
		}
		return path, nil
	}
}

func (s *Sender) pause() {
	d := s.Pause
	if d == 0 {
		d = 250 * time.Millisecond
	}
	time.Sleep(d)
}

// front checks that t's process is the one the target names, runs under within when it is not 0 (a pane's
// shell), is not stopped, and is in its terminal's foreground process group: keys typed into the terminal go to
// it, not to a shell that stopped it. tty, when not "", is the terminal the keys go to (a pane's): the process's
// controlling terminal must be it, or another program reads them while the process sits in a terminal of its own.
func (s *Sender) front(t Target, within int, tty string) error {
	if t.PID <= 0 || t.Start == 0 {
		return ErrGone // a process nobody can tell from a later one with its pid
	}
	tab, err := proc.Read(s.procRoot())
	if err != nil {
		return fmt.Errorf("process table: %w", err)
	}
	if !tab.Alive(t.PID, t.Start) {
		return ErrGone
	}
	if within > 0 {
		under := false
		for p := t.PID; p > 1; {
			if p == within {
				under = true
				break
			}
			pr := tab.ByPID[p]
			if pr == nil {
				break
			}
			p = pr.PPID
		}
		if !under {
			return ErrGone
		}
	}
	st, err := stat(s.procRoot(), t.PID)
	switch {
	case err != nil:
		return ErrGone
	case st.state == 'T' || st.state == 't' || st.state == 'Z':
		return ErrNotFront
	case st.tpgid <= 0 || st.pgrp != st.tpgid:
		return ErrNotFront
	}
	if tty != "" {
		var fi syscall.Stat_t
		if err := syscall.Stat(tty, &fi); err != nil || !sameDevice(uint64(fi.Rdev), st.tty) {
			return ErrNotFront
		}
	}
	return nil
}

// sameDevice reports whether a device number as stat gives it (st_rdev) and a stat file's tty_nr name the same
// terminal. They encode a major and a minor number differently.
func sameDevice(rdev uint64, ttyNr int) bool {
	if ttyNr <= 0 {
		return false
	}
	major := (rdev>>8)&0xfff | (rdev>>32)&^0xfff
	minor := rdev&0xff | (rdev>>12)&^0xff
	n := uint64(ttyNr)
	return major == (n>>8)&0xfff && minor == n&0xff|(n>>12)&0xfff00
}

type procStat struct {
	state     byte
	pgrp, tty int
	tpgid     int
}

// stat reads a process's state, process group, controlling terminal and its terminal's foreground process group
// from its stat file. The command name in parentheses may hold spaces and parentheses, so fields count from the
// last ')'.
func stat(root string, pid int) (procStat, error) {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	str := string(raw)
	i := strings.LastIndexByte(str, ')')
	if i < 0 || i+2 > len(str) {
		return procStat{}, errors.New("stat: no command name")
	}
	f := strings.Fields(str[i+2:])
	if len(f) < 6 || len(f[0]) != 1 {
		return procStat{}, errors.New("stat: short")
	}
	pgrp, err1 := strconv.Atoi(f[2])
	tty, err2 := strconv.Atoi(f[4])
	tpgid, err3 := strconv.Atoi(f[5])
	if err1 != nil || err2 != nil || err3 != nil {
		return procStat{}, errors.New("stat: bad numbers")
	}
	return procStat{state: f[0][0], pgrp: pgrp, tty: tty, tpgid: tpgid}, nil
}

// tmuxReady checks t's pane and returns its styled screen. empty asks for an input line with nothing on it;
// after a paste it holds the message, so the caller checks that itself.
func (s *Sender) tmuxReady(ctx context.Context, t Target, empty bool) (string, error) {
	screen, err := s.tmuxScreen(ctx, t)
	if err != nil {
		return "", err
	}
	return screen, ready(screen, t.Tool, empty)
}

// tmuxScreen checks that keys typed into t's pane reach t's process and nothing else, and returns the pane's styled
// screen: the pane is live, no tmux mode owns its keyboard, its input goes to it alone, and its terminal runs the
// process in its foreground.
func (s *Sender) tmuxScreen(ctx context.Context, t Target) (string, error) {
	out, err := s.tmux(ctx, "", "display-message", "-p", "-t", t.Pane, "#{pane_dead} #{pane_in_mode} #{pane_synchronized} #{pane_pid} #{pane_tty}")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGone, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 5 || f[0] != "0" {
		return "", ErrGone
	}
	if f[1] != "0" {
		return "", ErrMode
	}
	if f[2] == "1" {
		return "", ErrSync
	}
	shell, err := strconv.Atoi(f[3])
	if err != nil || shell <= 0 {
		return "", ErrGone
	}
	if err := s.front(t, shell, f[4]); err != nil {
		return "", err
	}
	screen, err := s.tmux(ctx, "", "capture-pane", "-p", "-e", "-t", t.Pane)
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	return string(screen), nil
}

// ready checks a styled screen: no dialog, no turn running, and an input line of the tool's, with nothing typed
// on it when empty.
func ready(styled, tool string, empty bool) error {
	plain := Plain(styled)
	if d := Dialog(plain); d != "" {
		return fmt.Errorf("%w: %s", ErrDialog, d)
	}
	if Running(plain) {
		return ErrBusy
	}
	typed, ok := Draft(styled, tool)
	switch {
	case !ok:
		return ErrNoInput
	case empty && typed != "":
		return ErrDraft
	}
	return nil
}

// settleReads is how many times the screen is read after the paste for the message to show on the input line: a
// busy terminal can draw it a moment late.
const settleReads = 4

// settled reads the screen after the paste, a pause apart, until the input line holds the message alone, at most
// settleReads times. A screen caught mid-redraw, without its input line yet, is read again; any other check that
// fails on the way (a dialog, a turn running, a mode, another process in front) ends it at once.
func (s *Sender) settled(read func() (string, error), tool, text string) error {
	var err error
	for range settleReads {
		s.pause()
		var screen string
		if screen, err = read(); err != nil && !errors.Is(err, ErrNoInput) {
			return fmt.Errorf("%w: %v", ErrUnstable, err)
		}
		if err == nil && holds(screen, tool, text) {
			return nil
		}
		if err == nil {
			err = errors.New("the input line does not hold the message alone")
		}
	}
	return fmt.Errorf("%w: %v", ErrUnstable, err)
}

// holds reports whether the input line on a styled screen holds the message and nothing else. A long message
// wraps across lines, so the two compare without their whitespace.
func holds(styled, tool, text string) bool {
	typed, ok := Draft(styled, tool)
	return ok && squeeze(typed) == squeeze(text)
}

func (s *Sender) sendTmux(ctx context.Context, t Target, text string) error {
	if _, err := s.tmuxReady(ctx, t, true); err != nil {
		return err
	}
	if err := s.pasteTmux(ctx, t, text); err != nil {
		return err
	}
	if err := s.settled(func() (string, error) { return s.tmuxReady(ctx, t, false) }, t.Tool, text); err != nil {
		return err
	}
	if _, err := s.tmux(ctx, "", "send-keys", "-t", t.Pane, "Enter"); err != nil {
		return fmt.Errorf("%w: enter: %v", ErrUnstable, err)
	}
	return nil
}

// dropBuffer deletes a paste buffer left behind; the send's own context may be done, so it has its own.
func (s *Sender) dropBuffer(buf string) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	s.tmux(ctx, "", "delete-buffer", "-b", buf)
}

func (s *Sender) kitty(ctx context.Context, t Target, stdin string, args ...string) ([]byte, error) {
	listen, win, _ := term.KittyEnv(s.procRoot(), t.PID)
	if n, err := strconv.Atoi(win); listen == "" || err != nil || n <= 0 {
		return nil, ErrGone // a window id is a number; anything else would widen kitty's match
	}
	cmd := append([]string{"@", "--to", listen, args[0], "--match", "id:" + win}, args[1:]...)
	return s.run(ctx, stdin, "kitty", cmd...)
}

// kittyRuns reports whether the kitty window runs pid in its foreground: a process that inherited the window's
// id (a tmux server started from it) is elsewhere.
func (s *Sender) kittyRuns(ctx context.Context, t Target) error {
	out, err := s.kitty(ctx, t, "", "ls")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	var ls []struct {
		Tabs []struct {
			Windows []struct {
				ID         int `json:"id"`
				Foreground []struct {
					PID int `json:"pid"`
				} `json:"foreground_processes"`
			} `json:"windows"`
		} `json:"tabs"`
	}
	if json.Unmarshal(out, &ls) != nil {
		return ErrGone
	}
	_, win, _ := term.KittyEnv(s.procRoot(), t.PID)
	var windows int
	runs := false
	for _, w0 := range ls {
		for _, tab := range w0.Tabs {
			for _, w := range tab.Windows {
				windows++
				if strconv.Itoa(w.ID) != win {
					return ErrGone
				}
				for _, p := range w.Foreground {
					runs = runs || p.PID == t.PID
				}
			}
		}
	}
	switch {
	case windows != 1: // the match names one window, or something widened it
		return ErrGone
	case !runs:
		return ErrNotFront
	}
	return nil
}

func (s *Sender) kittyReady(ctx context.Context, t Target, empty bool) (string, error) {
	screen, err := s.kittyScreen(ctx, t)
	if err != nil {
		return "", err
	}
	return screen, ready(screen, t.Tool, empty)
}

// kittyScreen checks that keys sent to t's kitty window reach t's process, and returns the window's styled screen.
func (s *Sender) kittyScreen(ctx context.Context, t Target) (string, error) {
	if err := s.front(t, 0, ""); err != nil { // kitty lists the window's foreground from its own terminal
		return "", err
	}
	if err := s.kittyRuns(ctx, t); err != nil {
		return "", err
	}
	screen, err := s.kitty(ctx, t, "", "get-text", "--extent", "screen", "--ansi")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGone, err)
	}
	return string(screen), nil
}

func (s *Sender) sendKitty(ctx context.Context, t Target, text string) error {
	if _, err := s.kittyReady(ctx, t, true); err != nil {
		return err
	}
	// --stdin sends the bytes as they are; the positional form would read escapes in the text.
	if _, err := s.kitty(ctx, t, text, "send-text", "--stdin", "--bracketed-paste=auto"); err != nil {
		return fmt.Errorf("%w: send-text: %v", ErrUnstable, err)
	}
	if err := s.settled(func() (string, error) { return s.kittyReady(ctx, t, false) }, t.Tool, text); err != nil {
		return err
	}
	if _, err := s.kitty(ctx, t, "", "send-key", "enter"); err != nil {
		return fmt.Errorf("%w: enter: %v", ErrUnstable, err)
	}
	return nil
}
