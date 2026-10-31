package act

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// reQueuedHint is what Claude Code writes on its input line while a message waits in its queue.
var reQueuedHint = regexp.MustCompile(`(?i)\bpress up to edit queued messages\b`)

// Queued reports a screen whose input line says a message waits in the harness's queue.
func Queued(screen string) bool { return reQueuedHint.MatchString(bottom(screen)) }

// bottom is the part of a screen a harness draws its input line, its dialogs and its running line in.
func bottom(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > dialogLines {
		lines = lines[len(lines)-dialogLines:]
	}
	return strings.Join(lines, "\n")
}

// Interrupt resumes a Claude Code turn whose model request never got its reply, the way its owner breaks one: it
// types text into the input line while the turn runs, so the harness holds it in its queue, and presses Escape,
// which ends the turn and sends what the queue holds as the next prompt. Pressing Escape alone, with nothing
// queued, takes the turn back without a record and puts its prompt back on the input line. text is typed as
// SafeText makes it.
//
// Before the paste it checks what Send checks (the message, the terminal, no dialog, an input line with nothing
// typed on it), except that the screen must show the turn running and no message waiting in the queue: ErrIdle
// and ErrQueued, and nothing was typed. After the paste it presses Enter only when the input line holds the message
// alone and the turn still runs; it presses Escape only once the input line is empty again and says that a
// message waits in the queue, with the turn still running and no dialog. A check that fails from the paste on, or
// a command that fails, is ErrUnstable: the message may sit on the input line, or in the queue. That the turn
// ended and the message went in is the transcript's to tell: Interrupt does not wait for it.
func (s *Sender) Interrupt(ctx context.Context, t Target, text string) (Path, error) {
	if err := CheckText(text); err != nil {
		return "", err
	}
	text = SafeText(text)
	if t.Tool != "claude" {
		return "", ErrNoPath // the queue and the key are Claude Code's
	}
	path, err := s.Choose(t)
	if err != nil {
		return "", err
	}
	var term keys
	switch path {
	case Tmux:
		term = keys{
			screen: func() (string, error) { return s.tmuxScreen(ctx, t) },
			paste:  func(text string) error { return s.pasteTmux(ctx, t, text) },
			press: func(key string) error {
				_, err := s.tmux(ctx, "", "send-keys", "-t", t.Pane, map[string]string{"enter": "Enter", "escape": "Escape"}[key])
				return err
			},
		}
	case Kitty:
		term = keys{
			screen: func() (string, error) { return s.kittyScreen(ctx, t) },
			paste: func(text string) error {
				if _, err := s.kitty(ctx, t, text, "send-text", "--stdin", "--bracketed-paste=auto"); err != nil {
					return fmt.Errorf("%w: send-text: %v", ErrUnstable, err)
				}
				return nil
			},
			press: func(key string) error { _, err := s.kitty(ctx, t, "", "send-key", key); return err },
		}
	default:
		return path, ErrNoPath
	}
	return path, s.interrupt(term, t.Tool, text)
}

// keys is a terminal as Interrupt drives it: its checked screen, a paste, and a key.
type keys struct {
	screen func() (string, error)
	paste  func(text string) error
	press  func(key string) error
}

func (s *Sender) interrupt(term keys, tool, text string) error {
	screen, err := term.screen()
	if err != nil {
		return err
	}
	if err := running(screen, tool, ""); err != nil {
		return err
	}
	if err := term.paste(text); err != nil {
		return err
	}
	if err := s.until(term.screen, func(screen string) error { return running(screen, tool, text) }); err != nil {
		return err
	}
	if err := term.press("enter"); err != nil {
		return fmt.Errorf("%w: enter: %v", ErrUnstable, err)
	}
	if err := s.until(term.screen, func(screen string) error { return waiting(screen, tool) }); err != nil {
		return err
	}
	if err := term.press("escape"); err != nil {
		return fmt.Errorf("%w: escape: %v", ErrUnstable, err)
	}
	return nil
}

// running checks a styled screen of a turn running on it: no dialog, the turn's running line, nothing waiting in
// the queue, and an input line of the tool's that holds text and nothing else ("" for nothing typed).
func running(styled, tool, text string) error {
	plain := Plain(styled)
	if d := Dialog(plain); d != "" {
		return fmt.Errorf("%w: %s", ErrDialog, d)
	}
	if !Running(plain) {
		return ErrIdle
	}
	if Queued(plain) {
		return ErrQueued
	}
	typed, ok := Draft(styled, tool)
	switch {
	case !ok:
		return ErrNoInput
	case text == "" && typed != "":
		return ErrDraft
	case squeeze(typed) != squeeze(text):
		return errors.New("the input line does not hold the message alone")
	}
	return nil
}

// waiting checks a styled screen after Enter put the message in the queue: the turn still runs, no dialog, the
// input line is empty again and says that a message waits in the queue.
func waiting(styled, tool string) error {
	plain := Plain(styled)
	if d := Dialog(plain); d != "" {
		return fmt.Errorf("%w: %s", ErrDialog, d)
	}
	if !Running(plain) {
		return ErrIdle
	}
	if typed, ok := Draft(styled, tool); !ok || typed != "" {
		return errors.New("the input line does not show the message gone to the queue")
	}
	if !Queued(plain) {
		return errors.New("no message waits in the queue")
	}
	return nil
}

// until reads the screen, a pause apart, until check passes on it, at most settleReads times: a terminal can draw a
// paste or a key a moment late. A screen whose terminal check fails ends it at once, and so does any failure but a
// screen caught mid-redraw, without its input line yet, or one that has not changed yet. Every failure is
// ErrUnstable: something was typed before it.
func (s *Sender) until(screen func() (string, error), check func(string) error) error {
	var err error
	for range settleReads {
		s.pause()
		var sc string
		if sc, err = screen(); err != nil {
			return fmt.Errorf("%w: %v", ErrUnstable, err)
		}
		if err = check(sc); err == nil {
			return nil
		}
		if errors.Is(err, ErrDialog) || errors.Is(err, ErrIdle) || errors.Is(err, ErrDraft) {
			return fmt.Errorf("%w: %v", ErrUnstable, err)
		}
	}
	return fmt.Errorf("%w: %v", ErrUnstable, err)
}

// pasteTmux pastes text into t's pane as one bracketed paste, through a buffer of its own that it drops. A buffer
// it could not load typed nothing; a paste that failed is ErrUnstable.
func (s *Sender) pasteTmux(ctx context.Context, t Target, text string) error {
	buf := "momentum-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := s.tmux(ctx, text, "load-buffer", "-b", buf, "-"); err != nil {
		s.dropBuffer(buf)
		return err
	}
	// -p wraps the paste in bracketed-paste codes when the program asked for them, so it lands as one paste and
	// does not submit itself; -d drops the buffer.
	if _, err := s.tmux(ctx, "", "paste-buffer", "-p", "-d", "-b", buf, "-t", t.Pane); err != nil {
		s.dropBuffer(buf)
		return fmt.Errorf("%w: paste: %v", ErrUnstable, err)
	}
	return nil
}
