package measure

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// The figures SPEC's Measurement section quotes are exact rates of the step's own decisions, enumerated over every outcome
// of the windows - no sampling, no seed. Each is recomputed here with Measurable and Compare (the watch's with an automaton
// over Back's rule), must be the figure SPEC prints, and SPEC must say it where it says it. They assume what SPEC says
// they assume: each unit of a window holds the problem independently of the others - but for the clustered rates, which
// say what hours that cluster in sessions do.

// pmf is a distribution of the hits in a window: pmf[k] is the chance of k units holding the problem.
type pmf []float64

func convolve(a, b pmf) pmf {
	out := make(pmf, len(a)+len(b)-1)
	for i, x := range a {
		if x == 0 {
			continue
		}
		for j, y := range b {
			out[i+j] += x * y
		}
	}
	return out
}

// bin is units that hold the problem independently, each at a share.
func bin(units int, share float64) pmf {
	if share >= 1 {
		out := make(pmf, units+1)
		out[units] = 1
		return out
	}
	return binomial(units, share)
}

// sessionsOf is a window of sessions of hours units each: a session is prone with probability pi, and a prone session holds
// the problem in each of its hours with probability h. A session that hits a wall hits it again.
func sessionsOf(sessions, hours int, pi, h float64) pmf {
	one := bin(hours, h)
	for i := range one {
		one[i] *= pi
	}
	one[0] += 1 - pi
	out := pmf{1}
	for i := 0; i < sessions; i++ {
		out = convolve(out, one)
	}
	return out
}

// flow is the step's path for a problem the plan was counted on: the baseline, three windows before the gated one, and the
// window after - one, or two when the first is too quiet to show anything. The gate window (five hit units) is chosen by
// chance and is independent of the other windows, so it does not move the rates among the plans the baseline admits.
type flow struct {
	units              int // the units of one window
	baseline           pmf
	afterOne, afterTwo pmf
}

// floor is the chance below which an outcome is not walked.
const floor = 1e-16

// rates are the share of the plans whose gate reached five hit units that the baseline admits (a drop to none over two
// windows at the gate's activity would pass), and, of the admitted plans, the share kept: the change helped, by the test as
// it decides.
func (f flow) rates() (admitted, kept float64) {
	adm, keep := 0.0, 0.0
	for hits, pb := range f.baseline {
		if pb < floor {
			continue
		}
		before := Counts{hits, BaselineWindows * f.units}
		if !Measurable(before, 2*f.units) {
			continue
		}
		adm += pb
		afterBase, outcomes := f.units, f.afterOne
		if !Measurable(before, afterBase) {
			afterBase, outcomes = 2*f.units, f.afterTwo
		}
		for a, pa := range outcomes {
			if pa < floor {
				continue
			}
			if res, ok := Compare(before, Counts{a, afterBase}); ok && res.Improved {
				keep += pb * pa
			}
		}
	}
	return adm, keep / adm
}

// independent is a problem at a share in every unit, before and after.
func independent(units int, before, after float64) flow {
	return flow{units, bin(BaselineWindows*units, before), bin(units, after), bin(2*units, after)}
}

// drifting is a problem whose share falls by itself by drift a week: the weeks go back from the window after at its share,
// the plan is delay days before the change, and the second window after goes on from the first.
func drifting(units int, after, drift float64, delayDays float64) flow {
	back := 1 / (1 - drift)
	share := func(week int) float64 { return math.Min(0.999, after*math.Pow(back, float64(week)+delayDays/7)) }
	baseline := bin(units, share(2))
	baseline = convolve(baseline, bin(units, share(3)))
	baseline = convolve(baseline, bin(units, share(4)))
	one := bin(units, after)
	return flow{units, baseline, one, convolve(one, bin(units, after/back))}
}

// weekly is a weekly cycle: 100 units on each of five weekdays at one share, 60 on each of two weekend days at another; a
// window of whole weeks holds every day of the week the same number of times.
func weekly(weekday, weekend float64) flow {
	week := convolve(bin(500, weekday), bin(120, weekend))
	return flow{620, convolve(convolve(week, week), week), week, convolve(week, week)}
}

func spec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// figure is a rate SPEC prints: the words around it, which hold the figure as printed.
type figure struct {
	printed, around string
}

// quote checks that a rate, as a percentage of decimals places, is the figure SPEC prints, and that SPEC says the words
// around it.
func quote(t *testing.T, doc, what string, rate float64, decimals int, f figure) {
	t.Helper()
	if got := fmt.Sprintf("%.*f%%", decimals, 100*rate); got != f.printed {
		t.Errorf("%s: %.6f%% prints as %s, not %s", what, 100*rate, got, f.printed)
	}
	if !strings.Contains(f.around, f.printed) || !strings.Contains(doc, f.around) {
		t.Errorf("%s: SPEC does not say %q", what, f.around)
	}
}

func TestSpecIndependentFalseKeepRatesAndGridMaximum(t *testing.T) {
	doc := spec(t)
	const admitted = "false-keep rates are 36.8%, 14.4% and 6.7% at 50 units and shares of 1%, 2% and 3%, and 14.6% at 100 units and a 1% share"
	for _, c := range []struct {
		units, share int
		printed      string
	}{{50, 1, "36.8%"}, {50, 2, "14.4%"}, {50, 3, "6.7%"}, {100, 1, "14.6%"}} {
		_, kept := independent(c.units, float64(c.share)/100, float64(c.share)/100).rates()
		quote(t, doc, fmt.Sprintf("%d units, %d%% share among admitted plans", c.units, c.share), kept, 1, figure{c.printed, admitted})
	}
	maximum := 0.0
	for _, units := range []int{50, 100, 200} {
		for share := 1; share <= 12; share++ {
			p := float64(share) / 100
			admission, kept := independent(units, p, p).rates()
			maximum = math.Max(maximum, admission*kept)
		}
	}
	quote(t, doc, "maximum among gate-passing plans", maximum, 1, figure{"3.8%", "at most 3.8% on this grid"})
}

// The false-keep rates of a problem nothing changed, among the plans the baseline admits, when its hours hold it
// independently, and when they cluster in sessions. A window is 10 sessions of 10 session hours and a 5% share; a session
// is prone with probability pi and holds the problem in each hour with probability h, so that pi * h is the share. The
// independent figure is the one the grid test pins; the others are what hits repeating inside a session do to it.
func TestSpecFalseKeepRatesWhenHoursClusterInSessions(t *testing.T) {
	doc := spec(t)
	for _, c := range []struct {
		pi, h float64
		figure
		decimals int
	}{
		{1, 0.05, figure{"2.65%", "2.65% of them with independent hours"}, 2},
		{0.5, 0.10, figure{"5.73%", "5.73% for `pi` 50% and `h` 10%"}, 2},
		{0.25, 0.20, figure{"12.6%", "12.6% for 25% and 20%"}, 1},
		{0.10, 0.50, figure{"32.3%", "32.3% for 10% and 50%"}, 1},
		{0.05, 1, figure{"49.6%", "49.6% for 5% and 100%"}, 1},
	} {
		f := flow{100, sessionsOf(30, 10, c.pi, c.h), sessionsOf(10, 10, c.pi, c.h), sessionsOf(20, 10, c.pi, c.h)}
		_, kept := f.rates()
		quote(t, doc, fmt.Sprintf("prone sessions %.0f%%, hours %.0f%%", 100*c.pi, 100*c.h), kept, c.decimals, c.figure)
		if c.pi == 1 { // hours that are independent: the clustered model at pi = 1 is the binomial one
			if _, want := independent(100, 0.05, 0.05).rates(); math.Abs(kept-want) > 1e-12 {
				t.Errorf("independent hours as sessions: %.9f, as units %.9f", kept, want)
			}
		}
	}
	for _, say := range []string{"The test assumes independent units", "every rate below is a floor, not a bound", "the rates above are floors for it"} {
		if !strings.Contains(doc, say) {
			t.Errorf("SPEC does not say %q", say)
		}
	}
}

// What the step does to a real partial fix, among the plans the baseline admits: the share of them it keeps, when the change
// cuts the problem's share to a fraction of its old one. A change that helped by half is kept less often than not.
func TestSpecKeepRatesOfARealPartialFix(t *testing.T) {
	doc := spec(t)
	for _, c := range []struct {
		units       int
		old, factor float64
		figure
	}{
		{100, 0.05, 0.5, figure{"18.4%", "a 5% problem cut in half is kept 18.4% of the time"}},
		{100, 0.05, 0.25, figure{"45.2%", "(cut to a quarter, 45.2%)"}},
		{100, 0.10, 0.5, figure{"38.7%", "a 10% problem cut in half 38.7%"}},
		{300, 0.05, 0.5, figure{"56.0%", "a 5% problem cut in half is kept 56.0%"}},
	} {
		_, kept := independent(c.units, c.old, c.old*c.factor).rates()
		quote(t, doc, fmt.Sprintf("%d units, %.0f%% cut to %.2g of it", c.units, 100*c.old, c.factor), kept, 1, c.figure)
	}
	// removal is kept in every plan the baseline admits
	for _, units := range []int{50, 100, 200} {
		for share := 1; share <= 12; share++ {
			if _, kept := independent(units, float64(share)/100, 0).rates(); math.Abs(kept-1) > 1e-12 {
				t.Errorf("%d units, share %d%%: a removed problem is kept in %.6f%% of admitted plans", units, share, 100*kept)
			}
		}
	}
}

// The share of plans that reached the gate which the baseline admits, at 100 units a week.
func TestSpecBaselineAdmission(t *testing.T) {
	doc := spec(t)
	for _, c := range []struct {
		share float64
		figure
	}{
		{0.02, figure{"55.6%", "baseline admits about 55.6% of gate-passing cases at a 2% share"}},
		{0.03, figure{"88.8%", "88.8% at 3%"}},
	} {
		admitted, _ := independent(100, c.share, c.share).rates()
		quote(t, doc, fmt.Sprintf("share %.0f%%", 100*c.share), admitted, 1, c.figure)
	}
	if admitted, _ := independent(100, 0.05, 0.05).rates(); admitted < 0.99 || !strings.Contains(doc, "and over 99% at 5%") {
		t.Errorf("a 5%% share: the baseline admits %.4f%%, SPEC says over 99%%", 100*admitted)
	}
}

// A problem drifting down by itself reads as a fix: the share of unchanged plans kept, at 100, 300 and 600 units a week,
// when the change is in effect at the plan and two days after it.
func TestSpecDriftRates(t *testing.T) {
	doc := spec(t)
	for _, c := range []struct {
		drift, delay float64
		printed      [3]string
		around       string
	}{
		{0.03, 0, [3]string{"4.0%", "6.8%", "9.6%"}, "a 3% weekly drift keeps 4.0%, 6.8% and 9.6% of unchanged plans at 100, 300 and 600 units a week"},
		{0.05, 0, [3]string{"5.3%", "10.2%", "16.4%"}, "a 5% drift keeps 5.3%, 10.2% and 16.4%"},
		{0.03, 2, [3]string{"4.1%", "7.2%", "10.4%"}, "at two days, they are 4.1%, 7.2% and 10.4%"},
		{0.05, 2, [3]string{"5.6%", "11.2%", "18.4%"}, "and 5.6%, 11.2% and 18.4%"},
	} {
		for i, units := range []int{100, 300, 600} {
			_, kept := drifting(units, 0.05, c.drift, c.delay).rates()
			quote(t, doc, fmt.Sprintf("drift %.0f%%, %.0f days, %d units", 100*c.drift, c.delay, units), kept, 1, figure{c.printed[i], c.around})
		}
	}
}

// Whole weeks keep a weekly cycle (100 weekday units at 4% or 5%, 60 weekend units at 0% or 1%) near 4% of admitted
// unchanged plans: SPEC says about 4%.
func TestSpecWeeklyCycle(t *testing.T) {
	doc := spec(t)
	for _, c := range []struct{ weekday, weekend float64 }{{0.05, 0.01}, {0.04, 0.01}, {0.05, 0}} {
		_, kept := weekly(c.weekday, c.weekend).rates()
		if kept < 0.0375 || kept > 0.0385 {
			t.Errorf("weekday %.0f%%, weekend %.0f%%: %.4f%% of admitted plans kept", 100*c.weekday, 100*c.weekend, 100*kept)
		}
	}
	if !strings.Contains(doc, "(about 4%)") {
		t.Error("SPEC does not say about 4%")
	}
}

// backWithin is the chance that the watch brings a case back within a number of windows of 100 units each, whose hits are
// binomial at a share: three consecutive windows each at the baseline's share or above (hits * baseline units at least
// baseline hits * 100), with five hit units among the three - the rule of Back, walked as an automaton over the last two
// windows (TestTheWatchAutomatonIsTheRuleOfBack says it is Back's).
func backWithin(before Counts, share float64, windows int) float64 {
	const units = 100
	t := 0
	for t*before.Base < before.Hits*units {
		t++
	}
	// an outcome is below the threshold (-1), or its hits, up to MinBefore
	class := func(h int) int {
		if h < t {
			return -1
		}
		return min(h, MinBefore)
	}
	chance := map[int]float64{}
	for h, x := range bin(units, share) {
		chance[class(h)] += x
	}
	type state struct{ a, b int } // the last two windows' classes; -2 when there is no such window yet
	now := map[state]float64{{-2, -2}: 1}
	found := 0.0
	for w := 0; w < windows; w++ {
		next := map[state]float64{}
		for s, pr := range now {
			for c, x := range chance {
				if s.a >= 0 && s.b >= 0 && c >= 0 && s.a+s.b+c >= MinBefore {
					found += pr * x
					continue
				}
				next[state{s.b, c}] += pr * x
			}
		}
		now = next
	}
	return found
}

// The watch's rates among the plans the step admits and keeps, at 100 units a window: a problem it removed that returns to its
// old share is caught, within 26 windows and within six; a halved one that returns to its old share; and a halved one that
// stays halved, which is the false comeback. Each walks every baseline the plan can have been admitted and kept on.
func TestSpecWatchRates(t *testing.T) {
	doc := spec(t)
	// among admitted and kept plans, with the baseline as counted: the chance of a comeback within some windows
	watched := func(old, after, watch float64, windows int) float64 {
		const units = 100
		kept, caught := 0.0, 0.0
		for hits, pb := range bin(3*units, old) {
			if pb < floor {
				continue
			}
			before := Counts{hits, 3 * units}
			if !Measurable(before, 2*units) {
				continue
			}
			n, outcomes := units, bin(units, after)
			if !Measurable(before, n) {
				n, outcomes = 2*units, bin(2*units, after)
			}
			keep := 0.0
			for a, pa := range outcomes {
				if res, ok := Compare(before, Counts{a, n}); ok && res.Improved {
					keep += pa
				}
			}
			kept += pb * keep
			caught += pb * keep * backWithin(before, watch, windows)
		}
		return caught / kept
	}
	const returns = "is caught within 26 windows about 74% of the time (74.1%), and within six about 37% (37.4%). A halved problem kept on a high baseline is caught about 60% (59.7%) and 25% (24.5%)"
	quote(t, doc, "old 10%: removed, back within 26", watched(0.10, 0, 0.10, 26), 1, figure{"74.1%", returns})
	quote(t, doc, "old 10%: removed, back within 6", watched(0.10, 0, 0.10, 6), 1, figure{"37.4%", returns})
	quote(t, doc, "old 10%: halved, back within 26", watched(0.10, 0.05, 0.10, 26), 1, figure{"59.7%", returns})
	quote(t, doc, "old 10%: halved, back within 6", watched(0.10, 0.05, 0.10, 6), 1, figure{"24.5%", returns})
	for _, c := range []struct {
		old     float64
		printed string
	}{{0.10, "0.41%"}, {0.06, "1.53%"}, {0.05, "2.75%"}, {0.04, "4.81%"}} {
		quote(t, doc, fmt.Sprintf("old %.0f%%: halved and stays halved within 26", 100*c.old), watched(c.old, c.old/2, c.old/2, 26), 2,
			figure{c.printed, "0.41%, 1.53%, 2.75% and 4.81%"})
	}
	// with a baseline that is exactly the old share, the false comebacks of a halved problem within 26 windows
	for _, c := range []struct {
		old, decimals int
		figure
	}{
		{10, 3, figure{"0.052%", "0.052%, 1.165%, 2.563% and 5.700%"}}, {6, 3, figure{"1.165%", "0.052%, 1.165%, 2.563% and 5.700%"}},
		{5, 3, figure{"2.563%", "0.052%, 1.165%, 2.563% and 5.700%"}}, {4, 3, figure{"5.700%", "0.052%, 1.165%, 2.563% and 5.700%"}},
		{3, 1, figure{"12.9%", "12.9% and 29.3%"}}, {2, 1, figure{"29.3%", "12.9% and 29.3%"}},
	} {
		got := backWithin(Counts{3 * c.old, 300}, float64(c.old)/200, 26)
		quote(t, doc, fmt.Sprintf("old %d%%, an exact baseline", c.old), got, c.decimals, c.figure)
	}
}

// The automaton above is the rule of Back: on every triple of windows, for baselines at which the five hit units among the
// three bind (a low share) and ones at which the share does (a high one), it says what Back says.
func TestTheWatchAutomatonIsTheRuleOfBack(t *testing.T) {
	for _, hits := range []int{0, 3, 6, 9, 18, 30} {
		before := Counts{hits, 300}
		for a := 0; a <= 12; a++ {
			for b := 0; b <= 12; b++ {
				for c := 0; c <= 12; c++ {
					want := Back(before, []Counts{{a, 100}, {b, 100}, {c, 100}})
					atShare := func(w int) bool { return w*before.Base >= before.Hits*100 }
					got := atShare(a) && atShare(b) && atShare(c) && a+b+c >= MinBefore
					if got != want {
						t.Fatalf("baseline %d of 300, windows %d %d %d: Back %v, rule %v", hits, a, b, c, want, got)
					}
				}
			}
		}
	}
}
