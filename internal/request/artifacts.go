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

// The texts an artifact is read from, matched in the decoded output, where a line ends at a newline (a harness
// that keeps the text as JSON) or the store joined the lines with spaces (one that keeps it plain). A match starts
// at the text's start or after whitespace.
var (
	// git's commit line: [BRANCH HASH] SUBJECT, the branch maybe "detached HEAD", a first commit "(root-commit)"
	commitRe = regexp.MustCompile(`(?m)(?:^|\s)\[([^\[\]\n]+?)(?: \(root-commit\))? ([0-9a-f]{7,40})\] ([^\n]+?)(?: \d+ files? changed|$)`)
	// git push's To line, and the ref updates after it: OLD..NEW SRC -> DST, or [new branch] SRC -> DST
	pushToRe  = regexp.MustCompile(`(?m)(?:^|\s)To (\S+)`)
	pushRefRe = regexp.MustCompile(`(?m)(?:^|\s)(?:\+ )?(?:([0-9a-f]{7,40})\.\.\.?([0-9a-f]{7,40})|\* \[new branch\]|\[new branch\])\s+(\S+) -> (\S+)`)
	prRe      = regexp.MustCompile(`https?://[^\s"']+/pull/\d+`)
	patchRe   = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: ([^\s"]+)`)
)

// Artifacts are what a call made, read off its result, and nothing else: a failed result made nothing. Both are
// decoded first, as the harness keeps them (Decode). A file-writing call made the files its input names. A shell
// call made commits when its command runs a git command that makes them (the commit lines its output printed),
// pushes when it runs git push and no push of it is a dry run (the ref updates after the To line, or, with no To
// line, those of a call that ran no fetch or pull), and a pull request when it runs gh pr create without --dry-run
// (the address it printed, not one it says already exists). A commit, push or pull request the call's success
// proves it made (Shell, proven) is one even when its output shows nothing of it (a quiet -q, an address kept in
// a variable): an artifact with what is known, made but its details not shown, unless a line of the output shows
// it. What any other tool printed (an agent's report, a read, a search, a fetched page) is no artifact.
func Artifacts(call, res store.Event) []Result {
	if metaOf(res.Meta).IsError {
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
	harness := call.Harness
	if writers[tool] {
		for _, p := range Decode(harness, tool, call.Summary, "").Paths {
			add(Result{Kind: KindFile, Key: KindFile + ":" + p + "@" + moment(call.TS), Text: p, Event: call.ID()})
		}
		return out
	}
	if !toolresult.IsShell(tool) {
		return nil
	}
	d := Decode(harness, tool, call.Summary, res.Summary)
	mk := made(d.Commands)
	text := strings.TrimSuffix(d.Output, "…")
	shown := map[string]bool{}
	if mk.commits {
		for _, c := range commitRe.FindAllStringSubmatch(text, -1) {
			add(Result{Kind: KindCommit, Key: KindCommit + ":" + c[2], Text: fmt.Sprintf("commit %s on %s: %s", c[2], c[1], strings.TrimSpace(c[3]))})
			shown[KindCommit] = true
		}
	}
	if mk.pushes {
		// The ref updates after git's To line; with no To line (a tail kept only the last line), every ref update
		// of a call that ran nothing else that prints them, its remote the push's own.
		refs, remote := "", mk.remote
		if to := pushToRe.FindStringSubmatchIndex(text); to != nil {
			refs, remote = text[to[1]:], text[to[2]:to[3]]
		} else if !mk.fetches {
			refs = text
		}
		if remote == "" {
			remote = "origin"
		}
		for _, p := range pushRefRe.FindAllStringSubmatch(refs, -1) {
			shown[KindPush] = true
			if p[2] != "" {
				add(Result{Kind: KindPush, Key: KindPush + ":" + remote + " " + p[4] + "@" + p[2],
					Text: fmt.Sprintf("pushed %s -> %s (%s..%s) to %s", p[3], p[4], p[1], p[2], remote)})
			} else {
				add(Result{Kind: KindPush, Key: KindPush + ":" + remote + " " + p[4] + "@new@" + moment(res.TS),
					Text: fmt.Sprintf("pushed new branch %s -> %s to %s", p[3], p[4], remote)})
			}
		}
	}
	if mk.prs {
		for _, loc := range prRe.FindAllStringIndex(text, -1) {
			if strings.Contains(text[max(0, loc[0]-200):loc[0]], "already exists") {
				continue
			}
			u := text[loc[0]:loc[1]]
			add(Result{Kind: KindPR, Key: KindPR + ":" + u, Text: "pull request " + u})
			shown[KindPR] = true
			break
		}
	}
	// Made, its details not shown: keyed by the call's time, which a fork shares.
	at := moment(res.TS)
	if mk.madeCommit && !shown[KindCommit] && !strings.Contains(text, "nothing to commit") && !strings.Contains(text, "up to date") {
		dir := firstText(mk.commitDir, d.Workdir, call.Cwd)
		add(Result{Kind: KindCommit, Key: KindCommit + ":?" + dir + "@" + at, Text: strings.TrimSuffix("commit (hash not shown) in "+dir, " in "),
			Hidden: true})
	}
	if mk.madePush && !shown[KindPush] && !strings.Contains(text, "Everything up-to-date") {
		add(Result{Kind: KindPush, Key: KindPush + ":?" + mk.remote + "@" + at, Text: strings.TrimSuffix("push (refs not shown) to "+mk.remote, " to "),
			Hidden: true})
	}
	if mk.madePR && !shown[KindPR] && !strings.Contains(text, "already exists") {
		add(Result{Kind: KindPR, Key: KindPR + ":?@" + at, Text: "pull request (address not shown)", Hidden: true})
	}
	return out
}

func firstText(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
