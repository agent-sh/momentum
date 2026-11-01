package detect

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// OwnerBurden finds what keeps making the owner step in: his own turns that push back, correct or repeat, read
// with what the agent did just before. Only his words count, as tacit credits them (an owner event of the
// transcripts, in a session's own thread), less what a program typed (Programs): never generated text. Each kind
// of step-in is a signature of its own, keyed by what the agent's last words did:
//   - attribution: they credited him with words or a rule, and he pushed back;
//   - handback: their last words handed him a step or a decision, and he pushed back or handed it back;
//   - done-claim: their last words claimed a result, and he said it does not hold;
//   - stopped: their last words said they wait or will report, and he had to nudge;
//   - repeat: he said again what he had said in the session that day.
//
// A turn is read with the agent's last words since his turn before it: words he answered once are not read again.
// A push back with none of those before it is not keyed: the words alone do not say what made him step in.
type OwnerBurden struct {
	Recur    Recur // default OwnerRecur
	Programs Programs
}

// OwnerRecur: three of his turns of one kind in two sessions in a week.
var OwnerRecur = Recur{Events: 3, Sessions: 2}

func (OwnerBurden) Family() string { return "owner-burden" }

// lookBack is how far before his turn the agent's last words may be: past it, he came back to something else.
const lookBack = 6 * time.Hour

// repeatSpan is how far back in a session a turn he repeats may be.
const repeatSpan = 24 * time.Hour

// The marks are plain English phrasing of a push back, a nudge, a hand-back and a claim, not any one person's
// idiom; their precision was checked on a month of labelled turns, whose labels and counts stay on the machine.
var (
	// pushRe are the marks of a push back in his words: why, a doubled ? or !, what he said or asked, what the
	// agent did not do, a what? or a wtf, what should have been, do it yourself, my rule, how many times, a turn
	// that opens with no.
	pushRe = regexp.MustCompile(`(?i)(\bwhy\b|\?\?|!!|\bi\s+(?:said|told|asked|wrote|mentioned)\b|\bi\s+(?:didn'?t|did\s+not|never)\s+(?:say|said|ask|want|mean)\b|` +
		`\byou\s+(?:didn'?t|did\s+not|dont|don'?t|never|keep|kept|missed|forgot|ignored|broke)\b|\bwhat\?|\bwtf\b|\bseriously\b|` +
		`\bshould\s+have\b|\bsupposed\s+to\b|\bunacceptable\b|\buseless\b|\bunusable\b|\bdo\s+it\s+yourself\b|` +
		`\bwhy\s+(?:do\s+)?you\s+need\s+me\b|\bmy\s+rule\b|\bhow\s+many\s+times\b|^\s*no\b)`)
	// nudgeRe are his nudges to a session that stopped.
	nudgeRe = regexp.MustCompile(`(?i)(\bare you (?:off|there|alive|stuck|done|working)\b|\bwhat are (?:we|you) waiting\b|^\s*status\??\s*$|\bwhats? the status\b|` +
		`\bwhy (?:did you )?stop|\bwhy (?:are you )?idle\b|^\s*(?:continue|go on|keep going|proceed)\b|\bany progress\b|^\s*\?+\s*$)`)
	// delegateRe: he hands a decision back to the agent that asked him for it.
	delegateRe = regexp.MustCompile(`(?i)(\byou (?:can )?decide\b|\bdo (?:as|what) you think\b|\byou can do\b|\bdo it yourself\b|\bjust do\b|` +
		`\bno need\b|\bdon'?t ask\b|\bdont ask\b|\bup to you\b|\byour call\b|\byou have (?:the )?approval\b)`)
	// notDoneRe: he says a result does not hold.
	notDoneRe = regexp.MustCompile(`(?i)(\bdoes?n'?t work|\bdont work|\bnot work|\bisn'?t work|\bstill (?:not|broken|fail|the same|doesn|dont|isn|no\b)|` +
		`\bnot fixed\b|\bbroken\b|\bdidn'?t (?:work|fix|change|land)|\bdidnt (?:work|fix|change|land)|\bnothing (?:changed|happen)|` +
		`\bi (?:dont|don'?t|cant|can'?t) see\b|\bwhere is\b|\bnot there\b|\bsame (?:issue|problem|error|bug)\b|\bfail(?:s|ed|ing)?\b|\bcrash|` +
		`\bnot (?:done|working|live|true)\b|\bunusable\b|\bnot what i\b)`)
	// handRe: the agent handed him a step or a decision.
	handRe = regexp.MustCompile(`(?i)(\byour call\b|\bonly you can\b|\byou(?:'ll| will)? need to\b|\bneeds? (?:your|you to)\b|\bwaiting (?:on|for) (?:you|your)\b|` +
		`\bsay (?:go|the word)\b|\bwhen you(?:'re| are) ready\b|\bplease run\b|\bfrom you\b|\bshould i\b|\bdo you want me to\b|\bwant me to\b|` +
		`\byour (?:go|ok|okay|approval|decision)\b|\bshall i\b|\bwould you like\b|\bif you want\b|\blet me know\b|\bdecision for you\b)`)
	// attrRe: the agent credited him with words or a rule.
	attrRe = regexp.MustCompile(`(?i)(\byou said\b|\byour rule\b|\bper your\b|\byou asked (?:me )?(?:to|for|not)\b|\byou told me\b|` +
		`\byour (?:standing )?(?:instruction|ruling|order)\b|\bas you (?:said|asked|wanted)\b|\byou prefer\b|\byou wanted\b)`)
	// doneRe: the agent claimed a result.
	doneRe = regexp.MustCompile(`(?i)(\bdone\b|\bfixed\b|\bverified\b|\bis live\b|\bworks\b|\bworking now\b|\bcomplete[d]?\b|\bshipped\b|\bmerged\b|` +
		`\ball green\b|\bpass(?:es|ed)\b|\blanded\b|\bready\b)`)
	// idleRe: the agent said it waits, or will come back.
	idleRe = regexp.MustCompile(`(?i)(\bidle\b|\bwaiting (?:for|on|until)\b|\bpaused\b|\bwill report\b|\bi'?ll (?:report|check back|ping|come back|let you know)\b|` +
		`\bstanding by\b|\buntil you\b|\bstill running\b|\bonce (?:it|they) (?:finish|land|complete)s?\b)`)
	wordRe = regexp.MustCompile(`[a-z0-9][a-z0-9/._-]{2,}`)
)

// ownerTitles are the kinds' cases' titles.
var ownerTitles = map[string]string{
	"attribution": "Agents credit the owner with words or rules he pushes back on",
	"handback":    "Agents hand the owner a step or a decision, and he pushes back",
	"done-claim":  "The owner pushes back on a result an agent claimed",
	"stopped":     "The owner has to nudge a session that said it waits or will report",
	"repeat":      "The owner has to say again what he said in the session",
}

// OwnerClass is the kind of step-in one of his turns is, read with the agent's last words before it (the store
// keeps an agent's text by its ends: what it set out to do, and its last words, which are what he answers) and his
// own earlier turns in the session that day; "" for none.
func OwnerClass(text, agentBefore string, earlier []string) string {
	push, last := pushRe.MatchString(text), lastWords(agentBefore)
	switch {
	case agentBefore == "":
	case push && attrRe.MatchString(agentBefore):
		return "attribution"
	case (push || delegateRe.MatchString(text)) && handRe.MatchString(last):
		return "handback"
	case notDoneRe.MatchString(text) && doneRe.MatchString(last):
		return "done-claim"
	case nudgeRe.MatchString(text) && idleRe.MatchString(last):
		return "stopped"
	}
	words := wordSet(text)
	if len(words) >= 5 {
		for _, e := range earlier {
			if jaccard(words, wordSet(e)) >= 0.6 {
				return "repeat"
			}
		}
	}
	return ""
}

// lastWords is the end of an agent's text as the store keeps it: what follows its cut, or its last 160 bytes.
func lastWords(s string) string {
	if i := strings.LastIndex(s, "…"); i >= 0 {
		return s[i+len("…"):]
	}
	if len(s) > 160 {
		return s[len(s)-160:]
	}
	return s
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		out[w] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// ownerTurns are the owner's turns from since to until, oldest first, and which of them a program typed.
func ownerTurns(ctx context.Context, st *store.Store, p Programs, since, until, window time.Time) ([]store.Event, map[int]bool, error) {
	turns, err := st.Events(ctx, store.Query{Since: since, Until: until, Kinds: []string{"owner_text"}, Source: store.Transcripts,
		Actor: store.ActorOwner, Main: true, Limit: maxRows})
	if err != nil {
		return nil, nil, err
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, p.Of(turns, window), nil
}

func (o OwnerBurden) Detect(ctx context.Context, st *store.Store, w Window) ([]store.Finding, error) {
	r := o.Recur
	if r == (Recur{}) {
		r = OwnerRecur
	}
	turns, program, err := ownerTurns(ctx, st, o.Programs, w.Since.Add(-repeatSpan), w.Until, w.Since)
	if err != nil {
		return nil, err
	}
	earlier := map[string][]store.Event{}
	var occ []occurrence
	for i, t := range turns {
		if program[i] {
			continue
		}
		var before []string
		prevAt := time.Time{}
		for _, e := range earlier[t.Session] {
			if t.TS.Sub(e.TS) <= repeatSpan {
				before = append(before, e.Summary)
			}
			prevAt = e.TS
		}
		earlier[t.Session] = append(earlier[t.Session], t)
		if t.TS.Before(w.Since) {
			continue // read only for what he repeats
		}
		after := t.TS.Add(-lookBack)
		if prevAt.After(after) {
			after = prevAt // what he answered already is not read again
		}
		prior, err := st.Before(ctx, t.Session, []string{"agent_text"}, after, t.TS, 1)
		if err != nil {
			return gather("owner-burden", occ, r), err
		}
		agent := ""
		if len(prior) > 0 {
			agent = prior[0].Summary
		}
		class := OwnerClass(strings.TrimSpace(t.Summary), agent, before)
		if class == "" {
			continue
		}
		occ = append(occ, occurrence{sig: signature("owner-burden", class), title: ownerTitles[class], ev: t})
	}
	return gather("owner-burden", occ, r), nil
}
