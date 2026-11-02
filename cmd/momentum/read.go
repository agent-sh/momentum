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
	"math"
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
	"github.com/agent-sh/momentum/internal/source"
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

// showMost is how many events one evidence show prints.
const showMost = 10

// evidenceShow prints events of the store and, where an event's reference names a record of a transcript or a log
// (path@offset), that record as the file holds it, or a Hermes event's message row, each scrubbed whole and then cut
// to --bytes. Only an event the store holds is shown, so a store made as of a moment shows nothing after it. A scan
// place names the next record, not the event. With MOMENTUM_NOW set (an evaluation's trial), a record dated at or
// after that end is withheld, and so is an undated one a scan place names.
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
		// back off only a rune the cut split, not to an earlier invalid byte
		for i := len(clean) - 1; i >= 0 && i >= len(clean)-utf8.UTFMax; i-- {
			if utf8.RuneStart(clean[i]) {
				if !utf8.FullRuneInString(clean[i:]) {
					clean = clean[:i]
				}
				break
			}
		}
		clean += fmt.Sprintf("\n  (its first %d of %d bytes)\n", len(clean), whole)
	}
	return text + clean, nil
}

// eventRecord reads the record an event's reference names: the record, or a note that says why there is none.
func eventRecord(ctx context.Context, e store.Event) (record, note string, err error) {
	if dir := os.Getenv("MOMENTUM_EVIDENCE_CAPTURE"); dir != "" {
		return capturedRecord(ctx, dir, e)
	}
	return readEventRecord(ctx, e)
}

func readEventRecord(ctx context.Context, e store.Event) (record, note string, err error) {
	path, off, ok := placeOf(e.Ref)
	if !ok {
		return "", "", nil
	}
	// opened without waiting on a writer, and read only when it is a regular file: a reference may now name a pipe or
	// a device, which would hold the run
	f, err := openRecord(path)
	if err != nil {
		if errors.Is(err, errWritableRecord) {
			return "", "  (record withheld: it lies where a run can write)\n", nil
		}
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

// hermesRow reads a Hermes message row by its id, read-only, as one JSON record. The row is the event's own only
// while it is of the event's session at the event's time (a Hermes store made again starts its ids over, and an old
// event's id then names another session's message), and only in the version the ingest read: Hermes rewrites rows in
// place (a turn's prologue rewrites the prompt it stored, a prune rewrites old rows), so an event keeps a digest of
// the row's content and tool calls, and a trial withholds a row whose digest is not the one the ingest kept.
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
	// the row's size and its place first: a row over the bound is not read, and one that is not the event's own is not
	// read either
	var size int64
	var session string
	var stamp float64
	err = db.QueryRowContext(cctx, `SELECT length(CAST(coalesce(content, '') AS BLOB)) + length(CAST(`+column("tool_calls")+` AS BLOB)),
	  coalesce(session_id, ''), timestamp FROM messages WHERE id = ?`, id).Scan(&size, &session, &stamp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "  (its record is gone)\n", nil
	case err != nil:
		return "", "  (its record could not be read: " + plain.Line(err.Error()) + ")\n", nil
	case size > readMax:
		return "", "  (oversized record withheld)\n", nil
	case math.IsNaN(stamp) || stamp < 0 || stamp >= hermesLatest:
		// a time no clock reads (Hermes writes seconds since 1970): it fails closed, before any conversion
		return "", "  (record withheld: its timestamp is no time)\n", nil
	}
	at := hermesAt(stamp)
	if diff := at.UnixMilli() - e.TS.UnixMilli(); !hermesSession(session, e) || diff < -1 || diff > 1 { // a float's millisecond
		return "", "  (its record is gone: the row at its id is another's, the store was made again since)\n", nil
	}
	if os.Getenv("MOMENTUM_NOW") != "" {
		end, err := commandNow()
		if err != nil {
			return "", "", err
		}
		if !at.Before(end) {
			return "", "  (record withheld: timestamp is at or after the evaluation end)\n", nil
		}
	}
	var row struct {
		ID        int64   `json:"id"`
		Session   string  `json:"session_id"`
		Role      string  `json:"role"`
		Content   string  `json:"content"`
		ToolCalls string  `json:"tool_calls,omitempty"`
		ToolName  string  `json:"tool_name,omitempty"`
		Timestamp float64 `json:"timestamp"`
	}
	err = db.QueryRowContext(cctx, `SELECT id, session_id, role, coalesce(content, ''), `+column("tool_calls")+`, `+column("tool_name")+`, timestamp
	  FROM messages WHERE id = ?`, id).Scan(&row.ID, &row.Session, &row.Role, &row.Content, &row.ToolCalls, &row.ToolName, &row.Timestamp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "  (its record is gone)\n", nil
	case err != nil:
		return "", "  (its record could not be read: " + plain.Line(err.Error()) + ")\n", nil
	}
	rewritten := false
	if want := eventMeta(e)[source.HermesDigestKey]; want != "" {
		rewritten = source.HermesRowDigest(row.Content, row.ToolCalls) != want
	} else if os.Getenv("MOMENTUM_NOW") != "" {
		return "", "  (record withheld: no digest of the row was kept when it was read, so a rewrite since cannot be told)\n", nil
	}
	note := ""
	if rewritten {
		if os.Getenv("MOMENTUM_NOW") != "" {
			return "", "  (record withheld: the row was rewritten since it was read)\n", nil
		}
		note = "  (the row was rewritten since it was read: this is its text now)\n"
	}
	// printed as Hermes stores it, one level deep: the content as written, each call's arguments as the JSON they
	// are, and no HTML escapes, so the scrubber reads the text as an agent wrote it
	out := map[string]any{"id": row.ID, "session_id": row.Session, "role": row.Role, "content": row.Content, "timestamp": row.Timestamp}
	if row.ToolName != "" {
		out["tool_name"] = row.ToolName
	}
	if row.ToolCalls != "" {
		out["tool_calls"] = hermesCalls(row.ToolCalls)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", "", err
	}
	return b.String(), note, nil
}

// hermesSession says whether a row's session is the event's: its own, or the delegation it ran as a subagent. A row
// of no session is no event's.
func hermesSession(row string, e store.Event) bool {
	return row != "" && (row == e.Session || e.Agent != "" && row == e.Agent)
}

// eventMeta is an event's meta as strings: the keys whose values are text.
func eventMeta(e store.Event) map[string]string {
	var all map[string]any
	out := map[string]string{}
	if json.Unmarshal([]byte(e.Meta), &all) == nil {
		for k, v := range all {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// hermesLatest bounds a Hermes row's timestamp, in seconds: past it (the year 33658) no row is dated, and the
// conversion to a time stays far from int64's overflow.
const hermesLatest = 1e12

// hermesAt is a Hermes row's time as tacit's reader dates its events: seconds, their fraction to the nanosecond.
func hermesAt(ts float64) time.Time {
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9))
}

// hermesCalls is a row's tool calls as JSON, each call's arguments (a JSON text in OpenAI's form) embedded as the JSON
// they hold; text that is no JSON stays text.
func hermesCalls(raw string) any {
	var calls []map[string]any
	if json.Unmarshal([]byte(raw), &calls) != nil {
		return raw
	}
	for _, c := range calls {
		if fn, ok := c["function"].(map[string]any); ok {
			if args, ok := fn["arguments"].(string); ok && json.Valid([]byte(args)) {
				fn["arguments"] = json.RawMessage(args)
			}
		}
	}
	return calls
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
// last lines of the unit (200, or --lines N up to 2000). In an evaluation's trial (MOMENTUM_NOW), only its log up to
// the end of store.
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
		// a trial reads the unit as it stood at the end: its log up to then, which a later line cannot change, and
		// not its state now
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
	switch {
	case len(phrases) != 1:
		return usageError{fmt.Sprintf("evidence search takes exactly one phrase (it has %d); flags may come anywhere, and each search is its own command", len(phrases))}
	case len([]rune(strings.TrimSpace(phrases[0]))) < 2:
		return usageError{"evidence search takes a phrase of 2 characters or more"}
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
