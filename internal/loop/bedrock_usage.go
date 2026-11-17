package loop

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"strings"
)

// These are Smithy's event-stream payload and headers limits.
// https://smithy.io/2.0/aws/amazon-eventstream.html
const maxEventFrame = (24 << 20) + (128 << 10) + 16

type relayUsage struct {
	Input  *int64 `json:"input_tokens"`
	Output *int64 `json:"output_tokens"`
	Read   int64  `json:"cache_read_input_tokens"`
	Write  int64  `json:"cache_creation_input_tokens"`
	Cache  struct {
		Five int64 `json:"ephemeral_5m_input_tokens"`
		Hour int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	// Iterations are the sampling passes of one response, each with its own usage. The top-level counts leave
	// out a pass the provider ran on its side (a compaction's), so what a response is billed is their sum. They
	// are kept as they came and read once, at the end (passes): decoding them in every event would hold many
	// times their bytes.
	Iterations json.RawMessage `json:"iterations"`
}

// A response lists a handful of passes. More than maxPasses, or more than maxPassBytes of them, is no usage the
// relay prices: the reservation stands.
const (
	maxPasses    = 64
	maxPassBytes = 64 << 10
)

// passes are a usage's iterations, or false when they are not a list of at most maxPasses valid usages.
func (u relayUsage) passes() ([]relayUsage, bool) {
	if len(u.Iterations) == 0 || string(u.Iterations) == "null" {
		return nil, true
	}
	if len(u.Iterations) > maxPassBytes {
		return nil, false
	}
	var list []relayUsage
	if json.Unmarshal(u.Iterations, &list) != nil || len(list) > maxPasses {
		return nil, false
	}
	for i, it := range list {
		if !it.valid() {
			return nil, false
		}
		if it.Cache.Hour == 0 && it.Cache.Five == 0 {
			list[i].Cache.Hour = it.Write // a pass that names what it wrote and no split is priced at the dearer write
		}
	}
	return list, true
}

// listed says a usage names its passes: a list with something in it.
func (u relayUsage) listed() bool {
	s := strings.TrimSpace(string(u.Iterations))
	return s != "" && s != "null" && s != "[]"
}

// price is what a usage costs at a rate, in dollars.
func (u relayUsage) price(r relayRate) float64 {
	return (float64(*u.Input)*r.input + float64(*u.Output)*r.output +
		float64(u.Read)*r.read + float64(u.Write-u.Cache.Hour)*r.write5 +
		float64(u.Cache.Hour)*r.write1) / 1e6
}

// Usage counts only usage observed on completed model responses, by billed kind.
type Usage struct {
	Input, Output, CacheRead, CacheWrite5, CacheWrite1 int64
}

func (u *Usage) add(v Usage) {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite5 += v.CacheWrite5
	u.CacheWrite1 += v.CacheWrite1
}

func (u Usage) tokens() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite5 + u.CacheWrite1
}

func (u relayUsage) counts() Usage {
	return Usage{Input: *u.Input, Output: *u.Output, CacheRead: u.Read, CacheWrite5: u.Write - u.Cache.Hour, CacheWrite1: u.Cache.Hour}
}

func (u relayUsage) valid() bool {
	return u.Input != nil && u.Output != nil && *u.Input >= 0 && *u.Output >= 0 &&
		u.Read >= 0 && u.Write >= 0 && u.Cache.Five >= 0 && u.Cache.Hour >= 0 && u.Cache.Hour <= u.Write
}

// usageParser receives unchanged wire bytes a piece at a time. It keeps one bounded
// frame, bounded report inputs, or a bounded non-streaming response, and requires
// complete final usage.
type usageParser struct {
	stream                    bool
	buf                       []byte
	want                      int
	bad, eof, start, final    bool
	usage                     relayUsage
	iterations                json.RawMessage // the last event's, when the response lists its passes
	blocks                    map[int]*reportBlock
	reports                   []json.RawMessage
	reportBytes, pendingBytes int
	refused                   bool
	errorResponse, errorOver  bool
}

type reportBlock struct {
	input json.RawMessage
	delta []byte
}

// Canonical object JSON preserves number precision and ignores key order and whitespace.
func canonicalReport(raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, io.ErrUnexpectedEOF
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, io.ErrUnexpectedEOF
	}
	return json.Marshal(value)
}

func (p *usageParser) report(raw []byte) {
	canonical, err := canonicalReport(raw)
	if err != nil || p.reportBytes+len(canonical) > relayBodyLimit {
		p.bad = true
		return
	}
	p.reportBytes += len(canonical)
	p.reports = append(p.reports, canonical)
}

func (p *usageParser) feed(b []byte) {
	if p.errorResponse {
		if p.errorOver {
			return
		}
		if len(p.buf)+len(b) > relayBodyLimit {
			p.errorOver, p.buf = true, nil
			return
		}
		p.buf = append(p.buf, b...)
		return
	}
	if p.bad {
		return
	}
	if !p.stream {
		if len(p.buf)+len(b) > relayBodyLimit {
			p.bad = true
			p.buf = nil
			return
		}
		p.buf = append(p.buf, b...)
		return
	}
	for len(b) > 0 {
		want := p.want
		if want == 0 {
			want = 12
		}
		k := min(want-len(p.buf), len(b))
		p.buf = append(p.buf, b[:k]...)
		b = b[k:]
		if len(p.buf) != want {
			continue
		}
		if p.want == 0 {
			total := int(binary.BigEndian.Uint32(p.buf[:4]))
			headers := int(binary.BigEndian.Uint32(p.buf[4:8]))
			if total < 16 || total > maxEventFrame || headers < 0 || headers > 128<<10 || headers > total-16 || total-headers-16 > 24<<20 ||
				binary.BigEndian.Uint32(p.buf[8:12]) != crc32.ChecksumIEEE(p.buf[:8]) {
				p.bad = true
				p.buf = nil
				return
			}
			frame := make([]byte, 12, total)
			copy(frame, p.buf)
			p.buf = frame
			p.want = total
			continue
		}
		if binary.BigEndian.Uint32(p.buf[want-4:]) != crc32.ChecksumIEEE(p.buf[:want-4]) {
			p.bad = true
			p.buf = nil
			return
		}
		headers := int(binary.BigEndian.Uint32(p.buf[4:8]))
		var envelope struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(p.buf[12+headers:want-4], &envelope) != nil {
			p.bad = true
			p.buf = nil
			return
		}
		event, err := base64.StdEncoding.DecodeString(envelope.Bytes)
		if err != nil {
			p.bad = true
			p.buf = nil
			return
		}
		var e struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Content struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type       string `json:"type"`
				Partial    string `json:"partial_json"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage relayUsage `json:"usage"`
			} `json:"message"`
			Usage relayUsage `json:"usage"`
		}
		if json.Unmarshal(event, &e) != nil {
			p.bad = true
			p.buf = nil
			return
		}
		switch e.Type {
		case "message_start":
			if p.start || !e.Message.Usage.valid() {
				p.bad = true
			} else {
				p.usage, p.start = e.Message.Usage, true
				if p.usage.listed() {
					p.iterations = p.usage.Iterations
				}
				p.usage.Iterations = nil // kept once, in p.iterations
			}
		case "message_delta":
			if !p.start || e.Usage.Output == nil || *e.Usage.Output < *p.usage.Output {
				p.bad = true
			} else {
				p.usage.Output, p.final = e.Usage.Output, true
				p.grow(e.Usage)
			}
			p.refused = p.refused || e.Delta.StopReason == "refusal"
		case "content_block_start":
			if e.Content.Type == "tool_use" && e.Content.Name == "StructuredOutput" {
				if p.blocks == nil {
					p.blocks = make(map[int]*reportBlock)
				}
				if !p.start || len(e.Content.Input) == 0 || p.blocks[e.Index] != nil || p.reportBytes+p.pendingBytes+len(e.Content.Input) > relayBodyLimit {
					p.bad = true
				} else {
					p.blocks[e.Index] = &reportBlock{input: e.Content.Input}
					p.pendingBytes += len(e.Content.Input)
				}
			}
		case "content_block_delta":
			if block := p.blocks[e.Index]; block != nil && e.Delta.Type == "input_json_delta" {
				if p.reportBytes+p.pendingBytes+len(e.Delta.Partial) > relayBodyLimit {
					p.bad = true
				} else {
					block.delta = append(block.delta, e.Delta.Partial...)
					p.pendingBytes += len(e.Delta.Partial)
				}
			}
		case "content_block_stop":
			if block := p.blocks[e.Index]; block != nil {
				raw := block.input
				if len(block.delta) > 0 {
					raw = block.delta
				}
				p.pendingBytes -= len(block.input) + len(block.delta)
				p.report(raw)
				delete(p.blocks, e.Index)
			}
		}
		p.buf, p.want = p.buf[:0], 0
		if p.bad {
			p.buf = nil
			return
		}
	}
}

// grow takes the input side of a later event's usage. The counts of a message_delta are cumulative: a response
// that loaded more on the provider's side after its first event (tool definitions it searched for) says so there,
// and the first event's counts are then less than what is billed. No count goes down, the one-hour part of the
// writes among them; a later event that names none leaves the first event's. A message_delta names the written
// total without its split, so what the total grew by is priced as one-hour writes, the dearer kind, unless the
// event's own split says more of them. An event whose counts make no sense leaves the usage as it was, unread.
func (p *usageParser) grow(u relayUsage) {
	if u.Read < 0 || u.Write < 0 || u.Cache.Hour < 0 || u.Cache.Five < 0 || u.Cache.Hour > u.Write || u.Input != nil && *u.Input < 0 {
		p.bad = true
		return
	}
	if u.Input != nil && *u.Input > *p.usage.Input {
		p.usage.Input = u.Input
	}
	p.usage.Read = max(p.usage.Read, u.Read)
	hour := max(p.usage.Cache.Hour, u.Cache.Hour)
	if u.Write > p.usage.Write {
		hour = max(hour, p.usage.Cache.Hour+u.Write-p.usage.Write)
		p.usage.Write = u.Write
	}
	p.usage.Cache.Hour = min(hour, p.usage.Write)
	if u.listed() { // a later event that names none leaves an earlier event's
		p.iterations = u.Iterations
	}
}

// known is what the input side of a response is known to cost though its usage was not read to the end: the
// counts of its first event, raised by any later one that was read, or nothing when there was none.
func (p *usageParser) known(r relayRate) float64 {
	if !p.start || !p.usage.valid() {
		return 0
	}
	u, none := p.usage, int64(0)
	u.Output = &none
	return u.price(r)
}

func (p *usageParser) cost(r relayRate) (float64, bool) {
	if p.errorResponse && p.eof && !p.errorOver {
		var response struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(p.buf, &response) == nil {
			p.refused = errorKind(firstOf(response.Error.Message, response.Message)) == "the model's safeguards"
		}
	}
	if p.bad || !p.eof {
		return 0, false
	}
	if p.stream {
		if len(p.buf) != 0 || !p.start || !p.final || len(p.blocks) != 0 {
			return 0, false
		}
	} else {
		var m struct {
			Usage      relayUsage `json:"usage"`
			StopReason string     `json:"stop_reason"`
			Content    []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if json.Unmarshal(p.buf, &m) != nil || !m.Usage.valid() {
			return 0, false
		}
		p.usage, p.iterations = m.Usage, m.Usage.Iterations
		p.refused = m.StopReason == "refusal"
		for _, block := range m.Content {
			if block.Type == "tool_use" && block.Name == "StructuredOutput" {
				p.report(block.Input)
			}
		}
		if p.bad {
			return 0, false
		}
	}
	// A response that lists its passes is billed their sum, which the top-level counts can fall short of. The
	// charge is the larger of the two readings, so neither shape of usage prices under the other.
	list, ok := relayUsage{Iterations: p.iterations}.passes()
	if !ok {
		return 0, false
	}
	cost, passes := p.usage.price(r), 0.0
	for _, it := range list {
		passes += it.price(r)
	}
	return max(cost, passes), true
}

type usageBody struct {
	io.ReadCloser
	parser   *usageParser
	complete func()
}

func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.parser.feed(p[:n])
	if err == io.EOF {
		b.parser.eof = true
		if b.complete != nil {
			b.complete()
		}
	}
	return n, err
}
