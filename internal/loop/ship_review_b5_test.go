package loop

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Adapted from the second review's synthetic probes. All remotes are local.
func TestPublicPatchIsScannedBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name, path, text string
		private          bool
		refused          bool
	}{
		{"review-probe", "fake.go", "// seen at /home/alice/work/x on build-box-7.internal.example by alice@example.com\n", false, true},
		{"home", "fake.go", "// /Users/alice/work\n", false, true},
		{"ipv4", "fake.go", "// 192.0.2.7\n", false, true},
		{"ipv6", "fake.go", "// 2001:db8::7\n", false, true},
		{"secret", "fake.go", "// password=marker-7f3a\n", false, true},
		{"filename", "alice@example.com.txt", "marker-7f3a\n", false, true},
		{"private", "fake.go", "// /home/alice/work alice@example.com\n", true, false},
		{"source-name", "main.go", "// marker-7f3a\n", false, false},
		{"binary-home", "notes.bin", "\x00marker-7f3a /home/someone/notes.txt\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, c, p, pr := b4Checked(t)
			write(t, filepath.Join(p.Work, tc.path), fixed("42")+tc.text)
			tree, paths, err := s.applyPatch(context.Background(), p, []Patch{fixturePatch(t, s, c.ID)})
			if err != nil {
				t.Fatal(err)
			}
			p.Check.Tree, p.Changed = tree, paths
			if err := s.save(&p); err != nil {
				t.Fatal(err)
			}
			private := "false"
			if tc.private {
				private = "true"
			}
			s.GH = b4GH(t, f, private, "OPEN")
			s.Public = nil
			_, err = proposePatch(t, s, context.Background(), c.ID, pr)
			if !tc.refused {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "patch") {
				t.Fatalf("personal patch was not refused with its cause: %v", err)
			}
			branches := run(t, f.main, "git", "--git-dir", f.origin, "branch", "--list", p.Branch)
			if strings.TrimSpace(branches) != "" {
				t.Fatal("personal patch reached the remote branch")
			}
		})
	}
}

func TestRollbackRebuildsItsJournalOnAMovedBase(t *testing.T) {
	fakeManager(t)
	s, f, c, _ := b4Reviewed(t)
	ctx := context.Background()
	if _, err := s.Land(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Gate.Busy = func(context.Context, string) (string, error) {
		calls++
		if calls == 2 {
			return "marker-7f3a busy", nil
		}
		return "", nil
	}
	p, err := s.RollBack(ctx, c.ID, "marker-7f3a")
	if !errors.Is(err, ErrRefused) || p.State != RollingBack || p.Undo == nil {
		t.Fatalf("fixture did not journal a refused revert: %s %v", p.State, err)
	}
	original := *p.Undo
	s.Gate.Busy = nil
	pushOther(t, f, "main", map[string]string{"notes.txt": "marker-7f3a"})
	moved := originHead(t, f, "main")
	p, err = s.RollBack(ctx, c.ID, "a retry keeps the original reason")
	if err != nil || p.State != RolledBack || p.Undo.Head == original.Head || p.Undo.Why != original.Why {
		t.Fatalf("rollback did not redo its journal: %s %v", p.State, err)
	}
	if _, err := s.git(ctx, p.Host, "merge-base", "--is-ancestor", moved, p.Undo.Head); err != nil {
		t.Fatal("rollback discarded the moved base", err)
	}
	if got := run(t, f.main, "git", "--git-dir", f.origin, "show", "main:notes.txt"); got != "marker-7f3a" {
		t.Fatal("rollback discarded the other change")
	}
	for i := 0; i < 2; i++ {
		if _, err := s.RollBack(ctx, c.ID, "marker-7f3a"); err != nil {
			t.Fatal(err)
		}
	}
	if originHead(t, f, "main") != p.Undo.Head {
		t.Fatal("a completed rollback pushed another revert")
	}
}

func TestRefusalNoteDedupesItsStoredForm(t *testing.T) {
	s, _, c, _ := b4Reviewed(t)
	r := &Runner{Ship: s, Store: s.Store}
	for _, detail := range []string{
		"git push: exit status 1\n ! [rejected] main (non-fast-forward)\nhint: updates rejected",
		strings.Repeat("marker-7f3a\n", 300),
	} {
		for i := 0; i < 3; i++ {
			if err := r.caseNoteOnce(context.Background(), c.ID, "not landed", detail); err != nil {
				t.Fatal(err)
			}
		}
	}
	log, err := s.Store.CaseLog(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range log {
		if e.Action == "not landed" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("two distinct refusals wrote %d notes", n)
	}
}

func TestClosedPRReleasesTheLandingJournal(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "resume"}[resume], func(t *testing.T) {
			fakeManager(t)
			s, f, c, p := b4Reviewed(t)
			ctx := context.Background()
			if resume {
				p.State, p.LandingHead, p.Landed = Landing, p.Head, []string{p.Head}
				if err := s.save(&p); err != nil {
					t.Fatal(err)
				}
			}
			s.GH = b4GH(t, f, "true", "CLOSED")
			before := originHead(t, f, "main")
			if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
				t.Fatal("closed pull request landed", err)
			}
			if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
				t.Fatal("closed pull request resumed a landing", err)
			}
			got, _, err := s.Load(c.ID)
			if err != nil || got.State != Checked || got.PR != "" || got.LandingHead != "" || len(got.Landed) != 0 {
				t.Fatalf("closed PR retained its landing: %s %v", got.State, err)
			}
			log, err := s.Store.CaseLog(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, e := range log {
				if e.Action == "proposal closed" {
					n++
				}
			}
			if n != 1 || originHead(t, f, "main") != before {
				t.Fatal("closed PR lacks one note or changed the base")
			}
			s.GH = script(t, f.dir, "gh", `case "$1 $2" in
"repo view") echo '{"isPrivate":true}';;
"pr list") echo '[{"number":7,"url":"https://example.com/pull/7","headRefOid":"`+p.Head+`","state":"CLOSED"}]';;
"pr create") echo 'https://example.com/pull/8';;
esac`)
			pr := Proposal{Title: "Answer is 42", Body: "One cause.", Rollback: "Revert.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
			next, err := proposePatch(t, s, ctx, c.ID, pr)
			if err != nil {
				t.Fatal("closed proposal cannot be proposed again", err)
			}
			if next.PR != "https://example.com/pull/8" {
				t.Fatal("proposal adopted the closed request again")
			}
		})
	}
}

func TestPublicKeepsSourceLocations(t *testing.T) {
	text := "**H1.** `internal/loop/ship.go:120`: reads main.go, notes.txt (e.g. config-copy.json) in go1.22.3"
	got, err := Public(context.Background(), text)
	if err != nil || got != text {
		t.Fatalf("source locations were masked: %q %v", got, err)
	}
}

func TestReviewPostingTakesTheLockAndReloads(t *testing.T) {
	s, f, c, p := b4Reviewed(t)
	s.GH = b4GH(t, f, "true", "OPEN")
	p.Reviews[0].Comment = ""
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Ship: s, Store: s.Store}
	posts := 0
	r.Post = func(context.Context, string, string, string) (string, error) {
		posts++
		return "https://example.com/review/1", nil
	}
	unlock, err := s.lock(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.postReview(context.Background(), p)
	unlock()
	if !errors.Is(err, ErrRefused) || posts != 0 {
		t.Fatal("posted while another step held the lock", err)
	}
	if _, err := r.postReview(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.postReview(context.Background(), p); err != nil || posts != 1 {
		t.Fatal("stale package posted a second review", err)
	}
}

func TestBuildPreparationTakesTheCaseLock(t *testing.T) {
	s, f, c, p, _ := b4Checked(t)
	setCaseRepo(t, s.Store, c, "fixture")
	r := &Runner{Ship: s, Store: s.Store, pass: &passInputs{manifest: InputsManifest{
		Repositories: []RepoClone{{Name: "fixture", Source: f.main}},
	}}}
	before, err := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = r.buildPackage(context.Background(), c, store.LoopRun{Dir: filepath.Join(f.dir, "new-run")})
	after, readErr := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
	if !errors.Is(err, ErrRefused) || readErr != nil || string(before) != string(after) {
		t.Fatal("build changed the package without its lock", err, readErr)
	}
	if _, err := os.Stat(p.Work); err != nil {
		t.Fatal("locked build moved its worktree", err)
	}
}

func TestPublicCommitMessageUsesHostText(t *testing.T) {
	s, f, c, _, pr := b4Checked(t)
	s.GH = b4GH(t, f, "false", "OPEN")
	pr.Title, pr.Body = "marker-7f3a title", "marker-7f3a body"
	p, err := proposePatch(t, s, context.Background(), c.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	message := run(t, f.main, "git", "--git-dir", f.origin, "show", "-s", "--format=%B", p.Head)
	if strings.Contains(message, "marker-7f3a") || !strings.Contains(message, p.Check.Tree) ||
		!strings.Contains(message, "Change for case "+p.Case) {
		t.Fatal("public commit contains builder prose")
	}
}

func TestCleanupResumeRequiresLive(t *testing.T) {
	s, f, c, p := b4Reviewed(t)
	p.State, p.LandingHead, p.Cleanup = Landed, p.Head, "pending"
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.Live = false
	if _, err := s.retryCleanup(context.Background(), c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "live") {
		t.Fatal("cleanup resume ignored live", err)
	}
	if originHead(t, f, p.Branch) != p.Head {
		t.Fatal("cleanup deleted the proposal")
	}
}

func TestUnreadableDeployedPackageDoesNotStopSelection(t *testing.T) {
	for _, status := range []store.CaseStatus{store.CaseDeployed, store.CaseMeasuring} {
		t.Run(string(status), func(t *testing.T) {
			s, _, c, _ := b4Reviewed(t)
			ctx := context.Background()
			if _, err := s.Store.DeployCase(ctx, c.ID, s.pkgDir(c.ID), time.Now(), "fixture", "fixture"); err != nil {
				t.Fatal(err)
			}
			if status == store.CaseMeasuring {
				db, err := sql.Open("sqlite", s.Store.Path())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec("UPDATE cases SET status='measuring' WHERE id=?", c.ID); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(s.pkgDir(c.ID), "package.json"), "{")
			other := openCase(t, s.Store, c.Family, "another", 1)
			r := &Runner{Ship: s, Store: s.Store, Budgets: DefaultBudgets}
			got, _, _, err := r.pick(ctx, c.Family, "", false)
			if err != nil || got.ID != other.ID || len(r.pickedSkips) != 1 || r.pickedSkips[0].Step != "package" {
				t.Fatalf("bad deployed package stopped selection: %s %v", got.ID, err)
			}
		})
	}
}
