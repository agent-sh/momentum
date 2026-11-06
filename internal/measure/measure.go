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
// whether the problem comes back. This is the zero-failure test of an exponential (Poisson) process: with incidents
// at a rate λ per unit of exposure, a watch of E = ln(1/Alpha)/λ units holds none with probability Alpha, so a change
// that did nothing would meet a recurrence within E with probability 1-Alpha.

// LongestWindow is the longest window the build step chooses for a measured plan: two weeks, for a rarer problem.
const LongestWindow = 14 * 24 * time.Hour

// MaxWatch is the longest a guard watches after its change: the longest span a measured plan counts after one, two
// of its longest windows (the window after, and the one more it waits when that was too quiet).
const MaxWatch = 2 * LongestWindow

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

// GuardNeed is the exposure a guard's watch must reach with no recurrence: E = ln(1/Alpha)/λ, with λ the incidents
// per unit of exposure, rounded up to a whole unit. With no incident or no exposure there is no rate, and it is 0.
func GuardNeed(incidents, exposure int) int {
	if incidents <= 0 || exposure <= 0 {
		return 0
	}
	return int(math.Ceil(math.Log(1/Alpha) * float64(exposure) / float64(incidents)))
}

// GuardP is the chance, under no change, that a watch of watched units of exposure holds no recurrence: exp(-λ watched).
func GuardP(incidents, exposure, watched int) float64 {
	if incidents <= 0 || exposure <= 0 {
		return 1
	}
	return math.Exp(-float64(incidents) / float64(exposure) * float64(watched))
}

// Guarded is a guard's watch as a result: incidents in a history of exposure units before the change, recurrences
// in watched units after it. It is improved only with no recurrence and the exposure GuardNeed asks for. Its p is
// GuardP, the chance under no change of a watch this quiet; its effect says it was guarded, never that a rate fell.
func Guarded(incidents, exposure, recurrences, watched int) Result {
	r := Result{Before: Counts{Hits: incidents, Base: exposure}, After: Counts{Hits: recurrences, Base: watched}, Guarded: true,
		P: GuardP(incidents, exposure, watched)}
	need := GuardNeed(incidents, exposure)
	r.Improved = recurrences == 0 && need > 0 && watched >= need
	if recurrences > 0 {
		r.P = 1
		r.Effect = fmt.Sprintf("guarded, not a measured drop: its problem came back after the change (%d of %d active hours watched); %d incidents in %d active hours before it",
			recurrences, watched, incidents, exposure)
		return r
	}
	r.Effect = fmt.Sprintf("guarded, not a measured drop: no recurrence in %d active hours after the change, of the %d its watch needs (%d incidents in %d active hours before it); a change that did nothing would have met one with probability %s, p=%.3f",
		watched, need, incidents, exposure, percent(1-r.P), r.P)
	return r
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
