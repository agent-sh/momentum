// Package jsonl reads append-only JSONL files incrementally. A Reader hands over only the complete lines added
// since its previous read, and starts over when the file shrinks, is replaced, or is rewritten in place, so a
// tracker built on it never mixes the state of two files.
//
// The file identity follows the OpenTelemetry filelog receiver, as momentum's log sources do: the inode plus a
// fingerprint of the file's first bytes.
package jsonl

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"syscall"
)

// fingerprint is how many leading bytes identify a file's content.
const fingerprint = 512

// Reader remembers how far into one file it has read.
type Reader struct {
	Path string
	off  int64
	dev  uint64
	ino  uint64
	head []byte // the file's first bytes, up to fingerprint, as last read
}

// Offset is how many bytes have been read so far.
func (r *Reader) Offset() int64 { return r.off }

// Read calls line for each complete line added since the last call. When the file is shorter than what was read,
// is a different file at the same path, or no longer starts with the bytes it started with, Read calls reset
// first and reads from the start. A partial last line is left for the next call. Read returns the file's size,
// or -1 when the file cannot be opened.
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
	restart := st.Size() < r.off || (r.ino != 0 && (dev != r.dev || ino != r.ino))
	if !restart && len(r.head) > 0 {
		now := make([]byte, len(r.head))
		if n, _ := f.ReadAt(now, 0); n < len(now) || !bytes.Equal(now, r.head) {
			restart = true
		}
	}
	if restart {
		r.off, r.head = 0, nil
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
		r.off += int64(len(raw))
		line(raw)
	}
	return st.Size()
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
