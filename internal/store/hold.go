package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

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

// A build whose fix needs a path no change of the loop's may touch holds its case for a person: a "held for a person"
// line with the path, the fix, why no change may touch the path, the case's episode and what its mechanism and fix
// were, and a "person hold ended" line when the hold ends. A case in testing is held for a person while its newest
// such line is a hold of its current episode, its mechanism and fix are what they were, and no later "set" line changed
// its status: new evidence alone does not end it.
const (
	ActionPersonHeld  = "held for a person"
	ActionPersonEnded = "person hold ended"
)

// PersonHold is a hold for a person: the path the case's fix needs, the change a person makes there, why no change
// of the loop's may touch the path, the episode of the build that answered, and the sums of the case's mechanism and
// fix (its next test) the build worked from.
type PersonHold struct {
	Path         string `json:"path"`
	Fix          string `json:"fix"`
	Reason       string `json:"reason"`
	Episode      int64  `json:"episode"`
	MechanismSum string `json:"mechanism_sum"`
	FixSum       string `json:"fix_sum"`
}

// NewPersonHold is the hold for a person a build of c answered: its episode and the sums of its mechanism and fix.
func NewPersonHold(c Case, path, fix, reason string) PersonHold {
	return PersonHold{Path: path, Fix: fix, Reason: reason, Episode: c.Episode, MechanismSum: textSum(c.Mechanism), FixSum: textSum(c.NextTest)}
}

// textSum is the hex SHA-256 of a case's text.
func textSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Why is the hold's reason as a case's held mark shows it.
func (h PersonHold) Why() string { return "for a person: its fix needs " + h.Path }

// PersonDetail is the detail of a "held for a person" line.
func PersonDetail(h PersonHold) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(h)
	return strings.TrimSpace(b.String())
}

// LastPersonHold is a case's newest hold for a person whose end its log does not hold yet, whatever the case's
// status or episode now: one that no longer stands (PersonHoldOf) is still to be ended.
func LastPersonHold(log []CaseEntry) (PersonHold, bool) {
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case ActionPersonEnded:
			return PersonHold{}, false
		case ActionPersonHeld:
			var h PersonHold
			if json.Unmarshal([]byte(log[i].Detail), &h) != nil || h.Path == "" {
				return PersonHold{}, false
			}
			return h, true
		}
	}
	return PersonHold{}, false
}

// PersonHoldOf is the hold for a person a case in testing stands on, from its log: its newest such hold of its
// current episode, with no end, the case's mechanism and fix as they were at it, and no later "set" line that changed
// the case's status (updateCase names a status change first, so no title or other text it holds reads as one).
func PersonHoldOf(c Case, log []CaseEntry) (PersonHold, bool) {
	if c.Status != CaseTesting {
		return PersonHold{}, false
	}
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case ActionPersonEnded:
			return PersonHold{}, false
		case "set":
			if strings.HasPrefix(log[i].Detail, "status ") {
				return PersonHold{}, false
			}
		case ActionPersonHeld:
			h, ok := LastPersonHold(log[:i+1])
			if !ok || h.Episode != c.Episode || h.MechanismSum != textSum(c.Mechanism) || h.FixSum != textSum(c.NextTest) {
				return PersonHold{}, false
			}
			return h, true
		}
	}
	return PersonHold{}, false
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

// A configuration proposal the loop filed for a person is kept in the case's log too: a "proposed for a configuration
// change" line with its file and the case's episode, and a "configuration proposal withdrawn" line when the loop
// withdraws it. A case in testing waits on it while its newest such line is a proposal of its current episode; the
// case's close, a new episode or a withdrawal ends the wait.
const (
	ActionConfigProposed  = "proposed for a configuration change"
	ActionConfigWithdrawn = "configuration proposal withdrawn"
)

// ConfigStage is how a case waiting on a configuration proposal shows its stage.
const ConfigStage = "proposed (configuration)"

type configRecord struct {
	Target  string `json:"target"`
	Episode int64  `json:"episode"`
}

// ConfigDetail is the detail of a "proposed for a configuration change" line.
func ConfigDetail(target string, episode int64) string {
	raw, _ := json.Marshal(configRecord{target, episode})
	return string(raw)
}

// ConfigOf returns the file a case in testing waits on for a configuration proposal, from its log, or "".
func ConfigOf(c Case, log []CaseEntry) string {
	if c.Status != CaseTesting {
		return ""
	}
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case ActionConfigWithdrawn:
			return ""
		case ActionConfigProposed:
			var r configRecord
			if json.Unmarshal([]byte(log[i].Detail), &r) != nil || r.Episode != c.Episode {
				return ""
			}
			return r.Target
		}
	}
	return ""
}
