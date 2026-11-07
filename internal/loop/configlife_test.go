package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// filedConfigCase is a configuration proposal reviewed clean by the loop and filed for an agent.
func filedConfigCase(t *testing.T) (*Runner, store.Case, string, Package) {
	t.Helper()
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	if done := r.reviews(ctx, c.ID); len(done) != 1 || done[0].Step != "file" || done[0].Skipped != "" {
		t.Fatalf("the review step of a clean configuration proposal: %+v", done)
	}
	p, _, _ = r.Ship.Load(c.ID)
	if p.Filed.IsZero() {
		t.Fatalf("the clean proposal was not filed: %+v", p)
	}
	return r, c, home, p
}

// agentWrites is an agent's or the owner's own edit of the file: momentum writes none.
func agentWrites(t *testing.T, home, content string) {
	t.Helper()
	write(t, filepath.Join(home, ruleTarget), content)
	if err := os.Chmod(filepath.Join(home, ruleTarget), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// otherTestingCase is a second case in testing, to name the same file.
func otherTestingCase(t *testing.T, st *store.Store) store.Case {
	t.Helper()
	other := openCase(t, st, "failures", "marker-c0ffee-other", 1)
	mech, next := "m", "n"
	c, err := st.UpdateCase(context.Background(), other.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A configuration proposal goes through the loop's clean-context review before anything is filed: a review with a
// medium finding is posted to the case's log and sends it back to its build, with no note; a clean review files it,
// with a low problem note that names the patch, a patch line to apply it, and how the case is closed or declined.
func TestAConfigurationProposalIsFiledOnlyAfterACleanReview(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	if _, ok := noteByID(t, r.Store, configNoteID(c.ID)); ok {
		t.Fatal("a proposal no review read has a note")
	}
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "One finding.", Findings: []Finding{{Severity: "medium", Where: ruleTarget, Problem: "y", Fix: "z"}}}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	if got, _, _ := r.Ship.Load(c.ID); got.State != Revising || !got.Filed.IsZero() || !strings.HasPrefix(got.Reviews[0].Comment, "case log") {
		t.Fatalf("a medium review: %+v", got)
	}
	if log, _ := r.Store.CaseLog(ctx, c.ID); !logHas(log, "review posted", "One finding.") {
		t.Fatalf("the review is not in the case's log: %+v", log)
	}
	if _, ok := noteByID(t, r.Store, configNoteID(c.ID)); ok {
		t.Fatal("a proposal with a medium finding was filed")
	}
	r2, c2, home2, p2 := filedConfigCase(t)
	n, ok := noteByID(t, r2.Store, configNoteID(c2.ID))
	patch := r2.Ship.proposalFile(p2, ".patch")
	if !ok || n.Kind != store.NoteProblem || n.Importance != store.ImportanceLow || !strings.Contains(n.Detail, configApplyLine(home2, p2, patch)) ||
		!strings.Contains(n.Detail, "momentum case close "+c2.ID+" --reason already-solved --fix '"+configFix(p2)+"'") ||
		!strings.Contains(n.Detail, "momentum case close "+c2.ID+" --reason not-worth-it") || !strings.Contains(n.Detail, "never writes") ||
		strings.Contains(n.Detail, "pull request") {
		t.Fatalf("the filed proposal's note: %+v", n)
	}
}

// A filed proposal waits in testing as proposed (configuration), with no read of its file and no hold on it: an agent
// may apply it, or the owner edit the file, or the file go, and nothing withdraws it; another case may propose a change
// to the same file.
func TestAFiledConfigurationProposalWaitsWithNoFileRead(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(home string){
		"applied":   func(home string) { agentWrites(t, home, newRule) },
		"edited":    func(home string) { agentWrites(t, home, oldRule+ownerEdit) },
		"gone":      func(home string) { os.Remove(filepath.Join(home, ruleTarget)) },
		"untouched": func(home string) {},
	} {
		t.Run(name, func(t *testing.T) {
			r, c, home, _ := filedConfigCase(t)
			change(home)
			if got, _, why, err := r.pick(ctx, "failures", c.ID, false); err != nil || got.ID != "" || !strings.Contains(why, store.ConfigStage) {
				t.Fatalf("the pass over a filed proposal: %q %q %v", got.ID, why, err)
			}
			if p, _, _ := r.Ship.Load(c.ID); p.State != Proposed {
				t.Fatalf("the filed proposal: %s", p.State)
			}
			cur, _ := r.Store.CaseByID(ctx, c.ID)
			log, _ := r.Store.CaseLog(ctx, c.ID)
			if cur.Status != store.CaseTesting || store.ConfigOf(cur, log) != ruleTarget {
				t.Fatalf("the case of a filed proposal: %s %q", cur.Status, store.ConfigOf(cur, log))
			}
			if name == "gone" {
				return
			}
			other := otherTestingCase(t, r.Store)
			if _, err := r.Ship.PrepareConfig(ctx, other.ID, ruleTarget, ""); err != nil {
				t.Fatalf("another case on the same file: %v", err)
			}
		})
	}
}

// The case's close ends a filed proposal: already solved with the fix the note names once an agent applied it, or not
// worth it to decline it. Either withdraws the package and resolves the note; the loop lands, deploys and measures
// nothing.
func TestAFiledConfigurationProposalEndsWithItsCasesClose(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"already-solved", "not-worth-it"} {
		t.Run(reason, func(t *testing.T) {
			r, c, home, p := filedConfigCase(t)
			var err error
			if reason == "already-solved" {
				agentWrites(t, home, newRule)
				_, err = r.Store.CloseSolvedBy(ctx, c.ID, configFix(p), "an agent applied the proposal", "test")
			} else {
				_, err = r.Store.CloseCase(ctx, c.ID, store.CloseReason(reason), "", "declined", "test")
			}
			if err != nil {
				t.Fatal(err)
			}
			r.pick(ctx, "failures", "", false)
			got, _, _ := r.Ship.Load(c.ID)
			if got.State != Withdrawn {
				t.Fatalf("the proposal of a closed case: %s", got.State)
			}
			if n, _ := noteByID(t, r.Store, configNoteID(c.ID)); n.Done.IsZero() {
				t.Fatal("the note stands after the close")
			}
			cur, _ := r.Store.CaseByID(ctx, c.ID)
			if cur.Status != store.CaseClosed || cur.Reason != store.CloseReason(reason) || !cur.Deployed.IsZero() {
				t.Fatalf("the closed case: %+v", cur)
			}
			if reason == "already-solved" {
				if log, _ := r.Store.CaseLog(ctx, c.ID); !logHas(log, "closed", configFix(p)) {
					t.Fatalf("the close does not name the fix: %+v", log)
				}
			}
		})
	}
}

// A case closed as already solved by a configuration fix comes back on new evidence, as any solved case does: the
// detector's recurrence is the only watch, and its next episode builds again.
func TestASolvedConfigurationCaseComesBackOnNewEvidence(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := filedConfigCase(t)
	agentWrites(t, home, newRule)
	if _, err := r.Store.CloseSolvedBy(ctx, c.ID, configFix(p), "an agent applied the proposal", "test"); err != nil {
		t.Fatal(err)
	}
	later := store.Event{Source: store.Transcripts, SourceKey: "again@1", TS: time.Now().Add(time.Minute), Session: "again", Harness: "claude",
		Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`}
	if _, err := r.Store.Put(ctx, []store.Event{later}); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title, Evidence: []store.Evidence{{Ref: "ev:" + later.ID()}}}
	back, _, err := r.Store.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != c.ID || back.Status == store.CaseClosed || back.Episode == c.Episode {
		t.Fatalf("a solved configuration case on new evidence: %+v", back)
	}
}

// A filed proposal that waits a week, the loop's shortest measurement window and the wait after which its other
// waiting notes speak up, has its note raised once from low to normal importance.
func TestAConfigurationProposalWaitingAWeekRaisesItsNote(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := filedConfigCase(t)
	clock := p.Filed.Add(6 * 24 * time.Hour)
	r.Now, r.Ship.Now = func() time.Time { return clock }, func() time.Time { return clock }
	r.pick(ctx, "failures", c.ID, false)
	if n, _ := noteByID(t, r.Store, configNoteID(c.ID)); n.Importance != store.ImportanceLow {
		t.Fatalf("a proposal waiting six days: %+v", n)
	}
	clock = p.Filed.Add(configRaise + time.Hour)
	r.pick(ctx, "failures", c.ID, false)
	n, _ := noteByID(t, r.Store, configNoteID(c.ID))
	if n.Importance != store.ImportanceNormal || !n.Done.IsZero() || !strings.Contains(n.Title, "a week") {
		t.Fatalf("a proposal waiting a week: %+v", n)
	}
	if _, _, err := r.Store.DoneNote(ctx, n.ID, clock); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(24 * time.Hour)
	r.pick(ctx, "failures", c.ID, false)
	if again, _ := noteByID(t, r.Store, configNoteID(c.ID)); again.Importance != store.ImportanceLow || !again.Done.IsZero() {
		t.Fatalf("the note after its one raise was resolved: %+v", again)
	}
}

// An unfiled proposal is withdrawn when its file moves under it; a filed one, never.
func TestAConfigurationProposalAppliedBeforeItsFilingIsWithdrawn(t *testing.T) {
	ctx := context.Background()
	for _, filed := range []bool{false, true} {
		t.Run(fmt.Sprint("filed ", filed), func(t *testing.T) {
			r, c, home, _ := proposedConfigCase(t)
			if filed {
				r, c, home, _ = filedConfigCase(t)
			}
			agentWrites(t, home, newRule)
			p, err := r.Ship.reconcileConfig(ctx, c.ID)
			if want := map[bool]string{false: Withdrawn, true: Proposed}[filed]; err != nil || p.State != want {
				t.Fatalf("a proposal whose file holds its change: %s %v, want %s", p.State, err, want)
			}
		})
	}
}

// An unapplied proposal's note stays while it waits: one its reader resolved is put again at the next pass.
func TestAnUnappliedConfigurationProposalsNoteStays(t *testing.T) {
	ctx := context.Background()
	r, c, _, _ := filedConfigCase(t)
	if _, _, err := r.Store.DoneNote(ctx, configNoteID(c.ID), time.Now()); err != nil {
		t.Fatal(err)
	}
	r.pick(ctx, "failures", c.ID, false)
	if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); !ok || !n.Done.IsZero() {
		t.Fatalf("the waiting proposal's note: %+v", n)
	}
}

// A configuration change that used its review rounds waits for a person, with no pull request to name.
func TestAConfigurationChangesRoundsNoteNamesNoPullRequest(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	for i := 1; i <= MaxRounds; i++ {
		p.Reviews = append(p.Reviews, Review{Round: i, Head: p.Head, Medium: 1, Text: "a finding", Comment: "case log", At: time.Now()})
	}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	r.pick(ctx, "failures", c.ID, false)
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	n, ok := noteByID(t, r.Store, configNoteID(c.ID))
	if !ok || !n.Done.IsZero() || strings.Contains(n.Detail, "pull request") {
		t.Fatalf("the rounds note of a configuration change: %+v", n)
	}
	if n, ok := noteByID(t, r.Store, waitingPrefix+store.IDPart(c.ID)); ok && n.Done.IsZero() {
		t.Fatalf("a second note for the same wait: %+v", n)
	}
}

// A proposal does not outlive its case's testing: a case that closed, or that began a new episode, withdraws it and
// resolves its note. A filed proposal whose note is gone gets it again while it waits.
func TestAConfigurationProposalEndsWithItsCasesTesting(t *testing.T) {
	ctx := context.Background()
	r, c, _, _ := filedConfigCase(t)
	if _, err := r.Store.CloseCase(ctx, c.ID, store.CloseReason("not-worth-it"), "", "declined", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.ReopenCase(ctx, c.ID, []store.Evidence{{Ref: "file:/t/marker-c0ffee-new"}}, "new evidence", "test"); err != nil {
		t.Fatal(err)
	}
	mech, next := "m", "n"
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Ship.reconcileConfig(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := r.Ship.Load(c.ID); got.State != Withdrawn {
		t.Fatalf("the proposal of a case in a new episode: %s", got.State)
	}
	if log, _ := r.Store.CaseLog(ctx, c.ID); !logHas(log, "configuration proposal withdrawn", "new episode") {
		t.Fatalf("the case's log: %+v", log)
	}

	// filed, but its note was never written (a pass stopped between the two): the wait puts it
	r, c, _, p := proposedConfigCase(t)
	p.Filed = time.Now()
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, _, why, err := r.pick(ctx, "failures", c.ID, false); err != nil || !strings.Contains(why, store.ConfigStage) {
		t.Fatalf("the wait: %q %v", why, err)
	}
	if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); !ok || !n.Done.IsZero() {
		t.Fatalf("the wait did not put the filed proposal's note: %+v", n)
	}
}

// A home that cannot be read as the targets' root (a link in its path) decides nothing for an unfiled proposal.
func TestAConfigurationHomeThatCannotBeReadDecidesNothing(t *testing.T) {
	ctx := context.Background()
	r, c, home, _ := proposedConfigCase(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(home, linked); err != nil {
		t.Fatal(err)
	}
	r.Ship.ConfigTargets.Home = linked
	if _, err := r.Ship.reconcileConfig(ctx, c.ID); err == nil || !strings.Contains(err.Error(), "holds a link") {
		t.Fatalf("a home through a link: %v", err)
	}
	if got, _, _ := r.Ship.Load(c.ID); got.State != Proposed {
		t.Fatalf("a home through a link withdrew the proposal: %s", got.State)
	}
}

// A file that keeps moving under its unfiled proposals stops its case: its first withdrawal is free, and each after it
// counts as a closed proposal.
func TestAConfigurationFileThatKeepsMovingStopsItsCase(t *testing.T) {
	ctx := context.Background()
	r, c, home, _ := proposedConfigCase(t)
	for i, want := range []int{0, 1, 2} {
		edit := strings.Replace(oldRule, "line d", "line d, edit "+string(rune('a'+i)), 1)
		agentWrites(t, home, edit)
		p, err := r.Ship.reconcileConfig(ctx, c.ID)
		if err != nil || p.State != Withdrawn || p.Withdrawals != i+1 || p.ClosedProposals != want {
			t.Fatalf("withdrawal %d: %+v %v", i+1, p, err)
		}
		if i == 2 {
			break
		}
		// the next build is on the file as it is, and its proposal carries the counts
		r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(edit))}}
		run := store.LoopRun{ID: "fixture-rebuild-" + string(rune('a'+i)), Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-rebuild-"+string(rune('a'+i))), CaseEpisode: c.Episode}
		p, err = r.buildPackage(ctx, c, run)
		if err != nil || p.Withdrawals != i+1 {
			t.Fatalf("the build after withdrawal %d: %+v %v", i+1, p, err)
		}
		write(t, filepath.Join(p.Work, ruleTarget), strings.Replace(edit, "old rule", "new rule", 1))
		write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
		if _, err := r.Ship.CheckArgs(ctx, c.ID, []string{"python3", "tests/test_rules.py"}, nil, fixturePatch(t, r.Ship, c.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := proposePatch(t, r.Ship, ctx, c.ID, configProposal); err != nil {
			t.Fatal(err)
		}
	}
	if _, step, why, err := r.pick(ctx, "failures", c.ID, false); err != nil || step == StepBuild || !strings.Contains(why, "used its proposals") {
		t.Fatalf("a case whose file kept moving: %q %q %v", step, why, err)
	}
}

// Harness settings that carry hooks, commands, permissions or sandbox settings are never targets, whatever allow says.
func TestHarnessSettingsAreNeverTargets(t *testing.T) {
	for _, name := range []string{".claude/settings.json", ".claude/settings.local.json", ".codex/config.toml", ".codex/work.config.toml",
		".codex/hooks.json", ".codex/rules/default.rules", ".codex/agents/a.toml", ".grok/config.toml", ".grok/sandbox.toml", ".grok/AGENTS.md",
		".grok/hooks-paths", ".config/opencode/opencode.json", ".config/opencode/cli.json", ".config/opencode/service.json",
		".config/opencode/package.json", ".hermes/cron/jobs.json", ".hermes/config.yaml", ".hermes/profile.yaml", ".pi/agent/settings.json"} {
		refusedTarget(t, ConfigTargets{}, name, "a harness's settings")
	}
	for _, name := range []string{".codex/AGENTS.md", ".pi/agent/AGENTS.md", ".hermes/SOUL.md", ruleTarget} {
		if why := (ConfigTargets{}).targetRefusal(name); strings.Contains(why, "harness") {
			t.Errorf("%s is an instruction file, refused as %q", name, why)
		}
	}
}

// A file that is gone for a pass decides nothing for an unfiled proposal: neither withdrawn nor kept, read again.
func TestAMissingConfigurationFileDecidesNothing(t *testing.T) {
	ctx := context.Background()
	r, c, home, _ := proposedConfigCase(t)
	file := filepath.Join(home, ruleTarget)
	os.Rename(file, file+".away")
	if got, err := r.Ship.reconcileConfig(ctx, c.ID); !held(err) || got.State != Proposed {
		t.Fatalf("a proposal whose file is gone for a pass: %+v %v", got.State, err)
	}
	os.Rename(file+".away", file)
	if got, err := r.Ship.reconcileConfig(ctx, c.ID); err != nil || got.State != Proposed {
		t.Fatalf("the proposal once its file is back: %+v %v", got.State, err)
	}
}

// A proposal whose file the loop may no longer change (configuration targets off, or allow no longer naming it) is
// withdrawn, without counting, and its note resolves; the case log says why.
func TestAConfigurationProposalEndsWhenItsFileIsNoLongerAllowed(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		off  func(s *Ship)
		says string
	}{
		"off":         {func(s *Ship) { s.ConfigTargets.Enabled = false }, "configuration targets are off"},
		"not allowed": {func(s *Ship) { s.ConfigTargets.Allow = []string{".config/agents/OTHER.md"} }, "allow names"},
	} {
		t.Run(name, func(t *testing.T) {
			r, c, _, _ := filedConfigCase(t)
			tc.off(r.Ship)
			r.pick(ctx, "failures", c.ID, false)
			p, _, _ := r.Ship.Load(c.ID)
			if p.State != Withdrawn || p.Withdrawals != 0 || p.ClosedProposals != 0 {
				t.Fatalf("a proposal the loop may no longer make: %+v", p)
			}
			if n, _ := noteByID(t, r.Store, configNoteID(c.ID)); n.Done.IsZero() {
				t.Fatal("its note stands")
			}
			if log, _ := r.Store.CaseLog(ctx, c.ID); !logHas(log, "configuration proposal withdrawn", tc.says) {
				t.Fatalf("the case's log does not say why: %+v", log)
			}
		})
	}
}

// A proposal whose file moved after the pass's read and before its clean review files it is not filed: it is
// withdrawn, and the case builds again on the file as it is. A file that is not there at filing holds the filing.
func TestAProposalWhoseFileMovedBeforeItsFilingIsNotFiled(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	r.pick(ctx, "failures", c.ID, false) // the pass's read: the file holds its base
	agentWrites(t, home, oldRule+ownerEdit)
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	got, _, _ := r.Ship.Load(c.ID)
	if !got.Filed.IsZero() || got.State != Withdrawn || got.Withdrawals != 1 {
		t.Fatalf("a proposal filed over a moved file: %+v", got)
	}
	if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); ok && n.Done.IsZero() {
		t.Fatalf("a note was filed: %+v", n)
	}

	r, c, home, p = proposedConfigCase(t)
	os.Rename(filepath.Join(home, ruleTarget), filepath.Join(home, ruleTarget+".away"))
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	if got, _, _ := r.Ship.Load(c.ID); !got.Filed.IsZero() || got.State != Proposed {
		t.Fatalf("a proposal whose file was not there at filing: %+v", got)
	}
}

// An unfiled proposal whose review rounds are used waits for a person, as a filed one does: its file is not read, so a
// change of it neither withdraws the proposal nor gives the case fresh rounds; the rounds note says how to end it.
func TestAConfigurationProposalThatUsedItsRoundsWaitsForAPerson(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	for i := 1; i <= MaxRounds; i++ {
		p.Reviews = append(p.Reviews, Review{Round: i, Head: p.Head, Medium: 1, Text: "a finding", Comment: "case log", At: time.Now()})
	}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	agentWrites(t, home, oldRule+ownerEdit)
	if _, step, why, err := r.pick(ctx, "failures", c.ID, false); err != nil || step == StepBuild || !strings.Contains(why, "used its review rounds") {
		t.Fatalf("a proposal whose rounds are used, over an edited file: %q %q %v", step, why, err)
	}
	if got, _, _ := r.Ship.Load(c.ID); got.State != Proposed || got.Withdrawals != 0 {
		t.Fatalf("the proposal: %+v", got)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	n, ok := noteByID(t, r.Store, configNoteID(c.ID))
	if !ok || strings.Contains(n.Detail, "Change the file by hand") || !strings.Contains(n.Detail, "close the case (not-worth-it)") ||
		!strings.Contains(n.Detail, "apply your own fix and close it as already-solved") {
		t.Fatalf("the rounds note: %+v", n)
	}
}

// A withdrawn proposal's case no longer shows as proposed (configuration).
func TestAWithdrawnConfigurationProposalShowsNoStage(t *testing.T) {
	ctx := context.Background()
	r, c, _, _ := filedConfigCase(t)
	r.Ship.ConfigTargets.Enabled = false
	r.pick(ctx, "failures", c.ID, false)
	cur, _ := r.Store.CaseByID(ctx, c.ID)
	log, _ := r.Store.CaseLog(ctx, c.ID)
	if p, _, _ := r.Ship.Load(c.ID); p.State != Withdrawn || store.ConfigOf(cur, log) != "" {
		t.Fatalf("a withdrawn proposal's stage: %s %q", p.State, store.ConfigOf(cur, log))
	}
}

// An unfiled proposal whose file is gone a day, past an editor's replace or a reorganization, gets a note: until then
// each pass holds it and reads again.
func TestAConfigurationFileGoneADayGetsANote(t *testing.T) {
	ctx := context.Background()
	r, c, home, _ := proposedConfigCase(t)
	clock := time.Now()
	r.Now, r.Ship.Now = func() time.Time { return clock }, func() time.Time { return clock }
	os.RemoveAll(filepath.Join(home, ".config/agents"))
	r.pick(ctx, "failures", c.ID, false)
	if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); ok && n.Done.IsZero() {
		t.Fatalf("a note at once: %+v", n)
	}
	clock = clock.Add(configGone + time.Hour)
	r.pick(ctx, "failures", c.ID, false)
	if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); !ok || !n.Done.IsZero() || !strings.Contains(n.Detail, ruleTarget) {
		t.Fatalf("no note for a file gone a day: %+v", n)
	}
	if p, _, _ := r.Ship.Load(c.ID); p.State != Proposed {
		t.Fatalf("the proposal: %s", p.State)
	}
}

// The fix a case with a filed proposal closes with is the one its note names: FiledConfigFix gives it, and none for a
// case without one.
func TestAFiledConfigurationProposalNamesItsFix(t *testing.T) {
	r, c, _, p := filedConfigCase(t)
	if fix, ok, err := FiledConfigFix(r.Ship.Dir, c); err != nil || !ok || fix != configFix(p) {
		t.Fatalf("the filed proposal's fix: %q %v %v", fix, ok, err)
	}
	r2, c2, _, _ := proposedConfigCase(t)
	if _, ok, err := FiledConfigFix(r2.Ship.Dir, c2); err != nil || ok {
		t.Fatalf("an unfiled proposal's fix: %v %v", ok, err)
	}
}

// A configuration proposal keeps its measurement plan as information: the loop never measures it, so the plan is
// neither counted nor waited on, and a signal with no event yet still proposes.
func TestAConfigurationProposalKeepsItsPlanAsInformation(t *testing.T) {
	r, c, _, _ := proposedConfigCase(t)
	p, _, _ := r.Ship.Load(c.ID)
	if p.Plan == nil || p.Plan.Signal.Match[0] != configProposal.Signal.Match[0] || p.Plan.Units != "" || p.Plan.Before != 0 {
		t.Fatalf("the proposal's plan: %+v", p.Plan)
	}
}

// configApplyLine is the line the note gives an agent: a check that the file holds the content the proposal was built
// on, and only then GNU patch, with no fuzz, forward only, and no backup or reject file.
func configApplyLine(home string, p Package, patch string) string {
	return "echo '" + p.TargetBase + " *" + filepath.Join(home, ruleTarget) + "' | sha256sum -c --status - && patch -p1 -F0 --forward --no-backup-if-mismatch --reject-file=- -d " +
		home + " -i " + patch
}

// caseStanding are the case's standing notes.
func caseStanding(t *testing.T, st *store.Store, caseID string) []store.Note {
	t.Helper()
	notes, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Note
	for _, n := range notes {
		if n.Done.IsZero() && strings.Contains(n.ID, store.IDPart(caseID)) {
			out = append(out, n)
		}
	}
	return out
}

// While a configuration proposal waits on a person, in any of its waiting states, exactly one standing note says why
// and what to do: a pass puts it back once every note was resolved. When the wait ends, no note of it stands.
func TestAWaitingConfigurationProposalHasExactlyOneStandingNote(t *testing.T) {
	ctx := context.Background()
	roundsUsed := func(t *testing.T) (*Runner, store.Case) {
		r, c, _, p := proposedConfigCase(t)
		for i := 1; i <= MaxRounds; i++ {
			p.Reviews = append(p.Reviews, Review{Round: i, Head: p.Head, Medium: 1, Text: "a finding", Comment: "case log", At: time.Now()})
		}
		if err := r.Ship.save(&p); err != nil {
			t.Fatal(err)
		}
		return r, c
	}
	gone := func(again bool) func(t *testing.T) (*Runner, store.Case) {
		return func(t *testing.T) (*Runner, store.Case) {
			r, c, home, _ := proposedConfigCase(t)
			clock := time.Now()
			r.Now, r.Ship.Now = func() time.Time { return clock }, func() time.Time { return clock }
			file := filepath.Join(home, ruleTarget)
			away := func() {
				os.Rename(file, file+".away")
				r.pick(ctx, "failures", c.ID, false)
				clock = clock.Add(configGone + time.Hour)
			}
			away()
			if again {
				r.pick(ctx, "failures", c.ID, false) // its note
				os.Rename(file+".away", file)
				r.pick(ctx, "failures", c.ID, false) // back: the wait ends
				if s := caseStanding(t, r.Store, c.ID); len(s) != 0 {
					t.Fatalf("notes stand once the file is back: %+v", s)
				}
				away()
			}
			return r, c
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*Runner, store.Case)
		says  string
		stage bool
	}{
		{"filed", func(t *testing.T) (*Runner, store.Case) {
			r, c, _, _ := filedConfigCase(t)
			return r, c
		}, "--reason already-solved --fix", true},
		{"rounds used", roundsUsed, "review rounds", true},
		{"file gone a day", gone(false), "has not been there since", false},
		{"file gone a second time", gone(true), "has not been there since", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := tc.setup(t)
			pass := func() {
				t.Helper()
				r.pick(ctx, "failures", c.ID, false)
				if err := r.noteWaiting(ctx); err != nil {
					t.Fatal(err)
				}
			}
			pass()
			for _, n := range caseStanding(t, r.Store, c.ID) {
				if _, _, err := r.Store.DoneNote(ctx, n.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			pass()
			s := caseStanding(t, r.Store, c.ID)
			if len(s) != 1 || s[0].ID != configNoteID(c.ID) || !strings.Contains(s[0].Detail, tc.says) {
				t.Fatalf("the standing notes of a waiting proposal: %+v", s)
			}
			cur, _ := r.Store.CaseByID(ctx, c.ID)
			log, _ := r.Store.CaseLog(ctx, c.ID)
			if got := store.ConfigOf(cur, log); tc.stage && got != ruleTarget {
				t.Fatalf("its stage: %q", got)
			}
			if _, err := r.Store.CloseCase(ctx, c.ID, store.CloseReason("not-worth-it"), "", "declined", "test"); err != nil {
				t.Fatal(err)
			}
			pass()
			if s := caseStanding(t, r.Store, c.ID); len(s) != 0 {
				t.Fatalf("notes stand once the wait ended: %+v", s)
			}
		})
	}
}

// The note's apply line checks the file holds the proposal's base before GNU patch runs: on the base it applies the
// change, on a file edited since it changes nothing and fails.
func TestTheConfigurationNotesApplyLineChecksItsBase(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("no GNU patch here")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum here")
	}
	r, c, home, p := filedConfigCase(t)
	line := configApplyLine(home, p, r.Ship.proposalFile(p, ".patch"))
	if n, _ := noteByID(t, r.Store, configNoteID(c.ID)); !strings.Contains(n.Detail, line) {
		t.Fatalf("the note's apply line: %s", n.Detail)
	}
	file := filepath.Join(home, ruleTarget)
	agentWrites(t, home, oldRule+ownerEdit)
	if err := exec.Command("sh", "-c", line).Run(); err == nil || readFile(t, file) != oldRule+ownerEdit {
		t.Fatalf("the line over an edited file: %v %q", err, readFile(t, file))
	}
	agentWrites(t, home, oldRule)
	if out, err := exec.Command("sh", "-c", line).CombinedOutput(); err != nil || readFile(t, file) != newRule {
		t.Fatalf("the line on the base: %v %s", err, out)
	}
}

// A configuration file the scrubber would mask is held for that, not for a copy that seems to differ: its copy is the
// scrubbed text, which the build never mounts.
func TestAMaskedConfigurationFileIsHeldForTheScrubber(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	home := fakeHome(t)
	secret := oldRule + "password=marker-7f3a\n"
	agentWrites(t, home, secret)
	r := localRunner(t, st)
	r.Ship = configShip(t, st, home)
	r.Ship.Dir = r.Dir
	r.pass = &passInputs{}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(strings.Replace(secret, "password=marker-7f3a", "password=[masked]", 1)))}}
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	run := store.LoopRun{ID: "fixture-build", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build"), CaseEpisode: c.Episode}
	if _, err := r.buildPackage(ctx, c, run); !held(err) || !strings.Contains(err.Error(), "scrubber would mask") {
		t.Fatalf("a masked file: %v", err)
	}
}

// A configuration path is one form wherever it is read: clean and under the home, with no whitespace, at most
// ConfigPathMax bytes, so its fix reference fits a reference.
func TestAConfigurationPathIsOneForm(t *testing.T) {
	long := ".config/agents/" + strings.Repeat("a", store.ConfigPathMax-len(".config/agents/")-3) + ".md"
	if err := store.CheckConfigPath(long); err != nil || len(long) != store.ConfigPathMax {
		t.Fatalf("a path of the bound: %d %v", len(long), err)
	}
	for _, bad := range []string{long + "x", ".config/agents/MY RULES.md", ".config/agents/\tRULES.md", "../x.md", "/abs/x.md"} {
		if err := store.CheckConfigPath(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
		if _, err := (ConfigTargets{Allow: []string{bad}}).admit(bad); err == nil {
			t.Errorf("admit %q", bad)
		}
	}
	if err := store.CheckConfigFix("config:~/" + long + "@" + strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("the fix of a path of the bound: %v", err)
	}
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{ConfigTargets: ConfigTargets{Enabled: true}, Store: st, Dir: r.Dir}
	c := openCase(t, st, "failures", "configuration-bound", 1)
	report := `{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"/ro/config/` + long + `x"}`
	if err := r.keepFindings(ctx, store.LoopRun{Case: c.ID, CaseEpisode: c.Episode, ID: "fixture"}, nativeFinding(Result{Report: json.RawMessage(report)}), true, ""); err != nil {
		t.Fatal(err)
	}
	c, _ = st.CaseByID(ctx, c.ID)
	if _, config, err := r.caseTarget(ctx, c); err != nil || config != "" {
		t.Fatalf("a path over the bound: %q %v", config, err)
	}
}

// A configuration package outlives no episode of its case. Once its case closes, a package not yet finished is
// withdrawn, and the case that comes back builds fresh on the file its new investigation names, the same or another,
// with no review of the old episode; its counts of closed proposals and withdrawals start again, since a new episode is
// a new instance of the problem. Within an episode they carry (TestAConfigurationFileThatKeepsMovingStopsItsCase).
func TestAConfigurationPackageEndsWithItsCasesEpisode(t *testing.T) {
	ctx := context.Background()
	const otherTarget = ".config/agents/OTHER.md"
	for _, tc := range []struct {
		name   string
		target string
		pass   bool // a pass runs between the close and the comeback
		used   bool // the package used its proposals; else a review sent it back to its build
	}{
		{"revising, back on another file", otherTarget, true, false},
		{"revising, back on the same file", ruleTarget, false, false},
		{"its proposals used, back on the same file", ruleTarget, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, home, p := proposedConfigCase(t)
			write(t, filepath.Join(home, otherTarget), oldRule)
			if err := os.Chmod(filepath.Join(home, otherTarget), 0o640); err != nil {
				t.Fatal(err)
			}
			r.Ship.ConfigTargets.Allow = append(r.Ship.ConfigTargets.Allow, otherTarget)
			r.pass.manifest.Configuration.Files = append(r.pass.manifest.Configuration.Files,
				ConfigFile{Path: otherTarget, Root: ".config/agents", SHA256: hashOf([]byte(oldRule))})
			if tc.used {
				p.State, p.ClosedProposals, p.Withdrawals = Withdrawn, MaxClosedProposals, MaxClosedProposals+1
				if err := r.Ship.save(&p); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Medium: 1, Text: "a finding"}); err != nil {
					t.Fatal(err)
				}
				r.reviews(ctx, c.ID)
				if got, _, _ := r.Ship.Load(c.ID); got.State != Revising {
					t.Fatalf("the proposal after a medium finding: %+v", got)
				}
			}
			if _, err := r.Store.CloseSolvedBy(ctx, c.ID, "config:~/"+ruleTarget+"@"+strings.Repeat("ab", 32), "solved by hand", "test"); err != nil {
				t.Fatal(err)
			}
			if tc.pass {
				r.pick(ctx, "failures", c.ID, false)
				if got, _, _ := r.Ship.Load(c.ID); got.State != Withdrawn {
					t.Fatalf("the package once its case closed: %+v", got)
				}
			}
			later := store.Event{Source: store.Transcripts, SourceKey: "again@1", TS: time.Now().Add(time.Minute), Session: "again", Harness: "claude",
				Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`}
			if _, err := r.Store.Put(ctx, []store.Event{later}); err != nil {
				t.Fatal(err)
			}
			back, _, err := r.Store.ObserveCase(ctx, store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title,
				Evidence: []store.Evidence{{Ref: "ev:" + later.ID()}}}, "test")
			if err != nil || back.ID != c.ID || back.Episode == c.Episode {
				t.Fatalf("the comeback: %+v %v", back, err)
			}
			mech, next := "the rule says the old thing", "say the new thing"
			if back, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"repo": "", "config": tc.target, "episode": back.Episode})
			if err := r.Store.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
				t.Fatal(err)
			}
			if _, _, why, _ := r.pick(ctx, "failures", c.ID, false); strings.Contains(why, "used its proposals") {
				t.Fatalf("the case back in a new episode: %s", why)
			}
			run := store.LoopRun{ID: "fixture-build-back", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build-back"), CaseEpisode: back.Episode}
			fresh, err := r.buildPackage(ctx, back, run)
			if err != nil || fresh.State != Prepared || fresh.Target != tc.target || len(fresh.Reviews) != 0 || fresh.Episode != back.Episode ||
				fresh.ClosedProposals != 0 || fresh.Withdrawals != 0 {
				t.Fatalf("the build of the case back: %+v %v", fresh, err)
			}
			r.pick(ctx, "failures", c.ID, false)
			if got, _, _ := r.Ship.Load(c.ID); got.State != Prepared || got.Target != tc.target {
				t.Fatalf("the fresh package after a pass of its own episode: %+v", got)
			}
		})
	}
}

// The full review of a configuration proposal is kept under its proposals directory, which the filed note names: the
// case's log keeps a review cut to its line.
func TestAFiledConfigurationProposalKeepsItsWholeReview(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	text := "Checked every changed file. " + strings.Repeat("Each line reads as the rule meant. ", 120) + "marker-end-of-review"
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: text}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	review := r.Ship.proposalFile(p, ".review.md")
	if got, err := os.ReadFile(review); err != nil || !strings.Contains(string(got), "marker-end-of-review") {
		t.Fatalf("the proposal's review: %v %.80q", err, got)
	}
	if n, _ := noteByID(t, r.Store, configNoteID(c.ID)); !strings.Contains(n.Detail, review) {
		t.Fatalf("the note does not name the review: %s", n.Detail)
	}
}

// A filed note leads with what an agent runs, the apply line and the two closes, so the phone's view of a note
// (store.Note's 400 characters) starts with them; the rest is in the proposal's record.
func TestAFiledConfigurationNoteLeadsWithItsCommands(t *testing.T) {
	r, c, home, p := filedConfigCase(t)
	n, _ := noteByID(t, r.Store, configNoteID(c.ID))
	apply := "Apply: " + configApplyLine(home, p, r.Ship.proposalFile(p, ".patch")) + ". "
	solved := "Then: momentum case close " + c.ID + " --reason already-solved --fix '" + configFix(p) + "' --why ...."
	declined := "Or decline it: momentum case close " + c.ID + " --reason not-worth-it --why ...."
	if !strings.HasPrefix(n.Detail, apply+solved+" "+declined) || !strings.Contains(n.Detail, r.Ship.proposalFile(p, ".json")) {
		t.Fatalf("the filed note: %s", n.Detail)
	}
}

// shellLine quotes every word with a character outside the few a POSIX shell takes as they are, and the empty word.
func TestShellLineQuotesAnyWordOutsideItsSafeSet(t *testing.T) {
	got := shellLine([]string{"patch", "-p1", "--reject-file=-", "/h/a.b_c/d@e:f%g+h,i", "/h/a^b", "", "a\tb", "it's"})
	want := "patch -p1 --reject-file=- /h/a.b_c/d@e:f%g+h,i '/h/a^b' '' 'a\tb' 'it'\"'\"'s'"
	if got != want {
		t.Fatalf("shellLine: %s, want %s", got, want)
	}
}
