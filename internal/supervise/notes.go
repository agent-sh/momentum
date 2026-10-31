package supervise

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
)

// The ids of supervision's notes, by the session's key: its problem when it gives up on a session
// (store.SessionProblem), and the question a session waits on its owner for.
const (
	ProblemNote = "supervise:"
	AskNote     = "supervise-ask:"
)

// seenSession is what a run made of one session, for its notes. A nil one is a session the run left alone.
type seenSession struct {
	s        *sessions.Session
	stop     Stop
	presence time.Time
	ask      ask
	asks     bool
}

// ask is what a session waits on that only its owner can answer: the needs note's title is the question itself.
type ask struct {
	title, detail string
	since         time.Time // when it began to wait: the note's time, so the same ask is the same note
	due           time.Time // when the note is written: the owner window after it began, or after he was last at it
}

// waitsOnOwner is what a session waits on that only its owner can answer, if it does: a question, a plan or a
// permission it asks him; a login or an organization block no sign of access has ended yet; an error no rule
// tries again, or one whose stop is too old for its rule; or a turn over and idle on words that ask him
// something. d is what the rules decided: a rule that still acts takes care of the session. An automated run is
// no one's to answer, and a Hermes conversation on a messaging platform asked him there already.
func waitsOnOwner(s *sessions.Session, stop Stop, d Decision, presence time.Time, p Params, stalledLeft bool) (ask, bool) {
	if s.Auto || stop.At.IsZero() || s.Tool == "hermes" && s.Where != "cli" && s.Where != "tui" {
		return ask{}, false
	}
	var title string
	wait, since := p.OwnerWindow, stop.At
	switch {
	case stop.Ask == sessions.AskQuestion, stop.Ask == sessions.AskPermission:
		title = s.Ask
	case stop.Ask == sessions.AskPlan:
		title = "Approve the plan? " + s.Ask
	case stalledLeft: // an ask per request: Claude Code's own retry starts the wait again
		title = "Resume it? Its request to the model from " + stop.RequestAt.Local().Format("Jan 2 15:04") + " never got a reply"
		since = stop.RequestAt
	case s.TurnOpen:
		return ask{}, false
	case d.Rule == RuleAccess && d.Verdict == Wait && d.At.IsZero():
		switch ErrorClass(stop.Error) {
		case Login:
			title = "Log in again: " + stop.Error
		case Org:
			title = "Access is off: " + stop.Error
		default: // a limit that states no reset lifts by itself: a note only once it outlasted the stale hour
			title, wait = "A usage limit: "+stop.Error, p.Stale
		}
	case d.Stale && stop.Error != "":
		title = "It stopped on an error: " + stop.Error
	case d.Rule != "":
		return ask{}, false
	case s.Phase == sessions.NeedsYou && stop.Error != "" && ErrorClass(stop.Error) == "":
		title = "It stopped on an error: " + stop.Error
	case s.Phase == sessions.Idle && AsksOwner(stop.Words):
		title = firstNonEmpty(Question(s.LastText), Asked(stop.Words))
	default:
		return ask{}, false
	}
	if strings.TrimSpace(title) == "" {
		return ask{}, false
	}
	due := since
	if presence.After(due) {
		due = presence
	}
	return ask{title: title, since: since, due: due.Add(wait),
		detail: fmt.Sprintf("%s (%s) has waited for you since %s. momentum does not answer for you.", sessionName(s), s.Tool,
			since.Local().Format("Jan 2 15:04"))}, true
}

// notes keeps supervision's notes in step with what the run saw. A session that has waited on its owner past the
// owner window gets a needs note, once per ask: a note he resolved while the session still waits on the same ask
// stays resolved. A needs note is done when its session moves on: the ask is answered or another took its place,
// or the session ended. A problem note is done when its owner came back to the session or it works again: a turn
// open, or a stop since without an error. A session the run left alone keeps its notes as they are.
func (sv *Supervisor) notes(ctx context.Context, seen map[string]*seenSession) error {
	ns, err := sv.Store.Notes(ctx)
	if err != nil {
		return err
	}
	now := sv.now()
	held := map[string]store.Note{}
	for _, n := range ns {
		held[n.ID] = n
	}
	done := func(id string) error {
		if _, _, err := sv.Store.DoneNote(ctx, id, now); err != nil && !errors.Is(err, store.ErrNoNote) {
			return err
		}
		return nil
	}
	for _, n := range ns {
		if !n.Done.IsZero() {
			continue
		}
		switch {
		case strings.HasPrefix(n.ID, AskNote):
			e, ok := seen[strings.TrimPrefix(n.ID, AskNote)]
			if ok && e == nil {
				continue
			}
			if !ok || !e.asks || e.ask.since.UnixMilli() != n.At.UnixMilli() {
				if err := done(n.ID); err != nil {
					return err
				}
			}
		case strings.HasPrefix(n.ID, ProblemNote):
			e, ok := seen[strings.TrimPrefix(n.ID, ProblemNote)]
			if ok && e == nil {
				continue
			}
			// it works again: a turn open now, but not one that still waits on a request from before the note, or
			// one that stopped since without an error
			stuck := ok && !e.stop.RequestAt.IsZero() && !e.stop.RequestAt.After(n.At)
			again := ok && (e.s.TurnOpen && !stuck || e.stop.At.After(n.At) && e.stop.Error == "")
			if !ok || e.presence.After(n.At) || again {
				if err := done(n.ID); err != nil {
					return err
				}
			}
		}
	}
	for key, e := range seen {
		if e == nil || !e.asks || now.Before(e.ask.due) {
			continue
		}
		id := AskNote + key
		if n, ok := held[id]; ok && n.At.UnixMilli() == e.ask.since.UnixMilli() {
			continue // written already; standing, or resolved by its owner
		}
		n := store.Note{ID: id, Kind: store.NoteNeeds, Title: e.ask.title, Detail: e.ask.detail, Source: store.DefaultNoteSource, At: e.ask.since}
		if _, err := sv.Store.PutNote(ctx, n); err != nil && !errors.Is(err, store.ErrBadNote) {
			return err
		}
	}
	return nil
}

// sessionName is how a note names a session: its name, else its directory, else its key.
func sessionName(s *sessions.Session) string {
	dir := ""
	if s.Cwd != "" {
		dir = filepath.Base(s.Cwd)
	}
	return firstNonEmpty(s.Name, dir, s.Key)
}
