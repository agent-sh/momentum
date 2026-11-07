package request

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// live phases: a thread in one runs.
var livePhases = map[string]bool{"working": true, "background": true, "scheduled": true}

// state decides a request's state, what it rests on, and its open step, from what was observed, the first that
// holds: a thread waits on the owner, a thread works, a standing check holds the newest result, the latest
// thread's phase is not known (never recorded, recorded only before the last turn there, or every thread moved
// on), a result came after his last turn, nothing did. A phase counts only when a sessions run saw it after the
// request's last turn in the thread.
func state(rq *Request) {
	var latest, needs *Thread
	working := false
	for i := range rq.Threads {
		th := &rq.Threads[i]
		if !th.Current {
			continue
		}
		if latest == nil || th.Last.After(latest.Last) {
			latest = th
		}
		if !th.Trusted {
			continue
		}
		if th.Phase == NeedsYou && needs == nil {
			needs = th
		}
		working = working || livePhases[th.Phase]
	}
	after := false
	for _, x := range rq.Results {
		after = after || x.At.After(rq.LastTurn)
	}
	if rq.Report != nil && rq.Report.At.After(rq.LastTurn) {
		after = true
	}
	c, standing, holds := rq.Standing()
	switch {
	case needs != nil:
		rq.State, rq.Why = NeedsYou, "a session of it was last seen waiting on the owner"
	case working:
		rq.State, rq.Why = Working, "a session of it was last seen working"
	case standing && holds:
		rq.State, rq.Why = Checked, "the owner checked its newest result at "+c.At.Local().Format(timeFormat)+"; that he checked it does not make it right"
	case latest == nil:
		rq.State, rq.Why = Unknown, "every session of it moved on to another request, so nothing records whether it is done"
	case latest.Phase == "":
		rq.State, rq.Why = Unknown, "no sessions run recorded the latest session's phase, so whether it still runs is not known"
	case !latest.Trusted:
		rq.State, rq.Why = Unknown, "the latest session's last recorded phase is from before the last turn in it, so whether it still runs is not known"
	case after:
		rq.State, rq.Why = Ready, "a result came after the last turn"
	default:
		rq.State, rq.Why = Open, "nothing came after the last turn"
	}
	if (rq.State == Ready || rq.State == Open) && standing && !holds {
		rq.Why += "; the check of " + c.At.Local().Format(timeFormat) + " is reopened by a newer result"
	}
	switch {
	case needs != nil:
		kind := needs.AskKind
		if kind == "" {
			kind = "a question"
		}
		rq.Open = fmt.Sprintf("session %s waits on the owner (%s): %s", short(needs.ID), kind, needs.Ask)
	case !after && !rq.LastTurn.IsZero():
		for i := len(rq.Asks) - 1; i >= 0; i-- {
			if a := rq.Asks[i]; !a.At.After(rq.LastTurn) {
				if a.Words != "" {
					rq.Open = "the last turn has no reply recorded: " + a.Words
				} else {
					rq.Open = "the handoff pasted at " + a.At.Local().Format(timeFormat) + " has no reply recorded"
				}
				break
			}
		}
	}
}

// ErrNoRequest is an id that names no request in the store.
var ErrNoRequest = errors.New("no request")

// Find is the request an id names, or, for a session's id or its start, the request its thread is in now. It reads
// the request's own threads: those that hold its opening or paste its handoff, and every thread a link reaches
// from them, with the links that name them since the request began. A request found from any of its threads holds
// the same.
func Find(ctx context.Context, st *store.Store, id string, opt Options) (*Request, error) {
	if at, ok := ParseID(id); ok {
		r := newReader(st, opt)
		os, err := r.openingsOf(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, o := range os {
			r.enqueue(o.thread)
		}
		if err := r.index(ctx, at); err != nil {
			return nil, err
		}
		for _, m := range r.markIDs[id[:len(id)-4]] {
			if ok, err := r.names(ctx, m, id); err != nil {
				return nil, err
			} else if ok {
				for _, s := range r.markers[m] {
					r.enqueue(s)
				}
			}
		}
		if len(r.queue) > 0 {
			reqs, err := r.settle(ctx, at, func(rq *Request) bool { return r.idOf(ctx, rq, id) })
			if err != nil {
				return nil, err
			}
			if len(reqs) > 0 {
				return reqs[0], nil
			}
		}
	}
	threads, err := st.ThreadsByPrefix(ctx, id)
	if err != nil {
		return nil, err
	}
	switch len(threads) {
	case 0:
		return nil, fmt.Errorf("%w: %s is neither a request's id nor a session's", ErrNoRequest, id)
	case 2:
		return nil, fmt.Errorf("%w: %s starts more than one session's id", ErrNoRequest, id)
	}
	first, _, err := st.ThreadFirst(ctx, threads[0])
	if err != nil {
		return nil, err
	}
	r := newReader(st, opt)
	r.enqueue(threads[0])
	reqs, err := r.settle(ctx, first, func(rq *Request) bool {
		for _, th := range rq.Threads {
			if th.ID == threads[0] && th.Current {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("%w: session %s holds none: no turn of the owner's opens one in it, and no handoff or link joins it to one",
			ErrNoRequest, threads[0])
	}
	return reqs[0], nil
}

// idOf reports an id naming a request: its own, one of its aliases, or one of their times while one opening alone
// has it.
func (r *reader) idOf(ctx context.Context, rq *Request, id string) bool {
	for _, x := range append([]string{rq.ID}, rq.Aliases...) {
		if ok, _ := r.names(ctx, id, x); ok {
			return true
		}
	}
	return false
}

// List is the requests with events since a time, newest first. It reads the threads with events since then, and
// every thread a link reaches from them, with the links that name them since the earliest of those requests
// began: each holds what it holds when found by its id.
func List(ctx context.Context, st *store.Store, since time.Time, opt Options) ([]*Request, error) {
	seeds, err := st.ThreadsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	r := newReader(st, opt)
	for _, s := range seeds {
		r.enqueue(s)
	}
	return r.settle(ctx, since, func(rq *Request) bool { return !rq.Last.Before(since) })
}

// Group is List: the requests the threads with events since a time hold.
func Group(ctx context.Context, st *store.Store, since time.Time, opt Options) ([]*Request, error) {
	return List(ctx, st, since, opt)
}

// Check records the owner's check of a request's newest result; it reports false when that check stands already,
// also when another check recorded it since the request was read.
func Check(ctx context.Context, st *store.Store, rq *Request, at time.Time) (bool, error) {
	n := rq.Newest()
	if n == nil {
		return false, fmt.Errorf("%w: nothing of %s to check: no result and no reply of it is recorded", ErrNoRequest, rq.ID)
	}
	if _, standing, holds := rq.Standing(); standing && holds {
		return false, nil
	}
	return st.CheckOnce(ctx, store.RequestCheck{Request: rq.ID, Result: n.Key, At: at}, rq.CheckIDs())
}

// Uncheck undoes the request's standing check, whichever of its ids it is stored under.
func Uncheck(ctx context.Context, st *store.Store, rq *Request, at time.Time) (store.RequestCheck, error) {
	return st.UndoRequestCheck(ctx, rq.CheckIDs(), at)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
