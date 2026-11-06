package measure

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// A guard's arithmetic is SPEC's (Measurement, Guards): events no more than the gap apart are one incident; the watch
// needs ln(1/Alpha)/λ units of exposure, rounded up; a watch of exactly that holds no recurrence, under no change, with
// probability at most Alpha; the longest watch is two of the build step's longest windows; and SPEC prints those figures.
func TestTheGuardsArithmeticIsSPECs(t *testing.T) {
	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
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
	if n := GuardNeed(1, 192); n != 576 {
		t.Fatalf("one incident in 192 hours needs %d", n)
	}
	if GuardNeed(0, 192) != 0 || GuardNeed(1, 0) != 0 {
		t.Fatal("a need without a rate")
	}
	for _, c := range []struct{ incidents, exposure int }{{1, 20}, {3, 500}, {7, 31}} {
		need := GuardNeed(c.incidents, c.exposure)
		if p := GuardP(c.incidents, c.exposure, need); p > Alpha || GuardP(c.incidents, c.exposure, need-1) <= Alpha {
			t.Errorf("%+v: a watch of %d leaves p=%v, one less %v", c, need, p, GuardP(c.incidents, c.exposure, need-1))
		}
	}
	r := Guarded(1, 20, 0, 65)
	if !r.Improved || !r.Guarded || math.Abs(r.P-math.Exp(-3.25)) > 1e-12 || !strings.HasPrefix(r.Effect, "guarded, not a measured drop") {
		t.Fatalf("a watch that reached its need: %+v", r)
	}
	if r := Guarded(1, 20, 0, 59); r.Improved {
		t.Fatalf("a watch short of its need: %+v", r)
	}
	if r := Guarded(1, 20, 1, 500); r.Improved || r.P != 1 || !strings.Contains(r.Effect, "came back") {
		t.Fatalf("a watch with a recurrence: %+v", r)
	}
	if MaxWatch != 28*24*time.Hour || LongestWindow != 2*week {
		t.Fatalf("the longest watch %s, the longest window %s", MaxWatch, LongestWindow)
	}
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, say := range []string{
		"runs with no gap longer than the plan's window between two of them",
		"E = ln(1/α)/λ units of exposure with no recurrence, rounded up to a whole unit, with α the measurement's own level, 0.05",
		"about ln(20), three, times the history over its incidents",
		"A watch longer than 28 days is refused",
		"two of the longest window the build step chooses (two weeks",
		"never saw the unit in use without the problem",
		"exp(-λ watched)",
	} {
		if !strings.Contains(spec, say) {
			t.Errorf("SPEC does not say %q", say)
		}
	}
	if math.Abs(math.Log(1/Alpha)-3) > 0.01 || Alpha != 0.05 {
		t.Errorf("ln(1/Alpha) = %v: SPEC says about three", math.Log(1/Alpha))
	}
}
