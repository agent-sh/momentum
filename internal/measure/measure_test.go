package measure

import (
	"math"
	"math/big"
	"math/rand"
	"sort"
	"testing"
	"time"
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

// A comeback needs three consecutive windows at the old share and five hit units among them.
func TestAProblemIsBackAfterThreeWindowsAtItsOldShare(t *testing.T) {
	for _, c := range []struct {
		before  Counts
		windows []Counts
		want    bool
	}{
		{Counts{6, 100}, []Counts{{6, 100}, {6, 100}, {6, 100}}, true},
		{Counts{6, 100}, []Counts{{6, 100}, {6, 100}}, false},
		{Counts{6, 100}, []Counts{{6, 100}, {5, 100}, {6, 100}}, false},
		{Counts{6, 100}, []Counts{{6, 100}, {0, 0}, {6, 100}}, false},
		{Counts{6, 100}, []Counts{{1, 10}, {1, 10}, {2, 20}}, false},
		{Counts{6, 100}, []Counts{{1, 10}, {2, 20}, {2, 20}}, true},
		{Counts{}, []Counts{{6, 100}, {6, 100}, {6, 100}}, false},
	} {
		if got := Back(c.before, c.windows); got != c.want {
			t.Errorf("%+v: %v", c, got)
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
			if want, ok := map[[2]int]float64{{50, 1}: 36.8, {50, 2}: 14.4, {50, 3}: 6.7, {100, 1}: 14.6}[[2]int{units, share}]; ok && math.Abs(100*kept/admitted-want) > 0.06 {
				t.Errorf("admitted rate %.4f%%, want %.1f%%", 100*kept/admitted, want)
			}
			if math.Abs(removed/admitted-1) > 1e-12 {
				t.Errorf("removal detection among admitted plans: %.4f%%", 100*removed/admitted)
			}
			if units == 200 && share == 12 && math.Abs(100*kept/gatePass-3.7719) > 0.0001 {
				t.Errorf("grid maximum: %.4f%%", 100*kept/gatePass)
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

// A fixed seed pins false comebacks of a halved problem and detection at the old share, even after quiet windows.
func TestWatchingThreeWindowsSeparatesAHalvedProblemFromAReturn(t *testing.T) {
	for _, old := range []int{10, 6, 5, 4} {
		falseRate := watchRate(old, float64(old)/200, 26, 0)
		detection := watchRate(old, float64(old)/100, 26, 0)
		late := watchRate(old, float64(old)/100, 26, 26)
		early := watchRate(old, float64(old)/100, 6, 0)
		t.Logf("old %d%%: halved comeback %.3f%%; detection in 6 %.3f%%, in 26 %.3f%%, late %.3f%%", old, 100*falseRate, 100*early, 100*detection, 100*late)
		if old >= 5 && falseRate > 0.05 {
			t.Errorf("false comeback %.4f exceeds 5%%", falseRate)
		}
		if detection < 0.90 || late < 0.90 {
			t.Errorf("return detection below 90%%")
		}
		if early < 0.35 || early > 0.50 {
			t.Errorf("six-window detection %.4f", early)
		}
		want := map[int]float64{10: 0.001, 6: 0.013, 5: 0.028, 4: 0.058}[old]
		if math.Abs(falseRate-want) > 0.005 {
			t.Errorf("halved comeback %.4f differs from %.4f", falseRate, want)
		}
	}
}

func watchRate(old int, share float64, windows, delay int) float64 {
	const draws = 100000
	rng := rand.New(rand.NewSource(431))
	cdf := binomial(100, share)
	for i := 1; i < len(cdf); i++ {
		cdf[i] += cdf[i-1]
	}
	found := 0
	for i := 0; i < draws; i++ {
		var recent []Counts
		for j := 0; j < windows+delay; j++ {
			hits := 0
			if j >= delay {
				hits = sort.SearchFloat64s(cdf, rng.Float64())
			}
			recent = append(recent, Counts{hits, 100})
			if len(recent) > 3 {
				recent = recent[1:]
			}
			if Back(Counts{3 * old, 300}, recent) {
				found++
				break
			}
		}
	}
	return float64(found) / draws
}

// Whole weeks compare the same mix of weekdays and weekends. The full gate, admission and quiet-window rule
// keep fewer than five percent of admitted unchanged problems under each weekly cycle.
func TestWholeWeeksKeepTheWeeklyCycleBelowFivePercent(t *testing.T) {
	for _, cycle := range [][2]float64{{0.05, 0.01}, {0.04, 0.01}, {0.05, 0}} {
		rng := rand.New(rand.NewSource(77))
		cdfs := map[bool][]float64{}
		for _, weekday := range []bool{true, false} {
			n, p := 60, cycle[1]
			if weekday {
				n, p = 100, cycle[0]
			}
			cdf := binomial(n, p)
			for i := 1; i < len(cdf); i++ {
				cdf[i] += cdf[i-1]
			}
			cdfs[weekday] = cdf
		}
		day := func(d int) Counts {
			weekday := ((d%7)+7)%7 < 5
			n := 60
			if weekday {
				n = 100
			}
			return Counts{sort.SearchFloat64s(cdfs[weekday], rng.Float64()), n}
		}
		cache := map[[4]int]bool{}
		measurable := map[[3]int]bool{}
		can := func(b Counts, n int) bool {
			key := [3]int{b.Hits, b.Base, n}
			v, ok := measurable[key]
			if !ok {
				v = Measurable(b, n)
				measurable[key] = v
			}
			return v
		}
		admitted, kept := 0, 0
		for i := 0; i < 40000; i++ {
			at := 60 + rng.Intn(7)
			hist := map[int]Counts{}
			for d := at - 56; d < at; d++ {
				hist[d] = day(d)
			}
			for w := 1; w <= 14; w++ {
				if !WholeWeeks(time.Duration(w) * 24 * time.Hour) {
					continue
				}
				before, gate := Counts{}, Counts{}
				for d := at - 4*w; d < at; d++ {
					c := hist[d]
					if d < at-w {
						before.Hits += c.Hits
						before.Base += c.Base
					} else {
						gate.Hits += c.Hits
						gate.Base += c.Base
					}
				}
				if gate.Hits < MinBefore || !can(before, 2*gate.Base) {
					continue
				}
				start := at + 1 + rng.Intn(3)
				after := Counts{}
				for d := start; d < start+w; d++ {
					c := day(d)
					after.Hits += c.Hits
					after.Base += c.Base
				}
				if !can(before, after.Base) {
					for d := start + w; d < start+2*w; d++ {
						c := day(d)
						after.Hits += c.Hits
						after.Base += c.Base
					}
				}
				key := [4]int{before.Hits, before.Base, after.Hits, after.Base}
				keep, ok := cache[key]
				if !ok {
					res, _ := Compare(before, after)
					keep = res.Improved
					cache[key] = keep
				}
				admitted++
				if keep {
					kept++
				}
				break
			}
		}
		rate := float64(kept) / float64(admitted)
		t.Logf("weekday %.0f%%, weekend %.0f%%: %d admitted, false keep %.3f%%", 100*cycle[0], 100*cycle[1], admitted, 100*rate)
		if admitted < 39000 || rate > 0.05 || rate < 0.025 {
			t.Fatalf("weekly cycle: %d admitted, %.4f kept", admitted, rate)
		}
	}
}
