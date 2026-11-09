package detect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/toolresult"
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
	var occ []occurrence
	// the transcript's failed results by session and tool, in time: a mistakes row is the same sighting as one of
	// them of its session and tool within a few seconds, whatever the text each keeps of it
	type seen struct {
		session, tool string
		at            time.Time
	}
	var failed []seen
	// when each thread (a session, or a subagent of it) wrote each path with success, from the results read so far:
	// the pages run newest first, so every write after a result is in its page or in one before it
	type wrote struct{ session, agent, path string }
	written := map[wrote][]time.Time{}
	// Successful results must not displace older unflagged failures at maxRows.
	var before *store.Event
	for {
		results, err := st.Events(ctx, store.Query{Since: w.Since, Until: w.Until, Kinds: []string{"tool_result"},
			Source: store.Transcripts, Limit: resultPage, Before: before})
		if err != nil {
			return gather("failures", occ, r), err
		}
		var candidates, writes []store.Event
		for _, e := range results {
			var m struct {
				IsError bool `json:"is_error"`
			}
			json.Unmarshal([]byte(e.Meta), &m)
			if toolresult.Failed(e.Harness, e.Tool, e.Summary, m.IsError) {
				candidates = append(candidates, e)
			} else if !toolresult.Cancelled(e.Summary) && (bashShell(e.Tool) || fileWrote(e.Harness, e.Tool, e.Summary)) {
				// a write the owner declined or the harness cancelled did not write: Failed leaves those out, flagged or not
				writes = append(writes, e)
			}
		}
		calls, err := st.CallsOf(ctx, slices.Concat(candidates, writes), callReach)
		if err != nil {
			return gather("failures", occ, r), err
		}
		for _, e := range writes {
			c := calls[e.ID()]
			for _, p := range writtenPaths(e.Harness, e.Tool, c.Input, e.Summary, c.Cwd) {
				k := wrote{e.Session, e.Agent, p}
				written[k] = append(written[k], e.TS)
			}
		}
		for _, e := range candidates {
			failed = append(failed, seen{e.Session, e.Tool, e.TS})
			call := calls[e.ID()]
			k, ok := failureKey(e.Harness, e.Tool, e.Summary, call)
			if !ok {
				continue
			}
			// a read of a file its thread writes within the settle time found it not there yet: no failure; one younger
			// than the settle time may have its write still to come. The read names its file as its call spells it,
			// as a write does, not as its key holds it, and a relative one also in its call's directory (readAs).
			if k.missed() {
				soon := func(t time.Time) bool { return t.After(e.TS) && !t.After(e.TS.Add(readSettle)) }
				if settling(e.TS, w) || slices.ContainsFunc(readAs(e.Harness, call), func(p string) bool {
					return slices.ContainsFunc(written[wrote{e.Session, e.Agent, p}], soon)
				}) {
					continue
				}
			}
			occ = append(occ, occurrence{sig: signature("failures", k.String()), title: k.Title(), ev: e})
		}
		if len(results) < resultPage {
			break
		}
		last := results[len(results)-1]
		before = &last
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
		// MistakeKey keys no read that found no file, which the transcript does not hold: the store keeps none of the
		// log's input for a tool error, so no call names its path
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

// readTools are the harnesses' file tools that read a file, by their names in lower case: Claude Code's, pi's,
// OpenCode's and Kimi's Read, Hermes's, Grok's and Codex's read_file.
var readTools = map[string]bool{"read": true, "read_file": true}

// fileWrites are the file tools' own words for a write that landed, per harness and tool (in lower case), as each
// harness writes them: only a result in one of them wrote its file. A refusal, a failure in the tool's own words, a
// compaction's summary or another tool's result is none (SPEC, Failures).
var fileWrites = map[string]*regexp.Regexp{
	"claude/write":         claudeWrote,
	"claude/edit":          claudeWrote,
	"kimi/write":           regexp.MustCompile(`^(?:Wrote|Appended) \d+ bytes to `),
	"kimi/edit":            regexp.MustCompile(`^Replaced \d+ occurrences? in `),
	"hermes/write_file":    regexp.MustCompile(`^\{"bytes_written": ?\d+[,}]`),
	"hermes/patch":         regexp.MustCompile(`^\{"success": ?true[,}]`),
	"opencode/write":       regexp.MustCompile(`^(?:Wrote|Created) file successfully\b`),
	"opencode/edit":        regexp.MustCompile(`^(?:Edit applied successfully\.|Edited \S.* \(\d+ replacements?\))`),
	"opencode/apply_patch": patchApplied,
	"opencode/patch":       patchApplied,
	"pi/write":             regexp.MustCompile(`^Successfully wrote (?:\d+ bytes )?to `),
	"pi/edit":              regexp.MustCompile(`^Successfully replaced (?:text|\d+ block\(s\)) in `),
	"codex/apply_patch":    regexp.MustCompile(`^(?:Exit code: 0|Process exited with code 0)\b.*\bSuccess\. Updated the following files:`),
}

var (
	// claudeWrote is Claude Code's Write and Edit: a file created, or one updated.
	claudeWrote = regexp.MustCompile(`^(?:File created successfully at: |The file \S.* has been updated(?: successfully)?\.)`)
	// patchApplied is a patch that applied: OpenCode's apply_patch and patch.
	patchApplied = regexp.MustCompile(`^Success\. Updated the following files:`)
	// hermesErrorKeyRe is a Hermes result's error field, its value after it; hermesNoChangeRe a patch that wrote
	// nothing.
	hermesErrorKeyRe = regexp.MustCompile(`"error": ?`)
	hermesNoChangeRe = regexp.MustCompile(`"no_change": ?true`)
	// patchLineRe is a patch's header for a file it adds or updates, at column 0, with the Move to line after it,
	// each path to its line's end; patchFoldedRe the same at the start of a text the store folded onto one line,
	// after its opening line, each path up to where the patch's next line begins (another header or its end, a line it
	// adds or removes, a chunk's @@).
	patchLineRe   = regexp.MustCompile(`(?m)^\*\*\* (Add|Update) File: *(\S.*)$(?:\n\*\*\* Move to: *(\S.*)$)?`)
	patchFoldedRe = regexp.MustCompile(`^(?:\*\*\* Begin Patch )?\*\*\* (Add|Update) File: (\S.*?)(?: \*\*\* Move to: (\S.*?))?(?: \*\*\*| \+| -| @@)`)
)

// fileWrote says a file tool's result is its harness's own word for a write that landed (fileWrites), and, for
// Hermes, as Hermes reads it: no error that is anything but null, false or empty, and no patch that changed nothing.
func fileWrote(harness, tool, result string) bool {
	re := fileWrites[harness+"/"+strings.ToLower(tool)]
	return re != nil && re.MatchString(result) && !(harness == "hermes" && (hermesError(result) || hermesNoChangeRe.MatchString(result)))
}

// hermesError says a Hermes result's error field holds anything but null, false or an empty string.
func hermesError(result string) bool {
	for _, m := range hermesErrorKeyRe.FindAllStringIndex(result, -1) {
		if v := result[m[1]:]; !strings.HasPrefix(v, "null") && !strings.HasPrefix(v, "false") && !strings.HasPrefix(v, `""`) {
			return true
		}
	}
	return false
}

// modeKeys are Hermes's patch field that says how it patches.
var modeKeys = map[string]bool{"mode": true}

// missForms are each harness's read tool's own words for a file that is not there, at its error's start, as its
// source writes them: Claude Code's Read, Kimi's Read (the path in quotes), Grok's read_file (after its envelope's
// "Error: "), OpenCode's read (after "Error: " as releases up to 1.2.27 stored it), Hermes's read_file (its JSON's error
// field, its missing file's or Python's), pi's read (Node's error for the access it makes first), Codex's read_file.
// Access refused, a guard's or the harness's refusal, and any other error that says something does not exist (Hermes's
// terminal directory gone) are none of them.
var missForms = map[string]*regexp.Regexp{
	"claude":   regexp.MustCompile(`^File does not exist\.`),
	"kimi":     regexp.MustCompile(`^".*" does not exist\.`),
	"grok":     regexp.MustCompile(`^Error: .* does not exist\.`),
	"opencode": regexp.MustCompile(`^(?:Error: )?File not found: `),
	"hermes":   regexp.MustCompile(`^(?:File not found: |\[Errno 2\] No such file or directory)`),
	"pi":       regexp.MustCompile(`^ENOENT: no such file or directory`),
	"codex":    regexp.MustCompile(`^failed to read file: No such file or directory`),
}

// missNames are the file a harness's words for a miss name (the first group that matched): Kimi's as its call spelled
// it; Grok's, OpenCode's, Hermes's and pi's as the tool resolved it; Python's errno's the file it could not open, which
// need not be the read's. Claude Code's and Codex's name none.
var missNames = map[string]*regexp.Regexp{
	"kimi":     regexp.MustCompile(`^"(.*)" does not exist\.`),
	"grok":     regexp.MustCompile(`^Error: (.*?) does not exist\.`),
	"opencode": regexp.MustCompile(`(?s)^(?:Error: )?File not found: (.*?)(?:\s+Did you mean one of these\?.*)?$`),
	"hermes":   regexp.MustCompile(`^(?:File not found: (.*)|\[Errno 2\] No such file or directory: (?:'(.*)'|"(.*)"))$`),
	"pi":       regexp.MustCompile(`^ENOENT: no such file or directory, access '(.*)'`),
}

// pathKeys are the fields of a file tool's input that name its file; readPathKeys, a read tool's, Grok's
// target_file too.
var (
	pathKeys     = map[string]bool{"file_path": true, "filePath": true, "path": true}
	readPathKeys = map[string]bool{"file_path": true, "filePath": true, "path": true, "target_file": true}
)

// missWords are a failed result's words when they are a read tool's own error that no file is at its path, in its
// harness's words (missForms): Claude Code's in its tool_use_error or not, Hermes's in its JSON's error field.
func missWords(harness, tool, text string) (string, bool) {
	re := missForms[harness]
	if re == nil || !readTools[strings.ToLower(tool)] {
		return "", false
	}
	text = strings.TrimSpace(text)
	if harness == "hermes" { // its error field, in a result the store cut after it too
		var r struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(text), &r) == nil {
			text = r.Error
		} else if m := hermesErrRe.FindStringSubmatch(text); m != nil {
			text = unescape(m[1])
		} else {
			return "", false
		}
	}
	text = strings.TrimPrefix(text, "<tool_use_error>")
	return text, re.MatchString(text)
}

// missOf is the path a read tool's failed result missed, before missedPath, and whether the result is a miss: its
// harness's words for one (missWords), and for Python's errno a name that is the read's own path, since Hermes passes
// on the errno of a terminal whose directory is gone, which names that directory for any read. The path is the call's
// (pi's without the leading @ it strips); a relative one is read in the call's directory when that is a whole path, but
// for Hermes, whose read_file resolves it in its terminal's directory, which a cd moves; else it is the whole path the
// error names, if it names one; else it stays as spelled. A path that opens with ~ is the home's. A miss whose call
// names no path has none.
func missOf(harness, tool, text string, c store.Call) (string, bool) {
	words, ok := missWords(harness, tool, text)
	if !ok {
		return "", false
	}
	named := ""
	if re := missNames[harness]; re != nil {
		if m := re.FindStringSubmatch(words); m != nil {
			named = firstOf(m[1:]...)
		}
	}
	p, _, _ := callField(c.Input, readPathKeys)
	if harness == "pi" {
		p = strings.TrimPrefix(p, "@")
	}
	if harness == "hermes" && strings.HasPrefix(words, "[Errno 2]") && !names(named, p) {
		return "", false
	}
	switch {
	case p == "", path.IsAbs(p), p == "~", strings.HasPrefix(p, "~/"):
	case harness != "hermes" && path.IsAbs(c.Cwd):
		p = path.Join(c.Cwd, p)
	case path.IsAbs(named):
		p = named
	}
	return p, true
}

// names says the file an error names is a read's path p: the same path, ~ read as the home, or, for a relative p, that
// path in some directory, its leading .. segments off (the directory they climb to is not known).
func names(named, p string) bool {
	if named == "" || p == "" {
		return false
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = paths.Home() + p[1:]
	}
	named, p = path.Clean(named), path.Clean(p)
	if named == p {
		return true
	}
	if path.IsAbs(p) {
		return false
	}
	for strings.HasPrefix(p, "../") {
		p = p[len("../"):]
	}
	return strings.HasSuffix(named, "/"+p)
}

// readAs are the paths a read names its file by, for its thread's write to clear it: its call's path as spelled, and
// a relative one (not the home's) also in its call's directory, when that is a whole path, as missOf reads it, but for
// Hermes, whose read_file resolves it in its terminal's directory, which a cd moves.
func readAs(harness string, c store.Call) []string {
	p, _, _ := callField(c.Input, pathKeys)
	out := []string{p}
	if p != "" && !path.IsAbs(p) && p != "~" && !strings.HasPrefix(p, "~/") && harness != "hermes" && path.IsAbs(c.Cwd) {
		out = append(out, path.Join(c.Cwd, p))
	}
	return out
}

// missedPath is the path a read found no file at as its key holds it: cleaned (no . or .. left in it) and the home
// folded to ~, no more. A path longer than keyChars characters, or one that is not plain text on one line, is kept as
// its end and a short hash of the whole, so two such paths stay two.
func missedPath(p string) string {
	p = path.Clean(p)
	if h := path.Clean(paths.Home()); path.IsAbs(h) && h != "/" {
		if p == h {
			p = "~"
		} else if rest, ok := strings.CutPrefix(p, h+"/"); ok {
			p = "~/" + rest
		}
	}
	if utf8.RuneCountInString(p) <= keyChars && plain.Line(p) == p {
		return p
	}
	sum := sha256.Sum256([]byte(p))
	tag := " #" + hex.EncodeToString(sum[:4])
	end, cut := []rune(plain.Line(p)), ""
	if n := keyChars - len(tag) - 1; len(end) > n {
		end, cut = end[len(end)-n:], "…"
	}
	return cut + string(end) + tag
}

// settling says a read at ts is still inside its hour at the window's end, as the store reads it (to the millisecond):
// its write may yet come.
func settling(ts time.Time, w Window) bool {
	return !ts.Before(w.Until.Truncate(time.Millisecond).Add(-readSettle))
}

// callField is the first field of keys a tool call's input names, as the store keeps the call (its first bytes),
// read up to where the store cut it: its text ("" for another value), whether the input names one before the cut,
// and whether it names none in an input the store kept whole.
func callField(call string, keys map[string]bool) (value string, named, absent bool) {
	raw, named, absent := callValue(call, keys)
	json.Unmarshal(raw, &value)
	return value, named, absent
}

// callValue is callField's field as its JSON value.
func callValue(call string, keys map[string]bool) (value json.RawMessage, named, absent bool) {
	d := json.NewDecoder(strings.NewReader(call))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, false, false
	}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, false, false
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, false, false
		}
		if key, _ := t.(string); keys[key] {
			return v, true, false
		}
	}
	return nil, false, true
}

// callReach is how long before its result a call may have started: a command runs for minutes, a background one for
// hours.
const callReach = 24 * time.Hour

const resultPage = 4000

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

// keyChars is how much of a failure's text its key keeps, in characters, and of a missed path (missedPath);
// execChars, of a failed command's message, which reach bounds already.
const (
	keyChars  = 80
	execChars = 200
)

// FailureKey is what a failure is known by: where it came from (a guard or a hook, the harness, a tool, an MCP
// server, a command, the model) and its text with what varies taken out; a read that found no file, by the read
// tool and the path it missed, and a command's, by the path its message names.
type FailureKey struct {
	Origin string // guard:NAME, harness:HARNESS, turn:HARNESS, tool:TOOL, mcp:SERVER, exec, model:refusal, miss:TOOL, miss:exec
	Text   string
	host   string // a command's miss on another host, the host its command line names (remoteHost), till failureKey keys it
}

func (k FailureKey) String() string { return k.Origin + ":" + k.Text }

// execMiss is the origin of a command's message that says no file is at the path it names (quotedMiss). No read tool
// is named exec (readTools), so no read's miss has it.
const execMiss = "miss:exec"

// missed says the key is a read's that found no file: its text is the path it missed. A command's (execMiss) is not
// a read's: its call names no file for a write to clear.
func (k FailureKey) missed() bool {
	return strings.HasPrefix(k.Origin, "miss:") && k.Origin != execMiss
}

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
	case "miss":
		if k.Origin == execMiss {
			return "A command keeps missing the same path: " + k.Text
		}
		return "The same path keeps missing through " + name + ": " + k.Text
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
		`this action was denied by the claude code auto mode classifier|` +
		`command failed due to container (?:sandboxdenied|sandboxerror)|sandbox(?:denied|error):|` +
		`patch rejected:|approval policy is |the (?:command|tool call|action) (?:was |is )?(?:rejected|denied|blocked) by (?:the )?(?:sandbox|policy|hook))`)
	// deniedCmdRe is Claude Code's deny of a command by a rule: what it ran stays out of the key but its programs.
	deniedCmdRe = regexp.MustCompile(`(?i)^permission to use (\S+) with command (.*?) has been denied`)
	// ansiRe is a terminal's escape sequence, which colours a line and is no part of its words.
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	// headerRe is what a harness prints before a failed command's output: Claude Code's exit code; Codex's exit code
	// and wall time, or its chunk, wall time, exit code and token count, up to its output.
	headerRe = regexp.MustCompile(`(?is)^(?:exit code:? -?\d+\s*(?:wall time:.*?(?:final )?output:\s*)?|chunk id:.*?(?:final )?output:\s*|(?:wall time:.*?)?process exited with code -?\d+\s*(?:final )?output:\s*)`)
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
		`(?:command|tool|execution|request) (?:execution )?timed out|context deadline exceeded|` +
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
//
// A read tool's own error that no file is at its path (missOf) is keyed miss:TOOL by the path its call names
// (missedPath), not by its words, which would merge every path into one: only the same path missed again recurs. No
// key of a failure's words opens with miss:, so the two never meet. A read whose call names no path (a call unknown,
// cut before its path, or none at all, as for the mistakes log, whose input the store does not keep for a tool error)
// says nothing to key it by.
//
// A failed command whose message says no file is at a path it names in quotes (quotedMiss), or unquoted after its
// program's name (unquotedMiss), is keyed miss:exec by that path (commandMissed, missedPath), for the same reason,
// and one of a command that ran on another host by that host and path (remote). One whose path is empty or cut short
// says nothing to key it by.
func FailureOf(harness, tool, text, call string) (FailureKey, bool) {
	return failureKey(harness, tool, text, store.Call{Input: call})
}

// failureKey is FailureOf's key for a result with its call as the store keeps it, its directory too, which a relative
// path a read missed is read in (missOf), and which a whole path a command's message names is keyed below
// (commandMissed).
func failureKey(harness, tool, text string, c store.Call) (FailureKey, bool) {
	k, ok := failureOf(harness, tool, text, c.Input)
	if !ok {
		return k, ok
	}
	p, miss := missOf(harness, tool, text, c)
	switch {
	case !miss && k.host != "": // another host's path: no directory of this host's is stripped from it
		if k.Text == "" {
			return FailureKey{}, false
		}
		return FailureKey{Origin: execMiss, Text: k.host + ":" + missedPath(k.Text)}, true
	case !miss && k.Origin == execMiss: // its text is the path as the message names it
		if p = commandMissed(harness, k.Text, c); p == "" {
			return FailureKey{}, false
		}
		return FailureKey{Origin: execMiss, Text: missedPath(p)}, true
	case !miss:
		return k, ok
	case p == "":
		return FailureKey{}, false
	}
	return FailureKey{Origin: "miss:" + slug(tool, 30), Text: missedPath(p)}, true
}

// commandMissed is the path a command's message named as the file it found no file at (quotedMiss), as its key holds
// it before missedPath cleans it, or "" for an empty one:
//   - A relative one as the message spells it, with no directory joined to it: the store often cuts the command
//     before what it ran, and a cd moves where it ran, so the directory is not known, and a name missed in each of
//     its directories recurs as one, as a read's whose directory is not known does (missOf). A ~ in a program's
//     message is such a name too: the shell expands the home's before the program sees it.
//   - A whole one under the directory the call ran in, by its part below it, as a relative one: the directory the
//     store says it ran in (shellDir: the workdir its input names, else the session's, but none for a call cut
//     before its directory's field could be read, or for Hermes's terminal without one), when that is a whole path
//     below the home or outside it (not /, the home or a directory above it). A per-task worktree's root is no part
//     of what a script in it misses, so one script missed in each worktree recurs as one. The cost: one relative
//     name in two unrelated projects is one key.
//   - Any other whole path as it is, and every whole path of a call whose directory the store does not say.
//
// The directory is the call's: a command that runs elsewhere through what is not ssh (bash -c with a cd in it,
// env -C, docker exec) is read as run in it.
func commandMissed(harness, p string, c store.Call) string {
	if p == "" || !path.IsAbs(p) {
		return p
	}
	dir := shellDir(harness, c.Input, c.Cwd)
	root, home := path.Clean(dir), path.Clean(paths.Home())
	if dir == "" || root == "/" || root == home || strings.HasPrefix(home, root+"/") {
		return p
	}
	if rest, ok := strings.CutPrefix(path.Clean(p), root+"/"); ok {
		return rest
	}
	return p
}

var (
	// commandKeyRe is a field of a shell call's input that holds what it runs, a command line or code, by its name: a
	// value the store cut still names it.
	commandKeyRe = regexp.MustCompile(`"(?:command|cmd|code)"\s*:`)
	// hostRe is a program that works on another host, as a word of a command's text: ssh (alone, after its path, in
	// rsync's -e "ssh ..." or a variable's value), scp and sftp. A word goes up to a quote, a space or a shell
	// separator; ~/.ssh and ssh-keygen are none.
	hostRe = regexp.MustCompile("(?:^|[\\s\"'(;&|=`/\\\\])(?:ssh|scp|sftp)(?:$|[\\s\"';&|)`\\\\])")
	// rsyncRe is rsync and its words up to the end of its command.
	rsyncRe = regexp.MustCompile(`(?:^|[\s"'(;&|/])rsync\s+([^;&|\n]*)`)
	// hostPathRe is an operand of rsync's that names a host: host:path, user@host:path, $HOST:path, rsync://.
	hostPathRe = regexp.MustCompile(`^(?:rsync://|[\w.@${}-]+:)`)
)

// remote says a path a shell call's output names may be another host's (far), and that host when its command line
// names it (remoteHost), so its message is keyed by that host and path; one far with no host keeps its words. Far:
//   - its command's text (its command line as the store keeps it, else its whole input, as Codex's exec script)
//     reaches another host (hops);
//   - its input names no command: a write to a running command's stdin or a poll of its output (Codex's
//     write_stdin, Claude Code's bash_output), whose event does not say which host printed it.
//
// before is the output before the message's mark. A call the store does not hold is read as local: the mistakes
// log's are Claude Code's own commands.
func remote(call, before string) (host string, far bool) {
	text := call
	if m := callRe.FindStringSubmatch(call); m != nil {
		text = unescape(m[1])
	} else if strings.HasPrefix(strings.TrimSpace(call), "{") && !commandKeyRe.MatchString(call) {
		return "", true
	}
	if hops(text) {
		return remoteHost(text, before), true
	}
	return "", false
}

// hops says a command's text names a program that works on another host (hostRe), in any line, a here-document's
// too, or runs rsync with -e or --rsh, or with an operand that names a host (hostPathRe).
func hops(text string) bool {
	if hostRe.MatchString(text) {
		return true
	}
	for _, m := range rsyncRe.FindAllStringSubmatch(text, -1) {
		for _, w := range strings.Fields(m[1]) {
			w = strings.Trim(w, `"'`)
			switch {
			case strings.HasPrefix(w, "--rsh"),
				strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "e"),
				!strings.HasPrefix(w, "-") && hostPathRe.MatchString(w):
				return true
			}
		}
	}
	return false
}

var (
	// hostNameRe is a host as a command line names it, its user@ off: a name or an address, no variable or port.
	hostNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// hostOperandRe is scp's or rsync's [user@]host:path operand, by its host; rsync's daemon (host::module,
	// rsync://) and scp's scp:// are none.
	hostOperandRe = regexp.MustCompile(`^(?:[^@/:]+@)?([A-Za-z0-9][A-Za-z0-9._-]*):(?:$|[^:/]|/$|/[^/])`)
	// rsyncRoleRe is rsync's message, by the role of the side that printed it: sender, receiver or generator.
	rsyncRoleRe = regexp.MustCompile(`(?i)rsync: \[(sender|receiver|generator)\] `)
)

// The options that take a value, in their word or the next.
const (
	sshValued = "pioJlFEbcDLRWwmOQSBeIP"
	// rsyncValued are the short options of rsync that take a value in the next word: -e, -f, -T, -B and -M
	rsyncValued = "efTBM"
	scpValued   = "cDFiJloPSX"
)

// remoteHost is the host whose path a plain command line's message names, as written (its user@ off, never
// resolved), or "" when it does not say. Plain is one command (commandWords), past its assignments and wrappers
// (as commandPrograms reads them), that is:
//   - ssh [options] HOST CMD: the first word past its options and their values, when CMD reaches no further host
//     (hops), whose path it may print;
//   - scp or rsync with [user@]host:path operands of one host, when its message is that host's: scp's that does not
//     say local ("stat local"), rsync's sender's when a source names the host and its receiver's or generator's when
//     its destination (its last operand) does. A message with no side is no host's.
//
// Anything else says no host: a host from a variable, a here-document or a file on ssh's stdin, a pipe or a second
// command, ssh inside a script. A command line the store cut is read up to the cut.
func remoteHost(text, before string) string {
	words, plain := commandWords(text)
	if !plain {
		return ""
	}
	for len(words) > 0 && assignRe.MatchString(words[0]) {
		words = words[1:]
	}
	for len(words) > 0 && wrappers[words[0]] {
		words = words[1:]
		for len(words) > 0 && (strings.HasPrefix(words[0], "-") || countRe.MatchString(words[0]) || assignRe.MatchString(words[0])) {
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return ""
	}
	switch path.Base(words[0]) {
	case "ssh":
		args := operands(words[1:], sshValued)
		if len(args) < 2 { // a host and a command
			return ""
		}
		h := args[0][strings.LastIndexByte(args[0], '@')+1:]
		if !hostNameRe.MatchString(h) || hops(strings.Join(words[slices.Index(words, args[0])+1:], " ")) { // a command that goes on to a third host
			return ""
		}
		return h
	case "scp":
		h, _ := operandHost(operands(words[1:], scpValued))
		i := strings.LastIndex(before, "scp: ")
		if i < 0 || len(mask(before[i:])) > reach || slices.Contains(strings.Fields(mask(before[i:])), "local") {
			return ""
		}
		return h
	case "rsync":
		args, late := operandsLate(words[1:], rsyncValued)
		if late { // rsync reads options anywhere, and a long one's value after the operands (--exclude X) reads as one
			return ""
		}
		h, at := operandHost(args)
		m := rsyncRoleRe.FindAllStringSubmatchIndex(before, -1)
		if h == "" || len(args) < 2 || m == nil || len(mask(before[m[len(m)-1][0]:])) > reach {
			return ""
		}
		sender := strings.EqualFold(before[m[len(m)-1][2]:m[len(m)-1][3]], "sender")
		if sender == at[len(args)-1] { // the sender is the sources' side, the receiver and generator the destination's
			return ""
		}
		return h
	}
	return ""
}

// operands are a command's words that are no option nor the value an option of valued takes after it in its word
// or as the next.
func operands(args []string, valued string) []string {
	out, _ := operandsLate(args, valued)
	return out
}

// operandsLate is operands, and whether an option comes after the first operand.
func operandsLate(args []string, valued string) (out []string, late bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
			continue
		}
		late = late || len(out) > 0
		for j := 1; j < len(a) && !strings.HasPrefix(a, "--"); j++ {
			if strings.IndexByte(valued, a[j]) >= 0 {
				if j == len(a)-1 {
					i++
				}
				break
			}
		}
	}
	return out, late
}

// operandHost is the host scp's or rsync's host:path operands name, and which of them name it; "" when none does or
// two hosts are named.
func operandHost(args []string) (string, []bool) {
	host, at := "", make([]bool, len(args))
	for i, a := range args {
		m := hostOperandRe.FindStringSubmatch(a)
		if m == nil {
			continue
		}
		if host != "" && m[1] != host {
			return "", at
		}
		host, at[i] = m[1], true
	}
	return host, at
}

// commandWords are a command line's first command's words as the shell splits them, quotes off, and whether the
// line holds no more than it: no other command (a list, a pipe, a background job, a subshell, a second line, a
// command substitution outside quotes), no input (a here-document, a file on its stdin), only output redirections,
// which it drops. A quote the store's cut left open ends the line.
func commandWords(line string) (words []string, plain bool) {
	var b strings.Builder
	open, quoted, target := false, false, false
	end := func() {
		if open && !target {
			words = append(words, b.String())
		}
		if open {
			target = false
		}
		b.Reset()
		open, quoted = false, false
	}
	line = strings.TrimSpace(line)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == ' ' || c == '\t':
			end()
		case c == '\'':
			k := strings.IndexByte(line[i+1:], '\'')
			if k < 0 {
				k = len(line) - i - 1
			}
			b.WriteString(line[i+1 : i+1+k])
			i += k + 1
			open, quoted = true, true
		case c == '"':
			open, quoted = true, true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) && strings.IndexByte("$`\"\\", line[i+1]) >= 0 {
					i++
				}
				b.WriteByte(line[i])
			}
		case c == '\\':
			if i++; i < len(line) && line[i] != '\n' { // a backslash before a newline joins two lines
				b.WriteByte(line[i])
				open, quoted = true, true
			}
		case c == '>': // an output redirection: its descriptor (2>), what it writes to (>&1, > FILE)
			if open && !quoted && strings.Trim(b.String(), "0123456789") == "" {
				b.Reset()
				open = false
			}
			end()
			for i+1 < len(line) && strings.IndexByte(">&|", line[i+1]) >= 0 {
				i++
			}
			target = true
		case strings.IndexByte(";&|<()`\n", c) >= 0:
			return words, false
		default:
			b.WriteByte(c)
			open = true
		}
	}
	end()
	return words, true
}

// failureOf is FailureOf's key by the failure's words alone.
func failureOf(harness, tool, text, call string) (FailureKey, bool) {
	text = strings.TrimSpace(ansiRe.ReplaceAllString(text, ""))
	// Native command results can carry their output in JSON rather than a
	// textual header. Read the output, not quoted JSON values as log words.
	if strings.HasPrefix(text, "{") {
		var result struct {
			Output   string          `json:"output"`
			Stdout   string          `json:"stdout"`
			Stderr   string          `json:"stderr"`
			Error    json.RawMessage `json:"error"`
			Status   string          `json:"status"`
			Message  string          `json:"message"`
			Type     string          `json:"type"`
			Matches  *int            `json:"match_count"`
			Exit     *int            `json:"exit_code"`
			Metadata struct {
				Exit *int `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(text), &result) == nil {
			raw := string(result.Error)
			emptyError := raw == "" || raw == "null" || raw == `""` || raw == "false" || raw == "{}" || raw == "[]"
			// A native search uses exit 1 for an empty result. Its wrapper
			// can be kept as JSON when there is no text content.
			if result.Type == "GrepSearch" && result.Exit != nil && *result.Exit == 1 &&
				result.Matches != nil && *result.Matches == 0 && result.Stderr == "" &&
				emptyError && result.Status != "failed" && result.Status != "error" {
				return FailureKey{}, false
			}
			var message string
			if !emptyError {
				if json.Unmarshal(result.Error, &message) != nil {
					var obj struct {
						Message string `json:"message"`
					}
					json.Unmarshal(result.Error, &obj)
					message = obj.Message
				}
			}
			if message == "" && (result.Status == "error" || result.Status == "failed") {
				message = result.Message
			}
			if message != "" {
				if toolresult.Cancelled(message) {
					return FailureKey{}, false
				}
				if isShell(tool) && workRe.MatchString(message) {
					return FailureKey{}, false
				}
				if g := guardRe.FindStringSubmatch(message); g != nil {
					return FailureKey{Origin: "guard:" + slug(g[1], 30), Text: Norm(firstClause(g[2]), keyChars)}, true
				}
				return FailureKey{Origin: toolOrigin(tool), Text: Norm(firstClause(message), keyChars)}, true
			}
			code := result.Exit
			if code == nil {
				code = result.Metadata.Exit
			}
			if code != nil && *code == 0 {
				return FailureKey{}, false
			}
			output := strings.TrimSpace(strings.Join([]string{result.Stdout, result.Stderr, result.Output}, "\n"))
			if output != "" && isShell(tool) {
				return execKey(output, call)
			}
			// a wrapper with nothing in it that says what failed is not keyed by its shape: unrelated errors would merge
			if message == "" && (result.Type != "" || result.Exit != nil || result.Matches != nil) {
				return FailureKey{}, false
			}
		}
	}
	wrapped := strings.HasPrefix(text, "<tool_use_error>")
	text = strings.TrimSpace(strings.TrimPrefix(text, "<tool_use_error>"))
	if text == "" || ownerNoRe.MatchString(text) || toolresult.Cancelled(text) {
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
		return FailureKey{Origin: toolOrigin(tool), Text: Norm(firstClause(text), keyChars)}, true
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
				return execKey(unescape(o[1]), call)
			}
		}
		return FailureKey{}, false
	case harness == "codex":
		if m := codexRe.FindStringSubmatch(text); m != nil {
			return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(m[1]), keyChars)}, true
		}
	}
	if isShell(tool) || headerRe.MatchString(text) {
		return execKey(headerRe.ReplaceAllString(text, ""), call)
	}
	// A tool's error in its own words (a read, an edit, a write the harness refused).
	return FailureKey{Origin: "tool:" + slug(tool, 30), Text: Norm(firstClause(text), keyChars)}, true
}

func toolOrigin(tool string) string {
	if strings.HasPrefix(tool, "mcp__") {
		server := strings.SplitN(strings.TrimPrefix(tool, "mcp__"), "__", 2)[0]
		return "mcp:" + slug(server, 30)
	}
	return "tool:" + slug(tool, 30)
}

// MistakeKey keys a row of the mistakes log as FailureOf keys a result: its summary is the tool, a colon and the
// error's first line. A deny or a hold is a guard's, as a tool_use_error of Claude Code's is; a model's refusal is
// keyed by what it said; a turn's error by the harness; a row that says no more than an exit code is not keyed, nor a
// read that found no file: the store keeps none of the log's input for a tool error, so no call names its path.
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
	return FailureOf("claude", tool, text, "") // the log is Claude Code's, and the store keeps no call of its
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
//
// The programs are the call's (callPrograms). A message that says no file is at a path it names in quotes
// (quotedMiss), or unquoted after its program's name or zsh's mark (unquotedMiss), is no message of words: its key
// is execMiss, its text that path as the message names it, which failureKey keys as commandMissed says. A call that
// may have run on another host (remote) is keyed by that host and the path when its command line names the host,
// else by its words.
func execKey(out, call string) (FailureKey, bool) {
	programs := callPrograms(call)
	out = strings.Join(strings.Fields(out), " ")
	if workRe.MatchString(out) {
		return FailureKey{}, false
	}
	loc := firstMark(out)
	if loc == nil {
		return FailureKey{}, false
	}
	p, ok := quotedMiss(out, loc)
	if !ok {
		p, ok = unquotedMiss(out, loc, programs)
	}
	if ok {
		switch host, far := remote(call, out[:loc[0]]); {
		case !far:
			return FailureKey{Origin: execMiss, Text: p}, true
		case host != "":
			return FailureKey{Origin: execMiss, Text: p, host: host}, true
		}
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
// error, a line of code), not what the command said: the first group's or the second's. A single quote opens one
// only with no letter or digit before it, as in mask, or after a string's prefix (Python's f, b, r or u): a word's
// apostrophe opens none, so two can'ts quote nothing between them, and one does not pair with a path's quote
// ("can't open file 'PATH'").
var quotedRe = regexp.MustCompile("(\"[^\"\n]*\"|`[^`\n]*`|‘[^’\n]*’|“[^”\n]*”)|(?:^|[^\\pL\\pN])(?i:[bfru]{1,2})?('[^'\n]*')")

// firstMark is where the first mark of the setup failing outside a quoted value is in out, or nil.
func firstMark(out string) []int {
	var quoted [][2]int
	for _, q := range quotedRe.FindAllStringSubmatchIndex(out, -1) {
		if q[2] >= 0 {
			quoted = append(quoted, [2]int{q[2], q[3]})
		} else {
			quoted = append(quoted, [2]int{q[4], q[5]})
		}
	}
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

var (
	// nodeMissRe is Node's message after its code ENOENT, up to the path of the call it names: ": no such file or
	// directory, open ".
	nodeMissRe = regexp.MustCompile(`^: (?i:no such file or directory), [a-z_]+ `)
	// missBeforeRe is what may stand between a quoted path and the mark of a missing file: up to three words (tail's
	// "for reading", rsync's "failed"), the colon, and Python's errno.
	missBeforeRe = regexp.MustCompile(`(?: [a-z]+){0,3}: (?:\[Errno -?\d+\] )?$`)
	// runnerRe is the message of a program that runs another, up to the quoted name of the one it could not run: a
	// wrapper's or strace's, by its name ("env: ", "timeout: failed to run command ", "strace: Can't stat "), and
	// Deno's ("Failed to spawn ").
	runnerRe = regexp.MustCompile(`(?i)(?:(?:^|[\s/])([\w.-]+): (?:failed to run command |can't stat )?|\bfailed to spawn )$`)
)

// runs says the message before a quoted name (runnerRe) is a program's that runs the one it names.
func runs(opened string) bool {
	r := runnerRe.FindStringSubmatch(opened)
	if r == nil {
		return false
	}
	name := strings.ToLower(r[1])
	return name == "" || name == "strace" || wrappers[name]
}

// quotedMiss is the path a command's message says no file is at, as it names it in quotes around its mark (loc),
// and whether it names one:
//   - after the mark's colon: Python's errno, "No such file or directory: 'PATH'";
//   - before the mark, the quoted value its colon follows (missBeforeRe): ls's "cannot access 'PATH': ", Python's
//     "can't open file 'PATH': [Errno 2] ", tail's "cannot open 'PATH' for reading: ";
//   - after Node's code and its message, the call's path: "ENOENT: no such file or directory, open 'PATH'".
//
// A message that names two paths (a rename's "'A' -> 'B'", mv's "'A' to 'B'") does not say which is not there, so it
// names none. Nor does a program that runs another name a path when it names a program without a slash, which it
// looked for on the PATH: a wrapper ("env: 'cargo': "), strace, Deno (runs), and Python's subprocess, whose traceback
// names its _execute_child. A quote the store's cut left open names a path cut short: "", which keys nothing.
func quotedMiss(out string, loc []int) (string, bool) {
	before, after := out[:loc[0]], out[loc[1]:]
	switch strings.ToLower(out[loc[0]:loc[1]]) {
	case "enoent":
		if m := nodeMissRe.FindStringIndex(after); m != nil {
			return quotedAt(after[m[1]:])
		}
	case "no such file or directory":
		if rest, ok := strings.CutPrefix(after, ": "); ok {
			if p, ok := quotedAt(rest); ok {
				if !strings.Contains(p, "/") && strings.Contains(out, "_execute_child") {
					return "", false
				}
				return p, true
			}
		}
		if m := missBeforeRe.FindStringIndex(before); m != nil {
			p, opened, ok := quotedEnd(before[:m[0]])
			if ok && runs(opened) && !strings.Contains(p, "/") {
				return "", false
			}
			return p, ok
		}
	}
	return "", false
}

// unquotedMissRe is the message of a program that names the one path it found no file at unquoted, up to the
// mark's colon: the program by its name or its path, the words it opens with, and the path as one word. The
// programs are the ones whose message recurs in this form: rg (ripgrep 15's "rg: PATH: IO error for operation on
// PATH" too), cat, wc, grep, cut, sha256sum, scp, sed's "can't read", ugrep's "warning", jq's "error: Could not open
// file", the shell's cd, and bash's own ("bash: PATH", "bash: line N: PATH").
var unquotedMissRe = regexp.MustCompile(`(?:^|\s)(?:[^\s:]*/)?(?:(?:rg|cat|wc|grep|cut|sha256sum|scp|cd): |rg: \S+: IO error for operation on |sed: can't read |` +
	`ugrep: warning: |jq: error: Could not open file |(bash)(?:: line \d+)?: )(\S+): $`)

// zshMissRe is zsh's message of a missing file up to its mark, after which zsh names the path: zsh by its name or
// its path, or eval's under Claude Code, the builtin that found no file (cd, source, .) and the line's number, as in
// "zsh:1: no such file or directory: PATH" and "(eval):cd:1: no such file or directory: PATH".
var zshMissRe = regexp.MustCompile(`(?:^|\s)(?:[^\s:]*/)?(?:zsh|\(eval\))(:(?:cd|source|\.))?(?::\d+)?: $`)

// zshPathRe is the word after zsh's mark.
var zshPathRe = regexp.MustCompile(`^: (\S+)`)

// unquotedMiss is the path a command's message says no file is at when it names it unquoted, and whether it names
// one: "PROG: PATH: No such file or directory" for a program of unquotedMissRe's, and zsh's "no such file or
// directory: PATH" (zshMissRe), its word after the mark. The path is one word that opens with /, ~/ or a name's
// letter, digit, _ or . (./ and ../ too), so an option ("rg: -g: ") or a glob the shell did not expand names none,
// nor does an assignment ("PYTHONPATH=/x: "), which bash ran as a command from a variable's expansion. Nor does
// bash's message when it names a program by a name with no slash, one of the call's programs or of a call that
// names none: that is the command the shell could not run, which recurs under one key, not an argument. Nor does
// zsh's own message (not its cd's, source's or .'s) when its word has no slash: zsh says "command not found" for a
// name it does not find, so that word opens a command line it ran as one word, a variable's value it does not split.
// Nor does a setup failure's message: a wrapper's own (env, timeout, nohup), whose program is none of these, and a
// bad interpreter ("bash: ./x: /bin/foo: bad interpreter: "), a deleted working directory ("getcwd: cannot access
// parent directories: ") or two paths, whose words stand between the program and the mark. A word of zsh's that the
// store's cut ends names a path cut short: "", which keys nothing.
func unquotedMiss(out string, loc []int, programs map[string]bool) (string, bool) {
	if !strings.EqualFold(out[loc[0]:loc[1]], "no such file or directory") {
		return "", false
	}
	var p string
	ran, cut := false, false // the message may name the command the shell could not run; the store cut its path
	if m := unquotedMissRe.FindStringSubmatch(out[:loc[0]]); m != nil {
		p, ran = m[2], m[1] != ""
	} else if z := zshMissRe.FindStringSubmatch(out[:loc[0]]); z != nil {
		w := zshPathRe.FindStringSubmatch(out[loc[1]:])
		if w == nil || z[1] == "" && !strings.Contains(w[1], "/") {
			return "", false
		}
		p, cut = w[1], loc[1]+len(w[0]) == len(out) && strings.HasSuffix(w[1], "…")
	} else {
		return "", false
	}
	if r, _ := utf8.DecodeRuneInString(p); !(r == '/' || strings.HasPrefix(p, "~/") || r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
		return "", false
	}
	if assignRe.MatchString(p) {
		return "", false
	}
	if ran && !strings.Contains(p, "/") && (len(programs) == 0 || programs[strings.ToLower(p)]) {
		return "", false
	}
	if cut {
		return "", true
	}
	return p, true
}

// closers are the quotes that close a value each quote opens: ASCII, GNU's `x' and curly ones.
var closers = map[rune]string{'\'': "'", '"': `"`, '`': "'`", '‘': "’", '“': "”"}

// quotedAt is the value of the quote s opens with, and whether s opens with one that holds one path: "" for one the
// store's cut left open; none when " -> " and another path follow it.
func quotedAt(s string) (string, bool) {
	r, n := utf8.DecodeRuneInString(s)
	closing, ok := closers[r]
	if !ok {
		return "", false
	}
	end := strings.IndexAny(s[n:], closing)
	if end < 0 {
		return "", true
	}
	_, m := utf8.DecodeRuneInString(s[n+end:])
	if strings.HasPrefix(s[n+end+m:], " -> ") {
		return "", false
	}
	return s[n : n+end], true
}

// pairRe ends the first of two quoted paths, before the second: "'A' -> ", mv's "'A' to ".
var pairRe = regexp.MustCompile("['\"`’”] (?:->|to) $")

// quotedEnd is the value of the quote s ends with, the text before it, and whether s ends with one: its quote opens
// no word (no letter or digit before it, as for can't), and no path before it is the other of two (pairRe). GNU's
// shell quoting is read: it spells a quote in a name as a closing quote, an escaped quote and an opening one. Quotes
// run together otherwise, as around a $'\n' it spells a line break with, make a value not read: "".
func quotedEnd(s string) (value, opened string, ok bool) {
	r, n := utf8.DecodeLastRuneInString(s)
	var opens string
	for o, c := range closers {
		if strings.ContainsRune(c, r) {
			opens += string(o)
		}
	}
	if opens == "" {
		return "", "", false
	}
	i := strings.LastIndexAny(s[:len(s)-n], opens)
	if i < 0 {
		return "", "", false
	}
	_, o := utf8.DecodeRuneInString(s[i:])
	value = s[i+o : len(s)-n]
	if s[i] == '\'' && r == '\'' {
		for strings.HasSuffix(s[:i], `'\'`) {
			j := i - len(`'\'`) // the part before closes there
			k := strings.LastIndexByte(s[:j], '\'')
			if k < 0 {
				return "", "", false
			}
			value, i = s[k+1:j]+"'"+value, k
		}
		if strings.HasSuffix(s[:i], "'") {
			return "", s[:i], true
		}
	}
	if b, _ := utf8.DecodeLastRuneInString(s[:i]); i > 0 && (unicode.IsLetter(b) || unicode.IsDigit(b)) {
		return "", "", false
	}
	if pairRe.MatchString(s[:i]) {
		return "", "", false
	}
	return value, s[:i], true
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
	return toolresult.IsShell(tool)
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
