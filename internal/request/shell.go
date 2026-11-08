package request

import (
	"errors"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// shellProgs run the script their -c option names.
var shellProgs = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true}

// invocation is a simple command: the program it runs and its arguments, past assignments and the wrappers that
// run the command after them. silent is true when its output goes to a file (a redirect of it, or of a command
// around it), so none of it is in the call's output. cut is true when it ends where the store cut the script: what
// the cut took may have been more of its arguments.
type invocation struct {
	prog   string
	args   []string
	silent bool
	cut    bool
}

// invocations are every simple command a shell script runs, parsed as a shell parses it: in lists, pipelines,
// subshells, loops' and conditions' bodies, functions, command substitutions, and the scripts a shell's -c runs. A
// script the store cut is parsed as far as it goes: with the quotes or parentheses it leaves open closed, else to
// the last whole command before the cut.
func invocations(script string, depth int) []invocation { return invocationsOf(script, depth, false) }

// invocationsOf are a script's invocations, those that end where the store cut it marked cut when cut is true.
func invocationsOf(script string, depth int, cut bool) []invocation {
	if depth > 4 {
		return nil
	}
	script = strings.TrimSuffix(script, "…")
	f, _ := parseCut(script)
	if f == nil {
		return nil
	}
	w := &walker{depth: depth, cut: cut, size: len(strings.TrimRight(script, " \t\n"))}
	syntax.Walk(f, w.visit)
	return w.out
}

// walker collects a parsed script's simple commands in order.
type walker struct {
	out    []invocation
	depth  int
	silent bool
	cut    bool // the store cut the script at size
	size   int
}

// toFile reports a redirect that sends standard output to a file: >, >>, >|, &> or &>> on it, to anything but
// /dev/stdout.
func toFile(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.ClbOut:
		if r.N != nil && r.N.Value != "1" {
			return false
		}
	case syntax.RdrAll, syntax.AppAll:
	default:
		return false
	}
	return wordText(r.Word) != "/dev/stdout"
}

func (w *walker) visit(n syntax.Node) bool {
	if st, ok := n.(*syntax.Stmt); ok && !w.silent && st.Cmd != nil {
		for _, r := range st.Redirs {
			if toFile(r) {
				in := &walker{depth: w.depth, silent: true, cut: w.cut, size: w.size}
				syntax.Walk(st.Cmd, in.visit)
				w.out = append(w.out, in.out...)
				return false
			}
		}
	}
	ce, ok := n.(*syntax.CallExpr)
	if !ok || len(ce.Args) == 0 {
		return true
	}
	args := make([]string, len(ce.Args))
	for i, wd := range ce.Args {
		args[i] = wordText(wd)
	}
	cut := w.cut && int(ce.End().Offset()) >= w.size
	if inv, ok := unwrap(args); ok {
		if shellProgs[inv.prog] {
			if s, ok := shellScript(inv.args); ok {
				for _, in := range invocationsOf(s, w.depth+1, cut) {
					in.silent = in.silent || w.silent
					w.out = append(w.out, in)
				}
				return true
			}
		}
		inv.silent, inv.cut = w.silent, cut
		w.out = append(w.out, inv)
	}
	return true
}

// closers end what a cut script leaves open.
var closers = []string{"", "'", `"`, ")", "\n}", "\ndone"}

// parseCut parses a script, closing what its cut leaves open, or else the part of it before the cut that parses.
// whole is false when it took anything but the script as it is.
func parseCut(script string) (f *syntax.File, whole bool) {
	parse := func(s string) (*syntax.File, error) {
		return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(s), "")
	}
	for i, c := range closers {
		if f, err := parse(script + c); err == nil {
			return f, i == 0
		}
	}
	s := script
	for len(s) > 0 {
		_, err := parse(s)
		var pe syntax.ParseError
		if err == nil {
			break
		}
		end := len(s)
		if errors.As(err, &pe) && int(pe.Pos.Offset()) < end {
			end = int(pe.Pos.Offset())
		}
		cut := strings.LastIndexAny(s[:end], ";&|\n")
		if cut <= 0 {
			return nil, false
		}
		s = strings.TrimRight(s[:cut], ";&| \n")
	}
	if s == "" {
		return nil, false
	}
	f, _ = parse(s)
	return f, false
}

// wordText is a word's text as the program gets it, where the shell can tell without running anything: its
// literal, quoted and escaped parts; an expansion stands as \x00.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	var parts func([]syntax.WordPart)
	parts = func(ps []syntax.WordPart) {
		for _, p := range ps {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				parts(p.Parts)
			default:
				b.WriteByte(0)
			}
		}
	}
	parts(w.Parts)
	return unescape(b.String())
}

// unescape takes the backslashes out of an unquoted literal's escapes.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// shellScript is the script a shell's -c option names.
func shellScript(args []string) (string, bool) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			return "", false
		}
		if strings.Contains(a[1:], "c") && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// wrapperOptions are each wrapper's options that take a value as the next word.
var wrapperOptions = map[string]map[string]bool{
	"timeout": {"-s": true, "-k": true},
	"env":     {"-u": true},
	"nice":    {"-n": true},
	"nohup":   {},
	"command": {},
	"exec":    {"-a": true},
	"time":    {"-f": true, "-o": true},
	"sudo":    {},
	"stdbuf":  {},
}

// unwrap is the program a command runs past the wrappers that run the command after them (timeout and its
// duration, env and its assignments, nice, nohup, command, exec, time, sudo, stdbuf), with its arguments. ok is false
// for a wrapper with nothing after it, or command -v's lookup.
func unwrap(args []string) (invocation, bool) {
	for len(args) > 0 {
		prog := filepath.Base(args[0])
		valued, isWrapper := wrapperOptions[prog]
		if !isWrapper {
			return invocation{prog: prog, args: args[1:]}, true // an expansion's program (\x00) is no program read here
		}
		i := 1
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			a := args[i]
			if prog == "command" && a == "-v" {
				return invocation{}, false
			}
			if valued[a] {
				i++
			}
			i++
		}
		if prog == "env" {
			for i < len(args) && strings.Contains(args[i], "=") {
				i++
			}
		}
		if prog == "timeout" && i < len(args) {
			i++ // the duration
		}
		args = args[min(i, len(args)):] // a valued option may be the last word
	}
	return invocation{}, false
}

// makes is what a shell call's commands make.
type makes struct {
	commits, pushes, prs bool
	// fetches is a git command that prints ref updates as a push does (a fetch, pull or remote update that is not
	// quiet); quiet is a call whose every push is quiet, so it printed no ref line; remote is the first push's
	// remote argument
	fetches, quiet bool
	remote         string
}

// commitCommands are git's commands that make commits.
var commitCommands = map[string]bool{"commit": true, "cherry-pick": true, "revert": true, "merge": true, "am": true, "rebase": true}

// made reads what a call's commands make, by every program they run: commits when one runs a git command that
// makes them (no dry run, no --no-commit, no --abort) and prints its commit line (not quiet, its output not sent to
// a file), so a commit line in an output where none does is another command's (a cat, a grep); pushes when one
// runs git push and none of its pushes is a dry run (whose ref lines would be read as pushed); and pull requests
// when one runs gh pr create without --dry-run and none runs another gh command that prints addresses (gh api, gh
// search, gh pr view, list, edit, comment, ...). A command that ends where the store cut the call's input makes nothing: the cut may have taken
// an option that changes what it does (--dry-run, --no-commit, -n).
func made(d Decoded) makes {
	var mk makes
	dry, lists, loud := false, false, false
	for i, c := range d.Commands {
		for _, inv := range invocationsOf(c, 0, d.CommandCut && i == len(d.Commands)-1) {
			if inv.prog == "gh" && printsAddresses(ghCommand(inv.args)) {
				lists = true
			}
			if inv.cut {
				continue
			}
			switch inv.prog {
			case "git":
				sub, args := gitCommand(inv.args)
				switch {
				case commitCommands[sub] && !makesNoCommit(sub, args):
					mk.commits = mk.commits || !quiet(args) && !inv.silent
				case sub == "push":
					mk.pushes = true
					dry = dry || dryRun(args)
					loud = loud || !quiet(args)
					if mk.remote == "" {
						mk.remote = pushRemote(args)
					}
				case (sub == "fetch" || sub == "pull" || sub == "remote" && len(args) > 0 && args[0] == "update") && !quiet(args):
					mk.fetches = true
				}
			case "gh":
				if cmd := ghCommand(inv.args); len(cmd) >= 2 && cmd[0] == "pr" && cmd[1] == "create" && !hasFlag(cmd[2:], "--dry-run") {
					mk.prs = true
				}
			}
		}
	}
	mk.pushes = mk.pushes && !dry
	mk.quiet = mk.pushes && !loud
	mk.prs = mk.prs && !lists
	return mk
}

// ghGlobal are gh's flags that take a value as the next word, which may come before its command.
var ghGlobal = map[string]bool{"-R": true, "--repo": true, "--hostname": true}

// ghCommand is a gh invocation's command and its arguments, past the flags before the command (gh -R o/r pr list
// is pr list).
func ghCommand(args []string) []string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case ghGlobal[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return args[i:]
		}
	}
	return nil
}

// printsAddresses reports a gh command that prints pull requests' addresses other than one it made: gh api, gh
// search, and every gh pr command but create (view, list, edit, comment, ready, ...).
func printsAddresses(cmd []string) bool {
	return len(cmd) > 0 && (cmd[0] == "api" || cmd[0] == "search") || len(cmd) > 1 && cmd[0] == "pr" && cmd[1] != "create"
}

// makesNoCommit reports a git command that makes commits run so it makes none: a dry run, --no-commit (merge,
// cherry-pick, revert; -n for the last two), or an --abort or --quit of one under way.
func makesNoCommit(sub string, args []string) bool {
	switch {
	case hasFlag(args, "--abort") || hasFlag(args, "--quit"):
		return true
	case sub == "commit":
		return hasFlag(args, "--dry-run")
	case sub == "merge":
		return hasFlag(args, "--no-commit")
	case sub == "cherry-pick" || sub == "revert":
		return hasFlag(args, "--no-commit") || hasFlag(args, "-n")
	}
	return false
}

// quiet reports a git command told to print nothing but errors: -q, --quiet, or q among short options.
func quiet(args []string) bool {
	for _, a := range args {
		if a == "--quiet" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "q") {
			return true
		}
	}
	return false
}

// pushRemote is a push's remote: its first argument that is no option nor an option's value (-o, --push-option).
func pushRemote(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-o" || a == "--push-option":
			i++
		case !strings.HasPrefix(a, "-"):
			return a
		}
	}
	return ""
}

// gitCommand is the command a git invocation runs, past git's own options, and its arguments.
func gitCommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "-c" || a == "--git-dir":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// dryRun reports a push that pushes nothing, and prints the ref lines a push does: --dry-run, or -n alone or among
// short options (-nv, -un, -fn).
func dryRun(args []string) bool {
	for _, a := range args {
		if a == "--dry-run" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "n") {
			return true
		}
	}
	return false
}
