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

// wholeText is a call's input and its result's output read whole from their transcript; ok is false, with why,
// when it read neither.
type wholeText struct {
	input, output string
	ok            bool
	why           string
}

// whole reads a call's input and its result's output whole from their transcript, where the store cut them: by
// the events' references (the transcript's path and the record's offset; a Hermes store and the message's id),
// while the record is there. A record is read once per run. Claude Code, Codex and pi records are opened with
// tacit's reader (evidence.OpenAt, which reads a compressed copy too), Hermes rows with tacit's Hermes store; the
// call's block is the one with the call's tool id, as the replay reads a record (supervise). The text read is raw:
// the caller redacts it before it reads anything off it.
func (r *reader) whole(ctx context.Context, call, res store.Event) wholeText {
	id := metaOf(call.Meta).ToolID
	in, okIn, whyIn := r.record(ctx, call, id, false)
	out, okOut, whyOut := r.record(ctx, res, id, true)
	w := wholeText{input: call.Summary, output: res.Summary, ok: okIn || okOut}
	if okIn {
		w.input = in
	}
	if okOut {
		w.output = out
	}
	switch {
	case !w.ok && whyIn == cutUnread || !w.ok && whyOut == cutUnread:
		w.why = cutUnread
	case !w.ok:
		w.why = cutGone
	}
	return w
}

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
	rows, err := db.Messages(ctx, sid, msg-1, recordMax)
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

// prefetch reads the records the pairs want from their transcript files, each file once, in the order of its
// records' offsets: one pass forward, so a compressed rollout is decompressed once, not once per record.
func (r *reader) prefetch(pairs []pair) {
	byFile := map[string][]need{}
	for _, p := range pairs {
		id := metaOf(p.call.Meta).ToolID
		for _, x := range []struct {
			e      store.Event
			output bool
		}{{p.call, false}, {p.res, true}} {
			key := x.e.Ref + "\x00" + id
			if _, ok := r.records[key]; ok || id == "" {
				continue
			}
			path, off, ok := refPlace(x.e.Ref)
			if !ok || x.e.Harness != "claude" && x.e.Harness != "codex" && x.e.Harness != "pi" {
				continue // read one by one (Hermes), or unread
			}
			byFile[path] = append(byFile[path], need{off: off, id: id, output: x.output, key: key})
		}
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

// resolve reads whole the calls the store cut that the requests hold, each once per run: the records first, then
// their texts redacted in one call, then what each made. A call whose record is not read is read as the store holds
// it, and counted on its request.
func (r *reader) resolve(ctx context.Context, reqs []*Request) error {
	key := func(p pair) string { return p.call.Ref + "\x00" + p.res.Ref }
	var todo []pair
	for _, rq := range reqs {
		for _, p := range rq.pending {
			if _, ok := r.resolved[key(p)]; !ok {
				todo = append(todo, p)
			}
		}
	}
	r.prefetch(todo)
	type job struct {
		p pair
		w wholeText
	}
	var jobs []job
	var texts []string
	for _, rq := range reqs {
		for _, p := range rq.pending {
			if _, ok := r.resolved[key(p)]; ok {
				continue
			}
			r.resolved[key(p)] = resolvedPair{} // claimed: a pair two requests hold is read once
			w := r.whole(ctx, p.call, p.res)
			jobs = append(jobs, job{p, w})
			if w.ok {
				texts = append(texts, w.input, w.output)
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
			masked = nil // nothing raw is read off: the calls are read as the store holds them
		}
	}
	i := 0
	for _, j := range jobs {
		call, res, why := j.p.call, j.p.res, j.w.why
		if j.w.ok {
			if masked != nil {
				call.Summary, res.Summary = masked[i], masked[i+1]
			} else {
				why = cutUnread
			}
			i += 2
		}
		out := Artifacts(call, res)
		for k := range out {
			out[k].Thread = j.p.res.Session
		}
		r.resolved[key(j.p)] = resolvedPair{results: out, why: why}
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
