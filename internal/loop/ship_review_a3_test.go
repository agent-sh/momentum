package loop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

func TestLandingPreservesAMovedProposalBranch(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, p := recoveryReviewed(t, s, st, f)
	other := strings.TrimSpace(run(t, p.Host, "git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"commit-tree", p.Head+"^{tree}", "-p", p.Head, "-m", "second writer"))
	realGit, _ := exec.LookPath("git")
	bin := filepath.Join(f.dir, "bin")
	script(t, bin, "git", `base=no
for arg do if [ "$arg" = "`+p.Head+`:refs/heads/main" ]; then base=yes; fi; done
'`+realGit+`' "$@"
code=$?
if [ "$code" = 0 ] && [ "$base" = yes ]; then
 '`+realGit+`' -C '`+p.Host+`' push --no-follow-tags origin '`+other+`:refs/heads/`+p.Branch+`' || exit 1
fi
exit "$code"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	landed, err := s.Land(ctx, c.ID)
	if landed.State != Landed || err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("landing cleanup did not report refusal: %+v %v", landed, err)
	}
	if originHead(t, f, p.Branch) != other || originHead(t, f, "main") != p.Head {
		t.Fatal("landing deleted a second writer's branch or failed to land")
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Status != store.CaseDeployed {
		t.Fatalf("successful landing receipt was lost: %s", got.Status)
	}
	if _, err := s.Land(ctx, c.ID); err == nil || originHead(t, f, p.Branch) != other {
		t.Fatal("cleanup refusal allowed a repeated landing")
	}
}

func TestLandingKeepsAnUncertainPublishedCandidate(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, p := recoveryReviewed(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"README": "next base\n"})
	realGit, _ := exec.LookPath("git")
	bin, lost := filepath.Join(f.dir, "bin"), filepath.Join(f.dir, "lost-response")
	script(t, bin, "git", `proposal=no
probe=no
for arg do
 case "$arg" in *:refs/heads/`+p.Branch+`) proposal=yes ;; ls-remote) probe=yes ;; esac
done
if [ "$probe" = yes ] && [ -f '`+lost+`' ]; then exit 1; fi
'`+realGit+`' "$@"
code=$?
if [ "$code" = 0 ] && [ "$proposal" = yes ] && [ ! -f '`+lost+`' ]; then
 echo lost > '`+lost+`'
 exit 1
fi
exit "$code"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("fixture did not lose the push response and its reconciliation")
	}
	accepted := originHead(t, f, p.Branch)
	saved, _, err := s.Load(c.ID)
	if err != nil || saved.State != Landing || saved.LandingHead != accepted || saved.PublishedHead != p.Head {
		t.Fatalf("uncertain candidate was discarded: %+v %v", saved, err)
	}
	// A new committer time must not produce another rebase candidate on retry.
	t.Setenv("GIT_COMMITTER_DATE", "2030-01-01T00:00:00Z")
	os.Remove(lost)
	// Restore the normal Git executable, retaining the exact uncertain journal.
	t.Setenv("PATH", filepath.Dir(realGit)+":"+os.Getenv("PATH"))
	landed, err := s.Land(ctx, c.ID)
	if err != nil || landed.State != Landed || landed.Head != accepted || landed.PublishedHead != accepted || originHead(t, f, "main") != accepted {
		t.Fatalf("retry did not adopt the exact journaled candidate: %+v %v", landed, err)
	}
}

func TestLandingCanRebaseAgainAfterItsPublicationIsKnown(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, _ := recoveryReviewed(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"README": "first base\n"})
	realGit, _ := exec.LookPath("git")
	bin := filepath.Join(f.dir, "bin")
	script(t, bin, "git", `for arg do
 case "$arg" in *:refs/heads/main) echo 'fixture refuses the default push' >&2; exit 1 ;; esac
done
exec '`+realGit+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("fixture did not refuse the default push")
	}
	journal, _, _ := s.Load(c.ID)
	if journal.State != Landing || journal.LandingHead == "" || journal.PublishedHead != journal.LandingHead {
		t.Fatalf("no known publication: %+v", journal)
	}
	t.Setenv("PATH", filepath.Dir(realGit)+":"+os.Getenv("PATH"))
	pushOther(t, f, "main", map[string]string{"README": "second base\n"})
	landed, err := s.Land(ctx, c.ID)
	if err != nil || landed.State != Landed || landed.Head == journal.LandingHead || originHead(t, f, "main") != landed.Head {
		t.Fatalf("known publication prevented a necessary rebase: %+v %v", landed, err)
	}
}

func TestExecutableTestKeepsItsModeInTheBaseline(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			st := testStore(t)
			files := map[string]string{"Makefile": "test:\n\t./tests/probe\n"}
			if existing {
				files["tests/probe"] = "#!/bin/sh\nexit 0\n"
			}
			f := newRepo(t, files)
			s := recoveryShip(t, st, f)
			c := testingCase(t, st)
			p, err := s.Prepare(context.Background(), c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(p.Work, "tests", "probe")
			write(t, probe, "#!/bin/sh\nexit 0\n")
			os.Chmod(probe, 0755)
			_, err = checkPatch(t, s, context.Background(), c.ID, "make test", []string{"tests/probe"})
			if err == nil || !strings.Contains(err.Error(), "passes without") {
				t.Fatalf("executable bit supplied false discrimination: %v", err)
			}
			var ck Check
			raw, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "check.json"))
			if json.Unmarshal(raw, &ck) != nil || ck.Before.Exit != 0 || ck.After.Exit != 0 {
				t.Fatalf("test modes differed between exports: %s", raw)
			}
		})
	}
}

func TestExecutableExportsKeepTheCandidateModeUnderARestrictiveUmask(t *testing.T) {
	mask := syscall.Umask(0077)
	t.Cleanup(func() { syscall.Umask(mask) })
	st := testStore(t)
	f := newRepo(t, map[string]string{"Makefile": "test:\n\t./tests/probe\n"})
	s := recoveryShip(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(p.Work, "tests", "probe")
	write(t, probe, "#!/bin/sh\ntest \"$(stat -c %a tests/probe)\" = 755\n")
	os.Chmod(probe, 0755)
	_, err = checkPatch(t, s, context.Background(), c.ID, "make test", []string{"tests/probe"})
	if err == nil || !strings.Contains(err.Error(), "passes without") {
		t.Fatalf("mode supplied false discrimination: %v", err)
	}
	var ck Check
	raw, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "check.json"))
	if json.Unmarshal(raw, &ck) != nil || ck.Before.Exit != 0 || ck.After.Exit != 0 {
		t.Fatalf("candidate and baseline modes differ: %s", raw)
	}
}

func TestReviewTextIsCleanedBeforePackageAndCaseLog(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, _, pr := recoveryChecked(t, s, st, f)
	if _, err := proposePatch(t, s, ctx, c.ID, pr); err != nil {
		t.Fatal(err)
	}
	old := Scrubber
	t.Cleanup(func() { Scrubber = old })
	Scrubber = []string{script(t, t.TempDir(), "cleaner", "sed 's/marker-7f3a/[masked]/g'")}
	rev := Review{Head: currentReviewHead(t, s, c.ID), Text: "marker-7f3a", Findings: []Finding{{Severity: "medium", Where: "marker-7f3a", Problem: "marker-7f3a", Fix: "marker-7f3a"}}}
	p, err := s.Reviewed(ctx, c.ID, rev)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
	if strings.Contains(string(raw), "marker-7f3a") || !strings.Contains(string(raw), "[masked]") || p.Reviews[0].Medium != 1 {
		t.Fatalf("unclean review or changed counts: %s", raw)
	}
	cleanCtx := store.WithTextCleaner(ctx, Redact)
	if err := st.CaseNote(cleanCtx, c.ID, "fixture", "marker-7f3a", "test"); err != nil {
		t.Fatal(err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	if log[len(log)-1].Detail != "[masked]" {
		t.Fatalf("unclean case note: %+v", log[len(log)-1])
	}
	Scrubber = []string{filepath.Join(t.TempDir(), "missing")}
	if _, err := s.Reviewed(ctx, c.ID, rev); err == nil {
		t.Fatal("failed cleaner accepted a review")
	}
	saved, _, _ := s.Load(c.ID)
	if len(saved.Reviews) != 1 {
		t.Fatal("failed cleaner persisted a review")
	}
	if err := st.CaseNote(cleanCtx, c.ID, "fixture", "marker-7f3a", "test"); err == nil {
		t.Fatal("failed cleaner accepted a case note")
	}
	after, _ := st.CaseLog(ctx, c.ID)
	if len(after) != len(log) {
		t.Fatal("failed cleaner persisted a case note")
	}
}
