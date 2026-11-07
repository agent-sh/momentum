package request

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/toolresult"
)

// writers are the file-writing tools of the harnesses the readers name, in lower case.
var writers = map[string]bool{"write": true, "edit": true, "multiedit": true, "notebookedit": true, "apply_patch": true,
	"write_file": true, "patch": true}

// shells are the harnesses' shell tools (toolresult).
func shells() []string { return toolresult.Shells() }

var (
	// git's commit line: [BRANCH HASH] SUBJECT, the branch maybe "detached HEAD", a first commit "(root-commit)"
	commitRe = regexp.MustCompile(`\[([^\[\]]+?)(?: \(root-commit\))? ([0-9a-f]{7,40})\] (.+?)(?: \d+ files? changed|$)`)
	// git push's ref updates after its To line: OLD..NEW SRC -> DST, or [new branch] SRC -> DST
	pushToRe  = regexp.MustCompile(`\bTo (\S+)`)
	pushRefRe = regexp.MustCompile(`(?:\b([0-9a-f]{7,40})\.\.\.?([0-9a-f]{7,40})|\[new branch\])\s+(\S+) -> (\S+)`)
	prRe      = regexp.MustCompile(`https?://\S+/pull/\d+`)
	pathRe    = regexp.MustCompile(`"(?:file_path|filePath|notebook_path|path)":("(?:[^"\\]|\\.)*")`)
	patchRe   = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: (\S+)`)
	// a shell command's separators: one command runs after another, or into it
	sepRe = regexp.MustCompile(`&&|\|\||[;|&\n]`)
	// a field the call's input names its command by
	cmdRe = regexp.MustCompile(`"(?:command|cmd)":\s*"((?:[^"\\]|\\.)*)`)
)

// makes is what a shell command's git and gh invocations make.
type makes struct {
	commits, pushes, prs bool
}

// commitCommands are git's commands that make commits.
var commitCommands = map[string]bool{"commit": true, "cherry-pick": true, "revert": true, "merge": true, "am": true, "rebase": true}

// Artifacts are what a call made, read off its result, and nothing else: a failed result made nothing. A
// file-writing call made the files its input names. A shell call made commits when its command ran a git command
// that makes them (the commit lines its result printed), pushes when it ran git push and no dry run (the ref
// updates after its To line), and a pull request when it ran gh pr create (the address it printed). What any
// other tool printed (an agent's report, a read, a search, a fetched page) is no artifact.
func Artifacts(call, res store.Event) []Result {
	if metaOf(res.Meta).IsError || res.Summary == "" {
		return nil
	}
	var out []Result
	add := func(r Result) {
		r.At = res.TS
		if r.Event == "" {
			r.Event = res.ID()
		}
		out = append(out, r)
	}
	tool := strings.ToLower(call.Tool)
	if writers[tool] {
		for _, p := range writtenPaths(call.Summary) {
			add(Result{Kind: KindFile, Key: KindFile + ":" + p + "@" + moment(call.TS), Text: p, Event: call.ID()})
		}
		return out
	}
	if !toolresult.IsShell(tool) {
		return nil
	}
	mk := made(command(call.Summary))
	if mk.commits {
		for _, c := range commitRe.FindAllStringSubmatch(res.Summary, -1) {
			add(Result{Kind: KindCommit, Key: KindCommit + ":" + c[2], Text: fmt.Sprintf("commit %s on %s: %s", c[2], c[1], strings.TrimSuffix(c[3], "…"))})
		}
	}
	if mk.pushes {
		if to := pushToRe.FindStringSubmatchIndex(res.Summary); to != nil {
			remote := res.Summary[to[2]:to[3]]
			for _, p := range pushRefRe.FindAllStringSubmatch(res.Summary[to[1]:], -1) {
				if p[2] != "" {
					add(Result{Kind: KindPush, Key: KindPush + ":" + remote + " " + p[4] + "@" + p[2],
						Text: fmt.Sprintf("pushed %s -> %s (%s..%s) to %s", p[3], p[4], p[1], p[2], remote)})
				} else {
					add(Result{Kind: KindPush, Key: KindPush + ":" + remote + " " + p[4] + "@new@" + moment(res.TS),
						Text: fmt.Sprintf("pushed new branch %s -> %s to %s", p[3], p[4], remote)})
				}
			}
		}
	}
	if mk.prs {
		if u := prRe.FindString(res.Summary); u != "" {
			add(Result{Kind: KindPR, Key: KindPR + ":" + u, Text: "pull request " + u})
		}
	}
	return out
}

// command is the command a shell call's input names: its command or cmd field (an array's words joined), or, when
// the store cut the input inside it, what is left of it.
func command(input string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &m) == nil {
		for _, k := range []string{"command", "cmd"} {
			raw, ok := m[k]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			var words []string
			if json.Unmarshal(raw, &words) == nil {
				return strings.Join(words, " ")
			}
		}
		return ""
	}
	if c := cmdRe.FindStringSubmatch(input); c != nil {
		if s, err := strconv.Unquote(`"` + c[1] + `"`); err == nil {
			return s
		}
		return c[1]
	}
	return ""
}

// wrappers run the command after them.
var wrappers = map[string]bool{"sudo": true, "env": true, "time": true, "nohup": true, "command": true, "exec": true, "nice": true}

// made reads what a command's git and gh invocations make: each command of the line, by the program it runs.
func made(cmd string) makes {
	var mk makes
	for _, part := range sepRe.Split(cmd, -1) {
		f := strings.Fields(part)
		i := 0
		for i < len(f) && (wrappers[f[i]] || strings.Contains(f[i], "=") && !strings.HasPrefix(f[i], "-")) {
			i++
		}
		if i >= len(f) {
			continue
		}
		switch prog := filepath.Base(f[i]); {
		case prog == "bash" || prog == "sh" || prog == "zsh":
			if i+2 <= len(f)-1 && strings.HasPrefix(f[i+1], "-") && strings.Contains(f[i+1], "c") {
				in := made(strings.Trim(strings.Join(f[i+2:], " "), `"'`))
				mk.commits, mk.pushes, mk.prs = mk.commits || in.commits, mk.pushes || in.pushes, mk.prs || in.prs
			}
		case prog == "git":
			sub, args := gitCommand(f[i+1:])
			switch {
			case commitCommands[sub]:
				mk.commits = true
			case sub == "push" && !dryRun(args):
				mk.pushes = true
			}
		case prog == "gh":
			for j := i + 1; j+1 < len(f); j++ {
				if f[j] == "pr" && f[j+1] == "create" {
					mk.prs = true
				}
			}
		}
	}
	return mk
}

// gitCommand is the command a git invocation runs, past git's own options, and its arguments.
func gitCommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "-c":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// dryRun reports a push that pushes nothing: -n, --dry-run, or -n among short options.
func dryRun(args []string) bool {
	for _, a := range args {
		if a == "--dry-run" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "n") {
			return true
		}
	}
	return false
}

// writtenPaths are the files a file-writing call names: its path field, or a patch's files.
func writtenPaths(input string) []string {
	var out []string
	for _, m := range pathRe.FindAllStringSubmatch(input, -1) {
		if p, err := strconv.Unquote(m[1]); err == nil && p != "" {
			out = append(out, p)
		}
	}
	if len(out) > 0 {
		return out[:1]
	}
	for _, m := range patchRe.FindAllStringSubmatch(input, -1) {
		out = append(out, m[1])
	}
	return out
}
