// Package verdict judges a finished turn with a model: the prompt, the reply parser and the evidence gate. It
// has no model client; the caller sends System and Prompt to a model of its choice and passes the reply to
// Parse.
//
// A review never decides whether a session is running. It only splits an idle session into needs_you or done,
// and Apply accepts that only when the review covered the same idle stretch and, for needs_you, quoted the
// request word for word from the session's last message.
package verdict

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/text"
)

// Review is a model's reading of one session.
type Review struct {
	ReviewedText string         `json:"reviewed_text"`          // the exact last message reviewed
	AskEvidence  string         `json:"ask_evidence,omitempty"` // the passage of it that asks the owner to act
	Overall      string         `json:"overall"`
	Now          string         `json:"now"`
	NeedsYou     string         `json:"needs_you"`
	Verdict      string         `json:"verdict"` // needs_you, done or working
	At           time.Time      `json:"at"`      // when the review was asked for
	Size         int64          `json:"size"`
	Seq          int            `json:"seq"` // the log length it covered
	Title        string         `json:"title,omitempty"`
	Seen         time.Time      `json:"seen,omitzero"` // the last scan that found the session
	Phase        sessions.Phase `json:"phase"`         // the phase it was asked in
}

// Verdicts a review can give.
const (
	VerdictNeedsYou = "needs_you"
	VerdictDone     = "done"
	VerdictWorking  = "working"
)

// System is the system prompt.
const System = `You keep the status of one AI agent session for its owner. The owner runs many sessions at once and visits a board to see which ones need him; he opens only those. You maintain two things: a DOSSIER (your private, durable understanding of the session) and a CARD (what the board shows).

Each review gives you FACTS, your current DOSSIER (or none, on the first review) and the NEW ACTIVITY since your last review. Read the activity against the dossier, then rewrite the dossier and the card.

FACTS are computed exactly from the session's files and processes. Never contradict them: if FACTS say a turn is running or background work is in flight, the session is not waiting on the owner, whatever its last message says.

Activity lines start with their kind. USER lines are text the harness recorded in the user role; the bracket, when there is one, is the harness's own label for where it came from (typed, queued, sdk, ...). They are not proof that the owner wrote them: programs, other sessions and scheduled prompts also write there. PEER lines are messages from other sessions.

Reply with one JSON object and nothing else:
{"dossier": string, "overall": string, "now": string, "needs_you": string or null, "ask_evidence": string or null, "verdict": "needs_you" | "done" | "working"}

dossier: markdown, at most 350 words, in this shape:
# <task in a few words>
**Goal:** one or two sentences: what the session is meant to achieve.
**Rulings:** bullets of what the user messages explicitly asked, decided or forbade. Keep these; they rarely go stale.
**Progress:** bullets of milestones with concrete facts (PR numbers, measured numbers, boxes, files), oldest first; fold old ones together as the list grows.
**State:** one or two sentences on where it stands right now.
**Open threads:** bullets: pending decisions, risks, things it is waiting for.
Keep what is still true from the old dossier, add what the new activity shows, drop what turned stale.

overall: at most 18 words. Where the whole task stands, with context: the goal and how far along. Concrete nouns. Not "The session is", not "The agent".
now: at most 10 words. What it is doing right now; if its turn is over, what it last did.
needs_you: null, or at most 28 words saying exactly what the owner must decide, approve, answer or do, as a question or an instruction. Set it when the last assistant message asks him something, offers a choice, waits for an approval, or reports a blocker only he can clear (credentials, money, access, a judgment call), even while background work runs. A report of finished work that asks nothing is null, and so is a session that simply has no task yet or sits idle with no question: the owner starts those himself.
ask_evidence: for needs_you, copy the exact passage from LAST ASSISTANT MESSAGE IN FULL that asks the owner to act. Otherwise null. The passage must contain the actual question, approval request or owner-only blocker. Never invent a follow-up question, request a visual check on delivered work, or carry a request forward from the dossier or older activity. A dependency on another agent, a scheduled start or an external job is not an owner request. A hypothetical offer of more work is not a blocker. If the latest message only reports progress or completion, needs_you and ask_evidence must both be null. When in doubt, do not claim the owner is needed.
verdict: "working" if FACTS say a turn or background work is running; otherwise "needs_you" when needs_you is set, else "done".

Plain words, no em dashes.`

// Limits on how much activity a prompt carries, in characters: a first review reads the whole condensed
// history, a later one only what is new since.
const (
	FirstLimit = 120_000
	DeltaLimit = 40_000
)

var toolNames = map[string]string{"claude": "Claude Code", "codex": "Codex CLI", "pi": "pi", "opencode": "opencode", "hermes": "Hermes"}

// Prompt builds the user message for one review: the facts, the dossier and the activity since the last review.
// gap says the oldest part of that activity was already dropped.
func Prompt(s *sessions.Session, delta []sessions.Entry, gap bool, dossier string, now time.Time) string {
	var f strings.Builder
	fmt.Fprintf(&f, "FACTS\nagent: %s\nsession: %s\nproject: %s\nrunning for: %s\n", text.FirstNonEmpty(toolNames[s.Tool], s.Tool), s.Name,
		text.FirstNonEmpty(s.Project, s.Cwd), ago(s.Started, now))
	switch s.Phase {
	case sessions.Working:
		f.WriteString("state: a turn is running")
	case sessions.Background:
		f.WriteString("state: its turn is over but its own background work is still running, so it will continue by itself")
	case sessions.Scheduled:
		f.WriteString("state: its turn is over and it scheduled its own next wakeup or loop")
	case sessions.NeedsYou:
		f.WriteString("state: it is stopped and waiting on the owner")
	default:
		f.WriteString("state: its turn is over and nothing it started is still running")
	}
	f.WriteString("\n")
	if s.Now != "" {
		fmt.Fprintf(&f, "current tool call: %s (for %s)\n", s.Now, ago(s.NowSince, now))
	}
	if s.Ask != "" && s.AskKind != sessions.AskModel {
		fmt.Fprintf(&f, "pending %s: %s\n", s.AskKind, s.Ask)
	}
	for _, t := range s.Tasks {
		fmt.Fprintf(&f, "in flight: %s %q started %s ago", t.Kind, t.Label, ago(t.Started, now))
		if t.Now != "" {
			fmt.Fprintf(&f, ", latest step: %s", t.Now)
		}
		f.WriteString("\n")
	}
	if !s.WakeAt.IsZero() {
		fmt.Fprintf(&f, "next wakeup: %s\n", s.WakeAt.Local().Format("15:04"))
	}
	if s.Loop != "" {
		fmt.Fprintf(&f, "recurring loop: %s\n", s.Loop)
	}
	lines := make([]string, len(delta))
	for i, e := range delta {
		lines[i] = e.String()
	}
	if dossier == "" {
		f.WriteString("\nDOSSIER\nnone yet: build it from the history below.\n\nHISTORY, oldest first\n")
		f.WriteString(Fit(lines, FirstLimit))
	} else {
		fmt.Fprintf(&f, "\nDOSSIER\n%s\n\nNEW ACTIVITY SINCE YOUR LAST REVIEW, oldest first\n", dossier)
		if gap {
			f.WriteString("(older activity since the last review was dropped; this is the most recent part)\n")
		}
		if len(delta) == 0 {
			f.WriteString("(none: only the facts changed)\n")
		}
		f.WriteString(Fit(lines, DeltaLimit))
	}
	if s.LastText != "" {
		fmt.Fprintf(&f, "\n\nLAST ASSISTANT MESSAGE IN FULL\n%s\n", s.LastText)
	}
	return f.String()
}

// Fit keeps activity lines under limit characters. It drops tool outputs first, then all but the last 150 tool
// calls, then the oldest lines one by one, skipping user-role lines: they usually carry the intent, so they go
// last. While the text is more than half again over the limit, the user-role lines it skips are shortened to
// 300 characters. The last line always stays.
func Fit(items []string, limit int) string {
	size := func(xs []string) int {
		n := 0
		for _, x := range xs {
			n += len(x) + 1
		}
		return n
	}
	if size(items) > limit {
		var kept []string
		for _, x := range items {
			if !strings.HasPrefix(x, "OUT: ") {
				kept = append(kept, x)
			}
		}
		items = kept
	}
	if size(items) > limit {
		var kept []string
		for i, x := range items {
			if !strings.HasPrefix(x, "TOOL ") || i >= len(items)-150 {
				kept = append(kept, x)
			}
		}
		items = kept
	}
	for size(items) > limit && len(items) > 1 {
		far := size(items) > limit*3/2
		i := 0
		for ; i < len(items)-1 && strings.HasPrefix(items[i], "USER"); i++ {
			if far {
				items[i] = text.OneLine(items[i], 300)
			}
		}
		if i == len(items)-1 {
			i = 0 // nothing but user-role text before the last line: its oldest goes
		}
		items = append(items[:i], items[i+1:]...)
	}
	return strings.Join(items, "\n")
}

var reJSON = regexp.MustCompile(`(?s)\{.*\}`)

// Parse reads a model's reply for session s, as it was when the review was asked for at dispatched (the
// review's At, which Apply compares with the stretch's start). It returns the review and the new dossier. A
// needs_you whose evidence is not word for word in the last message is turned into done: a generated question is
// not evidence. The caller sets Seen, and the Title from the dossier.
func Parse(reply string, s *sessions.Session, dispatched time.Time) (*Review, string, error) {
	m := reJSON.FindString(reply)
	var r struct {
		Dossier     string  `json:"dossier"`
		Overall     string  `json:"overall"`
		Now         string  `json:"now"`
		NeedsYou    *string `json:"needs_you"`
		AskEvidence *string `json:"ask_evidence"`
		Verdict     string  `json:"verdict"`
	}
	if m == "" || json.Unmarshal([]byte(m), &r) != nil {
		return nil, "", fmt.Errorf("verdict: no JSON object in the reply")
	}
	rv := &Review{Overall: clean(r.Overall, 140), Now: clean(r.Now, 90), Verdict: r.Verdict, At: dispatched, Size: s.Size, Seq: s.Seq,
		Phase: s.Phase, ReviewedText: s.LastText}
	if r.AskEvidence != nil {
		rv.AskEvidence = strings.TrimSpace(*r.AskEvidence)
	}
	if r.NeedsYou != nil {
		rv.NeedsYou = clean(*r.NeedsYou, 220)
	}
	if rv.Verdict != VerdictNeedsYou && rv.Verdict != VerdictDone {
		rv.Verdict = VerdictWorking
	}
	if rv.Verdict == VerdictNeedsYou && (rv.NeedsYou == "" || !HasEvidence(s.LastText, rv.AskEvidence)) {
		rv.Verdict, rv.NeedsYou, rv.AskEvidence = VerdictDone, "", ""
	}
	return rv, strings.ReplaceAll(strings.TrimSpace(r.Dossier), "—", ","), nil
}

func clean(s string, n int) string {
	s = strings.NewReplacer("—", ",", "–", "-", "**", "", "`", "").Replace(s)
	if strings.EqualFold(strings.TrimSpace(s), "null") {
		return ""
	}
	return text.OneLine(s, n)
}

// HasEvidence reports whether quote is word for word in lastText. Whitespace may differ; the words may not.
func HasEvidence(lastText, quote string) bool {
	q := strings.Join(strings.Fields(quote), " ")
	return q != "" && strings.Contains(strings.Join(strings.Fields(lastText), " "), q)
}

// Apply lets a review settle an idle session, and only the idle stretch it describes: the review was asked in
// idle, after the stretch began, at the same log length and on the same last message; a needs_you also needs
// its evidence in that message. dismissed says the owner waved this ask away at this log length. Call it after
// state.Stamps.Apply, which must see the scanned phase, not the settled one.
func Apply(s *sessions.Session, r *Review, dismissed bool) {
	if s.Phase != sessions.Idle || r == nil || r.Phase != sessions.Idle || r.At.Before(s.Since) || r.Seq != s.Seq ||
		r.ReviewedText != s.LastText || (r.Verdict == VerdictNeedsYou && !HasEvidence(s.LastText, r.AskEvidence)) {
		return
	}
	switch {
	case dismissed:
		s.Phase = sessions.Done
	case r.Verdict == VerdictNeedsYou && r.NeedsYou != "":
		s.Phase, s.Ask, s.AskKind = sessions.NeedsYou, r.NeedsYou, sessions.AskModel
	default:
		s.Phase = sessions.Done
	}
}

// Title is a dossier's heading, for naming a session that has no name of its own.
func Title(dossier string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(dossier), "\n")
	if !strings.HasPrefix(first, "# ") {
		return ""
	}
	return text.OneLine(strings.TrimPrefix(first, "# "), 48)
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
