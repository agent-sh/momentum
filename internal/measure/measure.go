// Package measure decides whether a deployed change helped: deterministic, no model. A change's signal (what its
// case's problem is in the store) is counted over a window before the change and over as long after it, in units of
// activity: the hours of the sessions that did anything in the window (the machine's own hours for a signal of the
// machine), and the units in which the signal counts an event. A quieter week is not a fix, so the two windows are
// compared as shares of their activity; a change cannot move the activity by adding work inside an hour, and a
// problem that repeats inside one hour of a session counts once there.
//
// The test is Fisher's exact test, one-sided: under no change, which of both windows' units hold the problem is
// chance, so the after window's count of them is hypergeometric, its draws the after window's units. The change
// helped when the after count is lower than chance makes likely (a p-value below Alpha) and its share fell.
package measure

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Alpha is the test's level: an effect this unlikely under no change is taken as the change's.
const Alpha = 0.05

// MinBefore is the least the signal must count before a change for a drop to tell from chance at Alpha: with as
// much activity after as before, a drop to none of five units has a chance of about 1 in 32 under no change, and one
// of four about 1 in 16, above Alpha. Five is the floor at equal activity; a quieter window after needs more
// (Measurable).
const MinBefore = 5

// BaselineWindows counts the three windows before the one selected by the gate.
const BaselineWindows = 3

// Counts are a window's: the units of activity in which the signal counts an event, and the units of activity.
type Counts struct {
	Hits int `json:"hits"`
	Base int `json:"base"`
}

// Rate is the share of the activity the signal counts.
func (c Counts) Rate() float64 {
	if c.Base == 0 {
		return 0
	}
	return float64(c.Hits) / float64(c.Base)
}

// Result is a measurement.
type Result struct {
	Before   Counts  `json:"before"`
	After    Counts  `json:"after"`
	P        float64 `json:"p"` // the chance, under no change, of an after count this low or lower
	Improved bool    `json:"improved"`
	Effect   string  `json:"effect"` // the effect in words
	// Guarded marks a guard's watch (Guard), not a measured drop: Before holds the history's incidents and its
	// exposure, After the watch's recurrences and its exposure.
	Guarded bool `json:"guarded,omitempty"`
}

// Compare decides whether the share fell from before to after. With no activity in either window there is nothing
// to compare, and ok is false.
func Compare(before, after Counts) (Result, bool) {
	r := Result{Before: before, After: after, P: 1}
	if before.Base == 0 || after.Base == 0 {
		return r, false
	}
	r.P = tail(after.Hits, before.Base+after.Base, before.Hits+after.Hits, after.Base)
	r.Improved = r.P < Alpha && after.Rate() < before.Rate()
	r.Effect = fmt.Sprintf("%d of %d active hours before, %d of %d after (%s to %s), p=%.3f", before.Hits, before.Base, after.Hits,
		after.Base, percent(before.Rate()), percent(after.Rate()), r.P)
	return r, true
}

// Measurable reports whether a window after with as much activity as afterBase could show any effect: whether a
// drop to none would pass the test. It reads the count before and the activity after, never the count after, so
// waiting for more activity looks at no outcome: when none would pass, no count after would.
func Measurable(before Counts, afterBase int) bool {
	if before.Base == 0 || afterBase == 0 {
		return false
	}
	return tail(0, before.Base+afterBase, before.Hits, afterBase) < Alpha
}

// WholeWeeks reports whether a plan compares the same days of the week.
func WholeWeeks(window time.Duration) bool {
	const week = 7 * 24 * time.Hour
	return window >= week && window%week == 0
}

// Back needs three consecutive completed windows at the baseline share, with five hit units among them.
func Back(before Counts, windows []Counts) bool {
	if before.Base == 0 || len(windows) != 3 {
		return false
	}
	hits := 0
	for _, recent := range windows {
		if recent.Base == 0 || recent.Rate() < before.Rate() {
			return false
		}
		hits += recent.Hits
	}
	return hits >= MinBefore
}

// A guard stands in for a measured plan when the problem has stopped, or is too rare for a share to fall: it asks
// whether the problem comes back. Its incidents are taken as a Poisson process in units of exposure, at a rate λ the
// history estimates from k incidents in T units. A watch of E units with no recurrence passes. Sized by the plug-in
// rate (E = ln(1/Alpha)·T/k), a change that did nothing passes far more often than Alpha when k is small, since the
// rate is uncertain. A guard sizes E from the posterior predictive with Jeffreys' prior instead: with λ ~ Gamma(k+½, T),
// a watch of E units holds no recurrence with probability (T/(T+E))^(k+½), which is Alpha at
// E = T·((1/Alpha)^(1/(k+½)) − 1).

// Incident is a run of a signal's events with no gap longer than the guard's gap between two of them: events close
// together share a cause, so one burst of hundreds is one incident.
type Incident struct {
	First, Last time.Time
	Events      int
}

// Incidents groups events by their times: a gap longer than gap between two of them starts a new incident.
func Incidents(at []time.Time, gap time.Duration) []Incident {
	sorted := slices.Clone(at)
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })
	var out []Incident
	for _, t := range sorted {
		if n := len(out); n > 0 && t.Sub(out[n-1].Last) <= gap {
			out[n-1].Last = t
			out[n-1].Events++
			continue
		}
		out = append(out, Incident{First: t, Last: t, Events: 1})
	}
	return out
}

// GuardNeed is the exposure a guard's watch must reach with no recurrence: the E at which the posterior predictive
// chance of a watch that quiet, under no change, is Alpha, E = T·((1/Alpha)^(1/(k+½)) − 1), rounded up to a whole
// unit. With no exposure there is no history, and it is 0.
func GuardNeed(incidents, exposure int) int {
	if incidents < 0 || exposure <= 0 {
		return 0
	}
	return int(math.Ceil(float64(exposure) * (math.Pow(1/Alpha, 1/(float64(incidents)+0.5)) - 1)))
}

// GuardP is the posterior predictive chance, under no change, that watched units of exposure hold no recurrence
// after incidents in exposure units: (T/(T+W))^(k+½). With no exposure before there is no rate, and it is 0.
func GuardP(incidents, exposure, watched int) float64 {
	if exposure <= 0 {
		return 0
	}
	return math.Pow(float64(exposure)/float64(exposure+watched), float64(incidents)+0.5)
}

// GuardHistory is what a guard knows of its problem before the change: from Since, over Span, its incidents in its
// units of exposure, the units up to the end of its last incident (Before: the rest is the quiet stretch since), and
// the units outside every incident (Quiet).
type GuardHistory struct {
	Since     time.Time
	Span      time.Duration
	Incidents int
	Exposure  int
	Before    int
	Quiet     int
}

// GuardCount is a guard's history over [since, until) from its incidents and the hours of its units of exposure (one
// entry for each unit, its hour since the epoch). When the history starts at the signal's source's first event
// (anchored), the incident that holds that event (its first event within gap of it) is no draw of the problem's rate:
// the source began with it. It is dropped, and the history starts at the hour after its last event.
func GuardCount(incidents []Incident, units []int64, since, until time.Time, gap time.Duration, anchored bool) GuardHistory {
	if anchored && len(incidents) > 0 && incidents[0].First.Sub(since) <= gap {
		since = incidents[0].Last.Truncate(time.Hour).Add(time.Hour)
		incidents = incidents[1:]
	}
	h := GuardHistory{Since: since, Span: until.Sub(since), Incidents: len(incidents)}
	if h.Span < 0 {
		h.Span = 0
	}
	from := since.UnixMilli() / 3_600_000
	var lastHour int64 = -1
	if n := len(incidents); n > 0 {
		lastHour = incidents[n-1].Last.UnixMilli() / 3_600_000
	}
	for _, u := range units {
		if u < from {
			continue
		}
		h.Exposure++
		if u <= lastHour {
			h.Before++
		}
		inside := slices.ContainsFunc(incidents, func(in Incident) bool {
			return u >= in.First.UnixMilli()/3_600_000 && u <= in.Last.UnixMilli()/3_600_000
		})
		if !inside {
			h.Quiet++
		}
	}
	return h
}

// GuardAdmit decides whether a guard can stand on its history, and its watch: the exposure it needs and the time the
// history took to hold that much. why says what refuses it: a history that never saw the unit in use without the
// problem (too thin to estimate a rate); a quiet stretch since the last incident that the history before it would
// make unlikely had the problem gone on (it had likely stopped before the change, so a watch could not tell the
// change from that); or a watch longer than retention, the span the store is sure to read again when it ends.
func GuardAdmit(h GuardHistory, retention time.Duration) (need int, expected time.Duration, why string) {
	switch {
	case h.Exposure == 0 || h.Quiet == 0:
		return 0, 0, fmt.Sprintf("its history is too thin to tell how often the problem starts: of its %d units of exposure none lies outside its incidents, so the unit was never seen in use without the problem", h.Exposure)
	case h.Incidents == 0:
		return 0, 0, "its problem has not come back since the incident its source began with, so nothing tells how often it starts"
	}
	if p := GuardP(h.Incidents, h.Before, h.Exposure-h.Before); p < Alpha {
		return 0, 0, fmt.Sprintf("its problem had likely stopped before the change: had it gone on as its %d incidents in %d units of exposure show, %d quiet units since its last had a chance of %s, below %.2f, so a watch could not tell the change from that",
			h.Incidents, h.Before, h.Exposure-h.Before, percent(p), Alpha)
	}
	need = GuardNeed(h.Incidents, h.Exposure)
	expected = time.Duration(float64(h.Span) * float64(need) / float64(h.Exposure))
	if expected > retention {
		return need, expected, fmt.Sprintf("too rare to prove by recurrence within the store's history: %d incidents in %d units of exposure over %s ask a watch of %d units with no recurrence, about %s, past the %s the store is sure to read again",
			h.Incidents, h.Exposure, days(h.Span), need, days(expected), days(retention))
	}
	return need, expected, ""
}

// GuardWatch is a guard's window: the expected time to its need in whole weeks, at least one, at most retention.
func GuardWatch(expected, retention time.Duration) time.Duration {
	const week = 7 * 24 * time.Hour
	return min(max(week, (expected+week-1)/week*week), retention)
}

// Guarded is a guard's watch as a result: incidents in a history of exposure units before the change, recurrences in
// watched units after it, counted in units. It is improved only with no recurrence and the exposure GuardNeed asks
// for. Its p is GuardP, the predictive chance under no change of a watch this quiet; its effect says it was guarded,
// never that a rate fell.
func Guarded(incidents, exposure, recurrences, watched int, units string) Result {
	r := Result{Before: Counts{Hits: incidents, Base: exposure}, After: Counts{Hits: recurrences, Base: watched}, Guarded: true,
		P: GuardP(incidents, exposure, watched)}
	need := GuardNeed(incidents, exposure)
	r.Improved = recurrences == 0 && need > 0 && watched >= need
	if recurrences > 0 {
		r.P = 1
		r.Effect = fmt.Sprintf("guarded, not a measured drop: its problem came back after the change (%d of %d %s watched); %d incidents in %d %s before it",
			recurrences, watched, units, incidents, exposure, units)
		return r
	}
	r.Effect = fmt.Sprintf("guarded, not a measured drop: no recurrence in %d %s after the change, of the %d its watch needs (%d incidents in %d %s before it); a change that did nothing would have met one with predictive probability %s, p=%.3f",
		watched, units, need, incidents, exposure, units, percent(1-r.P), r.P)
	return r
}

// days is a duration in whole days, or hours under two days.
func days(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func percent(x float64) string { return fmt.Sprintf("%.2f%%", 100*x) }

// tail is P(X <= k) for X hypergeometric: n draws, without replacement, from N units of which K hold the
// problem. Its terms are summed in logs.
func tail(k, N, K, n int) float64 {
	lo, hi := max(0, n-(N-K)), min(n, K)
	switch {
	case k >= hi:
		return 1
	case k < lo:
		return 0
	}
	den := logChoose(N, n)
	sum := 0.0
	for i := lo; i <= k; i++ {
		sum += math.Exp(logChoose(K, i) + logChoose(N-K, n-i) - den)
	}
	return math.Min(sum, 1)
}

func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}
