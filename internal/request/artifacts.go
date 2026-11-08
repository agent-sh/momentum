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

// The texts an artifact is read from, matched in the decoded output, where a line ends at a newline (a text read
// whole, or kept as JSON) or the store joined the lines with spaces (a text it keeps plain).
var (
	// git's commit line: [BRANCH HASH] SUBJECT, the branch maybe "detached HEAD", a first commit "(root-commit)"
	commitRe = regexp.MustCompile(`(?m)(?:^|\s)\[([^\[\]\n]+?)(?: \(root-commit\))? ([0-9a-f]{7,40})\] ([^\n]+?)(?: \d+ files? changed|$)`)
	// git push's To line: at a line's start in a text with lines, after whitespace in one the store joined
	pushToLineRe = regexp.MustCompile(`(?m)^To (\S+)`)
	pushToWordRe = regexp.MustCompile(`(?:^|\s)To (\S+)`)
	// what ends a push's block: a fetch's From line, or the next push's To line
	pushEndLineRe = regexp.MustCompile(`(?m)^(?:From|To) \S`)
	pushEndWordRe = regexp.MustCompile(`(?:^|\s)From \S`)
	// a ref update of a push: OLD..NEW SRC -> DST, or [new branch] SRC -> DST
	pushRefRe = regexp.MustCompile(`(?m)(?:^|\s)(?:\+ )?(?:([0-9a-f]{7,40})\.\.\.?([0-9a-f]{7,40})|\* \[new branch\]|\[new branch\])\s+(\S+) -> (\S+)`)
	prRe      = regexp.MustCompile(`https?://[^\s"']+/pull/\d+`)
	// a line that is a pull request's address alone, as gh pr create prints it
	prLineRe = regexp.MustCompile(`(?m)^[ \t]*(https?://[^\s"']+/pull/\d+)[ \t\r]*$`)
	patchRe  = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: ([^\s"]+)`)
	// a push's remote: a URL, an scp-like host:path, or a path
	remoteRe = regexp.MustCompile(`[:/]`)
)

// Artifacts are what a call made, as a line of its output shows it, and nothing else. Both are decoded first, as
// the harness keeps them (Decode), what the store's cut touches dropped. A file-writing call made the files its
// input names, when its result succeeded. A shell call made the commits its output's commit lines show, when its
// command runs a git command that makes them and prints them (not quiet, its output not sent to a file): git
// prints the line only when the commit was made, so a failed result's line counts too. It made the pushes its
// successful output's ref lines show inside a push's own block (from its To line to the next From or To line),
// when it runs git push and no push of it is a dry run, and the pull request whose address a successful gh pr
// create printed on a line of its own. A command that shows nothing (a quiet -q, an address kept in a
// variable, a command still running in the background) made nothing this can see. What any other tool printed (an
// agent's report, a read, a search, a fetched page) is no artifact.
func Artifacts(call, res store.Event) []Result {
	tool := strings.ToLower(call.Tool)
	if writers[tool] {
		if metaOf(res.Meta).IsError {
			return nil
		}
		return artifactsOf(call, res, Decode(call.Harness, tool, call.Summary, ""), makes{})
	}
	if !toolresult.IsShell(tool) {
		return nil
	}
	d := Decode(call.Harness, tool, call.Summary, res.Summary)
	return artifactsOf(call, res, d, made(d.Commands))
}

// artifactsOf reads a call's artifacts off its decoded input and output, with what its commands make, read once.
func artifactsOf(call, res store.Event, d Decoded, mk makes) []Result {
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
	if writers[strings.ToLower(call.Tool)] {
		for _, p := range d.Paths {
			add(Result{Kind: KindFile, Key: KindFile + ":" + p + "@" + once, Text: p, Event: call.ID()})
		}
		return out
	}
	text := d.Output
	lines := strings.Contains(text, "\n")
	var commits [][]int
	for _, c := range commitRe.FindAllStringSubmatchIndex(text, -1) {
		commits = append(commits, c) // a commit line is no other artifact's line, whoever printed it
		if !mk.commits {
			continue
		}
		hash, branch := text[c[4]:c[5]], text[c[2]:c[3]]
		r := Result{Kind: KindCommit, Key: KindCommit + ":" + hash, Text: fmt.Sprintf("commit %s on %s", hash, branch)}
		if !(d.Cut && c[7] == len(text)) { // a subject the cut ends is not known
			r.Text += ": " + strings.TrimSpace(text[c[6]:c[7]])
		}
		add(r)
	}
	if failed {
		return out // a failed result shows its commit lines, and nothing else it made
	}

	if mk.pushes {
		for _, b := range pushBlocks(text, lines, commits, mk) {
			for _, p := range pushRefRe.FindAllStringSubmatch(b.refs, -1) {
				if p[2] != "" {
					add(Result{Kind: KindPush, Key: KindPush + ":" + b.remote + " " + p[4] + "@" + p[2],
						Text: fmt.Sprintf("pushed %s -> %s (%s..%s) to %s", p[3], p[4], p[1], p[2], b.remote)})
				} else {
					add(Result{Kind: KindPush, Key: KindPush + ":" + b.remote + " " + p[4] + "@new@" + once,
						Text: fmt.Sprintf("pushed new branch %s -> %s to %s", p[3], p[4], b.remote)})
				}
			}
		}
	}
	if mk.prs {
		if u := createdPR(text, lines, commits); u != "" {
			add(Result{Kind: KindPR, Key: KindPR + ":" + u, Text: "pull request " + u})
		}
	}
	return out
}

// inSpans reports a place inside one of spans (commit lines).
func inSpans(spans [][]int, at int) bool {
	for _, c := range spans {
		if at >= c[0] && at < c[1] {
			return true
		}
	}
	return false
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// createdPR is the address gh pr create printed: the last one on a line of its own (in a text the store joined, a
// word of its own not after a field's name, such as gh pr view's url:), outside the commit lines, and not the
// address of one it says already exists (the line before it ends "already exists:").
func createdPR(text string, lines bool, commits [][]int) string {
	u := ""
	if lines {
		for _, m := range prLineRe.FindAllStringSubmatchIndex(text, -1) {
			before := strings.TrimRight(text[:m[0]], " \t\r\n")
			if strings.HasSuffix(before, "already exists:") { // a line of its own is never inside a commit line
				continue
			}
			u = text[m[2]:m[3]]
		}
		return u
	}
	for _, loc := range prRe.FindAllStringIndex(text, -1) {
		if inSpans(commits, loc[0]) || loc[0] > 0 && !isSpace(text[loc[0]-1]) || loc[1] < len(text) && !isSpace(text[loc[1]]) {
			continue
		}
		if before := strings.TrimRight(text[:loc[0]], " \t"); strings.HasSuffix(before, ":") {
			continue // a field's value (url:), or the address of one that already exists (already exists:)
		}
		u = text[loc[0]:loc[1]]
	}
	return u
}

// block is a push's part of an output: its remote and the text its ref lines are in.
type block struct {
	remote, refs string
}

// pushBlocks are the blocks git push printed: each from a To line (at a line's start, or in a text the store joined
// a word with a remote's shape outside a commit line) to the next From or To line, or the text's end. With no To
// line (a tail kept only the last lines), the whole text of a call that ran nothing else that prints ref lines,
// its remote the push's own.
func pushBlocks(text string, lines bool, commits [][]int, mk makes) []block {
	inCommit := func(at int) bool { return inSpans(commits, at) }
	var tos [][]int
	re, end := pushToWordRe, pushEndWordRe
	if lines {
		re, end = pushToLineRe, pushEndLineRe
	}
	for _, t := range re.FindAllStringSubmatchIndex(text, -1) {
		if remote := text[t[2]:t[3]]; !inCommit(t[0]) && (lines || remoteRe.MatchString(remote)) {
			tos = append(tos, t)
		}
	}
	var out []block
	for i, t := range tos {
		rest := text[t[1]:]
		if i+1 < len(tos) {
			rest = text[t[1]:tos[i+1][0]]
		}
		if e := end.FindStringIndex(rest); e != nil {
			rest = rest[:e[0]]
		}
		out = append(out, block{remote: text[t[2]:t[3]], refs: rest})
	}
	if len(tos) == 0 && !mk.fetches {
		remote := mk.remote
		if remote == "" {
			remote = "origin"
		}
		out = append(out, block{remote: remote, refs: text})
	}
	return out
}
