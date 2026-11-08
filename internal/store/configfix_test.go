package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A case closes as already solved by what fixed it, named as a reference: a configuration fix names the file and the
// SHA-256 its change leaves (config:PATH@SHA256). The close names it in the case's log. A reference of no known form is
// refused, and a component-health case names its fix through CloseFixed, which resumes the cases its stop held.
func TestACaseClosesAsSolvedByAConfigurationFix(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "config-fix", Title: "a case",
		Evidence: []Evidence{{Ref: "file:/a@1", At: time.Now()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"config:~/.config/agents/zz-rules.md", "config:.config/agents/zz-rules.md@" + strings.Repeat("ab", 32),
		"config:~/.config/agents/zz-rules.md@abc", "git:agent-sh/momentum@4814cc4"} {
		if _, err := st.CloseSolvedBy(ctx, c.ID, bad, "applied", "test"); !errors.Is(err, ErrBadCase) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	fix := "config:~/.config/agents/zz-rules.md@" + strings.Repeat("ab", 32)
	got, err := st.CloseSolvedBy(ctx, c.ID, fix, "an agent applied the proposal", "test")
	if err != nil || got.Status != CaseClosed || got.Reason != Solved {
		t.Fatalf("the close: %+v %v", got, err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	if l := log[len(log)-1]; l.Action != "closed" || !strings.Contains(l.Detail, fix) {
		t.Fatalf("the close does not name the fix: %+v", l)
	}
	h, _, err := st.ObserveCase(ctx, Finding{Family: HealthFamily, Signature: "health", Title: "a stop",
		Evidence: []Evidence{{Ref: "file:/b@1", At: time.Now()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseSolvedBy(ctx, h.ID, fix, "x", "test"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("a component-health case: %v", err)
	}
}

// A configuration fix names a file under the home by a clean relative path, never one outside it, and is no evidence.
func TestAConfigurationFixNamesAFileUnderTheHome(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "config-fix-path", Title: "a case",
		Evidence: []Evidence{{Ref: "file:/a@1", At: time.Now()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("ab", 32)
	for _, bad := range []string{"config:/etc/passwd@" + sha, "config:~/../../etc/shadow@" + sha, "config:~/.config/./agents/zz-rules.md@" + sha, "config:~/@" + sha} {
		if _, err := st.CloseSolvedBy(ctx, c.ID, bad, "applied", "test"); !errors.Is(err, ErrBadCase) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, _, err := st.ObserveCase(ctx, Finding{Family: "failures", Signature: "config-evidence", Title: "a case",
		Evidence: []Evidence{{Ref: "config:~/.config/agents/zz-rules.md@" + sha, At: time.Now()}}}, "test"); !errors.Is(err, ErrBadCase) {
		t.Fatalf("a configuration fix as evidence: %v", err)
	}
}
