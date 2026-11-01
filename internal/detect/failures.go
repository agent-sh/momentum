package detect

import (
	"context"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// Failures finds the tool and infrastructure failures that recur: a guard that keeps refusing the same form of a
// call (a hook that blocks it, a guard or gate that names itself), the harness's own refusal, a tool's error, a
// command that fails the setup's way, a model refusal, and the incidents an incident detector keeps reporting. The
// transcripts' tool results are read for every harness, with the call each answers; the mistakes log for what it
// alone records; and a sighting both hold counts once.
type Failures struct {
	Recur Recur // default FailuresRecur
}

// FailuresRecur: three sightings in two sessions in a week. One session that hits a wall repeats it many times; a
// second session meeting it says the wall is the setup's, not the session's.
var FailuresRecur = Recur{Events: 3, Sessions: 2}

// IncidentRecur: an incident's kind repeated ten times in a week, by its sightings' repeats. The incident detector
// counts every line a log writes, so a network that drops for a minute repeats a few times on its own.
var IncidentRecur = Recur{Events: 10}

func (Failures) Family() string { return "failures" }

func (f Failures) Detect(ctx context.Context, st *store.Store, w Window) ([]store.Finding, error) {
	r := f.Recur
	if r == (Recur{}) {
		r = FailuresRecur
	}
	results, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Kinds: []string{"tool_result"}, Source: store.Transcripts,
		MetaHas: `"is_error":true`, Limit: maxRows})
	if err != nil {
		return nil, err
	}
	// a failed command is known by the programs its call ran: without the calls, its keys would be other ones
	calls, err := st.CallsOf(ctx, results, callReach)
	if err != nil {
		return nil, err
	}
	var occ []occurrence
	// the transcript's failed results by session and tool, in time: a mistakes row is the same sighting as one of
	// them of its session and tool within a few seconds, whatever the text each keeps of it
	type seen struct {
		session, tool string
		at            time.Time
	}
	var failed []seen
	for _, e := range results {
		failed = append(failed, seen{e.Session, e.Tool, e.TS})
		k, ok := FailureOf(e.Harness, e.Tool, e.Summary, calls[e.ID()])
		if !ok {
			continue
		}
		occ = append(occ, occurrence{sig: signature("failures", k.String()), title: k.Title(), ev: e})
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].at.Before(failed[j].at) })
	same := func(session, tool string, at time.Time) bool {
		i := sort.Search(len(failed), func(i int) bool { return !failed[i].at.Before(at.Add(-sameSighting)) })
		for ; i < len(failed) && !failed[i].at.After(at.Add(sameSighting)); i++ {
			if failed[i].session == session && failed[i].tool == tool {
				return true
			}
		}
		return false
	}
	mistakes, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Source: "mistakes",
		Kinds: []string{"tool-error", "deny", "held", "refusal", "turn-error"}, Limit: maxRows})
	if err != nil {
		return gather("failures", occ, r), err
	}
	for _, e := range mistakes {
		if e.Tool != "" && same(e.Session, e.Tool, e.TS) {
			continue
		}
		k, ok := MistakeKey(e.Kind, e.Tool, e.Summary)
		if !ok {
			continue
		}
		occ = append(occ, occurrence{sig: signature("failures", k.String()), title: k.Title(), ev: e})
	}
	out := gather("failures", occ, r)
	inc, err := incidentSightings(ctx, st, w)
	return append(out, gather("failures", inc, IncidentRecur)...), err
}

// callReach is how long before its result a call may have started: a command runs for minutes, a background one for
// hours.
const callReach = 24 * time.Hour

// incidentSightings are the sightings of the incidents still open, each counting the repeats it saw. An incident
// is closed or resolved by its own event (its file now), which a sighting carries only when the count or the time
// moved too. A first sighting of an incident seen before the window counts once: its running total holds repeats
// from before the window, which no sighting dates.
func incidentSightings(ctx context.Context, st *store.Store, w Window) ([]occurrence, error) {
	sightings, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Kinds: []string{"incident-seen"}, Source: store.Incidents, Limit: maxRows})
	if err != nil {
		return nil, err
	}
	closed := map[string]bool{}
	var out []occurrence
	for _, e := range sightings {
		var m struct {
			Incident string `json:"incident"`
			New      int    `json:"new"`
			First    bool   `json:"first"`
			Since    string `json:"first_seen"`
		}
		json.Unmarshal([]byte(e.Meta), &m)
		if m.Incident == "" {
			continue
		}
		shut, ok := closed[m.Incident]
		if !ok {
			now, had, err := st.EventByKey(ctx, store.Incidents, m.Incident)
			if err != nil {
				return out, err
			}
			var s struct {
				Status string `json:"status"`
			}
			if had {
				json.Unmarshal([]byte(now.Meta), &s)
			}
			shut = s.Status == "closed" || s.Status == "resolved"
			closed[m.Incident] = shut
		}
		text := Norm(e.Summary, keyChars)
		if shut || text == "" {
			continue
		}
		weight := max(m.New, 1)
		if since, err := time.Parse(time.RFC3339Nano, m.Since); m.First && (err != nil || since.Before(w.Since)) {
			weight = 1
		}
		out = append(out, occurrence{sig: signature("failures", "incident:"+text), title: "An incident keeps recurring: " + text, ev: e, weight: weight})
	}
	return out, nil
}

// sameSighting is how far apart the mistakes log and the transcript may date one failure.
const sameSighting = 5 * time.Second

// keyChars is how much of a failure's text its key keeps, in characters; execChars, of a failed command's
// message, which reach bounds already.
const (
	keyChars  = 80
	execChars = 200
)

// FailureKey is what a failure is known by: where it came from (a guard or a hook, the harness, a tool, an MCP
// server, a command, the model) and its text with what varies taken out.
type FailureKey struct {
	Origin string // guard:NAME, harness:HARNESS, turn:HARNESS, tool:TOOL, mcp:SERVER, exec, model:refusal
	Text   string
}

func (k FailureKey) String() string { return k.Origin + ":" + k.Text }

// Title is the case's title for a failure of this key, its origin in words.
func (k FailureKey) Title() string {
	kind, name, _ := strings.Cut(k.Origin, ":")
	switch kind {
	case "guard":
		if name == "hook" {
			return "A hook keeps refusing calls: " + k.Text
		}
		return name + " keeps refusing calls: " + k.Text
	case "exec":
		return "A command keeps failing: " + k.Text
	case "model":
		return "The model keeps refusing: " + k.Text
	case "harness":
		return harnessName(name) + " keeps refusing calls: " + k.Text
	case "turn":
		return harnessName(name) + "'s turns keep failing: " + k.Text
	case "mcp":
		return "The " + name + " MCP server keeps failing: " + k.Text
	}
	return "The " + name + " tool keeps failing: " + k.Text
}

func harnessName(h string) string {
	switch h {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "hermes":
		return "Hermes"
	}
	return h
}

var (
	// hookRe is a hook that blocked a call: Claude Code's "PreToolUse:TOOL hook error: ", then what the hook said.
	hookRe = regexp.MustCompile(`(?is)^pretooluse:\S+ hook error:\s*(.*)`)
	// hookNameRe is how a hook's message names it: Claude Code's brackets around the hook's command, or a name of
	// the hook's own, lower case (a dashed word, or a word and guard or gate). A message's first words that end in a
	// colon ("Error: ", "Command blocked: ") name no hook.
	hookNameRe = regexp.MustCompile(`^(?:\[([^\]]*)\]:\s*)?(?:([a-z][a-z0-9]*(?:-[a-z0-9]+)+|[a-z][a-z0-9-]* (?:guard|gate)):\s+)?(.*)`)
	// guardRe is a guard's refusal at the start of a result, named by its name: a word that ends in guard or gate.
	guardRe = regexp.MustCompile(`(?i)^([a-z][a-z0-9-]*(?:[ -]guard|-gate))\s*:\s*(.*)`)
	// deniedRe is a guard's refusal as Claude Code reports a hook's deny (its tool_use_error) or the mistakes log a
	// deny: the guard's own name in lower case opens it ("x-guard: ", "ledger: "). Claude Code's own messages there
	// open with a capital ("Error: ", "Blocked: ").
	deniedRe = regexp.MustCompile(`^([a-z][a-z0-9]*(?:-[a-z0-9]+)*(?: guard| gate)?):\s+(.*)`)
	// heldRe is a refusal that names the guard after what it did ("held for the owner's review ... by the ledger").
	heldRe = regexp.MustCompile(`(?i)^(held for the owner's review .{0,40}?by the ([a-z][a-z0-9-]*))`)
	// harnessRe are the harness's own refusals that name no guard: a worktree's isolation, its permission rules and
	// modes, its classifier, its holds, its blocks.
	harnessRe = regexp.MustCompile(`(?i)^(this agent is isolated in the worktree|permission for this command was denied|claude code holds|blocked:|` +
		`permission to use \S+ (?:with command .*? )?has been denied|permission for this (?:action|tool use) (?:has been|was) denied|` +
		`this action was denied by the claude code auto mode classifier)`)
	// deniedCmdRe is Claude Code's deny of a command by a rule: what it ran stays out of the key but its programs.
	deniedCmdRe = regexp.MustCompile(`(?i)^permission to use (\S+) with command (.*?) has been denied`)
	// ansiRe is a terminal's escape sequence, which colours a line and is no part of its words.
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	// headerRe is what a harness prints before a failed command's output: Claude Code's exit code; Codex's exit code
	// and wall time, or its chunk, wall time, exit code and token count, up to its output.
	headerRe = regexp.MustCompile(`(?is)^(?:exit code:? -?\d+\s*(?:wall time:.*?output:\s*)?|chunk id:.*?output:\s*)`)
	// codexRe is Codex's failed script: what its error said.
	codexRe = regexp.MustCompile(`(?is)script error:\s*(.*)`)
	// hermesRe are the error, output and status of a Hermes tool's JSON result.
	hermesErrRe = regexp.MustCompile(`"error":\s*"((?:[^"\\]|\\.)*)"`)
	hermesOutRe = regexp.MustCompile(`"output":\s*"((?:[^"\\]|\\.)*)`)
	hermesExit  = regexp.MustCompile(`"exit_code":\s*(-?\d+)|"status":\s*"(error)"`)
	// workRe are the marks of the work itself failing: a test (Go's, pytest's, unittest's, cargo's, Jest's,
	// mocha's, node's), a panic, a lint, a compiler's error. A command whose output holds one failed for the work's
	// reasons, whatever else it printed.
	workRe = regexp.MustCompile(`(?i)(--- fail:|^fail\s|\sfail\s+[\w./-]+\s+[\d.]+s|panic: test timed out|\bpanicked at\b|` +
		`\b\w[\w./-]*\.(?:go|rs|py|ts|tsx|js|c|cc|cpp|h|java|kt|swift):\d+:\d+:|\berror\[e\d{4}\]|assertionerror|\btests? failed\b|` +
		`\b\d+ failed(?:,| in )|\bfailed [\w./-]+::\S|\bfailed \((?:failures|errors)=|\btest result: failed\b|\b\d+ failing\b|` +
		`=+ (?:failures|errors) =+|(?:^|\s)(?:fail|error): test\w* \(|[✗✖●]|lint(?:er)? (?:error|failed))`)
	// infraRe are the marks of a failure of the setup rather than of the work: a command, an option or a file that
	// is not there, an access refused, a network, a lock or a resource that failed, a usage message.
	infraRe = regexp.MustCompile(`(?i)(command not found|not found in path|no such command|no such file or directory|\bsh: \d+: [^\s:]+: not found|` +
		`unknown (?:option|flag|command|subcommand|parameter|argument|shorthand)|flag provided but not defined|` +
		`unrecognized (?:option|argument|arguments)|invalid (?:option|argument|choice|value|flag)|\busage: \S+(?: [a-z][\w-]*)* [\[<-]|accepts \d+ arg|` +
		`requires (?:an|at least|exactly) \w+ arg|` +
		`not a git repository|permission denied|operation not permitted|read-only file system|no space left|connection (?:refused|reset|timed out)|` +
		`couldn'?t connect to server|could not resolve host|temporary failure in name resolution|name or service not known|address already in use|` +
		`\b(?:etimedout|enotfound|econnrefused|econnreset|eai_again|ehostunreach|enetunreach|eaddrinuse)\b|cannot allocate memory|out of memory|` +
		`no module named|error obtaining vcs status|rate limit|too many requests|http [45]\d\d\b|` +
		`certificate (?:has )?expired|certificate verify failed|\bunauthorized\b|\bforbidden\b|authentication (?:failed|required)|not logged in|` +
		`(?:token|login|session|credentials?) (?:has |have )?expired|option used in invalid context|cannot open display|\benoent\b|spawn failed|` +
		`\bfile exists\b|database is locked|could not (?:get )?lock|could not acquire|resource temporarily unavailable|lock .{0,40}held by)`)
	// ownerNoRe is the owner's own no: Claude Code's "doesn't want to" and his denial with a word of his, and a call
	// the harness cancelled because one beside it failed.
	ownerNoRe = regexp.MustCompile(`(?i)^(the user doesn't want to (?:take this action|proceed with this tool use)|cancelled: parallel tool call|` +
		`permission for this tool use was denied\. the tool use was rejected)`)
)

// FailureOf keys a tool's failed result as the store keeps it (one line: a result's line breaks are folded), with
// the call it answers as the store keeps that ("" when unknown). A hook that blocked the call and a guard's refusal
// are keyed by the hook or guard and its first words; the harness's own refusal by the harness; a tool's error by
// the tool; a failed command, once the harness's header is off it, only by the message that holds a mark of the
// setup failing (execKey), and never when its output shows the work failing (workRe). ok is false for what is no
// setup's failure or says nothing to key it by: the owner's own no, and a call the harness cancelled because
// another one beside it failed.
func FailureOf(harness, tool, text, call string) (FailureKey, bool) {
	text = strings.TrimSpace(ansiRe.ReplaceAllString(text, ""))
	wrapped := strings.HasPrefix(text, "<tool_use_error>")
	text = strings.TrimSpace(strings.TrimPrefix(text, "<tool_use_error>"))
	if text == "" || ownerNoRe.MatchString(text) {
		return FailureKey{}, false
	}
	if m := hookRe.FindStringSubmatch(text); m != nil {
		n := hookNameRe.FindStringSubmatch(m[1])
		name := "hook"
		switch {
		case n[2] != "":
			name = slug(n[2], 30)
		case n[1] != "": // the hook's command: its last word's base name, without its ending
			if f := strings.Fields(n[1]); len(f) > 0 {
				base := path.Base(f[len(f)-1])
				name = firstOf(slug(strings.TrimSuffix(base, path.Ext(base)), 30), name)
			}
		}
		return FailureKey{Origin: "guard:" + name, Text: Norm(firstClause(n[3]), keyChars)}, true
	}
	if m := guardRe.FindStringSubmatch(text); m != nil {
		return FailureKey{Origin: "guard:" + slug(m[1], 30), Text: Norm(firstClause(m[2]), keyChars)}, true
	}
	if m := deniedRe.FindStringSubmatch(text); m != nil && wrapped {
		return FailureKey{Origin: "guard:" + slug(m[1], 30), Text: Norm(firstClause(m[2]), keyChars)}, true
	}
	if m := heldRe.FindStringSubmatch(text); m != nil {
		return FailureKey{Origin: "guard:" + slug(m[2], 30), Text: Norm(m[1], keyChars)}, true
	}
	if harnessRe.MatchString(text) {
		key := firstClause(text)
		if m := deniedCmdRe.FindStringSubmatch(text); m != nil {
			key = "permission to use " + m[1] + " with command " + programsOf(m[2]) + " has been denied"
		} else if i := strings.Index(key, ": "); i >= 0 {
			if j := strings.Index(key[i+2:], ": "); j >= 0 { // a refusal's kind, not the call it quotes after it
				key = key[:i+2+j]
			}
		}
		return FailureKey{Origin: "harness:" + slug(harness, 20), Text: Norm(key, keyChars)}, true
	}
	switch {
	case strings.HasPrefix(tool, "mcp__"):
		server := strings.SplitN(strings.TrimPrefix(tool, "mcp__"), "__", 2)[0]
		return FailureKey{Origin: "mcp:" + slug(server, 30), Text: Norm(firstClause(text), keyChars)}, true
	case harness == "hermes" && strings.HasPrefix(text, "{"):
		if m := hermesErrRe.FindStringSubmatch(text); m != nil && m[1] != "" {
			msg := unescape(m[1])
			if g := guardRe.FindStringSubmatch(msg); g != nil {
				return FailureKey{Origin: "guard:" + slug(g[1], 30), Text: Norm(firstClause(g[2]), keyChars)}, true
			}
			return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(msg), keyChars)}, true
		}
		// an exit of 0 is no failure; one past the store's cut is not in view, and the result is flagged an error
		if m := hermesExit.FindStringSubmatch(text); m == nil || m[1] != "0" {
			if o := hermesOutRe.FindStringSubmatch(text); o != nil {
				return execKey(unescape(o[1]), callPrograms(call))
			}
		}
		return FailureKey{}, false
	case harness == "codex":
		if m := codexRe.FindStringSubmatch(text); m != nil {
			return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(m[1]), keyChars)}, true
		}
	}
	if isShell(tool) || headerRe.MatchString(text) {
		return execKey(headerRe.ReplaceAllString(text, ""), callPrograms(call))
	}
	// A tool's error in its own words (a read, an edit, a write the harness refused).
	return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(text), keyChars)}, true
}

// MistakeKey keys a row of the mistakes log as FailureOf keys a result: its summary is the tool, a colon and the
// error's first line. A deny or a hold is a guard's, as a tool_use_error of Claude Code's is; a model's refusal is
// keyed by what it said; a turn's error by the harness; a row that says no more than an exit code is not keyed.
func MistakeKey(kind, tool, summary string) (FailureKey, bool) {
	text := strings.TrimSpace(strings.TrimPrefix(summary, tool+":"))
	switch kind {
	case "refusal":
		if text == "" {
			return FailureKey{}, false
		}
		return FailureKey{Origin: "model:refusal", Text: Norm(firstClause(text), keyChars)}, true
	case "turn-error":
		if text == "" {
			return FailureKey{}, false
		}
		return FailureKey{Origin: "turn:claude", Text: Norm(firstClause(text), keyChars)}, true
	case "deny", "held":
		if text != "" && !strings.HasPrefix(text, "<tool_use_error>") {
			text = "<tool_use_error>" + text
		}
	}
	return FailureOf("claude", tool, text, "") // the log is Claude Code's, and keeps no call
}

// reach is how far before its mark, in the masked output, a failed command's message may start: a program's name
// further back opens another message.
const reach = 100

// sentenceRe ends a sentence: a command's message does not start before it.
var sentenceRe = regexp.MustCompile(`[.;!?]\s`)

// tailRe is the word after a mark when a colon follows it.
var tailRe = regexp.MustCompile(`^:\s+([^\s,;]{1,40})`)

// execKey keys a failed command's output by the message that holds its first mark of the setup failing (infraRe)
// outside a quoted value, read in the output with what varies taken out (mask) but the mark, so a path or a
// quoted name is one letter whatever its length:
//   - The message ends at the mark, with what follows its colon: another mark (Node's "ENOENT: no such file or
//     directory"), or one word (zsh's "command not found: foo", the host that does not resolve). A usage message
//     ends at the program it names.
//   - It starts at the name of a program the call ran, the closest before the mark within reach ("ls: ", "curl:
//     "): a program's message opens with its name, and its later words may end in a colon too ("after 0 ms: "),
//     though an exception's name after it ("ModuleNotFoundError: ") opens a message of its own. Else it starts at
//     the name closest before the mark ("connect: "), else at the mark; never before a sentence's end.
//
// The key holds no program of its own: where a program is known, its name opens the message, and a word of the
// output alone does not say that it named one. An output that shows the work failing has no key.
func execKey(out string, programs map[string]bool) (FailureKey, bool) {
	out = strings.Join(strings.Fields(out), " ")
	if workRe.MatchString(out) {
		return FailureKey{}, false
	}
	loc := firstMark(out)
	if loc == nil {
		return FailureKey{}, false
	}
	before, mark, after := out[:loc[0]], markText(out[loc[0]:loc[1]]), out[loc[1]:]
	if u, ok := strings.CutPrefix(strings.ToLower(out[loc[0]:loc[1]]), "usage: "); ok { // a usage message names its program first, by its path or its name
		prog := strings.TrimRight(path.Base(strings.Trim(strings.Fields(u)[0], "'\"`")), ".,;:")
		return FailureKey{Origin: "exec", Text: cutWords("usage: "+strings.ToLower(plain.Line(prog)), execChars)}, true
	}
	floor := 0
	if ms := sentenceRe.FindAllStringIndex(before, -1); ms != nil {
		floor = ms[len(ms)-1][1]
	}
	within := func(p int) bool { return len(mask(before[p:])) <= reach }
	names := namesIn(before, floor)
	start := len(before)
	for k := len(names) - 1; k >= 0 && within(names[k][0]); k-- {
		if programs[strings.ToLower(before[names[k][0]:names[k][1]])] {
			start = names[k][0]
			for _, n := range names[k+1:] { // an exception's name after the program's opens a message of its own
				if exceptionRe.MatchString(before[n[0]:n[1]]) {
					start = n[0]
				}
			}
			break
		}
	}
	if start == len(before) && len(names) > 0 && within(names[len(names)-1][0]) {
		start = names[len(names)-1][0]
	}
	text := mask(before[start:])
	if text != "" && strings.HasSuffix(before, " ") {
		text += " "
	}
	text += mark
	rest := clipBytes(after, 200)
	if c, ok := strings.CutPrefix(rest, ": "); ok {
		if next := firstMark(c); next != nil && next[0] == 0 { // what follows the colon is another mark
			text += ": " + markText(c[:next[1]])
		} else if t := tailRe.FindStringSubmatch(mask(rest)); t != nil {
			if w := strings.TrimRight(t[1], ".)'\""); w != "" {
				text += ": " + w
			}
		}
	}
	return FailureKey{Origin: "exec", Text: cutWords(text, execChars)}, true
}

// exceptionRe is an exception's name: Python's, Java's, JavaScript's (ModuleNotFoundError, IOException).
var exceptionRe = regexp.MustCompile(`^(?:[\w.]+\.)?[A-Z][A-Za-z0-9]*(?:Error|Exception)$`)

// quotedRe are the quoted values of an output, inside which a mark is a value's word (a file named after an
// error, a line of code), not what the command said.
var quotedRe = regexp.MustCompile("'[^'\n]*'|\"[^\"\n]*\"|`[^`\n]*`|‘[^’\n]*’|“[^”\n]*”")

// firstMark is where the first mark of the setup failing outside a quoted value is in out, or nil.
func firstMark(out string) []int {
	quoted := quotedRe.FindAllStringIndex(out, -1)
	for _, m := range infraRe.FindAllStringIndex(out, -1) {
		in := false
		for _, q := range quoted {
			if m[0] > q[0] && m[0] < q[1] {
				in = true
				break
			}
		}
		if !in {
			return m
		}
	}
	return nil
}

// markText is a mark as a key keeps it: masked (a line's number, a lock's path), but for an HTTP status, which
// says what failed.
func markText(raw string) string {
	if m := strings.ToLower(strings.TrimSpace(raw)); strings.HasPrefix(m, "http ") {
		return m
	}
	return mask(raw)
}

// namesIn are the names before a colon and a space in s from floor on, in order, as [start, end) pairs: a word
// that opens at a space (a program's, an exception's, a context's), not a value (a path, a file's or an id).
func namesIn(s string, floor int) [][2]int {
	var out [][2]int
	for i := floor; i < len(s); i++ {
		if s[i] != ':' || i+1 < len(s) && s[i+1] != ' ' {
			continue
		}
		j := i
		for j > floor && nameByte(s[j-1]) {
			j--
		}
		if j == i || i-j > 40 || j > floor && s[j-1] != ' ' || !(s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
			continue
		}
		if masked(mask(s[j:i])) {
			continue
		}
		out = append(out, [2]int{j, i})
	}
	return out
}

func nameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.' || b == '+' || b == '-'
}

// clipBytes is s cut to at most n bytes, at a character.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// callRe is a shell call's command in its input as the store keeps it, which may be cut short: Claude Code's, pi's
// and Hermes's "command", Codex's "cmd".
var callRe = regexp.MustCompile(`"(?:command|cmd)"\s*:\s*"((?:[^"\\]|\\.)*)`)

var (
	// shellSepRe is what separates a command line's commands.
	shellSepRe = regexp.MustCompile("&&|\\|\\||[|;&\n(){}`]|\\$\\(")
	assignRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	countRe    = regexp.MustCompile(`^\d+(?:\.\d+)?[smhd]?$`)
	// wrappers run the command after them, past their options and a count.
	wrappers = map[string]bool{"sudo": true, "env": true, "nice": true, "ionice": true, "nohup": true, "time": true, "timeout": true,
		"exec": true, "command": true, "builtin": true, "xargs": true, "stdbuf": true, "chrt": true, "taskset": true, "systemd-run": true,
		"!": true, "if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true}
)

// programsOf names the programs of a command line, joined with "+", but cd when they are more: what a rule that
// refused it was about.
func programsOf(cmd string) string {
	var out []string
	for p := range commandPrograms(cmd) {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > 1 {
		out = slices.DeleteFunc(out, func(p string) bool { return p == "cd" })
	}
	if len(out) == 0 {
		return "a command"
	}
	return strings.Join(out, "+")
}

// callPrograms are the programs a shell call runs, by name: the first word of each command of its input, past
// assignments and the wrappers that run another program, as a base name. The store keeps a call's first 240
// bytes, so the programs of a long command's end are not known; nor is a here-document's text a command.
func callPrograms(call string) map[string]bool {
	m := callRe.FindStringSubmatch(call)
	if m == nil {
		return nil
	}
	return commandPrograms(unescape(m[1]))
}

// commandPrograms are the programs a command line runs, by name.
func commandPrograms(cmd string) map[string]bool {
	if i := strings.Index(cmd, "<<"); i >= 0 {
		cmd = cmd[:i]
	}
	out := map[string]bool{}
	for _, part := range shellSepRe.Split(cmd, -1) {
		words := strings.Fields(part)
		for len(words) > 0 {
			switch w := words[0]; {
			case assignRe.MatchString(w):
				words = words[1:]
				continue
			case wrappers[w]:
				words = words[1:]
				for len(words) > 0 && (strings.HasPrefix(words[0], "-") || countRe.MatchString(words[0]) || assignRe.MatchString(words[0])) {
					words = words[1:]
				}
				continue
			}
			break
		}
		if len(words) > 0 {
			if name := strings.ToLower(path.Base(strings.Trim(words[0], `"'`))); name != "" && name != "." && name != "/" {
				out[name] = true
			}
		}
	}
	return out
}

// firstClause is a text up to the end of its first sentence or line.
func firstClause(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i > 20 {
		s = s[:i]
	}
	return s
}

// isShell says a tool runs shell commands: Claude Code's Bash and PowerShell, pi's bash and its output, Codex's
// commands and the stdin it writes to them, Hermes's terminal and code.
func isShell(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash", "bash_output", "powershell", "exec", "exec_command", "shell", "local_shell", "write_stdin", "terminal", "execute_code":
		return true
	}
	return false
}

// unescape reads a JSON string's body; one cut short reads as it is.
func unescape(s string) string {
	var out string
	if json.Unmarshal([]byte(`"`+s+`"`), &out) == nil {
		return out
	}
	return strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(s)
}

func firstOf(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
