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
	"slices"
	"sort"
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
}

// kimiRule is the version of the Kimi attribution rule. A cursor saved under an older one holds owner turns
// credited by a prompt's origin alone, a `kimi -p` run's among them: its file is read again from its start, once,
// and each record holds what the new reading credits (PutFiles).
const kimiRule = 1

// kimiStale reports a Kimi cursor saved under an older rule. A compressed file's final cursor keeps no state, so
// it is not read again for that; Kimi does not compress its wire files.
func kimiStale(cursors map[string]store.FileCursor, path string) bool {
	cur, ok := cursors[path]
	if !ok || cur.Compressed {
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
			saved := cursors
			if src.Harness == "kimi" && kimiStale(cursors, path) {
				saved = nil
			}
			if j, ok := fileJob(path, evidence.Claude, saved); ok {
				j.native = &nativeJob{harness: src.Harness}
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
		typed := str(object(o["origin"]), "kind") == "user" && s.Subagent == ""
		if typed {
			s.kimiProvenance()
		}
		return s.user("kimi", nativeText(o["input"]), typed && s.Owner, typed && !s.Known, at, input)
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
	meta, ok := kimiMeta(s.dir)
	if !ok {
		return
	}
	// Kimi reads a lastPrompt that is not a string as none.
	last := bytes.TrimSpace(meta["lastPrompt"])
	s.Owner = len(last) > 0 && last[0] == '"'
	s.Known = s.Owner || !kimiUndone(s.wire)
}

// kimiMeta reads a session's metadata where Kimi reads it: its state.json, else the state.json of its older
// session-meta directory. A file that is missing, or not one JSON object, is not read.
func kimiMeta(dir string) (map[string]json.RawMessage, bool) {
	for _, path := range []string{filepath.Join(dir, "state.json"), filepath.Join(dir, "session-meta", "state.json")} {
		raw, err := os.ReadFile(path)
		var meta map[string]json.RawMessage
		if err == nil && json.Unmarshal(raw, &meta) == nil && meta != nil {
			return meta, true
		}
	}
	return nil, false
}

// kimiUndone reports a wire that holds an undo of its context (a context.undo or context.undone record). A wire
// that cannot be read reports one: what it holds is not known.
func kimiUndone(path string) bool {
	f, err := os.Open(path)
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
