package source

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/toolresult"
)

// TranscriptSource adds a native transcript location to the homes in sources.json.
// Glob is a JSONL pattern for grok or kimi. DB is an opencode SQLite store.
type TranscriptSource struct {
	Harness string `json:"harness"`
	Glob    string `json:"glob,omitempty"`
	DB      string `json:"db,omitempty"`
}

// NativeSources are fixed transcript locations only. No credential, configuration
// or diagnostic files are read. Existing tacit readers remain authoritative.
func NativeSources(home string) []TranscriptSource {
	data := filepath.Join(home, ".local", "share")
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		data = p
	}
	grok := filepath.Join(home, ".grok")
	if p := os.Getenv("GROK_HOME"); p != "" {
		grok = p
	}
	kimi := filepath.Join(home, ".kimi-code")
	if p := os.Getenv("KIMI_CODE_HOME"); p != "" {
		kimi = p
	}
	return []TranscriptSource{
		{Harness: "opencode", DB: filepath.Join(data, "opencode", "opencode.db")},
		{Harness: "grok", Glob: filepath.Join(grok, "sessions", "*", "*", "updates.jsonl")},
		{Harness: "grok", Glob: filepath.Join(grok, "sessions", "*", "*", "subagents", "*", "updates.jsonl")},
		{Harness: "kimi", Glob: filepath.Join(kimi, "sessions", "*", "*", "agents", "*", "wire.jsonl")},
	}
}

type nativeJob struct {
	harness string
	db      *openCodeDB
	session openCodeSession
	// stale says the file's cursor was saved under an older Kimi rule, so it is read from its start (kimiStale).
	stale bool
}

// nativeState is private reader state, saved atomically with its events. It holds
// tool names and attribution metadata, never a tool's command or output.
type nativeState struct {
	Session  string
	Parent   string
	Subagent string
	Model    string
	Cwd      string
	Prior    string
	Calls    map[string]string
	Owner    bool
	Known    bool
	// Rule is the Kimi attribution rule the events before the cursor were credited by (kimiRule).
	Rule int `json:",omitempty"`
	// dir is a Kimi session's directory and wire its agent's file; read says this read has read the session's
	// metadata (kimiProvenance).
	dir  string
	wire string
	read bool
	// sources counts the prompt text parts read with a meta.source Kimi is not known to write (kimiInput), until the
	// reader takes them into its tally.
	sources int
}

// kimiRule is the version of the Kimi attribution rule. A cursor saved under an older one holds owner turns
// credited by a prompt's origin alone, a `kimi -p` run's among them: its file is read again from its start, once,
// and each record holds what the new reading credits (PutFiles). How a prompt's generated text is split from his
// words (kimiInput) is no new rule: a record read before a change to it keeps its split, since a read from the start
// decides provenance again from the current metadata and would credit a resumed `kimi -p` run's launcher prompt to
// him.
const kimiRule = 1

// kimiStale reports a Kimi cursor saved under an older rule. A compressed file's final cursor keeps no state, so
// it is not read again for that; Kimi does not compress its wire files. A cursor that read nothing credited
// nothing: its file is read from its start anyway, and not counted as read again.
func kimiStale(cursors map[string]store.FileCursor, path string) bool {
	cur, ok := cursors[path]
	if !ok || cur.Compressed || cur.Offset == 0 {
		return false
	}
	var st nativeState
	return json.Unmarshal(cur.State, &st) != nil || st.Rule < kimiRule
}

func (s Transcripts) nativeSources() []TranscriptSource {
	out := append([]TranscriptSource(nil), s.Sources...)
	for _, home := range s.Homes {
		out = append(out, NativeSources(home)...)
	}
	return out
}

func (s Transcripts) nativeJobs(ctx context.Context, cursors map[string]store.FileCursor, seen map[string]bool, t *tally) ([]*job, []func(), bool) {
	var jobs []*job
	var closers []func()
	listed := true
	for _, src := range s.nativeSources() {
		if src.Harness == "opencode" && src.DB != "" {
			path, err := filepath.Abs(src.DB)
			if err != nil {
				t.fail("opencode source", errors.New("invalid opencode location"))
				listed = false
				continue
			}
			key := "opencode:" + path
			if seen[key] {
				continue
			}
			seen[key] = true
			db, err := openOpenCode(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.fail(key, err)
				listed = false
				continue
			}
			closers = append(closers, func() { db.Close() })
			ss, err := db.sessions(ctx)
			if err != nil {
				t.fail(key, err)
				listed = false
				continue
			}
			for _, ss := range ss {
				path := key + "#" + ss.id
				seen[path] = true
				cur, ok := cursors[path]
				if ok && cur.FP == ss.fingerprint {
					continue
				}
				jobs = append(jobs, &job{path: path, cur: cur, ok: ok,
					native: &nativeJob{harness: src.Harness, db: db, session: ss}})
			}
			continue
		}
		if src.Harness != "grok" && src.Harness != "kimi" || src.Glob == "" {
			t.fail("transcript source", errors.New("unsupported harness or missing location"))
			listed = false
			continue
		}
		files, err := evidence.Glob(src.Glob)
		if err != nil {
			t.fail("transcript source", errors.New("invalid transcript pattern"))
			listed = false
			continue
		}
		for _, path := range files {
			path = evidence.Plain(path)
			if seen[path] {
				continue
			}
			seen[path] = true
			saved, stale := cursors, src.Harness == "kimi" && kimiStale(cursors, path)
			if stale {
				saved = nil
			}
			if j, ok := fileJob(path, evidence.Claude, saved); ok {
				j.native = &nativeJob{harness: src.Harness, stale: stale}
				jobs = append(jobs, j)
			}
		}
	}
	return jobs, closers, listed
}

func (s Transcripts) readNativeJob(ctx context.Context, j *job, t *tally, add func(part) bool) {
	if j.native.db != nil {
		s.readOpenCodeJob(ctx, j, t, add)
		return
	}
	harness := j.native.harness
	state := nativeIdentity(j.path, harness)
	var offset int64
	if j.ok && j.cur.FP == j.fp && (j.cur.Inode == j.inode || j.cur.Compressed != j.packed) && (j.packed || j.cur.Offset <= j.info.Size()) {
		if json.Unmarshal(j.cur.State, &state) == nil {
			offset = j.cur.Offset
		}
	}
	if harness == "grok" {
		// Session kind is provenance. Headless prompts are generated input.
		// Current metadata wins over the saved cursor. Missing provenance
		// cannot retain an older ownership claim.
		state.Owner, state.Known = false, false
		var summary struct {
			Kind  string `json:"session_kind"`
			Model string `json:"current_model_id"`
			Info  struct {
				Cwd string `json:"cwd"`
			} `json:"info"`
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(evidence.Plain(j.path)), "summary.json"))
		if err == nil && json.Unmarshal(raw, &summary) == nil {
			state.Owner = summary.Kind == "tui" || summary.Kind == "interactive"
			state.Known = summary.Kind != ""
			state.Model, state.Cwd = summary.Model, summary.Info.Cwd
		}
	}
	if harness == "kimi" {
		// The session's metadata is provenance, read at the first prompt that needs it (kimiProvenance).
		// Current metadata wins over the saved cursor.
		state.Owner, state.Known, state.Rule = false, false, kimiRule
	}
	if state.Calls == nil {
		state.Calls = map[string]string{}
	}
	reader, err := evidence.OpenAt(j.path, offset)
	if err != nil {
		t.fail(j.path, errors.New("cannot open native transcript"))
		return
	}
	defer reader.Close()
	if offset == 0 && (j.native.stale || j.ok && j.cur.Offset > 0) {
		// A file replaced, cut or read under an older rule is read from its start: counted, as tacit's are, once
		// it is open, so a file that cannot be opened is not counted as read again.
		t.Lock()
		t.restarts++
		t.Unlock()
	}
	rd := bufio.NewReaderSize(reader, 64<<10)
	p := part{j: j}
	start := offset
	flush := func(final bool) bool {
		kept, err := json.Marshal(state)
		if err != nil {
			t.fail(j.path, err)
			return false
		}
		p.cur = store.FileCursor{Source: s.Name(), Path: j.path, Inode: j.inode, FP: j.fp,
			Compressed: j.packed, Offset: offset, State: kept}
		if j.packed && final {
			p.cur.State = nil
		}
		if final && offset == start && j.packed {
			// An unchanged compressed stream can end before any new line.
			p.cur.Size, p.cur.MTime = j.info.Size(), j.info.ModTime().UnixNano()
		} else if final && (j.packed || offset == j.info.Size()) {
			p.cur.Size, p.cur.MTime = j.info.Size(), j.info.ModTime().UnixNano()
		}
		ok := add(p)
		p, start = part{j: j}, offset
		return ok
	}
	for {
		if ctx.Err() != nil {
			t.fail(j.path, ctx.Err())
			return
		}
		line, err := rd.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			t.fail(j.path, errors.New("cannot read native transcript"))
			return
		}
		if len(line) == 0 || errors.Is(err, io.EOF) && strings.TrimSpace(string(line)) != "" && !json.Valid(line) {
			// A partially written last line stays before the cursor.
			flush(true)
			return
		}
		var record map[string]any
		var evs []evidence.Event
		if json.Unmarshal(line, &record) != nil || record == nil {
			// Complete malformed lines are counted by the writer. A partial
			// final line was left before the cursor above.
			p.skips = append(p.skips, fmt.Sprintf("%s@%d", j.path, offset))
		} else if harness == "grok" {
			evs = state.grok(record)
		} else {
			evs = state.kimi(record)
			if state.sources > 0 {
				t.Lock()
				t.sources += state.sources
				t.Unlock()
				state.sources = 0
			}
		}
		next := offset + int64(len(line))
		for index, e := range evs {
			e.Path, e.Offset, e.Next, e.Index = j.path, offset, next, index
			p.read++
			p.places = append(p.places, place(e))
			if it, ok := toPending(e); ok {
				p.items = append(p.items, it)
			}
		}
		offset = next
		if errors.Is(err, io.EOF) {
			flush(true)
			return
		}
		if offset-start >= chunkBytes && !flush(false) {
			return
		}
	}
}

func nativeIdentity(path, harness string) nativeState {
	dir := filepath.Dir(evidence.Plain(path))
	st := nativeState{Calls: map[string]string{}}
	if harness == "kimi" {
		st.Session = filepath.Base(filepath.Dir(filepath.Dir(dir)))
		st.dir, st.wire = filepath.Dir(filepath.Dir(dir)), evidence.Plain(path)
		agent := filepath.Base(dir)
		if agent != "main" {
			st.Subagent = agent
		}
	} else {
		st.Session = filepath.Base(dir)
		if filepath.Base(filepath.Dir(dir)) == "subagents" {
			st.Parent = filepath.Base(filepath.Dir(filepath.Dir(dir)))
			st.Subagent = st.Session
		}
	}
	return st
}

func (s *nativeState) event(harness string, kind evidence.Kind, text string, at time.Time) evidence.Event {
	who := evidence.Harness
	if kind == evidence.KindAgentText || kind == evidence.KindToolCall {
		who = evidence.Agent
	}
	return evidence.Event{Harness: harness, Session: s.Session, Parent: s.Parent, Subagent: s.Subagent,
		Model: s.Model, Cwd: s.Cwd, Kind: kind, Who: who, At: at, Text: text}
}

// user uses tacit's piece attribution rather than copying its frame/paste rules.
// Only explicit user origins in a main thread can be owner turns. Grok lacks
// an input-origin field; its main user updates remain subject to detector filters.
// unknown says the session's provenance could not be read: a main thread's
// input that is not the owner's is then credited to no one.
func (s *nativeState) user(harness, text string, owner, unknown bool, at time.Time, input evidence.InputKind) []evidence.Event {
	var out []evidence.Event
	for _, piece := range evidence.TurnPieces(text) {
		kind, who := evidence.KindHarnessText, piece.Who
		if owner && s.Subagent == "" && who == evidence.Owner {
			kind = evidence.KindOwnerText
		} else if who == evidence.Owner {
			who = evidence.Agent
			if unknown && s.Subagent == "" {
				who = evidence.Unknown
			}
		}
		e := s.event(harness, kind, piece.Text, at)
		e.Who, e.Prior = who, s.Prior
		if kind == evidence.KindOwnerText {
			e.InputKind = input
		} else {
			e.Why = "generated input"
			if who == evidence.Unknown {
				e.Why = "input origin unknown"
			}
		}
		out = append(out, e)
	}
	s.Prior = ""
	return out
}

func object(v any) map[string]any             { m, _ := v.(map[string]any); return m }
func str(m map[string]any, k string) string   { s, _ := m[k].(string); return s }
func number(m map[string]any, k string) int64 { n, _ := m[k].(float64); return int64(n) }
func encoded(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// whereFirst is a tool call's input with the fields that say where the call acts (its path, its directory, its
// command) first, so the store's cut of a long input keeps them; the rest follow by name, each value as it was. An
// input the cut keeps whole, or one that is not one JSON object, stays as it is.
func whereFirst(in string) string {
	t := strings.TrimSpace(in)
	if len(t) <= textChars || !strings.HasPrefix(t, "{") {
		return in
	}
	d := json.NewDecoder(strings.NewReader(t))
	var m map[string]json.RawMessage
	if d.Decode(&m) != nil {
		return in
	}
	if _, err := d.Token(); err != io.EOF {
		return in
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		if i := slices.Index(whereKeys, k); i >= 0 {
			return i
		}
		return len(whereKeys)
	}
	sort.Slice(keys, func(i, j int) bool {
		if a, b := rank(keys[i]), rank(keys[j]); a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(m[k])
	}
	b.WriteByte('}')
	return b.String()
}

// whereKeys are the input fields that say where a tool acts, in the order a stored input keeps them.
var whereKeys = []string{"filePath", "file_path", "notebook_path", "path", "filename", "workdir", "cwd", "working_directory",
	"workingDirectory", "directory", "dir", "command", "cmd", "argv"}

// nativeText takes only text content, never reasoning, images or attachments.
func nativeText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if str(x, "type") == "text" {
			return str(x, "text")
		}
		if str(x, "type") == "content" {
			return nativeText(x["content"])
		}
	case []any:
		var parts []string
		for _, b := range x {
			if t := nativeText(b); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func (s *nativeState) grok(o map[string]any) []evidence.Event {
	at := time.Unix(number(o, "timestamp"), 0)
	p := object(o["params"])
	u := object(p["update"])
	typ := str(u, "sessionUpdate")
	if id := str(p, "sessionId"); id != "" {
		s.Session = id
	}
	switch typ {
	case "user_message_chunk":
		return s.user("grok", nativeText(u["content"]), s.Owner, !s.Known, at, evidence.InputTyped)
	case "agent_message_chunk":
		return []evidence.Event{s.event("grok", evidence.KindAgentText, nativeText(u["content"]), at)}
	case "tool_call", "tool_call_update":
		id := str(u, "toolCallId")
		metadata := object(object(u["_meta"])["x.ai/tool"])
		r := object(u["rawOutput"])
		name := firstOf(str(metadata, "name"), str(metadata, "tool_name"), str(r, "name"), s.Calls[id])
		if name == "" {
			// A title can be a command; it is never a tool name.
			name = "unknown"
		}
		var out []evidence.Event
		if typ == "tool_call" {
			s.Calls[id] = name
			e := s.event("grok", evidence.KindToolCall, "", at)
			e.ToolID, e.ToolName, e.ToolInput = id, name, encoded(u["rawInput"])
			out = append(out, e)
		}
		status := str(u, "status")
		if status == "completed" || status == "failed" || status == "cancelled" {
			e := s.event("grok", evidence.KindToolResult, "", at)
			e.ToolID, e.ToolName = id, name
			e.ToolOutput = firstOf(str(r, "content"), str(r, "output"), nativeText(u["content"]), encoded(u["rawOutput"]))
			// Search exit 1 means no matches. Its ACP status decides failure.
			// Bash's exit code belongs to its native command envelope.
			e.IsError = status == "failed" || str(r, "type") == "Bash" && toolresult.EnvelopeFailed(encoded(r))
			if status == "cancelled" {
				e.ToolOutput = "Tool execution cancelled by user"
			}
			out = append(out, e)
			delete(s.Calls, id)
		}
		return out
	case "turn_completed":
		s.Prior = "task_complete"
		e := s.event("grok", evidence.KindHarnessText, "", at)
		e.Why = "turn ended"
		return []evidence.Event{e}
	case "auto_compact_completed":
		return []evidence.Event{s.event("grok", evidence.KindCompaction, str(u, "summary_preview"), at)}
	}
	return nil
}

func (s *nativeState) kimi(o map[string]any) []evidence.Event {
	at := time.UnixMilli(number(o, "time"))
	switch str(o, "type") {
	case "profile.bind":
		s.Model = str(o, "modelAlias")
		s.Cwd = str(object(o["environmentDisclosure"]), "cwd")
	case "llm.request":
		s.Model = str(o, "model")
	case "turn.prompt", "turn.steer":
		input := evidence.InputTyped
		if str(o, "type") == "turn.steer" {
			input = evidence.InputQueued
		}
		// A user origin says a prompt came in from outside the agent, not who sent it: `kimi -p` gives its
		// prompt the same origin as a person typing. The session's metadata says which (kimiProvenance).
		origin := object(o["origin"])
		typed := str(origin, "kind") == "user" && s.Subagent == ""
		if typed {
			s.kimiProvenance()
		}
		// The text Kimi and its clients generated into a prompt (an inline skill's body, a hook's output, a client's
		// notice) is generated input, as a slash command's activation is. Only the rest is the prompt's words
		// (kimiInput).
		generated, words, unknown := kimiInput(o["input"], origin)
		s.sources += unknown
		prior := s.Prior
		out := s.user("kimi", generated, false, false, at, input)
		s.Prior = prior
		owner := typed && s.Owner
		rest := s.user("kimi", words, owner, typed && !s.Known, at, input)
		if owner && generated != "" && strings.TrimSpace(words) == "" {
			// A prompt he sent is his turn though what was generated into it left none of his words: it holds empty
			// text. Words tacit's own rules take from him are read as on main, with no turn added.
			e := s.event("kimi", evidence.KindOwnerText, "", at)
			e.Who, e.Prior, e.InputKind = evidence.Owner, prior, input
			rest = append(rest, e)
		}
		return append(out, rest...)
	case "context.append_loop_event":
		event := object(o["event"])
		switch str(event, "type") {
		case "content.part":
			text := nativeText(event["part"])
			if text != "" {
				return []evidence.Event{s.event("kimi", evidence.KindAgentText, text, at)}
			}
		case "tool.call":
			id, name := str(event, "toolCallId"), str(event, "name")
			s.Calls[id] = name
			e := s.event("kimi", evidence.KindToolCall, "", at)
			e.ToolID, e.ToolName, e.ToolInput = id, name, encoded(event["args"])
			return []evidence.Event{e}
		case "tool.result":
			id := str(event, "toolCallId")
			result := object(event["result"])
			e := s.event("kimi", evidence.KindToolResult, "", at)
			e.ToolID, e.ToolName = id, s.Calls[id]
			e.ToolOutput = firstOf(str(result, "output"), encoded(result))
			e.IsError = toolresult.EnvelopeFailed(encoded(result))
			delete(s.Calls, id)
			return []evidence.Event{e}
		}
	case "turn.ended":
		s.Prior = "task_complete"
		e := s.event("kimi", evidence.KindHarnessText, "", at)
		e.Why = "turn ended"
		return []evidence.Event{e}
	case "context.apply_compaction":
		return []evidence.Event{s.event("kimi", evidence.KindCompaction, firstOf(str(o, "summary"), str(o, "contextSummary")), at)}
	}
	return nil
}

// The meta.source values Kimi gives the text parts it generates into a prompt: a skill's rendered body and a
// UserPromptSubmit hook's output.
const (
	kimiSkillSource = "skill activation"
	kimiHookSource  = "user prompt submit hook"
)

// The openings of the two system blocks Kimi's clients write for the model into a prompt: the image compression
// note, which the TUI and the web and ACP servers write as a text part of its own right before its image (and which
// the TUI's join of a steered or queued prompt's text parts, combineSteerInput, leaves at the end of the text part
// before its image), and VS Code's editor context, which VS Code puts at the start of a list prompt's first text
// part or appends to a string prompt. The tag is built from parts so this file holds none.
const (
	kimiNoteOpen    = "<" + "system>Image compressed to fit model limits:"
	kimiContextOpen = "<" + "system>Editor context (use only if relevant to user's query): "
)

// kimiBlock is one of those blocks: its opening, the rest of its line up to the next closing tag, and the newline
// after it when there is one. Any other system block is text he may have typed or pasted.
var kimiBlock = regexp.MustCompile(`(?:` + regexp.QuoteMeta(kimiNoteOpen) + `|` + regexp.QuoteMeta(kimiContextOpen) + `).*?</` + `system>\n?`)

// kimiResource and kimiResourceLink are a whole text part the ACP server writes for a resource a client attached:
// an embedded text resource, and a link that is not a local file. A local file link becomes a bare path, which
// cannot be told from a path he typed.
var (
	kimiResource     = regexp.MustCompile(`(?s)\A<resource uri="[^"]*">.*</resource>\z`)
	kimiResourceLink = regexp.MustCompile(`\A<resource_link uri="[^"]*" name="[^"]*" />\z`)
)

// kimiImageOmitted is the whole text part Kimi's engine and its web server write in place of an image the model
// cannot take: an unsupported format (named or not, with the provider's accepted formats) or a data URL that cannot
// be parsed, each in its fixed words.
var kimiImageOmitted = regexp.MustCompile(`\A\[Image omitted: (?:(?:unsupported image format \S+|".*" uses unsupported image format \S+)\. ` +
	`The current model provider accepts only (?:[^,\n]+(?:, [^,\n]+)*)?, and [^,\n]+ \x{2014} convert it to PNG or JPEG and try again\.|` +
	`".*" is not a valid data URL \(its header or payload could not be parsed\)\. Re-encode the image as PNG or JPEG and try again\.)\]\z`)

// kimiMediaMarker and kimiUnsupported are the whole text parts Kimi's migration of a session from its older CLI
// writes into a prompt in place of what it could not carry over: a media part with no url or whose local file is
// gone, and a part of a kind it does not know, as JSON.
var (
	kimiMediaMarker = regexp.MustCompile(`\A\[(?:image|audio|video) (?:missing url|expired)\]\z`)
	kimiUnsupported = regexp.MustCompile(`(?s)\A\[unsupported content: .*\]\z`)
)

// kimiAttached are the notices Kimi's web server writes, each a whole text part, for the files a user-origin prompt
// attaches: rebuilt from the name, type, size and path its origin.attachments keeps for each.
func kimiAttached(origin map[string]any) map[string]bool {
	list, _ := origin["attachments"].([]any)
	if str(origin, "kind") != "user" || len(list) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, a := range list {
		m := object(a)
		size, ok := m["size"].(float64)
		if !ok {
			continue
		}
		out["Attached file \""+str(m, "name")+"\" ("+str(m, "mediaType")+", "+strconv.FormatFloat(size, 'f', -1, 64)+" bytes): "+
			str(m, "path")+" \u2014 open it with the Read tool"] = true
	}
	return out
}

// kimiInput splits a prompt's input into the text Kimi generated into it and the rest, the prompt's words. These
// are generated: a text part with any meta.source (Kimi marks a skill's rendered body and a hook's output so, and
// its clients send the person's text with none); on a wire from before Kimi marked skill parts, in a user-origin
// prompt with no part marked "skill activation", the first text parts with no meta.source, one per activation its
// origin.skillActivations bundles, which Kimi reads as the skill's; a whole text part a client or Kimi writes in
// its fixed form (kimiWhole, kimiAttached); an image note that is the whole text part right before an image part,
// or the block that ends it; and VS Code's editor context at the start of the first text part, or at the end of a
// string prompt, which reaches the wire as one text part. What cannot be rebuilt exactly where Kimi
// writes it, such as an attached-file notice its origin does not list, a known block elsewhere in his words or the
// ACP server's bare file paths, stays in his words. unknown counts the parts whose meta.source is not one Kimi is
// known to write; they are generated all the same. Only text counts, as in nativeText.
func kimiInput(input any, origin map[string]any) (generated, words string, unknown int) {
	parts, listed := input.([]any)
	if !listed {
		parts = []any{map[string]any{"type": "text", "text": nativeText(input)}}
	}
	skill := func(b any) bool {
		p := object(b)
		return str(p, "type") == "text" && str(object(p["meta"]), "source") == kimiSkillSource
	}
	unmarked := func(b any) bool {
		_, sourced := object(object(b)["meta"])["source"]
		return !sourced
	}
	bundled := 0
	if activations, ok := origin["skillActivations"].([]any); ok && listed && str(origin, "kind") == "user" && !slices.ContainsFunc(parts, skill) {
		bundled = len(activations)
	}
	attached := kimiAttached(origin)
	// A string prompt is the input's one unmarked part.
	single := !listed || len(slices.DeleteFunc(slices.Clone(parts), func(b any) bool { return !unmarked(b) })) == 1
	first := true
	var gen, own []any
	for i, b := range parts {
		p := object(b)
		source, sourced := object(p["meta"])["source"]
		switch {
		case str(p, "type") != "text":
			own = append(own, b)
		case sourced:
			if source != kimiSkillSource && source != kimiHookSource {
				unknown++
			}
			gen = append(gen, b)
		case bundled > 0:
			bundled--
			gen = append(gen, b)
		default:
			t := str(p, "text")
			opens := first
			first = false
			if kimiWhole(t) || attached[t] {
				gen = append(gen, b)
				continue
			}
			beforeImage := i+1 < len(parts) && str(object(parts[i+1]), "type") == "image_url"
			lo, hi := 0, len(t)
			blocks := kimiBlock.FindAllStringIndex(t, -1)
			if opens && len(blocks) > 0 && blocks[0][0] == 0 && strings.HasPrefix(t, kimiContextOpen) {
				lo = blocks[0][1]
			}
			if n := len(blocks); n > 0 && blocks[n-1][1] == len(t) && blocks[n-1][0] >= lo {
				last := t[blocks[n-1][0]:]
				if beforeImage && strings.HasPrefix(last, kimiNoteOpen) || single && lo == 0 && strings.HasPrefix(last, kimiContextOpen) {
					hi = blocks[n-1][0]
				}
			}
			if lo > 0 {
				gen = append(gen, map[string]any{"type": "text", "text": t[:lo]})
			}
			if hi < len(t) {
				gen = append(gen, map[string]any{"type": "text", "text": t[hi:]})
			}
			own = append(own, map[string]any{"type": "text", "text": t[lo:hi]})
		}
	}
	return nativeText(gen), nativeText(own), unknown
}

// kimiWhole reports a whole text part Kimi or a client writes in a fixed form: the ACP server's resource elements
// (kimiResource), an image-omitted notice (kimiImageOmitted) and a migration's marker (kimiMediaMarker,
// kimiUnsupported).
func kimiWhole(t string) bool {
	return kimiResource.MatchString(t) || kimiResourceLink.MatchString(t) || kimiImageOmitted.MatchString(t) ||
		kimiMediaMarker.MatchString(t) || kimiUnsupported.MatchString(t)
}

// kimiProvenance reads whether a person's client ever prompted the session. Kimi's clients (its TUI, web UI and
// ACP server) record the session's lastPrompt in its metadata before each prompt's turn begins; `kimi -p` hands
// its prompt to the agent loop and records none. A session whose metadata holds no lastPrompt is a program's run,
// and one with no readable metadata has unknown provenance. So does one whose wire holds an undo: undoing every
// prompt takes its lastPrompt away. It is read once per read, at the first user prompt of the main agent, after
// that prompt's line: the lastPrompt of a prompt the reader holds was written before it.
func (s *nativeState) kimiProvenance() {
	if s.read || s.dir == "" {
		return
	}
	s.read = true
	owner, known := kimiMeta(s.dir)
	if !known {
		return
	}
	s.Owner = owner
	s.Known = owner || !kimiUndone(s.wire)
}

// kimiMeta reads a session's metadata where Kimi keeps it: its state.json and the state.json of its older
// session-meta directory. owner says one of them holds a lastPrompt (Kimi reads one that is not a string as
// none), and known that one of them could be read: a file that is missing, or not one JSON object, is not.
func kimiMeta(dir string) (owner, known bool) {
	for _, path := range []string{filepath.Join(dir, "state.json"), filepath.Join(dir, "session-meta", "state.json")} {
		raw, err := os.ReadFile(path)
		var meta map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &meta) != nil || meta == nil {
			continue
		}
		known = true
		if last := bytes.TrimSpace(meta["lastPrompt"]); len(last) > 0 && last[0] == '"' {
			return true, true
		}
	}
	return false, known
}

// kimiUndone reports a wire that holds an undo of its context (a context.undo or context.undone record), read
// from whichever file holds it, a compressed copy too. A wire that cannot be read reports one: what it holds is
// not known.
func kimiUndone(path string) bool {
	f, err := evidence.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"context.undo`)) {
			var record struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &record) == nil && (record.Type == "context.undo" || record.Type == "context.undone") {
				return true
			}
		}
		if err != nil {
			return !errors.Is(err, io.EOF)
		}
	}
}

// Native paths end in shared filenames such as wire.jsonl. Their source keys
// need the harness, session and agent, not the shared basename.
func nativePlace(e evidence.Event) string {
	return fmt.Sprintf("%s:%s:%s@%012d#", e.Harness, e.Session, e.Subagent, e.Offset)
}

func readOnlyDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
}

func firstOf(texts ...string) string {
	for _, text := range texts {
		if text != "" {
			return text
		}
	}
	return ""
}
