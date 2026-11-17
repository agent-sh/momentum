package supervise

import "testing"

// This file holds the tests of the rule changes the loop lands itself. The loop appends whole test functions to
// it and changes nothing it holds (internal/loop: selfTests, appendedTests). Its name sorts last in the package,
// so these tests run after every other test of the package. Tests a person writes go in the other files.

// Every rule the engine names has its function, and every function its name.
func TestEveryRuleHasItsFunction(t *testing.T) {
	for _, rule := range Rules {
		if ruleFuncs[rule] == nil {
			t.Errorf("the rule %s has no function", rule)
		}
	}
	if len(ruleFuncs) != len(Rules) {
		t.Errorf("%d rule functions for %d rules", len(ruleFuncs), len(Rules))
	}
}
