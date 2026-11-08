package request

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// A check is the owner's. He does not run tools: an agent relays the words he typed, as tacit's relays do (`tacit
// questions close --proof words`), and a check is recorded only when those words are in a turn of his in the
// request's sessions, at or after the result it binds. An agent's word alone records nothing (SPEC, Requests,
// Checks).

// ProofWords is the fewest words a proof holds: fewer are found in a turn by chance.
const ProofWords = 3

// ErrNoProof is a check or an undo the owner's own words do not prove.
var ErrNoProof = errors.New("not on the owner's words")

// spoken is an owner turn of a request: his words outside a pasted handoff, when, its event, and whether a
// program typed it (a configured prefix, momentum's own message, the same words sent to another session) or it is a
// harness command.
type spoken struct {
	at      time.Time
	event   string
	words   string
	program bool
	command bool
}

// Proof is the owner's turn a check or an undo rests on: its event and its time.
type Proof struct {
	Event string
	At    time.Time
}

// holdsWords reports a text holding words as whole words, both with their whitespace collapsed, case kept.
func holdsWords(text, words string) bool {
	w := strings.Join(strings.Fields(words), " ")
	return w != "" && strings.Contains(" "+strings.Join(strings.Fields(text), " ")+" ", " "+w+" ")
}

// proofShape refuses a proof that is no words, or fewer than ProofWords.
func proofShape(words string) error {
	switch n := len(strings.Fields(words)); {
	case n == 0:
		return fmt.Errorf(`%w: no proof; relay the owner's own words with --proof "WORDS", at least %d of them as he typed them: an agent's word alone records nothing`,
			ErrNoProof, ProofWords)
	case n < ProofWords:
		return fmt.Errorf("%w: a proof is at least %d of his words as he typed them, and %q is %d", ErrNoProof, ProofWords, words, n)
	}
	return nil
}

// prove is the owner's newest turn of the request that holds words and came at or after from (after it, when
// strictly). With none, it says why: no turn holds them, only a program's or a harness command does, or only a turn
// of his from before, which early says what of.
func (r *Request) prove(words string, from time.Time, strictly bool, early string) (Proof, error) {
	var found *spoken
	var program, command bool
	var before time.Time
	for i := range r.said {
		s := &r.said[i]
		if !holdsWords(s.words, words) {
			continue
		}
		switch {
		case s.program:
			program = true
		case s.command:
			command = true
		case s.at.Before(from) || strictly && s.at.Equal(from):
			if s.at.After(before) {
				before = s.at
			}
		case found == nil || s.at.After(found.at) || s.at.Equal(found.at) && s.event < found.event:
			found = s
		}
	}
	switch {
	case found != nil:
		return Proof{Event: found.event, At: found.at}, nil
	case !before.IsZero():
		return Proof{}, fmt.Errorf("%w: his turn of %s holds these words, but %s", ErrNoProof, before.Local().Format(timeFormat), early)
	case program:
		return Proof{}, fmt.Errorf("%w: only a turn a program typed holds these words (momentum's own message, a configured prefix, or the same words sent to another session), not one of his",
			ErrNoProof)
	case command:
		return Proof{}, fmt.Errorf("%w: only a harness command of his holds these words, which asks the agent nothing", ErrNoProof)
	}
	return Proof{}, fmt.Errorf("%w: no turn of the owner's in the request's sessions holds these words as he typed them", ErrNoProof)
}

// CheckOutcome is what a check did: whether it recorded one (none when a check on his same turn holds already), the
// result it binds, and the owner's turn it rests on.
type CheckOutcome struct {
	Added  bool
	Result Result
	Proof  Proof
}

// Check records the owner's check of the result a request binds (Bound), on his words: they must be in a turn of
// his at or after that result, and no ask of his after them, which would reopen it. It records none when a check of
// that result on the same turn of his stands, also one another check recorded since the request was read: a check
// that holds rests on his last ask, the one turn such words can come from.
func Check(ctx context.Context, st *store.Store, rq *Request, words string, at time.Time) (CheckOutcome, error) {
	if err := proofShape(words); err != nil {
		return CheckOutcome{}, err
	}
	b := rq.Bound()
	if b == nil {
		return CheckOutcome{}, fmt.Errorf("%w: nothing of %s to check: no result and no reply of it is recorded", ErrNoRequest, rq.ID)
	}
	p, err := rq.prove(words, b.At, false, "it came before the result a check binds ("+plain.Clip(plain.Line(b.Text), 120)+", "+
		b.At.Local().Format(timeFormat)+"), which he had not seen then")
	if err != nil {
		return CheckOutcome{}, err
	}
	if rq.LastOwnerTurn.After(p.At) {
		return CheckOutcome{}, fmt.Errorf("%w: his turn of %s holds these words, but he asked more at %s, which reopens a check on them; relay words of his from then on",
			ErrNoProof, p.At.Local().Format(timeFormat), rq.LastOwnerTurn.Local().Format(timeFormat))
	}
	out := CheckOutcome{Result: *b, Proof: p}
	out.Added, err = st.CheckOnce(ctx, store.RequestCheck{Request: rq.ID, Result: b.Key, ResultAt: b.At, Proof: p.Event, ProofAt: p.At, At: at},
		rq.CheckIDs())
	return out, err
}

// Uncheck undoes the request's standing check, whichever of its ids it is stored under, on the owner's words: they
// must be in a turn of his after the one the check rests on.
func Uncheck(ctx context.Context, st *store.Store, rq *Request, words string, at time.Time) (store.RequestCheck, Proof, error) {
	if err := proofShape(words); err != nil {
		return store.RequestCheck{}, Proof{}, err
	}
	c, standing, _ := rq.Standing()
	if !standing {
		return store.RequestCheck{}, Proof{}, store.ErrNoCheck
	}
	p, err := rq.prove(words, c.ProofAt, true, "it is not after his turn of "+c.ProofAt.Local().Format(timeFormat)+" that the check rests on")
	if err != nil {
		return store.RequestCheck{}, Proof{}, err
	}
	undone, err := st.UndoRequestCheck(ctx, rq.CheckIDs(), at, p.Event)
	return undone, p, err
}
