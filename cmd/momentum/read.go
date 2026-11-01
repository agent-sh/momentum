package main

import (
	"bufio"
	"bytes"
	"context"
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

// A loop run's file tools reach only its own directories; it reads the rest of the machine through these
// commands, which print nothing the secret scrubber has not passed: a record an event names, a file, a service's
// state and log. Each refuses where credentials are kept.

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
	if why := secretPath(path); why != "" {
		fmt.Fprintf(&output, "  (its record is not shown: %s)\n", why)
		return scrubbed(ctx, stdout, output.String())
	}
	// opened without waiting on a writer, and read only when it is a regular file: a reference may now name a pipe or
	// a device, which would hold the run
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		output.WriteString("  (its record is gone)\n")
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

// secretDirs are where the user's credentials are kept, under his home: none of it is shown.
var secretDirs = loop.CredentialDirs

// secretPath says why a path is not shown, or "".
func secretPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err.Error()
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	home := paths.Home()
	for _, d := range secretDirs {
		if within(abs, filepath.Join(home, d)) {
			return "it is where credentials are kept"
		}
	}
	if loop.CredentialName(filepath.Base(abs)) {
		return "its name says it holds a secret"
	}
	return ""
}

// evidenceFile prints a file of the machine, scrubbed: all of it up to 256 KiB, or its last lines. A file where
// credentials are kept, or named as one that holds them, is refused.
func evidenceFile(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence file", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tail := fs.Int("tail", 0, "its last N lines only")
	var file string
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && file == "" {
			file = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if file == "" || fs.NArg() > 0 {
		return usageError{"evidence file takes one file, and --tail N"}
	}
	file = paths.Expand(file)
	if why := secretPath(file); why != "" {
		return usageError{fmt.Sprintf("%s is not shown: %s", file, why)}
	}
	// opened without waiting on a writer, and read only when what it opened is a regular file: the path may name a
	// pipe by the time it is opened
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return usageError{err.Error()}
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return usageError{fmt.Sprintf("%s is not a file", file)}
	}
	text, err := scrubRecords(ctx, f, *tail)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, text)
	return err
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

// scrubRecords scrubs the whole file before selecting any lines. A file too large to scrub whole is refused.
func scrubRecords(ctx context.Context, input io.ReadSeeker, tail int) (string, error) {
	const limit = 64 << 20
	end, err := input.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	if end > limit {
		return "", fmt.Errorf("file too large to scrub whole (over 64 MiB)")
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return "", err
	}
	if len(raw) > limit {
		return "", fmt.Errorf("file too large to scrub whole (over 64 MiB)")
	}
	clean, err := loop.Redact(ctx, strings.ToValidUTF8(string(raw), "?"))
	if err != nil {
		return "", err
	}
	r := bufio.NewReaderSize(strings.NewReader(clean), readMax+1)
	var lines []string
	size := 0
	for {
		record, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull || len(record) > readMax {
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
			}
			record = []byte("(oversized record withheld)\n")
		}
		if err != nil && err != io.EOF {
			return "", err
		}
		if len(record) > 0 {
			if tail <= 0 && size+len(record) > readMax {
				break
			}
			lines = append(lines, string(record))
			size += len(record)
			for len(lines) > 0 && (size > readMax || tail > 0 && len(lines) > tail) {
				size -= len(lines[0])
				lines = lines[1:]
			}
		}
		if err == io.EOF {
			break
		}
	}
	return strings.Join(lines, ""), nil
}

// evidenceSearch matches stored summaries, newest first and scrubbed. evidence show reads the whole record.
func evidenceSearch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "30d", "how far back: a duration or a time")
	until := fs.String("until", "", "up to: a duration ago or a time (default now)")
	kinds := fs.String("kind", "", "comma-separated kinds (owner_text, agent_text, tool_call, tool_result, ...)")
	actor := fs.String("actor", "", "only one actor's: owner, agent, tool, harness")
	session := fs.String("session", "", "one session's")
	limit := fs.Int("limit", 50, "at most this many")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "evidence search [flags] TEXT: stored summaries, newest first. Agent text: first 160 and last 80 bytes; calls and results: first 240 (errors keep 1,000). evidence show reads the whole record.")
		fs.PrintDefaults()
	}
	// Consume each flag and its value before collecting positional arguments.
	var flags, phrases []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			phrases = append(phrases, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			phrases = append(phrases, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") || a == "-h" || a == "--help" {
			continue
		}
		if i+1 >= len(args) {
			return usageError{"missing flag value: " + a}
		}
		i++
		flags = append(flags, args[i])
	}
	if err := fs.Parse(flags); err != nil {
		return usageError{err.Error()}
	}
	if len(phrases) != 1 || len(strings.TrimSpace(phrases[0])) < 3 {
		return usageError{"evidence search takes exactly one phrase of 3 characters or more; flags may come anywhere"}
	}
	text := phrases[0]
	switch *actor {
	case "", "owner", "agent", "tool", "harness", "detector", "unknown":
	default:
		return usageError{"unknown actor: " + *actor}
	}
	from, err := parseWhen(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	now, err := commandNow()
	if err != nil {
		return err
	}
	q := store.Query{Until: now, Since: from, Text: text, Session: *session, Actor: store.Actor(*actor), Source: store.Transcripts, Limit: min(max(*limit, 1), 500)}
	if *until != "" {
		if q.Until, err = parseWhen(*until); err != nil {
			return usageError{err.Error()}
		}
	}
	if os.Getenv("MOMENTUM_NOW") != "" && q.Until.After(now) {
		q.Until = now
	}
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			switch k {
			case "owner_text", "agent_text", "tool_call", "tool_result", "harness_text", "compaction", "unknown":
			default:
				return usageError{"unknown kind: " + k}
			}
			q.Kinds = append(q.Kinds, k)
		}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	evs, err := st.Events(ctx, q)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "%s ev:%s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.ID(), e.Session, e.Actor, e.Kind,
			plain.Line(e.Tool), plain.Line(e.Summary))
	}
	if len(evs) == q.Limit {
		fmt.Fprintf(&b, "(the first %d; narrow it with --since, --kind, --actor or --session)\n", q.Limit)
	}
	return scrubbed(ctx, stdout, b.String())
}
