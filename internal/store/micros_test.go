package store

import (
	"math"
	"testing"
)

// An amount counts in whole micro-dollars rounded to the nearest, not cut down: $2.01 is 2009999.9999999998 millionths
// in floating point and counts 2010000. It holds within MaxMicros either way, and a sum within 1<<60.
func TestMicrosRoundsToTheNearestMicroDollar(t *testing.T) {
	for _, tc := range []struct {
		usd  float64
		want int64
	}{{2.01, 2010000}, {-2.01, -2010000}, {0.0000004, 0}, {0.0000006, 1}, {4, 4000000}, {1e20, MaxMicros}, {-1e20, -MaxMicros},
		{math.NaN(), 0}} {
		if got := Micros(tc.usd); got != tc.want {
			t.Errorf("Micros(%v) = %d, want %d", tc.usd, got, tc.want)
		}
	}
	if got := AddMicros(1<<60, MaxMicros); got != 1<<60 {
		t.Errorf("AddMicros past 1<<60 = %d, want 1<<60", got)
	}
	if got := AddMicros(-1<<60, -MaxMicros); got != -1<<60 {
		t.Errorf("AddMicros past -1<<60 = %d, want -1<<60", got)
	}
}
