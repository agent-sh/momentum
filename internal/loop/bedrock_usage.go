package loop

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
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
}

func (u relayUsage) valid() bool {
	return u.Input != nil && u.Output != nil && *u.Input >= 0 && *u.Output >= 0 &&
		u.Read >= 0 && u.Write >= 0 && u.Cache.Five >= 0 && u.Cache.Hour >= 0 && u.Cache.Hour <= u.Write
}

// usageParser receives unchanged wire bytes a piece at a time. It keeps one bounded
// frame, or a bounded non-streaming response, and requires complete final usage.
type usageParser struct {
	stream                 bool
	buf                    []byte
	want                   int
	bad, eof, start, final bool
	usage                  relayUsage
}

func (p *usageParser) feed(b []byte) {
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
			}
		case "message_delta":
			if !p.start || e.Usage.Output == nil || *e.Usage.Output < *p.usage.Output {
				p.bad = true
			} else {
				p.usage.Output, p.final = e.Usage.Output, true
			}
		}
		p.buf, p.want = p.buf[:0], 0
		if p.bad {
			p.buf = nil
			return
		}
	}
}

func (p *usageParser) cost(r relayRate) (float64, bool) {
	if p.bad || !p.eof {
		return 0, false
	}
	if p.stream {
		if len(p.buf) != 0 || !p.start || !p.final {
			return 0, false
		}
	} else {
		var m struct {
			Usage relayUsage `json:"usage"`
		}
		if json.Unmarshal(p.buf, &m) != nil || !m.Usage.valid() {
			return 0, false
		}
		p.usage = m.Usage
	}
	u := p.usage
	return (float64(*u.Input)*r.input + float64(*u.Output)*r.output +
		float64(u.Read)*r.read + float64(u.Write-u.Cache.Hour)*r.write5 +
		float64(u.Cache.Hour)*r.write1) / 1e6, true
}

type usageBody struct {
	io.ReadCloser
	parser *usageParser
}

func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.parser.feed(p[:n])
	if err == io.EOF {
		b.parser.eof = true
	}
	return n, err
}
