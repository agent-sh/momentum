package request

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
	stamp "github.com/agent-sh/momentum/sessions/state"
)

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

// requests groups the threads read so far: linked segments are one request.
func (r *reader) requests() []*Request {
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
		return node(id, o.segAt(when)), true
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
				u.union(node(a, 0), node(id, t.segAt(first)))
			}
		}
		for _, tn := range t.turns {
			k := t.segAt(tn.at)
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
	var keys []string
	for x := range u {
		keys = append(keys, x)
	}
	sort.Strings(keys)
	for _, x := range keys {
		if strings.HasPrefix(x, "id:") {
			root := u.find(x)
			marks[root] = append(marks[root], strings.TrimPrefix(x, "id:"))
		}
	}
	reqs := map[string]*Request{}
	var out []*Request
	for _, x := range keys {
		root := u.find(x)
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
		for _, o := range os {
			if o.id != rq.ID && !contains(rq.Aliases, o.id) {
				rq.Aliases = append(rq.Aliases, o.id)
			}
		}
		for _, m := range ms {
			if m != rq.ID && !contains(rq.Aliases, m) {
				rq.Aliases = append(rq.Aliases, m)
			}
		}
		reqs[root] = rq
		out = append(out, rq)
	}
	for _, id := range ids {
		fill(u, reqs, r.threads[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// fill adds a thread's turns, results and replies to the requests its segments are in, from each request's start
// on: what came before it in a thread is another request's, or no one's.
func fill(u sets, reqs map[string]*Request, t *thread) {
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
		if s := rq.start(); s.After(from) {
			from = s
		}
		if k+1 < len(t.segs) && t.segs[k+1].at.Before(to) {
			to = t.segs[k+1].at
		}
		if to.Before(from) {
			to = from
		}
		th := Thread{ID: t.id, Harness: t.harness, Cwd: t.cwd, First: from, Last: to, Current: k == len(t.segs)-1}
		merged := false
		for i := range rq.Threads {
			if p := &rq.Threads[i]; p.ID == t.id {
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
	thread := func(rq *Request) *Thread {
		for i := range rq.Threads {
			if rq.Threads[i].ID == t.id {
				return &rq.Threads[i]
			}
		}
		return nil
	}
	var prev *Ask
	var prevRq *Request
	for _, tn := range t.turns {
		rq := of(t.segAt(tn.at))
		if rq != prevRq {
			prev, prevRq = nil, rq
		}
		if rq == nil || tn.program && tn.marker == "" || tn.at.Before(rq.start()) {
			continue
		}
		if tn.at.After(rq.LastTurn) {
			rq.LastTurn = tn.at
		}
		if th := thread(rq); th != nil && tn.at.After(th.turn) {
			th.turn = tn.at
		}
		words := tn.words
		if tn.program {
			words = "" // a program's words are not his, though the handoff it pasted links
		}
		if words == "" && tn.marker == "" {
			continue
		}
		// The same words again soon, with no reply between, are one turn the transcript holds twice.
		if prev != nil && prev.Words == words && prev.Handoff == tn.marker && tn.at.Sub(prev.At) <= sameTurn && !t.repliedBetween(prev.At, tn.at) {
			continue
		}
		rq.Asks = append(rq.Asks, Ask{At: tn.at, Thread: t.id, Harness: t.harness, Words: words, Event: tn.ev.ID(), Handoff: tn.marker})
		a := rq.Asks[len(rq.Asks)-1]
		prev = &a
	}
	for _, it := range t.items {
		if rq := of(t.segAt(it.r.At)); rq != nil && !it.r.At.Before(rq.start()) {
			rq.Results = append(rq.Results, it.r)
		}
	}
	for _, p := range t.pending {
		if rq := of(t.segAt(p.res.TS)); rq != nil && !p.res.TS.Before(rq.start()) {
			rq.pending = append(rq.pending, p)
		}
	}
	for _, it := range t.replies {
		if rq := of(t.segAt(it.r.At)); rq != nil && !it.r.At.Before(rq.start()) && (rq.Report == nil || !it.r.At.Before(rq.Report.At)) {
			rp := it.r
			rq.Report = &rp
		}
	}
}

// fold orders what a request holds: a turn a fork carried over counts once, a result once by its key, a file once,
// by its newest write, with its count of writes.
func fold(rq *Request) {
	sort.SliceStable(rq.Asks, func(i, j int) bool { return rq.Asks[i].At.Before(rq.Asks[j].At) })
	var asks []Ask
	for _, a := range rq.Asks {
		dup := false
		for i := len(asks) - 1; i >= 0 && asks[i].At.Equal(a.At); i-- {
			if asks[i].Words == a.Words && asks[i].Handoff == a.Handoff {
				dup = true
			}
		}
		if !dup {
			asks = append(asks, a)
		}
	}
	rq.Asks = asks
	sort.SliceStable(rq.Results, func(i, j int) bool { return rq.Results[i].At.Before(rq.Results[j].At) })
	keys := map[string]bool{}
	writes := map[string]int{}
	newest := map[string]string{}
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
	sort.SliceStable(rq.Threads, func(i, j int) bool {
		if !rq.Threads[i].First.Equal(rq.Threads[j].First) {
			return rq.Threads[i].First.Before(rq.Threads[j].First)
		}
		return rq.Threads[i].ID < rq.Threads[j].ID
	})
}

// finish reads the requests' threads' phases, their checks, and their state.
func (r *reader) finish(ctx context.Context, reqs []*Request) error {
	current := false
	for _, rq := range reqs {
		for i := range rq.Threads {
			if th := &rq.Threads[i]; th.Current {
				if th.turn.IsZero() {
					th.turn = th.First
				}
				current = true
			}
		}
	}
	// Each thread's newest phase, whenever it was seen: what a read finds does not depend on where it began. A
	// session still in that phase was last seen in it when its stamp says (the runs that see no change write no
	// event).
	phases := map[string]store.Event{}
	var stamps stamp.Stamps
	if current {
		evs, err := r.st.PhasesSince(ctx, time.Time{})
		if err != nil {
			return err
		}
		phases = phasesBySession(evs)
		if stamps, err = r.st.SessionStamps(ctx); err != nil {
			return err
		}
	}
	for _, rq := range reqs {
		for i := range rq.Threads {
			th := &rq.Threads[i]
			e, ok := phases[th.ID]
			if !th.Current || !ok {
				continue
			}
			m := metaOf(e.Meta)
			seen := e.TS
			if sp, ok := stamps[e.Session]; ok && e.Kind != "vanished" && string(sp.Phase) == m.To && sp.Since.UnixMilli() == e.TS.UnixMilli() && sp.Seen.After(seen) {
				seen = sp.Seen // the same stretch, seen since
			}
			th.PhaseAt, th.Trusted = e.TS, seen.After(th.turn) // seen after the last turn there
			switch {
			case e.Kind == "vanished":
				th.Phase = "gone"
			case m.To != "":
				th.Phase, th.Ask, th.AskKind = m.To, m.Ask, m.AskKind
			}
		}
		ids := append([]string{rq.ID}, rq.Aliases...)
		var prefixes []string
		for _, id := range ids {
			alone, err := r.alone(ctx, id)
			if err != nil {
				return err
			}
			if tp := id[:len(id)-4]; alone && !contains(prefixes, tp) {
				prefixes = append(prefixes, tp)
			}
		}
		checks, err := r.st.RequestChecks(ctx, ids, prefixes, 4)
		if err != nil {
			return err
		}
		rq.Checks = checks
	}
	r.maskAsks(ctx, reqs)
	for _, rq := range reqs {
		state(rq)
	}
	return nil
}

// maskAsks masks the questions sessions wait on the owner with, as a sessions run recorded them, before the open
// step quotes one; one that cannot be masked is left out.
func (r *reader) maskAsks(ctx context.Context, reqs []*Request) {
	var asks []*string
	var texts []string
	for _, rq := range reqs {
		for i := range rq.Threads {
			if th := &rq.Threads[i]; th.Ask != "" {
				asks, texts = append(asks, &th.Ask), append(texts, th.Ask)
			}
		}
	}
	if len(texts) == 0 {
		return
	}
	masked, err := r.redact()(ctx, texts)
	for i, a := range asks {
		if err != nil || len(masked) != len(texts) {
			*a = ""
		} else {
			*a = masked[i]
		}
	}
}

// phasesBySession is each session's newest event of the sessions runs, by the id the transcripts give it: a
// sessions run keys a session by its harness's key (hermes:ID, opencode:ID, pi:NAME, a Claude Code background job
// by its job), which supervise.StoreSessionOf turns into that id, by the transcript its events name when they name
// one.
func phasesBySession(evs []store.Event) map[string]store.Event {
	byKey := map[string]string{}
	for _, e := range evs {
		if i := strings.LastIndexByte(e.Ref, '@'); i > 0 {
			byKey[e.Harness+"\x00"+e.Session] = supervise.StoreSessionOf(e.Harness, e.Session, e.Ref[:i])
		}
	}
	out := map[string]store.Event{}
	for _, e := range evs {
		id, ok := byKey[e.Harness+"\x00"+e.Session]
		if !ok {
			id = supervise.StoreSessionOf(e.Harness, e.Session, "")
		}
		out[id] = e
	}
	return out
}
