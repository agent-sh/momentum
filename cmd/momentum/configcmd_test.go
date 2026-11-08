package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// filedConfigCase is a case in testing whose configuration proposal of target is filed, the change leaving the hash
// leaves, and the file in the home as content holds it.
func filedConfigCase(t *testing.T, target, leaves, content string) store.Case {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := store.Finding{Family: "failures", Signature: "failures/v1:" + leaves[:8], Title: "the rule breaks", Evidence: []store.Evidence{{Ref: "file:/t/marker-" + leaves[:8]}}}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "the rule says the old thing", "say the new thing"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(loopDir(), "packages", c.ID, "package.json")
	if err := os.MkdirAll(filepath.Dir(pkg), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"case": c.ID, "kind": "config", "state": "proposed", "target": target, "target_new": leaves,
		"head": strings.Repeat("c0ffee", 7)[:40], "filed": "2026-11-07T00:00:00Z", "episode": c.Episode})
	if err := os.WriteFile(pkg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	homeFile(t, target, content)
	return c
}

// homeFile writes a file under the test's home, as an agent or the owner edits it.
func homeFile(t *testing.T, target, content string) {
	t.Helper()
	file := filepath.Join(os.Getenv("HOME"), target)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// closeAsSolved is case close --reason already-solved with fix, or none: its exit status and what it said.
func closeAsSolved(t *testing.T, id, fix string) (int, string) {
	t.Helper()
	args := []string{"case", "close", id, "--reason", "already-solved", "--why", "applied"}
	if fix != "" {
		args = append(args, "--fix", fix)
	}
	var out, errOut bytes.Buffer
	code := runCommand(context.Background(), args, &out, &errOut)
	return code, errOut.String()
}

// A case whose configuration proposal is filed closes as already solved by its file with the hash the file holds now:
// the note's fix once the agent applied it. Another file, no fix and no hash are refused, and the refusal names the
// fix the file holds now, quoted as a shell word. The close names the fix in the case's log.
func TestACaseClosesAsSolvedByAConfigurationFix(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	applied := "marker-c0ffee the new rule\n"
	target := ".config/agents/it's.md" // a quote in the file's name: the refusal quotes the fix as a shell word
	c := filedConfigCase(t, target, sha(applied), applied)
	fix := "config:~/" + target + "@" + sha(applied)
	quoted := "--fix 'config:~/.config/agents/it'\"'\"'s.md@" + sha(applied) + "'"
	for _, bad := range []string{"config:~/.config/agents/zz-rules.md@" + strings.Repeat("cd", 32), "", "config:~/" + target} {
		if code, said := closeAsSolved(t, c.ID, bad); code != 2 || bad != "config:~/"+target && !strings.Contains(said, quoted) {
			t.Fatalf("a close as already solved with %q: exit %d %s", bad, code, said)
		}
	}
	if code, said := closeAsSolved(t, c.ID, fix); code != 0 {
		t.Fatalf("the close: exit %d %s", code, said)
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, _ := st.CaseByID(context.Background(), c.ID)
	log, _ := st.CaseLog(context.Background(), c.ID)
	named := false
	for _, e := range log {
		named = named || e.Action == "closed" && strings.Contains(e.Detail, fix)
	}
	if got.Status != store.CaseClosed || got.Reason != store.Solved || !named {
		t.Fatalf("the case closed by a configuration fix: %+v %+v", got, log)
	}
	// the close settles the proposal there and then: withdrawn, not left to the next pass
	raw, err := os.ReadFile(filepath.Join(loopDir(), "packages", c.ID, "package.json"))
	var pkg struct {
		State string `json:"state"`
	}
	if err != nil || json.Unmarshal(raw, &pkg) != nil || pkg.State != "withdrawn" {
		t.Fatalf("the closed case's proposal: %s %v", raw, err)
	}
}

// A filed proposal whose file the owner edited after the filing: its apply line can no longer pass, and the agent
// makes the change by hand on the file as it is. The case closes with the hash the file holds now; the note's own fix,
// which the file does not hold, is refused.
func TestAnEditedFiledProposalClosesWithTheHashItsFileHolds(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	target := ".config/agents/zz-rules.md"
	c := filedConfigCase(t, target, sha("marker-c0ffee the new rule\n"), "marker-c0ffee the old rule\n")
	byHand := "marker-c0ffee the new rule\nthe owner's own line\n"
	homeFile(t, target, byHand)
	if code, said := closeAsSolved(t, c.ID, "config:~/"+target+"@"+sha("marker-c0ffee the new rule\n")); code != 2 || !strings.Contains(said, sha(byHand)) {
		t.Fatalf("the note's fix over a file that holds another: exit %d %s", code, said)
	}
	if code, said := closeAsSolved(t, c.ID, "config:~/"+target+"@"+sha(byHand)); code != 0 {
		t.Fatalf("the same file's fix with the hash it holds now: exit %d %s", code, said)
	}
}

// Two proposals on one file: A is applied first, so B's apply line fails on the file A left. A closes with its fix;
// B's own fix is refused, and once its change is made by hand on the file as it is, B closes with the hash it leaves.
func TestTwoProposalsOnOneFileCloseInTurn(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	target, base := ".config/agents/zz-rules.md", "line a\nline b\n"
	withA, withB, both := "line A\nline b\n", "line a\nline B\n", "line A\nline B\n"
	a := filedConfigCase(t, target, sha(withA), base)
	b := filedConfigCase(t, target, sha(withB), base)
	homeFile(t, target, withA)
	if code, said := closeAsSolved(t, a.ID, "config:~/"+target+"@"+sha(withA)); code != 0 {
		t.Fatalf("A's close: exit %d %s", code, said)
	}
	if code, said := closeAsSolved(t, b.ID, "config:~/"+target+"@"+sha(withB)); code != 2 || !strings.Contains(said, sha(withA)) {
		t.Fatalf("B's own fix over the file A left: exit %d %s", code, said)
	}
	homeFile(t, target, both)
	if code, said := closeAsSolved(t, b.ID, "config:~/"+target+"@"+sha(both)); code != 0 {
		t.Fatalf("B's close by hand on the file A left: exit %d %s", code, said)
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

// A filed proposal whose file became a link, or is gone, still closes as already solved: the close cannot check the
// file, so it names it with what it found (config:~/PATH@unchecked:link, @unchecked:missing), and any hash is refused
// with that fix named.
func TestAFiledProposalWhoseFileCannotBeCheckedClosesUnchecked(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	target := ".config/agents/zz-rules.md"
	applied := "marker-c0ffee the new rule\n"
	for _, found := range []string{"link", "missing"} {
		t.Run(found, func(t *testing.T) {
			c := filedConfigCase(t, target, sha(applied+found), applied)
			file := filepath.Join(os.Getenv("HOME"), target)
			if found == "link" {
				kept := filepath.Join(os.Getenv("HOME"), "kept", "zz-rules.md")
				if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(file, kept); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(kept, file); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			unchecked := "config:~/" + target + "@unchecked:" + found
			if code, said := closeAsSolved(t, c.ID, "config:~/"+target+"@"+sha(applied)); code != 2 || !strings.Contains(said, unchecked) {
				t.Fatalf("a hash over a file that cannot be read: exit %d %s", code, said)
			}
			if code, said := closeAsSolved(t, c.ID, unchecked); code != 0 {
				t.Fatalf("the unchecked close: exit %d %s", code, said)
			}
		})
	}
}

// holdCaseLock takes a case's ship lock as a pass that works the case holds it, until the test ends.
func holdCaseLock(t *testing.T, id string) {
	t.Helper()
	dir := filepath.Join(loopDir(), "packages", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() })
}

// closeCase is case close as not worth it: its exit status and what it said on standard error.
func closeCase(t *testing.T, id string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runCommand(context.Background(), []string{"case", "close", id, "--reason", "not-worth-it", "--why", "declined"}, &out, &errOut)
	return code, errOut.String()
}

// A close of a case with no configuration package touches no package: it takes no lock and makes no package
// directory, and with a repository package whose case a pass holds it says nothing of a configuration proposal.
func TestACloseOfACaseWithNoConfigurationPackageTouchesNoPackage(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, key := range []string{"a1b2c3d4", "e5f6a7b8"} {
		c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:" + key, Title: "it breaks",
			Evidence: []store.Evidence{{Ref: "file:/t/marker-" + key}}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	st.Close()
	if code, said := closeCase(t, ids[0]); code != 0 {
		t.Fatalf("the close: exit %d %s", code, said)
	}
	if _, err := os.Stat(filepath.Join(loopDir(), "packages", ids[0])); err == nil {
		t.Fatal("the close of a case with no package made its package directory")
	}
	pkg := filepath.Join(loopDir(), "packages", ids[1], "package.json")
	if err := os.MkdirAll(filepath.Dir(pkg), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"case": ids[1], "kind": "repo", "state": "checked", "repo": "agent-sh/fake"})
	if err := os.WriteFile(pkg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	holdCaseLock(t, ids[1])
	if code, said := closeCase(t, ids[1]); code != 0 || strings.Contains(said, "configuration proposal") {
		t.Fatalf("the close of a case with a repository package: exit %d %s", code, said)
	}
}

// The close resolves the case's configuration note first: a close whose settlement cannot run now (a pass holds the
// case) leaves no note standing, and says the next pass settles the proposal.
func TestACloseResolvesTheConfigurationNoteFirst(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	applied := "marker-c0ffee the new rule\n"
	c := filedConfigCase(t, ".config/agents/zz-rules.md", sha(applied), applied)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := "loop-config:" + store.IDPart(c.ID)
	if _, err := st.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, Title: "The loop proposes a change",
		Detail: "Apply it.", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	holdCaseLock(t, c.ID)
	if code, said := closeCase(t, c.ID); code != 0 || !strings.Contains(said, "settled by the next pass") {
		t.Fatalf("the close while a pass holds the case: exit %d %s", code, said)
	}
	if n, ok, err := st.Note(ctx, id); err != nil || !ok || n.Done.IsZero() {
		t.Fatalf("the configuration note after the close: %+v %v %v", n, ok, err)
	}
}
