package store

// The loop keeps a case's policy hold in the case's log: a "build held" line with its reason when the
// configuration holds the case's next step (builds off, an owner it may not write to, no repository), and a
// "build hold lifted" line when the hold ends. A case is held while its newest such line is a hold.
const (
	ActionHeld   = "build held"
	ActionUnheld = "build hold lifted"
)

// HoldOf returns the reason a case in testing is held, from its log, or "" when it is not held.
func HoldOf(c Case, log []CaseEntry) string {
	if c.Status != CaseTesting {
		return ""
	}
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case ActionHeld:
			return log[i].Detail
		case ActionUnheld:
			return ""
		}
	}
	return ""
}
