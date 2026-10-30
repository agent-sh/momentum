// Package term reads terminals without touching them: the tmux panes on the machine, and the visible screen of a
// pane or of a kitty window. Nothing here sends keys, focuses a window or changes a layout.
package term

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pane is one tmux pane.
type Pane struct {
	ID         string // the pane id, e.g. %12; tmux reuses ids after its server restarts
	Session    string
	Window     string // window index
	Index      string // pane index
	WindowName string
	PID        int // the pane's shell
	Path       string
	Title      string
}

// Target is where the pane sits, as session:window.pane.
func (p Pane) Target() string { return p.Session + ":" + p.Window + "." + p.Index }

const paneFormat = "#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t#{window_name}\t#{pane_pid}\t#{pane_current_path}\t#{pane_title}"

// Timeout bounds every terminal command.
const Timeout = 10 * time.Second

// command runs a read-only terminal command; a child that keeps its pipe open past the command does not hold it.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd
}

// ListPanes returns every pane of the default tmux server. No tmux, or no server, is no panes. A tmux that does
// not answer in time, or by the caller's deadline, is an error: its panes are unknown, not gone.
func ListPanes(ctx context.Context) ([]Pane, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := command(ctx, "tmux", "list-panes", "-a", "-F", paneFormat).Output()
	if err != nil && ctx.Err() != nil { // killed on its context, which also exits with an error
		return nil, fmt.Errorf("tmux list-panes: %w", ctx.Err())
	}
	var exit *exec.ExitError
	if errors.Is(err, exec.ErrNotFound) || errors.As(err, &exit) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePanes(string(out)), nil
}

// ParsePanes reads list-panes output in the format ListPanes asks for. The title comes last, so a tab a program
// put in it stays in it.
func ParsePanes(out string) []Pane {
	var ps []Pane
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(l, "\t", 8)
		if len(f) != 8 {
			continue
		}
		pid, _ := strconv.Atoi(f[5])
		ps = append(ps, Pane{ID: f[0], Session: f[1], Window: f[2], Index: f[3], WindowName: f[4], PID: pid, Path: f[6], Title: f[7]})
	}
	return ps
}

// CapturePane returns the visible screen of a tmux pane, or "".
func CapturePane(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, _ := command(ctx, "tmux", "capture-pane", "-p", "-t", id).Output()
	return string(out)
}

// KittyEnv returns the kitty remote-control socket, window id and kitty pid a process inherited from the kitty
// window it runs in, read from its environment under procRoot.
func KittyEnv(procRoot string, pid int) (listen, window string, kittyPID int) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", "", 0
	}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if v, ok := strings.CutPrefix(kv, "KITTY_LISTEN_ON="); ok {
			listen = v
		} else if v, ok := strings.CutPrefix(kv, "KITTY_WINDOW_ID="); ok {
			window = v
		} else if v, ok := strings.CutPrefix(kv, "KITTY_PID="); ok {
			kittyPID, _ = strconv.Atoi(v)
		}
	}
	return listen, window, kittyPID
}

// KittyScreen returns the visible screen of the kitty window a process runs in, or "" when it is not in a kitty
// window with remote control.
func KittyScreen(ctx context.Context, procRoot string, pid int) string {
	listen, win, _ := KittyEnv(procRoot, pid)
	if listen == "" || win == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, _ := command(ctx, "kitty", "@", "--to", listen, "get-text", "--match", "id:"+win, "--extent", "screen").Output()
	return string(out)
}

var rePermission = regexp.MustCompile(`(?m)^\s*(Do you want to [^\n]{3,120}\?)`)

// PermissionPrompt returns the question of a Claude Code tool-permission dialog on a screen, or "".
func PermissionPrompt(screen string) string {
	if m := rePermission.FindStringSubmatch(screen); m != nil && strings.Contains(screen, "1. Yes") {
		return strings.TrimSpace(m[1])
	}
	return ""
}
