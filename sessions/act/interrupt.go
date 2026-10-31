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
	"unicode"
)

var (
	// reQueuedHint is what Claude Code writes on its input line while a message waits in its queue, in any of its
	// wordings ("Press up to edit queued messages", "Press up to select a queued message ...").
	reQueuedHint = regexp.MustCompile(`(?i)\bqueued messages?\b`)
	// reSpinner is Claude Code's running row: a spinner glyph, what it does, an ellipsis, and how long it has run
	// ("✻ Fermenting… (31m 4s)", "✻ Running the test suite… (2m · ↓ 3k tokens)"). The row a finished turn leaves
	// says what it did and for how long, with no ellipsis.
	reSpinner = regexp.MustCompile(`^\s*[·✢✳✶✻✽*]\s+.{1,120}…\s*\(\d+[hms]`)
	// reRetrying is what Claude Code writes in that row's place while it waits for, or retries, a request.
	reRetrying = regexp.MustCompile(`Retrying in \d+s · attempt \d+/\d+|Waiting for API response · will retry in`)
)

// Queued reports a styled screen whose input line says, in what it draws dim, that a message waits in the
// harness's queue.
func Queued(styled, tool string) bool {
	line, ok := inputText(styled, tool)
	if typed, _ := Draft(styled, tool); typed != "" {
		line = "" // what is typed is not the harness's hint; the caller reads the draft first
	}
	return ok && reQueuedHint.MatchString(line)
}

// TurnRuns reports a styled screen of a Claude Code turn running, read only from Claude Code's own rows around
// its input box: its running row or the retry notice in its place, just above the box, and the key that
// interrupts the turn on the status line below it (dropped while text sits on the input line). The conversation
// above says nothing about the turn.
func TurnRuns(styled, tool string) bool {
	above, below, ok := statusRows(styled, tool)
	if !ok {
		return false
	}
	for _, row := range above {
		if reSpinner.MatchString(row) || reRetrying.MatchString(row) {
			return true
		}
	}
	return reRunning.MatchString(strings.Join(below, "\n"))
}

// statusRows are the plain rows Claude Code draws around its input box: up to three rows with text above the box's
// top rule, the nearest first, and every row below its bottom rule. false when the screen has no input box of
// the tool's.
func statusRows(styled, tool string) (above, below []string, ok bool) {
	lines := strings.Split(styled, "\n")
	for i := len(lines) - 1; i > 0; i-- {
		trimmed := strings.TrimLeftFunc(Plain(lines[i]), unicode.IsSpace)
		if tool != "claude" || !strings.HasPrefix(trimmed, claudeGlyph) || !rule(Plain(lines[i-1])) {
			continue
		}
		end := i + 1
		for ; end < len(lines); end++ {
			if p := strings.TrimSpace(Plain(lines[end])); p == "" || rule(p) {
				break
			}
		}
		if end >= len(lines) || !rule(strings.TrimSpace(Plain(lines[end]))) {
			continue // Claude Code's input line has a rule above it and below it
		}
		for j := i - 2; j >= 0 && len(above) < 3; j-- {
			if p := Plain(lines[j]); strings.TrimSpace(p) != "" {
				above = append(above, p)
			}
		}
		for _, l := range lines[end+1:] {
			below = append(below, Plain(l))
		}
		return above, below, true
	}
	return nil, nil, false
}

// CheckInterrupt reports whether Interrupt could type into t now, and by which path: the checks it makes before it
// types anything. ErrIdle: the screen shows no turn running.
func (s *Sender) CheckInterrupt(ctx context.Context, t Target) (Path, error) {
	path, term, err := s.keys(ctx, t)
	if err != nil {
		return path, err
	}
	screen, err := term.screen()
	if err != nil {
		return path, err
	}
	return path, running(screen, t.Tool)
}

// Interrupt resumes a Claude Code turn whose model request never got its reply, the way its owner breaks one: it
// types text into the input line while the turn runs, so the harness holds it in its queue, and presses Escape,
// which ends the turn and sends what the queue holds as the next prompt. Pressing Escape alone, with nothing
// queued, takes the turn back without a record and puts its prompt back on the input line. text is typed as
// SafeText makes it.
//
// Before the paste it checks what CheckInterrupt checks (the message, the terminal, no dialog, an input line with
// nothing typed on it, no message waiting in the queue, the turn running), and still: an error from it, as from
// the checks, means nothing was typed. After the paste it presses Enter only when the input line holds the
// message alone, with no dialog, the turn still running, and still passing. It presses Escape only once the input line is empty again and says that a
// message waits in the queue, with the turn running, no dialog, and still passing. A check that fails from the
// paste on, or a command that fails, is ErrUnstable: the message may sit on the input line, or in the queue. That
// the turn ended and the message went in is the transcript's to tell: Interrupt does not wait for it.
func (s *Sender) Interrupt(ctx context.Context, t Target, text string, still func() error) (Path, error) {
	if err := CheckText(text); err != nil {
		return "", err
	}
	text = SafeText(text)
	path, term, err := s.keys(ctx, t)
	if err != nil {
		return path, err
	}
	if still == nil {
		still = func() error { return nil }
	}
	return path, s.interrupt(term, t.Tool, text, still)
}

// keys is a terminal as Interrupt drives it: its checked screen, a paste, and a key.
type keys struct {
	screen func() (string, error)
	paste  func(text string) error
	press  func(key string) error
}

// keys is the terminal of a Claude Code session that a key can reach: its tmux pane or its kitty window.
func (s *Sender) keys(ctx context.Context, t Target) (Path, keys, error) {
	if t.Tool != "claude" {
		return "", keys{}, ErrNoPath // the queue and the key are Claude Code's
	}
	path, err := s.Choose(t)
	if err != nil {
		return "", keys{}, err
	}
	switch path {
	case Tmux:
		return path, keys{
			screen: func() (string, error) { return s.tmuxScreen(ctx, t) },
			paste:  func(text string) error { return s.pasteTmux(ctx, t, text) },
			press: func(key string) error {
				_, err := s.tmux(ctx, "", "send-keys", "-t", t.Pane, map[string]string{"enter": "Enter", "escape": "Escape"}[key])
				return err
			},
		}, nil
	case Kitty:
		return path, keys{
			screen: func() (string, error) { return s.kittyScreen(ctx, t) },
			paste: func(text string) error {
				if _, err := s.kitty(ctx, t, text, "send-text", "--stdin", "--bracketed-paste=auto"); err != nil {
					return fmt.Errorf("%w: send-text: %v", ErrUnstable, err)
				}
				return nil
			},
			press: func(key string) error { _, err := s.kitty(ctx, t, "", "send-key", key); return err },
		}, nil
	}
	return path, keys{}, ErrNoPath
}

func (s *Sender) interrupt(term keys, tool, text string, still func() error) error {
	screen, err := term.screen()
	if err != nil {
		return err
	}
	if err := running(screen, tool); err != nil {
		return err
	}
	if err := still(); err != nil {
		return err
	}
	if err := term.paste(text); err != nil {
		return err
	}
	if err := s.until(term.screen, func(screen string) error { return holding(screen, tool, text) }); err != nil {
		return err
	}
	if err := still(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnstable, err)
	}
	if err := term.press("enter"); err != nil {
		return fmt.Errorf("%w: enter: %v", ErrUnstable, err)
	}
	if err := s.until(term.screen, func(screen string) error { return waiting(screen, tool) }); err != nil {
		return err
	}
	if err := still(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnstable, err)
	}
	if err := term.press("escape"); err != nil {
		return fmt.Errorf("%w: escape: %v", ErrUnstable, err)
	}
	return nil
}

// running checks a styled screen before anything is typed: no dialog, an input line of the tool's with nothing
// typed on it (read first: text on the line hides the interrupt key, and a draft is its owner at the pane),
// nothing waiting in the queue, and the turn running.
func running(styled, tool string) error {
	if d := Dialog(Plain(styled)); d != "" {
		return fmt.Errorf("%w: %s", ErrDialog, d)
	}
	switch typed, ok := Draft(styled, tool); {
	case !ok:
		return ErrNoInput
	case typed != "":
		return ErrDraft
	}
	if Queued(styled, tool) {
		return ErrQueued
	}
	if !TurnRuns(styled, tool) {
		return ErrIdle
	}
	return nil
}

// holding checks a styled screen after the paste: no dialog, the turn still running by its running row, and an
// input line that holds the message and nothing else. A turn that ended meanwhile would take the message as its
// prompt at Enter, and Escape would end the turn it opened.
func holding(styled, tool, text string) error {
	if d := Dialog(Plain(styled)); d != "" {
		return fmt.Errorf("%w: %s", ErrDialog, d)
	}
	if !holds(styled, tool, text) {
		return errors.New("the input line does not hold the message alone")
	}
	if !TurnRuns(styled, tool) {
		return ErrIdle
	}
	return nil
}

// waiting checks a styled screen after Enter put the message in the queue: the turn still runs, no dialog, the
// input line is empty again and says that a message waits in the queue.
func waiting(styled, tool string) error {
	if d := Dialog(Plain(styled)); d != "" {
		return fmt.Errorf("%w: %s", ErrDialog, d)
	}
	if !TurnRuns(styled, tool) {
		return ErrIdle
	}
	if typed, ok := Draft(styled, tool); !ok || typed != "" {
		return errors.New("the input line does not show the message gone to the queue")
	}
	if !Queued(styled, tool) {
		return errors.New("no message waits in the queue")
	}
	return nil
}

// inputText is all the input line of a tool's interface shows after its prompt glyph, dim text with it, on its
// line and the lines it wraps to, as Draft finds the line; false when the screen shows none.
func inputText(styled, tool string) (string, bool) {
	if _, ok := Draft(styled, tool); !ok {
		return "", false
	}
	lines := strings.Split(styled, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimLeftFunc(Plain(lines[i]), unicode.IsSpace)
		glyphs := []string{claudeGlyph, boxedGlyph}
		if tool == "codex" {
			glyphs = []string{codexGlyph}
		}
		for _, glyph := range glyphs {
			if rest, ok := strings.CutPrefix(trimmed, glyph); ok && (glyph != claudeGlyph || i > 0 && rule(Plain(lines[i-1]))) {
				parts := []string{rest}
				for _, more := range lines[i+1:] {
					p := strings.TrimSpace(Plain(more))
					if p == "" || rule(p) || strings.HasPrefix(p, "╰") {
						break
					}
					parts = append(parts, p)
				}
				return strings.Join(strings.Fields(strings.Join(parts, " ")), " "), true
			}
		}
	}
	return "", false
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
		if errors.Is(err, ErrDialog) || errors.Is(err, ErrIdle) {
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
