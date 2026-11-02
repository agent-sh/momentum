package loop

import (
	"io"
)

// Capture retains at most Limit plus one byte. An oversized stream is withheld whole,
// never passed to a scrubber as a prefix of a record.
type Capture struct {
	buf   []byte
	Limit int
}

func (c *Capture) Write(p []byte) (int, error) {
	n := len(p)
	if left := c.Limit + 1 - c.Len(); left > 0 {
		k := min(left, n)
		want := len(c.buf) + k
		if want > cap(c.buf) {
			grown := make([]byte, len(c.buf), min(c.Limit+1, max(want, 2*cap(c.buf))))
			copy(grown, c.buf)
			c.buf = grown
		}
		c.buf = append(c.buf, p[:k]...)
	}
	return n, nil
}

func (c *Capture) Over() bool     { return c.Len() > c.Limit }
func (c *Capture) Len() int       { return len(c.buf) }
func (c *Capture) Bytes() []byte  { return c.buf }
func (c *Capture) String() string { return string(c.buf) }

// boundedFile bounds archives while the harness produces them, without buffering them.
type boundedFile struct {
	w    io.Writer
	left int64
	over bool
}

func (w *boundedFile) Write(p []byte) (int, error) {
	n := len(p)
	k := min(int64(n), w.left)
	if k > 0 {
		if _, err := w.w.Write(p[:k]); err != nil {
			return 0, err
		}
		w.left -= k
	}
	if int64(n) > k {
		w.over = true
	}
	return n, nil
}
