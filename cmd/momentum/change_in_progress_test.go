package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// A case in testing, with a change in the given state in the loop's directory, and the environment of a loop run on it.
func caseWithChange(t *testing.T, state, pr string) string {
	t.Helper()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:p", Title: "the thing breaks",
		Evidence: []store.Evidence{{Ref: "file:/t/p", At: time.Now().Add(-time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "the lock order", "take the lock first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(loopDir(), "packages", c.ID)
	os.MkdirAll(pkg, 0o700)
	body := `{"case":"` + c.ID + `","state":"` + state + `"`
	if pr != "" {
		body += `,"pr":"` + pr + `"`
	}
	os.WriteFile(filepath.Join(pkg, "package.json"), []byte(body+"}"), 0o600)
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_CASE", c.ID)
	return c.ID
}

// A loop run may not close, send back or merge by a root a case whose change is on its pull request (proposed,
// revising, landing, or checked again after a review): the pull request would stay with nothing in the loop to return
// to it. Its build resumes it, and its review decides.
func TestARunLeavesAProposedCaseToItsReview(t *testing.T) {
	for _, state := range []string{"checked", "proposed", "revising", "landing"} {
		t.Run(state, func(t *testing.T) {
			id := caseWithChange(t, state, "https://example.com/pr/1")
			for _, args := range [][]string{
				{"case", "close", id, "--reason", "already-solved", "--why", "a later commit fixed it"},
				{"case", "set", id, "--status", "investigating"},
				{"case", "set", id, "--status=investigating"},
				{"case", "set", id, "-status", "investigating"},
				{"case", "set", id, "-status=investigating"},
				{"case", "set", id, "--mechanism", "the order", "--status", "investigating"},
				{"case", "set", id, "--mechanism", "--", "--status", "investigating"},
				{"case", "set", id, "--alternative", "--", "-status=investigating"},
				{"case", "signature", id, "root:lock-order"},
			} {
				if code, _, diag := momentum(args...); code != 2 || !strings.Contains(diag, state) {
					t.Errorf("%v: exit %d %s", args, code, diag)
				}
			}
			if code, _, diag := momentum("case", "set", id, "--benefit", "fewer stalls"); code != 0 {
				t.Errorf("a field of a proposed case: exit %d %s", code, diag)
			}
			st, err := store.Open(paths.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if got, _ := st.CaseByID(context.Background(), id); got.Status != store.CaseTesting {
				t.Fatalf("the case is %s now", got.Status)
			}
		})
	}
}

// A change before its proposal (prepared, or checked with no pull request) is no bar: a build that finds it cannot go
// on closes its case or sends it back, and the loop drops the change at its next run.
func TestARunMayEndACaseWhoseChangeIsBeforeItsProposal(t *testing.T) {
	for _, state := range []string{"prepared", "checked"} {
		for _, args := range [][]string{{"close", "--reason", "not-worth-it", "--why", "its plan cannot count the problem"}, {"set", "--status", "investigating"}} {
			t.Run(state+" "+args[0], func(t *testing.T) {
				id := caseWithChange(t, state, "")
				if code, _, diag := momentum(append([]string{"case", args[0], id}, args[1:]...)...); code != 0 {
					t.Fatalf("exit %d %s", code, diag)
				}
			})
		}
	}
}

// Outside a loop run, nothing keeps a person from closing a case, whatever its change.
func TestAPersonMayCloseACaseWhoseChangeIsProposed(t *testing.T) {
	id := caseWithChange(t, "proposed", "https://example.com/pr/1")
	t.Setenv("MOMENTUM_LOOP_RUN", "")
	t.Setenv("MOMENTUM_LOOP_CASE", "")
	if code, _, diag := momentum("case", "close", id, "--reason", "already-solved", "--why", "a later commit fixed it"); code != 0 {
		t.Fatalf("exit %d %s", code, diag)
	}
}
