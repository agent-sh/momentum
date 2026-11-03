package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

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

// evidenceShow accepts up to ten live events. Each full referenced record is
// scrubbed before --bytes selects its prefix. An as-of child withholds later
// records and every Hermes row, whose earlier version cannot be reconstructed.
// The loop's records pass adds --separate: the events are then separated by the record separator (U+001E), which no
// scrubbed text holds, and an event that cannot be shown is an empty one, so one child serves up to ten records.
func evidenceShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if _, err := evidenceEnd(); err != nil {
		return err
	}
	separate := false
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		if args[i] == "--separate" {
			separate = true
			args = append(args[:i:i], args[i+1:]...)
			i--
		}
	}
	ids, cut, err := evidenceview.ParseShow(args, stderr)
	if err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	var texts []string
	shown := 0
	for _, raw := range ids {
		text, err := showEvent(ctx, st, strings.TrimPrefix(raw, "ev:"))
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
		texts = append(texts, text.header, text.record)
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

var errWritableRecord = errors.New("record lies where a run can write")

// A host event's reference does not authenticate a run-written file.
// Refuse both its spelling and its resolved target before opening it.
func openRecord(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = abs
	real := resolved(path)
	for _, name := range []string{"runs", "work", "cache", "exports", "tmp"} {
		root := filepath.Join(loopDir(), name)
		if within(path, root) || within(real, resolved(root)) {
			return nil, errWritableRecord
		}
	}
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
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

func showEvent(ctx context.Context, st *store.Store, id string) (eventText, error) {
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
	// opened without waiting on a writer, and read only when it is a regular file: a reference may now name a pipe or
	// a device, which would hold the run
	f, err := openRecord(path)
	if err != nil {
		if errors.Is(err, errWritableRecord) {
			output.WriteString("  (record withheld: it lies where a run can write)\n")
		} else {
			output.WriteString("  (its record is gone)\n")
		}
		return eventText{header: output.String()}, nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
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
	if off > 0 {
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], off-1); err != nil || prev[0] != '\n' {
			return eventText{}, usageError{"offset is not a record boundary"}
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return eventText{}, err
	}
	line, err := bufio.NewReaderSize(f, readMax+1).ReadSlice('\n')
	if err == bufio.ErrBufferFull || len(line) > readMax {
		output.WriteString("  (oversized record withheld)\n")
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
