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
// call (wait-guard and its kind), a harness or tool error, a command that fails the same way, a model refusal, and
// the incidents an incident detector keeps reporting. The transcripts' tool results are read for every harness, the
// mistakes log for what it alone records, and a sighting both hold counts once.
type Failures struct {
	Recur Recur // default FailuresRecur
}

// FailuresRecur: three sightings in two sessions in a week. One session that hits a wall repeats it many times; a
// second session meeting it says the wall is the setup's, not the session's.
var FailuresRecur = Recur{Events: 3, Sessions: 2}

// IncidentRecur: an incident reported ten times in a week (its repeats, over every incident of its kind). The
// incident detector counts every line a log writes, so a network that drops for a minute repeats a few times
// on its own.
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
	type seen struct {
		session, sig string
		at           time.Time
	}
	var fromTranscripts []seen
	for _, e := range results {
		k, ok := FailureOf(e.Harness, e.Tool, e.Summary)
		if !ok {
			continue
		}
		sig := signature("failures", k.String())
		occ = append(occ, occurrence{sig: sig, title: k.Title(), ev: e})
		fromTranscripts = append(fromTranscripts, seen{e.Session, sig, e.TS})
	}
	// The mistakes log: Claude Code's own record. Its guard refusals and tool errors are the transcripts' tool
	// results again, so a row within a few seconds of the same failure in the same session is the same sighting.
	mistakes, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Source: "mistakes",
		Kinds: []string{"tool-error", "deny", "held", "refusal", "turn-error"}, Limit: maxRows})
	if err != nil {
		return nil, err
	}
	sort.Slice(fromTranscripts, func(i, j int) bool { return fromTranscripts[i].at.Before(fromTranscripts[j].at) })
	same := func(session, sig string, at time.Time) bool {
		i := sort.Search(len(fromTranscripts), func(i int) bool { return !fromTranscripts[i].at.Before(at.Add(-sameSighting)) })
		for ; i < len(fromTranscripts) && !fromTranscripts[i].at.After(at.Add(sameSighting)); i++ {
			if fromTranscripts[i].session == session && fromTranscripts[i].sig == sig {
				return true
			}
		}
		return false
	}
	for _, e := range mistakes {
		k, ok := MistakeKey(e.Kind, e.Tool, e.Summary)
		if !ok {
			continue
		}
		sig := signature("failures", k.String())
		if same(e.Session, sig, e.TS) {
			continue
		}
		occ = append(occ, occurrence{sig: sig, title: k.Title(), ev: e})
	}
	out := gather("failures", occ, r)
	incidents, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Kinds: []string{"incident"}, Source: "incidents", Limit: maxRows})
	if err != nil {
		return nil, err
	}
	var inc []occurrence
	for _, e := range incidents {
		var m struct {
			Status string `json:"status"`
			Repeat int    `json:"repeat"`
		}
		json.Unmarshal([]byte(e.Meta), &m)
		if m.Status == "closed" || m.Status == "resolved" {
			continue
		}
		text := Norm(e.Summary, 90)
		if text == "" {
			continue
		}
		inc = append(inc, occurrence{sig: signature("failures", "incident:"+text), title: "Recurring incident: " + text, ev: e,
			weight: max(m.Repeat, 1)})
	}
	return append(out, gather("failures", inc, IncidentRecur)...), nil
}

// sameSighting is how far apart the mistakes log and the transcript may date one failure.
const sameSighting = 5 * time.Second

// FailureKey is what a failure is known by: where it came from (a guard, the harness, a tool, an MCP server, a
// command) and its text with what varies taken out.
type FailureKey struct {
	Origin string // guard:NAME, harness:TOOL, tool:TOOL, mcp:SERVER, exec:PROGRAM
	Text   string
}

func (k FailureKey) String() string { return k.Origin + ":" + k.Text }

// Title is the case's title for a failure of this key.
func (k FailureKey) Title() string {
	switch {
	case strings.HasPrefix(k.Origin, "guard:"):
		return strings.TrimPrefix(k.Origin, "guard:") + " keeps refusing: " + k.Text
	case strings.HasPrefix(k.Origin, "exec:"):
		return "A command keeps failing: " + k.Text
	case k.Origin == "model:refusal":
		return "The model keeps refusing: " + k.Text
	}
	return k.Origin + " keeps failing: " + k.Text
}

var (
	// guardRe finds a guard's refusal at the start of a result: its name (a word that ends in guard or gate, the
	// rulebook) and what it said.
	guardRe = regexp.MustCompile(`(?i)^(?:pretooluse:\w+ hook error:\s*)?([a-z][a-z0-9-]*(?:[ -]guard|-gate)|rulebook)\s*:\s*(.*)`)
	// heldRe is a refusal that names the guard after what it did ("held for the owner's review ... by the rulebook").
	heldRe = regexp.MustCompile(`(?i)^(held for the owner's review .{0,40}?by the ([a-z][a-z0-9-]*))`)
	// harnessRe are the harness's own refusals that name no guard.
	harnessRe = regexp.MustCompile(`(?i)^(this agent is isolated in the worktree|permission for this command was denied|claude code holds|blocked:)`)
	// exitRe is a shell tool's failed command: its exit code, then what it printed.
	exitRe = regexp.MustCompile(`(?is)^exit code -?\d+\s*(.*)`)
	// codexRe is Codex's failed script: what its error said.
	codexRe = regexp.MustCompile(`(?is)script error:\s*(.*)`)
	// hermesRe are the error and output of a Hermes tool's JSON result.
	hermesErrRe = regexp.MustCompile(`"error":\s*"((?:[^"\\]|\\.)*)"`)
	hermesOutRe = regexp.MustCompile(`"output":\s*"((?:[^"\\]|\\.)*)`)
	hermesExit  = regexp.MustCompile(`"exit_code":\s*(-?\d+)`)
	// infraRe are the marks of a failure of the setup rather than of the work: a command, an option or a file
	// that is not there, an access refused, a network or a resource that failed. A test or a build that fails, a
	// program's own traceback, is the work's.
	infraRe = regexp.MustCompile(`(?i)(command not found|not found in path|no such command|unknown (?:option|flag|command|subcommand|parameter|argument|shorthand)|` +
		`unrecognized (?:option|argument|arguments)|invalid (?:option|argument|choice|value|flag)|^usage:|\busage: |accepts \d+ arg|requires (?:an|at least|exactly) |` +
		`not a git repository|permission denied|operation not permitted|read-only file system|no space left|connection (?:refused|reset|timed out)|` +
		`could not resolve|name resolution|timed out|address already in use|cannot allocate|out of memory|segmentation fault|` +
		`modulenotfounderror|no module named|error obtaining vcs status|rate limit|too many requests|http [45]\d\d|certificate|` +
		`unauthorized|forbidden|authentication|not logged in|expired|option used in invalid context|cannot open display|enoent|spawn failed)`)
)

// FailureOf keys a tool's failed result: a guard's refusal by the guard and its first words; the harness's own
// refusal and a tool's error by the tool; a failed command only by a mark of the setup's failure (infraRe), by the
// first line that shows it. ok is false for what is no setup's failure, or says nothing to key it by: the owner's
// own no, and a call the harness cancelled because another one beside it failed.
func FailureOf(harness, tool, text string) (FailureKey, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "<tool_use_error>"))
	low := strings.ToLower(text)
	if text == "" || strings.HasPrefix(low, "the user doesn't want to take this action") || strings.HasPrefix(low, "cancelled: parallel tool call") {
		return FailureKey{}, false
	}
	if m := guardRe.FindStringSubmatch(text); m != nil {
		return FailureKey{Origin: "guard:" + slug(m[1], 30), Text: Norm(firstClause(m[2]), 80)}, true
	}
	if m := heldRe.FindStringSubmatch(text); m != nil {
		return FailureKey{Origin: "guard:" + slug(m[2], 30), Text: Norm(m[1], 80)}, true
	}
	if harnessRe.MatchString(text) {
		return FailureKey{Origin: "harness:" + slug(harness, 20), Text: Norm(firstClause(text), 80)}, true
	}
	switch {
	case strings.HasPrefix(tool, "mcp__"):
		server := strings.SplitN(strings.TrimPrefix(tool, "mcp__"), "__", 2)[0]
		return FailureKey{Origin: "mcp:" + slug(server, 30), Text: Norm(firstClause(text), 80)}, true
	case harness == "hermes" && strings.HasPrefix(text, "{"):
		if m := hermesErrRe.FindStringSubmatch(text); m != nil && m[1] != "" {
			if g := guardRe.FindStringSubmatch(m[1]); g != nil {
				return FailureKey{Origin: "guard:" + slug(g[1], 30), Text: Norm(firstClause(g[2]), 80)}, true
			}
			return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(unescape(m[1])), 80)}, true
		}
		if m := hermesExit.FindStringSubmatch(text); m != nil && m[1] != "0" {
			if o := hermesOutRe.FindStringSubmatch(text); o != nil {
				return execKey(unescape(o[1]))
			}
		}
		return FailureKey{}, false
	case harness == "codex":
		if m := codexRe.FindStringSubmatch(text); m != nil {
			return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(m[1]), 80)}, true
		}
	}
	if m := exitRe.FindStringSubmatch(text); m != nil {
		return execKey(m[1])
	}
	if strings.HasPrefix(strings.TrimSpace(text), "<") || isShell(tool) {
		return execKey(text)
	}
	// A tool's error in its own words (a read, an edit, a write the harness refused).
	return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(text), 80)}, true
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
		return FailureKey{Origin: "model:refusal", Text: Norm(firstClause(text), 80)}, true
	case "turn-error":
		if text == "" {
			return FailureKey{}, false
		}
		return FailureKey{Origin: "harness:turn", Text: Norm(firstClause(text), 80)}, true
	}
	return FailureOf("claude", tool, text) // the log is Claude Code's
}

// execKey keys a failed command by the first line of its output that marks a failure of the setup.
func execKey(out string) (FailureKey, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !infraRe.MatchString(line) {
			continue
		}
		prog := "command"
		if i := strings.Index(line, ": "); i > 0 && i < 30 && !strings.ContainsAny(line[:i], " /") {
			prog = slug(line[:i], 20)
		}
		return FailureKey{Origin: "exec:" + prog, Text: Norm(line, 80)}, true
	}
	return FailureKey{}, false
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
