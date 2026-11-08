package detect

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// readSettle is how long after a read that found no file its session's write of the file clears it, and so how long
// the read waits before it counts: its write may not be in the store yet. A session writes the file it looked for
// minutes later, well inside it.
const readSettle = time.Hour

// writtenPaths are the files a call that did not fail wrote, as it names them: a file tool's path, or the one file a
// shell call writes in a form the whitelist holds (shellWrite), its result's text read too.
func writtenPaths(harness, tool, call, result, cwd string) []string {
	if isShell(tool) {
		if p := shellWrite(harness, call, result, shellDir(harness, call, cwd)); p != "" {
			return []string{p}
		}
		return nil
	}
	if p, _, _ := callField(call, pathKeys); p != "" {
		return []string{p}
	}
	return nil
}

// workdirKeys are the fields of a shell call's input that name the directory it runs its command in, as capacity
// reads them.
var workdirKeys = map[string]bool{"workdir": true, "cwd": true, "working_directory": true, "workingDirectory": true, "dir": true, "directory": true}

// workdirShells are the harnesses whose shell call can name its directory (Codex's, OpenCode's and pi's workdir,
// Kimi's cwd): a call of theirs the store cut before such a field may have named one.
var workdirShells = map[string]bool{"codex": true, "opencode": true, "kimi": true, "pi": true}

// shellDir is the directory a shell call runs its command in, as far as the store says, or "" when it does not say:
// the workdir the call names, when that is a whole path; else the directory its session was in when it made the
// call. Hermes's terminal keeps a directory of its own from call to call, which its record does not hold, so only
// a workdir or a leading cd says where it is.
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

var (
	// backgroundKeys are the fields of a shell call that launch its command in the background (Claude Code's).
	backgroundKeys = map[string]bool{"run_in_background": true, "background": true}
	// backgroundRe is a result that says its command went on in the background, whose success says nothing of it.
	backgroundRe = regexp.MustCompile(`(?i)running in (?:the )?background`)
	// exitRe is the exit code a Codex result reports once its command has ended; one still running reports none.
	exitRe = regexp.MustCompile(`(?i)(?:process|command) exited with code -?\d+|exit code:? -?\d+`)
	// openErrRe is an error opening a file for writing.
	openErrRe = regexp.MustCompile(`(?i)no such file or directory|not a directory|is a directory|permission denied|read-only file system|cannot create|no space left`)
)

// shellWrite is the one file a shell call writes, or "". Only a form the whitelist holds counts, so any other line,
// however it writes, clears nothing:
//   - the command is one simple command, cat > FILE, cat >> FILE or tee [-a] FILE, with at most one here-document as
//     its input and nothing after the here-document's end, after at most a single leading cd DIR && to a whole path;
//   - FILE is a whole path, or a relative one read in DIR or else in dir, with no .. in it or in the directory;
//   - the call is no background launch (Claude Code's run_in_background, a result that says so, a Codex result with
//     no exit code), and its result shows no error opening FILE.
func shellWrite(harness, call, result, dir string) string {
	loc := callRe.FindStringSubmatchIndex(call)
	if loc == nil {
		return ""
	}
	if v, _, _ := callValue(call, backgroundKeys); string(v) == "true" || backgroundRe.MatchString(result) ||
		harness == "codex" && !exitRe.MatchString(result) {
		return ""
	}
	closed := loc[3] < len(call) && call[loc[3]] == '"' // the command's text is whole, whatever the cut took after it
	first, rest, ended := strings.Cut(unescapeCut(call[loc[2]:loc[3]]), "\n")
	toks := shellWords(first)
	if len(toks) > 3 && toks[0].word == "cd" && toks[1].op == "" && toks[2].op == "&&" {
		if !toks[1].exact || !path.IsAbs(toks[1].word) {
			return ""
		}
		dir, toks = toks[1].word, toks[3:]
	}
	file, end, ok := writeForm(toks)
	if !ok || !nothingAfter(end, rest, ended, closed) || dotdot(file) {
		return ""
	}
	if !path.IsAbs(file) {
		if dir == "" || dotdot(dir) {
			return ""
		}
		file = path.Join(dir, file)
	}
	file = path.Clean(file)
	for _, l := range strings.Split(result, "\n") {
		if strings.Contains(l, path.Base(file)) && openErrRe.MatchString(l) {
			return ""
		}
	}
	return file
}

// dotdot says a path climbs out of a directory: through a symbolic link, .. is not where its text says.
func dotdot(p string) bool { return slices.Contains(strings.Split(p, "/"), "..") }

// hereEnd is a here-document's end: its word, and whether tabs may lead its line (<<-).
type hereEnd struct {
	word string
	tabs bool
}

// writeForm reads the one command a whitelisted line holds: cat with one > or >> and its file, or tee with an
// optional -a and its file, either with at most one here-document (<< or <<- and its word) and nothing else. It
// returns the file, as its word spells it, and the here-document's end, if there is one.
func writeForm(toks []shellTok) (string, *hereEnd, bool) {
	if len(toks) == 0 || toks[0].word != "cat" && toks[0].word != "tee" {
		return "", nil, false
	}
	tee, option := toks[0].word == "tee", false
	var file *shellTok
	var end *hereEnd
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		next := i+1 < len(toks) && toks[i+1].op == ""
		switch {
		case (t.op == "<<" || t.op == "<<-") && end == nil && next:
			end = &hereEnd{word: toks[i+1].word, tabs: t.op == "<<-"}
			i++
		case !tee && (t.op == ">" || t.op == ">>") && file == nil && next:
			file = &toks[i+1]
			i++
		case tee && t.op == "" && t.word == "-a" && !option && file == nil:
			option = true
		case tee && t.op == "" && file == nil && !strings.HasPrefix(t.word, "-"):
			file = &toks[i]
		default:
			return "", nil, false
		}
	}
	if file == nil || !file.exact || file.word == "" {
		return "", nil, false
	}
	return file.word, end, true
}

// nothingAfter says nothing runs after a command's first line: bash reports the status of the last line it runs.
// The line is the command alone, or only its here-document's text follows, up to its end. A text the store cut
// inside the here-document keeps the benefit of the doubt; one it cut anywhere else may hide a next command.
func nothingAfter(end *hereEnd, rest string, ended, closed bool) bool {
	if !ended {
		return closed
	}
	lines := strings.Split(rest, "\n")
	i := 0
	if end != nil {
		for ; i < len(lines) && lines[i] != end.word && !(end.tabs && strings.TrimLeft(lines[i], "\t") == end.word); i++ {
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

// shellTok is a word of a command line, its quotes off, or one of the operators a whitelisted line can hold.
type shellTok struct {
	op    string // &&, >, >>, << or <<-; "" for a word
	word  string
	exact bool // a word the shell does not expand: no variable, command, glob or leading ~
}

// shellOps are the operators a whitelisted line can hold, the longest first. Any other operator is no write.
var shellOps = []string{"&&", "<<-", "<<", ">>", ">"}

// space says a byte parts two words.
func space(c byte) bool { return c == ' ' || c == '\t' }

// shellWords splits a command line into words and operators as the shell does, its quotes off and its escapes read.
// It is nil for a line it cannot read whole (an open quote, a backslash at its end) or with an operator no
// whitelisted line holds.
func shellWords(line string) []shellTok {
	var toks []shellTok
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case space(c):
			i++
			continue
		case strings.IndexByte(";&|()<>", c) >= 0:
			k := slices.IndexFunc(shellOps, func(o string) bool { return strings.HasPrefix(line[i:], o) })
			if k < 0 {
				return nil
			}
			toks = append(toks, shellTok{op: shellOps[k]})
			i += len(shellOps[k])
			continue
		}
		var b strings.Builder
		exact := c != '~'
	word:
		for ; i < len(line); i++ {
			switch c := line[i]; {
			case space(c) || strings.IndexByte(";&|()<>", c) >= 0:
				break word
			case c == '\\':
				if i++; i == len(line) {
					return nil
				}
				b.WriteByte(line[i])
			case c == '\'':
				k := strings.IndexByte(line[i+1:], '\'')
				if k < 0 {
					return nil
				}
				b.WriteString(line[i+1 : i+1+k])
				i += k + 1
			case c == '"':
				for i++; ; i++ {
					if i == len(line) {
						return nil
					}
					if line[i] == '"' {
						break
					}
					if line[i] == '\\' && i+1 < len(line) && strings.IndexByte("$`\"\\", line[i+1]) >= 0 {
						i++
					} else if line[i] == '$' || line[i] == '`' {
						exact = false
					}
					b.WriteByte(line[i])
				}
			default:
				if strings.IndexByte("$`*?[{", c) >= 0 {
					exact = false
				}
				b.WriteByte(c)
			}
		}
		toks = append(toks, shellTok{word: b.String(), exact: exact})
	}
	return toks
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
