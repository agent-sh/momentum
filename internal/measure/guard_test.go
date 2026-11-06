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

// A guard's arithmetic is SPEC's (Measurement, Guards): events no more than the gap apart are one incident; the watch
// needs T((1/Alpha)^(1/(k+½)) - 1) units of exposure, rounded up, where the Jeffreys posterior predictive chance of a
// watch that quiet is at most Alpha; an incident the source began with is dropped; the admission refuses a thin
// history, a problem that went quiet before the change, and a watch past the retention; and SPEC prints those rules.
func TestTheGuardsArithmeticIsSPECs(t *testing.T) {
	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	week := 7 * day
	var times []time.Time
	for i := 0; i < 300; i++ { // one burst of two and a half days
		times = append(times, at.Add(time.Duration(i)*12*time.Minute))
	}
	end := times[len(times)-1]
	times = append(times, end.Add(week), end.Add(week+time.Second)) // a week after it ends: still the burst
	times = append(times, end.Add(2*week+2*time.Second))            // more than a week after that: a new one
	got := Incidents(times, week)
	if len(got) != 2 || got[0].Events != 302 || !got[0].First.Equal(at) || got[1].Events != 1 {
		t.Fatalf("incidents %+v", got)
	}
	if n := GuardNeed(1, 192); n != 1223 { // 192 * (20^(2/3) - 1)
		t.Fatalf("one incident in 192 units needs %d", n)
	}
	if GuardNeed(1, 0) != 0 {
		t.Fatal("a need without a history")
	}
	for _, c := range []struct{ incidents, exposure int }{{0, 20}, {1, 20}, {3, 500}, {7, 31}} {
		need := GuardNeed(c.incidents, c.exposure)
		if p := GuardP(c.incidents, c.exposure, need); p > Alpha || GuardP(c.incidents, c.exposure, need-1) <= Alpha {
			t.Errorf("%+v: a watch of %d leaves p=%v, one less %v", c, need, p, GuardP(c.incidents, c.exposure, need-1))
		}
	}
	r := Guarded(4, 600, 0, 600, "session hours")
	if !r.Improved || !r.Guarded || math.Abs(r.P-math.Pow(0.5, 4.5)) > 1e-12 || !strings.HasPrefix(r.Effect, "guarded, not a measured drop") ||
		!strings.Contains(r.Effect, "600 session hours") || !strings.Contains(r.Effect, "predictive probability 95.58%") {
		t.Fatalf("a watch that reached its need: %+v", r)
	}
	if r := Guarded(4, 600, 0, 500, "session hours"); r.Improved {
		t.Fatalf("a watch short of its need: %+v", r)
	}
	if r := Guarded(4, 600, 1, 900, "session hours"); r.Improved || r.P != 1 || !strings.Contains(r.Effect, "came back") {
		t.Fatalf("a watch with a recurrence: %+v", r)
	}

	// the source began with a burst: anchored, it is dropped and the history starts at the hour after it
	since := at
	var units []int64
	for h := since.UnixMilli() / 3_600_000; h < since.Add(20*day).UnixMilli()/3_600_000; h++ {
		units = append(units, h)
	}
	burst := []Incident{{First: since, Last: since.Add(59 * time.Hour), Events: 300}}
	h := GuardCount(burst, units, since, since.Add(20*day), week, true)
	if h.Incidents != 0 || !h.Since.Equal(since.Add(60*time.Hour)) || h.Exposure != 20*24-60 || h.Quiet != h.Exposure {
		t.Fatalf("an anchored burst: %+v", h)
	}
	if _, _, why := GuardAdmit(h, retention); !strings.Contains(why, "has not come back since the incident its source began with") {
		t.Fatalf("no incident after the one the source began with: %q", why)
	}
	if h := GuardCount(burst, units, since, since.Add(20*day), week, false); h.Incidents != 1 || h.Quiet != 20*24-60 {
		t.Fatalf("the same burst in a history that began before it: %+v", h)
	}
	if _, _, why := GuardAdmit(GuardCount(burst, units[:60], since, since.Add(60*time.Hour), week, false), retention); !strings.Contains(why, "too thin") {
		t.Fatalf("all exposure inside the incident: %q", why)
	}
	// four incidents a week and a bit apart over 28 days of hourly use: admitted, unless the problem went quiet long
	// before the change, or the watch would pass the retention
	four := func(lastAgo time.Duration) GuardHistory {
		until := since.Add(28 * day)
		var u []int64
		for h := since.UnixMilli() / 3_600_000; h < until.UnixMilli()/3_600_000; h++ {
			u = append(u, h)
		}
		var in []Incident
		for i := 3; i >= 0; i-- {
			first := until.Add(-lastAgo - time.Duration(i)*(7*day+12*time.Hour))
			in = append(in, Incident{First: first, Last: first.Add(2 * time.Hour), Events: 3})
		}
		return GuardCount(in, u, since, until, week, false)
	}
	need, expected, why := GuardAdmit(four(4*day), retention)
	if why != "" || need != GuardNeed(4, 28*24) || expected > 27*day || GuardWatch(expected, retention) != 28*day {
		t.Fatalf("four incidents: need %d, expected %s: %q", need, expected, why)
	}
	if _, _, why := GuardAdmit(four(4*day), 20*day); !strings.Contains(why, "too rare to prove by recurrence within the store's history") {
		t.Fatalf("a watch past the retention: %q", why)
	}
	h = four(4 * day)
	h.Exposure += 40 * 24 // the same history, then forty quiet days of use before the change
	h.Quiet += 40 * 24
	if _, _, why := GuardAdmit(h, retention); !strings.Contains(why, "had likely stopped before the change") {
		t.Fatalf("a long quiet stretch before the change: %q", why)
	}

	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, say := range []string{
		"runs with no gap longer than the plan's window between two of them",
		"E = T·((1/α)^(1/(k+½)) - 1) units of exposure with no recurrence, rounded up to a whole unit",
		"the measurement's own level, 0.05",
		"it passes about 25% of the time at k = 1 and 11% at k = 4 under a flat prior",
		"The watch is at most 30 days",
		"too rare to prove by recurrence within the store's history",
		"never saw the unit in use without the problem",
		"(B/(B+Q))^(k+½)",
		"1 - (T/(T+W))^(k+½)",
		"keeps the share of admitted do-nothing guards that pass at or below α (4.4% of 361 admitted), where the plug-in formula with the same rules passes about 7% (7.2% of 91640)",
	} {
		if !strings.Contains(spec, say) {
			t.Errorf("SPEC does not say %q", say)
		}
	}
	// the flat-prior figures SPEC quotes for the plug-in watch: (1/(1+ln(20)/k))^k
	for k, want := range map[int]float64{1: 0.25, 4: 0.11} {
		if got := math.Pow(1/(1+math.Log(1/Alpha)/float64(k)), float64(k)); math.Abs(got-want) > 0.005 {
			t.Errorf("k=%d: a plug-in watch passes %.3f under a flat prior, SPEC says %.2f", k, got, want)
		}
	}
}

// A fixed-seed simulation of the admission rule and the watch. Incidents arrive as a Poisson process in days at a true
// rate from one a day to one in 80 days, the tool in use eight hours a day (an hour of exposure every three hours);
// histories begin with the span (28 days) or with the source, on an incident, 9 to 28 days back. A guard is admitted
// by GuardCount and GuardAdmit, with the week's gap and the 30-day retention; the change does nothing, and the watch
// passes when no incident comes before its window ends (GuardWatch), by which time its exposure has reached the need.
// Over the grid, admitted guards pass at most Alpha, within Monte Carlo slack; with the plug-in need, ln(1/Alpha)T/k,
// and every other rule the same, they pass about 7%. For one rate alone the share passing cannot be bounded (as the
// rate falls, the few histories that admit pass any finite watch more often), so each rate is held to the chance that
// a guard is admitted and passes.
func TestAdmittedDoNothingGuardsPassAtMostAlpha(t *testing.T) {
	rng := rand.New(rand.NewPCG(80, 2026))
	week := 7 * day
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	const trials = 60000
	admitted, passed := 0, 0
	for _, history := range []struct {
		span     time.Duration
		anchored bool
	}{{28 * day, false}, {9 * day, true}, {14 * day, true}, {20 * day, true}, {28 * day, true}} {
		var units []int64
		for at := base; at.Before(base.Add(history.span)); at = at.Add(3 * time.Hour) {
			units = append(units, at.UnixMilli()/3_600_000)
		}
		for _, every := range []float64{1, 2, 3, 5, 7, 10, 14, 20, 40, 80} {
			mu := 1 / every // incidents a day
			admits, passes := 0, 0
			for range trials {
				var times []time.Time
				if history.anchored {
					times = append(times, base)
				}
				for x := rng.ExpFloat64() / mu; x < history.span.Hours()/24; x += rng.ExpFloat64() / mu {
					times = append(times, base.Add(time.Duration(x*float64(day))))
				}
				if len(times) == 0 {
					continue
				}
				h := GuardCount(Incidents(times, week), units, base, base.Add(history.span), week, history.anchored)
				_, expected, why := GuardAdmit(h, retention)
				if why != "" {
					continue
				}
				admits++
				if watch := GuardWatch(expected, retention); rng.ExpFloat64()/mu > watch.Hours()/24 {
					passes++
				}
			}
			admitted, passed = admitted+admits, passed+passes
			if size := float64(passes) / trials; size > Alpha {
				t.Errorf("one incident in %.0f days, history %s anchored %v: a do-nothing guard is admitted and passes %.4f of the time", every, history.span,
					history.anchored, size)
			}
		}
	}
	if admitted == 0 {
		t.Fatal("no guard admitted on the grid")
	}
	share := float64(passed) / float64(admitted)
	slack := 2 * math.Sqrt(Alpha*(1-Alpha)/float64(admitted))
	t.Logf("%d guards admitted, %d of them passed: %.4f (slack %.4f)", admitted, passed, share, slack)
	if share > Alpha+slack {
		t.Fatalf("admitted do-nothing guards pass %.4f of the time, above %.2f", share, Alpha)
	}
}
