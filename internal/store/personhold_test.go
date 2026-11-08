package store

import "testing"

// A hold for a person stands for its episode while its case is in testing: a later episode (a reopen, whatever the
// evidence), its end, and a later "set" line that changes the case's status, mechanism or fix end it; a "set" line
// that changes the title alone, and new evidence, do not.
func TestAPersonHoldStandsUntilAPersonActs(t *testing.T) {
	held := PersonHold{Path: ".github/workflows/ci.yml", Fix: "Run the tests.", Reason: "CI configuration", Episode: 2}
	line := CaseEntry{Action: ActionPersonHeld, Detail: PersonDetail(held)}
	testing2 := Case{Status: CaseTesting, Episode: 2, Evidence: 4}
	for _, tc := range []struct {
		name  string
		c     Case
		log   []CaseEntry
		holds bool
	}{
		{"held", testing2, []CaseEntry{line}, true},
		{"new evidence", Case{Status: CaseTesting, Episode: 2, Evidence: 9}, []CaseEntry{line, {Action: "evidence", Detail: "5 new"}}, true},
		{"a title", testing2, []CaseEntry{line, {Action: "set", Detail: "title: the thing breaks in CI"}}, true},
		{"reopened, same evidence", Case{Status: CaseTesting, Episode: 3, Evidence: 4}, []CaseEntry{line}, false},
		{"not testing", Case{Status: CaseInvestigating, Episode: 2}, []CaseEntry{line}, false},
		{"ended", testing2, []CaseEntry{line, {Action: ActionPersonEnded, Detail: "the case left testing"}}, false},
		{"status", testing2, []CaseEntry{line, {Action: "set", Detail: "status testing -> investigating"}}, false},
		{"mechanism", testing2, []CaseEntry{line, {Action: "set", Detail: "title: x; mechanism: another cause"}}, false},
		{"fix", testing2, []CaseEntry{line, {Action: "set", Detail: "fix: another fix"}}, false},
		{"a set before it", testing2, []CaseEntry{{Action: "set", Detail: "fix: another fix"}, line}, true},
	} {
		got, ok := PersonHoldOf(tc.c, tc.log)
		if ok != tc.holds || ok && got != held {
			t.Errorf("%s: %+v %v", tc.name, got, ok)
		}
	}
	// a hold that no longer stands is still open until its end is written
	if h, ok := LastPersonHold([]CaseEntry{line, {Action: "set", Detail: "fix: another fix"}}); !ok || h != held {
		t.Errorf("the open hold: %+v %v", h, ok)
	}
	if _, ok := LastPersonHold([]CaseEntry{line, {Action: ActionPersonEnded, Detail: "the case is in a new episode"}}); ok {
		t.Error("an ended hold is open")
	}
}
