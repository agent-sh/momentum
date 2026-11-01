package measure

import (
	"math"
	"math/big"
	"testing"
)

// A change helped when its signal's share of the activity fell beyond what chance gives: the after window's units
// that hold the problem, out of both windows', are fewer than its share of the units makes likely at Alpha. A
// quieter week is no fix, a drop from fewer than MinBefore cannot tell from chance, and a problem that is a large
// share of its activity is tested as a share, not as a rate of rare events.
func TestAChangeHelpedWhenItsShareFellBeyondChance(t *testing.T) {
	for _, c := range []struct {
		before, after Counts
		p             float64
		improved      bool
	}{
		{Counts{6, 100}, Counts{0, 100}, 0.014465141011578784, true},
		{Counts{5, 100}, Counts{0, 100}, 0.029691605234293292, true},
		{Counts{4, 100}, Counts{0, 100}, 0.06062036068668214, false}, // below MinBefore no drop tells
		{Counts{6, 100}, Counts{0, 60}, 0.056246784519286865, false}, // a quieter week after: too quiet to tell
		{Counts{6, 100}, Counts{3, 100}, 0.248844699347126, false},
		{Counts{10, 100}, Counts{1, 50}, 0.06766068351825967, false},    // half the activity after: its share, not the before window's
		{Counts{30, 100}, Counts{18, 100}, 0.033955000506876096, true},  // a large share, tested as one
		{Counts{6, 100}, Counts{1, 10}, 0.8776401258351111, false},      // a quieter week, its share up
		{Counts{10, 1000}, Counts{2, 100}, 0.9123446741213653, false},   // fewer failures, more of the activity
		{Counts{40, 1000}, Counts{2, 500}, 7.677353352906547e-06, true}, // half the activity, a tenth of the share
		{Counts{0, 100}, Counts{0, 100}, 1, false},                      // nothing to fix
		{Counts{3, 100}, Counts{9, 100}, 0.9834799453379075, false},     // worse
	} {
		r, ok := Compare(c.before, c.after)
		if !ok || r.Improved != c.improved || math.Abs(r.P-c.p) > 1e-9 {
			t.Errorf("%+v -> %+v: p=%.12f improved=%v ok=%v, want p=%.12f improved=%v", c.before, c.after, r.P, r.Improved, ok, c.p, c.improved)
		}
	}
	if _, ok := Compare(Counts{3, 0}, Counts{0, 100}); ok {
		t.Error("no activity before compared")
	}
	if _, ok := Compare(Counts{3, 100}, Counts{0, 0}); ok {
		t.Error("no activity after compared")
	}
	if r, _ := Compare(Counts{6, 100}, Counts{0, 100}); r.Effect != "6 of 100 active hours before, 0 of 100 after (6.00% to 0.00%), p=0.014" {
		t.Errorf("effect %q", r.Effect)
	}
	if MinBefore != 5 {
		t.Errorf("MinBefore is %d: a drop to none of four, at equal activity, is above Alpha", MinBefore)
	}
}

// A window after can show an effect only when a drop to none would pass: a week 40% quieter after six units
// before cannot, a week as busy can.
func TestAWindowTooQuietToShowAnythingIsNotMeasurable(t *testing.T) {
	for _, c := range []struct {
		before    Counts
		afterBase int
		want      bool
	}{
		{Counts{6, 100}, 60, false},
		{Counts{6, 100}, 100, true},
		{Counts{5, 100}, 80, false},
		{Counts{5, 100}, 100, true},
		{Counts{6, 100}, 0, false},
		{Counts{6, 0}, 100, false},
	} {
		if got := Measurable(c.before, c.afterBase); got != c.want {
			t.Errorf("%+v with %d after: %v, want %v", c.before, c.afterBase, got, c.want)
		}
	}
}

// A problem a change helped is back when its share of the activity is at its old one again, and counts at least
// MinBefore units.
func TestAProblemIsBackAtItsOldShare(t *testing.T) {
	before := Counts{6, 100}
	for _, c := range []struct {
		recent Counts
		want   bool
	}{
		{Counts{6, 100}, true},
		{Counts{6, 90}, true},
		{Counts{12, 200}, true},
		{Counts{5, 100}, false}, // below its old share
		{Counts{4, 40}, false},  // above it, on too few units to tell
		{Counts{11, 200}, false},
	} {
		if got := Back(before, c.recent); got != c.want {
			t.Errorf("%+v after %+v: %v, want %v", c.recent, before, got, c.want)
		}
	}
}

// The hypergeometric's tail is the plain sum of its terms, in exact arithmetic.
func TestTheTailIsItsSum(t *testing.T) {
	for _, c := range []struct{ k, N, K, n int }{{0, 200, 6, 100}, {3, 200, 12, 100}, {1, 150, 11, 50}, {18, 200, 48, 100}, {5, 30, 10, 12}, {12, 40, 12, 20}, {0, 10, 0, 5}} {
		num := new(big.Int)
		for i := max(0, c.n-(c.N-c.K)); i <= min(c.k, c.K, c.n); i++ {
			num.Add(num, new(big.Int).Mul(new(big.Int).Binomial(int64(c.K), int64(i)), new(big.Int).Binomial(int64(c.N-c.K), int64(c.n-i))))
		}
		want, _ := new(big.Rat).SetFrac(num, new(big.Int).Binomial(int64(c.N), int64(c.n))).Float64()
		if got := tail(c.k, c.N, c.K, c.n); math.Abs(got-want) > 1e-12 {
			t.Errorf("P(X<=%d | N=%d, K=%d, n=%d) = %.15f, want %.15f", c.k, c.N, c.K, c.n, got, want)
		}
	}
}
