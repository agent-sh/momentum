package loop

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// These fixtures test host decisions with synthetic receipts. No compiler, model,
// network or real sandbox starts. The sandbox integration fixtures remain separate.
func b4Checked(t *testing.T) (*Ship, repoFixture, store.Case, Package, Proposal) {
	return b4CheckedFor(t, "agent-sh/fake")
}

func b4CheckedFor(t *testing.T, repo string) (*Ship, repoFixture, store.Case, Package, Proposal) {
	t.Helper()
	st := testStore(t)
	files := map[string]string{}
	if repo == SelfRepo {
		files["internal/supervise/rules.go"] = "package supervise\n"
		files["internal/capacity/rules.go"] = "package capacity\n"
	}
	f := newRepo(t, files)
	s := shipWithSandbox(st, f, Sandbox{})
	s.Gate.Resolve = func(context.Context, string) (string, error) { return repo, nil }
	c := testingCase(t, st)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	if repo == SelfRepo {
		write(t, filepath.Join(p.Work, "internal/supervise/rules.go"), "package supervise\n// One rule.\n")
		write(t, filepath.Join(p.Work, "internal/capacity/rules.go"), "package capacity\n// One discriminating check.\n")
	} else {
		write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
		write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	}
	tree, changed, err := s.applyPatch(context.Background(), p, []Patch{fixturePatch(t, s, c.ID)})
	if err != nil {
		t.Fatal(err)
	}
	p.State, p.Changed = Checked, changed
	p.Check = &Check{Tree: tree, Test: []string{"python3", "-c", "pass"}, Before: Outcome{Exit: 1}, After: Outcome{Exit: 0}}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	return s, f, c, p, Proposal{Title: "Answer is 42", Body: "One cause.", Rollback: "Revert.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
}

func b4Reviewed(t *testing.T) (*Ship, repoFixture, store.Case, Package) {
	t.Helper()
	s, f, c, _, pr := b4Checked(t)
	if _, err := proposePatch(t, s, context.Background(), c.ID, pr); err != nil {
		t.Fatal(err)
	}
	p, err := s.Reviewed(context.Background(), c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "Checked every changed file.", Comment: "https://example.com/review/1"})
	if err != nil {
		t.Fatal(err)
	}
	return s, f, c, p
}

func b4GH(t *testing.T, f repoFixture, private, state string) string {
	t.Helper()
	return script(t, f.dir, "gh", `echo "$@" >> '`+filepath.Join(f.dir, "gh-calls")+`'
case "$1 $2" in
 "repo view") echo '{"isPrivate":`+private+`}';;
 "pr list") echo '[]';;
 "pr view") echo '{"state":"`+state+`"}';;
 "pr create") echo 'https://example.com/pull/7';;
esac`)
}

func TestPublicWithholdsPersonalContent(t *testing.T) {
	text := "marker-7f3a /home/alice/work/x /Users/alice/x ~/work/x alice@example.com build-box-7.internal.example 192.0.2.7 2001:db8::7"
	got, err := Public(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range strings.Fields(text)[1:] {
		if strings.Contains(got, sensitive) {
			t.Errorf("personal content survived: %s", sensitive)
		}
	}
	if !strings.Contains(got, "marker-7f3a") {
		t.Fatal("lost harmless text")
	}
}

func TestPublicProposalUsesOnlyHostText(t *testing.T) {
	s, f, c, _, pr := b4Checked(t)
	s.GH = b4GH(t, f, "false", "OPEN")
	s.Public = nil
	mechanism := "marker-7f3a /home/alice/work/x build-box-7.internal.example alice@example.com"
	if _, err := s.Store.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Mechanism: &mechanism}, "fixture"); err != nil {
		t.Fatal(err)
	}
	pr.Title, pr.Body, pr.Rollback = "marker-7f3a title", "marker-7f3a body", "marker-7f3a rollback"
	if _, err := proposePatch(t, s, context.Background(), c.ID, pr); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "marker-7f3a") || !strings.Contains(string(calls), "repo view") ||
		!strings.Contains(string(calls), "isPrivate") || !strings.Contains(string(calls), "Base:") {
		t.Fatalf("public proposal lacks host-only visibility boundary: %s", calls)
	}
}

func TestVisibilityMustBeKnownBeforePublishingText(t *testing.T) {
	for _, reply := range []string{`{}`, `{"isPrivate":null}`, `not json`} {
		t.Run(reply, func(t *testing.T) {
			s, f, c, _, pr := b4Checked(t)
			s.GH = script(t, f.dir, "gh", `case "$1 $2" in
"repo view") printf '%s\n' '`+reply+`';;
"pr list") echo '[]';;
*) echo called >> '`+filepath.Join(f.dir, "posted")+`';;
esac`)
			if _, err := proposePatch(t, s, context.Background(), c.ID, pr); err == nil {
				t.Fatal("unknown visibility authorized public text")
			}
			if _, err := os.Stat(filepath.Join(f.dir, "posted")); !os.IsNotExist(err) {
				t.Fatal("posted without visibility")
			}
		})
	}
}

func TestPublicReviewUsesOnlyHostText(t *testing.T) {
	s, f, c, p := b4Reviewed(t)
	s.GH = b4GH(t, f, "false", "OPEN")
	p.Reviews[0].Comment, p.Reviews[0].Text = "", "marker-7f3a /home/alice/x alice@example.com"
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Ship: s, Store: s.Store}
	r.Post = func(_ context.Context, _, _, body string) (string, error) {
		if strings.Contains(body, "marker-7f3a") || !strings.Contains(body, p.Head) {
			t.Fatalf("run text posted: %s", body)
		}
		return "https://example.com/review/1", nil
	}
	if _, err := r.postReview(context.Background(), p); err != nil {
		t.Fatal(c.ID, err)
	}
}

func TestNotLiveDoesNotRepeatLandingNotes(t *testing.T) {
	s, _, c, _ := b4Reviewed(t)
	s.Live = false
	r := &Runner{Ship: s, Store: s.Store, Budgets: DefaultBudgets, Dir: s.Dir}
	for i := 0; i < 3; i++ {
		done := r.reviews(context.Background(), "", c.Family)
		if len(done) != 1 || !strings.Contains(done[0].Skipped, "live") {
			t.Fatalf("missing skipped landing: %+v", done)
		}
	}
	log, err := s.Store.CaseLog(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range log {
		if entry.Action == "not landed" {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("repeated one cause %d times", n)
	}
}

func TestReviewedOwnsTheLockAndRoundBoundary(t *testing.T) {
	for _, boundary := range []string{"lock", "same head", "round cap", "empty text"} {
		t.Run(boundary, func(t *testing.T) {
			s, _, c, p := b4Reviewed(t)
			switch boundary {
			case "lock":
				p.Reviews = nil
				unlock, err := s.lock(context.Background(), c.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "round cap":
				p.Reviews = []Review{{Head: "a"}, {Head: "b"}, {Head: "c"}}
			case "empty text":
				p.Reviews = nil
			}
			if err := s.save(&p); err != nil {
				t.Fatal(err)
			}
			rev := Review{Head: currentReviewHead(t, s, c.ID), Text: "Checked every changed file.", Comment: "posted"}
			if boundary == "empty text" {
				rev.Text = " \n "
			}
			if _, err := s.Reviewed(context.Background(), c.ID, rev); !errors.Is(err, ErrRefused) {
				t.Fatalf("review crossed %s: %v", boundary, err)
			}
		})
	}
}

func TestOneUnreadablePackageDoesNotStopThePass(t *testing.T) {
	fakeManager(t)
	r, c := inputRunner(t)
	r.Ship = &Ship{Store: r.Store, Dir: r.Dir}
	mech, next := "marker-7f3a", "one test"
	if _, err := r.Store.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "fixture"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Ship.pkgDir(c.ID), "package.json"), "{")
	other := openCase(t, r.Store, "owner-burden", "other", 1)
	Scrubber = []string{"cat"} // refuse inputs at their canary, without starting a model
	for i := 0; i < 2; i++ {
		done, err := r.Once(context.Background(), Options{Families: []string{c.Family, other.Family}})
		if err != nil || len(done) < 2 {
			t.Fatalf("one broken package stopped other families: %+v %v", done, err)
		}
	}
	notes, err := r.Store.Notes(context.Background())
	if err != nil || len(notes) != 1 || notes[0].Kind != store.NoteProblem {
		t.Fatalf("unreadable package needs one passive note: %d %v", len(notes), err)
	}
}

func TestFailedDeploymentDoesNotStopSelection(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.State, p.LandedAt = Landed, time.Now()
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", s.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_deploy BEFORE UPDATE OF status ON cases WHEN NEW.status='deployed' BEGIN SELECT RAISE(ABORT, 'marker-7f3a'); END`); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Ship: s, Store: s.Store, Budgets: DefaultBudgets}
	if _, _, why, err := r.pick(context.Background(), c.Family, "", false); err != nil || !strings.Contains(why, "marker-7f3a") {
		t.Fatalf("failed deployment stopped selection: %s %v", why, err)
	}
}

func TestResumedRollbackNeedsLiveAndDoesNotStopSelection(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.State = RollingBack
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DeployCase(context.Background(), c.ID, s.pkgDir(c.ID), time.Now(), "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	s.Live = false
	r := &Runner{Ship: s, Store: s.Store, Budgets: DefaultBudgets}
	if _, _, why, err := r.pick(context.Background(), c.Family, "", false); err != nil || !strings.Contains(why, "live") {
		t.Fatalf("resumed rollback ignored live or aborted pass: %s %v", why, err)
	}
	s.Live = true
	s.Gate.Busy = func(context.Context, string) (string, error) { return "marker-7f3a busy", nil }
	if _, _, why, err := r.pick(context.Background(), c.Family, "", false); err != nil || !strings.Contains(why, "busy") {
		t.Fatalf("busy rollback aborted pass: %s %v", why, err)
	}
}

func TestCleanupPushIsGatedAndRetried(t *testing.T) {
	fakeManager(t) // synthetic successful check result; host lifecycle only
	s, f, c, p := b4Reviewed(t)
	before := originHead(t, f, "main")
	s.Gate.Busy = func(context.Context, string) (string, error) {
		if originHead(t, f, "main") != before {
			return "marker-7f3a busy after landing", nil
		}
		return "", nil
	}
	landed, err := s.Land(context.Background(), c.ID)
	if !errors.Is(err, ErrRefused) || landed.State != Landed || landed.Cleanup == "" {
		t.Fatalf("cleanup not pending after refusal: %s %v", landed.State, err)
	}
	out, err := exec.Command("git", "--git-dir", f.origin, "branch", "--list", p.Branch).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatal("cleanup crossed busy gate")
	}
	s.Gate.Busy = nil
	r := &Runner{Ship: s, Store: s.Store, Budgets: DefaultBudgets}
	if _, _, _, err := r.pick(context.Background(), c.Family, "", false); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Load(c.ID)
	out, branchErr := exec.Command("git", "--git-dir", f.origin, "branch", "--list", p.Branch).Output()
	if err != nil || branchErr != nil || got.Cleanup != "" || strings.TrimSpace(string(out)) != "" {
		t.Fatal("next selection did not finish pending cleanup")
	}
}

func TestHostCloneDestinationChangedIsRefused(t *testing.T) {
	s, f, _, p := b4Reviewed(t)
	other := filepath.Join(f.dir, "other.git")
	run(t, f.dir, "git", "clone", "--bare", f.origin, other)
	run(t, p.Host, "git", "remote", "set-url", "origin", other)
	if _, err := s.push(context.Background(), p, p.Host, "--no-verify", "origin", "HEAD:refs/heads/"+p.Branch); !errors.Is(err, ErrRefused) {
		t.Fatalf("changed host destination accepted: %v", err)
	}
}

func TestClosedPullRequestCannotLand(t *testing.T) {
	fakeManager(t)
	s, f, c, p := b4Reviewed(t)
	s.GH = b4GH(t, f, "true", "CLOSED")
	before := originHead(t, f, "main")
	if _, err := s.Land(context.Background(), c.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("closed pull request landed: %v", err)
	}
	if originHead(t, f, "main") != before || originHead(t, f, p.Branch) != p.Head {
		t.Fatal("closed pull request changed a remote")
	}
}

func TestLandingResumeStillNeedsLive(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.State, p.LandingHead = Landing, p.Head
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.Live = false
	if _, err := s.Land(context.Background(), c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "live") {
		t.Fatalf("landing resume ignored live: %v", err)
	}
}

func TestLandingLostResponseThenBaseMovesKeepsCommits(t *testing.T) {
	fakeManager(t)
	s, f, c, _ := b4Reviewed(t)
	pushOther(t, f, "main", map[string]string{"README": "marker-7f3a before\n"})
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, count := filepath.Join(f.dir, "bin"), filepath.Join(f.dir, "pushes-seen")
	script(t, bin, "git", `push=no
for arg do if [ "$arg" = push ]; then push=yes; fi; done
'`+realGit+`' "$@"
code=$?
if [ "$code" = 0 ] && [ "$push" = yes ]; then
 echo x >> '`+count+`'
 if [ "$(wc -l < '`+count+`')" = 2 ]; then echo 'marker-7f3a lost response' >&2; exit 1; fi
fi
exit "$code"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := s.Land(context.Background(), c.ID); err == nil {
		t.Fatal("fixture did not lose response")
	}
	pushOther(t, f, "main", map[string]string{"README": "marker-7f3a after\n"})
	p, err := s.Land(context.Background(), c.ID)
	if err != nil || p.State != Landed || len(p.Landed) == 0 {
		t.Fatalf("resume lost landing receipt: %s commits=%d %v", p.State, len(p.Landed), err)
	}
	if n := strings.Count(run(t, f.main, "git", "--git-dir", f.origin, "log", "--format=%s", "main"), "Answer is 42"); n != 1 {
		t.Fatalf("landed %d times", n)
	}
}

func TestInterruptedDeploymentKeepsCleanupIntent(t *testing.T) {
	fakeManager(t)
	s, f, c, p := b4Reviewed(t)
	db, err := sql.Open("sqlite", s.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_deploy BEFORE UPDATE OF status ON cases WHEN NEW.status='deployed' BEGIN SELECT RAISE(ABORT, 'marker-7f3a'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Land(context.Background(), c.ID); err == nil {
		t.Fatal("deployment did not fail")
	}
	landed, _, err := s.Load(c.ID)
	if err != nil || landed.State != Landed || landed.Cleanup == "" {
		t.Fatal("interrupted deployment lost cleanup intent", err)
	}
	if _, err := db.Exec("DROP TRIGGER fail_deploy"); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Ship: s, Store: s.Store, Budgets: DefaultBudgets}
	if _, _, _, err := r.pick(context.Background(), c.Family, "", false); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "--git-dir", f.origin, "branch", "--list", p.Branch).Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatal("resumed deployment left its proposal branch")
	}
}

func TestBusyRollbackDoesNotStopOtherFamilies(t *testing.T) {
	fakeManager(t)
	s, _, c, p := b4Reviewed(t)
	p.State = RollingBack
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DeployCase(context.Background(), c.ID, s.pkgDir(c.ID), time.Now(), "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	s.Gate.Busy = func(context.Context, string) (string, error) { return "marker-7f3a busy", nil }
	other := openCase(t, s.Store, "owner-burden", "other", 1)
	r := localRunner(t, s.Store)
	r.Ship = s
	done, err := r.Once(context.Background(), Options{Families: []string{c.Family, other.Family}})
	if err != nil {
		t.Fatal("one refused rollback stopped the pass", err)
	}
	haveRollback, haveOther := false, false
	for _, step := range done {
		haveRollback = haveRollback || (step.Case == c.ID && step.Step == "rollback" && strings.Contains(step.Skipped, "busy"))
		haveOther = haveOther || step.Case == other.ID
	}
	if !haveRollback || !haveOther {
		t.Fatal("lost the case's skip or the other family's step")
	}
}
