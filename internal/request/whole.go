package request

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/toolresult"
)

// Why a cut call was read as the store holds it.
const (
	cutGone     = "gone"     // its transcript, or the record in it, is gone
	cutUnread   = "unread"   // no reader of its harness here
	cutUnmasked = "unmasked" // read whole, but what was taken off it could not be masked
)

// recordMax is the longest record a whole read takes: a line of a transcript can hold a large tool output, and
// the reader holds one at a time.
const recordMax = 16 << 20

// take is one event's text in its record, and lets go of it: a transcript file's record as prefetch read it, a
// Hermes row read here. The raw text is kept no longer than the job that reads it.
func (r *reader) take(ctx context.Context, e store.Event, id string, output bool) (string, bool, string) {
	key := e.Ref + "\x00" + id
	if v, ok := r.records[key]; ok {
		delete(r.records, key)
		return v.text, v.ok, v.why
	}
	path, off, ok := refPlace(e.Ref)
	switch {
	case !ok || id == "":
		return "", false, cutUnread
	case e.Harness == "hermes":
		return r.hermesRecord(ctx, e, path, off, id, output)
	case fileHarness(e.Harness):
		return "", false, cutGone // prefetch reads every record it is asked for: one it has not is not there
	}
	return "", false, cutUnread
}

// fileHarness reports a harness whose records are lines of a transcript file tacit's reader opens.
func fileHarness(h string) bool { return h == "claude" || h == "codex" || h == "pi" }

type recordRead struct {
	text string
	ok   bool
	why  string
}

// errTooLong is a record longer than recordMax: passed over to its end, so it alone is lost.
var errTooLong = errors.New("record too long")

// readRecord reads the record (the line) at the reader's place: its bytes and how many it took from the reader. A
// record over recordMax is passed over to its end and read as errTooLong.
func readRecord(br *bufio.Reader) ([]byte, int64, error) {
	var line []byte
	var used int64
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		used += int64(len(chunk))
		if !long && len(line)+len(chunk) > recordMax {
			long, line = true, nil
		}
		if !long {
			line = append(line, chunk...)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
		case long && (err == nil || errors.Is(err, io.EOF)):
			return nil, used, errTooLong
		default:
			return line, used, err
		}
	}
}

// refPlace splits a reference (path@offset#index) into its file and the record's offset.
func refPlace(ref string) (string, int64, bool) {
	at := strings.LastIndexByte(ref, '@')
	if at <= 0 {
		return "", 0, false
	}
	path, rest := ref[:at], ref[at+1:]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	off, err := strconv.ParseInt(rest, 10, 64)
	return path, off, err == nil && strings.HasPrefix(path, "/")
}

// blockOf is the text of the block of a transcript record that holds the call with tool id id: the call's input
// (as JSON, or as the raw text of a custom tool) or its result's output (the text of its content). Claude Code
// keeps them in message.content (tool_use, tool_result), Codex in payload (function_call, custom_tool_call and
// their outputs), pi in message.content (toolCall) and a toolResult message.
func blockOf(line []byte, id string, output bool) (string, bool) {
	var rec struct {
		Message struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"toolCallId"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
		Payload struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return "", false
	}
	if p := rec.Payload; p.CallID == id && p.CallID != "" {
		switch {
		case output && (p.Type == "function_call_output" || p.Type == "custom_tool_call_output"):
			return contentText(p.Output)
		case !output && p.Type == "function_call":
			return p.Arguments, true
		case !output && p.Type == "custom_tool_call":
			return p.Input, true
		}
		return "", false
	}
	if output && rec.Message.Role == "toolResult" && rec.Message.ToolCallID == id {
		return contentText(rec.Message.Content)
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		ToolUseID string          `json:"tool_use_id"`
		Input     json.RawMessage `json:"input"`
		Arguments json.RawMessage `json:"arguments"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return "", false
	}
	for _, b := range blocks {
		switch {
		case !output && (b.Type == "tool_use" || b.Type == "toolCall") && b.ID == id:
			if len(b.Input) > 0 {
				return string(b.Input), true
			}
			return string(b.Arguments), len(b.Arguments) > 0
		case output && b.Type == "tool_result" && b.ToolUseID == id:
			return contentText(b.Content)
		}
	}
	return "", false
}

// contentText is a content's text: a string, or the text of its blocks, joined by newlines.
func contentText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), true
}

// hermesRecord is a Hermes row's call input (the arguments of its tool call with tool id id) or its output (the
// row's content), read with tacit's Hermes store by the session that holds it.
func (r *reader) hermesRecord(ctx context.Context, e store.Event, path string, msg int64, id string, output bool) (string, bool, string) {
	db, ok := r.hermes[path]
	if !ok {
		var err error
		if db, err = evidence.OpenHermes(path); err != nil {
			db = nil
		}
		r.hermes[path] = db
	}
	if db == nil {
		return "", false, cutGone
	}
	sid := e.Session
	if e.Agent != "" && e.Agent != e.Session {
		sid = e.Agent // a delegation's rows are its own session's
	}
	rows, err := db.Messages(ctx, sid, msg-1, 1) // the row itself: Messages reads at least one, here no more
	if err != nil || len(rows) == 0 || rows[0].ID != msg {
		return "", false, cutGone
	}
	m := rows[0]
	if output {
		if m.ToolCallID != id && m.ToolCallID != "" {
			return "", false, cutGone
		}
		return m.Content, true, ""
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal([]byte(m.ToolCalls), &calls) != nil {
		return "", false, cutGone
	}
	for _, c := range calls {
		if c.ID == id {
			return c.Function.Arguments, true, ""
		}
	}
	return "", false, cutGone
}

// pair is a call and its result, as the store holds them, with only the fields a whole read and the artifacts
// read: a thread holds one for each call the store cut, until the read ends.
type pair struct {
	call, res *store.Event
}

// slim is an event with only what a pair needs of it.
func slim(e store.Event) *store.Event {
	return &store.Event{Source: e.Source, SourceKey: e.SourceKey, TS: e.TS, Session: e.Session, Agent: e.Agent,
		Harness: e.Harness, Kind: e.Kind, Tool: e.Tool, Summary: e.Summary, Meta: e.Meta, Ref: e.Ref}
}

// storeCut reports a call or its result the store cut where what a request reads of it may lie past the cut: a
// shell call's command or output, a writer's input that names no path.
func storeCut(call, res store.Event) bool {
	tool := strings.ToLower(call.Tool)
	if writers[tool] {
		// a cut patch may name more files past the cut than its stored part does
		d := Decode(call.Harness, tool, call.Summary, "")
		return strings.HasSuffix(call.Summary, "…") && (len(d.Paths) == 0 || d.Patch)
	}
	return strings.HasSuffix(call.Summary, "…") || strings.HasSuffix(res.Summary, "…")
}

// storeJoined reports a shell call's output the store keeps with its lines flattened to spaces (every harness's
// plain text but Hermes's and Codex code mode's JSON), when it may show an artifact's line: only a whole read gives
// its lines.
func storeJoined(call, res store.Event) bool {
	tool := strings.ToLower(call.Tool)
	if writers[tool] || !toolresult.IsShell(tool) || call.Harness == "hermes" {
		return false
	}
	if call.Harness == "codex" && tool == "exec" && chunkKeyRe.MatchString(res.Summary) {
		return false // a code-mode script's results are JSON, their lines kept; a printed output text is not
	}
	return !strings.Contains(res.Summary, "\n") && resultMarkRe.MatchString(res.Summary)
}

// chunkKeyRe is a code-mode result's output field.
var chunkKeyRe = regexp.MustCompile(`"(?:output|stdout|stderr)"\s*:`)

// resultMarkRe is what an output that shows an artifact's line holds: a commit line's "] ", a ref update's " -> ",
// a pull request's "/pull/" (the store's read selects the shells' results by the same marks).
var resultMarkRe = regexp.MustCompile(`\] | -> |/pull/`)

// need is a record a whole read wants from a transcript file: its offset, the tool id of its block, and whether
// it is a result.
type need struct {
	off    int64
	id     string
	output bool
	key    string
}

// fetch is a record a read wants: an event's, by the call's tool id; output is true for a result.
type fetch struct {
	e      store.Event
	id     string
	output bool
}

// prefetch reads the records wanted from transcript files, each file once, in the order of its records' offsets:
// one pass forward, so a compressed rollout is decompressed once, not once per record.
func (r *reader) prefetch(fs []fetch) {
	byFile := map[string][]need{}
	for _, f := range fs {
		key := f.e.Ref + "\x00" + f.id
		if _, ok := r.records[key]; ok || f.id == "" {
			continue
		}
		path, off, ok := refPlace(f.e.Ref)
		if !ok || f.e.Harness != "claude" && f.e.Harness != "codex" && f.e.Harness != "pi" {
			continue // read one by one (Hermes), or unread
		}
		byFile[path] = append(byFile[path], need{off: off, id: f.id, output: f.output, key: key})
	}
	for path, ns := range byFile {
		sort.Slice(ns, func(i, j int) bool { return ns[i].off < ns[j].off })
		r.readFile(path, ns)
	}
}

// compressed reports a transcript tacit's reader opens from a compressed copy (a rollout Codex compressed).
func (r *reader) compressed(path string) bool {
	if v, ok := r.packed[path]; ok {
		return v
	}
	v := evidence.Resolve(path) != path
	r.packed[path] = v
	return v
}

// readFile reads a file's wanted records in offset order, the file opened once: a plain file by seeking to each
// record, a compressed copy in one pass forward, so it is decompressed once.
func (r *reader) readFile(path string, ns []need) {
	r.opens[path]++
	gone := func(from int) {
		for _, n := range ns[from:] {
			if _, ok := r.records[n.key]; !ok {
				r.records[n.key] = recordRead{why: cutGone}
			}
		}
	}
	var f io.ReadCloser
	var seeker io.Seeker
	var err error
	if r.compressed(path) {
		f, err = evidence.OpenAt(path, ns[0].off)
	} else {
		var pf *os.File
		pf, err = os.Open(path)
		f, seeker = pf, pf
	}
	if err != nil {
		gone(0)
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	pos := ns[0].off
	if seeker != nil {
		pos = -1
	}
	var line []byte
	lineAt := int64(-1)
	for i, n := range ns {
		if n.off != lineAt {
			switch {
			case seeker != nil:
				if _, err := seeker.Seek(n.off, io.SeekStart); err != nil {
					gone(i)
					return
				}
				br.Reset(f)
			default:
				// a place inside the record before it (the transcript is not as the store read it) is a negative
				// count, which Discard refuses
				if _, err := br.Discard(int(n.off - pos)); err != nil {
					gone(i)
					return
				}
			}
			l, used, err := readRecord(br)
			switch {
			case errors.Is(err, errTooLong):
				// this record alone is lost: the reader is at the next one
			case err != nil && !errors.Is(err, io.EOF) || len(l) == 0:
				gone(i)
				return
			}
			line, lineAt, pos = l, n.off, n.off+used
		}
		if text, ok := blockOf(line, n.id, n.output); ok {
			r.records[n.key] = recordRead{text: text, ok: true}
		} else {
			r.records[n.key] = recordRead{why: cutGone} // the record at the place is no longer the call's
		}
	}
}

// namesGit reports git or gh as a word of a decoded command (not the gh of "high"), by a scan of the text.
func namesGit(s string) bool {
	word := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for i := 0; i+1 < len(s); i++ {
		if s[i] != 'g' || i > 0 && word(s[i-1]) {
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "git") && (i+3 == len(s) || !word(s[i+3])):
			return true
		case s[i+1] == 'h' && (i+2 == len(s) || !word(s[i+2])):
			return true
		}
	}
	return false
}

// polls reports a Codex call that polls a session an earlier call left running: a code-mode script's write_stdin
// call, as the script's parse finds it, or Codex's write_stdin tool. Its output is another command's: it names no
// git of its own.
func polls(call store.Event, tool string, sc *script) bool {
	if call.Harness != "codex" {
		return false
	}
	if tool == "write_stdin" {
		return true
	}
	if tool != "exec" || sc == nil {
		return false
	}
	for _, c := range sc.calls {
		if !c.exec {
			return true
		}
	}
	return false
}

// codexCommand reports a Codex shell call whose results a session ties to others: a code-mode script, Codex's
// exec_command and write_stdin tools. Its thread reads them in order, so a poll finds the session it polls.
func codexCommand(call store.Event) bool {
	switch strings.ToLower(call.Tool) {
	case "exec", "exec_command", "write_stdin":
		return call.Harness == "codex"
	}
	return false
}

// readWhole reads whole, as its thread loads, the calls of the thread the store cut or joined, so what is held
// raw is one thread's records at most and no call is held past its thread's load. It reads a call's input where
// the store cut it, and its output where the store cut or joined it and the command makes something (a commit, a
// push, a pull request): in a compressed transcript in the same pass as the input, in a plain one by a seek after
// it. A command that names neither git nor gh is not parsed and its output not read; a writer's input is read
// alone. What each made is taken off the text read, and each raw record is let go of as the thread's read ends;
// what was taken off is masked at the end of the run (mask). A call whose record is not read is read as the store
// holds it, and noted by why (lost), for its request to count.
func (r *reader) readWhole(ctx context.Context, t *thread) {
	cut := func(s string) bool { return strings.HasSuffix(s, "…") }
	type job struct {
		p     pair
		tool  string
		id    string
		raw   bool // a text read whole
		why   string
		skip  bool // a command that names neither git nor gh: it makes nothing this reads
		poll  bool // it polls a session an earlier call left running: its output may be that command's
		mk    makes
		sc    *script // a code-mode script, parsed once
		found []Result
	}
	js := make([]job, len(t.pending))
	for i, p := range t.pending {
		js[i] = job{p: p, tool: strings.ToLower(p.call.Tool), id: metaOf(p.call.Meta).ToolID}
	}
	t.pending = nil
	// an output read whole: one the store cut, or one it joined (only a whole read gives its lines)
	whole := func(j *job) bool {
		return !writers[j.tool] && (cut(j.p.res.Summary) || storeJoined(*j.p.call, *j.p.res))
	}
	var first, second []fetch
	for i := range js {
		j := &js[i]
		if cut(j.p.call.Summary) {
			first = append(first, fetch{*j.p.call, j.id, false})
		}
		// a compressed transcript is read once: its output in the same pass as the input
		if path, _, ok := refPlace(j.p.res.Ref); ok && whole(j) && fileHarness(j.p.res.Harness) && r.compressed(path) {
			first = append(first, fetch{*j.p.res, j.id, true})
		}
	}
	r.prefetch(first)
	inputs := make([]string, len(js))
	for i := range js {
		j := &js[i]
		inputs[i] = j.p.call.Summary
		if cut(j.p.call.Summary) {
			if text, ok, why := r.take(ctx, *j.p.call, j.id, false); ok {
				inputs[i], j.raw = text, true
			} else {
				j.why = why
			}
		}
		if writers[j.tool] {
			continue
		}
		// every check reads the decoded commands, the text the shell parser sees, never the stored JSON, whose
		// escapes (a newline before git) hide a word
		if j.p.call.Harness == "codex" && j.tool == "exec" {
			j.sc = parseScript(inputs[i])
		}
		d := decode(j.p.call.Harness, j.tool, inputs[i], "", j.sc)
		j.poll = polls(*j.p.call, j.tool, j.sc)
		// a Codex call that names no git is read too: a session it left running replaces one of the same id
		if !namesGit(strings.Join(d.Commands, "\n")) && !j.poll && !codexCommand(*j.p.call) {
			// it makes nothing this reads, whether or not its record is there
			// a command the store cut and whose record is gone keeps its why: its git may lie past the cut
			j.skip = true
			continue
		}
		j.mk = made(d)
		if j.p.call.Harness == "codex" && j.tool == "exec" {
			// a code-mode script's results are read each against its own command: what any one makes
			for k, c := range d.Commands {
				m := made(Decoded{Commands: []string{c}, CommandCut: d.CommandCut && k == len(d.Commands)-1})
				j.mk.commits, j.mk.pushes, j.mk.prs = j.mk.commits || m.commits, j.mk.pushes || m.pushes, j.mk.prs || m.prs
			}
		}
		if (j.mk.commits || j.mk.pushes || j.mk.prs || j.poll) && whole(j) {
			second = append(second, fetch{*j.p.res, j.id, true})
		}
	}
	r.prefetch(second)
	for i := range js {
		j := &js[i]
		output, lines := j.p.res.Summary, false
		switch {
		case !whole(j):
		case j.skip || !(j.mk.commits || j.mk.pushes || j.mk.prs || j.poll):
			// not wanted: a record a pass read already is let go of, so no raw output stays behind, and none is read
			// for it (a Hermes row is read only when asked)
			delete(r.records, j.p.res.Ref+"\x00"+j.id)
		default:
			text, ok, why := r.take(ctx, *j.p.res, j.id, true)
			switch {
			case ok:
				output, j.raw, lines = text, true, true
			case j.why == "":
				j.why = why
			}
		}
		if j.why != "" {
			t.lost = append(t.lost, lost{at: j.p.res.TS, why: j.why})
		}
		if j.skip {
			continue
		}
		call, res := *j.p.call, *j.p.res
		call.Summary, res.Summary = inputs[i], output
		if writers[j.tool] {
			if !toolresult.WriteFailed(res.Summary, metaOf(res.Meta).IsError) {
				j.found = artifactsOf(call, res, Decode(call.Harness, j.tool, inputs[i], ""), makes{}, nil)
			}
		} else {
			d := decode(call.Harness, j.tool, inputs[i], output, j.sc)
			d.Joined = d.Joined && !lines // a text read whole has its lines, one line or many
			j.found = artifactsOf(call, res, d, j.mk, t.sessions)
			agent := agentOf(call)
			for _, p := range d.Polls {
				k := sessionKey{agent, p.Session}
				switch s, ok := t.sessions[k]; {
				case !p.Running:
					delete(t.sessions, k) // the process exited: its id may be reused
				case ok && s.keep:
					s.output += "\n" + p.Output // it still runs: what it printed is read when it ends
					t.sessions[k] = s
				}
			}
			for n, sess := range yielded(call, d) {
				t.sessions[sessionKey{agent, n}] = sess // a later call's poll of it reads its output against this command
			}
		}
		if len(j.found) == 0 {
			continue
		}
		from := len(t.items)
		for _, x := range j.found {
			x.Thread = t.id
			t.items = append(t.items, item{r: x})
		}
		if j.raw {
			// kept unmasked until the run's end masks it: else the call as the store holds it
			var fallback []Result
			for _, x := range Artifacts(*j.p.call, *j.p.res) {
				x.Thread = t.id
				fallback = append(fallback, x)
			}
			r.raws = append(r.raws, raw{t: t, from: from, to: len(t.items), fallback: fallback, at: j.p.res.TS})
		}
	}
}

// lost is a call of a thread whose record was not read whole, and why: its request counts it.
type lost struct {
	at  time.Time
	why string
}

// raw is what a call read whole made, in its thread's items from from to to, not yet masked, and what the call
// made as the store holds it.
type raw struct {
	t        *thread
	from, to int
	fallback []Result
	at       time.Time
}

// mask masks, with tacit's redaction in one call, each artifact's text and key read off raw text in the run. When
// masking fails, nothing read off the raw text is kept: those calls are read as the store holds them, and noted
// unmasked.
func (r *reader) mask(ctx context.Context) {
	if len(r.raws) == 0 {
		return
	}
	var texts []string
	for _, w := range r.raws {
		for _, it := range w.t.items[w.from:w.to] {
			texts = append(texts, it.r.Text, it.r.Key)
		}
	}
	masked, err := r.redact()(ctx, texts)
	k := 0
	for _, w := range r.raws {
		if err != nil || len(masked) != len(texts) {
			for i := w.from; i < w.to; i++ {
				w.t.items[i].gone = true
			}
			for _, x := range w.fallback {
				w.t.items = append(w.t.items, item{r: x})
			}
			w.t.lost = append(w.t.lost, lost{at: w.at, why: cutUnmasked})
			continue
		}
		for i := w.from; i < w.to; i++ {
			w.t.items[i].r.Text, w.t.items[i].r.Key = masked[k], masked[k+1]
			k += 2
		}
	}
	r.raws = nil
}

// redact is what masks secrets in text this read keeps from outside the store: the option's, else tacit's redaction.
func (r *reader) redact() func(context.Context, []string) ([]string, error) {
	if r.opt.Redact != nil {
		return r.opt.Redact
	}
	return evidence.RedactAllContext
}
