// Package scorecard is the weekly observation of tacit and of momentum itself, and the rule that shuts either
// down when it does not deliver. It reads tacit's own records (its delivery log, its store, the transcripts its
// hooks ran in) and momentum's store, computes the week's numbers in code, judges each system by a fixed rule, and
// after two failing weeks in a row turns the system off. The loop never changes this package: what judges a system
// must not be in reach of the system it judges.
package scorecard

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Week is a week of the scorecard: from a Sunday 00:00 local time to the next. It is named by the ISO week of its
// Monday (YYYY-Www), so the week a Sunday morning run reports on is the one that ended at midnight.
type Week struct {
	Name       string
	Start, End time.Time
}

// WeekOf is the week that holds t, in t's location.
func WeekOf(t time.Time) Week {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	start := day.AddDate(0, 0, -int(day.Weekday()))
	return weekFrom(start)
}

func weekFrom(start time.Time) Week {
	y, w := start.AddDate(0, 0, 1).ISOWeek()
	return Week{Name: fmt.Sprintf("%04d-W%02d", y, w), Start: start, End: start.AddDate(0, 0, 7)}
}

// LastComplete is the newest week that has ended by now.
func LastComplete(now time.Time) Week { return WeekOf(now).Prev(1) }

// Prev is the week n weeks before w.
func (w Week) Prev(n int) Week { return weekFrom(w.Start.AddDate(0, 0, -7*n)) }

// Next is the week after w.
func (w Week) Next() Week { return weekFrom(w.End) }

var weekName = regexp.MustCompile(`^(\d{4})-W(\d{2})$`)

// ParseWeek reads a week's name in loc.
func ParseWeek(name string, loc *time.Location) (Week, error) {
	m := weekName.FindStringSubmatch(name)
	if m == nil {
		return Week{}, fmt.Errorf("week %q is not YYYY-Www", name)
	}
	y, _ := strconv.Atoi(m[1])
	n, _ := strconv.Atoi(m[2])
	// ISO week 1 holds January 4th; its Monday is the Monday on or before it
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, loc)
	monday := jan4.AddDate(0, 0, -((int(jan4.Weekday())+6)%7)+7*(n-1))
	w := weekFrom(monday.AddDate(0, 0, -1))
	if w.Name != name {
		return Week{}, fmt.Errorf("week %q does not exist", name)
	}
	return w, nil
}
