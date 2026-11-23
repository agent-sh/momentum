package scorecard

import (
	"fmt"
	"slices"
)

// The rules. Each is fixed here, in code the loop may not change, and each says in its verdict which numbers it
// read.

// Systems the scorecard judges.
const (
	Tacit    = "tacit"
	Momentum = "momentum"
)

// FailingWeeks is how many failing weeks in a row shut a system down: one week can be quiet, two in a row is not
// delivering.
const FailingWeeks = 2

// BaselineWeeks is how many weeks before a week its restatements are compared with.
const BaselineWeeks = 4

// JudgeTacit says whether tacit delivered in a week: (i) among the delivered lines agents marked in interactive
// sessions, at least one was used and used lines are at least as many as not relevant ones (a delivery that is noise
// more often than help costs more context and attention than it gives); and (ii) the owner's restatements of facts
// tacit holds are not above the median of the weeks before it that momentum's store covers (none: the test holds).
func JudgeTacit(cur TacitWeek, before []TacitWeek) (bool, string) {
	m := cur.InteractiveMarks
	marks := m.Used >= 1 && m.Used >= m.NotRelevant
	var base []int
	for _, b := range before {
		if b.OwnerCovered {
			base = append(base, b.Restatements)
		}
	}
	med, has := median(base)
	restated := !has || float64(cur.Restatements) <= med
	why := fmt.Sprintf("interactive marks: %d used, %d not relevant (%s); owner restatements: %d", m.Used, m.NotRelevant,
		pass(marks, "used >= 1 and used >= not relevant"), cur.Restatements)
	if has {
		why += fmt.Sprintf(" against a median of %s over %d weeks before (%s)", trim(med), len(base), pass(restated, "not above the median"))
	} else {
		why += " with no week before to compare (holds)"
	}
	return marks && restated, why
}

// JudgeMomentum says whether momentum delivered in a week: at least one case closed as improved, a measured
// recurrence drop after a landed fix. One real measured improvement a week is the floor that justifies its spend.
func JudgeMomentum(m MomentumWeek) (bool, string) {
	ok := m.Improved >= 1
	return ok, fmt.Sprintf("cases closed improved: %d (%s); spend $%.2f, changes landed %d", m.Improved, pass(ok, "at least 1"), m.SpendUSD, m.Landed)
}

func pass(ok bool, rule string) string {
	if ok {
		return "passes " + rule
	}
	return "fails " + rule
}

func median(xs []int) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if n := len(s); n%2 == 1 {
		return float64(s[n/2]), true
	} else {
		return float64(s[n/2-1]+s[n/2]) / 2, true
	}
}

func trim(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprint(int64(f))
	}
	return fmt.Sprintf("%.1f", f)
}
