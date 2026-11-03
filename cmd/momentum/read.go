package main

import (
	"bufio"
	"bytes"
	"context"
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
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// Host evidence commands prepare scrubbed records for the loop inputs.

// readMax bounds a complete record and the retained output. Oversized records are withheld.
const readMax = 256 << 10
const showMost = 10

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
// scrubbed before --bytes selects its prefix. No evaluation clock is used.
func evidenceShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cut := fs.Int("bytes", 0, "each record's first N bytes, once it is scrubbed whole (default: all of it)")
	var flags, ids []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // the end of the flags: every word after it is an id
			ids = append(ids, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			ids = append(ids, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && a != "-h" && a != "--help" && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 { // a flag's value that is no value: nothing given is left unread
		return usageError{fmt.Sprintf("evidence show: %q is no flag's value: give each event's id, flags as --bytes N", fs.Arg(0))}
	}
	if len(ids) == 0 || len(ids) > showMost || *cut < 0 {
		return usageError{fmt.Sprintf("evidence show takes 1 to %d events, each its id or ev:ID, and --bytes N", showMost)}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	var output strings.Builder
	shown := 0
	for i, raw := range ids {
		text, err := showEvent(ctx, st, strings.TrimPrefix(raw, "ev:"), *cut)
		var ue usageError
		switch {
		case err == nil:
			shown++
		case errors.As(err, &ue) && len(ids) > 1:
			text = fmt.Sprintf("%s: %s\n", plain.Line(raw), ue.msg)
		default:
			return err
		}
		if i > 0 {
			output.WriteString("\n")
		}
		output.WriteString(text)
	}
	if err := scrubbed(ctx, stdout, output.String()); err != nil {
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
	for _, args := range [][]string{
		{"systemctl", "--user", "status", "--no-pager", "--lines=0", "--", unit},
		{"journalctl", "--user", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n), "-u", unit},
	} {
		cmd := exec.CommandContext(cctx, args[0], args[1:]...)
		cmd.Stdout, cmd.Stderr = capture, capture
		_ = cmd.Run()
	}
	if output.Len() > readMax {
		return usageError{"unit output exceeds the record limit; narrow the command"}
	}
	return scrubbed(ctx, stdout, strings.ToValidUTF8(output.String(), "?"))
}

func showEvent(ctx context.Context, st *store.Store, id string, n int) (string, error) {
	e, ok, err := st.EventByID(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", usageError{fmt.Sprintf("the store holds no event %s", id)}
	}
	var output strings.Builder
	fmt.Fprintf(&output, "%s ev:%s %s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.ID(), e.Source, e.Actor, e.Kind, e.Session,
		plain.Line(e.Tool), plain.Line(e.Summary))
	path, off, ok := placeOf(e.Ref)
	if !ok {
		return loop.Redact(ctx, output.String())
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
		return loop.Redact(ctx, output.String())
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		output.WriteString("  (its record is not a regular file)\n")
		return loop.Redact(ctx, output.String())
	}
	if off < 0 {
		return "", usageError{"negative record offset"}
	}
	if off > 0 {
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], off-1); err != nil || prev[0] != '\n' {
			return "", usageError{"offset is not a record boundary"}
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	line, err := bufio.NewReaderSize(f, readMax+1).ReadSlice('\n')
	if err == bufio.ErrBufferFull || len(line) > readMax {
		output.WriteString("  (oversized record withheld)\n")
	} else if err != nil && err != io.EOF {
		return "", err
	} else {
		clean, err := loop.Redact(ctx, string(line))
		if err != nil {
			return "", err
		}
		if n > 0 && len(clean) > n {
			whole := len(clean)
			clean = clean[:n]
			for !utf8.ValidString(clean) && len(clean) > 0 {
				clean = clean[:len(clean)-1]
			}
			clean += fmt.Sprintf("\n  (its first %d of %d bytes)\n", len(clean), whole)
		}
		output.WriteString(clean)
	}
	return loop.Redact(ctx, output.String())
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
