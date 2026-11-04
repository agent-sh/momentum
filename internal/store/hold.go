package store

import "encoding/json"

// The loop keeps a case's policy hold in the case's log: a "build held" line with its reason and the case's
// episode when the configuration holds the case's next step (builds off, an owner it may not write to, no
// repository), and a "build hold lifted" line when the hold ends. A case in testing is held while its newest such
// line is a hold of its current episode.
const (
	ActionHeld   = "build held"
	ActionUnheld = "build hold lifted"
)

type holdRecord struct {
	Reason  string `json:"reason"`
	Episode int64  `json:"episode"`
}

// HoldDetail is the detail of a "build held" line.
func HoldDetail(reason string, episode int64) string {
	raw, _ := json.Marshal(holdRecord{reason, episode})
	return string(raw)
}

// HoldOf returns the reason a case in testing is held, from its log, or "" when it is not held.
func HoldOf(c Case, log []CaseEntry) string {
	if c.Status != CaseTesting {
		return ""
	}
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case ActionHeld:
			var h holdRecord
			if json.Unmarshal([]byte(log[i].Detail), &h) != nil || h.Episode != c.Episode {
				return ""
			}
			return h.Reason
		case ActionUnheld:
			return ""
		}
	}
	return ""
}
