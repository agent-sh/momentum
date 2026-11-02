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

// A loop run may not close, send back or merge by a root a case whose change is in progress (prepared, checked,
// proposed, revising or landing): its work tree, and its pull request when it has one, would stay with nothing in
// the loop to return to them. Its build resumes it, and its review decides.
func TestARunLeavesAProposedCaseToItsReview(t *testing.T) {
	for _, state := range []string{"prepared", "checked", "proposed", "revising", "landing"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("MOMENTUM_HOME", t.TempDir())
			t.Setenv("MOMENTUM_CONFIG", t.TempDir())
			fakeScrubber(t)
			ctx := context.Background()
			st, err := store.Open(paths.DB())
			if err != nil {
				t.Fatal(err)
			}
			c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:p", Title: "the thing breaks",
				Evidence: []store.Evidence{{Ref: "file:/t/p", At: time.Now().Add(-time.Hour)}}}, "test")
			if err != nil {
				t.Fatal(err)
			}
			mech, next := "the lock order", "take the lock first"
			if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			pkg := filepath.Join(loopDir(), "packages", c.ID)
			os.MkdirAll(pkg, 0o700)
			os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"case":"`+c.ID+`","state":"`+state+`"}`), 0o600)
			t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
			t.Setenv("MOMENTUM_LOOP_CASE", c.ID)
			for _, args := range [][]string{
				{"case", "close", c.ID, "--reason", "already-solved", "--why", "a later commit fixed it"},
				{"case", "set", c.ID, "--status", "investigating"},
				{"case", "set", c.ID, "--status=investigating"},
				{"case", "set", c.ID, "-status", "investigating"},
				{"case", "set", c.ID, "-status=investigating"},
				{"case", "set", c.ID, "--mechanism", "the order", "--status", "investigating"},
				{"case", "set", c.ID, "--mechanism", "--", "--status", "investigating"},
				{"case", "set", c.ID, "--alternative", "--", "-status=investigating"},
				{"case", "signature", c.ID, "root:lock-order"},
			} {
				if code, _, diag := momentum(args...); code != 2 || !strings.Contains(diag, state) {
					t.Errorf("%v: exit %d %s", args, code, diag)
				}
			}
			if code, _, diag := momentum("case", "set", c.ID, "--benefit", "fewer stalls"); code != 0 {
				t.Errorf("a field of a proposed case: exit %d %s", code, diag)
			}
			st, err = store.Open(paths.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
				t.Fatalf("the case is %s now", got.Status)
			}
		})
	}
}
