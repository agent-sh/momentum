// Package request groups the owner's work into requests across the sessions it ran in, and reads what was observed
// of each (SPEC, Requests). It reads the store's events and decides by rules, with no model: the links tacit's
// reader records (subagents, parents, the copies a fork or a resume carries over) and a handoff's id pasted into a
// turn. The one thing kept is the owner's check of a result.
package request

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
)

// States of a request, the first that holds (SPEC, Requests, State).
const (
	NeedsYou = "needs_you"
	Working  = "working"
	Checked  = "checked"
	Unknown  = "unknown"
	Ready    = "ready"
	Open     = "open"
)

// Kinds of results.
const (
	KindCommit = "commit"
	KindPush   = "push"
	KindPR     = "pr"
	KindFile   = "file"
	KindReport = "report"
)

// Ask is a turn of the owner's in a request: his words, outside any handoff the turn pasted. Handoff is the id the
// turn pasted, when it pasted one.
type Ask struct {
	At      time.Time
	Thread  string
	Harness string
	Words   string
	Event   string
	Handoff string
}

// Result is what a request's tools recorded (an artifact) or the agent's last reply (its report, a claim). Key
// names it for a check, the same in a fork that carried it over.
type Result struct {
	Kind   string
	Key    string
	Text   string
	At     time.Time
	Thread string
	Event  string
	Writes int // a file's writes in the request
}

// Thread is a session of a request: when its events in the request ran, and its last observed phase. Phase is
// empty when no sessions run recorded one; Trusted is false for a phase seen before the request's last turn in the
// session, which says nothing of what came after. Current is true when its latest segment is this request's: only
// then does its phase count for it.
type Thread struct {
	ID          string
	Harness     string
	Cwd         string
	First, Last time.Time
	Phase       string
	PhaseAt     time.Time
	Trusted     bool
	Ask         string
	AskKind     string
	Current     bool
	turn        time.Time // the request's last turn in it
}

// Request is one piece of the owner's work and what was observed of it.
type Request struct {
	ID       string
	Aliases  []string  // other ids that resolve to it
	Opened   time.Time // zero when its opening is not in the store
	Asks     []Ask     // his words, oldest first
	LastTurn time.Time // his last turn, or the last turn that pasted its handoff
	Threads  []Thread
	Results  []Result // the artifacts, newest first
	Report   *Result  // the agent's last reply: its claim
	Checks   []store.RequestCheck
	State    string
	Why      string // what the state rests on
	Open     string // the open step; empty when nothing records one
	Last     time.Time
	// CutGone, CutUnread and CutUnmasked count its calls the store cut that were read as the store holds them:
	// their transcript is gone, no reader of their harness is here, or what was read whole could not be masked
	CutGone, CutUnread, CutUnmasked int
	pending                         []pair
}

// start is when the request began: its opening, else the time its id names.
func (r *Request) start() time.Time {
	if !r.Opened.IsZero() {
		return r.Opened
	}
	at, _ := ParseID(r.ID)
	return at
}

// Newest is the result a check binds: the newest artifact, else the report; nil when there is neither.
func (r *Request) Newest() *Result {
	if len(r.Results) > 0 {
		return &r.Results[0]
	}
	return r.Report
}

// Standing is the newest check not undone, and whether it holds the newest result.
// A check whose result can no longer be read (its transcript is gone, so the result it named is not among what the
// store holds) and that nothing came after still holds: nothing reopened it.
func (r *Request) Standing() (store.RequestCheck, bool, bool) {
	for i := len(r.Checks) - 1; i >= 0; i-- {
		if c := r.Checks[i]; c.Undone.IsZero() {
			n := r.Newest()
			return c, true, n != nil && n.Key == c.Result || r.unreadable(c)
		}
	}
	return store.RequestCheck{}, false, false
}

// unreadable reports a check whose result is no longer among the request's and that no result came after.
func (r *Request) unreadable(c store.RequestCheck) bool {
	for _, x := range r.Results {
		if x.Key == c.Result || x.At.After(c.At) {
			return false
		}
	}
	if r.Report != nil && (r.Report.Key == c.Result || r.Report.At.After(c.At)) {
		return false
	}
	return true
}

// CheckIDs are the ids its checks are stored under.
func (r *Request) CheckIDs() []string {
	ids := []string{r.ID}
	for _, c := range r.Checks {
		if !contains(ids, c.Request) {
			ids = append(ids, c.Request)
		}
	}
	return ids
}

// Options are the rules' configuration: the prefixes of the turns the machine's programs type
// (detect.program_prefixes), and what masks secrets in text read whole from a transcript (default tacit's
// redaction, scrump's text profile).
type Options struct {
	Prefixes []string
	Redact   func(context.Context, []string) ([]string, error)
}

// Rules' spans.
const (
	twinSpan = 2 * time.Second // the same words in another session this close, at another instant: a program sent them (detect.Programs)
	sameTurn = 2 * time.Minute // the same words again in a thread this soon, nothing of the agent's between: one turn recorded twice
)

// markerRe finds a handoff's id line, `momentum request ID`; idRe is an id alone.
var (
	markerRe = regexp.MustCompile(`\bmomentum request (r[0-9a-z]{6,12}[0-9a-f]{4})\b`)
	idRe     = regexp.MustCompile(`^r[0-9a-z]{6,12}[0-9a-f]{4}$`)
)

// ID is a request's id for an opening turn at at with words: r, the time in milliseconds in base 36, and four hex
// digits of a hash of the words.
func ID(at time.Time, words string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(words)))
	return timePart(at) + hex.EncodeToString(h[:2])
}

// timePart is an id's start: r and the time in base 36.
func timePart(at time.Time) string { return "r" + strconv.FormatInt(at.UnixMilli(), 36) }

// ParseID is the opening's time an id names; ok is false for a text that is no request id.
func ParseID(id string) (time.Time, bool) {
	if !idRe.MatchString(id) {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(id[1:len(id)-4], 36, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// marker is the request id a text names in a handoff's line, and the text with the pasted handoff taken out: from
// the first id line to the end of the last, or to the end of a text the store cut (it ends with an ellipsis) when
// only one line is left.
func marker(text string) (id, rest string) {
	locs := markerRe.FindAllStringSubmatchIndex(text, -1)
	for _, l := range locs {
		if c := text[l[2]:l[3]]; validID(c) {
			id = c
			break
		}
	}
	if id == "" {
		return "", text
	}
	start, end := locs[0][0], locs[len(locs)-1][1]
	if len(locs) == 1 && strings.HasSuffix(text, "…") {
		end = len(text)
	}
	return id, strings.TrimSpace(strings.TrimSpace(text[:start]) + " " + strings.TrimSpace(text[end:]))
}

func validID(id string) bool {
	_, ok := ParseID(id)
	return ok
}

// linkWhys are tacit's reasons for user-role text that is not the owner's own words but may carry his handoff:
// what it cut from his turn (a paste, agent text he relayed), and the brief an agent started a session with (the
// session's verdict). Another agent's report to its parent ("message from another agent"), a scheduled run's
// prompt and the harness's text are not among them.
var (
	linkWhys        = []string{"cut from the owner's turn", "agent text in the owner's turn", "codex exec", "pi headless", "agent cwd"}
	linkWhyPrefixes = []string{"entrypoint ", "originator ", "codex thread_source ", "hermes session from "}
)

func linkable(why string) bool {
	if contains(linkWhys, why) {
		return true
	}
	for _, p := range linkWhyPrefixes {
		if strings.HasPrefix(why, p) {
			return true
		}
	}
	return false
}

// meta is what a request reads of an event's meta.
type meta struct {
	ToolID  string `json:"tool_id"`
	IsError bool   `json:"is_error"`
	Why     string `json:"why"`
	Parent  string `json:"parent"`
	Ask     string `json:"ask"`
	AskKind string `json:"ask_kind"`
	To      string `json:"to"`
}

func metaOf(s string) meta {
	var m meta
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

// turn is a main-thread turn a request reads: the owner's, or one whose user-role text pasted a handoff.
type turn struct {
	ev      store.Event
	at      time.Time
	words   string // his words, outside a pasted handoff
	owner   bool
	marker  string
	program bool
	copies  []ref // the same turn in other threads
}

// ref is a moment in a thread.
type ref struct {
	thread string
	at     time.Time
}

// seg is a segment of a thread: from at (the zero time for the first) to the next one, of a handoff's request when
// marker is set.
type seg struct {
	at     time.Time
	marker string
}

// item is a result or a reply, placed in a segment by when it happened: a subagent's, a fork's or a resume's too,
// whose copies of earlier events keep their times.
type item struct {
	r Result
}

// thread is what one session holds for requests.
type thread struct {
	id      string
	harness string
	cwd     string
	first   time.Time
	last    time.Time
	parent  string
	namedBy string               // the session that names it as one of its agents, when it has no main thread of its own
	agents  map[string]time.Time // a subagent's first event
	turns   []*turn
	segs    []seg
	items   []item
	pending []pair // calls the store cut: read whole when a request holds them (whole)
	replies []item
	calls   map[string]store.Event // calls waiting for their results
	done    map[string]bool        // the tool ids of the calls read, each once
}

// segAt is the segment of an event at at, by its own time, whichever agent of the thread recorded it.
func (t *thread) segAt(at time.Time) int {
	k := 0
	for i := 1; i < len(t.segs); i++ {
		if !t.segs[i].at.After(at) {
			k = i
		}
	}
	return k
}

// repliedBetween reports a reply of the agent's in the thread after from and up to to.
func (t *thread) repliedBetween(from, to time.Time) bool {
	for _, r := range t.replies {
		if r.r.At.After(from) && !r.r.At.After(to) {
			return true
		}
	}
	return false
}

func (t *thread) add(e store.Event) {
	main := e.Agent == "" || e.Agent == t.id
	if t.harness == "" {
		t.harness = e.Harness
	}
	if main && e.Cwd != "" {
		t.cwd = e.Cwd
	}
	m := metaOf(e.Meta)
	switch e.Kind {
	case "owner_text":
		if main && e.Actor == store.ActorOwner {
			t.addTurn(e, true)
		}
	case "agent_text":
		switch {
		case !main:
		case m.Why == "": // the agent's own reply
			t.replies = append(t.replies, item{r: Result{Kind: KindReport, Key: KindReport + ":" + moment(e.TS) + "@" + short4(e.Summary),
				Text: e.Summary, At: e.TS, Thread: t.id, Event: e.ID()}})
		case linkable(m.Why):
			if id, _ := marker(e.Summary); id != "" {
				t.addTurn(e, false)
			}
		}
	case "tool_call":
		if m.ToolID != "" {
			t.calls[m.ToolID] = e
		}
	case "tool_result":
		call, ok := t.calls[m.ToolID]
		if !ok || m.ToolID == "" {
			return
		}
		delete(t.calls, m.ToolID)
		// A call is one by its session and tool id: a harness that records it again later (Hermes, a Codex
		// subagent forked with its parent's context) made nothing more, and not in the segment of the copy.
		if t.done[m.ToolID] {
			return
		}
		t.done[m.ToolID] = true
		if storeCut(call, e) {
			t.pending = append(t.pending, pair{call: call, res: e})
			return
		}
		for _, r := range Artifacts(call, e) {
			r.Thread = t.id
			t.items = append(t.items, item{r: r})
		}
	}
}

// addTurn adds a turn, or joins it to the turn of the same record before it (his words and the paste tacit cut
// from them share their turn's time).
func (t *thread) addTurn(e store.Event, owner bool) {
	id, words := marker(e.Summary)
	if n := len(t.turns); n > 0 && t.turns[n-1].at.Equal(e.TS) {
		p := t.turns[n-1]
		if p.marker == "" {
			p.marker = id
		}
		if owner && !p.owner {
			p.owner, p.words, p.ev = true, words, e
		}
		return
	}
	tn := &turn{ev: e, at: e.TS, owner: owner, marker: id}
	if owner {
		tn.words = words
	}
	t.turns = append(t.turns, tn)
}

// moment is a time to the millisecond, as a key's part: a fork carries its events over with their times.
func moment(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }

func short4(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:2])
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// reader groups the threads it reads. It reads each thread once, whole; links in both directions reach the
// threads a request holds: a thread's own (its parent, its agents' threads, its turns' copies, the openings its
// handoffs name), and, from the index it keeps since a time, the threads that name it (as their parent, or its
// openings in their handoffs).
type reader struct {
	st       *store.Store
	opt      Options
	threads  map[string]*thread
	queue    []string
	openings map[string][]ref // an id's opening turns
	single   map[string]bool  // a time part one opening has, alone
	indexed  time.Time        // the index holds what came since
	children map[string][]string
	markers  map[string][]string   // the sessions whose turns name an id, by the id's time part
	markIDs  map[string][]string   // the ids of a time part the markers name
	records  map[string]recordRead // raw records a batch read, each let go of by the job that takes it
	packed   map[string]bool       // a transcript read from a compressed copy
	opens    map[string]int        // how often a transcript file was opened in the run
	hermes   map[string]*evidence.HermesDB
	resolved map[string]resolvedPair // a cut call's artifacts, read whole once per run
}

func newReader(st *store.Store, opt Options) *reader {
	return &reader{st: st, opt: opt, threads: map[string]*thread{}, openings: map[string][]ref{}, single: map[string]bool{},
		children: map[string][]string{}, markers: map[string][]string{}, markIDs: map[string][]string{},
		records: map[string]recordRead{}, packed: map[string]bool{}, opens: map[string]int{}, hermes: map[string]*evidence.HermesDB{},
		resolved: map[string]resolvedPair{}}
}

// close lets go of the Hermes stores a run opened.
func (r *reader) close() {
	for _, db := range r.hermes {
		if db != nil {
			db.Close()
		}
	}
}

func (r *reader) enqueue(id string) {
	if id == "" {
		return
	}
	if _, ok := r.threads[id]; !ok {
		r.threads[id] = nil
		r.queue = append(r.queue, id)
	}
}

// read is what a thread's read selects: the main thread's text, the file writers' calls and results, and the
// shells' calls that may run git or gh, or that the store cut (their git may be past the cut), with their results
// that may show an artifact's line (a commit's "] ", a ref update's " -> ", a pull request's "/pull/") or that the
// store cut.
var read = store.ThreadRead{Writers: sortedKeys(writers), Shells: shells(), CallMarks: []string{"git", "gh"},
	ResultMarks: []string{"] ", " -> ", "/pull/"}, CutMark: "…"}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// load reads a thread, then follows its links.
func (r *reader) load(ctx context.Context, id string) error {
	t := &thread{id: id, agents: map[string]time.Time{}, calls: map[string]store.Event{}, done: map[string]bool{}, segs: []seg{{}}}
	spans, err := r.st.ThreadSpans(ctx, id)
	if err != nil {
		return err
	}
	for i, sp := range spans {
		if i == 0 || sp.First.Before(t.first) {
			t.first = sp.First
		}
		if sp.Last.After(t.last) {
			t.last = sp.Last
		}
		if sp.Agent != "" && sp.Agent != id {
			t.agents[sp.Agent] = sp.First
		}
	}
	r.threads[id] = t
	if len(spans) == 0 {
		return nil
	}
	main := false
	for _, sp := range spans {
		main = main || sp.Agent == "" || sp.Agent == id
	}
	if !main {
		// A session that holds only its subagents' events (a Codex subagent's own subagents): the session that
		// names it as an agent holds its own work.
		if t.namedBy, err = r.st.AgentParent(ctx, id, t.last); err != nil {
			return err
		}
	}
	pm, err := r.st.ThreadParentMeta(ctx, id)
	if err != nil {
		return err
	}
	if p := metaOf(pm).Parent; p != id {
		t.parent = p
	}
	if err := r.st.ThreadEvents(ctx, id, read, func(e store.Event) error { t.add(e); return nil }); err != nil {
		return err
	}
	t.calls, t.done = nil, nil
	for _, tn := range t.turns {
		if tn.marker != "" {
			t.segs = append(t.segs, seg{at: tn.at, marker: tn.marker})
		}
		if tn.owner {
			if err := r.near(ctx, t, tn); err != nil {
				return err
			}
		}
	}
	return r.follow(ctx, t)
}

// follow enqueues the threads a thread's links reach.
func (r *reader) follow(ctx context.Context, t *thread) error {
	if t == nil || t.first.IsZero() {
		return nil
	}
	r.enqueue(t.parent)
	r.enqueue(t.namedBy)
	for a := range t.agents {
		r.enqueue(a)
	}
	for _, c := range r.children[t.id] {
		r.enqueue(c)
	}
	for _, tn := range t.turns {
		for _, c := range tn.copies {
			r.enqueue(c.thread)
		}
		if tn.marker != "" {
			os, err := r.openingsOf(ctx, tn.marker)
			if err != nil {
				return err
			}
			for _, o := range os {
				r.enqueue(o.thread)
			}
		}
		if tn.owner && tn.words != "" && tn.marker == "" {
			id := ID(tn.at, tn.words)
			for _, m := range r.markIDs[timePart(tn.at)] {
				ok, err := r.names(ctx, m, id)
				if err != nil {
					return err
				}
				if ok {
					for _, s := range r.markers[m] {
						r.enqueue(s)
					}
				}
			}
		}
	}
	return nil
}

// names reports a handoff's id naming an opening's: the same id, or the same time while one opening has it.
func (r *reader) names(ctx context.Context, m, id string) (bool, error) {
	if m == id {
		return true, nil
	}
	if m[:len(m)-4] != id[:len(id)-4] {
		return false, nil
	}
	return r.alone(ctx, m)
}

// alone reports one opening alone having the time an id names: then the id names it, whatever words a read again
// stored for it.
func (r *reader) alone(ctx context.Context, id string) (bool, error) {
	tp := id[:len(id)-4]
	if v, ok := r.single[tp]; ok {
		return v, nil
	}
	at, ok := ParseID(id)
	if !ok {
		return false, nil
	}
	evs, err := r.st.OwnerTurnsBetween(ctx, at, at)
	if err != nil {
		return false, err
	}
	var ids []string
	for _, e := range evs {
		if w := strings.TrimSpace(e.Summary); w != "" && !contains(ids, ID(e.TS, w)) {
			ids = append(ids, ID(e.TS, w))
		}
	}
	r.single[tp] = len(ids) == 1
	return len(ids) == 1, nil
}

// near finds a turn's copies (the same words at the same millisecond in another thread) and its twins (the same
// words in another thread within twinSpan, at another instant: a program sent them), and marks a turn a configured
// prefix opens as a program's. A program's turn has no copies: what it typed into two sessions joins nothing.
func (r *reader) near(ctx context.Context, t *thread, tn *turn) error {
	w := strings.TrimSpace(tn.ev.Summary)
	if strings.HasPrefix(w, supervise.Marker) {
		tn.program = true // momentum's own message, typed into the session
	}
	for _, p := range r.opt.Prefixes {
		if p != "" && strings.HasPrefix(w, p) {
			tn.program = true
		}
	}
	if w == "" {
		return nil
	}
	evs, err := r.st.OwnerTurnsBetween(ctx, tn.at.Add(-twinSpan), tn.at.Add(twinSpan))
	if err != nil {
		return err
	}
	var copies []ref
	for _, e := range evs {
		if e.Session == t.id || strings.TrimSpace(e.Summary) != w {
			continue
		}
		if e.TS.Equal(tn.at) {
			copies = append(copies, ref{e.Session, e.TS})
		} else {
			tn.program = true
		}
	}
	if !tn.program {
		tn.copies = copies
	}
	return nil
}

// openingsOf are the opening turns an id names: those whose time and words make it, else, while one opening alone
// has its time, that one (a read again may store other words for it).
func (r *reader) openingsOf(ctx context.Context, id string) ([]ref, error) {
	if os, ok := r.openings[id]; ok {
		return os, nil
	}
	r.openings[id] = nil
	at, ok := ParseID(id)
	if !ok {
		return nil, nil
	}
	evs, err := r.st.OwnerTurnsBetween(ctx, at, at)
	if err != nil {
		return nil, err
	}
	var exact, any []ref
	for _, e := range evs {
		if strings.TrimSpace(e.Summary) == "" {
			continue
		}
		any = append(any, ref{e.Session, e.TS})
		if ID(e.TS, e.Summary) == id {
			exact = append(exact, ref{e.Session, e.TS})
		}
	}
	os := exact
	if len(os) == 0 && len(any) > 0 {
		if alone, err := r.alone(ctx, id); err != nil {
			return nil, err
		} else if alone {
			os = any
		}
	}
	r.openings[id] = os
	return os, nil
}

// index reads the links that name a thread from elsewhere since a time: the sessions that continue another, and
// the turns that paste a handoff. A wider index follows every thread read so far again.
func (r *reader) index(ctx context.Context, since time.Time) error {
	if !r.indexed.IsZero() && !since.Before(r.indexed) {
		return nil
	}
	kids, err := r.st.Children(ctx, since)
	if err != nil {
		return err
	}
	r.children = map[string][]string{}
	for _, c := range kids {
		if p := metaOf(c.Meta).Parent; p != "" && p != c.Session {
			r.children[p] = append(r.children[p], c.Session)
		}
	}
	texts, err := r.st.TextsHolding(ctx, since, "momentum request r")
	if err != nil {
		return err
	}
	r.markers, r.markIDs = map[string][]string{}, map[string][]string{}
	for _, e := range texts {
		m := metaOf(e.Meta)
		if !(e.Kind == "owner_text" && e.Actor == store.ActorOwner || e.Kind == "agent_text" && linkable(m.Why)) {
			continue
		}
		id, _ := marker(e.Summary)
		if id == "" {
			continue
		}
		if !contains(r.markers[id], e.Session) {
			r.markers[id] = append(r.markers[id], e.Session)
		}
		if tp := id[:len(id)-4]; !contains(r.markIDs[tp], id) {
			r.markIDs[tp] = append(r.markIDs[tp], id)
		}
	}
	r.indexed = since
	for _, t := range r.threads {
		if err := r.follow(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// run reads every thread the queue holds, and what their links reach.
func (r *reader) run(ctx context.Context) error {
	for len(r.queue) > 0 {
		id := r.queue[0]
		r.queue = r.queue[1:]
		if err := r.load(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// settle reads from since, and reads again from earlier while a request match picks began before the index does:
// what a request holds does not depend on where it was found from. It returns the requests match picks.
func (r *reader) settle(ctx context.Context, since time.Time, match func(*Request) bool) ([]*Request, error) {
	from := since
	for {
		if err := r.index(ctx, from); err != nil {
			return nil, err
		}
		if err := r.run(ctx); err != nil {
			return nil, err
		}
		var out []*Request
		start := from
		for _, rq := range r.requests() {
			if match(rq) {
				out = append(out, rq)
				if s := rq.start(); s.Before(start) {
					start = s
				}
			}
		}
		if !start.Before(from) {
			if err := r.resolve(ctx, out); err != nil {
				return nil, err
			}
			for _, rq := range out {
				fold(rq)
			}
			return out, r.finish(ctx, out)
		}
		from = start
	}
}
