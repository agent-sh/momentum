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

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// A loop run's sandbox holds only its own directories; it reads the rest of the machine through these commands,
// which its door runs outside the sandbox and which print nothing the secret scrubber has not passed: a record an
// event names, and a service's state and log.

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

// evidenceShow prints an event of the store and, where its reference names a record of a transcript or a log
// (path@offset), that record as the file holds it, scrubbed: what the event's summary cut. Only an event the store
// holds is shown, so a store made as of a moment shows nothing after it.
func evidenceShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageError{"evidence show takes one event: its id, or ev:ID"}
	}
	id := strings.TrimPrefix(args[0], "ev:")
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	e, ok, err := st.EventByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return usageError{fmt.Sprintf("the store holds no event %s", id)}
	}
	var output strings.Builder
	fmt.Fprintf(&output, "%s %s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.Source, e.Actor, e.Kind, e.Session,
		plain.Line(e.Tool), plain.Line(e.Summary))
	path, off, ok := placeOf(e.Ref)
	if !ok {
		return scrubbed(ctx, stdout, output.String())
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
		return scrubbed(ctx, stdout, output.String())
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		output.WriteString("  (its record is not a regular file)\n")
		return scrubbed(ctx, stdout, output.String())
	}
	if off < 0 {
		return usageError{"negative record offset"}
	}
	if off > 0 {
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], off-1); err != nil || prev[0] != '\n' {
			return usageError{"offset is not a record boundary"}
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	line, err := bufio.NewReaderSize(f, readMax+1).ReadSlice('\n')
	if err == bufio.ErrBufferFull || len(line) > readMax {
		output.WriteString("  (oversized record withheld)\n")
	} else if err != nil && err != io.EOF {
		return err
	} else {
		output.Write(line)
	}
	return scrubbed(ctx, stdout, output.String())
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
	runDir := os.Getenv("MOMENTUM_LOOP_DIR")
	if runDir != "" && (within(path, runDir) || within(real, resolved(runDir))) {
		return nil, errWritableRecord
	}
	var roots []string
	if os.Getenv("MOMENTUM_LOOP_RUN") != "" {
		roots = append(roots, filepath.SplitList(os.Getenv("MOMENTUM_LOOP_READ"))...)
	}
	for _, dir := range roots {
		if dir != "" && within(path, dir) {
			name, err := filepath.Rel(dir, path)
			if err != nil {
				return nil, err
			}
			return loop.OpenFile(dir, name)
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
