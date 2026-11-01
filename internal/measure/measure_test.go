package measure

import (
	"math"
	"math/big"
	"math/rand"
	"sort"
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
		if got := Back(before, Counts{0, 100}, c.recent, 1); got != c.want {
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

// The gate selects the last window, disjoint from the three baseline windows. Enumerating all binomial
// outcomes includes plan admission, Compare and the quiet-window rule. No sampling error hides a breach.
func TestTheGatedBaselineKeepsAtMostFivePercentOfUnchangedProblems(t *testing.T) {
	if Alpha != 0.05 || BaselineWindows != 3 {
		t.Fatal("the test uses a 5% level and three baseline windows")
	}
	for _, units := range []int{50, 100, 200} {
		for share := 1; share <= 12; share++ {
			p := float64(share) / 100
			gate, older := binomial(units, p), binomial(units*BaselineWindows, p)
			gatePass := 0.0
			for g := MinBefore; g <= units; g++ {
				gatePass += gate[g]
			}
			one, two := binomial(units, p), binomial(2*units, p)
			kept, admitted, removed := 0.0, 0.0, 0.0
			for hits, probability := range older {
				before := Counts{hits, units * BaselineWindows}
				if !Measurable(before, 2*units) {
					continue
				}
				weight := gatePass * probability
				admitted += weight
				afterBase, outcomes := units, one
				if !Measurable(before, afterBase) {
					afterBase, outcomes = 2*units, two
				}
				for a, probability := range outcomes {
					res, ok := Compare(before, Counts{a, afterBase})
					if !ok || !res.Improved {
						break
					}
					kept += weight * probability
				}
				res, _ := Compare(before, Counts{0, afterBase})
				if res.Improved {
					removed += weight
				}
			}
			if units == 100 {
				floor := map[int]float64{2: 0.555, 3: 0.887, 5: 0.997}[share]
				if removed/gatePass < floor {
					t.Errorf("share %d%%: removal power %.4f%% below %.1f%%", share, 100*removed/gatePass, 100*floor)
				}
			}
			rate := kept / gatePass
			t.Logf("%d units, share %d%%: false keep %.4f%% of gated plans; %.4f%% of admitted plans; removal power %.4f%% of gated plans", units, share, 100*rate, 100*kept/admitted, 100*removed/gatePass)
			if rate > Alpha {
				t.Errorf("%d units, share %d%%: false keep %.4f%% exceeds 5%%", units, share, 100*rate)
			}
		}
	}
}

// binomial is the distribution of hits in n independent units at share p.
func binomial(n int, p float64) []float64 {
	out := make([]float64, n+1)
	out[0] = math.Pow(1-p, float64(n))
	for i := 1; i <= n; i++ {
		out[i] = out[i-1] * float64(n-i+1) / float64(i) * p / (1 - p)
	}
	return out
}

// Watching spends its 5% level over every window, never a fresh 5% on each run. A fixed seed pins false
// comebacks of a halved problem over 26 windows, pooling every earlier window that did not bring it back.
func TestWatchingAHalvedProblemSpendsAtMostFivePercent(t *testing.T) {
	rate := watchRate(0.05, 26)
	t.Logf("halved problem: false comeback within 26 windows %.4f%%", 100*rate)
	if rate > Alpha {
		t.Fatalf("false comeback %.4f%% exceeds 5%%", 100*rate)
	}
}

// watchRate simulates 100000 independent paths from a measured drop from 10% to 5%, at 100 units a window.
func watchRate(share float64, windows int) float64 {
	const draws = 100000
	rng := rand.New(rand.NewSource(431))
	cdf := binomial(100, share)
	for i := 1; i < len(cdf); i++ {
		cdf[i] += cdf[i-1]
	}
	decisions := map[[3]int]bool{}
	found := 0
	for i := 0; i < draws; i++ {
		reference := Counts{5, 100}
		for j := 1; j <= windows; j++ {
			hits := sort.SearchFloat64s(cdf, rng.Float64())
			key := [3]int{j, reference.Hits, hits}
			back, ok := decisions[key]
			if !ok {
				back = Back(Counts{30, 300}, reference, Counts{hits, 100}, j)
				decisions[key] = back
			}
			if back {
				found++
				break
			}
			reference.Hits += hits
			reference.Base += 100
		}
	}
	return float64(found) / draws
}

// The same count can pass the first watch level and fail the second. Watch uses the upper tail: a share that
// fell since measurement is not a comeback, even if it reaches the older baseline.
func TestEachWatchWindowSpendsItsOwnLevel(t *testing.T) {
	before, after, recent := Counts{6, 100}, Counts{0, 100}, Counts{6, 100}
	if !Back(before, after, recent, 1) || Back(before, after, recent, 2) || Back(before, after, recent, 0) {
		t.Fatal("watch level did not shrink from .025 to .0125")
	}
	if Back(before, Counts{20, 100}, recent, 1) {
		t.Fatal("a falling share came back")
	}
}

// A return to the old 10% share has limited power at 100 units a window. The fixed seed pins the measured
// detection rate within six completed windows, with the preceding quiet windows pooled into the reference.
func TestWatchDetectsAReturnAtItsMeasuredRate(t *testing.T) {
	rate := watchRate(0.10, 6)
	t.Logf("return to 10%%: %.4f%% detected within six windows", 100*rate)
	if rate < 0.128 {
		t.Fatalf("return detected in %.4f%%, below the measured floor", 100*rate)
	}
}
