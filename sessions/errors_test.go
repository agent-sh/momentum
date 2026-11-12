package sessions

import (
	"testing"
	"time"
)

// An API error holds a notification when its kind is an access kind and its words do not read as transient, or,
// with no kind, when its words read as an access error; a limit holds only until the reset it states.
func TestHeldByReadsTheKindAndTheWords(t *testing.T) {
	errAt := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	const limit = "You've hit your weekly limit · resets Mar 7, 9am (Europe/Lisbon)"
	const org = "Your organization has disabled Claude subscription access"
	soon, late := errAt.Add(time.Hour), time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name, kind, words string
		at                time.Time
		held              bool
	}{
		{"a usage limit", "rate_limit", limit, soon, true},
		{"a usage limit past its reset", "rate_limit", limit, late, false},
		{"a throttle named rate_limit", "rate_limit", "API Error: 429 Too many requests, slow down", soon, false},
		{"a failed login", "authentication_failed", "Login expired · Please run /login", soon, true},
		{"a transient error", "server_error", "API Error: 529 overloaded", soon, false},
		{"organization words on a kind that is no access kind", "model_not_found", org, soon, false},
		{"a usage limit, no kind", "", limit, soon, true},
		{"organization words, no kind", "", org, soon, true},
		{"a throttle, no kind", "", "API Error: 429 Too many requests, slow down", soon, false},
	} {
		if got := HeldBy(row.kind, row.words, errAt, row.at); got != row.held {
			t.Errorf("%s: held %v, want %v", row.name, got, row.held)
		}
	}
}
