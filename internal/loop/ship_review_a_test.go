package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func recoveryShip(t *testing.T, st *store.Store, f repoFixture) *Ship {
	t.Helper()
	return shipFor(t, st, f)
}

func recoveryChecked(t *testing.T, s *Ship, st *store.Store, f repoFixture) (store.Case, Package, Proposal) {
	t.Helper()
	ctx := context.Background()
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	// This fixture checks the same change without invoking a compiler.
	test := `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`
	p, err = checkPatch(t, s, ctx, c.ID, test, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	return c, p, pr
}

func recoveryReviewed(t *testing.T, s *Ship, st *store.Store, f repoFixture) (store.Case, Package) {
	t.Helper()
	c, _, pr := recoveryChecked(t, s, st, f)
	if _, err := proposePatch(t, s, context.Background(), c.ID, pr); err != nil {
		t.Fatal(err)
	}
	p, err := s.Reviewed(context.Background(), c.ID, Review{Comment: "https://example.invalid/review/1"})
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

func TestShipPushesPublishNoReachableTags(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	run(t, f.main, "git", "config", "push.followTags", "true")
	run(t, f.main, "git", "tag", "-a", "proposal-tag", "-m", "fixture")
	c, prepared, pr := recoveryChecked(t, s, st, f)
	run(t, prepared.Host, "git", "config", "push.followTags", "true")
	checkTags := func(stage string) {
		t.Helper()
		if got := run(t, f.dir, "git", "--git-dir", f.origin, "for-each-ref", "refs/tags"); got != "" {
			t.Fatalf("%s published a tag: %s", stage, got)
		}
	}
	if _, err := proposePatch(t, s, ctx, c.ID, pr); err != nil {
		t.Fatal(err)
	}
	checkTags("proposal")
	p, err := s.Reviewed(ctx, c.ID, Review{Comment: "https://example.invalid/review/1"})
	if err != nil {
		t.Fatal(err)
	}
	run(t, p.Host, "git", "tag", "-a", "landing-tag", "-m", "fixture", "origin/main")
	// Advance the base, so landing pushes both the rebased proposal branch and the base.
	pushOther(t, f, "main", map[string]string{"README": "fixture\n"})
	if p, err = s.Land(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	checkTags("landing")
	run(t, p.Host, "git", "tag", "-a", "rollback-tag", "-m", "fixture")
	if _, err := s.RollBack(ctx, c.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	checkTags("rollback")
}

func TestProposalAdoptsACreatedPullRequestAfterALostResponse(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, _, pr := recoveryChecked(t, s, st, f)
	receipt, calls := filepath.Join(f.dir, "receipt"), filepath.Join(f.dir, "creates")
	s.GH = script(t, f.dir, "lost-gh", `case "$2" in
 list)
  if [ -f '`+receipt+`' ]; then cat '`+receipt+`'; else echo '[]'; fi ;;
 create)
  echo create >> '`+calls+`'
  head=$(git rev-parse HEAD)
  printf '[{"number":7,"url":"https://example.invalid/pull/7","headRefOid":"%s"}]\n' "$head" > '`+receipt+`'
  echo 'fixture lost response' >&2
  exit 1 ;;
esac`)
	if p, err := proposePatch(t, s, ctx, c.ID, pr); err == nil || p.Head == "" || p.PR != "" || p.State != Checked {
		t.Fatalf("fixture did not lose the receipt: %+v %v", p, err)
	}
	p, err := proposePatch(t, s, ctx, c.ID, pr)
	if err != nil || p.State != Proposed || p.PR != "https://example.invalid/pull/7" {
		t.Fatalf("created pull request was not adopted: %+v %v", p, err)
	}
	if raw, _ := os.ReadFile(calls); string(raw) != "create\n" {
		t.Fatalf("creation repeated: %q", raw)
	}
	saved, _, err := s.Load(c.ID)
	if err != nil || saved.PR != p.PR {
		t.Fatalf("adopted receipt not kept: %+v %v", saved, err)
	}
}

func TestProposalDoesNotAdoptAHistoricalPullRequestAtAnotherHead(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, _, pr := recoveryChecked(t, s, st, f)
	calls := filepath.Join(f.dir, "creates")
	s.GH = script(t, f.dir, "mismatched-gh", `if [ "$2" = list ]; then
 echo '[{"number":7,"url":"https://example.invalid/pull/7","headRefOid":"different"}]'
else echo create > '`+calls+`'; echo 'https://example.invalid/pull/8'; fi`)
	p, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil || p.PR != "https://example.invalid/pull/8" {
		t.Fatalf("a historical head was adopted or blocked creation: %+v %v", p, err)
	}
	if b, err := os.ReadFile(calls); err != nil || string(b) != "create\n" {
		t.Fatalf("a new commit did not create its own proposal: %q %v", b, err)
	}
}

func TestRollbackResumesAfterRemoteAcceptance(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, p := recoveryReviewed(t, s, st, f)
	landed, err := s.Land(ctx, c.ID)
	if err != nil || landed.State != Landed {
		t.Fatalf("land: %+v %v", p, err)
	}
	p = landed
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(f.dir, "bin")
	once := filepath.Join(f.dir, "lost-push")
	write(t, once, "")
	script(t, bin, "git", `push=no
for arg do if [ "$arg" = push ]; then push=yes; fi; done
'`+realGit+`' "$@"
code=$?
if [ "$code" = 0 ] && [ "$push" = yes ] && [ -f '`+once+`' ]; then
 rm '`+once+`'
 echo 'fixture lost push response' >&2
 exit 1
fi
exit "$code"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := s.RollBack(ctx, c.ID, "original reason"); err == nil {
		t.Fatal("fixture did not lose the push response")
	}
	accepted := originHead(t, f, "main")
	saved, _, err := s.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(saved)
	var journal struct {
		Undo struct {
			Head    string   `json:"head"`
			Commits []string `json:"commits"`
		} `json:"undo"`
	}
	json.Unmarshal(raw, &journal)
	if saved.State != "rolling back" || journal.Undo.Head != accepted || len(journal.Undo.Commits) != len(p.Landed) {
		t.Fatalf("no durable rollback candidate: %s", raw)
	}
	// Another ordinary push after acceptance must not hide the accepted revert.
	pushOther(t, f, "main", map[string]string{"README": "after the revert\n"})
	advanced := originHead(t, f, "main")
	if p, err := s.RollBack(ctx, c.ID, "retry reason"); err != nil || p.State != RolledBack {
		t.Fatalf("rollback did not resume: %+v %v", p, err)
	}
	if originHead(t, f, "main") != advanced {
		t.Fatal("retry reverted twice or pushed again")
	}
	got, err := st.CaseByID(ctx, c.ID)
	if err != nil || got.Status != store.CaseTesting {
		t.Fatalf("case receipt not finished: %+v %v", got, err)
	}
	if _, err := s.RollBack(ctx, c.ID, "again"); err != nil || originHead(t, f, "main") != advanced {
		t.Fatalf("receipt retry is not idempotent: %v", err)
	}
}

func TestRollbackSelectionFinishesASavedPackageReceipt(t *testing.T) {
	for _, measuring := range []bool{false, true} {
		t.Run(map[bool]string{false: "deployed", true: "measuring"}[measuring], func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "store.db")
			st, err := store.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			f := newRepo(t, nil)
			s := recoveryShip(t, st, f)
			ctx := context.Background()
			c, _ := recoveryReviewed(t, s, st, f)
			if _, err := s.Land(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if measuring {
				if _, err := db.Exec(`UPDATE cases SET status = 'measuring' WHERE id = ?`, c.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`CREATE TRIGGER fail_rollback BEFORE UPDATE OF status ON cases WHEN NEW.status='testing' BEGIN SELECT RAISE(ABORT, 'fixture interruption'); END`); err != nil {
				t.Fatal(err)
			}
			p, err := s.RollBack(ctx, c.ID, "original reason")
			if err == nil || p.State != RolledBack {
				t.Fatalf("fixture did not fail after saving the package: %+v %v", p, err)
			}
			if _, err := db.Exec(`DROP TRIGGER fail_rollback`); err != nil {
				t.Fatal(err)
			}
			head := originHead(t, f, "main")
			r := Runner{Store: st, Ship: s}
			if chosen, _, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || chosen.ID != "" {
				t.Fatalf("dry pick: %s %v", chosen.ID, err)
			}
			got, _ := st.CaseByID(ctx, c.ID)
			if got.Status == store.CaseTesting {
				t.Fatal("dry pick wrote the rollback receipt")
			}
			if chosen, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || chosen.ID != "" {
				t.Fatalf("reconciliation pick: %s %v", chosen.ID, err)
			}
			got, _ = st.CaseByID(ctx, c.ID)
			if got.Status != store.CaseTesting || originHead(t, f, "main") != head {
				t.Fatalf("receipt did not finish without another push: %+v", got)
			}
			if _, err := s.RollBack(ctx, c.ID, "again"); err != nil {
				t.Fatal(err)
			}
			log, _ := st.CaseLog(ctx, c.ID)
			count := 0
			for _, entry := range log {
				if entry.Action == "rolled back" {
					count++
					if entry.Detail != "original reason" {
						t.Fatalf("lost rollback reason: %s", entry.Detail)
					}
				}
			}
			if count != 1 {
				t.Fatalf("rollback receipt repeated %d times", count)
			}
		})
	}
}

func TestShipSandboxHasExplicitGoEnvironmentNames(t *testing.T) {
	env := testEnvironment()
	for _, name := range []string{"GOFLAGS", "GOTOOLCHAIN", "GOCACHE", "GOPROXY", "GOSUMDB"} {
		if _, ok := env[name]; !ok {
			t.Fatalf("missing environment name: %s", name)
		}
	}
}

func TestShipSandboxBuildsAnExportedGoCommand(t *testing.T) {
	sb := testSandbox(t)
	st := testStore(t)
	f := newRepo(t, map[string]string{"cmd/probe/main.go": "package main\nfunc main() {}\n"})
	s := shipWithSandbox(st, f, sb)
	c := testingCase(t, st)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := s.export(context.Background(), p, "before", "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	defer s.unexport(p, dir)
	o, err := sb.Run(context.Background(), dir, []string{"go", "build", "./cmd/probe"}, nil)
	if err != nil || o.Exit != 0 {
		t.Fatalf("exported Go command: %+v %v", o, err)
	}
}
