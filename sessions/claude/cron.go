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

// parseSchedule reads a cron expression. false for one it cannot read (names, a sixth field): it names no run.
func parseSchedule(expr string) (schedule, bool) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return schedule{}, false
	}
	c := schedule{expr: strings.Join(f, " "), anyDom: f[2] == "*", anyWd: f[4] == "*"}
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
	return c, true
}

func cronField(field string, lo, hi int, set *[60]bool) bool {
	for _, part := range strings.Split(field, ",") {
		step := 1
		if base, s, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return false
			}
			part, step = base, n
		}
		a, b := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			x, y, _ := strings.Cut(part, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(x)
			b, err2 = strconv.Atoi(y)
			if err1 != nil || err2 != nil {
				return false
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return false
			}
			a, b = n, n
			if step > 1 {
				b = hi
			}
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
func (c schedule) next(t time.Time) time.Time {
	m := t.Local().Truncate(time.Minute).Add(time.Minute)
	for end := m.AddDate(1, 0, 0); m.Before(end); m = m.Add(time.Minute) {
		if c.runs(m) {
			return m
		}
	}
	return time.Time{}
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

// LoopRun is the next run after after of a recurring job on the cron expression expr, and the latest it starts:
// the run's time and the most Claude Code delays it. false for an expression it cannot read, or one that names no
// run.
func LoopRun(expr string, after time.Time) (run, latest time.Time, ok bool) {
	sc, ok := parseSchedule(expr)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if run = sc.next(after); run.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	return run, run.Add(sc.lateness(run)), true
}
