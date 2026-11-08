package loop

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A scope check that cannot decide holds the build for this pass, as a repository in use does: report only for
// reasons that each say it could not tell (activity or provenance unknown, metadata it could not verify), an exit
// other than 0 with no decision in its output, or an allow with such an exit. A decided report only refuses the write:
// an exclusion, inactivity, another's repository, or any of those beside an unknown. An allow lets it through. The
// gate passes each on as it is, held or not, with its reasons.
func TestAScopeCheckThatCannotDecideHoldsTheBuild(t *testing.T) {
	ctx := context.Background()
	f := newRepo(t, nil)
	answer := filepath.Join(f.dir, "answer")
	guard := script(t, f.dir, "scope", `cat "`+answer+`"; exit "$(cat "`+answer+`.exit")"`)
	g := Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{guard},
		Resolve: func(context.Context, string) (string, error) { return "agent-sh/fake", nil },
		Busy:    func(context.Context, string) (string, error) { return "", nil }}
	reportOnly := func(reasons ...string) string {
		q := make([]string, len(reasons))
		for i, r := range reasons {
			q[i] = strconv.Quote(r)
		}
		return `{"allowed": false, "disposition": "report_only", "reasons": [` + strings.Join(q, ", ") + `]}`
	}
	const (
		unknown = "non-bot development activity unknown: a decision required"
		allowed = `{"allowed": true, "disposition": "allowed_scope_only", "reasons": []}`
	)
	for _, c := range []struct {
		name, out string
		exit      int
		want      string // hold, refusal or pass
	}{
		{"activity unknown", reportOnly(unknown), 2, "hold"},
		{"metadata it could not verify", reportOnly("could not verify repository, item or activity metadata"), 2, "hold"},
		{"each reason undecided", reportOnly("repository identity unavailable", "active-agent metadata inaccessible; cannot establish idleness"), 2, "hold"},
		{"no output", "", 1, "hold"},
		{"no JSON", "Traceback (most recent call last):", 1, "hold"},
		{"no allowed in it", `{"reasons": []}`, 0, "hold"},
		{"an allow with an exit of 1", allowed, 1, "hold"},
		{"excluded", reportOnly("existing do-not-touch exclusion"), 2, "refusal"},
		{"inactive", reportOnly("repository dormant over 180 days: a decision required"), 2, "refusal"},
		{"another's", reportOnly("external repository"), 2, "refusal"},
		{"an unknown beside an exclusion", reportOnly("external repository", unknown), 2, "refusal"},
		{"report only for no reason", reportOnly(), 2, "refusal"},
		{"report only with an allow", `{"allowed": true, "disposition": "report_only", "reasons": ["archived repository"]}`, 0, "refusal"},
		{"allowed", allowed, 0, "pass"},
	} {
		write(t, answer, c.out)
		write(t, answer+".exit", strconv.Itoa(c.exit))
		err := g.scope(ctx, "agent-sh/fake", f.main)
		switch c.want {
		case "hold":
			if !held(err) || !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "the build waits for the next pass") {
				t.Errorf("%s: want a hold, got %v (held %v)", c.name, err, held(err))
			}
		case "refusal":
			if held(err) || !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "report only") {
				t.Errorf("%s: want a refusal, got %v (held %v)", c.name, err, held(err))
			}
		case "pass":
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		}
	}

	// the gate gives the build's step the scope check's hold as it is, its reasons with it, and a decision's report only
	write(t, answer, reportOnly(unknown))
	write(t, answer+".exit", "2")
	if _, err := g.Repository(ctx, f.main); !held(err) || !strings.Contains(err.Error(), "could not decide for agent-sh/fake ("+unknown+"): the build waits") {
		t.Errorf("the gate's hold: %v", err)
	}
	write(t, answer, reportOnly("external repository"))
	if _, err := g.Repository(ctx, f.main); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "does not allow agent-sh/fake (report_only external repository): report only") {
		t.Errorf("the gate's refusal: %v", err)
	}
	write(t, answer, allowed)
	write(t, answer+".exit", "0")
	if _, err := g.Repository(ctx, f.main); err != nil {
		t.Errorf("the gate's allow: %v", err)
	}
}
