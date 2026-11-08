package store

import "testing"

// A hold for a person stands for its episode while its case is in testing with the mechanism and fix it had at the
// hold: a later episode (a reopen, whatever the evidence), its end, a changed mechanism or fix, and a later "set" line
// that changes the case's status end it; a "set" line that changes the title or benefit alone, whatever its text
// reads as, and new evidence do not.
func TestAPersonHoldStandsUntilAPersonActs(t *testing.T) {
	testing2 := Case{Status: CaseTesting, Episode: 2, Evidence: 4, Mechanism: "one cause", NextTest: "one fix"}
	held := NewPersonHold(testing2, ".github/workflows/ci.yml", "Run the tests.", "CI configuration")
	line := CaseEntry{Action: ActionPersonHeld, Detail: PersonDetail(held)}
	with := func(f func(*Case)) Case {
		c := testing2
		f(&c)
		return c
	}
	for _, tc := range []struct {
		name  string
		c     Case
		log   []CaseEntry
		holds bool
	}{
		{"held", testing2, []CaseEntry{line}, true},
		{"new evidence", with(func(c *Case) { c.Evidence = 9 }), []CaseEntry{line, {Action: "evidence", Detail: "5 new"}}, true},
		{"a title", testing2, []CaseEntry{line, {Action: "set", Detail: "title: the thing breaks; fix: in CI"}}, true},
		{"a benefit", testing2, []CaseEntry{line, {Action: "set", Detail: "benefit: fewer breaks; mechanism: none; status x -> y"}}, true},
		{"reopened, same evidence", with(func(c *Case) { c.Episode = 3 }), []CaseEntry{line}, false},
		{"not testing", with(func(c *Case) { c.Status = CaseInvestigating }), []CaseEntry{line}, false},
		{"ended", testing2, []CaseEntry{line, {Action: ActionPersonEnded, Detail: "the case left testing"}}, false},
		{"status", testing2, []CaseEntry{line, {Action: "set", Detail: "status testing -> investigating"}}, false},
		{"mechanism", with(func(c *Case) { c.Mechanism = "another cause" }), []CaseEntry{line, {Action: "set", Detail: "mechanism: another cause"}}, false},
		{"fix", with(func(c *Case) { c.NextTest = "another fix" }), []CaseEntry{line, {Action: "set", Detail: "fix: another fix"}}, false},
		{"a status change before it", testing2, []CaseEntry{{Action: "set", Detail: "status investigating -> testing"}, line}, true},
	} {
		got, ok := PersonHoldOf(tc.c, tc.log)
		if ok != tc.holds || ok && got != held {
			t.Errorf("%s: %+v %v", tc.name, got, ok)
		}
	}
	// a hold that no longer stands is still open until its end is written
	if h, ok := LastPersonHold([]CaseEntry{line, {Action: "set", Detail: "status testing -> open"}}); !ok || h != held {
		t.Errorf("the open hold: %+v %v", h, ok)
	}
	if _, ok := LastPersonHold([]CaseEntry{line, {Action: ActionPersonEnded, Detail: "the case is in a new episode"}}); ok {
		t.Error("an ended hold is open")
	}
}
