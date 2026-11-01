package measure

import (
	"math"
	"testing"
)

// A change helped when its signal's rate of its activity fell beyond what chance gives: the after window's count,
// out of both windows', is lower than its share of the activity makes likely at Alpha. A quieter week is no fix,
// and a drop from fewer than MinBefore cannot tell from chance.
func TestAChangeHelpedWhenItsRateFellBeyondChance(t *testing.T) {
	for _, c := range []struct {
		before, after Counts
		p             float64
		improved      bool
	}{
		{Counts{6, 100}, Counts{0, 100}, 1.0 / 64, true},
		{Counts{5, 100}, Counts{0, 100}, 1.0 / 32, true},
		{Counts{4, 100}, Counts{0, 100}, 1.0 / 16, false}, // below MinBefore no drop tells
		{Counts{6, 100}, Counts{3, 100}, 130.0 / 512, false},
		{Counts{6, 100}, Counts{1, 10}, 0, false},    // a quieter week, its rate up
		{Counts{10, 1000}, Counts{2, 100}, 0, false}, // fewer failures, more of the activity
		{Counts{40, 1000}, Counts{2, 500}, 0, true},  // half the activity, a tenth of the rate
		{Counts{0, 100}, Counts{0, 100}, 1, false},   // nothing to fix
		{Counts{3, 100}, Counts{9, 100}, 0, false},   // worse
	} {
		r, ok := Compare(c.before, c.after)
		if !ok || r.Improved != c.improved || c.p != 0 && math.Abs(r.P-c.p) > 1e-9 {
			t.Errorf("%+v -> %+v: p=%.4f improved=%v ok=%v, want p=%.4f improved=%v", c.before, c.after, r.P, r.Improved, ok, c.p, c.improved)
		}
	}
	if _, ok := Compare(Counts{3, 0}, Counts{0, 100}); ok {
		t.Error("no activity before compared")
	}
	if _, ok := Compare(Counts{3, 100}, Counts{0, 0}); ok {
		t.Error("no activity after compared")
	}
	if r, _ := Compare(Counts{6, 100}, Counts{0, 100}); r.Effect != "6 of 100 before, 0 of 100 after (6.00% to 0.00%), p=0.016" {
		t.Errorf("effect %q", r.Effect)
	}
}

// The binomial's tail is the plain sum of its terms.
func TestTheBinomialTailIsItsSum(t *testing.T) {
	choose := func(n, k int) float64 {
		r := 1.0
		for i := 1; i <= k; i++ {
			r = r * float64(n-k+i) / float64(i)
		}
		return r
	}
	for _, c := range []struct {
		k, n int
		q    float64
	}{{0, 6, 0.5}, {3, 9, 0.5}, {1, 7, 10.0 / 110}, {2, 12, 0.3}, {12, 12, 0.3}} {
		want := 0.0
		for i := 0; i <= c.k; i++ {
			want += choose(c.n, i) * math.Pow(c.q, float64(i)) * math.Pow(1-c.q, float64(c.n-i))
		}
		if got := binomialCDF(c.k, c.n, c.q); math.Abs(got-want) > 1e-12 {
			t.Errorf("P(X<=%d | %d, %.3f) = %.12f, want %.12f", c.k, c.n, c.q, got, want)
		}
	}
}
