package request

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/toolresult"
)

// writers are the file-writing tools of the harnesses the readers name, in lower case.
var writers = map[string]bool{"write": true, "edit": true, "multiedit": true, "notebookedit": true, "apply_patch": true,
	"write_file": true, "patch": true}

// shells are the harnesses' shell tools (toolresult).
func shells() []string { return toolresult.Shells() }

// The lines an artifact is read from, in an output whose lines are kept (read whole, or kept as JSON).
var (
	// the head of git's commit line, [BRANCH HASH], the branch maybe "detached HEAD", a first commit "(root-commit)";
	// its subject is the rest of the line
	commitRe = regexp.MustCompile(`(?m)^\[([^\[\]\n]+?)(?: \(root-commit\))? ([0-9a-f]{7,40})\](?: |$)`)
	// git push's To line, To REMOTE
	pushToRe = regexp.MustCompile(`(?m)^To (\S+)`)
	// what ends a push's block: a fetch's From line (the next push's To line ends it too)
	pushEndRe = regexp.MustCompile(`(?m)^From \S`)
	// a ref update of a push: OLD..NEW SRC -> DST, + OLD...NEW SRC -> DST for a forced one, or * [new branch] SRC
	// -> DST
	pushRefRe = regexp.MustCompile(`(?m)^[ \t]*(?:\+[ \t]*)?(?:([0-9a-f]{7,40})\.\.\.?([0-9a-f]{7,40})|\* \[new branch\])[ \t]+(\S+) -> (\S+)`)
	// a line that is a pull request's address alone, as gh pr create prints it
	prLineRe = regexp.MustCompile(`(?m)^(https?://[^\s"']+/pull/\d+)[ \t\r]*$`)
	// a patch's file header, a line of its own: a header its content mentions (an added line, a string) is none
	patchRe = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+?)[ \t\r]*$`)
	// a push's remote: a URL, an scp-like host:path, or a path
	remoteRe = regexp.MustCompile(`[:/]`)
)

// Artifacts are what a call made, as a line of its output shows it, and nothing else. Both are decoded first, as
// the harness keeps them (Decode), what the store's cut touches dropped. A file-writing call made the files its
// input names, when its result succeeded. A shell call made the commits its output's commit lines show, when its
// command runs a git command that makes them and prints them (not quiet, its output not sent to a file): git
// prints the line only when the commit was made, so a failed result's line counts too. It made the pushes its
// successful output's ref lines show inside a push's own block (from its To line to the next From or To line),
// when it runs git push and no push of it is a dry run, and the pull requests whose addresses a successful gh pr
// create printed, each on a line of its own, when the call runs nothing else of gh that prints addresses. A
// command that shows nothing (a quiet -q, an address kept in a
// variable, a command still running in the background) made nothing this can see. What any other tool printed (an
// agent's report, a read, a search, a fetched page) is no artifact.
func Artifacts(call, res store.Event) []Result {
	tool := strings.ToLower(call.Tool)
	if writers[tool] {
		if toolresult.WriteFailed(res.Summary, metaOf(res.Meta).IsError) {
			return nil
		}
		return artifactsOf(call, res, Decode(call.Harness, tool, call.Summary, ""), makes{}, nil)
	}
	if !toolresult.IsShell(tool) {
		return nil
	}
	d := Decode(call.Harness, tool, call.Summary, res.Summary)
	return artifactsOf(call, res, d, made(d), nil)
}

// session is the command a Codex session runs, as the call that left it running named it, and what it printed
// while it ran, kept for a command that pushes or opens pull requests: its ref lines and addresses count once a
// poll shows it ended successfully.
type session struct {
	cmd    string
	cut    bool
	keep   bool // it pushes or opens pull requests: what it prints while it runs is kept
	output string
}

// sessionKey is a Codex session as a thread knows it: each agent of the thread (its main thread "", a subagent)
// numbers its own.
type sessionKey struct {
	agent string
	id    int64
}

// agentOf is the agent of a thread an event is of: "" for its main thread.
func agentOf(e store.Event) string {
	if e.Agent == e.Session {
		return ""
	}
	return e.Agent
}

// yielded are the sessions a Codex call left running, each with the command it runs: a code-mode script's
// exec_command calls still running when it ended, and the command of Codex's exec_command tool when it yielded.
func yielded(call store.Event, d Decoded) map[int64]session {
	out := map[int64]session{}
	for _, c := range d.Chunks {
		if c.Running {
			out[c.Session] = running(c.Command, c.Cut, c.Output)
		}
	}
	if strings.ToLower(call.Tool) == "exec_command" {
		if c := plainChunk(d.Output, 0); c.Running {
			out[c.Session] = running(strings.Join(d.Commands, "\n"), d.CommandCut, d.Output)
		}
	}
	return out
}

// running is the session of a command still running, with what it printed so far when it pushes or opens pull
// requests.
func running(cmd string, cut bool, output string) session {
	s := session{cmd: cmd, cut: cut}
	if mk := made(Decoded{Commands: []string{cmd}, CommandCut: cut}); mk.pushes || mk.prs {
		s.keep, s.output = true, output
	}
	return s
}

// artifactsOf reads a call's artifacts off its decoded input and output, with what its commands make, read once.
// An output the store joined shows nothing; a Codex code-mode script's results are read each against its own
// command, and none when they cannot be matched to its calls; a poll's result against the command of the session
// it polls, among sessions (those its thread's earlier calls left running), and as nothing when none is known.
func artifactsOf(call, res store.Event, d Decoded, mk makes, sessions map[sessionKey]session) []Result {
	failed := metaOf(res.Meta).IsError
	once := metaOf(call.Meta).ToolID // what a call recorded again (a harness's copy of the same call) shares
	var out []Result
	add := func(r Result) {
		r.At = res.TS
		if r.Event == "" {
			r.Event = res.ID()
		}
		out = append(out, r)
	}
	switch {
	case writers[strings.ToLower(call.Tool)]:
		for _, p := range d.Paths {
			add(Result{Kind: KindFile, Key: KindFile + ":" + p + "@" + once, Text: p, Event: call.ID()})
		}
	case d.Unpaired || d.Joined && d.Polls == nil:
	case d.Chunks != nil || d.Polls != nil:
		for _, c := range d.Chunks {
			mk := made(Decoded{Commands: []string{c.Command}, CommandCut: c.Cut})
			if c.Plain || c.Running {
				// no exit code shows, or the command still runs: only a commit line, which git prints when it made
				// it; ref lines and addresses count once a result shows the command ended successfully
				mk.pushes, mk.prs = false, false
			}
			for _, r := range shown(c.Output, c.OutputCut, failed || c.Failed, mk, once) {
				add(r)
			}
		}
		for _, p := range d.Polls {
			if s, ok := sessions[sessionKey{agentOf(call), p.Session}]; ok {
				mk := made(Decoded{Commands: []string{s.cmd}, CommandCut: s.cut})
				text := p.Output // joined by the store, it shows no line: only its header, how the command ended, is read
				if p.Running {
					mk.pushes, mk.prs = false, false
				} else {
					text = s.output + "\n" + p.Output // what it printed while it ran, read now that it ended
				}
				for _, r := range shown(text, p.OutputCut, failed || p.Failed, mk, once) {
					add(r)
				}
			}
		}
	default:
		if call.Harness == "codex" && strings.ToLower(call.Tool) == "exec_command" && plainChunk(d.Output, 0).Running {
			mk.pushes, mk.prs = false, false // it still runs: what it printed is read when a poll shows it ended
		}
		for _, r := range shown(d.Output, d.OutputCut, failed, mk, once) {
			add(r)
		}
	}
	return out
}

// shown are the artifacts a text with its lines shows, of commands that make what mk says: the commit lines, in a
// failed result too, the ref lines of a push's blocks and the addresses gh pr create printed. cut is a text the
// store cut where it ends.
func shown(text string, cut, failed bool, mk makes, once string) []Result {
	var out []Result
	if mk.commits {
		for _, c := range commitRe.FindAllStringSubmatchIndex(text, -1) {
			hash, branch := text[c[4]:c[5]], text[c[2]:c[3]]
			r := Result{Kind: KindCommit, Key: KindCommit + ":" + hash, Text: fmt.Sprintf("commit %s on %s", hash, branch)}
			// the subject is the rest of its line, left out when the line runs to the end of a text the store cut:
			// what is left of it is shortened
			rest, _, ended := strings.Cut(text[c[1]:], "\n")
			if s := strings.TrimSpace(rest); s != "" && (ended || !cut) {
				r.Text += ": " + s
			}
			out = append(out, r)
		}
	}
	if failed {
		return out // a failed result shows its commit lines, and nothing else it made
	}
	if mk.pushes && !mk.quiet { // every push quiet (-q prints no ref line): a To block is another program's
		for _, b := range pushBlocks(text, mk) {
			for _, p := range pushRefRe.FindAllStringSubmatch(b.refs, -1) {
				if p[2] != "" {
					out = append(out, Result{Kind: KindPush, Key: KindPush + ":" + b.remote + " " + p[4] + "@" + p[2],
						Text: fmt.Sprintf("pushed %s -> %s (%s..%s) to %s", p[3], p[4], p[1], p[2], b.remote)})
				} else {
					out = append(out, Result{Kind: KindPush, Key: KindPush + ":" + b.remote + " " + p[4] + "@new@" + once,
						Text: fmt.Sprintf("pushed new branch %s -> %s to %s", p[3], p[4], b.remote)})
				}
			}
		}
	}
	if mk.prs {
		for _, m := range prLineRe.FindAllStringSubmatchIndex(text, -1) {
			// not the address of one it says already exists, on the line after the refusal
			if !strings.HasSuffix(strings.TrimRight(text[:m[0]], " \t\r\n"), "already exists:") {
				u := text[m[2]:m[3]]
				out = append(out, Result{Kind: KindPR, Key: KindPR + ":" + u, Text: "pull request " + u})
			}
		}
	}
	return out
}

// block is a push's part of an output: its remote and the text its ref lines are in.
type block struct {
	remote, refs string
}

// pushBlocks are the blocks git push printed, for a call whose pushes are not all quiet (shown): each from a To line
// whose remote has a remote's shape (a URL, host:path, a path) to the next From or To line, or the text's end. With
// no To line (a tail kept only the last lines), the whole text, its remote the push's own, of a call that ran
// nothing else that prints ref lines and whose output holds no fetch's From line.
func pushBlocks(text string, mk makes) []block {
	var tos [][]int
	for _, t := range pushToRe.FindAllStringSubmatchIndex(text, -1) {
		if remoteRe.MatchString(text[t[2]:t[3]]) {
			tos = append(tos, t)
		}
	}
	var out []block
	for i, t := range tos {
		rest := text[t[1]:]
		if i+1 < len(tos) {
			rest = text[t[1]:tos[i+1][0]]
		}
		if e := pushEndRe.FindStringIndex(rest); e != nil {
			rest = rest[:e[0]]
		}
		out = append(out, block{remote: text[t[2]:t[3]], refs: rest})
	}
	// a quiet push printed no ref line, and a From line is a fetch's (one the call ran where its parse cannot see,
	// as over ssh or in a script): the ref lines are another's
	if len(tos) == 0 && !mk.fetches && !pushEndRe.MatchString(text) {
		remote := mk.remote
		if remote == "" {
			remote = "origin"
		}
		out = append(out, block{remote: remote, refs: text})
	}
	return out
}
