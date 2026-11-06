package measure

import (
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// retention is the loop's guard cap, the store's evidence retention (store.CaseRetention): 30 days.
const retention = 30 * day

// A guard's arithmetic is SPEC's (Measurement, Guards): events no more than a week apart are one incident; the watch
// runs to T((1/Alpha)^(1/(k+½)) - 1) units of exposure, rounded up, where the Jeffreys posterior predictive chance of
// a watch that quiet is at most Alpha; it reports P = 1 - (T/(T+W))^(k+½); an incident the source began with is
// dropped; a guard's result is never improved; and SPEC prints those rules. The pinned needs fail for a level of 0.10,
// an exponent of k + 1 and the plug-in need, ln(1/Alpha)T/k.
func TestTheGuardsArithmeticIsSPECs(t *testing.T) {
	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	if GuardGap != 7*day {
		t.Fatalf("the gap is %s", GuardGap)
	}
	var times []time.Time
	for i := 0; i < 300; i++ { // one burst of two and a half days
		times = append(times, at.Add(time.Duration(i)*12*time.Minute))
	}
	end := times[len(times)-1]
	times = append(times, end.Add(GuardGap), end.Add(GuardGap+time.Second)) // a week after it ends: still the burst
	times = append(times, end.Add(2*GuardGap+2*time.Second))                // more than a week after that: a new one
	got := Incidents(times, GuardGap)
	if len(got) != 2 || got[0].Events != 302 || !got[0].First.Equal(at) || got[1].Events != 1 {
		t.Fatalf("incidents %+v", got)
	}
	for _, c := range []struct{ incidents, exposure, need int }{{1, 192, 1223}, {4, 684, 647}, {0, 132, 52668}} {
		if n := GuardNeed(c.incidents, c.exposure); n != c.need {
			t.Errorf("%d incidents in %d units: a need of %d, SPEC's formula gives %d", c.incidents, c.exposure, n, c.need)
		}
		if p := GuardP(c.incidents, c.exposure, c.need); p > Alpha || GuardP(c.incidents, c.exposure, c.need-1) <= Alpha {
			t.Errorf("%+v: a watch of its need leaves p=%v, one less %v", c, p, GuardP(c.incidents, c.exposure, c.need-1))
		}
		if r := GuardRecur(c.incidents, c.exposure, c.need); math.Abs(r-(1-GuardP(c.incidents, c.exposure, c.need))) > 1e-15 || r < 1-Alpha {
			t.Errorf("%+v: P at its need is %v", c, r)
		}
	}
	if GuardNeed(1, 0) != 0 {
		t.Fatal("a need without a history")
	}
	r := Guarded(4, 684, 0, 700, "session hours")
	if r.Improved || !r.Guarded || math.Abs(r.Recur-(1-math.Pow(684.0/1384, 4.5))) > 1e-12 || !strings.HasPrefix(r.Effect, "guarded, not measured: no recurrence in 700 session hours") ||
		!strings.Contains(r.Effect, "P=95.81%") || !strings.Contains(r.Effect, "stopped by itself") || strings.Contains(r.Effect, "improv") || strings.Contains(r.Effect, "drop") {
		t.Fatalf("a watch that reached its need: %+v", r)
	}
	if r := Guarded(4, 684, 0, 240, "session hours"); r.Improved || r.Recur >= 1-Alpha || !strings.Contains(r.Effect, "no recurrence in 240 session hours") {
		t.Fatalf("a watch the retention ended first is reported as it is: %+v", r)
	}
	if r := Guarded(4, 684, 1, 900, "session hours"); r.Improved || !strings.Contains(r.Effect, "came back") {
		t.Fatalf("a watch with a recurrence: %+v", r)
	}

	// the source began with a burst: anchored, it is dropped and the history starts at the hour after it
	since := at
	var units []int64
	for h := since.UnixMilli() / 3_600_000; h < since.Add(20*day).UnixMilli()/3_600_000; h++ {
		units = append(units, h)
	}
	burst := []Incident{{First: since, Last: since.Add(59 * time.Hour), Events: 300}}
	h := GuardCount(burst, units, since, since.Add(20*day), GuardGap, true)
	if h.Incidents != 0 || !h.Since.Equal(since.Add(60*time.Hour)) || h.Exposure != 20*24-60 || h.Before != 0 {
		t.Fatalf("an anchored burst: %+v", h)
	}
	if h := GuardCount(burst, units, since, since.Add(20*day), GuardGap, false); h.Incidents != 1 || h.Exposure != 20*24 || h.Before != 60 {
		t.Fatalf("the same burst in a history that began before it: %+v", h)
	}
	if w := GuardWatch(h, retention); w != retention {
		t.Fatalf("a watch whose need takes longer than the retention: %s", w)
	}

	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, say := range []string{
		"A guard is a watch, not a proof",
		"runs with no gap longer than a week between two of them, whatever the plan's window",
		"E = T·((1/α)^(1/(k+½)) - 1) units or 30 days after the change, whichever comes first",
		"the measurement's own level, 0.05",
		"P = 1 - (T/(T+W))^(k+½)",
		"A problem that stopped by itself gives the same result",
		"closes the case as `guarded`, a close reason of its own, never `improved`",
		"a do-nothing change passes about 25% of the time at k = 1 and 11% at k = 4 under a flat prior",
		"the share of watches that met a recurrence matches the mean reported P within Monte Carlo slack",
	} {
		if !strings.Contains(spec, say) {
			t.Errorf("SPEC does not say %q", say)
		}
	}
	for k, want := range map[int]float64{1: 0.25, 4: 0.11} { // the flat-prior figures SPEC quotes: (1/(1+ln(20)/k))^k
		if got := math.Pow(1/(1+math.Log(1/Alpha)/float64(k)), float64(k)); math.Abs(got-want) > 0.005 {
			t.Errorf("k=%d: a plug-in watch passes %.3f under a flat prior, SPEC says %.2f", k, got, want)
		}
	}
}

// A fixed-seed simulation of what a guard reports. A problem keeps happening and the change does nothing: its
// incidents are a Poisson process in units of exposure at a rate drawn from Jeffreys' prior (density μ^(-½), over a
// range wide enough that its ends do not show), the tool in use eight hours a day. A history begins with the span (28
// days) or with the source, on an incident, 9 to 28 days back; GuardCount drops that incident. The watch runs to the
// guard's need or the 30-day retention, whichever comes first. For each k, the share of watches that met a recurrence
// must match the mean P the guard reports (GuardRecur) within Monte Carlo slack. An exponent of k + 1, the plug-in
// P = 1 - exp(-kW/T), or keeping the incident the source began with, each miscalibrates it.
func TestAGuardsReportedPIsCalibrated(t *testing.T) {
	rng := rand.New(rand.NewPCG(80, 2026))
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	const perDay, trials = 8, 150000
	lo, hi := math.Sqrt(1e-7), math.Sqrt(0.3) // μ per unit of exposure, μ^(-½): √μ uniform
	capUnits := int(retention.Hours()/24) * perDay
	type bucket struct {
		n, recurred int
		reported    float64
	}
	buckets := map[int]*bucket{}
	for _, history := range []struct {
		span     time.Duration
		anchored bool
	}{{28 * day, false}, {9 * day, true}, {14 * day, true}, {28 * day, true}} {
		total := int(history.span.Hours()/24) * perDay
		units := make([]int64, total)
		unitTime := func(x float64) time.Time { return base.Add(time.Duration(x / perDay * float64(day))) }
		for i := range units {
			units[i] = unitTime(float64(i)).UnixMilli() / 3_600_000
		}
		for range trials {
			s := lo + rng.Float64()*(hi-lo)
			mu := s * s
			var incidents []Incident
			if history.anchored {
				incidents = append(incidents, Incident{First: base, Last: base, Events: 1})
			}
			for x := rng.ExpFloat64() / mu; x < float64(total); x += rng.ExpFloat64() / mu {
				at := unitTime(x)
				incidents = append(incidents, Incident{First: at, Last: at, Events: 1})
			}
			h := GuardCount(incidents, units, base, base.Add(history.span), GuardGap, history.anchored)
			if h.Exposure == 0 {
				continue
			}
			watched := min(GuardNeed(h.Incidents, h.Exposure), capUnits)
			k := min(h.Incidents, 6)
			b := buckets[k]
			if b == nil {
				b = &bucket{}
				buckets[k] = b
			}
			b.n++
			b.reported += GuardRecur(h.Incidents, h.Exposure, watched)
			if rng.ExpFloat64()/mu < float64(watched) {
				b.recurred++
			}
		}
	}
	for k := 0; k <= 6; k++ {
		b := buckets[k]
		if b == nil || b.n < 2000 {
			t.Fatalf("k=%d: too few watches to tell (%v)", k, b)
		}
		share, mean := float64(b.recurred)/float64(b.n), b.reported/float64(b.n)
		slack := 3*math.Sqrt(share*(1-share)/float64(b.n)) + 0.005
		t.Logf("k=%d: %d watches, %.4f met a recurrence, P reported %.4f (slack %.4f)", k, b.n, share, mean, slack)
		if math.Abs(share-mean) > slack {
			t.Errorf("k=%d: %.4f of watches met a recurrence, but the guard reported P=%.4f on average", k, share, mean)
		}
	}
}
