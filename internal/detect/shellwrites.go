package detect

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"time"
)

// readSettle is how long after a read that found no file its session's write of the file clears it, and so how long
// the read waits before it counts: its write may not be in the store yet. A session writes the file it looked for
// minutes later, well inside it.
const readSettle = time.Hour

// writtenPaths are the files a call that did not fail wrote, as it names them: a file tool's path, or the files a
// shell's command line writes (shellWrites), a relative one read in the directory the call ran in (shellDir).
func writtenPaths(harness, tool, call, cwd string) []string {
	if isShell(tool) {
		return shellWrites(call, shellDir(harness, call, cwd))
	}
	if p, _, _ := callField(call, pathKeys); p != "" {
		return []string{p}
	}
	return nil
}

// workdirKeys are the fields of a shell call's input that name the directory it runs its command in, as capacity
// reads them.
var workdirKeys = map[string]bool{"workdir": true, "cwd": true, "working_directory": true, "workingDirectory": true, "dir": true, "directory": true}

// workdirShells are the harnesses whose shell call can name its directory (Codex's and OpenCode's workdir): a call
// of theirs the store cut before such a field may have named one.
var workdirShells = map[string]bool{"codex": true, "opencode": true}

// shellDir is the directory a shell call runs its command in, as far as the store says, or "" when it does not say:
// the workdir the call names, when that is a whole path; else the directory its session was in when it made the
// call. Hermes's terminal keeps a directory of its own from call to call, which its record does not hold, so only
// a workdir or a cd on the line says where it is.
func shellDir(harness, call, cwd string) string {
	w, named, absent := callField(call, workdirKeys)
	switch {
	case named:
		if path.IsAbs(w) {
			return w
		}
		return ""
	case harness == "hermes", !absent && workdirShells[harness], !path.IsAbs(cwd):
		return ""
	}
	return cwd
}

// shellWrites are the files a shell call's command line writes, as the shell reads its words (quotes off, escapes
// read): the targets of its standard output's redirections (>, >>, >|, 1>, &>, &>>, >& FILE) and tee's files. The
// line is the command's first, continued lines joined, up to a comment. Bash reports the last line's status, so
// the line counts only when nothing visible runs after it (nothingAfter). Of the line, only a command whose
// success the line's own says is read: the line's last, or one joined to it by && only, with no || before it, and
// not negated. A relative file is read in dir, or in the directory a cd to a whole path just before it entered (cd
// DIR && ...); after another cd, a pushd or a popd it is not known, and nor is a file the shell expands (a
// variable, a command's output, a glob, a leading ~).
func shellWrites(call, dir string) []string {
	loc := callRe.FindStringSubmatchIndex(call)
	if loc == nil {
		return nil
	}
	closed := loc[3] < len(call) && call[loc[3]] == '"' // the command's text is whole, whatever the cut took after it
	line, ok := shellLine(unescapeCut(call[loc[2]:loc[3]]))
	if !ok || !nothingAfter(line, closed) {
		return nil
	}
	cmds := shellCommands(line.toks, dir)
	var out []string
	for i, c := range cmds {
		if !reported(cmds[i:]) || slices.ContainsFunc(cmds[:i], func(d shellCmd) bool { return d.after == "||" }) {
			continue
		}
		prog, args := program(c.words)
		if prog == "!" {
			continue
		}
		var files []shellTok
		for _, r := range c.redirs {
			if r.writes() {
				files = append(files, r.target)
			}
		}
		if prog == "tee" {
			for _, a := range args {
				if !strings.HasPrefix(a.word, "-") {
					files = append(files, a)
				}
			}
		}
		for _, f := range files {
			switch {
			case !f.exact || f.word == "":
			case path.IsAbs(f.word):
				out = append(out, f.word)
			case c.dir != "":
				out = append(out, path.Join(c.dir, f.word))
			}
		}
	}
	return out
}

// nothingAfter says nothing visible runs after a command's first line: the command is that line alone, or the line
// opens here-documents and only their text follows. A text the store cut inside a here-document keeps the benefit
// of the doubt; one it cut anywhere else may hide a next command.
func nothingAfter(line shellFirst, closed bool) bool {
	if !line.ended {
		return closed
	}
	lines := strings.Split(line.rest, "\n")
	i := 0
	for k, t := range line.toks {
		if t.op != "<<" && t.op != "<<-" || k+1 >= len(line.toks) || line.toks[k+1].op != "" {
			continue
		}
		for ; i < len(lines); i++ {
			if l := lines[i]; l == line.toks[k+1].word || t.op == "<<-" && strings.TrimLeft(l, "\t") == line.toks[k+1].word {
				break
			}
		}
		if i == len(lines) { // the text ends inside the here-document
			return true
		}
		i++
	}
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return false
		}
	}
	return closed
}

// shellTok is a word of a command line, its quotes off, or one of its operators.
type shellTok struct {
	op    string // a separator, a parenthesis or a redirection; "" for a word
	fd    string // the descriptor a redirection names before it (2 in 2>), "" for none
	word  string
	exact bool // a word the shell does not expand
}

// shellRedir is a redirection of a command: its operator, its descriptor and its target.
type shellRedir struct {
	op, fd string
	target shellTok
}

// writes says a redirection sends the command's standard output to its target, a file.
func (r shellRedir) writes() bool {
	switch r.op {
	case ">", ">>", ">|":
		return r.fd == "" || r.fd == "1"
	case "&>", "&>>":
		return r.fd == ""
	case ">&": // >&N and >&- copy or close a descriptor; >& FILE writes both outputs there
		return (r.fd == "" || r.fd == "1") && r.target.word != "-" && strings.Trim(r.target.word, "0123456789") != ""
	}
	return false
}

// shellCmd is a simple command of a line: its words and redirections, the separator after it ("" at the line's
// end) and the directory it runs in ("" when not known).
type shellCmd struct {
	words  []shellTok
	redirs []shellRedir
	after  string
	dir    string
}

// reported says the line's success says the first of rest, a command and the ones after it, succeeded: every
// separator from it to the line's end is &&, and the last command ends the line (or a ; does).
func reported(rest []shellCmd) bool {
	for i, d := range rest {
		if i == len(rest)-1 {
			return d.after == "" || d.after == ";"
		}
		if d.after != "&&" {
			return false
		}
	}
	return false
}

// shellCommands groups a line's tokens into its simple commands, each with the directory it runs in: dir, then the
// one a cd to a whole path enters for the commands joined to it by && (and a pipe on from them); another cd, a
// pushd or a popd leaves it unknown. A subshell's (parentheses) directory ends with its last command; the
// parentheses are no separator.
func shellCommands(toks []shellTok, dir string) []shellCmd {
	type state struct {
		dir     string
		chained bool // dir is a cd's, which the commands since it held by &&
	}
	at := state{dir: dir}
	var stack []state
	pops := 0 // the subshells that close on the command in progress
	pop := func() {
		if len(stack) > 0 {
			at, stack = stack[len(stack)-1], stack[:len(stack)-1]
		}
	}
	var out []shellCmd
	var cur shellCmd
	begun := false
	begin := func() {
		if !begun {
			cur.dir, begun = at.dir, true
		}
	}
	finish := func(after string) {
		if !begun {
			return
		}
		cur.after = after
		out = append(out, cur)
		prog, args := program(cur.words)
		switch prog {
		case "cd":
			if len(args) == 1 && args[0].exact && path.IsAbs(args[0].word) && after == "&&" {
				at = state{dir: args[0].word, chained: true}
			} else {
				at = state{}
			}
		case "pushd", "popd":
			at = state{}
		default:
			if at.chained && after != "&&" && after != "|" && after != "|&" {
				at = state{}
			}
		}
		for ; pops > 0; pops-- {
			pop()
		}
		cur, begun = shellCmd{}, false
	}
	for k := 0; k < len(toks); k++ {
		switch t := toks[k]; {
		case t.op == "":
			begin()
			cur.words = append(cur.words, t)
		case t.op == "(":
			stack = append(stack, at)
		case t.op == ")":
			if begun {
				pops++
			} else {
				pop()
			}
		case strings.ContainsAny(t.op, "<>"):
			begin()
			r := shellRedir{op: t.op, fd: t.fd}
			if k+1 < len(toks) && toks[k+1].op == "" {
				r.target = toks[k+1]
				k++
			}
			cur.redirs = append(cur.redirs, r)
		default:
			finish(t.op)
		}
	}
	finish("")
	return out
}

// program is a command's program and its arguments: its first word past assignments and the wrappers that run
// another program (sudo, env, timeout and the like, with their options). A negated command's program is !.
func program(words []shellTok) (string, []shellTok) {
	for len(words) > 0 {
		w := words[0].word
		switch {
		case w == "!":
			return "!", nil
		case assignRe.MatchString(w):
			words = words[1:]
			continue
		case wrappers[w]:
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0].word, "-") || countRe.MatchString(words[0].word) || assignRe.MatchString(words[0].word)) {
				words = words[1:]
			}
			continue
		}
		break
	}
	if len(words) == 0 {
		return "", nil
	}
	return path.Base(words[0].word), words[1:]
}

// shellOps are the shell's operators, the longest first.
var shellOps = []string{"&>>", "<<<", "<<-", "&&", "||", "|&", "&>", ">>", ">|", ">&", "<<", "<>", "<&", ";;", ";", "&", "|", "(", ")", ">", "<"}

// openers are the words after which the next word still opens a command: a test or an arithmetic may follow them.
var openers = map[string]bool{"time": true, "!": true, "if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true}

// shellFirst is a command's first line as the shell splits it: its words and operators, and the text after it.
type shellFirst struct {
	toks  []shellTok
	rest  string // the text after the line
	ended bool   // the line ended in the text, so rest follows it
}

// shellLine splits a command's first line into words and operators as the shell does: a backslash before the
// line's end joins the next, and a comment runs to the line's end. A test's or an arithmetic's text ([[ ]], (( )))
// and a process substitution's (<( ), >( )) are one word each, whose > compares or runs elsewhere. It is false for
// a text it cannot read.
func shellLine(cmd string) (shellFirst, bool) {
	var toks []shellTok
	start := true // the next word opens a command
	for i := 0; ; {
		for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t' || strings.HasPrefix(cmd[i:], "\\\n")) {
			if cmd[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(cmd) {
			return shellFirst{toks: toks}, true
		}
		c := cmd[i]
		switch {
		case c == '#':
			k := strings.IndexByte(cmd[i:], '\n')
			if k < 0 {
				return shellFirst{toks: toks}, true
			}
			i += k
			fallthrough
		case c == '\n':
			return shellFirst{toks: toks, rest: cmd[i+1:], ended: true}, true
		case start && strings.HasPrefix(cmd[i:], "(("):
			toks = append(toks, shellTok{word: "(("})
			if i = closing(cmd, i+2, 2); i < 0 {
				return shellFirst{toks: toks}, true
			}
			start = false
			continue
		case (c == '<' || c == '>') && strings.HasPrefix(cmd[i+1:], "("):
			toks = append(toks, shellTok{word: cmd[i : i+1]})
			if i = closing(cmd, i+2, 1); i < 0 {
				return shellFirst{toks: toks}, true
			}
			start = false
			continue
		case strings.IndexByte(";&|()<>", c) >= 0:
			op := opAt(cmd[i:])
			toks = append(toks, shellTok{op: op})
			i += len(op)
			if !strings.ContainsAny(op, "<>") {
				start = true
			}
			continue
		}
		w, j := shellWordAt(cmd, i)
		if j == i { // a byte no rule above reads
			return shellFirst{}, false
		}
		if j < len(cmd) && (cmd[j] == '<' || cmd[j] == '>') && w.exact && w.word != "" && strings.Trim(w.word, "0123456789") == "" &&
			!strings.ContainsAny(cmd[i:j], `'"\`) {
			op := opAt(cmd[j:]) // the word is a descriptor's number, which the redirection after it takes
			toks, i = append(toks, shellTok{op: op, fd: w.word}), j+len(op)
			continue
		}
		if start && cmd[i:j] == "[[" {
			k := strings.Index(cmd[j:], "]]")
			if k < 0 {
				return shellFirst{toks: append(toks, shellTok{word: "[["})}, true
			}
			toks, i, start = append(toks, shellTok{word: "[["}), j+k+2, false
			continue
		}
		toks, i, start = append(toks, w), j, w.exact && openers[w.word]
	}
}

// opAt is the operator s opens with.
func opAt(s string) string {
	for _, o := range shellOps {
		if strings.HasPrefix(s, o) {
			return o
		}
	}
	return s[:1]
}

// shellWordAt reads the word at i: its quotes off and its escapes read, and whether the shell takes it as it is (no
// variable, command, glob or leading ~). It returns where the word ends.
func shellWordAt(s string, i int) (shellTok, int) {
	var b strings.Builder
	exact := s[i] != '~'
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || strings.IndexByte(";&|()<>", c) >= 0:
			return shellTok{word: b.String(), exact: exact}, i
		case c == '\\':
			if i+1 < len(s) && s[i+1] != '\n' {
				b.WriteByte(s[i+1])
			}
			i = min(i+2, len(s)) // a backslash can end the text
		case c == '\'':
			k := strings.IndexByte(s[i+1:], '\'')
			if k < 0 {
				return shellTok{word: b.String() + s[i+1:]}, len(s)
			}
			b.WriteString(s[i+1 : i+1+k])
			i += k + 2
		case c == '"':
			for i++; ; i++ {
				if i >= len(s) {
					return shellTok{word: b.String()}, len(s)
				}
				if s[i] == '"' {
					i++
					break
				}
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
					if s[i+1] != '\n' {
						b.WriteByte(s[i+1])
					}
					i++
					continue
				}
				if s[i] == '$' || s[i] == '`' {
					exact = false
				}
				b.WriteByte(s[i])
			}
		case c == '$' && strings.HasPrefix(s[i+1:], "("):
			j := closing(s, i+2, 1)
			if j < 0 {
				return shellTok{word: b.String()}, len(s)
			}
			exact = false
			b.WriteString(s[i:j])
			i = j
		case c == '`':
			k := strings.IndexByte(s[i+1:], '`')
			if k < 0 {
				return shellTok{word: b.String()}, len(s)
			}
			exact = false
			b.WriteString(s[i : i+k+2])
			i += k + 2
		default:
			if strings.IndexByte("$*?[{", c) >= 0 {
				exact = false
			}
			b.WriteByte(c)
			i++
		}
	}
	return shellTok{word: b.String(), exact: exact}, i
}

// closing is where parentheses open depth deep before i close: the index after the last one, or -1 when the text
// ends first. Quoted text and escapes are passed over.
func closing(s string, i, depth int) int {
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			k := strings.IndexByte(s[i+1:], '\'')
			if k < 0 {
				return -1
			}
			i += k + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// unescapeCut reads a JSON string's body the store may have cut short, up to where it reads whole: a cut can end it
// inside an escape.
func unescapeCut(s string) string {
	for cut := 0; cut < 8 && cut <= len(s); cut++ {
		var out string
		if json.Unmarshal([]byte(`"`+s[:len(s)-cut]+`"`), &out) == nil {
			return out
		}
	}
	return ""
}
