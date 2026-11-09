package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Only a request a person closed counts as closed for the owner's withdrawal: a hand landing's merged request is its
// landing, which the next pass records, so the withdrawal refuses it; another package's merged request is withdrawn
// with a line that says the loop does not measure it; a read that fails is the withdrawal's error.
func TestTheOwnersWithdrawalReadsTheRequestsState(t *testing.T) {
	ctx := context.Background()
	t.Run("hand landing merged", func(t *testing.T) {
		s, c, _, p := proposedHand(t)
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED"}'`)
		if _, err := s.Withdraw(ctx, c.ID, "its fix moved"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "the next pass records") {
			t.Fatalf("the withdrawal over a merged hand landing: %v", err)
		}
		if got := loadPackage(t, s, c.ID); got.State != p.State || got.PR != p.PR {
			t.Fatalf("the merged hand landing changed: %+v", got)
		}
	})
	t.Run("hand landing closed", func(t *testing.T) {
		s, c, _, p := proposedHand(t)
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED"}'`)
		got, err := s.Withdraw(ctx, c.ID, "its fix moved")
		if err != nil || got.State != Withdrawn || got.PR != "" || got.ClosedProposals != p.ClosedProposals+1 || got.Withdrew != p.PR {
			t.Fatalf("the withdrawal of a closed hand landing: %+v %v", got, err)
		}
		cur, _ := s.Store.CaseByID(ctx, c.ID)
		log, _ := s.Store.CaseLog(ctx, c.ID)
		if store.HandOf(cur, log) != "" || !logHas(log, store.ActionHandEnded, p.PR) {
			t.Fatalf("the hand landing's record stands: %q", store.HandOf(cur, log))
		}
		if n, ok := handNote(t, s.Store, c.ID); ok && n.Done.IsZero() {
			t.Fatalf("the hand landing's note stands: %+v", n)
		}
	})
	t.Run("merged by a person", func(t *testing.T) {
		w := newLifeWorld(t)
		for _, e := range []string{"owner prepare", "check", "propose", "a person merges the request"} {
			w.step(e, 0)
		}
		old, _ := w.load()
		got, err := w.s.Withdraw(ctx, w.c.ID, "its fix moved")
		log, _ := w.st.CaseLog(ctx, w.c.ID)
		if err != nil || got.State != Withdrawn || got.ClosedProposals != old.ClosedProposals || !logHas(log, ActionWithdrawn, old.PR+" was merged by a person") {
			t.Fatalf("the withdrawal over a request a person merged: %+v %v", got, err)
		}
	})
	t.Run("gh fails", func(t *testing.T) {
		w := newLifeWorld(t)
		for _, e := range []string{"owner prepare", "check", "propose", "a person closes the request", "gh fails"} {
			w.step(e, 0)
		}
		if _, err := w.s.Withdraw(ctx, w.c.ID, "its fix moved"); err == nil || errors.Is(err, ErrRefused) {
			t.Fatalf("the withdrawal with gh down: %v", err)
		}
		if got, _ := w.load(); got.State == Withdrawn {
			t.Fatal("withdrawn with gh down")
		}
	})
}

// A case merged into another while its request is open is not withdrawn on a read gh refused, and writes nothing to
// the case it went into.
func TestAMergedCasesOwnerWithdrawalWritesNothingToItsSurvivor(t *testing.T) {
	ctx := context.Background()
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "propose", "the case merges into another"} {
		w.step(e, 0)
	}
	before, _ := w.st.CaseLog(ctx, w.twin.ID)
	if _, err := w.s.Withdraw(ctx, w.c.ID, "its fix moved"); err == nil {
		t.Fatal("a merged case's package with its request open was withdrawn")
	}
	after, _ := w.st.CaseLog(ctx, w.twin.ID)
	if len(after) != len(before) {
		t.Fatalf("the withdrawal wrote %d lines to the case it went into", len(after)-len(before))
	}
	if got, _ := w.load(); got.State == Withdrawn {
		t.Fatal("withdrawn")
	}
}

// A package that published its branch with no request it names (a create whose response was lost) is not withdrawn
// while a request is open at its branch.
func TestAWithdrawalFindsARequestOpenAtTheBranch(t *testing.T) {
	ctx := context.Background()
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "the next create's response is lost", "propose"} {
		w.step(e, 0)
	}
	p, _ := w.load()
	if p.PR != "" || p.PublishedHead == "" {
		t.Fatalf("the fixture: PR %q, published %q", p.PR, p.PublishedHead)
	}
	if _, err := w.s.Withdraw(ctx, w.c.ID, "its fix moved"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has an open pull request") {
		t.Fatalf("the withdrawal with a request open at its branch: %v", err)
	}
	if got, _ := w.load(); got.State == Withdrawn {
		t.Fatal("withdrawn")
	}
}

// With no request saved, the withdrawal reads every request at the package's branch, of any state and base, and reads
// the package's own (its commit) as it reads a saved one: one a person retargeted and left open refuses, one a person
// merged or closed is withdrawn as such. A closed request at the branch of another commit (an earlier package's) is
// not the package's.
func TestAWithdrawalReadsTheBranchsRequestsOfAnyStateAndBase(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, state string
		own         bool
		refuses     string
		line        string
		closed      int
	}{
		{"retargeted and open", "OPEN", true, "has an open pull request", "", 0},
		{"merged by a person", "MERGED", true, "", "was merged by a person", 0},
		{"closed by a person", "CLOSED", true, "", "was closed without merging", 1},
		{"an earlier package's, closed", "CLOSED", false, "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newLifeWorld(t)
			for _, e := range []string{"owner prepare", "check", "the next create's response is lost", "propose"} {
				w.step(e, 0)
			}
			old, _ := w.load()
			if old.PR != "" || old.PublishedHead == "" || old.Head == "" {
				t.Fatalf("the fixture: PR %q, published %q, head %q", old.PR, old.PublishedHead, old.Head)
			}
			head := old.Head
			if !tc.own {
				head = strings.Repeat("c0ffee", 7)[:40]
			}
			url := "https://github.com/" + old.Repo + "/pull/7"
			// GitHub as it answers once the request was retargeted, merged or closed: a list by the old base, or of open
			// requests alone, does not hold it
			w.s.GH = script(t, t.TempDir(), "gh", `case "$1 $2 $*" in
"pr list"*"--base"*|"pr list"*"--state open"*) echo '[]';;
"pr list"*) echo '[{"url":"`+url+`","state":"`+tc.state+`","headRefOid":"`+head+`"}]';;
*) echo "gh: unexpected $*" >&2; exit 1;;
esac`)
			got, err := w.s.Withdraw(ctx, w.c.ID, "its fix moved")
			if tc.refuses != "" {
				if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.refuses) {
					t.Fatalf("the withdrawal: %v, want a refusal naming %q", err, tc.refuses)
				}
				if p, _ := w.load(); p.State == Withdrawn {
					t.Fatal("withdrawn")
				}
				return
			}
			log, _ := w.st.CaseLog(ctx, w.c.ID)
			if err != nil || got.State != Withdrawn || got.ClosedProposals != old.ClosedProposals+tc.closed {
				t.Fatalf("the withdrawal: %+v %v", got, err)
			}
			if tc.line != "" && !logHas(log, ActionWithdrawn, url+" "+tc.line) || tc.line == "" && logHas(log, ActionWithdrawn, url) {
				t.Fatalf("the withdrawal's line, want %q: %+v", tc.line, log)
			}
		})
	}
}

// A package rolling back is not withdrawn: it ends with its rollback.
func TestAPackageRollingBackIsNotWithdrawn(t *testing.T) {
	w := newLifeWorld(t)
	w.step("owner prepare", 0)
	p, _ := w.load()
	p.State = RollingBack
	if err := w.s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Withdraw(context.Background(), w.c.ID, "its fix moved"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "rolling back") {
		t.Fatalf("the withdrawal of a package rolling back: %v", err)
	}
}

// The owner's withdrawal of a configuration proposal is not a moved file: it counts no withdrawal toward the case's
// closed proposals.
func TestTheOwnersConfigurationWithdrawalIsNoMovedFile(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	got, err := r.Ship.Withdraw(ctx, c.ID, "its fix moved")
	if err != nil || got.State != Withdrawn || got.Withdrawals != p.Withdrawals || got.ClosedProposals != p.ClosedProposals {
		t.Fatalf("the owner's withdrawal of a configuration proposal: %+v %v", got, err)
	}
}

// A hold names only the paths that end the package in its state: ship land for a proposal reviewed clean, a person's
// merge for a hand landing's request, and the withdrawal for every one.
func TestTheHoldNamesOnlyWhatWorksForThePackage(t *testing.T) {
	c := store.Case{ID: "c0ffee123"}
	clean := []Review{{Round: 1, Head: "h", Text: "clean", Comment: "u#review"}}
	for _, tc := range []struct {
		name  string
		p     Package
		lands bool
		hand  bool
	}{
		{"checked", Package{Kind: KindRepo, Repo: "agent-sh/fake", State: Checked}, false, false},
		{"revising", Package{Kind: KindRepo, Repo: "agent-sh/fake", State: Revising, PR: "u", Reviews: []Review{{Round: 1, Medium: 1}}}, false, false},
		{"proposed clean", Package{Kind: KindRepo, Repo: "agent-sh/fake", State: Proposed, Head: "h", PR: "u", Reviews: clean}, true, false},
		{"hand landing", Package{Kind: KindRepo, Repo: "agent-sh/fake", State: Proposed, Head: "h", PR: "u", Hand: []string{"go.mod"}, Reviews: clean}, false, true},
	} {
		why := unfinishedHold(c, tc.p, KindConfig, ruleTarget)
		if !strings.Contains(why, "momentum ship withdraw --case "+c.ID) || strings.Contains(why, "momentum ship land") != tc.lands || strings.Contains(why, "by hand") != tc.hand {
			t.Errorf("%s: %s", tc.name, why)
		}
	}
}

// A dry pass settles a configuration package that used its proposals and came back in a new episode as the live pass
// does: the new episode's counts start again.
func TestADryPassResetsANewEpisodesCountsAsALivePassDoes(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	p.State, p.ClosedProposals, p.Withdrawals = Withdrawn, MaxClosedProposals, MaxClosedProposals+1
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	comeBack(t, r.Store, c, "", ruleTarget)
	dc, dstep, dry, err := r.pick(ctx, "failures", c.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	lc, lstep, live, err := r.pick(ctx, "failures", c.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if dc.ID != lc.ID || dstep != lstep || dry != live {
		t.Fatalf("the dry pass: %s %s %q; the live one: %s %s %q", dc.ID, dstep, dry, lc.ID, lstep, live)
	}
}

// The owner's prepare after a configuration package was withdrawn does not start a proposal down: a configuration
// package's withdrawals count toward its own kind only.
func TestARepositoryPackageCarriesNoConfigurationCounts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipWithSandbox(st, f, Sandbox{})
	home := fakeHome(t)
	s.ConfigTargets = ConfigTargets{Enabled: true, Allow: []string{ruleTarget}, Home: home}
	c := testingCase(t, st)
	p, err := s.PrepareConfig(ctx, c.ID, ruleTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	p.State, p.ClosedProposals, p.Withdrawals = Withdrawn, 1, 2 // its file moved twice
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	next, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil || next.Kind != KindRepo || next.ClosedProposals != 0 {
		t.Fatalf("the owner's prepare after a configuration package's withdrawal: %+v %v", next, err)
	}
}

// A configuration package carries its withdrawn predecessor's counts within the case's episode, and starts them again
// in a new one: a new episode is a new instance of the problem.
func TestAConfigurationPackageCarriesItsCountsWithinItsEpisode(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := shipWithSandbox(st, newRepo(t, nil), Sandbox{})
	s.ConfigTargets = ConfigTargets{Enabled: true, Allow: []string{ruleTarget}, Home: fakeHome(t)}
	c := testingCase(t, st)
	p, err := s.PrepareConfig(ctx, c.ID, ruleTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	p.State, p.ClosedProposals, p.Withdrawals = Withdrawn, 1, 2 // its file moved twice
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	same, err := s.PrepareConfig(ctx, c.ID, ruleTarget, "")
	if err != nil || same.ClosedProposals != 1 || same.Withdrawals != 2 {
		t.Fatalf("a package of the same episode: %+v %v", same, err)
	}
	same.State = Withdrawn
	if err := s.save(&same); err != nil {
		t.Fatal(err)
	}
	c = comeBack(t, st, c, "", ruleTarget)
	next, err := s.PrepareConfig(ctx, c.ID, ruleTarget, "")
	if err != nil || next.Episode != c.Episode || next.ClosedProposals != 0 || next.Withdrawals != 0 {
		t.Fatalf("a package of a new episode: %+v %v", next, err)
	}
}

// A proposal's files are written whole, through no link at their name: a link there is replaced, and the file it points
// to stays as it was.
func TestAProposalsFileIsWrittenThroughNoLink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	write(t, outside, "the owner's own\n")
	name := filepath.Join(dir, "c0ffee.review.md")
	if err := os.Symlink(outside, name); err != nil {
		t.Fatal(err)
	}
	if err := writeFileSafe(name, []byte("the review\n")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "the owner's own\n" {
		t.Fatalf("the write went through the link: %q", raw)
	}
	if info, err := os.Lstat(name); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the file: %v %v", info, err)
	}
	if raw, _ := os.ReadFile(name); string(raw) != "the review\n" {
		t.Fatalf("the file holds %q", raw)
	}
}

// A posted review is written through no link at its file's name.
func TestAPostedReviewIsWrittenThroughNoLink(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	write(t, outside, "the owner's own\n")
	if err := os.Symlink(outside, r.Ship.proposalFile(p, ".review.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	if raw, _ := os.ReadFile(outside); string(raw) != "the owner's own\n" {
		t.Fatalf("the review went through the link: %q", raw)
	}
	if info, err := os.Lstat(r.Ship.proposalFile(p, ".review.md")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the review file: %v %v", info, err)
	}
}

// The review step between an agent's apply of a filed proposal and its case's close reads nothing of the file and
// files nothing again: the proposal stays filed as it was.
func TestTheReviewStepLeavesAnAppliedFilingAsItWas(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := filedConfigCase(t)
	n, _ := noteByID(t, r.Store, configNoteID(c.ID))
	agentWrites(t, home, newRule)
	r.reviews(ctx, c.ID)
	got := loadPackage(t, r.Ship, c.ID)
	after, _ := noteByID(t, r.Store, configNoteID(c.ID))
	if got.State != Proposed || !got.Filed.Equal(p.Filed) || after.Detail != n.Detail || !after.Done.IsZero() {
		t.Fatalf("the filed proposal after the review step: %s filed %v (was %v)", got.State, got.Filed, p.Filed)
	}
}

// A package's build checks the run's copy of its file: a copy whose hash is not the file's holds the build.
func TestABuildChecksTheRunsCopyOfAnExistingPackagesFile(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Medium: 1, Text: "a finding"}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	if got := loadPackage(t, r.Ship, c.ID); got.State != Revising {
		t.Fatalf("the package after a blocking review: %s", got.State)
	}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(newRule))}}
	run := store.LoopRun{ID: "fixture-build-copy", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build-copy"), CaseEpisode: c.Episode}
	if _, err := r.buildPackage(ctx, c, run); !held(err) || !strings.Contains(err.Error(), "changed since the run's configuration copy was made") {
		t.Fatalf("the build with a copy that is not the file: %v", err)
	}
}

var _ = time.Now

// A dry pass counts a moved file's withdrawal as the live pass does: a proposal whose file moved a third time used its
// proposals, and both passes stop the case alike.
func TestADryPassCountsAMovedFileAsALivePassDoes(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	p.Withdrawals, p.ClosedProposals = 2, 1
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	agentWrites(t, home, oldRule+ownerEdit)
	dc, dstep, dry, err := r.pick(ctx, "failures", c.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	lc, lstep, live, err := r.pick(ctx, "failures", c.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if dc.ID != lc.ID || dstep != lstep || dry != live {
		t.Fatalf("the dry pass: %s %s %q; the live one: %s %s %q", dc.ID, dstep, dry, lc.ID, lstep, live)
	}
	if got := loadPackage(t, r.Ship, c.ID); got.State != Withdrawn || got.Withdrawals != 3 || got.ClosedProposals != 2 {
		t.Fatalf("the live withdrawal's counts: %+v", got)
	}
}

// A dry pass holds a case as the live pass does once its settlement withdrew the package: a proposal of one file whose
// case came back naming another is withdrawn by the settlement, and neither pass holds the case for it.
func TestADryPassHoldsByItsSettledPackage(t *testing.T) {
	ctx := context.Background()
	const other = ".config/agents/zz-other.md"
	r, c, home, _ := proposedConfigCase(t)
	write(t, filepath.Join(home, other), oldRule)
	chmod(t, filepath.Join(home, other), 0o640)
	r.Ship.ConfigTargets.Allow = append(r.Ship.ConfigTargets.Allow, other)
	comeBack(t, r.Store, c, "", other)
	dc, dstep, dry, err := r.pick(ctx, "failures", c.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	lc, lstep, live, err := r.pick(ctx, "failures", c.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if dc.ID != lc.ID || dstep != lstep || dry != live || strings.Contains(live, "must end first") {
		t.Fatalf("the dry pass: %s %s %q; the live one: %s %s %q", dc.ID, dstep, dry, lc.ID, lstep, live)
	}
}

// The owner's withdrawal refuses a hand landing that a person landed by a push to the default branch and whose request
// they then closed: its landing, which the next pass records.
func TestTheOwnersWithdrawalRefusesAHandLandingLandedByPush(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED","headRefOid":"`+p.Head+`"}'`)
	if _, err := s.Withdraw(ctx, c.ID, "its fix moved"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "the next pass records") {
		t.Fatalf("the withdrawal of a hand landing landed by push: %v", err)
	}
	if got := loadPackage(t, s, c.ID); got.State != p.State || got.PR != p.PR || got.ClosedProposals != p.ClosedProposals {
		t.Fatalf("the hand landing changed: %+v", got)
	}
}

// A proposal reviewed clean whose case's fix moved to a configuration file keeps its review and landing going, and its
// case holds with the reason and a standing note, so a person learns the fix moved.
func TestAProposalOfAnotherPlaceHoldsItsCaseWithANote(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "propose", "review clean", "new episode, a configuration file", "pass", "pass"} {
		w.step(e, 0)
	}
	c, _ := w.st.CaseByID(w.ctx, w.c.ID)
	log, _ := w.st.CaseLog(w.ctx, w.c.ID)
	why := store.HoldOf(c, log)
	if !strings.HasPrefix(why, unfinishedPrefix+"agent-sh/fake must end first") || !strings.Contains(why, "momentum ship land "+w.c.ID) {
		t.Fatalf("the hold: %q", why)
	}
	standing := false
	notes, _ := w.st.Notes(w.ctx)
	for _, n := range notes {
		standing = standing || n.Done.IsZero() && strings.HasPrefix(n.ID, holdNotePrefix(w.c.ID)) && n.Detail == why
	}
	if p, _ := w.load(); !standing || p.State != Proposed || p.PR == "" {
		t.Fatalf("the hold's note stands %v; the package %s %q", standing, p.State, p.PR)
	}
}
