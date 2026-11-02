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

// A loop run may not close, send back or merge by a root a case whose change is proposed: its pull request would
// stay open with nothing in the loop to return to it. Its revision answers the review.
func TestARunLeavesAProposedCaseToItsReview(t *testing.T) {
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
	os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"case":"`+c.ID+`","state":"revising"}`), 0o600)
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_CASE", c.ID)
	for _, args := range [][]string{
		{"case", "close", c.ID, "--reason", "already-solved", "--why", "a later commit fixed it"},
		{"case", "set", c.ID, "--status", "investigating"},
		{"case", "signature", c.ID, "root:lock-order"},
	} {
		if code, _, diag := momentum(args...); code != 2 || !strings.Contains(diag, "revising") {
			t.Errorf("%v: exit %d %s", args, code, diag)
		}
	}
	if code, _, diag := momentum("case", "set", c.ID, "--benefit", "fewer stalls"); code != 0 {
		t.Errorf("a field of a proposed case: exit %d %s", code, diag)
	}
}
