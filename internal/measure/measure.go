// Package measure decides whether a deployed change helped: deterministic, no model. A change's signal (what its
// case's problem is in the store) is counted over a window before the change and over as long after it, each with
// the activity it is a part of: the events of the signal's source, kinds, tool and harness, matched or not. A
// quieter week after a change is not a fix, so the two counts are compared as rates of that activity.
//
// The test is the exact conditional test of two Poisson rates: under no change, the share of the problem's events
// that fall after the change is binomial, with the after window's share of the activity as its chance. The change
// helped when the after count is lower than that share makes likely (a one-sided p-value below Alpha) and its rate
// fell.
package measure

import (
	"fmt"
	"math"
)

// Alpha is the test's level: an effect this unlikely under no change is taken as the change's.
const Alpha = 0.05

// MinBefore is the least the signal must count before a change for any drop to tell from chance at Alpha: with
// as much activity after as before, a drop to none of five has a chance of 1 in 32 under no change, and of four 1 in
// 16, above Alpha.
const MinBefore = 5

// Counts are a window's: the events the signal counts, and the activity they are a part of.
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
	Before, After Counts
	P             float64 // the chance, under no change, of an after count this low or lower
	Improved      bool
	Effect        string // the effect in words
}

// Compare decides whether the rate fell from before to after. With no activity in either window there is nothing
// to compare, and ok is false.
func Compare(before, after Counts) (Result, bool) {
	r := Result{Before: before, After: after, P: 1}
	if before.Base == 0 || after.Base == 0 {
		return r, false
	}
	n := before.Hits + after.Hits
	q := float64(after.Base) / float64(after.Base+before.Base)
	if n > 0 {
		r.P = binomialCDF(after.Hits, n, q)
	}
	r.Improved = r.P < Alpha && after.Rate() < before.Rate()
	r.Effect = fmt.Sprintf("%d of %d before, %d of %d after (%s to %s), p=%.3f", before.Hits, before.Base, after.Hits, after.Base,
		percent(before.Rate()), percent(after.Rate()), r.P)
	return r, true
}

func percent(x float64) string { return fmt.Sprintf("%.2f%%", 100*x) }

// binomialCDF is P(X <= k) for X binomial over n tries at chance q, summed in logs.
func binomialCDF(k, n int, q float64) float64 {
	if k >= n {
		return 1
	}
	if q <= 0 {
		return 1
	}
	if q >= 1 {
		if k >= n {
			return 1
		}
		return 0
	}
	lq, lp := math.Log(q), math.Log1p(-q)
	lnN, _ := math.Lgamma(float64(n + 1))
	sum := 0.0
	for i := 0; i <= k; i++ {
		li, _ := math.Lgamma(float64(i + 1))
		lni, _ := math.Lgamma(float64(n - i + 1))
		sum += math.Exp(lnN - li - lni + float64(i)*lq + float64(n-i)*lp)
	}
	return math.Min(sum, 1)
}
