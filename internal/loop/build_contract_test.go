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
	if _, err := buildReport(report(prose)); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), `as a list, such as ["go", "test", "./..."]`) {
		t.Fatalf("a prose test command: %v", err)
	}
	if _, err := buildReport(report(map[string]string{"run": "go test"})); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no list of words") {
		t.Fatalf("an object as the test command: %v", err)
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

// A review run is given the paths its build was given. A fix in a path no change may touch, or outside the repository,
// is a low finding for a person, which names no host path; it lowers no other finding, and a change not worth landing
// without it is a high one. In the loop's own repository only, a fix a hand landing may make is the build's to make.
func TestAReviewIsToldTheBuildsPaths(t *testing.T) {
	for _, repo := range []string{"agent-sh/fake", SelfRepo} {
		t.Run(repo, func(t *testing.T) {
			ctx := context.Background()
			r, c, p := reviewFixture(t)
			p.Repo = repo
			if err := r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			if err := r.prepareInputs(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := r.prepareRun(ctx, c, c.Family, StepReview)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := readHostInput(HostDir(run.Dir), "prompt.json")
			if err != nil {
				t.Fatal(err)
			}
			var prompt string
			if err := json.Unmarshal(raw, &prompt); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"reviewed_files", buildScope(p.Repo), "follow-up for a person: file it as a low finding",
				"or anything outside this repository", "names that path in this repository, or the thing outside it by its kind", "no part of the finding names a host path", "The follow-up lowers no finding on what this change does", "file a high finding"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("the prepared review prompt for %q lacks %q: %s", p.Repo, want, prompt)
				}
			}
			if hand := strings.Contains(prompt, "a hand landing may change is the build's to make, not a follow-up"); hand != strings.EqualFold(p.Repo, SelfRepo) {
				t.Fatalf("the prompt for %q names hand landing paths: %v", p.Repo, hand)
			}
		})
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
	outside := t.TempDir()
	script(t, outside, "uv", "exit 0")
	if err := os.Symlink(filepath.Join(outside, "uv"), filepath.Join(tools, "uv")); err != nil {
		t.Fatal(err)
	}
	if err := (Sandbox{ReadOnly: []string{tools}, Path: []string{tools}}).resolve("uv"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a link out of the sandbox's mounts: %v", err)
	}
	// inside a mounted root, an absolute link names the host path, which the sandbox mounts at /opt/toolchain-N
	script(t, tools, "real-npm", "exit 0")
	if err := os.Symlink(filepath.Join(tools, "real-npm"), filepath.Join(tools, "npm")); err != nil {
		t.Fatal(err)
	}
	if err := (Sandbox{ReadOnly: []string{tools}, Path: []string{tools}}).resolve("npm"); !errors.Is(err, ErrRefused) {
		t.Fatalf("an absolute link inside a toolchain root: %v", err)
	}
	if err := os.Symlink("real-npm", filepath.Join(tools, "npx")); err != nil {
		t.Fatal(err)
	}
	if err := (Sandbox{ReadOnly: []string{tools}, Path: []string{tools}}).resolve("npx"); err != nil {
		t.Fatalf("a relative link inside a toolchain root: %v", err)
	}
	if err := os.Symlink("/usr/bin/env", filepath.Join(tools, "node")); err != nil {
		t.Fatal(err)
	}
	if err := (Sandbox{ReadOnly: []string{tools}, Path: []string{tools}}).resolve("node"); err != nil {
		t.Fatalf("an absolute link into /usr: %v", err)
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
	if hand, err := PathScope(self, []string{"internal/supervise/rules_test.go"}); err != nil || len(hand) != 1 {
		t.Fatalf("the supervision rules' test: %v %v", hand, err)
	}
	for _, p := range []string{"go.work", "go.work.sum", "vendor/modules.txt", "Internal/Loop/gate.go", "GO.MOD", ".GitHub/workflows/x.yml"} {
		if _, err := PathScope(self, []string{p}); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s may be proposed: %v", p, err)
		}
	}
	// a protected file is that file: a neighbour that only begins with its name is a hand landing path
	if hand, err := PathScope(self, []string{"go.mod.orig", "internal/capacity/run.go.bak"}); err != nil || len(hand) != 2 {
		t.Fatalf("files that only begin with a protected name: %v %v", hand, err)
	}
	if _, err := PathScope(self, []string{"Internal/Capacity/RUN.go"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a protected file in other case: %v", err)
	}
	for _, p := range []string{"Jenkinsfile.release", ".gitlab-ci.yml.example"} {
		if _, err := PathScope(Target{Repo: "agent-sh/fake"}, []string{p}); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s is CI configuration a host may run: %v", p, err)
		}
	}
	if _, err := PathScope(self, []string{"internal/supervise/new.go"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a new supervision file may be proposed: %v", err)
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
	if _, err := r.Ship.Prepare(ctx, c.ID, f.main); err != nil { // the package the refused build had
		t.Fatal(err)
	}
	why := `build check: refused: the test command "New file names_test.go" holds '~', which only a shell reads: give one program and its arguments`
	if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-063759-0000aaaa", Case: c.ID, Family: c.Family, Step: StepBuild,
		Started: time.Now(), Ended: time.Now(), Status: store.RunFailed, Failure: "build check",
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
		Started: time.Now(), Ended: time.Now(), Status: store.RunFailed, Failure: "build prepare",
		Summary: "build prepare: the clone failed", CaseEpisode: c.Episode}); err != nil {
		t.Fatal(err)
	}
	// a newer failure of a host stage is read past: the build's own last refusal still reaches the next build
	if text, err := r.lastBuildRefusal(ctx, c); err != nil || !strings.Contains(text, "only a shell reads") || strings.Contains(text, "clone failed") {
		t.Fatalf("a host failure hid or replaced the build's refusal: %q %v", text, err)
	}
	if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-083759-0000cccc", Case: c.ID, Family: c.Family, Step: StepBuild,
		Started: time.Now().Add(time.Millisecond), Ended: time.Now(), Status: store.RunFailed, Failure: "build propose",
		Summary: "build propose: gh: network down", CaseEpisode: c.Episode}); err != nil {
		t.Fatal(err)
	}
	if text, err := r.lastBuildRefusal(ctx, c); err != nil || text != "" {
		t.Fatalf("a gh failure at the proposal read as the build's refusal: %q %v", text, err)
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
	if log, _ := s.Store.CaseLog(ctx, c.ID); slices.ContainsFunc(log, func(e store.CaseEntry) bool { return e.Action == "landed head differs" }) {
		t.Fatal("a pull request that named no head was noted as a different head")
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
	other := strings.Repeat("a", 40)
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+time.Now().UTC().Format(time.RFC3339)+`","headRefOid":"`+other+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Landed {
		t.Fatalf("the retry: %+v %v", got, err)
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	if !slices.ContainsFunc(log, func(e store.CaseEntry) bool {
		return e.Action == "landed head differs" && strings.Contains(e.Detail, other)
	}) {
		t.Fatal("a merged head other than the checked one was not noted")
	}
}

// A request closed after a person pushed its change to the default branch by hand (a rebased head) is a landing, not
// a withdrawal: the default branch holds the change's files as proposed.
func TestAClosedRequestWhoseChangeIsOnMainIsALanding(t *testing.T) {
	ctx := context.Background()
	s, c, f, p := proposedHand(t)
	pushed := pushOther(t, f, "main", map[string]string{
		"internal/capacity/names.go":      "package capacity\n\n// Named is the count.\nfunc Named() int { return 42 }\n",
		"internal/capacity/names_test.go": "package capacity\n\nimport \"testing\"\n\nfunc TestNamed(t *testing.T) {\n\tif Named() != 42 {\n\t\tt.Fatal(Named())\n\t}\n}\n",
	})
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED"}'`)
	got, err := s.reconcileHand(ctx, c.ID)
	if err != nil || got.State != Landed || !slices.Equal(got.Landed, []string{pushed}) || got.ClosedProposals != 0 {
		t.Fatalf("a closed request whose change is on main: %+v %v (pushed %s, head %s)", got, err, pushed, p.Head)
	}
	if cur, _ := s.Store.CaseByID(ctx, c.ID); cur.Status != store.CaseDeployed {
		t.Fatalf("the case: %s", cur.Status)
	}
}

// A hand landing more than a window after its plan is counted again as of its landing.
func TestALateHandLandingIsCountedAgain(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	p.Plan.At = p.Plan.At.Add(-15 * 24 * time.Hour)
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+time.Now().UTC().Format(time.RFC3339)+`","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
	got, err := s.reconcileHand(ctx, c.ID)
	if err != nil || got.State != Landed || got.Plan == nil || got.Plan.At.Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("a late hand landing's plan: %+v %v", got.Plan, err)
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	if !slices.ContainsFunc(log, func(e store.CaseEntry) bool {
		return e.Action == "plan counted again"
	}) {
		t.Fatal("the late landing's plan was not counted again")
	}
}

// A revision of a hand landing proposal stays one; a revision that stays one updates its request with the label and
// the first line again.
func TestAHandLandingRevisionStaysOne(t *testing.T) {
	ctx := context.Background()
	s, c, f, p := proposedHand(t)
	p.Reviews = []Review{{Round: 1, Head: p.Head, Medium: 1, Text: "fix the comment", Comment: "https://example.invalid/review/1", At: time.Now(),
		Findings: []Finding{{Severity: "medium", Where: "names.go", Problem: "p", Fix: "f"}}}}
	p.State = Revising
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "internal/capacity/names.go"), "package capacity\n\n// Named is the count, 42.\nfunc Named() int { return 42 }\n")
	if _, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, nil, fixturePatch(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "t", Body: "b2", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "pr edit") || !strings.Contains(string(calls), "--add-label "+HandLabel) {
		t.Fatalf("the revision did not keep its request marked:\n%s", calls)
	}
	// a revision inside the landable paths would make the hand landing a landing of the loop's: its check is refused
	// and the package keeps its hand paths
	p, _, _ = s.Load(c.ID)
	write(t, filepath.Join(p.Work, "internal/capacity/names.go"), "package capacity\n\n// Named is the count.\nfunc Named() int { return 41 }\n")
	if err := os.Remove(filepath.Join(p.Work, "internal/capacity/names_test.go")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "internal/capacity/rules.go"), "package capacity\n")
	in := fixturePatch(t, s, c.ID)
	p.State = Revising
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckArgs(ctx, c.ID, []string{"go", "test", "./..."}, []string{"internal/capacity/rules.go"}, in); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "stays one") {
		t.Fatalf("a hand landing checked into a landable change: %v", err)
	}
	if kept, _, _ := s.Load(c.ID); len(kept.Hand) == 0 {
		t.Fatal("a refused revision dropped the package's hand paths")
	}
	// and so is a proposal of such a tree
	tree, paths, err := s.applyPatch(ctx, p, []Patch{in})
	if err != nil || !slices.Equal(paths, []string{"internal/capacity/rules.go"}) {
		t.Fatalf("the landable revision's patch: %v %v", paths, err)
	}
	p.State, p.Check = Checked, &Check{Test: []string{"go", "test", "./..."}, Tree: tree, At: time.Now()}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, c.ID, Proposal{Title: "t", Body: "b3", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour}, in); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "stays one") {
		t.Fatalf("a hand landing revised into a landable change: %v", err)
	}
}

// A case that leaves testing while its hand landing request is open ends the record and resolves the note.
func TestAHandLandingRecordEndsWhenItsCaseLeavesTesting(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "owner"); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	cs, err := s.Store.Cases(ctx, store.CaseFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.releaseHolds(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if n, _ := handNote(t, s.Store, c.ID); n.Done.IsZero() {
		t.Fatal("the hand landing note stands after its case left testing")
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	as, _ := s.Store.CaseByID(ctx, c.ID)
	as.Status = store.CaseTesting
	if store.HandOf(as, log) != "" {
		t.Fatal("the hand landing record stands after its case left testing")
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
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	notes, err := s.Store.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(notes, func(n store.Note) bool {
		return n.ID == waitingPrefix+store.IDPart(c.ID) && strings.Contains(n.Detail, "Take the change over by hand")
	}) {
		t.Fatalf("no note asks a person to take the change over: %+v", notes)
	}
}

// A withdrawal that stopped after its case reopened does not reopen it again once the case is back in testing.
func TestAWithdrawalAfterItsReopenIsNotRepeated(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	p.State, p.ClosedProposals = Withdrawn, 1
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.CaseNote(ctx, c.ID, store.ActionHandEnded, "closed without merging: "+p.PR, "loop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseOpen}, "loop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting}, "investigation"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.PR != "" {
		t.Fatalf("the finished withdrawal: %+v %v", got, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case was reopened again: %s", got.Status)
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
	p.State, p.LandingHead = Landing, p.Head
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "never its to land") {
		t.Fatalf("resumed the landing of a path outside the allow-list: %v", err)
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

// A late hand landing whose recount waits on a reader holds the landing; one whose recount is refused lands and waits
// to be measured by hand.
func TestALateHandLandingsRecountWaitsOrLeavesItToAPerson(t *testing.T) {
	ctx := context.Background()
	t.Run("waits", func(t *testing.T) {
		s, c, _, p := proposedHand(t)
		run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
		p.Plan.At = p.Plan.At.Add(-15 * 24 * time.Hour)
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339) // more than a window past what the store has read
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+later+`","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
		if _, err := s.reconcileHand(ctx, c.ID); !held(err) {
			t.Fatalf("a recount that waits: %v", err)
		}
		if got, _, _ := s.Load(c.ID); got.State != Proposed {
			t.Fatalf("a waiting recount landed: %s", got.State)
		}
	})
	t.Run("refused", func(t *testing.T) {
		s, c, _, p := proposedHand(t)
		run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
		p.Plan.At = p.Plan.At.Add(-15 * 24 * time.Hour)
		p.Plan.Signal.Match = []string{"nothing counts this text"}
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+time.Now().UTC().Format(time.RFC3339)+`","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
		got, err := s.reconcileHand(ctx, c.ID)
		if err != nil || got.State != Landed || got.Plan.Units != "" {
			t.Fatalf("a refused recount: %+v %v", got.Plan, err)
		}
		log, _ := s.Store.CaseLog(ctx, c.ID)
		if !slices.ContainsFunc(log, func(e store.CaseEntry) bool { return e.Action == "measured by hand" }) {
			t.Fatal("no log line says the change is measured by hand")
		}
	})
}

// A case that comes back to testing reads its open request again and writes its record and note again; a request that
// cannot be read holds its review for the pass, with a note.
func TestAReturnedCaseReadsItsHandLandingAgain(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "owner"); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	if _, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting}, "investigation"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil {
		t.Fatal(err)
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	cur, _ := s.Store.CaseByID(ctx, c.ID)
	if store.HandOf(cur, log) != p.PR {
		t.Fatal("the returned case does not wait on its request again")
	}
	if n, _ := handNote(t, s.Store, c.ID); !n.Done.IsZero() {
		t.Fatal("the returned case's hand landing note was not raised again")
	}
	s.GH = script(t, t.TempDir(), "gh", `echo "gh: network down" >&2; exit 1`)
	if got, _, why, err := r.pick(ctx, c.Family, c.ID, false); err != nil || got.ID != "" || !strings.Contains(why, "hand landing") {
		t.Fatalf("an unread hand landing took a slot: %q %q %v", got.ID, why, err)
	}
	done := r.reviews(ctx, c.ID, c.Family)
	if len(done) != 1 || !strings.Contains(done[0].Skipped, "could not be read") {
		t.Fatalf("a review of an unread hand landing: %+v", done)
	}
	// a read that goes through resolves the failed read's note
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"OPEN"}'`)
	if _, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil {
		t.Fatal(err)
	}
	notes, err := s.Store.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if strings.HasPrefix(n.ID, handNoteID(c.ID)+":read:") && n.Done.IsZero() {
			t.Fatalf("a failed read's note stands after a good read: %+v", n)
		}
	}
}

// A revision's build puts a hand landing package back to Prepared with its request open: the request is still read,
// and a merge meanwhile lands it.
func TestAPreparedRevisionStillReadsItsHandLanding(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	p.State = Prepared
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+time.Now().UTC().Format(time.RFC3339)+`","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	if got, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || got.ID != "" {
		t.Fatalf("pick: %q %v", got.ID, err)
	}
	if got, _, _ := s.Load(c.ID); got.State != Landed {
		t.Fatalf("a prepared revision's merged request: %s", got.State)
	}
}

// A request closed while main holds only part of the change is withdrawn, not landed.
func TestAClosedRequestWithPartOfItsChangeOnMainIsWithdrawn(t *testing.T) {
	ctx := context.Background()
	s, c, f, _ := proposedHand(t)
	pushOther(t, f, "main", map[string]string{"internal/capacity/names.go": "package capacity\n\n// Named is the count.\nfunc Named() int { return 42 }\n"})
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED"}'`)
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Withdrawn || got.Withdrew == "" {
		t.Fatalf("a closed request with part of its change on main: %+v %v", got, err)
	}
}

// A request closed after its case left testing and came back is withdrawn without reopening the case again: what the
// case holds now is newer than the request.
func TestAClosedRequestOfACaseThatCameBackDoesNotReopenIt(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting}, "investigation"); err != nil {
		t.Fatal(err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED"}'`)
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Withdrawn {
		t.Fatalf("the close: %+v %v", got, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case was reopened again: %s", got.Status)
	}
}

// On a public repository, whose request carries host-built text, the body names the hand landing paths.
func TestAPublicHandLandingBodyNamesItsPaths(t *testing.T) {
	s, c, f, _ := handFixture(t)
	write(t, filepath.Join(f.dir, "public"), "")
	if _, err := proposePatch(t, s, context.Background(), c.ID, Proposal{Title: "t", Body: "b", Rollback: "r", Signal: goodSignal, Window: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "--body "+HandLine) || !strings.Contains(string(calls), "Hand landing paths: internal/capacity/names.go, internal/capacity/names_test.go") ||
		!strings.Contains(string(calls), "momentum ship status --case "+c.ID+" --json") {
		t.Fatalf("the public body:\n%s", calls)
	}
}

// A preparation that fails after a hand landing withdrawal keeps the withdrawn package and what it counts.
func TestAFailedPreparationKeepsTheClosedProposalCount(t *testing.T) {
	ctx := context.Background()
	s, c, f, p := proposedHand(t)
	p.State, p.ClosedProposals, p.PR, p.Withdrew = Withdrawn, 1, "", p.PR
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	run(t, f.main, "git", "remote", "set-url", "origin", filepath.Join(f.dir, "gone.git"))
	if _, err := s.Prepare(ctx, c.ID, f.main); err == nil {
		t.Fatal("a preparation whose fetch fails went through")
	}
	if kept, had, err := s.Load(c.ID); err != nil || !had || kept.State != Withdrawn || kept.ClosedProposals != 1 {
		t.Fatalf("the failed preparation lost the withdrawn package: %+v %v %v", kept, had, err)
	}
	run(t, f.main, "git", "remote", "set-url", "origin", f.origin)
	if got, err := s.Prepare(ctx, c.ID, f.main); err != nil || got.ClosedProposals != 1 {
		t.Fatalf("the next preparation: %+v %v", got, err)
	}
}

// A late recount that cannot read the store decides nothing: the landing is read again, and the plan keeps its units.
func TestALateRecountThatCannotReadTheStoreDecidesNothing(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	p.Plan.At = p.Plan.At.Add(-15 * 24 * time.Hour)
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.Store.Close()
	if err := s.landHand(ctx, c, &p, p.Head, time.Now(), time.Now()); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("a recount over a closed store: %v", err)
	}
	if got, _, _ := s.Load(c.ID); got.State != Proposed || got.Plan.Units == "" {
		t.Fatalf("a failed recount changed the package: %s %q", got.State, got.Plan.Units)
	}
}

// A request closed after its case was closed and came back in a new episode does not reopen the case.
func TestAClosedRequestOfACaseInANewEpisodeDoesNotReopenIt(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	if _, err := s.Store.CloseCase(ctx, c.ID, store.NotWorthIt, "", "closed by hand", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.ReopenCase(ctx, c.ID, []store.Evidence{{Ref: "file:/new-evidence@1", At: time.Now()}}, "new evidence", "owner"); err != nil {
		t.Fatal(err)
	}
	mech, next := "Named is off by one", "return 42"
	if _, err := s.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "investigation"); err != nil {
		t.Fatal(err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED"}'`)
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Withdrawn {
		t.Fatalf("the close: %+v %v", got, err)
	}
	if got, _ := s.Store.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case was reopened again: %s", got.Status)
	}
}

// A late hand landing whose recount still waits a window after the merge lands, to be measured by hand: a wait that
// never clears holds no merged change for good.
func TestALateHandLandingThatStillWaitsAWindowLaterLands(t *testing.T) {
	ctx := context.Background()
	s, c, _, p := proposedHand(t)
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	p.Plan.At = time.Now().Add(-23 * 24 * time.Hour)
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.SourceLive = func(string) bool { return false } // the signal's reader stopped: the count waits
	merged := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"MERGED","mergedAt":"`+merged+`","headRefOid":"`+p.Head+`","mergeCommit":{"oid":"`+p.Head+`"}}'`)
	got, err := s.reconcileHand(ctx, c.ID)
	if err != nil || got.State != Landed || got.Plan.Units != "" {
		t.Fatalf("a landing whose recount still waits: %+v %v", got.Plan, err)
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	if !slices.ContainsFunc(log, func(e store.CaseEntry) bool { return e.Action == "measured by hand" }) {
		t.Fatal("no log line says the change is measured by hand")
	}
}

// A request whose head a person moved, or that a person retargeted, is theirs: the loop neither revises nor reviews it.
func TestAHandLandingAPersonTookOverIsLeftToThem(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	other := strings.Repeat("b", 40)
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"OPEN","headRefOid":"`+other+`","baseRefName":"main"}'`)
	if _, err := s.reconcileHand(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "a person's now") {
		t.Fatalf("a request whose head moved: %v", err)
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	if !slices.ContainsFunc(log, func(e store.CaseEntry) bool { return e.Action == "hand landing taken over" }) {
		t.Fatal("no log line says a person has the request")
	}
	// the loop's own heads are not a person's: a published head that was not saved leaves the proposed one
	p, _, _ := s.Load(c.ID)
	p.PublishedHead = other
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"OPEN","headRefOid":"`+p.Head+`","baseRefName":"main"}'`)
	if _, err := s.reconcileHand(ctx, c.ID); err != nil {
		t.Fatalf("a request at the loop's own head: %v", err)
	}
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"OPEN","headRefOid":"`+p.Head+`","baseRefName":"release"}'`)
	if _, err := s.reconcileHand(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "targets release") {
		t.Fatalf("a retargeted request: %v", err)
	}
	// closed, a retargeted request is withdrawn as any other
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED","baseRefName":"release"}'`)
	if got, err := s.reconcileHand(ctx, c.ID); err != nil || got.State != Withdrawn {
		t.Fatalf("a closed retargeted request: %+v %v", got, err)
	}
}

// A refusal of a package a withdrawal replaced tells the new package's build nothing, and an unfinished withdrawal
// is not prepared over.
func TestAWithdrawnPackagesRefusalStaysWithIt(t *testing.T) {
	ctx := context.Background()
	s, c, f, p := proposedHand(t)
	if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-063759-0000dddd", Case: c.ID, Family: c.Family, Step: StepBuild,
		Started: time.Now(), Ended: time.Now(), Status: store.RunFailed, Failure: "build check",
		Summary: "build check: refused: the old package's test", CaseEpisode: c.Episode}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	if text, err := r.lastBuildRefusal(ctx, c); err != nil || text == "" {
		t.Fatalf("the package's own refusal: %q %v", text, err)
	}
	p.State, p.ClosedProposals = Withdrawn, 1
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, c.ID, f.main); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("prepared over an unfinished withdrawal: %v", err)
	}
	p.PR = ""
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, c.ID, f.main); err != nil {
		t.Fatal(err)
	}
	if text, err := r.lastBuildRefusal(ctx, c); err != nil || text != "" {
		t.Fatalf("a replaced package's refusal reached the new one: %q %v", text, err)
	}
}

// A reservation the budgets refused between two builds hides nothing: the next build still reads the last refusal.
func TestARefusedReservationKeepsTheLastRefusal(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-063759-0000eeee", Case: c.ID, Family: c.Family, Step: StepBuild,
		Started: time.Now(), Ended: time.Now(), Status: store.RunFailed, Failure: "build check",
		Summary: "build check: refused: the test command is no list of words", CaseEpisode: c.Episode}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PutLoopRun(ctx, store.LoopRun{ID: "20261105-073759-0000ffff", Case: c.ID, Family: c.Family, Step: StepBuild,
		Ended: time.Now().Add(time.Second), Status: store.RunRefused, Summary: "the loop's day budget"}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
	if text, err := r.lastBuildRefusal(ctx, c); err != nil || !strings.Contains(text, "no list of words") {
		t.Fatalf("a refused reservation hid the last refusal: %q %v", text, err)
	}
}

// A window that is no duration is a refusal of the build's output, with what to give instead.
func TestAnInvalidWindowIsARefusal(t *testing.T) {
	for _, w := range []string{"1 week", "7 days", "fortnight"} {
		if _, err := planWindow(w); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "7d or 14d") {
			t.Fatalf("the window %q: %v", w, err)
		}
	}
	if d, err := planWindow("14d"); err != nil || d != 14*24*time.Hour {
		t.Fatalf("a window of two weeks: %v %v", d, err)
	}
}

// A request a person changed, pushed to main as their own version and then closed is a landing: main holds the files
// as the request's own head has them.
func TestAClosedRequestWhosePersonsVersionIsOnMainIsALanding(t *testing.T) {
	ctx := context.Background()
	s, c, f, _ := proposedHand(t)
	theirs := pushOther(t, f, "main", map[string]string{
		"internal/capacity/names.go":      "package capacity\n\n// Named is the count, as a person wrote it.\nfunc Named() int { return 42 }\n",
		"internal/capacity/names_test.go": "package capacity\n\nimport \"testing\"\n\nfunc TestNamed(t *testing.T) {\n\tif Named() != 42 {\n\t\tt.Fatal(Named())\n\t}\n}\n",
	})
	run(t, f.dir, "git", "--git-dir", f.origin, "update-ref", "refs/pull/7/head", theirs)
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"CLOSED","headRefOid":"`+theirs+`"}'`)
	got, err := s.reconcileHand(ctx, c.ID)
	if err != nil || got.State != Landed || !slices.Equal(got.Landed, []string{theirs}) {
		t.Fatalf("a closed request whose person's version is on main: %+v %v", got, err)
	}
}

// What the ship writes into a case's log is cleaned first, and written once.
func TestTheShipsCaseLogLinesAreCleaned(t *testing.T) {
	ctx := context.Background()
	s, c, _, _ := proposedHand(t)
	s.GH = script(t, t.TempDir(), "gh", `echo '{"state":"OPEN","headRefOid":"password=marker-7f3a","baseRefName":"main"}'`)
	for i := 0; i < 2; i++ {
		if _, err := s.reconcileHand(ctx, c.ID); !errors.Is(err, ErrRefused) {
			t.Fatalf("a request a person took over: %v", err)
		}
	}
	log, _ := s.Store.CaseLog(ctx, c.ID)
	n := 0
	for _, e := range log {
		if e.Action == "hand landing taken over" {
			n++
			if strings.Contains(e.Detail, "marker-7f3a") || !strings.Contains(e.Detail, "[masked]") {
				t.Fatalf("an uncleaned log line: %q", e.Detail)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d taken over lines", n)
	}
}

// A lesson that breaks its form drops alone: a multi-line lesson is joined into one line, a longer one is dropped,
// and the report, with its change, still stands. The schema tells the agent the form.
func TestALessonOutOfFormDoesNotFailTheBuild(t *testing.T) {
	report := func(lesson string) []byte {
		raw, err := json.Marshal(map[string]any{"summary": "s", "test_command": []string{"go", "test", "./..."}, "title": "t", "body": "b",
			"rollback": "r", "window": "7d", "lesson": lesson, "signal": goodSignal})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	got, err := buildReport(report("Read MCP configs\nbefore deleting a target."))
	if err != nil || got.Lesson != "Read MCP configs before deleting a target." {
		t.Fatalf("a multi-line lesson: %q %v", got.Lesson, err)
	}
	got, err = buildReport(report(strings.Repeat("word ", 70)))
	if err != nil || got.Lesson != "" || got.Title != "t" {
		t.Fatalf("a lesson over 300 characters: %q %v", got.Lesson, err)
	}
	got, err = buildReport(report(""))
	if err != nil || got.Lesson != "" {
		t.Fatalf("no lesson: %q %v", got.Lesson, err)
	}
	// 300 characters is the limit, counted as characters, not bytes
	if got, err = buildReport(report(strings.Repeat("a", 300))); err != nil || len(got.Lesson) != 300 {
		t.Fatalf("a lesson of exactly 300 characters: %d %v", len(got.Lesson), err)
	}
	hebrew := strings.Repeat("ש", 300)
	if got, err = buildReport(report(hebrew)); err != nil || got.Lesson != hebrew {
		t.Fatalf("300 characters in more than 300 bytes: %d bytes kept, %v", len(got.Lesson), err)
	}
	if got, err = buildReport(report(strings.Repeat("a", 301))); err != nil || got.Lesson != "" {
		t.Fatalf("a lesson of 301 characters: %d %v", len(got.Lesson), err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(buildSchema), &schema); err != nil || !strings.Contains(schema.Properties["lesson"].Description, "joined into one line; one longer than 300 characters is dropped") {
		t.Fatalf("the schema does not give the lesson's form: %+v %v", schema.Properties["lesson"], err)
	}
}
