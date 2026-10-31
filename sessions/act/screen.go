package act

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-sh/momentum/sessions/term"
)

// A dialog on a screen owns the keyboard: typed text would answer it, and Enter would pick the option it
// highlights. These are the dialogs and menus of the harnesses' terminal interfaces.
var (
	reMenuRow = regexp.MustCompile(`(?m)^[\s│]*[❯›>▶]\s*\d+[.)]\s+\S`) // a numbered option under a selection cursor
	reFooter  = regexp.MustCompile(`(?mi)(\b(esc to (cancel|go back|exit)|enter to (select|confirm|submit|continue)|tab to amend)\b|↑/?↓ to (navigate|select|move))`)
	reYesNo   = regexp.MustCompile(`(?m)(\(y/n\)|\[y/n\]|\[Y/n\]|\[y/N\])\s*$`)
	reAsk     = regexp.MustCompile(`(?m)^\s*(Resume from summary|Would you like to (run|make|apply|allow)|Allow (command|this)|Approve (this|command))\b`)
)

// dialogLines is how much of the bottom of a screen a dialog is looked for in: a harness draws its dialogs where
// its input line goes, and what scrolled above is transcript.
const dialogLines = 20

// Dialog returns the line that shows a dialog or a menu is on the screen, or "": a permission prompt, a
// numbered choice under a selection cursor, a menu's key footer, a yes/no prompt, a resume or approval question.
// It reads the bottom of the plain screen, without styles.
func Dialog(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > dialogLines {
		lines = lines[len(lines)-dialogLines:]
	}
	bottom := strings.Join(lines, "\n")
	if q := term.PermissionPrompt(bottom); q != "" {
		return q
	}
	for _, re := range []*regexp.Regexp{reMenuRow, reFooter, reYesNo, reAsk} {
		if m := re.FindString(bottom); m != "" {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

// reRunning is what Claude Code and Codex draw while a turn runs: the key that interrupts it.
var reRunning = regexp.MustCompile(`(?i)\besc to interrupt\b`)

// Running reports a screen whose bottom shows a turn running: a message typed now waits in a queue for it.
func Running(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > dialogLines {
		lines = lines[len(lines)-dialogLines:]
	}
	return reRunning.MatchString(strings.Join(lines, "\n"))
}

// The input lines this package knows, by tool: Claude Code's prompt glyph under its top rule, Codex's prompt
// glyph, and Claude Code's older boxed prompt.
const (
	claudeGlyph = "❯"
	codexGlyph  = "›"
	boxedGlyph  = "│ >"
)

// Draft finds the input line of a tool's interface on a styled screen (tmux capture-pane -e, kitty get-text
// --ansi) and returns the text in it that someone typed and has not sent: everything after the prompt glyph, on
// its line and the lines it wraps to, that is not drawn dim. A harness draws its placeholder and its suggestions
// dim. ok is false when the screen has no input line this package knows for the tool, so nobody can tell whether
// it holds a draft.
func Draft(styled, tool string) (typed string, ok bool) {
	lines := strings.Split(styled, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimLeftFunc(Plain(lines[i]), unicode.IsSpace)
		var glyph string
		switch {
		case tool == "claude" && strings.HasPrefix(trimmed, claudeGlyph) && i > 0 && rule(Plain(lines[i-1])):
			glyph = claudeGlyph
		case tool == "claude" && (strings.HasPrefix(trimmed, boxedGlyph+" ") || trimmed == boxedGlyph):
			glyph = boxedGlyph
		case tool == "codex" && strings.HasPrefix(trimmed, codexGlyph+" "):
			glyph = codexGlyph
		default:
			continue
		}
		parts := []string{undimmed(lines[i], glyph, true)}
		closed := false
		for _, more := range lines[i+1:] { // the lines it wraps to, up to the frame's end
			p := strings.TrimSpace(Plain(more))
			if p == "" || rule(p) || strings.HasPrefix(p, "╰") {
				closed = rule(p)
				break
			}
			parts = append(parts, undimmed(more, glyph, false))
		}
		if glyph == claudeGlyph && !closed {
			continue // Claude Code's input line has a rule above it and below it
		}
		typed = strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
		if glyph == boxedGlyph { // the box's sides
			typed = strings.Join(strings.Fields(strings.ReplaceAll(typed, "│", "")), " ")
		}
		return typed, true
	}
	return "", false
}

// rule reports a line drawn as a horizontal rule: Claude Code frames its input line with them, and may write the
// session's name into the top one.
func rule(s string) bool {
	rs := []rune(strings.TrimSpace(s))
	n := 0
	for _, r := range rs {
		if r == '─' || r == '━' {
			n++
		}
	}
	return n >= 10 && n*10 >= len(rs)*6 && (rs[0] == '─' || rs[0] == '━')
}

// undimmed returns the characters of a styled line that are not drawn dim: after glyph on the input line's first
// line, all of them on a line it wraps to. The cursor is a character drawn reversed: on a dim placeholder it sits
// on the placeholder's first letter, so a reversed character followed by dim text is the placeholder's.
func undimmed(line, glyph string, first bool) string {
	var out strings.Builder
	dim, rev, past := false, false, !first
	seen, held := "", ""
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			n, d := escape(line[i:], dim)
			rev = sgrReverse(line[i:i+n], rev)
			dim = d
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		if !past {
			seen += string(r)
			if strings.HasSuffix(seen, glyph) {
				past = true
			}
			continue
		}
		if held != "" {
			if !dim {
				out.WriteString(held) // the cursor on a typed character
			}
			held = ""
		}
		switch {
		case unicode.IsSpace(r):
			out.WriteRune(' ')
		case !dim && rev && unicode.IsPrint(r):
			held = string(r) // typed, unless dim text follows it
		case !dim && unicode.IsPrint(r):
			out.WriteRune(r)
		}
	}
	return out.String() + held
}

// sgrReverse applies an escape sequence to the reversed state, when it is an SGR: 7 sets it, 27 and 0 (or an
// empty list) clear it. Extended colors carry numbers that are not attributes, as sgrDim reads them.
func sgrReverse(seq string, rev bool) bool {
	if !strings.HasPrefix(seq, "\x1b[") || !strings.HasSuffix(seq, "m") {
		return rev
	}
	params := seq[2 : len(seq)-1]
	if params == "" {
		return false
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		code, _, sub := strings.Cut(ps[i], ":")
		switch strings.TrimLeft(code, "0") {
		case "":
			if !sub {
				rev = false
			}
		case "7":
			rev = true
		case "27":
			rev = false
		case "38", "48", "58":
			if sub {
				continue
			}
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		}
	}
	return rev
}

// escape reads one escape sequence at the start of s and returns its length and the dim state after it. Only
// SGR (ESC [ ... m) changes the state.
func escape(s string, dim bool) (int, bool) {
	if len(s) < 2 {
		return len(s), dim
	}
	switch s[1] {
	case '[':
		j := 2
		for j < len(s) && (s[j] >= 0x30 && s[j] <= 0x3f || s[j] >= 0x20 && s[j] <= 0x2f) {
			j++
		}
		if j >= len(s) {
			return len(s), dim
		}
		if s[j] == 'm' {
			dim = sgrDim(s[2:j], dim)
		}
		return j + 1, dim
	case ']': // OSC, ended by BEL or ESC \
		for j := 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1, dim
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2, dim
			}
		}
		return len(s), dim
	}
	return 2, dim
}

// sgrDim applies one SGR parameter list to the dim state: 2 sets it, 22 and 0 (or an empty list) clear it. A
// parameter's colon subparameters belong to it (4:2 is a double underline, 38:2:r:g:b a color), and the
// semicolon form of an extended color (38;5;n, 38;2;r;g;b, and 48 and 58 the same) carries numbers that are not
// attributes.
func sgrDim(params string, dim bool) bool {
	if params == "" {
		return false
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		code, _, sub := strings.Cut(ps[i], ":")
		switch strings.TrimLeft(code, "0") {
		case "":
			if !sub {
				dim = false
			}
		case "2":
			if !sub {
				dim = true
			}
		case "22":
			dim = false
		case "38", "48", "58":
			if sub {
				continue
			}
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		}
	}
	return dim
}

// Plain strips escape sequences from a styled screen.
func Plain(styled string) string {
	var out strings.Builder
	for i := 0; i < len(styled); {
		if styled[i] == 0x1b {
			n, _ := escape(styled[i:], false)
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(styled[i:])
		out.WriteRune(r)
		i += size
	}
	return out.String()
}

// SafeText is text a program can type: one line, with every control character dropped (a stray ESC or ^C would
// drive the interface) and whitespace folded. A newline would submit what came before it.
func SafeText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// command reports a message an interface would take as a command, not as text for the model: a slash command, a
// shell escape, a memory note, a help key.
func command(text string) bool {
	return text != "" && strings.ContainsRune("/!#&?", rune(text[0]))
}

// squeeze is s without its whitespace, so a message and the input line it wrapped across compare equal.
func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
