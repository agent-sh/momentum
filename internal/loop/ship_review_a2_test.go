package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProposalRefusesAProtectedEditDuringValidation(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/momentum", nil }
	c, p, pr := recoveryChecked(t, s, st, f)
	s.Public = func(_ context.Context, text string) (string, error) {
		write(t, filepath.Join(p.Work, "internal/sandbox/probe.go"), "package sandbox // marker-7f3a\n")
		return text, nil
	}
	q, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	if tree := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", q.Head+"^{tree}")); tree != p.Check.Tree {
		t.Fatal("concurrent run write changed the commit")
	}
	if strings.Contains(run(t, p.Host, "git", "ls-tree", "-r", q.Head), "internal/sandbox") {
		t.Fatal("protected run file shipped")
	}
}

func TestFetchedBranchDoesNotWeakenProposalLease(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	ctx := context.Background()
	if _, err := s.Reviewed(ctx, c.ID, Review{Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "p", Fix: "f"}}, Comment: "c1"}); err != nil {
		t.Fatal(err)
	}
	theirs := pushOther(t, f, p.Branch, map[string]string{"theirs.txt": "marker-7f3a\n"})
	run(t, f.main, "git", "fetch", "origin", p.Branch)
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fixed("42"), "// Answer is the answer.", "// Answer is 42.", 1))
	if _, err := checkPatch(t, s, ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil); err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	if _, err := proposePatch(t, s, ctx, c.ID, pr); err == nil {
		t.Fatal("revision overwrote the fetched remote change")
	}
	if originHead(t, f, p.Branch) != theirs {
		t.Fatal("other push was lost")
	}
}

func TestProposalHookRefusesAProtectedEditBeforeCommit(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/momentum", nil }
	c, p, pr := recoveryChecked(t, s, st, f)
	s.BeforeCommit = func() {
		write(t, filepath.Join(p.Work, "internal/sandbox/probe.go"), "package sandbox // marker-7f3a\n")
	}
	q, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	if tree := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", q.Head+"^{tree}")); tree != p.Check.Tree {
		t.Fatal("concurrent run write changed the commit")
	}
	if strings.Contains(run(t, p.Host, "git", "ls-tree", "-r", q.Head), "internal/sandbox") {
		t.Fatal("protected run file shipped")
	}
}

func TestFetchedBranchDoesNotWeakenRebasedLandingLease(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"base.txt": "marker-7f3a\n"})
	var theirs string
	s.Sandbox.Gate = func(context.Context) error {
		theirs = pushOther(t, f, p.Branch, map[string]string{"theirs.txt": "marker-7f3a\n"})
		run(t, f.main, "git", "fetch", "origin", p.Branch)
		return nil
	}
	if _, err := s.Land(context.Background(), c.ID); err == nil {
		t.Fatal("rebased landing overwrote the fetched branch")
	}
	if theirs == "" || originHead(t, f, p.Branch) != theirs {
		t.Fatal("another push was lost")
	}
	if strings.TrimSpace(run(t, p.Host, "git", "rev-parse", "HEAD")) != p.Head {
		t.Fatal("reviewed head was not restored")
	}
}

func TestHostWritesIgnorePlantedRunFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	write(t, canary, "marker-7f3a")
	for _, name := range []string{"system.md", "schema.json", "transcript.jsonl", "stderr.txt"} {
		if err := os.Symlink(canary, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	host := HostDir(dir)
	if err := hostDir(host); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []string{"primary", "fallback"} {
		input := filepath.Join(host, attempt)
		if err := hostDir(input); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"system.md", "schema.json"} {
			if err := writeExclusive(input, name, []byte("safe input")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeExclusive(host, "transcript.jsonl", []byte(okResult+"\n")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "replacement"), `{"type":"result","subtype":"success","total_cost_usd":0,"structured_output":{"summary":"fake"}}`)
	if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "transcript.jsonl")); err != nil {
		t.Fatal(err)
	}
	result, err := readStream(filepath.Join(host, "transcript.jsonl"))
	if err != nil || result.Harness.USD == nil || *result.Harness.USD != .75 || result.USD != 0 {
		t.Fatalf("replacement changed the result: %+v %v", result, err)
	}
	if b, _ := os.ReadFile(canary); string(b) != "marker-7f3a" {
		t.Fatal("host canary changed")
	}
	if err := writeExclusive(host, "transcript.jsonl", []byte("replacement")); err == nil {
		t.Fatal("host archive was replaced")
	}
}

func TestProposalAdoptsTheExactPushAfterALostResponse(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, _, pr := recoveryChecked(t, s, st, f)
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.dir, "lose-response")
	write(t, marker, "marker-7f3a")
	bin := filepath.Join(f.dir, "bin")
	script(t, bin, "git", `case " $* " in
 *" push "*)
  if [ -f '`+marker+`' ]; then
   rm '`+marker+`'
   '`+gitBinary+`' "$@" || exit $?
   echo 'fixture lost the push response' >&2
   exit 1
  fi ;;
esac
exec '`+gitBinary+`' "$@"`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil || p.PublishedHead != p.Head || p.State != Proposed {
		t.Fatalf("lost response was not adopted: %+v %v", p, err)
	}
	saved, _, err := s.Load(c.ID)
	if err != nil || saved.PublishedHead != p.Head || originHead(t, f, p.Branch) != p.Head {
		t.Fatal("publication receipt was not persisted")
	}
}

func TestPackageReadsRefuseTraversal(t *testing.T) {
	s := Ship{Dir: t.TempDir()}
	for _, name := range []string{"", ".", "..", "../work/fixture", "/fixture"} {
		if _, _, err := s.Load(name); err == nil {
			t.Fatalf("package path %q accepted", name)
		}
	}
}
