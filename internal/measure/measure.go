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

// Back tests a completed watch window against the pooled reference, by Fisher's upper tail.
// The reference holds the measured window after and every earlier window that did not bring the case back.
// Window j spends Alpha / 2^j, starting at one. The levels sum to Alpha over all windows.
// Its share must also reach the baseline's, in at least MinBefore units.
func Back(before, reference, recent Counts, window int) bool {
	if window < 1 || before.Base == 0 || reference.Base == 0 || recent.Base == 0 ||
		recent.Hits < MinBefore || recent.Rate() < before.Rate() || recent.Rate() <= reference.Rate() {
		return false
	}
	// Exchange the windows: the lower tail of the reference count is the upper tail of the recent count.
	p := tail(reference.Hits, reference.Base+recent.Base, reference.Hits+recent.Hits, reference.Base)
	return p < math.Ldexp(Alpha, -window)
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
