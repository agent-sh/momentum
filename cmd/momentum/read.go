package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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

// showMost is how many events one evidence show prints.
const showMost = 10

// evidenceShow prints events of the store and, where an event's reference names a record of a transcript or a log
// (path@offset), that record as the file holds it, or a Hermes event's message row, each scrubbed whole and then cut
// to --bytes. A scan place names the next record, not the event. With MOMENTUM_NOW set, a record dated at or after
// that end is withheld, and so is an undated one a scan place names.
func evidenceShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cut := fs.Int("bytes", 0, "each record's first N bytes, once it is scrubbed whole (default: all of it)")
	var flags, ids []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
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
		return usageError{"none of them is an event of the store"}
	}
	return nil
}

// noEvent says why an id names no event, and what does.
func noEvent(id string) string {
	switch {
	case strings.Contains(id, "@"):
		return fmt.Sprintf("%s is a record's place, not an event's id: evidence events and evidence search print each event's ev:ID", id)
	case uuidLike.MatchString(id):
		return fmt.Sprintf("%s is a session's id, not an event's: evidence events --session %s reads its events", id, id)
	}
	return fmt.Sprintf("the store holds no event %s (an event's id is the 24 hex characters evidence events and evidence search print after ev:)", id)
}

var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-`)

// showEvent is one event as evidence show prints it, with its record, scrubbed: the record whole first, then its
// first n bytes when n is set.
func showEvent(ctx context.Context, st *store.Store, id string, n int) (string, error) {
	e, ok, err := st.EventByID(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", usageError{noEvent(id)}
	}
	head := fmt.Sprintf("%s ev:%s %s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.ID(), e.Source, e.Actor, e.Kind,
		e.Session, plain.Line(e.Tool), plain.Line(e.Summary))
	record, note, err := eventRecord(ctx, e)
	if err != nil {
		return "", err
	}
	text, err := loop.Redact(ctx, head+note)
	if err != nil || record == "" {
		return text, err
	}
	clean, err := loop.Redact(ctx, record)
	if err != nil {
		return "", err
	}
	if n > 0 && len(clean) > n {
		whole := len(clean)
		clean = clean[:n]
		for !utf8.ValidString(clean) {
			clean = clean[:len(clean)-1]
		}
		clean += fmt.Sprintf("\n  (its first %d of %d bytes)\n", len(clean), whole)
	}
	return text + clean, nil
}

// eventRecord reads the record an event's reference names: the record, or a note that says why there is none.
func eventRecord(ctx context.Context, e store.Event) (record, note string, err error) {
	path, off, ok := placeOf(e.Ref)
	if !ok {
		return "", "", nil
	}
	if why := secretPath(path); why != "" {
		return "", fmt.Sprintf("  (its record is not shown: %s)\n", why), nil
	}
	// opened without waiting on a writer, and read only when it is a regular file: a reference may now name a pipe or
	// a device, which would hold the run
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "  (its record is gone)\n", nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", "  (its record is not a regular file)\n", nil
	}
	if off < 0 {
		return "", "", usageError{"negative record offset"}
	}
	var magic [16]byte
	if n, _ := f.ReadAt(magic[:], 0); n == len(magic) && string(magic[:]) == "SQLite format 3\x00" {
		// a Hermes event's reference names its message row: the offset is the row's id
		return hermesRow(ctx, path, off, e)
	}
	if off > 0 {
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], off-1); err != nil || prev[0] != '\n' {
			return "", "", usageError{"offset is not a record boundary"}
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", "", err
	}
	line, err := bufio.NewReaderSize(f, readMax+1).ReadSlice('\n')
	switch {
	case err == bufio.ErrBufferFull || len(line) > readMax:
		return "", "  (oversized record withheld)\n", nil
	case err != nil && err != io.EOF:
		return "", "", err
	case !ownRecord(e, line):
		// a log read again from its start after a rotation gives its rows new places: the old place names another row
		return "", "  (its record is gone: the line at its place is another's, the log was rewritten since)\n", nil
	}
	if os.Getenv("MOMENTUM_NOW") == "" {
		return string(line), "", nil
	}
	end, err := commandNow()
	if err != nil {
		return "", "", err
	}
	var dated struct {
		Timestamp string `json:"timestamp"`
	}
	_ = json.Unmarshal(line, &dated)
	at, err := time.Parse(time.RFC3339Nano, dated.Timestamp)
	switch {
	case err == nil && !at.IsZero() && !at.Before(end):
		return "", "  (record withheld: timestamp is at or after the evaluation end)\n", nil
	case err == nil && !at.IsZero(), !scanPlace(e.Source):
		// an event's own record is as old as the event, which the snapshot bounds, whatever its format dates it by
		return string(line), "", nil
	}
	return "", "  (record withheld: no valid timestamp)\n", nil
}

// hermesRow reads a Hermes message row by its id, read-only, as one JSON record.
func hermesRow(ctx context.Context, path string, id int64, e store.Event) (string, string, error) {
	if scanPlace(e.Source) {
		return "", "  (a scan place in a Hermes store names no record)\n", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var row struct {
		ID        int64   `json:"id"`
		Session   string  `json:"session_id"`
		Role      string  `json:"role"`
		Content   string  `json:"content"`
		ToolCalls string  `json:"tool_calls,omitempty"`
		ToolName  string  `json:"tool_name,omitempty"`
		Timestamp float64 `json:"timestamp"`
	}
	// an older Hermes store has no tool columns
	has := map[string]bool{}
	if names, err := db.QueryContext(cctx, `SELECT name FROM pragma_table_info('messages')`); err == nil {
		for names.Next() {
			var name string
			if names.Scan(&name) == nil {
				has[name] = true
			}
		}
		names.Close()
	}
	column := func(name string) string {
		if has[name] {
			return "coalesce(" + name + ", '')"
		}
		return "''"
	}
	err = db.QueryRowContext(cctx, `SELECT id, session_id, role, coalesce(content, ''), `+column("tool_calls")+`, `+column("tool_name")+`, timestamp
	  FROM messages WHERE id = ?`, id).Scan(&row.ID, &row.Session, &row.Role, &row.Content, &row.ToolCalls, &row.ToolName, &row.Timestamp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "  (its record is gone)\n", nil
	case err != nil:
		return "", "  (its record could not be read: " + plain.Line(err.Error()) + ")\n", nil
	case len(row.Content)+len(row.ToolCalls) > readMax:
		return "", "  (oversized record withheld)\n", nil
	}
	if os.Getenv("MOMENTUM_NOW") != "" {
		end, err := commandNow()
		if err != nil {
			return "", "", err
		}
		if at := time.UnixMilli(int64(row.Timestamp * 1000)); !at.Before(end) {
			return "", "  (record withheld: timestamp is at or after the evaluation end)\n", nil
		}
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return "", "", err
	}
	return string(raw) + "\n", "", nil
}

// ownRecord says whether a line is the event's own record where the event says which line that is: a mistakes
// row's key is the first 16 bytes of the SHA-256 of its line (source.Mistakes). Other sources' references name an
// append-only file's place.
func ownRecord(e store.Event, line []byte) bool {
	if e.Source != "mistakes" {
		return true
	}
	h := sha256.Sum256(line)
	return hex.EncodeToString(h[:16]) == e.SourceKey
}

// scanPlace says whether a source's references name a scan's place in a transcript (how far it had read), whose
// record is the next one written, rather than the event's own record.
func scanPlace(source string) bool {
	return source == store.Sessions || source == store.Supervise
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
	if os.Getenv("MOMENTUM_EVAL") != "" {
		return usageError{"in an evaluation, evidence file is not available: a file read now holds what came after the end of store"}
	}
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
	calls := [][]string{
		{"systemctl", "--user", "status", "--no-pager", "--lines=0", "--", unit},
		{"journalctl", "--user", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n), "-u", unit},
	}
	if os.Getenv("MOMENTUM_NOW") != "" {
		// an evaluation reads the unit as it stood at the end: its log up to then, which a later line cannot change,
		// and not its state now
		end, err := commandNow()
		if err != nil {
			return err
		}
		calls = [][]string{{"journalctl", "--user", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n), "--until", "@" + strconv.FormatInt(end.Unix(), 10), "-u", unit}}
	}
	for _, args := range calls {
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
	actor := fs.String("actor", "", "only one actor: owner, agent, tool, harness, detector, unknown")
	source := fs.String("source", "", "one source (default every source)")
	session := fs.String("session", "", "one session's")
	limit := fs.Int("limit", 50, "at most this many")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "evidence search [flags] TEXT: stored summaries of every source, newest first; --source narrows it. Agent text: first 160 and last 80 bytes; calls and results: first 240 (errors keep 1,000). evidence show reads the whole record.")
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
		if name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); fs.Lookup(name) == nil && a != "-h" && a != "--help" {
			return usageError{fmt.Sprintf("flag provided but not defined: %s; a phrase that starts with a dash goes after --: evidence search [flags] -- PHRASE", a)}
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
		if strings.Contains(err.Error(), "flag provided but not defined") {
			return usageError{err.Error() + "; a phrase that starts with a dash goes after --: evidence search [flags] -- PHRASE"}
		}
		return usageError{err.Error()}
	}
	if len(phrases) != 1 || len([]rune(strings.TrimSpace(phrases[0]))) < 2 {
		return usageError{fmt.Sprintf("evidence search takes exactly one phrase of 2 characters or more (it has %d); flags may come anywhere, and each search is its own command", len(phrases))}
	}
	text := phrases[0]
	if err := knownActor(*actor); err != nil {
		return err
	}
	from, err := parseWhen(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	now, err := commandNow()
	if err != nil {
		return err
	}
	q := store.Query{Until: now, Since: from, Text: text, Session: *session, Actor: store.Actor(*actor), Source: *source, Limit: min(max(*limit, 1), 500)}
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
			q.Kinds = append(q.Kinds, k)
		}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if *source != "" {
		counts, err := st.Counts(ctx, time.Time{})
		if err != nil {
			return err
		}
		if _, ok := counts[*source]; !ok {
			var sources []string
			for name := range counts {
				sources = append(sources, name)
			}
			sort.Strings(sources)
			return usageError{fmt.Sprintf("unknown source %q; the store holds: %s", *source, strings.Join(sources, ", "))}
		}
	}
	if err := knownKinds(ctx, st, q.Kinds); err != nil {
		return err
	}
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
