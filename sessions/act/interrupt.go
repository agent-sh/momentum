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
	// reSpinner is Claude Code's running row: a spinner glyph at the left edge, what it does, and an ellipsis, then
	// how long it has run when the row has room ("✻ Fermenting… (31m 4s)", "✻ Running the test suite…"). The row a
	// finished turn leaves says what it did and for how long, with no ellipsis.
	reSpinner = regexp.MustCompile(`^[·✢✳✶✻✽*] +\S.*…(\s*\(.*\))?\s*$`)
	// reTimed is a running row with how long it has run, which only a running row shows: the conversation draws a
	// row like the rest of it for a thinking block (verbose, "✻ Thinking…").
	reTimed = regexp.MustCompile(`…\s*\(.*\b\d+[hms]\b`)
	// reGlyph is the spinner glyph Claude Code starts its running row with, and its retry notices too.
	reGlyph = regexp.MustCompile(`^[·✢✳✶✻✽*] `)
	// reStill is the running row as Claude Code draws it with reduced motion: a still dot, the glyph its
	// conversation's messages start with too, so only with how long it has run.
	reStill = regexp.MustCompile(`^● +\S.*…\s*\(.*\b\d+[hms]\b`)
	// reRetrying is what Claude Code writes in that row's place while it waits for, or retries, a request.
	reRetrying = regexp.MustCompile(`Retrying in \d+[hms]|Waiting for API response · will retry in|No response from the API after \d|retrying(?: once)?, waiting up to \d|next try in \d+[hms]`)
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
// its input box (statusRows): its running row, or the retry notice in its place, and the key that interrupts the
// turn on the status line below the box. The conversation above says nothing about the turn. Claude Code drops
// the key while text sits on the input line, or when the status line has no room, and the running row's time
// when it has none.
func TurnRuns(styled, tool string) bool {
	row, under, below, ok := statusRows(styled, tool)
	return ok && turnRuns(row, under, below, false)
}

// turnRuns reads statusRows for a running turn. sure asks for what only a running turn shows: the key on the
// status line, the running row with its time, or a retry notice in its place; without it, a running row with no
// time counts too, and one drawn with reduced motion.
func turnRuns(row string, under, below []string, sure bool) bool {
	if reRunning.MatchString(strings.Join(below, "\n")) {
		return true
	}
	if !sure && reStill.MatchString(row) {
		return true
	}
	if !reGlyph.MatchString(row) {
		return false
	}
	if reRetrying.MatchString(strings.Join(strings.Fields(row+" "+strings.Join(under, " ")), " ")) {
		return true // a notice may wrap onto the rows under its first
	}
	return reSpinner.MatchString(row) && (!sure || reTimed.MatchString(row))
}

// turnRunsSure is TurnRuns by what only a running turn shows (turnRuns): before anything is typed, the input line
// is empty and Claude Code shows the key, and the time once the turn has run a while.
func turnRunsSure(styled, tool string) bool {
	row, under, below, ok := statusRows(styled, tool)
	return ok && turnRuns(row, under, below, true)
}

// statusRows are the plain rows Claude Code draws around its input box. row is the one its running row would be:
// the first row above the box's top rule, past blank rows, that starts at the left edge, past the indented rows
// right under it (Claude Code draws a tip or its task list there, the first under ⎿); empty when a blank row parts
// those from the row above them, since nothing parts them from the running row. under are those indented rows, top
// first, and below every row under the box's bottom rule. false when the screen has no input box of the tool's:
// Claude Code's between its two rules.
func statusRows(styled, tool string) (row string, under, below []string, ok bool) {
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
		j := i - 2
		for ; j >= 0 && strings.TrimSpace(Plain(lines[j])) == ""; j-- {
		}
		for ; j >= 0; j-- {
			p := Plain(lines[j])
			if strings.TrimSpace(p) == "" {
				break // a blank row: the indented rows are not the running row's
			}
			if r := []rune(p); !unicode.IsSpace(r[0]) {
				row = p
				break
			}
			under = append([]string{p}, under...)
		}
		for _, l := range lines[end+1:] {
			below = append(below, Plain(l))
		}
		return row, under, below, true
	}
	return "", nil, nil, false
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
// nothing waiting in the queue, and the turn running, by what only a running turn shows (turnRunsSure).
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
	if !turnRunsSure(styled, tool) {
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
