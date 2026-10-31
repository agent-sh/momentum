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

// resultCreates reports whether a call's output says it created a folder: a writer that made directories, or the
// path mktemp printed.
func resultCreates(result, call string, spellings []string) bool {
	text := unescapeJSON(result)
	if !namesPath(text, spellings) {
		return false
	}
	return strings.Contains(text, `"dirs_created": true`) || strings.Contains(text, `"dirs_created":true`) ||
		strings.Contains(unescapeJSON(call), "mktemp")
}

// writeTools are the tools that write a file at a path they are given, making its directories.
var writeTools = map[string]bool{"Write": true, "write_file": true, "create_file": true}

var (
	pathField  = regexp.MustCompile(`"(?:file_path|path)"\s*:\s*"$`)
	scriptMade = regexp.MustCompile(`mkdir\(|makedirs\(|mkdirSync\(|mkdirp|\.mkdir\(`)
	commandKey = regexp.MustCompile(`"(?:command|cmd)"\s*:\s*"|[^A-Za-z_"]cmd\s*:\s*"`)
)

// callCreates reports whether a call's input creates a folder: a write tool's path in it, a patch's Add File in it,
// or a command that makes it.
func callCreates(tool, input string, spellings []string) bool {
	text := unescapeJSON(input)
	for _, at := range occurrences(text, spellings) {
		before := text[:at[0]]
		if (writeTools[tool] && pathField.MatchString(before)) || strings.HasSuffix(before, "Add File: ") {
			return true
		}
	}
	script := scriptMade.MatchString(text)
	for _, c := range commandsIn(input) {
		if commandCreates(c, spellings, script) {
			return true
		}
	}
	return false
}

// commandsIn are the commands a call's input holds: a shell tool's "command", an exec tool's cmd. The input is the
// call as the store keeps it, JSON escaped and maybe cut.
func commandsIn(input string) []string {
	var out []string
	for _, m := range commandKey.FindAllStringIndex(input, -1) {
		var b strings.Builder
		for i := m[1]; i < len(input); i++ {
			c := input[i]
			if c == '\\' && i+1 < len(input) {
				b.WriteByte(c)
				b.WriteByte(input[i+1])
				i++
				continue
			}
			if c == '"' {
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
	// outputOpt is an option that names where a program writes: its output, a directory, a cache, a prefix.
	outputOpt = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(out|dir|dest|save|cache|home|prefix|root|work|target|log|ckpt|checkpoint|tmp)[a-z0-9_-]*$`)
	// dirVar is a variable that names a directory a program writes to: HF_HOME, TMPDIR, UV_CACHE_DIR, OUT.
	dirVar = regexp.MustCompile(`(?i)(^|_)(dir|home|cache|out|output|root|prefix|tmp|tmpdir|temp|models|dest|work|workdir|log|logs|state|data)$`)
)

// shortOutput are the one-letter options that name where a program writes: -o, curl's and many others' output; -d,
// unzip's directory; -C, tar's (git's and make's is a cd); -P, wget's prefix; -O.
var shortOutput = map[string]bool{"-o": true, "-d": true, "-C": true, "-P": true, "-O": true}

// wrappers run the command that follows them; deletions and cd make nothing of their arguments.
var (
	wrappers  = map[string]bool{"sudo": true, "env": true, "nice": true, "ionice": true, "timeout": true, "time": true, "command": true, "exec": true, "nohup": true, "setsid": true, "stdbuf": true, "systemd-run": true, "xargs": true}
	deletions = map[string]bool{"rm": true, "rmdir": true, "unlink": true, "trash": true, "trash-put": true, "shred": true, "evidence-offload": true, "cd": true}
	copies    = map[string]bool{"cp": true, "mv": true, "rsync": true, "scp": true}
)

// commandCreates reports whether a shell command creates a folder at one of its names. script says whether the call
// makes directories in a script, where a quoted path is what it makes.
func commandCreates(cmd string, spellings []string, script bool) bool {
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
		k := 0
		for k < len(words) && (wrappers[words[k].text] || strings.HasPrefix(words[k].text, "-") || isNumber(words[k].text) ||
			strings.Contains(words[k].text, "=")) {
			k++
		}
		prog := ""
		if k < len(words) {
			prog = filepath.Base(words[k].text)
		}
		if deletions[prog] {
			continue
		}
		last := idx == len(words)-1
		var sub []string // a git command's subcommand and its words, without its options
		for i := k + 1; i < idx && i < len(words); i++ {
			if w := words[i].text; prog == "git" && (w == "-C" || w == "-c") {
				i++ // the option's value
			} else if !strings.HasPrefix(w, "-") {
				sub = append(sub, w)
			}
		}
		switch {
		case prog == "mkdir" || prog == "mktemp":
			return true
		case copies[prog] && last:
			return true
		case prog == "git" && len(sub) > 0 && ((sub[0] == "clone" && last) || sub[0] == "init" ||
			(len(sub) > 1 && sub[0] == "worktree" && sub[1] == "add")):
			return true
		}
		if idx > 0 {
			prev := words[idx-1].text
			cd := prev == "-C" && (prog == "git" || prog == "make") // git's and make's -C is a cd
			if outputOpt.MatchString(prev) || shortOutput[prev] && !cd {
				return true
			}
		}
		if idx >= 0 {
			if name, _, ok := strings.Cut(words[idx].text, "="); ok && (outputOpt.MatchString(name) || dirVar.MatchString(name)) {
				return true
			}
		}
		if script && at[0] > 0 && (cmd[at[0]-1] == '\'' || cmd[at[0]-1] == '"') {
			return true
		}
	}
	return false
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

// occurrences are where a text names a path in one of its spellings, as a whole name: the spelling, then what cannot
// go on a name (a slash, a space, a quote, the end). A name the store cut ends at its ellipsis, and is not a whole
// name.
func occurrences(text string, spellings []string) [][2]int {
	var out [][2]int
	for _, s := range spellings {
		for i := 0; i < len(text); {
			j := strings.Index(text[i:], s)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(s)
			if (end == len(text) || !nameByte(text[end])) && !strings.HasPrefix(text[end:], "…") {
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

// nameByte reports whether a byte may go on a file name as agents write them.
func nameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.' || b == '-' ||
		b == '@' || b == '+'
}
