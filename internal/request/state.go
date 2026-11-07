package request

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// live phases: a thread in one runs, or waits on the owner.
var livePhases = map[string]bool{"working": true, "background": true, "scheduled": true}

// state decides a request's state and its open step from what was observed, the first that holds: a thread waits
// on the owner, a thread works, a standing check holds the newest result, the latest thread's phase was never
// recorded, a result came after his last turn, nothing did.
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
	_, standing, holds := rq.Standing()
	switch {
	case needs != nil:
		rq.State = NeedsYou
	case working:
		rq.State = Working
	case standing && holds:
		rq.State = Checked
	case latest == nil || latest.Phase == "":
		rq.State = Unknown
	case after:
		rq.State = Ready
	default:
		rq.State = Open
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
			if a := rq.Asks[i]; a.At.Equal(rq.LastTurn) {
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

// Find is the request an id names, or, for a session's id or its start, the request its thread is in now. It
// groups the threads with events since the request opened (since the thread's first event), as a list does.
func Find(ctx context.Context, st *store.Store, id string, opt Options) (*Request, error) {
	if at, ok := ParseID(id); ok {
		reqs, err := Group(ctx, st, at, opt)
		if err != nil {
			return nil, err
		}
		for _, rq := range reqs {
			if rq.ID == id {
				return rq, nil
			}
			for _, a := range rq.Aliases {
				if a == id {
					return rq, nil
				}
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
	reqs, err := Group(ctx, st, first, opt)
	if err != nil {
		return nil, err
	}
	for _, rq := range reqs {
		for _, th := range rq.Threads {
			if th.ID == threads[0] && th.Current {
				return rq, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: session %s holds none: no turn of the owner's opens one in it, and no handoff or link joins it to one",
		ErrNoRequest, threads[0])
}

// List is the requests with events since a time, newest first.
func List(ctx context.Context, st *store.Store, since time.Time, opt Options) ([]*Request, error) {
	reqs, err := Group(ctx, st, since, opt)
	if err != nil {
		return nil, err
	}
	out := reqs[:0]
	for _, rq := range reqs {
		if !rq.Last.Before(since) {
			out = append(out, rq)
		}
	}
	return out, nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
