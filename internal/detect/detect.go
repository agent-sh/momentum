// Package detect holds the improvement loop's detectors: deterministic reads of the store (and, for sessions, of
// what the replay of supervision reads), one per family, that report the problems that recur by their
// signatures. No model takes part. Each finding opens a case or adds its evidence to the case its signature names
// (store.ObserveCase), so the loop starts from what the store shows, and the same problem seen again lands in the
// same case.
package detect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// Versions name each family's signatures. A detector that keys a problem otherwise takes its family a new
// version: its signatures are new, and the cases of the old version keep theirs. Another family's stay.
var Versions = map[string]string{"failures": "v1", "owner-burden": "v1", "sessions": "v1", "component-health": "v1"}

// Window is the span a detector reads.
type Window struct{ Since, Until time.Time }

// DefaultWindow is how far back each run reads: a week, the span the owner's work repeats over.
const DefaultWindow = 7 * 24 * time.Hour

// Detector reads the store over a window and reports what recurs. A detector that fails partway returns what it
// found with its error: what it found is filed all the same.
type Detector interface {
	Family() string
	Detect(ctx context.Context, st *store.Store, w Window) ([]store.Finding, error)
}

// Recur is how often a problem must show in a window to be a finding: at least Events times, in at least
// Sessions sessions (an incident, which has none, counts its repeats).
type Recur struct{ Events, Sessions int }

// MaxEvidence bounds the evidence one finding files on one run: its newest. A case keeps what earlier runs filed.
const MaxEvidence = 200

// maxRows bounds the events a detector reads of one kind over a window.
const maxRows = 200_000

// occurrence is one sighting of a problem: its signature, the case's title if it opens one, and the event.
type occurrence struct {
	sig, title string
	ev         store.Event
	weight     int // how many times it counts: an incident sighting's repeats; 1 otherwise
}

// signatureMax is the longest signature the case store keeps, in bytes.
const signatureMax = 300

// signature is a family's versioned key for a problem: family/v1:key, cut to what the case store keeps. A key too
// long for it keeps its start and a short hash of the whole, so two long keys stay two.
func signature(family, key string) string {
	prefix := family + "/" + Versions[family] + ":"
	if len(prefix)+len(key) <= signatureMax {
		return prefix + key
	}
	sum := sha256.Sum256([]byte(key))
	tag := "~" + hex.EncodeToString(sum[:4])
	cut := signatureMax - len(prefix) - len(tag)
	for cut > 0 && !utf8.RuneStart(key[cut]) {
		cut--
	}
	return prefix + key[:cut] + tag
}

// gather turns occurrences into findings: the signatures that recur, each with its newest evidence.
func gather(family string, occ []occurrence, r Recur) []store.Finding {
	type group struct {
		title    string
		evs      []store.Event
		weight   int
		sessions map[string]bool
	}
	by := map[string]*group{}
	for _, o := range occ {
		g := by[o.sig]
		if g == nil {
			g = &group{title: o.title, sessions: map[string]bool{}}
			by[o.sig] = g
		}
		g.evs = append(g.evs, o.ev)
		g.weight += max(o.weight, 1)
		if o.ev.Session != "" {
			g.sessions[o.ev.Session] = true
		}
	}
	var out []store.Finding
	for sig, g := range by {
		if g.weight < r.Events || len(g.sessions) < r.Sessions {
			continue
		}
		sort.Slice(g.evs, func(i, j int) bool { return g.evs[i].TS.After(g.evs[j].TS) })
		if len(g.evs) > MaxEvidence {
			g.evs = g.evs[:MaxEvidence]
		}
		f := store.Finding{Family: family, Signature: sig, Title: plain.Clip(plain.Line(g.title), store.TitleMax)}
		for _, e := range g.evs {
			f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID(), At: e.TS, Kind: e.Kind, Summary: e.Summary})
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signature < out[j].Signature })
	return out
}

// Filed is what one finding did to the case store.
type Filed struct {
	Finding  store.Finding
	Case     store.Case
	Observed store.Observed // empty on a dry run, and for a finding the store refused (Err)
	Err      error
}

// Outcome is one detector's run: what it found and filed, and why it stopped short, if it did.
type Outcome struct {
	Family string
	Filed  []Filed
	Err    error
}

// Run runs each detector over the window and files what it finds. A detector that fails is named, and what it
// found before it failed is filed; a finding the case store refuses is named, and the next one is filed. Then it
// sweeps the cases unseen past their retention. A dry run files and sweeps nothing.
func Run(ctx context.Context, st *store.Store, ds []Detector, w Window, actor string, dry bool) ([]Outcome, []store.Case, error) {
	var out []Outcome
	for _, d := range ds {
		fs, err := d.Detect(ctx, st, w)
		o := Outcome{Family: d.Family(), Err: err}
		for _, f := range fs {
			if dry {
				o.Filed = append(o.Filed, Filed{Finding: f})
				continue
			}
			c, obs, err := st.ObserveCase(ctx, f, actor)
			o.Filed = append(o.Filed, Filed{Finding: f, Case: c, Observed: obs, Err: err})
			if err != nil {
				o.Err = errors.Join(o.Err, fmt.Errorf("%s: %w", f.Signature, err))
			}
		}
		out = append(out, o)
	}
	if dry {
		return out, nil, nil
	}
	swept, err := st.SweepCases(ctx, w.Until, store.CaseRetention, actor)
	return out, swept, err
}

// Norm is a text with what varies between two sightings of one problem taken out, so they share a key: tags,
// quoted values, addresses, paths, ids and numbers become a capital letter each (Q, U, P, H, N) in the lower-cased
// text; one space between words, at most n characters, cut at a word.
func Norm(s string, n int) string {
	s = strings.ToLower(s)
	for _, r := range normRules {
		s = r.re.ReplaceAllString(s, r.to)
	}
	s = idRe.ReplaceAllStringFunc(s, func(w string) string {
		if len(w) >= 8 && strings.IndexFunc(w, unicode.IsLetter) >= 0 && strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			return "H"
		}
		return w
	})
	s = numRe.ReplaceAllString(s, "N")
	s = strings.TrimRight(strings.Join(strings.Fields(plain.Line(s)), " "), " .,;:")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	cut := string([]rune(s)[:n])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return cut
}

var normRules = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`</?tool_use_error>`), " "},
	{regexp.MustCompile("`[^`\n]*`"), "Q"},
	{regexp.MustCompile(`"[^"\n]*"`), "Q"},
	// a single quote opens a quoted value only with no letter or digit before it: can't is a word, not a quote
	{regexp.MustCompile(`(^|[^\pL\pN])'[^'\n]*'`), "${1}Q"},
	{regexp.MustCompile(`[a-z][a-z0-9+.-]*://\S+`), "U"},
	{regexp.MustCompile(`\b[\w.-]+(?:/[\w.@%+-]+)+`), "P"}, // a relative path, by its slashes
	{regexp.MustCompile(`~[\w./@%+-]*|\.{0,2}/[\w./@%+-]*[\w/]`), "P"},
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "H"},
	{regexp.MustCompile(`\b0x[0-9a-f]+\b`), "H"},
	{regexp.MustCompile(`\b(?:[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*|[0-9a-f]*[a-f][0-9a-f]*[0-9][0-9a-f]*)\b`), "H"},
}

var (
	idRe  = regexp.MustCompile(`[\pL\pN_]+`) // a word: an id when it is long and mixes letters and digits
	numRe = regexp.MustCompile(`\d+(?:\.\d+)*`)
)

// slug is a short key made of a text's words: lower case, joined by dashes.
func slug(s string, n int) string {
	w := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	out := strings.Join(w, "-")
	if len(out) > n {
		out = out[:n]
		if i := strings.LastIndexByte(out, '-'); i > n/2 {
			out = out[:i]
		}
	}
	return out
}

// Programs are what marks a turn the transcripts credit to the owner as a program's (SPEC, Boundaries): momentum's
// own marker, the prefixes of the machine's wake loops and briefs (configured), the same text in two sessions
// within 2 seconds, and a long text sent in three sessions of a window (a template a program fills). A short text
// he types every day, a nudge, is his.
type Programs struct {
	Prefixes []string // the machine's wake loops' and briefs' prefixes, from its configuration
}

// Program-text rules.
const (
	momentumMarker = "[momentum]"
	twinSpan       = 2 * time.Second // the same text in two sessions this close: a program sent it
	templateSess   = 3               // a long text in this many sessions of a window: a template
	templateWords  = 8               // a text this long or longer can be a template; a shorter one is his
	sameTurn       = 2 * time.Minute // the same text again in a session this soon: one turn recorded twice
)

// Of marks the owner events (oldest first) that a program typed, and the ones that repeat a turn recorded twice;
// both are left out. A template's sessions are counted from since on, the window's.
func (p Programs) Of(turns []store.Event, since time.Time) map[int]bool {
	out := map[int]bool{}
	sessions := map[string]map[string]bool{}
	last := map[string]store.Event{}
	for i, t := range turns {
		text := strings.TrimSpace(t.Summary)
		if prev, ok := last[t.Session]; ok && strings.TrimSpace(prev.Summary) == text && t.TS.Sub(prev.TS) <= sameTurn {
			out[i] = true // one turn the transcript holds twice (queued, then sent)
			continue
		}
		last[t.Session] = t
		if t.TS.Before(since) {
			continue
		}
		if sessions[text] == nil {
			sessions[text] = map[string]bool{}
		}
		sessions[text][t.Session] = true
	}
	for i, t := range turns {
		text := strings.TrimSpace(t.Summary)
		switch {
		case text == "" || strings.HasPrefix(text, momentumMarker):
			out[i] = true
		case len(strings.Fields(text)) >= templateWords && len(sessions[text]) >= templateSess:
			out[i] = true
		}
		for _, pre := range p.Prefixes {
			if pre != "" && strings.HasPrefix(text, pre) {
				out[i] = true
			}
		}
		for j := i + 1; j < len(turns) && turns[j].TS.Sub(t.TS) <= twinSpan; j++ {
			if turns[j].Session != t.Session && strings.TrimSpace(turns[j].Summary) == text {
				out[i], out[j] = true, true
			}
		}
	}
	return out
}
