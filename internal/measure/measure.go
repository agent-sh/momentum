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
	// Guarded marks a guard's watch, not a measurement: Before holds the history's incidents and its exposure, After
	// the watch's recurrences and its exposure, and Improved is never set; P, Fisher's for a measured plan, stays 0.
	// Recur is the predictive probability that the problem, had it kept happening, would have recurred within the
	// exposure watched (GuardRecur): 0 when the history gives no rate (NoRate).
	Guarded bool    `json:"guarded,omitempty"`
	Recur   float64 `json:"recur,omitempty"`
	NoRate  bool    `json:"no_rate,omitempty"`
	// Unwatched says its unit was not used in the watch at all: it watched nothing, and gives no P.
	Unwatched bool `json:"unwatched,omitempty"`
	// Exposure and Watched are a guard's: the units of exposure its history's incidents came in, and those its watch
	// saw.
	Exposure int `json:"exposure,omitempty"`
	Watched  int `json:"watched,omitempty"`
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

// A guard stands in for a measured plan when the problem has stopped, or is too rare for a share to fall. It is a
// recurrence watch, not a proof, and nothing here decides it: the loop watches for the problem to come back. What
// this reports is P: with its incidents a Poisson process in units of exposure, k of them in T units before the change,
// Jeffreys' prior gives the rate the posterior Gamma(k+½, T), and a watch of W units would have held a recurrence with
// predictive probability 1 − (T/(T+W))^(k+½), had the problem kept happening.

// Incident is a run of a signal's events with no gap longer than the guard's gap between two of them: events close
// together share a cause, so one burst of hundreds is one incident.
type Incident struct {
	First, Last time.Time
	Events      int
}

// GuardGap is the longest quiet stretch inside one incident: a week, the weekly cycle the whole-weeks rule exists for.
// A shorter one can be its lull (a night, a weekend) rather than the end of a cause.
const GuardGap = 7 * 24 * time.Hour

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

// GuardRecur is the predictive probability that the problem, had it kept happening, would have recurred within
// watched units of exposure, after incidents in exposure units before: 1 − (T/(T+W))^(k+½). It assumes the problem was
// still happening when the change took effect: a problem that stopped by itself shows the same quiet. With no exposure
// or no incident there is no rate, and it is 0.
func GuardRecur(incidents, exposure, watched int) float64 {
	if exposure <= 0 || incidents <= 0 {
		return 0
	}
	return 1 - math.Pow(float64(exposure)/float64(exposure+watched), float64(incidents)+0.5)
}

// GuardHistory is what a guard knows of its problem before the change: from Since, its incidents in its units of
// exposure.
type GuardHistory struct {
	Since     time.Time
	Incidents int
	Exposure  int
}

// GuardCount is a guard's history from since on, from its incidents and the hours of its units of exposure (one entry
// for each unit, its hour since the epoch). When the history starts at the signal's source's first event (anchored),
// the incident that holds that event (its first event within gap of it) is no draw of the problem's rate: the source
// began with it. It is dropped, and the history starts at the hour after its last event.
func GuardCount(incidents []Incident, units []int64, since time.Time, gap time.Duration, anchored bool) GuardHistory {
	if anchored && len(incidents) > 0 && incidents[0].First.Sub(since) <= gap {
		since = incidents[0].Last.Truncate(time.Hour).Add(time.Hour)
		incidents = incidents[1:]
	}
	h := GuardHistory{Since: since, Incidents: len(incidents)}
	from := since.UnixMilli() / 3_600_000
	for _, u := range units {
		if u >= from {
			h.Exposure++
		}
	}
	return h
}

// Guarded is a guard's watch as a result: incidents in a history of exposure units before the change, recurrences in
// watched units after it, counted in units. It is never improved: a guard makes no claim, and its p stays 0. With no
// recurrence its effect says it was guarded, with the exposure watched and P (Recur), had the problem kept happening,
// or that there is no rate to compare, and that a problem that stopped by itself shows the same.
func Guarded(incidents, exposure, recurrences, watched int, units string) Result {
	r := Result{Before: Counts{Hits: incidents, Base: exposure}, After: Counts{Hits: recurrences, Base: watched}, Guarded: true, Exposure: exposure,
		Watched: watched}
	if recurrences > 0 {
		r.Effect = fmt.Sprintf("guarded: its problem came back after the change (%d of %d %s watched); %d incidents in %d %s before it",
			recurrences, watched, units, incidents, exposure, units)
		return r
	}
	if watched == 0 {
		r.Unwatched, r.NoRate = true, true
		r.Effect = fmt.Sprintf("guarded, not watched: the unit was not used in the 30 days after the change (%d incidents in %d %s before it); there is no P",
			incidents, exposure, units)
		return r
	}
	chance := "no rate to compare: the history holds no incident after its first"
	if r.Recur = GuardRecur(incidents, exposure, watched); incidents > 0 && exposure > 0 {
		chance = fmt.Sprintf("P=%s that the problem, had it kept happening, would have recurred within them", percent(r.Recur))
	} else {
		r.NoRate = true
	}
	r.Effect = fmt.Sprintf("guarded, not measured: no recurrence in %d %s watched after the change (%d incidents in %d %s before it); %s; a problem that stopped by itself shows the same",
		watched, units, incidents, exposure, units, chance)
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
