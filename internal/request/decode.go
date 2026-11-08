package request

import (
	"encoding/json"
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
	Patch      bool // a writer's input is a patch, whose file headers name its files
	CommandCut bool // the store cut the last command: what it ran last may have had more arguments
	// OutputCut is an output whose text the store cut where it ends, its last line perhaps shortened: one it keeps
	// as JSON (Hermes); a plain text the store keeps has its lines joined, so nothing is read off it anyway
	OutputCut bool
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
	OutputCut       bool // the store cut its output where its text ends (a code-mode result's JSON)
	// Index and Polled are what the script printed with the result, when it did: its call's place in a
	// Promise.all array ("i"), or the session a poll named ("id"); -1 when none
	Index  int
	Polled int64
	// Plain is a command whose output text the script printed (text(r.output)), not its result: no exit code
	// shows with it, so only its commit lines are read
	Plain bool
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
	pollSessionRe = regexp.MustCompile(`["']?session_id["']?\s*:\s*(?:(\d+)|([A-Za-z_$][\w$]*)\.session_id)`)
	// a Codex command's plain result: still running in a session, or done with an exit code
	runningRe = regexp.MustCompile(`Process running with session ID (\d+)`)
	exitedRe  = regexp.MustCompile(`Process exited with code (-?\d+)`)
)

// plainPrintRe is a script's text() of a call's output text: text(r.output).
var plainPrintRe = regexp.MustCompile(`\btext\(\s*([A-Za-z_$][\w$]*)\.output\s*\)`)

// plainPrint is the one exec_command whose output text a script printed (text(r.output)), when the script printed
// no result but that text: its index among the script's exec_command calls. The text must be of one variable, which
// one exec_command set and nothing else did, so the printed lines are that command's alone.
func plainPrint(script string, calls []codeCall, prints []int, chunks []Chunk) (int, bool) {
	if len(prints) > 0 || len(chunks) > 0 {
		return 0, false
	}
	ms := plainPrintRe.FindAllStringSubmatch(script, -1)
	if len(ms) == 0 {
		return 0, false
	}
	name := ms[0][1]
	for _, m := range ms[1:] {
		if m[1] != name {
			return 0, false // two commands' output texts: which line is whose is not known
		}
	}
	k, n, exec := 0, 0, 0
	for _, c := range calls {
		if !c.exec {
			return 0, false
		}
		if c.assign == name {
			k, n = exec, n+1
		}
		exec++
	}
	return k, n == 1
}

// printedText is what a code-mode script printed: its output after the "Output:" header, less the last token of a
// text the store cut.
func printedText(output string) string {
	text := uncut(output, strings.HasSuffix(output, "…"))
	if i := strings.Index(text, "Output:"); i >= 0 {
		text = text[i+len("Output:"):]
	}
	return text
}

// printRe is a script's text() of a call's result: inline (text(await tools.X(...))) or by the variable the
// result was set to (text(r)).
var printRe = regexp.MustCompile(`\btext\(\s*(?:await\s+(tools)\.(?:exec_command|write_stdin)\(|([A-Za-z_$][\w$]*)\s*\))`)

// script is a code-mode script parsed once: its calls, its prints, and how it runs them (codeCalls).
type script struct {
	calls    []codeCall
	prints   []int
	parallel bool
	arrayAt  int
}

func parseScript(s string) *script {
	sc := &script{}
	sc.calls, sc.prints, sc.parallel, sc.arrayAt = codeCalls(s)
	return sc
}

// codeCallLocs are a script's tools.exec_command( and tools.write_stdin( calls, each as [start, end, the start and
// end of the variable its result is set to (-1 for none), the start and end of the tool's name]: from "tools."
// found by a scan, back over an assignment before it (const|let|var NAME = await), so no byte but those is tried.
func codeCallLocs(s string) [][]int {
	var out [][]int
	lo := 0
	for k := 0; ; {
		i := strings.Index(s[k:], "tools.")
		if i < 0 {
			return out
		}
		at := k + i
		k = at + len("tools.")
		if at > 0 && nameByte(s[at-1]) {
			continue
		}
		name := ""
		for _, n := range []string{"exec_command", "write_stdin"} {
			if strings.HasPrefix(s[k:], n+"(") {
				name = n
			}
		}
		if name == "" {
			continue
		}
		loc := []int{at, k + len(name) + 1, -1, -1, k, k + len(name)}
		if start, a, b, ok := assignBefore(s, at, lo); ok {
			loc[0], loc[2], loc[3] = start, a, b
		}
		out = append(out, loc)
		k, lo = loc[1], loc[1]
	}
}

// assignBefore is the assignment that sets a variable to the call at at, read back from it but not past lo: the
// statement's start, the variable's start and end, and false when none is there.
func assignBefore(s string, at, lo int) (start, a, b int, ok bool) {
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	j := at
	for j > lo && space(s[j-1]) {
		j--
	}
	if j < at && j-5 >= lo && s[j-5:j] == "await" && (j-5 == 0 || !nameByte(s[j-6])) {
		j -= 5
		for j > lo && space(s[j-1]) {
			j--
		}
	}
	if j <= lo || s[j-1] != '=' {
		return 0, 0, 0, false
	}
	j--
	for j > lo && space(s[j-1]) {
		j--
	}
	b = j
	for j > lo && (nameByte(s[j-1]) || s[j-1] == '$') {
		j--
	}
	if j == b || s[j] >= '0' && s[j] <= '9' {
		return 0, 0, 0, false
	}
	a, start = j, j
	k := j
	for k > lo && space(s[k-1]) {
		k--
	}
	for _, kw := range []string{"const", "let", "var"} {
		if k < j && k-len(kw) >= lo && s[k-len(kw):k] == kw && (k-len(kw) == 0 || !nameByte(s[k-len(kw)-1])) {
			start = k - len(kw)
		}
	}
	return start, a, b, true
}

// codeCalls are a code-mode script's exec_command and write_stdin calls, in order, and the calls its text() calls
// print, in the order it prints them (-1 for a variable no call's result was set to). parallel is true when the
// script runs calls side by side (Promise.all, a call not awaited), so its results' order says nothing of whose
// they are; arrayAt is the first call after a Promise.all, whose array the results' places ("i") index (-1 when
// none).
func codeCalls(script string) (calls []codeCall, prints []int, parallel bool, arrayAt int) {
	locs := codeCallLocs(script)
	at := map[int]int{}                          // a call's "tools." offset: its index
	promise := strings.Index(script, "Promise.") // a call in its array is not awaited on its own: parallel
	arrayAt = -1                                 // the first call of a Promise.all array
	for k, l := range locs {
		if promise >= 0 && l[0] > promise {
			arrayAt = k
			break
		}
	}
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
		toolsAt := l[4] - len("tools.")
		at[toolsAt] = k
		if !strings.HasSuffix(strings.TrimRight(script[:toolsAt], " \t\r\n"), "await") {
			parallel = true
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
		calls = append(calls, c)
	}
	for _, m := range printRe.FindAllStringSubmatchIndex(script, -1) {
		if m[2] >= 0 {
			if k, ok := at[m[2]]; ok {
				prints = append(prints, k)
			}
			continue
		}
		name, k := script[m[4]:m[5]], -1
		for i, l := range locs {
			if l[0] < m[0] && calls[i].assign == name {
				k = i // the latest call whose result the variable was set to before the print
			}
		}
		prints = append(prints, k)
	}
	return calls, prints, parallel, arrayAt
}

// pairCode matches a code-mode script's results to its calls, and each poll's result to the command whose session
// it names: one of the script's (by the variable its result was set to, or the session it yielded), else a session
// an earlier call left running (Polls). A result is its call's by what the script printed with it when it printed
// one (its place in a Promise.all array, the session a poll named), else by the order the script prints results.
// A poll by a variable the script did not set from an exec_command is read as nothing. Unpaired is true when the
// script runs calls side by side and its results carry no place, or its results are not one for each print (of
// the prints it reached, when it failed or still runs).
func pairCode(chunks []Chunk, calls []codeCall, prints []int, parallel bool, stopped bool, arrayAt int) (own, polls []Chunk, unpaired bool) {
	result := map[int]Chunk{}
	var rest []Chunk
	for _, ch := range chunks {
		switch {
		case ch.Index >= 0 && arrayAt >= 0 && arrayAt+ch.Index < len(calls):
			result[arrayAt+ch.Index] = ch
		case ch.Polled > 0:
			ch.Session = ch.Polled // a poll in a loop: the session it named
			polls = append(polls, ch)
		default:
			rest = append(rest, ch)
		}
	}
	if len(rest) > 0 {
		if parallel || len(rest) > len(prints) || len(rest) < len(prints) && !stopped {
			return nil, nil, true
		}
		for k, ch := range rest {
			if i := prints[k]; i >= 0 {
				if _, ok := result[i]; !ok {
					result[i] = ch
				}
			}
		}
	}
	vars, sessions := map[string]int{}, map[int64]int{}
	for i, c := range calls {
		ch, printed := result[i]
		if c.exec {
			ch.Command, ch.Cut = c.cmd, c.cut
			own = append(own, ch)
			k := len(own) - 1
			if c.assign != "" {
				vars[c.assign] = k
			}
			if ch.Running {
				sessions[ch.Session] = k
			}
			continue
		}
		if !printed {
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
	return decode(harness, tool, input, output, nil)
}

// decode is Decode with a code-mode script parsed already (nil to parse it here), so a call's script is parsed once.
func decode(harness, tool, input, output string, sc *script) Decoded {
	var d Decoded
	tool = strings.ToLower(tool)
	addPatch := func(t string) {
		d.Patch = d.Patch || patchRe.MatchString(t)
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
		if sc == nil {
			sc = parseScript(input)
		}
		calls, prints, parallel, arrayAt := sc.calls, sc.prints, sc.parallel, sc.arrayAt
		d.Commands, d.CommandCut = nil, false
		for _, c := range calls {
			if c.exec {
				d.Commands, d.CommandCut = append(d.Commands, c.cmd), c.cut
			}
		}
		// a script that failed or still runs printed the results it reached
		stopped := strings.HasPrefix(strings.TrimSpace(output), "Script failed") || strings.HasPrefix(strings.TrimSpace(output), "Script running")
		d.Chunks, d.Polls, d.Unpaired = pairCode(chunks, calls, prints, parallel, stopped, arrayAt)
		if k, ok := plainPrint(input, calls, prints, chunks); ok && !d.Unpaired && !parallel {
			// the script printed its one command's output text, not its result: that text's lines are the command's
			d.Chunks[k].Output, d.Chunks[k].Plain = printedText(output), true
			d.Output = d.Chunks[k].Output
		}
	case harness == "codex" && tool == "write_stdin":
		// Codex's write_stdin tool polls a session an earlier exec_command left running
		d.Output = uncut(output, strings.HasSuffix(output, "…"))
		d.Joined = output != "" && !strings.Contains(output, "\n")
		if n, ok := inputSession(input); ok {
			c := plainChunk(output, n)
			c.Output = d.Output
			d.Polls = []Chunk{c}
		}
	case harness == "hermes":
		var outs []string
		for _, f := range fields(output, `"`, false) {
			if outputKeys[f.key] && !f.list && len(f.values) > 0 {
				outs = append(outs, uncut(f.values[0], f.cut))
				d.OutputCut = f.cut // a field the cut ends is the last the store kept
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

// inputSession is the session a Codex write_stdin tool's input names: its JSON's session_id, read as JSON. A cut
// input names none.
func inputSession(input string) (int64, bool) {
	var in struct {
		Session json.Number `json:"session_id"`
	}
	if json.Unmarshal([]byte(input), &in) != nil || in.Session == "" {
		return 0, false
	}
	n, err := in.Session.Int64()
	return n, err == nil
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
	// only the header before Output: is Codex's; what follows is the command's own, which may print these words
	if i := strings.Index(output, "Output:"); i >= 0 {
		output = output[:i]
	}
	if m := exitedRe.FindStringSubmatch(output); m != nil {
		c.Failed = m[1] != "0"
	} else if m := runningRe.FindStringSubmatch(output); m != nil {
		c.Running = true
		c.Session, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return c
}

// codeChunks are the results a Codex code-mode output holds, in order: each object's output fields (output,
// stdout, stderr) as one text, the last token of one the cut ends dropped, whether its exit code is not 0, and what
// the script printed with the result in that object (carried): "i", its call's place in a Promise.all array, and
// "id", the session a poll named. A key of another object (another tool's structuredContent id) is not its.
func codeChunks(output string) []Chunk {
	// a frame is an object being read: the chunk its own output fields make, the chunks it and the objects inside
	// it made, and the keys it carries, which go to those chunks that carry none of an object nearer them. A key of
	// an object beside the result's (another tool's structuredContent) reaches no chunk of it.
	type frame struct {
		start, chunk int
		made         []int
		index        int
		polled       int64
	}
	var out []Chunk
	var stack []frame
	pop := func() {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, k := range f.made {
			if out[k].Index < 0 && f.index >= 0 {
				out[k].Index = f.index
			}
			if out[k].Polled < 0 && f.polled > 0 {
				out[k].Polled = f.polled
			}
		}
		if len(stack) > 0 {
			stack[len(stack)-1].made = append(stack[len(stack)-1].made, f.made...)
		}
	}
	for i := 0; i < len(output); {
		switch output[i] {
		case '{':
			stack = append(stack, frame{start: i, chunk: -1, index: -1})
			i++
			continue
		case '}':
			if len(stack) > 0 {
				pop()
			}
			i++
			continue
		case '"':
		default:
			i++
			continue
		}
		key, n, _ := literal(output[i:])
		k := skipSpace(output, i+n)
		if len(stack) == 0 || k >= len(output) || output[k] != ':' {
			i += n // a string that is no key of an object
			continue
		}
		top := &stack[len(stack)-1]
		v := skipSpace(output, k+1)
		switch {
		case outputKeys[key] && v < len(output) && output[v] == '"':
			text, m, closed := literal(output[v:])
			if top.chunk >= 0 {
				out[top.chunk].Output += "\n" + uncut(text, !closed)
				out[top.chunk].OutputCut = !closed
			} else {
				c := Chunk{Output: uncut(text, !closed), OutputCut: !closed, Index: -1, Polled: -1}
				if e := exitRe.FindStringSubmatch(output[top.start:i]); e != nil {
					c.Failed = e[1] != "0"
				} else if m := sessionRe.FindStringSubmatch(output[top.start:i]); m != nil {
					c.Running = true
					c.Session, _ = strconv.ParseInt(m[1], 10, 64)
				}
				top.chunk = len(out)
				top.made = append(top.made, len(out))
				out = append(out, c)
			}
			i = v + m
		case key == "i" || key == "id":
			e := v
			for e < len(output) && output[e] >= '0' && output[e] <= '9' {
				e++
			}
			if e > v && key == "i" {
				if n, err := strconv.Atoi(output[v:e]); err == nil {
					top.index = n
				}
			} else if e > v {
				top.polled, _ = strconv.ParseInt(output[v:e], 10, 64)
			}
			i = max(e, v)
		default:
			i = k + 1
		}
	}
	for len(stack) > 0 { // objects the cut ends
		pop()
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
