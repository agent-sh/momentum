package request

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
)

// Why a cut call was read as the store holds it.
const (
	cutGone   = "gone"   // its transcript, or the record in it, is gone
	cutUnread = "unread" // no reader of its harness here, or its text could not be redacted
)

// recordMax is the longest record a whole read takes: a line of a transcript can hold a large tool output, and
// the reader holds one at a time.
const recordMax = 16 << 20

// record is one event's text in its record: a call's input or a result's output.
func (r *reader) record(ctx context.Context, e store.Event, id string, output bool) (string, bool, string) {
	key := e.Ref + "\x00" + id
	if v, ok := r.records[key]; ok {
		return v.text, v.ok, v.why
	}
	text, ok, why := r.readRecord(ctx, e, id, output)
	r.records[key] = recordRead{text, ok, why}
	return text, ok, why
}

type recordRead struct {
	text string
	ok   bool
	why  string
}

func (r *reader) readRecord(ctx context.Context, e store.Event, id string, output bool) (string, bool, string) {
	path, off, ok := refPlace(e.Ref)
	if !ok || id == "" {
		return "", false, cutUnread
	}
	if e.Harness == "hermes" {
		return r.hermesRecord(ctx, e, path, off, id, output)
	}
	if e.Harness != "claude" && e.Harness != "codex" && e.Harness != "pi" {
		return "", false, cutUnread
	}
	f, err := evidence.OpenAt(path, off)
	if err != nil {
		return "", false, cutGone
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		line, err = readLong(br, line)
	}
	if err != nil && !errors.Is(err, io.EOF) || len(line) == 0 {
		return "", false, cutGone
	}
	text, ok := blockOf(line, id, output)
	if !ok {
		return "", false, cutGone // the record at the place is no longer the call's: the transcript was rewritten
	}
	return text, true, ""
}

// readLong reads the rest of a line longer than the reader's buffer, up to recordMax.
func readLong(br *bufio.Reader, head []byte) ([]byte, error) {
	line := append([]byte(nil), head...)
	for len(line) <= recordMax {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
	return nil, errors.New("record too long")
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

// pair is a call and its result, as the store holds them.
type pair struct {
	call, res store.Event
}

// resolvedPair is what a cut call made, read whole, and why it was not when it was not.
type resolvedPair struct {
	results []Result
	why     string
}

// storeCut reports a call or its result the store cut where what a request reads of it may lie past the cut: a
// shell call's command or output, a writer's input that names no path.
func storeCut(call, res store.Event) bool {
	tool := strings.ToLower(call.Tool)
	if writers[tool] {
		return strings.HasSuffix(call.Summary, "…") && len(Decode(call.Harness, tool, call.Summary, "").Paths) == 0
	}
	return strings.HasSuffix(call.Summary, "…") || strings.HasSuffix(res.Summary, "…")
}

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

// readFile reads a file's wanted records in offset order, through tacit's reader.
func (r *reader) readFile(path string, ns []need) {
	gone := func(from int) {
		for _, n := range ns[from:] {
			if _, ok := r.records[n.key]; !ok {
				r.records[n.key] = recordRead{why: cutGone}
			}
		}
	}
	f, err := evidence.OpenAt(path, ns[0].off)
	if err != nil {
		gone(0)
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	pos := ns[0].off
	var line []byte
	lineAt := int64(-1)
	for i, n := range ns {
		if n.off != lineAt {
			if n.off < pos {
				gone(i) // overlapping places: the transcript is not as the store read it
				return
			}
			if _, err := br.Discard(int(n.off - pos)); err != nil {
				gone(i)
				return
			}
			l, err := br.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				l, err = readLong(br, l)
			} else {
				l = append([]byte(nil), l...)
			}
			if err != nil && !errors.Is(err, io.EOF) || len(l) == 0 {
				gone(i)
				return
			}
			line, lineAt, pos = l, n.off, n.off+int64(len(l))
		}
		if text, ok := blockOf(line, n.id, n.output); ok {
			r.records[n.key] = recordRead{text: text, ok: true}
		} else {
			r.records[n.key] = recordRead{why: cutGone} // the record at the place is no longer the call's
		}
	}
}

// resolve reads whole the calls the store cut that the requests hold, each once per run. It reads a call's input
// where the store cut it, then its output where the store cut it and the command makes something (a commit, a
// push, a pull request); a writer's input only. What each made is taken off the text read, and only that (each
// artifact's text and key) is masked, with tacit's redaction in one call, before it is kept: the raw text is read
// for its artifacts and dropped. A call whose record is not read, or whose artifacts cannot be masked, is read as
// the store holds it, and counted on its request.
func (r *reader) resolve(ctx context.Context, reqs []*Request) error {
	key := func(p pair) string { return p.call.Ref + "\x00" + p.res.Ref }
	cut := func(s string) bool { return strings.HasSuffix(s, "…") }
	var todo []pair
	for _, rq := range reqs {
		for _, p := range rq.pending {
			if _, ok := r.resolved[key(p)]; !ok {
				r.resolved[key(p)] = resolvedPair{} // claimed: a pair two requests hold is read once
				todo = append(todo, p)
			}
		}
	}
	type job struct {
		p             pair
		input, output string
		raw           bool // a text read whole
		why           string
	}
	jobs := make([]job, len(todo))
	var ins []fetch
	for i, p := range todo {
		jobs[i] = job{p: p, input: p.call.Summary, output: p.res.Summary}
		if cut(p.call.Summary) {
			ins = append(ins, fetch{p.call, metaOf(p.call.Meta).ToolID, false})
		}
	}
	r.prefetch(ins)
	var outs []fetch
	for i := range jobs {
		j := &jobs[i]
		id := metaOf(j.p.call.Meta).ToolID
		if cut(j.p.call.Summary) {
			if text, ok, why := r.record(ctx, j.p.call, id, false); ok {
				j.input, j.raw = text, true
			} else {
				j.why = why
			}
		}
		tool := strings.ToLower(j.p.call.Tool)
		if writers[tool] || !cut(j.p.res.Summary) {
			continue
		}
		if mk := made(Decode(j.p.call.Harness, tool, j.input, "").Commands); mk.commits || mk.pushes || mk.prs {
			outs = append(outs, fetch{j.p.res, id, true})
		}
	}
	r.prefetch(outs)
	wanted := map[string]bool{}
	for _, f := range outs {
		wanted[f.e.Ref] = true
	}
	results := make([][]Result, len(jobs))
	var texts []string
	for i := range jobs {
		j := &jobs[i]
		if wanted[j.p.res.Ref] {
			if text, ok, why := r.record(ctx, j.p.res, metaOf(j.p.call.Meta).ToolID, true); ok {
				j.output, j.raw = text, true
			} else if j.why == "" {
				j.why = why
			}
		}
		call, res := j.p.call, j.p.res
		call.Summary, res.Summary = j.input, j.output
		results[i] = Artifacts(call, res)
		if j.raw {
			for _, x := range results[i] {
				texts = append(texts, x.Text, x.Key)
			}
		}
	}
	redact := r.opt.Redact
	if redact == nil {
		redact = evidence.RedactAllContext
	}
	var masked []string
	if len(texts) > 0 {
		var err error
		if masked, err = redact(ctx, texts); err != nil || len(masked) != len(texts) {
			masked = nil
		}
	}
	k := 0
	for i := range jobs {
		j := &jobs[i]
		if j.raw {
			if masked == nil && len(texts) > 0 {
				// nothing read off the raw text is kept unmasked: the call is read as the store holds it
				results[i], j.why = Artifacts(j.p.call, j.p.res), cutUnread
			} else {
				for x := range results[i] {
					results[i][x].Text, results[i][x].Key = masked[k], masked[k+1]
					k += 2
				}
			}
		}
		for x := range results[i] {
			results[i][x].Thread = j.p.res.Session
		}
		r.resolved[key(j.p)] = resolvedPair{results: results[i], why: j.why}
	}
	for _, rq := range reqs {
		for _, p := range rq.pending {
			rp := r.resolved[key(p)]
			rq.Results = append(rq.Results, rp.results...)
			switch rp.why {
			case cutGone:
				rq.CutGone++
			case cutUnread:
				rq.CutUnread++
			}
		}
		rq.pending = nil
	}
	return nil
}
