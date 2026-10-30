// Package state is what the session layer keeps between runs: when each session entered its phase, and which
// asks its owner waved away. It only computes; the caller stores it (momentum keeps it in its SQLite store), so
// a caller that runs once and exits, such as a CLI on a timer, keeps phase times across runs.
package state

import (
	"time"

	"github.com/agent-sh/momentum/sessions"
)

// Stamp is when a session entered its current phase, how long its activity log was then, and when a scan last
// saw it.
type Stamp struct {
	Phase sessions.Phase `json:"phase"`
	Since time.Time      `json:"since"`
	Seq   int            `json:"seq"`
	Seen  time.Time      `json:"seen"`
}

// Stamps maps a session key to its stamp.
type Stamps map[string]Stamp

// Keep is how long a stamp outlives its session's last sighting. A session back within it, in the same phase,
// keeps its time: the same window a source keeps a departed session's tracker.
const Keep = 15 * time.Minute

// Apply sets every session's Since and updates the stamps, then drops stamps unseen for longer than Keep. It
// takes the phases a scan decided, before verdict.Apply settles idle sessions: a stamp of a reviewed phase would
// start a new idle stretch on the next scan, and the review of the last one would never apply again.
//
// A session starts a new stretch when it is seen for the first time, when its phase changed, or when it did
// something (its log grew) while in a phase other than working: an idle session that ran a whole turn between
// two scans is idle again, but not since before that turn. A new stretch of working began now. Any other phase
// began when the session last moved, if that was after the previous sighting, since a turn ends with its last
// write and the scan that notices comes later; otherwise it began now.
func (st Stamps) Apply(ss []*sessions.Session, now time.Time) {
	for _, s := range ss {
		prev, ok := st[s.Key]
		moved := ok && s.Phase != sessions.Working && s.Seq != prev.Seq
		if !ok || prev.Phase != s.Phase || moved {
			at := now
			if s.Phase != sessions.Working && !s.LastActive.IsZero() && !s.LastActive.After(now) &&
				(!ok || s.LastActive.After(prev.Seen)) {
				at = s.LastActive
			}
			prev = Stamp{Phase: s.Phase, Since: at}
		}
		prev.Seq, prev.Seen = s.Seq, now
		st[s.Key] = prev
		s.Since = prev.Since
	}
	for k, r := range st {
		if now.Sub(r.Seen) > Keep {
			delete(st, k)
		}
	}
}

// Dismissals maps a session key to the length of its activity log when its owner waved its ask away. The
// dismissal holds until the session does something new.
type Dismissals map[string]int

// Covers reports whether a dismissal holds for a session whose log is at seq.
func (d Dismissals) Covers(key string, seq int) bool {
	at, ok := d[key]
	return ok && at == seq
}
