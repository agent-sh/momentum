package supervise

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Label is one of the owner's manual steps a replay is measured against: a nudge to a session that stopped, or
// a step of a restart.
type Label struct {
	Session string
	At      time.Time
	Kind    string // nudge, restart
	Text    string
}

// ReadLabels reads labels, one JSON object per line: {"session", "at" (RFC 3339, or Unix milliseconds),
// "kind", "text"}. The form of the hand count that set the baseline is read too: {"sid", "ep" (Unix seconds),
// "fam": ["F1"] for a nudge or ["F5"] for a restart step, "text"}.
func ReadLabels(r io.Reader) ([]Label, error) {
	var out []Label
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v struct {
			Session string          `json:"session"`
			At      json.RawMessage `json:"at"`
			Kind    string          `json:"kind"`
			Text    string          `json:"text"`
			Sid     string          `json:"sid"`
			Ep      float64         `json:"ep"`
			Fam     []string        `json:"fam"`
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		l := Label{Session: v.Session, Kind: v.Kind, Text: v.Text}
		if l.Session == "" {
			l.Session = v.Sid
		}
		switch {
		case len(v.At) > 0:
			var s string
			if json.Unmarshal(v.At, &s) == nil {
				t, err := time.Parse(time.RFC3339Nano, s)
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", n, err)
				}
				l.At = t
			} else if ms, err := strconv.ParseInt(string(v.At), 10, 64); err == nil {
				l.At = time.UnixMilli(ms)
			}
		case v.Ep > 0:
			l.At = time.UnixMilli(int64(v.Ep * 1000))
		}
		if len(v.Fam) > 0 { // the hand count's own kind field is the input's kind, not the label's
			l.Kind = ""
			for _, f := range v.Fam {
				switch f {
				case "F1":
					l.Kind = "nudge"
				case "F5":
					l.Kind = "restart"
				}
			}
		}
		if l.Session == "" || l.At.IsZero() || l.Kind == "" {
			return nil, fmt.Errorf("line %d: a label needs a session, a time and a kind", n)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// Outcomes of a replayed stop.
const (
	Covered    = "covered"     // a rule would have acted before the owner's labelled step
	OwnerFirst = "owner first" // a rule applied, but the owner stepped in before it would have acted
	SelfWoke   = "self"        // a rule would have acted, but the session went on by itself
	OwnerOther = "owner other" // a rule would have acted, and the owner came back with something else
	Left       = "left"        // a rule would have acted, and nothing came after it in the window
	Missed     = "missed"      // the owner's labelled step, and no rule would have acted
	// Unlabelled: a rule would have acted, and the owner came back with a nudge the labels do not hold ("try
	// again" after an API error). Counted apart from both.
	Unlabelled = "unlabelled nudge"
)

// nudges are the owner's short continue and retry texts, normalized: lower case, no apostrophes, words only.
var nudges = map[string]bool{}

func init() {
	for _, n := range strings.Split("resume|continue|go on|keep going|carry on|proceed|keep working|continue please|resume please|"+
		"pls continue|please continue|continue the work|continue working|keep on|go on please|continue with it|continue with that|"+
		"resume the work|resume work|next|go next|move on|lets continue|ok continue|ok resume|yes continue|continue then|"+
		"keep going please|dont stop|you can continue|try again|retry|again|once more|try again please|please try again", "|") {
		nudges[n] = true
	}
}

// Nudge reports a text that only tells a session to go on: "continue", "resume", "try again", or a lone "." or ",".
func Nudge(text string) bool {
	t := strings.TrimSpace(text)
	if t != "" && strings.Trim(t, ".,;") == "" {
		return true
	}
	t = strings.ToLower(strings.NewReplacer("'", "", "’", "").Replace(t))
	t = strings.Join(strings.FieldsFunc(t, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }), " ")
	return nudges[t]
}

// Result is one replayed stop.
type Result struct {
	Point    Point
	Decision Decision
	Fired    bool          // the rule would have acted before what came next
	Outcome  string        // "" for a stop no rule acted on that no label marks
	Label    *Label        // the owner's labelled step that ended the stop
	Earlier  time.Duration // covered: how much before the owner it would have acted
}

// labelSlack is how far apart a label and the store's event of the same input may be.
const labelSlack = 3 * time.Second

// Replay runs the rules over the points, as the first try of each would go, and compares the time each would
// act with what came next. until is the end of the window: a stop nothing came after was left until then.
func Replay(points []Point, labels []Label, p Params, probe Probe, until time.Time) []Result {
	bySession := map[string][]*Label{}
	for i := range labels {
		l := &labels[i]
		bySession[l.Session] = append(bySession[l.Session], l)
	}
	taken := map[*Label]bool{}
	labelOf := func(pt Point) *Label {
		if pt.Next.Kind != "owner" {
			return nil
		}
		for _, id := range []string{pt.Session, strings.TrimPrefix(pt.Session, "hermes:")} { // a Hermes id without its prefix
			for _, l := range bySession[id] {
				if d := l.At.Sub(pt.Next.At); !taken[l] && d > -labelSlack && d < labelSlack {
					taken[l] = true // a label ends one stop: two inputs in the same second share one
					return l
				}
			}
		}
		return nil
	}
	var out []Result
	for _, pt := range points {
		d := Plan(pt.Stop, Tries{}, p, probe)
		r := Result{Point: pt, Decision: d, Label: labelOf(pt)}
		end := pt.Next.At
		if pt.Next.Kind == "none" {
			end = until
		}
		if (d.Verdict == Wait || d.Verdict == Hold) && !d.At.IsZero() && d.At.Before(end) {
			r.Fired = true
		}
		switch {
		case r.Label != nil && r.Fired:
			r.Outcome, r.Earlier = Covered, pt.Next.At.Sub(d.At)
		case r.Label != nil && d.Rule != "" && (d.Verdict == Wait || d.Verdict == Hold):
			r.Outcome = OwnerFirst
		case r.Label != nil:
			r.Outcome = Missed
		case pt.Next.Kind == "momentum": // momentum acted: what came of it is in its own events
		case r.Fired && pt.Next.Kind == "self":
			r.Outcome = SelfWoke
		case r.Fired && pt.Next.Kind == "owner" && Nudge(pt.Next.Text):
			r.Outcome, r.Earlier = Unlabelled, pt.Next.At.Sub(d.At)
		case r.Fired && pt.Next.Kind == "owner":
			r.Outcome = OwnerOther
		case r.Fired:
			r.Outcome = Left
		}
		out = append(out, r)
	}
	return out
}

// Why says why no rule acted on a stop: its phase and what kept each rule off.
func Why(r Result) string {
	s, d := r.Point.Stop, r.Decision
	switch {
	case d.Verdict == Skip:
		return "skipped: " + d.Why
	case d.Rule != "" && d.At.IsZero():
		return d.Rule + ": " + d.Why
	case d.Rule != "":
		return d.Rule + ": the owner came first"
	case s.Busy:
		return "working: a tool call was running"
	case s.Working:
		return "working: the model owed its reply"
	case s.Error != "":
		return "an error never tried again: " + clip(s.Error, 60)
	case s.Tasks > 0:
		return "background work in flight"
	case !s.WakeAt.IsZero() || s.Loop != "":
		return "a wakeup or a loop was set"
	case AsksOwner(s.Words):
		return "its last words ask the owner"
	}
	return "its last words promise nothing"
}

// Summary is a replay's counts by rule.
type Summary struct {
	Rule                                   string
	Covered, OwnerFirst, Self, Other, Left int
	Bare                                   int // unlabelled nudges
	Earlier                                []time.Duration
}

// FalseActions are the acts the owner did not ask for by the labels: the session went on by itself, he came
// back with something else (an unlabelled nudge among them), or nothing came after.
func (s Summary) FalseActions() int { return s.Self + s.Other + s.Left + s.Bare }

// Acts are all the times the rule would have acted.
func (s Summary) Acts() int { return s.Covered + s.FalseActions() }

// Summarize counts a replay's results by rule, in Rules order.
func Summarize(rs []Result) []Summary {
	by := map[string]*Summary{}
	for _, rule := range Rules {
		by[rule] = &Summary{Rule: rule}
	}
	for _, r := range rs {
		s := by[r.Decision.Rule]
		if s == nil {
			continue
		}
		switch r.Outcome {
		case Covered:
			s.Covered++
			s.Earlier = append(s.Earlier, r.Earlier)
		case OwnerFirst:
			s.OwnerFirst++
		case SelfWoke:
			s.Self++
		case OwnerOther:
			s.Other++
		case Left:
			s.Left++
		case Unlabelled:
			s.Bare++
		}
	}
	var out []Summary
	for _, rule := range Rules {
		out = append(out, *by[rule])
	}
	return out
}

// Quantile is the q-quantile of ds (nearest rank), 0 for none.
func Quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(q*float64(len(s)-1) + 0.5)
	return s[min(max(i, 0), len(s)-1)]
}
