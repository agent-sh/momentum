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
	if bashShell(tool) {
		if p := shellWrite(harness, call, result, shellDir(harness, call, cwd)); p != "" {
			return []string{p}
		}
		return nil
	}
	if strings.EqualFold(tool, "apply_patch") || strings.EqualFold(tool, "patch") && harness == "opencode" {
		var out []string
		for _, m := range patchFileRe.FindAllStringSubmatch(call, -1) {
			if m[1] == "Update" && m[3] != "" { // a file moved: written under its new name
				out = append(out, m[3])
			} else {
				out = append(out, m[2])
			}
		}
		return out
	}
	if strings.EqualFold(tool, "patch") { // Hermes's: OpenCode's is read above
		if mode, _, _ := callField(call, modeKeys); mode == "patch" { // its files are only in its patch text
			return nil
		}
	}
	if p, _, _ := callField(call, pathKeys); p != "" {
		return []string{p}
	}
	return nil
}

// bashShell says a shell tool runs its command in bash or a shell like it, whose words the whitelist reads: not
// PowerShell, nor exec, Codex's JavaScript, which can make several calls or branch (no other harness has an exec).
func bashShell(tool string) bool {
	return isShell(tool) && !strings.EqualFold(tool, "powershell") && !strings.EqualFold(tool, "exec")
}

// workdirKeys are the fields of a shell call's input that name the directory it runs its command in, as capacity
// reads them.
var workdirKeys = map[string]bool{"workdir": true, "cwd": true, "working_directory": true, "workingDirectory": true, "dir": true, "directory": true}

// workdirShells are the harnesses whose shell call can name its directory (Codex's and OpenCode's workdir, Kimi's
// cwd, and a cwd an extension adds to pi's bash, which itself takes only command and timeout): a call of theirs the
// store cut before such a field may have named one.
var workdirShells = map[string]bool{"codex": true, "opencode": true, "kimi": true, "pi": true}

// shellDir is the directory a shell call runs its command in, as far as the store says, or "" when it does not say:
// the workdir the call names, when that is a whole path; else the directory its session was in when it made the
// call. Hermes's and Grok's terminals keep a directory of their own from call to call, which their record does not
// hold, so only a workdir or a leading cd says where theirs is.
func shellDir(harness, call, cwd string) string {
	w, named, absent := callField(call, workdirKeys)
	switch {
	case named:
		if path.IsAbs(w) {
			return w
		}
		return ""
	case harness == "hermes", harness == "grok", !absent && workdirShells[harness], !path.IsAbs(cwd):
		return ""
	}
	return cwd
}

var (
	// backgroundKeys are each harness's shell call fields that launch its command in the background, as its source
	// names them: Claude Code's and Kimi's run_in_background, Hermes's and OpenCode's background, Grok's
	// is_background. Codex has none (its command goes on when the call yields, and its result has no exit code),
	// nor has pi's bash.
	backgroundKeys = map[string]map[string]bool{
		"claude":   {"run_in_background": true},
		"kimi":     {"run_in_background": true},
		"hermes":   {"background": true},
		"opencode": {"background": true},
		"grok":     {"is_background": true},
	}
	// backgroundWords are each harness's words for a launch it did not wait for, as its source writes them, whose
	// success says nothing of the command.
	backgroundWords = map[string]*regexp.Regexp{
		"claude": regexp.MustCompile(`Command running in background with ID: |was moved to the background \(ID: |` +
			`manually backgrounded by user with ID: `),
		"hermes":   regexp.MustCompile(`Background process started|"status": ?"yielded_to_background"|tracked background process`),
		"kimi":     regexp.MustCompile(`^task_id: |automatic_notification: true`),
		"grok":     regexp.MustCompile(`Background command started successfully\.|automatically moved to background|Process is still running`),
		"opencode": regexp.MustCompile(`Command moved to the background \(shell ID: `),
	}
	// blockKeys are Grok's field for how long its shell waits: 0 starts the command in the background.
	blockKeys = map[string]bool{"block_until_ms": true}
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
	if background(harness, call, result) {
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

// background says a shell call launched its command in the background, by its harness's own signals (backgroundKeys,
// backgroundWords, Grok's block_until_ms of 0, a Codex result with no exit code): its result says nothing of the
// command's end.
func background(harness, call, result string) bool {
	if keys := backgroundKeys[harness]; keys != nil {
		if v, _, _ := callValue(call, keys); string(v) == "true" {
			return true
		}
	}
	if re := backgroundWords[harness]; re != nil && re.MatchString(result) {
		return true
	}
	if v, _, _ := callValue(call, blockKeys); harness == "grok" && string(v) == "0" {
		return true
	}
	return harness == "codex" && !exitRe.MatchString(result)
}

// dotdot says a path climbs out of a directory: through a symbolic link, .. is not where its text says.
func dotdot(p string) bool { return slices.Contains(strings.Split(p, "/"), "..") }

// hereEnd is a here-document's end: its word, whether tabs may lead its line (<<-), and whether its word was quoted,
// which keeps the shell from joining the text's lines that end in a backslash.
type hereEnd struct {
	word         string
	tabs, quoted bool
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
			end = &hereEnd{word: toks[i+1].word, tabs: t.op == "<<-", quoted: toks[i+1].quoted}
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
	if end != nil && !end.quoted {
		lines = joinContinued(lines)
	}
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

// joinContinued joins each line that ends in an odd run of backslashes to the next, as the shell reads an unquoted
// here-document's text.
func joinContinued(lines []string) []string {
	var out []string
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		for i+1 < len(lines) && (len(l)-len(strings.TrimRight(l, "\\")))%2 == 1 {
			i++
			l = l[:len(l)-1] + lines[i]
		}
		out = append(out, l)
	}
	return out
}

// shellTok is a word of a command line, its quotes off, or one of the operators a whitelisted line can hold.
type shellTok struct {
	op     string // &&, >, >>, << or <<-; "" for a word
	word   string
	exact  bool // a word the shell does not expand: no variable, command, glob or leading ~
	quoted bool // the word holds a quote or a backslash
}

// shellOps are the operators a whitelisted line can hold, the longest first. Any other operator is no write.
var shellOps = []string{"&&", "<<-", "<<", ">>", ">"}

// space says a byte parts two words.
func space(c byte) bool { return c == ' ' || c == '\t' }

// shellWords splits a command line into words and operators as the shell does, its quotes off and its escapes read.
// It is nil for a line it cannot read whole (an open quote, a backslash at its end), with an operator no whitelisted
// line holds, or with a comment.
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
		if c == '#' { // a comment: what follows names no file
			return nil
		}
		var b strings.Builder
		exact, quoted := c != '~', false
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
				quoted = true
			case c == '\'':
				k := strings.IndexByte(line[i+1:], '\'')
				if k < 0 {
					return nil
				}
				b.WriteString(line[i+1 : i+1+k])
				i += k + 1
				quoted = true
			case c == '"':
				quoted = true
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
		toks = append(toks, shellTok{word: b.String(), exact: exact, quoted: quoted})
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
