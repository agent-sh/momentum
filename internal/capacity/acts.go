package capacity

import (
	"path/filepath"
	"regexp"
	"slices"
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
	if m := workdirKey.FindStringSubmatch(input); m != nil { // the directory the call's tool runs it in
		if cwd = resolvePath(unescapeJSON(m[1]), cwd, home); cwd != "" && under(cwd, tree) {
			return true
		}
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
	cmds := append(commandsIn(input), commandArrays(input)...)
	if len(cmds) == 0 { // a call it cannot read: the tree's path, or a path field in it
		for _, m := range pathKey.FindAllStringSubmatch(input, -1) {
			if underTree(unescapeJSON(m[1]), cwd, tree, home) {
				return true
			}
		}
		return mentions
	}
	cut := strings.HasSuffix(strings.TrimSpace(input), "…") && commandOpen(input)
	for i, c := range cmds {
		if commandActs(c, cwd, tree, home, cut && i == len(cmds)-1, 0) {
			return true
		}
	}
	return !shellTools[tool] && mentions // a tool that is no shell may run its command elsewhere, as its other fields say
}

// commandOpen reports whether a call's last command runs to the input's end, its closing quote or bracket cut off.
func commandOpen(input string) bool {
	if ms := commandKey.FindAllStringIndex(input, -1); len(ms) > 0 {
		m := ms[len(ms)-1]
		closing := input[m[1]-1]
		for i := m[1]; i < len(input); i++ {
			switch input[i] {
			case '\\':
				i++
			case closing:
				return false
			}
		}
		return true
	}
	if ms := commandArray.FindAllStringIndex(input, -1); len(ms) > 0 {
		return !strings.Contains(input[ms[len(ms)-1][1]:], "]")
	}
	return true
}

// shellTools run the command they are given in the call's directory, or in the one its workdir names.
var shellTools = map[string]bool{
	"Bash": true, "bash": true, "shell": true, "Shell": true, "exec": true, "exec_command": true, "shell_command": true,
	"local_shell": true, "terminal": true, "run_terminal_command": true,
}

var (
	// workdirKey is the directory a call's tool runs its command in, in the call's JSON.
	workdirKey = regexp.MustCompile(`"(?:workdir|cwd|working_directory|workingDirectory|dir|directory)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	// commandArray is the start of a command given as its words: "command":["bash","-lc","..."].
	commandArray = regexp.MustCompile(`"(?:command|cmd|argv)"\s*:\s*\[`)
)

// commandArrays are the commands a call gives as arrays of words, each as a command line; a shell's -c script is
// the line itself.
func commandArrays(input string) []string {
	var out []string
	for _, m := range commandArray.FindAllStringIndex(input, -1) {
		var words []string
		for i := m[1]; i < len(input) && input[i] != ']'; i++ {
			if input[i] != '"' {
				continue
			}
			var b strings.Builder
			for i++; i < len(input) && input[i] != '"'; i++ {
				if input[i] == '\\' && i+1 < len(input) {
					b.WriteByte(input[i])
					i++
				}
				b.WriteByte(input[i])
			}
			words = append(words, unescapeJSON(b.String()))
		}
		if len(words) == 0 {
			continue
		}
		if n := len(words); n >= 3 && shells[filepath.Base(words[0])] && strings.HasPrefix(words[n-2], "-") && strings.Contains(words[n-2], "c") {
			out = append(out, words[n-1])
			continue
		}
		for i, w := range words {
			if strings.ContainsAny(w, " \t'\"") {
				words[i] = "'" + strings.ReplaceAll(w, "'", "") + "'"
			}
		}
		out = append(out, strings.Join(words, " "))
	}
	return out
}

// shells are the programs that run a script.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

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
	case "yq":
		return !has(func(a string) bool { return a == "--inplace" || strings.HasPrefix(a, "--inplace=") || short(a, "i") })
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
	"diff-tree": true, "diff-files": true, "diff-index": true, "show-branch": true, "archive": true,
}

// gitRead reports whether a git subcommand with its arguments reads only. One it does not know writes.
func gitRead(sub string, rest []string) bool {
	if gitReads[sub] {
		return true
	}
	var plain []string
	opt := func(names ...string) bool {
		for _, a := range rest {
			name, _, _ := strings.Cut(a, "=")
			if contains(names, name) {
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
	case "fetch": // it writes remote refs, unless a refspec moves a local branch (main:main)
		for _, a := range plain {
			if _, dst, ok := strings.Cut(a, ":"); ok && dst != "" && !strings.HasPrefix(dst, "refs/remotes/") && !strings.Contains(a, "://") {
				return false
			}
		}
		return true
	case "stash", "notes":
		return first == "list" || first == "show"
	case "worktree":
		return first == "list"
	case "reflog":
		return first == "" || first == "show"
	case "remote":
		return first == "" || first == "show" || first == "get-url"
	case "branch":
		return len(plain) == 0 && !opt("-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-f", "--force", "-u", "--set-upstream-to", "--unset-upstream", "--edit-description") ||
			opt("-l", "--list", "--contains", "--no-contains", "--merged", "--no-merged", "--show-current")
	case "tag":
		return len(plain) == 0 || opt("-l", "--list", "-v", "--verify", "--contains")
	case "config":
		return opt("--global", "--system") || len(plain) <= 1 && !opt("--unset", "--unset-all", "--add", "--replace-all", "--rename-section", "--remove-section", "-e", "--edit") ||
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
	gdir, sub := gitSub(args, dir, home)
	if len(sub) == 0 {
		return false
	}
	name, rest := sub[0], sub[1:]
	for i, a := range rest { // a file a git command writes: diff --output, archive -o, format-patch -o, bundle create
		v, ok := "", false
		switch {
		case strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "--output-directory="):
			_, v, ok = strings.Cut(a, "=")
		case (a == "-o" || a == "--output" || a == "--output-directory") && i+1 < len(rest):
			v, ok = rest[i+1], true
		}
		if ok && underTree(v, gdir, tree, home) {
			return true
		}
	}
	switch name {
	case "format-patch": // it writes its patches where -o says, else where it runs
		if contains(rest, "-o") || contains(rest, "--output-directory") || slices.ContainsFunc(rest, func(a string) bool { return strings.HasPrefix(a, "--output-directory=") }) || contains(rest, "--stdout") {
			return false
		}
		return gdir != "" && under(gdir, tree)
	case "bundle":
		pl := plainArgs(rest)
		return len(pl) > 1 && pl[0] == "create" && underTree(pl[1], gdir, tree, home)
	}
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
	case "worktree": // a worktree made, removed or moved in the tree, from any repository
		if len(plain) > 1 && (plain[0] == "add" || plain[0] == "remove" || plain[0] == "move") && underTree(plain[1], gdir, tree, home) ||
			len(plain) > 2 && plain[0] == "move" && underTree(plain[2], gdir, tree, home) {
			return true
		}
	}
	return gdir != "" && under(gdir, tree)
}

// gitWrites reports whether a git command writes wherever it runs.
func gitWrites(args []string) bool {
	_, sub := gitSub(args, "", "")
	return len(sub) > 0 && !gitRead(sub[0], sub[1:])
}

// gitSub is the directory a git command runs in (dir, or where -C, --git-dir or --work-tree point) and its
// subcommand with the subcommand's arguments.
func gitSub(args []string, dir, home string) (string, []string) {
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
	return gdir, sub
}

// ghActs reports whether a gh command changes files where it runs: a checkout, a clone, a sync, a download.
func ghActs(args []string, dir, tree, home string) bool {
	plain := plainArgs(args, "-R", "--repo", "-b", "--branch", "--json", "-q", "--jq", "-t", "--template", "-p", "--pattern", "-D", "--dir")
	for i := 0; i+1 < len(plain); i++ {
		switch plain[i] + " " + plain[i+1] {
		case "pr checkout", "pr merge", "repo sync", "release download", "run download":
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
		return namesPath(cmd, treeSpellings(tree, home))
	}
	cmd = expandVars(strings.ReplaceAll(cmd, "${HOME}", "$HOME")) // its braces are no group
	cmd, script := heredocs(cmd)
	if namesPath(script, treeSpellings(tree, home)) { // a script a program reads from a heredoc, naming the tree
		return true
	}
	cmd = spaceRedirects(cmd)
	segs, seps, balanced := shellSegments(cmd)
	if !balanced { // a quote it cannot pair (a heredoc's text, a cut): read it as separators split it, too
		start := 0
		for _, m := range shellSep.FindAllStringIndex(cmd, -1) {
			segs, seps = append(segs, cmd[start:m[0]]), append(seps, cmd[m[0]:m[1]])
			start = m[1]
		}
		segs = append(segs, cmd[start:])
		seps = append(seps, "")
	}
	spellings := treeSpellings(tree, home)
	piped := false            // an earlier command of this pipeline named the tree
	vars := map[string]bool{} // variables a loop or a read sets to what names the tree
	for i, seg := range segs {
		if i == 0 || seps[i-1] != "|" {
			piped = false
		}
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
		if len(words) > 0 && (words[0] == "for" || words[0] == "select" || words[0] == "case") {
			// a loop's head runs nothing: its variable holds the tree when its list names it
			if in := slices.Index(words, "in"); words[0] != "case" && len(words) > 1 && in > 0 &&
				(namesPath(seg, spellings) || namesIn(words[in+1:], dir, tree, home)) {
				vars[words[1]] = true
			}
			continue
		}
		at, k, feeds := dir, 0, false
		var assigned []string // the values of the assignments before the program
		for wrap := ""; k < len(words); k++ {
			w := words[k]
			if wrappers[w] {
				wrap, feeds = w, feeds || w == "xargs"
				continue
			}
			if strings.HasPrefix(w, "-") {
				name, val, inline := strings.Cut(w, "=")
				switch {
				case contains(wrapperDirs[wrap], name) && inline:
					at = resolvePath(val, at, home)
				case contains(wrapperDirs[wrap], name) && k+1 < len(words):
					k++
					at = resolvePath(words[k], at, home)
				case !inline && contains(wrapperValued[wrap], name):
					k++
				}
				continue
			}
			if envAssign.MatchString(w) {
				_, v, _ := strings.Cut(w, "=")
				assigned = append(assigned, v)
				continue
			}
			if !duration.MatchString(w) {
				break
			}
		}
		if k < len(words) {
			prog, args := filepath.Base(words[k]), words[k+1:]
			// a program of the tree's, run by its path; a writer told by its environment to write in the tree
			if strings.ContainsAny(words[k], "/~") && underTree(words[k], at, tree, home) {
				return true
			}
			for _, v := range assigned {
				if (filepath.IsAbs(v) || strings.HasPrefix(v, "~") || strings.HasPrefix(v, "$HOME")) && underTree(v, at, tree, home) && writes(prog, args) {
					return true
				}
			}
			// what an earlier command of the pipeline named reaches a writer that takes its input as arguments or as a script
			if piped && (feeds || prog == "parallel" || shells[prog] && !hasScriptFlag(args)) && writes(prog, args) {
				return true
			}
			if prog == "read" && piped {
				for _, a := range args {
					if !strings.HasPrefix(a, "-") {
						vars[a] = true
					}
				}
			}
			if usesVar(args, vars) && writes(prog, args) {
				return true
			}
			d := at
			if progActs(prog, args, &d, tree, home, depth) {
				return true
			}
			if prog == "cd" || prog == "pushd" {
				dir = d
				if usesVar(args, vars) { // a loop's directory that holds the tree
					dir = tree
				}
			}
		}
		if !piped {
			piped = namesPath(seg, spellings) || namesIn(words, dir, tree, home) ||
				k < len(words) && listsAbove(filepath.Base(words[k]), words[k+1:], dir, tree, home)
		}
		if cut && i == len(segs)-1 && dir != "" && under(dir, tree) {
			return true
		}
	}
	return false
}

// usesVar reports whether an argument uses one of the variables.
func usesVar(args []string, vars map[string]bool) bool {
	for v := range vars {
		for _, a := range args {
			if strings.Contains(a, "$"+v) || strings.Contains(a, "${"+v) {
				return true
			}
		}
	}
	return false
}

// underTree reports whether a word names a path in the tree, resolved in dir.
func underTree(w, dir, tree, home string) bool {
	p := resolvePath(w, dir, home)
	return p != "" && under(p, tree)
}

// heredocStart is a heredoc's operator and its word: <<EOF, <<-'EOF', << "EOF".
var heredocStart = regexp.MustCompile(`<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// heredocs takes the heredocs' text out of a command line: the lines a shell reads as its script stay commands, any
// other program's are its data, or a script it runs (script). A heredoc the store cut runs to the line's end.
func heredocs(cmd string) (string, string) {
	var script strings.Builder
	for from := 0; from < len(cmd); {
		m := heredocStart.FindStringSubmatchIndex(cmd[from:])
		if m == nil {
			break
		}
		if at := from + m[0]; at > 0 && cmd[at-1] == '<' { // a here-string, <<<word
			from = at + 2
			continue
		}
		word := cmd[from+m[4] : from+m[5]]
		nl := strings.IndexByte(cmd[from+m[1]:], '\n')
		if nl < 0 {
			break
		}
		bodyAt := from + m[1] + nl + 1
		dash := strings.HasPrefix(cmd[from+m[0]:], "<<-") // its lines, the last too, may start with tabs
		end, next := len(cmd), len(cmd)
		for pos := bodyAt; pos < len(cmd); {
			lineEnd := len(cmd)
			if j := strings.IndexByte(cmd[pos:], '\n'); j >= 0 {
				lineEnd = pos + j
			}
			if line := cmd[pos:lineEnd]; line == word || dash && strings.TrimLeft(line, "\t") == word {
				end, next = pos, lineEnd
				break
			}
			pos = lineEnd + 1
		}
		line := cmd[strings.LastIndexAny(cmd[:from+m[0]], "\n;&|(")+1 : from+m[0]]
		prog := ""
		for _, w := range shellWords(line) {
			if !wrappers[w.text] && !strings.HasPrefix(w.text, "-") && !envAssign.MatchString(w.text) {
				prog = filepath.Base(w.text)
				break
			}
		}
		if shells[prog] {
			from = next
			continue
		}
		if interpreters[prog] {
			script.WriteString(cmd[bodyAt:end] + "\n")
		}
		cmd = cmd[:bodyAt] + cmd[next:]
		from = bodyAt
	}
	return cmd, script.String()
}

// interpreters run the script a heredoc hands them; any other program's heredoc is its data.
var interpreters = map[string]bool{"python": true, "python3": true, "python2": true, "node": true, "perl": true, "ruby": true,
	"php": true, "deno": true, "bun": true, "lua": true, "julia": true, "Rscript": true, "uv": true, "osascript": true}

// spaceRedirects puts spaces around the redirections no quote holds, so a target written onto its word
// (echo x>file) is a word of its own.
func spaceRedirects(cmd string) string {
	var b strings.Builder
	in := byte(0)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case in != 0:
			if c == in {
				in = 0
			} else if c == '\\' && in == '"' && i+1 < len(cmd) {
				b.WriteByte(c)
				i++
				c = cmd[i]
			}
		case c == '\'' || c == '"':
			in = c
		case c == '\\' && i+1 < len(cmd):
			b.WriteByte(c)
			i++
			c = cmd[i]
		case c == '>':
			j := i + 1
			for j < len(cmd) && (cmd[j] == '>' || cmd[j] == '|' || cmd[j] == '&') {
				j++
			}
			if p := b.String(); p != "" && strings.IndexByte(" \t0123456789&", p[len(p)-1]) < 0 {
				b.WriteByte(' ')
			}
			b.WriteString(cmd[i:j])
			if j == len(cmd) || cmd[j-1] != '&' { // >&2 names no file
				b.WriteByte(' ')
			}
			i = j - 1
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// wrapperValued are the options of a wrapper that take a value; wrapperDirs those that set the directory the command
// runs in.
var (
	wrapperValued = map[string][]string{
		"env":         {"-u", "--unset", "-S", "--split-string"},
		"timeout":     {"-s", "--signal", "-k", "--kill-after"},
		"sudo":        {"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-U", "--other-user", "-C", "--close-from", "-r", "--role", "-t", "--type"},
		"nice":        {"-n", "--adjustment"},
		"ionice":      {"-c", "--class", "-n", "--classdata", "-p", "--pid"},
		"systemd-run": {"-p", "--property", "-u", "--unit", "-E", "--setenv", "--slice", "-M", "--machine", "--uid", "--gid", "--nice", "--description"},
		"xargs":       {"-I", "-n", "--max-args", "-P", "--max-procs", "-L", "--max-lines", "-d", "--delimiter", "-s", "--max-chars", "-a", "--arg-file", "-E"},
		"stdbuf":      {"-i", "-o", "-e"},
	}
	wrapperDirs = map[string][]string{"env": {"-C", "--chdir"}, "sudo": {"-D", "--chdir"}, "systemd-run": {"--working-directory"}}
)

// hasScriptFlag reports whether a shell's arguments give it its script with -c.
func hasScriptFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			return true
		}
	}
	return false
}

// writes reports whether a program may write wherever it runs: git with a subcommand that writes, any program that
// does not only read.
func writes(prog string, args []string) bool {
	if prog == "git" {
		return gitWrites(args)
	}
	switch prog {
	case "gh":
		return ghActs(args, "/", "/", "")
	case "cd", "pushd", "waitfor":
		return false
	}
	if prog == "parallel" && len(args) > 0 {
		return writes(filepath.Base(args[0]), args[1:])
	}
	return !readOnly(prog, args)
}

// namesIn reports whether a command's words name a path in the tree, the tree itself by a relative name too.
func namesIn(words []string, dir, tree, home string) bool {
	for _, w := range words {
		if p := resolvePath(w, dir, home); p != "" && (under(p, tree) || globReaches(p, tree)) {
			return true
		}
	}
	return false
}

// listsAbove reports whether a program lists what is under a directory above the tree, the tree among it: a walk
// over it, or ls of it.
func listsAbove(prog string, args []string, dir, tree, home string) bool {
	return (walks(prog, args) || prog == "ls") && aboveIn(args, dir, tree, home)
}

// globReaches reports whether a pattern's matches can be the tree or inside it: its first words, as many as the
// tree has, match the tree's (** matches any).
func globReaches(pattern, tree string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	ps, ts := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(tree, "/"), "/")
	for i, t := range ts {
		if i == len(ps) {
			return false // the pattern stops above the tree
		}
		if ps[i] == "**" {
			return true
		}
		if ok, err := filepath.Match(ps[i], t); err != nil || !ok {
			return false
		}
	}
	return true
}

// walks reports whether a program given a directory works through everything under it: find, a recursive search,
// deletion, copy or mode change.
func walks(prog string, args []string) bool {
	switch prog {
	case "find", "fd", "rg", "du", "tree":
		return true
	case "rm", "chmod", "chown", "chgrp", "grep", "cp", "ls":
		for _, a := range args {
			if a == "--recursive" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR") {
				return true
			}
		}
	}
	return false
}

// aboveIn reports whether a program is given a directory above the tree: its parent or higher.
func aboveIn(args []string, dir, tree, home string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if p := resolvePath(a, dir, home); p != "" && p != tree && under(tree, p) {
			return true
		}
	}
	return false
}

// sepAt is a separator at the start of a text: shellSep's but braces, which group only as words of their own.
var sepAt = regexp.MustCompile("^(?:&&|\\|\\||[;|\n()`]|\\$\\()")

// loneAmp reports whether an & at i sends its command to the background and ends it: not && and not part of a
// redirection (&>, >&, 2>&1).
func loneAmp(cmd string, i int) bool {
	return cmd[i] == '&' && (i+1 == len(cmd) || cmd[i+1] != '&' && cmd[i+1] != '>') && (i == 0 || cmd[i-1] != '&' && cmd[i-1] != '>')
}

// braceAt reports whether a brace at i is a word of its own, a group's start or end ({ a; b; }), not a part of a
// word (xargs -I{}, ${x}).
func braceAt(cmd string, i int) bool {
	if cmd[i] != '{' && cmd[i] != '}' {
		return false
	}
	before := i == 0 || strings.IndexByte(" \t\n;&|", cmd[i-1]) >= 0
	after := i+1 == len(cmd) || strings.IndexByte(" \t\n;", cmd[i+1]) >= 0
	return before && after
}

// shellSegments splits a command line into its simple commands at the separators shellSep names that no quote
// holds, and names the separator after each ("" after the last). balanced is false when a quote is left open.
func shellSegments(cmd string) (segs, seps []string, balanced bool) {
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
			if loneAmp(cmd, i) || braceAt(cmd, i) {
				segs, seps = append(segs, cmd[start:i]), append(seps, cmd[i:i+1])
				start = i + 1
			} else if m := sepAt.FindStringIndex(cmd[i:]); m != nil {
				segs, seps = append(segs, cmd[start:i]), append(seps, cmd[i:i+m[1]])
				i += m[1] - 1
				start = i + 1
			}
		}
	}
	return append(segs, cmd[start:]), append(seps, ""), in == 0
}

// nameOpts are options whose value is a name, not a path: a repository's, a unit's, a container's. nameProgs take
// names as their arguments.
var (
	nameOpts  = []string{"-R", "--repo", "--repository", "-u", "--unit", "--user-unit", "--name", "--container", "--service", "--label", "--tag"}
	nameProgs = map[string]bool{"journalctl": true, "systemctl": true, "docker": true, "podman": true, "kill": true, "pkill": true,
		"killall": true, "tacit": true, "momentum": true, "tmux": true}
)

// progActs reports whether one program run in *dir with its arguments acts in the tree; a cd moves *dir.
func progActs(prog string, args []string, dir *string, tree, home string, depth int) bool {
	switch prog {
	case "cd", "pushd":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
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
	if prog == "awk" || prog == "gawk" || prog == "sed" { // a script's own output file: awk's print > "f", sed's w f
		for _, a := range args {
			if strings.ContainsAny(a, ">w") && namesPath(a, treeSpellings(tree, home)) {
				return true
			}
		}
	}
	if readOnly(prog, args) {
		return false
	}
	// anything else may write: it acts when it runs in the tree or is given a path in it
	if *dir != "" && under(*dir, tree) {
		return true
	}
	if prog != "cp" && walks(prog, args) && aboveIn(args, *dir, tree, home) { // a recursive writer over a directory above the tree
		return true
	}
	switch prog {
	case "cp", "rsync", "scp", "install", "ln": // a copy writes its destination; its sources it reads
		if prog != "install" || !contains(args, "-d") && !contains(args, "--directory") {
			return destActs(args, *dir, tree, home)
		}
	case "tar":
		if acts, known := tarActs(args, *dir, tree, home); known {
			return acts
		}
	}
	spellings := treeSpellings(tree, home)
	for i, a := range args {
		if namesPath(a, spellings) { // the tree's path, as a word or inside one (a script, an option's value)
			return true
		}
		name, v, inline := strings.Cut(a, "=")
		if inline && !strings.ContainsAny(a, " \t\n") {
			a = v
		}
		if strings.ContainsAny(a, " \t\n;|&") {
			continue
		}
		p := resolvePath(a, *dir, home)
		if p != "" && globReaches(p, tree) {
			return true
		}
		if p == "" || !under(p, tree) {
			continue
		}
		bare := !filepath.IsAbs(a) && !strings.HasPrefix(a, "~") && !strings.HasPrefix(a, "$HOME") &&
			!strings.HasPrefix(a, "./") && !strings.HasPrefix(a, "../") && p == tree
		// a bare name is a path to a program that may write, unless an option says it is a name (--repo, --unit)
		// or the program takes names
		named := inline && contains(nameOpts, name) || i > 0 && contains(nameOpts, args[i-1])
		if !bare || !named && !nameProgs[prog] {
			return true
		}
	}
	return false
}

// destActs reports whether a copy's destination is in the tree: -t's directory, else its last argument.
func destActs(args []string, dir, tree, home string) bool {
	dest := ""
	for i, a := range args {
		switch {
		case (a == "-t" || a == "--target-directory") && i+1 < len(args):
			return under(resolvePath(args[i+1], dir, home), tree)
		case strings.HasPrefix(a, "--target-directory="):
			return under(resolvePath(strings.TrimPrefix(a, "--target-directory="), dir, home), tree)
		case !strings.HasPrefix(a, "-"):
			dest = a
		}
	}
	p := resolvePath(dest, dir, home)
	deletes := slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--delete") })
	return dest != "" && p != "" && (under(p, tree) || globReaches(p, tree) || deletes && under(tree, p))
}

// tarActs reports whether tar writes in the tree: an extraction into it, or an archive made in it. known is false
// when tar's mode is not one of the two.
func tarActs(args []string, dir, tree, home string) (acts, known bool) {
	var create, extract bool
	file, into := "", dir
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--create":
			create = true
		case a == "--extract" || a == "--get":
			extract = true
		case (a == "-C" || a == "--directory") && i+1 < len(args):
			i++
			into = resolvePath(args[i], into, home)
		case strings.HasPrefix(a, "--directory="):
			into = resolvePath(strings.TrimPrefix(a, "--directory="), into, home)
		case (a == "-f" || a == "--file") && i+1 < len(args):
			i++
			file = args[i]
		case strings.HasPrefix(a, "--file="):
			file = strings.TrimPrefix(a, "--file=")
		case i == 0 && !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			flags := strings.TrimPrefix(a, "-")
			create = create || strings.Contains(flags, "c")
			extract = extract || strings.Contains(flags, "x")
			if strings.HasSuffix(flags, "f") && i+1 < len(args) {
				i++
				file = args[i]
			}
		}
	}
	switch {
	case extract:
		return into != "" && under(into, tree), true
	case create:
		p := resolvePath(file, dir, home)
		return file != "" && p != "" && under(p, tree), true
	}
	return false, false
}
