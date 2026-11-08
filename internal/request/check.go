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

// A check is the owner's. He does not run tools: an agent relays the turn where he says he checked it, as tacit's
// relays do (`tacit questions close --proof words`), and a check is recorded only when the relayed words are that
// whole turn of his in the request's sessions, after the result it binds, and the turn is no question and no
// instruction about a check. An agent's word alone records nothing (SPEC, Requests, Checks).

// ProofWords is the fewest words a proof holds: a shorter turn says too little to be a verdict.
const ProofWords = 3

var (
	// ErrNoProof is a check or an undo the owner's own turn does not prove.
	ErrNoProof = errors.New("not on the owner's words")
	// ErrNoTurn is a proof that is no turn of his the store holds; the store may not hold his newest turns yet.
	ErrNoTurn = errors.New("no turn of the owner's in the request's sessions, as the store holds them, is these words whole")
)

// spoken is an owner turn of a request: his words outside anything that may be a pasted handoff (ownWords), when,
// its event, and whether a program typed it (a configured prefix, momentum's own message, the same words sent to
// another session) or it is a harness command.
type spoken struct {
	at      time.Time
	event   string
	words   string
	program bool
	command bool
}

// Proof is the owner's turn a check or an undo rests on: its event, its time and its words.
type Proof struct {
	Event string
	At    time.Time
	Words string
}

// collapse is a text with its whitespace collapsed to single spaces.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// question reports a turn that asks: it ends in a question mark (before closing quotes or brackets).
func question(words string) bool {
	return strings.HasSuffix(strings.TrimRight(collapse(words), `"')]`+"`"), "?")
}

// instruction reports a turn that names the check command or a request's id: it tells an agent what to do about a
// check, which is no verdict of his.
func instruction(words string) bool {
	lower := strings.ToLower(collapse(words))
	for _, cmd := range []string{"request check", "request uncheck", "--proof"} {
		if strings.Contains(lower, cmd) {
			return true
		}
	}
	for _, w := range strings.Fields(words) {
		if validID(strings.Trim(w, ".,;:!?()[]{}<>\"'`")) {
			return true
		}
	}
	return false
}

// ProofShape refuses a proof that is no words, or fewer than ProofWords.
func ProofShape(words string) error {
	switch n := len(strings.Fields(words)); {
	case n == 0:
		return fmt.Errorf("%w: no proof; a check rests only on the owner's own turn where he says he checked it, relayed whole as he typed it: an agent's word alone records nothing",
			ErrNoProof)
	case n < ProofWords:
		return fmt.Errorf("%w: %q is fewer than %d words, too short to say he checked it", ErrNoProof, words, ProofWords)
	}
	return nil
}

// prove is the owner's newest turn of the request whose words, whitespace collapsed, are words whole, after from (at
// or after it unless strictly), that asks no question and gives no instruction about a check. With none, it says
// why: no turn is them, only a program's or a harness command is, or only a turn of his from before (early says
// what of), a question or an instruction.
func (r *Request) prove(words string, from time.Time, strictly bool, early string) (Proof, error) {
	want := collapse(words)
	var found *spoken
	var program, command bool
	var before, asks, tells time.Time
	for i := range r.said {
		s := &r.said[i]
		if collapse(s.words) != want {
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
		case question(s.words):
			asks = s.at
		case instruction(s.words):
			tells = s.at
		case found == nil || s.at.After(found.at) || s.at.Equal(found.at) && s.event < found.event:
			found = s
		}
	}
	turn := func(at time.Time) string { return "his turn of " + at.Local().Format(timeFormat) + " is these words" }
	switch {
	case found != nil:
		return Proof{Event: found.event, At: found.at, Words: want}, nil
	case !asks.IsZero():
		return Proof{}, fmt.Errorf("%w: %s, and it asks a question, which is no verdict", ErrNoProof, turn(asks))
	case !tells.IsZero():
		return Proof{}, fmt.Errorf("%w: %s, and it names the check command or a request's id: an instruction about a check is no verdict", ErrNoProof, turn(tells))
	case !before.IsZero():
		return Proof{}, fmt.Errorf("%w: %s, but %s", ErrNoProof, turn(before), early)
	case program:
		return Proof{}, fmt.Errorf("%w: only a turn a program typed is these words (momentum's own message, a configured prefix, or the same words sent to another session), not one of his",
			ErrNoProof)
	case command:
		return Proof{}, fmt.Errorf("%w: only a harness command of his is these words, which asks the agent nothing", ErrNoProof)
	}
	return Proof{}, fmt.Errorf("%w: %w", ErrNoProof, ErrNoTurn)
}

// lastUndo is the newest turn of the owner's that undid a check of the request; zero when none did.
func (r *Request) lastUndo() time.Time {
	var at time.Time
	for _, c := range r.Checks {
		if !c.Undone.IsZero() && c.Undo.ProofAt.After(at) {
			at = c.Undo.ProofAt
		}
	}
	return at
}

// CheckOutcome is what a check did: whether it recorded one (none when a check of the same result on his same turn
// stands already), the result it binds, and the owner's turn it rests on.
type CheckOutcome struct {
	Added  bool
	Result Result
	Proof  Proof
}

// Check records the owner's check of the result a request binds, on his turn: the relayed words must be a whole
// turn of his after that result and after his last undo, no question and no instruction, with no ask of his after
// it, which would reopen it. A request with no artifact binds the agent's newest reply before that turn. It
// records none when a check of that result on the same turn of his stands, also one another check recorded since
// the request was read: a check that holds rests on his last ask, the one turn it can rest on.
func Check(ctx context.Context, st *store.Store, rq *Request, words string, at time.Time) (CheckOutcome, error) {
	if err := ProofShape(words); err != nil {
		return CheckOutcome{}, err
	}
	b := rq.Bound()
	var from time.Time
	early := ""
	switch {
	case b != nil:
		from = b.At
		early = "it is not after the result a check binds (" + plain.Clip(plain.Line(b.Text), 120) + ", " + b.At.Local().Format(timeFormat) +
			"), which he had not seen then"
	case len(rq.replies) > 0:
		from = rq.replies[0].At
		early = "it is not after a reply of the agent's, which a check of a request with no artifact binds"
	default:
		return CheckOutcome{}, fmt.Errorf("%w: nothing of %s to check: no result and no reply of it is recorded", ErrNoRequest, rq.ID)
	}
	if u := rq.lastUndo(); !u.Before(from) {
		from = u
		early = "it is not after his turn of " + u.Local().Format(timeFormat) + " that undid his last check"
	}
	p, err := rq.prove(words, from, true, early)
	if err != nil {
		return CheckOutcome{}, err
	}
	if rq.LastOwnerTurn.After(p.At) {
		return CheckOutcome{}, fmt.Errorf("%w: his turn of %s is these words, but he asked more at %s, which reopens a check on them",
			ErrNoProof, p.At.Local().Format(timeFormat), rq.LastOwnerTurn.Local().Format(timeFormat))
	}
	if b == nil {
		b = rq.replyBefore(p.At)
	}
	out := CheckOutcome{Result: *b, Proof: p}
	out.Added, err = st.CheckOnce(ctx, store.RequestCheck{Request: rq.ID, Result: b.Key, ResultAt: b.At, Proof: p.Event, ProofAt: p.At,
		Words: p.Words, At: at}, rq.CheckIDs())
	return out, err
}

// Uncheck undoes the request's standing check, whichever of its ids it is stored under, on the owner's turn: the
// relayed words must be a whole turn of his after the one the check rests on, no question and no instruction.
func Uncheck(ctx context.Context, st *store.Store, rq *Request, words string, at time.Time) (store.RequestCheck, Proof, error) {
	if err := ProofShape(words); err != nil {
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
	undone, err := st.UndoRequestCheck(ctx, rq.CheckIDs(), at, store.RequestUndo{Proof: p.Event, ProofAt: p.At, Words: p.Words})
	return undone, p, err
}
