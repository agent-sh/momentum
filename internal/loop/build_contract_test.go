package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A build report's test command is its words, not a text: a prose description of the test, the words of one, or an
// empty command is refused with what to give instead.
func TestABuildReportsTestCommandIsItsWords(t *testing.T) {
	report := func(test any) []byte {
		raw, err := json.Marshal(map[string]any{"summary": "s", "test_command": test, "title": "t", "body": "b", "rollback": "r",
			"window": "7d", "lesson": "l", "signal": goodSignal})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	got, err := buildReport(report([]string{"go", "test", "./internal/x", "-run", "TestA|TestB"}))
	if err != nil || !slices.Equal(got.Test, []string{"go", "test", "./internal/x", "-run", "TestA|TestB"}) {
		t.Fatalf("a command as its words: %v %v", got.Test, err)
	}
	prose := "New file internal/x/names_test.go, all fixtures synthetic, HOME set to a temp dir."
	if _, err := buildReport(report(prose)); err == nil {
		t.Fatal("accepted a prose test command")
	}
	for _, words := range [][]string{{}, strings.Fields(prose), {"go test ./..."}, {"go", ""}, {"/usr/bin/go", "test"}} {
		_, err := buildReport(report(words))
		if err == nil {
			t.Fatalf("accepted the test command %q", words)
		}
		if len(words) > 0 && words[0] == "New" && !strings.Contains(err.Error(), `["go", "test", "./..."]`) {
			t.Fatalf("a refusal that does not say what to give: %v", err)
		}
	}
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(buildSchema), &schema); err != nil || schema.Properties["test_command"].Type != "array" {
		t.Fatalf("the schema's test command is no array: %+v %v", schema.Properties["test_command"], err)
	}
}

// The build's brief shows the command's form by example and says the host runs it before and after in the sandbox,
// and it reads the investigation's fix as a fix, never as the test.
func TestTheBuildBriefShowsTheTestCommandContract(t *testing.T) {
	system, err := stepSystem(StepBuild, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`["go", "test", "./internal/capacity", "-run"`, "runs it twice in the sandbox", "on BASE with only your test files (it must fail)",
		"on your change (it must pass)", "is the investigation's proposed fix, in prose", "it is not the test command", "refusal.md"} {
		if !strings.Contains(system, want) {
			t.Fatalf("the build brief lacks %q", want)
		}
	}
}

// A build is told the paths it may land before it starts: in the loop's own repository its two landable files, that a
// fix elsewhere is proposed for a hand landing, and what no change touches.
func TestABuildIsToldItsPathsUpFront(t *testing.T) {
	self := buildScope(SelfRepo)
	for _, want := range []string{"internal/supervise/rules.go", "internal/capacity/rules.go", "prefer a fix inside them", "land by hand",
		"internal/loop/", "internal/tracked/", ".github/"} {
		if !strings.Contains(self, want) {
			t.Fatalf("the loop's own scope lacks %q: %s", want, self)
		}
	}
	other := buildScope("agent-sh/fake")
	if !strings.Contains(other, ".github/") || strings.Contains(other, "rules.go") {
		t.Fatalf("another repository's scope: %s", other)
	}
}

// A check's program must be one the sandbox can start: on a toolchain directory of its PATH or in /usr/bin and /bin.
func TestACheckResolvesItsProgramOnTheSandboxPath(t *testing.T) {
	tools, empty := t.TempDir(), t.TempDir()
	script(t, tools, "pytest", "exit 0")
	saved := systemPath
	systemPath = []string{empty}
	t.Cleanup(func() { systemPath = saved })
	if err := (Sandbox{ReadOnly: []string{tools}, Path: []string{tools}}).resolve("pytest"); err != nil {
		t.Fatalf("a program on the toolchain path: %v", err)
	}
	if err := (Sandbox{Path: []string{tools}}).resolve("pytest"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a program in a directory the sandbox does not mount: %v", err)
	}
	if err := (Sandbox{Path: []string{empty}}).resolve("pytest"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not on the sandbox's PATH") {
		t.Fatalf("a program the sandbox lacks: %v", err)
	}
	write(t, filepath.Join(empty, "bats"), "not executable")
	if err := (Sandbox{}).resolve("bats"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a file that cannot run: %v", err)
	}
}

// PathScope refuses CI configuration anywhere and the loop's measurement and scanners in its own repository, lands
// its two allowed files, and returns every other path of its own repository for a hand landing.
func TestPathScopeSortsHandLandingPaths(t *testing.T) {
	self := Target{Repo: SelfRepo}
	if hand, err := PathScope(self, []string{"internal/capacity/rules.go", "internal/supervise/rules.go"}); err != nil || len(hand) != 0 {
		t.Fatalf("landable paths: %v %v", hand, err)
	}
	hand, err := PathScope(self, []string{"internal/capacity/rules.go", "internal/capacity/names.go", "internal/capacity/names_test.go", "SPEC.md"})
	if err != nil || !slices.Equal(hand, []string{"internal/capacity/names.go", "internal/capacity/names_test.go", "SPEC.md"}) {
		t.Fatalf("hand landing paths: %v %v", hand, err)
	}
	if err := CheckPaths(self, hand); !errors.Is(err, ErrRefused) {
		t.Fatalf("a hand landing path lands: %v", err)
	}
	for _, p := range []string{".github/workflows/ci.yml", "internal/tracked/check/main.go", "internal/loop/gate.go", "internal/measure/fisher.go",
		"sessions/claude/scan.go", "internal/source/transcripts.go", "internal/store/store.go", "internal/capacity/run.go", "internal/supervise/engine.go",
		"cmd/momentum/main.go", "go.mod"} {
		if _, err := PathScope(self, []string{"internal/capacity/names.go", p}); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s may be proposed: %v", p, err)
		}
	}
	if hand, err := PathScope(Target{Repo: "agent-sh/fake"}, []string{"internal/loop/gate.go", "go.mod"}); err != nil || len(hand) != 0 {
		t.Fatalf("another repository's paths: %v %v", hand, err)
	}
	if _, err := PathScope(Target{Repo: "agent-sh/fake"}, []string{".gitlab-ci.yml"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("CI configuration of another repository: %v", err)
	}
}

// The next build of a case reads why the host refused its last one, in its inputs, without the relay's accounting.
func TestANextBuildReadsWhyTheLastWasRefused(t *testing.T) {
	ctx := context.Background()
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	var err error
	c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	why := `build check: refused: the test command "New file names_test.go" holds '~', which only a shell reads: give one program and its arguments`
	if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-063759-0000aaaa", Case: c.ID, Family: c.Family, Step: StepBuild,
		Started: time.Now().Add(-time.Hour), Ended: time.Now().Add(-50 * time.Minute), Status: store.RunFailed, Failure: "build check",
		Summary: "store copy 30 days\n" + why + " (relay: 26 admitted, 0 refused, 26 charged, $1.049046)", CaseEpisode: c.Episode}); err != nil {
		t.Fatal(err)
	}
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := r.prepareRun(ctx, c, c.Family, StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readHostInput(HostDir(run.Dir), "input/refusal.md")
	if err != nil || !strings.Contains(string(got), "only a shell reads: give one program and its arguments") || strings.Contains(string(got), "relay:") ||
		strings.Contains(string(got), "store copy") {
		t.Fatalf("the next build's refusal input: %q %v", got, err)
	}
	prompt, err := readHostInput(HostDir(run.Dir), "prompt.json")
	if err != nil || !strings.Contains(string(prompt), "test_command") || !strings.Contains(string(prompt), "CI configuration") {
		t.Fatalf("the build prompt lacks the command or the paths: %q %v", prompt, err)
	}
	// a refusal of another episode, or a newer failure of the host's own, tells a build nothing
	later := c
	later.Episode++
	if text, err := r.lastBuildRefusal(ctx, later); err != nil || text != "" {
		t.Fatalf("another episode's refusal: %q %v", text, err)
	}
	if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-073759-0000bbbb", Case: c.ID, Family: c.Family, Step: StepBuild,
		Started: time.Now().Add(-time.Minute), Ended: time.Now(), Status: store.RunFailed, Failure: "build prepare",
		Summary: "build prepare: the clone failed", CaseEpisode: c.Episode}); err != nil {
		t.Fatal(err)
	}
	if text, err := r.lastBuildRefusal(ctx, c); err != nil || text != "" {
		t.Fatalf("a host failure read as the build's refusal: %q %v", text, err)
	}
}

// handFixture is a ship tool over a repository the gate names as the loop's own, with a change to a path the loop may
// not land and its test, checked.
func handFixture(t *testing.T) (*Ship, store.Case, repoFixture, Package) {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	f := newRepo(t, map[string]string{"internal/capacity/names.go": "package capacity\n\n// Named is the count.\nfunc Named() int { return 41 }\n"})
	s := shipFor(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return SelfRepo, nil }
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "internal/capacity/names.go"), "package capacity\n\n// Named is the count.\nfunc Named() int { return 42 }\n")
	write(t, filepath.Join(p.Work, "internal/capacity/names_test.go"),
		"package capacity\n\nimport \"testing\"\n\nfunc TestNamed(t *testing.T) {\n\tif Named() != 42 {\n\t\tt.Fatal(Named())\n\t}\n}\n")
	p, err = s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, nil, fixturePatch(t, s, c.ID))
	if err != nil || p.State != Checked || !slices.Equal(p.Hand, []string{"internal/capacity/names.go", "internal/capacity/names_test.go"}) {
		t.Fatalf("the check of a hand landing change: %+v %v", p, err)
	}
	return s, c, f, p
}

func handNote(t *testing.T, st *store.Store, caseID string) (store.Note, bool) {
	t.Helper()
	notes, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.ID == handNoteID(caseID) {
			return n, true
		}
	}
	return store.Note{}, false
}

// A change the loop may not land in its own repository is checked, proposed with the hand landing label and first
// line, reviewed, and never landed; a low problem note and the case's log say it waits for a person. Landed by hand,
// it is deployed from the default branch and its measurement starts; nothing is pushed.
func TestAHandLandingProposalIsReviewedAndNeverLanded(t *testing.T) {
	ctx := context.Background()
	s, c, f, _ := handFixture(t)
	pr := Proposal{Title: "Name the target", Body: "Keep a named target.", Rollback: "revert", Lesson: "Name targets.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	p, err := proposePatch(t, s, ctx, c.ID, pr)
	if err != nil || p.State != Proposed || p.PR == "" {
		t.Fatalf("propose: %+v %v", p, err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "label create "+HandLabel) || !strings.Contains(string(calls), "--label "+HandLabel) ||
		!strings.Contains(string(calls), "--body "+HandLine) {
		t.Fatalf("the pull request is not marked for a hand landing:\n%s", calls)
	}
	log, err := s.Store.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := s.Store.CaseByID(ctx, c.ID)
	if store.HandOf(cur, log) != p.PR {
		t.Fatal("the case's log does not say it waits for a hand landing")
	}
	if n, ok := handNote(t, s.Store, c.ID); !ok || n.Kind != store.NoteProblem || n.Importance != store.ImportanceLow || !n.Done.IsZero() {
		t.Fatalf("no low problem note for the hand landing: %+v", n)
	}
	// reviewed clean and posted, it is still a person's to land
	p.Reviews = []Review{{Round: 1, Head: p.Head, Text: "clean", Comment: "https://example.invalid/review/1", At: time.Now()}}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "hand landing") {
		t.Fatalf("landed a hand landing proposal: %v", err)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	done := r.reviews(ctx, "", c.Family)
	if len(done) != 1 || done[0].Step != "land" || !strings.Contains(done[0].Skipped, store.HandStage) {
		t.Fatalf("the reviews step: %+v", done)
	}
	before := originHead(t, f, "main")
	// a session lands it by fast-forward; the pull request reads merged at its head
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"2026-11-05T06:00:00Z","mergeCommit":{"oid":"`+p.Head+`"}}'`)
	if got, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || got.ID != "" {
		t.Fatalf("pick after the hand landing: %v %v", got.ID, err)
	}
	p, _, err = s.Load(c.ID)
	if err != nil || p.State != Landed || !slices.Equal(p.Landed, []string{p.LandingHead}) || p.LandingHead == before || p.Cleanup != "" {
		t.Fatalf("the hand landing was not recorded: %+v %v", p, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseDeployed {
		t.Fatalf("the case after its hand landing: %s", got.Status)
	}
	if n, _ := handNote(t, s.Store, c.ID); n.Done.IsZero() {
		t.Fatal("the hand landing note stands after the landing")
	}
	if pushes, _ := os.ReadFile(filepath.Join(f.dir, "pushes")); strings.Count(string(pushes), "refs/heads/main") != 1 {
		t.Fatalf("the loop pushed to the default branch:\n%s", pushes)
	}
}

// A hand landing proposal closed without merging reopens its case; its package is withdrawn, and a later build prepares a new one.
func TestAClosedHandLandingReopensItsCase(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := handFixture(t)
	p, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "t", Body: "b", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour})
	if err != nil || p.State != Proposed {
		t.Fatalf("propose: %+v %v", p, err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED","mergedAt":null,"mergeCommit":null}'`)
	if p, err = s.reconcileHand(ctx, c.ID); err != nil || p.State != Withdrawn {
		t.Fatalf("reconcile a closed proposal: %+v %v", p, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseOpen {
		t.Fatalf("the case after its proposal closed: %s", got.Status)
	}
	if p, had, err := s.Load(c.ID); !had || err != nil || p.State != Withdrawn || p.PR != "" || p.ClosedProposals != 1 {
		t.Fatalf("the withdrawn package: %+v %v", p, err)
	}
	if n, _ := handNote(t, s.Store, c.ID); n.Done.IsZero() {
		t.Fatal("the hand landing note stands after the close")
	}
}

// Even a hand landing proposal never touches the loop's measurement or scanners, nor CI configuration.
func TestAHandLandingNeverTouchesMeasurementOrCI(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"internal/loop/gate.go", ".github/workflows/ci.yml", "internal/tracked/check.go"} {
		s, c, _, p := handFixture(t)
		write(t, filepath.Join(p.Work, path), "package x\n")
		want := "measurement or its event scanners"
		if strings.HasPrefix(path, ".github/") {
			want = "is CI configuration"
		}
		if _, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, nil, fixturePatch(t, s, c.ID)); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s was checked: %v", path, err)
		}
	}
}

// A change a person landed by hand is not rolled back by the loop, live or not: the rollback is held with a note that
// says to revert it through a pull request.
func TestAHandLandedChangeIsNotRolledBackByTheLoop(t *testing.T) {
	m := measuredCase(t)
	p, _, err := m.r.Ship.Load(m.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.Hand = []string{"internal/capacity/names.go"}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.r.Ship.Live = true
	m.measure(t, Options{})
	effect := m.clock
	hours(t, m.st, "a", effect.Add(time.Minute), 23*time.Hour, 100, 6)
	m.clock = effect.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "landed by hand") {
		t.Fatalf("a hand landed change without effect: %+v", got)
	}
	if ns := m.notes(t); len(ns) != 1 || ns[0].Kind != store.NoteDecided || !strings.Contains(ns[0].Detail, "revert it through a pull request") {
		t.Fatalf("the held rollback's note %+v", ns)
	}
	if m.theCase(t).Status != store.CaseMeasuring {
		t.Fatal("a held rollback moved the case")
	}
}

// proposedHand is a hand landing proposal of handFixture's change.
func proposedHand(t *testing.T) (*Ship, store.Case, repoFixture, Package) {
	t.Helper()
	s, c, f, _ := handFixture(t)
	p, err := proposePatch(t, s, context.Background(), c.ID, Proposal{Title: "t", Body: "b", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour})
	if err != nil || p.State != Proposed || p.PR == "" {
		t.Fatalf("propose: %+v %v", p, err)
	}
	return s, c, f, p
}

// A hand landing recorded as landed whose deployment did not finish is deployed at the next selection.
func TestAHandLandingThatStoppedBeforeItsDeploymentIsDeployed(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	p.State, p.Landed, p.LandingHead, p.LandedAt = Landed, []string{p.Head}, p.Head, time.Now()
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	if _, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseDeployed {
		t.Fatalf("the case after a stopped hand landing: %s", got.Status)
	}
	if n, _ := handNote(t, s.Store, c.ID); n.Done.IsZero() {
		t.Fatal("the hand landing note stands after its deployment")
	}
}

// A merged pull request whose merge commit the default branch does not hold yet lands nothing; the next read retries.
func TestAMergeNotOnTheDefaultBranchLandsNothing(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"2026-11-05T06:00:00Z","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
	if _, err := s.reconcileHand(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not on main yet") {
		t.Fatalf("a merge commit off the default branch: %v", err)
	}
	if got, _, _ := s.Load(c.ID); got.State != Proposed || len(got.Landed) != 0 {
		t.Fatalf("the package after an early read: %+v", got)
	}
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Landed {
		t.Fatalf("the retry: %+v %v", got, err)
	}
}

// A dry pass reads no pull request.
func TestADryPassReadsNoHandLanding(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	calls := filepath.Join(t.TempDir(), "calls")
	s.GH = script(t, t.TempDir(), "gh", `echo "$@" >> `+calls+`; echo '{"state":"CLOSED"}'`)
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	if _, _, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Fatalf("a dry pass called gh: %s", b)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("a dry pass moved the case: %s", got.Status)
	}
}

// A withdrawal that stopped before the case reopened is finished at the next read, once.
func TestAWithdrawalThatStoppedIsFinished(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	p.State, p.ClosedProposals = Withdrawn, 1
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.PR != "" {
		t.Fatalf("the resumed withdrawal: %+v %v", got, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseOpen {
		t.Fatalf("the case after the resumed withdrawal: %s", got.Status)
	}
}

// Closed hand landing proposals count toward the case's two: after the second, the case waits for a person.
func TestTwoClosedHandLandingsStopTheCase(t *testing.T) {
	ctx := context.Background()
	s, c, f, _ := proposedHand(t)
	s.GH = script(t, t.TempDir(), "gh", `if [ "$1 $2" = "pr view" ]; then echo '{"state":"CLOSED"}'; else exec `+f.gh+` "$@"; fi`)
	if p, err := s.reconcileHand(ctx, c.ID); err != nil || p.State != Withdrawn || p.ClosedProposals != 1 {
		t.Fatalf("the first close: %+v %v", p, err)
	}
	mech, next := "Named is off by one", "return 42"
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil || p.ClosedProposals != 1 {
		t.Fatalf("a new package after the close: %+v %v", p, err)
	}
	write(t, filepath.Join(p.Work, "internal/capacity/names.go"), "package capacity\n\n// Named is the count.\nfunc Named() int { return 42 }\n")
	write(t, filepath.Join(p.Work, "internal/capacity/names_test.go"),
		"package capacity\n\nimport \"testing\"\n\nfunc TestNamed(t *testing.T) {\n\tif Named() != 42 {\n\t\tt.Fatal(Named())\n\t}\n}\n")
	if _, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, nil, fixturePatch(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	if p, err = proposePatch(t, s, ctx, c.ID, Proposal{Title: "t2", Body: "b2", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if p, err = s.reconcileHand(ctx, c.ID); err != nil || p.State != Withdrawn || p.ClosedProposals != 2 {
		t.Fatalf("the second close: %+v %v", p, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case after its second closed proposal: %s", got.Status)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	got, _, why, err := r.pick(ctx, c.Family, c.ID, false)
	if err != nil || got.ID != "" || !strings.Contains(why, "used its proposals") {
		t.Fatalf("the case was picked after two closed proposals: %q %q %v", got.ID, why, err)
	}
}

// Land rechecks a package's changed paths: a package that lost its hand landing mark still does not land them.
func TestLandRechecksTheChangedPaths(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	p.Hand = nil
	p.Reviews = []Review{{Round: 1, Head: p.Head, Text: "clean", Comment: "https://example.invalid/review/1", At: time.Now()}}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "never its to land") {
		t.Fatalf("landed a path outside the allow-list: %v", err)
	}
}

// The owner's rollback of a change landed by hand is refused: it is reverted through a pull request.
func TestARollbackOfAHandLandingIsRefused(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	p.State, p.Landed, p.LandedAt = Landed, []string{p.Head}, time.Now()
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollBack(ctx, c.ID, "no effect"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "revert it through a pull request") {
		t.Fatalf("rolled back a hand landing: %v", err)
	}
}
