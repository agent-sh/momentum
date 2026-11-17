package supervise

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/sessions"
)

// Error classes: what an API error that ended a turn says about trying again (sessions.ErrorClass).
const (
	Transient = sessions.Transient
	Login     = sessions.Login
	Limit     = sessions.Limit
	Org       = sessions.Org
)

// ErrorClass says what an API error that ended a turn is (sessions.ErrorClass): the session layer reads it too.
func ErrorClass(text string) string { return sessions.ErrorClass(text) }

// ResetAt is the time a usage limit resets, as its error states it (sessions.ResetAt): the session layer reads it
// too.
func ResetAt(text string, at time.Time) (time.Time, bool) { return sessions.ResetAt(text, at) }

// The agent's last words, as the store keeps an agent's text: its first headChars bytes and its end
// (plain.KeptEnd: its last two sentences in at most tailMost bytes, else its last tailMost bytes), whitespace
// folded. The rules read only this, live and in the replay alike.
const (
	headChars = plain.SummaryHead
	tailChars = plain.SummaryEnd
	tailMost  = plain.SummaryEndMost
)

// reTrailer is what a harness appends to an agent's text that is not the agent's words: Codex's memory
// citations, and an automation heartbeat's envelope.
var reTrailer = regexp.MustCompile(`(?s)<oai-mem-citation>.*?(</oai-mem-citation>|$)|</?heartbeat>|</?(automation_id|decision|message)>`)

// reTrailerEnd is the end of a Codex citation, as the end the store keeps of a text holds it.
var reTrailerEnd = regexp.MustCompile(`</(rollout_ids|citation_entries|oai-mem-citation)>|\[[^\]]*guidance\]`)

// Words is a text as the rules read it: what the store keeps of an agent's text, without what a harness appended
// to it.
func Words(s string) string {
	s = reTrailer.ReplaceAllString(s, " ")
	if len(s) <= headChars+tailChars {
		return fold(s)
	}
	head, end := prefix(s, headChars), plain.KeptEnd(s, tailChars, tailMost)
	if len(head)+len(end) >= len(strings.TrimRightFunc(s, unicode.IsSpace)) {
		return fold(s) // they meet: the text is kept whole
	}
	return fold(head) + "… " + fold(end)
}

func fold(s string) string { return strings.Join(strings.Fields(s), " ") }

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

func suffix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && s[i]&0xc0 == 0x80 {
		i++
	}
	return s[i:]
}

// tail is the end of the words: after the cut when the store cut them, else all of them. When the end the store
// kept is a harness's trailer, the agent's own end is lost, and the start is all there is.
func tail(words string) string {
	words = reTrailer.ReplaceAllString(words, " ")
	if i := strings.LastIndex(words, "… "); i >= 0 {
		if t := words[i+len("… "):]; !reTrailerEnd.MatchString(t) {
			return t
		}
		return strings.TrimSpace(words[:i])
	}
	return words
}

// sentences splits a text where its sentences end: at a mark that a space follows, when what comes next does not
// begin with a small letter, and at the end. A mark with no space after it ("v2.1", "config.yaml", "docs/plan.md")
// ends nothing, and neither does one that small letters follow ("e.g. two columns"): the rest would read as a
// sentence of its own, without what it hangs on. A sentence that opens with a small letter (a file's or a command's
// name) so joins the one before it, which widens what a reader of asks sees and what is quoted. An abbreviation
// before a capital or a digit ("No. 3", "Dr. Smith") still ends one.
func sentences(text string) []string { return split(text, false) }

// pieces splits a text at every mark that a space follows, whatever comes next: the sentences, with one that opens
// with a small letter on its own. A promise is looked for in the last two of these, so that a sentence joined to
// the one before does not reach further back for one.
func pieces(text string) []string { return split(text, true) }

func split(text string, every bool) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	start := 0
	for _, m := range plain.SentenceEnds(text, every) {
		add(text[start:m[1]])
		start = m[1]
	}
	add(text[start:])
	return out
}

// lastSentences are the last n sentences of the words' tail.
func lastSentences(words string, n int) []string {
	out := sentences(tail(words))
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

var (
	// a question, an offer, or work that waits on the owner: "tell me and I'll", "once you confirm", "when you're done"
	reAsks = regexp.MustCompile(`(?i)\?|\b(should i|shall i|want me to|do you want|would you like|which (one|option|do you)|let me know|your call|you decide|up to you|reply with|tell me\b|say (go|the word|so|yes)\b|send me\b|ask (me|"|“)|once you\b|when you(['’]re| are| have| get)?\b|after you\b|if you\b|until you\b|you can (now )?(start|run|do|try|test|restart|open|check)\b|or (both|neither|something else)\b|waiting (for|on) (you|your)|need(s)? (you|your)\b|on your side|your (approval|decision|go-ahead|ok|turn)\b)`)
	// what a turn says when it is over and nothing is owed
	reFinal = regexp.MustCompile(`(?i)\b(nothing (needs you|is running|else|left|more)|all (done|set)|done for now|no further|ready for (you|review)|whenever you (like|want|are ready)|over to you|that's (all|it)|no action (needed|required))\b`)
	// a promise to come back to the work, or a wait it says it is in
	rePromise = regexp.MustCompile(`(?i)\b(i'?ll|i’ll|i will|i'm going to|i’m going to|going to) (\w+)|\b(waiting (for|on|until)|next check (is )?(at|in)|check(ing)? (back|again) (at|in|once|when)|once (it|they|that|this|the [\w-]+) (finish(es)?|complete[sd]?|land[sd]?|is done|are done|return[s]?|report[s]?)|when (it|they|that|this|the [\w-]+) (finish(es)?|complete[sd]?|land[sd]?|is done|are done|return[s]?|comes? back|report[s]?)|still (running|going|working|in progress)|meanwhile)\b`)
	// a step it announces and has not taken
	reStep = regexp.MustCompile(`(?i)^(now |next,? |then |so |first,? )?(i'?m |i’m )?(checking|running|writing|fixing|rebasing|adding|updating|building|testing|starting|launching|re-?running|retrying|deploying|merging|opening|reading|moving|applying|looking|investigating|continuing|resuming|cutting|pushing|publishing|installing|arming|polling|watching|verifying|trying|recording|reviewing|drafting|preparing|cleaning|removing|restoring|relaunching|spawning|dispatching|queuing|filing|posting|comparing|measuring|benchmarking|profiling|debugging|tracing|switching|porting|wiring|re-?arming|regenerating|retraining|training|evaluating|scoring|collecting|gathering|pulling|fetching|syncing|uploading|downloading)\b|^(next( step)?:|then i |on it|starting now)`)
)

// AsksOwner reports whether the words end by asking the owner something: a question, an offer that waits for
// his yes, or work only he can do.
func AsksOwner(words string) bool { return Asked(words) != "" }

// Asked is the sentence the words end on that asks the owner something, the last of the last two that does, or "".
func Asked(words string) string { return asking(lastSentences(words, 2)) }

func asking(ss []string) string {
	for i := len(ss) - 1; i >= 0; i-- {
		if reAsks.MatchString(ss[i]) {
			if !strings.ContainsFunc(ss[i], unicode.IsLetter) {
				return strings.Join(ss[i:], " ") // a kept end's opening mark: the question it closed is gone
			}
			return ss[i]
		}
	}
	return ""
}

// Question is the sentence of a whole text, the agent's last, that asks the owner something: the last of its last
// two sentences that does, or "". Words keep only the ends of a long text, so a question there can be cut.
func Question(text string) string {
	out := sentences(fold(reTrailer.ReplaceAllString(text, " ")))
	if len(out) > 2 {
		out = out[len(out)-2:]
	}
	return asking(out)
}

// Promise returns the sentence of the last words that says the turn left work for later: a promise to report or
// check when something finishes, a wait it says it is in, or a step it announced and did not take. "" when the
// words end the work, ask the owner, or say nothing of the kind.
//
// The first sentence of a kept end is no promise: the store may have cut its start, and what it hangs on may be in
// the part that is gone ("If you approve it, I'll build the page" kept as "ve it, I'll build the page"), so it
// cannot be told from an offer that waits for the owner. A kept end that opens with the mark that closed the
// sentence before has that mark as its first sentence. The promise is looked for in the last two pieces of the
// kept end and quoted from the piece that holds it to the end of its sentence. A promise in small letters right
// after the cut sentence joins it and is lost with it.
func Promise(words string) string {
	if AsksOwner(words) {
		return ""
	}
	if ps := pieces(tail(words)); len(ps) > 0 && reFinal.MatchString(ps[len(ps)-1]) {
		return ""
	}
	ss := sentences(tail(words))
	first, left := 0, 2
	if cutInside(words) {
		first = 1
	}
	for i := len(ss) - 1; i >= first && left > 0; i-- {
		ps := pieces(ss[i])
		for j := len(ps) - 1; j >= 0 && left > 0; j, left = j-1, left-1 {
			if rePromise.MatchString(ps[j]) || reStep.MatchString(ps[j]) {
				return strings.Join(ps[j:], " ")
			}
		}
	}
	return ""
}

// cutInside reports words the store cut, whose kept end is not their start (tail): the first sentence of that end
// may lack its start. Nothing in the end says whether it does: a capital letter begins "…, I'll build" too. So a
// promise whose sentence the cut fell in, or that fills the kept end to the byte, is lost; and so is one after an
// agent's own "… " in words kept whole, which reads as the store's.
func cutInside(words string) bool {
	words = reTrailer.ReplaceAllString(words, " ")
	i := strings.LastIndex(words, "… ")
	return i >= 0 && tail(words) != strings.TrimSpace(words[:i])
}

var (
	// Hermes stopped at max_tokens: "The model's reply was cut off before it finished ... Send `continue`".
	reCutoff = regexp.MustCompile("(?i)cut off before it finished|hit its output length limit|raise max_tokens|send `continue`, ask for the work in smaller steps")
	// Hermes closed a turn that a restart or a failure cut short.
	reCutShort = regexp.MustCompile(`(?i)your request was not processed\. send it again|this turn did not complete\. some actions may already have run`)
)

// Cutoff reports Hermes's notice that the model's reply hit its output limit and the turn asks for `continue`.
func Cutoff(words string) bool { return reCutoff.MatchString(words) }

// CutShort reports Hermes's notice that a turn did not complete, as it writes it after a restart or a failure.
func CutShort(words string) bool { return reCutShort.MatchString(words) }
