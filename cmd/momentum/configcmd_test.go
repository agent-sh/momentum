package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// A case in testing closes as already solved by the fix a configuration proposal's note names: the file and the hash
// its change leaves. The close names the fix in the case's log; a reference that names no hash is refused.
func TestACaseClosesAsSolvedByAConfigurationFix(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:rules", Title: "the rule breaks", Evidence: []store.Evidence{{Ref: "file:/t/marker-c0ffee"}}}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "the rule says the old thing", "say the new thing"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// a quote in the file's name: the refusal quotes the fix as a shell word
	fix := "config:~/.config/agents/it's.md@" + strings.Repeat("ab", 32)
	// the case's filed proposal names its fix: another is refused
	pkg := filepath.Join(loopDir(), "packages", c.ID, "package.json")
	if err := os.MkdirAll(filepath.Dir(pkg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte(`{"case":"`+c.ID+`","kind":"config","state":"proposed","target":".config/agents/it's.md","target_new":"`+strings.Repeat("ab", 32)+
		`","head":"`+strings.Repeat("c0ffee", 7)[:40]+`","filed":"2026-11-07T00:00:00Z","episode":`+fmt.Sprint(c.Episode)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	other := "config:~/.config/agents/RULES.md@" + strings.Repeat("cd", 32)
	quoted := "--fix 'config:~/.config/agents/it'\"'\"'s.md@" + strings.Repeat("ab", 32) + "'"
	if code := runCommand(ctx, []string{"case", "close", c.ID, "--reason", "already-solved", "--fix", other, "--why", "applied"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), quoted) {
		t.Fatalf("a fix other than the filed proposal's: exit %d %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runCommand(ctx, []string{"case", "close", c.ID, "--reason", "already-solved", "--why", "applied"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), quoted) {
		t.Fatalf("a close as already solved without the filed proposal's fix: exit %d %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runCommand(ctx, []string{"case", "close", c.ID, "--reason", "already-solved", "--fix", "config:~/.config/agents/RULES.md", "--why", "applied"}, &out, &errOut); code != 2 {
		t.Fatalf("a fix with no hash: exit %d %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runCommand(ctx, []string{"case", "close", c.ID, "--reason", "already-solved", "--fix", fix, "--why", "an agent applied the proposal"}, &out, &errOut); code != 0 {
		t.Fatalf("the close: exit %d %s", code, errOut.String())
	}
	if st, err = store.Open(paths.DB()); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, _ := st.CaseByID(ctx, c.ID)
	log, _ := st.CaseLog(ctx, c.ID)
	named := false
	for _, e := range log {
		named = named || e.Action == "closed" && strings.Contains(e.Detail, fix)
	}
	if got.Status != store.CaseClosed || got.Reason != store.Solved || !named {
		t.Fatalf("the case closed by a configuration fix: %+v %+v", got, log)
	}
}

// case close's help names a configuration fix in the form a close takes: config:~/PATH@SHA256.
func TestCaseCloseHelpNamesTheConfigurationFixForm(t *testing.T) {
	var out, errOut bytes.Buffer
	runCommand(context.Background(), []string{"case", "close", "c0ffee", "--help"}, &out, &errOut)
	if help := out.String() + errOut.String(); !strings.Contains(help, "config:~/PATH@SHA256") {
		t.Fatalf("case close's help: %s", help)
	}
}
