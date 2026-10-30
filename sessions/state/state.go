// Package state is what the session layer keeps between runs: when each session entered its phase, and which
// asks its owner waved away. It only computes; the caller stores it (momentum keeps it in its SQLite store), so
// a caller that runs once and exits, such as a CLI on a timer, keeps phase times across runs.
package state

import (
	"sort"
	"time"

	"github.com/agent-sh/momentum/sessions"
)

// Stamp is when a session entered its current phase, how long its activity log was then, and when a scan last
// saw it. It also keeps what a later run needs to report the session once it is gone: its tool, its name, whether
// it is an automated run, and what a needs_you stretch waits on, as the stretch began.
type Stamp struct {
	Phase   sessions.Phase   `json:"phase"`
	Since   time.Time        `json:"since"`
	Seq     int              `json:"seq"`
	Seen    time.Time        `json:"seen"`
	Tool    string           `json:"tool,omitempty"`
	Name    string           `json:"name,omitempty"`
	Auto    bool             `json:"auto,omitempty"`
	Ask     string           `json:"ask,omitempty"`
	AskKind sessions.AskKind `json:"ask_kind,omitempty"`
}

// Stamps maps a session key to its stamp.
type Stamps map[string]Stamp

// Keep is how long a stamp outlives its session's last sighting. A session back within it, in the same phase,
// keeps its time: the same window a source keeps a departed session's tracker.
const Keep = 15 * time.Minute

// Change is a session moving from one stretch to the next, as a run sees it: into another phase, into a new
// stretch of the same phase (it did something between two runs), seen for the first time, or gone.
type Change struct {
	Key  string
	Tool string
	Name string
	Auto bool
	From sessions.Phase // "" for a session seen for the first time
	To   sessions.Phase // "" for a session gone for longer than Keep
	// At is when the new stretch began, as Apply dates it; for a session gone, when a scan last saw it.
	At time.Time
	// Since is when the old stretch began, and Seen when a scan last saw it; zero without one. For is how long it
	// lasted, up to At: it lasted at least until Seen.
	Since time.Time
	Seen  time.Time
	For   time.Duration
	// What the old stretch waited on, when it was needs_you, and what the new one waits on.
	FromAsk, ToAsk         string
	FromAskKind, ToAskKind sessions.AskKind
}

// Apply sets every session's Since and updates the stamps, then drops stamps unseen for longer than Keep. It
// returns what changed, oldest first. It takes the phases a scan decided, before verdict.Apply settles idle
// sessions: a stamp of a reviewed phase would start a new idle stretch on the next scan, and the review of the
// last one would never apply again.
//
// A session starts a new stretch when it is seen for the first time, when its phase changed, or when it did
// something (its log grew) while in a phase other than working: an idle session that ran a whole turn between
// two scans is idle again, but not since before that turn. A new stretch of working began now. Any other phase
// began when the session last moved, if that was after the previous sighting, since a turn ends with its last
// write and the scan that notices comes later; otherwise it began now.
//
// A stamp that a scan later than now already updated (a run that started earlier and saved later) is left as it
// is: an older observation changes nothing.
func (st Stamps) Apply(ss []*sessions.Session, now time.Time) []Change {
	var out []Change
	for _, s := range ss {
		prev, ok := st[s.Key]
		if ok && prev.Seen.After(now) {
			s.Since = prev.Since
			continue
		}
		tool, name := s.Tool, s.Name // a scan that lost them does not erase them
		if tool == "" {
			tool = prev.Tool
		}
		if name == "" {
			name = prev.Name
		}
		moved := ok && s.Phase != sessions.Working && s.Seq != prev.Seq
		if !ok || prev.Phase != s.Phase || moved {
			at := now
			if s.Phase != sessions.Working && !s.LastActive.IsZero() && !s.LastActive.After(now) &&
				(!ok || s.LastActive.After(prev.Seen)) {
				at = s.LastActive
			}
			c := Change{Key: s.Key, Tool: tool, Name: name, Auto: s.Auto, To: s.Phase, At: at}
			if ok {
				c.From, c.Since, c.Seen, c.For = prev.Phase, prev.Since, prev.Seen, max(0, at.Sub(prev.Since))
				c.FromAsk, c.FromAskKind = prev.Ask, prev.AskKind
			}
			prev = Stamp{Phase: s.Phase, Since: at}
			if s.Phase == sessions.NeedsYou {
				c.ToAsk, c.ToAskKind = s.Ask, s.AskKind
				prev.Ask, prev.AskKind = s.Ask, s.AskKind
			}
			out = append(out, c)
		}
		prev.Seq, prev.Seen, prev.Tool, prev.Name, prev.Auto = s.Seq, now, tool, name, s.Auto
		st[s.Key] = prev
		s.Since = prev.Since
	}
	for k, r := range st {
		if now.Sub(r.Seen) > Keep {
			out = append(out, Change{Key: k, Tool: r.Tool, Name: r.Name, Auto: r.Auto, From: r.Phase, At: r.Seen, Since: r.Since,
				Seen: r.Seen, For: max(0, r.Seen.Sub(r.Since)), FromAsk: r.Ask, FromAskKind: r.AskKind})
			delete(st, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Dismissals maps a session key to the length of its activity log when its owner waved its ask away. The
// dismissal holds until the session does something new.
type Dismissals map[string]int

// Covers reports whether a dismissal holds for a session whose log is at seq.
func (d Dismissals) Covers(key string, seq int) bool {
	at, ok := d[key]
	return ok && at == seq
}
