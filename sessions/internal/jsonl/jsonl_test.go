package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sink struct {
	lines  []string
	resets int
}

func (s *sink) read(r *Reader) int64 {
	return r.Read(func() { s.resets++; s.lines = nil }, func(b []byte) { s.lines = append(s.lines, strings.TrimSpace(string(b))) })
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestReadHandsOverOnlyNewCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "{\"a\":1}\n{\"a\":2")
	r, s := &Reader{Path: path}, &sink{}
	if size := s.read(r); size != 14 || len(s.lines) != 1 {
		t.Fatalf("first read: size %d lines %v", size, s.lines)
	}
	appendTo(t, path, "}\n{\"a\":3}\n")
	s.read(r)
	if strings.Join(s.lines, " ") != `{"a":1} {"a":2} {"a":3}` || s.resets != 0 {
		t.Fatalf("lines %v resets %d", s.lines, s.resets)
	}
	s.read(r)
	if len(s.lines) != 3 {
		t.Fatalf("a read with nothing new handed over %v", s.lines)
	}
}

func TestReadStartsOverWhenTheFileShrinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "{\"a\":1}\n{\"a\":2}\n")
	r, s := &Reader{Path: path}, &sink{}
	s.read(r)
	write(t, path, "{\"b\":1}\n")
	s.read(r)
	if s.resets != 1 || strings.Join(s.lines, " ") != `{"b":1}` {
		t.Fatalf("resets %d lines %v", s.resets, s.lines)
	}
}

func TestReadStartsOverWhenTheFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	head := `{"a":"` + strings.Repeat("x", 600) + `"}` + "\n" // longer than the fingerprint, so only the inode tells
	write(t, path, head+"{\"x\":1}\n")
	r, s := &Reader{Path: path}, &sink{}
	s.read(r)
	next := filepath.Join(dir, "next")
	write(t, next, head+"{\"y\":2}\n{\"z\":3}\n") // the same start, then different, and longer
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	s.read(r)
	if s.resets != 1 || len(s.lines) != 3 || s.lines[1] != `{"y":2}` {
		t.Fatalf("resets %d lines %d %v", s.resets, len(s.lines), s.lines[1:])
	}
}

func TestReadStartsOverWhenTheFileIsRewrittenInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "{\"a\":1}\n")
	r, s := &Reader{Path: path}, &sink{}
	s.read(r)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600) // same inode, new content, longer
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"b\":1}\n{\"b\":2}\n")
	f.Close()
	s.read(r)
	if s.resets != 1 || strings.Join(s.lines, " ") != `{"b":1} {"b":2}` {
		t.Fatalf("resets %d lines %v", s.resets, s.lines)
	}
}

func TestReadOfAMissingFileIsMinusOne(t *testing.T) {
	r, s := &Reader{Path: filepath.Join(t.TempDir(), "none")}, &sink{}
	if size := s.read(r); size != -1 {
		t.Fatalf("size %d", size)
	}
}

func TestTailDropsTheFragmentBeforeTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "first line\nsecond\nthird\n")
	got := Tail(path, 15) // starts inside "first line"
	if len(got) != 3 || string(got[0]) != "second" || string(got[1]) != "third" || string(got[2]) != "" {
		t.Fatalf("tail %q", got)
	}
	if all := Tail(path, 1000); string(all[0]) != "first line" {
		t.Fatalf("a window over the whole file keeps the first line: %q", all)
	}
}
