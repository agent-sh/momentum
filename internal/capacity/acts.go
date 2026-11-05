package capacity

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ActsIn reports whether an agent's tool call acts in a work tree: its directory is inside the tree; it writes or
// edits a file under it (a write tool's path, a patch's file); or a command of it does: a git command that writes
// (commit, checkout, reset, stash, worktree add, pull, merge, rebase and the like) run in the tree or pointed into it
// with -C, or any other program run in the tree or given a path in it (a build, a test, a copy, a deletion). Reads
// do not act: a read, search or web tool, gh, cat, grep, journalctl, git log, status, diff and show, whatever they
// name. A call it cannot read acts when it names the tree's path; the repository's name alone is not a path. input
// is the call as the store keeps it (JSON, maybe cut), cwd the directory it ran in, tree and home absolute.
func ActsIn(tool, input, cwd, tree, home string) bool {
	tree = filepath.Clean(tree)
	if cwd != "" && under(filepath.Clean(cwd), tree) {
		return true
	}
	text := unescapeJSON(input)
	mentions := namesPath(text, treeSpellings(tree, home))
	if readTools[tool] {
		return false
	}
	if patchActs(text, cwd, tree, home) {
		return true
	}
	if editTools[tool] {
		if p, ok := pathArg(input); ok {
			return under(resolvePath(p, cwd, home), tree)
		}
		return mentions // its path was cut off
	}
	cmds := commandsIn(input)
	if len(cmds) == 0 {
		return mentions // a call it cannot read
	}
	cut := strings.HasSuffix(strings.TrimSpace(input), "…")
	for i, c := range cmds {
		if commandActs(c, cwd, tree, home, cut && i == len(cmds)-1, 0) {
			return true
		}
	}
	return false
}

// treeSpellings are the ways a call names the tree by its path: in full, and from home.
func treeSpellings(tree, home string) []string {
	out := []string{tree}
	if rel, ok := strings.CutPrefix(tree, home+"/"); ok && home != "" {
		out = append(out, "~/"+rel, "$HOME/"+rel)
	}
	return out
}

// plainArgs are a command's arguments that are not options or an option's value (valued names the options that
// take one).
func plainArgs(args []string, valued ...string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case contains(valued, a):
			i++
		case !strings.HasPrefix(a, "-"):
			out = append(out, a)
		}
	}
	return out
}

// readTools are the tools that only read, search, fetch or talk: what they name, they do not change.
var readTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "LS": true, "NotebookRead": true, "WebFetch": true, "WebSearch": true,
	"ToolSearch": true, "Agent": true, "Task": true, "SendMessage": true, "TodoWrite": true, "TaskStop": true,
	"TaskOutput": true, "BashOutput": true, "KillShell": true, "ScheduleWakeup": true, "Skill": true, "ExitPlanMode": true,
	"read_file": true, "search_files": true, "web_search": true, "web_extract": true, "skill_view": true,
	"read": true, "grep": true, "glob": true, "list": true, "webfetch": true, "websearch": true, "todowrite": true,
	"todoread": true, "task": true, "ReadFile": true, "ReadMediaFile": true, "FetchURL": true, "SearchWeb": true,
	"TodoList": true, "view_image": true,
}

// editTools write or edit a file at the path they are given.
var editTools = map[string]bool{
	"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true, "write_file": true, "create_file": true,
	"patch": true, "edit": true, "write": true, "multiedit": true, "apply_patch": true, "WriteFile": true,
	"StrReplaceFile": true, "str_replace_based_edit_tool": true,
}

var (
	// pathKey is a write tool's path field, in the call's JSON.
	pathKey = regexp.MustCompile(`"(?:file_path|filePath|notebook_path|path|filename)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	// patchFile is a file a patch changes: apply_patch's headers and a unified diff's target.
	patchFile = regexp.MustCompile(`(?m)(?:\*\*\* (?:Add|Update|Delete) File: |^\+\+\+ (?:b/)?)([^\s"]+)`)
)

// pathArg is the path a write tool's call names, unescaped.
func pathArg(input string) (string, bool) {
	m := pathKey.FindStringSubmatch(input)
	if m == nil {
		return "", false
	}
	return unescapeJSON(m[1]), true
}

// patchActs reports whether a patch in the call changes a file under the tree.
func patchActs(text, cwd, tree, home string) bool {
	for _, m := range patchFile.FindAllStringSubmatch(text, -1) {
		if under(resolvePath(m[1], cwd, home), tree) {
			return true
		}
	}
	return false
}

// resolvePath is p as an absolute path: ~ and $HOME are home, a relative path is under dir. A relative path with no
// dir to resolve it in is "".
func resolvePath(p, dir, home string) string {
	switch {
	case p == "~" || p == "$HOME" || p == "${HOME}":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(home, p[6:])
	case strings.HasPrefix(p, "${HOME}/"):
		return filepath.Join(home, p[8:])
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	case dir == "":
		return ""
	}
	return filepath.Join(dir, p)
}

// readProgs are programs that only read what they are given, unless an option says otherwise (readOnly). Their
// redirections still count.
var readProgs = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true, "bat": true, "grep": true, "egrep": true,
	"fgrep": true, "rg": true, "ag": true, "ack": true, "ls": true, "eza": true, "exa": true, "find": true, "fd": true,
	"wc": true, "stat": true, "file": true, "du": true, "df": true, "tree": true, "jq": true, "yq": true, "diff": true,
	"cmp": true, "comm": true, "sha1sum": true, "sha256sum": true, "sha512sum": true, "md5sum": true, "b2sum": true,
	"cksum": true, "echo": true, "printf": true, "test": true, "[": true, "[[": true, "readlink": true, "realpath": true,
	"basename": true, "dirname": true, "sort": true, "uniq": true, "cut": true, "tr": true, "column": true, "nl": true,
	"fold": true, "od": true, "xxd": true, "hexdump": true, "strings": true, "awk": true, "gawk": true, "sed": true,
	"journalctl": true, "ps": true, "pgrep": true, "pidof": true, "free": true, "uptime": true, "nproc": true,
	"lscpu": true, "lsblk": true, "date": true, "sleep": true, "true": true, "false": true, ":": true, "which": true,
	"whereis": true, "type": true, "pwd": true, "id": true, "whoami": true, "hostname": true, "uname": true,
	"curl": true, "ssh": true, "printenv": true, "export": true, "set": true, "unset": true, "local": true,
	"declare": true, "shopt": true, "alias": true, "exit": true, "return": true, "man": true, "ldd": true, "nm": true,
	"objdump": true, "readelf": true, "lsof": true, "ss": true, "systemctl": true, "loginctl": true, "fi": true,
	"done": true, "esac": true, "wait": true, "go": true,
}

// readOnly reports whether a program reads only, with its arguments: sed and awk not in place, find without an
// action that deletes or runs, curl and sort with no output file, go's doc, env, list and version.
func readOnly(prog string, args []string) bool {
	if !readProgs[prog] {
		return false
	}
	has := func(f func(string) bool) bool {
		for _, a := range args {
			if f(a) {
				return true
			}
		}
		return false
	}
	short := func(a string, flags string) bool {
		return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], flags)
	}
	switch prog {
	case "sed":
		return !has(func(a string) bool { return strings.HasPrefix(a, "--in-place") || short(a, "i") })
	case "awk", "gawk":
		return !has(func(a string) bool { return a == "-i" || strings.HasPrefix(a, "--include") })
	case "find":
		return !has(func(a string) bool {
			return a == "-delete" || a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" || strings.HasPrefix(a, "-fprint") || a == "-fls"
		})
	case "curl":
		return !has(func(a string) bool {
			return a == "--output" || a == "--remote-name" || a == "--remote-name-all" || a == "--dump-header" || strings.HasPrefix(a, "--output=") || short(a, "oOD")
		})
	case "sort":
		return !has(func(a string) bool { return a == "--output" || strings.HasPrefix(a, "--output=") || short(a, "o") })
	case "go":
		return len(args) > 0 && (args[0] == "doc" || args[0] == "env" || args[0] == "list" || args[0] == "version" || args[0] == "help")
	}
	return true
}

// writeProgs change the paths they are given: a bare relative name in their arguments counts too.
var writeProgs = map[string]bool{
	"rm": true, "rmdir": true, "mv": true, "cp": true, "rsync": true, "install": true, "mkdir": true, "touch": true,
	"ln": true, "chmod": true, "chown": true, "truncate": true, "tee": true, "patch": true, "tar": true, "unzip": true,
	"gofmt": true, "goimports": true, "shred": true, "dd": true,
}

// gitReads are git's subcommands that change nothing in a work tree, its index or HEAD. A fetch writes only remote
// refs.
var gitReads = map[string]bool{
	"log": true, "status": true, "diff": true, "show": true, "blame": true, "annotate": true, "grep": true,
	"ls-files": true, "ls-tree": true, "ls-remote": true, "rev-parse": true, "rev-list": true, "describe": true,
	"shortlog": true, "cat-file": true, "for-each-ref": true, "merge-base": true, "name-rev": true, "whatchanged": true,
	"help": true, "version": true, "count-objects": true, "fsck": true, "check-ignore": true, "check-attr": true,
	"show-ref": true, "var": true, "range-diff": true, "cherry": true, "verify-commit": true, "verify-tag": true,
	"fetch": true, "diff-tree": true, "diff-files": true, "diff-index": true, "show-branch": true,
}

// gitRead reports whether a git subcommand with its arguments reads only. One it does not know writes.
func gitRead(sub string, rest []string) bool {
	if gitReads[sub] {
		return true
	}
	var plain []string
	opt := func(names ...string) bool {
		for _, a := range rest {
			if contains(names, a) {
				return true
			}
		}
		return false
	}
	for _, a := range rest {
		if !strings.HasPrefix(a, "-") {
			plain = append(plain, a)
		}
	}
	first := ""
	if len(plain) > 0 {
		first = plain[0]
	}
	switch sub {
	case "stash", "notes":
		return first == "list" || first == "show"
	case "worktree":
		return first == "list"
	case "reflog":
		return first == "" || first == "show"
	case "remote":
		return first == "" || first == "show" || first == "get-url"
	case "branch":
		return len(plain) == 0 && !opt("-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-f", "--force", "-u", "--unset-upstream") ||
			opt("-l", "--list", "--contains", "--no-contains", "--merged", "--no-merged", "--show-current")
	case "tag":
		return len(plain) == 0 || opt("-l", "--list", "-v", "--verify", "--contains")
	case "config":
		return len(plain) <= 1 && !opt("--unset", "--unset-all", "--add", "--replace-all", "--rename-section", "--remove-section", "-e", "--edit") ||
			opt("--get", "--get-all", "--get-regexp", "-l", "--list")
	case "submodule":
		return first == "status" || first == "summary"
	case "bisect":
		return first == "log" || first == "view" || first == "visualize"
	case "lfs":
		return first == "ls-files" || first == "status" || first == "env"
	}
	return false
}

// gitActs reports whether a git command (its arguments after git) acts in the tree, run in dir.
func gitActs(args []string, dir, tree, home string) bool {
	gdir := dir
	var sub []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-C" || a == "--git-dir" || a == "--work-tree") && i+1 < len(args):
			gdir = resolvePath(args[i+1], gdir, home)
			i++
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			_, v, _ := strings.Cut(a, "=")
			gdir = resolvePath(v, gdir, home)
		case (a == "-c" || a == "--namespace" || a == "--exec-path") && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "-"):
		default:
			sub = args[i:]
		}
		if sub != nil {
			break
		}
	}
	if len(sub) == 0 {
		return false
	}
	name, rest := sub[0], sub[1:]
	if gitRead(name, rest) {
		return false
	}
	plain := plainArgs(rest, "-b", "--branch", "-B", "-o", "--origin", "-c", "--config", "-u", "--upload-pack", "-j", "--jobs",
		"--depth", "--reference", "--filter", "--template", "--separate-git-dir", "--shallow-since", "--shallow-exclude", "--reason")
	switch name {
	case "clone": // it makes its destination, the repository's name in dir by default
		dest := ""
		if len(plain) > 1 {
			dest = plain[1]
		} else if len(plain) == 1 {
			dest = strings.TrimSuffix(filepath.Base(strings.TrimSuffix(plain[0], "/")), ".git")
		}
		return dest != "" && under(resolvePath(dest, gdir, home), tree)
	case "worktree":
		if len(plain) > 1 && plain[0] == "add" && under(resolvePath(plain[1], gdir, home), tree) {
			return true
		}
	}
	return gdir != "" && under(gdir, tree)
}

// ghActs reports whether a gh command changes files where it runs: a checkout, a clone, a sync, a download.
func ghActs(args []string, dir, tree, home string) bool {
	plain := plainArgs(args, "-R", "--repo", "-b", "--branch", "--json", "-q", "--jq", "-t", "--template", "-p", "--pattern", "-D", "--dir")
	for i := 0; i+1 < len(plain); i++ {
		switch plain[i] + " " + plain[i+1] {
		case "pr checkout", "repo sync", "release download", "run download":
			return dir != "" && under(dir, tree)
		case "repo clone":
			dest := ""
			if len(plain) > i+3 {
				dest = plain[i+3]
			} else if len(plain) == i+3 {
				dest = filepath.Base(plain[i+2])
			}
			return dest != "" && under(resolvePath(dest, dir, home), tree)
		}
	}
	return false
}

// duration is a number a wrapper takes, as timeout's 5m.
var duration = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[smhd]?$`)

// redirect is a redirection's operator at the start of a word: >, >>, >|, 2>, &>.
var redirect = regexp.MustCompile(`^(?:[0-9]*|&)>>?\|?`)

// commandActs reports whether a command line acts in the tree. dir is where it starts; a cd moves it. cut says the
// store cut the line: when it ends in the tree, what it ran next there is unknown, so it acts.
func commandActs(cmd, dir, tree, home string, cut bool, depth int) bool {
	if depth > 4 {
		return namesPath(cmd, []string{tree})
	}
	cmd = expandVars(strings.ReplaceAll(cmd, "${HOME}", "$HOME")) // its braces are no group
	segs, balanced := shellSegments(cmd)
	if !balanced { // a quote it cannot pair (a heredoc's text, a cut): read it as separators split it, too
		var plain []string
		start := 0
		for _, m := range shellSep.FindAllStringIndex(cmd, -1) {
			plain = append(plain, cmd[start:m[0]])
			start = m[1]
		}
		segs = append(segs, append(plain, cmd[start:])...)
	}
	for i, seg := range segs {
		var words []string
		all := shellWords(seg)
		for j := 0; j < len(all); j++ {
			w := all[j].text
			if strings.HasPrefix(w, "<") { // input and heredocs read
				if w == "<" && j+1 < len(all) {
					j++
				}
				continue
			}
			if op := redirect.FindString(w); op != "" {
				target := w[len(op):]
				if target == "" && j+1 < len(all) {
					j++
					target = all[j].text
				}
				if !strings.HasPrefix(target, "&") && target != "/dev/null" && under(resolvePath(target, dir, home), tree) {
					return true
				}
				continue
			}
			words = append(words, w)
		}
		k := 0
		for k < len(words) && (wrappers[words[k]] || strings.HasPrefix(words[k], "-") || duration.MatchString(words[k]) || envAssign.MatchString(words[k])) {
			k++
		}
		if k < len(words) {
			prog, args := filepath.Base(words[k]), words[k+1:]
			if progActs(prog, args, &dir, tree, home, depth) {
				return true
			}
		}
		if cut && i == len(segs)-1 && dir != "" && under(dir, tree) {
			return true
		}
	}
	return false
}

// sepAt is shellSep at the start of a text.
var sepAt = regexp.MustCompile("^(?:" + shellSep.String() + ")")

// shellSegments splits a command line into its simple commands at the separators shellSep names that no quote
// holds. balanced is false when a quote is left open.
func shellSegments(cmd string) (segs []string, balanced bool) {
	start, in := 0, byte(0)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case in != 0:
			if c == in {
				in = 0
			} else if c == '\\' && in == '"' && i+1 < len(cmd) {
				i++
			}
		case c == '\'' || c == '"':
			in = c
		case c == '\\' && i+1 < len(cmd):
			i++
		default:
			if m := sepAt.FindStringIndex(cmd[i:]); m != nil {
				segs = append(segs, cmd[start:i])
				i += m[1] - 1
				start = i + 1
			}
		}
	}
	return append(segs, cmd[start:]), in == 0
}

// progActs reports whether one program run in *dir with its arguments acts in the tree; a cd moves *dir.
func progActs(prog string, args []string, dir *string, tree, home string, depth int) bool {
	switch prog {
	case "cd", "pushd":
		switch {
		case len(args) == 0:
			*dir = home
		case args[0] != "-":
			*dir = resolvePath(args[0], *dir, home)
		}
		return false
	case "git":
		return gitActs(args, *dir, tree, home)
	case "gh":
		return ghActs(args, *dir, tree, home)
	case "sh", "bash", "zsh", "dash":
		for i, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
				return commandActs(args[i+1], *dir, tree, home, false, depth+1)
			}
		}
	case "eval":
		return commandActs(strings.Join(args, " "), *dir, tree, home, false, depth+1)
	case "waitfor": // it runs only its --until probe; the rest it watches
		for i, a := range args {
			if a == "--until" && i+1 < len(args) && commandActs(args[i+1], *dir, tree, home, false, depth+1) {
				return true
			}
		}
		return false
	}
	if readOnly(prog, args) {
		return false
	}
	// anything else may write: it acts when it runs in the tree or is given a path in it
	if *dir != "" && under(*dir, tree) {
		return true
	}
	spellings := treeSpellings(tree, home)
	for _, a := range args {
		if _, v, ok := strings.Cut(a, "="); ok && !strings.ContainsAny(a, " \t\n") {
			a = v
		}
		if strings.ContainsAny(a, " \t\n;|&") { // a script or command in an argument: its paths by name
			if namesPath(a, spellings) {
				return true
			}
			continue
		}
		p := resolvePath(a, *dir, home)
		if p == "" || !under(p, tree) {
			continue
		}
		bare := !filepath.IsAbs(a) && !strings.HasPrefix(a, "~") && !strings.HasPrefix(a, "$HOME") && !strings.HasPrefix(a, "${HOME}") &&
			!strings.HasPrefix(a, "./") && !strings.HasPrefix(a, "../") && p == tree
		if !bare || writeProgs[prog] { // a bare name is the repository's name as often as a path, for a program that need not write
			return true
		}
	}
	return false
}
