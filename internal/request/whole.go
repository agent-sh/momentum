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

// storeJoined reports a shell call's output the store keeps with its lines flattened to spaces (every harness's
// plain text but Hermes's and Codex code mode's JSON), when it may show an artifact's line: only a whole read gives
// its lines.
func storeJoined(call, res store.Event) bool {
	tool := strings.ToLower(call.Tool)
	if writers[tool] || !toolresult.IsShell(tool) || call.Harness == "hermes" || call.Harness == "codex" && tool == "exec" {
		return false
	}
	return !strings.Contains(res.Summary, "\n") && resultMarkRe.MatchString(res.Summary)
}

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

// gitWordRe is git or gh as a word of a command: not the gh of "high".
var gitWordRe = regexp.MustCompile(`\b(?:git|gh)\b`)

// resolve reads whole the calls the store cut that the requests hold, each once per run, one thread at a time, so
// what is held raw is one thread's records at most. It reads a call's input where the store cut it, and its output
// where the store cut it and the command makes something (a commit, a push, a pull request): in a compressed
// transcript in the same pass as the input, in a plain one by a seek after it. A command that names neither git
// nor gh is not parsed and its output not read; a writer's input is read alone. What each made is taken off the
// text read, and each raw record is let go of as its thread's read ends; only what was taken off (each artifact's
// text and key) is masked, with tacit's redaction in one call, before it is kept. A call whose record is not read,
// or whose artifacts cannot be masked, is read as the store holds it, and counted on its request by why.
func (r *reader) resolve(ctx context.Context, reqs []*Request) error {
	// a call is one by its session and tool id
	key := func(p pair) string { return p.call.Session + "\x00" + metaOf(p.call.Meta).ToolID }
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
	sort.SliceStable(todo, func(a, b int) bool { return todo[a].call.Session < todo[b].call.Session })
	type job struct {
		p     pair
		tool  string
		id    string
		raw   bool // a text read whole
		why   string
		skip  bool // a command that names neither git nor gh: it makes nothing this reads
		mk    makes
		found []Result
	}
	jobs := make([]job, len(todo))
	for i, p := range todo {
		jobs[i] = job{p: p, tool: strings.ToLower(p.call.Tool), id: metaOf(p.call.Meta).ToolID}
	}
	// read reads one thread's jobs whole and takes off what each made, letting go of the raw records
	// an output read whole: one the store cut, or one it joined (only a whole read gives its lines)
	whole := func(j *job) bool {
		return !writers[j.tool] && (cut(j.p.res.Summary) || storeJoined(*j.p.call, *j.p.res))
	}
	read := func(js []job) {
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
			if !gitWordRe.MatchString(inputs[i]) {
				// it makes nothing this reads, whether or not its record is there
				j.skip, j.why = true, ""
				continue
			}
			j.mk = made(Decode(j.p.call.Harness, j.tool, inputs[i], ""))
			if (j.mk.commits || j.mk.pushes || j.mk.prs) && whole(j) {
				second = append(second, fetch{*j.p.res, j.id, true})
			}
		}
		r.prefetch(second)
		for i := range js {
			j := &js[i]
			output, lines := j.p.res.Summary, false
			if whole(j) {
				// taken whether used or not, so no raw output stays behind
				text, ok, why := r.take(ctx, *j.p.res, j.id, true)
				switch {
				case j.skip || !(j.mk.commits || j.mk.pushes || j.mk.prs):
				case ok:
					output, j.raw, lines = text, true, true
				case j.why == "":
					j.why = why
				}
			}
			if j.skip {
				continue
			}
			call, res := *j.p.call, *j.p.res
			call.Summary, res.Summary = inputs[i], output
			if writers[j.tool] {
				if !metaOf(res.Meta).IsError {
					j.found = artifactsOf(call, res, Decode(call.Harness, j.tool, inputs[i], ""), makes{})
				}
			} else {
				d := Decode(call.Harness, j.tool, inputs[i], output)
				d.Joined = d.Joined && !lines // a text read whole has its lines, one line or many
				j.found = artifactsOf(call, res, d, j.mk)
			}
		}
	}
	for a := 0; a < len(jobs); {
		b := a + 1
		for b < len(jobs) && jobs[b].p.call.Session == jobs[a].p.call.Session {
			b++
		}
		read(jobs[a:b])
		a = b
	}
	var texts []string
	for i := range jobs {
		if jobs[i].raw {
			for _, x := range jobs[i].found {
				texts = append(texts, x.Text, x.Key)
			}
		}
	}
	var masked []string
	if len(texts) > 0 {
		var err error
		if masked, err = r.redact()(ctx, texts); err != nil || len(masked) != len(texts) {
			masked = nil
		}
	}
	k := 0
	for i := range jobs {
		j := &jobs[i]
		if j.raw && !j.skip && len(j.found) > 0 {
			if masked == nil {
				// nothing read off the raw text is kept unmasked: the call is read as the store holds it
				j.found, j.why = Artifacts(*j.p.call, *j.p.res), cutUnmasked
			} else {
				for x := range j.found {
					j.found[x].Text, j.found[x].Key = masked[k], masked[k+1]
					k += 2
				}
			}
		}
		for x := range j.found {
			j.found[x].Thread = j.p.res.Session
		}
		r.resolved[key(j.p)] = resolvedPair{results: j.found, why: j.why}
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
			case cutUnmasked:
				rq.CutUnmasked++
			}
		}
		rq.pending = nil
	}
	return nil
}

// redact is what masks secrets in text this read keeps from outside the store: the option's, else tacit's redaction.
func (r *reader) redact() func(context.Context, []string) ([]string, error) {
	if r.opt.Redact != nil {
		return r.opt.Redact
	}
	return evidence.RedactAllContext
}
