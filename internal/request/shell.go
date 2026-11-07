package request

import (
	"errors"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// shellProgs run the script their -c option names.
var shellProgs = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true}

// invocation is a simple command: the program it runs and its arguments, past assignments and the wrappers that
// run the command after them. proven is true when the whole call's success proves this command succeeded; dir is
// the directory it runs in, when the line says (a cd before it, git -C).
type invocation struct {
	prog   string
	args   []string
	proven bool
	dir    string
}

// invocations are every simple command a shell script runs, parsed as a shell parses it: in lists, pipelines,
// subshells, loops' and conditions' bodies, command substitutions, and the scripts a shell's -c runs. A script the
// store cut is parsed as far as it goes: with the quotes or parentheses it leaves open closed, else to the last
// whole command before the cut; then no command of it is proven, since what the cut took may have run after.
func invocations(script string, depth int) []invocation {
	if depth > 4 {
		return nil
	}
	cut := strings.HasSuffix(script, "…")
	f, whole := parseCut(strings.TrimSuffix(script, "…"))
	if f == nil {
		return nil
	}
	w := &walker{depth: depth}
	w.stmts(f.Stmts, !cut && whole)
	return w.out
}

// walker reads a parsed script's simple commands in order, and which of them the script's success proves: the
// exit of a list is its last statement's, of a && its two sides' (the second runs only after the first succeeded),
// of a pipeline its last command's. Nothing after ||, in a condition or a loop, in a substitution, run in the
// background or negated is proven.
type walker struct {
	out   []invocation
	dir   string
	depth int
}

func (w *walker) stmts(list []*syntax.Stmt, proven bool) {
	for i, st := range list {
		w.stmt(st, proven && i == len(list)-1)
	}
}

func (w *walker) stmt(st *syntax.Stmt, proven bool) {
	if st == nil {
		return
	}
	if st.Negated || st.Background || st.Coprocess {
		proven = false
	}
	w.cmd(st.Cmd, proven)
	for _, r := range st.Redirs {
		w.words(false, r.Word, r.Hdoc)
	}
}

func (w *walker) cmd(c syntax.Command, proven bool) {
	switch c := c.(type) {
	case *syntax.CallExpr:
		w.call(c, proven)
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.AndStmt:
			w.stmt(c.X, proven)
			w.stmt(c.Y, proven)
		case syntax.OrStmt:
			w.stmt(c.X, false)
			w.stmt(c.Y, false)
		default: // a pipeline: its last command's exit
			w.stmt(c.X, false)
			w.stmt(c.Y, proven)
		}
	case *syntax.Subshell:
		dir := w.dir
		w.stmts(c.Stmts, proven)
		w.dir = dir
	case *syntax.Block:
		w.stmts(c.Stmts, proven)
	case *syntax.IfClause:
		for ic := c; ic != nil; ic = ic.Else {
			w.stmts(ic.Cond, false)
			w.stmts(ic.Then, false)
		}
	case *syntax.WhileClause:
		w.stmts(c.Cond, false)
		w.stmts(c.Do, false)
	case *syntax.ForClause:
		w.stmts(c.Do, false)
	case *syntax.CaseClause:
		for _, it := range c.Items {
			w.stmts(it.Stmts, false)
		}
	case *syntax.TimeClause:
		w.stmt(c.Stmt, proven)
	case *syntax.FuncDecl:
		w.stmt(c.Body, false)
	case *syntax.DeclClause:
		for _, a := range c.Args {
			w.words(false, a.Value)
		}
	}
}

// call reads a simple command, then the commands its words substitute. A command of assignments alone exits as
// its last substitution did, so that one is proven with it (N=$(gh pr create ...)).
func (w *walker) call(c *syntax.CallExpr, proven bool) {
	if len(c.Args) == 0 {
		var last *syntax.CmdSubst
		for _, a := range c.Assigns {
			if a.Value != nil {
				syntax.Walk(a.Value, func(n syntax.Node) bool {
					if cs, ok := n.(*syntax.CmdSubst); ok {
						last = cs
						return false
					}
					return true
				})
			}
		}
		for _, a := range c.Assigns {
			if a.Value == nil {
				continue
			}
			syntax.Walk(a.Value, func(n syntax.Node) bool {
				switch n := n.(type) {
				case *syntax.CmdSubst:
					w.stmts(n.Stmts, proven && n == last)
					return false
				case *syntax.ProcSubst:
					w.stmts(n.Stmts, false)
					return false
				}
				return true
			})
		}
		return
	}
	args := make([]string, len(c.Args))
	for i, wd := range c.Args {
		args[i] = wordText(wd)
	}
	if inv, ok := unwrap(args); ok {
		if shellProgs[inv.prog] {
			if s, ok := shellScript(inv.args); ok {
				in := invocations(s, w.depth+1)
				for i := range in {
					in[i].proven = in[i].proven && proven
					if in[i].dir == "" {
						in[i].dir = w.dir
					}
				}
				w.out = append(w.out, in...)
				return
			}
		}
		inv.proven, inv.dir = proven, w.dir
		switch inv.prog {
		case "cd":
			if d := firstArg(inv.args); d != "" && !strings.ContainsRune(d, 0) {
				w.dir = joinDir(w.dir, d)
			}
		case "git":
			if d := gitDir(inv.args); d != "" {
				inv.dir = joinDir(w.dir, d)
			}
		}
		w.out = append(w.out, inv)
	}
	for _, a := range c.Assigns {
		w.words(false, a.Value)
	}
	w.words(false, c.Args...)
}

// words reads the commands the words substitute.
func (w *walker) words(proven bool, ws ...*syntax.Word) {
	for _, wd := range ws {
		if wd == nil {
			continue
		}
		syntax.Walk(wd, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.CmdSubst:
				w.stmts(n.Stmts, proven)
				return false
			case *syntax.ProcSubst:
				w.stmts(n.Stmts, false)
				return false
			}
			return true
		})
	}
}

// joinDir is dir d taken from dir: an absolute or home-relative d as it is, a relative one under dir.
func joinDir(dir, d string) string {
	if dir == "" || strings.HasPrefix(d, "/") || strings.HasPrefix(d, "~") {
		return d
	}
	return filepath.Join(dir, d)
}

// gitDir is the directory a git invocation's -C option names.
func gitDir(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" {
			return args[i+1]
		}
		if !strings.HasPrefix(args[i], "-") {
			break
		}
	}
	return ""
}

// closers end what a cut script leaves open.
var closers = []string{"", "'", `"`, "`", `"'`, `'"`, ")", "')", `")`, "))", "'))", `"))`, "\n}", "\nfi", "\ndone"}

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
	"timeout": {"-s": true, "--signal": true, "-k": true, "--kill-after": true},
	"env":     {"-u": true, "--unset": true, "-C": true, "--chdir": true, "-S": true, "--split-string": true},
	"nice":    {"-n": true, "--adjustment": true},
	"nohup":   {},
	"command": {},
	"exec":    {"-a": true},
	"time":    {"-f": true, "--format": true, "-o": true, "--output": true},
	"sudo":    {"-u": true, "--user": true, "-g": true, "--group": true, "-C": true, "-D": true, "--chdir": true, "-h": true, "-p": true, "--prompt": true, "-r": true, "-t": true, "-U": true, "-T": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
}

// unwrap is the program a command runs past the wrappers that run the command after them (timeout and its
// duration, env and its assignments, nice, nohup, command, exec, time, sudo, stdbuf), with its arguments. ok is false
// for a command that runs no program by name: an expansion, or a wrapper with nothing after it, or command -v's
// lookup.
func unwrap(args []string) (invocation, bool) {
	for len(args) > 0 {
		prog := filepath.Base(args[0])
		valued, isWrapper := wrapperOptions[prog]
		if !isWrapper {
			if strings.ContainsRune(args[0], 0) {
				return invocation{}, false
			}
			return invocation{prog: prog, args: args[1:]}, true
		}
		i := 1
		for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
			a := args[i]
			if a == "--" {
				i++
				break
			}
			if prog == "command" && (a == "-v" || a == "-V") {
				return invocation{}, false
			}
			if prog == "nice" && len(a) > 1 && a[1] >= '0' && a[1] <= '9' {
				i++
				continue
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
	// quiet); remote is the first push's remote argument
	fetches bool
	remote  string
	// madeCommit, madePush and madePR are a commit, push or pull request the call's success proves it made, whatever
	// its output shows; commitDir is where the commit was made, when the line says
	madeCommit, madePush, madePR bool
	commitDir                    string
}

// commitCommands are git's commands that make commits.
var commitCommands = map[string]bool{"commit": true, "cherry-pick": true, "revert": true, "merge": true, "am": true, "rebase": true}

// made reads what a call's commands make, by every program they run (and which of them the call's success proves
// made something): commits when one runs a git command that
// makes them (no dry run, no --no-commit, no --abort), pushes when one runs git push and none of its pushes is a
// dry run (whose ref lines would be read as pushed), and a pull request when one runs gh pr create without
// --dry-run.
func made(commands []string) makes {
	var mk makes
	dry := false
	for _, c := range commands {
		for _, inv := range invocations(c, 0) {
			switch inv.prog {
			case "git":
				sub, args := gitCommand(inv.args)
				switch {
				case commitCommands[sub] && !makesNoCommit(sub, args):
					mk.commits = true
					if inv.proven && !mk.madeCommit {
						mk.madeCommit, mk.commitDir = true, inv.dir
					}
				case sub == "push":
					mk.pushes = true
					dry = dry || dryRun(args)
					if mk.remote == "" {
						mk.remote = firstArg(args)
					}
					mk.madePush = mk.madePush || inv.proven
				case (sub == "fetch" || sub == "pull" || sub == "remote") && !quiet(args):
					mk.fetches = true
				}
			case "gh":
				for j := 0; j+1 < len(inv.args); j++ {
					if inv.args[j] == "pr" && inv.args[j+1] == "create" && !hasFlag(inv.args[j+2:], "--dry-run") {
						mk.prs = true
						mk.madePR = mk.madePR || inv.proven
					}
				}
			}
		}
	}
	mk.pushes = mk.pushes && !dry
	mk.madePush = mk.madePush && mk.pushes
	return mk
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

// firstArg is the first argument that is no option.
func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// gitCommand is the command a git invocation runs, past git's own options, and its arguments.
func gitCommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
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

// dryRun reports a push that pushes nothing: -n, --dry-run, or -n among short options.
func dryRun(args []string) bool {
	for _, a := range args {
		if a == "--dry-run" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "n") {
			return true
		}
	}
	return false
}
