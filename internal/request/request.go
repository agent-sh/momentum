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
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
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
// names it for a check.
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
// empty when no sessions run recorded one. Current is true when its latest segment is this request's: only then
// does its phase count for it.
type Thread struct {
	ID          string
	Harness     string
	Cwd         string
	First, Last time.Time
	Phase       string
	PhaseAt     time.Time
	Ask         string
	AskKind     string
	Current     bool
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
	Open     string // the open step; empty when nothing records one
	Last     time.Time
}

// Newest is the result a check binds: the newest artifact, else the report; nil when there is neither.
func (r *Request) Newest() *Result {
	if len(r.Results) > 0 {
		return &r.Results[0]
	}
	return r.Report
}

// Standing is the newest check not undone, and whether it holds the newest result.
func (r *Request) Standing() (store.RequestCheck, bool, bool) {
	for i := len(r.Checks) - 1; i >= 0; i-- {
		if c := r.Checks[i]; c.Undone.IsZero() {
			n := r.Newest()
			return c, true, n != nil && n.Key == c.Result
		}
	}
	return store.RequestCheck{}, false, false
}

// Options are the rules' configuration: the prefixes of the turns the machine's programs type
// (detect.program_prefixes).
type Options struct {
	Prefixes []string
}

// Rules' spans.
const (
	twinSpan = 2 * time.Second // the same words in another session this close, at another instant: a program sent them (detect.Programs)
	sameTurn = 2 * time.Minute // the same words again in a thread this soon: one turn recorded twice (detect.Programs)
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
	return "r" + strconv.FormatInt(at.UnixMilli(), 36) + hex.EncodeToString(h[:2])
}

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

// item is a result or a reply, placed in a segment by when it happened, or by its subagent's first event.
type item struct {
	r     Result
	agent string
}

// thread is what one session holds for requests.
type thread struct {
	id      string
	harness string
	cwd     string
	first   time.Time
	last    time.Time
	parent  string
	agents  map[string]time.Time // a subagent's first event
	turns   []*turn
	segs    []seg
	items   []item
	replies []item
	calls   map[string]store.Event // file-writing and pull-request calls waiting for their results
}

// segAt is the segment of an event at at, of agent (empty for the main thread).
func (t *thread) segAt(at time.Time, agent string) int {
	if a, ok := t.agents[agent]; ok && agent != "" {
		at = a
	}
	k := 0
	for i := 1; i < len(t.segs); i++ {
		if !t.segs[i].at.After(at) {
			k = i
		}
	}
	return k
}

func (t *thread) add(e store.Event) {
	if t.first.IsZero() || e.TS.Before(t.first) {
		t.first = e.TS
	}
	if e.TS.After(t.last) {
		t.last = e.TS
	}
	if t.harness == "" {
		t.harness = e.Harness
	}
	m := metaOf(e.Meta)
	sub := e.Agent != "" && e.Agent != t.id
	if sub {
		if _, ok := t.agents[e.Agent]; !ok {
			t.agents[e.Agent] = e.TS
		}
	} else {
		if e.Cwd != "" {
			t.cwd = e.Cwd
		}
		if m.Parent != "" && m.Parent != t.id && t.parent == "" {
			t.parent = m.Parent
		}
	}
	switch e.Kind {
	case "owner_text":
		if !sub && e.Actor == store.ActorOwner {
			t.addTurn(e, true)
		}
	case "agent_text":
		switch {
		case sub:
		case m.Why == "": // the agent's own reply
			t.replies = append(t.replies, item{r: Result{Kind: KindReport, Key: KindReport + ":" + e.ID(), Text: e.Summary, At: e.TS,
				Thread: t.id, Event: e.ID()}})
		default: // user-role text that is not his: a paste cut from his turn, a brief another agent sent
			if id, _ := marker(e.Summary); id != "" {
				t.addTurn(e, false)
			}
		}
	case "tool_call":
		if m.ToolID != "" && (writers[e.Tool] || strings.Contains(e.Summary, "gh pr create")) {
			t.calls[m.ToolID] = e
		}
	case "tool_result":
		t.result(e, m)
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

// writers are the file-writing tools of the harnesses the readers name.
var writers = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true, "apply_patch": true,
	"write": true, "edit": true, "write_file": true, "patch": true}

var (
	// git's commit line: [BRANCH HASH] SUBJECT, the branch maybe "detached HEAD", a first commit "(root-commit)"
	commitRe = regexp.MustCompile(`\[([^\[\]]+?)(?: \(root-commit\))? ([0-9a-f]{7,40})\] (.+?)(?: \d+ files? changed|$)`)
	// git push's ref updates after its To line: OLD..NEW SRC -> DST, or [new branch] SRC -> DST
	pushToRe  = regexp.MustCompile(`\bTo (\S+)`)
	pushRefRe = regexp.MustCompile(`(?:\b([0-9a-f]{7,40})\.\.\.?([0-9a-f]{7,40})|\[new branch\])\s+(\S+) -> (\S+)`)
	prRe      = regexp.MustCompile(`https?://\S+/pull/\d+`)
	pathRe    = regexp.MustCompile(`"(?:file_path|filePath|notebook_path|path)":("(?:[^"\\]|\\.)*")`)
	patchRe   = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: (\S+)`)
)

// result reads an artifact off a successful tool result.
func (t *thread) result(e store.Event, m meta) {
	call, paired := t.calls[m.ToolID]
	if paired {
		delete(t.calls, m.ToolID)
	}
	if m.IsError || e.Summary == "" {
		return
	}
	add := func(r Result) {
		r.At, r.Thread = e.TS, t.id
		if r.Event == "" {
			r.Event = e.ID()
		}
		t.items = append(t.items, item{r: r, agent: e.Agent})
	}
	if paired && writers[call.Tool] {
		for _, p := range writtenPaths(call.Summary) {
			add(Result{Kind: KindFile, Key: KindFile + ":" + p + "@" + call.ID(), Text: p, Event: call.ID()})
		}
	}
	if paired && strings.Contains(call.Summary, "gh pr create") {
		if u := prRe.FindString(e.Summary); u != "" {
			add(Result{Kind: KindPR, Key: KindPR + ":" + u, Text: "pull request " + u})
		}
	}
	if c := commitRe.FindStringSubmatch(e.Summary); c != nil {
		add(Result{Kind: KindCommit, Key: KindCommit + ":" + c[2], Text: fmt.Sprintf("commit %s on %s: %s", c[2], c[1], strings.TrimSuffix(c[3], "…"))})
	}
	if to := pushToRe.FindStringSubmatchIndex(e.Summary); to != nil {
		remote := e.Summary[to[2]:to[3]]
		for _, p := range pushRefRe.FindAllStringSubmatch(e.Summary[to[1]:], -1) {
			if p[2] != "" {
				add(Result{Kind: KindPush, Key: KindPush + ":" + remote + " " + p[4] + "@" + p[2],
					Text: fmt.Sprintf("pushed %s -> %s (%s..%s) to %s", p[3], p[4], p[1], p[2], remote)})
			} else {
				add(Result{Kind: KindPush, Key: KindPush + ":" + remote + " " + p[4] + "@" + e.ID(),
					Text: fmt.Sprintf("pushed new branch %s -> %s to %s", p[3], p[4], remote)})
			}
		}
	}
}

// writtenPaths are the files a file-writing call names: its path field, or a patch's files.
func writtenPaths(input string) []string {
	var out []string
	for _, m := range pathRe.FindAllStringSubmatch(input, -1) {
		if p, err := strconv.Unquote(m[1]); err == nil && p != "" {
			out = append(out, p)
		}
	}
	if len(out) > 0 {
		return out[:1]
	}
	for _, m := range patchRe.FindAllStringSubmatch(input, -1) {
		out = append(out, m[1])
	}
	return out
}

// reader groups the threads it reads.
type reader struct {
	st       *store.Store
	opt      Options
	threads  map[string]*thread
	queue    []string
	openings map[string][]ref // a handoff id's opening turns
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

// load reads a thread, then what its links reach.
func (r *reader) load(ctx context.Context, id string) error {
	t := &thread{id: id, agents: map[string]time.Time{}, calls: map[string]store.Event{}, segs: []seg{{}}}
	if err := r.st.ThreadEvents(ctx, id, func(e store.Event) error { t.add(e); return nil }); err != nil {
		return err
	}
	t.calls = nil
	if t.first.IsZero() {
		r.threads[id] = t
		return nil
	}
	for _, tn := range t.turns {
		if tn.marker != "" {
			t.segs = append(t.segs, seg{at: tn.at, marker: tn.marker})
		}
	}
	r.threads[id] = t
	r.enqueue(t.parent)
	for a := range t.agents {
		r.enqueue(a)
	}
	for _, tn := range t.turns {
		if tn.owner {
			if err := r.near(ctx, t, tn); err != nil {
				return err
			}
		}
		if tn.marker != "" {
			if err := r.resolve(ctx, tn.marker); err != nil {
				return err
			}
		}
	}
	return nil
}

// near finds a turn's copies (the same words at the same millisecond in another thread) and its twins (the same
// words in another thread within twinSpan, at another instant: a program sent them), and marks a turn a configured
// prefix opens as a program's.
func (r *reader) near(ctx context.Context, t *thread, tn *turn) error {
	w := strings.TrimSpace(tn.ev.Summary)
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
	for _, e := range evs {
		if e.Session == t.id || strings.TrimSpace(e.Summary) != w {
			continue
		}
		if e.TS.Equal(tn.at) {
			tn.copies = append(tn.copies, ref{e.Session, e.TS})
			r.enqueue(e.Session)
		} else {
			tn.program = true
		}
	}
	return nil
}

// resolve finds the opening turns a handoff's id names, by the time it holds and the hash of their words.
func (r *reader) resolve(ctx context.Context, id string) error {
	if _, ok := r.openings[id]; ok {
		return nil
	}
	r.openings[id] = nil
	at, ok := ParseID(id)
	if !ok {
		return nil
	}
	evs, err := r.st.OwnerTurnsBetween(ctx, at, at)
	if err != nil {
		return err
	}
	for _, e := range evs {
		if ID(e.TS, e.Summary) == id {
			r.openings[id] = append(r.openings[id], ref{e.Session, e.TS})
			r.enqueue(e.Session)
		}
	}
	return nil
}

// Group reads the threads with events since a time, and every thread a link from them reaches, and groups them
// into requests.
func Group(ctx context.Context, st *store.Store, since time.Time, opt Options) ([]*Request, error) {
	seeds, err := st.ThreadsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	r := &reader{st: st, opt: opt, threads: map[string]*thread{}, openings: map[string][]ref{}}
	for _, s := range seeds {
		r.enqueue(s)
	}
	for len(r.queue) > 0 {
		id := r.queue[0]
		r.queue = r.queue[1:]
		if err := r.load(ctx, id); err != nil {
			return nil, err
		}
	}
	return r.group(ctx)
}

// union-find over segments ("THREAD#K") and handoff ids ("id:ID").
type sets map[string]string

func (s sets) find(x string) string {
	if _, ok := s[x]; !ok {
		s[x] = x
	}
	for s[x] != x {
		s[x] = s[s[x]]
		x = s[x]
	}
	return x
}

func (s sets) union(a, b string) {
	if ra, rb := s.find(a), s.find(b); ra != rb {
		if rb < ra {
			ra, rb = rb, ra
		}
		s[rb] = ra
	}
}

func node(t string, k int) string { return t + "#" + strconv.Itoa(k) }

// opening is a base segment's first turn of his that a program did not type and that pasted no handoff.
type opening struct {
	id string
	at time.Time
}

func (r *reader) group(ctx context.Context) ([]*Request, error) {
	ids := make([]string, 0, len(r.threads))
	for id, t := range r.threads {
		if t != nil && !t.first.IsZero() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	u := sets{}
	at := func(id string, when time.Time) (string, bool) {
		o := r.threads[id]
		if o == nil || o.first.IsZero() {
			return "", false
		}
		return node(id, o.segAt(when, "")), true
	}
	for _, id := range ids {
		t := r.threads[id]
		for k := range t.segs {
			u.find(node(id, k))
		}
		if n, ok := at(t.parent, t.first); ok {
			u.union(node(id, 0), n)
		}
		for a, first := range t.agents {
			if c := r.threads[a]; c != nil && !c.first.IsZero() {
				u.union(node(a, 0), node(id, t.segAt(first, "")))
			}
		}
		for _, tn := range t.turns {
			k := t.segAt(tn.at, "")
			for _, c := range tn.copies {
				if n, ok := at(c.thread, c.at); ok {
					u.union(node(id, k), n)
				}
			}
		}
		for k := 1; k < len(t.segs); k++ {
			m := t.segs[k].marker
			u.union(node(id, k), "id:"+m)
			for _, o := range r.openings[m] {
				if n, ok := at(o.thread, o.at); ok {
					u.union(node(id, k), n)
				}
			}
		}
	}
	// Each set's openings and handoff ids name it.
	openings := map[string][]opening{}
	marks := map[string][]string{}
	for _, id := range ids {
		t := r.threads[id]
		for _, tn := range t.turns {
			if tn.marker != "" {
				break
			}
			if tn.owner && !tn.program && tn.words != "" {
				root := u.find(node(id, 0))
				openings[root] = append(openings[root], opening{ID(tn.at, tn.words), tn.at})
				break
			}
		}
	}
	for x := range u {
		if strings.HasPrefix(x, "id:") {
			root := u.find(x)
			marks[root] = append(marks[root], strings.TrimPrefix(x, "id:"))
		}
	}
	reqs := map[string]*Request{}
	var out []*Request
	for root := range u {
		root = u.find(root)
		if _, ok := reqs[root]; ok {
			continue
		}
		os, ms := openings[root], marks[root]
		sort.Slice(os, func(i, j int) bool {
			if !os[i].at.Equal(os[j].at) {
				return os[i].at.Before(os[j].at)
			}
			return os[i].id < os[j].id
		})
		sort.Strings(ms)
		rq := &Request{}
		switch {
		case len(os) > 0:
			rq.ID, rq.Opened = os[0].id, os[0].at
		case len(ms) > 0:
			rq.ID = ms[0]
		default:
			reqs[root] = nil
			continue
		}
		seen := map[string]bool{rq.ID: true}
		for _, o := range os {
			if !seen[o.id] {
				seen[o.id] = true
				rq.Aliases = append(rq.Aliases, o.id)
			}
		}
		for _, m := range ms {
			if !seen[m] {
				seen[m] = true
				rq.Aliases = append(rq.Aliases, m)
			}
		}
		reqs[root] = rq
		out = append(out, rq)
	}
	for _, id := range ids {
		r.fill(u, reqs, r.threads[id])
	}
	for _, rq := range out {
		if err := r.finish(ctx, rq); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// fill adds a thread's turns, results and replies to the requests its segments are in.
func (r *reader) fill(u sets, reqs map[string]*Request, t *thread) {
	of := func(k int) *Request { return reqs[u.find(node(t.id, k))] }
	for k := range t.segs {
		rq := of(k)
		if rq == nil {
			continue
		}
		from, to := t.first, t.last
		if k > 0 && t.segs[k].at.After(from) {
			from = t.segs[k].at
		}
		if k+1 < len(t.segs) && t.segs[k+1].at.Before(to) {
			to = t.segs[k+1].at
		}
		th := Thread{ID: t.id, Harness: t.harness, Cwd: t.cwd, First: from, Last: to, Current: k == len(t.segs)-1}
		merged := false
		for i := range rq.Threads {
			if rq.Threads[i].ID == t.id {
				p := &rq.Threads[i]
				if from.Before(p.First) {
					p.First = from
				}
				if to.After(p.Last) {
					p.Last = to
				}
				p.Current = p.Current || th.Current
				merged = true
			}
		}
		if !merged {
			rq.Threads = append(rq.Threads, th)
		}
		if to.After(rq.Last) {
			rq.Last = to
		}
	}
	for _, tn := range t.turns {
		rq := of(t.segAt(tn.at, ""))
		if rq == nil || tn.program && tn.marker == "" {
			continue
		}
		if tn.at.After(rq.LastTurn) {
			rq.LastTurn = tn.at
		}
		words := tn.words
		if tn.program {
			words = "" // a program's words are not his, though the handoff it pasted links
		}
		if words != "" || tn.marker != "" {
			rq.Asks = append(rq.Asks, Ask{At: tn.at, Thread: t.id, Harness: t.harness, Words: words, Event: tn.ev.ID(), Handoff: tn.marker})
		}
	}
	for _, it := range t.items {
		if rq := of(t.segAt(it.r.At, it.agent)); rq != nil {
			rq.Results = append(rq.Results, it.r)
		}
	}
	for _, it := range t.replies {
		if rq := of(t.segAt(it.r.At, "")); rq != nil && (rq.Report == nil || !it.r.At.Before(rq.Report.At)) {
			rp := it.r
			rq.Report = &rp
		}
	}
}

// finish orders what a request holds, folds its copies and files, and reads its threads' phases, its checks and
// its state.
func (r *reader) finish(ctx context.Context, rq *Request) error {
	sort.SliceStable(rq.Asks, func(i, j int) bool { return rq.Asks[i].At.Before(rq.Asks[j].At) })
	var asks []Ask
	for _, a := range rq.Asks {
		dup := false
		for i := len(asks) - 1; i >= 0 && a.At.Sub(asks[i].At) <= sameTurn; i-- {
			if asks[i].Words == a.Words && asks[i].Handoff == a.Handoff && (asks[i].At.Equal(a.At) || asks[i].Thread == a.Thread) {
				dup = true
				break
			}
		}
		if !dup {
			asks = append(asks, a)
		}
	}
	rq.Asks = asks
	// One result per key, the earliest seen; a file once, by its newest write, with its count of writes.
	sort.SliceStable(rq.Results, func(i, j int) bool { return rq.Results[i].At.Before(rq.Results[j].At) })
	keys := map[string]bool{}
	writes := map[string]int{}
	newest := map[string]string{} // a file's newest write, by its key
	var res []Result
	for _, x := range rq.Results {
		if keys[x.Key] {
			continue
		}
		keys[x.Key] = true
		if x.Kind == KindFile {
			writes[x.Text]++
			newest[x.Text] = x.Key
		}
		res = append(res, x)
	}
	rq.Results = rq.Results[:0]
	for _, x := range res {
		if x.Kind == KindFile {
			if newest[x.Text] != x.Key {
				continue
			}
			x.Writes = writes[x.Text]
		}
		rq.Results = append(rq.Results, x)
	}
	// Newest first; results of one moment keep the order they were recorded in.
	sort.SliceStable(rq.Results, func(i, j int) bool { return rq.Results[i].At.After(rq.Results[j].At) })
	sort.Slice(rq.Threads, func(i, j int) bool { return rq.Threads[i].First.Before(rq.Threads[j].First) })
	for i := range rq.Threads {
		th := &rq.Threads[i]
		if !th.Current {
			continue
		}
		e, ok, err := r.st.LastPhase(ctx, th.ID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		m := metaOf(e.Meta)
		th.PhaseAt = e.TS
		switch {
		case e.Kind == "vanished":
			th.Phase = "gone"
		case m.To != "":
			th.Phase, th.Ask, th.AskKind = m.To, m.Ask, m.AskKind
		}
	}
	checks, err := r.st.RequestChecks(ctx, append([]string{rq.ID}, rq.Aliases...))
	if err != nil {
		return err
	}
	rq.Checks = checks
	state(rq)
	return nil
}
