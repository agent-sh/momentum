package measure

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// A guard's arithmetic is SPEC's (Measurement, Guards): events no more than a week apart are one incident; an incident
// the source began with is dropped; P = 1 - (T/(T+W))^(k+½) is reported, had the problem kept happening, and with no
// incident after the first there is no rate and no P; a guard's result is never improved and leaves Fisher's p at 0;
// and SPEC prints those rules.
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
	for _, c := range []struct {
		incidents, exposure, watched int
		recur                        float64
	}{{4, 684, 700, 1 - math.Pow(684.0/1384, 4.5)}, {1, 192, 1223, 1 - math.Pow(192.0/1415, 1.5)}, {0, 132, 720, 0}, {3, 0, 10, 0}} {
		if r := GuardRecur(c.incidents, c.exposure, c.watched); math.Abs(r-c.recur) > 1e-12 {
			t.Errorf("%+v: P=%v", c, r)
		}
	}
	r := Guarded(4, 684, 0, 700, "session hours")
	if r.Improved || !r.Guarded || r.P != 0 || r.NoRate || math.Abs(r.Recur-(1-math.Pow(684.0/1384, 4.5))) > 1e-12 ||
		!strings.HasPrefix(r.Effect, "guarded, not measured: no recurrence in 700 session hours watched") || !strings.Contains(r.Effect, "P=95.81% that the problem, had it kept happening") ||
		!strings.Contains(r.Effect, "stopped by itself") || strings.Contains(r.Effect, "improv") {
		t.Fatalf("a watch with no recurrence: %+v", r)
	}
	if r := Guarded(0, 132, 0, 720, "session hours"); r.Improved || !r.NoRate || r.Recur != 0 || strings.Contains(r.Effect, "P=") ||
		!strings.Contains(r.Effect, "no rate to compare: the history holds no incident after its first") {
		t.Fatalf("a watch whose history holds no incident after its first: %+v", r)
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
	if h := GuardCount(burst, units, since, GuardGap, true); h.Incidents != 0 || !h.Since.Equal(since.Add(60*time.Hour)) || h.Exposure != 20*24-60 {
		t.Fatalf("an anchored burst: %+v", h)
	}
	if h := GuardCount(burst, units, since, GuardGap, false); h.Incidents != 1 || h.Exposure != 20*24 {
		t.Fatalf("the same burst in a history that began before it: %+v", h)
	}

	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, say := range []string{
		"A guard is a recurrence watch, not a proof",
		"runs with no gap longer than a week between two of them, whatever the plan's window",
		"It lasts 30 days of wall time from it",
		"each of its weeks (the days past its fourth counted with the fourth) must hold at least one unit of its exposure",
		"P = 1 - (T/(T+W))^(k+½), had the problem kept happening",
		"\"no rate to compare: the history holds no incident after its first\"",
		"**P** is for the report alone: nothing decides on it",
		"a problem that stopped by itself gives the same result",
		"closes the case as `guarded`, a close reason of its own, never `improved`",
	} {
		if !strings.Contains(spec, say) {
			t.Errorf("SPEC does not say %q", say)
		}
	}
}
