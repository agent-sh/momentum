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

// A proposal for a hand landing is kept in the case's log too: a "proposed for a hand landing" line with its pull
// request and the case's episode when the loop proposes a change it may not land itself, and a "hand landing ended"
// line when that pull request is merged or closed. A case in testing waits for a hand landing while its newest such
// line is a proposal of its current episode.
const (
	ActionHandProposed = "proposed for a hand landing"
	ActionHandEnded    = "hand landing ended"
)

// HandStage is how a case waiting for a hand landing shows its stage.
const HandStage = "proposed (hand landing)"

type handRecord struct {
	PR      string `json:"pr"`
	Episode int64  `json:"episode"`
}

// HandDetail is the detail of a "proposed for a hand landing" line.
func HandDetail(pr string, episode int64) string {
	raw, _ := json.Marshal(handRecord{pr, episode})
	return string(raw)
}

// HandOf returns the pull request a case in testing waits on for a hand landing, from its log, or "".
func HandOf(c Case, log []CaseEntry) string {
	if c.Status != CaseTesting {
		return ""
	}
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case ActionHandProposed:
			var h handRecord
			if json.Unmarshal([]byte(log[i].Detail), &h) != nil || h.Episode != c.Episode || h.PR == "" {
				return ""
			}
			return h.PR
		case ActionHandEnded:
			return ""
		}
	}
	return ""
}

// HandRecordOf reads a "proposed for a hand landing" line: its pull request and episode.
func HandRecordOf(e CaseEntry) (string, int64, bool) {
	if e.Action != ActionHandProposed {
		return "", 0, false
	}
	var h handRecord
	if json.Unmarshal([]byte(e.Detail), &h) != nil || h.PR == "" {
		return "", 0, false
	}
	return h.PR, h.Episode, true
}
