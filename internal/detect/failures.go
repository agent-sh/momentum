package detect

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Failures finds the tool and infrastructure failures that recur: a guard that keeps refusing the same form of a
// call (wait-guard and its kind, any hook that blocks a call), a harness or tool error, a command that fails the
// setup's way, a model refusal, and the incidents an incident detector keeps reporting. The transcripts' tool
// results are read for every harness, the mistakes log for what it alone records, and a sighting both hold
// counts once.
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
		k, ok := FailureOf(e.Harness, e.Tool, e.Summary)
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
	sightings, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Kinds: []string{"incident-seen"}, Source: "incidents", Limit: maxRows})
	if err != nil {
		return out, err
	}
	var inc []occurrence
	for _, e := range sightings {
		var m struct {
			Status string `json:"status"`
			New    int    `json:"new"`
		}
		json.Unmarshal([]byte(e.Meta), &m)
		if m.Status == "closed" || m.Status == "resolved" {
			continue
		}
		text := Norm(e.Summary, keyChars)
		if text == "" {
			continue
		}
		inc = append(inc, occurrence{sig: signature("failures", "incident:"+text), title: "An incident keeps recurring: " + text, ev: e,
			weight: max(m.New, 1)})
	}
	return append(out, gather("failures", inc, IncidentRecur)...), nil
}

// sameSighting is how far apart the mistakes log and the transcript may date one failure.
const sameSighting = 5 * time.Second

// keyChars is how much of a failure's text its key keeps, in characters.
const keyChars = 80

// FailureKey is what a failure is known by: where it came from (a guard or a hook, the harness, a tool, an MCP
// server, a command, the model) and its text with what varies taken out.
type FailureKey struct {
	Origin string // guard:NAME, harness:HARNESS, tool:TOOL, mcp:SERVER, exec:PROGRAM, model:refusal
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
		if name == "command" {
			return "A command keeps failing: " + k.Text
		}
		return name + " keeps failing: " + k.Text
	case "model":
		return "The model keeps refusing: " + k.Text
	case "harness":
		return harnessName(name) + " keeps refusing calls: " + k.Text
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
	// hookRe is a hook that blocked a call: Claude Code's "PreToolUse:TOOL hook error:", then the hook's name and
	// its message, or its message alone.
	hookRe = regexp.MustCompile(`(?i)^pretooluse:\S+ hook error:\s*(?:([a-z][a-z0-9 .-]{1,40}?):\s+)?(.*)`)
	// guardRe finds a guard's refusal at the start of a result: its name (a word that ends in guard or gate, the
	// rulebook) and what it said.
	guardRe = regexp.MustCompile(`(?i)^([a-z][a-z0-9-]*(?:[ -]guard|-gate)|rulebook)\s*:\s*(.*)`)
	// heldRe is a refusal that names the guard after what it did ("held for the owner's review ... by the rulebook").
	heldRe = regexp.MustCompile(`(?i)^(held for the owner's review .{0,40}?by the ([a-z][a-z0-9-]*))`)
	// harnessRe are the harness's own refusals that name no guard.
	harnessRe = regexp.MustCompile(`(?i)^(this agent is isolated in the worktree|permission for this command was denied|claude code holds|blocked:)`)
	// headerRe is what a harness prints before a failed command's output: Claude Code's exit code; Codex's chunk,
	// wall time, exit code and token count, up to its output.
	headerRe = regexp.MustCompile(`(?is)^(?:exit code -?\d+\s*|chunk id:.*?output:\s*)`)
	// codexRe is Codex's failed script: what its error said.
	codexRe = regexp.MustCompile(`(?is)script error:\s*(.*)`)
	// hermesRe are the error and output of a Hermes tool's JSON result.
	hermesErrRe = regexp.MustCompile(`"error":\s*"((?:[^"\\]|\\.)*)"`)
	hermesOutRe = regexp.MustCompile(`"output":\s*"((?:[^"\\]|\\.)*)`)
	hermesExit  = regexp.MustCompile(`"exit_code":\s*(-?\d+)`)
	// workRe are the marks of the work itself failing: a test, a lint, a compiler's error. A command whose output
	// holds one failed for the work's reasons, whatever else it printed.
	workRe = regexp.MustCompile(`(?i)(--- fail:|^fail\s|\sfail\s+[\w./-]+\s+[\d.]+s|panic: test timed out|\b\w[\w./-]*\.(?:go|rs|py|ts|tsx|js|c|cc|cpp|h|java|kt|swift):\d+:\d+:|` +
		`\berror\[e\d{4}\]|assertionerror|\btests? failed\b|\d+ failed, \d+ passed|✗|lint(?:er)? (?:error|failed))`)
	// infraRe are the marks of a failure of the setup rather than of the work: a command, an option or a file that
	// is not there, an access refused, a network, a lock or a resource that failed, a usage message.
	infraRe = regexp.MustCompile(`(?i)(command not found|not found in path|no such command|no such file or directory|unknown (?:option|flag|command|subcommand|parameter|argument|shorthand)|` +
		`unrecognized (?:option|argument|arguments)|invalid (?:option|argument|choice|value|flag)|\busage: |accepts \d+ arg|requires (?:an|at least|exactly) \w+ arg|` +
		`not a git repository|permission denied|operation not permitted|read-only file system|no space left|connection (?:refused|reset|timed out)|` +
		`could not resolve host|temporary failure in name resolution|name or service not known|address already in use|cannot allocate memory|out of memory|` +
		`segmentation fault|modulenotfounderror|no module named|error obtaining vcs status|rate limit|too many requests|http [45]\d\d\b|` +
		`certificate (?:has )?expired|certificate verify failed|\bunauthorized\b|\bforbidden\b|authentication (?:failed|required)|not logged in|` +
		`(?:token|login|session|credentials?) (?:has |have )?expired|option used in invalid context|cannot open display|\benoent\b|spawn failed|` +
		`\.lock'?: file exists|database is locked|could not (?:lock|acquire)|resource temporarily unavailable|lock .{0,40}held by)`)
	ownerNoRe = regexp.MustCompile(`(?i)^(the user doesn't want to (?:take this action|proceed with this tool use)|cancelled: parallel tool call)`)
)

// FailureOf keys a tool's failed result as the store keeps it (one line: a result's line breaks are folded). A
// hook that blocked the call and a guard's refusal are keyed by the hook or guard and its first words; the
// harness's own refusal by the harness; a tool's error by the tool; a failed command, once the harness's header is
// off it, only by the clause that holds a mark of the setup failing (infraRe), and never when its output shows the
// work failing (workRe). ok is false for what is no setup's failure or says nothing to key it by: the owner's own
// no, and a call the harness cancelled because another one beside it failed.
func FailureOf(harness, tool, text string) (FailureKey, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "<tool_use_error>"))
	if text == "" || ownerNoRe.MatchString(text) {
		return FailureKey{}, false
	}
	if m := hookRe.FindStringSubmatch(text); m != nil {
		name := "hook"
		if m[1] != "" {
			name = slug(m[1], 30)
		}
		return FailureKey{Origin: "guard:" + name, Text: Norm(firstClause(m[2]), keyChars)}, true
	}
	if m := guardRe.FindStringSubmatch(text); m != nil {
		return FailureKey{Origin: "guard:" + slug(m[1], 30), Text: Norm(firstClause(m[2]), keyChars)}, true
	}
	if m := heldRe.FindStringSubmatch(text); m != nil {
		return FailureKey{Origin: "guard:" + slug(m[2], 30), Text: Norm(m[1], keyChars)}, true
	}
	if harnessRe.MatchString(text) {
		return FailureKey{Origin: "harness:" + slug(harness, 20), Text: Norm(firstClause(text), keyChars)}, true
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
		if m := hermesExit.FindStringSubmatch(text); m != nil && m[1] != "0" {
			if o := hermesOutRe.FindStringSubmatch(text); o != nil {
				return execKey(unescape(o[1]))
			}
		}
		return FailureKey{}, false
	case harness == "codex":
		if m := codexRe.FindStringSubmatch(text); m != nil {
			return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(m[1]), keyChars)}, true
		}
	}
	if isShell(tool) || headerRe.MatchString(text) {
		return execKey(headerRe.ReplaceAllString(text, ""))
	}
	// A tool's error in its own words (a read, an edit, a write the harness refused).
	return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(text), keyChars)}, true
}

// MistakeKey keys a row of the mistakes log as FailureOf keys a result: its summary is the tool, a colon and the
// error's first line. A model's refusal is keyed by what it said; a row that says no more than an exit code is not.
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
		return FailureKey{Origin: "harness:claude", Text: Norm(firstClause(text), keyChars)}, true
	}
	return FailureOf("claude", tool, text) // the log is Claude Code's
}

// programRe is a program's name opening its message ("tail: ", "fatal: ", "gh: "): a word with no dot or slash,
// at the start or after a space, then a colon and a space.
var programRe = regexp.MustCompile(`(?i)(?:^|\s)([a-z][\w-]{0,23}):\s`)

// execKey keys a failed command's output by the clause around its first mark of the setup failing: from the
// program's name before it (a program prints its name, a colon, its message) or a few words before it, to a few
// words after it. An output that shows the work failing has no key.
func execKey(out string) (FailureKey, bool) {
	out = strings.Join(strings.Fields(out), " ")
	if workRe.MatchString(out) {
		return FailureKey{}, false
	}
	loc := infraRe.FindStringIndex(out)
	if loc == nil {
		return FailureKey{}, false
	}
	start := max(0, loc[0]-60)
	prog := "command"
	if strings.HasPrefix(strings.ToLower(out[loc[0]:loc[1]]), "usage") {
		start, prog = loc[0], "usage" // a usage message names its program after it
	} else if ms := programRe.FindAllStringSubmatchIndex(out[start:loc[0]], -1); len(ms) > 0 {
		m := ms[len(ms)-1]
		start, prog = start+m[2], slug(out[start+m[2]:start+m[3]], 20)
	} else if i := strings.LastIndexAny(out[start:loc[0]], ".;"); i >= 0 {
		start += i + 1
	} else if sp := strings.IndexByte(out[start:loc[0]], ' '); start > 0 && sp >= 0 {
		start += sp + 1 // at a word
	}
	end := min(len(out), loc[1]+40)
	if i := strings.IndexAny(out[loc[1]:end], ".;"); i >= 0 {
		end = loc[1] + i
	}
	return FailureKey{Origin: "exec:" + prog, Text: Norm(out[start:end], keyChars)}, true
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

func isShell(tool string) bool {
	switch tool {
	case "Bash", "exec", "exec_command", "shell", "terminal", "execute_code", "PowerShell":
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
