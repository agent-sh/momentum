package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// comeBack closes a case as solved, brings it back on new evidence in a new episode and moves it to testing, its
// investigation naming repo or config: the case of the new episode.
func comeBack(t *testing.T, st *store.Store, c store.Case, repo, config string) store.Case {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CloseCase(ctx, c.ID, store.Solved, "", "solved by hand", "test"); err != nil {
		t.Fatal(err)
	}
	later := store.Event{Source: store.Transcripts, SourceKey: "again@" + c.ID, TS: time.Now().Add(time.Minute), Session: "again", Harness: "claude",
		Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`}
	if _, err := st.Put(ctx, []store.Event{later}); err != nil {
		t.Fatal(err)
	}
	back, _, err := st.ObserveCase(ctx, store.Finding{Family: c.Family, Signature: c.Signatures[0], Title: c.Title,
		Evidence: []store.Evidence{{Ref: "ev:" + later.ID()}}}, "test")
	if err != nil || back.ID != c.ID || back.Episode == c.Episode {
		t.Fatalf("the comeback: %+v %v", back, err)
	}
	mech, next := "the rule says the old thing", "say the new thing"
	if back, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"repo": repo, "config": config, "episode": back.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	return back
}

// A repository package an earlier episode left unfinished gives way to the configuration package the case's new
// episode names: the case builds fresh on its file, with no review of the old episode.
func TestARepositoryPackageOfAnEarlierEpisodeGivesWayToAConfigurationOne(t *testing.T) {
	ctx := context.Background()
	r, c, _, p := proposedConfigCase(t)
	p.Kind, p.State, p.Target, p.TargetBase, p.Repo, p.Dir = KindRepo, Revising, "", "", "main", t.TempDir()
	p.Reviews = []Review{{Round: 1, Head: p.Head, Medium: 1, Text: "a finding", Comment: "case log", At: time.Now()}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	back := comeBack(t, r.Store, c, "", ruleTarget)
	r.pick(ctx, "failures", c.ID, false)
	run := store.LoopRun{ID: "fixture-build-back", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build-back"), CaseEpisode: back.Episode}
	fresh, err := r.buildPackage(ctx, back, run)
	if err != nil || fresh.Kind != KindConfig || fresh.State != Prepared || fresh.Target != ruleTarget || len(fresh.Reviews) != 0 || fresh.Episode != back.Episode {
		t.Fatalf("the build of the case back on a configuration file: %+v %v", fresh, err)
	}
}

// A configuration package gives way to the repository package its case now names, and the repository package records
// its case's episode.
func TestAConfigurationPackageGivesWayToARepositoryOne(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	stale := Package{Case: c.ID, Kind: KindConfig, Target: ruleTarget, State: Prepared, Episode: c.Episode, Created: time.Now()}
	if err := r.Ship.save(&stale); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatalf("the build after the case named a repository: %v", err)
	}
	if p, _, _ := r.Ship.Load(c.ID); p.Kind != KindRepo || p.Dir != f.main || p.Episode != c.Episode {
		t.Fatalf("the package: %+v", p)
	}
}

// A repository package an earlier episode left unfinished gives way to the other repository the case's new episode
// names, and the new package records that episode.
func TestARepositoryPackageGivesWayToAnotherRepository(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := r.Ship.Load(c.ID); p.Kind != KindRepo || p.Dir != f.main || p.State != Prepared {
		t.Fatalf("the first package: %+v", p)
	}
	other := filepath.Join(f.dir, "other")
	run(t, f.dir, "git", "clone", "-q", f.origin, other)
	r.Inputs.ReadDirs = append(r.Inputs.ReadDirs, other)
	back := comeBack(t, r.Store, c, "other", "")
	newPass(t, r)
	r.pick(ctx, c.Family, c.ID, false)
	if _, err := r.prepareRun(ctx, back, back.Family, StepBuild); err != nil {
		t.Fatalf("the build of the case back on another repository: %v", err)
	}
	if p, _, _ := r.Ship.Load(c.ID); p.Kind != KindRepo || p.Dir != other || p.State != Prepared || p.Episode != back.Episode {
		t.Fatalf("the package of the case back: %+v", p)
	}
}

// A merged case's proposal is withdrawn with no line in a case's log: the store writes a merged case's lines to the
// case it went into, whose own proposal they would end.
func TestAMergedCasesWithdrawalWritesNothingToItsSurvivor(t *testing.T) {
	ctx := context.Background()
	r, a, _, _ := filedConfigCase(t)
	b := otherTestingCase(t, r.Store)
	if err := r.Store.CaseNote(ctx, b.ID, store.ActionConfigProposed, store.ConfigDetail(".config/agents/B.md", b.Episode), "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.CloseCase(ctx, a.ID, store.Duplicate, b.ID, "same root", "test"); err != nil {
		t.Fatal(err)
	}
	r.pick(ctx, "failures", a.ID, false)
	if p, _, _ := r.Ship.Load(a.ID); p.State != Withdrawn {
		t.Fatalf("the merged case's proposal: %+v", p)
	}
	b, _ = r.Store.CaseByID(ctx, b.ID)
	log, _ := r.Store.CaseLog(ctx, b.ID)
	if got := store.ConfigOf(b, log); got != ".config/agents/B.md" {
		t.Fatalf("the survivor's stage after the merged case's withdrawal: %q %+v", got, log)
	}
}

// A component-health case is the loop's own: its investigation names no configuration file.
func TestAHealthCaseNamesNoConfigurationFile(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{ConfigTargets: ConfigTargets{Enabled: true}, Store: st, Dir: r.Dir}
	c := openCase(t, st, store.HealthFamily, store.HealthFamily+"/v1:loop:investigate:turns", 1)
	if strings.Contains(r.investigationSchemaFor(c), `"config"`) {
		t.Fatal("a health case's investigation is offered a configuration file")
	}
	report := `{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"/ro/config/` + ruleTarget + `"}`
	if err := r.keepFindings(ctx, store.LoopRun{Case: c.ID, CaseEpisode: c.Episode, ID: "fixture"}, nativeFinding(Result{Report: json.RawMessage(report)}), true, ""); err != nil {
		t.Fatal(err)
	}
	c, _ = st.CaseByID(ctx, c.ID)
	if _, config, err := r.caseTarget(ctx, c); err != nil || config != "" {
		t.Fatalf("a health case's configuration target: %q %v", config, err)
	}
}

// The fix a close must name is a filed proposal's only while the proposal waits: once its case came back in a new
// episode it waits on nobody, even before a pass withdraws it.
func TestAFiledFixIsNamedOnlyWhileItsProposalWaits(t *testing.T) {
	r, c, _, _ := filedConfigCase(t)
	back := comeBack(t, r.Store, c, "", ruleTarget)
	if fix, ok, err := FiledConfigFix(r.Ship.Dir, back); err != nil || ok {
		t.Fatalf("the fix of a proposal of an earlier episode: %q %v %v", fix, ok, err)
	}
}

// A proposal whose file is gone is not reviewed: a review sending it back to its build would leave it waiting on its
// file with no note.
func TestAProposalWhoseFileIsGoneIsNotReviewed(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	if err := os.Rename(filepath.Join(home, ruleTarget), filepath.Join(home, ruleTarget+".away")); err != nil {
		t.Fatal(err)
	}
	r.pick(ctx, "failures", c.ID, false)
	if got, _, _ := r.Ship.Load(c.ID); got.GoneSince.IsZero() {
		t.Fatalf("the proposal over a gone file: %+v", got)
	}
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Medium: 1, Text: "a finding"}); err != nil {
		t.Fatal(err)
	}
	done := r.reviews(ctx, c.ID)
	if got, _, _ := r.Ship.Load(c.ID); got.State != Proposed || len(done) != 1 || !strings.Contains(done[0].Skipped, "gone") {
		t.Fatalf("the review of a proposal over a gone file: %+v %+v", got, done)
	}
}

// A pass narrowed to one case settles no other case's configuration package.
func TestANarrowedPassSettlesNoOtherCasesPackage(t *testing.T) {
	ctx := context.Background()
	r, a, _, _ := proposedConfigCase(t)
	b := otherTestingCase(t, r.Store)
	other := Package{Case: b.ID, Kind: KindConfig, Target: ruleTarget, State: Prepared, Episode: b.Episode, Created: time.Now()}
	if err := r.Ship.save(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.CloseCase(ctx, b.ID, store.CloseReason("not-worth-it"), "", "declined", "test"); err != nil {
		t.Fatal(err)
	}
	r.pick(ctx, "failures", a.ID, false)
	if got, _, _ := r.Ship.Load(b.ID); got.State != Prepared {
		t.Fatalf("another case's package after a pass narrowed to %s: %+v", a.ID, got)
	}
	r.pick(ctx, "failures", "", false)
	if got, _, _ := r.Ship.Load(b.ID); got.State != Withdrawn {
		t.Fatalf("a closed case's package after a whole pass: %+v", got)
	}
}

// A package built before and not yet proposed is held, not built, once its file holds text the scrubber would mask:
// the run's copy of it is the scrubbed text, whose hash says nothing of the file.
func TestAMaskedFileHoldsTheBuildOfItsPackage(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: p.Head, Medium: 1, Text: "a finding"}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	if got, _, _ := r.Ship.Load(c.ID); got.State != Revising {
		t.Fatalf("the proposal after a medium finding: %+v", got)
	}
	secret := oldRule + "password=marker-7f3a\n"
	agentWrites(t, home, secret)
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents",
		SHA256: hashOf([]byte(strings.Replace(secret, "password=marker-7f3a", "password=[masked]", 1)))}}
	run := store.LoopRun{ID: "fixture-build-2", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build-2"), CaseEpisode: c.Episode}
	if _, err := r.buildPackage(ctx, c, run); !held(err) || !strings.Contains(err.Error(), "scrubber would mask") {
		t.Fatalf("the build over a masked file: %v", err)
	}
}

// A configuration case through two passes of the runner, with a real build run and a real review run in the
// sandbox: the first builds, checks, proposes and reviews it clean; the second files it, and says it waits for its
// filing rather than its review. Nothing writes the file.
func TestAConfigurationCaseThroughTheRunnersPasses(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	st := testStore(t)
	home := fakeHome(t)
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	report, _ := json.Marshal(BuildReport{Summary: "The rule is corrected.", Test: []string{"python3", "tests/test_rules.py"}, Title: "Say the new rule",
		Body: "Fix one cause with a discriminating test.", Rollback: "Apply the reverse patch.", Window: "7d", Lesson: "Rules say the new rule.", Signal: configProposal.Signal})
	result := `{"type":"result","subtype":"success","structured_output":` + string(report) + `}`
	harness := script(t, t.TempDir(), "harness", `schema=""
while [ "$#" -gt 0 ]; do
 if [ "$1" = "--json-schema" ]; then schema="$2"; shift; fi
 shift
done
case "$schema" in *findings*)
 `+fixtureResult(`{"type":"result","subtype":"success","structured_output":{"summary":"Checked the rule and its test.","reviewed_files":["`+ruleTarget+`","tests/test_rules.py"],"findings":[]}}`)+`
 ;;
*)
 sed -i 's/old rule/new rule/' /work/`+ruleTarget+`
 mkdir -p /work/tests
 cat > /work/tests/test_rules.py <<'TEST'
`+ruleTest+`
TEST
 `+fixtureResult(result)+`
 ;;
esac`)
	r := runnerFor(t, st, []string{harness})
	r.Inputs.Home = home
	r.Ship = configShip(t, st, home)
	r.Ship.Dir = r.Dir
	r.Toolchains = Toolchains{ReadOnly: r.Ship.Sandbox.ReadOnly, Path: r.Ship.Sandbox.Path}
	done, err := r.Once(ctx, Options{Families: []string{c.Family}})
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]Done{}
	for _, d := range done {
		steps[d.Step] = d
	}
	if steps[StepBuild].Run.Status != store.RunOK || steps[StepReview].Run.Status != store.RunOK {
		t.Fatalf("the first pass: %+v", done)
	}
	p, _, _ := r.Ship.Load(c.ID)
	if p.State != Proposed || p.Check == nil || len(p.Reviews) != 1 || !p.Clean() || !p.Filed.IsZero() {
		t.Fatalf("the package after the first pass: %+v", p)
	}
	if done, err = r.Once(ctx, Options{Families: []string{c.Family}}); err != nil {
		t.Fatal(err)
	}
	said, filed := "", false
	for _, d := range done {
		said += d.Skipped + "\n"
		filed = filed || d.Step == "file" && d.Skipped == ""
	}
	if !filed || !strings.Contains(said, "waits for its filing") || strings.Contains(said, "waits for its review") {
		t.Fatalf("the second pass: %s %+v", said, done)
	}
	if p, _, _ = r.Ship.Load(c.ID); p.Filed.IsZero() {
		t.Fatalf("the package after the second pass: %+v", p)
	}
	if n, ok := noteByID(t, st, configNoteID(c.ID)); !ok || !n.Done.IsZero() {
		t.Fatalf("the filed proposal's note: %+v", n)
	}
	if got := readFile(t, filepath.Join(home, ruleTarget)); got != oldRule {
		t.Fatalf("the file after two passes: %q", got)
	}
}
