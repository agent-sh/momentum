package sessions

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// resetZone is the zone after a reset time: in parentheses, or bare (UTC, an IANA name, an abbreviation).
const resetZone = `(?:\s*\(([^)]+)\)|\s+(UTC|GMT|[A-Za-z]+/[A-Za-z_]+(?:/[A-Za-z_]+)?|(?-i:[A-Z]{2,5}))\b)?`

var (
	reResetDate  = regexp.MustCompile(`(?i)resets? (?:at |on )?([A-Z][a-z]{2,8}) (\d{1,2}),? (\d{1,2})(?::(\d{2}))?(?:\s*(am|pm))?` + resetZone)
	reResetClock = regexp.MustCompile(`(?i)resets? (?:at )?(\d{1,2})(?::(\d{2}))?(?:\s*(am|pm))?` + resetZone)
	reResetEpoch = regexp.MustCompile(`limit reached\|(\d{10})`)
)

// resetAhead is how far past the turn of a year a reset stated by its day can be: a date a little before the
// error it ends is a misreading, not a reset a year away.
const resetAhead = 31 * 24 * time.Hour

// ResetAt is the time a usage limit resets, as its error states it ("resets Mar 7, 9am (Europe/Lisbon)",
// "resets 3pm UTC", "resets at 15:30", or the older "limit reached|<unix seconds>"): the first such time after the
// error. A time without a zone is local; one in a zone it does not know, or a date before the error, is no time.
func ResetAt(text string, at time.Time) (time.Time, bool) {
	if m := reResetEpoch.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if t := time.Unix(n, 0); t.After(at) {
			return t, true
		}
		return time.Time{}, false // a reset before the error it ends is a misreading
	}
	if m := reResetDate.FindStringSubmatch(text); m != nil {
		mon, okMon := month(m[1])
		day, _ := strconv.Atoi(m[2])
		h, mi, okClock := clock(m[3], m[4], m[5])
		if okMon && okClock {
			loc, ok := zone(m[6], m[7])
			if !ok {
				return time.Time{}, false
			}
			t := time.Date(at.In(loc).Year(), mon, day, h, mi, 0, 0, loc)
			if !t.After(at) {
				if next := t.AddDate(1, 0, 0); next.Sub(at) <= resetAhead {
					return next, true // the turn of a year
				}
				return time.Time{}, false
			}
			return t, true
		}
	}
	if m := reResetClock.FindStringSubmatch(text); m != nil {
		h, mi, ok := clock(m[1], m[2], m[3])
		if ok {
			loc, ok := zone(m[4], m[5])
			if !ok {
				return time.Time{}, false
			}
			l := at.In(loc)
			t := time.Date(l.Year(), l.Month(), l.Day(), h, mi, 0, 0, loc)
			if !t.After(at) {
				t = t.AddDate(0, 0, 1)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

func month(s string) (time.Month, bool) {
	s = strings.ToLower(s)
	for m := time.January; m <= time.December; m++ {
		if name := strings.ToLower(m.String()); strings.HasPrefix(name, s) && len(s) >= 3 {
			return m, true
		}
	}
	return 0, false
}

func clock(h, m, ampm string) (int, int, bool) {
	hh, err := strconv.Atoi(h)
	if err != nil {
		return 0, 0, false
	}
	mm := 0
	if m != "" {
		mm, _ = strconv.Atoi(m)
	}
	switch strings.ToLower(ampm) {
	case "am":
		if hh == 12 {
			hh = 0
		}
	case "pm":
		if hh < 12 {
			hh += 12
		}
	case "":
		if m == "" { // a bare number is not a time
			return 0, 0, false
		}
	}
	if hh > 23 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// zone is the first zone named, or local when none is; false for a name it does not know.
func zone(names ...string) (*time.Location, bool) {
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			loc, err := time.LoadLocation(name)
			return loc, err == nil
		}
	}
	return time.Local, true
}
