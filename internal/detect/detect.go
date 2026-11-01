// Package detect holds the improvement loop's detectors: deterministic reads of the store, one per family, that
// report the problems that recur by their signatures. No model takes part. Each finding opens a case or adds its
// evidence to the case its signature names (store.ObserveCase), so the loop starts from what the store shows, and
// the same problem seen again lands in the same case.
package detect

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// Version names the detectors' signatures. A detector that keys a problem otherwise is a new version: its
// signatures are new, and the cases of the old version keep theirs.
const Version = "v1"

// Window is the span a detector reads.
type Window struct{ Since, Until time.Time }

// DefaultWindow is how far back each run reads: a week, the span the owner's work repeats over.
const DefaultWindow = 7 * 24 * time.Hour

// Detector reads the store over a window and reports what recurs.
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
	weight     int // how many times it counts: an incident's repeats; 1 otherwise
}

// signature is a family's versioned key for a problem: family/v1:key.
func signature(family, key string) string { return family + "/" + Version + ":" + key }

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
	Observed store.Observed // empty on a dry run
}

// Outcome is one detector's run: what it found and filed, or why it failed.
type Outcome struct {
	Family string
	Filed  []Filed
	Err    error
}

// Run runs each detector over the window and files what it finds; a detector that fails is named and the
// others still run. Then it sweeps the cases unseen past their retention. A dry run files and sweeps nothing.
func Run(ctx context.Context, st *store.Store, ds []Detector, w Window, actor string, dry bool) ([]Outcome, []store.Case, error) {
	var out []Outcome
	for _, d := range ds {
		o := Outcome{Family: d.Family()}
		fs, err := d.Detect(ctx, st, w)
		if err != nil {
			o.Err = err
			out = append(out, o)
			continue
		}
		for _, f := range fs {
			if dry {
				o.Filed = append(o.Filed, Filed{Finding: f})
				continue
			}
			c, obs, err := st.ObserveCase(ctx, f, actor)
			if err != nil {
				o.Err = fmt.Errorf("%s: %w", f.Signature, err)
				break
			}
			o.Filed = append(o.Filed, Filed{Finding: f, Case: c, Observed: obs})
		}
		out = append(out, o)
	}
	if dry {
		return out, nil, nil
	}
	swept, err := st.SweepCases(ctx, w.Until, store.CaseRetention)
	return out, swept, err
}

// Norm is a text with what varies between two sightings of one problem taken out, so they share a key: tags,
// quoted values, addresses, paths, ids and numbers become a letter each; lower case, one space, at most n
// characters, cut at a word.
func Norm(s string, n int) string {
	s = strings.ToLower(s)
	for _, r := range normRules {
		s = r.re.ReplaceAllString(s, r.to)
	}
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
	{regexp.MustCompile(`'[^'\n]*'`), "Q"},
	{regexp.MustCompile(`[a-z][a-z0-9+.-]*://\S+`), "U"},
	{regexp.MustCompile(`~[\w./@%+-]*|\.{0,2}/[\w./@%+-]*[\w/]`), "P"},
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "H"},
	{regexp.MustCompile(`\b0x[0-9a-f]+\b`), "H"},
	{regexp.MustCompile(`\b(?:[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*|[0-9a-f]*[a-f][0-9a-f]*[0-9][0-9a-f]*)\b`), "H"},
	{regexp.MustCompile(`\d+(?:\.\d+)*`), "N"},
}

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
