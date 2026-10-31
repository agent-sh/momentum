package claude

import (
	"strconv"
	"strings"
	"time"
)

// schedule is a 5-field cron expression as Claude Code's CronCreate takes it (minute, hour, day of month, month,
// day of week), read in local time: each field a list of numbers, ranges and steps, or *.
type schedule struct {
	expr                         string
	minute, hour, dom, month, wd [60]bool
	anyDom, anyWd                bool
}

// parseSchedule reads a cron expression as Claude Code does. false for one it refuses (names, a sixth field, a
// number with a step): it names no run. A day field that holds every day is unrestricted, as a * is.
func parseSchedule(expr string) (schedule, bool) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return schedule{}, false
	}
	c := schedule{expr: strings.Join(f, " ")}
	for i, spec := range []struct {
		set    *[60]bool
		lo, hi int
	}{{&c.minute, 0, 59}, {&c.hour, 0, 23}, {&c.dom, 1, 31}, {&c.month, 1, 12}, {&c.wd, 0, 7}} {
		if !cronField(f[i], spec.lo, spec.hi, spec.set) {
			return schedule{}, false
		}
	}
	if c.wd[7] {
		c.wd[0] = true // 7 is Sunday too
	}
	c.anyDom, c.anyWd = full(c.dom[1:32]), full(c.wd[0:7])
	return c, true
}

func full(set []bool) bool {
	for _, v := range set {
		if !v {
			return false
		}
	}
	return true
}

func cronField(field string, lo, hi int, set *[60]bool) bool {
	for _, part := range strings.Split(field, ",") {
		step, stepped := 1, false
		if base, s, ok := strings.Cut(part, "/"); ok {
			n, err := number(s)
			if err != nil || n <= 0 {
				return false
			}
			part, step, stepped = base, n, true
		}
		a, b := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			x, y, _ := strings.Cut(part, "-")
			var err1, err2 error
			a, err1 = number(x)
			b, err2 = number(y)
			if err1 != nil || err2 != nil {
				return false
			}
		default:
			n, err := number(part)
			if err != nil || stepped {
				return false // a step takes a range or *: Claude Code refuses 5/15
			}
			a, b = n, n
		}
		if a < lo || b > hi || a > b {
			return false
		}
		for v := a; v <= b; v += step {
			set[v] = true
		}
	}
	return true
}

// number is a field's number, digits only: Claude Code refuses a sign.
func number(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, strconv.ErrSyntax
	}
	return strconv.Atoi(s)
}

func (c schedule) runs(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	dom, wd := c.dom[t.Day()], c.wd[int(t.Weekday())]
	switch {
	case c.anyDom && c.anyWd:
		return true
	case c.anyDom:
		return wd
	case c.anyWd:
		return dom
	}
	return dom || wd // with both restricted, cron runs on either
}

// next is the schedule's first run after t, at a whole minute of local time; zero when none comes within a year.
// Claude Code matches a minute of the wall clock once: the hour a clock change repeats does not run again, so the
// clock is read from two hours before t, and a minute it showed before t counts as seen.
func (c schedule) next(t time.Time) time.Time {
	from := t.Local().Truncate(time.Minute)
	m := from.Add(-2 * time.Hour)
	seen := wall(m)
	for end := from.AddDate(1, 0, 0); m.Before(end); {
		m = m.Add(time.Minute)
		w := wall(m)
		if w <= seen {
			continue // the wall clock went back: a minute it showed already
		}
		seen = w
		if m.After(from) && c.runs(m) {
			return m
		}
	}
	return time.Time{}
}

// wall is a local time's minute on the wall clock, in an order that follows the calendar.
func wall(t time.Time) int64 {
	y, mo, d := t.Date()
	return ((int64(y)*100+int64(mo))*100+int64(d))*10000 + int64(t.Hour())*100 + int64(t.Minute())
}

// lateness is how late Claude Code may start a recurring task's run, by its documentation: up to half the gap to
// the task's next run, and at most 30 minutes; a task that runs every five minutes is not delayed.
func (c schedule) lateness(run time.Time) time.Duration {
	if c.expr == "*/5 * * * *" {
		return 0
	}
	n := c.next(run)
	if n.IsZero() {
		return 30 * time.Minute
	}
	return min(n.Sub(run)/2, 30*time.Minute)
}

// ScheduledRun is the next run after after of a job on the cron expression expr, and the latest a recurring job
// starts it: the run's time and the most Claude Code delays it (a one-shot runs on time, or a little early). false
// for an expression it cannot read, or one that names no run within a year.
func ScheduledRun(expr string, after time.Time) (run, latest time.Time, ok bool) {
	sc, ok := parseSchedule(expr)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if run = sc.next(after); run.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	return run, run.Add(sc.lateness(run)), true
}
