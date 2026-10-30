package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
)

// Transcripts reads the agents' session history through tacit's reader (tacit's pkg/evidence): Claude Code
// with its subagents, Codex with its compressed rollouts, pi with its tasks, and the Hermes store, where tacit
// finds them under each home. Who wrote each item is tacit's call, never a copy of its rules.
//
// Each transcript has a file cursor: the offset after the last line stored and the reader's saved State, so
// a growing session costs only what was added to it. The file is told from a new one by its inode and a
// fingerprint of its first line (the mistakes log's rule), except that a transcript the harness compressed in
// place keeps its cursor: tacit names offsets in the decompressed bytes. A compressed copy is final, so its
// cursor keeps no State. A Hermes session's cursor is its last message id.
//
// Only redacted text is stored: every text goes through tacit's redaction (scrump), which fails closed. When
// redaction cannot run, nothing is read; when it fails on a file, that file stores nothing past its cursor
// this run and is reported, and the next run takes it up from there.
type Transcripts struct {
	Homes   []string // home directories to find sessions under
	Workers int      // files read at once; 0 is DefaultWorkers()

	// redact and ready are tacit's redaction (evidence.RedactEvents, evidence.RedactionReady); tests replace them.
	redact func(context.Context, []evidence.Event) ([]evidence.Event, error)
	ready  func() error
}

func (Transcripts) Name() string { return "transcripts" }

// DefaultWorkers is how many files an ingest reads at once: a third of the machine's cores. A worker parses
// and, while it redacts, runs a scrump process, so it keeps about one core busy; a first ingest of every
// session is a batch job that leaves the rest of the machine to everything else.
func DefaultWorkers() int { return max(1, runtime.NumCPU()/3) }

const (
	// chunkBytes is how much of a transcript is read before its part is queued for storing: what a kill
	// makes the next run read again.
	chunkBytes = 64 << 20
	// redactBytes is how much text a worker gathers, across its files, for one scrump call. A call costs a
	// process start whatever its size, and tacit gives it 60 s.
	redactBytes = 4 << 20
	// spare is kept past every cut until redaction is done, so a secret that straddles a cut is whole when
	// scrump sees it and the cut is made after masking (tacit's digest keeps the same 512).
	spare = 512
)

// How much of each text a summary keeps. An owner's turn is what the families quote, and a typed turn is
// seldom longer than ownerChars. An error's output holds the failure, so it keeps more than other results.
// Agent text keeps its start and its end: what it set out to do and what it asks or claims.
const (
	ownerChars = 2000
	errorChars = 1000
	textChars  = 240
	headChars  = 160
	tailChars  = 80
)

// skipWhy are the items not stored, by tacit's reason, with why: Codex's "[Recorded tool history]" message
// replays calls and results that are already events of their own.
var skipWhy = map[string]bool{"recorded tool history": true}

// plainWhy is tacit's reason that only restates the kind; meta leaves it out.
var plainWhy = map[evidence.Kind]string{evidence.KindAgentText: "assistant", evidence.KindToolCall: "tool call", evidence.KindToolResult: "tool result"}

// job is one transcript, or one Hermes session, to read.
type job struct {
	path   string // the plain path, or the Hermes session's cursor key
	format evidence.Format
	cur    store.FileCursor // the saved cursor, when ok
	ok     bool
	// a transcript
	info   os.FileInfo
	inode  uint64
	fp     string
	packed bool
	// a Hermes session
	db      *evidence.HermesDB
	session evidence.HermesSession
}

// part is a piece of a job read and not yet stored: its items, and the cursor after them.
type part struct {
	j     *job
	items []pending
	read  int
	cur   store.FileCursor
}

// pending is an item to store, its summary's text cut to what is kept plus spare, not yet redacted.
type pending struct {
	e    evidence.Event
	text string
	keep func(redacted string) string // the summary from the redacted text
}

// tally is a run's counts, shared by the workers.
type tally struct {
	sync.Mutex
	res      Result
	unknown  int
	failed   map[string]bool
	restarts int
}

func (t *tally) note(format string, a ...any) {
	t.Lock()
	t.res.Notes = append(t.res.Notes, fmt.Sprintf(format, a...))
	t.Unlock()
}

func (t *tally) fail(path string, err error) {
	t.Lock()
	if !t.failed[path] {
		t.failed[path] = true
		t.res.Notes = append(t.res.Notes, fmt.Sprintf("not stored past its cursor, retried next run: %s: %v", path, err))
	}
	t.Unlock()
}

func (s Transcripts) Ingest(ctx context.Context, st *store.Store) (Result, error) {
	ready, redact := s.ready, s.redact
	if ready == nil {
		ready = evidence.RedactionReady
	}
	if redact == nil {
		redact = evidence.RedactEvents
	}
	if err := ready(); err != nil {
		// Never raw text: with no redaction nothing is read.
		return Result{}, fmt.Errorf("redaction unavailable, nothing stored: %w", err)
	}
	cursors, err := st.FileCursors(ctx, s.Name())
	if err != nil {
		return Result{}, err
	}
	t := &tally{failed: map[string]bool{}}
	var jobs []*job
	seen := map[string]bool{}
	var dbs []*evidence.HermesDB
	defer func() {
		for _, db := range dbs {
			db.Close()
		}
	}()
	for _, home := range s.Homes {
		for _, src := range evidence.Sources(home) {
			if src.Format == evidence.Hermes {
				js, db, err := hermesJobs(ctx, src, cursors, seen)
				if db != nil {
					dbs = append(dbs, db)
				}
				if err != nil {
					t.note("hermes store %s: %v", src.DB, err)
				}
				jobs = append(jobs, js...)
				continue
			}
			files, err := evidence.Files(src)
			if err != nil {
				t.note("%s: %v", src.Glob, err)
				continue
			}
			for _, f := range files {
				if seen[f] {
					continue
				}
				seen[f] = true
				if j, ok := fileJob(f, src.Format, cursors); ok {
					jobs = append(jobs, j)
				}
			}
		}
	}
	// The largest first, so the workers end together.
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].size() > jobs[b].size() })

	workers := s.Workers
	if workers <= 0 {
		workers = DefaultWorkers()
	}
	w := writer{st: st, redact: redact, t: t, source: s.Name()}
	ch := make(chan *job)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker gathers its files' parts for one scrump call (a process start each), and stores them
			// in the order it read them, so a file's parts are stored in order.
			var b batch
			for j := range ch {
				s.readJob(ctx, j, t, func(p part) bool {
					b.add(p)
					if b.size >= redactBytes {
						w.flush(ctx, &b)
					}
					return !t.isFailed(j.path)
				})
			}
			w.flush(ctx, &b)
		}()
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		ch <- j
	}
	close(ch)
	wg.Wait()

	// Cursors of files no longer found are dropped; their events stay.
	var gone []string
	for p := range cursors {
		if !seen[p] {
			gone = append(gone, p)
		}
	}
	if err := st.DropFileCursors(ctx, s.Name(), gone); err != nil {
		return t.res, err
	}
	if t.unknown > 0 {
		t.res.Notes = append(t.res.Notes, fmt.Sprintf("items no rule credits to anyone (actor unknown): %d", t.unknown))
	}
	if t.restarts > 0 {
		t.res.Notes = append(t.res.Notes, fmt.Sprintf("files read again from the start (replaced, cut or rewritten): %d", t.restarts))
	}
	sort.Strings(t.res.Notes)
	if err := ctx.Err(); err != nil {
		return t.res, err
	}
	if len(t.failed) > 0 {
		return t.res, fmt.Errorf("%d of %d files not stored past their cursor", len(t.failed), len(jobs))
	}
	return t.res, nil
}

func (t *tally) isFailed(path string) bool {
	t.Lock()
	defer t.Unlock()
	return t.failed[path]
}

func (j *job) size() int64 {
	if j.info != nil {
		return j.info.Size()
	}
	return 0
}

// fileJob is a transcript to read, or false when it has not changed since its cursor was saved.
func fileJob(path string, f evidence.Format, cursors map[string]store.FileCursor) (*job, bool) {
	holder := evidence.Resolve(path)
	info, err := os.Stat(holder)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false // gone between the listing and now; the next run sees where it went
	}
	j := &job{path: path, format: f, info: info, packed: holder != path}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		j.inode = sys.Ino
	}
	j.cur, j.ok = cursors[path]
	if j.ok && j.cur.Inode == j.inode && j.cur.Size == info.Size() && j.cur.MTime == info.ModTime().UnixNano() {
		return nil, false
	}
	j.fp = headPrint(path)
	return j, true
}

// headPrint is the fingerprint of a transcript's first line (at most fingerprintLen bytes of it), read from
// its JSONL bytes whichever file holds them.
func headPrint(path string) string {
	head := evidence.Head(path, fingerprintLen)
	if i := strings.IndexByte(string(head), '\n'); i >= 0 {
		head = head[:i+1]
	}
	if len(head) == 0 {
		return ""
	}
	h := sha256.Sum256(head)
	return hex.EncodeToString(h[:8])
}

// from is where a read starts: the saved cursor, with the reader's saved State when there is one, when the
// file is the one it was saved for (a Hermes session always is), else the start; restart says a saved cursor
// was let go. A transcript compressed in place since (or decompressed) keeps its cursor.
func (j *job) from() (from evidence.State, restart bool) {
	if !j.ok {
		return evidence.State{}, false
	}
	if j.format != evidence.Hermes && (j.cur.FP != j.fp || j.cur.Inode != j.inode && j.cur.Compressed == j.packed) {
		return evidence.State{}, true
	}
	from = evidence.State{Offset: j.cur.Offset}
	if len(j.cur.State) > 0 {
		var saved evidence.State
		if saved.UnmarshalBinary(j.cur.State) == nil && saved.Offset == j.cur.Offset {
			from = saved
		}
	}
	return from, false
}

// hermesJobs are the sessions of a Hermes store with rows past their cursor.
func hermesJobs(ctx context.Context, src evidence.Source, cursors map[string]store.FileCursor, seen map[string]bool) ([]*job, *evidence.HermesDB, error) {
	if src.DB == "" {
		return nil, nil, nil
	}
	if _, err := os.Stat(src.DB); errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	db, err := evidence.OpenHermes(src.DB)
	if err != nil {
		return nil, nil, err
	}
	ss, err := db.Sessions(ctx)
	if err != nil {
		return nil, db, err
	}
	last, err := db.LastIDs(ctx)
	if err != nil {
		return nil, db, err
	}
	var out []*job
	for _, s := range ss {
		key := "hermes:" + db.Path + "#" + s.ID
		seen[key] = true
		c, ok := cursors[key]
		if ok && c.Offset == last[s.ID] || !ok && last[s.ID] == 0 {
			continue
		}
		out = append(out, &job{path: key, format: evidence.Hermes, cur: c, ok: ok, db: db, session: s})
	}
	return out, db, nil
}

// readJob reads what is new in one transcript or Hermes session, a part at a time, and hands each part to
// add, which says whether to go on.
func (s Transcripts) readJob(ctx context.Context, j *job, t *tally, add func(part) bool) {
	from, restart := j.from()
	retried := false
	for {
		if restart {
			t.Lock()
			t.restarts++
			t.Unlock()
			restart = false
		}
		p := part{j: j}
		emit := func(e evidence.Event) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.read++
			if it, ok := toPending(e); ok {
				p.items = append(p.items, it)
			}
			return nil
		}
		var next evidence.State
		var err error
		if j.format == evidence.Hermes {
			next, err = evidence.ReadHermesState(ctx, j.db, j.session, from, emit)
		} else {
			next, err = evidence.ReadFileState(j.path, j.format, from, chunkBytes, emit)
		}
		if errors.Is(err, evidence.ErrPastEnd) && !retried {
			// Cut, replaced or restored since the cursor was saved: read it again from its start.
			from, restart, retried = evidence.State{}, true, true
			continue
		}
		if err != nil {
			t.fail(j.path, err)
			return
		}
		p.cur = store.FileCursor{Source: s.Name(), Path: j.path, Inode: j.inode, FP: j.fp, Compressed: j.packed, Offset: next.Offset}
		if j.info != nil {
			p.cur.Size, p.cur.MTime = j.info.Size(), j.info.ModTime().UnixNano()
		}
		if !j.packed && next.Kept() {
			// A compressed copy is final: it keeps no State.
			if p.cur.State, err = next.MarshalBinary(); err != nil {
				t.fail(j.path, err)
				return
			}
		}
		if !add(p) {
			return
		}
		// A Hermes session is read whole; a transcript until the size it had when it was listed (what was
		// added since is the next run's).
		if j.format == evidence.Hermes || next.Offset == from.Offset || next.Offset >= j.contentSize() {
			return
		}
		from = next
	}
}

// contentSize is the transcript's size in JSONL bytes as it was listed.
func (j *job) contentSize() int64 {
	if !j.packed {
		return j.info.Size()
	}
	if _, n, err := evidence.Stat(j.path); err == nil {
		return n
	}
	return 0
}

// batch is a worker's parts waiting for redaction.
type batch struct {
	parts []part
	size  int
}

func (b *batch) add(p part) {
	b.parts = append(b.parts, p)
	for _, it := range p.items {
		b.size += len(it.text)
	}
}

// writer redacts batches and stores them: one writer at a time, each batch's events and the cursors after
// them in one transaction.
type writer struct {
	mu     sync.Mutex
	st     *store.Store
	redact func(context.Context, []evidence.Event) ([]evidence.Event, error)
	t      *tally
	source string
}

// flush redacts a batch in as few scrump calls as its size allows and stores it. When redaction fails, each
// file's parts are redacted on their own, so only the file it failed on stores nothing more, and none of
// that file's later parts is stored.
func (w *writer) flush(ctx context.Context, b *batch) {
	parts := b.parts
	*b = batch{}
	if len(parts) == 0 {
		return
	}
	evs, unknown, err := w.events(ctx, parts)
	if err != nil {
		var files []string
		byFile := map[string][]part{}
		for _, p := range parts {
			if byFile[p.j.path] == nil {
				files = append(files, p.j.path)
			}
			byFile[p.j.path] = append(byFile[p.j.path], p)
		}
		parts, evs, unknown = nil, nil, 0
		for _, f := range files {
			fe, fu, ferr := w.events(ctx, byFile[f])
			if ferr != nil {
				w.t.fail(f, ferr)
				continue
			}
			parts, evs, unknown = append(parts, byFile[f]...), append(evs, fe...), unknown+fu
		}
	}
	ws := make([]store.FileWrite, 0, len(parts))
	read := 0
	for i, p := range parts {
		if w.t.isFailed(p.j.path) {
			continue // a part after one that failed would move the cursor past it
		}
		ws = append(ws, store.FileWrite{Events: evs[i], Cursor: p.cur})
		read += p.read
	}
	w.mu.Lock()
	n, err := w.st.PutFiles(ctx, ws)
	w.mu.Unlock()
	if err != nil {
		for _, x := range ws {
			w.t.fail(x.Cursor.Path, err)
		}
		return
	}
	w.t.Lock()
	w.t.res.New += n
	w.t.res.Read += read
	w.t.unknown += unknown
	w.t.Unlock()
}

// events redacts the parts' texts, at most redactBytes per scrump call, and maps their items to store
// events, part by part. Each call gets its texts joined by newlines as one text, and the redacted text is
// cut back at the same places: masks keep the length. A mask that runs from one text into the next (a quoted
// value a cut left open masks up to the next quote) masks the start of the next text too, which hides more,
// never less; as separate records, such a mask makes tacit's redaction split the batch and mask it again,
// half by half. Redaction fails closed: on any failure no event is returned.
func (w *writer) events(ctx context.Context, parts []part) ([][]store.Event, int, error) {
	var items []pending
	for _, p := range parts {
		items = append(items, p.items...)
	}
	out := make([]store.Event, 0, len(items))
	unknown := 0
	var joined strings.Builder
	for start := 0; start < len(items); {
		end, size := start, 0
		for end < len(items) && (end == start || size+len(items[end].text)+1 <= redactBytes) {
			size += len(items[end].text) + 1
			end++
		}
		joined.Reset()
		for i := start; i < end; i++ {
			if i > start {
				joined.WriteByte('\n')
			}
			joined.WriteString(items[i].text)
		}
		red, err := w.redact(ctx, []evidence.Event{{Text: joined.String()}})
		if err != nil {
			return nil, 0, fmt.Errorf("redaction failed: %w", err)
		}
		if len(red) != 1 || len(red[0].Text) != joined.Len() {
			return nil, 0, errors.New("redaction changed the text's length")
		}
		masked, at := red[0].Text, 0
		for i := start; i < end; i++ {
			n := len(items[i].text)
			e, unk := toEvent(w.source, items[i].e, items[i].keep(masked[at:at+n]))
			if unk {
				unknown++
			}
			out = append(out, e)
			at += n + 1
		}
		start = end
	}
	byPart := make([][]store.Event, len(parts))
	for i, p := range parts {
		byPart[i], out = out[:len(p.items)], out[len(p.items):]
	}
	return byPart, unknown, nil
}

// toPending is the item to store for an event, or false for one not stored (skipWhy).
func toPending(e evidence.Event) (pending, bool) {
	if skipWhy[e.Why] {
		return pending{}, false
	}
	p := pending{e: e}
	switch e.Kind {
	case evidence.KindOwnerText:
		p.text, p.keep = cutHead(e.Text, ownerChars)
	case evidence.KindAgentText:
		p.text, p.keep = cutEnds(e.Text, headChars, tailChars)
	case evidence.KindToolCall:
		p.text, p.keep = cutHead(e.ToolInput, textChars)
	case evidence.KindToolResult:
		n := textChars
		if e.IsError {
			n = errorChars
		}
		p.text, p.keep = cutHead(e.ToolOutput, n)
	default: // harness text, a compaction's summary, and kinds tacit adds later
		p.text, p.keep = cutHead(e.Text, textChars)
	}
	return p, true
}

// cutHead keeps n bytes of s, plus spare until redaction.
func cutHead(s string, n int) (string, func(string) string) {
	return prefix(s, n+spare), func(r string) string { return oneLine(prefix(r, n), len(s) > n) }
}

// cutEnds keeps the first head and the last tail bytes of s, each with spare until redaction. Masks keep
// the length, so the redacted text's own ends are the ends kept.
func cutEnds(s string, head, tail int) (string, func(string) string) {
	if len(s) <= head+tail {
		return s, func(r string) string { return oneLine(r, false) }
	}
	keep := func(r string) string { return oneLine(prefix(r, head), true) + " " + oneLine(suffix(r, tail), false) }
	if len(s) <= head+tail+2*spare {
		return s, keep
	}
	return prefix(s, head+spare) + "\n" + suffix(s, tail+spare), keep
}

// prefix is s cut to at most n bytes at a character boundary.
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// suffix is the last n bytes of s at most, from a character boundary.
func suffix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// oneLine collapses whitespace, and marks a cut with an ellipsis.
func oneLine(s string, cut bool) string {
	s = strings.Join(strings.Fields(s), " ")
	if cut {
		s += "…"
	}
	return s
}

// toEvent maps one item. The actor follows the kind: the owner's text is his; agent text and tool calls an
// agent's; a tool's result the tool's; harness text and compactions the harness's. A call the owner made
// himself (a !command he ran in pi) is his, and an item tacit credits to no one is unknown.
func toEvent(source string, e evidence.Event, summary string) (store.Event, bool) {
	var actor store.Actor
	switch e.Kind {
	case evidence.KindOwnerText:
		actor = store.ActorOwner
	case evidence.KindAgentText:
		actor = store.ActorAgent
	case evidence.KindToolCall:
		actor = store.ActorAgent
		if e.Who == evidence.Owner {
			actor = store.ActorOwner
		}
	case evidence.KindToolResult:
		actor = store.ActorTool
	default:
		actor = store.ActorHarness
	}
	unknown := e.Who == evidence.Unknown
	if unknown {
		actor = store.ActorUnknown
	}
	// A transcript's file name is its session's own, so its events keep their keys when the harness moves
	// or compresses the file; a Hermes row is named by its store.
	name := e.Path
	if e.Harness != "hermes" {
		name = evidence.Name(e.Path)
	}
	why := e.Why
	if why == plainWhy[e.Kind] {
		why = ""
	}
	return store.Event{
		Source:    source,
		SourceKey: fmt.Sprintf("%s@%012d#%03d", name, e.Offset, e.Index),
		TS:        e.At,
		Session:   e.Session,
		Agent:     e.Subagent,
		Harness:   e.Harness,
		Model:     e.Model,
		Cwd:       e.Cwd,
		Actor:     actor,
		Kind:      string(e.Kind),
		Tool:      e.ToolName,
		Summary:   summary,
		Meta: meta(map[string]any{"tool_id": e.ToolID, "is_error": e.IsError, "input_kind": string(e.InputKind),
			"prior": e.Prior, "why": why, "parent": e.Parent}),
		Ref: fmt.Sprintf("%s@%d#%d", e.Path, e.Offset, e.Index),
	}, unknown
}
