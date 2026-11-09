package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	tacit "github.com/agent-sh/tacit/client/pkg/evidence"
	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/evidenceview"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// Host evidence commands prepare scrubbed records for the loop inputs.

// readMax bounds a complete record and the retained output. Oversized records are withheld.
const readMax = 256 << 10

// scrubbed writes a text through the secret scrubber, or refuses to write it.
func scrubbed(ctx context.Context, w io.Writer, text string) error {
	out, err := loop.Redact(ctx, text)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, out)
	return err
}

// evidenceShow accepts up to ten live events. Each full referenced record (a transcript's line, read from its
// compressed copy once the harness compressed it, or a Hermes row) is scrubbed before --bytes selects its prefix.
// An as-of child withholds later records and every Hermes row, whose earlier version cannot be reconstructed.
// The loop's records pass adds --separate: the events are then separated by the record separator (U+001E), which no
// scrubbed text holds, and an event that cannot be shown is an empty one, so one child serves up to ten records.
// The events are read in the order of their records' places and printed in the order given, so the events of one
// compressed copy are read from one stream (copyReader).
func evidenceShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if _, err := evidenceEnd(); err != nil {
		return err
	}
	ids, cut, separate, err := evidenceview.ParseHostShow(args, stderr)
	if err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	var copies copyReader
	defer copies.close()
	texts := make([]string, 2*len(ids))
	shown := 0
	for _, i := range byPlace(ctx, st, ids) {
		raw := ids[i]
		text, err := showEvent(ctx, st, strings.TrimPrefix(raw, "ev:"), &copies)
		var ue usageError
		switch {
		case err == nil:
			shown++
		case errors.As(err, &ue) && len(ids) > 1 && separate:
			text = eventText{} // an empty event: the records pass withholds it
		case errors.As(err, &ue) && len(ids) > 1:
			text.header = fmt.Sprintf("%s: %s\n", plain.Line(raw), ue.msg)
		default:
			return err
		}
		texts[2*i], texts[2*i+1] = text.header, text.record
	}
	// All full records and headers fit in one bounded scrub batch (ten records).
	clean, err := loop.RedactAll(ctx, texts)
	if err != nil {
		return err
	}
	var output strings.Builder
	for i := range ids {
		switch {
		case i > 0 && separate:
			output.WriteString("\x1e")
		case i > 0:
			output.WriteString("\n")
		}
		output.WriteString(clean[2*i])
		output.WriteString(evidenceview.RecordPrefix(clean[2*i+1], cut))
	}
	if _, err := io.WriteString(stdout, output.String()); err != nil {
		return err
	}
	if shown == 0 {
		return usageError{"none of them could be shown"}
	}
	return nil
}

// byPlace is the order to read the events of ids in: by the file their references name and, in one file, by offset.
func byPlace(ctx context.Context, st *store.Store, ids []string) []int {
	type place struct {
		path string
		off  int64
	}
	at := make([]place, len(ids))
	order := make([]int, len(ids))
	for i, raw := range ids {
		order[i] = i
		if e, ok, err := st.EventByID(ctx, strings.TrimPrefix(raw, "ev:")); err == nil && ok {
			at[i].path, at[i].off, _ = placeOf(e.Ref)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Or(strings.Compare(at[a].path, at[b].path), cmp.Compare(at[a].off, at[b].off))
	})
	return order
}

// copyReader reads the compressed copies of one show invocation. It decompresses a copy from the file openRecord
// opened and checked, never by its name again, and keeps that stream for the next event: the events of one copy,
// read in ascending offset order, pass over its bytes once instead of each from its start.
type copyReader struct {
	f   *os.File
	fi  os.FileInfo
	dec *zstd.Decoder
	in  *countingReader
	br  *bufio.Reader
}

var errPastEnd = errors.New("offset is past the copy's end")

// at is the copy's JSONL bytes from start, read from f, a regular file openRecord opened (fi). It takes f: it keeps
// it for a new stream, or closes it when the stream it kept reads the same file and has not passed start. A stream
// that met an error or its end is not kept. An offset past the copy's end is errPastEnd.
func (c *copyReader) at(f *os.File, fi os.FileInfo, start int64) (*bufio.Reader, error) {
	if c.dec != nil && c.in.err == nil && os.SameFile(c.fi, fi) && c.pos() <= start {
		f.Close()
	} else {
		c.close()
		// as tacit's reader decodes a copy: one goroutine, little memory and at most a 128 MiB window
		dec, err := zstd.NewReader(bufio.NewReaderSize(f, 256<<10),
			zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(128<<20))
		if err != nil {
			f.Close()
			return nil, err
		}
		in := &countingReader{r: dec}
		*c = copyReader{f: f, fi: fi, dec: dec, in: in, br: bufio.NewReaderSize(in, readMax+1)}
	}
	if _, err := c.br.Discard(int(start - c.pos())); err == io.EOF {
		return nil, errPastEnd
	} else if err != nil {
		return nil, err
	}
	return c.br, nil
}

// pos is how many of the copy's JSONL bytes the stream has passed.
func (c *copyReader) pos() int64 { return c.in.n - int64(c.br.Buffered()) }

func (c *copyReader) close() {
	if c.dec != nil {
		c.dec.Close()
		c.f.Close()
	}
	*c = copyReader{}
}

// countingReader reads r, counting its bytes and keeping its first error.
type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.err == nil {
		c.err = err
	}
	return n, err
}

var errWritableRecord = errors.New("record lies where a run can write")

// openRecord opens the file that holds a host event's record: the file its reference names or, once the harness
// compressed it (a Codex rollout), its compressed copy (evidence.Resolve), whose name it also returns ("" for the
// named file). A reference does not authenticate a run-written file: the spelling and the resolved target of the
// named file and of the copy are refused inside a run-writable root before either is opened.
func openRecord(path string) (*os.File, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	if runWritable(abs) {
		return nil, "", errWritableRecord
	}
	holder, packed := abs, ""
	if zst := tacit.Resolve(abs); zst != abs {
		if runWritable(zst) {
			return nil, "", errWritableRecord
		}
		holder, packed = zst, zst
	}
	f, err := os.OpenFile(holder, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	return f, packed, err
}

// runWritable reports a path whose spelling or resolved target lies in a root a run can write.
func runWritable(path string) bool {
	real := resolved(path)
	for _, name := range []string{"runs", "work", "cache", "exports", "tmp"} {
		root := filepath.Join(loopDir(), name)
		if within(path, root) || within(real, resolved(root)) {
			return true
		}
	}
	return false
}

// placeOf splits a reference (path@offset, with #index) into its file and the offset of its record.
func placeOf(ref string) (string, int64, bool) {
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

var unitRe = regexp.MustCompile(`^[A-Za-z0-9@._:-]{1,128}$`)

// evidenceUnit prints a user service's state and the end of its log, scrubbed: systemd's status, and journald's
// last lines of the unit (200, or --lines N up to 2000).
func evidenceUnit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence unit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lines := fs.Int("lines", 200, "how many of its log's last lines")
	cut := fs.Int("bytes", 0, "the last N bytes of the scrubbed output, at a line start (default: all of it)")
	var unit string
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && unit == "" {
			unit = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if !unitRe.MatchString(unit) || fs.NArg() > 0 {
		return usageError{"evidence unit takes one user unit's name, and --lines N"}
	}
	n := min(max(*lines, 1), 2000)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var output bytes.Buffer
	capture := &capped{w: &output, n: readMax + 1}
	calls := [][]string{
		{"systemctl", "--user", "status", "--no-pager", "--lines=0", "--", unit},
		{"journalctl", "--user", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n), "-u", unit},
	}
	end, err := evidenceEnd()
	if err != nil {
		return err
	}
	if !end.IsZero() {
		// Journald timestamps have microsecond precision and --until includes
		// its bound. Use the last microsecond strictly before the end.
		before := end.Add(-time.Nanosecond)
		stamp := fmt.Sprintf("@%d.%06d", before.Unix(), before.Nanosecond()/1000)
		calls = [][]string{{"journalctl", "--user", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n), "--until", stamp, "-u", unit}}
	}
	for _, args := range calls {
		cmd := exec.CommandContext(cctx, args[0], args[1:]...)
		cmd.Stdout, cmd.Stderr = capture, capture
		_ = cmd.Run()
	}
	if output.Len() > readMax {
		return usageError{"unit output exceeds the record limit; narrow the command"}
	}
	cleaned, err := loop.RedactAll(ctx, []string{strings.ToValidUTF8(output.String(), "?")})
	if err != nil {
		return err
	}
	clean := cleaned[0]
	if *cut > 0 && len(clean) > *cut {
		// the end of the log is what a reader wants: drop from the front, at a line start
		whole := len(clean)
		tail := clean[whole-*cut:]
		if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
			tail = tail[i+1:]
		}
		clean = fmt.Sprintf("(the last %d of %d bytes)\n", len(tail), whole) + tail
	}
	_, err = io.WriteString(stdout, clean)
	return err
}

type eventText struct{ header, record string }

func showEvent(ctx context.Context, st *store.Store, id string, copies *copyReader) (eventText, error) {
	end, err := evidenceEnd()
	if err != nil {
		return eventText{}, err
	}
	e, ok, err := st.EventByID(ctx, id)
	if err != nil {
		return eventText{}, err
	}
	if !ok {
		return eventText{}, usageError{fmt.Sprintf("the store holds no event %s", id)}
	}
	var output strings.Builder
	output.WriteString(evidenceview.EventHeader(e))
	if !end.IsZero() && !e.TS.Before(end) {
		return eventText{header: "  (record withheld: event is at or after the evaluation end)\n"}, nil
	}
	if !end.IsZero() && e.Harness == "hermes" {
		output.WriteString("  (record withheld: historical Hermes message rows are unavailable)\n")
		return eventText{header: output.String()}, nil
	}
	path, off, ok := placeOf(e.Ref)
	if !ok {
		return eventText{header: output.String()}, nil
	}
	if e.Source == store.Transcripts && e.Harness == "hermes" {
		// tacit's reader keeps a Hermes event's place as its store and its row's id, not as bytes of a file
		record, note := hermesRow(ctx, e, path, off)
		output.WriteString(note)
		return eventText{header: output.String(), record: record}, nil
	}
	// opened without waiting on a writer, and read only when it is a regular file: a reference may now name a pipe or
	// a device, which would hold the run
	f, packed, err := openRecord(path)
	if err != nil {
		output.WriteString(unopened(err))
		return eventText{header: output.String()}, nil
	}
	defer func() {
		if f != nil { // copies takes a compressed copy's file
			f.Close()
		}
	}()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		output.WriteString("  (its record is not a regular file)\n")
		return eventText{header: output.String()}, nil
	}
	var magic [16]byte
	if count, _ := f.ReadAt(magic[:], 0); count == len(magic) && string(magic[:]) == "SQLite format 3\x00" {
		output.WriteString("  (record withheld: database rows cannot be read as transcript bytes)\n")
		return eventText{header: output.String()}, nil
	}
	if off < 0 {
		return eventText{}, usageError{"negative record offset"}
	}
	var br *bufio.Reader
	if packed == "" {
		if off > 0 {
			var prev [1]byte
			if _, err := f.ReadAt(prev[:], off-1); err != nil || prev[0] != '\n' {
				return eventText{}, usageError{"offset is not a record boundary"}
			}
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return eventText{}, err
		}
		br = bufio.NewReaderSize(f, readMax+1)
	} else {
		// The harness compressed the transcript after it was read: the copy is decompressed from the file checked
		// above, and its JSONL bytes are passed over up to the byte before the record, which must end the record
		// before it.
		r, err := copies.at(f, fi, max(off-1, 0))
		f = nil
		if errors.Is(err, errPastEnd) {
			return eventText{}, usageError{"offset is not a record boundary"}
		}
		if err != nil {
			output.WriteString(packedUnread)
			return eventText{header: output.String()}, nil
		}
		br = r
		if off > 0 {
			if prev, err := br.ReadByte(); err != nil || prev != '\n' {
				return eventText{}, usageError{"offset is not a record boundary"}
			}
		}
	}
	line, err := br.ReadSlice('\n')
	if err == bufio.ErrBufferFull || len(line) > readMax {
		output.WriteString("  (oversized record withheld)\n")
	} else if err != nil && err != io.EOF && packed != "" {
		output.WriteString(packedUnread)
	} else if err != nil && err != io.EOF {
		return eventText{}, err
	} else {
		if e.Source == "mistakes" {
			sum := sha256.Sum256(line)
			if hex.EncodeToString(sum[:16]) != e.SourceKey {
				output.WriteString("  (record withheld: the line was rewritten since it was read)\n")
				return eventText{header: output.String()}, nil
			}
		}
		if !end.IsZero() {
			var dated struct {
				Timestamp string `json:"timestamp"`
			}
			_ = json.Unmarshal(line, &dated)
			at, stampErr := time.Parse(time.RFC3339Nano, dated.Timestamp)
			if stampErr == nil && !at.Before(end) {
				output.WriteString("  (record withheld: timestamp is at or after the evaluation end)\n")
				return eventText{header: output.String()}, nil
			}
			if stampErr != nil && (e.Source == store.Sessions || e.Source == store.Supervise) {
				output.WriteString("  (record withheld: no valid timestamp at the scan's place)\n")
				return eventText{header: output.String()}, nil
			}
		}
		return eventText{header: output.String(), record: string(line)}, nil
	}
	return eventText{header: output.String()}, nil
}

// packedUnread is the note for a compressed copy that holds a transcript but cannot be read to its record, and
// hermesUnread for a Hermes store that cannot be opened or read as one.
const (
	packedUnread = "  (record withheld: its compressed copy cannot be read)\n"
	hermesUnread = "  (record withheld: its Hermes store cannot be read)\n"
)

// unopened is the note for a record openRecord refused or could not open.
func unopened(err error) string {
	if errors.Is(err, errWritableRecord) {
		return "  (record withheld: it lies where a run can write)\n"
	}
	return "  (its record is gone)\n"
}

// hermesRow is a Hermes event's record: its row in the Hermes store whose path its reference names, by the row's
// id, read with tacit's Hermes reader in the session that holds the row (a delegation's own), as the request reader
// reads it. It returns the row as one JSON line, or a note on why there is none. The store is opened read-only,
// only when it is a regular file outside the roots a run can write, and a row over the record bound is withheld.
func hermesRow(ctx context.Context, e store.Event, path string, msg int64) (string, string) {
	f, _, err := openRecord(path)
	if err != nil {
		return "", unopened(err)
	}
	fi, err := f.Stat()
	f.Close()
	if err != nil || !fi.Mode().IsRegular() {
		return "", "  (its record is not a regular file)\n"
	}
	if msg <= 0 {
		return "", "  (its record is gone)\n"
	}
	db, err := tacit.OpenHermes(path)
	if err != nil {
		return "", hermesUnread
	}
	defer db.Close()
	// SQLite opens the store by name: a name that no longer holds the file checked above is refused. A swap there
	// and back between the check and SQLite's open would pass, but a run cannot write where the store lies
	// (openRecord), so it cannot make one.
	if again, err := os.Lstat(db.Path); err != nil || !os.SameFile(again, fi) {
		return "", "  (its record is gone)\n"
	}
	sid := e.Session
	if e.Agent != "" && e.Agent != e.Session {
		sid = e.Agent
	}
	rows, err := db.Messages(ctx, sid, msg-1, 1) // the row itself: Messages reads at least one, here no more
	if err != nil {
		return "", hermesUnread
	}
	if len(rows) == 0 || rows[0].ID != msg {
		return "", "  (its record is gone)\n"
	}
	m := rows[0]
	row := hermesLine{ID: m.ID, Timestamp: time.UnixMicro(int64(math.Round(m.Timestamp * 1e6))).UTC().Format(time.RFC3339Nano),
		Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolName: m.ToolName, Finish: m.Finish,
		DisplayKind: m.DisplayKind, Summary: m.Summary}
	if m.ToolCalls != "" {
		row.ToolCalls = json.RawMessage(m.ToolCalls)
		if !json.Valid(row.ToolCalls) {
			row.ToolCalls, _ = json.Marshal(m.ToolCalls)
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(row); err != nil || b.Len() > readMax {
		return "", "  (oversized record withheld)\n"
	}
	return b.String(), ""
}

// hermesLine is a Hermes row as evidence show prints it: one JSON line of its columns, its time in RFC 3339 as a
// transcript line's, and its tool calls as the JSON Hermes stored.
type hermesLine struct {
	ID          int64           `json:"id"`
	Timestamp   string          `json:"timestamp"`
	Role        string          `json:"role"`
	Content     string          `json:"content,omitempty"`
	ToolCalls   json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	ToolName    string          `json:"tool_name,omitempty"`
	Finish      string          `json:"finish_reason,omitempty"`
	DisplayKind string          `json:"display_kind,omitempty"`
	Summary     bool            `json:"compressed_summary,omitempty"`
}

func evidenceEnd() (time.Time, error) {
	if stamp := os.Getenv("MOMENTUM_NOW"); stamp != "" {
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || at.IsZero() {
			return time.Time{}, usageError{"MOMENTUM_NOW must be a nonzero RFC3339 time"}
		}
		return at, nil
	}
	return time.Time{}, nil
}

// resolved is a path made absolute with its links followed, as far as it exists.
func resolved(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// capped writes at most n bytes, and takes the rest without writing it.
type capped struct {
	w io.Writer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n <= 0 {
		return len(p), nil
	}
	k := min(len(p), c.n)
	c.n -= k
	if _, err := c.w.Write(p[:k]); err != nil {
		return 0, err
	}
	return len(p), nil
}
