package capacity

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Who made a folder at the top of a scratch root. A folder is a task's when an agent made it: an agent's tool call
// in the store's transcripts creates it, from madeBefore before the folder's birth (statx) to madeAfter after. A call
// creates it when its command makes it (mkdir, mktemp, git clone, git worktree add or git init, the destination of
// cp, mv, rsync or scp), hands it to a program to write to (an output, directory or cache option, an assignment of a
// directory variable such as HF_HOME or TMPDIR), or makes it in a script (a path in quotes in a command that makes
// directories), or when the call writes a file in it (an editor's Write, a patch's Add File). Its output counts only
// when it says it created directories, or when its call ran mktemp, which prints the name it made. A folder that is
// only an argument of a deletion or of cd, a listing's output, and a call that is not an agent's make nothing: a
// tool that makes its cache again after an agent deleted it, or that a listing names, made it itself.

// madeBefore and madeAfter bound when an agent's call that creates a folder is recorded, around the folder's birth:
// a call is recorded as it is issued, and a command may run minutes before it makes its output directory; an agent
// names what mktemp printed, or writes the first file, minutes after.
const (
	madeBefore = 10 * time.Minute
	madeAfter  = 10 * time.Minute
)

// The kinds of a transcript event that carry an agent's command and its output.
const (
	toolCall   = "tool_call"
	toolResult = "tool_result"
)

// maker is an agent session a task folder's birth is on record with, and where it worked then.
type maker struct {
	session, harness, cwd string
	at                    time.Time
}

// madeBy are the agent sessions whose calls created a folder around its birth, earliest first, each once. Without
// a store none is on record.
func (r *runner) madeBy(ctx context.Context, spellings []string, born time.Time) ([]maker, error) {
	if r.st == nil {
		return nil, nil
	}
	evs, err := r.st.Events(ctx, store.Query{Source: store.Transcripts, Kinds: []string{toolCall, toolResult},
		Since: born.Add(-madeBefore), Until: born.Add(madeAfter), Limit: math.MaxInt32})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS.Before(evs[j].TS) })
	calls := map[string]store.Event{} // by tool id, for an output's call
	for _, e := range evs {
		if id := toolID(e); e.Kind == toolCall && id != "" {
			calls[id] = e
		}
	}
	var out []maker
	seen := map[string]bool{}
	for _, e := range evs {
		by := e
		switch e.Kind {
		case toolCall:
			if e.Actor != store.ActorAgent || !callCreates(e.Tool, e.Summary, spellings) {
				continue
			}
		case toolResult:
			c, ok := calls[toolID(e)]
			if !ok || c.Actor != store.ActorAgent || !resultCreates(e.Summary, c.Summary, spellings) {
				continue
			}
			by = c
		}
		if key := by.Harness + "\x00" + by.Session; !seen[key] {
			seen[key] = true
			out = append(out, maker{session: by.Session, harness: by.Harness, cwd: by.Cwd, at: by.TS})
		}
	}
	return out, nil
}

// toolID is the id a call and its output share, from the event's meta.
func toolID(e store.Event) string {
	var m struct {
		ToolID string `json:"tool_id"`
	}
	if json.Unmarshal([]byte(e.Meta), &m) != nil {
		return ""
	}
	return m.ToolID
}

// resultCreates reports whether a call's output says it created a folder: a writer that made directories (a file's
// parents among them), or the path mktemp printed, which is the folder itself (mktemp makes no parents).
func resultCreates(result, call string, spellings []string) bool {
	text := unescapeJSON(result)
	for _, at := range occurrences(text, spellings) {
		if strings.Contains(text, `"dirs_created": true`) || strings.Contains(text, `"dirs_created":true`) ||
			wholeName(text, at) && strings.Contains(unescapeJSON(call), "mktemp") {
			return true
		}
	}
	return false
}

// writeTools are the tools that write a file at a path they are given, making its directories.
var writeTools = map[string]bool{"Write": true, "write_file": true, "create_file": true}

var (
	pathField = regexp.MustCompile(`"(?:file_path|path)"\s*:\s*"$`)
	// scriptMakes ends the text before a name a script makes a directory of, with its parents: os.makedirs("P",
	// fs.mkdirSync('P', mkdirp("P". scriptMakesOne is the call that makes only the last directory of its name.
	scriptMakes    = regexp.MustCompile(`(?:makedirs|mkdirSync|mkdirp)\(\s*[rRbBuUfF]{0,2}["']$`)
	scriptMakesOne = regexp.MustCompile(`mkdir\(\s*[rRbBuUfF]{0,2}["']$`)
	// pathOpen ends the text before a name a Path is made of; pathMkdir follows the rest of its string, with the
	// call's arguments as far as the store kept them: Path("P").mkdir(parents=True).
	pathOpen  = regexp.MustCompile(`Path\(\s*[rR]?["']$`)
	pathMkdir = regexp.MustCompile(`^[^"'\n]*["']\s*\)\s*\.mkdir\(([^)]*)`)
	// pathVar ends the text before a name a variable's Path is made of (out = Path("P); strVar a variable's string
	// (R = "P); strEnd ends the name's string.
	pathVar    = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(?:pathlib\.)?Path\(\s*[rR]?["']$`)
	strVar     = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*=\s*[rR]?["']$`)
	strEnd     = regexp.MustCompile(`^[^"'\n]*["']`)
	commandKey = regexp.MustCompile("\"(?:command|cmd)\"\\s*:\\s*\"|[^A-Za-z_\"]cmd\\s*:\\s*[\"`]")
)

// callCreates reports whether a call's input creates a folder: a write tool's path in it, a patch's Add File in it,
// a script that makes it, or a command that makes it.
func callCreates(tool, input string, spellings []string) bool {
	text := unescapeJSON(input)
	for _, at := range occurrences(text, spellings) {
		before, after, whole := text[:at[0]], text[at[1]:], wholeName(text, at)
		switch {
		case writeTools[tool] && pathField.MatchString(before), strings.HasSuffix(before, "Add File: "):
			return true
		case scriptCreates(before, after, whole):
			return true
		}
	}
	for _, c := range commandsIn(input) {
		if commandCreates(c, spellings) {
			return true
		}
	}
	return false
}

// scriptCreates reports whether a script makes the folder a name in it names, from the text before the name and
// after it: a call that makes a directory of the name, or of a variable that holds it (out = Path("P");
// out.mkdir(), or R = "P"; os.makedirs(R)). A call that makes only the last directory of its name makes the folder
// only when the name is the folder itself, or when it is told to make parents (parents=True).
func scriptCreates(before, after string, whole bool) bool {
	parents := func(args string) bool { return whole || strings.Contains(args, "parents=True") }
	if scriptMakes.MatchString(before) || whole && scriptMakesOne.MatchString(before) {
		return true
	}
	if pathOpen.MatchString(before) {
		if m := pathMkdir.FindStringSubmatch(after); m != nil && parents(m[1]) {
			return true
		}
	}
	end := strEnd.FindStringIndex(after)
	if end == nil {
		return false
	}
	rest := after[end[1]:]
	if m := pathVar.FindStringSubmatch(before); m != nil {
		v := regexp.QuoteMeta(m[1])
		mm := regexp.MustCompile(`\b` + v + `\s*\.mkdir\(([^)]*)`).FindStringSubmatch(rest)
		return mm != nil && parents(mm[1])
	}
	if m := strVar.FindStringSubmatch(before); m != nil {
		v := regexp.QuoteMeta(m[1])
		if regexp.MustCompile(`(?:makedirs|mkdirSync|mkdirp)\(\s*`+v+`\b`).MatchString(rest) ||
			whole && regexp.MustCompile(`\bmkdir\(\s*`+v+`\b`).MatchString(rest) {
			return true
		}
		mm := regexp.MustCompile(`Path\(\s*` + v + `\s*\)\s*\.mkdir\(([^)]*)`).FindStringSubmatch(rest)
		return mm != nil && parents(mm[1])
	}
	return false
}

// commandsIn are the commands a call's input holds: a shell tool's "command", an exec tool's cmd in quotes or in a
// template literal. The input is the call as the store keeps it, JSON escaped and maybe cut.
func commandsIn(input string) []string {
	var out []string
	for _, m := range commandKey.FindAllStringIndex(input, -1) {
		closing := input[m[1]-1] // the quote or backtick that opened it
		var b strings.Builder
		for i := m[1]; i < len(input); i++ {
			c := input[i]
			if c == '\\' && i+1 < len(input) {
				b.WriteByte(c)
				b.WriteByte(input[i+1])
				i++
				continue
			}
			if c == closing {
				break
			}
			b.WriteByte(c)
		}
		out = append(out, unescapeJSON(b.String()))
	}
	return out
}

// unescapeJSON undoes the escapes JSON text uses for what a command holds.
func unescapeJSON(s string) string {
	return strings.NewReplacer(`\"`, `"`, `\n`, "\n", `\t`, "\t", `\u0026`, "&", `\u003c`, "<", `\u003e`, ">",
		`\u0027`, "'", `\\`, `\`).Replace(s)
}

var (
	// shellSep ends a simple command.
	shellSep = regexp.MustCompile("&&|\\|\\||[;|\n(){}`]|\\$\\(")
	// envAssign is an assignment of an environment variable: NAME=value.
	envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// dirVar is a variable that names a directory the program it is given to makes: HF_HOME, UV_CACHE_DIR,
	// CARGO_TARGET_DIR, OLLAMA_MODELS. TMPDIR is not one: a program fails without it.
	dirVar = regexp.MustCompile(`(?i)(^|_)(home|cache|cache_dir|out_dir|output_dir|outdir|target_dir|prefix|dest|dest_dir|save_dir|log_dir|logdir|checkpoint_dir|ckpt_dir|download_dir|local_dir|models)$`)
)

// outWords and dirWords name the long options of a directory the program makes: --prefix, --dest, --outdir;
// --output-dir, --cache-dir, --local-dir, --log-dir, --target-dir (a dirWord with dir after it). --output and
// --checkpoint alone name a file, often one to read, and make nothing.
var (
	outWords = map[string]bool{"dest": true, "destdir": true, "prefix": true, "outdir": true, "outputdir": true,
		"logdir": true, "targetdir": true, "cachedir": true, "ckptdir": true, "checkpointdir": true, "savedir": true,
		"downloaddir": true, "localdir": true}
	dirWords = map[string]bool{"out": true, "output": true, "log": true, "logging": true, "target": true,
		"checkpoint": true, "ckpt": true, "local": true, "save": true, "cache": true, "download": true, "dest": true}
)

// outputOpt reports whether a word is a long option that names a directory its program makes.
func outputOpt(w string) bool {
	name := strings.ToLower(strings.TrimLeft(w, "-"))
	if name == w {
		return false // not an option
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if outWords[p] || (dirWords[p] && i+1 < len(parts) && parts[i+1] == "dir") {
			return true
		}
	}
	return false
}

// shortOutput are the one-letter options, per program, that name a directory it makes: unzip's -d, wget's -P.
var shortOutput = map[string]map[string]bool{"unzip": {"-d": true}, "wget": {"-P": true}}

// wrappers run the command that follows them, as the shell's keywords come before one; copies make their last
// argument.
var (
	wrappers = map[string]bool{"sudo": true, "env": true, "nice": true, "ionice": true, "timeout": true, "time": true, "command": true, "exec": true, "nohup": true, "setsid": true, "stdbuf": true, "systemd-run": true, "xargs": true,
		"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true}
	copies = map[string]bool{"cp": true, "mv": true, "rsync": true, "scp": true}
)

// commandCreates reports whether a shell command creates a folder at one of its names. What it counts is a list of
// what makes a directory, so a command it does not know (a test, a listing, a read, a deletion) makes nothing:
//   - mkdir makes its arguments, and their parents with -p; install -d makes its arguments;
//   - git clone, git init and git worktree add make their directory, with its parents;
//   - cp, mv, rsync and scp make their destination, the folder itself;
//   - an option that names a directory its program makes, by its words (--output-dir, --cache-dir, --prefix) or per
//     program (unzip -d, wget -P, curl -o with --create-dirs);
//   - an assignment of a directory variable the program makes (HF_HOME=, UV_CACHE_DIR=), before the program or
//     exported.
//
// A name inside another (P/x) is the folder only for what makes parents too. mktemp's name is in its output.
func commandCreates(cmd string, spellings []string) bool {
	cmd = expandVars(cmd)
	for _, at := range occurrences(cmd, spellings) {
		start := 0
		for _, m := range shellSep.FindAllStringIndex(cmd[:at[0]], -1) {
			start = m[1]
		}
		stop := len(cmd)
		if m := shellSep.FindStringIndex(cmd[at[1]:]); m != nil {
			stop = at[1] + m[0]
		}
		words := shellWords(cmd[start:stop])
		idx := -1
		for i, w := range words {
			if w.start <= at[0]-start && at[0]-start <= w.end {
				idx = i
			}
		}
		if idx < 0 {
			continue
		}
		k := 0
		for k < len(words) && (wrappers[words[k].text] || strings.HasPrefix(words[k].text, "-") || isNumber(words[k].text) ||
			envAssign.MatchString(words[k].text)) {
			k++
		}
		prog := ""
		if k < len(words) {
			prog = filepath.Base(words[k].text)
		}
		whole := wholeName(cmd, at)
		has := func(opts ...string) bool {
			for _, w := range words[min(k+1, len(words)):] {
				if contains(opts, w.text) {
					return true
				}
			}
			return false
		}
		var sub []string // a git command's subcommand and its words, without its options
		for i := k + 1; i < idx && i < len(words); i++ {
			if w := words[i].text; prog == "git" && (w == "-C" || w == "-c") {
				i++ // the option's value
			} else if !strings.HasPrefix(w, "-") {
				sub = append(sub, w)
			}
		}
		if idx > k {
			switch {
			case prog == "mkdir" && (whole || has("-p", "--parents")):
				return true
			case prog == "install" && has("-d", "--directory"):
				return true
			case copies[prog] && whole && idx == len(words)-1:
				return true
			case prog == "git" && len(sub) > 0 && ((sub[0] == "clone" && idx == len(words)-1) || sub[0] == "init" ||
				(len(sub) > 1 && sub[0] == "worktree" && sub[1] == "add")):
				return true
			}
			prev := words[idx-1].text
			if outputOpt(prev) || shortOutput[prog][prev] || prog == "curl" && (prev == "-o" || prev == "--output") && has("--create-dirs") {
				return true
			}
		}
		if name, _, ok := strings.Cut(words[idx].text, "="); ok {
			if (idx > k && outputOpt(name)) || ((idx < k || prog == "export") && dirVar.MatchString(name)) {
				return true
			}
		}
	}
	return false
}

// shellAssign is a variable a shell command assigns: NAME=value, or export NAME=value.
var shellAssign = regexp.MustCompile(`(?:^|[\s;&|(])(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=("[^"]*"|'[^']*'|[^\s;&|)]+)`)

// expandVars puts in a command the values of the variables it assigns where it uses them after: S=P; mkdir "$S" is
// mkdir "P", and D=$HOME/x; mkdir "$D" is mkdir "$HOME/x". A value that uses its own variable stays as it is.
func expandVars(cmd string) string {
	for round := 0; round < 16; round++ {
		changed := false
		for _, m := range shellAssign.FindAllStringSubmatchIndex(cmd, -1) {
			name, val := cmd[m[2]:m[3]], strings.Trim(cmd[m[4]:m[5]], `"'`)
			if val == "" || strings.Contains(val, "$"+name) || strings.Contains(val, "${"+name+"}") {
				continue
			}
			use := regexp.MustCompile(`\$\{` + name + `\}|\$` + name + `\b`)
			if rest := cmd[m[1]:]; use.MatchString(rest) {
				cmd, changed = cmd[:m[1]]+use.ReplaceAllLiteralString(rest, val), true
				break // its places moved: find the assignments again
			}
		}
		if !changed {
			break
		}
	}
	return cmd
}

// wholeName reports whether an occurrence of a folder's name is the folder itself, not a path inside it: the name,
// with a trailing slash at most.
func wholeName(text string, at [2]int) bool {
	end := at[1]
	return end == len(text) || text[end] != '/' || end+1 == len(text) || !nameByte(text[end+1])
}

// shellWord is a word of a shell command, unquoted, and where it was.
type shellWord struct {
	text       string
	start, end int
}

// shellWords splits a simple command into its words, keeping what quotes hold together.
func shellWords(s string) []shellWord {
	var out []shellWord
	var b strings.Builder
	start, in := -1, byte(0)
	flush := func(end int) {
		if start >= 0 {
			out = append(out, shellWord{b.String(), start, end})
		}
		b.Reset()
		start = -1
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case in != 0:
			if c == in {
				in = 0
				continue
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			if start < 0 {
				start = i
			}
			in = c
		case c == ' ' || c == '\t':
			flush(i)
		default:
			if start < 0 {
				start = i
			}
			b.WriteByte(c)
		}
	}
	flush(len(s))
	return out
}

func isNumber(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// occurrences are where a text names a path in one of its spellings, as a whole name: it starts a word (the text's
// start, a space, a quote, an assignment's or a separator's end; not after a host's colon or inside another path)
// and ends at what cannot go on a name (a slash, a space, a quote, the end). A name the store cut ends at its
// ellipsis, and is not a whole name.
func occurrences(text string, spellings []string) [][2]int {
	var out [][2]int
	for _, s := range spellings {
		for i := 0; i < len(text); {
			j := strings.Index(text[i:], s)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(s)
			if (start == 0 || nameStart(text[start-1])) && (end == len(text) || !nameByte(text[end])) &&
				!strings.HasPrefix(text[end:], "…") {
				out = append(out, [2]int{start, end})
			}
			i = start + 1
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a][0] < out[b][0] })
	return out
}

// namesPath reports whether a text names a path in one of its spellings, as a whole name.
func namesPath(text string, spellings []string) bool { return len(occurrences(text, spellings)) > 0 }

// nameStart reports whether a byte may come just before a path a command names: a space, a quote, an assignment, a
// separator or a bracket.
func nameStart(b byte) bool { return strings.IndexByte(" \t\r\n=\"'`(;|&<>{[,", b) >= 0 }

// nameByte reports whether a byte may go on a file name as agents write them.
func nameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.' || b == '-' ||
		b == '@' || b == '+'
}
