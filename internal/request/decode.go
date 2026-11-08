package request

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decoded is what a call's stored input and its result's stored output hold, in the harness's own format: the
// shell commands the input runs, the output's text (its last token dropped when the store cut it), and the files a
// writer's input names.
type Decoded struct {
	Commands   []string
	Output     string
	Paths      []string
	CommandCut bool // the store cut the last command: what it ran last may have had more arguments
	// Joined is an output the store keeps as plain text with its lines flattened to spaces: where a line ends is
	// not known, so nothing is read off it (a whole read gives its lines)
	Joined bool
	// Chunks are a Codex code-mode script's exec_command calls, each with its own result and the results of the
	// polls (write_stdin) of its session in the script; Polls are the results of the polls of a session no command
	// of the script yielded (one an earlier call left running), and of Codex's write_stdin tool. Unpaired is a
	// script whose results cannot be matched one to one to its calls.
	Chunks   []Chunk
	Polls    []Chunk
	Unpaired bool
}

// Chunk is one result of a Codex command: the command, its output, whether it exited non-zero, whether it still
// ran when it yielded (no exit code) and the session it runs in then, and whether the store cut the command.
type Chunk struct {
	Command, Output string
	Failed, Running bool
	Session         int64
	Cut             bool
}

// codeCall is a tool call of a code-mode script: an exec_command with its command, or a write_stdin with the
// session it polls, by its id or by the variable an exec_command's result was set to.
type codeCall struct {
	exec    bool
	cmd     string
	cut     bool
	assign  string
	session int64
	ref     string
}

var (
	codeCallRe    = regexp.MustCompile(`(?:\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:await\s+)?)?\btools\.(exec_command|write_stdin)\(`)
	pollSessionRe = regexp.MustCompile(`["']?session_id["']?\s*:\s*(?:(\d+)|([A-Za-z_$][\w$]*)\.session_id)`)
	// a Codex command's plain result: still running in a session, or done with an exit code
	runningRe = regexp.MustCompile(`Process running with session ID (\d+)`)
	exitedRe  = regexp.MustCompile(`Process exited with code (-?\d+)`)
)

// codeCalls are a code-mode script's exec_command and write_stdin calls, in order.
func codeCalls(script string) []codeCall {
	locs := codeCallRe.FindAllStringSubmatchIndex(script, -1)
	var out []codeCall
	for k, l := range locs {
		end := len(script)
		if k+1 < len(locs) {
			end = locs[k+1][0]
		}
		arg := script[l[1]:end]
		c := codeCall{exec: script[l[4]:l[5]] == "exec_command"}
		if l[2] >= 0 {
			c.assign = script[l[2]:l[3]]
		}
		if c.exec {
			for _, f := range fields(arg, `"'`+"`", true) {
				if commandKeys[f.key] && !f.list && len(f.values) > 0 {
					c.cmd, c.cut = uncut(f.values[0], f.cut), f.cut
					break
				}
			}
		} else if m := pollSessionRe.FindStringSubmatch(arg); m != nil {
			if m[1] != "" {
				c.session, _ = strconv.ParseInt(m[1], 10, 64)
			} else {
				c.ref = m[2]
			}
		}
		out = append(out, c)
	}
	return out
}

// pairCode matches a code-mode script's results to its calls, one each in order, and each poll's result to the
// command whose session it names: one of the script's (by the variable its result was set to, or the session it
// yielded), else a session an earlier call left running (Polls). A poll by a variable the script did not set
// from an exec_command is read as nothing. Unpaired is true when the results are not one per call.
func pairCode(chunks []Chunk, calls []codeCall) (own, polls []Chunk, unpaired bool) {
	if len(chunks) != len(calls) {
		return nil, nil, true
	}
	vars, sessions := map[string]int{}, map[int64]int{}
	for i, c := range calls {
		ch := chunks[i]
		if c.exec {
			ch.Command, ch.Cut = c.cmd, c.cut
			own = append(own, ch)
			k := len(own) - 1
			if c.assign != "" {
				vars[c.assign] = k
			}
			if ch.Running && ch.Session > 0 {
				sessions[ch.Session] = k
			}
			continue
		}
		k, ok := vars[c.ref]
		if c.ref == "" {
			k, ok = sessions[c.session]
		}
		switch {
		case ok:
			o := &own[k]
			o.Output += "\n" + ch.Output
			o.Failed, o.Running = ch.Failed, ch.Running
		case c.ref == "" && c.session > 0:
			ch.Session = c.session
			polls = append(polls, ch)
		}
	}
	return own, polls, false
}

// Keys of the stored inputs and outputs, every harness's.
var (
	commandKeys = map[string]bool{"command": true, "cmd": true}
	pathKeys    = map[string]bool{"file_path": true, "filePath": true, "path": true}
	patchKeys   = map[string]bool{"patch": true, "patchText": true}
	outputKeys  = map[string]bool{"output": true, "stdout": true, "stderr": true}
)

// Decode reads a call's stored input and its result's stored output as the harness stores them. Claude Code, pi,
// opencode and Kimi keep a call's input as JSON, Hermes as Python's JSON (a space after each colon), Codex an
// exec_command's as JSON, an apply_patch's as the patch itself, and a code-mode exec's as a JavaScript script,
// whose tool calls name their command in a cmd or command field, quoted key or not. Codex's code mode and Hermes keep a result as JSON whose output fields hold the text, its
// newlines escaped.
//
// The store keeps each text's first bytes only (240 for an input), and every value goes through one rule for its
// cut: what touches the cut is not known, so it is dropped, never read shorter. A path value the cut ends is no
// path; a command, a patch or an output the cut ends loses its last token (the one the cut may have shortened)
// before anything is matched in it.
func Decode(harness, tool, input, output string) Decoded {
	var d Decoded
	tool = strings.ToLower(tool)
	addPatch := func(t string) {
		for _, m := range patchRe.FindAllStringSubmatch(t, -1) {
			if !contains(d.Paths, m[1]) {
				d.Paths = append(d.Paths, m[1])
			}
		}
	}
	switch {
	case writers[tool] && !strings.HasPrefix(strings.TrimSpace(input), "{"):
		// a patch that is the whole input (Codex's apply_patch): only its file headers name files
		addPatch(uncut(input, strings.HasSuffix(input, "…")))
	case writers[tool]:
		in := fields(input, `"'`+"`", true)
		for _, f := range in {
			if pathKeys[f.key] && len(f.values) > 0 && f.values[0] != "" && !f.cut {
				d.Paths = append(d.Paths, f.values[0])
				break
			}
		}
		if len(d.Paths) == 0 {
			// a patch in a field (Hermes, opencode)
			for _, f := range in {
				if patchKeys[f.key] && !f.list && len(f.values) > 0 {
					addPatch(uncut(f.values[0], f.cut))
				}
			}
		}
	default:
		for _, f := range fields(input, `"'`+"`", true) {
			if !commandKeys[f.key] {
				continue
			}
			switch {
			case f.list:
				d.Commands = append(d.Commands, argvScript(f.values))
			case len(f.values) > 0:
				d.Commands = append(d.Commands, uncut(f.values[0], f.cut))
			}
			d.CommandCut = f.cut
		}
	}
	// Codex's code mode and Hermes keep a result as JSON whose output fields are its text, its lines kept; every
	// other harness keeps the text itself, which may print JSON of its own, and the store joins its lines.
	switch {
	case harness == "codex" && tool == "exec":
		chunks := codeChunks(output)
		var outs []string
		for _, c := range chunks {
			outs = append(outs, c.Output)
		}
		d.Output = strings.Join(outs, "\n")
		calls := codeCalls(input)
		d.Commands, d.CommandCut = nil, false
		for _, c := range calls {
			if c.exec {
				d.Commands, d.CommandCut = append(d.Commands, c.cmd), c.cut
			}
		}
		d.Chunks, d.Polls, d.Unpaired = pairCode(chunks, calls)
	case harness == "codex" && tool == "write_stdin":
		// Codex's write_stdin tool polls a session an earlier exec_command left running
		d.Output = uncut(output, strings.HasSuffix(output, "…"))
		d.Joined = output != "" && !strings.Contains(output, "\n")
		if m := pollSessionRe.FindStringSubmatch(input); m != nil && m[1] != "" {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			c := plainChunk(output, n)
			c.Output = d.Output
			d.Polls = []Chunk{c}
		}
	case harness == "hermes":
		var outs []string
		for _, f := range fields(output, `"`, false) {
			if outputKeys[f.key] && !f.list && len(f.values) > 0 {
				outs = append(outs, uncut(f.values[0], f.cut))
			}
		}
		d.Output = strings.Join(outs, "\n")
		if len(outs) == 0 {
			d.Output = output // a writer's result: no output field
		}
	default:
		d.Output = uncut(output, strings.HasSuffix(output, "…"))
		d.Joined = output != "" && !strings.Contains(output, "\n")
	}
	return d
}

// exitRe is a code-mode result's exit code; sessionRe the session a running one yielded.
var (
	exitRe    = regexp.MustCompile(`"exit_code":\s*(-?\d+)`)
	sessionRe = regexp.MustCompile(`"session_id":\s*(\d+)`)
)

// plainChunk is a Codex command's plain result (Codex's exec_command and write_stdin tools): whether it exited
// non-zero, and the session it still runs in (or the one it polled).
func plainChunk(output string, session int64) Chunk {
	c := Chunk{Session: session}
	if m := exitedRe.FindStringSubmatch(output); m != nil {
		c.Failed = m[1] != "0"
	} else if m := runningRe.FindStringSubmatch(output); m != nil {
		c.Running = true
		c.Session, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return c
}

// codeChunks are the results a Codex code-mode output holds, in order: each object's output fields (output,
// stdout, stderr) as one text, the last token of one the cut ends dropped, and whether its exit code is not 0.
func codeChunks(output string) []Chunk {
	var out []Chunk
	obj := -1
	for i := 0; i < len(output); {
		j := strings.IndexByte(output[i:], '"')
		if j < 0 {
			break
		}
		j += i
		key, n, _ := literal(output[j:])
		k := skipSpace(output, j+n)
		if !outputKeys[key] || k >= len(output) || output[k] != ':' {
			i = j + n
			continue
		}
		v := skipSpace(output, k+1)
		if v >= len(output) || output[v] != '"' {
			i = v
			continue
		}
		text, m, closed := literal(output[v:])
		start := strings.LastIndexByte(output[:j], '{')
		if start == obj && len(out) > 0 {
			out[len(out)-1].Output += "\n" + uncut(text, !closed)
		} else {
			c := Chunk{Output: uncut(text, !closed)}
			if e := exitRe.FindStringSubmatch(output[max(start, 0):j]); e != nil {
				c.Failed = e[1] != "0"
			} else if m := sessionRe.FindStringSubmatch(output[max(start, 0):j]); m != nil {
				c.Running = true
				c.Session, _ = strconv.ParseInt(m[1], 10, 64)
			}
			out = append(out, c)
		}
		obj = start
		i = v + m
	}
	return out
}

// uncut is a text with its cut taken into account: when the store cut it, without the ellipsis and the token the
// cut ends (one the cut may have shortened). A text cut just after a space keeps every token.
func uncut(s string, cut bool) string {
	if !cut {
		return s
	}
	s = strings.TrimSuffix(s, "…")
	if i := strings.LastIndexAny(s, " \t\r\n"); i >= 0 {
		return s[:i+1]
	}
	return ""
}

// argvScript is a command named as its words: a shell's -c script as it is, else the words quoted for a shell.
func argvScript(argv []string) string {
	if len(argv) >= 3 && shellProgs[filepath.Base(argv[0])] && strings.HasPrefix(argv[1], "-") && strings.Contains(argv[1], "c") {
		return argv[2]
	}
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// field is a key and its value: a string, or a list of strings. cut is true when the text ends inside its (last)
// value: the store cut it there.
type field struct {
	key    string
	values []string
	list   bool
	cut    bool
}

// fields reads the keys of a text whose values are strings or lists of strings, in order: JSON, Python's JSON and
// a JavaScript object literal alike. A string literal opens with one of quotes; with bare, a key may be a bare
// name, as JavaScript writes one. A string that is no key's value is passed over whole, so a key inside a value
// is never read. A value the text ends inside keeps what the text holds of it, marked cut.
func fields(text, quotes string, bare bool) []field {
	var out []field
	i := 0
	for i < len(text) {
		c := text[i]
		var key string
		switch {
		case strings.IndexByte(quotes, c) >= 0:
			s, n, _ := literal(text[i:])
			i += n
			j := skipSpace(text, i)
			if j >= len(text) || text[j] != ':' {
				continue
			}
			key, i = s, j+1
		case bare && (c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') && (i == 0 || !nameByte(text[i-1])):
			j := i
			for j < len(text) && nameByte(text[j]) {
				j++
			}
			k := skipSpace(text, j)
			if k >= len(text) || text[k] != ':' {
				i = j
				continue
			}
			key, i = text[i:j], k+1
		default:
			i++
			continue
		}
		i = skipSpace(text, i)
		if i >= len(text) {
			break
		}
		switch {
		case strings.IndexByte(quotes, text[i]) >= 0:
			s, n, closed := literal(text[i:])
			out = append(out, field{key: key, values: []string{s}, cut: !closed})
			i += n
		case text[i] == '[':
			f := field{key: key, list: true}
			i++
			for {
				i = skipSpace(text, i)
				if i >= len(text) || text[i] == ']' {
					break
				}
				if text[i] == ',' {
					i++
					continue
				}
				if strings.IndexByte(quotes, text[i]) < 0 {
					break
				}
				s, n, closed := literal(text[i:])
				f.values = append(f.values, s)
				f.cut = !closed
				i += n
			}
			out = append(out, f)
		}
	}
	return out
}

func nameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func skipSpace(text string, i int) int {
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	return i
}

// literal reads the string literal text opens with (its first byte the quote): JSON's escapes and JavaScript's
// (\', \xHH, \u{...}). It returns the value, the bytes read, and whether the literal closed; one the text ends
// inside keeps what it holds, less the ellipsis the store puts at a cut.
func literal(text string) (string, int, bool) {
	q := text[0]
	var b strings.Builder
	i := 1
	for i < len(text) {
		c := text[i]
		switch {
		case c == q:
			return b.String(), i + 1, true
		case c == '\\' && i+1 < len(text):
			i++
			switch e := text[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if i+1 < len(text) && text[i+1] == '{' {
					if end := strings.IndexByte(text[i:], '}'); end > 0 {
						if r, err := strconv.ParseUint(text[i+2:i+end], 16, 32); err == nil && r <= utf8.MaxRune {
							b.WriteRune(rune(r))
							i += end
							break
						}
					}
				}
				if i+4 < len(text) {
					if r, err := strconv.ParseUint(text[i+1:i+5], 16, 32); err == nil && r <= utf8.MaxRune {
						r1 := rune(r)
						i += 4
						if r1 >= 0xd800 && r1 < 0xdc00 && i+6 < len(text) && text[i+1] == '\\' && text[i+2] == 'u' {
							if r2, err := strconv.ParseUint(text[i+3:i+7], 16, 32); err == nil && r2 >= 0xdc00 && r2 < 0xe000 {
								r1 = (r1-0xd800)<<10 + (rune(r2) - 0xdc00) + 0x10000
								i += 6
							}
						}
						b.WriteRune(r1)
						break
					}
				}
				b.WriteString(`\u`)
			case 'x':
				if i+2 < len(text) {
					if r, err := strconv.ParseUint(text[i+1:i+3], 16, 8); err == nil {
						b.WriteRune(rune(r))
						i += 2
						break
					}
				}
				b.WriteByte(e)
			default:
				b.WriteByte(e) // \" \' \\ \/ \`
			}
			i++
		default:
			_, n := utf8.DecodeRuneInString(text[i:])
			b.WriteString(text[i : i+n])
			i += n
		}
	}
	return strings.TrimSuffix(b.String(), "…"), len(text), false
}
