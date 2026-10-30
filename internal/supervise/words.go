package supervise

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Error classes: what an API error that ended a turn says about trying again.
const (
	Transient = "transient" // the provider or the network failed for a while: try again later
	Login     = "login"     // the login expired: only its owner can log in again
	Limit     = "limit"     // a usage limit: it resets at a time the error states
	Org       = "org"       // the organization turned the access off
)

var (
	// Never tried again: a refusal, a request too long to send, a misuse.
	reNoRetry = regexp.MustCompile(`(?i)safeguards flagged|safety classifier|usage policy|prompt is too long|too long for|context (window|length) (exceeded|is full)|invalid[_ ]request|credit balance|capture only|tool use concurrency`)
	reOrg     = regexp.MustCompile(`(?i)organization has disabled|disabled (claude )?subscription access|ask your admin to enable`)
	reLogin   = regexp.MustCompile(`(?i)login expired|not logged in|please run /login|oauth token (has )?(expired|revoked)|authentication_error|invalid api key|401 unauthorized`)
	reLimit   = regexp.MustCompile(`(?i)hit your (\w+[- ])?limit|(usage|session|weekly|daily|\d+-hour) limit (reached|hit)|limit reached\s*[·∙•|]|out of (extra )?usage|^rate_limit: `)
	reTrans   = regexp.MustCompile(`(?i)api error: (5\d\d|429|529)\b|bedrock is unable to process|can't reach the api server|server error mid-response|unexpected error during processing|connection (refused|error|reset|closed)|request timed out|timed out|overloaded|econnreset|eai_again|enotfound|fetch failed|socket hang up|network error|throttl|too many requests|service unavailable|internal server error|bad gateway|gateway time-?out`)
)

// ErrorClass says what an API error that ended a turn is, or "" for one never tried again blindly.
func ErrorClass(text string) string {
	switch {
	case text == "", reNoRetry.MatchString(text):
		return ""
	case reOrg.MatchString(text):
		return Org
	case reLogin.MatchString(text):
		return Login
	case reLimit.MatchString(text):
		return Limit
	case reTrans.MatchString(text):
		return Transient
	}
	return ""
}

// resetZone is the zone after a reset time: in parentheses, or bare (UTC, an IANA name, an abbreviation).
const resetZone = `(?:\s*\(([^)]+)\)|\s+(UTC|GMT|[A-Za-z]+/[A-Za-z_]+(?:/[A-Za-z_]+)?|(?-i:[A-Z]{2,5}))\b)?`

var (
	reResetDate  = regexp.MustCompile(`(?i)resets? (?:at |on )?([A-Z][a-z]{2,8}) (\d{1,2}),? (\d{1,2})(?::(\d{2}))?(?:\s*(am|pm))?` + resetZone)
	reResetClock = regexp.MustCompile(`(?i)resets? (?:at )?(\d{1,2})(?::(\d{2}))?(?:\s*(am|pm))?` + resetZone)
	reResetEpoch = regexp.MustCompile(`limit reached\|(\d{10})`)
)

// resetAhead is how far past the turn of a year a reset stated by its day can be: a date a little before the
// error it ends is a misreading, not a reset a year away.
const resetAhead = 31 * 24 * time.Hour

// ResetAt is the time a usage limit resets, as its error states it ("resets Mar 7, 9am (Europe/Lisbon)",
// "resets 3pm UTC", "resets at 15:30", or the older "limit reached|<unix seconds>"): the first such time after the
// error. A time without a zone is local; one in a zone it does not know, or a date before the error, is no time.
func ResetAt(text string, at time.Time) (time.Time, bool) {
	if m := reResetEpoch.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if t := time.Unix(n, 0); t.After(at) {
			return t, true
		}
		return time.Time{}, false // a reset before the error it ends is a misreading
	}
	if m := reResetDate.FindStringSubmatch(text); m != nil {
		mon, okMon := month(m[1])
		day, _ := strconv.Atoi(m[2])
		h, mi, okClock := clock(m[3], m[4], m[5])
		if okMon && okClock {
			loc, ok := zone(m[6], m[7])
			if !ok {
				return time.Time{}, false
			}
			t := time.Date(at.In(loc).Year(), mon, day, h, mi, 0, 0, loc)
			if !t.After(at) {
				if next := t.AddDate(1, 0, 0); next.Sub(at) <= resetAhead {
					return next, true // the turn of a year
				}
				return time.Time{}, false
			}
			return t, true
		}
	}
	if m := reResetClock.FindStringSubmatch(text); m != nil {
		h, mi, ok := clock(m[1], m[2], m[3])
		if ok {
			loc, ok := zone(m[4], m[5])
			if !ok {
				return time.Time{}, false
			}
			l := at.In(loc)
			t := time.Date(l.Year(), l.Month(), l.Day(), h, mi, 0, 0, loc)
			if !t.After(at) {
				t = t.AddDate(0, 0, 1)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

func month(s string) (time.Month, bool) {
	s = strings.ToLower(s)
	for m := time.January; m <= time.December; m++ {
		if name := strings.ToLower(m.String()); strings.HasPrefix(name, s) && len(s) >= 3 {
			return m, true
		}
	}
	return 0, false
}

func clock(h, m, ampm string) (int, int, bool) {
	hh, err := strconv.Atoi(h)
	if err != nil {
		return 0, 0, false
	}
	mm := 0
	if m != "" {
		mm, _ = strconv.Atoi(m)
	}
	switch strings.ToLower(ampm) {
	case "am":
		if hh == 12 {
			hh = 0
		}
	case "pm":
		if hh < 12 {
			hh += 12
		}
	case "":
		if m == "" { // a bare number is not a time
			return 0, 0, false
		}
	}
	if hh > 23 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// zone is the first zone named, or local when none is; false for a name it does not know.
func zone(names ...string) (*time.Location, bool) {
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			loc, err := time.LoadLocation(name)
			return loc, err == nil
		}
	}
	return time.Local, true
}

// The agent's last words, as the store keeps an agent's text: its first headChars and last tailChars bytes,
// whitespace folded. The rules read only this, live and in the replay alike.
const (
	headChars = 160
	tailChars = 80
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
	return fold(prefix(s, headChars)) + "… " + fold(suffix(s, tailChars))
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

var reSentence = regexp.MustCompile(`[^.!?]+(?:[.!?]+|$)`)

// lastSentences are the last n sentences of the words' tail.
func lastSentences(words string, n int) []string {
	ss := reSentence.FindAllString(tail(words), -1)
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
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
			return ss[i]
		}
	}
	return ""
}

// Question is the sentence of a whole text, the agent's last, that asks the owner something: the last of its last
// two sentences that does, or "". Words keep only the ends of a long text, so a question there can be cut.
func Question(text string) string {
	ss := reSentence.FindAllString(fold(reTrailer.ReplaceAllString(text, " ")), -1)
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > 2 {
		out = out[len(out)-2:]
	}
	return asking(out)
}

// Promise returns the sentence of the last words that says the turn left work for later: a promise to report or
// check when something finishes, a wait it says it is in, or a step it announced and did not take. "" when the
// words end the work, ask the owner, or say nothing of the kind.
func Promise(words string) string {
	if AsksOwner(words) {
		return ""
	}
	ss := lastSentences(words, 2)
	if len(ss) > 0 && reFinal.MatchString(ss[len(ss)-1]) {
		return ""
	}
	for i := len(ss) - 1; i >= 0; i-- {
		if rePromise.MatchString(ss[i]) || reStep.MatchString(ss[i]) {
			return ss[i]
		}
	}
	return ""
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
