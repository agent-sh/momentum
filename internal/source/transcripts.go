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
// Events hold only redacted text: every text goes through tacit's redaction (scrump), which fails closed.
// When redaction cannot run, nothing is read; when it fails on a file, that file stores nothing past its
// cursor this run and is reported, and the next run takes it up from there. The reader's saved State, kept
// with the cursor, holds transcript lines unredacted (tacit's rules need them whole): it is as private as the
// transcript, and is never printed or sent.
type Transcripts struct {
	Homes   []string // home directories to find sessions under
	Workers int      // files read at once; 0 is DefaultWorkers()

	// records, redact and ready are tacit's redaction (evidence.RedactRecordsContext, evidence.RedactEvents,
	// evidence.RedactionReady); tests replace them.
	records func(context.Context, []string) ([]string, error)
	redact  func(context.Context, []evidence.Event) ([]evidence.Event, error)
	ready   func() error
}

func (Transcripts) Name() string { return store.Transcripts }

// DefaultWorkers is how many files an ingest reads at once: a third of the machine's cores. A worker parses
// and, while it redacts, runs a scrump process, so it keeps about one core busy; a first ingest of every
// session is a batch job that leaves the rest of the machine to everything else.
func DefaultWorkers() int { return max(1, runtime.NumCPU()/3) }

// chunkBytes is how much of a transcript is read before its part is queued for storing: what a kill makes
// the next run read again. redactBytes is how much text a worker gathers, across its files, for one scrump
// call: a call costs a process start whatever its size, and tacit gives it 60 s. Tests make both small.
var (
	chunkBytes  int64 = 64 << 20
	redactBytes       = 4 << 20
)

// spare is kept past every cut until redaction is done, so a secret that straddles a cut is whole when scrump
// sees it and the cut is made after masking (tacit's digest keeps the same 512).
const spare = 512

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

// part is a piece of a job read and not yet stored: its items, the cursor after them, and the places of the
// records it read (place), those of items not stored among them.
type part struct {
	j      *job
	items  []pending
	read   int
	cur    store.FileCursor
	places []string
}

// pending is an item to store: its event with the texts taken out (a batch holds only what it stores), the
// texts its summary is made of cut to what is kept plus spare, not yet redacted (one, or an agent text's
// head and tail), and how the summary is made of them once they are.
type pending struct {
	e     evidence.Event
	texts []string
	keep  func(redacted []string) string
}

// tally is a run's counts, shared by the workers.
type tally struct {
	sync.Mutex
	res      Result
	unknown  int
	failed   map[string]bool
	restarts int
	dropped  int
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
	ready, records, redact := s.ready, s.records, s.redact
	if ready == nil {
		ready = evidence.RedactionReady
	}
	if records == nil {
		records = evidence.RedactRecordsContext
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
	listed := true // every source was listed, so a cursor not seen is a file gone
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
					listed = false
				}
				jobs = append(jobs, js...)
				continue
			}
			files, err := evidence.Files(src)
			if err != nil {
				t.note("%s: %v", src.Glob, err)
				listed = false
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
	w := writer{st: st, records: records, redact: redact, t: t, source: s.Name()}
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

	// Cursors of files no longer found are dropped; their events stay. A run that could not list a source
	// drops none: what it did not see may still be there.
	var gone []string
	for p := range cursors {
		if listed && !seen[p] && missing(p) {
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
	if t.dropped > 0 {
		t.res.Notes = append(t.res.Notes, fmt.Sprintf("events of records read again that this reading does not give, dropped: %d", t.dropped))
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

// missing reports a cursor's file gone: a transcript whose plain file and compressed copy both do not exist
// (a folder that cannot be listed lists as empty), or a Hermes session not in its store.
func missing(path string) bool {
	if strings.HasPrefix(path, "hermes:") {
		return true
	}
	for _, p := range []string{path, path + ".zst"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
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
		if seen[key] {
			continue // the same store through another home
		}
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
			if pl := place(e); len(p.places) == 0 || p.places[len(p.places)-1] != pl {
				p.places = append(p.places, pl)
			}
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
		// A Hermes session is read whole; a transcript until the size it had when it was listed (what was
		// added since is the next run's).
		final := j.format == evidence.Hermes || next.Offset == from.Offset
		if size, ok := j.contentSize(); !final && ok && next.Offset >= size {
			final = true
		}
		p.cur = store.FileCursor{Source: s.Name(), Path: j.path, Inode: j.inode, FP: j.fp, Compressed: j.packed, Offset: next.Offset}
		if j.info != nil && final {
			// Only the part that ends the read marks the file read as it was listed: when a later part is
			// not stored, the next run finds the file unmarked and reads on from the cursor.
			p.cur.Size, p.cur.MTime = j.info.Size(), j.info.ModTime().UnixNano()
		}
		if !j.packed && next.Kept() {
			// A compressed copy is final: it keeps no State.
			if p.cur.State, err = next.MarshalBinary(); err != nil {
				t.fail(j.path, err)
				return
			}
		}
		if !add(p) || final {
			return
		}
		from = next
	}
}

// contentSize is the transcript's size in JSONL bytes as it was listed, and false when it cannot be read
// (a compressed copy whose frame headers do not parse): the read then goes on until a part makes no progress.
func (j *job) contentSize() (int64, bool) {
	if j.info == nil {
		return 0, false
	}
	if !j.packed {
		return j.info.Size(), true
	}
	if _, n, err := evidence.Stat(j.path); err == nil {
		return n, true
	}
	return 0, false
}

// batch is a worker's parts waiting for redaction.
type batch struct {
	parts []part
	size  int
}

func (b *batch) add(p part) {
	b.parts = append(b.parts, p)
	for _, it := range p.items {
		for _, t := range it.texts {
			b.size += len(t)
		}
	}
}

// writer redacts batches and stores them: one writer at a time, each batch's events and the cursors after
// them in one transaction.
type writer struct {
	mu      sync.Mutex
	st      *store.Store
	records func(context.Context, []string) ([]string, error) // nil: no records mode, the boundary
	redact  func(context.Context, []evidence.Event) ([]evidence.Event, error)
	t       *tally
	source  string
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
		ws = append(ws, store.FileWrite{Events: evs[i], Cursor: p.cur, Places: p.places})
		read += p.read
	}
	w.mu.Lock()
	n, dropped, err := w.st.PutFiles(ctx, ws)
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
	w.t.dropped += dropped
	w.t.Unlock()
}

// events redacts the parts' texts and maps their items to store events, part by part. Redaction fails
// closed: on any failure no event is returned.
func (w *writer) events(ctx context.Context, parts []part) ([][]store.Event, int, error) {
	var items []pending
	var texts []string
	for _, p := range parts {
		for _, it := range p.items {
			items = append(items, it)
			texts = append(texts, it.texts...)
		}
	}
	red, err := w.mask(ctx, texts)
	if err != nil {
		return nil, 0, err
	}
	out := make([]store.Event, 0, len(items))
	unknown := 0
	for _, it := range items {
		n := len(it.texts)
		e, unk := toEvent(w.source, it.e, it.keep(red[:n]))
		red = red[n:]
		if unk {
			unknown++
		}
		out = append(out, e)
	}
	byPart := make([][]store.Event, len(parts))
	for i, p := range parts {
		byPart[i], out = out[:len(p.items)], out[len(p.items):]
	}
	return byPart, unknown, nil
}

// boundary separates the texts of one scrump call when the installed scrump has no records mode, so each is
// masked as it would be alone. scrump scans its input as one stream, and a hit that starts in one text can run
// into the next: a quoted value a cut left open runs to the next quote, and a rule like
// `keyword(?:.|[\n\r]){0,40}?secret` reaches across lines. Where it runs in, it masks the start of the next text
// but ends where scrump resumes its scan, which can be inside a secret of that text, past the key that marks
// it; a hit the rule's filter rejects masks nothing and still moves the scan on. The boundary closes an open
// quoted value (its quotes), then is longer than the text profile's longest reach across lines (1,000 bytes),
// and ends with a mark no secret's value starts with, so no hit that starts in one text reaches the next.
var boundary = "\n#'\"\n" + strings.Repeat("\n", 1024) + "#\n"

// mask redacts texts, at most redactBytes per scrump call. scrump's records mode (scrump 0.2.0 and later)
// scans each text on its own, as a record (tacit's RedactRecordsContext): no hit can cross from one text into
// the next, and nothing is added to the text scrump reads. A scrump without it gets the texts joined by the
// boundary (joined).
func (w *writer) mask(ctx context.Context, texts []string) ([]string, error) {
	out := make([]string, 0, len(texts))
	for start := 0; start < len(texts); {
		end, size := start, 0
		for end < len(texts) && (end == start || size+len(texts[end]) <= redactBytes) {
			size += len(texts[end])
			end++
		}
		red, err := w.asRecords(ctx, texts[start:end])
		if errors.Is(err, evidence.ErrNoRecords) {
			red, err = w.joined(ctx, texts[start:end])
		}
		if err != nil {
			return nil, err
		}
		out = append(out, red...)
		start = end
	}
	return out, nil
}

// asRecords redacts texts in one call in scrump's records mode, or gives evidence.ErrNoRecords when the
// installed scrump has none.
func (w *writer) asRecords(ctx context.Context, texts []string) ([]string, error) {
	if w.records == nil {
		return nil, evidence.ErrNoRecords
	}
	red, err := w.records(ctx, texts)
	if errors.Is(err, evidence.ErrNoRecords) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("redaction failed: %w", err)
	}
	if len(red) != len(texts) {
		return nil, errors.New("redaction returned a different number of texts")
	}
	for i := range red {
		if len(red[i]) != len(texts[i]) {
			return nil, errors.New("redaction changed a text's length")
		}
	}
	return red, nil
}

// joined redacts texts in one call that gets them joined by the boundary as one text, and cuts the redacted
// text back at the same places, since masks keep the length.
//
// A text the call masked from its first byte, past any newlines (a hit ran in from the boundary's side,
// which a rule longer than the boundary could do), is masked again on its own in one more call, as records,
// and a byte stays masked when either call masked it.
func (w *writer) joined(ctx context.Context, texts []string) ([]string, error) {
	out := make([]string, len(texts))
	var joined strings.Builder
	for i, t := range texts {
		if i > 0 {
			joined.WriteString(boundary)
		}
		joined.WriteString(t)
	}
	red, err := w.redact(ctx, []evidence.Event{{Text: joined.String()}})
	if err != nil {
		return nil, fmt.Errorf("redaction failed: %w", err)
	}
	if len(red) != 1 || len(red[0].Text) != joined.Len() {
		return nil, errors.New("redaction changed the text's length")
	}
	masked, at := red[0].Text, 0
	var again []int
	for i, t := range texts {
		n := len(t)
		out[i] = masked[at : at+n]
		if first := strings.TrimLeft(out[i], "\n"); i > 0 && first != "" && first[0] == '*' {
			again = append(again, i)
		}
		at += n + len(boundary)
	}
	if len(again) > 0 {
		carriers := make([]evidence.Event, len(again))
		for k, i := range again {
			carriers[k].Text = texts[i]
		}
		alone, err := w.redact(ctx, carriers)
		if err != nil {
			return nil, fmt.Errorf("redaction failed: %w", err)
		}
		if len(alone) != len(again) {
			return nil, errors.New("redaction returned a different number of texts")
		}
		for k, i := range again {
			if len(alone[k].Text) != len(out[i]) {
				return nil, errors.New("redaction changed a text's length")
			}
			out[i] = union(out[i], alone[k].Text)
		}
	}
	return out, nil
}

// union is a text masked where either of two maskings of it masked it.
func union(a, b string) string {
	if a == b {
		return a
	}
	u := []byte(a)
	for i := range u {
		if b[i] == '*' {
			u[i] = '*'
		}
	}
	return string(u)
}

// toPending is the item to store for an event, or false for one not stored (skipWhy).
func toPending(e evidence.Event) (pending, bool) {
	if skipWhy[e.Why] {
		return pending{}, false
	}
	var p pending
	switch e.Kind {
	case evidence.KindOwnerText:
		p.texts, p.keep = cutHead(e.Text, ownerChars)
	case evidence.KindAgentText:
		p.texts, p.keep = cutEnds(e.Text, headChars, tailChars)
	case evidence.KindToolCall:
		p.texts, p.keep = cutHead(e.ToolInput, textChars)
	case evidence.KindToolResult:
		n := textChars
		if e.IsError {
			n = errorChars
		}
		p.texts, p.keep = cutHead(e.ToolOutput, n)
	default: // harness text, a compaction's summary, and kinds tacit adds later
		p.texts, p.keep = cutHead(e.Text, textChars)
	}
	e.Text, e.ToolInput, e.ToolOutput = "", "", ""
	p.e = e
	return p, true
}

// cutHead keeps n bytes of s, plus spare until redaction. The cut text is a copy, so the batch does not hold
// the whole of s.
func cutHead(s string, n int) ([]string, func([]string) string) {
	cut := len(s) > n
	return []string{strings.Clone(prefix(s, n+spare))}, func(r []string) string { return oneLine(prefix(r[0], n), cut) }
}

// cutEnds keeps the first head and the last tail bytes of s, each with spare until redaction, as two texts
// (so a quote the head leaves open is not paired with one of the tail's), or one when they meet.
func cutEnds(s string, head, tail int) ([]string, func([]string) string) {
	if len(s) <= head+tail {
		return []string{strings.Clone(s)}, func(r []string) string { return oneLine(r[0], false) }
	}
	if len(s) <= head+tail+2*spare {
		return []string{strings.Clone(s)}, func(r []string) string {
			return oneLine(prefix(r[0], head), true) + " " + oneLine(suffix(r[0], tail), false)
		}
	}
	return []string{strings.Clone(prefix(s, head+spare)), strings.Clone(suffix(s, tail+spare))}, func(r []string) string {
		return oneLine(prefix(r[0], head), true) + " " + oneLine(suffix(r[1], tail), false)
	}
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

// place is the prefix of the keys of an item's record (a transcript's line, a Hermes row): the record's offset,
// after the transcript's file name, which is its session's own, so its events keep their keys when the harness
// moves or compresses the file, or after the Hermes store. An item's key is its place and its index in the record.
func place(e evidence.Event) string {
	name := e.Path
	if e.Harness != "hermes" {
		name = evidence.Name(e.Path)
	}
	return fmt.Sprintf("%s@%012d#", name, e.Offset)
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
	why := e.Why
	if why == plainWhy[e.Kind] {
		why = ""
	}
	// A Codex subagent's thread, a pi task and a Hermes delegation are sessions of their own in tacit's
	// events, their parent named; here they are a subagent inside the parent's session, as Claude Code's are.
	session := e.Session
	if e.Subagent != "" && e.Subagent == e.Session && e.Parent != "" {
		session = e.Parent
	}
	return store.Event{
		Source:    source,
		SourceKey: fmt.Sprintf("%s%03d", place(e), e.Index),
		TS:        e.At,
		Session:   session,
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
