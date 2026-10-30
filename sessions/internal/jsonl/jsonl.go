// Package jsonl reads append-only JSONL files incrementally. A Reader hands over only the complete lines added
// since its previous read, and starts over when the file shrinks, is replaced, or is rewritten in place, so a
// tracker built on it never mixes the state of two files.
//
// The file identity follows the OpenTelemetry filelog receiver, as momentum's log sources do: the inode plus a
// fingerprint of the file's first bytes. Like Fluent Bit's offset marker, a Reader also keeps the bytes just
// before its position, which tells a rewrite that kept the first bytes from an append. A Position carries all of
// it, so a Reader in another process resumes where this one stopped.
package jsonl

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"syscall"
)

// fingerprint is how many leading bytes identify a file's content, and how many bytes before the position mark
// where reading stopped.
const fingerprint = 512

// Reader remembers how far into one file it has read.
type Reader struct {
	Path string
	off  int64
	dev  uint64
	ino  uint64
	head []byte // the file's first bytes, up to fingerprint, as last read
	mark []byte // the bytes just before off, up to fingerprint
}

// Position is how far a Reader has read and what identifies the file it read.
type Position struct {
	Off  int64  `json:"off"`
	Dev  uint64 `json:"dev,omitempty"`
	Ino  uint64 `json:"ino,omitempty"`
	Head []byte `json:"head,omitempty"` // the file's first bytes, up to 512
	Mark []byte `json:"mark,omitempty"` // the bytes just before Off, up to 512
}

// Offset is how many bytes have been read so far.
func (r *Reader) Offset() int64 { return r.off }

// Position returns where the Reader stands.
func (r *Reader) Position() Position {
	return Position{Off: r.off, Dev: r.dev, Ino: r.ino, Head: bytes.Clone(r.head), Mark: bytes.Clone(r.mark)}
}

// Resume makes the Reader continue from p, as if it had read the file up to there itself. The next Read checks
// the file against p and starts over when it is no longer the file p describes.
func (r *Reader) Resume(p Position) {
	r.off, r.dev, r.ino, r.head, r.mark = p.Off, p.Dev, p.Ino, bytes.Clone(p.Head), bytes.Clone(p.Mark)
}

// Read calls line for each complete line added since the last call. When the file is shorter than what was read,
// is a different file at the same path, no longer starts with the bytes it started with, or no longer holds the
// bytes it held just before the position, Read calls reset first and reads from the start. A partial last line
// is left for the next call. Read returns the file's size, or -1 when the file cannot be opened.
func (r *Reader) Read(reset func(), line func([]byte)) int64 {
	f, err := os.Open(r.Path)
	if err != nil {
		return -1
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return -1
	}
	var dev, ino uint64
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		dev, ino = uint64(sys.Dev), sys.Ino
	}
	restart := st.Size() < r.off || (r.ino != 0 && (dev != r.dev || ino != r.ino)) ||
		!holds(f, 0, r.head) || !holds(f, r.off-int64(len(r.mark)), r.mark)
	if restart {
		r.off, r.head, r.mark = 0, nil, nil
		if reset != nil {
			reset()
		}
	}
	r.dev, r.ino = dev, ino
	if st.Size() == r.off {
		return st.Size()
	}
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return st.Size()
	}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil {
			break
		}
		if len(r.head) < fingerprint {
			r.head = append(r.head, raw[:min(len(raw), fingerprint-len(r.head))]...)
		}
		r.mark = keepTail(r.mark, raw, fingerprint)
		r.off += int64(len(raw))
		line(raw)
	}
	return st.Size()
}

// holds reports whether the file has want at offset at. Nothing to compare holds.
func holds(f *os.File, at int64, want []byte) bool {
	if len(want) == 0 {
		return true
	}
	if at < 0 {
		return false
	}
	got := make([]byte, len(want))
	n, _ := f.ReadAt(got, at)
	return n == len(got) && bytes.Equal(got, want)
}

// keepTail appends add to buf and keeps the last n bytes.
func keepTail(buf, add []byte, n int) []byte {
	if len(add) >= n {
		return append(buf[:0], add[len(add)-n:]...)
	}
	if extra := len(buf) + len(add) - n; extra > 0 {
		buf = append(buf[:0], buf[extra:]...)
	}
	return append(buf, add...)
}

// Tail returns the complete lines in the last n bytes of a file, oldest first.
func Tail(path string, n int64) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(0, st.Size()-n)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // after a seek the first line is a fragment
	}
	return lines
}
